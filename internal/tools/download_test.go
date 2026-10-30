package tools

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/procexec/procexectest"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const rawName = "raw"

type flaky struct {
	fails int
	tries int
	next  http.RoundTripper
}

func (f *flaky) RoundTrip(r *http.Request) (*http.Response, error) {
	f.tries++
	if f.tries <= f.fails {
		return nil, errors.New("TLS handshake timeout")
	}
	return f.next.RoundTrip(r)
}

func flakyInstaller(t *testing.T, tr *flaky) Installer {
	t.Helper()
	inst := installer(t, transport{}, &procexectest.Fake{})
	inst.Client = &http.Client{Transport: tr}
	return inst
}

func rawManifest(data []byte) Manifest {
	return Manifest{Binaries: []Binary{{Name: rawName, URL: rawURL, SHA256: digest(data)}}}
}

func TestDownloadRetriesAndKeepsAVerifiedCache(t *testing.T) {
	raw := []byte("raw-binary")
	tr := &flaky{fails: 2, next: transport{rawURL: respond(http.StatusOK, raw)}}
	inst := flakyInstaller(t, tr)
	inst.Cache = filepath.Join(t.TempDir(), "cache")
	testutil.NoErr(t, inst.Install(context.Background(), rawManifest(raw)))
	testutil.Equal(t, tr.tries, downloadTries)
	testutil.Equal(t, testutil.Read(t, filepath.Join(inst.Cache, digest(raw))), "raw-binary")
	inst.Client = &http.Client{Transport: transport{}}
	testutil.NoErr(t, inst.Install(context.Background(), rawManifest(raw)))
	testutil.Equal(t, testutil.Read(t, filepath.Join(inst.Dir, rawName)), "raw-binary")
	testutil.Write(t, filepath.Join(inst.Cache, digest(raw)), "tampered")
	testutil.Fails(t, inst.Install(context.Background(), rawManifest(raw)))
}

func TestDownloadGivesUpAfterItsTries(t *testing.T) {
	tr := &flaky{fails: 10, next: transport{}}
	testutil.Fails(t, flakyInstaller(t, tr).Install(context.Background(), rawManifest(nil)))
	testutil.Equal(t, tr.tries, downloadTries)
}

func TestDownloadStopsWaitingWhenCancelled(t *testing.T) {
	inst := flakyInstaller(t, &flaky{fails: 10, next: transport{}})
	inst.Retry = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	testutil.Is(t, inst.Install(ctx, rawManifest(nil)), context.Canceled)
}

func TestCacheThatCannotBeWrittenIsSkipped(t *testing.T) {
	raw := []byte("raw-binary")
	inst := installer(t, transport{rawURL: respond(http.StatusOK, raw)}, &procexectest.Fake{})
	var logs bytes.Buffer
	inst.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	inst.Cache = filepath.Join(t.TempDir(), "file")
	testutil.Write(t, inst.Cache, "not a directory")
	testutil.NoErr(t, inst.Install(context.Background(), rawManifest(raw)))
	testutil.Contains(t, logs.String(), `"msg":"tool cache not written"`)
}
