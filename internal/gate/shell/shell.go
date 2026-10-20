package shell

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/junit"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/source"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
)

const (
	Stack             = "shell"
	ShellcheckBinary  = "shellcheck"
	ShfmtBinary       = "shfmt"
	BatsBinary        = "bats"
	BashBinary        = "bash"
	JUnitReport       = "report.xml"
	CoverageReport    = "coverage.json"
	traceScript       = "trace.bash"
	traceLog          = "trace.log"
	testSuffix        = ".bats"
	ValueFunctions    = "functions"
	ValueMaxCognitive = "max_cognitive_complexity"
	ValueLineCoverage = "line_coverage"
	ValueBranchCover  = "branch_coverage"
	ValueLines        = "lines"
	ValueBranches     = "branches"
	percentScale      = 100
	reportPerm        = 0o600
	reasonLint        = "shellcheck or shfmt reported findings"
	reasonComplexity  = "functions exceed the cognitive complexity limit"
	reasonNoTests     = "no bats test files"
	reasonNoReport    = "bats produced no report"
	reasonLines       = "line coverage is below the policy threshold"
	reasonBranches    = "branch coverage is below the policy threshold"
	reasonNoDeps      = "shell scripts have no package dependencies"
	reasonNoFuzz      = "shell scripts have no property-based fuzzing tool; their inputs are exercised by bats tests"
	reasonDeadCode    = "unused functions or variables"
	shellcheckForm    = "%s:%d SC%d %s"
	complexityForm    = "%s: %s has cognitive complexity %d"
	uncoveredForm     = "%s: %d of %d lines uncovered"
	formatForm        = "%s: not formatted"
	traceSetup        = "exec {DEPLOY_HUB_TRACE_FD}>>\"$DEPLOY_HUB_TRACE\"\nBASH_XTRACEFD=$DEPLOY_HUB_TRACE_FD\nPS4='+${BASH_SOURCE[0]}:${LINENO}: '\nset -x\n"
	envTrace          = "DEPLOY_HUB_TRACE="
	envBashEnv        = "BASH_ENV="
	idSeparator       = " > "
	unusedVariable    = 2034
	unusedFunction    = 2329
)

var (
	Sources   = []string{".sh", ".bash"}
	traceLine = regexp.MustCompile(`^\++(.+?):(\d+): `)
	deadCodes = []int{unusedVariable, unusedFunction}
)

func NotApplicable(reason string) func(context.Context, gate.Workspace) gate.Outcome {
	return func(context.Context, gate.Workspace) gate.Outcome {
		return gate.Outcome{Status: gate.StatusNA, Reason: reason}
	}
}

var (
	Integrity    = NotApplicable(reasonNoDeps)
	Dependencies = NotApplicable(reasonNoDeps)
	Fuzz         = NotApplicable(reasonNoFuzz)
)

func tracked(ctx context.Context, ws gate.Workspace, keep func(string) bool) ([]string, error) {
	files, err := source.Tracked(ctx, ws.Exec, ws.Src)
	var out []string
	for _, f := range files {
		if keep(f) {
			out = append(out, f)
		}
	}
	return out, err
}

func scripts(ctx context.Context, ws gate.Workspace) ([]string, error) {
	return tracked(ctx, ws, func(f string) bool { return slices.Contains(Sources, path.Ext(f)) })
}

