package tools

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const planted = "planted"

func sharedInstaller(t *testing.T, tr http.RoundTripper, fake *procexectest.Fake) (Installer, *bytes.Buffer) {
	t.Helper()
	inst := installer(t, transport{}, fake)
	inst.Client = &http.Client{Transport: tr}
	inst.Shared = filepath.Join(t.TempDir(), "shared")
	inst.Cache = filepath.Join(t.TempDir(), "cache")
	var logs bytes.Buffer
	inst.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	return inst, &logs
}

func shared(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		testutil.Write(t, filepath.Join(dir, name), content)
	}
	t.Cleanup(func() {
		entries, err := os.ReadDir(dir)
		testutil.NoErr(t, err)
		testutil.Equal(t, len(entries), len(files))
	})
}

func TestSharedCacheServesTheToolWithoutNetwork(t *testing.T) {
	raw := []byte("raw-binary")
	tr := &flaky{fails: 100, next: transport{}}
	inst, logs := sharedInstaller(t, tr, &procexectest.Fake{})
	shared(t, inst.Shared, map[string]string{digest(raw): string(raw)})
	testutil.NoErr(t, inst.Install(context.Background(), rawManifest(raw)))
	testutil.Equal(t, testutil.Read(t, filepath.Join(inst.Dir, rawName)), "raw-binary")
	testutil.Equal(t, tr.tries, 0)
	testutil.Missing(t, filepath.Join(inst.Cache, digest(raw)))
	testutil.Contains(t, logs.String(), `"msg":"tool taken from the cache","dir":"`+inst.Shared+`"`)
}

func TestSharedCacheRejectsAFileWithAnotherHash(t *testing.T) {
	raw := []byte("raw-binary")
	inst, logs := sharedInstaller(t, transport{rawURL: respond(http.StatusOK, raw)}, &procexectest.Fake{})
	shared(t, inst.Shared, map[string]string{digest(raw): planted})
	testutil.NoErr(t, inst.Install(context.Background(), rawManifest(raw)))
	testutil.Equal(t, testutil.Read(t, filepath.Join(inst.Dir, rawName)), "raw-binary")
	testutil.Contains(t, logs.String(), `"msg":"cached tool ignored"`, digest([]byte(planted)))
	testutil.Equal(t, testutil.Read(t, filepath.Join(inst.Shared, digest(raw))), planted)

	offline, _ := sharedInstaller(t, &flaky{fails: 100, next: transport{}}, &procexectest.Fake{})
	shared(t, offline.Shared, map[string]string{digest(raw): planted})
	testutil.Fails(t, offline.Install(context.Background(), rawManifest(raw)))
	testutil.Missing(t, filepath.Join(offline.Dir, rawName))
}

func TestBusyboxBuildsWhileBusyboxNetIsDown(t *testing.T) {
	fakeHost(t)
	data := []byte(payload)
	b := &busyboxMake{t: t, links: awkLink}
	tr := &flaky{fails: 100, next: transport{busyboxURL: respond(http.StatusOK, data)}}
	inst, _ := sharedInstaller(t, tr, &procexectest.Fake{Handler: b.run})
	shared(t, inst.Shared, map[string]string{digest(data): payload})
	testutil.NoErr(t, inst.Install(context.Background(), busyboxManifest(t, data)))
	testutil.Equal(t, testutil.Read(t, filepath.Join(inst.Dir, BusyboxDir, sourceBusybox)), "elf")
	testutil.Equal(t, tr.tries, 0)
}

func TestFillStoresVerifiedFilesOnlyItsOwnerCanWrite(t *testing.T) {
	raw := []byte("raw-binary")
	data := []byte(payload)
	m := rawManifest(raw)
	m.Sources = busyboxManifest(t, data).Sources
	inst, _ := sharedInstaller(t, transport{rawURL: respond(http.StatusOK, raw), busyboxURL: respond(http.StatusOK, data)}, &procexectest.Fake{})
	dir := filepath.Join(t.TempDir(), "deploy-hub", "tools")
	testutil.NoErr(t, os.MkdirAll(dir, 0o700))
	added, err := inst.Fill(context.Background(), m, dir)
	testutil.NoErr(t, err)
	testutil.Equal(t, added, 2)
	testutil.Equal(t, testutil.Read(t, filepath.Join(dir, digest(raw))), "raw-binary")
	testutil.Equal(t, testutil.Read(t, filepath.Join(dir, digest(data))), payload)
	testutil.Perm(t, dir, sharedDirPerm)
	testutil.Perm(t, filepath.Join(dir, digest(raw)), sharedPerm)
	testutil.Perm(t, filepath.Join(dir, digest(data)), sharedPerm)

	testutil.Write(t, filepath.Join(dir, digest(raw)), planted)
	tr := &flaky{next: transport{rawURL: respond(http.StatusOK, raw)}}
	inst.Client = &http.Client{Transport: tr}
	added, err = inst.Fill(context.Background(), m, dir)
	testutil.NoErr(t, err)
	testutil.Equal(t, added, 1)
	testutil.Equal(t, tr.tries, 1)
	testutil.Equal(t, testutil.Read(t, filepath.Join(dir, digest(raw))), "raw-binary")
}

func TestFillRefusesADownloadWithAnotherHash(t *testing.T) {
	raw := []byte("raw-binary")
	inst, _ := sharedInstaller(t, transport{rawURL: respond(http.StatusOK, []byte("evil"))}, &procexectest.Fake{})
	dir := filepath.Join(t.TempDir(), "tools")
	added, err := inst.Fill(context.Background(), rawManifest(raw), dir)
	testutil.Is(t, err, ErrChecksum)
	testutil.Equal(t, added, 0)
	testutil.Missing(t, filepath.Join(dir, digest(raw)))
}

func TestFillCoversTheWholeManifest(t *testing.T) {
	m, err := ParseManifest(mustRead(t, filepath.Join("..", "..", "policy", "tools.yaml")), "")
	testutil.NoErr(t, err)
	urls := map[string]bool{}
	for _, d := range m.archives() {
		urls[d.url] = true
	}
	for _, b := range m.Binaries {
		testutil.True(t, urls[b.URL], b.Name+" is cached")
	}
	for _, s := range m.Sources {
		testutil.True(t, urls[s.URL], s.Name+" is cached")
	}
	testutil.True(t, urls[busyboxURL], "busybox is cached")
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	testutil.NoErr(t, err)
	return data
}
