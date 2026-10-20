package dotnet

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/junit"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	gitBinary   = "git"
	fileCore    = "src/Core/Pricing.cs"
	projCore    = "src/Core/Core.csproj"
	lockCore    = "src/Core/packages.lock.json"
	projTests   = "tests/Core.Tests/Core.Tests.csproj"
	lockTests   = "tests/Core.Tests/packages.lock.json"
	testFile    = "tests/Core.Tests/PricingTests.cs"
	coreSource  = "namespace Core;\n\npublic static class Pricing\n{\n    public static bool Above(int value) => value > 3;\n\n    public static int Never(int value)\n    {\n        return value + 1;\n    }\n}\n"
	lockJSON    = `{"version":1,"dependencies":{"net10.0":{"xunit":{"type":"Direct"},"CsCheck":{"type":"Direct"},"Core":{"type":"Project"},"Rogue.Lib":{"type":"Transitive"}}}}`
	trxGreen    = `<TestRun><Results><UnitTestResult testName="Core.Tests.PricingTests.Above" outcome="Passed" duration="00:00:01.5000000"/></Results></TestRun>`
	cmdLocked   = "restore --locked-mode"
	cmdAnalysis = "build --no-restore -v q -clp:NoSummary"
	cmdFormat   = "format whitespace"
	cmdTestRun  = "--collect:XPlat Code Coverage"
	readmeFile  = "README.md"
	editorFile  = ".editorconfig"
)

var errSim = errors.New("simulated")

func exitError(t *testing.T) error {
	t.Helper()
	err := exec.CommandContext(context.Background(), "sh", "-c", "exit 1").Run()
	testutil.True(t, err != nil, "exit status")
	return err
}

type reply struct {
	out string
	err error
}

type world struct {
	t         *testing.T
	ws        gate.Workspace
	fake      *procexectest.Fake
	mu        sync.Mutex
	files     []string
	rules     map[string]reply
	trx       string
	cobertura string
	kill      func(src string) error
	broken    func(src string) bool
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
		Src: src, Out: t.TempDir(), PolicyDir: t.TempDir(), Exec: w.fake, Log: slog.New(slog.DiscardHandler),
		Policy: policy.Policy{
			Thresholds:   policy.Thresholds{LineCoverage: 50, BranchCoverage: 50, MaxCognitive: 9, MutationScore: 90},
			Limits:       policy.Limits{FuzzExamples: 400, MutantFloor: time.Second, MutantFactor: 2},
			Dependencies: []string{"nuget:XUnit", "nuget:cscheck", "go.example/x"},
		},
	}
	for _, name := range []string{beforeProps, afterTargets, editorConfig} {
		testutil.Write(t, filepath.Join(w.ws.PolicyDir, policySub, name), name)
	}
	return w
}

func solution(t *testing.T) *world {
	t.Helper()
	return newWorld(t, map[string]string{
		"App.slnx": "", projCore: "", lockCore: lockJSON, projTests: "", lockTests: lockJSON, fileCore: coreSource, testFile: "x",
		editorFile: "app rules", "src/.globalconfig": "x", readmeFile: "",
	})
}

func (w *world) on(substr, out string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rules[substr] = reply{out: out, err: err}
}

func (w *world) handle(c procexec.Command) (procexec.Result, error) {
	line := procexectest.Line(c)
	if c.Name == gitBinary {
		return procexec.Result{Stdout: strings.Join(w.files, "\x00") + "\x00"}, nil
	}
	w.writeReports(c)
	if w.kill != nil && strings.HasPrefix(c.Dir, w.ws.Report(sandboxPrefix)) && strings.Contains(line, "build --no-restore") {
		if w.broken(testutil.Read(w.t, filepath.Join(c.Dir, filepath.FromSlash(fileCore)))) {
			return procexec.Result{}, errSim
		}
	}
	if w.kill != nil && strings.Contains(line, "test --no-build") {
		return procexec.Result{}, w.kill(testutil.Read(w.t, filepath.Join(c.Dir, filepath.FromSlash(fileCore))))
	}
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

func (w *world) writeReports(c procexec.Command) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, a := range c.Args {
		if trx, ok := strings.CutPrefix(a, "trx;LogFileName="); ok && w.trx != "" {
			testutil.Write(w.t, trx, w.trx)
		}
		if a == "--results-directory" && w.cobertura != "" {
			testutil.Write(w.t, filepath.Join(c.Args[i+1], "run-1", coberturaFile), strings.ReplaceAll(w.cobertura, "STAGE", stage(w.ws)))
			testutil.Write(w.t, filepath.Join(c.Args[i+1], "run-2", coberturaFile), strings.ReplaceAll(w.cobertura, "STAGE", stage(w.ws)))
		}
	}
}

