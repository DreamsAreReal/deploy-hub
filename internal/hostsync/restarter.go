package hostsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/lock"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	IdleWindow    = "2m"
	PollEvery     = 10 * time.Second
	verbRestart   = "restart"
	busyQuery     = "max(max_over_time(%s[%s]))"
	versionQuery  = `max(%s{version=~"%s.*"})`
	unitExtra     = "unit=%s"
	reportedValue = 1
)

var (
	ErrNotReported = errors.New("the restarted unit did not report the new version in time, previous files restored")
	ErrNoPrevious  = errors.New("no previous copy to restore")
)

type Querier interface {
	Scalar(ctx context.Context, expr string) (float64, bool, error)
}

type Restarter struct {
	Policy  policy.Policy
	States  string
	Root    string
	Exec    procexec.Runner
	Query   Querier
	Journal journal.Journal
	Now     func() time.Time
	Sleep   func(ctx context.Context, d time.Duration) error
	Out     *console.Printer
	Log     *slog.Logger
	Metrics obs.Pusher
}

func (r Restarter) Run(ctx context.Context) error {
	names := make([]string, 0, len(r.Policy.Apps))
	for name, app := range r.Policy.Apps {
		if app.Host != nil && app.Host.Restart != nil {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	errs := make([]error, 0, len(names))
	for _, name := range names {
		errs = append(errs, r.app(ctx, name, *r.Policy.Apps[name].Host))
	}
	return errors.Join(errs...)
}

func (r Restarter) app(ctx context.Context, app string, h policy.Host) error {
	release, err := LockState(ctx, r.States, app, 0)
	if errors.Is(err, lock.ErrTimeout) {
		r.Log.Info("host restart skipped, a host-sync holds the state", "app", app)
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	st, err := LoadState(r.States, app)
	if err != nil {
		return err
	}
	r.pushPending(ctx, app, st)
	if st.Pending == nil {
		return nil
	}
	idle, err := r.idle(ctx, *h.Restart)
	if err != nil || !idle {
		r.Log.Info("host restart waits for an idle unit", "app", app, "tag", st.Pending.Tag, "unit", h.Restart.Unit, "error", errText(err))
		return err
	}
	return r.restart(ctx, app, h, st)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (r Restarter) pushPending(ctx context.Context, app string, st RestartState) {
	waited := 0.0
	if st.Pending != nil {
		waited = r.Now().Sub(st.Pending.Since).Seconds()
	}
	r.Metrics.Push(context.WithoutCancel(ctx), []obs.Sample{{Name: obs.MetricHostRestartPending, Labels: obs.Labels{obs.LabelApp: app}, Value: waited}})
}

func (r Restarter) idle(ctx context.Context, spec policy.HostRestart) (bool, error) {
	v, ok, err := r.Query.Scalar(ctx, fmt.Sprintf(busyQuery, spec.Busy, IdleWindow))
	return err == nil && ok && v == 0, err
}

func (r Restarter) restart(ctx context.Context, app string, h policy.Host, st RestartState) error {
	started := r.Now()
	p := *st.Pending
	err := r.cycle(ctx, *h.Restart, p.Tag)
	st.Installed = p.Tag
	if err != nil {
		r.Log.Error("host restart failed, restoring the previous files", "app", app, "tag", p.Tag, "error", err.Error())
		err = errors.Join(err, r.rollback(ctx, h, p))
		st.Installed = p.PrevTag
	}
	st.Pending = nil
	err = errors.Join(err, SaveState(r.States, app, st))
	r.finish(ctx, app, p.Tag, h.Restart.Unit, started, err)
	return err
}

func (r Restarter) finish(ctx context.Context, app, tag, unit string, started time.Time, err error) {
	result := journal.ResultOK
	if err != nil {
		result = journal.ResultFail
	}
	e := journal.Entry{App: app, Sha7: registry.Sha7(tag), Action: journal.ActionHostRestart, Result: result, Seconds: int(r.Now().Sub(started).Seconds()), Extra: fmt.Sprintf(unitExtra, unit)}
	if jerr := r.Journal.Append(e); jerr != nil {
		r.Log.Error("journal append failed", "error", jerr.Error())
	}
	labels := obs.Labels{obs.LabelApp: app}
	r.Metrics.Push(context.WithoutCancel(ctx), []obs.Sample{
		{Name: obs.MetricHostRestartSuccess, Labels: labels, Value: obs.Bool(err == nil)},
		{Name: obs.MetricHostRestartPending, Labels: labels, Value: 0},
	})
	if err == nil {
		r.Out.Printf("host-restart: %s %s: %s restarted\n", app, tag, unit)
		r.Log.Info("host restart finished", "app", app, "tag", tag, "unit", unit)
	}
}

func (r Restarter) cycle(ctx context.Context, spec policy.HostRestart, tag string) error {
	if _, err := r.Exec.Run(ctx, procexec.Command{Name: SystemctlBinary, Args: []string{verbRestart, spec.Unit}}); err != nil {
		return fmt.Errorf("%s %s %s: %w: %s", SystemctlBinary, verbRestart, spec.Unit, err, procexec.Output(err))
	}
	return r.verify(ctx, spec, tag)
}

func (r Restarter) verify(ctx context.Context, spec policy.HostRestart, tag string) error {
	expr := fmt.Sprintf(versionQuery, spec.Version, registry.Sha7(tag))
	deadline := r.Now().Add(spec.Within)
	for {
		v, ok, err := r.Query.Scalar(ctx, expr)
		if err == nil && ok && v == reportedValue {
			return nil
		}
		if !r.Now().Before(deadline) {
			return fmt.Errorf("%w: %s within %s", ErrNotReported, expr, spec.Within)
		}
		if err := r.Sleep(ctx, PollEvery); err != nil {
			return err
		}
	}
}

func (r Restarter) rollback(ctx context.Context, h policy.Host, p Pending) error {
	ctx = context.WithoutCancel(ctx)
	for _, f := range h.Kept() {
		if err := r.restorePrev(f, p.PrevTag); err != nil {
			return err
		}
	}
	if p.PrevTag == "" {
		_, err := r.Exec.Run(ctx, procexec.Command{Name: SystemctlBinary, Args: []string{verbRestart, h.Restart.Unit}})
		return err
	}
	return r.cycle(ctx, *h.Restart, p.PrevTag)
}

func (r Restarter) restorePrev(f policy.HostFile, prevTag string) error {
	dst := Syncer{Root: r.Root}.hostPath(f.Dst)
	prev := PrevName(dst, prevTag)
	snap, existed, err := current(prev, f)
	if err == nil && !existed {
		err = fmt.Errorf("%w: %s", ErrNoPrevious, prev)
	}
	if err != nil {
		return err
	}
	return Syncer{Chown: (*os.File).Chown}.writeFile(dst, snap)
}
