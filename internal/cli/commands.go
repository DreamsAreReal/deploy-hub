package cli

import (
	"context"
	"flag"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/accesslog"
	"github.com/DreamsAreReal/deploy-hub/internal/caddy"
	"github.com/DreamsAreReal/deploy-hub/internal/compose"
	"github.com/DreamsAreReal/deploy-hub/internal/docker"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/arch"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/health"
	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/lock"
	"github.com/DreamsAreReal/deploy-hub/internal/netprobe"
	"github.com/DreamsAreReal/deploy-hub/internal/oidc"
	"github.com/DreamsAreReal/deploy-hub/internal/onboard"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/provision"
	"github.com/DreamsAreReal/deploy-hub/internal/prune"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/runner"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
	embedded "github.com/DreamsAreReal/deploy-hub/policy"
)

const (
	flagOnly     = "--only"
	LockWait     = 300 * time.Second
	lockPoll     = time.Second
	renderArgs   = 3
	factsArgs    = 1
	renderedPerm = 0o600
	usageRender  = "usage: deploy-hub render <app> <in_compose> <out_compose>"
	usageFacts   = "usage: deploy-hub facts <app>"
	renderPrefix = "compose-render: "
	msgNoVPSAddr = "onboard: set VPS_ADDR to the VPS address (and VPS_HOST_ALIAS to its root SSH alias)"
)

func (a *App) apps() registry.Apps {
	return registry.Apps{Path: a.Config.Paths.AppsList(), Root: a.Config.Paths.AppsRoot()}
}

func (a *App) journal() journal.Journal {
	return journal.Journal{Path: a.Config.Paths.Journal(), Now: a.Now}
}

func runRunner(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	only := ""
	if len(args) > 1 && args[0] == flagOnly {
		only, args = args[1], args[2:]
	}
	raw := a.Config.SSHCommand
	if raw == "" {
		raw = strings.Join(args, " ")
	}
	svc := a.runnerService(ex)
	svc.Only = only
	return svc.Dispatch(ctx, raw, a.IO.Stdin)
}

func (a *App) runnerService(ex procexec.Runner) *runner.Service {
	d := docker.Client{Exec: ex}
	prober := netprobe.NewProber()
	return &runner.Service{
		Paths:       a.Config.Paths,
		Apps:        a.apps(),
		Journal:     a.journal(),
		Docker:      d,
		Gate:        health.Gate{Docker: d, HTTP: prober, Timeout: a.Config.GateWait, Interval: a.Config.GateTick, Sleep: health.Sleep},
		HTTP:        prober,
		Router:      runner.SudoRouter{Exec: ex, Binary: a.Config.Paths.Binary(), Log: a.Log},
		Locker:      lock.File{Path: a.Config.Paths.Lock(), Wait: LockWait, Poll: lockPoll, Sleep: health.Sleep},
		CacheLocker: lock.File{Path: a.Config.Paths.CacheLock(), Wait: LockWait, Poll: lockPoll, Sleep: health.Sleep},
		LockWait:    LockWait,
		Listening:   netprobe.Listening,
		Now:         a.Now,
		Sleep:       health.Sleep,
		Out:         a.Out,
		Err:         a.Err,
		Log:         a.Log,
		Metrics:     a.metrics(),
		Delivery:    a.delivery(),
		Secrets:     a.secrets(ex),
		Identity: runner.OIDCVerifier{Fetcher: oidc.Fetcher{
			Client: &http.Client{}, URL: a.Config.JWKSURL, Cache: a.Config.JWKSCache, Log: a.Log,
		}},
		Audience:    a.Config.Audience,
		WorkflowRef: a.Config.WorkflowRef(),
		Host:        runner.SudoHost{Exec: ex, Binary: a.Config.Paths.Binary()},
		Catalog:     a.catalog(),
		HostDirs:    arch.HostDirs{Units: policy.UnitDir, Sites: policy.CaddySiteDir, Net: arch.ProcNetDir, Accounts: arch.AccountsDir},
		Visits:      a.visits(),
		HostApps:    a.hostApps(),
	}
}

func runRender(_ context.Context, a *App, _ procexec.Runner, args []string) int {
	if len(args) != renderArgs {
		a.Err.Println(usageRender)
		return ExitUsage
	}
	app, in, out := args[0], args[1], args[2]
	if err := validate.AppName(app); err != nil {
		return a.failWith(renderPrefix, err)
	}
	data, err := os.ReadFile(filepath.Clean(in))
	if err != nil {
		return a.failWith(renderPrefix, err)
	}
	res, err := compose.Render(app, data)
	if err != nil {
		return a.failWith(renderPrefix, err)
	}
	if err := os.WriteFile(out, res.Compose, renderedPerm); err != nil {
		return a.failWith(renderPrefix, err)
	}
	a.Out.Print(res.Metadata())
	a.Log.Info("compose rendered", "app", app, "main", res.Main, "port", res.Port, "health", res.Health)
	return ExitOK
}

