package historysweep

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	repo      = "owner/app"
	fakePAT   = "github_pat_test"
	goneRef   = "/repos/owner/app/git/refs/heads/old"
	listRefs  = "for-each-ref"
	tasksRepo = "DreamsAreReal/tasks"
	weirdRef  = "refs/heads/fix/50%#1+é"
)

type capture struct{ samples []obs.Sample }

func (c *capture) Push(_ context.Context, s []obs.Sample) { c.samples = append(c.samples, s...) }

func (c *capture) value(name, repo string) float64 {
	for _, s := range c.samples {
		if s.Name == name && s.Labels["repo"] == repo {
			return s.Value
		}
	}
	return -1
}

type api struct {
	mu      sync.Mutex
	deleted []string
	auth    []string
	status  int
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.Method != http.MethodDelete {
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		return
	}
	a.deleted = append(a.deleted, r.URL.EscapedPath())
	a.auth = append(a.auth, r.Header.Get("Authorization"))
	if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") != apiVersion {
		http.Error(w, "missing GitHub API headers", http.StatusBadRequest)
		return
	}
	if a.status != 0 {
		http.Error(w, `{"message":"Resource not accessible by personal access token"}`, a.status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) paths() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.deleted)
}

type fixture struct {
	s       Sweeper
	api     *api
	work    string
	root    string
	metrics *capture
	log     *bytes.Buffer
	journal string
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "work")
	testutil.Mkdir(t, work)
	gitIn(t, work, "init", "-q", "-b", "main")
	testutil.Write(t, filepath.Join(work, "a"), "snapshot")
	gitIn(t, work, "add", "a")
	gitIn(t, work, "commit", "-q", "-m", "snapshot")
	root := gitIn(t, work, "rev-parse", "HEAD")
	gitIn(t, work, "branch", "feature/ok")
	gitIn(t, work, "tag", "-a", "v1", "-m", "v1")
	f := &fixture{api: &api{}, work: work, root: root, metrics: &capture{}, log: &bytes.Buffer{},
		journal: filepath.Join(base, "deploys.log")}
	srv := httptest.NewServer(f.api)
	t.Cleanup(srv.Close)
	now := func() time.Time { return time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC) }
	f.s = Sweeper{
		Targets: []Target{{Repo: repo, Root: root}}, API: srv.URL, Git: "file://" + filepath.Join(base, "remote"),
		Token: fakePAT, Client: srv.Client(), Exec: procexec.NewOS(GitBinary), TempDir: t.TempDir(),
		Journal: journal.Journal{Path: f.journal, Now: now}, Metrics: f.metrics, Now: now,
		Log: slog.New(slog.NewJSONHandler(f.log, nil)),
	}
	return f
}

func (f *fixture) orphan(t *testing.T, ref string) {
	t.Helper()
	gitIn(t, f.work, "checkout", "-q", "--orphan", "tmp-orphan")
	gitIn(t, f.work, "commit", "-q", "--allow-empty", "-m", "old history")
	gitIn(t, f.work, "update-ref", ref, "HEAD")
	gitIn(t, f.work, "checkout", "-q", "-f", "main")
	gitIn(t, f.work, "branch", "-D", "tmp-orphan")
}

func (f *fixture) publish(t *testing.T) {
	t.Helper()
	remote := strings.TrimPrefix(f.s.Git, "file://")
	testutil.Mkdir(t, filepath.Join(remote, "owner"))
	gitIn(t, f.work, "clone", "-q", "--bare", f.work, filepath.Join(remote, repo+gitSuffix))
}

