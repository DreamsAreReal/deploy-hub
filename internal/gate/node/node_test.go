package node

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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

var errSim = packtest.ErrSim

var nodePolicy = policy.Policy{
	Thresholds:   policy.Thresholds{LineCoverage: 90, BranchCoverage: 50, MaxCognitive: 9, MutationScore: 90},
	Limits:       policy.Limits{FuzzExamples: 250, MutantFloor: time.Second, MutantFactor: 2},
	Dependencies: []string{"npm:vitest", "npm:b", "example.com/go"},
}

type world struct {
	*packtest.World
}

func newWorld(t *testing.T, files map[string]string) *world {
	t.Helper()
	w := &world{World: packtest.New(t, files, nodePolicy)}
	w.Expand = func(content string) string { return strings.ReplaceAll(content, "STAGE", stage(w.WS)) }
	w.Hook = w.hook
	testutil.Write(t, filepath.Join(w.WS.PolicyDir, policySub, toolsManifest), `{"name":"tools"}`)
	testutil.Write(t, filepath.Join(w.WS.PolicyDir, policySub, toolsLock), `{"lockfileVersion":3}`)
	return w
}

func (w *world) hook(c procexec.Command, line string) (procexec.Result, error) {
	if c.Name == NPMBinary && strings.Contains(line, cmdNpmCI) && !strings.Contains(line, "--ignore-scripts") {
		testutil.Mkdir(w.T, filepath.Join(c.Dir, modulesDir, "vitest"))
		testutil.Write(w.T, filepath.Join(c.Dir, modulesDir, "vitest", Manifest), `{"version":"5.0.5"}`)
	}
	if w.Broken != nil && strings.Contains(line, cmdSyntax) && w.Broken(testutil.Read(w.T, c.Args[len(c.Args)-1])) {
		return procexec.Result{}, errSim
	}
	if w.Kill != nil && strings.Contains(line, "--bail") {
		return procexec.Result{}, w.Kill(w.Read(fileCart, c))
	}
	return procexec.Result{}, packtest.ErrPass
}

func staged(t *testing.T, w *world) {
	t.Helper()
	testutil.NoErr(t, prepare(context.Background(), w.WS))
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
	lines := w.Fake.Lines()
	testutil.Equal(t, lines[1], "npm ci --no-audit --no-fund")
	testutil.Equal(t, lines[2], "npm ci --ignore-scripts --no-audit --no-fund")
	calls := w.Fake.Calls()
	testutil.Equal(t, calls[1].Dir, stage(w.WS))
	testutil.Equal(t, calls[2].Dir, tools(w.WS))
	testutil.Equal(t, calls[1].Env, []string{envCache + w.WS.Report(cacheDir)})
	testutil.Equal(t, testutil.Read(t, filepath.Join(stage(w.WS), fileCart)), cartSource)
	testutil.Equal(t, testutil.Read(t, filepath.Join(tools(w.WS), Manifest)), `{"name":"tools"}`)
	testutil.Equal(t, testutil.Read(t, filepath.Join(tools(w.WS), Lockfile)), `{"lockfileVersion":3}`)
	testutil.Contains(t, testutil.Read(t, filepath.Join(tools(w.WS), eslintConfigFile)), `"sonarjs/cognitive-complexity": ["error", 9]`,
		"...globals.browser, ...globals.serviceworker, ...globals.node", `"**/testdata/**"`)
	testutil.Contains(t, testutil.Read(t, filepath.Join(tools(w.WS), knipConfigFile)), `"ignore": ["**/testdata/**"]`)
	testutil.Contains(t, testutil.Read(t, filepath.Join(tools(w.WS), complexityConfigFile)), `["error", 9]`)
	testutil.Equal(t, testutil.Read(t, w.WS.Report(readyStamp)), readyStamp)
	staged(t, w)
	testutil.Equal(t, len(w.Fake.Lines()), 3)
	version, err := installedVersion(stage(w.WS), "vitest")
	testutil.NoErr(t, err)
	testutil.Equal(t, version, "5.0.5")
	_, err = installedVersion(stage(w.WS), "missing")
	testutil.Fails(t, err)
}

