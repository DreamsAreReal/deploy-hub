package node

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
	fileCart       = "src/cart.ts"
	fileSpec       = "test/cart.test.ts"
	gitBinary      = "git"
	cartSource     = "export const LIMIT = 3;\n\nexport function above(value: number): boolean {\n  return value > LIMIT;\n}\n\nexport function never(value: number): number {\n  return value + 1;\n}\n"
	lockGood       = `{"lockfileVersion":3,"packages":{"":{},"node_modules/vitest":{"integrity":"sha512-a","resolved":"https://registry.npmjs.org/vitest"},"node_modules/a/node_modules/b":{"integrity":"sha512-b","resolved":"https://registry.npmjs.org/b"},"node_modules/local":{"link":true},"node_modules/x/node_modules/bundled":{"inBundle":true}}}`
	manifestVite   = `{"name":"cart","devDependencies":{"vitest":"5.0.5","fast-check":"4.10.2"}}`
	junitGreen     = `<testsuites><testsuite><testcase classname="test/cart.test.ts" name="cart &gt; works" time="0.5"/></testsuite></testsuites>`
	istanbul       = `{"STAGE/src/cart.ts":{"statementMap":{"0":{"start":{"line":1}},"1":{"start":{"line":4}},"2":{"start":{"line":8}}},"s":{"0":1,"1":3,"2":0},"b":{"0":[1,0]}}}`
	idWorks        = "test/cart.test.ts > cart > works"
	cmdVitest      = "vitest.mjs"
	cmdNpmCI       = "npm ci"
	cmdSyntax      = syntaxCheckFile
	cmdESLint      = "eslint.js"
	cmdTSC         = "/bin/tsc"
	cmdKnip        = "knip.js"
	readme         = "README.md"
	viteConfig     = "vite.config.ts"
	flagOutputFile = "--outputFile="
	plainManifest  = `{"name":"plain"}`
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
	t       *testing.T
	ws      gate.Workspace
	fake    *procexectest.Fake
	mu      sync.Mutex
	files   []string
	rules   map[string]reply
	outputs map[string]string
	kill    func(src string) error
	broken  func(src string) bool
}

func newWorld(t *testing.T, files map[string]string) *world {
	t.Helper()
	w := &world{t: t, rules: map[string]reply{}, outputs: map[string]string{}}
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
			Thresholds:   policy.Thresholds{LineCoverage: 90, BranchCoverage: 50, MaxCognitive: 9, MutationScore: 90},
			Limits:       policy.Limits{FuzzExamples: 250, MutantFloor: time.Second, MutantFactor: 2},
			Dependencies: []string{"npm:vitest", "npm:b", "example.com/go"},
		},
	}
	testutil.Write(t, filepath.Join(w.ws.PolicyDir, policySub, toolsManifest), `{"name":"tools"}`)
	testutil.Write(t, filepath.Join(w.ws.PolicyDir, policySub, toolsLock), `{"lockfileVersion":3}`)
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
	w.outputs[flag] = content
}

func (w *world) handle(c procexec.Command) (procexec.Result, error) {
	line := procexectest.Line(c)
	if c.Name == gitBinary {
		return procexec.Result{Stdout: strings.Join(w.files, "\x00") + "\x00"}, nil
	}
	w.write(c)
	if c.Name == NPMBinary && strings.Contains(line, cmdNpmCI) && !strings.Contains(line, "--ignore-scripts") {
		testutil.Mkdir(w.t, filepath.Join(c.Dir, modulesDir, "vitest"))
		testutil.Write(w.t, filepath.Join(c.Dir, modulesDir, "vitest", Manifest), `{"version":"5.0.5"}`)
	}
	if w.broken != nil && strings.Contains(line, cmdSyntax) && w.broken(testutil.Read(w.t, c.Args[len(c.Args)-1])) {
		return procexec.Result{}, errSim
	}
	if w.kill != nil && strings.Contains(line, "--bail") {
		return procexec.Result{}, w.kill(testutil.Read(w.t, filepath.Join(c.Dir, filepath.FromSlash(fileCart))))
	}
	return w.reply(line)
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

func (w *world) write(c procexec.Command) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, a := range c.Args {
		for flag, content := range w.outputs {
			if value, ok := strings.CutPrefix(a, flag); ok {
				testutil.Write(w.t, value, strings.ReplaceAll(content, "STAGE", stage(w.ws)))
			}
		}
	}
}

