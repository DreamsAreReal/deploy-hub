package preempt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	owner    = "Owner"
	repoA    = "tasks"
	repoB    = "router-bot"
	pat      = "github_pat_hub"
	wrongPAT = "github_pat_wrong"
	queued   = "queued"
	running  = "in_progress"
	done     = "completed"
	gateFlow = 7
	prBranch = "feature"
	dispatch = "workflow_dispatch"
	passed   = "success"
)

type fakeGitHub struct {
	mu        sync.Mutex
	runs      map[string]map[int64]*Run
	cancelled []string
	reruns    []string
	failGet   bool
	failPost  bool
	failRun   int64
	gets      []string
	notPR     int64
	srv       *httptest.Server
}

func newFake(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{runs: map[string]map[int64]*Run{repoA: {}, repoB: {}}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) add(repo string, r Run) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.WorkflowID == 0 {
		r.WorkflowID = gateFlow
	}
	r.Attempt = 1
	f.runs[repo][r.ID] = &r
}

func (f *fakeGitHub) set(id int64, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[repoA][id].Status = status
}

func (f *fakeGitHub) run(repo string, id int64) Run {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.runs[repo][id]
}

func (f *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+pat {
		http.Error(w, "Bad credentials", http.StatusUnauthorized)
		return
	}
	if r.Header.Get("Accept") != acceptJSON || r.Header.Get("X-GitHub-Api-Version") != apiVersion {
		http.Error(w, "bad headers", http.StatusBadRequest)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"+owner+"/"), "/")
	runs, ok := f.runs[parts[0]]
	if !ok || len(parts) < 3 || parts[1] != "actions" || parts[2] != "runs" {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 3 {
		f.list(w, runs)
		return
	}
	id, _ := strconv.ParseInt(parts[3], 10, 64)
	run, ok := runs[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	f.one(w, r, parts, run)
}

func (f *fakeGitHub) list(w http.ResponseWriter, runs map[int64]*Run) {
	if f.failGet {
		http.Error(w, "boom", http.StatusBadGateway)
		return
	}
	all := make([]Run, 0, len(runs))
	for _, r := range runs {
		all = append(all, *r)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID > all[j].ID })
	_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": all})
}

func (f *fakeGitHub) one(w http.ResponseWriter, r *http.Request, parts []string, run *Run) {
	key := parts[0] + "/" + parts[3]
	switch {
	case len(parts) == 4 && r.Method == http.MethodGet:
		f.gets = append(f.gets, key)
		if run.ID == f.failRun {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		out := *run
		if run.ID == f.notPR {
			out.Event = eventPush
		}
		_ = json.NewEncoder(w).Encode(out)
	case f.failPost:
		http.Error(w, "boom", http.StatusInternalServerError)
	case len(parts) == 5 && parts[4] == "cancel" && r.Method == http.MethodPost:
		f.cancelled = append(f.cancelled, key)
		run.Status, run.Conclusion = done, conclusionCancel
		w.WriteHeader(http.StatusAccepted)
	case len(parts) == 5 && parts[4] == "rerun" && r.Method == http.MethodPost:
		f.reruns = append(f.reruns, key)
		run.Status, run.Conclusion, run.Attempt = queued, "", run.Attempt+1
		w.WriteHeader(http.StatusCreated)
	default:
		http.NotFound(w, r)
	}
}

type capture struct{ samples []obs.Sample }

func (c *capture) Push(_ context.Context, s []obs.Sample) { c.samples = append(c.samples, s...) }

func (c *capture) value(name, repo string) float64 {
	v := -1.0
	for _, s := range c.samples {
		if s.Name == name && s.Labels[labelRepo] == repo {
			v = s.Value
		}
	}
	return v
}

type fixture struct {
	gh      *fakeGitHub
	p       Preempter
	metrics *capture
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	gh := newFake(t)
	f := &fixture{gh: gh, metrics: &capture{}}
	now := time.Date(2026, 10, 23, 15, 0, 0, 0, time.UTC)
	f.p = Preempter{
		GitHub: GitHub{API: gh.srv.URL, Owner: owner, Token: pat, Client: gh.srv.Client()},
		Repos:  []string{repoA, repoB}, State: filepath.Join(t.TempDir(), "preempt.json"), Keep: DefaultKeep,
		Now: func() time.Time { return now }, Metrics: f.metrics, Log: slog.New(slog.DiscardHandler),
	}
	return f
}

func push(id int64, status string) Run {
	return Run{ID: id, Event: eventPush, HeadBranch: mainBranch, Status: status}
}

func pr(id int64, branch, status string) Run {
	return Run{ID: id, Event: EventPullRequest, HeadBranch: branch, Status: status}
}

func TestAWaitingMainDeployCancelsOnlyPullRequestRuns(t *testing.T) {
	f := newFixture(t)
	f.gh.add(repoA, push(1, running))
	f.gh.add(repoA, push(2, queued))
	f.gh.add(repoA, pr(3, prBranch, running))
	f.gh.add(repoA, pr(4, "other", queued))
	f.gh.add(repoA, pr(5, "old", done))
	f.gh.add(repoA, Run{ID: 6, Event: dispatch, HeadBranch: mainBranch, Status: running})
	f.gh.add(repoA, Run{ID: 8, Event: "schedule", HeadBranch: mainBranch, Status: queued})
	f.gh.add(repoB, pr(9, prBranch, running))
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, strings.Join(f.gh.cancelled, " "), "tasks/4 tasks/3")
	testutil.Equal(t, f.gh.run(repoA, 1).Status, running)
	testutil.Equal(t, f.gh.run(repoA, 2).Status, queued)
	testutil.Equal(t, f.gh.run(repoB, 9).Status, running)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptCancelled, repoA), 2.0)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptCancelled, repoB), 0.0)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptSuccess, ""), 1.0)
}

