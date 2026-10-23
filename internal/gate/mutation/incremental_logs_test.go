package mutation

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func logged(ws *gate.Workspace) *bytes.Buffer {
	var buf bytes.Buffer
	ws.Log = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &buf
}

func TestCacheDecisionsAreLogged(t *testing.T) {
	ws, fg := cachedSetup(t)
	first := logged(&ws)
	runOnce(t, ws, fg)
	testutil.Excludes(t, first.String(), logStaleCache, logCacheLoaded)
	ws.MutationCache = keepVerdicts(t, ws)
	loaded := logged(&ws)
	runOnce(t, ws, fg)
	testutil.Contains(t, loaded.String(), `"msg":"`+logCacheLoaded+`","verdicts":`)
	testutil.Excludes(t, loaded.String(), logStaleCache)
	fg.env = goEnvNew
	stale := logged(&ws)
	runOnce(t, ws, fg)
	testutil.Contains(t, stale.String(), logStaleCache)
	ws.MutationCache.SHA256 = Digest([]byte("other"))
	ignored := logged(&ws)
	runOnce(t, ws, fg)
	testutil.Contains(t, ignored.String(), `"msg":"mutation verdict cache ignored"`)
	EngineVersion = ""
	unversioned := logged(&ws)
	runOnce(t, ws, fg)
	testutil.Contains(t, unversioned.String(), logUnversioned)
}

func TestEmptyCacheOfAnotherBaseIsNotStale(t *testing.T) {
	ws, fg := cachedSetup(t)
	path := filepath.Join(t.TempDir(), VerdictsFile)
	digest, err := Verdicts{Version: verdictsVersion, Bases: []string{baseOne}, Entries: map[string]Status{}}.Save(path)
	testutil.NoErr(t, err)
	ws.MutationCache = gate.CachedVerdicts{Path: path, SHA256: digest}
	buf := logged(&ws)
	runOnce(t, ws, fg)
	testutil.Excludes(t, buf.String(), logStaleCache)
}

func TestIncrementalRunFailures(t *testing.T) {
	ws, fg := cachedSetup(t)
	fg.depsErr = errors.New("exit 1")
	_, err := Run(context.Background(), ws)
	testutil.Contains(t, err.Error(), "go list -deps -test")
	fg.depsErr = nil
	testutil.Write(t, filepath.Join(ws.Out, cacheDir+workerSep+"0"), "a file where the worker cache belongs")
	_, err = Run(context.Background(), ws)
	testutil.Contains(t, err.Error(), "cannot seed the worker build caches")
}
