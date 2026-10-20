package golang

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	module      = "example.com/app"
	cmdList     = "go list"
	cmdListJSON = cmdList + " -json"
	cmdListMod  = cmdList + " -m"
	cmdLint     = LintBinary + " run"
	cmdEnv      = "go env"
	cmdEnvJSON  = cmdEnv + " -json"
	cmdTest     = "go test"
	cmdMod      = "go mod"
	fileMain    = "m.go"
	fileLibTest = "lib_test.go"
	testA       = "lib.TestA"
)

var errExit = errors.New("exit status 1")

type (
	reply  = procexectest.Reply
	script = procexectest.Script
)

func ws(t *testing.T, s script) gate.Workspace {
	t.Helper()
	return gate.Workspace{
		Src: t.TempDir(), Out: t.TempDir(), PolicyDir: "/policy", Module: module,
		Exec: &procexectest.Fake{Handler: s.Handle}, Log: slog.New(slog.DiscardHandler),
		Policy: policy.Policy{
			Thresholds:   policy.Thresholds{LineCoverage: 80, BranchCoverage: 80, MaxCognitive: 9},
			Limits:       policy.Limits{FuzzTime: time.Second},
			Dependencies: []string{"go.yaml.in/yaml/v3"},
		},
	}
}

func ok(stdout string) reply { return reply{Res: procexec.Result{Stdout: stdout}} }

func pkgJSON(t *testing.T, pkgs ...Package) string {
	t.Helper()
	var b strings.Builder
	for _, p := range pkgs {
		data, err := json.Marshal(p)
		testutil.NoErr(t, err)
		b.Write(data)
		b.WriteString("\n")
	}
	return b.String()
}

func samplePackages() Packages {
	return Packages{
		{ImportPath: module, Name: "main", GoFiles: []string{"main.go"}, Imports: []string{module + "/lib"}, Deps: []string{module + "/lib"}},
		{ImportPath: module + "/lib", Name: "lib", GoFiles: []string{"lib.go"}, TestGoFiles: []string{fileLibTest}, TestImports: []string{module + "/testkit"}},
		{ImportPath: module + "/testkit", Name: "testkit", GoFiles: []string{"kit.go"}, Imports: []string{module + "/fakes"}},
		{ImportPath: module + "/fakes", Name: "fakes", GoFiles: []string{"fake.go"}},
		{ImportPath: module + "/e2e", Name: "e2e", XTestGoFiles: []string{"e2e_test.go"}, Deps: []string{module + "/lib"}, XTestImports: []string{module + "/fixtures", module + "/lib"}},
		{ImportPath: module + "/fixtures", Name: "fixtures", GoFiles: []string{"f.go"}},
		{ImportPath: module + "/orphan", Name: "orphan", GoFiles: []string{"o.go"}},
	}
}

func TestPackageClassification(t *testing.T) {
	pkgs := samplePackages()
	testutil.Equal(t, pkgs.TestSupport(), map[string]bool{module + "/testkit": true, module + "/fakes": true, module + "/fixtures": true})
	testutil.Equal(t, pkgs.Production().Paths(), []string{module, module + "/lib", module + "/orphan"})
	testutil.Equal(t, pkgs.Dependents(module+"/lib"), []string{module + "/e2e"})
	testutil.Equal(t, pkgs.Dependents(module+"/testkit"), []string{module + "/lib"})
	testutil.Equal(t, len(pkgs.Dependents(module+"/orphan")), 0)
	testutil.True(t, pkgs[1].HasTests() && pkgs[4].HasTests() && !pkgs[0].HasTests(), "HasTests")
}

func TestListAndModule(t *testing.T) {
	w := ws(t, script{cmdList + " -json ./...": ok(pkgJSON(t, samplePackages()...)), cmdListMod: ok(module + "\n")})
	pkgs, err := List(context.Background(), w)
	testutil.NoErr(t, err)
	testutil.Equal(t, len(pkgs), len(samplePackages()))
	m, err := Module(context.Background(), w)
	testutil.NoErr(t, err)
	testutil.Equal(t, m, module)
	bad := ws(t, script{cmdListJSON: {Res: procexec.Result{Stderr: "no go.mod"}, Err: errExit}, cmdListMod: {Err: errExit}})
	_, err = List(context.Background(), bad)
	testutil.Fails(t, err)
	_, err = Module(context.Background(), bad)
	testutil.Contains(t, err.Error(), "go list -m: ")
	_, err = decodePackages([]byte("{bad"))
	testutil.Fails(t, err)
}

