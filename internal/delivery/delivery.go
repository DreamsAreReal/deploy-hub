package delivery

import (
	"context"
	"log/slog"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
)

const (
	StepChecks = "checks"
	StepDeploy = "deploy"
	StepLive   = "live"

	StateOK   = "ok"
	StateFail = "fail"

	DefaultTarget = "prod"
	githubHost    = "github.com/"
	hostSeparator = "."
	detailRestore = "rollback"
)

type Step struct {
	Repo   string `json:"repo"`
	Target string `json:"target"`
	SHA    string `json:"sha"`
	Step   string `json:"step"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

type Notifier interface {
	Notify(ctx context.Context, steps []Step)
}

func RepoLabel(repo string) string {
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if repo == "" {
		return ""
	}
	if first, _, _ := strings.Cut(repo, "/"); strings.Contains(first, hostSeparator) {
		return repo
	}
	return githubHost + repo
}

func FromEntry(e journal.Entry, repo string) []Step {
	label := RepoLabel(repo)
	if label == "" || e.Sha7 == "" || e.Sha7 == journal.NoValue {
		return nil
	}
	base := Step{Repo: label, Target: DefaultTarget, SHA: e.Sha7}
	switch {
	case e.Action == journal.ActionGate:
		return []Step{with(base, StepChecks, stateOf(e.Result), "")}
	case e.Action == journal.ActionDeploy || e.Action == journal.ActionRedeploy:
		if e.Result != journal.ResultOK {
			return []Step{with(base, StepDeploy, StateFail, "")}
		}
		return []Step{with(base, StepDeploy, StateOK, ""), with(base, StepLive, StateOK, "")}
	case e.Action == journal.ActionRollback && e.Result == journal.ResultOK:
		return []Step{with(base, StepLive, StateOK, detailRestore)}
	}
	return nil
}

func with(base Step, step, state, detail string) Step {
	base.Step, base.State, base.Detail = step, state, detail
	return base
}

func stateOf(result string) string {
	if result == journal.ResultOK {
		return StateOK
	}
	return StateFail
}

const (
	EventStep = "delivery_step"
	msgStep   = "delivery step"
)

type LogNotifier struct {
	Log *slog.Logger
}

func (n LogNotifier) Notify(ctx context.Context, steps []Step) {
	for _, step := range steps {
		n.Log.InfoContext(ctx, msgStep, "event", EventStep, "repo", step.Repo, "target", step.Target, "sha", step.SHA,
			"step", step.Step, "state", step.State, "detail", step.Detail)
	}
}