func TestARunningMainDeployOrNoMainLeavesPullRequestsAlone(t *testing.T) {
	f := newFixture(t)
	f.gh.add(repoA, push(1, running))
	f.gh.add(repoA, pr(2, prBranch, queued))
	f.gh.add(repoA, Run{ID: 3, Event: eventPush, HeadBranch: prBranch, Status: queued})
	f.gh.add(repoB, pr(4, prBranch, running))
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, len(f.gh.cancelled), 0)
}

func TestARunThatTurnsIntoAnotherEventIsNotCancelled(t *testing.T) {
	f := newFixture(t)
	f.gh.add(repoA, push(1, queued))
	f.gh.add(repoA, pr(2, prBranch, running))
	gh := f.gh
	f.p.GitHub.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/runs/2") {
			gh.mu.Lock()
			gh.runs[repoA][2].Status = done
			gh.mu.Unlock()
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, len(f.gh.cancelled), 0)
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCancelledRunsAreRerunOnceMainHasTheRunner(t *testing.T) {
	f := newFixture(t)
	f.gh.add(repoA, push(1, queued))
	f.gh.add(repoA, pr(2, prBranch, running))
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, len(f.gh.reruns), 0)
	f.gh.set(1, running)
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, strings.Join(f.gh.reruns, " "), "tasks/2")
	testutil.Equal(t, f.gh.run(repoA, 2).Status, queued)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRerun, repoA), 1.0)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRerunsMax, repoA), 1.0)
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, len(f.gh.reruns), 1)
}

func TestRerunsAreCountedAcrossPreemptions(t *testing.T) {
	f := newFixture(t)
	f.gh.add(repoA, pr(2, prBranch, running))
	for i := int64(1); i <= 4; i++ {
		f.gh.add(repoA, push(10+i, queued))
		testutil.NoErr(t, f.p.Run(context.Background()))
		f.gh.set(10+i, done)
		testutil.NoErr(t, f.p.Run(context.Background()))
		f.gh.set(2, running)
	}
	testutil.Equal(t, len(f.gh.cancelled), 4)
	testutil.Equal(t, len(f.gh.reruns), 4)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRerunsMax, repoA), 4.0)
	testutil.Equal(t, f.gh.run(repoA, 2).Attempt, 5)
}

