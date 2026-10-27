package mutation

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/policy"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	pkgA      = "m/a"
	pkgB      = "m/b"
	pkgC      = "m/c"
	pkgE2E    = "m/e2e"
	docsFile  = "docs/guide.md"
	pkgStd    = "fmt"
	pkgExt    = "x.y/z"
	fileA     = "a/a.go"
	fileATest = "a/a_test.go"
	envFoo    = "FOO"
	goEnvOld  = `{"GOVERSION":"go1.27.3"}`
	goEnvNew  = `{"GOVERSION":"go1.27.4"}`
	emptyLog  = "# test log\n"
	libTest   = "lib_test.go"
	goList    = "list"
	srcPkgA   = "package a\n"
	baseOne   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baseTwo   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	keyOne    = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	keyTwo    = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

func closureFixture(t *testing.T) (string, golang.Closure) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{fileA: srcPkgA, fileATest: srcPkgA, "a/testdata/in.txt": "in", "a/embed/e.txt": "e",
		"b/b.go": "package b\n", "c/c.go": "package c\n", "e2e/e2e_test.go": "package e2e\n", "docs/guide.md": "guide"}
	for rel, body := range files {
		testutil.Write(t, filepath.Join(root, rel), body)
	}
	built := golang.Closure{
		Packages: map[string]golang.Built{
			pkgA:   {ImportPath: pkgA, Dir: filepath.Join(root, "a"), GoFiles: []string{"a.go"}, TestGoFiles: []string{"a_test.go"}, EmbedFiles: []string{"embed/e.txt"}},
			pkgB:   {ImportPath: pkgB, Dir: filepath.Join(root, "b"), GoFiles: []string{"b.go"}},
			pkgC:   {ImportPath: pkgC, Dir: filepath.Join(root, "c"), GoFiles: []string{"c.go"}},
			pkgE2E: {ImportPath: pkgE2E, Dir: filepath.Join(root, "e2e"), TestGoFiles: []string{"e2e_test.go"}},
			pkgStd: {ImportPath: pkgStd, Standard: true},
			pkgExt: {ImportPath: pkgExt, Dir: "/mod/x.y/z@v1.0.0", Module: &golang.BuiltModule{Path: pkgExt, Version: extVersion}},
		},
		Tests: map[string][]string{pkgA: {pkgStd, pkgA, pkgB, pkgExt}, pkgE2E: {pkgA, pkgB, pkgE2E}},
	}
	return root, built
}

type keyInputs struct {
	root  string
	built golang.Closure
	base  KeyBase
	reads string
}

func (in keyInputs) key(t *testing.T) string {
	t.Helper()
	d := newDigests(in.root, in.base.Digest(), in.built, map[string]string{pkgA: in.reads})
	m := NewMutant(filepath.Join(in.root, fileA), 1, OpArithmetic, "a + b", 0, Edit{Offset: 2, Length: 1, Text: "-"})
	key, err := d.key(target{mutant: m, pkg: golang.Package{ImportPath: pkgB}, dependents: []string{pkgA}})
	testutil.NoErr(t, err)
	return key
}

func baseFixture() KeyBase {
	return KeyBase{Engine: "e1", Policy: "p1", Toolchain: "go1.27.3", Module: "sum1", Flags: "-count=1"}
}

type closureChange struct {
	name    string
	prepare func(*keyInputs)
	change  func(*keyInputs)
	changed bool
}

func editFile(t *testing.T, rel string) func(*keyInputs) {
	return func(in *keyInputs) { testutil.Write(t, filepath.Join(in.root, rel), "changed") }
}

func fixtureInputs(t *testing.T) keyInputs {
	root, built := closureFixture(t)
	return keyInputs{root: root, built: built, base: baseFixture(), reads: "r1"}
}