func staged(t *testing.T, w *world) {
	t.Helper()
	testutil.NoErr(t, prepare(context.Background(), w.ws))
}

func TestManifestAndRunner(t *testing.T) {
	cases := map[string]runner{
		`{"devDependencies":{"vitest":"1"}}`:           runnerVitest,
		`{"dependencies":{"jest":"1"}}`:                runnerJest,
		`{"devDependencies":{"mocha":"1","jest":"1"}}`: runnerJest,
		`{"devDependencies":{"mocha":"1"}}`:            runnerMocha,
		`{"scripts":{"test":"node --test"}}`:           runnerNode,
	}
	for text, want := range cases {
		dir := t.TempDir()
		testutil.Write(t, filepath.Join(dir, Manifest), text)
		m, err := readManifest(dir)
		testutil.NoErr(t, err)
		testutil.Equal(t, m.runner(), want)
	}
	_, err := readManifest(t.TempDir())
	testutil.Fails(t, err)
	testutil.Equal(t, firstArg(nil), "")
	testutil.Equal(t, firstArg([]string{"/a/b/eslint.js", "x"}), "eslint.js")
}

func TestPrepareStagesAndInstalls(t *testing.T) {
	w := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood, fileCart: cartSource})
	staged(t, w)
	lines := w.fake.Lines()
	testutil.Equal(t, lines[1], "npm ci --no-audit --no-fund")
	testutil.Equal(t, lines[2], "npm ci --ignore-scripts --no-audit --no-fund")
	calls := w.fake.Calls()
	testutil.Equal(t, calls[1].Dir, stage(w.ws))
	testutil.Equal(t, calls[2].Dir, tools(w.ws))
	testutil.Equal(t, calls[1].Env, []string{envCache + w.ws.Report(cacheDir)})
	testutil.Equal(t, testutil.Read(t, filepath.Join(stage(w.ws), fileCart)), cartSource)
	testutil.Equal(t, testutil.Read(t, filepath.Join(tools(w.ws), Manifest)), `{"name":"tools"}`)
	testutil.Equal(t, testutil.Read(t, filepath.Join(tools(w.ws), Lockfile)), `{"lockfileVersion":3}`)
	testutil.Contains(t, testutil.Read(t, filepath.Join(tools(w.ws), eslintConfigFile)), `"sonarjs/cognitive-complexity": ["error", 9]`)
	testutil.Contains(t, testutil.Read(t, filepath.Join(tools(w.ws), complexityConfigFile)), `["error", 9]`)
	testutil.Equal(t, testutil.Read(t, w.ws.Report(readyStamp)), Stack)
	staged(t, w)
	testutil.Equal(t, len(w.fake.Lines()), 3)
	version, err := installedVersion(stage(w.ws), "vitest")
	testutil.NoErr(t, err)
	testutil.Equal(t, version, "5.0.5")
	_, err = installedVersion(stage(w.ws), "missing")
	testutil.Fails(t, err)
}

func TestPrepareFailures(t *testing.T) {
	testutil.Is(t, prepare(context.Background(), newWorld(t, map[string]string{Manifest: manifestVite}).ws), ErrNoLock)
	ci := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	ci.on(cmdNpmCI, "", errSim)
	testutil.Contains(t, prepare(context.Background(), ci.ws).Error(), "npm ci")
	toolsCI := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	toolsCI.on("--ignore-scripts", "", errSim)
	testutil.Fails(t, prepare(context.Background(), toolsCI.ws))
	noPolicy := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	testutil.NoErr(t, os.RemoveAll(filepath.Join(noPolicy.ws.PolicyDir, policySub)))
	testutil.Fails(t, prepare(context.Background(), noPolicy.ws))
	unlisted := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	unlisted.files = append(unlisted.files, "gone.ts")
	testutil.Fails(t, prepare(context.Background(), unlisted.ws))
	blocked := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	testutil.Write(t, tools(blocked.ws), "")
	testutil.Fails(t, prepare(context.Background(), blocked.ws))
	scripts := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	testutil.Mkdir(t, filepath.Join(tools(scripts.ws), eslintConfigFile))
	testutil.Fails(t, prepare(context.Background(), scripts.ws))
}

