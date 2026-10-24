package shell

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/junit"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/linecov"
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
	traceScript       = "trace.bash"
	traceLog          = "trace.log"
	testSuffix        = ".bats"
	ValueFunctions    = "functions"
	ValueMaxCognitive = "max_cognitive_complexity"
	traceBufferScale  = 100
	reportPerm        = 0o600
	reasonLint        = "shellcheck or shfmt reported findings"
	reasonComplexity  = "functions exceed the cognitive complexity limit"
	reasonNoTests     = "no bats test files"
	reasonNoReport    = "bats produced no report"
	reasonNoDeps      = "shell scripts have no package dependencies"
	reasonNoFuzz      = "shell scripts have no property-based fuzzing tool; their inputs are exercised by bats tests"
	reasonDeadCode    = "unused functions or variables"
	shellcheckForm    = "%s:%d SC%d %s"
	complexityForm    = "%s: %s has cognitive complexity %d"
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

var (
	Integrity    = gate.NotApplicable(reasonNoDeps)
	Dependencies = gate.NotApplicable(reasonNoDeps)
	Fuzz         = gate.NotApplicable(reasonNoFuzz)
)

func scripts(ctx context.Context, ws gate.Workspace) ([]string, error) {
	return source.Select(ctx, ws, func(f string) bool { return slices.Contains(Sources, source.Ext(ws.Src, f)) })
}

func Lint(ctx context.Context, ws gate.Workspace) gate.Outcome {
	files, checks, err := checked(ctx, ws, false)
	if err != nil {
		return gate.Fail(err.Error())
	}
	if len(files) == 0 {
		return gate.Pass(nil)
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
	_, findings, err := checked(ctx, ws, true)
	if err != nil {
		return gate.Fail(err.Error())
	}
	return gate.Findings(reasonDeadCode, findings)
}

func checked(ctx context.Context, ws gate.Workspace, dead bool) ([]string, []string, error) {
	files, err := scripts(ctx, ws)
	if err != nil || len(files) == 0 {
		return files, nil, err
	}
	findings, err := shellcheck(ctx, ws, files, func(code int) bool { return slices.Contains(deadCodes, code) == dead })
	return files, findings, err
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
		for _, name := range slices.Sorted(maps.Keys(s.functions)) {
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

func traceEnv(ws gate.Workspace, log string) ([]string, error) {
	err := os.WriteFile(ws.Report(traceScript), []byte(traceSetup), reportPerm)
	return []string{envBashEnv + ws.Report(traceScript), envTrace + log}, err
}

func batsFiles(ctx context.Context, ws gate.Workspace) ([]string, error) {
	return source.Select(ctx, ws, func(f string) bool { return path.Ext(f) == testSuffix })
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
	sc.Buffer(nil, bufio.MaxScanTokenSize*traceBufferScale)
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
	cov := linecov.Report{}
	for _, f := range files {
		s, aerr := analyzeFile(ws, f)
		if aerr != nil {
			return aerr
		}
		cov[f] = coverageOf(s, countsFor(ws, counts, f))
	}
	return cov.Save(ws)
}

func coverageOf(s script, counts map[uint]int) *linecov.File {
	fc := &linecov.File{Lines: map[int]int{}, Branches: len(s.branches)}
	for _, l := range s.lines {
		fc.Lines[int(l)] = counts[l]
	}
	for _, b := range s.branches {
		if b.covered(counts) {
			fc.CoveredBranches++
		}
	}
	return fc
}

func Passed(_ context.Context, ws gate.Workspace) (map[string]bool, error) {
	cases, err := junit.Load(ws.Report(JUnitReport))
	if err != nil {
		return nil, err
	}
	return junit.Passed(cases, testID), nil
}