// sharedChanges invalidate both the full key and the key of a killed verdict.
func sharedChanges(t *testing.T) []closureChange {
	return []closureChange{
		{"package outside the closure", nil, editFile(t, "c/c.go"), false},
		{"file or variable the tests read", nil, func(in *keyInputs) { in.reads = "other" }, true},
		{"external module version", nil, func(in *keyInputs) {
			b := in.built.Packages[pkgExt]
			b.Module = &golang.BuiltModule{Path: pkgExt, Version: "v1.0.1"}
			in.built.Packages[pkgExt] = b
		}, true},
		{"go.mod or go.sum", nil, func(in *keyInputs) { in.base.Module = "sum2" }, true},
		{"go version or build environment", nil, func(in *keyInputs) { in.base.Toolchain = "go1.27.4" }, true},
		{"build and test flags", nil, func(in *keyInputs) { in.base.Flags = "-count=1 -race" }, true},
		{"mutation engine", nil, func(in *keyInputs) { in.base.Engine = "e2" }, true},
		{"gate policy", nil, func(in *keyInputs) { in.base.Policy = "p2" }, true},
		{"host the verdicts were measured on", nil, func(in *keyInputs) { in.base.Host = "cpus=12" }, true},
		{"package testdata", nil, editFile(t, "a/testdata/in.txt"), true},
		{"package test", nil, editFile(t, fileATest), true},
	}
}

func checkChanges(t *testing.T, cases []closureChange, key func(keyInputs, *testing.T) string) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := fixtureInputs(t)
			if c.prepare != nil {
				c.prepare(&in)
			}
			before := key(in, t)
			testutil.True(t, before != "", "the inputs are keyed")
			testutil.Equal(t, key(in, t), before)
			c.change(&in)
			testutil.Equal(t, key(in, t) != before, c.changed)
		})
	}
}

func TestKeyChangesWithEveryPartOfTheBuildClosure(t *testing.T) {
	checkChanges(t, append(sharedChanges(t),
		closureChange{"package source", nil, editFile(t, fileA), true},
		closureChange{"embedded file", nil, editFile(t, "a/embed/e.txt"), true},
		closureChange{"new file next to the package", nil, editFile(t, "a/notes.txt"), true},
		closureChange{"dependency source", nil, editFile(t, "b/b.go"), true},
		closureChange{"test package that links it only through test imports", nil, editFile(t, "e2e/e2e_test.go"), true},
		closureChange{"module data without setup code", nil, editFile(t, docsFile), false},
		closureChange{"module data with a TestMain", func(in *keyInputs) {
			testutil.Write(t, filepath.Join(in.root, fileATest), "package a\n\nfunc TestMain(m *testing.M) {}\n")
		}, editFile(t, docsFile), true},
	), keyInputs.key)
}

func TestKeyRefusesAPackageMissingFromTheClosure(t *testing.T) {
	root, built := closureFixture(t)
	built.Tests[pkgA] = append(built.Tests[pkgA], "m/gone")
	d := newDigests(root, "b", built, map[string]string{})
	_, err := d.key(target{mutant: Mutant{File: filepath.Join(root, fileA)}, pkg: golang.Package{ImportPath: pkgA}})
	testutil.True(t, errors.Is(err, ErrClosure), "missing package is an error")
	delete(built.Tests, pkgA)
	built.Packages[pkgB] = golang.Built{ImportPath: pkgB, Dir: filepath.Join(root, "gone")}
	built.Tests[pkgB] = []string{pkgB}
	d = newDigests(root, "b", built, map[string]string{})
	_, err = d.key(target{mutant: Mutant{File: filepath.Join(root, fileA)}, pkg: golang.Package{ImportPath: pkgB}})
	testutil.True(t, errors.Is(err, ErrClosure), "unreadable package directory is an error")
}

func TestVolatilePackageIsNeverCached(t *testing.T) {
	root, built := closureFixture(t)
	d := newDigests(root, "b", built, map[string]string{})
	d.volatile[pkgA] = true
	key, err := d.key(target{mutant: Mutant{File: filepath.Join(root, fileA)}, pkg: golang.Package{ImportPath: pkgA}})
	testutil.NoErr(t, err)
	testutil.Equal(t, key, "")
}