func TestPrepareNeutralizesProjectSettings(t *testing.T) {
	w := solution(t)
	testutil.NoErr(t, prepare(context.Background(), w.ws))
	lines := w.fake.Lines()
	testutil.Equal(t, lines[1], "dotnet restore --locked-mode -v q")
	testutil.Equal(t, lines[2], "dotnet restore -v q "+strings.Join(analysisProps(w.ws), " "))
	testutil.Equal(t, w.fake.Calls()[1].Env, baseEnv)
	testutil.Equal(t, testutil.Read(t, filepath.Join(stage(w.ws), editorFile)), editorConfig)
	testutil.Missing(t, filepath.Join(stage(w.ws), "src", ".globalconfig"))
	testutil.Missing(t, filepath.Join(stage(w.ws), filepath.FromSlash(lockCore)))
	testutil.Equal(t, testutil.Read(t, config(w.ws, beforeProps)), beforeProps)
	testutil.Contains(t, testutil.Read(t, config(w.ws, sonarLint)), "<Value>9</Value>")
	testutil.Equal(t, testutil.Read(t, w.ws.Report(readyStamp)), Stack)
	testutil.NoErr(t, prepare(context.Background(), w.ws))
	testutil.Equal(t, len(w.fake.Lines()), 3)
}

func TestPrepareFailures(t *testing.T) {
	for _, step := range []string{cmdLocked, "restore -v q"} {
		w := solution(t)
		w.on(step, "", errSim)
		testutil.Contains(t, prepare(context.Background(), w.ws).Error(), "dotnet restore")
	}
	noPolicy := solution(t)
	testutil.NoErr(t, os.RemoveAll(filepath.Join(noPolicy.ws.PolicyDir, policySub, editorConfig)))
	testutil.Fails(t, prepare(context.Background(), noPolicy.ws))
	noTargets := solution(t)
	testutil.NoErr(t, os.RemoveAll(filepath.Join(noTargets.ws.PolicyDir, policySub, afterTargets)))
	testutil.Fails(t, prepare(context.Background(), noTargets.ws))
	gone := solution(t)
	gone.files = append(gone.files, "missing.cs")
	testutil.Fails(t, prepare(context.Background(), gone.ws))
	blocked := solution(t)
	testutil.Write(t, blocked.ws.Report(configDir), "")
	testutil.Fails(t, prepare(context.Background(), blocked.ws))
	sonar := solution(t)
	testutil.Mkdir(t, config(sonar.ws, sonarLint))
	testutil.Fails(t, prepare(context.Background(), sonar.ws))
	testutil.Fails(t, neutralize(filepath.Join(t.TempDir(), "missing")))
	testutil.Fails(t, copyFile(filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "x")))
	testutil.Fails(t, copyFile(filepath.Join(sonar.ws.PolicyDir, policySub, beforeProps), filepath.Join(t.TempDir(), "missing", "x")))
}

