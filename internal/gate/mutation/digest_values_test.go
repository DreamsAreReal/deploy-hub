package mutation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/gate/golang"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	extVersion = "v1.0.0"
	readsValue = "reads"
)

func sumOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestHashOfSeparatesItsParts(t *testing.T) {
	testutil.Equal(t, hashOf("ab", "c"), sumOf("ab", "c"))
	testutil.True(t, hashOf("ab", "c") != hashOf("a", "bc"), "parts are separated")
}

func digestOf(t *testing.T, d *digests, pkg string) string {
	t.Helper()
	v, err := d.pkg(pkg)
	testutil.NoErr(t, err)
	again, err := d.pkg(pkg)
	testutil.NoErr(t, err)
	testutil.Equal(t, again, v)
	return v
}

func TestPackageDigestsAreExact(t *testing.T) {
	root, built := closureFixture(t)
	d := newDigests(root, "base", built, map[string]string{pkgA: readsValue})
	testutil.Equal(t, digestOf(t, d, pkgStd), sumOf(digestStd, pkgStd))
	testutil.Equal(t, digestOf(t, d, pkgExt), sumOf(digestModule, pkgExt, pkgExt+"@"+extVersion))
	testutil.Equal(t, digestOf(t, d, pkgB), sumOf(digestFiles, pkgB, "b/b.go", sumOf("package b\n")))
	testutil.Equal(t, digestOf(t, d, pkgA), sumOf(digestFiles, pkgA, fileA, sumOf(srcPkgA), fileATest, sumOf(srcPkgA),
		"a/embed/e.txt", sumOf("e"), "a/testdata/in.txt", sumOf("in")))
}

func TestVendoredModuleIsHashedByItsFiles(t *testing.T) {
	root, built := closureFixture(t)
	testutil.Write(t, filepath.Join(root, "vendor/x.y/z/z.go"), "package z\n")
	built.Packages[pkgExt] = golang.Built{ImportPath: pkgExt, Dir: filepath.Join(root, "vendor/x.y/z"), GoFiles: []string{"z.go"},
		Module: &golang.BuiltModule{Path: pkgExt, Version: extVersion}}
	d := newDigests(root, "base", built, nil)
	testutil.Equal(t, digestOf(t, d, pkgExt), sumOf(digestFiles, pkgExt, "vendor/x.y/z/z.go", sumOf("package z\n")))
}

func TestTestBinaryAndKeyAreExact(t *testing.T) {
	root, built := closureFixture(t)
	d := newDigests(root, "base", built, map[string]string{pkgA: readsValue})
	std, ext := digestOf(t, d, pkgStd), digestOf(t, d, pkgExt)
	a, b, e2e := digestOf(t, d, pkgA), digestOf(t, d, pkgB), digestOf(t, d, pkgE2E)
	binA := sumOf(readsValue, pkgStd, std, pkgA, a, pkgB, b, pkgExt, ext)
	binE2E := sumOf("", pkgA, a, pkgB, b, pkgE2E, e2e)
	for range 2 {
		got, err := d.testBinary(pkgA)
		testutil.NoErr(t, err)
		testutil.Equal(t, got, binA)
	}
	testutil.Equal(t, d.linkers(pkgB), []string{pkgA, pkgE2E})
	m := NewMutant(filepath.Join(root, "b/b.go"), 1, OpArithmetic, "a + b", 0, Edit{Offset: 2, Length: 1, Text: "-"})
	identity := sumOf("b/b.go", "1", OpArithmetic, "a + b", m.Mutated, `[{"Offset":2,"Length":1,"Text":"-"}]`)
	testutil.Equal(t, mutantIdentity(root, m), identity)
	key, err := d.key(target{mutant: m, pkg: golang.Package{ImportPath: pkgB}})
	testutil.NoErr(t, err)
	testutil.Equal(t, key, sumOf("base", identity, pkgA, binA, pkgE2E, binE2E))
}

func TestFileDigestsAreExact(t *testing.T) {
	root := t.TempDir()
	testutil.Write(t, filepath.Join(root, "sub/z.md"), "z")
	testutil.Write(t, filepath.Join(root, "sub/y.go"), "package y\n")
	testutil.Write(t, filepath.Join(root, "x.txt"), "x")
	outside := filepath.Join(t.TempDir(), "o.txt")
	testutil.Write(t, outside, "o")
	d := newDigests(root, "", golang.Closure{}, nil)
	testutil.Equal(t, d.content(filepath.Join(root, "x.txt")), sumOf("x"))
	testutil.Equal(t, d.content(filepath.Join(root, "x.txt")), sumOf("x"))
	testutil.Equal(t, d.content(filepath.Join(root, "sub")), sumOf(digestDir, "y.go", "z.md"))
	testutil.Equal(t, d.content(filepath.Join(root, "missing")), digestAbsent)
	testutil.Equal(t, d.content(outside), digestAbsent)
	testutil.Equal(t, d.rel(filepath.Join(root, "sub/z.md")), "sub/z.md")
	want := sumOf(digestData, "sub/z.md", sumOf("z"), "x.txt", sumOf("x"))
	testutil.Equal(t, d.moduleData(), want)
	testutil.Write(t, filepath.Join(root, "x.txt"), "changed")
	testutil.Equal(t, d.moduleData(), want)
	empty := newDigests(t.TempDir(), "", golang.Closure{}, nil)
	testutil.Equal(t, empty.content(filepath.Join(empty.root, ".")), sumOf(digestDir))
	gone := newDigests(filepath.Join(t.TempDir(), "gone"), "", golang.Closure{}, nil)
	testutil.Equal(t, gone.content(filepath.Join(gone.root, "x")), digestAbsent)
}

func TestReadsDigestIsExact(t *testing.T) {
	root := t.TempDir()
	testutil.Write(t, filepath.Join(root, "x.txt"), "x")
	d := newDigests(root, "", golang.Closure{}, nil)
	lookup := func(k string) (string, bool) { return "v", k == "K" }
	got := d.readsDigest(root, emptyLog+"open x.txt\ngetenv K\nopen x.txt\n", lookup)
	testutil.Equal(t, got, sumOf("getenv\x00K\x00true\x00v", "x.txt\x00"+sumOf("x")))
}

func TestEngineHelpersAreExact(t *testing.T) {
	out := t.TempDir()
	e := &engine{ws: gate.Workspace{Out: out}, cache: cacheEnv + "/c"}
	testutil.Equal(t, e.testLog(pkgA), filepath.Join(out, testLogDir, sumOf(pkgA)+testLogExt))
	v, ok := e.lookupEnv("GOCACHE")
	testutil.True(t, ok && v == "/c", "the engine's own build cache is what the tests see")
	t.Setenv("DH653_PROBE", "p")
	v, ok = e.lookupEnv("DH653_PROBE")
	testutil.True(t, ok && v == "p", "other variables come from the environment")
	_, ok = e.lookupEnv("DH653_UNSET_PROBE")
	testutil.True(t, !ok, "an unset variable stays unset")
	testutil.Equal(t, hostProfile(), fmt.Sprintf("cpus=%d workers=%d memory=%d", procexec.CPUs(), Workers(), MemoryBudget()))
}
