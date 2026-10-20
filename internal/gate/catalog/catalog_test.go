package catalog

import (
	"context"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
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
	for _, required := range []string{"comments", "markers", "language", "hygiene", "lint", "cognitive-complexity", "duplication", "dead-code", "branch-coverage", "fuzz",
		"secrets-gitleaks", "secrets-trufflehog", "sast", "vulnerabilities", "sca", "sbom-licenses", "workflows", "dockerfile", "iac", "image", "dast", "load"} {
		testutil.True(t, seen[required], "missing check "+required)
	}
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

func TestGoOnly(t *testing.T) {
	run := goOnly(func(context.Context, gate.Workspace) gate.Outcome { return gate.Pass(nil) })
	testutil.Equal(t, run(context.Background(), gate.Workspace{}).Reason, reasonToolchain)
	testutil.Equal(t, run(context.Background(), gate.Workspace{Module: "m"}).Status, gate.StatusPass)
}