func TestPrepareFailures(t *testing.T) {
	testutil.Is(t, prepare(context.Background(), newWorld(t, map[string]string{Manifest: manifestVite}).WS), ErrNoLock)
	ci := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	ci.On(cmdNpmCI, "", errSim)
	testutil.Contains(t, prepare(context.Background(), ci.WS).Error(), "npm ci")
	toolsCI := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	toolsCI.On("--ignore-scripts", "", errSim)
	testutil.Fails(t, prepare(context.Background(), toolsCI.WS))
	noPolicy := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	testutil.NoErr(t, os.RemoveAll(filepath.Join(noPolicy.WS.PolicyDir, policySub)))
	testutil.Fails(t, prepare(context.Background(), noPolicy.WS))
	unlisted := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	unlisted.Files = append(unlisted.Files, "gone.ts")
	testutil.Fails(t, prepare(context.Background(), unlisted.WS))
	blocked := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	testutil.Write(t, tools(blocked.WS), "")
	testutil.Fails(t, prepare(context.Background(), blocked.WS))
	scripts := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	testutil.Mkdir(t, filepath.Join(tools(scripts.WS), eslintConfigFile))
	testutil.Fails(t, prepare(context.Background(), scripts.WS))
}

func TestRelativePaths(t *testing.T) {
	w := newWorld(t, nil)
	testutil.Mkdir(t, stage(w.WS))
	testutil.Equal(t, rel(w.WS, filepath.Join(stage(w.WS), "src", "a.ts")), "src/a.ts")
	resolved, err := filepath.EvalSymlinks(stage(w.WS))
	testutil.NoErr(t, err)
	testutil.Equal(t, rel(w.WS, filepath.Join(resolved, "b.ts")), "b.ts")
	testutil.Equal(t, rel(w.WS, "/elsewhere/c.ts"), "/elsewhere/c.ts")
	testutil.Equal(t, rel(newWorld(t, nil).WS, "x/y.ts"), "x/y.ts")
}

func TestIntegrityAndDependencies(t *testing.T) {
	stale := `{"lockfileVersion":1,"packages":{"":{},"node_modules/x":{"integrity":"","resolved":"https://r/x"},"node_modules/y":{"integrity":"sha","resolved":"git+ssh://y"}}}`
	w := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: stale})
	testutil.Equal(t, Integrity(context.Background(), w.WS).Findings, []string{
		"package-lock.json: lockfileVersion 1 is older than 2",
		"package-lock.json: x has no integrity hash or registry source",
		"package-lock.json: y has no integrity hash or registry source",
	})
	testutil.Equal(t, w.Fake.Count(cmdNpmCI), 0)
	good := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	testutil.Equal(t, Integrity(context.Background(), good.WS).Status, gate.StatusPass)
	failing := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	failing.On(cmdNpmCI, "", errSim)
	testutil.Contains(t, Integrity(context.Background(), failing.WS).Findings[0], "npm ci")
	testutil.Equal(t, Integrity(context.Background(), newWorld(t, nil).WS).Reason, reasonNoLock)
	testutil.Equal(t, Integrity(context.Background(), newWorld(t, map[string]string{Lockfile: "{"}).WS).Status, gate.StatusFail)
	deps := Dependencies(context.Background(), good.WS)
	testutil.Equal(t, deps.Findings, []string{"npm:bundled is not in the approved dependency list"})
	testutil.Equal(t, deps.Values[pack.ValueDependencies], float64(3))
	testutil.Equal(t, Dependencies(context.Background(), newWorld(t, nil).WS).Reason, reasonNoLock)
	testutil.Equal(t, packageName("node_modules/@scope/a/node_modules/b"), "b")
	testutil.Equal(t, packageName("plain"), "plain")
}

const eslintOut = `[{"filePath":"STAGE/src/cart.ts","messages":[{"ruleId":"no-magic-numbers","message":"No magic number: 42.","line":3}]},{"filePath":"STAGE/src/ok.ts","messages":[]}]`

