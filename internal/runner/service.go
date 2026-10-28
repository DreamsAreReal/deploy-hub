package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/compose"
	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/delivery"

	"github.com/DreamsAreReal/deploy-hub/internal/docker"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/arch"
	"github.com/DreamsAreReal/deploy-hub/internal/health"
	"github.com/DreamsAreReal/deploy-hub/internal/hub"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/smoke"
)

const (
	ExitOK          = 0
	ExitFail        = 1
	cleanupTimeout  = 30 * time.Second
	historyLines    = 20
	statusProbeTime = 3 * time.Second
	fileMode        = 0o600
)

type Locker interface {
	Acquire(ctx context.Context, onWait func()) (func(), error)
}

type Router interface {
	Sync(ctx context.Context)
}

type Service struct {
	Paths       hub.Paths
	Apps        registry.Apps
	Journal     journal.Journal
	Docker      docker.Client
	Gate        health.Gate
	HTTP        health.HTTPChecker
	Router      Router
	Locker      Locker
	CacheLocker Locker
	LockWait    time.Duration
	Listening   func(int) bool
	Now         func() time.Time
	Sleep       func(context.Context, time.Duration) error
	Out         *console.Printer
	Err         *console.Printer
	Log         *slog.Logger
	Metrics     obs.Pusher
	Identity    Verifier
	Audience    string
	WorkflowRef string
	Delivery    delivery.Notifier
	Secrets     SecretLayer
	Only        string
	Host        HostSyncer
	HostApps    []string
	Catalog     *arch.Catalog
	samples     []obs.Sample
	steps       []delivery.Step
}

type operation struct {
	app   string
	dir   string
	tag   string
	sha7  string
	t0    time.Time
	start time.Time
}

func (s *Service) Dispatch(ctx context.Context, raw string, stdin io.Reader) int {
	defer s.pushMetrics(ctx)
	defer s.pushDeliveries(ctx)
	req, err := ParseRequest(raw)
	if err != nil {
		return s.refuse(raw, refusalMessage(err))
	}
	s.Log.Info("request accepted", "verb", req.Verb, "app", req.App, "tag", req.Tag)
	if req.Verb == VerbStatus {
		s.requested(req.Verb, journal.NoValue)
		return s.status(ctx)
	}
	if s.Only != "" && req.App != s.Only {
		return s.refuse(raw, msgOtherApp)
	}
	entry, err := s.Apps.Lookup(req.App)
	if err != nil {
		return s.refuse(raw, msgUnknownApp)
	}
	s.requested(req.Verb, entry.Name)
	switch req.Verb {
	case VerbHistory:
		return s.history(req.App)
	case VerbDeploy:
		return s.deploy(ctx, raw, entry, req.Tag, stdin)
	case VerbReport:
		return s.report(ctx, raw, entry, req.Tag, stdin)
	case VerbCacheGet:
		return s.cacheGet(ctx, raw, entry, req.Tag, stdin)
	case VerbCachePut:
		return s.cachePut(ctx, raw, entry, req.Tag, stdin)
	case VerbBaselineGet:
		return s.baselineGet(ctx, raw, entry, req.Tag, stdin)
	case VerbBaselinePut:
		return s.baselinePut(ctx, raw, entry, req.Tag, stdin)
	default:
		return s.rollback(ctx, raw, entry, req.Tag)
	}
}

func (s *Service) requested(verb, app string) {
	s.samples = append(s.samples, obs.Sample{Name: obs.MetricRequest, Labels: obs.Labels{obs.LabelVerb: verb, obs.LabelApp: app}, Value: 1})
}

