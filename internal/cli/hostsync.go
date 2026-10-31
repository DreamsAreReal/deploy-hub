package cli

import (
	"context"
	"net/http"
	"os"
	"os/user"
	"strconv"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/health"
	"github.com/DreamsAreReal/deploy-hub/internal/hostsync"
	"github.com/DreamsAreReal/deploy-hub/internal/obs"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	embedded "github.com/DreamsAreReal/deploy-hub/policy"
)

const (
	hostSyncArgs   = 2
	usageHostSync  = "usage: deploy-hub host-sync <app> <tag>"
	hostSyncPrefix = "host-sync: "

	usageHostRestart  = "usage: deploy-hub host-restart"
	hostRestartPrefix = "host-restart: "
)

func runHostSync(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	if len(args) != hostSyncArgs {
		a.Err.Println(usageHostSync)
		return ExitUsage
	}
	pol, err := policy.Parse(embedded.Gate())
	if err != nil {
		return a.failWith(hostSyncPrefix, err)
	}
	s := hostsync.Syncer{
		Policy: pol, Apps: a.apps(), Journal: a.journal(), Exec: ex, Backups: hostsync.DefaultBackups, Restarts: a.Restarts,
		Accounts: LookupAccounts, Chown: (*os.File).Chown, Now: a.Now, Out: a.Out, Log: a.Log, Metrics: a.metrics(),
	}
	if err := s.Run(ctx, args[0], args[1]); err != nil {
		return a.failWith(hostSyncPrefix, err)
	}
	return ExitOK
}

func runHostRestart(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	if len(args) != 0 {
		a.Err.Println(usageHostRestart)
		return ExitUsage
	}
	pol, err := policy.Parse(embedded.Gate())
	if err != nil {
		return a.failWith(hostRestartPrefix, err)
	}
	r := hostsync.Restarter{
		Policy: pol, States: a.Restarts, Exec: ex, Journal: a.journal(), Now: a.Now, Sleep: health.Sleep,
		Query: obs.HTTPQuery{URL: obs.QueryURL(a.Config.MetricsURL), Client: &http.Client{}},
		Out:   a.Out, Log: a.Log, Metrics: a.metrics(),
	}
	if err := r.Run(ctx); err != nil {
		return a.failWith(hostRestartPrefix, err)
	}
	return ExitOK
}

func LookupAccounts(owner, group string) (int, int, error) {
	u, err := user.Lookup(owner)
	if err != nil {
		return 0, 0, err
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return 0, 0, err
	}
	uid, uerr := strconv.Atoi(u.Uid)
	gid, gerr := strconv.Atoi(g.Gid)
	if uerr != nil {
		return 0, 0, uerr
	}
	return uid, gid, gerr
}

func (a *App) hostApps() []string {
	return a.appsWhere("host", func(app policy.App) bool { return app.Host != nil })
}

func (a *App) hostOnlyApps() []string {
	return a.appsWhere("host", policy.App.HostOnly)
}

func (a *App) appsWhere(what string, keep func(policy.App) bool) []string {
	pol, err := policy.Parse(embedded.Gate())
	if err != nil {
		a.Log.Error("embedded gate policy unreadable, "+what+" sync disabled", "error", err.Error())
		return nil
	}
	var out []string
	for name, app := range pol.Apps {
		if keep(app) {
			out = append(out, name)
		}
	}
	return out
}
