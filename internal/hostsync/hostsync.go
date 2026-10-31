package hostsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/gatedimage"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	DockerBinary    = gatedimage.DockerBinary
	SystemctlBinary = "systemctl"
	AnalyzeBinary   = "systemd-analyze"
	CaddyBinary     = "caddy"
	DefaultBackups  = "/var/lib/deploy-hub/host-backup"
	tempPattern     = "deploy-hub-host-"
	bundleDir       = "bundle"
	changedFormat   = "changed=%d"
)

var (
	ErrNoSpec   = errors.New("app has no host spec in the gate policy")
	ErrNotGated = gatedimage.ErrNotGated
	ErrNoDigest = gatedimage.ErrNoDigest
)

type Syncer struct {
	Policy   policy.Policy
	Apps     registry.Apps
	Journal  journal.Journal
	Exec     procexec.Runner
	Root     string
	Backups  string
	Restarts string
	Accounts func(owner, group string) (int, int, error)
	Chown    func(f *os.File, uid, gid int) error
	Now      func() time.Time
	Out      *console.Printer
	Log      *slog.Logger
	Metrics  obs.Pusher
}

type target struct {
	app   string
	tag   string
	dir   string
	image string
	spec  policy.Host
}

func (s Syncer) Run(ctx context.Context, app, tag string) error {
	if err := errors.Join(validate.AppName(app), validate.Tag(tag)); err != nil {
		return err
	}
	started := s.Now()
	changed, err := s.sync(ctx, app, tag)
	s.record(app, tag, journal.ActionHostSync, err == nil, started, fmt.Sprintf(changedFormat, changed))
	s.Metrics.Push(context.WithoutCancel(ctx), []obs.Sample{
		{Name: obs.MetricHostSyncSuccess, Labels: obs.Labels{obs.LabelApp: app}, Value: obs.Bool(err == nil)},
		{Name: obs.MetricHostFilesChanged, Labels: obs.Labels{obs.LabelApp: app}, Value: float64(changed)},
	})
	if err != nil {
		s.Log.Error("host sync failed", "app", app, "tag", tag, "error", err.Error())
		return err
	}
	s.Out.Printf("host-sync: %s %s: %d file(s) changed\n", app, tag, changed)
	s.Log.Info("host sync finished", "app", app, "tag", tag, "changed", changed)
	return nil
}

func (s Syncer) record(app, tag, action string, ok bool, started time.Time, extra string) {
	result := journal.ResultOK
	if !ok {
		result = journal.ResultFail
	}
	e := journal.Entry{App: app, Sha7: registry.Sha7(tag), Action: action, Result: result, Seconds: int(s.Now().Sub(started).Seconds()), Extra: extra}
	if err := s.Journal.Append(e); err != nil {
		s.Log.Error("journal append failed", "error", err.Error())
	}
}

func (s Syncer) sync(ctx context.Context, app, tag string) (int, error) {
	t, err := s.resolve(app, tag)
	if err != nil {
		return 0, err
	}
	src := s.source()
	ref, err := src.Pinned(ctx, t.image, t.tag)
	if err != nil {
		return 0, err
	}
	tmp, err := os.MkdirTemp("", tempPattern)
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	bundle := filepath.Join(tmp, bundleDir)
	if err = src.Extract(ctx, ref, policy.HostBundleDir, bundle); err != nil {
		return 0, err
	}
	files, err := s.plan(t, bundle)
	if err != nil {
		return 0, err
	}
	return s.install(ctx, t, files)
}

func (s Syncer) resolve(app, tag string) (target, error) {
	pa, err := s.Policy.App(app)
	if err != nil || pa.Host == nil {
		return target{}, fmt.Errorf("%w: %s", ErrNoSpec, app)
	}
	entry, err := s.Apps.Lookup(app)
	if err != nil {
		return target{}, err
	}
	conf, err := registry.LoadAppConf(entry.Dir)
	if err != nil {
		return target{}, err
	}
	if err := validate.Image(conf.Image); err != nil {
		return target{}, err
	}
	t := target{app: app, tag: tag, dir: entry.Dir, image: conf.Image, spec: *pa.Host}
	return t, s.source().Gated(app, tag)
}

func (s Syncer) source() gatedimage.Source {
	return gatedimage.Source{Exec: s.Exec, Journal: s.Journal, Log: s.Log}
}