func TestReadsDigestFollowsWhatTheTestsRead(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	testutil.Write(t, filepath.Join(root, "data/x.txt"), "x")
	testutil.Write(t, filepath.Join(root, "data/y.txt"), "y")
	testutil.Write(t, outside, "o")
	log := emptyLog + "open ../data/x.txt\ngetenv FOO\nstat " + outside + "\nstat ../missing\nchdir ../data\nopen y.txt\nopen .\nbogus\n"
	env := map[string]string{envFoo: "1"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	digest := func() string {
		return newDigests(root, "", golang.Closure{}, nil).readsDigest(filepath.Join(root, "p"), log, lookup)
	}
	before := digest()
	steps := []struct {
		name    string
		change  func()
		changed bool
	}{
		{"file outside the module", func() { testutil.Write(t, outside, "o2") }, false},
		{"opened file", func() { testutil.Write(t, filepath.Join(root, "data/x.txt"), "x2") }, true},
		{"file opened after chdir", func() { testutil.Write(t, filepath.Join(root, "data/y.txt"), "y2") }, true},
		{"file that did not exist", func() { testutil.Write(t, filepath.Join(root, "missing"), "m") }, true},
		{"listed directory", func() { testutil.Write(t, filepath.Join(root, "data/z.txt"), "z") }, true},
		{"environment value", func() { env[envFoo] = "2" }, true},
		{"unset environment variable", func() { delete(env, envFoo) }, true},
	}
	for _, s := range steps {
		s.change()
		after := digest()
		testutil.Equal(t, after != before, s.changed)
		before = after
	}
}

func TestVerdictCacheIsVerifiedBySHA256(t *testing.T) {
	path := filepath.Join(t.TempDir(), VerdictsFile)
	v := Verdicts{Version: verdictsVersion, Bases: []string{baseOne}, Entries: map[string]Status{keyOne: StatusKilled}}
	digest, err := v.Save(path)
	testutil.NoErr(t, err)
	got, err := LoadVerdicts(path, digest)
	testutil.NoErr(t, err)
	s, ok := got.lookup(keyOne)
	testutil.True(t, ok && s == StatusKilled, "a stored verdict is found")
	_, ok = got.lookup(keyTwo)
	testutil.True(t, !ok, "an unknown key is a miss")
	data := testutil.Read(t, path)
	testutil.Write(t, path, strings.Replace(data, string(StatusKilled), string(StatusSurvived), 1))
	_, err = LoadVerdicts(path, digest)
	testutil.True(t, errors.Is(err, ErrVerdicts), "a tampered cache is rejected")
	_, err = LoadVerdicts(path+".missing", digest)
	testutil.True(t, errors.Is(err, ErrVerdicts), "a missing cache is rejected")
	for _, body := range []string{"{", `{"version":9}`, `{"version":1,"entries":{"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee":"errored"}}`, `{"version":1,"entries":{"k":"killed"}}`, `{"version":1,"bases":["b"]}`} {
		testutil.Write(t, path, body)
		_, err = LoadVerdicts(path, Digest([]byte(body)))
		testutil.True(t, errors.Is(err, ErrVerdicts), "an unreadable or foreign cache is rejected")
	}
}

func TestKeyBaseFollowsModuleToolchainAndPolicy(t *testing.T) {
	src := t.TempDir()
	testutil.Write(t, filepath.Join(src, "go.sum"), "x v1 h1:a=\n")
	env := goEnvOld
	ws := gate.Workspace{Src: src, Log: slog.New(slog.DiscardHandler), Exec: &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) { return procexec.Result{Stdout: env}, nil }}}
	e := &engine{ws: ws, cache: cacheEnv + t.TempDir()}
	base := func() KeyBase {
		k, err := e.keyBase(context.Background())
		testutil.NoErr(t, err)
		testutil.Equal(t, k.Engine, EngineVersion)
		return k
	}
	first := base()
	testutil.Write(t, filepath.Join(src, "go.sum"), "x v2 h1:b=\n")
	second := base()
	testutil.True(t, second.Module != first.Module && second.Toolchain == first.Toolchain, "go.sum changes the module part")
	env = goEnvNew
	third := base()
	testutil.True(t, third.Toolchain != second.Toolchain, "the go version changes the toolchain part")
	e.ws.Policy.Limits.MutantFactor = 5
	fourth := base()
	testutil.True(t, fourth.Policy != third.Policy && fourth.Digest() != third.Digest(), "the policy changes the policy part")
	e.ws.Exec = &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errors.New("exit 1") }}
	_, err := e.keyBase(context.Background())
	testutil.True(t, err != nil, "a broken go env is an error")
}