func TestLint(t *testing.T) {
	w := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood, fileCart: cartSource, tsconfigFile: "{}"})
	staged(t, w)
	w.On(cmdESLint, strings.ReplaceAll(eslintOut, "STAGE", stage(w.WS)), errSim)
	w.On(cmdTSC, "src/cart.ts(4,10): error TS2322: Type 'number' is not assignable to type 'string'.\nnoise\n", errSim)
	out := Lint(context.Background(), w.WS)
	testutil.Equal(t, out.Findings, []string{
		"src/cart.ts:3 no-magic-numbers No magic number: 42.",
		"src/cart.ts:4 TS2322 Type 'number' is not assignable to type 'string'.",
	})
	eslintCall, _ := w.Fake.Find(cmdESLint)
	testutil.Equal(t, eslintCall.Args[1:], []string{flagConfig, filepath.Join(tools(w.WS), eslintConfigFile), "--no-config-lookup", flagFormat, formatJSON, "."})
	tscCall, _ := w.Fake.Find(cmdTSC)
	testutil.Equal(t, tscCall.Args, []string{toolScript(w.WS, "typescript", "bin", "tsc"), "--noEmit", "--strict", "--pretty", "false", "-p", tsconfigFile})
	testutil.Mkdir(t, filepath.Join(stage(w.WS), modulesDir, "typescript", "bin"))
	testutil.Write(t, filepath.Join(stage(w.WS), modulesDir, "typescript", "bin", "tsc"), "")
	Lint(context.Background(), w.WS)
	calls := w.Fake.Calls()
	testutil.Equal(t, calls[len(calls)-1].Args[0], filepath.Join(stage(w.WS), modulesDir, "typescript", "bin", "tsc"))
}

func TestLintFailures(t *testing.T) {
	garbled := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	garbled.On(cmdESLint, "oops", errSim)
	testutil.Contains(t, Lint(context.Background(), garbled.WS).Reason, "eslint")
	tscCrash := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood, fileCart: cartSource})
	tscCrash.On(cmdESLint, "[]", nil)
	tscCrash.On(cmdTSC, "", errSim)
	testutil.Contains(t, Lint(context.Background(), tscCrash.WS).Reason, "node tsc")
	plain := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood, "src/a.js": "export const a = 1;\n"})
	plain.On(cmdESLint, "[]", nil)
	testutil.Equal(t, Lint(context.Background(), plain.WS).Status, gate.StatusPass)
	testutil.Equal(t, plain.Fake.Count(cmdTSC), 0)
	noConfig := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood, fileCart: cartSource})
	noConfig.On(cmdESLint, "[]", nil)
	Lint(context.Background(), noConfig.WS)
	tscCall, _ := noConfig.Fake.Find(cmdTSC)
	testutil.True(t, !slices.Contains(tscCall.Args, "-p"), "no tsconfig, default options")
	testutil.Equal(t, Lint(context.Background(), newWorld(t, nil).WS).Reason, reasonNoLock)
	testutil.True(t, !hasTypeScript(filepath.Join(t.TempDir(), "missing")), "missing dir")
	hidden := t.TempDir()
	testutil.Write(t, filepath.Join(hidden, modulesDir, "dep", "index.ts"), "")
	testutil.True(t, !hasTypeScript(hidden), "dependencies are not the project")
}

func TestComplexityAndDeadCode(t *testing.T) {
	w := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	staged(t, w)
	w.On(complexityConfigFile, `[{"filePath":"`+stage(w.WS)+`/src/a.ts","messages":[{"ruleId":"sonarjs/cognitive-complexity","message":"Refactor","line":2}]}]`, errSim)
	testutil.Equal(t, Complexity(context.Background(), w.WS).Findings, []string{"src/a.ts:2 sonarjs/cognitive-complexity Refactor"})
	w.On(cmdKnip, `{"files":["src/old.ts"],"issues":[{"file":"src/a.ts","exports":[{"name":"unused"}],"dependencies":[{"name":"lodash"}],"duplicates":"skip"}]}`, errSim)
	dead := DeadCode(context.Background(), w.WS)
	testutil.Equal(t, dead.Findings, []string{"src/old.ts: unused file ", "src/a.ts: unused dependencies lodash", "src/a.ts: unused exports unused"})
	knipCall, _ := w.Fake.Find(cmdKnip)
	testutil.Equal(t, knipCall.Args[1:], []string{flagConfig, filepath.Join(tools(w.WS), knipConfigFile), "--reporter", formatJSON, "--no-progress", "--no-config-hints"})
	w.On(cmdKnip, "nope", errSim)
	testutil.Contains(t, DeadCode(context.Background(), w.WS).Reason, "knip")
	w.On(complexityConfigFile, "nope", errSim)
	testutil.Contains(t, Complexity(context.Background(), w.WS).Reason, "eslint")
	testutil.Equal(t, Complexity(context.Background(), newWorld(t, nil).WS).Reason, reasonNoLock)
	testutil.Equal(t, DeadCode(context.Background(), newWorld(t, nil).WS).Reason, reasonNoLock)
}

func vitestWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood, fileCart: cartSource, fileSpec: "import fc from \"fast-check\";\n", viteConfig: "", readme: "",
		"test/testdata/old.test.ts": fastCheck})
	w.Produce(flagOutputFile, junitGreen)
	w.Produce("--coverage.reportsDirectory=", "")
	return w
}

func TestVitestRun(t *testing.T) {
	w := vitestWorld(t)
	staged(t, w)
	testutil.Write(t, filepath.Join(w.WS.Report(coverageDir), istanbulFile), strings.ReplaceAll(istanbul, "STAGE", stage(w.WS)))
	w.Produce("--coverage.reportsDirectory=", "x")
	w.Clear()
	w.Produce(flagOutputFile, junitGreen)
	out := Tests(context.Background(), w.WS)
	testutil.Equal(t, out.Status, gate.StatusPass)
	provider, _ := w.Fake.Find("npm install")
	testutil.Equal(t, procexectest.Line(provider), "npm install --no-save --no-audit --no-fund @vitest/coverage-v8@5.0.5")
	run, _ := w.Fake.Find(cmdVitest)
	testutil.Equal(t, run.Args[1:], []string{cmdRun, flagJUnit, flagOutputFile + w.WS.Report(JUnitReport), "--coverage.enabled", "--coverage.provider=v8",
		"--coverage.reporter=json", "--coverage.reportsDirectory=" + w.WS.Report(coverageDir)})
	lines := linecov.LineCoverage(context.Background(), w.WS)
	testutil.Equal(t, lines.Values, map[string]float64{linecov.ValueLineCoverage: float64(2) * 100 / 3, linecov.ValueLines: 3})
	testutil.Equal(t, lines.Findings, []string{"src/cart.ts: 1 of 3 lines uncovered"})
	testutil.Contains(t, lines.Reason, "line coverage")
	passed, err := Passed(context.Background(), w.WS)
	testutil.NoErr(t, err)
	testutil.Equal(t, passed, map[string]bool{"test/cart.test.ts > cart > works": true})
	branches := linecov.Branches(context.Background(), w.WS)
	testutil.Equal(t, branches.Status, gate.StatusPass)
	testutil.Equal(t, branches.Values, map[string]float64{linecov.ValueBranchCover: 50, linecov.ValueBranches: 2})
}

func TestTestFailures(t *testing.T) {
	testutil.Equal(t, Tests(context.Background(), newWorld(t, nil).WS).Reason, reasonNoLock)
	silent := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	silent.On(cmdVitest, "", errSim)
	testutil.Contains(t, Tests(context.Background(), silent.WS).Reason, reasonNoReport)
	noVersion := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood})
	staged(t, noVersion)
	testutil.NoErr(t, os.RemoveAll(filepath.Join(stage(noVersion.WS), modulesDir)))
	testutil.Equal(t, Tests(context.Background(), noVersion.WS).Status, gate.StatusFail)
	badCoverage := vitestWorld(t)
	staged(t, badCoverage)
	testutil.Write(t, filepath.Join(badCoverage.WS.Report(coverageDir), istanbulFile), "{")
	badCoverage.Clear()
	badCoverage.Produce(flagOutputFile, junitGreen)
	testutil.Equal(t, Tests(context.Background(), badCoverage.WS).Status, gate.StatusFail)
	noManifest := newWorld(t, map[string]string{Manifest: "{", Lockfile: lockGood})
	testutil.Equal(t, Tests(context.Background(), noManifest.WS).Status, gate.StatusFail)
	missing := newWorld(t, nil)
	_, err := Passed(context.Background(), missing.WS)
	testutil.Fails(t, err)
}

