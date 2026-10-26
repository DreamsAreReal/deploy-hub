package shell

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/linecov"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/packtest"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/tokmut"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	gitBinary     = "git"
	readmeFile    = "README.md"
	invalidScript = "if then fi"
	badScriptFile = "bad.sh"
	fileGrade     = "bin/grade.sh"
	fileTest      = "test/grade.bats"
	gradeScript   = `#!/usr/bin/env bash
set -euo pipefail

readonly TOP=90

grade() {
  local score="$1"
  if [[ ${score} -ge ${TOP} ]]; then
    echo "A"
  elif [[ ${score} -gt 50 ]]; then
    echo "B"
  else
    echo "C"
  fi
  case "${score}" in
    0) echo zero ;;
    *) echo other ;;
  esac
  if [[ -n ${score} ]]; then
    echo set
  fi
}

grade "$@"
`
	junitGreen = `<testsuites><testsuite name="grade.bats"><testcase classname="grade.bats" name="top score" time="0.5"/></testsuite></testsuites>`
)

var errSim = packtest.ErrSim

func TestAnalysisOfAScript(t *testing.T) {
	f, err := parse(fileGrade, []byte(gradeScript))
	testutil.NoErr(t, err)
	s := analyze(f)
	testutil.Equal(t, s.lines, []uint{2, 4, 7, 8, 9, 10, 11, 13, 16, 17, 19, 20, 24})
	testutil.Equal(t, s.branches, []branch{
		{arm: 9, condition: 8}, {arm: 11, condition: 10}, {arm: 13, condition: 10}, {arm: 16}, {arm: 17}, {arm: 20, condition: 19}, {arm: 20, condition: 19, implicit: true},
	})
	testutil.Equal(t, s.functions, map[string]int{"grade": 5, mainFunction: 0})
	counts := map[uint]int{9: 1, 13: 0, 16: 0, 17: 2, 19: 3, 20: 3}
	covered := 0
	for _, b := range s.branches {
		if b.covered(counts) {
			covered++
		}
	}
	testutil.Equal(t, covered, 3)
	testutil.True(t, branch{arm: 20, condition: 19, implicit: true}.covered(map[uint]int{19: 3, 20: 2}), "condition ran without its branch")
	_, err = parse(badScriptFile, []byte(invalidScript))
	testutil.Fails(t, err)
}

func TestComplexityRules(t *testing.T) {
	cases := map[string]int{
		"f() { a && b && c; }":                                      1,
		"f() { a && b || c; }":                                      2,
		"f() { while x; do if y; then z; fi; done; }":               3,
		"f() { for i in 1; do case $i in a) x;; esac; done; }":      3,
		"f() { if a; then x; elif b; then y; else z; fi; }":         3,
		"f() { { if a; then b; fi; }; ( if c; then d; fi ); }":      2,
		"f() { g() { if a; then b; fi; }; }":                        2,
		"f() { if a; then x; elif b; then y; elif c; then z; fi; }": 3,
	}
	for src, want := range cases {
		f, err := parse("x.sh", []byte(src))
		testutil.NoErr(t, err)
		testutil.Equal(t, analyze(f).functions["f"], want)
	}
	top, err := parse("x.sh", []byte("if a; then b; fi\nf() { x; }\n"))
	testutil.NoErr(t, err)
	testutil.Equal(t, analyze(top).functions[mainFunction], 1)
	empty, err := parse("x.sh", []byte("if a; then\nfi\n"))
	testutil.Equal(t, err != nil || len(analyze(empty).branches) == 0, true)
	_, ok := first(nil)
	testutil.True(t, !ok, "no statements")
}

var shellPolicy = policy.Policy{
	Thresholds: policy.Thresholds{LineCoverage: 90, BranchCoverage: 50, MaxCognitive: 9, MutationScore: 90},
	Limits:     policy.Limits{MutantFloor: time.Second, MutantFactor: 2},
}

type world struct {
	*packtest.World
	trace string
	junit string
}

func newWorld(t *testing.T, files map[string]string) *world {
	t.Helper()
	saved := userProcesses
	t.Cleanup(func() { userProcesses = saved })
	userProcesses = func() int { return 7 }
	w := &world{World: packtest.New(t, files, shellPolicy)}
	w.Hook = w.hook
	return w
}

func project(t *testing.T) *world {
	t.Helper()
	return newWorld(t, map[string]string{fileGrade: gradeScript, fileTest: "@test \"top score\" { run bin/grade.sh 95; }\n", readmeFile: "x"})
}

