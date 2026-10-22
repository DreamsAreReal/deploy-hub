package python

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/junit"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/linecov"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/pack"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/packtest"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	fileApp    = "src/shop/app.py"
	fileTest   = "tests/test_app.py"
	testOK     = "tests/test_app.py::test_ok"
	cmdImport  = "-c " + testImports
	cmdMypy    = "-m mypy"
	cmdVulture = "-m vulture"
	cmdCovRun  = "coverage run"
	cmdCovJSON = "coverage json"
	cmdCompile = "-m py_compile"
	cmdPytest  = "-m pytest"
	cmdCplx    = "-- complexipy"
	cmdLock    = "lock --check"
	cmdExport  = "export"
	cmdSync    = "sync --quiet --locked"
	cmdVenv    = "venv --quiet --clear"
	cmdInstall = "pip install"
	cmdRuffChk = "ruff check"
	cmdRuffFmt = "ruff format"
	appSource  = "LIMIT = 3\n\n\ndef above(value: int) -> bool:\n    return value > LIMIT\n\n\ndef never(value: int) -> int:\n    return value + 1\n"
	gitBinary  = "git"
	reqsFile   = "requirements.txt"
	scriptFile = "run.py"
	otherTest  = "other"
	readmeFile = "README.md"
	junitGreen = `<testsuites><testsuite><testcase classname="tests.test_app" name="test_ok" file="tests/test_app.py" time="0.5"/></testsuite></testsuites>`
)

var errSim = packtest.ErrSim

var pythonPolicy = policy.Policy{
	Thresholds:   policy.Thresholds{LineCoverage: 90, BranchCoverage: 90, MaxCognitive: 9, MutationScore: 90},
	Limits:       policy.Limits{FuzzExamples: 300, MutantFloor: time.Second, MutantFactor: 2},
	Dependencies: []string{"pypi:pytest", "pypi:Foo_Bar", "example.com/go/module"},
}

func newWorld(t *testing.T, files map[string]string) *world {
	t.Helper()
	w := &world{World: packtest.New(t, files, pythonPolicy)}
	w.WS.PolicyDir = "/policy"
	w.Hook = w.hook
	return w
}

type world struct {
	*packtest.World
}

func (w *world) hook(c procexec.Command, line string) (procexec.Result, error) {
	if strings.Contains(line, cmdPytest) && w.Kill != nil && !strings.Contains(line, "hypothesis") {
		return procexec.Result{}, w.Kill(w.Read(fileApp, c))
	}
	if strings.Contains(line, cmdCompile) && w.Broken != nil && w.Broken(testutil.Read(w.T, c.Args[len(c.Args)-1])) {
		return procexec.Result{Stderr: "SyntaxError"}, errSim
	}
	return procexec.Result{}, packtest.ErrPass
}

func TestDetectLayouts(t *testing.T) {
	dir := t.TempDir()
	kind, reqs, err := detect(dir)
	testutil.True(t, kind == layoutLoose && reqs == nil && err == nil, "empty dir is loose")
	testutil.Write(t, filepath.Join(dir, reqsFile), "")
	testutil.Write(t, filepath.Join(dir, "requirements-dev.txt"), "")
	testutil.Write(t, filepath.Join(dir, "notes.txt"), "")
	testutil.Mkdir(t, filepath.Join(dir, "requirements-dir.txt"))
	kind, reqs, err = detect(dir)
	testutil.NoErr(t, err)
	testutil.Equal(t, kind, layoutRequirements)
	testutil.Equal(t, reqs, []string{filepath.Join(dir, "requirements-dev.txt"), filepath.Join(dir, reqsFile)})
	testutil.Write(t, filepath.Join(dir, ProjectFile), "")
	kind, _, err = detect(dir)
	testutil.Equal(t, kind, layoutUnlocked)
	testutil.Is(t, err, ErrNoLock)
	testutil.Write(t, filepath.Join(dir, lockFile), "")
	kind, _, err = detect(dir)
	testutil.True(t, kind == layoutUV && err == nil, "uv project")
	_, _, err = detect(filepath.Join(dir, "missing"))
	testutil.Fails(t, err)
	testutil.Equal(t, venvPython("/v"), filepath.Join("/v", "bin", "python"))
}