func TestNodeTestRunnerWithLcov(t *testing.T) {
	w := newWorld(t, map[string]string{Manifest: plainManifest, Lockfile: lockGood, "src/grade.js": "export const a = 1;\nexport const b = 2;\n", "src/extra.js": "export const c = 3;\n// note\n", "src/testdata/old.js": "var d = 4;\n"})
	staged(t, w)
	w.Produce("--test-reporter-destination=", "")
	w.Clear()
	testutil.Write(t, w.WS.Report(JUnitReport), `<testsuites><testcase name="top" classname="test" file="STAGE/test/a.test.js"/></testsuites>`)
	testutil.Write(t, w.WS.Report(lcovFile), "TN:\nSF:src/grade.js\nDA:1,1\nDA:2,0\nBRDA:1,0,0,1\nBRDA:1,1,0,-\nBRDA:2,2,0,0\nend_of_record\nDA:9,9\n")
	testutil.Equal(t, Tests(context.Background(), w.WS).Status, gate.StatusPass)
	run, _ := w.Fake.Find("--experimental-test-coverage")
	testutil.Equal(t, run.Args[0], flagTest)
	cov, err := linecov.Load(w.WS)
	testutil.NoErr(t, err)
	testutil.Equal(t, cov["src/grade.js"], &linecov.File{Lines: map[int]int{1: 1, 2: 0}, Branches: 3, CoveredBranches: 1})
	testutil.Equal(t, cov["src/extra.js"], &linecov.File{Lines: map[int]int{1: 0}})
	testutil.True(t, cov["src/testdata/old.js"] == nil, "fixtures are not production code")
	passed, err := Passed(context.Background(), w.WS)
	testutil.NoErr(t, err)
	testutil.Equal(t, len(passed), 1)
	blocked := newWorld(t, map[string]string{Manifest: plainManifest, Lockfile: lockGood})
	staged(t, blocked)
	testutil.Write(t, blocked.WS.Report(JUnitReport), `<testsuite><testcase name="t" classname="c"/></testsuite>`)
	testutil.Mkdir(t, blocked.WS.Report(lcovFile))
	testutil.Equal(t, Tests(context.Background(), blocked.WS).Status, gate.StatusFail)
	gone := newWorld(t, map[string]string{Manifest: plainManifest, Lockfile: lockGood, "src/gone.js": "x"})
	staged(t, gone)
	testutil.Write(t, gone.WS.Report(JUnitReport), `<testsuite><testcase name="t" classname="c"/></testsuite>`)
	testutil.NoErr(t, os.Remove(filepath.Join(stage(gone.WS), "src", "gone.js")))
	testutil.Equal(t, Tests(context.Background(), gone.WS).Status, gate.StatusFail)
}

func TestJestResults(t *testing.T) {
	w := newWorld(t, map[string]string{Manifest: `{"devDependencies":{"jest":"30"}}`, Lockfile: lockGood})
	staged(t, w)
	report := `{"testResults":[{"name":"STAGE/test/a.test.js","assertionResults":[
{"ancestorTitles":["math"],"title":"adds","status":"passed"},
{"ancestorTitles":[],"title":"breaks","status":"failed","failureMessages":["Expected 1\nReceived 2"]},
{"ancestorTitles":["math"],"title":"later","status":"pending"}]}]}`
	w.Produce(flagOutputFile, report)
	out := Tests(context.Background(), w.WS)
	testutil.Equal(t, out.Findings, []string{"test/a.test.js > breaks failed: Expected 1", "test/a.test.js > math > later skipped: pending"})
	run, _ := w.Fake.Find("jest.js")
	testutil.Contains(t, procexectest.Line(run), "--ci --coverage --coverageProvider=v8 --coverageReporters=json", "--json --outputFile="+w.WS.Report(JestReport))
	_, err := jestCases(filepath.Join(t.TempDir(), "missing.json"))
	testutil.Fails(t, err)
}

func TestMochaArguments(t *testing.T) {
	w := newWorld(t, map[string]string{Manifest: `{"devDependencies":{"mocha":"11"}}`, Lockfile: lockGood})
	testutil.Equal(t, adapters[runnerMocha].test(w.WS)[0], toolScript(w.WS, "c8", "bin", "c8.js"))
	testutil.Equal(t, adapters[runnerMocha].mutant(w.WS), []string{local(w.WS, "mocha", "bin", "mocha.js"), flagBail, flagReporter, "dot"})
	args, env := adapters[runnerMocha].fuzz(w.WS, []string{"test/p.test.js"})
	testutil.Equal(t, args[len(args)-1], "test/p.test.js")
	testutil.Equal(t, env, []string{envNodeOptions + filepath.Join(tools(w.WS), fuzzSetupFile)})
	jestArgs, jestEnv := adapters[runnerJest].fuzz(w.WS, []string{"t.test.js"})
	testutil.Contains(t, strings.Join(jestArgs, " "), "--setupFilesAfterEnv "+filepath.Join(tools(w.WS), fuzzSetupCJS))
	testutil.Equal(t, jestEnv, []string(nil))
	testutil.Equal(t, adapters[runnerNode].mutant(w.WS), []string{flagTest})
	testutil.Equal(t, adapters[runnerJest].mutant(w.WS)[1:], []string{flagCI, flagBail})
}

