package preempt

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/obs"
)

const (
	EventPullRequest = "pull_request"
	eventPush        = "push"
	mainBranch       = "main"
	statusCompleted  = "completed"
	statusInProgress = "in_progress"
	conclusionCancel = "cancelled"
	runnerMarker     = "runner/.runner"
	DefaultCIRoot    = "/var/lib/ci"
	DefaultState     = "/var/lib/deploy-hub/preempt.json"
	DefaultKeep      = 24 * time.Hour
	labelRepo        = "repo"
)

var waiting = map[string]bool{"queued": true, "waiting": true, "pending": true, "requested": true}

type Preempter struct {
	GitHub  GitHub
	Repos   []string
	State   string
	Keep    time.Duration
	Now     func() time.Time
	Metrics obs.Pusher
	Log     *slog.Logger
}

type tally struct {
	cancelled int
	rerun     int
}

func isPR(r Run) bool {
	return r.Event == EventPullRequest
}

func active(r Run) bool {
	return waiting[r.Status] || r.Status == statusInProgress
}

func mainWaits(runs []Run) bool {
	for _, r := range runs {
		if r.Event == eventPush && r.HeadBranch == mainBranch && waiting[r.Status] {
			return true
		}
	}
	return false
}

func RunnerRepos(root string) []string {
	found, _ := filepath.Glob(filepath.Join(root, "*", runnerMarker))
	repos := make([]string, 0, len(found))
	for _, f := range found {
		repos = append(repos, filepath.Base(filepath.Dir(filepath.Dir(f))))
	}
	return repos
}

func (p Preempter) Run(ctx context.Context) error {
	st, err := Load(p.State)
	if err != nil {
		return err
	}
	samples := make([]obs.Sample, 0, 3*len(p.Repos)+1)
	var errs []error
	for _, repo := range p.Repos {
		t, rerr := p.repo(ctx, repo, &st)
		errs = append(errs, rerr)
		labels := obs.Labels{labelRepo: repo}
		samples = append(samples,
			obs.Sample{Name: obs.MetricPreemptCancelled, Labels: labels, Value: float64(t.cancelled)},
			obs.Sample{Name: obs.MetricPreemptRerun, Labels: labels, Value: float64(t.rerun)},
			obs.Sample{Name: obs.MetricPreemptRerunsMax, Labels: labels, Value: float64(st.maxReruns(repo))})
	}
	st.prune(p.Now().Add(-p.Keep))
	errs = append(errs, st.Save(p.State))
	err = errors.Join(errs...)
	p.Metrics.Push(context.WithoutCancel(ctx), append(samples,
		obs.Sample{Name: obs.MetricPreemptSuccess, Labels: obs.Labels{}, Value: obs.Bool(err == nil)}))
	return err
}

func (p Preempter) repo(ctx context.Context, repo string, st *State) (tally, error) {
	runs, err := p.GitHub.Recent(ctx, repo)
	if err != nil {
		return tally{}, err
	}
	if mainWaits(runs) {
		return p.cancel(ctx, repo, runs, st)
	}
	return p.requeue(ctx, repo, runs, st)
}

func superseded(runs []Run, e Entry) bool {
	for _, r := range runs {
		if isPR(r) && r.WorkflowID == e.WorkflowID && r.HeadBranch == e.Branch && r.ID > e.ID {
			return true
		}
	}
	return false
}

func (p Preempter) cancel(ctx context.Context, repo string, runs []Run, st *State) (tally, error) {
	var t tally
	var errs []error
	for _, r := range runs {
		if !isPR(r) || !active(r) {
			continue
		}
		done, err := p.cancelOne(ctx, repo, r.ID, st)
		if done {
			t.cancelled++
		}
		errs = append(errs, err)
	}
	return t, errors.Join(errs...)
}

func (p Preempter) cancelOne(ctx context.Context, repo string, id int64, st *State) (bool, error) {
	fresh, err := p.GitHub.Get(ctx, repo, id)
	if err != nil || !isPR(fresh) || !active(fresh) {
		return false, err
	}
	if err := p.GitHub.Cancel(ctx, repo, fresh.ID); err != nil {
		return false, err
	}
	st.cancelled(repo, fresh, p.Now())
	p.Log.Info("pull request gate preempted by a main deploy", "repo", repo, "run", fresh.ID, "branch", fresh.HeadBranch)
	return true, nil
}

func (p Preempter) requeue(ctx context.Context, repo string, runs []Run, st *State) (tally, error) {
	var t tally
	var errs []error
	for i := range st.Runs {
		e := &st.Runs[i]
		if e.Repo != repo || !e.Pending {
			continue
		}
		rerun, err := p.settle(ctx, repo, runs, e)
		if rerun {
			t.rerun++
		}
		errs = append(errs, err)
	}
	return t, errors.Join(errs...)
}

func (p Preempter) settle(ctx context.Context, repo string, runs []Run, e *Entry) (bool, error) {
	if superseded(runs, *e) {
		e.Pending = false
		return false, nil
	}
	r, err := p.GitHub.Get(ctx, repo, e.ID)
	if err != nil || r.Status != statusCompleted {
		return false, err
	}
	if !isPR(r) || r.Conclusion != conclusionCancel {
		e.Pending = false
		return false, nil
	}
	if err := p.GitHub.Rerun(ctx, repo, e.ID); err != nil {
		return false, err
	}
	e.Pending, e.Reruns, e.Changed = false, e.Reruns+1, p.Now()
	p.Log.Info("pull request gate re-run after a main deploy", "repo", repo, "run", e.ID, "reruns", e.Reruns)
	return true, nil
}