func (w *world) hook(c procexec.Command, line string) (procexec.Result, error) {
	if !slices.Contains(c.Args, BatsBinary) {
		return procexec.Result{}, packtest.ErrPass
	}
	if w.Kill != nil && strings.Contains(line, "--abort") {
		return procexec.Result{}, w.Kill(w.Read(fileGrade, c))
	}
	if w.junit != "" {
		testutil.Write(w.T, w.WS.Report(JUnitReport), w.junit)
	}
	for _, e := range c.Env {
		if log, ok := strings.CutPrefix(e, envTrace); ok && w.trace != "" {
			testutil.Write(w.T, log, strings.ReplaceAll(w.trace, "SRC", w.WS.Src))
		}
	}
	return procexec.Result{}, packtest.ErrPass
}

func TestLintAndDeadCode(t *testing.T) {
	w := project(t)
	w.On(ShellcheckBinary, `{"comments":[{"file":"bin/grade.sh","line":3,"code":2292,"message":"Prefer [[ ]]"},{"file":"bin/grade.sh","line":4,"code":2034,"message":"x appears unused"}]}`, errSim)
	w.On(ShfmtBinary, "bin/grade.sh\n", errSim)
	lint := Lint(context.Background(), w.WS)
	testutil.Equal(t, lint.Findings, []string{"bin/grade.sh:3 SC2292 Prefer [[ ]]", "bin/grade.sh: not formatted"})
	check, _ := w.Fake.Find(ShellcheckBinary)
	testutil.Equal(t, check.Args, []string{"--norc", "--enable=all", "--severity=style", "--external-sources", "--format=json1", fileGrade})
	dead := DeadCode(context.Background(), w.WS)
	testutil.Equal(t, dead.Findings, []string{"bin/grade.sh:4 SC2034 x appears unused"})
	broken := project(t)
	broken.On(ShellcheckBinary, "nope", errSim)
	testutil.Contains(t, Lint(context.Background(), broken.WS).Reason, "shellcheck")
	testutil.Contains(t, DeadCode(context.Background(), broken.WS).Reason, "shellcheck")
	fmtCrash := project(t)
	fmtCrash.On(ShellcheckBinary, `{"comments":[]}`, nil)
	fmtCrash.On(ShfmtBinary, "", errSim)
	testutil.Contains(t, Lint(context.Background(), fmtCrash.WS).Reason, "shfmt")
	none := newWorld(t, map[string]string{readmeFile: "x"})
	testutil.Equal(t, Lint(context.Background(), none.WS).Status, gate.StatusPass)
	testutil.Equal(t, DeadCode(context.Background(), none.WS).Status, gate.StatusPass)
	unlisted := project(t)
	unlisted.FailGit()
	for _, run := range []func(context.Context, gate.Workspace) gate.Outcome{Lint, DeadCode, Complexity, Tests} {
		testutil.Contains(t, run(context.Background(), unlisted.WS).Reason, "git ls-files")
	}
}

func TestComplexityCheck(t *testing.T) {
	w := project(t)
	w.WS.Policy.Thresholds.MaxCognitive = 4
	out := Complexity(context.Background(), w.WS)
	testutil.Equal(t, out.Findings, []string{"bin/grade.sh: grade has cognitive complexity 5"})
	testutil.Equal(t, out.Values, map[string]float64{ValueFunctions: 2, ValueMaxCognitive: 5})
	w.WS.Policy.Thresholds.MaxCognitive = 5
	testutil.Equal(t, Complexity(context.Background(), w.WS).Status, gate.StatusPass)
	invalid := newWorld(t, map[string]string{badScriptFile: invalidScript})
	testutil.Equal(t, Complexity(context.Background(), invalid.WS).Status, gate.StatusFail)
	gone := project(t)
	gone.Files = append(gone.Files, "gone.sh")
	testutil.Equal(t, Complexity(context.Background(), gone.WS).Status, gate.StatusFail)
}

func gradeTrace() string {
	lines := []string{"+SRC/bin/grade.sh:2: set -euo pipefail", "++bin/grade.sh:4: readonly TOP=90", "+bin/grade.sh:8: [[ 95 -ge 90 ]]", "+bin/grade.sh:9: echo A",
		"+./bin/grade.sh:15: case", "+/other/bin/grade.sh:16: echo zero", "+bats-core/x.bash:3: noise", "continuation line", "+bin/grade.sh:19: [[ -n 95 ]]", "+bin/grade.sh:19: [[ -n 1 ]]", "+bin/grade.sh:20: echo set"}
	return strings.Join(lines, "\n") + "\n"
}