func cachedSetup(t *testing.T) (gate.Workspace, *fakeGo) {
	t.Helper()
	versioned(t)
	ws, fg := setup(t, func(mutated string, _ []string) outcome {
		return outcome{killed: strings.Contains(mutated, "!(a > 0)")}
	})
	testutil.Write(t, filepath.Join(ws.Src, "lib", libTest), "package lib\n")
	testutil.Write(t, filepath.Join(ws.Src, "go.mod"), "module m\n")
	fg.tracked = "go.mod\x00lib/" + fileLib + "\x00lib/" + libTest + "\x00"
	fg.env, fg.testLogs = goEnvOld, emptyLog
	built := []golang.Built{
		{ImportPath: pkgLib, Dir: srcMarker + "/lib", GoFiles: []string{fileLib}, TestGoFiles: []string{libTest}},
		{ImportPath: pkgLib + ".test", Dir: srcMarker + "/lib", Deps: []string{pkgStd, pkgLib + " [" + pkgLib + ".test]"}},
		{ImportPath: pkgStd, Standard: true},
	}
	var listing strings.Builder
	for _, b := range built {
		data, err := json.Marshal(b)
		testutil.NoErr(t, err)
		listing.Write(data)
	}
	fg.closure = listing.String()
	ws.Policy.Thresholds.MutationScore = 10
	return ws, fg
}

func keepVerdicts(t *testing.T, ws gate.Workspace) gate.MutationPlan {
	t.Helper()
	data := testutil.Read(t, ws.Report(VerdictsFile))
	path := filepath.Join(t.TempDir(), VerdictsFile)
	testutil.Write(t, path, data)
	return gate.MutationPlan{Cache: path, CacheSHA256: Digest([]byte(data))}
}

func runOnce(t *testing.T, ws gate.Workspace, fg *fakeGo) (Report, int) {
	t.Helper()
	before := len(fg.overlays)
	rep, err := Run(context.Background(), ws)
	testutil.NoErr(t, err)
	return rep, len(fg.overlays) - before
}

func TestCachedVerdictsAreReusedUntilTheClosureChanges(t *testing.T) {
	ws, fg := cachedSetup(t)
	full, ran := runOnce(t, ws, fg)
	testutil.True(t, ran > 0 && full.Cached == 0, "the first run has no cache")
	ws.Mutation = keepVerdicts(t, ws)
	again, rerun := runOnce(t, ws, fg)
	testutil.Equal(t, again.Cached, full.Counts[StatusKilled])
	testutil.Equal(t, rerun, ran-again.Cached)
	testutil.Equal(t, again.Counts, full.Counts)
	for _, e := range again.Entries {
		testutil.Equal(t, e.Source == SourceCache, e.Status == StatusKilled)
	}
	out := Judge(ws, again, nil)
	testutil.Equal(t, out.Values[ValueCached], float64(again.Cached))
	testutil.Equal(t, out.Values[ValueRun], float64(totalMutants(again.Counts)-again.Cached))
	testutil.Equal(t, out.Values[ValueScore], full.Score())
	ws.Mutation = keepVerdicts(t, ws)
	kept, _ := runOnce(t, ws, fg)
	testutil.Equal(t, kept.Cached, again.Cached)
	testutil.Write(t, filepath.Join(ws.Src, "lib", libTest), "package lib\n\nvar changed = 1\n")
	edited, ran := runOnce(t, ws, fg)
	testutil.True(t, ran > 0 && edited.Cached == 0, "a test edit reruns every mutant of the package")
}

func TestSurvivorsAlwaysRunAgainOutsideAFullRun(t *testing.T) {
	ws, fg := cachedSetup(t)
	ws.Mutation.Full = true
	first, ran := runOnce(t, ws, fg)
	testutil.True(t, ran > 0 && first.Cached == 0, "the first run has no cache")
	v, err := LoadVerdicts(ws.Report(VerdictsFile), Digest([]byte(testutil.Read(t, ws.Report(VerdictsFile)))))
	testutil.NoErr(t, err)
	testutil.Equal(t, len(v.Entries), totalMutants(first.Counts)+first.Counts[StatusKilled])
	ws.Mutation = keepVerdicts(t, ws)
	ws.Mutation.Full = true
	resumed, ran := runOnce(t, ws, fg)
	testutil.Equal(t, ran, 0)
	testutil.Equal(t, resumed.Cached, totalMutants(first.Counts))
	ws.Mutation.Full = false
	gated, ran := runOnce(t, ws, fg)
	testutil.Equal(t, gated.Cached, first.Counts[StatusKilled])
	testutil.True(t, ran > 0, "survivors run again in a gate")
	testutil.Equal(t, gated.Counts, first.Counts)
}