func TestIntegrityAndDependencies(t *testing.T) {
	w := newWorld(t, map[string]string{projCore: "", projTests: "", lockTests: lockJSON})
	testutil.Equal(t, Integrity(context.Background(), w.ws).Findings, []string{"src/Core/Core.csproj: no packages.lock.json next to it (set RestorePackagesWithLockFile)"})
	good := solution(t)
	testutil.Equal(t, Integrity(context.Background(), good.ws).Status, gate.StatusPass)
	locked := solution(t)
	locked.on(cmdLocked, "", errSim)
	testutil.Contains(t, Integrity(context.Background(), locked.ws).Findings[0], "dotnet restore")
	deps := Dependencies(context.Background(), good.ws)
	testutil.Equal(t, deps.Findings, []string{"nuget:Rogue.Lib is not in the approved dependency list"})
	testutil.Equal(t, deps.Values[ValueDependencies], float64(3))
	broken := newWorld(t, map[string]string{lockCore: "{"})
	testutil.Contains(t, Dependencies(context.Background(), broken.ws).Reason, lockCore)
	unlisted := solution(t)
	unlisted.fake.Handler = func(c procexec.Command) (procexec.Result, error) { return procexec.Result{}, errSim }
	testutil.Contains(t, Integrity(context.Background(), unlisted.ws).Reason, "git ls-files")
	testutil.Contains(t, Dependencies(context.Background(), unlisted.ws).Reason, "git ls-files")
}

const buildOutput = `  STAGE/src/Core/Pricing.cs(5,52): error S109: Assign this magic number '3' to a well-named variable or constant. (https://rules.sonarsource.com) [STAGE/src/Core/Core.csproj]
  STAGE/src/Core/Pricing.cs(5,52): error S109: Assign this magic number '3' to a well-named variable or constant. (https://rules.sonarsource.com) [STAGE/src/Core/Core.csproj]
  STAGE/src/Core/Pricing.cs(7,5): error S3776: Refactor this method to reduce its Cognitive Complexity from 17 to the 9 allowed. [STAGE/src/Core/Core.csproj]
  STAGE/src/Core/Pricing.cs(9,5): warning IDE0051: Private member 'Pricing.Unused' is unused
noise line
`

func TestAnalysisFeedsThreeChecks(t *testing.T) {
	w := solution(t)
	testutil.NoErr(t, prepare(context.Background(), w.ws))
	w.on(cmdAnalysis, strings.ReplaceAll(buildOutput, "STAGE", stage(w.ws)), errSim)
	w.on(cmdFormat, "  "+stage(w.ws)+"/src/Core/Pricing.cs(3,1): error WHITESPACE: Fix whitespace formatting.\n  "+stage(w.ws)+"/src/Core/Pricing.cs(4,1): error WHITESPACE: Fix whitespace formatting.\n", errSim)
	testutil.Write(t, filepath.Join(w.ws.Src, filepath.FromSlash(fileCore)), coreSource+"#pragma warning disable S109\n[ExcludeFromCodeCoverage]\n")
	lint := Lint(context.Background(), w.ws)
	testutil.Equal(t, lint.Findings, []string{
		"src/Core/Pricing.cs:5 S109 Assign this magic number '3' to a well-named variable or constant. (https://rules.sonarsource.com)",
		"src/Core/Pricing.cs: not formatted",
		"src/Core/Pricing.cs:12 analyzer suppression: #pragma warning disable",
		"src/Core/Pricing.cs:13 analyzer suppression: ExcludeFromCodeCoverage",
	})
	testutil.Equal(t, Complexity(context.Background(), w.ws).Findings, []string{"src/Core/Pricing.cs:7 S3776 Refactor this method to reduce its Cognitive Complexity from 17 to the 9 allowed."})
	testutil.Equal(t, DeadCode(context.Background(), w.ws).Findings, []string{"src/Core/Pricing.cs:9 IDE0051 Private member 'Pricing.Unused' is unused"})
	testutil.Equal(t, w.fake.Count(cmdAnalysis), 1)
	format, _ := w.fake.Find(cmdFormat)
	testutil.Equal(t, format.Args, []string{"format", "whitespace", "--verify-no-changes", flagVerbosity, verbosityQuiet})
}

