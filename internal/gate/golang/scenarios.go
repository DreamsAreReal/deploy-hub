package golang

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	ValueSecurityScenarios = "security_scenarios_percent"
	ValueBusinessScenarios = "business_scenarios_percent"
	ValueScenarios         = "scenarios"
	ValueFuzzTargets       = "fuzz_targets"
	fuzzPrefix             = "Fuzz"
	reasonScenarios        = "mandatory scenarios are not covered by passing tests"
	reasonFuzz             = "fuzzing found failures"
	reasonNoFuzz           = "no fuzz targets: input handlers and parsers must be fuzzed"
	scenarioFormat         = "%s (%s): test %s did not pass"
	fuzzFormat             = "%s.%s: %s"
)

func Scenarios(_ context.Context, ws gate.Workspace) gate.Outcome {
	run, err := LoadTestRun(ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	module := ws.Module
	tally := map[string]*scenarioTally{policy.CategorySecurity: {}, policy.CategoryBusiness: {}}
	var findings []string
	for _, s := range ws.App.Scenarios {
		missing := missingTests(run, module, s.Tests)
		for _, m := range missing {
			findings = append(findings, fmt.Sprintf(scenarioFormat, s.ID, s.Category, m))
		}
		tally[s.Category].add(len(missing) == 0)
	}
	values := map[string]float64{
		ValueSecurityScenarios: tally[policy.CategorySecurity].percent(),
		ValueBusinessScenarios: tally[policy.CategoryBusiness].percent(),
		ValueScenarios:         float64(len(ws.App.Scenarios)),
	}
	return gate.Judge(len(findings) == 0 && len(ws.App.Scenarios) > 0, reasonScenarios, values, findings)
}

type scenarioTally struct {
	covered, total int
}

func (t *scenarioTally) add(covered bool) {
	t.total++
	if covered {
		t.covered++
	}
}

func (t *scenarioTally) percent() float64 {
	return ratio(t.covered, t.total)
}

func missingTests(run TestRun, module string, tests []string) []string {
	var out []string
	for _, t := range tests {
		pkg, name, _ := strings.Cut(t, testSeparator)
		full := module
		if pkg != "" {
			full = module + "/" + pkg
		}
		if !run.Passed[full+testSeparator+name] {
			out = append(out, t)
		}
	}
	return out
}

type fuzzTarget struct {
	pkg  string
	name string
}

func fuzzTargets(pkgs Packages) ([]fuzzTarget, error) {
	var out []fuzzTarget
	for _, p := range pkgs {
		for _, f := range append(append([]string(nil), p.TestGoFiles...), p.XTestGoFiles...) {
			names, err := fuzzFuncs(filepath.Join(p.Dir, f))
			if err != nil {
				return nil, err
			}
			for _, n := range names {
				out = append(out, fuzzTarget{pkg: p.ImportPath, name: n})
			}
		}
	}
	slices.SortFunc(out, func(a, b fuzzTarget) int { return strings.Compare(a.pkg+a.name, b.pkg+b.name) })
	return out, nil
}

func fuzzFuncs(path string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, fuzzPrefix) {
			out = append(out, fn.Name.Name)
		}
	}
	return out, nil
}

func Fuzz(ctx context.Context, ws gate.Workspace) gate.Outcome {
	pkgs, err := List(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	targets, err := fuzzTargets(pkgs)
	if err != nil {
		return gate.Fail(err.Error())
	}
	if len(targets) == 0 {
		return gate.Fail(reasonNoFuzz)
	}
	var findings []string
	for _, t := range targets {
		_, err := ws.Run(ctx, GoBinary, "test", "-run=^$", "-fuzz=^"+t.name+"$", "-fuzztime="+ws.Policy.Limits.FuzzTime.String(), t.pkg)
		if err != nil {
			findings = append(findings, fmt.Sprintf(fuzzFormat, t.pkg, t.name, gate.Tail(procexec.Output(err))))
		}
	}
	values := map[string]float64{ValueFuzzTargets: float64(len(targets)), gate.ValueFindings: float64(len(findings))}
	return gate.Judge(len(findings) == 0, reasonFuzz, values, findings)
}
