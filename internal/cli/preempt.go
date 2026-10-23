package cli

import (
	"context"
	"net/http"
	"os"

	"github.com/DreamsAreReal/deploy-hub/internal/preempt"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/selfupdate"
)

const (
	usagePreempt  = "usage: deploy-hub preempt   (no arguments; run by deploy-hub-preempt.timer as root)"
	preemptPrefix = "preempt: "
)

func runPreempt(ctx context.Context, a *App, _ procexec.Runner, args []string) int {
	if len(args) > 0 {
		a.Err.Println(usagePreempt)
		return ExitUsage
	}
	token, err := selfupdate.ReadToken(a.Token, os.Geteuid())
	if err != nil {
		return a.failWith(preemptPrefix, err)
	}
	repos := preempt.RunnerRepos(a.CIRoot)
	p := preempt.Preempter{
		GitHub: preempt.GitHub{API: selfupdate.DefaultAPI, Owner: a.Config.RepoOwner, Token: token, Client: &http.Client{}},
		Repos:  repos, State: a.PreemptState, Keep: preempt.DefaultKeep,
		Now: a.Now, Metrics: a.metrics(), Log: a.Log,
	}
	if err := p.Run(ctx); err != nil {
		return a.failWith(preemptPrefix, err)
	}
	return ExitOK
}