func TestLint(t *testing.T) {
	report := `{"Issues":[{"FromLinter":"mnd","Text":"magic number","Pos":{"Filename":"a.go","Line":3}}]}`
	out := Lint(context.Background(), ws(t, script{cmdLint + " --config /policy/golangci.yml": {Res: procexec.Result{Stdout: report}, Err: errExit}}))
	testutil.Equal(t, out.Findings, []string{"a.go:3: magic number (mnd)"})
	clean := Lint(context.Background(), ws(t, script{cmdLint: ok(`{"Issues":[]}`)}))
	testutil.Equal(t, clean.Status, gate.StatusPass)
	broken := Lint(context.Background(), ws(t, script{cmdLint: {Res: procexec.Result{Stdout: "panic"}, Err: errExit}}))
	testutil.Contains(t, broken.Reason, "tool report unreadable")
}

func TestCognitive(t *testing.T) {
	entries := `[{"PkgName":"a","FuncName":"Hard","Complexity":12,"Pos":{"Filename":"SRC/a.go","Line":4}},{"PkgName":"a","FuncName":"Easy","Complexity":9,"Pos":{"Filename":"a.go","Line":9}}]`
	w := ws(t, nil)
	w.Exec = &procexectest.Fake{Handler: script{CognitBinary + " -json": ok(strings.ReplaceAll(entries, "SRC", w.Src))}.Handle}
	out := Cognitive(context.Background(), w)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Findings, []string{"a.go:4: Hard has cognitive complexity 12"})
	testutil.Equal(t, out.Values[ValueMaxCognit], float64(12))
	testutil.Equal(t, out.Values[ValueFunctions], float64(2))
	limit := ws(t, script{CognitBinary: ok(`[{"FuncName":"Edge","Complexity":9}]`)})
	testutil.Equal(t, Cognitive(context.Background(), limit).Status, gate.StatusPass)
	testutil.Contains(t, Cognitive(context.Background(), ws(t, script{CognitBinary: {Err: errExit}})).Reason, "gocognit failed")
	testutil.Contains(t, Cognitive(context.Background(), ws(t, script{CognitBinary: ok("[")})).Reason, "unreadable")
}

func TestModulesAndIntegrity(t *testing.T) {
	cleanEnv := `{"GOSUMDB":"sum.golang.org","GONOSUMDB":"","GONOSUMCHECK":"","GOINSECURE":"","GOPRIVATE":"","GOFLAGS":""}`
	good := Integrity(context.Background(), ws(t, script{cmdEnvJSON: ok(cleanEnv), cmdMod + " verify": ok("all modules verified"), cmdMod + " tidy -diff": ok("")}))
	testutil.Equal(t, good.Status, gate.StatusPass)
	weak := `{"GOSUMDB":"off","GONOSUMDB":"x","GONOSUMCHECK":"","GOINSECURE":"y","GOPRIVATE":"","GOFLAGS":"-mod=mod"}`
	bad := Integrity(context.Background(), ws(t, script{cmdEnvJSON: ok(weak), cmdMod + " verify": {Res: procexec.Result{Stderr: "dir has been modified"}, Err: errExit}, cmdMod + " tidy -diff": {Res: procexec.Result{Stdout: "+require x"}, Err: errExit}}))
	testutil.Equal(t, bad.Findings, []string{
		`GOSUMDB="off" must not be set`, `GONOSUMDB="x" must not be set`, `GOINSECURE="y" must not be set`, `GOFLAGS="-mod=mod" must not be set`,
		"go mod verify: dir has been modified", "go mod tidy -diff: +require x",
	})
	empty := Integrity(context.Background(), ws(t, script{cmdEnvJSON: ok(`{}`), cmdMod: ok("")}))
	testutil.Equal(t, empty.Findings, []string{`GOSUMDB="" must not be set`})
	testutil.Contains(t, Integrity(context.Background(), ws(t, script{cmdEnv: {Err: errExit}})).Reason, cmdEnv)
	testutil.Contains(t, Integrity(context.Background(), ws(t, script{cmdEnv: ok("{")})).Reason, "unreadable")
}