func TestTestsAndCoverage(t *testing.T) {
	w := project(t)
	w.junit = junitGreen
	w.trace = gradeTrace()
	out := Tests(context.Background(), w.WS)
	testutil.Equal(t, out.Status, gate.StatusPass)
	run, _ := w.Fake.Find(BatsBinary)
	testutil.Equal(t, run.Name, BashBinary)
	testutil.Equal(t, run.Args, append(limited(), "--report-formatter", "junit", "--output", w.WS.Out, fileTest))
	testutil.Equal(t, run.Env, []string{envBashEnv + w.WS.Report(traceScript), envTrace + w.WS.Report(traceLog)})
	testutil.Equal(t, testutil.Read(t, w.WS.Report(traceScript)), traceSetup)
	cov, err := linecov.Load(w.WS)
	testutil.NoErr(t, err)
	testutil.Equal(t, cov[fileGrade].Lines, map[int]int{2: 1, 4: 1, 7: 0, 8: 1, 9: 1, 10: 0, 11: 0, 13: 0, 16: 1, 17: 0, 19: 2, 20: 1, 24: 0})
	testutil.Equal(t, cov[fileGrade].Branches, 7)
	testutil.Equal(t, cov[fileGrade].CoveredBranches, 4)
	lines := linecov.LineCoverage(context.Background(), w.WS)
	testutil.Equal(t, lines.Findings, []string{"bin/grade.sh: 6 of 13 lines uncovered"})
	testutil.Equal(t, lines.Values, map[string]float64{linecov.ValueLineCoverage: float64(7) * 100 / 13, linecov.ValueLines: 13})
	branches := linecov.Branches(context.Background(), w.WS)
	testutil.Equal(t, branches.Values, map[string]float64{linecov.ValueBranchCover: float64(4) * 100 / 7, linecov.ValueBranches: 7})
	testutil.Equal(t, branches.Status, gate.StatusPass)
	passed, err := Passed(context.Background(), w.WS)
	testutil.NoErr(t, err)
	testutil.Equal(t, passed, map[string]bool{"grade.bats > top score": true})
}

func TestTestFailures(t *testing.T) {
	noTests := newWorld(t, map[string]string{fileGrade: gradeScript})
	testutil.Equal(t, Tests(context.Background(), noTests.WS).Reason, reasonNoTests)
	silent := project(t)
	silent.On(BatsBinary, "", errSim)
	testutil.Contains(t, Tests(context.Background(), silent.WS).Reason, reasonNoReport)
	badScript := newWorld(t, map[string]string{badScriptFile: invalidScript, fileTest: "x"})
	badScript.junit = junitGreen
	testutil.Equal(t, Tests(context.Background(), badScript.WS).Status, gate.StatusFail)
	blocked := project(t)
	blocked.junit = junitGreen
	testutil.Mkdir(t, blocked.WS.Report(traceLog))
	testutil.Write(t, filepath.Join(blocked.WS.Report(traceLog), "x"), "")
	testutil.Equal(t, Tests(context.Background(), blocked.WS).Status, gate.StatusFail)
	noScript := project(t)
	noScript.junit = junitGreen
	testutil.Mkdir(t, noScript.WS.Report(traceScript))
	testutil.Write(t, filepath.Join(noScript.WS.Report(traceScript), "x"), "")
	testutil.Equal(t, Tests(context.Background(), noScript.WS).Status, gate.StatusFail)
	missing := project(t)
	_, err := Passed(context.Background(), missing.WS)
	testutil.Fails(t, err)
	counts, err := readTrace(filepath.Join(t.TempDir(), "absent"))
	testutil.NoErr(t, err)
	testutil.Equal(t, len(counts), 0)
	for _, check := range []func(context.Context, gate.Workspace) gate.Outcome{Integrity, Dependencies, Fuzz} {
		testutil.Equal(t, check(context.Background(), missing.WS).Status, gate.StatusNA)
	}
}