func TestRelativePaths(t *testing.T) {
	w := newWorld(t, nil)
	testutil.Mkdir(t, stage(w.ws))
	testutil.Equal(t, rel(w.ws, filepath.Join(stage(w.ws), "src", "a.ts")), "src/a.ts")
	resolved, err := filepath.EvalSymlinks(stage(w.ws))
	testutil.NoErr(t, err)
	testutil.Equal(t, rel(w.ws, filepath.Join(resolved, "b.ts")), "b.ts")
	testutil.Equal(t, rel(w.ws, "/elsewhere/c.ts"), "/elsewhere/c.ts")
	testutil.Equal(t, rel(newWorld(t, nil).ws, "x/y.ts"), "x/y.ts")
}

func TestIntegrityAndDependencies(t *testing.T) {
	stale := `{"lockfileVersion":1,"packages":{"":{},"node_modules/x":{"integrity":"","resolved":"https://r/x"},"node_modules/y":{"integrity":"sha","resolved":"git+ssh://y"}}}`
	w := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: stale})
	testutil.Equal(t, Integrity(context.Background(), w.ws).Findings, []string{
		"package-lock.json: lockfileVersion 1 is older than 2",
		"package-lock.json: x has no integrity hash or registry source",
		"package-lock.json: y has no integrity hash or registry source",
	})
	testutil.Equal(t, w.fake.Count(cmdNpmCI), 0)
	good := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	testutil.Equal(t, Integrity(context.Background(), good.ws).Status, gate.StatusPass)
	failing := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	failing.on(cmdNpmCI, "", errSim)
	testutil.Contains(t, Integrity(context.Background(), failing.ws).Findings[0], "npm ci")
	testutil.Equal(t, Integrity(context.Background(), newWorld(t, nil).ws).Reason, reasonNoLock)
	testutil.Equal(t, Integrity(context.Background(), newWorld(t, map[string]string{Lockfile: "{"}).ws).Status, gate.StatusFail)
	deps := Dependencies(context.Background(), good.ws)
	testutil.Equal(t, deps.Findings, []string{"npm:bundled is not in the approved dependency list"})
	testutil.Equal(t, deps.Values[ValueDependencies], float64(3))
	testutil.Equal(t, Dependencies(context.Background(), newWorld(t, nil).ws).Reason, reasonNoLock)
	testutil.Equal(t, packageName("node_modules/@scope/a/node_modules/b"), "b")
	testutil.Equal(t, packageName("plain"), "plain")
}

const eslintOut = `[{"filePath":"STAGE/src/cart.ts","messages":[{"ruleId":"no-magic-numbers","message":"No magic number: 42.","line":3}]},{"filePath":"STAGE/src/ok.ts","messages":[]}]`

func TestLint(t *testing.T) {
	w := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood, fileCart: cartSource, tsconfigFile: "{}"})
	staged(t, w)
	w.on(cmdESLint, strings.ReplaceAll(eslintOut, "STAGE", stage(w.ws)), errSim)
	w.on(cmdTSC, "src/cart.ts(4,10): error TS2322: Type 'number' is not assignable to type 'string'.\nnoise\n", errSim)
	out := Lint(context.Background(), w.ws)
	testutil.Equal(t, out.Findings, []string{
		"src/cart.ts:3 no-magic-numbers No magic number: 42.",
		"src/cart.ts:4 TS2322 Type 'number' is not assignable to type 'string'.",
	})
	eslintCall, _ := w.fake.Find(cmdESLint)
	testutil.Equal(t, eslintCall.Args[1:], []string{flagConfig, filepath.Join(tools(w.ws), eslintConfigFile), "--no-config-lookup", flagFormat, formatJSON, "."})
	tscCall, _ := w.fake.Find(cmdTSC)
	testutil.Equal(t, tscCall.Args, []string{toolScript(w.ws, "typescript", "bin", "tsc"), "--noEmit", "--strict", "--pretty", "false", "-p", tsconfigFile})
	testutil.Mkdir(t, filepath.Join(stage(w.ws), modulesDir, "typescript", "bin"))
	testutil.Write(t, filepath.Join(stage(w.ws), modulesDir, "typescript", "bin", "tsc"), "")
	Lint(context.Background(), w.ws)
	calls := w.fake.Calls()
	testutil.Equal(t, calls[len(calls)-1].Args[0], filepath.Join(stage(w.ws), modulesDir, "typescript", "bin", "tsc"))
}

