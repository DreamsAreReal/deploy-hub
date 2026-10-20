package stacks

import (
	"context"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/dotnet"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/linecov"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/node"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/python"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/scan"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/shell"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/static"
)

const (
	StackGo    = "go"
	goManifest = "go.mod"
	goSource   = ".go"

	requirementsPattern = "requirements*.txt"
)

func All() []pack.Pack {
	return []pack.Pack{goPack(), pythonPack(), nodePack(), dotnetPack(), shellPack(), staticPack()}
}

func staticPack() pack.Pack {
	return pack.Pack{
		Stack:   static.Stack,
		Sources: static.Sources,
		Loose:   true,
		NoCode:  true,
		Checks: map[string]pack.Run{
			pack.CheckModules:      static.NoDeps,
			pack.CheckDependencies: static.NoDeps,
			pack.CheckLint:         static.Lint,
			pack.CheckComplexity:   static.NoCode,
			pack.CheckDeadCode:     static.NoCode,
			pack.CheckTests:        static.NoCode,
			pack.CheckLineCoverage: static.NoCode,
			pack.CheckBranches:     static.NoCode,
			pack.CheckFuzz:         static.NoCode,
			pack.CheckMutation:     static.NoCode,
			pack.CheckVulns:        static.NoDeps,
		},
		Passed: static.Passed,
	}
}

func shellPack() pack.Pack {
	return pack.Pack{
		Stack:   shell.Stack,
		Sources: shell.Sources,
		Loose:   true,
		Checks: map[string]pack.Run{
			pack.CheckModules:      shell.Integrity,
			pack.CheckDependencies: shell.Dependencies,
			pack.CheckLint:         shell.Lint,
			pack.CheckComplexity:   shell.Complexity,
			pack.CheckDeadCode:     shell.DeadCode,
			pack.CheckTests:        shell.Tests,
			pack.CheckLineCoverage: linecov.LineCoverage,
			pack.CheckBranches:     linecov.Branches,
			pack.CheckFuzz:         shell.Fuzz,
			pack.CheckMutation:     shell.Mutation,
			pack.CheckVulns:        shell.Dependencies,
		},
		Passed: shell.Passed,
	}
}

func dotnetPack() pack.Pack {
	return pack.Pack{
		Stack:     dotnet.Stack,
		Manifests: dotnet.Manifests,
		Sources:   dotnet.Sources,
		Outermost: true,
		Checks: map[string]pack.Run{
			pack.CheckModules:      dotnet.Integrity,
			pack.CheckDependencies: dotnet.Dependencies,
			pack.CheckLint:         dotnet.Lint,
			pack.CheckComplexity:   dotnet.Complexity,
			pack.CheckDeadCode:     dotnet.DeadCode,
			pack.CheckTests:        dotnet.Tests,
			pack.CheckLineCoverage: linecov.LineCoverage,
			pack.CheckBranches:     linecov.Branches,
			pack.CheckFuzz:         dotnet.Fuzz,
			pack.CheckMutation:     dotnet.Mutation,
			pack.CheckVulns:        scan.DependencyVulnerabilities,
		},
		Passed: dotnet.Passed,
	}
}

func nodePack() pack.Pack {
	return pack.Pack{
		Stack:     node.Stack,
		Manifests: []string{node.Manifest},
		Sources:   node.Sources,
		Checks: map[string]pack.Run{
			pack.CheckModules:      node.Integrity,
			pack.CheckDependencies: node.Dependencies,
			pack.CheckLint:         node.Lint,
			pack.CheckComplexity:   node.Complexity,
			pack.CheckDeadCode:     node.DeadCode,
			pack.CheckTests:        node.Tests,
			pack.CheckLineCoverage: linecov.LineCoverage,
			pack.CheckBranches:     linecov.Branches,
			pack.CheckFuzz:         node.Fuzz,
			pack.CheckMutation:     node.Mutation,
			pack.CheckVulns:        scan.DependencyVulnerabilities,
		},
		Passed: node.Passed,
	}
}

func pythonPack() pack.Pack {
	return pack.Pack{
		Stack:     python.Stack,
		Manifests: []string{python.ProjectFile, requirementsPattern},
		Sources:   []string{python.Source, python.Stubs},
		Loose:     true,
		Checks: map[string]pack.Run{
			pack.CheckModules:      python.Integrity,
			pack.CheckDependencies: python.Dependencies,
			pack.CheckLint:         python.Lint,
			pack.CheckComplexity:   python.Complexity,
			pack.CheckDeadCode:     python.DeadCode,
			pack.CheckTests:        python.Tests,
			pack.CheckLineCoverage: python.LineCoverage,
			pack.CheckBranches:     python.Branches,
			pack.CheckFuzz:         python.Fuzz,
			pack.CheckMutation:     python.Mutation,
			pack.CheckVulns:        scan.DependencyVulnerabilities,
		},
		Passed: python.Passed,
	}
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
