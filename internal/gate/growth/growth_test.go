package growth

import (
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

var start = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func lines(n int) string {
	return strings.Repeat("x := 1\n", n)
}

type repo struct {
	t  *testing.T
	ws gate.Workspace
}

func newRepo(t *testing.T) repo {
	t.Helper()
	r := repo{t: t, ws: gate.Workspace{Src: t.TempDir(), Exec: procexec.NewOS(source.GitBinary), Log: slog.New(slog.DiscardHandler)}}
	r.git(nil, "init", "-q")
	return r
}

func (r repo) git(env []string, args ...string) string {
	r.t.Helper()
	res, err := r.ws.RunEnv(r.t.Context(), env, source.GitBinary, append([]string{"-c", "user.name=gate", "-c", "user.email=gate@example.com"}, args...)...)
	if err != nil {
		r.t.Fatalf("git %v: %v %s", args, err, procexec.Output(err))
	}
	return strings.TrimSpace(res.Stdout)
}

func (r repo) commit(at time.Time, files map[string]string) {
	r.t.Helper()
	for name, body := range files {
		testutil.Write(r.t, filepath.Join(r.ws.Src, filepath.FromSlash(name)), body)
	}
	stamp := at.Format(time.RFC3339)
	r.git(nil, "add", "-A")
	r.git([]string{"GIT_AUTHOR_DATE=" + stamp, "GIT_COMMITTER_DATE=" + stamp}, "commit", "-q", "-m", stamp)
}

func (r repo) check(limit int) gate.Outcome {
	r.ws.Policy.Thresholds.MaxNetGrowth = limit
	return Check(r.t.Context(), r.ws)
}

func history(t *testing.T) repo {
	t.Helper()
	r := newRepo(t)
	r.commit(start, map[string]string{"a.go": lines(100), "README.md": lines(3)})
	r.commit(start.Add(24*time.Hour), map[string]string{
		"a.go": lines(95), "b.go": lines(10), "docs/guide.md": lines(50), "x/testdata/case.go": lines(20), "go.sum": lines(30), "logo.bin": "\x00\x01",
	})
	return r
}

func TestCountsOnlyCodeSinceTheFirstCommitWhenTheHistoryIsShort(t *testing.T) {
	outcome := history(t).check(0)
	testutil.Equal(t, outcome.Status, gate.StatusPass)
	testutil.Equal(t, outcome.Values, map[string]float64{ValueAdded: 10, ValueRemoved: 5, ValueNet: 5})
}

func TestCountsTheLastThirtyDaysAndJudgesTheThreshold(t *testing.T) {
	r := history(t)
	r.commit(start.Add(40*24*time.Hour), map[string]string{"b.go": lines(8), "c.go": lines(7)})
	outcome := r.check(0)
	testutil.Equal(t, outcome.Values, map[string]float64{ValueAdded: 7, ValueRemoved: 2, ValueNet: 5})
	testutil.Equal(t, r.check(5).Status, gate.StatusPass)
	over := r.check(4)
	testutil.Equal(t, over.Status, gate.StatusFail)
	testutil.Contains(t, over.Reason, "max_net_growth_lines")
}

func TestTheWindowStartsOnTheMainLine(t *testing.T) {
	r := newRepo(t)
	r.commit(start, map[string]string{"a.go": lines(100)})
	r.commit(start.Add(24*time.Hour), map[string]string{"b.go": lines(10)})
	r.git(nil, "checkout", "-q", "-b", "side", "HEAD~1")
	r.commit(start.Add(5*24*time.Hour), map[string]string{"c.go": lines(3)})
	r.git(nil, "checkout", "-q", "-")
	stamp := start.Add(40 * 24 * time.Hour).Format(time.RFC3339)
	r.git([]string{"GIT_AUTHOR_DATE=" + stamp, "GIT_COMMITTER_DATE=" + stamp}, "merge", "-q", "--no-ff", "-m", "merge", "side")
	testutil.Equal(t, r.check(0).Values, map[string]float64{ValueAdded: 3, ValueRemoved: 0, ValueNet: 3})
}

func TestFailsWithoutHistory(t *testing.T) {
	outcome := Check(t.Context(), gate.Workspace{Src: t.TempDir(), Exec: procexec.NewOS(source.GitBinary), Log: slog.New(slog.DiscardHandler)})
	testutil.Equal(t, outcome.Status, gate.StatusFail)
	testutil.Contains(t, outcome.Reason, "git show")
}

func TestFailsOnUnreadableGitAnswers(t *testing.T) {
	answer := func(show string, diff error) *procexectest.Fake {
		return &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
			switch c.Args[0] {
			case "show":
				return procexec.Result{Stdout: show}, nil
			case "diff":
				return procexec.Result{}, diff
			}
			return procexec.Result{Stdout: "abc\n"}, nil
		}}
	}
	for want, fake := range map[string]*procexectest.Fake{
		`commit time "soon"`: answer("soon\n", nil),
		"git diff":           answer("1785000000\n", errors.New("broken")),
	} {
		outcome := Check(t.Context(), gate.Workspace{Exec: fake, Log: slog.New(slog.DiscardHandler)})
		testutil.Equal(t, outcome.Status, gate.StatusFail)
		testutil.Contains(t, outcome.Reason, want)
	}
}

func TestCountSkipsBinaryAndMalformedRecords(t *testing.T) {
	added, removed := count("3\t1\ta.go\x00-\t-\tlogo.png\x00broken\x00x\t2\tb.go\x00")
	testutil.Equal(t, [2]int{added, removed}, [2]int{3, 1})
}
