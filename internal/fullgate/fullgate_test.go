package fullgate

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/delivery"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/preempt"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	owner       = "Owner"
	tasksRepo   = "tasks"
	botRepo     = "router-bot"
	deployFlow  = "deploy.yml"
	sttFlow     = "deploy-stt.yml"
	gatedPath   = ".github/workflows/deploy.yml"
	guardPath   = ".github/workflows/history-guard.yml"
	running     = "in_progress"
	queued      = "queued"
	failure     = "failure"
	headSHA     = "0123456789abcdef0123456789abcdef01234567"
	runURL      = "https://github.com/Owner/tasks/actions/runs/41"
	appTasks    = "tasks"
	appSTT      = "stt"
	appBot      = "router-bot"
	dispatchRun = 41
	sttPath     = ".github/workflows/deploy-stt.yml"
	sharedRepo  = "Owner/tasks"
	hubFlow     = "ci.yml"
	listing     = "listing"
	hubApp      = "deploy-hub"
)

var errBoom = errors.New("boom")

type dispatch struct {
	repo, workflow, ref string
	inputs              map[string]string
}

type fakeActions struct {
	recent        map[string][]preempt.Run
	runs          map[int64]preempt.Run
	dispatched    []preempt.Run
	dispatches    []dispatch
	cancelled     []int64
	failRecent    bool
	failGet       bool
	failCancel    bool
	failDispatch  bool
	failListing   bool
	listedFlows   []string
	recentQueried []string
}

func (f *fakeActions) Recent(_ context.Context, repo string) ([]preempt.Run, error) {
	f.recentQueried = append(f.recentQueried, repo)
	if f.failRecent {
		return nil, errBoom
	}
	return f.recent[repo], nil
}

func (f *fakeActions) Get(_ context.Context, _ string, id int64) (preempt.Run, error) {
	if f.failGet {
		return preempt.Run{}, errBoom
	}
	return f.runs[id], nil
}

func (f *fakeActions) Cancel(_ context.Context, _ string, id int64) error {
	if f.failCancel {
		return errBoom
	}
	f.cancelled = append(f.cancelled, id)
	return nil
}

func (f *fakeActions) Dispatch(_ context.Context, repo, workflow, ref string, inputs map[string]string) error {
	f.dispatches = append(f.dispatches, dispatch{repo: repo, workflow: workflow, ref: ref, inputs: inputs})
	if f.failDispatch {
		return errBoom
	}
	return nil
}

func (f *fakeActions) Dispatched(_ context.Context, repo, workflow, branch string) ([]preempt.Run, error) {
	f.listedFlows = append(f.listedFlows, repo+"/"+workflow+"@"+branch)
	if f.failListing {
		return nil, errBoom
	}
	return f.dispatched, nil
}

type capture struct{ samples []obs.Sample }

func (c *capture) Push(_ context.Context, s []obs.Sample) { c.samples = append(c.samples, s...) }

func (c *capture) value(name, app string) (float64, bool) {
	v, found := 0.0, false
	for _, s := range c.samples {
		if s.Name == name && s.Labels[obs.LabelApp] == app {
			v, found = s.Value, true
		}
	}
	return v, found
}

type notes struct {
	steps []delivery.Step
	err   error
}

func (n *notes) Notify(_ context.Context, steps []delivery.Step) error {
	n.steps = append(n.steps, steps...)
	return n.err
}

type fixture struct {
	gh      *fakeActions
	metrics *capture
	notes   *notes
	now     time.Time
	s       Scheduler
}

func tbilisi(day, hour, minute int) time.Time {
	return time.Date(2026, 11, day, hour, minute, 0, 0, Tbilisi)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{gh: &fakeActions{recent: map[string][]preempt.Run{}, runs: map[int64]preempt.Run{}},
		metrics: &capture{}, notes: &notes{}, now: tbilisi(2, 1, 30)}
	f.s = Scheduler{
		Actions: f.gh, Owner: owner, State: filepath.Join(t.TempDir(), "full-gate.json"),
		Targets: []Target{{App: appBot, Repo: botRepo, Workflow: deployFlow}, {App: appSTT, Repo: tasksRepo, Workflow: sttFlow}, {App: appTasks, Repo: tasksRepo, Workflow: deployFlow}},
		Now:     func() time.Time { return f.now }, Metrics: f.metrics, Delivery: f.notes, Log: slog.New(slog.DiscardHandler),
	}
	return f
}

func (f *fixture) tick(t *testing.T) error {
	t.Helper()
	f.metrics.samples = nil
	return f.s.Run(context.Background())
}

