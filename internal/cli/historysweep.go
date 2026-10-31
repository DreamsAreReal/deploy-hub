package cli

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/historysweep"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/selfupdate"
)

const (
	historySweepPrefix = "history-sweep: "
	usageHistorySweep  = "usage: deploy-hub history-sweep"
	githubTimeout      = 30 * time.Second
)

func runHistorySweep(ctx context.Context, a *App, ex procexec.Runner, args []string) int {
	if len(args) > 0 {
		a.Err.Println(usageHistorySweep)
		return ExitUsage
	}
	targets, err := historysweep.ParseTargets(a.Config.HistorySweep)
	if err != nil {
		return a.failWith(historySweepPrefix, err)
	}
	token, err := selfupdate.ReadToken(a.Token, os.Geteuid())
	if err != nil {
		return a.failWith(historySweepPrefix, err)
	}
	s := historysweep.Sweeper{
		Targets: targets, API: historysweep.DefaultAPI, Git: historysweep.DefaultGit, Token: token,
		Client: &http.Client{Timeout: githubTimeout}, Exec: ex,
		Journal: a.journal(), Metrics: a.metrics(), Now: a.Now, Log: a.Log,
	}
	if err := s.Run(ctx); err != nil {
		return a.failWith(historySweepPrefix, err)
	}
	return ExitOK
}
