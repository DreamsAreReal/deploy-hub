package cli

import (
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/selfupdate"
	embedded "github.com/DreamsAreReal/deploy-hub/policy"
	cirunner "github.com/DreamsAreReal/deploy-hub/server/ci-runner"
)

var Commit = "dev"

const (
	usageSelfUpdate  = "usage: deploy-hub self-update [sha-<40 hex>]"
	usageBootstrap   = "usage: deploy-hub bootstrap [sha-<40 hex>]"
	bootstrapPrefix  = "bootstrap: "
	selfUpdatePrefix = "self-update: "
	sudoersPrefix    = "sudoers-sync: "
	runnerSyncPrefix = "ci-runner-sync: "
)

var ErrNotInstalled = errors.New("only the binary installed as " + selfupdate.DefaultBinary + " updates the server")

func runVersion(_ context.Context, a *App, _ procexec.Runner, _ []string) int {
	a.Out.Println(Commit)
	return ExitOK
}

func (a *App) sudoers(ex procexec.Runner) (selfupdate.Sudoers, error) {
	pol, err := policy.Parse(embedded.Gate())
	return selfupdate.Sudoers{Path: a.Sudoers, Binary: selfupdate.DefaultBinary, Hub: pol.Hub, Exec: ex}, err
}

func runSudoersSync(ctx context.Context, a *App, ex procexec.Runner, _ []string) int {
	s, err := a.sudoers(ex)
	if err == nil {
		err = s.Sync(ctx)
	}
	if err != nil {
		return a.failWith(sudoersPrefix, err)
	}
	return ExitOK
}

func runSelfUpdate(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	tag := ""
	if len(args) == 1 {
		tag = args[0]
	}
	if len(args) > 1 || (tag != "" && !selfupdate.ValidTag(tag)) {
		a.Err.Println(usageSelfUpdate)
		return ExitUsage
	}
	if a.Config.Paths.Binary() != selfupdate.DefaultBinary {
		return a.failWith(selfUpdatePrefix, ErrNotInstalled)
	}
	sudo, err := a.sudoers(ex)
	if err != nil {
		return a.failWith(selfUpdatePrefix, err)
	}
	token, err := selfupdate.ReadToken(a.Token, os.Geteuid())
	if err != nil {
		return a.failWith(selfUpdatePrefix, err)
	}
	u := selfupdate.Updater{
		Source: selfupdate.Source{API: selfupdate.DefaultAPI, Repo: defaultHubRepo, Token: token, Client: &http.Client{}},
		Binary: selfupdate.DefaultBinary, Current: Commit, Exec: ex, Sudo: sudo, Runner: a.runnerFiles(ex),
		Journal: a.journal(), Metrics: a.metrics(), Now: a.Now, Out: a.Out, Log: a.Log,
	}
	if err := u.Run(ctx, tag); err != nil {
		return a.failWith(selfUpdatePrefix, err)
	}
	return ExitOK
}

func (a *App) runnerFiles(ex procexec.Runner) selfupdate.RunnerFiles {
	return selfupdate.RunnerFiles{Files: cirunner.Files(), Dir: selfupdate.DefaultRunnerDir, Exec: ex, Runners: selfupdate.Runners{
		Pending: selfupdate.DefaultRestartDir, Wants: selfupdate.DefaultWantsDir, Cgroups: selfupdate.DefaultCgroupRoot,
		Proc: selfupdate.DefaultProcRoot, Exec: ex, Log: a.Log,
	}}
}

func runRunnerSync(ctx context.Context, a *App, ex procexec.Runner, _ []string) int {
	if a.Config.Paths.Binary() != selfupdate.DefaultBinary {
		return a.failWith(runnerSyncPrefix, ErrNotInstalled)
	}
	changed, err := a.runnerFiles(ex).Sync(ctx)
	if err != nil {
		return a.failWith(runnerSyncPrefix, err)
	}
	if changed {
		a.Out.Println(runnerSyncPrefix + "installed " + selfupdate.DefaultRunnerDir)
	}
	return ExitOK
}

func runBootstrap(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	tag := ""
	if len(args) == 1 {
		tag = args[0]
	}
	if len(args) > 1 || (tag != "" && !selfupdate.ValidTag(tag)) {
		a.Err.Println(usageBootstrap)
		return ExitUsage
	}
	b := selfupdate.Bootstrap{
		Source: selfupdate.Source{API: selfupdate.DefaultAPI, Repo: defaultHubRepo, Client: &http.Client{}},
		Host:   a.Config.HostAlias, Exec: ex,
	}
	r, err := b.Run(ctx, tag)
	if err != nil {
		return a.failWith(bootstrapPrefix, err)
	}
	a.Out.Printf("bootstrap: installed %s on %s\n", r.Tag, a.Config.HostAlias)
	return ExitOK
}
