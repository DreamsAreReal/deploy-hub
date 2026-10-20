package catalog

import (
	"context"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/dupl"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/scan"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/stage"
)

const (
	reasonUnregistered = "the app is not registered in the central gate policy"
	reasonWrongRepo    = "the repository is not the registered source of the app"
	reasonToolchain    = "unsupported toolchain: no go.mod at the repository root; the gate supports Go"
)

func Checks() []gate.Check {
	return []gate.Check{
		{Name: "registration", Kind: gate.KindSecurity, Run: Registration},
		{Name: "hygiene", Kind: gate.KindSecurity, Run: source.Hygiene},
		{Name: "comments", Kind: gate.KindQuality, Run: source.Comments},
		{Name: "markers", Kind: gate.KindQuality, Run: source.Markers},
		{Name: "language", Kind: gate.KindQuality, Run: source.Language},
		{Name: "modules", Kind: gate.KindSecurity, Run: goOnly(golang.Integrity)},
		{Name: "dependencies", Kind: gate.KindSecurity, Run: goOnly(golang.Dependencies)},
		{Name: "lint", Kind: gate.KindQuality, Run: goOnly(golang.Lint)},
		{Name: "cognitive-complexity", Kind: gate.KindQuality, Run: goOnly(golang.Cognitive)},
		{Name: "duplication", Kind: gate.KindQuality, Run: dupl.Check},
		{Name: "dead-code", Kind: gate.KindQuality, Run: goOnly(golang.Deadcode)},
		{Name: "tests", Kind: gate.KindQuality, Run: goOnly(golang.Tests)},
		{Name: "line-coverage", Kind: gate.KindQuality, Run: goOnly(golang.Coverage)},
		{Name: "branch-coverage", Kind: gate.KindQuality, Run: goOnly(golang.Branches)},
		{Name: "scenarios", Kind: gate.KindQuality, Run: goOnly(golang.Scenarios)},
		{Name: "fuzz", Kind: gate.KindQuality, Run: goOnly(golang.Fuzz)},
		{Name: "mutation", Kind: gate.KindQuality, Run: goOnly(mutation.Check)},
		{Name: "secrets-gitleaks", Kind: gate.KindSecurity, Run: scan.Gitleaks},
		{Name: "secrets-trufflehog", Kind: gate.KindSecurity, Run: scan.Trufflehog},
		{Name: "sast", Kind: gate.KindSecurity, Run: scan.SAST},
		{Name: "vulnerabilities", Kind: gate.KindSecurity, Run: goOnly(golang.Vulnerabilities)},
		{Name: "sca", Kind: gate.KindSecurity, Run: scan.SCA},
		{Name: "sbom-licenses", Kind: gate.KindSecurity, Run: scan.SBOM},
		{Name: "workflows", Kind: gate.KindSecurity, Run: scan.Workflows},
		{Name: "dockerfile", Kind: gate.KindSecurity, Needs: gate.NeedImage, Run: scan.Dockerfiles},
		{Name: "iac", Kind: gate.KindSecurity, Run: scan.IaC},
		{Name: "image", Kind: gate.KindSecurity, Needs: gate.NeedImage, Run: scan.Image},
		{Name: "dast", Kind: gate.KindSecurity, Needs: gate.NeedHTTP, Run: stage.DAST},
		{Name: "load", Kind: gate.KindQuality, Needs: gate.NeedHTTP, Run: stage.Load},
	}
}

func goOnly(run func(context.Context, gate.Workspace) gate.Outcome) func(context.Context, gate.Workspace) gate.Outcome {
	return func(ctx context.Context, ws gate.Workspace) gate.Outcome {
		if ws.Module == "" {
			return gate.Fail(reasonToolchain)
		}
		return run(ctx, ws)
	}
}

func Registration(_ context.Context, ws gate.Workspace) gate.Outcome {
	app, err := ws.Policy.App(ws.AppName)
	if err != nil {
		return gate.Fail(reasonUnregistered, err.Error())
	}
	if !strings.EqualFold(app.Repo, ws.Repo) {
		return gate.Fail(reasonWrongRepo, ws.Repo+" is not "+app.Repo)
	}
	return gate.Pass(nil)
}
