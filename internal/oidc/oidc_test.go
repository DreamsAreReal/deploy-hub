package oidc

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const (
	testKid      = "k1"
	testAudience = "deploy-hub"
	testWorkflow = "DreamsAreReal/deploy-hub/.github/workflows/deploy.yml@refs/heads/main"
	testRepo     = "DreamsAreReal/web"
	testSha      = "0123456789abcdef0123456789abcdef01234567"
	testKeyBits  = 2048
	claimAlg     = "alg"
	claimKid     = "kid"
	claimRepo    = "repository"
	otherValue   = "other"
	badURL       = "http://bad host/"
)

var testNow = time.Unix(1_800_000_000, 0)

func newKey(t testing.TB) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, testKeyBits)
	testutil.NoErr(t, err)
	return k
}

func jwks(t testing.TB, pub *rsa.PublicKey) []byte {
	t.Helper()
	doc := map[string]any{"keys": []map[string]string{{
		"kty": keyTypeRSA, claimKid: testKid, claimAlg: algRS256, "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}}
	data, err := json.Marshal(doc)
	testutil.NoErr(t, err)
	return data
}

func goodClaims() map[string]any {
	return map[string]any{
		"iss": Issuer, "aud": testAudience, "exp": testNow.Add(5 * time.Minute).Unix(), "nbf": testNow.Add(-time.Minute).Unix(),
		"iat": testNow.Add(-time.Minute).Unix(), claimRepo: testRepo, "repository_owner": "DreamsAreReal",
		"job_workflow_ref": testWorkflow, "sha": testSha, "ref": "refs/heads/main", "run_id": "42", "actor": "dev", "event_name": "push",
	}
}

func sign(t testing.TB, key *rsa.PrivateKey, hdr, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		data, err := json.Marshal(v)
		testutil.NoErr(t, err)
		return base64.RawURLEncoding.EncodeToString(data)
	}
	signing := enc(hdr) + "." + enc(claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	testutil.NoErr(t, err)
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func goodHeader() map[string]any {
	return map[string]any{claimAlg: algRS256, claimKid: testKid, "typ": "JWT"}
}

func expect() Expect {
	return Expect{Audience: testAudience, WorkflowRef: testWorkflow, Repository: testRepo, Commit: testSha[:7], Now: testNow}
}

func keySet(t testing.TB, key *rsa.PrivateKey) KeySet {
	t.Helper()
	ks, err := ParseKeySet(jwks(t, &key.PublicKey))
	testutil.NoErr(t, err)
	return ks
}

func TestVerifyAcceptsGenuineToken(t *testing.T) {
	key := newKey(t)
	c, err := Verify(sign(t, key, goodHeader(), goodClaims()), keySet(t, key), expect())
	testutil.NoErr(t, err)
	testutil.Equal(t, c.Repository, testRepo)
	testutil.Equal(t, c.RunID, "42")
	testutil.Equal(t, []string(c.Audience), []string{testAudience})
}

func TestVerifyAcceptsAudienceList(t *testing.T) {
	key := newKey(t)
	claims := goodClaims()
	claims["aud"] = []string{otherValue, testAudience}
	_, err := Verify(sign(t, key, goodHeader(), claims), keySet(t, key), expect())
	testutil.NoErr(t, err)
}

func TestVerifyRejectsForgedAndTamperedTokens(t *testing.T) {
	key, attacker := newKey(t), newKey(t)
	ks := keySet(t, key)
	good := sign(t, key, goodHeader(), goodClaims())
	parts := strings.Split(good, ".")
	tamperedClaims := goodClaims()
	tamperedClaims[claimRepo] = "Evil/web"
	tampered := parts[0] + "." + strings.Split(sign(t, attacker, goodHeader(), tamperedClaims), ".")[1] + "." + parts[2]
	cases := map[string]string{
		"attacker key":       sign(t, attacker, goodHeader(), goodClaims()),
		"tampered claims":    tampered,
		"alg none":           sign(t, key, map[string]any{claimAlg: "none", claimKid: testKid}, goodClaims()),
		"alg hs256":          sign(t, key, map[string]any{claimAlg: "HS256", claimKid: testKid}, goodClaims()),
		"unknown kid":        sign(t, key, map[string]any{claimAlg: algRS256, claimKid: otherValue}, goodClaims()),
		"two parts":          parts[0] + "." + parts[1],
		"bad header base64":  "%%%." + parts[1] + "." + parts[2],
		"bad signature b64":  parts[0] + "." + parts[1] + ".%%%",
		"oversized":          strings.Repeat("a", maxTokenSize+1),
		"empty":              "",
		"header not json":    base64.RawURLEncoding.EncodeToString([]byte("x")) + "." + parts[1] + "." + parts[2],
		"claims not json":    resign(t, key, parts[0]+"."+base64.RawURLEncoding.EncodeToString([]byte("[1]"))),
		"claims bad base64":  resign(t, key, parts[0]+".%%%"),
		"signature stripped": parts[0] + "." + parts[1] + ".",
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Verify(token, ks, expect())
			testutil.Is(t, err, ErrToken)
		})
	}
}

func resign(t testing.TB, key *rsa.PrivateKey, signing string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	testutil.NoErr(t, err)
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestVerifyRejectsClaims(t *testing.T) {
	key := newKey(t)
	ks := keySet(t, key)
	cases := map[string]func(c map[string]any){
		"issuer":           func(c map[string]any) { c["iss"] = "https://evil.example" },
		"audience":         func(c map[string]any) { c["aud"] = otherValue },
		"audience list":    func(c map[string]any) { c["aud"] = []string{"a", "b"} },
		"expired":          func(c map[string]any) { c["exp"] = testNow.Add(-2 * time.Minute).Unix() },
		"expiry at skew":   func(c map[string]any) { c["exp"] = testNow.Add(-clockSkew).Unix() },
		"not yet valid":    func(c map[string]any) { c["nbf"] = testNow.Add(2 * time.Minute).Unix() },
		"issued in future": func(c map[string]any) { c["iat"] = testNow.Add(2 * time.Minute).Unix() },
		"lifetime":         func(c map[string]any) { c["iat"] = testNow.Add(-2 * time.Hour).Unix() },
		"workflow branch":  func(c map[string]any) { c["job_workflow_ref"] = strings.Replace(testWorkflow, "main", "dev", 1) },
		"own workflow": func(c map[string]any) {
			c["job_workflow_ref"] = testRepo + "/.github/workflows/deploy.yml@refs/heads/main"
		},
		"repository": func(c map[string]any) { c["repository"] = "DreamsAreReal/other" },
		"commit":     func(c map[string]any) { c["sha"] = strings.Repeat("f", 40) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			claims := goodClaims()
			mutate(claims)
			_, err := Verify(sign(t, key, goodHeader(), claims), ks, expect())
			testutil.Is(t, err, ErrClaims)
		})
	}
}

func TestVerifyRequiresExpectedCommit(t *testing.T) {
	key := newKey(t)
	want := expect()
	want.Commit = ""
	_, err := Verify(sign(t, key, goodHeader(), goodClaims()), keySet(t, key), want)
	testutil.Is(t, err, ErrClaims)
}

func TestVerifyBoundaries(t *testing.T) {
	key := newKey(t)
	ks := keySet(t, key)
	edge := goodClaims()
	edge["nbf"] = testNow.Add(clockSkew).Unix()
	edge["iat"] = testNow.Add(clockSkew).Unix()
	edge["exp"] = testNow.Add(clockSkew).Unix() + int64(maxLifetime.Seconds())
	_, err := Verify(sign(t, key, goodHeader(), edge), ks, expect())
	testutil.NoErr(t, err)
	repoCase := goodClaims()
	repoCase[claimRepo] = strings.ToLower(testRepo)
	_, err = Verify(sign(t, key, goodHeader(), repoCase), ks, expect())
	testutil.NoErr(t, err)
}

func TestParseKeySet(t *testing.T) {
	key := newKey(t)
	ks, err := ParseKeySet(jwks(t, &key.PublicKey))
	testutil.NoErr(t, err)
	testutil.True(t, ks[testKid].Equal(&key.PublicKey), "key round trip")
	bad := []string{
		`not json`,
		`{"keys":[]}`,
		`{"keys":[{"kty":"EC","kid":"a","n":"AQ","e":"AQAB"}]}`,
		`{"keys":[{"kty":"RSA","kid":"","n":"AQ","e":"AQAB"}]}`,
		`{"keys":[{"kty":"RSA","kid":"a","n":"%%","e":"AQAB"}]}`,
		`{"keys":[{"kty":"RSA","kid":"a","n":"AQ","e":"%%"}]}`,
		`{"keys":[{"kty":"RSA","kid":"a","n":"AQ","e":""}]}`,
		`{"keys":[{"kty":"RSA","kid":"a","n":"AQ","e":"AQAAAAAAAAAA"}]}`,
		`{"keys":[{"kty":"RSA","kid":"a","n":"AQ","e":"gAAAAA"}]}`,
	}
	for _, b := range bad {
		_, perr := ParseKeySet([]byte(b))
		testutil.Is(t, perr, ErrKeys)
	}
	ks, err = ParseKeySet([]byte(`{"keys":[{"kty":"RSA","kid":"a","n":"AQ","e":"f____w"}]}`))
	testutil.NoErr(t, err)
	testutil.Equal(t, ks["a"].E, 1<<31-1)
}

func TestFetcherDownloadsAndCaches(t *testing.T) {
	key := newKey(t)
	body := jwks(t, &key.PublicKey)
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		serve(w, r, body)
	}))
	defer srv.Close()
	cache := filepath.Join(t.TempDir(), "cache", "jwks.json")
	f := Fetcher{Client: srv.Client(), URL: srv.URL, Cache: cache, Log: slog.New(slog.DiscardHandler)}
	ks, err := f.Keys(context.Background())
	testutil.NoErr(t, err)
	testutil.True(t, ks[testKid] != nil, "fetched key")
	testutil.Equal(t, testutil.Read(t, cache), string(body))
	testutil.Perm(t, cache, cachePerm)
	fail.Store(true)
	ks, err = f.Keys(context.Background())
	testutil.NoErr(t, err)
	testutil.True(t, ks[testKid] != nil, "cached key used when GitHub is unreachable")
}

