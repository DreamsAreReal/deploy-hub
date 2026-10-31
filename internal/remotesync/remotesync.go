package remotesync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/console"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/gatedimage"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	DefaultDir     = "/opt/deploy-hub/remotes"
	DefaultBackups = "/var/lib/deploy-hub/remotes"
	DefaultUser    = policy.RemoteUser
	DefaultHealth  = 170 * time.Second
	DefaultVerdict = 5 * time.Minute
	DefaultPoll    = 5 * time.Second
	tempPattern    = "deploy-hub-remote-"
	bundleDir      = "bundle"
	extraFormat    = "target=%s via=%s: %s"
	noHost         = "-"
)

var ErrNoSpec = errors.New("app has no remote spec in the gate policy")

type Syncer struct {
	Policy  policy.Policy
	Apps    registry.Apps
	Journal journal.Journal
	Exec    procexec.Runner
	Courier Deliverer
	Now     func() time.Time
	Out     *console.Printer
	Log     *slog.Logger
}

type job struct {
	app   string
	tag   string
	sha7  string
	image string
	spec  policy.Remote
}

func (s Syncer) Run(ctx context.Context, app, tag string) error {
	if err := errors.Join(validate.AppName(app), validate.Tag(tag)); err != nil {
		return err
	}
	started := s.Now()
	target, out, err := s.sync(ctx, app, tag)
	if err != nil && out.Result == "" {
		out.Result = err.Error()
	}
	s.record(app, tag, err == nil, started, fmt.Sprintf(extraFormat, target, out.Host, oneLine(out.Result)))
	if err != nil {
		s.Log.Error("remote sync failed", "app", app, "tag", tag, "target", target, "error", err.Error())
		return err
	}
	s.Out.Printf("remote-sync: %s %s: %s %s\n", app, tag, target, out.Result)
	s.Log.Info("remote sync finished", "app", app, "tag", tag, "target", target, "via", out.Host)
	return nil
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func (s Syncer) record(app, tag string, ok bool, started time.Time, extra string) {
	result := journal.ResultOK
	if !ok {
		result = journal.ResultFail
	}
	e := journal.Entry{App: app, Sha7: registry.Sha7(tag), Action: journal.ActionRemoteSync, Result: result, Seconds: int(s.Now().Sub(started).Seconds()), Extra: extra}
	if err := s.Journal.Append(e); err != nil {
		s.Log.Error("journal append failed", "error", err.Error())
	}
}

func (s Syncer) sync(ctx context.Context, app, tag string) (string, Outcome, error) {
	pa, err := s.Policy.App(app)
	if err != nil || pa.Remote == nil {
		return "", Outcome{Host: noHost}, fmt.Errorf("%w: %s", ErrNoSpec, app)
	}
	j := job{app: app, tag: tag, sha7: registry.Sha7(tag), spec: *pa.Remote}
	if err = s.resolve(&j); err != nil {
		return j.spec.Target, Outcome{Host: noHost}, err
	}
	tmp, err := os.MkdirTemp("", tempPattern)
	if err != nil {
		return j.spec.Target, Outcome{Host: noHost}, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	sh, err := s.shipment(ctx, j, filepath.Join(tmp, bundleDir))
	if err != nil {
		return j.spec.Target, Outcome{Host: noHost}, err
	}
	out, err := s.Courier.Deliver(ctx, sh)
	return j.spec.Target, out, err
}

func (s Syncer) resolve(j *job) error {
	entry, err := s.Apps.Lookup(j.app)
	if err != nil {
		return err
	}
	conf, err := registry.LoadAppConf(entry.Dir)
	if err != nil {
		return err
	}
	if err := validate.Image(conf.Image); err != nil {
		return err
	}
	j.image = conf.Image
	return s.source().Gated(j.app, j.tag)
}

func (s Syncer) source() gatedimage.Source {
	return gatedimage.Source{Exec: s.Exec, Journal: s.Journal, Log: s.Log}
}