func (a *App) failWith(prefix string, err error) int {
	a.Err.Println(prefix + err.Error())
	a.Log.Error("command failed", "error", err.Error())
	return ExitFail
}

func (a *App) syncer(ex procexec.Runner) caddy.Syncer {
	cfg := a.Config.Caddy
	cfg.Owner = a.Owner(a.Config.CaddyUser)
	return caddy.Syncer{Config: cfg, Apps: a.apps(), Exec: ex, Listening: netprobe.Listening, Out: a.Out, Log: a.Log, Metrics: a.metrics()}
}

func runCaddySync(ctx context.Context, a *App, ex procexec.Runner, _ []string) int {
	if err := a.syncer(ex).Run(ctx); err != nil {
		return a.failWith("", err)
	}
	return ExitOK
}

func runPrune(ctx context.Context, a *App, ex procexec.Runner, _ []string) int {
	p := prune.Pruner{Apps: a.apps(), Journal: a.journal(), Images: docker.Client{Exec: ex}, Now: a.Now, Log: a.Log, Metrics: a.metrics()}
	if err := p.Run(ctx); err != nil {
		return a.failWith("prune: ", err)
	}
	return ExitOK
}

func runProvision(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	var r provision.Request
	fs := flag.NewFlagSet(CmdProvision, flag.ContinueOnError)
	fs.SetOutput(a.Err)
	fs.StringVar(&r.App, "app", "", "app name")
	fs.StringVar(&r.Profile, "profile", "", "static|service|bot")
	fs.StringVar(&r.Port, "port", "", "host port on 127.0.0.1")
	fs.StringVar(&r.CPort, "cport", "", "container port")
	fs.StringVar(&r.Mem, "mem", "", "container mem_limit")
	fs.StringVar(&r.HealthPath, "health-path", registry.DefaultHealthPath, "health gate path")
	fs.StringVar(&r.Image, "image", "", "GHCR image without tag")
	fs.StringVar(&r.Repo, "repo", "", "GitHub repository owner/name that deploys the app")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	p := provision.Provisioner{
		Apps:    a.apps(),
		Owner:   a.Owner(a.Config.DeployUser),
		Sync:    a.syncer(ex).Run,
		Out:     a.Out,
		Log:     a.Log,
		Metrics: a.metrics(),
	}
	if err := p.Run(ctx, r); err != nil {
		return a.failWith("provision: ", err)
	}
	return ExitOK
}

func runFacts(_ context.Context, a *App, _ procexec.Runner, args []string) int {
	if len(args) != factsArgs || validate.AppName(args[0]) != nil {
		a.Err.Println(usageFacts)
		return ExitUsage
	}
	f := provision.Gather(a.apps(), a.Config.Paths.AppsRoot(), args[0], provision.ProcNetFiles)
	a.Out.Print(f.Encode())
	return ExitOK
}

func runOnboard(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	if a.Config.VPSAddr == "" {
		a.Err.Println(msgNoVPSAddr)
		return ExitUsage
	}
	o := onboard.Onboarder{
		Config: onboard.Config{
			HostAlias: a.Config.HostAlias, Addr: a.Config.VPSAddr, DeployKey: a.Config.DeployKey,
			HubRepo: a.Config.HubRepo, Owner: a.Config.RepoOwner, RemoteBinary: a.Config.Paths.Binary(),
			DeployUser: a.Config.DeployUser,
		},
		Exec: ex, Out: a.Out, Err: a.Err, Log: a.Log,
	}
	return o.Run(ctx, args)
}

func (a *App) visits() func(context.Context) ([]arch.Visit, error) {
	if a.Config.LogsURL == "" {
		return nil
	}
	reader := accesslog.Reader{URL: a.Config.LogsURL, Domain: caddy.Domain(a.Config.Caddy.HostSlug), Client: &http.Client{}, Addresses: net.InterfaceAddrs}
	return reader.FromServer
}

func (a *App) catalog() *arch.Catalog {
	catalog, err := arch.Parse(embedded.Architecture())
	if err != nil {
		a.Log.Warn("catalog unreadable, no live drift check", "error", err.Error())
		return nil
	}
	return &catalog
}
