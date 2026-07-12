package tenantauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// jwksFetcher returns the raw JWKS document. It is an indirection so unit
// tests can serve a JWKS in-process; production wires it to the cluster's
// API server over HTTPS.
type jwksFetcher func(ctx context.Context) ([]byte, error)

// Sentinel errors so Authenticate can turn a keyfunc failure into a precise
// `reason` label instead of burying every JWKS problem in "malformed".
var (
	errUnknownKID      = errors.New("tenantauth: unknown key id (kid) — not in the cluster JWKS")
	errJWKSUnavailable = errors.New("tenantauth: cluster JWKS unavailable")
	errNoKeys          = errors.New("tenantauth: cluster JWKS contains no usable keys")
	errAlgNotAllowed   = errors.New("tenantauth: token algorithm is not in the allowed set")
)

// jwksCache holds the cluster's verification keys, keyed by `kid`.
//
// The API server rotates its signing keys, so the cache must be able to
// learn a new one: a token whose `kid` is unknown triggers a refetch
// (rate-limited by minRefresh, so a flood of bogus kids cannot be turned
// into a flood of API-server requests). A token whose kid is still unknown
// after that refetch is rejected — an unknown key is never trusted.
type jwksCache struct {
	fetch      jwksFetcher
	minRefresh time.Duration

	mu          sync.RWMutex
	keys        map[string]jwksKey
	lastAttempt time.Time
}

// jwksKey is one verification key plus the algorithm the JWKS says it is
// for ("" when the JWKS does not pin one).
type jwksKey struct {
	key crypto.PublicKey
	alg string
}

func newJWKSCache(fetch jwksFetcher, minRefresh time.Duration) *jwksCache {
	return &jwksCache{fetch: fetch, minRefresh: minRefresh, keys: map[string]jwksKey{}}
}

// keyFor resolves the verification key(s) for a token header.
//
// With a `kid` (what Kubernetes always emits) it returns exactly that key.
// Without one, it returns every cached key whose type can carry `alg` as a
// jwt.VerificationKeySet — golang-jwt then tries each, which keeps very old
// clusters that emit no `kid` working without weakening anything: a
// signature still has to verify against a key the cluster published.
func (c *jwksCache) keyFor(ctx context.Context, kid, alg string) (any, error) {
	if k, ok := c.lookup(kid); ok && keyMatchesAlg(k, alg) {
		return k.key, nil
	}
	// Unknown kid (or a cached key that does not match the token's alg):
	// the cluster may have rotated. Refetch once, then decide.
	if err := c.refresh(ctx); err != nil {
		return nil, err
	}
	if k, ok := c.lookup(kid); ok {
		if !keyMatchesAlg(k, alg) {
			return nil, fmt.Errorf("%w: kid %q is not a %s key", errAlgNotAllowed, kid, alg)
		}
		return k.key, nil
	}
	if kid == "" {
		return c.allKeysFor(alg)
	}
	return nil, fmt.Errorf("%w: kid=%q", errUnknownKID, kid)
}

