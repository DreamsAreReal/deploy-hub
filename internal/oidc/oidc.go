package oidc

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	Issuer       = "https://token.actions.githubusercontent.com"
	JWKSURL      = Issuer + "/.well-known/jwks"
	algRS256     = "RS256"
	keyTypeRSA   = "RSA"
	tokenParts   = 3
	clockSkew    = time.Minute
	maxLifetime  = time.Hour
	fetchTimeout = 5 * time.Second
	maxJWKSBytes = 1 << 20
	maxTokenSize = 16 << 10
	cachePerm    = 0o600
	cacheDirPerm = 0o700
	firstBadCode = 300
)

var (
	ErrToken  = errors.New("invalid OIDC token")
	ErrClaims = errors.New("OIDC claims rejected")
	ErrKeys   = errors.New("OIDC signing keys unavailable")
)

type audience []string

func (a *audience) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func (a audience) has(v string) bool {
	for _, x := range a {
		if x == v {
			return true
		}
	}
	return false
}

type Claims struct {
	Issuer          string   `json:"iss"`
	Audience        audience `json:"aud"`
	Expiry          int64    `json:"exp"`
	NotBefore       int64    `json:"nbf"`
	IssuedAt        int64    `json:"iat"`
	Repository      string   `json:"repository"`
	RepositoryOwner string   `json:"repository_owner"`
	JobWorkflowRef  string   `json:"job_workflow_ref"`
	Sha             string   `json:"sha"`
	Ref             string   `json:"ref"`
	RunID           string   `json:"run_id"`
	Actor           string   `json:"actor"`
	EventName       string   `json:"event_name"`
}

type Expect struct {
	Audience    string
	WorkflowRef string
	Repository  string
	Commit      string
	Now         time.Time
}

type header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type KeySet map[string]*rsa.PublicKey

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func ParseKeySet(data []byte) (KeySet, error) {
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKeys, err)
	}
	out := KeySet{}
	for _, k := range doc.Keys {
		if pub, ok := k.rsa(); ok {
			out[k.Kid] = pub
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no RSA keys", ErrKeys)
	}
	return out, nil
}

func (k jwk) rsa() (*rsa.PublicKey, bool) {
	n, nerr := base64.RawURLEncoding.DecodeString(k.N)
	e, eerr := base64.RawURLEncoding.DecodeString(k.E)
	if k.Kty != keyTypeRSA || k.Kid == "" || nerr != nil || eerr != nil || len(e) == 0 {
		return nil, false
	}
	exp := new(big.Int).SetBytes(e)
	if !exp.IsInt64() || exp.Int64() > int64(^uint32(0)>>1) {
		return nil, false
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exp.Int64())}, true
}

func Verify(token string, keys KeySet, want Expect) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(token) > maxTokenSize || len(parts) != tokenParts {
		return Claims{}, fmt.Errorf("%w: malformed", ErrToken)
	}
	var h header
	if err := decodePart(parts[0], &h); err != nil || h.Alg != algRS256 {
		return Claims{}, fmt.Errorf("%w: unsupported header", ErrToken)
	}
	pub, ok := keys[h.Kid]
	if !ok {
		return Claims{}, fmt.Errorf("%w: unknown key %q", ErrToken, h.Kid)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: bad signature encoding", ErrToken)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return Claims{}, fmt.Errorf("%w: signature does not verify", ErrToken)
	}
	var c Claims
	if err := decodePart(parts[1], &c); err != nil {
		return Claims{}, fmt.Errorf("%w: bad claims", ErrToken)
	}
	return c, c.check(want)
}

func decodePart(part string, v any) error {
	data, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func (c Claims) check(want Expect) error {
	now := want.Now.Unix()
	skew := int64(clockSkew.Seconds())
	switch {
	case c.Issuer != Issuer:
		return fmt.Errorf("%w: issuer %q", ErrClaims, c.Issuer)
	case !c.Audience.has(want.Audience):
		return fmt.Errorf("%w: audience", ErrClaims)
	case c.Expiry <= now-skew || c.NotBefore > now+skew || c.IssuedAt > now+skew:
		return fmt.Errorf("%w: token is expired or not yet valid", ErrClaims)
	case c.Expiry-c.IssuedAt > int64(maxLifetime.Seconds()):
		return fmt.Errorf("%w: token lifetime too long", ErrClaims)
	}
	return c.checkSubject(want)
}

func (c Claims) checkSubject(want Expect) error {
	switch {
	case c.JobWorkflowRef != want.WorkflowRef:
		return fmt.Errorf("%w: workflow %q is not the hub deploy workflow", ErrClaims, c.JobWorkflowRef)
	case !strings.EqualFold(c.Repository, want.Repository):
		return fmt.Errorf("%w: repository %q is not bound to this app", ErrClaims, c.Repository)
	case want.Commit == "" || !strings.HasPrefix(c.Sha, want.Commit):
		return fmt.Errorf("%w: commit %q does not match the requested tag", ErrClaims, c.Sha)
	default:
		return nil
	}
}

type Fetcher struct {
	Client *http.Client
	URL    string
	Cache  string
	Log    *slog.Logger
}

func (f Fetcher) Keys(ctx context.Context) (KeySet, error) {
	data, err := f.fetch(ctx)
	if err == nil {
		if keys, perr := ParseKeySet(data); perr == nil {
			f.store(data)
			return keys, nil
		}
	}
	cached, cerr := os.ReadFile(filepath.Clean(f.Cache))
	if cerr != nil {
		return nil, fmt.Errorf("%w: %w", ErrKeys, errors.Join(err, cerr))
	}
	return ParseKeySet(cached)
}

func (f Fetcher) store(data []byte) {
	err := os.MkdirAll(filepath.Dir(f.Cache), cacheDirPerm)
	if err == nil {
		err = os.WriteFile(filepath.Clean(f.Cache), data, cachePerm)
	}
	if err != nil {
		f.Log.Warn("signing key cache not written", "error", err.Error())
	}
}

func (f Fetcher) fetch(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= firstBadCode {
		return nil, fmt.Errorf("%w: status %d", ErrKeys, resp.StatusCode)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(resp.Body, maxJWKSBytes)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
