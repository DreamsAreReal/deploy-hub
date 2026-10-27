package catalog

import (
	"context"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/dupl"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/scan"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/stacks"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/stage"
)

const (
	reasonUnregistered = "the app is not registered in the central gate policy"
	reasonWrongRepo    = "the repository is not the registered source of the app"
)

func Checks() []gate.Check {
	packs := stacks.All()
	byPack := func(name string) gate.Check {
		mutation := name == pack.CheckMutation
		return gate.Check{Name: name, Kind: kinds[name], Long: mutation, Costly: mutation, Run: pack.Dispatch(packs, name)}
	}
	return []gate.Check{
		{Name: "registration", Kind: gate.KindSecurity, Run: Registration},
		{Name: "hygiene", Kind: gate.KindSecurity, Run: source.Hygiene},
		{Name: "comments", Kind: gate.KindQuality, Run: source.Comments},
		{Name: "markers", Kind: gate.KindQuality, Run: source.Markers},
		{Name: "language", Kind: gate.KindQuality, Run: source.Language},
		{Name: "stacks", Kind: gate.KindSecurity, Run: pack.Stacks(packs)},
		byPack(pack.CheckModules),
		byPack(pack.CheckDependencies),
		byPack(pack.CheckLint),
		byPack(pack.CheckComplexity),
		{Name: "duplication", Kind: gate.KindQuality, Run: dupl.Check},
		byPack(pack.CheckDeadCode),
		byPack(pack.CheckTests),
		byPack(pack.CheckLineCoverage),
		byPack(pack.CheckBranches),
		{Name: "scenarios", Kind: gate.KindQuality, Run: pack.Scenarios(packs)},
		byPack(pack.CheckFuzz),
		{Name: "secrets-gitleaks", Kind: gate.KindSecurity, Run: scan.Gitleaks},
		{Name: "secrets-trufflehog", Kind: gate.KindSecurity, Run: scan.Trufflehog},
		{Name: "sast", Kind: gate.KindSecurity, Docker: true, Run: scan.SAST},
		byPack(pack.CheckVulns),
		{Name: "sca", Kind: gate.KindSecurity, Run: scan.SCA},
		{Name: "sbom-licenses", Kind: gate.KindSecurity, Run: scan.SBOM},
		{Name: "workflows", Kind: gate.KindSecurity, Run: scan.Workflows},
		{Name: "dockerfile", Kind: gate.KindSecurity, Needs: gate.NeedImage, Run: scan.Dockerfiles},
		{Name: "iac", Kind: gate.KindSecurity, Run: scan.IaC},
		{Name: "image", Kind: gate.KindSecurity, Needs: gate.NeedImage, Docker: true, Run: scan.Image},
		{Name: "dast", Kind: gate.KindSecurity, Needs: gate.NeedHTTP, Costly: true, Docker: true, Run: stage.DAST},
		{Name: "load", Kind: gate.KindQuality, Needs: gate.NeedHTTP, Costly: true, Docker: true, Run: stage.Load},
		byPack(pack.CheckMutation),
	}
}

var kinds = map[string]gate.Kind{
	pack.CheckModules: gate.KindSecurity, pack.CheckDependencies: gate.KindSecurity, pack.CheckVulns: gate.KindSecurity,
	pack.CheckLint: gate.KindQuality, pack.CheckComplexity: gate.KindQuality, pack.CheckDeadCode: gate.KindQuality,
	pack.CheckTests: gate.KindQuality, pack.CheckLineCoverage: gate.KindQuality, pack.CheckBranches: gate.KindQuality,
	pack.CheckFuzz: gate.KindQuality, pack.CheckMutation: gate.KindQuality,
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
