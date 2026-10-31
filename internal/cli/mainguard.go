package cli

import (
	"context"
	"net/http"
	"os"
	"path/filepath"

	"github.com/DreamsAreReal/deploy-hub/internal/mainguard"
	"github.com/DreamsAreReal/deploy-hub/internal/preempt"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/selfupdate"
)

const (
	usageMainGuard  = "usage: deploy-hub main-guard   (no arguments; run by deploy-hub-main-guard.timer as root)"
	mainGuardPrefix = "main-guard: "
	mainGuardFile   = "main-guard.json"
)

func runMainGuard(ctx context.Context, a *App, _ procexec.Runner, args []string) int {
	if len(args) > 0 {
		a.Err.Println(usageMainGuard)
		return ExitUsage
	}
	token, err := selfupdate.ReadToken(a.Token, os.Geteuid())
	if err != nil {
		return a.failWith(mainGuardPrefix, err)
	}
	g := mainguard.Guard{
		GitHub: preempt.GitHub{API: selfupdate.DefaultAPI, Owner: a.Config.RepoOwner, Token: token, Client: &http.Client{}},
		Repos:  preempt.RunnerRepos(a.CIRoot), State: filepath.Join(filepath.Dir(a.PreemptState), mainGuardFile),
		Hold: mainguard.DefaultHold, Now: a.Now, Metrics: a.metrics(), Log: a.Log,
	}
	if err := g.Run(ctx); err != nil {
		return a.failWith(mainGuardPrefix, err)
	}
	return ExitOK
}