func TestPrepareUVProjectOnce(t *testing.T) {
	w := newWorld(t, map[string]string{lockFile: "", ProjectFile: "", fileApp: appSource})
	testutil.NoErr(t, prepare(context.Background(), w.WS))
	lines := w.Fake.Lines()
	testutil.Equal(t, len(lines), 4)
	testutil.Contains(t, lines[0], "uv sync --quiet --locked --all-groups --all-extras")
	testutil.Contains(t, lines[1], "uv run --quiet --no-project --python "+venvPython(w.WS.Report(venvDir))+" -- python "+cmdImport)
	testutil.Equal(t, lines[2], "uv "+cmdVenv+" "+w.WS.Report(toolsDir))
	testutil.Equal(t, lines[3], "uv pip install --quiet --python "+venvPython(w.WS.Report(toolsDir))+" --require-hashes -r /policy/python/tools-lock.txt")
	sync := w.Fake.Calls()[0]
	testutil.Equal(t, sync.Env, []string{envCache + w.WS.Report(cacheDir), envNoProg, envProject + w.WS.Report(venvDir)})
	testutil.Equal(t, sync.Dir, w.WS.Src)
	testutil.Equal(t, testutil.Read(t, w.WS.Report(readyStamp)), Stack)
	testutil.NoErr(t, prepare(context.Background(), w.WS))
	testutil.Equal(t, len(w.Fake.Lines()), 4)
}

func TestPrepareRequirementsProject(t *testing.T) {
	w := newWorld(t, map[string]string{reqsFile: "", "requirements-dev.txt": "", fileApp: appSource})
	w.On(cmdImport, "", errSim)
	testutil.NoErr(t, prepare(context.Background(), w.WS))
	lines := w.Fake.Lines()
	testutil.Equal(t, lines[0], "uv "+cmdVenv+" "+w.WS.Report(venvDir))
	testutil.Contains(t, lines[1], "--require-hashes -r "+filepath.Join(w.WS.Src, "requirements-dev.txt"))
	testutil.Contains(t, lines[2], "--require-hashes -r "+filepath.Join(w.WS.Src, reqsFile))
	testutil.Contains(t, lines[4], "-r /policy/python/test-lock.txt")
}

func TestPrepareFailures(t *testing.T) {
	for _, step := range []string{cmdSync, cmdVenv, cmdInstall} {
		w := newWorld(t, map[string]string{lockFile: ""})
		w.On(step, "", errSim)
		err := prepare(context.Background(), w.WS)
		testutil.Contains(t, err.Error(), "simulated")
		testutil.Missing(t, w.WS.Report(readyStamp))
	}
	unlocked := newWorld(t, map[string]string{ProjectFile: ""})
	testutil.Is(t, prepare(context.Background(), unlocked.WS), ErrNoLock)
	loose := newWorld(t, map[string]string{scriptFile: ""})
	loose.On(cmdVenv, "", errSim)
	testutil.Fails(t, prepare(context.Background(), loose.WS))
	reqs := newWorld(t, map[string]string{reqsFile: ""})
	reqs.On(cmdInstall, "", errSim)
	testutil.Contains(t, prepare(context.Background(), reqs.WS).Error(), "uv pip")
	blocked := newWorld(t, map[string]string{scriptFile: ""})
	testutil.Mkdir(t, blocked.WS.Report(readyStamp+"x"))
	blocked.WS.Out = filepath.Join(t.TempDir(), "missing")
	testutil.Fails(t, prepare(context.Background(), blocked.WS))
}

