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
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	ValueFuzzTargets = "fuzz_targets"
	fuzzPrefix       = "Fuzz"
	reasonFuzz       = "fuzzing found failures"
	reasonNoFuzz     = "no fuzz targets: input handlers and parsers must be fuzzed"
	fuzzFormat       = "%s.%s: %s"
)

func PassedTests(ws gate.Workspace) (map[string]bool, error) {
	run, err := LoadTestRun(ws)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(run.Passed))
	for full := range run.Passed {
		out[strings.TrimPrefix(strings.TrimPrefix(full, ws.Module), "/")] = true
	}
	return out, nil
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