func TestKillKeyFollowsOnlyTheKillingTestBinary(t *testing.T) {
	checkChanges(t, append(sharedChanges(t),
		closureChange{"mutated package source", nil, editFile(t, "b/b.go"), true},
		closureChange{"killing package source", nil, editFile(t, fileA), true},
		closureChange{"another test binary that links the mutated package", nil, editFile(t, "e2e/e2e_test.go"), false},
	), func(in keyInputs, t *testing.T) string { return in.killKey(t, pkgA) })
}

func (in keyInputs) killKey(t *testing.T, killer string) string {
	t.Helper()
	d := newDigests(in.root, in.base.Digest(), in.built, map[string]string{pkgA: in.reads, pkgE2E: "r2"})
	m := NewMutant(filepath.Join(in.root, "b/b.go"), 1, OpArithmetic, "a + b", 0, Edit{Offset: 2, Length: 1, Text: "-"})
	return d.killKey(target{mutant: m, pkg: golang.Package{ImportPath: pkgB}}, killer)
}

func TestKillKeyNeedsAStableKillerThatLinksTheMutant(t *testing.T) {
	in := fixtureInputs(t)
	root, built := in.root, in.built
	testutil.Equal(t, in.killKey(t, pkgC), "")
	testutil.True(t, in.killKey(t, pkgE2E) != in.killKey(t, pkgA), "each killer has its own key")
	killed := target{mutant: Mutant{File: filepath.Join(root, fileA)}, pkg: golang.Package{ImportPath: pkgB}}
	d := newDigests(root, "b", built, map[string]string{})
	d.volatile[pkgA] = true
	testutil.Equal(t, d.killKey(killed, pkgA), "")
	built.Tests[pkgA] = append(built.Tests[pkgA], "m/gone")
	testutil.Equal(t, newDigests(root, "b", built, map[string]string{}).killKey(killed, pkgA), "")
}

func TestFailedPackagesComeFromTheTestSummary(t *testing.T) {
	out := failedTest + "\nFAIL\tm/b\t0.1s\nok  \tm/a\t0.2s\nFAIL\tm/c\t0.1s\nFAIL\tother/pkg\t0.1s\nFAIL\tm/b\t0.1s\n"
	testutil.Equal(t, failedPackages(out, []string{pkgA, pkgB, pkgC}), []string{pkgB, pkgC})
	testutil.Equal(t, len(failedPackages("FAIL\n", []string{pkgA})), 0)
}

func TestCacheIsIgnoredWhenTamperedOrStale(t *testing.T) {
	ws, fg := cachedSetup(t)
	runOnce(t, ws, fg)
	ws.Mutation = keepVerdicts(t, ws)
	ws.Mutation.CacheSHA256 = Digest([]byte("other"))
	tampered, ran := runOnce(t, ws, fg)
	testutil.True(t, ran > 0 && tampered.Cached == 0, "a cache with a wrong sha256 is not used")
	ws.Mutation = keepVerdicts(t, ws)
	fg.env = goEnvNew
	stale, ran := runOnce(t, ws, fg)
	testutil.True(t, ran > 0 && stale.Cached == 0, "a new go version reruns every mutant")
	ws.Policy.Limits = policy.Limits{MutantFloor: ws.Policy.Limits.MutantFloor * 2, MutantFactor: 2}
	ws.Mutation = keepVerdicts(t, ws)
	fg.testLogs = ""
	volatile, ran := runOnce(t, ws, fg)
	testutil.True(t, ran > 0 && volatile.Cached == 0, "a package without a test log is never cached")
	v, err := LoadVerdicts(ws.Report(VerdictsFile), Digest([]byte(testutil.Read(t, ws.Report(VerdictsFile)))))
	testutil.NoErr(t, err)
	testutil.Equal(t, len(v.Entries), 0)
}

