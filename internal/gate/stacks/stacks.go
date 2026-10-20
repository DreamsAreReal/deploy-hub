package stacks

import (
	"context"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
)

const (
	StackGo    = "go"
	goManifest = "go.mod"
	goSource   = ".go"
)

func All() []pack.Pack {
	return []pack.Pack{goPack()}
}

func goPack() pack.Pack {
	return pack.Pack{
		Stack:     StackGo,
		Manifests: []string{goManifest},
		Sources:   []string{goSource},
		Checks: map[string]pack.Run{
			pack.CheckModules:      withModule(golang.Integrity),
			pack.CheckDependencies: withModule(golang.Dependencies),
			pack.CheckLint:         withModule(golang.Lint),
			pack.CheckComplexity:   withModule(golang.Cognitive),
			pack.CheckDeadCode:     withModule(golang.Deadcode),
			pack.CheckTests:        withModule(golang.Tests),
			pack.CheckLineCoverage: withModule(golang.Coverage),
			pack.CheckBranches:     withModule(golang.Branches),
			pack.CheckFuzz:         withModule(golang.Fuzz),
			pack.CheckMutation:     withModule(mutation.Check),
			pack.CheckVulns:        withModule(golang.Vulnerabilities),
		},
		Passed: func(ctx context.Context, ws gate.Workspace) (map[string]bool, error) {
			module, err := golang.Module(ctx, ws)
			if err != nil {
				return nil, err
			}
			ws.Module = module
			return golang.PassedTests(ws)
		},
	}
}

func withModule(run pack.Run) pack.Run {
	return func(ctx context.Context, ws gate.Workspace) gate.Outcome {
		module, err := golang.Module(ctx, ws)
		if err != nil {
			return gate.Fail(err.Error())
		}
		ws.Module = module
		return run(ctx, ws)
	}
}
