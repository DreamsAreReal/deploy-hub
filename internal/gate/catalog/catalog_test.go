package catalog

import (
	"context"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func TestChecksAreUniqueAndOrdered(t *testing.T) {
	seen := map[string]bool{}
	names := make([]string, 0, len(Checks()))
	for _, c := range Checks() {
		testutil.True(t, !seen[c.Name] && c.Run != nil, "unique runnable check "+c.Name)
		seen[c.Name] = true
		names = append(names, c.Name)
	}
	testutil.Equal(t, names[0], "registration")
	index := map[string]int{}
	for i, n := range names {
		index[n] = i
	}
	testutil.True(t, index["tests"] < index["line-coverage"] && index["line-coverage"] < index["mutation"] && index["tests"] < index["scenarios"], "report producers run first")
	testutil.True(t, index["dast"] < index["mutation"] && index["load"] < index["mutation"], "the stage runs before the long mutation check")
	for _, c := range Checks() {
		testutil.Equal(t, c.Long, c.Name == "mutation")
	}
	for _, required := range []string{"comments", "markers", "language", "hygiene", "lint", "cognitive-complexity", "duplication", "dead-code", "branch-coverage", "fuzz",
		"secrets-gitleaks", "secrets-trufflehog", "sast", "vulnerabilities", "sca", "sbom-licenses", "workflows", "dockerfile", "iac", "image", "dast", "load"} {
		testutil.True(t, seen[required], "missing check "+required)
	}
}

func TestChecksWaitOnlyForEarlierChecks(t *testing.T) {
	checks := Checks()
	index := map[string]int{}
	for i, c := range checks {
		index[c.Name] = i
	}
	for i, c := range checks {
		for _, name := range c.After {
			j, ok := index[name]
			testutil.True(t, ok && j < i, c.Name+" waits for an earlier check "+name)
		}
	}
	testutil.Equal(t, len(checks[0].After), 0)
	testutil.Equal(t, checks[1].After, []string{"registration"})
	testutil.Equal(t, checks[index[pack.CheckLint]].After, []string{pack.CheckDependencies})
	testutil.Equal(t, checks[index[pack.CheckLineCoverage]].After, []string{pack.CheckDependencies, pack.CheckTests})
	testutil.Equal(t, checks[index[checkLoad]].After, []string{pack.CheckDependencies, checkDAST})
}

func TestMutationRunsOnlyInFullRunsWhenThePolicySkipsDeploys(t *testing.T) {
	ran := 0
	check := fullRunsOnly(gate.Check{Name: pack.CheckMutation, Run: func(context.Context, gate.Workspace) gate.Outcome {
		ran++
		return gate.Pass(nil)
	}})
	ws := gate.Workspace{}
	testutil.Equal(t, check.Run(context.Background(), ws).Status, gate.StatusPass)
	ws.Policy.Mutation.SkipInDeploys = true
	out := check.Run(context.Background(), ws)
	testutil.Equal(t, out.Status, gate.StatusNA)
	testutil.Equal(t, out.Reason, reasonFullRunsOnly)
	ws.Mutation.Full = true
	testutil.Equal(t, check.Run(context.Background(), ws).Status, gate.StatusPass)
	testutil.Equal(t, ran, 2)
	testutil.Equal(t, check.Name, pack.CheckMutation)
}

func TestRegistration(t *testing.T) {
	const web = "web"
	ws := gate.Workspace{AppName: web, Repo: "owner/WEB", Policy: policy.Policy{Apps: map[string]policy.App{web: {Repo: "Owner/web"}}}}
	testutil.Equal(t, Registration(context.Background(), ws).Status, gate.StatusPass)
	ws.Repo = "evil/web"
	out := Registration(context.Background(), ws)
	testutil.Equal(t, out.Reason, reasonWrongRepo)
	testutil.Equal(t, out.Findings, []string{"evil/web is not Owner/web"})
	ws.AppName = "ghost"
	testutil.Equal(t, Registration(context.Background(), ws).Reason, reasonUnregistered)
}