func TestAnalysisFailures(t *testing.T) {
	crashed := solution(t)
	crashed.on(cmdAnalysis, "MSBUILD : error MSB1009: Project file does not exist.", errSim)
	testutil.Contains(t, Lint(context.Background(), crashed.ws).Reason, "dotnet build")
	testutil.Contains(t, Complexity(context.Background(), crashed.ws).Reason, "dotnet build")
	testutil.Contains(t, DeadCode(context.Background(), crashed.ws).Reason, "dotnet build")
	formatCrash := solution(t)
	formatCrash.on(cmdFormat, "", errSim)
	testutil.Contains(t, Lint(context.Background(), formatCrash.ws).Reason, "dotnet format")
	unprepared := solution(t)
	unprepared.on(cmdLocked, "", errSim)
	testutil.Contains(t, Lint(context.Background(), unprepared.ws).Reason, "dotnet restore")
	cached := solution(t)
	testutil.Write(t, cached.ws.Report(diagnosticsReport), "{")
	testutil.Equal(t, Lint(context.Background(), cached.ws).Status, gate.StatusFail)
	unreadable := solution(t)
	testutil.NoErr(t, prepare(context.Background(), unreadable.ws))
	unreadable.files = append(unreadable.files, "src/gone.cs")
	testutil.Equal(t, Lint(context.Background(), unreadable.ws).Status, gate.StatusFail)
	clean := solution(t)
	testutil.Equal(t, Lint(context.Background(), clean.ws).Status, gate.StatusPass)
	blocked := solution(t)
	testutil.NoErr(t, prepare(context.Background(), blocked.ws))
	testutil.Mkdir(t, blocked.ws.Report(diagnosticsReport))
	testutil.NoErr(t, os.WriteFile(filepath.Join(blocked.ws.Report(diagnosticsReport), "x"), nil, stampPerm))
	testutil.Equal(t, Lint(context.Background(), blocked.ws).Status, gate.StatusFail)
}

const coberturaReport = `<coverage><sources><source>/nowhere</source><source>STAGE</source></sources><packages><package><classes>
<class filename="src/Core/Pricing.cs"><lines>
<line number="5" hits="3" condition-coverage="50% (1/2)"/>
<line number="9" hits="0"/>
</lines></class>
<class filename="src/Core/Gone.cs"><lines><line number="1" hits="1"/></lines></class>
</classes></package></packages></coverage>`

func TestTestsAndCoverage(t *testing.T) {
	w := solution(t)
	w.trx = trxGreen
	w.cobertura = coberturaReport
	out := Tests(context.Background(), w.ws)
	testutil.Equal(t, out.Status, gate.StatusPass)
	run, _ := w.fake.Find(cmdTestRun)
	testutil.Equal(t, run.Args[:7], []string{"test", flagNoRestore, "--results-directory", w.ws.Report(resultsDir), flagLogger, "trx;LogFileName=" + w.ws.Report(TRXReport), cmdTestRun})
	testutil.Equal(t, run.Args[len(run.Args)-2:], quietAnalyzers)
	lines := LineCoverage(context.Background(), w.ws)
	testutil.Equal(t, lines.Values, map[string]float64{ValueLineCoverage: float64(2) * 100 / 3, ValueLines: 3})
	testutil.Equal(t, lines.Findings, []string{"src/Core/Pricing.cs: 1 of 2 lines uncovered"})
	branches := Branches(context.Background(), w.ws)
	testutil.Equal(t, branches.Values, map[string]float64{ValueBranchCover: 50, ValueBranches: 2})
	passed, err := Passed(context.Background(), w.ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, passed, map[string]bool{"Core.Tests.PricingTests.Above": true})
}

func TestTestFailures(t *testing.T) {
	silent := solution(t)
	silent.on("test --no-restore", "", errSim)
	testutil.Contains(t, Tests(context.Background(), silent.ws).Reason, reasonNoReport)
	unprepared := solution(t)
	unprepared.on(cmdLocked, "", errSim)
	testutil.Contains(t, Tests(context.Background(), unprepared.ws).Reason, "dotnet restore")
	badCoverage := solution(t)
	badCoverage.trx = trxGreen
	badCoverage.cobertura = "<coverage"
	testutil.Equal(t, Tests(context.Background(), badCoverage.ws).Status, gate.StatusFail)
	blocked := solution(t)
	blocked.trx = trxGreen
	testutil.NoErr(t, prepare(context.Background(), blocked.ws))
	testutil.Mkdir(t, blocked.ws.Report(CoverageReport))
	testutil.NoErr(t, os.WriteFile(filepath.Join(blocked.ws.Report(CoverageReport), "x"), nil, stampPerm))
	testutil.Equal(t, Tests(context.Background(), blocked.ws).Status, gate.StatusFail)
	missing := solution(t)
	testutil.Contains(t, LineCoverage(context.Background(), missing.ws).Reason, "coverage report missing")
	testutil.Contains(t, Branches(context.Background(), missing.ws).Reason, "coverage report missing")
	_, err := Passed(context.Background(), missing.ws)
	testutil.Fails(t, err)
	testutil.Equal(t, ratio(0, 0), float64(percentScale))
}

