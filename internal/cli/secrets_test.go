package cli

import (
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/secrets"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	storePath = "/srv/server-ops/secrets/values.sops.yaml"
	agePath   = "/run/k"
	sopsPath  = "/opt/sops"
)

func TestSecretsStoreIsUsedOnlyWhenConfigured(t *testing.T) {
	h := newHarness(t)
	cfg, err := LoadConfig(h.lookup)
	testutil.NoErr(t, err)
	testutil.Equal(t, cfg.Secrets, SecretsConfig{Key: defaultAgeKey, Sops: defaultSops})
	testutil.True(t, (&App{Config: cfg}).secrets(nil) == nil, "no store file, no secrets layer")
	h.env[envStoreFile], h.env[envStoreAge], h.env[envSops] = storePath, agePath, sopsPath
	cfg, err = LoadConfig(h.lookup)
	testutil.NoErr(t, err)
	layer, ok := (&App{Config: cfg}).secrets(nil).(secrets.Store)
	testutil.True(t, ok, "a configured store lays secrets")
	testutil.Equal(t, []string{layer.File, layer.KeyFile, layer.Sops}, []string{storePath, agePath, sopsPath})
}
