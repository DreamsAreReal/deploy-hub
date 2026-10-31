package hostsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	restartCmd = "systemctl restart runner.service"
	busyExpr   = "max(max_over_time(tasks_runner_active_runs[2m]))"
	newExpr    = `max(tasks_runner_info{version=~"2222222.*"})`
	oldExpr    = `max(tasks_runner_info{version=~"1111111.*"})`
)

type answer struct {
	value   float64
	present bool
	err     error
}

type fakeQuery struct {
	answers map[string][]answer
	asked   []string
}

func (q *fakeQuery) Scalar(_ context.Context, expr string) (float64, bool, error) {
	q.asked = append(q.asked, expr)
	list := q.answers[expr]
	if len(list) == 0 {
		return 0, false, nil
	}
	a := list[0]
	if len(list) > 1 {
		q.answers[expr] = list[1:]
	}
	return a.value, a.present, a.err
}

type restartFixture struct {
	r       Restarter
	query   *fakeQuery
	exec    *procexectest.Fake
	script  procexectest.Script
	metrics *recorder
	root    string
	now     time.Time
}

func newRestartFixture(t *testing.T) *restartFixture {
	t.Helper()
	base := t.TempDir()
	f := &restartFixture{
		query:   &fakeQuery{answers: map[string][]answer{busyExpr: {{value: 0, present: true}}, newExpr: {{value: 1, present: true}}}},
		script:  procexectest.Script{restartCmd: {}},
		metrics: &recorder{},
		root:    filepath.Join(base, "host"),
		now:     time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC),
	}
	testutil.Mkdir(t, filepath.Join(f.root, policy.RunnerDir))
	testutil.Write(t, filepath.Join(f.root, runnerDst), newBinary)
	testutil.Write(t, filepath.Join(f.root, runnerDst+".prev-1111111"), oldBinary)
	f.exec = &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) { return f.script.Handle(c) }}
	f.r = Restarter{
		Policy: policy.Policy{Apps: map[string]policy.App{runnerApp: {Host: runnerSpec()}, "obs": {Host: spec()}, "web": {}}},
		States: filepath.Join(base, "restarts"), Root: f.root, Exec: f.exec, Query: f.query,
		Journal: journal.Journal{Path: filepath.Join(base, "deploys.log"), Now: func() time.Time { return f.now }},
		Now:     func() time.Time { return f.now },
		Sleep:   func(context.Context, time.Duration) error { f.now = f.now.Add(PollEvery); return nil },
		Out:     console.New(&bytes.Buffer{}), Log: slog.New(slog.DiscardHandler), Metrics: f.metrics,
	}
	return f
}

func (f *restartFixture) pending(t *testing.T, prevTag string, since time.Time) {
	t.Helper()
	testutil.NoErr(t, SaveState(f.r.States, runnerApp, RestartState{Installed: prevTag, Pending: &Pending{Tag: newTag, PrevTag: prevTag, Since: since}}))
}

func (f *restartFixture) state(t *testing.T) RestartState {
	t.Helper()
	st, err := LoadState(f.r.States, runnerApp)
	testutil.NoErr(t, err)
	return st
}

func (f *restartFixture) journal(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.r.Journal.Path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	testutil.NoErr(t, err)
	return string(data)
}

func TestNothingPendingRestartsNothing(t *testing.T) {
	f := newRestartFixture(t)
	testutil.NoErr(t, f.r.Run(context.Background()))
	testutil.Equal(t, len(f.exec.Lines()), 0)
	testutil.Equal(t, len(f.query.asked), 0)
	testutil.Equal(t, f.metrics.value(obs.MetricHostRestartPending), 0.0)
}

func TestBusyUnitIsNotRestartedHoweverLongItWaits(t *testing.T) {
	f := newRestartFixture(t)
	f.pending(t, oldTag, f.now.Add(-30*time.Hour))
	for _, busy := range []answer{{value: 2, present: true}, {present: false}, {err: errors.New("victoria down")}} {
		f.query.answers[busyExpr] = []answer{busy}
		err := f.r.Run(context.Background())
		testutil.True(t, (err != nil) == (busy.err != nil), fmt.Sprintf("query error is reported: %v", err))
	}
	testutil.Equal(t, f.exec.Count("systemctl"), 0)
	testutil.Equal(t, f.metrics.value(obs.MetricHostRestartPending), (30 * time.Hour).Seconds())
	testutil.True(t, f.state(t).Pending != nil, "still pending")
	testutil.Equal(t, f.journal(t), "")
}

