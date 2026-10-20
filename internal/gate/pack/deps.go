package pack

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
)

const (
	stampPerm         = 0o600
	ValueDependencies = "dependencies"
	reasonDeps        = "dependencies are not approved"
	unapprovedForm    = "%s is not in the approved dependency list"
)

func Approval(ws gate.Workspace, prefix string, names []string, normalize func(string) string) gate.Outcome {
	approved := map[string]bool{}
	for _, d := range ws.Policy.Dependencies {
		if name, ok := strings.CutPrefix(d, prefix); ok {
			approved[normalize(name)] = true
		}
	}
	var findings []string
	for _, n := range names {
		if !approved[normalize(n)] {
			findings = append(findings, fmt.Sprintf(unapprovedForm, prefix+n))
		}
	}
	return gate.Judge(len(findings) == 0, reasonDeps, map[string]float64{ValueDependencies: float64(len(names))}, findings)
}

func Locked(ctx context.Context, ws gate.Workspace, reason string, findings []string, prepare func(context.Context, gate.Workspace) error) gate.Outcome {
	if len(findings) == 0 {
		if err := prepare(ctx, ws); err != nil {
			findings = append(findings, err.Error())
		}
	}
	return gate.Findings(reason, findings)
}

func Prepared(prepare func(context.Context, gate.Workspace) error) func(Run) Run {
	return func(run Run) Run {
		return func(ctx context.Context, ws gate.Workspace) gate.Outcome {
			if err := prepare(ctx, ws); err != nil {
				return gate.Fail(err.Error())
			}
			return run(ctx, ws)
		}
	}
}

func Once(ctx context.Context, ws gate.Workspace, stamp string, steps ...func(context.Context, gate.Workspace) error) error {
	if _, err := os.Stat(ws.Report(stamp)); err == nil {
		return nil
	}
	for _, step := range steps {
		if err := step(ctx, ws); err != nil {
			return err
		}
	}
	return os.WriteFile(ws.Report(stamp), []byte(stamp), stampPerm)
}