func (c *jwksCache) lookup(kid string) (jwksKey, bool) {
	if kid == "" {
		return jwksKey{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	k, ok := c.keys[kid]
	return k, ok
}

// allKeysFor is the no-kid fallback: hand golang-jwt every cached key that
// could carry this algorithm and let the signature decide.
func (c *jwksCache) allKeysFor(alg string) (any, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	set := make([]crypto.PublicKey, 0, len(c.keys))
	for _, k := range c.keys {
		if keyMatchesAlg(k, alg) {
			set = append(set, k.key)
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("%w (token carries no kid, no cached %s key)", errNoKeys, alg)
	}
	return set, nil
}

// refresh refetches the JWKS. Attempts are rate-limited: within
// minRefresh of the last attempt the call is a no-op (successful refreshes
// have already replaced the cache; failed ones must not be retried per
// request). A refresh that fails while the cache is still populated is not
// fatal — the cached keys stay usable.
func (c *jwksCache) refresh(ctx context.Context) error {
	c.mu.Lock()
	if !c.lastAttempt.IsZero() && time.Since(c.lastAttempt) < c.minRefresh {
		c.mu.Unlock()
		return nil
	}
	c.lastAttempt = time.Now()
	c.mu.Unlock()

	raw, err := c.fetch(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", errJWKSUnavailable, err)
	}
	keys, err := parseJWKS(raw)
	if err != nil {
		return fmt.Errorf("%w: %w", errJWKSUnavailable, err)
	}
	if len(keys) == 0 {
		return errNoKeys
	}
	c.mu.Lock()
	c.keys = keys
	c.mu.Unlock()
	return nil
}

// keyMatchesAlg pins key type to algorithm family. Even inside the allowed
// (asymmetric-only) algorithm set, an RSA key must never be handed to an
// ECDSA verifier or vice versa; and if the JWKS itself pins an `alg` on the
// key, the token header must match it exactly.
func keyMatchesAlg(k jwksKey, alg string) bool {
	if k.alg != "" && k.alg != alg {
		return false
	}
	switch k.key.(type) {
	case *rsa.PublicKey:
		return alg == "RS256"
	case *ecdsa.PublicKey:
		return alg == "ES256"
	case ed25519.PublicKey:
		return alg == "EdDSA"
	default:
		return false
	}
}

// --- JWKS document parsing (RFC 7517 / 7518) ---
//
// Hand-rolled rather than pulling in a JOSE library: the extension is its
// own Go module and this is ~60 lines of well-specified decoding against a
// document the API server produces. Only the key types the allowed
// algorithms can use are decoded; anything else in the JWKS is ignored
// rather than guessed at.

type jwksDocument struct {
	Keys []jwkDocument `json:"keys"`
}

type jwkDocument struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func parseJWKS(raw []byte) (map[string]jwksKey, error) {
	var doc jwksDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse JWKS: %w", err)
	}
	out := make(map[string]jwksKey, len(doc.Keys))
	for _, jwk := range doc.Keys {
		if jwk.Use != "" && jwk.Use != "sig" {
			continue // encryption keys are none of our business
		}
		key, err := jwk.publicKey()
		if err != nil || key == nil {
			continue // unusable/unknown key type: ignore, do not fail the whole set
		}
		out[jwk.Kid] = jwksKey{key: key, alg: jwk.Alg}
	}
	return out, nil
}

func (j jwkDocument) publicKey() (crypto.PublicKey, error) {
	switch j.Kty {
	case "RSA":
		return j.rsaKey()
	case "EC":
		return j.ecKey()
	case "OKP":
		return j.okpKey()
	default:
		return nil, fmt.Errorf("unsupported kty %q", j.Kty)
	}
}

func (j jwkDocument) rsaKey() (crypto.PublicKey, error) {
	n, err := b64uint(j.N)
	if err != nil {
		return nil, fmt.Errorf("rsa n: %w", err)
	}
	eBytes, err := b64(j.E)
	if err != nil {
		return nil, fmt.Errorf("rsa e: %w", err)
	}
	if len(eBytes) == 0 || len(eBytes) > 8 {
		return nil, fmt.Errorf("rsa e: bad length %d", len(eBytes))
	}
	var e uint64
	for _, b := range eBytes {
		e = e<<8 | uint64(b)
	}
	if e == 0 || e > 1<<31 {
		return nil, fmt.Errorf("rsa e: out of range")
	}
	return &rsa.PublicKey{N: n, E: int(e)}, nil
}

func (j jwkDocument) ecKey() (crypto.PublicKey, error) {
	// Only P-256 — the only curve in the allowed algorithm set (ES256).
	if j.Crv != "P-256" {
		return nil, fmt.Errorf("unsupported EC curve %q", j.Crv)
	}
	x, err := b64(j.X)
	if err != nil {
		return nil, fmt.Errorf("ec x: %w", err)
	}
	y, err := b64(j.Y)
	if err != nil {
		return nil, fmt.Errorf("ec y: %w", err)
	}
	const coordLen = 32 // P-256
	if len(x) != coordLen || len(y) != coordLen {
		return nil, fmt.Errorf("ec coordinates have length %d/%d, want %d each", len(x), len(y), coordLen)
	}
	// SEC 1 uncompressed point. ParseUncompressedPublicKey does the
	// on-curve check for us, so a JWKS cannot smuggle in an invalid point.
	point := append([]byte{4}, append(x, y...)...)
	return ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
}

func (j jwkDocument) okpKey() (crypto.PublicKey, error) {
	if j.Crv != "Ed25519" {
		return nil, fmt.Errorf("unsupported OKP curve %q", j.Crv)
	}
	x, err := b64(j.X)
	if err != nil {
		return nil, fmt.Errorf("okp x: %w", err)
	}
	if len(x) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("okp x: bad length %d", len(x))
	}
	return ed25519.PublicKey(x), nil
}

func b64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func b64uint(s string) (*big.Int, error) {
	b, err := b64(s)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, errors.New("empty value")
	}
	return new(big.Int).SetBytes(b), nil
}

// --- HTTP fetching ---

// newHTTPJWKSFetcher builds the production fetcher: an HTTPS client that
// trusts the cluster CA and presents the gateway's own ServiceAccount token
// (re-read per fetch, because the kubelet rotates it).
func newHTTPJWKSFetcher(cfg *ServiceAccountConfig) (jwksFetcher, error) {
	url := cfg.jwksURL()
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	caFile := cfg.caFile()
	if strings.HasPrefix(url, "https://") {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("tenantauth: read serviceaccount.ca_file %q: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tenantauth: serviceaccount.ca_file %q contains no certificates", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
	tokenFile := cfg.tokenFile()

	return func(ctx context.Context) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if tokenFile != noTokenFile {
			token, err := os.ReadFile(tokenFile)
			if err != nil {
				return nil, fmt.Errorf("read serviceaccount.token_file %q: %w", tokenFile, err)
			}
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch JWKS %s: %w", url, err)
		}
		defer func() { _ = resp.Body.Close() }()
		// 1 MiB is far more than any real JWKS; bound it so a hostile
		// endpoint cannot balloon the gateway's memory.
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return nil, fmt.Errorf("read JWKS %s: %w", url, err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch JWKS %s: unexpected status %s", url, resp.Status)
		}
		return body, nil
	}, nil
}
