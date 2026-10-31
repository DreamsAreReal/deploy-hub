package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/remotesync"
	"github.com/DreamsAreReal/deploy-hub/internal/runner"
	embedded "github.com/DreamsAreReal/deploy-hub/policy"
)

const (
	remoteSyncArgs     = 2
	remoteDeliverArgs  = 3
	maxShipment        = 32 << 20
	usageRemoteSync    = "usage: deploy-hub remote-sync <app> <tag>"
	usageRemoteDeliver = "usage: deploy-hub remote-deliver <target> <sha7> <bundled|installed> < bundle.tar"
	remoteSyncPrefix   = "remote-sync: "
	remoteDeliverPfx   = "remote-deliver: "
)

func runRemoteSync(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	if len(args) != remoteSyncArgs {
		a.Err.Println(usageRemoteSync)
		return ExitUsage
	}
	pol, err := policy.Parse(embedded.Gate())
	if err != nil {
		return a.failWith(remoteSyncPrefix, err)
	}
	s := a.remoteSyncer(ex, pol)
	if err := s.Run(ctx, args[0], args[1]); err != nil {
		return a.failWith(remoteSyncPrefix, err)
	}
	return ExitOK
}

func (a *App) remoteSyncer(ex procexec.Runner, pol policy.Policy) remotesync.Syncer {
	return remotesync.Syncer{
		Policy: pol, Apps: a.apps(), Journal: a.journal(), Exec: ex, Now: a.Now, Out: a.Out, Log: a.Log,
		Courier: remotesync.SudoCourier{Exec: ex, Binary: a.Config.Paths.Binary(), User: remotesync.DefaultUser},
	}
}

func runRemoteDeliver(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	if len(args) != remoteDeliverArgs {
		a.Err.Println(usageRemoteDeliver)
		return ExitUsage
	}
	archive, err := io.ReadAll(io.LimitReader(a.IO.Stdin, maxShipment+1))
	if err == nil && len(archive) > maxShipment {
		err = fmt.Errorf("bundle is larger than %d bytes", maxShipment)
	}
	if err != nil {
		return a.failWith(remoteDeliverPfx, err)
	}
	c := remotesync.Courier{
		Exec: ex, Dir: remotesync.DefaultDir, Backups: remotesync.DefaultBackups, Now: a.Now, Log: a.Log,
		HealthWait: remotesync.DefaultHealth, VerdictWait: remotesync.DefaultVerdict, Poll: remotesync.DefaultPoll, Sleep: time.Sleep,
	}
	out, err := c.Deliver(ctx, remotesync.Shipment{Target: args[0], Sha7: args[1], Engine: args[2], Archive: archive})
	a.Out.Print(out.Format())
	if err != nil {
		return a.failWith(remoteDeliverPfx, err)
	}
	return ExitOK
}

func (a *App) remote(ex procexec.Runner) runner.RemoteSyncer {
	pol, err := policy.Parse(embedded.Gate())
	if err != nil {
		return nil
	}
	return a.remoteSyncer(ex, pol)
}

func (a *App) remoteApps() []string {
	return a.appsWhere("remote", func(app policy.App) bool { return app.Remote != nil })
}