func TestIntegrity(t *testing.T) {
	reqs := "pytest==9.1.1 \\\n    --hash=sha256:aa\nloose>=1\nnohash==2\n-r other.txt\nextra[x]==1 --hash=sha256:bb\n\n"
	w := newWorld(t, map[string]string{reqsFile: reqs})
	out := Integrity(context.Background(), w.WS)
	testutil.Equal(t, out.Findings, []string{
		"requirements.txt: loose is not pinned to one version with hashes",
		"requirements.txt: nohash is not pinned to one version with hashes",
	})
	testutil.Equal(t, w.Fake.Count("uv sync"), 0)
	locked := newWorld(t, map[string]string{lockFile: ""})
	locked.On(cmdLock, "", errSim)
	testutil.Contains(t, Integrity(context.Background(), locked.WS).Findings[0], "uv lock")
	clean := newWorld(t, map[string]string{lockFile: ""})
	testutil.Equal(t, Integrity(context.Background(), clean.WS).Status, gate.StatusPass)
	testutil.True(t, clean.Fake.Count(cmdSync) == 1, "environment prepared")
	broken := newWorld(t, map[string]string{lockFile: ""})
	broken.On(cmdSync, "", errSim)
	testutil.Contains(t, Integrity(context.Background(), broken.WS).Findings[0], "uv sync")
	testutil.Equal(t, Integrity(context.Background(), newWorld(t, map[string]string{ProjectFile: ""}).WS).Reason, reasonNoLock)
	testutil.Equal(t, unhashed(filepath.Join(t.TempDir(), "gone.txt"))[0][:4], "open")
}

func TestDependencies(t *testing.T) {
	w := newWorld(t, map[string]string{lockFile: ""})
	w.On(cmdExport, "pytest==9.1.1\nfoo-bar==1 ; sys_platform == 'win32'\nPytest==9.1.1\nrogue[x]==2\n", nil)
	out := Dependencies(context.Background(), w.WS)
	testutil.Equal(t, out.Findings, []string{"pypi:rogue is not in the approved dependency list"})
	testutil.Equal(t, out.Values[pack.ValueDependencies], float64(3))
	testutil.Contains(t, w.Fake.Lines()[0], "--no-emit-project --no-header --no-annotate")
	reqs := newWorld(t, map[string]string{reqsFile: "Foo.Bar==1 --hash=sha256:aa\n"})
	testutil.Equal(t, Dependencies(context.Background(), reqs.WS).Status, gate.StatusPass)
	failing := newWorld(t, map[string]string{lockFile: ""})
	failing.On(cmdExport, "", errSim)
	testutil.Equal(t, Dependencies(context.Background(), failing.WS).Status, gate.StatusFail)
	testutil.Equal(t, Dependencies(context.Background(), newWorld(t, map[string]string{ProjectFile: ""}).WS).Reason, reasonNoLock)
	testutil.Equal(t, normalize("A__b.C"), "a-b-c")
}

const ruffIssues = `[{"code":"PLR2004","message":"Magic value","filename":"SRC/src/shop/app.py","location":{"row":5}}]`

func TestLint(t *testing.T) {
	w := newWorld(t, map[string]string{lockFile: "", fileApp: appSource, "stubs.pyi": "", readmeFile: ""})
	w.On(cmdRuffChk, strings.ReplaceAll(ruffIssues, "SRC", w.WS.Src), errSim)
	w.On(cmdRuffFmt, `[{"code":"unformatted","message":"File would be reformatted","filename":"`+w.WS.Src+`/src/shop/app.py","location":{"row":1}}]`, errSim)
	mypyOut := `{"file":"` + w.WS.Src + `/src/shop/app.py","line":5,"message":"bad","code":"x"}` + "\n" +
		`{"file":"src/shop/app.py","line":5,"message":"bad","code":"x"}` + "\nnot json\n{\"line\":1}\n"
	w.On(cmdMypy, mypyOut, errSim)
	out := Lint(context.Background(), w.WS)
	testutil.Equal(t, out.Findings, []string{
		"src/shop/app.py:5 PLR2004 Magic value", "src/shop/app.py:1 unformatted File would be reformatted", "src/shop/app.py:5 bad [x]",
	})
	mypyCall, _ := w.Fake.Find(cmdMypy)
	testutil.Contains(t, procexectest.Line(mypyCall), "--config-file /policy/python/mypy.ini", "--python-executable "+venvPython(w.WS.Report(venvDir)), fileApp, "stubs.pyi")
	testutil.Equal(t, mypyCall.Env[2], "MYPYPATH="+filepath.Join(w.WS.Src, "src")+string(os.PathListSeparator)+w.WS.Src)
	ruffCall, _ := w.Fake.Find(cmdRuffChk)
	testutil.Equal(t, ruffCall.Args, []string{"check", "--config", "/policy/python/ruff.toml", "--no-cache", "--output-format", formatJSON, w.WS.Src})
}

