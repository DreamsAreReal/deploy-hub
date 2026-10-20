package e2e

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/oidc"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	issuerKid   = "e2e"
	claimKid    = "kid"
	keyBits     = 2048
	hubWorkflow = "DreamsAreReal/deploy-hub/.github/workflows/deploy.yml@refs/heads/main"
	tokenTTL    = 5 * time.Minute
)

type issuer struct {
	key *rsa.PrivateKey
	url string
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	testutil.NoErr(t, err)
	doc, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", claimKid: issuerKid,
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}})
	testutil.NoErr(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "jwks.json", time.Time{}, bytes.NewReader(doc))
	}))
	t.Cleanup(srv.Close)
	return &issuer{key: key, url: srv.URL}
}

func (i *issuer) token(t *testing.T, repo, tagName string) string {
	t.Helper()
	return i.sign(t, map[string]any{
		"iss": oidc.Issuer, "aud": "deploy-hub", "exp": time.Now().Add(tokenTTL).Unix(), "iat": time.Now().Unix(), "nbf": time.Now().Unix(),
		"repository": repo, "job_workflow_ref": hubWorkflow, "sha": strings.TrimPrefix(tagName, "sha-"), "run_id": "1", "actor": "ci",
	})
}

func (i *issuer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		data, err := json.Marshal(v)
		testutil.NoErr(t, err)
		return base64.RawURLEncoding.EncodeToString(data)
	}
	signing := enc(map[string]string{"alg": "RS256", claimKid: issuerKid}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, sum[:])
	testutil.NoErr(t, err)
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}
