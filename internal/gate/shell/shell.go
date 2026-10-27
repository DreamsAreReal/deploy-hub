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
	"syscall"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/junit"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/linecov"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
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
	testIndexFile     = "tests.json"
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
	traceSetup        = "exec {DEPLOY_HUB_TRACE_FD}>>\"$DEPLOY_HUB_TRACE\"\nBASH_XTRACEFD=$DEPLOY_HUB_TRACE_FD\nPS4='+${BATS_TEST_FILENAME:-}|${BASH_SOURCE[0]}:${LINENO}: '\nset -x\n"
	envTrace          = "DEPLOY_HUB_TRACE="
	envBashEnv        = "BASH_ENV="
	idSeparator       = " > "
	unusedVariable    = 2034
	unusedFunction    = 2329
	testProcesses     = 4096
	BusyboxDir        = "busybox.d"
	busyboxBinary     = "busybox"
	pathVar           = "PATH"
	limitScript       = `cur=$(ulimit -u); if [ "$cur" = unlimited ] || [ "$cur" -gt "$1" ]; then ulimit -u "$1" || exit; fi; shift; exec "$@"`
)

var (
	Sources   = []string{".sh", ".bash"}
	traceLine = regexp.MustCompile(`^\++(?:([^|]*)\|)?(.+?):(\d+): `)
	deadCodes = []int{unusedVariable, unusedFunction}
)

var ErrNoBusybox = errors.New("the app tests against busybox, but no busybox.d with busybox is on PATH: run deploy-hub tools")

var (
	userProcesses = countUserProcesses
	procDir       = "/proc"
)

func countUserProcesses() int {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return 0
	}
	uid := int64(os.Getuid())
	n := 0
	for _, e := range entries {
		if ownedBy(e, uid) {
			n += threads(e.Name())
		}
	}
	return n
}

func ownedBy(e os.DirEntry, uid int64) bool {
	if _, err := strconv.Atoi(e.Name()); err != nil {
		return false
	}
	info, err := e.Info()
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int64(st.Uid) == uid
}

func threads(pid string) int {
	tasks, err := os.ReadDir(filepath.Join(procDir, pid, "task"))
	if err != nil || len(tasks) == 0 {
		return 1
	}
	return len(tasks)
}

func utilities(ws gate.Workspace) ([]string, error) {
	if ws.App.Shell != policy.ShellBusybox {
		return nil, nil
	}
	for _, dir := range filepath.SplitList(os.Getenv(pathVar)) {
		applets := filepath.Join(dir, BusyboxDir)
		if hasBusybox(applets) {
			return []string{pathVar + "=" + applets}, nil
		}
	}
	return nil, ErrNoBusybox
}

func hasBusybox(applets string) bool {
	root, err := os.OpenRoot(applets)
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	info, err := root.Stat(busyboxBinary)
	return err == nil && info.Mode().IsRegular()
}

func batsArgs(args ...string) []string {
	return append([]string{"-c", limitScript, BashBinary, strconv.Itoa(userProcesses() + testProcesses), BatsBinary}, args...)
}

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
	utils, err := utilities(ws)
	if err != nil {
		return gate.Fail(err.Error())
	}
	env = append(env, utils...)
	_ = os.Remove(ws.Report(traceLog))
	_, runErr := ws.RunEnv(ctx, env, BashBinary, batsArgs(append([]string{"--report-formatter", "junit", "--output", ws.Out}, tests...)...)...)
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

type trace struct {
	counts map[string]map[uint]int
	tests  map[string]map[uint]map[string]bool
}

func readTrace(file string) (trace, error) {
	tr := trace{counts: map[string]map[uint]int{}, tests: map[string]map[uint]map[string]bool{}}
	f, err := os.Open(filepath.Clean(file))
	if errors.Is(err, os.ErrNotExist) {
		return tr, nil
	}
	if err != nil {
		return trace{}, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, bufio.MaxScanTokenSize*traceBufferScale)
	for sc.Scan() {
		if m := traceLine.FindStringSubmatch(sc.Text()); m != nil {
			tr.add(m[1], filepath.Clean(m[2]), m[3])
		}
	}
	return tr, sc.Err()
}

func (tr trace) add(test, name, line string) {
	n, _ := strconv.ParseUint(line, 10, 32)
	if tr.counts[name] == nil {
		tr.counts[name] = map[uint]int{}
		tr.tests[name] = map[uint]map[string]bool{}
	}
	tr.counts[name][uint(n)]++
	if test == "" {
		return
	}
	if tr.tests[name][uint(n)] == nil {
		tr.tests[name][uint(n)] = map[string]bool{}
	}
	tr.tests[name][uint(n)][test] = true
}

func matches(ws gate.Workspace, rel string) func(string) bool {
	abs := filepath.Join(ws.Src, filepath.FromSlash(rel))
	resolved, _ := filepath.EvalSymlinks(abs)
	return func(name string) bool {
		return name == filepath.FromSlash(rel) || name == abs || name == resolved || strings.HasSuffix(name, string(filepath.Separator)+filepath.FromSlash(rel))
	}
}

func countsFor(ws gate.Workspace, counts map[string]map[uint]int, rel string) map[uint]int {
	out := map[uint]int{}
	match := matches(ws, rel)
	for name, lines := range counts {
		if match(name) {
			for l, c := range lines {
				out[l] += c
			}
		}
	}
	return out
}

func testsFor(ws gate.Workspace, tr trace, rel string) map[int][]string {
	sets := map[int]map[string]bool{}
	match := matches(ws, rel)
	for name, lines := range tr.tests {
		if match(name) {
			addTests(ws, sets, lines)
		}
	}
	out := make(map[int][]string, len(sets))
	for l, set := range sets {
		out[l] = slices.Sorted(maps.Keys(set))
	}
	return out
}

func addTests(ws gate.Workspace, sets map[int]map[string]bool, lines map[uint]map[string]bool) {
	for l, tests := range lines {
		if sets[int(l)] == nil {
			sets[int(l)] = map[string]bool{}
		}
		for t := range tests {
			sets[int(l)][testPath(ws, t)] = true
		}
	}
}

func testPath(ws gate.Workspace, test string) string {
	if rel, err := filepath.Rel(ws.Src, test); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return test
}

func writeCoverage(ctx context.Context, ws gate.Workspace) error {
	tr, err := readTrace(ws.Report(traceLog))
	if err != nil {
		return err
	}
	files, err := scripts(ctx, ws)
	if err != nil {
		return err
	}
	cov := linecov.Report{}
	index := testIndex{}
	for _, f := range files {
		s, aerr := analyzeFile(ws, f)
		if aerr != nil {
			return aerr
		}
		cov[f] = coverageOf(s, countsFor(ws, tr.counts, f))
		index[f] = testsFor(ws, tr, f)
	}
	if err := index.save(ws); err != nil {
		return err
	}
	return cov.Save(ws)
}

type testIndex map[string]map[int][]string

func (t testIndex) save(ws gate.Workspace) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return os.WriteFile(ws.Report(testIndexFile), data, reportPerm)
}

func loadTestIndex(ws gate.Workspace) (testIndex, error) {
	data, err := os.ReadFile(ws.Report(testIndexFile))
	if err != nil {
		return nil, err
	}
	index := testIndex{}
	return index, json.Unmarshal(data, &index)
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