func TestARunReplacedByANewerPushIsNotRerun(t *testing.T) {
	f := newFixture(t)
	f.gh.add(repoA, push(1, queued))
	f.gh.add(repoA, pr(2, prBranch, running))
	testutil.NoErr(t, f.p.Run(context.Background()))
	f.gh.set(1, done)
	f.gh.add(repoA, pr(3, prBranch, queued))
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, len(f.gh.reruns), 0)
	st, err := Load(f.p.State)
	testutil.NoErr(t, err)
	testutil.Equal(t, st.Runs[0].Pending, false)
}

func TestARunFinishedOtherwiseIsNotRerun(t *testing.T) {
	f := newFixture(t)
	f.gh.add(repoA, push(1, queued))
	f.gh.add(repoA, pr(2, prBranch, running))
	f.p.GitHub.Client = f.gh.srv.Client()
	testutil.NoErr(t, f.p.Run(context.Background()))
	f.gh.mu.Lock()
	f.gh.runs[repoA][2].Conclusion = passed
	f.gh.runs[repoA][1].Status = done
	f.gh.mu.Unlock()
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, len(f.gh.reruns), 0)
}

func TestACancelStillInFlightWaits(t *testing.T) {
	f := newFixture(t)
	f.gh.add(repoA, push(1, queued))
	f.gh.add(repoA, pr(2, prBranch, running))
	testutil.NoErr(t, f.p.Run(context.Background()))
	f.gh.set(2, running)
	f.gh.set(1, done)
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, len(f.gh.reruns), 0)
	f.gh.set(2, done)
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, len(f.gh.reruns), 1)
}

func TestFailuresAreReportedAndKeepTheState(t *testing.T) {
	f := newFixture(t)
	f.gh.add(repoA, push(1, queued))
	f.gh.add(repoA, pr(2, prBranch, running))
	f.gh.failPost = true
	onlyTasksFails(t, f)
	f.gh.failPost = false
	testutil.NoErr(t, f.p.Run(context.Background()))
	f.gh.set(1, done)
	f.gh.failPost = true
	onlyTasksFails(t, f)
	f.gh.failPost = false
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, len(f.gh.reruns), 1)
	f.gh.failGet = true
	testutil.Is(t, f.p.Run(context.Background()), ErrHTTP)
}

func TestAWrongTokenFails(t *testing.T) {
	f := newFixture(t)
	f.p.GitHub.Token = wrongPAT
	err := f.p.Run(context.Background())
	testutil.Is(t, err, ErrHTTP)
	testutil.Contains(t, err.Error(), "401")
}

func TestStateIsPrunedAfterTheKeepTime(t *testing.T) {
	now := time.Date(2026, 10, 23, 15, 0, 0, 0, time.UTC)
	st := State{Runs: []Entry{
		{Repo: repoA, ID: 1, Changed: now.Add(-25 * time.Hour)},
		{Repo: repoA, ID: 2, Changed: now.Add(-25 * time.Hour), Pending: true},
		{Repo: repoA, ID: 3, Changed: now.Add(-time.Hour), Reruns: 2},
	}}
	st.prune(now.Add(-DefaultKeep))
	testutil.Equal(t, len(st.Runs), 2)
	testutil.Equal(t, st.maxReruns(repoA), 2)
	testutil.Equal(t, st.maxReruns(repoB), 0)
}

func TestStateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "preempt.json")
	st, err := Load(path)
	testutil.NoErr(t, err)
	testutil.Equal(t, len(st.Runs), 0)
	st.cancelled(repoA, pr(2, prBranch, running), time.Now())
	testutil.NoErr(t, st.Save(path))
	testutil.Perm(t, path, statePerm)
	back, err := Load(path)
	testutil.NoErr(t, err)
	testutil.Equal(t, back.Runs[0].Branch, prBranch)
	testutil.NoErr(t, os.WriteFile(path, []byte("{"), statePerm))
	_, err = Load(path)
	testutil.Fails(t, err)
	f := newFixture(t)
	f.p.State = path
	testutil.Fails(t, f.p.Run(context.Background()))
	testutil.Fails(t, st.Save(filepath.Join(dir, "missing", "preempt.json")))
	_, err = Load(dir)
	testutil.Fails(t, err)
}