func TestShellMutation(t *testing.T) {
	w := project(t)
	w.junit = junitGreen
	w.trace = gradeTrace()
	testutil.Equal(t, Tests(context.Background(), w.WS).Status, gate.StatusPass)
	w.Kill = func(src string) error {
		if strings.Contains(src, "-ge ${TOP}") {
			return nil
		}
		return packtest.ExitError(t, 1)
	}
	w.On("bash -n", "", nil)
	rep, err := tokmut.Mutate(context.Background(), w.WS, mutationSuite)
	testutil.NoErr(t, err)
	testutil.Equal(t, rep.Counts[mutation.StatusSurvived] > 0 && rep.Counts[mutation.StatusKilled] > 0 && rep.Counts[mutation.StatusNotCovered] > 0, true)
	for i := range mutation.Workers() {
		testutil.Missing(t, w.WS.Report(tokmut.SandboxPrefix+strconv.Itoa(i)))
	}
	abort, _ := w.Fake.Find("--abort")
	testutil.Equal(t, abort.Args, append(limited(), "--abort", fileTest))
	syntax, _ := w.Fake.Find("bash -n")
	testutil.Equal(t, syntax.Args, []string{"-n", fileGrade})
	unviable := project(t)
	unviable.junit = junitGreen
	unviable.trace = gradeTrace()
	Tests(context.Background(), unviable.WS)
	unviable.On("bash -n", "", errSim)
	rep, err = tokmut.Mutate(context.Background(), unviable.WS, mutationSuite)
	testutil.NoErr(t, err)
	testutil.True(t, rep.Counts[mutation.StatusNotViable] > 0 && rep.Counts[mutation.StatusKilled] == 0, "syntax errors are not viable")
	testutil.Equal(t, Mutation(context.Background(), unviable.WS).Status, gate.StatusFail)
}

func TestShellMutationNeedsReports(t *testing.T) {
	w := project(t)
	_, err := tokmut.Mutate(context.Background(), w.WS, mutationSuite)
	testutil.Contains(t, err.Error(), "coverage report missing")
	testutil.Write(t, w.WS.Report(linecov.ReportFile), "{}")
	_, err = tokmut.Mutate(context.Background(), w.WS, mutationSuite)
	testutil.Fails(t, err)
	testutil.Write(t, w.WS.Report(JUnitReport), junitGreen)
	unlisted := project(t)
	testutil.Write(t, unlisted.WS.Report(linecov.ReportFile), "{}")
	testutil.Write(t, unlisted.WS.Report(JUnitReport), junitGreen)
	unlisted.FailGit()
	_, err = tokmut.Mutate(context.Background(), unlisted.WS, mutationSuite)
	testutil.Contains(t, err.Error(), "git ls-files")
	_, err = os.Stat(w.WS.Report(linecov.ReportFile))
	testutil.NoErr(t, err)
}

func TestScriptsWithoutExtensionAreChecked(t *testing.T) {
	w := newWorld(t, map[string]string{"agent/apply": "#!/bin/sh\necho ok\n", "agent/run": "#!/usr/bin/ucode\nlet x = 1;\n", readmeFile: "x"})
	w.On(ShellcheckBinary, `{"comments":[]}`, nil)
	w.On(ShfmtBinary, "", nil)
	testutil.Equal(t, Lint(context.Background(), w.WS).Status, gate.StatusPass)
	check, _ := w.Fake.Find(ShellcheckBinary)
	testutil.Equal(t, check.Args[len(check.Args)-1], "agent/apply")
	testutil.Equal(t, Complexity(context.Background(), w.WS).Values[ValueFunctions], float64(1))
}

func limited() []string {
	return []string{"-c", limitScript, BashBinary, "4103", BatsBinary}
}

func TestTestsRunUnderAProcessLimit(t *testing.T) {
	budget := strconv.Itoa(countUserProcesses() + 128)
	out, err := exec.CommandContext(context.Background(), BashBinary, "-c", limitScript, BashBinary, budget, BashBinary, "-c", "ulimit -u").Output()
	testutil.NoErr(t, err)
	testutil.Equal(t, strings.TrimSpace(string(out)), budget)
	hard := strconv.Itoa(countUserProcesses() + 256)
	lower, err := exec.CommandContext(context.Background(), BashBinary, "-c", `ulimit -u "$1" && shift && exec "$@"`, BashBinary, hard, BashBinary, "-c", limitScript, BashBinary, "1000000", BashBinary, "-c", "ulimit -u").Output()
	testutil.NoErr(t, err)
	testutil.Equal(t, strings.TrimSpace(string(lower)), hard)
	testutil.True(t, countUserProcesses() > 0, "the test process itself belongs to the user")
	saved := procDir
	t.Cleanup(func() { procDir = saved })
	procDir = filepath.Join(t.TempDir(), "absent")
	testutil.Equal(t, countUserProcesses(), 0)
	procDir = t.TempDir()
	testutil.Mkdir(t, filepath.Join(procDir, "self"))
	testutil.Mkdir(t, filepath.Join(procDir, "42"))
	testutil.Equal(t, countUserProcesses(), 1)
	testutil.Mkdir(t, filepath.Join(procDir, "42", "task", "42"))
	testutil.Mkdir(t, filepath.Join(procDir, "42", "task", "43"))
	testutil.Mkdir(t, filepath.Join(procDir, "7", "task"))
	testutil.Equal(t, countUserProcesses(), 3)
}