func Lint(ctx context.Context, ws gate.Workspace) gate.Outcome {
	files, err := scripts(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	if len(files) == 0 {
		return gate.Pass(nil)
	}
	checks, err := shellcheck(ctx, ws, files, func(code int) bool { return !slices.Contains(deadCodes, code) })
	if err != nil {
		return gate.Fail(err.Error())
	}
	res, err := ws.Run(ctx, ShfmtBinary, append([]string{"-l"}, files...)...)
	unformatted := strings.Fields(res.Stdout)
	if err != nil && len(unformatted) == 0 {
		return gate.Fail("shfmt: " + gate.Tail(procexec.Output(err)))
	}
	for _, f := range unformatted {
		checks = append(checks, fmt.Sprintf(formatForm, f))
	}
	return gate.Findings(reasonLint, checks)
}

func DeadCode(ctx context.Context, ws gate.Workspace) gate.Outcome {
	files, err := scripts(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	if len(files) == 0 {
		return gate.Pass(nil)
	}
	findings, err := shellcheck(ctx, ws, files, func(code int) bool { return slices.Contains(deadCodes, code) })
	if err != nil {
		return gate.Fail(err.Error())
	}
	return gate.Findings(reasonDeadCode, findings)
}

func shellcheck(ctx context.Context, ws gate.Workspace, files []string, keep func(int) bool) ([]string, error) {
	res, err := ws.Run(ctx, ShellcheckBinary, append([]string{"--norc", "--enable=all", "--severity=style", "--external-sources", "--format=json1"}, files...)...)
	var report struct {
		Comments []struct {
			File    string `json:"file"`
			Line    int    `json:"line"`
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"comments"`
	}
	if perr := json.Unmarshal([]byte(res.Stdout), &report); perr != nil {
		return nil, fmt.Errorf("shellcheck: %w: %s", perr, gate.Tail(procexec.Output(err)))
	}
	var out []string
	for _, c := range report.Comments {
		if keep(c.Code) {
			out = append(out, fmt.Sprintf(shellcheckForm, c.File, c.Line, c.Code, c.Message))
		}
	}
	return out, nil
}

func Complexity(ctx context.Context, ws gate.Workspace) gate.Outcome {
	files, err := scripts(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	var findings []string
	functions, worst := 0, 0
	for _, f := range files {
		s, perr := analyzeFile(ws, f)
		if perr != nil {
			return gate.Fail(perr.Error())
		}
		for _, name := range sortedKeys(s.functions) {
			score := s.functions[name]
			functions, worst = functions+1, max(worst, score)
			if score > ws.Policy.Thresholds.MaxCognitive {
				findings = append(findings, fmt.Sprintf(complexityForm, f, name, score))
			}
		}
	}
	values := map[string]float64{ValueFunctions: float64(functions), ValueMaxCognitive: float64(worst)}
	return gate.Judge(len(findings) == 0, reasonComplexity, values, findings)
}

func analyzeFile(ws gate.Workspace, rel string) (script, error) {
	data, err := os.ReadFile(filepath.Clean(filepath.Join(ws.Src, filepath.FromSlash(rel))))
	if err != nil {
		return script{}, err
	}
	f, err := parse(rel, data)
	if err != nil {
		return script{}, err
	}
	return analyze(f), nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func traceEnv(ws gate.Workspace, log string) ([]string, error) {
	err := os.WriteFile(ws.Report(traceScript), []byte(traceSetup), reportPerm)
	return []string{envBashEnv + ws.Report(traceScript), envTrace + log}, err
}

func batsFiles(ctx context.Context, ws gate.Workspace) ([]string, error) {
	return tracked(ctx, ws, func(f string) bool { return path.Ext(f) == testSuffix })
}

func Tests(ctx context.Context, ws gate.Workspace) gate.Outcome {
	tests, err := batsFiles(ctx, ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	if len(tests) == 0 {
		return gate.Fail(reasonNoTests)
	}
	env, err := traceEnv(ws, ws.Report(traceLog))
	if err != nil {
		return gate.Fail(err.Error())
	}
	_ = os.Remove(ws.Report(traceLog))
	_, runErr := ws.RunEnv(ctx, env, BatsBinary, append([]string{"--report-formatter", "junit", "--output", ws.Out}, tests...)...)
	cases, lerr := junit.Load(ws.Report(JUnitReport))
	if lerr != nil {
		return gate.Fail(reasonNoReport + ": " + gate.Tail(errors.Join(runErr, lerr).Error()))
	}
	if cerr := writeCoverage(ctx, ws); cerr != nil {
		return gate.Fail(cerr.Error())
	}
	return junit.Judge(cases, testID)
}

func testID(c junit.Case) string {
	return c.Classname + idSeparator + c.Name
}

type fileCoverage struct {
	Lines           map[uint]int `json:"lines"`
	Branches        int          `json:"branches"`
	CoveredBranches int          `json:"covered_branches"`
}

func readTrace(file string) (map[string]map[uint]int, error) {
	counts := map[string]map[uint]int{}
	f, err := os.Open(filepath.Clean(file))
	if errors.Is(err, os.ErrNotExist) {
		return counts, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, bufio.MaxScanTokenSize*percentScale)
	for sc.Scan() {
		m := traceLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		n, _ := strconv.ParseUint(m[2], 10, 32)
		name := filepath.Clean(m[1])
		if counts[name] == nil {
			counts[name] = map[uint]int{}
		}
		counts[name][uint(n)]++
	}
	return counts, sc.Err()
}

func countsFor(ws gate.Workspace, counts map[string]map[uint]int, rel string) map[uint]int {
	out := map[uint]int{}
	abs := filepath.Join(ws.Src, filepath.FromSlash(rel))
	resolved, _ := filepath.EvalSymlinks(abs)
	for name, lines := range counts {
		if name == filepath.FromSlash(rel) || name == abs || name == resolved || strings.HasSuffix(name, string(filepath.Separator)+filepath.FromSlash(rel)) {
			for l, c := range lines {
				out[l] += c
			}
		}
	}
	return out
}

func writeCoverage(ctx context.Context, ws gate.Workspace) error {
	counts, err := readTrace(ws.Report(traceLog))
	if err != nil {
		return err
	}
	files, err := scripts(ctx, ws)
	if err != nil {
		return err
	}
	cov := map[string]*fileCoverage{}
	for _, f := range files {
		s, aerr := analyzeFile(ws, f)
		if aerr != nil {
			return aerr
		}
		cov[f] = coverageOf(s, countsFor(ws, counts, f))
	}
	data, err := json.Marshal(cov)
	if err == nil {
		err = os.WriteFile(ws.Report(CoverageReport), data, reportPerm)
	}
	return err
}

func coverageOf(s script, counts map[uint]int) *fileCoverage {
	fc := &fileCoverage{Lines: map[uint]int{}, Branches: len(s.branches)}
	for _, l := range s.lines {
		fc.Lines[l] = counts[l]
	}
	for _, b := range s.branches {
		if b.covered(counts) {
			fc.CoveredBranches++
		}
	}
	return fc
}

func loadCoverage(ws gate.Workspace) (map[string]*fileCoverage, error) {
	cov := map[string]*fileCoverage{}
	data, err := os.ReadFile(ws.Report(CoverageReport))
	if err != nil {
		return nil, fmt.Errorf("coverage report missing: run the tests check first: %w", err)
	}
	return cov, json.Unmarshal(data, &cov)
}

func ratio(part, whole int) float64 {
	if whole == 0 {
		return percentScale
	}
	return float64(part) * percentScale / float64(whole)
}

func LineCoverage(_ context.Context, ws gate.Workspace) gate.Outcome {
	cov, err := loadCoverage(ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	covered, total := 0, 0
	var findings []string
	for _, name := range sortedKeys(cov) {
		hit := 0
		for _, c := range cov[name].Lines {
			if c > 0 {
				hit++
			}
		}
		lines := len(cov[name].Lines)
		covered, total = covered+hit, total+lines
		if hit < lines {
			findings = append(findings, fmt.Sprintf(uncoveredForm, name, lines-hit, lines))
		}
	}
	pct := ratio(covered, total)
	return gate.Judge(pct >= ws.Policy.Thresholds.LineCoverage, reasonLines, map[string]float64{ValueLineCoverage: pct, ValueLines: float64(total)}, findings)
}

func Branches(_ context.Context, ws gate.Workspace) gate.Outcome {
	cov, err := loadCoverage(ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	covered, total := 0, 0
	for _, f := range cov {
		covered, total = covered+f.CoveredBranches, total+f.Branches
	}
	pct := ratio(covered, total)
	return gate.Judge(pct >= ws.Policy.Thresholds.BranchCoverage, reasonBranches, map[string]float64{ValueBranchCover: pct, ValueBranches: float64(total)}, nil)
}

func Passed(_ context.Context, ws gate.Workspace) (map[string]bool, error) {
	cases, err := junit.Load(ws.Report(JUnitReport))
	if err != nil {
		return nil, err
	}
	return junit.Passed(cases, testID), nil
}
