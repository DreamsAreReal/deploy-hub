package mainguard

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/preempt"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	owner    = "Owner"
	repo     = "deploy-hub"
	pat      = "github_pat_hub"
	base     = "1111111111111111111111111111111111111111"
	next     = "2222222222222222222222222222222222222222"
	other    = "3333333333333333333333333333333333333333"
	prHead   = "4444444444444444444444444444444444444444"
	passed   = "success"
	failed   = "failure"
	running  = "in_progress"
	gateName = "quality gate / gate"
	skipped  = "skipped"
	sweep    = "sweep"
	keySHA   = "sha"
	keyStat  = "status"
)

type fake struct {
	mu     sync.Mutex
	routes map[string]any
	srv    *httptest.Server
}

func (f *fake) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+pat {
		http.Error(w, "Bad credentials", http.StatusUnauthorized)
		return
	}
	body, ok := f.routes[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fake) set(path string, body any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes["/repos/"+owner+"/"+repo+path] = body
}

func (f *fake) drop(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.routes, "/repos/"+owner+"/"+repo+path)
}

type capture struct{ samples []obs.Sample }

func (c *capture) Push(_ context.Context, s []obs.Sample) { c.samples = append(c.samples, s...) }

func (c *capture) last(name string) float64 {
	v := -1.0
	for _, s := range c.samples {
		if s.Name == name {
			v = s.Value
		}
	}
	return v
}

type fixture struct {
	gh      *fake
	g       Guard
	metrics *capture
	log     *bytes.Buffer
	now     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{gh: &fake{routes: map[string]any{}}, metrics: &capture{}, log: &bytes.Buffer{}}
	f.gh.srv = httptest.NewServer(http.HandlerFunc(f.gh.handle))
	t.Cleanup(f.gh.srv.Close)
	f.now = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	f.g = Guard{
		GitHub: preempt.GitHub{API: f.gh.srv.URL, Owner: owner, Token: pat, Client: f.gh.srv.Client()},
		Repos:  []string{repo}, State: filepath.Join(t.TempDir(), "main-guard.json"), Hold: DefaultHold,
		Now: func() time.Time { return f.now }, Metrics: f.metrics, Log: slog.New(slog.NewTextHandler(f.log, nil)),
	}
	f.main(base)
	return f
}

func (f *fixture) main(sha string) {
	f.gh.set("/branches/main", map[string]any{"commit": map[string]string{keySHA: sha}})
}

func (f *fixture) moved(status string, commits ...string) {
	list := make([]map[string]string, 0, len(commits))
	for _, c := range commits {
		list = append(list, map[string]string{keySHA: c})
	}
	f.gh.set("/compare/"+base+"..."+next, map[string]any{keyStat: status, "commits": list})
	f.main(next)
}

func (f *fixture) pull(commit, baseRef string, merged bool) {
	p := map[string]any{"number": 7, "base": map[string]string{"ref": baseRef}, "head": map[string]string{keySHA: prHead}}
	if merged {
		p["merged_at"] = "2026-10-31T11:00:00Z"
	}
	f.gh.set("/commits/"+commit+"/pulls", []any{p})
}

func (f *fixture) checks(runs ...[2]string) {
	list := make([]map[string]string, 0, len(runs))
	for _, r := range runs {
		status := "completed"
		if r[1] == running {
			status = running
		}
		list = append(list, map[string]string{"name": r[0], keyStat: status, "conclusion": r[1]})
	}
	f.gh.set("/commits/"+prHead+"/check-runs", map[string]any{"check_runs": list})
}

func (f *fixture) run(t *testing.T) {
	t.Helper()
	testutil.NoErr(t, f.g.Run(context.Background()))
}

func (f *fixture) violation(t *testing.T) string {
	t.Helper()
	st, err := Load(f.g.State)
	testutil.NoErr(t, err)
	return st[repo].Violation
}

func TestTheFirstRunOnlyRemembersMain(t *testing.T) {
	f := newFixture(t)
	f.run(t)
	testutil.Equal(t, f.metrics.last(obs.MetricMainGuardOK), 1.0)
	testutil.Equal(t, f.metrics.last(obs.MetricMainGuardSuccess), 1.0)
	st, err := Load(f.g.State)
	testutil.NoErr(t, err)
	testutil.Equal(t, st[repo].Head, base)
}

func TestAMergedGreenPullRequestIsFine(t *testing.T) {
	f := newFixture(t)
	f.run(t)
	f.moved(compareAhead, next)
	f.pull(next, mainBranch, true)
	f.checks([2]string{gateName, passed}, [2]string{sweep, skipped})
	f.run(t)
	testutil.Equal(t, f.metrics.last(obs.MetricMainGuardOK), 1.0)
	testutil.Equal(t, f.violation(t), "")
	f.run(t)
	testutil.Equal(t, f.metrics.last(obs.MetricMainGuardOK), 1.0)
}

