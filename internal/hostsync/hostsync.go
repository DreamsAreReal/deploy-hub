package hostsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	DockerBinary    = "docker"
	SystemctlBinary = "systemctl"
	AnalyzeBinary   = "systemd-analyze"
	CaddyBinary     = "caddy"
	DefaultBackups  = "/var/lib/deploy-hub/host-backup"
	tempPattern     = "deploy-hub-host-"
	bundleDir       = "bundle"
	changedFormat   = "changed=%d"
	digestSeparator = "@"
)

var (
	ErrNoSpec   = errors.New("app has no host spec in the gate policy")
	ErrNotGated = errors.New("commit has no passing gate verdict in the journal")
	ErrNoDigest = errors.New("image was not pulled by digest from the app's registry")
)

type Syncer struct {
	Policy   policy.Policy
	Apps     registry.Apps
	Journal  journal.Journal
	Exec     procexec.Runner
	Root     string
	Backups  string
	Accounts func(owner, group string) (int, int, error)
	Chown    func(path string, uid, gid int) error
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
	ref, err := s.pinned(ctx, t)
	if err != nil {
		return 0, err
	}
	tmp, err := os.MkdirTemp("", tempPattern)
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	bundle, err := s.extract(ctx, ref, tmp)
	if err != nil {
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
	return t, s.gated(t)
}

func (s Syncer) gated(t target) error {
	entries, err := s.Journal.Entries()
	if err != nil {
		return err
	}
	sha7 := registry.Sha7(t.tag)
	for _, e := range journal.ForApp(entries, t.app) {
		if e.Sha7 == sha7 && e.Action == journal.ActionGate && e.Result == journal.ResultOK {
			return nil
		}
	}
	return fmt.Errorf("%w: %s@%s", ErrNotGated, t.app, sha7)
}

func (s Syncer) pinned(ctx context.Context, t target) (string, error) {
	res, err := s.Exec.Run(ctx, procexec.Command{Name: DockerBinary, Args: []string{"image", "inspect", "--format", "{{json .RepoDigests}}", t.image + ":" + t.tag}})
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrNoDigest, procexec.Output(err))
	}
	var digests []string
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &digests); jerr != nil {
		return "", fmt.Errorf("%w: %s", ErrNoDigest, jerr.Error())
	}
	for _, d := range digests {
		repo, digest, _ := strings.Cut(d, digestSeparator)
		if repo == t.image && validate.Digest(digest) == nil {
			return d, nil
		}
	}
	return "", fmt.Errorf("%w: %s:%s", ErrNoDigest, t.image, t.tag)
}
