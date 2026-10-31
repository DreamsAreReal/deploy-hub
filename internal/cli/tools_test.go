package cli

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

func TestToolsCacheFollowsTheRunner(t *testing.T) {
	t.Setenv(toolCacheEnv, "/runner/tool")
	testutil.Equal(t, toolsCache(), filepath.Join("/runner/tool", toolCacheDir))
	t.Setenv(toolCacheEnv, "")
	t.Setenv("XDG_CACHE_HOME", "/home/x/.cache")
	testutil.Equal(t, toolsCache(), filepath.Join("/home/x/.cache", toolCacheDir))
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	testutil.Equal(t, toolsCache(), "")
	tr, ok := toolsClient().Transport.(*http.Transport)
	testutil.True(t, ok && tr.TLSHandshakeTimeout == toolsTLSHandshake, "slow mirrors get a longer TLS handshake")
}

func TestToolCacheFillsFromTheEmbeddedManifest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tools")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := toolCache{Log: slog.New(slog.DiscardHandler), Dir: dir}.Fill(ctx)
	testutil.Is(t, err, context.Canceled)
	testutil.Contains(t, err.Error(), "busybox", "golangci-lint", "dotnet")
	testutil.Perm(t, dir, 0o755)
}