func (f *fixture) state(t *testing.T) State {
	t.Helper()
	st, err := Load(f.s.State)
	testutil.NoErr(t, err)
	return st
}

func (f *fixture) save(t *testing.T, st State) {
	t.Helper()
	testutil.NoErr(t, st.Save(f.s.State))
}

func (f *fixture) success(t *testing.T) float64 {
	t.Helper()
	v, found := f.metrics.value(obs.MetricFullGateSuccess, "")
	testutil.True(t, found, "success sample pushed")
	return v
}

func (f *fixture) startTasks(t *testing.T) {
	t.Helper()
	st := f.state(t)
	st.Current = &Current{App: appTasks, Dispatched: f.now.Add(-time.Hour), RunID: dispatchRun}
	st.Night = f.now.Format(time.DateOnly)
	f.save(t, st)
}

func gateRun(id int64, status string) preempt.Run {
	return preempt.Run{ID: id, Path: gatedPath, Status: status, Event: preempt.EventDispatch}
}

func finished(conclusion string) preempt.Run {
	return preempt.Run{ID: dispatchRun, Path: gatedPath, Status: preempt.StatusCompleted, Conclusion: conclusion, HeadSHA: headSHA, URL: runURL}
}

func TestTargetsAreTheAppsWithAFullGateWhoseRepoHasARunnerHere(t *testing.T) {
	apps := map[string]policy.App{
		appTasks:    {Repo: "owner/tasks", FullGate: deployFlow},
		appSTT:      {Repo: sharedRepo, FullGate: sttFlow},
		appBot:      {Repo: "Owner/router-bot", FullGate: deployFlow},
		"no-runner": {Repo: "Owner/elsewhere", FullGate: deployFlow},
		"no-full":   {Repo: sharedRepo},
		"foreign":   {Repo: "Other/tasks", FullGate: deployFlow},
		hubApp:      {Repo: "Owner/deploy-hub", FullGate: hubFlow},
	}
	got := Targets(apps, owner, []string{tasksRepo, botRepo, hubApp})
	testutil.Equal(t, got, []Target{
		{App: hubApp, Repo: hubApp, Workflow: hubFlow},
		{App: appBot, Repo: botRepo, Workflow: deployFlow},
		{App: appSTT, Repo: tasksRepo, Workflow: sttFlow},
		{App: appTasks, Repo: tasksRepo, Workflow: deployFlow},
	})
}

func TestNothingStartsOutsideTheNightWindowOfTbilisi(t *testing.T) {
	for _, at := range []time.Time{tbilisi(2, 0, 59), tbilisi(2, 3, 0), tbilisi(2, 14, 0), tbilisi(1, 23, 59)} {
		f := newFixture(t)
		f.now = at
		testutil.NoErr(t, f.tick(t))
		testutil.Equal(t, len(f.gh.dispatches), 0)
		testutil.Equal(t, f.success(t), 1.0)
		testutil.Equal(t, f.state(t).Since[appTasks], at.UTC())
	}
}

func TestTheWindowOpensAtOneAndClosesAtThreeTbilisiTime(t *testing.T) {
	for _, at := range []time.Time{tbilisi(2, 1, 0), tbilisi(2, 2, 59), time.Date(2026, 11, 1, 21, 0, 0, 0, time.UTC)} {
		f := newFixture(t)
		f.now = at
		testutil.NoErr(t, f.tick(t))
		testutil.Equal(t, len(f.gh.dispatches), 1)
	}
}

func TestAnIdleNightDispatchesTheFirstDueAppAsAFullGateOfMain(t *testing.T) {
	f := newFixture(t)
	testutil.NoErr(t, f.tick(t))
	testutil.Equal(t, f.gh.dispatches, []dispatch{{repo: botRepo, workflow: deployFlow, ref: preempt.MainBranch, inputs: map[string]string{"full": "true"}}})
	st := f.state(t)
	testutil.Equal(t, *st.Current, Current{App: appBot, Dispatched: f.now.UTC()})
	testutil.Equal(t, st.Night, "2026-11-02")
	testutil.Equal(t, st.Tried[appBot], f.now.UTC())
	running, _ := f.metrics.value(obs.MetricFullGateRunning, appBot)
	testutil.Equal(t, running, 1.0)
	idle, _ := f.metrics.value(obs.MetricFullGateRunning, appTasks)
	testutil.Equal(t, idle, 0.0)
	testutil.Equal(t, f.gh.recentQueried, []string{botRepo, tasksRepo})
}

