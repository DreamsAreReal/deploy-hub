package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/secrets"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	storePath = "/srv/server-ops/secrets/values.json.age"
	keyPath   = "/run/k"
	agePath   = "/opt/age"
	appOther  = "other"
	msgBound  = "bound to another app"
)

func TestSecretsStoreIsUsedOnlyWhenConfigured(t *testing.T) {
	h := newHarness(t)
	cfg, err := LoadConfig(h.lookup)
	testutil.NoErr(t, err)
	testutil.Equal(t, cfg.Secrets, SecretsConfig{Key: defaultAgeKey, Age: defaultAge})
	testutil.True(t, (&App{Config: cfg}).secrets(nil) == nil, "no store file, no secrets layer")
	h.env[envStoreFile], h.env[envStoreAge], h.env[envAge] = storePath, keyPath, agePath
	cfg, err = LoadConfig(h.lookup)
	testutil.NoErr(t, err)
	layer, ok := (&App{Config: cfg}).secrets(nil).(secrets.Store)
	testutil.True(t, ok, "a configured store lays secrets")
	testutil.Equal(t, []string{layer.File, layer.KeyFile, layer.Age}, []string{storePath, keyPath, agePath})
}

func runBound(t *testing.T, only string) (*harness, int) {
	h := newHarness(t)
	h.write(filepath.Join(h.hubDir, "apps.list"), appWeb+"\n")
	h.env[envSSHOriginal] = "history " + appWeb
	return h, h.run(CmdRunner, flagOnly, only)
}

func TestRunnerKeyBoundToOneApp(t *testing.T) {
	h, code := runBound(t, appOther)
	if code != ExitFail {
		t.Fatalf("a key bound to another app was let through: %s", h.out)
	}
	testutil.Contains(t, h.err.String(), msgBound)
	_, code = runBound(t, appWeb)
	testutil.Equal(t, code, ExitOK)
}

func TestRunnerKeyBoundToTheAppsOfOneRepository(t *testing.T) {
	for only, refused := range map[string]bool{appOther + "," + appWeb: false, "": true} {
		h, code := runBound(t, only)
		testutil.Equal(t, code == ExitFail, refused)
		testutil.Equal(t, strings.Contains(h.err.String(), msgBound), refused)
	}
}