func TestLintFailures(t *testing.T) {
	garbled := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	garbled.on(cmdESLint, "oops", errSim)
	testutil.Contains(t, Lint(context.Background(), garbled.ws).Reason, "eslint")
	tscCrash := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood, fileCart: cartSource})
	tscCrash.on(cmdESLint, "[]", nil)
	tscCrash.on(cmdTSC, "", errSim)
	testutil.Contains(t, Lint(context.Background(), tscCrash.ws).Reason, "node tsc")
	plain := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood, "src/a.js": "export const a = 1;\n"})
	plain.on(cmdESLint, "[]", nil)
	testutil.Equal(t, Lint(context.Background(), plain.ws).Status, gate.StatusPass)
	testutil.Equal(t, plain.fake.Count(cmdTSC), 0)
	noConfig := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood, fileCart: cartSource})
	noConfig.on(cmdESLint, "[]", nil)
	Lint(context.Background(), noConfig.ws)
	tscCall, _ := noConfig.fake.Find(cmdTSC)
	testutil.True(t, !slices.Contains(tscCall.Args, "-p"), "no tsconfig, default options")
	testutil.Equal(t, Lint(context.Background(), newWorld(t, nil).ws).Reason, reasonNoLock)
	testutil.True(t, !hasTypeScript(filepath.Join(t.TempDir(), "missing")), "missing dir")
	hidden := t.TempDir()
	testutil.Write(t, filepath.Join(hidden, modulesDir, "dep", "index.ts"), "")
	testutil.True(t, !hasTypeScript(hidden), "dependencies are not the project")
}

func TestComplexityAndDeadCode(t *testing.T) {
	w := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	staged(t, w)
	w.on(complexityConfigFile, `[{"filePath":"`+stage(w.ws)+`/src/a.ts","messages":[{"ruleId":"sonarjs/cognitive-complexity","message":"Refactor","line":2}]}]`, errSim)
	testutil.Equal(t, Complexity(context.Background(), w.ws).Findings, []string{"src/a.ts:2 sonarjs/cognitive-complexity Refactor"})
	w.on(cmdKnip, `{"files":["src/old.ts"],"issues":[{"file":"src/a.ts","exports":[{"name":"unused"}],"dependencies":[{"name":"lodash"}],"duplicates":"skip"}]}`, errSim)
	dead := DeadCode(context.Background(), w.ws)
	testutil.Equal(t, dead.Findings, []string{"src/old.ts: unused file ", "src/a.ts: unused dependencies lodash", "src/a.ts: unused exports unused"})
	knipCall, _ := w.fake.Find(cmdKnip)
	testutil.Equal(t, knipCall.Args[1:], []string{flagConfig, filepath.Join(tools(w.ws), knipConfigFile), "--reporter", formatJSON, "--no-progress", "--no-config-hints"})
	w.on(cmdKnip, "nope", errSim)
	testutil.Contains(t, DeadCode(context.Background(), w.ws).Reason, "knip")
	w.on(complexityConfigFile, "nope", errSim)
	testutil.Contains(t, Complexity(context.Background(), w.ws).Reason, "eslint")
	testutil.Equal(t, Complexity(context.Background(), newWorld(t, nil).ws).Reason, reasonNoLock)
	testutil.Equal(t, DeadCode(context.Background(), newWorld(t, nil).ws).Reason, reasonNoLock)
}

func vitestWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood, fileCart: cartSource, fileSpec: "import fc from \"fast-check\";\n", viteConfig: "", readme: ""})
	w.produce(flagOutputFile, junitGreen)
	w.produce("--coverage.reportsDirectory=", "")
	return w
}

func TestVitestRun(t *testing.T) {
	w := vitestWorld(t)
	staged(t, w)
	testutil.Write(t, filepath.Join(w.ws.Report(coverageDir), istanbulFile), strings.ReplaceAll(istanbul, "STAGE", stage(w.ws)))
	w.produce("--coverage.reportsDirectory=", "x")
	w.outputs = map[string]string{flagOutputFile: junitGreen}
	out := Tests(context.Background(), w.ws)
	testutil.Equal(t, out.Status, gate.StatusPass)
	provider, _ := w.fake.Find("npm install")
	testutil.Equal(t, procexectest.Line(provider), "npm install --no-save --no-audit --no-fund @vitest/coverage-v8@5.0.5")
	run, _ := w.fake.Find(cmdVitest)
	testutil.Equal(t, run.Args[1:], []string{cmdRun, flagJUnit, flagOutputFile + w.ws.Report(JUnitReport), "--coverage.enabled", "--coverage.provider=v8",
		"--coverage.reporter=json", "--coverage.reportsDirectory=" + w.ws.Report(coverageDir)})
	lines := LineCoverage(context.Background(), w.ws)
	testutil.Equal(t, lines.Values, map[string]float64{ValueLineCoverage: float64(2) * 100 / 3, ValueLines: 3})
	testutil.Equal(t, lines.Findings, []string{"src/cart.ts: 1 of 3 lines uncovered"})
	testutil.Equal(t, lines.Reason, reasonLines)
	branches := Branches(context.Background(), w.ws)
	testutil.Equal(t, branches.Values, map[string]float64{ValueBranchCover: 50, ValueBranches: 2})
	testutil.Equal(t, branches.Status, gate.StatusPass)
	passed, err := Passed(context.Background(), w.ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, passed, map[string]bool{"test/cart.test.ts > cart > works": true})
}

func TestTestFailures(t *testing.T) {
	testutil.Equal(t, Tests(context.Background(), newWorld(t, nil).ws).Reason, reasonNoLock)
	silent := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	silent.on(cmdVitest, "", errSim)
	testutil.Contains(t, Tests(context.Background(), silent.ws).Reason, reasonNoReport)
	noVersion := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	staged(t, noVersion)
	testutil.NoErr(t, os.RemoveAll(filepath.Join(stage(noVersion.ws), modulesDir)))
	testutil.Equal(t, Tests(context.Background(), noVersion.ws).Status, gate.StatusFail)
	badCoverage := vitestWorld(t)
	staged(t, badCoverage)
	testutil.Write(t, filepath.Join(badCoverage.ws.Report(coverageDir), istanbulFile), "{")
	badCoverage.outputs = map[string]string{flagOutputFile: junitGreen}
	testutil.Equal(t, Tests(context.Background(), badCoverage.ws).Status, gate.StatusFail)
	noManifest := newWorld(t, map[string]string{Manifest: "{", Lockfile: lockGood})
	testutil.Equal(t, Tests(context.Background(), noManifest.ws).Status, gate.StatusFail)
	missing := newWorld(t, nil)
	testutil.Contains(t, LineCoverage(context.Background(), missing.ws).Reason, "coverage report missing")
	testutil.Contains(t, Branches(context.Background(), missing.ws).Reason, "coverage report missing")
	_, err := Passed(context.Background(), missing.ws)
	testutil.Fails(t, err)
	testutil.Equal(t, ratio(0, 0), float64(percentScale))
}

