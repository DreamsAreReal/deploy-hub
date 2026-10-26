package tools

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	jsoncURL    = "https://github.com/json-c/json-c/archive/refs/tags/json-c-0.18-20240915.tar.gz"
	uboxURL     = "https://github.com/openwrt/libubox/archive/7dd127841e82eb1cfb61185da37dde7b9bd9ba6d.tar.gz"
	jsonpathURL = "https://github.com/openwrt/jsonpath/archive/b9034210bd331749673416c6bf389cccd4e23610.tar.gz"
	cmakeURL    = "https://github.com/Kitware/CMake/releases/download/v4.4.4/cmake-4.4.4-linux-x86_64.tar.gz"
	srcTree     = "src"
	cmakeMember = "cmake/bin/cmake"
	badSum      = "abc"
	upTree      = "../up"
)

func jsonfilterManifest(sum string) Manifest {
	return Manifest{
		Binaries: []Binary{{Name: CmakeBinary, URL: cmakeURL, SHA256: sum, Member: cmakeMember}},
		Sources: []Source{
			{Name: sourceJSONC, URL: jsoncURL, SHA256: sum, Tree: srcTree},
			{Name: sourceLibubox, URL: uboxURL, SHA256: sum, Tree: srcTree},
			{Name: sourceJSONPath, URL: jsonpathURL, SHA256: sum, Tree: srcTree},
		},
	}
}

type builder struct {
	t      *testing.T
	failOn string
	trees  map[string]string
}

func (b *builder) run(c procexec.Command) (procexec.Result, error) {
	line := procexectest.Line(c)
	if b.failOn != "" && strings.Contains(line, b.failOn) {
		return procexec.Result{Stderr: failMsg}, errors.New("exit 1")
	}
	if c.Name == TarBinary {
		root := c.Args[3]
		name := filepath.Base(filepath.Dir(root))
		testutil.Write(b.t, filepath.Join(root, srcTree, "parser.y"), "grammar")
		testutil.Write(b.t, filepath.Join(root, srcTree, "list.h"), "header")
		testutil.Write(b.t, filepath.Join(root, srcTree, "uloop.c"), "source")
		testutil.NoErr(b.t, os.Mkdir(filepath.Join(root, srcTree, "nested.h"), dirPerm))
		b.trees[name] = filepath.Join(root, srcTree)
	}
	return procexec.Result{}, nil
}

func newBuilder(t *testing.T, failOn string) *builder {
	t.Helper()
	return &builder{t: t, failOn: failOn, trees: map[string]string{}}
}

func assets(t *testing.T, data []byte) transport {
	t.Helper()
	tr := transport{}
	for _, u := range []string{jsoncURL, uboxURL, jsonpathURL} {
		tr[u] = respond(http.StatusOK, data)
	}
	tr[cmakeURL] = respond(http.StatusOK, archive(t, map[string]string{cmakeMember: payload}))
	return tr
}

func TestJSONFilterIsBuiltFromThePinnedSources(t *testing.T) {
	data := []byte(payload)
	b := newBuilder(t, "")
	fake := &procexectest.Fake{Handler: b.run}
	tr := assets(t, data)
	m := jsonfilterManifest(digest(data))
	m.Binaries[0].SHA256 = digest(tr[cmakeURL].body)
	inst := installer(t, tr, fake)
	testutil.NoErr(t, inst.Install(context.Background(), m))
	var lines []string
	for _, c := range fake.Calls() {
		if c.Name != TarBinary {
			lines = append(lines, procexectest.Line(c))
		}
	}
	jsonc, ubox, jp := b.trees[sourceJSONC], b.trees[sourceLibubox], b.trees[sourceJSONPath]
	build := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(jsonc))), jsoncBuildDir)
	prefix := filepath.Join(filepath.Dir(build), prefixDir)
	lemon := filepath.Join(jp, "contrib", "lemon")
	compile := make([]string, 0, 2+len(jsonpathFiles)+len(libuboxFiles))
	compile = append(compile, "cc -Os -std=gnu99 -D_GNU_SOURCE -DJSONC -I"+prefix+"/include -I"+prefix+"/include/json-c -o "+filepath.Join(inst.Dir, JSONFilter))
	for _, f := range jsonpathFiles {
		compile = append(compile, filepath.Join(jp, f))
	}
	for _, f := range libuboxFiles {
		compile = append(compile, filepath.Join(ubox, f))
	}
	compile = append(compile, prefix+"/lib/libjson-c.a")
	testutil.Equal(t, lines, []string{
		"cmake -S " + jsonc + " -B " + build + " -DCMAKE_INSTALL_PREFIX=" + prefix + " " + strings.Join(jsoncFlags, " "),
		"cmake --build " + build + " --parallel 2",
		"cmake --install " + build,
		"cc -std=gnu17 -o " + lemon + " " + lemon + ".c",
		"jsonpath-lemon parser.y",
		strings.Join(compile, " "),
	})
	lemonRun, _ := fake.Find("jsonpath-lemon")
	testutil.Equal(t, lemonRun.Dir, jp)
	testutil.Equal(t, m.Count(), 2)
	entries, err := os.ReadDir(inst.Dir)
	testutil.NoErr(t, err)
	testutil.Equal(t, len(entries), 1)
}

