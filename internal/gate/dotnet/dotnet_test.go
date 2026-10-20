package dotnet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/junit"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/linecov"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/packtest"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/tokmut"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
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
	flagResults = "--results-directory"
)

var errSim = packtest.ErrSim

var dotnetPolicy = policy.Policy{
	Thresholds:   policy.Thresholds{LineCoverage: 50, BranchCoverage: 50, MaxCognitive: 9, MutationScore: 90},
	Limits:       policy.Limits{FuzzExamples: 400, MutantFloor: time.Second, MutantFactor: 2},
	Dependencies: []string{"nuget:XUnit", "nuget:cscheck", "go.example/x"},
}

type world struct {
	*packtest.World
	trx       string
	cobertura string
}

func newWorld(t *testing.T, files map[string]string) *world {
	t.Helper()
	w := &world{World: packtest.New(t, files, dotnetPolicy)}
	w.Hook = w.hook
	for _, name := range []string{beforeProps, afterTargets, editorConfig} {
		testutil.Write(t, filepath.Join(w.WS.PolicyDir, policySub, name), name)
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

func (w *world) hook(c procexec.Command, line string) (procexec.Result, error) {
	w.writeReports(c)
	if w.Kill != nil && strings.HasPrefix(c.Dir, w.WS.Report(tokmut.SandboxPrefix)) && strings.Contains(line, "build --no-restore") && w.Broken(w.Read(fileCore, c)) {
		return procexec.Result{}, errSim
	}
	if w.Kill != nil && strings.Contains(line, "test --no-build") {
		return procexec.Result{}, w.Kill(w.Read(fileCore, c))
	}
	return procexec.Result{}, packtest.ErrPass
}

func (w *world) writeReports(c procexec.Command) {
	for i, a := range c.Args {
		if trx, ok := strings.CutPrefix(a, "trx;LogFileName="); ok && w.trx != "" {
			testutil.Write(w.T, trx, w.trx)
		}
		if a == flagResults && w.cobertura != "" {
			w.writeCobertura(c.Args[i+1])
		}
	}
}

func (w *world) writeCobertura(dir string) {
	for _, run := range []string{"run-1", "run-2"} {
		testutil.Write(w.T, filepath.Join(dir, run, coberturaFile), strings.ReplaceAll(w.cobertura, "STAGE", stage(w.WS)))
	}
}

func TestPrepareNeutralizesProjectSettings(t *testing.T) {
	w := solution(t)
	testutil.NoErr(t, prepare(context.Background(), w.WS))
	lines := w.Fake.Lines()
	testutil.Equal(t, lines[1], "dotnet restore --locked-mode -v q")
	testutil.Equal(t, lines[2], "dotnet restore -v q "+strings.Join(analysisProps(w.WS), " "))
	testutil.Equal(t, w.Fake.Calls()[1].Env, baseEnv)
	testutil.Equal(t, testutil.Read(t, filepath.Join(stage(w.WS), editorFile)), editorConfig)
	testutil.Missing(t, filepath.Join(stage(w.WS), "src", ".globalconfig"))
	testutil.Missing(t, filepath.Join(stage(w.WS), filepath.FromSlash(lockCore)))
	testutil.Equal(t, testutil.Read(t, config(w.WS, beforeProps)), beforeProps)
	testutil.Contains(t, testutil.Read(t, config(w.WS, sonarLint)), "<Value>9</Value>")
	testutil.Equal(t, testutil.Read(t, w.WS.Report(readyStamp)), readyStamp)
	testutil.NoErr(t, prepare(context.Background(), w.WS))
	testutil.Equal(t, len(w.Fake.Lines()), 3)
}

func TestPrepareFailures(t *testing.T) {
	for _, step := range []string{cmdLocked, "restore -v q"} {
		w := solution(t)
		w.On(step, "", errSim)
		testutil.Contains(t, prepare(context.Background(), w.WS).Error(), "dotnet restore")
	}
	noPolicy := solution(t)
	testutil.NoErr(t, os.RemoveAll(filepath.Join(noPolicy.WS.PolicyDir, policySub, editorConfig)))
	testutil.Fails(t, prepare(context.Background(), noPolicy.WS))
	noTargets := solution(t)
	testutil.NoErr(t, os.RemoveAll(filepath.Join(noTargets.WS.PolicyDir, policySub, afterTargets)))
	testutil.Fails(t, prepare(context.Background(), noTargets.WS))
	gone := solution(t)
	gone.Files = append(gone.Files, "missing.cs")
	testutil.Fails(t, prepare(context.Background(), gone.WS))
	blocked := solution(t)
	testutil.Write(t, blocked.WS.Report(configDir), "")
	testutil.Fails(t, prepare(context.Background(), blocked.WS))
	sonar := solution(t)
	testutil.Mkdir(t, config(sonar.WS, sonarLint))
	testutil.Fails(t, prepare(context.Background(), sonar.WS))
	testutil.Fails(t, neutralize(filepath.Join(t.TempDir(), "missing")))
	testutil.Fails(t, copyFile(filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "x")))
	testutil.Fails(t, copyFile(filepath.Join(sonar.WS.PolicyDir, policySub, beforeProps), filepath.Join(t.TempDir(), "missing", "x")))
}

func TestIntegrityAndDependencies(t *testing.T) {
	w := newWorld(t, map[string]string{projCore: "", projTests: "", lockTests: lockJSON})
	testutil.Equal(t, Integrity(context.Background(), w.WS).Findings, []string{"src/Core/Core.csproj: no packages.lock.json next to it (set RestorePackagesWithLockFile)"})
	good := solution(t)
	testutil.Equal(t, Integrity(context.Background(), good.WS).Status, gate.StatusPass)
	locked := solution(t)
	locked.On(cmdLocked, "", errSim)
	testutil.Contains(t, Integrity(context.Background(), locked.WS).Findings[0], "dotnet restore")
	deps := Dependencies(context.Background(), good.WS)
	testutil.Equal(t, deps.Findings, []string{"nuget:Rogue.Lib is not in the approved dependency list"})
	testutil.Equal(t, deps.Values[pack.ValueDependencies], float64(3))
	broken := newWorld(t, map[string]string{lockCore: "{"})
	testutil.Contains(t, Dependencies(context.Background(), broken.WS).Reason, lockCore)
	unlisted := solution(t)
	unlisted.FailGit()
	testutil.Contains(t, Integrity(context.Background(), unlisted.WS).Reason, "git ls-files")
	testutil.Contains(t, Dependencies(context.Background(), unlisted.WS).Reason, "git ls-files")
}

const buildOutput = `  STAGE/src/Core/Pricing.cs(5,52): error S109: Assign this magic number '3' to a well-named variable or constant. (https://rules.sonarsource.com) [STAGE/src/Core/Core.csproj]
  STAGE/src/Core/Pricing.cs(5,52): error S109: Assign this magic number '3' to a well-named variable or constant. (https://rules.sonarsource.com) [STAGE/src/Core/Core.csproj]
  STAGE/src/Core/Pricing.cs(7,5): error S3776: Refactor this method to reduce its Cognitive Complexity from 17 to the 9 allowed. [STAGE/src/Core/Core.csproj]
  STAGE/src/Core/Pricing.cs(9,5): warning IDE0051: Private member 'Pricing.Unused' is unused
noise line
`

func TestAnalysisFeedsThreeChecks(t *testing.T) {
	w := solution(t)
	testutil.NoErr(t, prepare(context.Background(), w.WS))
	w.On(cmdAnalysis, strings.ReplaceAll(buildOutput, "STAGE", stage(w.WS)), errSim)
	w.On(cmdFormat, "  "+stage(w.WS)+"/src/Core/Pricing.cs(3,1): error WHITESPACE: Fix whitespace formatting.\n  "+stage(w.WS)+"/src/Core/Pricing.cs(4,1): error WHITESPACE: Fix whitespace formatting.\n", errSim)
	testutil.Write(t, filepath.Join(w.WS.Src, filepath.FromSlash(fileCore)), coreSource+"#pragma warning disable S109\n[ExcludeFromCodeCoverage]\n")
	lint := Lint(context.Background(), w.WS)
	testutil.Equal(t, lint.Findings, []string{
		"src/Core/Pricing.cs:5 S109 Assign this magic number '3' to a well-named variable or constant. (https://rules.sonarsource.com)",
		"src/Core/Pricing.cs: not formatted",
		"src/Core/Pricing.cs:12 analyzer suppression: #pragma warning disable",
		"src/Core/Pricing.cs:13 analyzer suppression: ExcludeFromCodeCoverage",
	})
	testutil.Equal(t, Complexity(context.Background(), w.WS).Findings, []string{"src/Core/Pricing.cs:7 S3776 Refactor this method to reduce its Cognitive Complexity from 17 to the 9 allowed."})
	testutil.Equal(t, DeadCode(context.Background(), w.WS).Findings, []string{"src/Core/Pricing.cs:9 IDE0051 Private member 'Pricing.Unused' is unused"})
	testutil.Equal(t, w.Fake.Count(cmdAnalysis), 1)
	format, _ := w.Fake.Find(cmdFormat)
	testutil.Equal(t, format.Args, []string{"format", "whitespace", "--verify-no-changes", flagVerbosity, verbosityQuiet})
}

func TestAnalysisFailures(t *testing.T) {
	crashed := solution(t)
	crashed.On(cmdAnalysis, "MSBUILD : error MSB1009: Project file does not exist.", errSim)
	testutil.Contains(t, Lint(context.Background(), crashed.WS).Reason, "dotnet build")
	testutil.Contains(t, Complexity(context.Background(), crashed.WS).Reason, "dotnet build")
	testutil.Contains(t, DeadCode(context.Background(), crashed.WS).Reason, "dotnet build")
	formatCrash := solution(t)
	formatCrash.On(cmdFormat, "", errSim)
	testutil.Contains(t, Lint(context.Background(), formatCrash.WS).Reason, "dotnet format")
	unprepared := solution(t)
	unprepared.On(cmdLocked, "", errSim)
	testutil.Contains(t, Lint(context.Background(), unprepared.WS).Reason, "dotnet restore")
	cached := solution(t)
	testutil.Write(t, cached.WS.Report(diagnosticsReport), "{")
	testutil.Equal(t, Lint(context.Background(), cached.WS).Status, gate.StatusFail)
	unreadable := solution(t)
	testutil.NoErr(t, prepare(context.Background(), unreadable.WS))
	unreadable.Files = append(unreadable.Files, "src/gone.cs")
	testutil.Equal(t, Lint(context.Background(), unreadable.WS).Status, gate.StatusFail)
	clean := solution(t)
	testutil.Equal(t, Lint(context.Background(), clean.WS).Status, gate.StatusPass)
	blocked := solution(t)
	testutil.NoErr(t, prepare(context.Background(), blocked.WS))
	testutil.Mkdir(t, blocked.WS.Report(diagnosticsReport))
	testutil.NoErr(t, os.WriteFile(filepath.Join(blocked.WS.Report(diagnosticsReport), "x"), nil, stampPerm))
	testutil.Equal(t, Lint(context.Background(), blocked.WS).Status, gate.StatusFail)
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
	out := Tests(context.Background(), w.WS)
	testutil.Equal(t, out.Status, gate.StatusPass)
	run, _ := w.Fake.Find(cmdTestRun)
	testutil.Equal(t, run.Args[:7], []string{cmdTest, flagNoRestore, flagResults, w.WS.Report(resultsDir), flagLogger, "trx;LogFileName=" + w.WS.Report(TRXReport), cmdTestRun})
	testutil.Equal(t, run.Args[len(run.Args)-2:], quietAnalyzers)
	lines := linecov.LineCoverage(context.Background(), w.WS)
	testutil.Equal(t, lines.Values, map[string]float64{linecov.ValueLineCoverage: float64(2) * 100 / 3, linecov.ValueLines: 3})
	testutil.Equal(t, lines.Findings, []string{"src/Core/Pricing.cs: 1 of 2 lines uncovered"})
	branches := linecov.Branches(context.Background(), w.WS)
	testutil.Equal(t, branches.Values, map[string]float64{linecov.ValueBranchCover: 50, linecov.ValueBranches: 2})
	passed, err := Passed(context.Background(), w.WS)
	testutil.NoErr(t, err)
	testutil.Equal(t, passed, map[string]bool{"Core.Tests.PricingTests.Above": true})
}

func TestTestFailures(t *testing.T) {
	silent := solution(t)
	silent.On("test --no-restore", "", errSim)
	testutil.Contains(t, Tests(context.Background(), silent.WS).Reason, reasonNoReport)
	unprepared := solution(t)
	unprepared.On(cmdLocked, "", errSim)
	testutil.Contains(t, Tests(context.Background(), unprepared.WS).Reason, "dotnet restore")
	badCoverage := solution(t)
	badCoverage.trx = trxGreen
	badCoverage.cobertura = "<coverage"
	testutil.Equal(t, Tests(context.Background(), badCoverage.WS).Status, gate.StatusFail)
	blocked := solution(t)
	blocked.trx = trxGreen
	testutil.NoErr(t, prepare(context.Background(), blocked.WS))
	testutil.Mkdir(t, blocked.WS.Report(linecov.ReportFile))
	testutil.NoErr(t, os.WriteFile(filepath.Join(blocked.WS.Report(linecov.ReportFile), "x"), nil, stampPerm))
	testutil.Equal(t, Tests(context.Background(), blocked.WS).Status, gate.StatusFail)
	missing := solution(t)
	_, err := Passed(context.Background(), missing.WS)
	testutil.Fails(t, err)
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
	testutil.Equal(t, Fuzz(context.Background(), w.WS).Status, gate.StatusPass)
	run, _ := w.Fake.Find("fuzz-" + TRXReport)
	testutil.Equal(t, run.Env[len(run.Env)-1], envFuzzBudget+"400")
	failing := solution(t)
	failing.trx = `<TestRun><Results><UnitTestResult testName="A.prop" outcome="Failed"/></Results></TestRun>`
	testutil.Equal(t, Fuzz(context.Background(), failing.WS).Reason, reasonFuzz)
	noLibrary := newWorld(t, map[string]string{lockCore: `{"version":1,"dependencies":{"net10.0":{"xunit":{"type":"Direct"}}}}`})
	noLibrary.On(cmdLocked, "", nil)
	testutil.Equal(t, Fuzz(context.Background(), noLibrary.WS).Reason, reasonNoFuzz)
	silent := solution(t)
	testutil.Contains(t, Fuzz(context.Background(), silent.WS).Reason, reasonNoReport)
	unprepared := solution(t)
	unprepared.On(cmdLocked, "", errSim)
	testutil.Contains(t, Fuzz(context.Background(), unprepared.WS).Reason, "dotnet restore")
	brokenLock := newWorld(t, map[string]string{lockCore: "{"})
	testutil.Contains(t, Fuzz(context.Background(), brokenLock.WS).Reason, lockCore)
}

func TestPathHelpers(t *testing.T) {
	w := solution(t)
	testutil.Mkdir(t, stage(w.WS))
	testutil.Equal(t, rel(w.WS, filepath.Join(stage(w.WS), "a", "b.cs")), "a/b.cs")
	resolved, err := filepath.EvalSymlinks(stage(w.WS))
	testutil.NoErr(t, err)
	testutil.Equal(t, rel(w.WS, filepath.Join(resolved, "c.cs")), "c.cs")
	testutil.Equal(t, rel(w.WS, "/other/d.cs"), "/other/d.cs")
	for _, p := range []string{"tests/a.cs", "Shop.Tests/b.cs", "src/Shop.Test/c.cs", "test/x.cs"} {
		testutil.True(t, isTest(p), p)
	}
	testutil.True(t, !isTest("src/Testing/d.cs") && !isTest("src/Core/e.cs"), "production paths")
	testutil.True(t, isProject("a/b.fsproj") && !isProject("a/b.sln"), "project files")
	testutil.Equal(t, itoa(7), "7")
	testutil.Equal(t, firstLine("  one\ntwo"), "one")
}
