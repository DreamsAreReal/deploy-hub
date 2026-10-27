package fullgate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/delivery"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/preempt"
)

const (
	DefaultState  = "/var/lib/deploy-hub/full-gate.json"
	Every         = 7 * 24 * time.Hour
	dueAfter      = Every - 12*time.Hour
	windowOpens   = time.Hour
	windowCloses  = 3 * time.Hour
	lostAfter     = 30 * time.Minute
	clockSkew     = 2 * time.Minute
	tbilisiOffset = 4 * 60 * 60
	inputFull     = "full"
	inputOn       = "true"
	conclusionOK  = "success"
	workflowDir   = ".github/workflows/"
	refSeparator  = "@"
	repoSeparator = "/"
	shortSHA      = 7
	DetailFull    = "full gate"
	samplesPerApp = 3
)

var (
	Tbilisi         = time.FixedZone("Asia/Tbilisi", tbilisiOffset)
	ErrDispatchLost = errors.New("the dispatched full gate run never appeared")
)

type Target struct {
	App      string
	Repo     string
	Workflow string
}

type Actions interface {
	Recent(ctx context.Context, repo string) ([]preempt.Run, error)
	Get(ctx context.Context, repo string, id int64) (preempt.Run, error)
	Cancel(ctx context.Context, repo string, id int64) error
	Dispatch(ctx context.Context, repo, workflow, ref string, inputs map[string]string) error
	Dispatched(ctx context.Context, repo, workflow, branch string) ([]preempt.Run, error)
}

type Scheduler struct {
	Actions  Actions
	Targets  []Target
	Owner    string
	State    string
	Now      func() time.Time
	Metrics  obs.Pusher
	Delivery delivery.Notifier
	Log      *slog.Logger
}

func Targets(apps map[string]policy.App, owner string, runners []string) []Target {
	var out []Target
	for _, name := range slices.Sorted(maps.Keys(apps)) {
		app := apps[name]
		repoOwner, repo, _ := strings.Cut(app.Repo, repoSeparator)
		if app.FullGate != "" && strings.EqualFold(repoOwner, owner) && slices.Contains(runners, repo) {
			out = append(out, Target{App: name, Repo: repo, Workflow: app.FullGate})
		}
	}
	return out
}

func (s Scheduler) Run(ctx context.Context) error {
	now := s.Now().UTC()
	st, err := Load(s.State)
	var samples []obs.Sample
	if err == nil {
		st.track(s.Targets, now)
		err = errors.Join(s.step(ctx, &st, now), st.Save(s.State))
		samples = s.appSamples(&st, now)
	}
	s.Metrics.Push(context.WithoutCancel(ctx), append(samples,
		obs.Sample{Name: obs.MetricFullGateSuccess, Labels: obs.Labels{}, Value: obs.Bool(err == nil)}))
	return err
}

func (s Scheduler) step(ctx context.Context, st *State, now time.Time) error {
	busy, err := s.busy(ctx)
	if err != nil {
		return err
	}
	if st.Current != nil {
		return s.follow(ctx, st, busy, now)
	}
	return s.start(ctx, st, busy, now)
}

func (s Scheduler) busy(ctx context.Context) ([]preempt.Run, error) {
	var active []preempt.Run
	for _, repo := range s.repos() {
		runs, err := s.Actions.Recent(ctx, repo)
		if err != nil {
			return nil, err
		}
		for _, r := range runs {
			if preempt.Active(r) && s.gated(repo, r.Path) {
				active = append(active, r)
			}
		}
	}
	return active, nil
}

func (s Scheduler) repos() []string {
	var repos []string
	for _, t := range s.Targets {
		if !slices.Contains(repos, t.Repo) {
			repos = append(repos, t.Repo)
		}
	}
	return repos
}

func (s Scheduler) gated(repo, path string) bool {
	path, _, _ = strings.Cut(path, refSeparator)
	return slices.ContainsFunc(s.Targets, func(t Target) bool {
		return t.Repo == repo && workflowDir+t.Workflow == path
	})
}

func (s Scheduler) target(app string) (Target, bool) {
	i := slices.IndexFunc(s.Targets, func(t Target) bool { return t.App == app })
	if i < 0 {
		return Target{}, false
	}
	return s.Targets[i], true
}

func (s Scheduler) follow(ctx context.Context, st *State, busy []preempt.Run, now time.Time) error {
	cur := st.Current
	t, ok := s.target(cur.App)
	if !ok {
		st.Current = nil
		return nil
	}
	if cur.RunID == 0 {
		return s.resolve(ctx, st, t, now)
	}
	run, err := s.Actions.Get(ctx, t.Repo, cur.RunID)
	if err != nil {
		return err
	}
	if run.Status == preempt.StatusCompleted {
		s.finish(ctx, st, t, run, now)
		return nil
	}
	if cur.Yielded || !othersBusy(busy, run.ID) {
		return nil
	}
	if err := s.Actions.Cancel(ctx, t.Repo, run.ID); err != nil {
		return err
	}
	cur.Yielded = true
	s.Log.Info("full gate yields to another gate and runs again on a later night", "app", t.App, "run", run.ID)
	return nil
}

