package python

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
	"github.com/DreamsAreReal/deploy-hub/internal/gate/junit"
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
	cmdVenv    = "venv --quiet"
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

var errSim = errors.New("simulated")

func exitError(t *testing.T, code string) error {
	t.Helper()
	err := exec.CommandContext(context.Background(), "sh", "-c", "exit "+code).Run()
	testutil.True(t, err != nil, "exit status")
	return err
}

type reply struct {
	out string
	err error
}

type world struct {
	t      *testing.T
	ws     gate.Workspace
	fake   *procexectest.Fake
	mu     sync.Mutex
	files  []string
	rules  map[string]reply
	writes map[string]string
	kill   func(src string) error
	broken func(src string) bool
}

func newWorld(t *testing.T, files map[string]string) *world {
	t.Helper()
	w := &world{t: t, rules: map[string]reply{}, writes: map[string]string{}}
	src := t.TempDir()
	for name, content := range files {
		testutil.Write(t, filepath.Join(src, filepath.FromSlash(name)), content)
		w.files = append(w.files, name)
	}
	slices.Sort(w.files)
	w.fake = &procexectest.Fake{Handler: w.handle}
	w.ws = gate.Workspace{
		Src: src, Out: t.TempDir(), PolicyDir: "/policy", Exec: w.fake, Log: slog.New(slog.DiscardHandler),
		Policy: policy.Policy{
			Thresholds:   policy.Thresholds{LineCoverage: 90, BranchCoverage: 90, MaxCognitive: 9, MutationScore: 90},
			Limits:       policy.Limits{FuzzExamples: 300, MutantFloor: time.Second, MutantFactor: 2},
			Dependencies: []string{"pypi:pytest", "pypi:Foo_Bar", "example.com/go/module"},
		},
	}
	return w
}

func (w *world) on(substr, out string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rules[substr] = reply{out: out, err: err}
}

func (w *world) produce(flag, content string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes[flag] = content
}

