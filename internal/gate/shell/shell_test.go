package shell

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/mutation"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
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

var errSim = errors.New("simulated")

func exitError(t *testing.T) error {
	t.Helper()
	err := exec.CommandContext(context.Background(), "sh", "-c", "exit 1").Run()
	testutil.True(t, err != nil, "exit status")
	return err
}

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

type world struct {
	t     *testing.T
	ws    gate.Workspace
	fake  *procexectest.Fake
	mu    sync.Mutex
	files []string
	rules map[string]reply
	trace string
	junit string
	kill  func(src string) error
}

type reply struct {
	out string
	err error
}

func newWorld(t *testing.T, files map[string]string) *world {
	t.Helper()
	w := &world{t: t, rules: map[string]reply{}}
	src := t.TempDir()
	for name, content := range files {
		testutil.Write(t, filepath.Join(src, filepath.FromSlash(name)), content)
		w.files = append(w.files, name)
	}
	slices.Sort(w.files)
	w.fake = &procexectest.Fake{Handler: w.handle}
	w.ws = gate.Workspace{
		Src: src, Out: t.TempDir(), Exec: w.fake, Log: slog.New(slog.DiscardHandler),
		Policy: policy.Policy{
			Thresholds: policy.Thresholds{LineCoverage: 90, BranchCoverage: 50, MaxCognitive: 9, MutationScore: 90},
			Limits:     policy.Limits{MutantFloor: time.Second, MutantFactor: 2},
		},
	}
	return w
}

func project(t *testing.T) *world {
	t.Helper()
	return newWorld(t, map[string]string{fileGrade: gradeScript, fileTest: "@test \"top score\" { run bin/grade.sh 95; }\n", readmeFile: "x"})
}

func (w *world) on(substr, out string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rules[substr] = reply{out: out, err: err}
}

func (w *world) handle(c procexec.Command) (procexec.Result, error) {
	if c.Name == gitBinary {
		return procexec.Result{Stdout: strings.Join(w.files, "\x00") + "\x00"}, nil
	}
	line := procexectest.Line(c)
	if c.Name == BatsBinary && w.kill != nil && strings.Contains(line, "--abort") {
		return procexec.Result{}, w.kill(testutil.Read(w.t, filepath.Join(c.Dir, filepath.FromSlash(fileGrade))))
	}
	if c.Name == BatsBinary {
		w.writeRun(c)
	}
	return w.reply(line)
}

func (w *world) writeRun(c procexec.Command) {
	if w.junit != "" {
		testutil.Write(w.t, w.ws.Report(JUnitReport), w.junit)
	}
	for _, e := range c.Env {
		if log, ok := strings.CutPrefix(e, envTrace); ok && w.trace != "" {
			testutil.Write(w.t, log, strings.ReplaceAll(w.trace, "SRC", w.ws.Src))
		}
	}
}

func (w *world) reply(line string) (procexec.Result, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	best := ""
	for substr := range w.rules {
		if strings.Contains(line, substr) && len(substr) > len(best) {
			best = substr
		}
	}
	r := w.rules[best]
	return procexec.Result{Stdout: r.out}, r.err
}

func TestLintAndDeadCode(t *testing.T) {
	w := project(t)
	w.on(ShellcheckBinary, `{"comments":[{"file":"bin/grade.sh","line":3,"code":2292,"message":"Prefer [[ ]]"},{"file":"bin/grade.sh","line":4,"code":2034,"message":"x appears unused"}]}`, errSim)
	w.on(ShfmtBinary, "bin/grade.sh\n", errSim)
	lint := Lint(context.Background(), w.ws)
	testutil.Equal(t, lint.Findings, []string{"bin/grade.sh:3 SC2292 Prefer [[ ]]", "bin/grade.sh: not formatted"})
	check, _ := w.fake.Find(ShellcheckBinary)
	testutil.Equal(t, check.Args, []string{"--norc", "--enable=all", "--severity=style", "--external-sources", "--format=json1", fileGrade})
	dead := DeadCode(context.Background(), w.ws)
	testutil.Equal(t, dead.Findings, []string{"bin/grade.sh:4 SC2034 x appears unused"})
	broken := project(t)
	broken.on(ShellcheckBinary, "nope", errSim)
	testutil.Contains(t, Lint(context.Background(), broken.ws).Reason, "shellcheck")
	testutil.Contains(t, DeadCode(context.Background(), broken.ws).Reason, "shellcheck")
	fmtCrash := project(t)
	fmtCrash.on(ShellcheckBinary, `{"comments":[]}`, nil)
	fmtCrash.on(ShfmtBinary, "", errSim)
	testutil.Contains(t, Lint(context.Background(), fmtCrash.ws).Reason, "shfmt")
	none := newWorld(t, map[string]string{readmeFile: "x"})
	testutil.Equal(t, Lint(context.Background(), none.ws).Status, gate.StatusPass)
	testutil.Equal(t, DeadCode(context.Background(), none.ws).Status, gate.StatusPass)
	unlisted := project(t)
	unlisted.fake.Handler = func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errSim }
	for _, run := range []func(context.Context, gate.Workspace) gate.Outcome{Lint, DeadCode, Complexity, Tests} {
		testutil.Contains(t, run(context.Background(), unlisted.ws).Reason, "git ls-files")
	}
}