func TestSweepDeletesRefsWithAForeignRootAndKeepsTheRest(t *testing.T) {
	f := newFixture(t)
	f.orphan(t, "refs/heads/old")
	f.orphan(t, "refs/tags/v0")
	gitIn(t, f.work, "checkout", "-q", "-b", "mixed", "main")
	gitIn(t, f.work, "merge", "-q", "--allow-unrelated-histories", "-m", "join", "old")
	gitIn(t, f.work, "checkout", "-q", "main")
	f.publish(t)

	testutil.NoErr(t, f.s.Run(context.Background()))

	testutil.Equal(t, f.api.paths(), []string{
		"/repos/owner/app/git/refs/heads/mixed",
		goneRef,
		"/repos/owner/app/git/refs/tags/v0",
	})
	testutil.Equal(t, f.api.auth[0], "Bearer "+fakePAT)
	testutil.Equal(t, f.metrics.value(obs.MetricHistorySweepSuccess, repo), 1.0)
	testutil.Equal(t, f.metrics.value(obs.MetricHistorySweepDeleted, repo), 3.0)
	testutil.Contains(t, testutil.Read(t, f.journal), "deploy-hub@- history-sweep ok 0s repo=owner/app deleted=3")
	testutil.Contains(t, f.log.String(), `"msg":"ref deleted"`, `"ref":"refs/heads/old"`)
}

func TestSweepOfACleanRepoDeletesNothingAndSkipsTheJournal(t *testing.T) {
	f := newFixture(t)
	f.publish(t)

	testutil.NoErr(t, f.s.Run(context.Background()))

	testutil.Equal(t, len(f.api.paths()), 0)
	testutil.Equal(t, f.metrics.value(obs.MetricHistorySweepSuccess, repo), 1.0)
	testutil.Equal(t, f.metrics.value(obs.MetricHistorySweepDeleted, repo), 0.0)
	testutil.Missing(t, f.journal)
	testutil.Contains(t, f.log.String(), `"msg":"history sweep clean"`)
}

func TestSweepEncodesRefNamesForTheAPI(t *testing.T) {
	f := newFixture(t)
	f.orphan(t, weirdRef)
	f.publish(t)

	testutil.NoErr(t, f.s.Run(context.Background()))

	testutil.Equal(t, f.api.paths(), []string{"/repos/owner/app/git/refs/heads/fix/50%25%231+%C3%A9"})
}

func TestAPIErrorFailsTheSweepAndIsJournaled(t *testing.T) {
	f := newFixture(t)
	f.orphan(t, "refs/heads/old")
	f.orphan(t, "refs/heads/older")
	f.publish(t)
	f.api.status = http.StatusForbidden

	err := f.s.Run(context.Background())

	testutil.Is(t, err, ErrDelete)
	testutil.Contains(t, err.Error(), "owner/app: ", "refs/heads/old: HTTP 403", "refs/heads/older: HTTP 403", "Resource not accessible")
	testutil.Equal(t, len(f.api.paths()), 2)
	testutil.Equal(t, f.metrics.value(obs.MetricHistorySweepSuccess, repo), 0.0)
	testutil.Contains(t, testutil.Read(t, f.journal), "deploy-hub@- history-sweep fail 0s repo=owner/app deleted=0")
	testutil.Contains(t, f.log.String(), `"level":"ERROR"`, `"msg":"history sweep failed"`)
}

func TestUnreachableRepoFailsAndOtherTargetsStillRun(t *testing.T) {
	f := newFixture(t)
	f.orphan(t, "refs/heads/old")
	f.publish(t)
	f.s.Targets = append([]Target{{Repo: "owner/missing", Root: f.root}}, f.s.Targets...)

	err := f.s.Run(context.Background())

	testutil.Fails(t, err)
	testutil.Contains(t, err.Error(), "owner/missing: git clone")
	testutil.Equal(t, f.api.paths(), []string{goneRef})
	testutil.Equal(t, f.metrics.value(obs.MetricHistorySweepSuccess, "owner/missing"), 0.0)
	testutil.Equal(t, f.metrics.value(obs.MetricHistorySweepDeleted, "owner/missing"), 0.0)
	testutil.Equal(t, f.metrics.value(obs.MetricHistorySweepSuccess, repo), 1.0)
	testutil.Contains(t, testutil.Read(t, f.journal), "history-sweep fail 0s repo=owner/missing deleted=0")
}