func (s *Service) refuse(raw, msg string) int {
	s.Err.Println(msg)
	reason, verb := refusalReason(msg), verbOf(raw)
	s.Log.Warn("request refused", "reason", msg, "reason_code", reason, "verb", verb, "fingerprint", Fingerprint(raw))
	s.samples = append(s.samples, obs.Sample{Name: obs.MetricRequestRefused, Labels: obs.Labels{obs.LabelVerb: verb, obs.LabelReason: reason}, Value: 1})
	s.record(journal.Entry{App: journal.NoValue, Sha7: journal.NoValue, Action: journal.ActionRefuse, Result: fmt.Sprintf(deniedFormat, Fingerprint(raw))})
	return ExitFail
}

func (s *Service) record(e journal.Entry) {
	if journal.IsContainerOp(e.Action) {
		e.Extra = "smoke=" + smoke.Check(s.Paths.Smoke(), s.Listening)
	}
	if err := s.Journal.Append(e); err != nil {
		s.Log.Error("journal append failed", "error", err.Error())
	}
	s.Log.Info("journal entry", "app", e.App, "sha", e.Sha7, "action", e.Action, "result", e.Result, "seconds", e.Seconds, "extra", e.Extra)
	s.steps = append(s.steps, delivery.FromEntry(e, s.repoOf(e.App))...)
	if e.Action != journal.ActionRefuse {
		s.samples = append(s.samples, obs.Sample{
			Name:   obs.MetricOperationDuration,
			Labels: obs.Labels{obs.LabelApp: e.App, obs.LabelAction: e.Action, obs.LabelResult: e.Result},
			Value:  float64(e.Seconds),
		})
	}
}

func (s *Service) repoOf(app string) string {
	entry, err := s.Apps.Lookup(app)
	if err != nil {
		return ""
	}
	conf, err := registry.LoadAppConf(entry.Dir)
	if err != nil {
		return ""
	}
	return conf.Repo
}

func (s *Service) pushDeliveries(ctx context.Context) {
	if s.Delivery == nil || len(s.steps) == 0 {
		return
	}
	s.Delivery.Notify(context.WithoutCancel(ctx), s.steps)
}

func (s *Service) pushMetrics(ctx context.Context) {
	samples := s.journalSamples()
	s.Metrics.Push(context.WithoutCancel(ctx), append(samples, s.samples...))
}

func (s *Service) journalSamples() []obs.Sample {
	entries, err := s.Journal.Entries()
	readable := obs.Sample{Name: obs.MetricJournalReadable, Labels: obs.Labels{}, Value: obs.Bool(err == nil)}
	if err != nil {
		s.Log.Error("journal read failed, journal counters not pushed", "error", err.Error())
		return []obs.Sample{readable}
	}
	names := s.registeredNames()
	return append(obs.JournalSamples(journal.ForApps(entries, names), names), readable)
}

func (s *Service) registeredNames() []string {
	apps, err := s.Apps.All()
	if err != nil {
		s.Log.Warn("apps.list unreadable", "error", err.Error())
	}
	names := make([]string, 0, len(apps))
	for _, a := range apps {
		names = append(names, a.Name)
	}
	return names
}

func (s *Service) since(t time.Time) int {
	return int(s.Now().Sub(t).Seconds())
}

func (s *Service) acquire(ctx context.Context, app string) (func(), error) {
	t := s.Now()
	release, err := s.Locker.Acquire(ctx, func() { s.Out.Println(msgLockWait) })
	waited := s.Now().Sub(t).Seconds()
	s.samples = append(s.samples, obs.Sample{Name: obs.MetricLockWait, Labels: obs.Labels{obs.LabelApp: app}, Value: waited})
	if err != nil {
		s.Err.Printf(msgLockBusy, s.LockWait)
		s.Log.Error("lock not acquired", "app", app, "error", err.Error(), "waited_seconds", waited)
		return nil, err
	}
	s.Log.Debug("lock acquired", "app", app, "waited_seconds", waited)
	return release, nil
}

func (s *Service) loadConf(dir, app string) (registry.AppConf, error) {
	conf, err := registry.LoadAppConf(dir)
	if err != nil {
		s.Err.Printf("%s (%s)\n", msgNoConf, app)
		s.Log.Error("app.conf missing", "app", app, "dir", dir, "error", err.Error())
	}
	return conf, err
}