func TestRunnerRepos(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"tasks/runner", "router-bot/runner", "lost+found"} {
		testutil.Mkdir(t, filepath.Join(root, d))
	}
	testutil.Write(t, filepath.Join(root, "tasks", "runner", ".runner"), "{}")
	testutil.Write(t, filepath.Join(root, "router-bot", "runner", ".runner"), "{}")
	repos := RunnerRepos(root)
	sort.Strings(repos)
	testutil.Equal(t, strings.Join(repos, " "), "router-bot tasks")
}

func logged(f *fixture) *strings.Builder {
	var b strings.Builder
	f.p.Log = slog.New(slog.NewTextHandler(&b, nil))
	return &b
}

func TestOnlyActivePullRequestRunsAreLookedAtAgain(t *testing.T) {
	f := newFixture(t)
	f.gh.add(repoA, push(1, queued))
	f.gh.add(repoA, pr(2, prBranch, done))
	f.gh.add(repoA, Run{ID: 3, Event: dispatch, HeadBranch: mainBranch, Status: running})
	f.gh.add(repoA, pr(4, prBranch, running))
	log := logged(f)
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, strings.Join(f.gh.gets, " "), "tasks/4")
	testutil.Contains(t, log.String(), "pull request gate preempted by a main deploy", "run=4")
}

func TestARunThatIsNoLongerAPullRequestIsNotCancelled(t *testing.T) {
	f := newFixture(t)
	f.gh.add(repoA, push(1, queued))
	f.gh.add(repoA, pr(2, prBranch, running))
	f.gh.notPR = 2
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, len(f.gh.cancelled), 0)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptCancelled, repoA), 0.0)
}

func TestAFailedLookupOrCancelCountsNothing(t *testing.T) {
	f := newFixture(t)
	f.gh.add(repoA, push(1, queued))
	f.gh.add(repoA, pr(2, prBranch, running))
	f.gh.failRun = 2
	onlyTasksFails(t, f)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptCancelled, repoA), 0.0)
	f.gh.failRun = 0
	f.gh.failPost = true
	onlyTasksFails(t, f)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptCancelled, repoA), 0.0)
	st, err := Load(f.p.State)
	testutil.NoErr(t, err)
	testutil.Equal(t, len(st.Runs), 0)
}

func TestRequeueCountsOnlyRealReruns(t *testing.T) {
	f := newFixture(t)
	f.gh.add(repoA, push(1, queued))
	f.gh.add(repoA, pr(2, prBranch, running))
	f.gh.add(repoA, pr(3, "other", running))
	testutil.NoErr(t, f.p.Run(context.Background()))
	f.gh.set(1, done)
	f.gh.add(repoA, pr(4, "other", queued))
	f.gh.set(2, running)
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRerun, repoA), 0.0)
	f.gh.failRun = 2
	f.gh.set(2, done)
	onlyTasksFails(t, f)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRerun, repoA), 0.0)
	f.gh.failRun = 0
	f.gh.failPost = true
	onlyTasksFails(t, f)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRerun, repoA), 0.0)
	f.gh.failPost = false
	log := logged(f)
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRerun, repoA), 1.0)
	testutil.Contains(t, log.String(), "pull request gate re-run after a main deploy", "run=2")
	testutil.Equal(t, strings.Join(f.gh.reruns, " "), "tasks/2")
}

func TestARunFinishedOtherwiseCountsNoRerun(t *testing.T) {
	f := newFixture(t)
	f.gh.add(repoA, push(1, queued))
	f.gh.add(repoA, pr(2, prBranch, running))
	testutil.NoErr(t, f.p.Run(context.Background()))
	f.gh.mu.Lock()
	f.gh.runs[repoA][2].Conclusion = passed
	f.gh.runs[repoA][1].Status = done
	f.gh.mu.Unlock()
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRerun, repoA), 0.0)
	f.gh.notPR = 2
	f.gh.mu.Lock()
	f.gh.runs[repoA][2].Conclusion = conclusionCancel
	f.gh.mu.Unlock()
	st, err := Load(f.p.State)
	testutil.NoErr(t, err)
	st.Runs[0].Pending = true
	testutil.NoErr(t, st.Save(f.p.State))
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, len(f.gh.reruns), 0)
}

