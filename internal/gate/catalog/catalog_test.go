package catalog

import (
	"context"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	checkMutation = "mutation"
	checkDast     = "dast"
	checkLoad     = "load"
	checkSAST     = "sast"
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
	testutil.True(t, index["tests"] < index["line-coverage"] && index["line-coverage"] < index[checkMutation] && index["tests"] < index["scenarios"], "report producers run first")
	costlyLast := []string{checkDast, checkLoad, checkMutation}
	testutil.Equal(t, names[len(names)-len(costlyLast):], costlyLast)
	costly := map[string]bool{}
	for _, n := range costlyLast {
		costly[n] = true
	}
	inContainer := map[string]bool{checkSAST: true}
	for _, n := range []string{"image", checkDast, checkLoad} {
		inContainer[n] = true
	}
	for _, c := range Checks() {
		testutil.Equal(t, c.Long, c.Name == checkMutation)
		testutil.Equal(t, c.Costly, costly[c.Name])
		testutil.Equal(t, c.Docker, inContainer[c.Name])
	}
	for _, required := range []string{"comments", "markers", "language", "hygiene", "lint", "cognitive-complexity", "duplication", "dead-code", "branch-coverage", "fuzz",
		"secrets-gitleaks", "secrets-trufflehog", checkSAST, "vulnerabilities", "sca", "sbom-licenses", "workflows", "dockerfile", "iac"} {
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