func composePath(dir string) string {
	return filepath.Join(dir, registry.ComposeFile)
}

func (s *Service) container(ctx context.Context, app, dir string, conf registry.AppConf) string {
	if conf.IsBYOC() {
		return app
	}
	return s.Docker.ComposeContainer(ctx, composePath(dir), docker.MainService)
}

func (s *Service) composeUp(ctx context.Context, dir string, conf registry.AppConf, ref string) {
	opts := docker.UpOptions{File: composePath(dir), Image: ref, Pull: docker.PullNever, Services: []string{docker.MainService}}
	if conf.IsBYOC() {
		opts.Pull, opts.Services = docker.PullMissing, nil
	}
	if err := s.Docker.ComposeUp(ctx, opts); err != nil {
		s.Log.Warn("compose up failed", "dir", dir, "image", ref, "error", err.Error())
	}
}

func (s *Service) upSidecars(ctx context.Context, dir string, conf registry.AppConf, ref string) {
	if !conf.IsBYOC() {
		return
	}
	err := s.Docker.ComposeUp(ctx, docker.UpOptions{File: composePath(dir), Image: ref, Pull: docker.PullMissing, NoRecreate: true})
	if err != nil {
		s.Log.Warn("sidecars up failed", "dir", dir, "error", err.Error())
	}
	data, _ := os.ReadFile(composePath(dir))
	sidecars := compose.Sidecars(data, conf.Main)
	if len(sidecars) == 0 {
		return
	}
	err = s.Docker.ComposeUp(ctx, docker.UpOptions{File: composePath(dir), Image: ref, Pull: docker.PullMissing, NoDeps: true, Services: sidecars})
	if err != nil {
		s.Log.Warn("changed sidecars not recreated", "dir", dir, "error", err.Error())
	}
}

func targetFor(conf registry.AppConf) health.Target {
	return health.Target{Mode: conf.HealthMode(), Port: conf.Port, Path: conf.HealthPath}
}

func (s *Service) gateMain(ctx context.Context, op *operation, conf registry.AppConf, t health.Target) bool {
	t.Container = s.container(ctx, op.app, op.dir, conf)
	started := s.Now()
	ok := s.Gate.Wait(ctx, t)
	s.samples = append(s.samples, obs.Sample{
		Name:   obs.MetricHealthGateDuration,
		Labels: obs.Labels{obs.LabelApp: op.app, obs.LabelTarget: targetMain, obs.LabelResult: resultOf(ok)},
		Value:  s.Now().Sub(started).Seconds(),
	})
	s.Log.Info("health gate finished", "app", op.app, "target", targetMain, "mode", t.Mode, "healthy", ok)
	return ok
}

func resultOf(ok bool) string {
	if ok {
		return journal.ResultOK
	}
	return journal.ResultFail
}

func ensureFile(path string) error {
	f, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_WRONLY, fileMode)
	if err != nil {
		return err
	}
	return f.Close()
}

func (s *Service) appendLastError(app, out string) {
	f, err := os.OpenFile(s.Paths.LastError(), os.O_APPEND|os.O_WRONLY|os.O_CREATE, fileMode)
	if err != nil {
		s.Log.Warn("last-error.log not writable", "error", err.Error())
		return
	}
	line := fmt.Sprintf(lastErrorFormat, s.Now().UTC().Format(journal.TimeLayout), app, strings.TrimSpace(out))
	_, werr := f.WriteString(line)
	if err := errors.Join(werr, f.Close()); err != nil {
		s.Log.Warn("last-error.log write failed", "error", err.Error())
	}
}

func FormatDuration(d time.Duration) string {
	secs := int(d.Seconds())
	const minute = 60
	if secs >= minute {
		return fmt.Sprintf("%dm%02ds", secs/minute, secs%minute)
	}
	return fmt.Sprintf("%ds", secs)
}