func TestNodeTestRunnerWithLcov(t *testing.T) {
	w := newWorld(t, map[string]string{Manifest: plainManifest, Lockfile: lockGood, "src/grade.js": "export const a = 1;\nexport const b = 2;\n", "src/extra.js": "export const c = 3;\n// note\n"})
	staged(t, w)
	w.produce("--test-reporter-destination=", "")
	w.outputs = map[string]string{}
	testutil.Write(t, w.ws.Report(JUnitReport), `<testsuites><testcase name="top" classname="test" file="STAGE/test/a.test.js"/></testsuites>`)
	testutil.Write(t, w.ws.Report(lcovFile), "TN:\nSF:src/grade.js\nDA:1,1\nDA:2,0\nBRDA:1,0,0,1\nBRDA:1,1,0,-\nBRDA:2,2,0,0\nend_of_record\nDA:9,9\n")
	testutil.Equal(t, Tests(context.Background(), w.ws).Status, gate.StatusPass)
	run, _ := w.fake.Find("--experimental-test-coverage")
	testutil.Equal(t, run.Args[0], flagTest)
	cov, err := loadCoverage(w.ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, cov["src/grade.js"], &fileCoverage{Lines: map[int]int{1: 1, 2: 0}, Branches: 3, CoveredBranches: 1})
	testutil.Equal(t, cov["src/extra.js"], &fileCoverage{Lines: map[int]int{1: 0}})
	passed, err := Passed(context.Background(), w.ws)
	testutil.NoErr(t, err)
	testutil.Equal(t, len(passed), 1)
	blocked := newWorld(t, map[string]string{Manifest: plainManifest, Lockfile: lockGood})
	staged(t, blocked)
	testutil.Write(t, blocked.ws.Report(JUnitReport), `<testsuite><testcase name="t" classname="c"/></testsuite>`)
	testutil.Mkdir(t, blocked.ws.Report(lcovFile))
	testutil.Equal(t, Tests(context.Background(), blocked.ws).Status, gate.StatusFail)
	gone := newWorld(t, map[string]string{Manifest: plainManifest, Lockfile: lockGood, "src/gone.js": "x"})
	staged(t, gone)
	testutil.Write(t, gone.ws.Report(JUnitReport), `<testsuite><testcase name="t" classname="c"/></testsuite>`)
	testutil.NoErr(t, os.Remove(filepath.Join(stage(gone.ws), "src", "gone.js")))
	testutil.Equal(t, Tests(context.Background(), gone.ws).Status, gate.StatusFail)
}

func TestJestResults(t *testing.T) {
	w := newWorld(t, map[string]string{Manifest: `{"devDependencies":{"jest":"30"}}`, Lockfile: lockGood})
	staged(t, w)
	report := `{"testResults":[{"name":"STAGE/test/a.test.js","assertionResults":[
{"ancestorTitles":["math"],"title":"adds","status":"passed"},
{"ancestorTitles":[],"title":"breaks","status":"failed","failureMessages":["Expected 1\nReceived 2"]},
{"ancestorTitles":["math"],"title":"later","status":"pending"}]}]}`
	w.produce(flagOutputFile, report)
	out := Tests(context.Background(), w.ws)
	testutil.Equal(t, out.Findings, []string{"test/a.test.js > breaks failed: Expected 1", "test/a.test.js > math > later skipped: pending"})
	run, _ := w.fake.Find("jest.js")
	testutil.Contains(t, procexectest.Line(run), "--ci --coverage --coverageProvider=v8 --coverageReporters=json", "--json --outputFile="+w.ws.Report(JestReport))
	_, err := jestCases(filepath.Join(t.TempDir(), "missing.json"))
	testutil.Fails(t, err)
}

func TestMochaArguments(t *testing.T) {
	w := newWorld(t, map[string]string{Manifest: `{"devDependencies":{"mocha":"11"}}`, Lockfile: lockGood})
	testutil.Equal(t, adapters[runnerMocha].test(w.ws)[0], toolScript(w.ws, "c8", "bin", "c8.js"))
	testutil.Equal(t, adapters[runnerMocha].mutant(w.ws), []string{local(w.ws, "mocha", "bin", "mocha.js"), flagBail, flagReporter, "dot"})
	args, env := adapters[runnerMocha].fuzz(w.ws, []string{"test/p.test.js"})
	testutil.Equal(t, args[len(args)-1], "test/p.test.js")
	testutil.Equal(t, env, []string{envNodeOptions + filepath.Join(tools(w.ws), fuzzSetupFile)})
	jestArgs, jestEnv := adapters[runnerJest].fuzz(w.ws, []string{"t.test.js"})
	testutil.Contains(t, strings.Join(jestArgs, " "), "--setupFilesAfterEnv "+filepath.Join(tools(w.ws), fuzzSetupCJS))
	testutil.Equal(t, jestEnv, []string(nil))
	testutil.Equal(t, adapters[runnerNode].mutant(w.ws), []string{flagTest})
	testutil.Equal(t, adapters[runnerJest].mutant(w.ws)[1:], []string{flagCI, flagBail})
}