func TestRunForgetsOldSettledRuns(t *testing.T) {
	f := newFixture(t)
	old := State{Runs: []Entry{{Repo: repoA, ID: 9, Reruns: 5, Changed: f.p.Now().Add(-48 * time.Hour)}}}
	testutil.NoErr(t, old.Save(f.p.State))
	testutil.NoErr(t, f.p.Run(context.Background()))
	st, err := Load(f.p.State)
	testutil.NoErr(t, err)
	testutil.Equal(t, len(st.Runs), 0)
}

func TestTheClientFailsWithoutAServerOrWithABadURL(t *testing.T) {
	f := newFixture(t)
	f.gh.srv.Close()
	_, err := f.p.GitHub.Recent(context.Background(), repoA)
	testutil.Fails(t, err)
	testutil.True(t, !errors.Is(err, ErrHTTP), err.Error())
	bad := GitHub{API: "http://\x7f", Owner: owner, Token: pat, Client: http.DefaultClient}
	_, err = bad.Get(context.Background(), repoA, 1)
	testutil.Fails(t, err)
}

func TestStateMarksTheRunOfTheRightRepo(t *testing.T) {
	now := time.Now()
	st := State{Runs: []Entry{{Repo: repoB, ID: 2}, {Repo: repoA, ID: 3}}}
	st.cancelled(repoA, pr(2, prBranch, running), now)
	testutil.Equal(t, len(st.Runs), 3)
	testutil.Equal(t, st.Runs[0].Pending, false)
	testutil.Equal(t, st.Runs[1].Pending, false)
	st.cancelled(repoA, pr(2, prBranch, running), now)
	testutil.Equal(t, len(st.Runs), 3)
}

func TestStateFileErrors(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(dir)
	testutil.Is(t, err, syscall.EISDIR)
	path := filepath.Join(dir, "preempt.json")
	testutil.Mkdir(t, path+".tmp")
	st := State{}
	testutil.Fails(t, st.Save(path))
	testutil.Missing(t, path)
}

func onlyTasksFails(t *testing.T, f *fixture) {
	t.Helper()
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRepoSuccess, repoA), 0.0)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRepoDenied, repoA), 0.0)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRepoSuccess, repoB), 1.0)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptSuccess, ""), 1.0)
}

func TestARepoWithoutAccessDoesNotStopTheOthers(t *testing.T) {
	f := newFixture(t)
	f.p.Repos = []string{"server-ops", repoA, repoB}
	f.gh.add(repoA, push(1, queued))
	f.gh.add(repoA, pr(2, prBranch, running))
	log := logged(f)
	testutil.NoErr(t, f.p.Run(context.Background()))
	testutil.Equal(t, strings.Join(f.gh.cancelled, " "), "tasks/2")
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRepoSuccess, "server-ops"), 0.0)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRepoDenied, "server-ops"), 1.0)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRepoSuccess, repoA), 1.0)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRepoDenied, repoA), 0.0)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRepoSuccess, repoB), 1.0)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptSuccess, ""), 1.0)
	testutil.Contains(t, log.String(), "level=WARN", "preempt skipped a repo", "repo=server-ops", "denied=true", "404 Not Found")
}

func TestAPassFailsWhenNoRepoCanBeHandled(t *testing.T) {
	f := newFixture(t)
	f.p.GitHub.Token = wrongPAT
	err := f.p.Run(context.Background())
	testutil.Contains(t, err.Error(), "tasks: ", "router-bot: ", "401")
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRepoDenied, repoA), 1.0)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptRepoDenied, repoB), 1.0)
	testutil.Equal(t, f.metrics.value(obs.MetricPreemptSuccess, ""), 0.0)
}

func TestOnlyRefusalsCountAsDenied(t *testing.T) {
	for code, want := range map[int]bool{401: true, 403: true, 404: true, 500: false, 502: false} {
		testutil.Equal(t, Denied(fmt.Errorf("wrapped: %w", &StatusError{Code: code})), want)
	}
	testutil.Equal(t, Denied(errors.New("dial tcp: refused")), false)
	testutil.Equal(t, Denied(nil), false)
	testutil.Is(t, &StatusError{Code: 404}, ErrHTTP)
}