func TestOnlyOneAppStartsPerNight(t *testing.T) {
	f := newFixture(t)
	testutil.NoErr(t, f.tick(t))
	f.gh.runs[dispatchRun] = finished(preempt.ConclusionCancel)
	f.gh.dispatched = []preempt.Run{{ID: dispatchRun, Created: f.now}}
	f.now = f.now.Add(10 * time.Minute)
	testutil.NoErr(t, f.tick(t))
	testutil.NoErr(t, f.tick(t))
	testutil.True(t, f.state(t).Current == nil, "run settled")
	f.now = f.now.Add(10 * time.Minute)
	testutil.NoErr(t, f.tick(t))
	testutil.Equal(t, len(f.gh.dispatches), 1)
	f.now = f.now.Add(24 * time.Hour)
	testutil.NoErr(t, f.tick(t))
	testutil.Equal(t, len(f.gh.dispatches), 2)
	testutil.Equal(t, f.gh.dispatches[1].workflow, sttFlow)
}

func TestAGateOnAnyRunnerKeepsTheNightRunFromStarting(t *testing.T) {
	for _, status := range []string{queued, running} {
		f := newFixture(t)
		f.gh.recent[tasksRepo] = []preempt.Run{{ID: 7, Path: sttPath, Status: status}}
		testutil.NoErr(t, f.tick(t))
		testutil.Equal(t, len(f.gh.dispatches), 0)
		testutil.Equal(t, f.state(t).Night, "")
	}
}

func TestRunsOutsideTheGatedWorkflowsDoNotCount(t *testing.T) {
	f := newFixture(t)
	f.gh.recent[tasksRepo] = []preempt.Run{
		{ID: 7, Path: guardPath, Status: running},
		{ID: 8, Path: gatedPath + "@refs/heads/main", Status: preempt.StatusCompleted},
	}
	f.gh.recent[botRepo] = []preempt.Run{{ID: 9, Path: sttPath, Status: running}}
	testutil.NoErr(t, f.tick(t))
	testutil.Equal(t, len(f.gh.dispatches), 1)
}

func TestAWorkflowPathWithARefStillCounts(t *testing.T) {
	f := newFixture(t)
	f.gh.recent[tasksRepo] = []preempt.Run{{ID: 7, Path: gatedPath + "@refs/heads/main", Status: running}}
	testutil.NoErr(t, f.tick(t))
	testutil.Equal(t, len(f.gh.dispatches), 0)
}

func TestTheAppWaitingLongestGoesFirstAndAFreshResultIsNotDue(t *testing.T) {
	f := newFixture(t)
	st := f.state(t)
	st.Results[appBot] = Result{At: f.now.Add(-6 * 24 * time.Hour), Passed: true}
	st.Results[appSTT] = Result{At: f.now.Add(-8 * 24 * time.Hour), Passed: true}
	st.Results[appTasks] = Result{At: f.now.Add(-9 * 24 * time.Hour), Passed: true}
	f.save(t, st)
	testutil.NoErr(t, f.tick(t))
	testutil.Equal(t, f.gh.dispatches[0].workflow, deployFlow)
	testutil.Equal(t, f.gh.dispatches[0].repo, tasksRepo)
}

func TestAnAppIsDueSixAndAHalfDaysAfterItsLastResult(t *testing.T) {
	for _, c := range []struct {
		ago  time.Duration
		runs int
	}{{dueAfter - time.Minute, 0}, {dueAfter, 1}} {
		f := newFixture(t)
		st := f.state(t)
		for _, app := range []string{appBot, appSTT, appTasks} {
			st.Results[app] = Result{At: f.now.Add(-c.ago), Passed: true}
		}
		f.save(t, st)
		testutil.NoErr(t, f.tick(t))
		testutil.Equal(t, len(f.gh.dispatches), c.runs)
	}
}

func TestAFailedDispatchUsesUpTheNightAndMovesTheAppBehindTheOthers(t *testing.T) {
	f := newFixture(t)
	f.gh.failDispatch = true
	err := f.tick(t)
	testutil.Is(t, err, errBoom)
	testutil.Contains(t, err.Error(), "dispatch deploy.yml of router-bot for router-bot")
	testutil.Equal(t, f.success(t), 0.0)
	st := f.state(t)
	testutil.True(t, st.Current == nil, "nothing runs")
	testutil.Equal(t, st.Tried[appBot], f.now.UTC())
	f.now = f.now.Add(5 * time.Minute)
	testutil.NoErr(t, f.tick(t))
	testutil.Equal(t, len(f.gh.dispatches), 1)
	f.gh.failDispatch = false
	f.now = f.now.Add(24 * time.Hour)
	testutil.NoErr(t, f.tick(t))
	testutil.Equal(t, f.gh.dispatches[1].workflow, sttFlow)
}