func othersBusy(busy []preempt.Run, own int64) bool {
	return slices.ContainsFunc(busy, func(r preempt.Run) bool { return r.ID != own })
}

func (s Scheduler) resolve(ctx context.Context, st *State, t Target, now time.Time) error {
	runs, err := s.Actions.Dispatched(ctx, t.Repo, t.Workflow, preempt.MainBranch)
	if err != nil {
		return err
	}
	if id := firstSince(runs, st.Current.Dispatched.Add(-clockSkew)); id != 0 {
		st.Current.RunID = id
		s.Log.Info("full gate run started", "app", t.App, "run", id)
		return nil
	}
	if now.Sub(st.Current.Dispatched) < lostAfter {
		return nil
	}
	st.Current = nil
	return fmt.Errorf("%w: %s, %s of %s", ErrDispatchLost, t.App, t.Workflow, t.Repo)
}

func firstSince(runs []preempt.Run, after time.Time) int64 {
	var first int64
	for _, r := range runs {
		if !r.Created.Before(after) && (first == 0 || r.ID < first) {
			first = r.ID
		}
	}
	return first
}

func (s Scheduler) finish(ctx context.Context, st *State, t Target, run preempt.Run, now time.Time) {
	st.Current = nil
	if run.Conclusion == preempt.ConclusionCancel {
		s.Log.Info("full gate run was cancelled and runs again on a later night", "app", t.App, "run", run.ID)
		return
	}
	res := Result{At: now, Passed: run.Conclusion == conclusionOK, SHA: run.HeadSHA, URL: run.URL}
	st.Results[t.App] = res
	s.Log.Info("full gate finished", "app", t.App, "run", run.ID, "passed", res.Passed, "conclusion", run.Conclusion, "sha", res.SHA)
	s.notify(ctx, t, res)
}

func (s Scheduler) notify(ctx context.Context, t Target, res Result) {
	state := delivery.StateFail
	if res.Passed {
		state = delivery.StateOK
	}
	step := delivery.Step{Repo: delivery.RepoLabel(s.Owner + repoSeparator + t.Repo), Target: delivery.DefaultTarget,
		SHA: res.SHA[:min(shortSHA, len(res.SHA))], Step: delivery.StepChecks, State: state, Detail: DetailFull, URL: res.URL}
	err := s.Delivery.Notify(context.WithoutCancel(ctx), []delivery.Step{step})
	if errors.Is(err, delivery.ErrDisabled) {
		return
	}
	s.Metrics.Push(context.WithoutCancel(ctx), []obs.Sample{{Name: obs.MetricDeliverySuccess, Labels: obs.Labels{}, Value: obs.Bool(err == nil)}})
}

func (s Scheduler) start(ctx context.Context, st *State, busy []preempt.Run, now time.Time) error {
	local := now.In(Tbilisi)
	night := local.Format(time.DateOnly)
	if !inWindow(local) || st.Night == night || len(busy) > 0 {
		return nil
	}
	t, ok := s.due(st, now)
	if !ok {
		return nil
	}
	st.Night, st.Tried[t.App] = night, now
	if err := s.Actions.Dispatch(ctx, t.Repo, t.Workflow, preempt.MainBranch, map[string]string{inputFull: inputOn}); err != nil {
		return fmt.Errorf("dispatch %s of %s for %s: %w", t.Workflow, t.Repo, t.App, err)
	}
	st.Current = &Current{App: t.App, Dispatched: now}
	s.Log.Info("full gate dispatched", "app", t.App, "repo", t.Repo, "workflow", t.Workflow)
	return nil
}

func inWindow(local time.Time) bool {
	clock := time.Duration(local.Hour())*time.Hour + time.Duration(local.Minute())*time.Minute
	return clock >= windowOpens && clock < windowCloses
}

func (s Scheduler) due(st *State, now time.Time) (Target, bool) {
	var pick Target
	found := false
	for _, t := range s.Targets {
		if now.Sub(st.lastAt(t.App)) >= dueAfter && (!found || st.turn(t.App).Before(st.turn(pick.App))) {
			pick, found = t, true
		}
	}
	return pick, found
}

func (s Scheduler) appSamples(st *State, now time.Time) []obs.Sample {
	samples := make([]obs.Sample, 0, samplesPerApp*len(s.Targets))
	for _, t := range s.Targets {
		labels := obs.Labels{obs.LabelApp: t.App}
		running := st.Current != nil && st.Current.App == t.App
		samples = append(samples,
			obs.Sample{Name: obs.MetricFullGateAge, Labels: labels, Value: st.age(t.App, now).Seconds()},
			obs.Sample{Name: obs.MetricFullGateRunning, Labels: labels, Value: obs.Bool(running)})
		if r, ok := st.Results[t.App]; ok {
			samples = append(samples, obs.Sample{Name: obs.MetricFullGateVerdict, Labels: labels, Value: obs.Bool(r.Passed)})
		}
	}
	return samples
}