func TestDeadcode(t *testing.T) {
	w := ws(t, nil)
	pkgs := samplePackages()
	for i := range pkgs {
		pkgs[i].Dir = filepath.Join(w.Src, strings.TrimPrefix(strings.TrimPrefix(pkgs[i].ImportPath, module), "/"))
	}
	w.Exec = &procexectest.Fake{Handler: script{
		cmdListJSON:                       ok(pkgJSON(t, pkgs...)),
		DeadcodeBinary + " -test=false":   ok("lib/lib.go:3:6: unreachable func: Old\ntestkit/kit.go:2:6: unreachable func: Helper\n"),
		DeadcodeBinary + " -test=true":    ok("lib/lib.go:3:6: unreachable func: Old\nnoise line\n"),
		DeadcodeBinary + " -test=unknown": ok(""),
	}.Handle}
	out := Deadcode(context.Background(), w)
	testutil.Equal(t, out.Findings, []string{"lib/lib.go:3:6: unreachable func: Old"})
	testutil.Contains(t, Deadcode(context.Background(), ws(t, script{cmdList: {Err: errExit}})).Reason, cmdList)
	failing := ws(t, script{cmdListJSON: ok(""), DeadcodeBinary: {Err: errExit}})
	testutil.Contains(t, Deadcode(context.Background(), failing).Reason, DeadcodeBinary)
	clean := ws(t, script{cmdListJSON: ok(""), DeadcodeBinary: ok("")})
	testutil.Equal(t, Deadcode(context.Background(), clean).Status, gate.StatusPass)
}

const events = `{"Action":"start","Package":"example.com/app/lib"}
{"Action":"run","Package":"example.com/app/lib","Test":"TestA"}
{"Action":"pass","Package":"example.com/app/lib","Test":"TestA"}
{"Action":"pass","Package":"example.com/app/lib","Test":"TestA/sub"}
{"Action":"fail","Package":"example.com/app/lib","Test":"TestB"}
{"Action":"skip","Package":"example.com/app/lib","Test":"TestC"}
{"Action":"fail","Package":"example.com/app/lib"}
{"ImportPath":"example.com/app/broken","Action":"build-fail"}
{"Action":"output","Package":"example.com/app/lib","Output":"ok\n"}
# example.com/app/broken
`

func TestParseTestEvents(t *testing.T) {
	run, err := ParseTestEvents(events)
	testutil.NoErr(t, err)
	testutil.Equal(t, run.Tests, 4)
	testutil.Equal(t, run.Failed, 1)
	testutil.Equal(t, run.Skipped, 1)
	testutil.True(t, run.Passed[module+"/lib.TestA"] && run.Passed[module+"/lib.TestA/sub"] && !run.Passed[module+"/lib.TestB"], "passed set")
	testutil.Equal(t, run.Failures, []string{
		module + "/lib TestB failed", module + "/lib TestC skipped",
		module + "/lib: package failed (build error, panic or TestMain exit)", module + "/broken: build failed",
	})
	_, err = ParseTestEvents("{bad json\n")
	testutil.Fails(t, err)
	long := `{"Action":"output","Package":"p","Test":"TestL","Output":"` + strings.Repeat("x", bufio.MaxScanTokenSize) + `"}` + "\n" + `{"Action":"pass","Package":"p","Test":"TestL"}` + "\n"
	run, err = ParseTestEvents(long)
	testutil.NoErr(t, err)
	testutil.True(t, run.Passed["p.TestL"], "long output lines are read")
	_, err = ParseTestEvents(strings.Repeat("x", maxEventLine+1))
	testutil.Is(t, err, bufio.ErrTooLong)
}