func TestLintFailures(t *testing.T) {
	garbled := newWorld(t, map[string]string{lockFile: "", fileApp: appSource})
	garbled.On(cmdRuffChk, "not json", errSim)
	testutil.Contains(t, Lint(context.Background(), garbled.WS).Reason, "ruff check")
	crashed := newWorld(t, map[string]string{lockFile: "", fileApp: appSource})
	crashed.On(cmdRuffChk, "[]", nil)
	crashed.On(cmdRuffFmt, "[]", nil)
	crashed.On(cmdMypy, "", errSim)
	testutil.Contains(t, Lint(context.Background(), crashed.WS).Reason, "uv run")
	clean := newWorld(t, map[string]string{lockFile: ""})
	clean.On(cmdRuffChk, "[]", nil)
	clean.On(cmdRuffFmt, "[]", nil)
	testutil.Equal(t, Lint(context.Background(), clean.WS).Status, gate.StatusPass)
	testutil.Equal(t, clean.Fake.Count(cmdMypy), 0)
	unprepared := newWorld(t, map[string]string{ProjectFile: ""})
	testutil.Equal(t, Lint(context.Background(), unprepared.WS).Reason, reasonNoLock)
	unlisted := newWorld(t, map[string]string{lockFile: ""})
	unlisted.On(cmdRuffChk, "[]", nil)
	unlisted.On(cmdRuffFmt, "[]", nil)
	unlisted.Files = nil
	unlisted.FailGit()
	testutil.Contains(t, Lint(context.Background(), unlisted.WS).Reason, "git ls-files")
}

func TestComplexity(t *testing.T) {
	w := newWorld(t, map[string]string{lockFile: ""})
	w.Produce("--output", `[{"complexity":12,"function_name":"messy","path":"`+w.WS.Src+`/src/a.py"},{"complexity":9,"function_name":"ok","path":"b.py"}]`)
	out := Complexity(context.Background(), w.WS)
	testutil.Equal(t, out.Findings, []string{"src/a.py: messy has cognitive complexity 12"})
	testutil.Equal(t, out.Values, map[string]float64{ValueFunctions: 2, ValueMaxCognitive: 12})
	call, _ := w.Fake.Find(cmdCplx)
	testutil.Contains(t, procexectest.Line(call), "--max-complexity-allowed 9 --quiet --no-ignore --output-format json", "--cache-dir "+w.WS.Report(complexityCache))
	missing := newWorld(t, map[string]string{lockFile: ""})
	testutil.Contains(t, Complexity(context.Background(), missing.WS).Reason, "complexipy")
	garbled := newWorld(t, map[string]string{lockFile: ""})
	garbled.Produce("--output", "{")
	testutil.Contains(t, Complexity(context.Background(), garbled.WS).Reason, "complexipy")
	testutil.Equal(t, Complexity(context.Background(), newWorld(t, map[string]string{ProjectFile: ""}).WS).Reason, reasonNoLock)
	flat := newWorld(t, map[string]string{lockFile: ""})
	flat.Produce("--output", `[{"complexity":9,"function_name":"edge","path":"c.py"}]`)
	testutil.Equal(t, Complexity(context.Background(), flat.WS).Status, gate.StatusPass)
}

func TestDeadCode(t *testing.T) {
	w := newWorld(t, map[string]string{lockFile: ""})
	w.On(cmdVulture, w.WS.Src+"/src/a.py:3: unused function 'x' (60% confidence)\nnoise\n", errSim)
	out := DeadCode(context.Background(), w.WS)
	testutil.Equal(t, out.Findings, []string{"src/a.py:3: unused function 'x' (60% confidence)"})
	call, _ := w.Fake.Find(cmdVulture)
	testutil.Contains(t, procexectest.Line(call), "--config /policy/python/vulture.toml "+w.WS.Src)
	crashed := newWorld(t, map[string]string{lockFile: ""})
	crashed.On(cmdVulture, "", errSim)
	testutil.Contains(t, DeadCode(context.Background(), crashed.WS).Reason, "uv run")
	testutil.Equal(t, DeadCode(context.Background(), newWorld(t, map[string]string{lockFile: ""}).WS).Status, gate.StatusPass)
	testutil.Equal(t, DeadCode(context.Background(), newWorld(t, map[string]string{ProjectFile: ""}).WS).Reason, reasonNoLock)
}