func TestFuzz(t *testing.T) {
	w := vitestWorld(t)
	w.produce(flagOutputFile, junitGreen)
	out := Fuzz(context.Background(), w.ws)
	testutil.Equal(t, out.Status, gate.StatusPass)
	testutil.Equal(t, out.Values, map[string]float64{ValueFuzzFiles: 1})
	run, _ := w.fake.Find(cmdVitest)
	testutil.Equal(t, run.Args[len(run.Args)-1], fileSpec)
	testutil.Equal(t, run.Env[1:], []string{envNodeOptions + filepath.Join(tools(w.ws), fuzzSetupFile), envFuzzRuns + "250"})
	failing := vitestWorld(t)
	failing.produce(flagOutputFile, `<testsuite><testcase classname="test/cart.test.ts" name="prop"><failure message="Counterexample"/></testcase></testsuite>`)
	bad := Fuzz(context.Background(), failing.ws)
	testutil.Equal(t, bad.Reason, reasonFuzz)
	testutil.Equal(t, bad.Findings, []string{"test/cart.test.ts > prop failed: Counterexample"})
	none := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood, fileSpec: "plain"})
	testutil.Equal(t, Fuzz(context.Background(), none.ws).Reason, reasonNoFuzz)
	silent := vitestWorld(t)
	silent.outputs = map[string]string{}
	testutil.Contains(t, Fuzz(context.Background(), silent.ws).Reason, reasonNoReport)
	testutil.Equal(t, Fuzz(context.Background(), newWorld(t, nil).ws).Reason, reasonNoLock)
	brokenManifest := newWorld(t, map[string]string{Manifest: "{", Lockfile: lockGood, fileSpec: "fast-check"})
	testutil.Equal(t, Fuzz(context.Background(), brokenManifest.ws).Status, gate.StatusFail)
	unreadable := vitestWorld(t)
	unreadable.files = append(unreadable.files, "test/gone.test.ts")
	staged(t, newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood}))
	testutil.NoErr(t, prepareWithoutFiles(unreadable))
	testutil.Equal(t, Fuzz(context.Background(), unreadable.ws).Status, gate.StatusFail)
}

func prepareWithoutFiles(w *world) error {
	files := w.files
	w.files = slices.DeleteFunc(slices.Clone(files), func(f string) bool { return strings.Contains(f, "gone") })
	defer func() { w.files = files }()
	return prepare(context.Background(), w.ws)
}

func TestClassificationHelpers(t *testing.T) {
	for _, rel := range []string{"test/a.ts", "tests/b.js", "src/c.test.ts", "src/d.spec.js", "pkg/test/e.js", "pkg/tests/f.js", "src/__tests__/g.ts"} {
		testutil.True(t, isTest(rel), rel)
	}
	for _, rel := range []string{"src/a.ts", "src/testing.ts", "latest/x.js"} {
		testutil.True(t, !isTest(rel), rel)
	}
	testutil.True(t, isConfig(viteConfig) && !isConfig("src/configure.ts"), "config files")
	testutil.Equal(t, codeLines("a.ts", []byte("const a = 1;\n// note\n\nconst b = 2;\n")), []int{1, 4})
	testutil.Equal(t, countPositive([]int{0, 2, 1, 0}), 2)
	testutil.Equal(t, exitCode(errSim), 0)
	testutil.Equal(t, firstLine("a\nb"), "a")
	c := junit.Case{Classname: "suite", Name: "case"}
	testutil.Equal(t, testID(newWorld(t, nil).ws)(c), "suite > case")
	unknown := junit.Case{}
	setStatus(&unknown, "todo", "")
	testutil.Equal(t, unknown.Skipped.Message, "todo")
}