func TestTestsCheck(t *testing.T) {
	w := ws(t, script{cmdListJSON: ok(pkgJSON(t, samplePackages()...)), cmdTest + " -json": {Res: procexec.Result{Stdout: events}, Err: errExit}})
	out := Tests(context.Background(), w)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, len(out.Findings), 4)
	testutil.Equal(t, out.Values[ValueTests], float64(4))
	testutil.Equal(t, out.Values[ValuePackages], float64(len(samplePackages())))
	testutil.Equal(t, testutil.Read(t, w.Report(TestsReport)), events)
	calls := procexectest.LinesOf(w.Exec)
	testutil.Contains(t, calls[1], "-coverpkg="+module+","+module+"/lib,"+module+"/orphan", "-race", "-coverprofile="+w.Report(CoverReport))
	green := `{"Action":"pass","Package":"example.com/app/lib","Test":"TestA"}` + "\n"
	pass := Tests(context.Background(), ws(t, script{cmdListJSON: ok(""), cmdTest: ok(green)}))
	testutil.Equal(t, pass.Status, gate.StatusPass)
	none := Tests(context.Background(), ws(t, script{cmdListJSON: ok(""), cmdTest: ok("")}))
	testutil.Equal(t, none.Status, gate.StatusFail)
	crash := Tests(context.Background(), ws(t, script{cmdListJSON: ok(""), cmdTest: {Res: procexec.Result{Stderr: "signal: killed"}, Err: errExit}}))
	testutil.Contains(t, crash.Reason, "go test failed: signal: killed")
	garbled := Tests(context.Background(), ws(t, script{cmdListJSON: ok(""), cmdTest: ok("{x\n")}))
	testutil.Contains(t, garbled.Reason, "test report unreadable")
	testutil.Contains(t, Tests(context.Background(), ws(t, script{cmdList: {Err: errExit}})).Reason, cmdList)
	unwritable := ws(t, script{cmdListJSON: ok(""), cmdTest: ok(green)})
	unwritable.Out = filepath.Join(unwritable.Out, "missing", "dir")
	testutil.Contains(t, Tests(context.Background(), unwritable).Reason, "cannot keep the test report")
}

const profile = `mode: atomic
example.com/app/lib/lib.go:3.10,5.2 2 0
example.com/app/lib/lib.go:3.10,5.2 2 1
example.com/app/lib/lib.go:6.10,8.2 1 0
example.com/app/lib/lib.go:8.3,9.2 1 0
example.com/app/main.go:3.13,4.2 1 0
example.com/app/main.go:5.13,6.2 3 4
`

func TestParseProfile(t *testing.T) {
	blocks, err := parseProfile(profile)
	testutil.NoErr(t, err)
	testutil.Equal(t, len(blocks), 5)
	testutil.Equal(t, blocks[0].count, int64(1))
	for _, bad := range []string{"x.go:1.1,2.2 1", "x.go:1,2 1 1", "x.go:1.1,2.2 a 1", "x.go:1.1,2.2 1 b"} {
		_, perr := parseProfile(bad)
		testutil.Is(t, perr, ErrProfile)
	}
	lc := measureLines(blocks)
	testutil.Equal(t, lc.total, 11)
	testutil.Equal(t, lc.covered, 5)
	testutil.Equal(t, lc.stmts, 8)
	testutil.Equal(t, lc.stmtCovered, 5)
	testutil.Equal(t, lc.uncovered(), []string{"example.com/app/lib/lib.go: 4 of 7 lines uncovered", "example.com/app/main.go: 2 of 4 lines uncovered"})
	testutil.Equal(t, ratio(0, 0), float64(percentScale))
	testutil.Equal(t, ratio(1, 4), float64(25))
	_, err = parseProfile(strings.Repeat("x", bufio.MaxScanTokenSize+1))
	testutil.Is(t, err, bufio.ErrTooLong)
	tied := measureLines([]block{{file: "z.go", start: 1, end: 1}, {file: "a.go", start: 1, end: 1}, {file: "m.go", start: 1, end: 2}})
	testutil.Equal(t, tied.uncovered(), []string{"m.go: 2 of 2 lines uncovered", "a.go: 1 of 1 lines uncovered", "z.go: 1 of 1 lines uncovered"})
}

func TestCoverageCheck(t *testing.T) {
	w := ws(t, nil)
	testutil.Contains(t, Coverage(context.Background(), w).Reason, "test report missing")
	testutil.Write(t, w.Report(CoverReport), profile)
	out := Coverage(context.Background(), w)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Values[ValueLineCoverage], float64(5)*100/11)
	testutil.Equal(t, out.Values[ValueStmtCoverage], 62.5)
	w.Policy.Thresholds.LineCoverage = float64(5) * 100 / 11
	testutil.Equal(t, Coverage(context.Background(), w).Status, gate.StatusPass)
	testutil.Write(t, w.Report(CoverReport), "mode: atomic\n")
	testutil.Equal(t, Coverage(context.Background(), w).Status, gate.StatusFail)
	testutil.Write(t, w.Report(CoverReport), "garbage")
	testutil.Contains(t, Coverage(context.Background(), w).Reason, "malformed")
	lines, err := CoveredLines(w)
	testutil.Fails(t, err)
	testutil.Equal(t, len(lines), 0)
	testutil.Write(t, w.Report(CoverReport), profile)
	lines, err = CoveredLines(w)
	testutil.NoErr(t, err)
	testutil.True(t, lines["example.com/app/lib/lib.go"][4] && !lines["example.com/app/lib/lib.go"][7], "line states")
	_, err = CoveredLines(ws(t, nil))
	testutil.Fails(t, err)
}