func TestTRXOutcomes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "r.trx")
	testutil.Write(t, file, `<TestRun><Results>
<UnitTestResult testName="A.ok" outcome="Passed" duration="00:01:02.5"/>
<UnitTestResult testName="A.bad" outcome="Failed"><Output><ErrorInfo><Message>Assert.Equal() Failure
Expected: 1</Message></ErrorInfo></Output></UnitTestResult>
<UnitTestResult testName="A.skip" outcome="NotExecuted"/>
</Results></TestRun>`)
	cases, err := loadTRX(file)
	testutil.NoErr(t, err)
	testutil.Equal(t, junit.Judge(cases, testName).Findings, []string{"A.bad failed: Assert.Equal() Failure", "A.skip skipped: NotExecuted"})
	testutil.Equal(t, cases[0].Time, 62.5)
	testutil.Equal(t, seconds("bogus"), float64(0))
}

func TestFuzz(t *testing.T) {
	w := solution(t)
	w.trx = trxGreen
	testutil.Equal(t, Fuzz(context.Background(), w.ws).Status, gate.StatusPass)
	run, _ := w.fake.Find("fuzz-" + TRXReport)
	testutil.Equal(t, run.Env[len(run.Env)-1], envFuzzBudget+"400")
	failing := solution(t)
	failing.trx = `<TestRun><Results><UnitTestResult testName="A.prop" outcome="Failed"/></Results></TestRun>`
	testutil.Equal(t, Fuzz(context.Background(), failing.ws).Reason, reasonFuzz)
	noLibrary := newWorld(t, map[string]string{lockCore: `{"version":1,"dependencies":{"net10.0":{"xunit":{"type":"Direct"}}}}`})
	noLibrary.on(cmdLocked, "", nil)
	testutil.Equal(t, Fuzz(context.Background(), noLibrary.ws).Reason, reasonNoFuzz)
	silent := solution(t)
	testutil.Contains(t, Fuzz(context.Background(), silent.ws).Reason, reasonNoReport)
	unprepared := solution(t)
	unprepared.on(cmdLocked, "", errSim)
	testutil.Contains(t, Fuzz(context.Background(), unprepared.ws).Reason, "dotnet restore")
	brokenLock := newWorld(t, map[string]string{lockCore: "{"})
	testutil.Contains(t, Fuzz(context.Background(), brokenLock.ws).Reason, lockCore)
}

func TestPathHelpers(t *testing.T) {
	w := solution(t)
	testutil.Mkdir(t, stage(w.ws))
	testutil.Equal(t, rel(w.ws, filepath.Join(stage(w.ws), "a", "b.cs")), "a/b.cs")
	resolved, err := filepath.EvalSymlinks(stage(w.ws))
	testutil.NoErr(t, err)
	testutil.Equal(t, rel(w.ws, filepath.Join(resolved, "c.cs")), "c.cs")
	testutil.Equal(t, rel(w.ws, "/other/d.cs"), "/other/d.cs")
	for _, p := range []string{"tests/a.cs", "Shop.Tests/b.cs", "src/Shop.Test/c.cs", "test/x.cs"} {
		testutil.True(t, isTest(p), p)
	}
	testutil.True(t, !isTest("src/Testing/d.cs") && !isTest("src/Core/e.cs"), "production paths")
	testutil.True(t, isProject("a/b.fsproj") && !isProject("a/b.sln"), "project files")
	testutil.Equal(t, itoa(7), "7")
	testutil.Equal(t, firstLine("  one\ntwo"), "one")
}
