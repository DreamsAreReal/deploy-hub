package cli

import (
	"context"
	"errors"
	"net/http"

	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/selfupdate"
	embedded "github.com/DreamsAreReal/deploy-hub/policy"
)

var Commit = "dev"

const (
	usageSelfUpdate  = "usage: deploy-hub self-update [sha-<40 hex>]"
	selfUpdatePrefix = "self-update: "
	sudoersPrefix    = "sudoers-sync: "
)

var ErrNotInstalled = errors.New("self-update only replaces " + selfupdate.DefaultBinary)

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
	u := selfupdate.Updater{
		Source: selfupdate.Source{API: selfupdate.DefaultAPI, Download: selfupdate.DefaultDownload, Repo: defaultHubRepo, Client: &http.Client{}},
		Binary: selfupdate.DefaultBinary, Current: Commit, Exec: ex, Sudo: sudo,
		Journal: a.journal(), Metrics: a.metrics(), Now: a.Now, Out: a.Out, Log: a.Log,
	}
	if err := u.Run(ctx, tag); err != nil {
		return a.failWith(selfUpdatePrefix, err)
	}
	return ExitOK
}