func TestUnguardedChangesOfMainAreFlagged(t *testing.T) {
	cases := map[string]struct {
		setup func(f *fixture)
		want  string
	}{
		"force push":        {func(f *fixture) { f.moved("diverged") }, KindRewrite + " " + next},
		"reset back":        {func(f *fixture) { f.moved("behind") }, KindRewrite + " " + next},
		"direct push":       {func(f *fixture) { f.moved(compareAhead, next); f.gh.set("/commits/"+next+"/pulls", []any{}) }, KindNoPR + " " + next},
		"open pull request": {func(f *fixture) { f.moved(compareAhead, next); f.pull(next, mainBranch, false) }, KindNoPR + " " + next},
		"pull into another branch": {func(f *fixture) {
			f.moved(compareAhead, next)
			f.pull(next, "release", true)
		}, KindNoPR + " " + next},
		"red gate": {func(f *fixture) {
			f.moved(compareAhead, next)
			f.pull(next, mainBranch, true)
			f.checks([2]string{gateName, failed})
		}, KindRedChecks + " " + next},
		"gate still running": {func(f *fixture) {
			f.moved(compareAhead, next)
			f.pull(next, mainBranch, true)
			f.checks([2]string{gateName, running})
		}, KindRedChecks + " " + next},
		"no checks": {func(f *fixture) {
			f.moved(compareAhead, next)
			f.pull(next, mainBranch, true)
			f.checks()
		}, KindRedChecks + " " + next},
		"only skipped checks": {func(f *fixture) {
			f.moved(compareAhead, next)
			f.pull(next, mainBranch, true)
			f.checks([2]string{sweep, skipped})
		}, KindRedChecks + " " + next},
		"second commit unguarded": {func(f *fixture) {
			f.moved(compareAhead, other, next)
			f.pull(other, mainBranch, true)
			f.checks([2]string{gateName, passed})
			f.gh.set("/commits/"+next+"/pulls", []any{})
		}, KindNoPR + " " + next},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.run(t)
			tc.setup(f)
			f.run(t)
			testutil.Equal(t, f.violation(t), tc.want)
			testutil.Equal(t, f.metrics.last(obs.MetricMainGuardOK), 0.0)
			testutil.Contains(t, f.log.String(), "main moved without a green pull request", "repo="+repo)
		})
	}
}

func TestAViolationStaysVisibleForTheHoldTime(t *testing.T) {
	f := newFixture(t)
	f.run(t)
	f.moved("diverged")
	f.run(t)
	f.now = f.now.Add(DefaultHold - time.Minute)
	f.run(t)
	testutil.Equal(t, f.metrics.last(obs.MetricMainGuardOK), 0.0)
	f.now = f.now.Add(2 * time.Minute)
	f.run(t)
	testutil.Equal(t, f.metrics.last(obs.MetricMainGuardOK), 1.0)
}

func TestFailuresAreReportedAndMainIsCheckedAgain(t *testing.T) {
	f := newFixture(t)
	f.run(t)
	f.moved(compareAhead, next)
	f.pull(next, mainBranch, true)
	testutil.Is(t, f.g.Run(context.Background()), preempt.ErrHTTP)
	testutil.Equal(t, f.metrics.last(obs.MetricMainGuardSuccess), 0.0)
	st, err := Load(f.g.State)
	testutil.NoErr(t, err)
	testutil.Equal(t, st[repo].Head, base)
	for _, path := range []string{"/compare/" + base + "..." + next, "/commits/" + next + "/pulls", "/branches/main"} {
		g := newFixture(t)
		g.run(t)
		g.moved(compareAhead, next)
		g.pull(next, mainBranch, true)
		g.checks([2]string{gateName, passed})
		g.gh.drop(path)
		testutil.Is(t, g.g.Run(context.Background()), preempt.ErrHTTP)
		testutil.Equal(t, g.violation(t), "")
	}
	f.checks([2]string{gateName, passed})
	f.run(t)
	testutil.Equal(t, f.violation(t), "")
}

func TestTooManyCommitsAreAnError(t *testing.T) {
	f := newFixture(t)
	f.run(t)
	shas := make([]string, maxCommits)
	for i := range shas {
		shas[i] = next
	}
	f.moved(compareAhead, shas...)
	testutil.Is(t, f.g.Run(context.Background()), ErrTooMany)
}

func TestAWrongTokenFails(t *testing.T) {
	f := newFixture(t)
	f.g.GitHub.Token = "github_pat_wrong"
	err := f.g.Run(context.Background())
	testutil.Is(t, err, preempt.ErrHTTP)
	testutil.Contains(t, err.Error(), "401")
}

func TestStateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main-guard.json")
	testutil.NoErr(t, os.WriteFile(path, []byte("{"), statePerm))
	_, err := Load(path)
	testutil.Fails(t, err)
	f := newFixture(t)
	f.g.State = path
	testutil.Fails(t, f.g.Run(context.Background()))
	_, err = Load(dir)
	testutil.Fails(t, err)
	testutil.Mkdir(t, filepath.Join(dir, "x.json.tmp"))
	testutil.Fails(t, State{}.Save(filepath.Join(dir, "x.json")))
	testutil.Missing(t, filepath.Join(dir, "x.json"))
	g := newFixture(t)
	g.run(t)
	testutil.Perm(t, g.g.State, statePerm)
	testutil.True(t, strings.Contains(testutil.Read(t, g.g.State), base), "head saved")
}
