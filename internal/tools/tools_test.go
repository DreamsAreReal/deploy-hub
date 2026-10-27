package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec"
	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	assetURL = "https://github.com/o/r/releases/download/v1/tool.tar.gz"
	rawURL   = "https://github.com/o/r/releases/download/v1/tool-linux"
	member   = "dir/tool"
	payload  = "#!binary"
	toolFile = "tool"
	treeURL  = "https://nodejs.org/dist/v1.2.3/node-v1.2.3-linux-x64.tar.xz"
	treeTop  = "node-v1"
	nodeLink = "bin/node"
	npmLink  = "bin/npm"
	nodeName = "node"
	dotnet   = "dotnet"
)

type asset struct {
	status int
	body   []byte
}

type transport map[string]asset

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	a, ok := t[r.URL.String()]
	if !ok {
		return nil, errors.New("offline")
	}
	return &http.Response{StatusCode: a.status, Body: io.NopCloser(bytes.NewReader(a.body)), Header: http.Header{}}, nil
}

func respond(status int, body []byte) asset {
	return asset{status: status, body: body}
}

func archive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	testutil.NoErr(t, tw.WriteHeader(&tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o755}))
	for name, content := range files {
		testutil.NoErr(t, tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(content))}))
		_, err := tw.Write([]byte(content))
		testutil.NoErr(t, err)
	}
	testutil.NoErr(t, tw.Close())
	testutil.NoErr(t, gz.Close())
	return buf.Bytes()
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func installer(t *testing.T, tr transport, fake *procexectest.Fake) Installer {
	t.Helper()
	return Installer{Client: &http.Client{Transport: tr}, Dir: filepath.Join(t.TempDir(), "bin"), Exec: fake, Log: slog.New(slog.DiscardHandler)}
}

func TestInstallVerifiesAndExtracts(t *testing.T) {
	tarball := archive(t, map[string]string{"dir/other": "x", member: payload})
	raw := []byte("raw-binary")
	fake := &procexectest.Fake{}
	inst := installer(t, transport{assetURL: respond(http.StatusOK, tarball), rawURL: respond(http.StatusOK, raw)}, fake)
	m := Manifest{
		Binaries: []Binary{{Name: toolFile, URL: assetURL, SHA256: digest(tarball), Member: member}, {Name: "raw", URL: rawURL, SHA256: digest(raw)}},
		Go:       []string{"example.com/x/cmd/y@v1.2.3"},
	}
	var logs bytes.Buffer
	inst.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	testutil.NoErr(t, inst.Install(context.Background(), m))
	testutil.Contains(t, logs.String(), `"msg":"tool installed","tool":"tool","sha256":"`+digest(tarball)+`"`, `"msg":"tool installed","tool":"example.com/x/cmd/y@v1.2.3"`)
	testutil.Equal(t, testutil.Read(t, filepath.Join(inst.Dir, toolFile)), payload)
	testutil.Equal(t, testutil.Read(t, filepath.Join(inst.Dir, "raw")), "raw-binary")
	testutil.Perm(t, filepath.Join(inst.Dir, toolFile), binPerm)
	calls := fake.Calls()
	testutil.Equal(t, procexectest.Line(calls[0]), "go install example.com/x/cmd/y@v1.2.3")
	testutil.Equal(t, calls[0].Env, []string{"GOBIN=" + inst.Dir})
	testutil.Equal(t, calls[0].Dir, inst.Dir)
}

