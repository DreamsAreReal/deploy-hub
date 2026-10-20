package caddy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

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
	header           = "# Managed by deploy-hub caddy-sync — do not edit by hand.\n"
	globalBlock      = "{\n\temail %s\n\tgrace_period 10s\n}\n\n"
	extraImport      = "import /etc/caddy/extra/*.caddy\n\n"
	siteOpen         = "%s.%s.sslip.io {\n\tlog\n\treverse_proxy 127.0.0.1:%s"
	standbyUpstream  = " 127.0.0.1:%s"
	siteClose        = " {\n\t\tlb_policy first\n\t\tlb_try_duration 30s\n\t\tlb_try_interval 250ms\n\t\tfail_duration %s\n\t\tmax_fails 1\n\t}\n}\n\n"
	msgUpToDate      = "caddy-sync: Caddyfile already up to date (no reload)"
	msgUpdated       = "caddy-sync: Caddyfile updated and Caddy reloaded"
	candidateSuffix  = ".new"

	standbyPortOffset = 10000
	maxPort           = 65535

	// FailDuration is how long Caddy keeps an upstream out of rotation after
	// one failed request. Traffic moves between the permanent port and the
	// standby port of a blue-green deploy only through these passive health
	// checks, never through a reload: a reload leaves the previous server
	// alive on open HTTP/2 connections with the previous upstreams (#407).
	FailDuration = 5 * time.Second
	// Settle is how long a blue-green deploy waits after the permanent port
	// is healthy again before it removes the standby, so Caddy has forgotten
	// the failures of the recreate and sends nothing to the standby any more.
	Settle = FailDuration + time.Second
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
	App     string
	Port    string
	Standby string
}

// Routes lists the apps with a port. Each gets the fixed loopback port of
// its blue-green standby, port+10000, as a second upstream, unless that is
// past 65535 or is the port of another app: Caddy must never fail over to
// something that is not this app.
func Routes(apps []registry.Entry) []Route {
	var out []Route
	taken := map[string]bool{}
	for _, a := range apps {
		conf, err := registry.LoadAppConf(a.Dir)
		if err != nil || validate.Port(conf.Port) != nil {
			continue
		}
		out = append(out, Route{App: a.Name, Port: conf.Port})
		taken[conf.Port] = true
	}
	for i, r := range out {
		port, _ := strconv.Atoi(r.Port)
		if standby := strconv.Itoa(port + standbyPortOffset); port+standbyPortOffset <= maxPort && !taken[standby] {
			out[i].Standby = standby
		}
	}
	return out
}

// StandbyPort is the standby port the Caddyfile lists for app, or "" when it
// has none and a deploy has to recreate in place.
func StandbyPort(apps []registry.Entry, app string) string {
	for _, r := range Routes(apps) {
		if r.App == app {
			return r.Standby
		}
	}
	return ""
}

func Render(routes []Route, slug, email string) string {
	var b strings.Builder
	b.WriteString(header)
	fmt.Fprintf(&b, globalBlock, email)
	b.WriteString(extraImport)
	for _, r := range routes {
		fmt.Fprintf(&b, siteOpen, r.App, slug, r.Port)
		if r.Standby != "" {
			fmt.Fprintf(&b, standbyUpstream, r.Standby)
		}
		fmt.Fprintf(&b, siteClose, FailDuration)
	}
	return b.String()
}

type Syncer struct {
	Config  Config
	Apps    registry.Apps
	Exec    procexec.Runner
	Out     *console.Printer
	Log     *slog.Logger
	Metrics obs.Pusher
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
	routes := Routes(apps)
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