func TestTheDispatchedRunIsFoundAmongTheRunsCreatedSinceTheDispatch(t *testing.T) {
	f := newFixture(t)
	testutil.NoErr(t, f.tick(t))
	f.gh.dispatched = []preempt.Run{
		{ID: 50, Created: f.now.Add(time.Minute)},
		{ID: 40, Created: f.now.Add(-clockSkew)},
		{ID: 30, Created: f.now.Add(-clockSkew - time.Second)},
	}
	f.gh.recent[botRepo] = []preempt.Run{gateRun(40, queued)}
	testutil.NoErr(t, f.tick(t))
	testutil.Equal(t, f.state(t).Current.RunID, int64(40))
	testutil.Equal(t, f.gh.listedFlows, []string{"router-bot/deploy.yml@main"})
	testutil.Equal(t, len(f.gh.cancelled), 0)
}

func TestARunThatNeverAppearsIsGivenUpAfterHalfAnHour(t *testing.T) {
	f := newFixture(t)
	testutil.NoErr(t, f.tick(t))
	f.now = f.now.Add(lostAfter - time.Second)
	testutil.NoErr(t, f.tick(t))
	testutil.True(t, f.state(t).Current != nil, "still waiting for the run")
	f.now = f.now.Add(time.Second)
	err := f.tick(t)
	testutil.Is(t, err, ErrDispatchLost)
	testutil.Contains(t, err.Error(), "router-bot, deploy.yml of router-bot")
	testutil.True(t, f.state(t).Current == nil, "given up")
}

func TestAPassedRunIsRecordedAndPostedToTheFeed(t *testing.T) {
	f := newFixture(t)
	f.startTasks(t)
	f.gh.runs[dispatchRun] = finished("success")
	testutil.NoErr(t, f.tick(t))
	st := f.state(t)
	testutil.True(t, st.Current == nil, "run settled")
	testutil.Equal(t, st.Results[appTasks], Result{At: f.now.UTC(), Passed: true, SHA: headSHA, URL: runURL})
	testutil.Equal(t, f.notes.steps, []delivery.Step{{Repo: "github.com/Owner/tasks", Target: "prod", SHA: "0123456", Step: "checks", State: "ok", Detail: DetailFull, URL: runURL}})
	passed, found := f.metrics.value(obs.MetricFullGateVerdict, appTasks)
	testutil.True(t, found && passed == 1, "passed gauge")
	_, botKnown := f.metrics.value(obs.MetricFullGateVerdict, appBot)
	testutil.True(t, !botKnown, "no gauge before the first result")
	sent, _ := f.metrics.value(obs.MetricDeliverySuccess, "")
	testutil.Equal(t, sent, 1.0)
	age, _ := f.metrics.value(obs.MetricFullGateAge, appTasks)
	testutil.Equal(t, age, 0.0)
}

func TestEveryOtherConclusionIsARedRun(t *testing.T) {
	for _, conclusion := range []string{failure, "timed_out", "startup_failure", "skipped", ""} {
		f := newFixture(t)
		f.startTasks(t)
		f.gh.runs[dispatchRun] = finished(conclusion)
		testutil.NoErr(t, f.tick(t))
		testutil.True(t, !f.state(t).Results[appTasks].Passed, conclusion+" is red")
		testutil.Equal(t, f.notes.steps[0].State, "fail")
		passed, _ := f.metrics.value(obs.MetricFullGateVerdict, appTasks)
		testutil.Equal(t, passed, 0.0)
	}
}

func TestAFailedFeedPostIsMeasuredAndADisabledFeedIsNot(t *testing.T) {
	f := newFixture(t)
	f.startTasks(t)
	f.gh.runs[dispatchRun] = finished(failure)
	f.notes.err = errBoom
	testutil.NoErr(t, f.tick(t))
	sent, found := f.metrics.value(obs.MetricDeliverySuccess, "")
	testutil.True(t, found && sent == 0, "failed post measured")
	g := newFixture(t)
	g.startTasks(t)
	g.gh.runs[dispatchRun] = finished(failure)
	g.notes.err = delivery.ErrDisabled
	testutil.NoErr(t, g.tick(t))
	_, found = g.metrics.value(obs.MetricDeliverySuccess, "")
	testutil.True(t, !found, "no sample while the feed is off")
}