func coverageReport(lines, statements, branchesCovered, branches int) string {
	return `{"totals":{"covered_lines":` + itoa(lines) + `,"num_statements":` + itoa(statements) + `,"covered_branches":` + itoa(branchesCovered) +
		`,"num_branches":` + itoa(branches) + `},"files":{"src/shop/app.py":{"executed_lines":[1,4,5],"missing_lines":[],"contexts":{"1":[""],"4":[""],"5":["` + testOK + `"]}},` +
		`"src/b.py":{"executed_lines":[1],"missing_lines":[2,3],"contexts":{}},"src/a.py":{"executed_lines":[],"missing_lines":[1],"contexts":{}}}}`
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

func TestTestsAndCoverage(t *testing.T) {
	w := newWorld(t, map[string]string{lockFile: ""})
	w.Produce("--junitxml", junitGreen)
	w.Produce("-o", coverageReport(9, 10, 9, 10))
	out := Tests(context.Background(), w.WS)
	testutil.Equal(t, out.Status, gate.StatusPass)
	run, _ := w.Fake.Find(cmdCovRun)
	testutil.Contains(t, procexectest.Line(run), "-m coverage run --branch --source=. -m pytest -p deploy_hub_pytest -p no:cacheprovider -o addopts= -q -o junit_family=xunit1 --junitxml="+w.WS.Report(JUnitReport))
	testutil.Equal(t, run.Env[2:], []string{
		"PYTHONPATH=" + w.WS.Report(pluginDir), "PYTHONDONTWRITEBYTECODE=1", "COVERAGE_CORE=ctrace", "HYPOTHESIS_STORAGE_DIRECTORY=" + w.WS.Report(hypothesisDir),
		"COVERAGE_RCFILE=/policy/python/coverage.ini", "COVERAGE_FILE=" + w.WS.Report(coverageData),
	})
	testutil.Equal(t, testutil.Read(t, filepath.Join(w.WS.Report(pluginDir), pluginFile)), pluginSource)
	covJSON, _ := w.Fake.Find(cmdCovJSON)
	testutil.Contains(t, procexectest.Line(covJSON), "--show-contexts -o "+w.WS.Report(CoverageReport))
	lines := LineCoverage(context.Background(), w.WS)
	testutil.Equal(t, lines.Values, map[string]float64{linecov.ValueLineCoverage: 90, ValueStatements: 10})
	testutil.Equal(t, lines.Status, gate.StatusPass)
	testutil.Equal(t, lines.Findings, []string{"src/a.py: 1 of 1 lines uncovered", "src/b.py: 2 of 3 lines uncovered"})
	branches := Branches(context.Background(), w.WS)
	testutil.Equal(t, branches.Values, map[string]float64{linecov.ValueBranchCover: 90, linecov.ValueBranches: 10})
	testutil.Equal(t, branches.Status, gate.StatusPass)
	passed, err := Passed(context.Background(), w.WS)
	testutil.NoErr(t, err)
	testutil.Equal(t, passed, map[string]bool{testOK: true})
	low := newWorld(t, map[string]string{lockFile: ""})
	low.Produce("--junitxml", junitGreen)
	low.Produce("-o", coverageReport(8, 10, 8, 10))
	Tests(context.Background(), low.WS)
	testutil.Contains(t, LineCoverage(context.Background(), low.WS).Reason, "line coverage")
	testutil.Contains(t, Branches(context.Background(), low.WS).Reason, "branch coverage")
}

func TestTestsFailures(t *testing.T) {
	none := newWorld(t, map[string]string{lockFile: ""})
	none.On(cmdCovRun, "", errSim)
	testutil.Contains(t, Tests(context.Background(), none.WS).Reason, "pytest produced no report")
	noJSON := newWorld(t, map[string]string{lockFile: ""})
	noJSON.Produce("--junitxml", junitGreen)
	noJSON.On(cmdCovJSON, "", errSim)
	testutil.Contains(t, Tests(context.Background(), noJSON.WS).Reason, "uv run")
	testutil.Equal(t, Tests(context.Background(), newWorld(t, map[string]string{ProjectFile: ""}).WS).Reason, reasonNoLock)
	for _, run := range []func(context.Context, gate.Workspace) gate.Outcome{Tests, Fuzz} {
		blocked := newWorld(t, map[string]string{lockFile: ""})
		testutil.NoErr(t, prepare(context.Background(), blocked.WS))
		testutil.Write(t, blocked.WS.Report(pluginDir), "")
		testutil.Equal(t, run(context.Background(), blocked.WS).Status, gate.StatusFail)
	}
	missing := newWorld(t, nil)
	testutil.Contains(t, LineCoverage(context.Background(), missing.WS).Reason, "coverage report missing")
	testutil.Contains(t, Branches(context.Background(), missing.WS).Reason, "coverage report missing")
	_, err := Passed(context.Background(), missing.WS)
	testutil.Fails(t, err)
}

func TestNodeIDs(t *testing.T) {
	cases, err := junit.Parse([]byte(`<testsuite>
<testcase classname="tests.test_app" name="test_ok" file="tests/test_app.py"/>
<testcase classname="tests.test_app.TestGroup" name="test_in[1-2]" file="tests/test_app.py"/>
<testcase classname="tests.test_app.Outer.Inner" name="test_deep" file="tests/test_app.py"/>
<testcase classname="legacy.Suite" name="test_old"/>
</testsuite>`))
	testutil.NoErr(t, err)
	ids := make([]string, 0, len(cases))
	for _, c := range cases {
		ids = append(ids, nodeID(c))
	}
	testutil.Equal(t, ids, []string{testOK, "tests/test_app.py::TestGroup::test_in[1-2]", "tests/test_app.py::Outer::Inner::test_deep", "legacy.Suite.test_old"})
}

func TestHypothesisFuzz(t *testing.T) {
	w := newWorld(t, map[string]string{lockFile: ""})
	w.Produce("--junitxml", junitGreen)
	out := Fuzz(context.Background(), w.WS)
	testutil.Equal(t, out.Status, gate.StatusPass)
	testutil.Equal(t, out.Values, map[string]float64{ValueFuzzTests: 1})
	call, _ := w.Fake.Find("-m hypothesis")
	testutil.Contains(t, procexectest.Line(call), "--junitxml="+w.WS.Report("fuzz-"+JUnitReport))
	testutil.Equal(t, call.Env[len(call.Env)-1], "DEPLOY_HUB_FUZZ_EXAMPLES=300")
	failing := newWorld(t, map[string]string{lockFile: ""})
	failing.Produce("--junitxml", `<testsuite><testcase classname="tests.test_app" name="test_prop" file="tests/test_app.py"><failure message="Falsifying example"/></testcase></testsuite>`)
	bad := Fuzz(context.Background(), failing.WS)
	testutil.Equal(t, bad.Reason, reasonFuzz)
	testutil.Equal(t, bad.Findings, []string{"tests/test_app.py::test_prop failed: Falsifying example"})
	empty := newWorld(t, map[string]string{lockFile: ""})
	empty.On("-m hypothesis", "", packtest.ExitError(t, 5))
	testutil.Equal(t, Fuzz(context.Background(), empty.WS).Reason, reasonNoFuzz)
	silent := newWorld(t, map[string]string{lockFile: ""})
	testutil.Contains(t, Fuzz(context.Background(), silent.WS).Reason, "no fuzz report")
	testutil.Equal(t, Fuzz(context.Background(), newWorld(t, map[string]string{ProjectFile: ""}).WS).Reason, reasonNoLock)
	testutil.Equal(t, procexec.ExitCode(errSim), 0)
}

func TestFileClassification(t *testing.T) {
	for _, rel := range []string{"tests/a.py", "pkg/tests/b.py", "test_c.py", "pkg/d_test.py", "conftest.py"} {
		testutil.True(t, isTest(rel), rel)
	}
	for _, rel := range []string{"src/app.py", "contest.py", "testing/x.py"} {
		testutil.True(t, !isTest(rel), rel)
	}
	testutil.Equal(t, sortedStrings([]string{"b", "a", "c"}), []string{"a", "b", "c"})
	testutil.Equal(t, testsOf([]string{testOK, testOK, otherTest}), []string{testOK, otherTest})
	testutil.Equal(t, testsOf([]string{"", testOK}), []string(nil))
}
