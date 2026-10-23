// Package preempt lets a main deploy overtake pull request gates on the one
// self-hosted runner of a repo. A systemd timer on the hub host runs it: while
// a push run of main waits for the runner, it cancels the repo's active
// pull_request runs; once no main run waits, it re-runs the cancelled ones that
// no newer push of the same pull request replaced. It never cancels anything
// but a pull_request run.
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

// waiting are the statuses of a run that has not got a runner yet.
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

// RunnerRepos lists the repos that have a runner registered on this host.
func RunnerRepos(root string) ([]string, error) {
	found, err := filepath.Glob(filepath.Join(root, "*", runnerMarker))
	repos := make([]string, 0, len(found))
	for _, f := range found {
		repos = append(repos, filepath.Base(filepath.Dir(filepath.Dir(f))))
	}
	return repos, err
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

func (p Preempter) cancel(ctx context.Context, repo string, runs []Run, st *State) (tally, error) {
	var t tally
	var errs []error
	for _, r := range runs {
		if !isPR(r) || !active(r) {
			continue
		}
		// Look again right before cancelling: only a pull_request run that is
		// still active is ever cancelled.
		fresh, err := p.GitHub.Get(ctx, repo, r.ID)
		if err == nil && isPR(fresh) && active(fresh) {
			err = p.GitHub.Cancel(ctx, repo, fresh.ID)
			if err == nil {
				st.cancelled(repo, fresh, p.Now())
				t.cancelled++
				p.Log.Info("pull request gate preempted by a main deploy", "repo", repo, "run", fresh.ID, "branch", fresh.HeadBranch)
			}
		}
		errs = append(errs, err)
	}
	return t, errors.Join(errs...)
}

func superseded(runs []Run, e Entry) bool {
	for _, r := range runs {
		if isPR(r) && r.WorkflowID == e.WorkflowID && r.HeadBranch == e.Branch && r.ID > e.ID {
			return true
		}
	}
	return false
}

func (p Preempter) requeue(ctx context.Context, repo string, runs []Run, st *State) (tally, error) {
	var t tally
	var errs []error
	for i := range st.Runs {
		e := &st.Runs[i]
		if e.Repo != repo || !e.Pending {
			continue
		}
		if superseded(runs, *e) {
			e.Pending = false
			continue
		}
		r, err := p.GitHub.Get(ctx, repo, e.ID)
		switch {
		case err != nil:
			errs = append(errs, err)
		case r.Status != statusCompleted:
		case !isPR(r) || r.Conclusion != conclusionCancel:
			e.Pending = false
		default:
			if err := p.GitHub.Rerun(ctx, repo, e.ID); err != nil {
				errs = append(errs, err)
				continue
			}
			e.Pending, e.Reruns, e.Changed = false, e.Reruns+1, p.Now()
			t.rerun++
			p.Log.Info("pull request gate re-run after a main deploy", "repo", repo, "run", e.ID, "reruns", e.Reruns)
		}
	}
	return t, errors.Join(errs...)
}