func TestIdleUnitIsRestartedAndTheNewVersionConfirmed(t *testing.T) {
	f := newRestartFixture(t)
	f.pending(t, oldTag, f.now.Add(-time.Minute))
	f.query.answers[newExpr] = []answer{{present: false}, {value: 0, present: true}, {value: 1, present: true}}
	testutil.NoErr(t, f.r.Run(context.Background()))
	testutil.Equal(t, strings.Join(f.exec.Lines(), "\n"), restartCmd)
	testutil.Equal(t, testutil.Read(t, filepath.Join(f.root, runnerDst)), newBinary)
	st := f.state(t)
	testutil.True(t, st.Pending == nil && st.Installed == newTag, "the new tag is installed")
	testutil.Contains(t, f.journal(t), "tasks@2222222 host-restart ok", "unit=runner.service")
	testutil.Equal(t, f.metrics.value(obs.MetricHostRestartSuccess), 1.0)
	testutil.Contains(t, strings.Join(f.query.asked, "\n"), busyExpr, newExpr)
}

func TestSilentNewVersionRollsBackToThePreviousBinary(t *testing.T) {
	f := newRestartFixture(t)
	f.pending(t, oldTag, f.now)
	f.query.answers[newExpr] = []answer{{value: 0, present: true}}
	f.query.answers[oldExpr] = []answer{{value: 1, present: true}}
	err := f.r.Run(context.Background())
	testutil.Is(t, err, ErrNotReported)
	testutil.Equal(t, testutil.Read(t, filepath.Join(f.root, runnerDst)), oldBinary)
	testutil.Equal(t, f.exec.Count(restartCmd), 2)
	st := f.state(t)
	testutil.True(t, st.Pending == nil && st.Installed == oldTag, "back on the previous tag")
	testutil.Contains(t, f.journal(t), "tasks@2222222 host-restart fail")
	testutil.Equal(t, f.metrics.value(obs.MetricHostRestartSuccess), 0.0)
	testutil.True(t, f.now.Sub(time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)) >= 2*time.Minute, "waited the whole window")
}

func TestFailedRestartRollsBackToAnUnknownPreviousWithoutVerifying(t *testing.T) {
	f := newRestartFixture(t)
	testutil.Write(t, filepath.Join(f.root, runnerDst+".prev-unknown"), oldBinary)
	f.pending(t, "", f.now)
	calls := 0
	f.script[restartCmd] = procexectest.Reply{Run: func(procexec.Command) { calls++ }}
	f.query.answers[newExpr] = nil
	testutil.Is(t, f.r.Run(context.Background()), ErrNotReported)
	testutil.Equal(t, testutil.Read(t, filepath.Join(f.root, runnerDst)), oldBinary)
	testutil.Equal(t, calls, 2)
	testutil.Equal(t, f.state(t).Installed, "")
}

func TestRestartCommandFailureAlsoRollsBack(t *testing.T) {
	f := newRestartFixture(t)
	f.pending(t, oldTag, f.now)
	f.script[restartCmd] = procexectest.Reply{Err: errors.New("unit failed")}
	err := f.r.Run(context.Background())
	testutil.Fails(t, err)
	testutil.Contains(t, err.Error(), "systemctl restart runner.service")
	testutil.Equal(t, testutil.Read(t, filepath.Join(f.root, runnerDst)), oldBinary)
}

func TestMissingPreviousCopyIsReported(t *testing.T) {
	f := newRestartFixture(t)
	testutil.NoErr(t, os.Remove(filepath.Join(f.root, runnerDst+".prev-1111111")))
	f.pending(t, oldTag, f.now)
	f.query.answers[newExpr] = nil
	testutil.Is(t, f.r.Run(context.Background()), ErrNoPrevious)
	testutil.Equal(t, testutil.Read(t, filepath.Join(f.root, runnerDst)), newBinary)
	testutil.Contains(t, f.journal(t), "host-restart fail")
}

func TestCancelledWaitStopsTheVerify(t *testing.T) {
	f := newRestartFixture(t)
	f.pending(t, oldTag, f.now)
	f.query.answers[newExpr] = nil
	f.query.answers[oldExpr] = []answer{{value: 1, present: true}}
	f.r.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
	testutil.Is(t, f.r.Run(context.Background()), context.Canceled)
	testutil.Equal(t, testutil.Read(t, filepath.Join(f.root, runnerDst)), oldBinary)
}

func TestHeldStateIsLeftForTheNextRun(t *testing.T) {
	f := newRestartFixture(t)
	f.pending(t, oldTag, f.now)
	release, err := LockState(context.Background(), f.r.States, runnerApp, 0)
	testutil.NoErr(t, err)
	defer release()
	testutil.NoErr(t, f.r.Run(context.Background()))
	testutil.Equal(t, len(f.exec.Lines()), 0)
}

func TestUnreadableStateIsReported(t *testing.T) {
	f := newRestartFixture(t)
	testutil.Write(t, statePath(f.r.States, runnerApp), "not json")
	testutil.Fails(t, f.r.Run(context.Background()))
	testutil.Equal(t, len(f.exec.Lines()), 0)
}

func TestUnwritableStateDirectoryIsReported(t *testing.T) {
	f := newRestartFixture(t)
	testutil.Write(t, f.r.States, "a file")
	testutil.Fails(t, f.r.Run(context.Background()))
	testutil.Equal(t, len(f.exec.Lines()), 0)
}