func TestInstallFailures(t *testing.T) {
	tarball := archive(t, map[string]string{member: payload})
	cases := map[string]struct {
		tr  transport
		bin Binary
		err error
	}{
		"checksum": {transport{assetURL: respond(http.StatusOK, tarball)}, Binary{Name: "t", URL: assetURL, SHA256: strings.Repeat("0", 64), Member: member}, ErrChecksum},
		"member":   {transport{assetURL: respond(http.StatusOK, tarball)}, Binary{Name: "t", URL: assetURL, SHA256: digest(tarball), Member: "dir/missing"}, ErrMember},
		"not gzip": {transport{assetURL: respond(http.StatusOK, []byte("plain"))}, Binary{Name: "t", URL: assetURL, SHA256: digest([]byte("plain")), Member: member}, nil},
		"status":   {transport{assetURL: respond(http.StatusNotFound, nil)}, Binary{Name: "t", URL: assetURL, SHA256: digest(nil)}, nil},
		"redirect": {transport{assetURL: respond(http.StatusMultipleChoices, nil)}, Binary{Name: "t", URL: assetURL, SHA256: digest(nil)}, nil},
		"offline":  {transport{}, Binary{Name: "t", URL: assetURL, SHA256: digest(nil)}, nil},
		"bad url":  {transport{}, Binary{Name: "t", URL: "http://bad host/", SHA256: digest(nil)}, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := installer(t, c.tr, &procexectest.Fake{}).Install(context.Background(), Manifest{Binaries: []Binary{c.bin}})
			testutil.Fails(t, err)
			if c.err != nil {
				testutil.Is(t, err, c.err)
			}
		})
	}
	broken := gzipOf(t, []byte("not a tar archive"))
	err := installer(t, transport{assetURL: respond(http.StatusOK, broken)}, &procexectest.Fake{}).Install(context.Background(),
		Manifest{Binaries: []Binary{{Name: "t", URL: assetURL, SHA256: digest(broken), Member: member}}})
	testutil.Fails(t, err)
	goFails := &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) {
		return procexec.Result{Stderr: "no such module"}, errors.New("exit 1")
	}}
	err = installer(t, transport{}, goFails).Install(context.Background(), Manifest{Go: []string{"example.com/x@v1.0.0"}})
	testutil.Contains(t, err.Error(), "go install example.com/x@v1.0.0", "no such module")
	blocked := installer(t, transport{}, &procexectest.Fake{})
	testutil.Write(t, blocked.Dir, "file in the way")
	testutil.Fails(t, blocked.Install(context.Background(), Manifest{}))
}

func unpacker(t *testing.T, build func(root string)) *procexectest.Fake {
	t.Helper()
	return &procexectest.Fake{Handler: func(c procexec.Command) (procexec.Result, error) {
		testutil.Equal(t, c.Name, TarBinary)
		testutil.Equal(t, testutil.Read(t, c.Args[1]), payload)
		build(c.Args[3])
		return procexec.Result{}, nil
	}}
}

func nodeTree(t *testing.T) func(string) {
	t.Helper()
	return func(root string) {
		top := filepath.Join(root, treeTop)
		testutil.Write(t, filepath.Join(top, filepath.FromSlash(nodeLink)), payload)
		testutil.Write(t, filepath.Join(top, "lib", "npm-cli.js"), "npm")
		testutil.NoErr(t, os.Symlink(filepath.Join("..", "lib", "npm-cli.js"), filepath.Join(top, filepath.FromSlash(npmLink))))
	}
}