func TestJSONFilterHeadersReachTheBuild(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "include", sourceLibubox)
	testutil.Write(t, filepath.Join(src, "list.h"), "list")
	testutil.Write(t, filepath.Join(src, "uloop.c"), "code")
	testutil.NoErr(t, os.Mkdir(filepath.Join(src, "dir.h"), dirPerm))
	testutil.NoErr(t, copyHeaders(src, dst))
	entries, err := os.ReadDir(dst)
	testutil.NoErr(t, err)
	testutil.Equal(t, len(entries), 1)
	testutil.Equal(t, testutil.Read(t, filepath.Join(dst, "list.h")), "list")
	testutil.Fails(t, copyHeaders(filepath.Join(src, "missing"), dst))
	testutil.Write(t, filepath.Join(t.TempDir(), "x"), "")
	blocked := filepath.Join(t.TempDir(), "file")
	testutil.Write(t, blocked, "")
	testutil.Fails(t, copyHeaders(src, filepath.Join(blocked, "sub")))
	readonly := filepath.Join(t.TempDir(), "ro")
	testutil.NoErr(t, os.MkdirAll(readonly, 0o500))
	testutil.Fails(t, copyHeaders(src, readonly))
	unreadable := t.TempDir()
	testutil.Write(t, filepath.Join(unreadable, "secret.h"), "x")
	testutil.NoErr(t, os.Chmod(filepath.Join(unreadable, "secret.h"), 0))
	testutil.Fails(t, copyHeaders(unreadable, filepath.Join(t.TempDir(), "out")))
}

func TestJSONFilterBuildFailures(t *testing.T) {
	data := []byte(payload)
	for _, step := range []string{"cmake -S", "cmake --build", "cmake --install", "cc -std=gnu17", "jsonpath-lemon", "cc -Os", tarStep} {
		fake := &procexectest.Fake{Handler: newBuilder(t, step).run}
		tr := assets(t, data)
		m := jsonfilterManifest(digest(data))
		m.Binaries[0].SHA256 = digest(tr[cmakeURL].body)
		err := installer(t, tr, fake).Install(context.Background(), m)
		testutil.Contains(t, err.Error(), JSONFilter+": ")
	}
	tr := assets(t, data)
	tr[uboxURL] = respond(http.StatusOK, []byte("forged"))
	m := jsonfilterManifest(digest(data))
	m.Binaries[0].SHA256 = digest(tr[cmakeURL].body)
	err := installer(t, tr, &procexectest.Fake{Handler: newBuilder(t, "").run}).Install(context.Background(), m)
	testutil.Is(t, err, ErrChecksum)
	testutil.Contains(t, err.Error(), "libubox: ")
	m.Sources[0].Tree = "elsewhere"
	err = installer(t, assets(t, data), &procexectest.Fake{Handler: newBuilder(t, "").run}).Install(context.Background(), Manifest{Sources: m.Sources})
	testutil.Is(t, err, ErrMember)
	blocked := installer(t, assets(t, data), &procexectest.Fake{Handler: newBuilder(t, "").run})
	testutil.NoErr(t, os.MkdirAll(blocked.Dir, 0o500))
	testutil.Fails(t, blocked.buildJSONFilter(context.Background(), m.Sources))
}

func TestSourceValidation(t *testing.T) {
	sum := strings.Repeat("b", 64)
	testutil.NoErr(t, jsonfilterManifest(sum).Validate())
	testutil.NoErr(t, Manifest{}.Validate())
	mutate := []func(*Manifest){
		func(m *Manifest) { m.Sources[0].Name = "openssl" },
		func(m *Manifest) { m.Sources[1].URL = "https://git.openwrt.org/project/libubox.git" },
		func(m *Manifest) { m.Sources[1].URL = "https://github.com/openwrt/libubox/archive/main.tar.gz" },
		func(m *Manifest) { m.Sources[2].SHA256 = badSum },
		func(m *Manifest) { m.Sources[2].Tree = upTree },
		func(m *Manifest) { m.Sources = m.Sources[:2] },
		func(m *Manifest) { m.Sources = append(m.Sources, m.Sources[0]) },
		func(m *Manifest) { m.Binaries = nil },
	}
	for _, f := range mutate {
		m := jsonfilterManifest(sum)
		f(&m)
		testutil.Is(t, m.Validate(), ErrManifest)
	}
	testutil.Equal(t, Manifest{Binaries: make([]Binary, 2), Go: []string{"x"}}.Count(), 3)
}