func TestBranches(t *testing.T) {
	w := ws(t, nil)
	pkgs := samplePackages()
	for i := range pkgs {
		pkgs[i].Dir = filepath.Join(w.Src, strings.TrimPrefix(strings.TrimPrefix(pkgs[i].ImportPath, module), "/"))
	}
	w.Exec = &procexectest.Fake{Handler: script{
		cmdListJSON:              ok(pkgJSON(t, pkgs...)),
		"gobco -branch ./lib":    ok("lib.go:4:5: condition \"x\" was once true but never false\nBranch coverage: 3/4\n"),
		"gobco -branch ./":       ok("Branch coverage: 0/0\n"),
		"gobco -branch ./orphan": ok("Branch coverage: 1/2\n"),
	}.Handle}
	out := Branches(context.Background(), w)
	testutil.Equal(t, out.Values[ValueBranchCover], float64(4)*100/6)
	testutil.Equal(t, out.Values[ValueBranches], float64(6))
	testutil.Equal(t, out.Findings, []string{`lib.go:4:5: condition "x" was once true but never false`})
	testutil.Equal(t, out.Status, gate.StatusFail)
	w.Policy.Thresholds.BranchCoverage = float64(4) * 100 / 6
	testutil.Equal(t, Branches(context.Background(), w).Status, gate.StatusPass)
	broken := ws(t, nil)
	broken.Exec = &procexectest.Fake{Handler: script{cmdListJSON: ok(pkgJSON(t, Package{ImportPath: module, Dir: broken.Src, GoFiles: []string{fileMain}})), "gobco": ok("FAIL")}.Handle}
	testutil.Contains(t, Branches(context.Background(), broken).Reason, "no branch coverage summary")
	testutil.Contains(t, Branches(context.Background(), ws(t, script{cmdList: {Err: errExit}})).Reason, cmdList)
	outside := ws(t, script{cmdListJSON: ok(pkgJSON(t, Package{ImportPath: module, Dir: "relative", GoFiles: []string{fileMain}}))})
	testutil.Contains(t, Branches(context.Background(), outside).Reason, "Rel: can't make relative")
}

func TestPassedTestsUseRegistryNames(t *testing.T) {
	w := ws(t, nil)
	_, err := PassedTests(w)
	testutil.Contains(t, err.Error(), "test report missing")
	testutil.Write(t, w.Report(TestsReport), events+`{"Action":"pass","Package":"example.com/app","Test":"TestRoot"}`+"\n")
	passed, err := PassedTests(w)
	testutil.NoErr(t, err)
	testutil.Equal(t, passed, map[string]bool{testA: true, "lib.TestA/sub": true, ".TestRoot": true})
}

func TestFuzz(t *testing.T) {
	w := ws(t, nil)
	dir := filepath.Join(w.Src, "lib")
	testutil.Write(t, filepath.Join(dir, fileLibTest), "package lib\n\nimport \"testing\"\n\nfunc FuzzZero(f *testing.F) {}\nfunc FuzzParse(f *testing.F) {}\nfunc TestX(t *testing.T) {}\ntype T struct{}\nfunc (T) FuzzMethod() {}\nvar FuzzVar = 1\n")
	pkg := Package{ImportPath: module + "/lib", Dir: dir, TestGoFiles: []string{fileLibTest}}
	w.Exec = &procexectest.Fake{Handler: script{
		cmdListJSON:                                 ok(pkgJSON(t, pkg)),
		cmdTest + " -run=^$ -fuzz=^FuzzParse$":      {Res: procexec.Result{Stdout: "Failing input written to testdata/fuzz/FuzzParse/abc"}, Err: errExit},
		cmdTest + " -run=^$ -fuzz=^FuzzZero$ -fuzz": ok("ok"),
	}.Handle}
	out := Fuzz(context.Background(), w)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Values[ValueFuzzTargets], float64(2))
	testutil.Contains(t, out.Findings[0], "FuzzParse: Failing input written to testdata/fuzz/FuzzParse/abc")
	calls := procexectest.LinesOf(w.Exec)
	testutil.Contains(t, calls[1], "-fuzz=^FuzzParse$ -fuzztime=1s "+module+"/lib")
	testutil.Contains(t, calls[2], "-fuzz=^FuzzZero$")
	none := ws(t, script{cmdListJSON: ok("")})
	testutil.Equal(t, Fuzz(context.Background(), none).Reason, reasonNoFuzz)
	testutil.Contains(t, Fuzz(context.Background(), ws(t, script{cmdList: {Err: errExit}})).Reason, cmdList)
	broken := ws(t, nil)
	testutil.Write(t, filepath.Join(broken.Src, "x_test.go"), "package x\nfunc {")
	broken.Exec = &procexectest.Fake{Handler: script{cmdListJSON: ok(pkgJSON(t, Package{ImportPath: module, Dir: broken.Src, XTestGoFiles: []string{"x_test.go"}}))}.Handle}
	testutil.Contains(t, Fuzz(context.Background(), broken).Reason, "x_test.go:2:6: expected 'IDENT'")
	green := ws(t, nil)
	testutil.Write(t, filepath.Join(green.Src, "g_test.go"), "package g\n\nimport \"testing\"\n\nfunc FuzzOK(f *testing.F) {}\n")
	green.Exec = &procexectest.Fake{Handler: script{cmdListJSON: ok(pkgJSON(t, Package{ImportPath: module, Dir: green.Src, TestGoFiles: []string{"g_test.go"}})), cmdTest: ok("ok")}.Handle}
	testutil.Equal(t, Fuzz(context.Background(), green).Status, gate.StatusPass)
}