func TestComplexityCheck(t *testing.T) {
	w := project(t)
	w.ws.Policy.Thresholds.MaxCognitive = 4
	out := Complexity(context.Background(), w.ws)
	testutil.Equal(t, out.Findings, []string{"bin/grade.sh: grade has cognitive complexity 5"})
	testutil.Equal(t, out.Values, map[string]float64{ValueFunctions: 2, ValueMaxCognitive: 5})
	w.ws.Policy.Thresholds.MaxCognitive = 5
	testutil.Equal(t, Complexity(context.Background(), w.ws).Status, gate.StatusPass)
	invalid := newWorld(t, map[string]string{badScriptFile: invalidScript})
	testutil.Equal(t, Complexity(context.Background(), invalid.ws).Status, gate.StatusFail)
	gone := project(t)
	gone.files = append(gone.files, "gone.sh")
	testutil.Equal(t, Complexity(context.Background(), gone.ws).Status, gate.StatusFail)
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
	out := Tests(context.Background(), w.ws)
	testutil.Equal(t, out.Status, gate.StatusPass)
	run, _ := w.fake.Find(BatsBinary)
	testutil.Equal(t, run.Args, []string{"--report-formatter", "junit", "--output", w.ws.Out, fileTest})
	testutil.Equal(t, run.Env, []string{envBashEnv + w.ws.Report(traceScript), envTrace + w.ws.Report(traceLog)})
	testutil.Equal(t, testutil.Read(t, w.ws.Report(traceScript)), traceSetup)
	cov, err := loadCoverage(w.ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, cov[fileGrade].Lines, map[uint]int{2: 1, 4: 1, 7: 0, 8: 1, 9: 1, 10: 0, 11: 0, 13: 0, 16: 1, 17: 0, 19: 2, 20: 1, 24: 0})
	testutil.Equal(t, cov[fileGrade].Branches, 7)
	testutil.Equal(t, cov[fileGrade].CoveredBranches, 4)
	lines := LineCoverage(context.Background(), w.ws)
	testutil.Equal(t, lines.Findings, []string{"bin/grade.sh: 6 of 13 lines uncovered"})
	testutil.Equal(t, lines.Values, map[string]float64{ValueLineCoverage: float64(7) * 100 / 13, ValueLines: 13})
	branches := Branches(context.Background(), w.ws)
	testutil.Equal(t, branches.Values, map[string]float64{ValueBranchCover: float64(4) * 100 / 7, ValueBranches: 7})
	testutil.Equal(t, branches.Status, gate.StatusPass)
	passed, err := Passed(context.Background(), w.ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, passed, map[string]bool{"grade.bats > top score": true})
}

func TestTestFailures(t *testing.T) {
	noTests := newWorld(t, map[string]string{fileGrade: gradeScript})
	testutil.Equal(t, Tests(context.Background(), noTests.ws).Reason, reasonNoTests)
	silent := project(t)
	silent.on(BatsBinary, "", errSim)
	testutil.Contains(t, Tests(context.Background(), silent.ws).Reason, reasonNoReport)
	badScript := newWorld(t, map[string]string{badScriptFile: invalidScript, fileTest: "x"})
	badScript.junit = junitGreen
	testutil.Equal(t, Tests(context.Background(), badScript.ws).Status, gate.StatusFail)
	blocked := project(t)
	blocked.junit = junitGreen
	testutil.Mkdir(t, blocked.ws.Report(traceLog))
	testutil.Write(t, filepath.Join(blocked.ws.Report(traceLog), "x"), "")
	testutil.Equal(t, Tests(context.Background(), blocked.ws).Status, gate.StatusFail)
	noScript := project(t)
	noScript.junit = junitGreen
	testutil.Mkdir(t, noScript.ws.Report(traceScript))
	testutil.Write(t, filepath.Join(noScript.ws.Report(traceScript), "x"), "")
	testutil.Equal(t, Tests(context.Background(), noScript.ws).Status, gate.StatusFail)
	missing := project(t)
	testutil.Contains(t, LineCoverage(context.Background(), missing.ws).Reason, "coverage report missing")
	testutil.Contains(t, Branches(context.Background(), missing.ws).Reason, "coverage report missing")
	_, err := Passed(context.Background(), missing.ws)
	testutil.Fails(t, err)
	counts, err := readTrace(filepath.Join(t.TempDir(), "absent"))
	testutil.NoErr(t, err)
	testutil.Equal(t, len(counts), 0)
	testutil.Equal(t, ratio(0, 0), float64(percentScale))
	for _, check := range []func(context.Context, gate.Workspace) gate.Outcome{Integrity, Dependencies, Fuzz} {
		testutil.Equal(t, check(context.Background(), missing.ws).Status, gate.StatusNA)
	}
}