func TestFastCheckFuzz(t *testing.T) {
	w := vitestWorld(t)
	w.Produce(flagOutputFile, junitGreen)
	out := Fuzz(context.Background(), w.WS)
	testutil.Equal(t, out.Status, gate.StatusPass)
	testutil.Equal(t, out.Values, map[string]float64{ValueFuzzFiles: 1})
	run, _ := w.Fake.Find(cmdVitest)
	testutil.Equal(t, run.Args[len(run.Args)-1], fileSpec)
	testutil.Equal(t, run.Env[1:], []string{envNodeOptions + filepath.Join(tools(w.WS), fuzzSetupFile), envFuzzRuns + "250"})
	failing := vitestWorld(t)
	failing.Produce(flagOutputFile, `<testsuite><testcase classname="test/cart.test.ts" name="prop"><failure message="Counterexample"/></testcase></testsuite>`)
	bad := Fuzz(context.Background(), failing.WS)
	testutil.Equal(t, bad.Reason, reasonFuzz)
	testutil.Equal(t, bad.Findings, []string{"test/cart.test.ts > prop failed: Counterexample"})
	none := newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood, fileSpec: "plain"})
	testutil.Equal(t, Fuzz(context.Background(), none.WS).Reason, reasonNoFuzz)
	silent := vitestWorld(t)
	silent.Clear()
	testutil.Contains(t, Fuzz(context.Background(), silent.WS).Reason, reasonNoReport)
	testutil.Equal(t, Fuzz(context.Background(), newWorld(t, nil).WS).Reason, reasonNoLock)
	brokenManifest := newWorld(t, map[string]string{Manifest: "{", Lockfile: lockGood, fileSpec: "fast-check"})
	testutil.Equal(t, Fuzz(context.Background(), brokenManifest.WS).Status, gate.StatusFail)
	unreadable := vitestWorld(t)
	unreadable.Files = append(unreadable.Files, "test/gone.test.ts")
	staged(t, newWorld(t, map[string]string{Manifest: manifestVite, Lockfile: lockGood}))
	testutil.NoErr(t, prepareWithoutFiles(unreadable))
	testutil.Equal(t, Fuzz(context.Background(), unreadable.WS).Status, gate.StatusFail)
}

func prepareWithoutFiles(w *world) error {
	files := w.Files
	w.Files = slices.DeleteFunc(slices.Clone(files), func(f string) bool { return strings.Contains(f, "gone") })
	defer func() { w.Files = files }()
	return prepare(context.Background(), w.WS)
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
	testutil.Equal(t, procexec.ExitCode(errSim), 0)
	testutil.Equal(t, firstLine("a\nb"), "a")
	c := junit.Case{Classname: "suite", Name: "case"}
	testutil.Equal(t, testID(newWorld(t, nil).WS)(c), "suite > case")
	unknown := junit.Case{}
	setStatus(&unknown, "todo", "")
	testutil.Equal(t, unknown.Skipped.Message, "todo")
}

func TestToolsInstallOnce(t *testing.T) {
	w := newWorld(t, nil)
	dir, err := Tools(context.Background(), w.WS)
	testutil.NoErr(t, err)
	testutil.Equal(t, dir, tools(w.WS))
	testutil.Contains(t, testutil.Read(t, filepath.Join(dir, HTMLValidateConfig)), `"require-sri": ["error", {"target": "crossorigin"}]`)
	testutil.Contains(t, testutil.Read(t, filepath.Join(dir, StylelintConfig)), "stylelint-config-standard")
	_, err = Tools(context.Background(), w.WS)
	testutil.NoErr(t, err)
	testutil.Equal(t, w.Fake.Count("--ignore-scripts"), 1)
	testutil.Equal(t, ToolScript(w.WS, "x", "y.js"), filepath.Join(tools(w.WS), modulesDir, "x", "y.js"))
	failing := newWorld(t, nil)
	failing.On("--ignore-scripts", "", errSim)
	_, err = Tools(context.Background(), failing.WS)
	testutil.Fails(t, err)
}
