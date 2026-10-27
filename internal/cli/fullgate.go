package cli

import (
	"context"
	"net/http"
	"os"

	"github.com/DreamsAreReal/deploy-hub/internal/fullgate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/preempt"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/selfupdate"
	embedded "github.com/DreamsAreReal/deploy-hub/policy"
)

const (
	usageFullGate  = "usage: deploy-hub full-gate   (no arguments; run by deploy-hub-full-gate.timer as root)"
	fullGatePrefix = "full-gate: "
)

func runFullGate(ctx context.Context, a *App, _ procexec.Runner, args []string) int {
	if len(args) > 0 {
		a.Err.Println(usageFullGate)
		return ExitUsage
	}
	pol, err := policy.Parse(embedded.Gate())
	if err != nil {
		return a.failWith(fullGatePrefix, err)
	}
	token, err := selfupdate.ReadToken(a.Token, os.Geteuid())
	if err != nil {
		return a.failWith(fullGatePrefix, err)
	}
	owner := a.Config.RepoOwner
	s := fullgate.Scheduler{
		Actions: preempt.GitHub{API: selfupdate.DefaultAPI, Owner: owner, Token: token, Client: &http.Client{}},
		Targets: fullgate.Targets(pol.Apps, owner, preempt.RunnerRepos(a.CIRoot)), Owner: owner,
		State: a.FullGateState, Now: a.Now, Metrics: a.metrics(), Delivery: a.delivery(), Log: a.Log,
	}
	if err := s.Run(ctx); err != nil {
		return a.failWith(fullGatePrefix, err)
	}
	return ExitOK
}