func TestShellMutation(t *testing.T) {
	w := project(t)
	w.junit = junitGreen
	w.trace = gradeTrace()
	testutil.Equal(t, Tests(context.Background(), w.ws).Status, gate.StatusPass)
	w.kill = func(src string) error {
		if strings.Contains(src, "-ge ${TOP}") {
			return nil
		}
		return exitError(t)
	}
	w.on("bash -n", "", nil)
	rep, err := runMutation(context.Background(), w.ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, rep.Counts[mutation.StatusSurvived] > 0 && rep.Counts[mutation.StatusKilled] > 0 && rep.Counts[mutation.StatusNotCovered] > 0, true)
	for i := range mutation.Workers() {
		testutil.Missing(t, w.ws.Report(sandboxPrefix+strconv.Itoa(i)))
	}
	abort, _ := w.fake.Find("--abort")
	testutil.Equal(t, abort.Args, []string{"--abort", fileTest})
	syntax, _ := w.fake.Find("bash -n")
	testutil.Equal(t, syntax.Args, []string{"-n", fileGrade})
	unviable := project(t)
	unviable.junit = junitGreen
	unviable.trace = gradeTrace()
	Tests(context.Background(), unviable.ws)
	unviable.on("bash -n", "", errSim)
	rep, err = runMutation(context.Background(), unviable.ws)
	testutil.NoErr(t, err)
	testutil.True(t, rep.Counts[mutation.StatusNotViable] > 0 && rep.Counts[mutation.StatusKilled] == 0, "syntax errors are not viable")
	testutil.Equal(t, Mutation(context.Background(), unviable.ws).Status, gate.StatusFail)
}

func TestShellMutationNeedsReports(t *testing.T) {
	w := project(t)
	_, err := runMutation(context.Background(), w.ws)
	testutil.Contains(t, err.Error(), "coverage report missing")
	testutil.Write(t, w.ws.Report(CoverageReport), "{}")
	_, err = runMutation(context.Background(), w.ws)
	testutil.Fails(t, err)
	testutil.Write(t, w.ws.Report(JUnitReport), junitGreen)
	w.files = append(w.files, "gone.sh")
	_, err = runMutation(context.Background(), w.ws)
	testutil.Fails(t, err)
	invalid := newWorld(t, map[string]string{"x.sh": "a=\"\xff\"\n", fileTest: "x"})
	testutil.Write(t, invalid.ws.Report(CoverageReport), "{}")
	testutil.Write(t, invalid.ws.Report(JUnitReport), junitGreen)
	_, err = runMutation(context.Background(), invalid.ws)
	testutil.Contains(t, err.Error(), "x.sh")
	noSandbox := project(t)
	noSandbox.junit = junitGreen
	noSandbox.trace = gradeTrace()
	Tests(context.Background(), noSandbox.ws)
	for i := range mutation.Workers() {
		testutil.Write(t, noSandbox.ws.Report(sandboxPrefix+strconv.Itoa(i)), "")
	}
	_, err = runMutation(context.Background(), noSandbox.ws)
	testutil.Is(t, err, mutation.ErrIncomplete)
	unlisted := project(t)
	testutil.Write(t, unlisted.ws.Report(CoverageReport), "{}")
	testutil.Write(t, unlisted.ws.Report(JUnitReport), junitGreen)
	unlisted.fake.Handler = func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errSim }
	_, err = runMutation(context.Background(), unlisted.ws)
	testutil.Contains(t, err.Error(), "git ls-files")
	ctx := context.Background()
	testutil.Equal(t, classify(ctx, errSim), mutation.StatusErrored)
	expired, cancel := context.WithCancel(ctx)
	cancel()
	testutil.Equal(t, classify(expired, exitError(t)), mutation.StatusTimedOut)
	testutil.Equal(t, sortedKeys(map[string]int{"b": 1, "a": 2}), []string{"a", "b"})
	_, err = os.Stat(w.ws.Report(CoverageReport))
	testutil.NoErr(t, err)
}
