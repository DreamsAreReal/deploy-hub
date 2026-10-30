package cli

import (
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/secrets"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	storePath = "/srv/server-ops/secrets/values.json.age"
	keyPath   = "/run/k"
	agePath   = "/opt/age"
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

func TestRunnerKeyBoundToOneApp(t *testing.T) {
	h := newHarness(t)
	h.write(filepath.Join(h.hubDir, "apps.list"), appWeb+"\n")
	h.env[envSSHOriginal] = "history " + appWeb
	if code := h.run(CmdRunner, flagOnly, "other"); code != ExitFail {
		t.Fatalf("a key bound to another app was let through: %s", h.out)
	}
	testutil.Contains(t, h.err.String(), "bound to another app")
	own := newHarness(t)
	own.write(filepath.Join(own.hubDir, "apps.list"), appWeb+"\n")
	own.env[envSSHOriginal] = "history " + appWeb
	testutil.Equal(t, own.run(CmdRunner, flagOnly, appWeb), ExitOK)
}
