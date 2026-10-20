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
	bad := []Binary{
		{Name: "Tool!", URL: good.URL, SHA256: good.SHA256},
		{Name: toolFile, URL: "https://evil.example/tool", SHA256: good.SHA256},
		{Name: toolFile, URL: "http://github.com/o/r/releases/download/v1/t", SHA256: good.SHA256},
		{Name: toolFile, URL: good.URL, SHA256: "abc"},
		{Name: toolFile, URL: good.URL, SHA256: good.SHA256, Member: "../escape"},
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