func TestGitGetsTheTokenOnlyThroughItsEnvironment(t *testing.T) {
	fake := &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) {
		return procexec.Result{}, errors.New("offline")
	}}
	s := Sweeper{Token: fakePAT, Exec: fake}

	out, err := s.git(context.Background(), "clone", "x")
	testutil.Equal(t, out, "")

	testutil.Contains(t, err.Error(), "git clone: offline")
	c := fake.Calls()[0]
	testutil.Excludes(t, strings.Join(c.Args, " "), fakePAT)
	testutil.Equal(t, c.Env, []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+fakePAT))})
}

func TestUnreadableRefListOrRootsFailTheTarget(t *testing.T) {
	for _, failing := range []string{listRefs, "rev-list"} {
		t.Run(failing, func(t *testing.T) {
			fake := &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
				if slices.Contains(c.Args, failing) {
					return procexec.Result{}, errors.New("broken")
				}
				return procexec.Result{Stdout: "refs/heads/main\n"}, nil
			}}
			s := Sweeper{Exec: fake, TempDir: t.TempDir()}
			_, err := s.sweep(context.Background(), Target{Repo: repo, Root: strings.Repeat("a", rootLength)})
			testutil.Contains(t, err.Error(), "git --git-dir: broken")
		})
	}
}

func TestUnreachableAPIFailsTheRef(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	s := Sweeper{API: srv.URL, Client: srv.Client()}
	testutil.Contains(t, s.delete(context.Background(), repo, "refs/heads/x").Error(), "delete refs/heads/x: ")
	s.API = "http://bad host"
	testutil.Fails(t, s.delete(context.Background(), repo, "refs/heads/x"))
}

func TestMissingTempDirFailsTheTarget(t *testing.T) {
	s := Sweeper{TempDir: filepath.Join(t.TempDir(), "absent")}
	deleted, err := s.sweep(context.Background(), Target{Repo: repo})
	testutil.Is(t, err, os.ErrNotExist)
	testutil.Equal(t, deleted, 0)
}

func TestJournalFailureIsLogged(t *testing.T) {
	log := &bytes.Buffer{}
	s := Sweeper{Journal: journal.Journal{Path: t.TempDir(), Now: time.Now}, Metrics: obs.Discard{}, Now: time.Now,
		Log: slog.New(slog.NewJSONHandler(log, nil))}
	s.report(context.Background(), Target{Repo: repo}, 1, nil, time.Now())
	testutil.Contains(t, log.String(), `"msg":"journal append failed"`)
}

func TestParseTargets(t *testing.T) {
	root := strings.Repeat("0a", rootLength/2)
	got, err := ParseTargets(" " + tasksRepo + "@" + root + "\tDreamsAreReal/deploy-hub@" + root + " ")
	testutil.NoErr(t, err)
	testutil.Equal(t, got, []Target{{Repo: tasksRepo, Root: root}, {Repo: "DreamsAreReal/deploy-hub", Root: root}})
	_, err = ParseTargets("  ")
	testutil.Is(t, err, ErrNoTargets)
	for _, bad := range []string{"DreamsAreReal/tasks", "tasks@" + root, "o/n@" + root[1:], "o/n@" + strings.ToUpper(root), "o/n@" + root + "0", "o/n@" + root[1:] + "g"} {
		_, err = ParseTargets(bad)
		testutil.Is(t, err, ErrTarget)
	}
}

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "github.token")
	testutil.Write(t, path, "  "+fakePAT+"\n")
	got, err := ReadToken(path)
	testutil.NoErr(t, err)
	testutil.Equal(t, got, fakePAT)
	testutil.Write(t, path, "\n")
	got, err = ReadToken(path)
	testutil.Is(t, err, ErrNoToken)
	testutil.Equal(t, got, "")
	got, err = ReadToken(filepath.Join(dir, "absent"))
	testutil.Is(t, err, os.ErrNotExist)
	testutil.Equal(t, got, "")
}

func TestEscapeRef(t *testing.T) {
	testutil.Equal(t, EscapeRef("refs/heads/main"), "refs/heads/main")
	testutil.Equal(t, EscapeRef("refs/tags/a b?c"), "refs/tags/a%20b%3Fc")
}