func TestInstallUnpacksTrees(t *testing.T) {
	data := []byte(payload)
	fake := unpacker(t, nodeTree(t))
	inst := installer(t, transport{treeURL: respond(http.StatusOK, data)}, fake)
	m := Manifest{Binaries: []Binary{{Name: nodeName, URL: treeURL, SHA256: digest(data), Tree: treeTop, Links: []string{nodeLink, npmLink}}}}
	testutil.NoErr(t, inst.Install(context.Background(), m))
	testutil.NoErr(t, inst.Install(context.Background(), m))
	testutil.Equal(t, testutil.Read(t, filepath.Join(inst.Dir, nodeName)), payload)
	testutil.Equal(t, testutil.Read(t, filepath.Join(inst.Dir, "npm")), "npm")
	target, err := os.Readlink(filepath.Join(inst.Dir, nodeName))
	testutil.NoErr(t, err)
	testutil.Equal(t, target, filepath.Join(treesDir, nodeName, "bin", nodeName))
	entries, err := os.ReadDir(inst.Dir)
	testutil.NoErr(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	testutil.Equal(t, names, []string{nodeName, "npm", treesDir})
	testutil.Equal(t, fake.Calls()[0].Args[0], "-xf")
}

func TestUnpackFailures(t *testing.T) {
	data := []byte(payload)
	tree := func(links ...string) Manifest {
		return Manifest{Binaries: []Binary{{Name: nodeName, URL: treeURL, SHA256: digest(data), Tree: treeTop, Links: links}}}
	}
	run := func(fake *procexectest.Fake, m Manifest) error {
		return installer(t, transport{treeURL: respond(http.StatusOK, data)}, fake).Install(context.Background(), m)
	}
	broken := &procexectest.Fake{Handler: func(procexec.Command) (procexec.Result, error) { return procexec.Result{}, errors.New("corrupt") }}
	testutil.Contains(t, run(broken, tree(nodeLink)).Error(), "unpack: corrupt")
	testutil.Fails(t, run(unpacker(t, func(string) {}), tree(nodeLink)))
	testutil.Is(t, run(unpacker(t, nodeTree(t)), tree("bin/missing")), ErrMember)
	testutil.Is(t, run(unpacker(t, nodeTree(t)), tree("lib")), ErrMember)
	locked := installer(t, transport{treeURL: respond(http.StatusOK, data)}, unpacker(t, nodeTree(t)))
	testutil.NoErr(t, os.MkdirAll(locked.Dir, 0o500))
	testutil.Fails(t, locked.Install(context.Background(), tree(nodeLink)))
}

func gzipOf(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write(data)
	testutil.NoErr(t, err)
	testutil.NoErr(t, gz.Close())
	return buf.Bytes()
}

func TestManifestValidation(t *testing.T) {
	good := Binary{Name: toolFile, URL: assetURL, SHA256: strings.Repeat("a", 64), Member: member}
	testutil.NoErr(t, Manifest{Binaries: []Binary{good}, Go: []string{"golang.org/x/tools/cmd/deadcode@v0.52.0"}}.Validate())
	trees := []Binary{
		{Name: nodeName, URL: treeURL, SHA256: good.SHA256, Tree: treeTop, Links: []string{nodeLink, npmLink}},
		{Name: dotnet, URL: "https://builds.dotnet.microsoft.com/dotnet/Sdk/10.0.302/dotnet-sdk-10.0.302-linux-x64.tar.gz", SHA256: good.SHA256, Tree: wholeArchive, Links: []string{dotnet}},
		{Name: batsName, URL: "https://github.com/bats-core/bats-core/archive/refs/tags/v1.14.0.tar.gz", SHA256: good.SHA256, Tree: "bats-core-1.14.0", Links: []string{"bin/bats"}},
	}
	testutil.NoErr(t, Manifest{Binaries: trees}.Validate())
	bad := []Binary{
		{Name: "Tool!", URL: good.URL, SHA256: good.SHA256},
		{Name: toolFile, URL: "https://evil.example/tool", SHA256: good.SHA256},
		{Name: toolFile, URL: "http://github.com/o/r/releases/download/v1/t", SHA256: good.SHA256},
		{Name: toolFile, URL: "https://nodejs.org/dist/latest/node.tar.xz", SHA256: good.SHA256},
		{Name: toolFile, URL: good.URL, SHA256: badSum},
		{Name: toolFile, URL: good.URL, SHA256: good.SHA256, Member: "../escape"},
		{Name: toolFile, URL: good.URL, SHA256: good.SHA256, Member: member, Tree: treeTop, Links: []string{nodeLink}},
		{Name: toolFile, URL: good.URL, SHA256: good.SHA256, Tree: treeTop},
		{Name: toolFile, URL: good.URL, SHA256: good.SHA256, Links: []string{nodeLink}},
		{Name: toolFile, URL: good.URL, SHA256: good.SHA256, Tree: upTree, Links: []string{nodeLink}},
		{Name: toolFile, URL: good.URL, SHA256: good.SHA256, Tree: treeTop, Links: []string{"../node"}},
		{Name: toolFile, URL: good.URL, SHA256: good.SHA256, Tree: treeTop, Links: []string{"bin/Node.exe"}},
	}
	for _, b := range bad {
		testutil.Is(t, Manifest{Binaries: []Binary{b}}.Validate(), ErrManifest)
	}
	for _, g := range []string{"golang.org/x/tools/cmd/deadcode@latest", "deadcode@v1.0.0", "golang.org/x/tools/cmd/deadcode"} {
		testutil.Is(t, Manifest{Go: []string{g}}.Validate(), ErrManifest)
	}
}

func TestLoadManifest(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	m, err := LoadManifest(filepath.Join(filepath.Dir(file), "..", "..", "policy", "tools.yaml"))
	testutil.NoErr(t, err)
	testutil.True(t, len(m.Binaries) > 0 && len(m.Go) > 0, "repository manifest lists tools")
	dir := t.TempDir()
	testutil.Write(t, filepath.Join(dir, "unknown.yaml"), "binaries: []\nextra: 1\n")
	_, err = LoadManifest(filepath.Join(dir, "unknown.yaml"))
	testutil.Is(t, err, ErrManifest)
	_, err = LoadManifest(filepath.Join(dir, "missing.yaml"))
	testutil.True(t, errors.Is(err, os.ErrNotExist), "missing manifest reports the file error")
	testutil.Write(t, filepath.Join(dir, "invalid.yaml"), "go:\n  - example.com/x@latest\n")
	_, err = LoadManifest(filepath.Join(dir, "invalid.yaml"))
	testutil.Is(t, err, ErrManifest)
}

func FuzzExtract(f *testing.F) {
	f.Add([]byte{}, member)
	f.Fuzz(func(t *testing.T, data []byte, name string) {
		out, err := extract(data, name)
		if err == nil && len(out) > maxDownload {
			t.Fatal("extraction exceeded the size limit")
		}
	})
}