func TestDependencies(t *testing.T) {
	out := Dependencies(context.Background(), ws(t, script{cmdList + " -deps -test": ok(module + "\ngo.yaml.in/yaml/v3\n\n" + strings.Repeat("evil.example/lib\n", 2))}))
	testutil.Equal(t, out.Findings, []string{"evil.example/lib is not in the approved dependency list"})
	testutil.Equal(t, out.Values[ValueModules], float64(2))
	clean := Dependencies(context.Background(), ws(t, script{cmdList + " -deps": ok(module + "\n")}))
	testutil.Equal(t, clean.Status, gate.StatusPass)
	testutil.Contains(t, Dependencies(context.Background(), ws(t, script{cmdList: {Err: errExit}})).Reason, cmdList+" -deps")
}

func TestVulnerabilities(t *testing.T) {
	stream := `{"config":{}}
{"osv":{"id":"GO-1"}}
{"finding":{"osv":"GO-2024-1","fixed_version":"v1.2.3","trace":[{"module":"golang.org/x/net"}]}}
{"finding":{"osv":"GO-2024-1","fixed_version":"v1.2.3","trace":[{"module":"golang.org/x/net"}]}}
{"finding":{"osv":"GO-2024-2","fixed_version":"","trace":[]}}
`
	out := Vulnerabilities(context.Background(), ws(t, script{VulncheckBinary: ok(stream)}))
	testutil.Equal(t, out.Findings, []string{"GO-2024-1 in golang.org/x/net (fixed in v1.2.3)", "GO-2024-2 in  (fixed in )"})
	testutil.Equal(t, out.Values[ValueVulns], float64(2))
	testutil.Equal(t, Vulnerabilities(context.Background(), ws(t, script{VulncheckBinary: ok(`{"config":{}}`)})).Status, gate.StatusPass)
	testutil.Contains(t, Vulnerabilities(context.Background(), ws(t, script{VulncheckBinary: {Err: errExit}})).Reason, VulncheckBinary)
	testutil.Equal(t, Vulnerabilities(context.Background(), ws(t, script{VulncheckBinary: ok("{")})).Status, gate.StatusFail)
}

func TestLoadTestRunMissing(t *testing.T) {
	_, err := LoadTestRun(ws(t, nil))
	testutil.True(t, err != nil && errors.Is(err, os.ErrNotExist), "missing report")
}

func FuzzParseTestEvents(f *testing.F) {
	f.Add(events)
	f.Fuzz(func(t *testing.T, stream string) {
		run, err := ParseTestEvents(stream)
		if err == nil && run.Tests < run.Failed+run.Skipped {
			t.Fatalf("counts inconsistent: %+v", run)
		}
	})
}

func FuzzParseProfile(f *testing.F) {
	f.Add(profile)
	f.Fuzz(func(t *testing.T, text string) {
		blocks, err := parseProfile(text)
		if err != nil {
			return
		}
		lc := measureLines(blocks)
		if lc.covered > lc.total || lc.stmtCovered > lc.stmts {
			t.Fatalf("coverage over 100%%: %+v", lc)
		}
	})
}