func TestTheNightRunGivesWayToAnyOtherGateAndRunsAgainLater(t *testing.T) {
	f := newFixture(t)
	f.startTasks(t)
	f.gh.runs[dispatchRun] = gateRun(dispatchRun, running)
	f.gh.recent[tasksRepo] = []preempt.Run{gateRun(dispatchRun, running)}
	testutil.NoErr(t, f.tick(t))
	testutil.Equal(t, len(f.gh.cancelled), 0)
	f.gh.recent[botRepo] = []preempt.Run{gateRun(77, queued)}
	testutil.NoErr(t, f.tick(t))
	testutil.NoErr(t, f.tick(t))
	testutil.Equal(t, f.gh.cancelled, []int64{dispatchRun})
	testutil.True(t, f.state(t).Current.Yielded, "yielded")
	f.gh.runs[dispatchRun] = finished(preempt.ConclusionCancel)
	testutil.NoErr(t, f.tick(t))
	st := f.state(t)
	testutil.True(t, st.Current == nil, "run settled")
	_, recorded := st.Results[appTasks]
	testutil.True(t, !recorded, "a cancelled run is no result")
	testutil.Equal(t, len(f.notes.steps), 0)
}

func TestARunOfAnAppThatLeftTheRotationIsDropped(t *testing.T) {
	f := newFixture(t)
	st := f.state(t)
	st.Current = &Current{App: "gone", Dispatched: f.now, RunID: 5}
	st.Night = f.now.Format(time.DateOnly)
	f.save(t, st)
	testutil.NoErr(t, f.tick(t))
	testutil.True(t, f.state(t).Current == nil, "dropped")
}

func TestTheAgeCountsFromTheFirstSightingUntilAResultExists(t *testing.T) {
	f := newFixture(t)
	f.now = tbilisi(2, 12, 0)
	testutil.NoErr(t, f.tick(t))
	f.now = f.now.Add(9 * 24 * time.Hour)
	st := f.state(t)
	st.Results[appBot] = Result{At: f.now.Add(-time.Hour), Passed: false}
	f.save(t, st)
	testutil.NoErr(t, f.tick(t))
	age, _ := f.metrics.value(obs.MetricFullGateAge, appTasks)
	testutil.Equal(t, age, (9 * 24 * time.Hour).Seconds())
	botAge, _ := f.metrics.value(obs.MetricFullGateAge, appBot)
	testutil.Equal(t, botAge, time.Hour.Seconds())
}

func TestGitHubErrorsFailTheRunAndKeepTheState(t *testing.T) {
	cases := map[string]func(f *fakeActions){
		"recent": func(f *fakeActions) { f.failRecent = true },
		"get":    func(f *fakeActions) { f.failGet = true },
		listing:  func(f *fakeActions) { f.failListing = true },
		"cancel": func(f *fakeActions) { f.failCancel = true },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.startTasks(t)
			st := f.state(t)
			if name == listing {
				st.Current.RunID = 0
				f.save(t, st)
			}
			f.gh.runs[dispatchRun] = gateRun(dispatchRun, running)
			f.gh.recent[botRepo] = []preempt.Run{gateRun(77, running)}
			breakIt(f.gh)
			testutil.Is(t, f.tick(t), errBoom)
			testutil.Equal(t, f.success(t), 0.0)
			testutil.True(t, f.state(t).Current != nil && !f.state(t).Current.Yielded, "state kept")
		})
	}
}

func TestAnUnreadableStatePushesOnlyTheFailure(t *testing.T) {
	f := newFixture(t)
	testutil.NoErr(t, os.WriteFile(f.s.State, []byte("{"), 0o600))
	testutil.Fails(t, f.tick(t))
	testutil.Equal(t, len(f.metrics.samples), 1)
	testutil.Equal(t, f.success(t), 0.0)
	testutil.Equal(t, len(f.gh.recentQueried), 0)
}

func TestAStateThatCannotBeSavedFailsTheRun(t *testing.T) {
	f := newFixture(t)
	f.s.State = filepath.Join(t.TempDir(), "missing", "full-gate.json")
	testutil.Fails(t, f.tick(t))
	testutil.Equal(t, f.success(t), 0.0)
}

func TestTheStateFileIsPrivate(t *testing.T) {
	f := newFixture(t)
	testutil.NoErr(t, f.tick(t))
	info, err := os.Stat(f.s.State)
	testutil.NoErr(t, err)
	testutil.Equal(t, info.Mode().Perm(), os.FileMode(0o600))
	_, err = os.Stat(f.s.State + ".tmp")
	testutil.True(t, os.IsNotExist(err), "no staged file left")
}

func TestLoadReportsAnUnreadableFile(t *testing.T) {
	_, err := Load(t.TempDir())
	testutil.Fails(t, err)
}
