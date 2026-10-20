package caddy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/console"

	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	DefaultCaddyfile = "/etc/caddy/Caddyfile"
	DefaultUser      = "caddy"
	caddyBinary      = "caddy"
	systemctlBinary  = "systemctl"
	caddyService     = "caddy"
	adapter          = "caddyfile"
	filePerm         = 0o600
	overrideDigits   = 5
	header           = "# Managed by deploy-hub caddy-sync — do not edit by hand.\n"
	globalBlock      = "{\n\temail %s\n\tgrace_period 10s\n}\n\n"
	extraImport      = "import /etc/caddy/extra/*.caddy\n\n"
	siteBlock        = "%s.%s.sslip.io {\n\tlog\n\treverse_proxy 127.0.0.1:%s {\n\t\tlb_try_duration 30s\n\t\tlb_try_interval 250ms\n\t}\n}\n\n"
	msgUpToDate      = "caddy-sync: Caddyfile already up to date (no reload)"
	msgUpdated       = "caddy-sync: Caddyfile updated and Caddy reloaded"
	candidateSuffix  = ".new"
)

var (
	ErrInvalidConfig = errors.New("caddy-sync: generated Caddyfile failed validation, aborting")
	hostSlug         = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)
	emailAddr        = regexp.MustCompile(`^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9.-]{1,190}$`)
	ErrBadSlug       = errors.New("invalid HOST_SLUG")
	ErrBadEmail      = errors.New("invalid ACME_EMAIL")
)

type Config struct {
	Caddyfile string
	HostSlug  string
	Email     string
	Owner     func(path string) error
}

func (c Config) Validate() error {
	if !hostSlug.MatchString(c.HostSlug) {
		return ErrBadSlug
	}
	if !emailAddr.MatchString(c.Email) {
		return ErrBadEmail
	}
	return nil
}

type Route struct {
	App  string
	Port string
}

func Routes(apps []registry.Entry, listening func(int) bool) []Route {
	var out []Route
	for _, a := range apps {
		conf, err := registry.LoadAppConf(a.Dir)
		if err != nil {
			continue
		}
		port := override(a.Dir, listening)
		if port == "" {
			port = conf.Port
		}
		if validate.Port(port) != nil {
			continue
		}
		out = append(out, Route{App: a.Name, Port: port})
	}
	return out
}

func override(dir string, listening func(int) bool) string {
	data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, registry.CaddyPortFile)))
	if err != nil {
		return ""
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, string(data))
	if len(digits) > overrideDigits {
		digits = digits[:overrideDigits]
	}
	p, err := strconv.Atoi(digits)
	if err != nil || !listening(p) {
		return ""
	}
	return digits
}

func Render(routes []Route, slug, email string) string {
	var b strings.Builder
	b.WriteString(header)
	fmt.Fprintf(&b, globalBlock, email)
	b.WriteString(extraImport)
	for _, r := range routes {
		fmt.Fprintf(&b, siteBlock, r.App, slug, r.Port)
	}
	return b.String()
}

type Syncer struct {
	Config    Config
	Apps      registry.Apps
	Exec      procexec.Runner
	Listening func(int) bool
	Out       *console.Printer
	Log       *slog.Logger
	Metrics   obs.Pusher
}

type syncResult struct {
	reloaded bool
	routes   int
}

func (s Syncer) Run(ctx context.Context) error {
	res, err := s.sync(ctx)
	samples := []obs.Sample{
		{Name: obs.MetricCaddySyncSuccess, Labels: obs.Labels{}, Value: boolValue(err == nil)},
		{Name: obs.MetricCaddyReloads, Labels: obs.Labels{}, Value: boolValue(res.reloaded)},
	}
	if err == nil {
		samples = append(samples, obs.Sample{Name: obs.MetricCaddyRoutes, Labels: obs.Labels{}, Value: float64(res.routes)})
	}
	s.Metrics.Push(ctx, samples)
	return err
}

func boolValue(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

func (s Syncer) sync(ctx context.Context) (syncResult, error) {
	if err := s.Config.Validate(); err != nil {
		return syncResult{}, err
	}
	apps, err := s.Apps.All()
	if err != nil {
		return syncResult{}, err
	}
	routes := Routes(apps, s.Listening)
	content := []byte(Render(routes, s.Config.HostSlug, s.Config.Email))
	if current, rerr := os.ReadFile(s.Config.Caddyfile); rerr == nil && bytes.Equal(current, content) {
		s.Out.Println(msgUpToDate)
		s.Log.Info("caddyfile unchanged", "routes", len(routes))
		return syncResult{routes: len(routes)}, nil
	}
	if err := s.install(ctx, content); err != nil {
		return syncResult{}, err
	}
	if _, err := s.Exec.Run(ctx, procexec.Command{Name: systemctlBinary, Args: []string{"reload", caddyService}}); err != nil {
		s.Log.Error("caddy reload failed", "error", err.Error(), "output", procexec.Output(err))
		return syncResult{}, err
	}
	s.Out.Println(msgUpdated)
	s.Log.Info("caddyfile updated", "routes", len(routes))
	return syncResult{reloaded: true, routes: len(routes)}, nil
}

func (s Syncer) install(ctx context.Context, content []byte) error {
	candidate := s.Config.Caddyfile + candidateSuffix
	if err := os.WriteFile(candidate, content, filePerm); err != nil {
		return err
	}
	err := s.check(ctx, candidate)
	if err == nil {
		err = os.Rename(candidate, s.Config.Caddyfile)
	}
	if err != nil {
		_ = os.Remove(candidate)
	}
	return err
}

func (s Syncer) check(ctx context.Context, path string) error {
	if err := s.Config.Owner(path); err != nil {
		return err
	}
	if _, err := s.Exec.Run(ctx, procexec.Command{Name: caddyBinary, Args: []string{"validate", "--config", path, "--adapter", adapter}}); err != nil {
		s.Log.Error("caddyfile invalid", "error", err.Error(), "output", procexec.Output(err))
		return ErrInvalidConfig
	}
	return nil
}
