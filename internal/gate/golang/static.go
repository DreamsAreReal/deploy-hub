package golang

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	LintBinary      = "golangci-lint"
	CognitBinary    = "gocognit"
	DeadcodeBinary  = "deadcode"
	LintConfig      = "golangci.yml"
	ValueMaxCognit  = "max_cognitive_complexity"
	ValueFunctions  = "functions"
	issueFormat     = "%s:%d: %s (%s)"
	cognitFormat    = "%s:%d: %s has cognitive complexity %d"
	deadcodePrefix  = "unreachable func"
	subMod          = "mod"
	flagJSON        = "-json"
	reasonLint      = "linters reported issues"
	reasonCognitive = "functions exceed the cognitive complexity limit"
	reasonModules   = "go.mod/go.sum are not tidy or do not verify"
	reasonDeadcode  = "unreachable code"
)

type lintReport struct {
	Issues []struct {
		FromLinter string
		Text       string
		Pos        struct {
			Filename string
			Line     int
		}
	}
}

func Lint(ctx context.Context, ws gate.Workspace) gate.Outcome {
	cfg := filepath.Join(ws.PolicyDir, LintConfig)
	res, err := ws.Run(ctx, LintBinary, "run", "--config", cfg, "--show-stats=false", "--output.json.path=stdout", "--output.text.path=stderr", "--issues-exit-code=1", allPkgs)
	var rep lintReport
	perr := json.Unmarshal([]byte(res.Stdout), &rep)
	findings := make([]string, 0, len(rep.Issues))
	for _, i := range rep.Issues {
		findings = append(findings, fmt.Sprintf(issueFormat, i.Pos.Filename, i.Pos.Line, i.Text, i.FromLinter))
	}
	return gate.FromTool(reasonLint, res, err, findings, perr)
}

type cognitEntry struct {
	PkgName    string
	FuncName   string
	Complexity int
	Pos        struct {
		Filename string
		Line     int
	}
}

func Cognitive(ctx context.Context, ws gate.Workspace) gate.Outcome {
	res, err := ws.Run(ctx, CognitBinary, flagJSON, "-test=true", "-exact-recursion", ".")
	if err != nil {
		return gate.Fail("gocognit failed: " + gate.Tail(procexec.Output(err)))
	}
	var entries []cognitEntry
	if err := json.Unmarshal([]byte(res.Stdout), &entries); err != nil {
		return gate.Fail("gocognit report unreadable: " + err.Error())
	}
	limit := ws.Policy.Thresholds.MaxCognitive
	maxSeen := 0
	var findings []string
	for _, e := range entries {
		maxSeen = max(maxSeen, e.Complexity)
		if e.Complexity > limit {
			findings = append(findings, fmt.Sprintf(cognitFormat, ws.Rel(e.Pos.Filename), e.Pos.Line, e.FuncName, e.Complexity))
		}
	}
	values := map[string]float64{ValueMaxCognit: float64(maxSeen), ValueFunctions: float64(len(entries)), gate.ValueFindings: float64(len(findings))}
	return gate.Judge(len(findings) == 0, reasonCognitive, values, findings)
}

func Modules(ctx context.Context, ws gate.Workspace) gate.Outcome {
	var findings []string
	for _, args := range [][]string{{subMod, "verify"}, {subMod, "tidy", "-diff"}} {
		_, err := ws.Run(ctx, GoBinary, args...)
		if err != nil {
			findings = append(findings, "go "+strings.Join(args, " ")+": "+gate.Tail(procexec.Output(err)))
		}
	}
	return gate.Findings(reasonModules, findings)
}

func Deadcode(ctx context.Context, ws gate.Workspace) gate.Outcome {
	pkgs, err := List(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	dirs := pkgs.supportDirs()
	var findings []string
	for _, withTests := range []bool{false, true} {
		lines, err := deadcode(ctx, ws, withTests)
		if err != nil {
			return gate.Fail(err.Error())
		}
		findings = append(findings, outsideSupport(ws.Src, lines, dirs, withTests)...)
	}
	return gate.Findings(reasonDeadcode, unique(findings))
}

func (ps Packages) supportDirs() map[string]bool {
	support := ps.TestSupport()
	dirs := map[string]bool{}
	for _, p := range ps {
		if support[p.ImportPath] {
			dirs[p.Dir] = true
		}
	}
	return dirs
}

func unique(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func deadcode(ctx context.Context, ws gate.Workspace, withTests bool) ([]string, error) {
	res, err := ws.Run(ctx, DeadcodeBinary, fmt.Sprintf("-test=%t", withTests), allPkgs)
	if err != nil {
		return nil, fmt.Errorf("deadcode: %w: %s", err, gate.Tail(procexec.Output(err)))
	}
	var out []string
	for _, l := range strings.Split(res.Stdout, "\n") {
		if strings.Contains(l, deadcodePrefix) {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out, nil
}

func outsideSupport(src string, lines []string, supportDirs map[string]bool, withTests bool) []string {
	var out []string
	for _, l := range lines {
		file, _, _ := strings.Cut(l, ":")
		dir := filepath.Dir(filepath.Join(src, file))
		if withTests || !supportDirs[dir] {
			out = append(out, l)
		}
	}
	return out
}