func (w *world) handle(c procexec.Command) (procexec.Result, error) {
	line := procexectest.Line(c)
	if c.Name == gitBinary {
		return procexec.Result{Stdout: strings.Join(w.files, "\x00") + "\x00"}, nil
	}
	w.writeOutputs(c)
	if strings.Contains(line, cmdPytest) && w.kill != nil && !strings.Contains(line, "hypothesis") {
		return w.mutant(c)
	}
	if strings.Contains(line, cmdCompile) && w.broken != nil && w.broken(testutil.Read(w.t, c.Args[len(c.Args)-1])) {
		return procexec.Result{Stderr: "SyntaxError"}, errSim
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

func (w *world) writeOutputs(c procexec.Command) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, a := range c.Args {
		for flag, content := range w.writes {
			w.writeFlag(c.Args[i+1:], a, flag, content)
		}
	}
}

func (w *world) writeFlag(rest []string, arg, flag, content string) {
	if value, ok := strings.CutPrefix(arg, flag+"="); ok {
		testutil.Write(w.t, value, content)
	}
	if arg == flag && len(rest) > 0 {
		testutil.Write(w.t, rest[0], content)
	}
}

func (w *world) mutant(c procexec.Command) (procexec.Result, error) {
	data, err := os.ReadFile(filepath.Join(c.Dir, filepath.FromSlash(fileApp)))
	testutil.NoErr(w.t, err)
	return procexec.Result{}, w.kill(string(data))
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
	testutil.NoErr(t, prepare(context.Background(), w.ws))
	lines := w.fake.Lines()
	testutil.Equal(t, len(lines), 4)
	testutil.Contains(t, lines[0], "uv sync --quiet --locked --all-groups --all-extras")
	testutil.Contains(t, lines[1], "uv run --quiet --no-project --python "+venvPython(w.ws.Report(venvDir))+" -- python "+cmdImport)
	testutil.Equal(t, lines[2], "uv venv --quiet "+w.ws.Report(toolsDir))
	testutil.Equal(t, lines[3], "uv pip install --quiet --python "+venvPython(w.ws.Report(toolsDir))+" --require-hashes -r /policy/python/tools-lock.txt")
	sync := w.fake.Calls()[0]
	testutil.Equal(t, sync.Env, []string{envCache + w.ws.Report(cacheDir), envNoProg, envProject + w.ws.Report(venvDir)})
	testutil.Equal(t, sync.Dir, w.ws.Src)
	testutil.Equal(t, testutil.Read(t, w.ws.Report(readyStamp)), Stack)
	testutil.NoErr(t, prepare(context.Background(), w.ws))
	testutil.Equal(t, len(w.fake.Lines()), 4)
}

func TestPrepareRequirementsProject(t *testing.T) {
	w := newWorld(t, map[string]string{reqsFile: "", "requirements-dev.txt": "", fileApp: appSource})
	w.on(cmdImport, "", errSim)
	testutil.NoErr(t, prepare(context.Background(), w.ws))
	lines := w.fake.Lines()
	testutil.Equal(t, lines[0], "uv venv --quiet "+w.ws.Report(venvDir))
	testutil.Contains(t, lines[1], "--require-hashes -r "+filepath.Join(w.ws.Src, "requirements-dev.txt"))
	testutil.Contains(t, lines[2], "--require-hashes -r "+filepath.Join(w.ws.Src, reqsFile))
	testutil.Contains(t, lines[4], "-r /policy/python/test-lock.txt")
}

func TestPrepareFailures(t *testing.T) {
	for _, step := range []string{cmdSync, "venv --quiet", cmdInstall} {
		w := newWorld(t, map[string]string{lockFile: ""})
		w.on(step, "", errSim)
		err := prepare(context.Background(), w.ws)
		testutil.Contains(t, err.Error(), "simulated")
		testutil.Missing(t, w.ws.Report(readyStamp))
	}
	unlocked := newWorld(t, map[string]string{ProjectFile: ""})
	testutil.Is(t, prepare(context.Background(), unlocked.ws), ErrNoLock)
	loose := newWorld(t, map[string]string{scriptFile: ""})
	loose.on(cmdVenv, "", errSim)
	testutil.Fails(t, prepare(context.Background(), loose.ws))
	reqs := newWorld(t, map[string]string{reqsFile: ""})
	reqs.on(cmdInstall, "", errSim)
	testutil.Contains(t, prepare(context.Background(), reqs.ws).Error(), "uv pip")
	blocked := newWorld(t, map[string]string{scriptFile: ""})
	testutil.Mkdir(t, blocked.ws.Report(readyStamp+"x"))
	blocked.ws.Out = filepath.Join(t.TempDir(), "missing")
	testutil.Fails(t, prepare(context.Background(), blocked.ws))
}

func TestIntegrity(t *testing.T) {
	reqs := "pytest==9.1.1 \\\n    --hash=sha256:aa\nloose>=1\nnohash==2\n-r other.txt\nextra[x]==1 --hash=sha256:bb\n\n"
	w := newWorld(t, map[string]string{reqsFile: reqs})
	out := Integrity(context.Background(), w.ws)
	testutil.Equal(t, out.Findings, []string{
		"requirements.txt: loose is not pinned to one version with hashes",
		"requirements.txt: nohash is not pinned to one version with hashes",
	})
	testutil.Equal(t, w.fake.Count("uv sync"), 0)
	locked := newWorld(t, map[string]string{lockFile: ""})
	locked.on(cmdLock, "", errSim)
	testutil.Contains(t, Integrity(context.Background(), locked.ws).Findings[0], "uv lock")
	clean := newWorld(t, map[string]string{lockFile: ""})
	testutil.Equal(t, Integrity(context.Background(), clean.ws).Status, gate.StatusPass)
	testutil.True(t, clean.fake.Count(cmdSync) == 1, "environment prepared")
	broken := newWorld(t, map[string]string{lockFile: ""})
	broken.on(cmdSync, "", errSim)
	testutil.Contains(t, Integrity(context.Background(), broken.ws).Findings[0], "uv sync")
	testutil.Equal(t, Integrity(context.Background(), newWorld(t, map[string]string{ProjectFile: ""}).ws).Reason, reasonNoLock)
	testutil.Equal(t, unhashed(filepath.Join(t.TempDir(), "gone.txt"))[0][:4], "open")
}

func TestDependencies(t *testing.T) {
	w := newWorld(t, map[string]string{lockFile: ""})
	w.on(cmdExport, "pytest==9.1.1\nfoo-bar==1 ; sys_platform == 'win32'\nPytest==9.1.1\nrogue[x]==2\n", nil)
	out := Dependencies(context.Background(), w.ws)
	testutil.Equal(t, out.Findings, []string{"pypi:rogue is not in the approved dependency list"})
	testutil.Equal(t, out.Values[ValueDependencies], float64(3))
	testutil.Contains(t, w.fake.Lines()[0], "--no-emit-project --no-header --no-annotate")
	reqs := newWorld(t, map[string]string{reqsFile: "Foo.Bar==1 --hash=sha256:aa\n"})
	testutil.Equal(t, Dependencies(context.Background(), reqs.ws).Status, gate.StatusPass)
	failing := newWorld(t, map[string]string{lockFile: ""})
	failing.on(cmdExport, "", errSim)
	testutil.Equal(t, Dependencies(context.Background(), failing.ws).Status, gate.StatusFail)
	testutil.Equal(t, Dependencies(context.Background(), newWorld(t, map[string]string{ProjectFile: ""}).ws).Reason, reasonNoLock)
	testutil.Equal(t, normalize("A__b.C"), "a-b-c")
}

const ruffIssues = `[{"code":"PLR2004","message":"Magic value","filename":"SRC/src/shop/app.py","location":{"row":5}}]`

func TestLint(t *testing.T) {
	w := newWorld(t, map[string]string{lockFile: "", fileApp: appSource, "stubs.pyi": "", readmeFile: ""})
	w.on(cmdRuffChk, strings.ReplaceAll(ruffIssues, "SRC", w.ws.Src), errSim)
	w.on(cmdRuffFmt, `[{"code":"unformatted","message":"File would be reformatted","filename":"`+w.ws.Src+`/src/shop/app.py","location":{"row":1}}]`, errSim)
	mypyOut := `{"file":"` + w.ws.Src + `/src/shop/app.py","line":5,"message":"bad","code":"x"}` + "\n" +
		`{"file":"src/shop/app.py","line":5,"message":"bad","code":"x"}` + "\nnot json\n{\"line\":1}\n"
	w.on(cmdMypy, mypyOut, errSim)
	out := Lint(context.Background(), w.ws)
	testutil.Equal(t, out.Findings, []string{
		"src/shop/app.py:5 PLR2004 Magic value", "src/shop/app.py:1 unformatted File would be reformatted", "src/shop/app.py:5 bad [x]",
	})
	mypyCall, _ := w.fake.Find(cmdMypy)
	testutil.Contains(t, procexectest.Line(mypyCall), "--config-file /policy/python/mypy.ini", "--python-executable "+venvPython(w.ws.Report(venvDir)), fileApp, "stubs.pyi")
	testutil.Equal(t, mypyCall.Env[2], "MYPYPATH="+filepath.Join(w.ws.Src, "src")+string(os.PathListSeparator)+w.ws.Src)
	ruffCall, _ := w.fake.Find(cmdRuffChk)
	testutil.Equal(t, ruffCall.Args, []string{"check", "--config", "/policy/python/ruff.toml", "--no-cache", "--output-format", formatJSON, w.ws.Src})
}

func TestLintFailures(t *testing.T) {
	garbled := newWorld(t, map[string]string{lockFile: "", fileApp: appSource})
	garbled.on(cmdRuffChk, "not json", errSim)
	testutil.Contains(t, Lint(context.Background(), garbled.ws).Reason, "ruff check")
	crashed := newWorld(t, map[string]string{lockFile: "", fileApp: appSource})
	crashed.on(cmdRuffChk, "[]", nil)
	crashed.on(cmdRuffFmt, "[]", nil)
	crashed.on(cmdMypy, "", errSim)
	testutil.Contains(t, Lint(context.Background(), crashed.ws).Reason, "uv run")
	clean := newWorld(t, map[string]string{lockFile: ""})
	clean.on(cmdRuffChk, "[]", nil)
	clean.on(cmdRuffFmt, "[]", nil)
	testutil.Equal(t, Lint(context.Background(), clean.ws).Status, gate.StatusPass)
	testutil.Equal(t, clean.fake.Count(cmdMypy), 0)
	unprepared := newWorld(t, map[string]string{ProjectFile: ""})
	testutil.Equal(t, Lint(context.Background(), unprepared.ws).Reason, reasonNoLock)
	unlisted := newWorld(t, map[string]string{lockFile: ""})
	unlisted.on(cmdRuffChk, "[]", nil)
	unlisted.on(cmdRuffFmt, "[]", nil)
	unlisted.files = nil
	unlisted.fake.Handler = func(c procexec.Command) (procexec.Result, error) {
		if c.Name == gitBinary {
			return procexec.Result{}, errSim
		}
		return unlisted.handle(c)
	}
	testutil.Contains(t, Lint(context.Background(), unlisted.ws).Reason, "git ls-files")
}

func TestComplexity(t *testing.T) {
	w := newWorld(t, map[string]string{lockFile: ""})
	w.produce("--output", `[{"complexity":12,"function_name":"messy","path":"`+w.ws.Src+`/src/a.py"},{"complexity":9,"function_name":"ok","path":"b.py"}]`)
	out := Complexity(context.Background(), w.ws)
	testutil.Equal(t, out.Findings, []string{"src/a.py: messy has cognitive complexity 12"})
	testutil.Equal(t, out.Values, map[string]float64{ValueFunctions: 2, ValueMaxCognitive: 12})
	call, _ := w.fake.Find(cmdCplx)
	testutil.Contains(t, procexectest.Line(call), "--max-complexity-allowed 9 --quiet --no-ignore --output-format json", "--cache-dir "+w.ws.Report(complexityCache))
	missing := newWorld(t, map[string]string{lockFile: ""})
	testutil.Contains(t, Complexity(context.Background(), missing.ws).Reason, "complexipy")
	garbled := newWorld(t, map[string]string{lockFile: ""})
	garbled.produce("--output", "{")
	testutil.Contains(t, Complexity(context.Background(), garbled.ws).Reason, "complexipy")
	testutil.Equal(t, Complexity(context.Background(), newWorld(t, map[string]string{ProjectFile: ""}).ws).Reason, reasonNoLock)
	flat := newWorld(t, map[string]string{lockFile: ""})
	flat.produce("--output", `[{"complexity":9,"function_name":"edge","path":"c.py"}]`)
	testutil.Equal(t, Complexity(context.Background(), flat.ws).Status, gate.StatusPass)
}

func TestDeadCode(t *testing.T) {
	w := newWorld(t, map[string]string{lockFile: ""})
	w.on(cmdVulture, w.ws.Src+"/src/a.py:3: unused function 'x' (60% confidence)\nnoise\n", errSim)
	out := DeadCode(context.Background(), w.ws)
	testutil.Equal(t, out.Findings, []string{"src/a.py:3: unused function 'x' (60% confidence)"})
	call, _ := w.fake.Find(cmdVulture)
	testutil.Contains(t, procexectest.Line(call), "--config /policy/python/vulture.toml "+w.ws.Src)
	crashed := newWorld(t, map[string]string{lockFile: ""})
	crashed.on(cmdVulture, "", errSim)
	testutil.Contains(t, DeadCode(context.Background(), crashed.ws).Reason, "uv run")
	testutil.Equal(t, DeadCode(context.Background(), newWorld(t, map[string]string{lockFile: ""}).ws).Status, gate.StatusPass)
	testutil.Equal(t, DeadCode(context.Background(), newWorld(t, map[string]string{ProjectFile: ""}).ws).Reason, reasonNoLock)
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
	w.produce("--junitxml", junitGreen)
	w.produce("-o", coverageReport(9, 10, 9, 10))
	out := Tests(context.Background(), w.ws)
	testutil.Equal(t, out.Status, gate.StatusPass)
	run, _ := w.fake.Find(cmdCovRun)
	testutil.Contains(t, procexectest.Line(run), "-m coverage run --branch --source=. -m pytest -p deploy_hub_pytest -p no:cacheprovider -o addopts= -q -o junit_family=xunit1 --junitxml="+w.ws.Report(JUnitReport))
	testutil.Equal(t, run.Env[2:], []string{
		"PYTHONPATH=" + w.ws.Report(pluginDir), "PYTHONDONTWRITEBYTECODE=1", "COVERAGE_CORE=ctrace", "HYPOTHESIS_STORAGE_DIRECTORY=" + w.ws.Report(hypothesisDir),
		"COVERAGE_RCFILE=/policy/python/coverage.ini", "COVERAGE_FILE=" + w.ws.Report(coverageData),
	})
	testutil.Equal(t, testutil.Read(t, filepath.Join(w.ws.Report(pluginDir), pluginFile)), pluginSource)
	covJSON, _ := w.fake.Find(cmdCovJSON)
	testutil.Contains(t, procexectest.Line(covJSON), "--show-contexts -o "+w.ws.Report(CoverageReport))
	lines := LineCoverage(context.Background(), w.ws)
	testutil.Equal(t, lines.Values, map[string]float64{ValueLineCoverage: 90, ValueStatements: 10})
	testutil.Equal(t, lines.Status, gate.StatusPass)
	testutil.Equal(t, lines.Findings, []string{"src/a.py: 1 of 1 lines uncovered", "src/b.py: 2 of 3 lines uncovered"})
	branches := Branches(context.Background(), w.ws)
	testutil.Equal(t, branches.Values, map[string]float64{ValueBranchCover: 90, ValueBranches: 10})
	testutil.Equal(t, branches.Status, gate.StatusPass)
	passed, err := Passed(context.Background(), w.ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, passed, map[string]bool{testOK: true})
	low := newWorld(t, map[string]string{lockFile: ""})
	low.produce("--junitxml", junitGreen)
	low.produce("-o", coverageReport(8, 10, 8, 10))
	Tests(context.Background(), low.ws)
	testutil.Equal(t, LineCoverage(context.Background(), low.ws).Reason, reasonLines)
	testutil.Equal(t, Branches(context.Background(), low.ws).Reason, reasonBranches)
}

func TestTestsFailures(t *testing.T) {
	none := newWorld(t, map[string]string{lockFile: ""})
	none.on(cmdCovRun, "", errSim)
	testutil.Contains(t, Tests(context.Background(), none.ws).Reason, "pytest produced no report")
	noJSON := newWorld(t, map[string]string{lockFile: ""})
	noJSON.produce("--junitxml", junitGreen)
	noJSON.on(cmdCovJSON, "", errSim)
	testutil.Contains(t, Tests(context.Background(), noJSON.ws).Reason, "uv run")
	testutil.Equal(t, Tests(context.Background(), newWorld(t, map[string]string{ProjectFile: ""}).ws).Reason, reasonNoLock)
	blocked := newWorld(t, map[string]string{lockFile: ""})
	testutil.NoErr(t, prepare(context.Background(), blocked.ws))
	testutil.Write(t, blocked.ws.Report(pluginDir), "")
	testutil.Equal(t, Tests(context.Background(), blocked.ws).Status, gate.StatusFail)
	missing := newWorld(t, nil)
	testutil.Contains(t, LineCoverage(context.Background(), missing.ws).Reason, "coverage report missing")
	testutil.Contains(t, Branches(context.Background(), missing.ws).Reason, "coverage report missing")
	_, err := Passed(context.Background(), missing.ws)
	testutil.Fails(t, err)
	testutil.Equal(t, ratio(0, 0), float64(percentScale))
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

func TestFuzz(t *testing.T) {
	w := newWorld(t, map[string]string{lockFile: ""})
	w.produce("--junitxml", junitGreen)
	out := Fuzz(context.Background(), w.ws)
	testutil.Equal(t, out.Status, gate.StatusPass)
	testutil.Equal(t, out.Values, map[string]float64{ValueFuzzTests: 1})
	call, _ := w.fake.Find("-m hypothesis")
	testutil.Contains(t, procexectest.Line(call), "--junitxml="+w.ws.Report("fuzz-"+JUnitReport))
	testutil.Equal(t, call.Env[len(call.Env)-1], "DEPLOY_HUB_FUZZ_EXAMPLES=300")
	failing := newWorld(t, map[string]string{lockFile: ""})
	failing.produce("--junitxml", `<testsuite><testcase classname="tests.test_app" name="test_prop" file="tests/test_app.py"><failure message="Falsifying example"/></testcase></testsuite>`)
	bad := Fuzz(context.Background(), failing.ws)
	testutil.Equal(t, bad.Reason, reasonFuzz)
	testutil.Equal(t, bad.Findings, []string{"tests/test_app.py::test_prop failed: Falsifying example"})
	empty := newWorld(t, map[string]string{lockFile: ""})
	empty.on("-m hypothesis", "", exitError(t, "5"))
	testutil.Equal(t, Fuzz(context.Background(), empty.ws).Reason, reasonNoFuzz)
	silent := newWorld(t, map[string]string{lockFile: ""})
	testutil.Contains(t, Fuzz(context.Background(), silent.ws).Reason, "no fuzz report")
	testutil.Equal(t, Fuzz(context.Background(), newWorld(t, map[string]string{ProjectFile: ""}).ws).Reason, reasonNoLock)
	blocked := newWorld(t, map[string]string{lockFile: ""})
	testutil.NoErr(t, prepare(context.Background(), blocked.ws))
	testutil.Write(t, blocked.ws.Report(pluginDir), "")
	testutil.Equal(t, Fuzz(context.Background(), blocked.ws).Status, gate.StatusFail)
	testutil.Equal(t, exitCode(errSim), 0)
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
