package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/DreamsAreReal/deploy-hub/internal/console"

	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
)

const (
	dirPerm        = 0o750
	filePerm       = 0o600
	msgDirCreated  = "  %s: created\n"
	msgWritten     = "  %s: written\n"
	msgEnvCreated  = "  %s: created (600, empty)\n"
	msgListAdded   = "  apps.list: added '%s %s'\n"
	msgSyncSkipped = "  caddy-sync: skipped (see logs)"
	msgDone        = "provision: ok (%s -> %s)\n"
)

const (
	composeHead  = "services:\n  app:\n    image: ${DEPLOY_IMAGE:?set by deploy runner, see .deploy-state}\n    container_name: %s\n    restart: unless-stopped\n    mem_limit: %s\n    stop_grace_period: 30s\n"
	composePorts = "    ports:\n      - \"127.0.0.1:%s:%s\"\n"
	composeTail  = "    env_file: .env\n    logging:\n      driver: json-file\n      options:\n        max-size: \"10m\"\n        max-file: \"3\"\n"
)

func RenderCompose(r Request) []byte {
	d := r.templateData()
	out := fmt.Sprintf(composeHead, d.App, d.Mem)
	if d.Port != "" {
		out += fmt.Sprintf(composePorts, d.Port, d.CPort)
	}
	return []byte(out + composeTail)
}

type Provisioner struct {
	Apps    registry.Apps
	Owner   func(path string) error
	Sync    func(ctx context.Context) error
	Out     *console.Printer
	Log     *slog.Logger
	Metrics obs.Pusher
}

type step struct {
	path   string
	msg    string
	create func(path string) error
}

func (p Provisioner) Run(ctx context.Context, r Request) error {
	if err := r.Validate(); err != nil {
		return err
	}
	dir := p.Apps.DirFor(r.App)
	steps := []step{
		{path: dir, msg: msgDirCreated, create: p.mkdir},
		{path: filepath.Join(dir, registry.ComposeFile), msg: msgWritten, create: writer(RenderCompose(r))},
		{path: filepath.Join(dir, registry.AppConfFile), msg: msgWritten, create: writer([]byte(r.conf().Encode()))},
		{path: filepath.Join(dir, registry.EnvFile), msg: msgEnvCreated, create: writer(nil)},
	}
	var created []string
	for _, s := range steps {
		if err := p.ensure(s, &created); err != nil {
			return err
		}
	}
	if err := p.register(r.App, dir, &created); err != nil {
		return err
	}
	if err := p.Sync(ctx); err != nil {
		p.Out.Println(msgSyncSkipped)
		p.Log.Warn("caddy-sync after provision failed", "app", r.App, "error", err.Error())
	}
	p.Out.Printf(msgDone, r.App, dir)
	p.Log.Info("app provisioned", "app", r.App, "dir", dir, "created", created, "profile", r.Profile)
	p.Metrics.Push(ctx, []obs.Sample{{Name: obs.MetricProvisionedResources, Labels: obs.Labels{obs.LabelApp: r.App}, Value: float64(len(created))}})
	return nil
}

func (p Provisioner) ensure(s step, created *[]string) error {
	if _, err := os.Stat(s.path); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := s.create(s.path); err != nil {
		return err
	}
	if err := p.Owner(s.path); err != nil {
		return err
	}
	p.Out.Printf(s.msg, s.path)
	*created = append(*created, s.path)
	return nil
}

func (p Provisioner) mkdir(path string) error {
	return os.MkdirAll(path, dirPerm)
}

func writer(data []byte) func(string) error {
	return func(path string) error {
		return os.WriteFile(path, data, filePerm)
	}
}

func (p Provisioner) register(app, dir string, created *[]string) error {
	if _, err := p.Apps.Lookup(app); !errors.Is(err, registry.ErrUnknownApp) {
		return err
	}
	if err := p.Apps.Add(registry.Entry{Name: app, Dir: dir}); err != nil {
		return err
	}
	p.Out.Printf(msgListAdded, app, dir)
	*created = append(*created, p.Apps.Path)
	return nil
}