func serve(w http.ResponseWriter, r *http.Request, body []byte) {
	http.ServeContent(w, r, "jwks.json", time.Time{}, bytes.NewReader(body))
}

func TestFetcherFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	for _, f := range []Fetcher{
		{Client: srv.Client(), URL: srv.URL, Cache: filepath.Join(dir, "none.json")},
		{Client: srv.Client(), URL: "http://127.0.0.1:1/jwks", Cache: filepath.Join(dir, "none.json")},
		{Client: srv.Client(), URL: badURL, Cache: filepath.Join(dir, "none.json")},
	} {
		_, err := f.Keys(context.Background())
		testutil.Is(t, err, ErrKeys)
	}
	stale := filepath.Join(dir, "stale.json")
	testutil.Write(t, stale, `{"keys":[]}`)
	_, err := Fetcher{Client: srv.Client(), URL: srv.URL, Cache: stale}.Keys(context.Background())
	testutil.Is(t, err, ErrKeys)
	blocked := filepath.Join(dir, "file")
	testutil.Write(t, blocked, "")
	key := newKey(t)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, jwks(t, &key.PublicKey))
	}))
	defer good.Close()
	var logs strings.Builder
	_, err = Fetcher{Client: good.Client(), URL: good.URL, Cache: filepath.Join(blocked, "jwks.json"), Log: slog.New(slog.NewJSONHandler(&logs, nil))}.Keys(context.Background())
	testutil.NoErr(t, err)
	testutil.Contains(t, logs.String(), "signing key cache not written")
	_, statErr := os.Stat(filepath.Join(blocked, "jwks.json"))
	testutil.True(t, statErr != nil, "cache under a file cannot be written")
}

func TestFetcherLimitsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, []byte(strings.Repeat(" ", maxJWKSBytes+1)))
	}))
	defer srv.Close()
	data, err := Fetcher{Client: srv.Client(), URL: srv.URL}.fetch(context.Background())
	testutil.NoErr(t, err)
	testutil.Equal(t, len(data), maxJWKSBytes)
}

func FuzzVerify(f *testing.F) {
	key := newKey(f)
	ks := keySet(f, key)
	f.Add(sign(f, key, goodHeader(), goodClaims()))
	f.Add("a.b.c")
	f.Add("")
	f.Fuzz(func(t *testing.T, token string) {
		c, err := Verify(token, ks, expect())
		if err == nil && (!strings.EqualFold(c.Repository, testRepo) || c.JobWorkflowRef != testWorkflow) {
			t.Fatalf("accepted foreign claims: %+v", c)
		}
	})
}

func TestVerifyRejectsNonStringAudience(t *testing.T) {
	key := newKey(t)
	claims := goodClaims()
	claims["aud"] = 42
	_, err := Verify(sign(t, key, goodHeader(), claims), keySet(t, key), expect())
	testutil.Is(t, err, ErrToken)
	testutil.Contains(t, err.Error(), "bad claims")
}

func TestParseKeySetReportsTheDecodeError(t *testing.T) {
	_, err := ParseKeySet([]byte("not json"))
	testutil.Contains(t, err.Error(), "invalid character")
}

func TestFetchFailures(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultipleChoices)
	}))
	defer redirect.Close()
	_, err := Fetcher{Client: redirect.Client(), URL: redirect.URL}.fetch(context.Background())
	testutil.Is(t, err, ErrKeys)
	testutil.Contains(t, err.Error(), "status 300")
	truncated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, `{"keys":`)
	}))
	defer truncated.Close()
	_, err = Fetcher{Client: truncated.Client(), URL: truncated.URL}.fetch(context.Background())
	testutil.Fails(t, err)
	_, err = Fetcher{Client: http.DefaultClient, URL: badURL}.fetch(context.Background())
	testutil.Fails(t, err)
}