func TestMergeVerdictsJoinsProjects(t *testing.T) {
	dir := t.TempDir()
	parts := []Verdicts{
		{Version: verdictsVersion, Bases: []string{baseOne}, Entries: map[string]Status{keyOne: StatusKilled}},
		{Version: verdictsVersion, Bases: []string{baseTwo, baseOne}, Entries: map[string]Status{keyTwo: StatusSurvived, keyOne: StatusTimedOut}},
	}
	paths := make([]string, 0, len(parts))
	for i, v := range parts {
		p := filepath.Join(dir, string(rune('a'+i))+".json")
		_, err := v.Save(p)
		testutil.NoErr(t, err)
		paths = append(paths, p)
	}
	out := filepath.Join(dir, VerdictsFile)
	digest, err := MergeVerdicts(out, paths)
	testutil.NoErr(t, err)
	all, err := LoadVerdicts(out, digest)
	testutil.NoErr(t, err)
	testutil.Equal(t, all.Bases, []string{baseOne, baseTwo})
	testutil.Equal(t, all.Entries, map[string]Status{keyOne: StatusTimedOut, keyTwo: StatusSurvived})
	_, err = MergeVerdicts(out, []string{filepath.Join(dir, "missing.json")})
	testutil.True(t, err != nil, "a missing project file is an error")
	testutil.Write(t, paths[0], "{")
	_, err = MergeVerdicts(out, paths)
	testutil.True(t, errors.Is(err, ErrVerdicts), "a broken project file is an error")
}

func TestCancelledJobsAreNeverFinalVerdicts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	statuses := []func() Status{
		func() Status { return StatusKilled },
		func() Status { cancel(); return StatusTimedOut },
		func() Status { return StatusSurvived },
	}
	jobs := make([]Job, 0, len(statuses)+1)
	jobs = append(jobs, Job{Mutant: Mutant{File: fileLib}, Cached: StatusSurvived})
	for _, s := range statuses {
		jobs = append(jobs, Job{Mutant: Mutant{File: fileLib}, Run: func(context.Context, int) Status { return s() }})
	}
	rep, err := Execute(ctx, gate.Workspace{Src: "/", Log: slog.New(slog.DiscardHandler)}, jobs, 1)
	testutil.True(t, errors.Is(err, ErrIncomplete), "a cancelled run is incomplete")
	got := make([]Status, 0, len(rep.Entries))
	for _, e := range rep.Entries {
		got = append(got, e.Status)
	}
	testutil.Equal(t, got, []Status{StatusSurvived, StatusKilled, StatusErrored, StatusErrored})
}

func TestIncompleteRunStillKeepsItsFinalVerdicts(t *testing.T) {
	ws, fg := cachedSetup(t)
	ws.Mutation.Full = true
	fg.decide = func(mutated string, _ []string) outcome {
		return outcome{broken: strings.Contains(mutated, "!(a > 0)")}
	}
	rep, err := Run(context.Background(), ws)
	testutil.True(t, errors.Is(err, ErrIncomplete), "an errored mutant makes the run incomplete")
	testutil.Equal(t, rep.Counts[StatusErrored], 1)
	data := testutil.Read(t, ws.Report(VerdictsFile))
	v, err := LoadVerdicts(ws.Report(VerdictsFile), Digest([]byte(data)))
	testutil.NoErr(t, err)
	testutil.Equal(t, len(v.Entries), len(rep.Entries)-1)
}

func versioned(t *testing.T) {
	t.Helper()
	old := EngineVersion
	EngineVersion = "test-engine"
	t.Cleanup(func() { EngineVersion = old })
}

func TestUnversionedEngineNeverCachesOrReuses(t *testing.T) {
	ws, fg := cachedSetup(t)
	runOnce(t, ws, fg)
	ws.Mutation = keepVerdicts(t, ws)
	EngineVersion = ""
	rep, ran := runOnce(t, ws, fg)
	testutil.True(t, ran > 0 && rep.Cached == 0, "an engine without a version reuses nothing")
	v, err := LoadVerdicts(ws.Report(VerdictsFile), Digest([]byte(testutil.Read(t, ws.Report(VerdictsFile)))))
	testutil.NoErr(t, err)
	testutil.Equal(t, len(v.Entries), 0)
}
