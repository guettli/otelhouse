package tenantauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"go.opentelemetry.io/collector/client"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// keyMaterial bundles a matched public/private pair for one test run so
// each case is fully self-contained (no shared state, safe to parallelise).
type keyMaterial struct {
	alg     string
	pubPEM  []byte
	private crypto.PrivateKey
}

// newKeys generates a fresh keypair for `alg` and returns it PEM-encoded.
// A fresh pair per test prevents cross-test leakage and matches how gitops
// mints one keypair per gateway.
func newKeys(t *testing.T, alg string) keyMaterial {
	t.Helper()
	switch alg {
	case "EdDSA":
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("gen ed25519: %v", err)
		}
		return keyMaterial{alg: alg, pubPEM: mustMarshalPKIXPub(t, pub), private: priv}
	case "ES256":
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("gen ecdsa: %v", err)
		}
		return keyMaterial{alg: alg, pubPEM: mustMarshalPKIXPub(t, &priv.PublicKey), private: priv}
	case "RS256":
		priv, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("gen rsa: %v", err)
		}
		return keyMaterial{alg: alg, pubPEM: mustMarshalPKIXPub(t, &priv.PublicKey), private: priv}
	}
	t.Fatalf("unknown alg %q", alg)
	return keyMaterial{}
}

func mustMarshalPKIXPub(t *testing.T, pub any) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal pkix: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func signingMethod(alg string) jwt.SigningMethod {
	switch alg {
	case "EdDSA":
		return jwt.SigningMethodEdDSA
	case "ES256":
		return jwt.SigningMethodES256
	case "RS256":
		return jwt.SigningMethodRS256
	}
	return nil
}

// signToken produces a JWT signed with the caller-provided claims. Any
// intentional deviation (wrong iss, missing tenant, expired, ...) belongs
// in the claims map so the test reads top-to-bottom.
func signToken(t *testing.T, km keyMaterial, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(signingMethod(km.alg), claims)
	s, err := tok.SignedString(km.private)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

// baseClaims returns a valid claim set for the "happy path" cases; tests
// tweak individual fields to cover the negative cases.
func baseClaims() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss":    "otelhouse-mint",
		"aud":    "otelhouse-gateway",
		"tenant": "alice",
		"iat":    now.Unix(),
		"exp":    now.Add(1 * time.Hour).Unix(),
	}
}

func newAuth(t *testing.T, km keyMaterial) *tenantAuth {
	t.Helper()
	cfg := &Config{
		Issuer:       "otelhouse-mint",
		Audience:     "otelhouse-gateway",
		Algorithm:    km.alg,
		PublicKeyPEM: string(km.pubPEM),
		TenantClaim:  defaultTenantClaim,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	a, err := newTenantAuth(cfg, nil)
	if err != nil {
		t.Fatalf("newTenantAuth: %v", err)
	}
	return a
}

func bearer(token string) map[string][]string {
	return map[string][]string{"Authorization": {"Bearer " + token}}
}

// TestAuthenticate_ValidToken_Ed25519 exercises the happy path for the
// preferred algorithm. A verified JWT must land its tenant claim in the
// client.Info of the returned context — that IS the contract the
// tenanttagger processor reads.
func TestAuthenticate_ValidToken_Ed25519(t *testing.T) {
	km := newKeys(t, "EdDSA")
	a := newAuth(t, km)
	token := signToken(t, km, baseClaims())

	ctx, err := a.Authenticate(context.Background(), bearer(token))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	info := client.FromContext(ctx)
	if info.Auth == nil {
		t.Fatal("expected client.Info.Auth to be set")
	}
	got, _ := info.Auth.GetAttribute("tenant").(string)
	if got != "alice" {
		t.Fatalf("tenant = %q, want %q", got, "alice")
	}
}

// TestAuthenticate_ValidToken_ES256 keeps ES256 covered so a config that
// pins ES256 (e.g. because the operator prefers NIST curves) still passes
// the same happy-path assertion.
func TestAuthenticate_ValidToken_ES256(t *testing.T) {
	km := newKeys(t, "ES256")
	a := newAuth(t, km)
	token := signToken(t, km, baseClaims())
	ctx, err := a.Authenticate(context.Background(), bearer(token))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got, _ := client.FromContext(ctx).Auth.GetAttribute("tenant").(string); got != "alice" {
		t.Fatalf("tenant = %q, want alice", got)
	}
}

// TestAuthenticate_ValidToken_RS256 keeps RS256 covered too. RSA is the
// most common asymmetric alg in the wild and the one an operator is most
// likely to inherit from an existing PKI.
func TestAuthenticate_ValidToken_RS256(t *testing.T) {
	km := newKeys(t, "RS256")
	a := newAuth(t, km)
	token := signToken(t, km, baseClaims())
	ctx, err := a.Authenticate(context.Background(), bearer(token))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got, _ := client.FromContext(ctx).Auth.GetAttribute("tenant").(string); got != "alice" {
		t.Fatalf("tenant = %q, want alice", got)
	}
}

// TestAuthenticate_MissingHeader locks in the fail-closed behaviour when
// the request carries no Authorization at all. Untagged data must not
// pass the gateway.
func TestAuthenticate_MissingHeader(t *testing.T) {
	km := newKeys(t, "EdDSA")
	a := newAuth(t, km)
	_, err := a.Authenticate(context.Background(), map[string][]string{})
	if err == nil {
		t.Fatal("expected error for missing header")
	}
}

// TestAuthenticate_MalformedHeader covers a header present but not a
// Bearer scheme (e.g. Basic auth), and a Bearer with empty token.
func TestAuthenticate_MalformedHeader(t *testing.T) {
	km := newKeys(t, "EdDSA")
	a := newAuth(t, km)
	cases := map[string]map[string][]string{
		"not-bearer":   {"Authorization": {"Basic Zm9vOmJhcg=="}},
		"empty-token":  {"Authorization": {"Bearer "}},
		"empty-values": {"Authorization": {}},
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := a.Authenticate(context.Background(), src); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

// TestAuthenticate_BadSignature — same claims, but signed with a
// DIFFERENT keypair. Verification must fail; the attacker cannot forge a
// tenant by knowing only the public key.
func TestAuthenticate_BadSignature(t *testing.T) {
	km := newKeys(t, "EdDSA")
	other := newKeys(t, "EdDSA")
	a := newAuth(t, km)
	token := signToken(t, other, baseClaims())
	if _, err := a.Authenticate(context.Background(), bearer(token)); err == nil {
		t.Fatal("expected signature-verification error")
	}
}

// TestAuthenticate_Expired proves `exp` is enforced. A stale token from a
// rotated key or leaked-and-revoked instance must not still work.
func TestAuthenticate_Expired(t *testing.T) {
	km := newKeys(t, "EdDSA")
	a := newAuth(t, km)
	claims := baseClaims()
	claims["iat"] = time.Now().Add(-2 * time.Hour).Unix()
	claims["exp"] = time.Now().Add(-1 * time.Hour).Unix()
	token := signToken(t, km, claims)
	_, err := a.Authenticate(context.Background(), bearer(token))
	if err == nil {
		t.Fatal("expected expired-token error")
	}
	if !strings.Contains(err.Error(), "expired") && !errors.Is(err, jwt.ErrTokenExpired) {
		t.Fatalf("expected 'expired' error, got %v", err)
	}
}

// TestAuthenticate_MissingExp fails-closed when the mint job forgot to
// stamp exp. Without exp we could not tell an ancient token from a fresh
// one — safer to reject.
func TestAuthenticate_MissingExp(t *testing.T) {
	km := newKeys(t, "EdDSA")
	a := newAuth(t, km)
	claims := baseClaims()
	delete(claims, "exp")
	token := signToken(t, km, claims)
	if _, err := a.Authenticate(context.Background(), bearer(token)); err == nil {
		t.Fatal("expected error for token without exp")
	}
}

// TestAuthenticate_WrongAudience blocks cross-gateway token replay. A
// token minted for another gateway sharing the same key must not work
// here.
func TestAuthenticate_WrongAudience(t *testing.T) {
	km := newKeys(t, "EdDSA")
	a := newAuth(t, km)
	claims := baseClaims()
	claims["aud"] = "some-other-gateway"
	token := signToken(t, km, claims)
	if _, err := a.Authenticate(context.Background(), bearer(token)); err == nil {
		t.Fatal("expected wrong-audience error")
	}
}

// TestAuthenticate_WrongIssuer blocks tokens minted by a rogue issuer
// that happens to have the corresponding public key advertised.
func TestAuthenticate_WrongIssuer(t *testing.T) {
	km := newKeys(t, "EdDSA")
	a := newAuth(t, km)
	claims := baseClaims()
	claims["iss"] = "not-the-real-mint"
	token := signToken(t, km, claims)
	if _, err := a.Authenticate(context.Background(), bearer(token)); err == nil {
		t.Fatal("expected wrong-issuer error")
	}
}

// TestAuthenticate_MissingTenantClaim locks in the invariant that the
// extension NEVER resolves an empty tenant. A cryptographically valid
// token without a tenant claim is still rejected, so the tenanttagger's
// fail-closed guarantee is upheld at the auth layer as well.
func TestAuthenticate_MissingTenantClaim(t *testing.T) {
	km := newKeys(t, "EdDSA")
	a := newAuth(t, km)
	claims := baseClaims()
	delete(claims, "tenant")
	token := signToken(t, km, claims)
	if _, err := a.Authenticate(context.Background(), bearer(token)); err == nil {
		t.Fatal("expected error for token without tenant claim")
	}
}

// TestAuthenticate_EmptyTenantClaim covers a `tenant` claim present but
// empty — same fail-closed reason as the missing case.
func TestAuthenticate_EmptyTenantClaim(t *testing.T) {
	km := newKeys(t, "EdDSA")
	a := newAuth(t, km)
	claims := baseClaims()
	claims["tenant"] = "   "
	token := signToken(t, km, claims)
	if _, err := a.Authenticate(context.Background(), bearer(token)); err == nil {
		t.Fatal("expected error for empty tenant claim")
	}
}

// TestAuthenticate_AlgNone blocks the classic `alg:none` attack — a token
// with no signature but valid-looking claims.
func TestAuthenticate_AlgNone(t *testing.T) {
	km := newKeys(t, "EdDSA")
	a := newAuth(t, km)
	// Build an unsigned token by hand. golang-jwt refuses to construct one,
	// so we craft the header+claims+empty-signature encoding directly.
	// The parser we use pins WithValidMethods to EdDSA, so this MUST fail.
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, baseClaims())
	s, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	if _, err := a.Authenticate(context.Background(), bearer(s)); err == nil {
		t.Fatal("expected alg:none to be rejected")
	}
}

// TestAuthenticate_AlgConfusion blocks the HS-vs-RS downgrade: an
// attacker signs an HS256 token using the RSA public key as the HMAC
// secret. The parser's WithValidMethods pin (algorithm allowlist) refuses
// any method not in {EdDSA, RS256, ...} so this must fail.
func TestAuthenticate_AlgConfusion(t *testing.T) {
	km := newKeys(t, "RS256")
	a := newAuth(t, km)
	// Craft an HS256 token with the RSA public PEM as the HMAC secret.
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, baseClaims())
	s, err := tok.SignedString(km.pubPEM)
	if err != nil {
		t.Fatalf("sign HS256: %v", err)
	}
	if _, err := a.Authenticate(context.Background(), bearer(s)); err == nil {
		t.Fatal("expected alg-confusion HS256 token to be rejected against RS256 config")
	}
}

// TestAuthenticate_WrongMethodForConfig — a valid ES256 token presented
// to a gateway pinned to EdDSA. Legitimate token, wrong gateway.
func TestAuthenticate_WrongMethodForConfig(t *testing.T) {
	edKM := newKeys(t, "EdDSA")
	a := newAuth(t, edKM)
	// Present an ES256 token instead.
	esKM := newKeys(t, "ES256")
	token := signToken(t, esKM, baseClaims())
	if _, err := a.Authenticate(context.Background(), bearer(token)); err == nil {
		t.Fatal("expected wrong-method token to be rejected")
	}
}

// TestConfig_Validate walks the config-level guardrails: an operator
// mistake at deploy time should turn into a startup error, not silent
// mis-verification at runtime.
func TestConfig_Validate(t *testing.T) {
	valid := func() *Config {
		km := newKeys(t, "EdDSA")
		return &Config{
			Issuer:       "iss",
			Audience:     "aud",
			Algorithm:    "EdDSA",
			PublicKeyPEM: string(km.pubPEM),
		}
	}
	t.Run("ok", func(t *testing.T) {
		if err := valid().Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
	t.Run("missing_issuer", func(t *testing.T) {
		c := valid()
		c.Issuer = ""
		if err := c.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("missing_audience", func(t *testing.T) {
		c := valid()
		c.Audience = ""
		if err := c.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("missing_algorithm", func(t *testing.T) {
		c := valid()
		c.Algorithm = ""
		if err := c.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("hmac_rejected", func(t *testing.T) {
		c := valid()
		c.Algorithm = "HS256"
		if err := c.Validate(); err == nil {
			t.Fatal("expected HS256 to be rejected")
		}
	})
	t.Run("none_rejected", func(t *testing.T) {
		c := valid()
		c.Algorithm = "none"
		if err := c.Validate(); err == nil {
			t.Fatal("expected 'none' to be rejected")
		}
	})
	t.Run("missing_key_material", func(t *testing.T) {
		c := valid()
		c.PublicKeyPEM = ""
		if err := c.Validate(); err == nil {
			t.Fatal("expected error when both key sources empty")
		}
	})
	t.Run("both_key_sources_set", func(t *testing.T) {
		c := valid()
		c.PublicKeyFile = "/tmp/foo.pem"
		if err := c.Validate(); err == nil {
			t.Fatal("expected error when both key sources set")
		}
	})
}

// TestFactory checks the factory produces a working extension with the
// documented defaults — a small sanity check on the ocb integration
// point.
func TestFactory(t *testing.T) {
	f := NewFactory()
	cfg := f.CreateDefaultConfig().(*Config)
	if cfg.Algorithm != "EdDSA" {
		t.Fatalf("default alg = %q, want EdDSA", cfg.Algorithm)
	}
	if cfg.TenantClaim != defaultTenantClaim {
		t.Fatalf("default tenant claim = %q, want %q", cfg.TenantClaim, defaultTenantClaim)
	}
}

// ---------------------------------------------------------------------------
// Kubernetes ServiceAccount-token identity source
//
// Everything below runs entirely in-process: keys are generated per test,
// the JWKS document is built from them and served by a fake fetcher (or, in
// one case, a loopback TLS server), and tokens are minted with the matching
// private key. No cluster, no outbound network.
// ---------------------------------------------------------------------------

const (
	testSAIssuer   = "https://kubernetes.default.svc.cluster.local"
	testSAAudience = "otelhouse-gateway"
)

// fakeJWKS is a stand-in for the cluster's /openid/v1/jwks endpoint. It
// records how often it was fetched, so tests can assert that an unknown
// `kid` triggers exactly one (rate-limited) refresh.
type fakeJWKS struct {
	mu      sync.Mutex
	doc     []byte
	err     error
	fetches int
}

func (f *fakeJWKS) fetch(context.Context) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches++
	if f.err != nil {
		return nil, f.err
	}
	return f.doc, nil
}

func (f *fakeJWKS) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches
}

// serve replaces the served document — the cluster rotating its signing key.
func (f *fakeJWKS) serve(doc []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.doc, f.err = doc, nil
}

func (f *fakeJWKS) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// jwksDoc renders `keys` as a JWKS document, exactly as the API server's
// /openid/v1/jwks would.
func jwksDoc(t *testing.T, keys map[string]keyMaterial) []byte {
	t.Helper()
	out := map[string]any{"keys": []any{}}
	list := make([]any, 0, len(keys))
	for kid, km := range keys {
		list = append(list, jwkFor(t, kid, km))
	}
	out["keys"] = list
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	return b
}

// jwkFor encodes one public key as a JWK (RFC 7517), the way the API server
// publishes its signing keys.
func jwkFor(t *testing.T, kid string, km keyMaterial) map[string]any {
	t.Helper()
	signer, ok := km.private.(crypto.Signer)
	if !ok {
		t.Fatalf("key type %T is not a crypto.Signer", km.private)
	}
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	switch pub := signer.Public().(type) {
	case *rsa.PublicKey:
		return map[string]any{
			"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
			"n": b64(pub.N.Bytes()),
			"e": b64(big.NewInt(int64(pub.E)).Bytes()),
		}
	case *ecdsa.PublicKey:
		// SEC 1 uncompressed point: 0x04 || X || Y.
		point, err := pub.Bytes()
		if err != nil {
			t.Fatalf("encode ec public key: %v", err)
		}
		return map[string]any{
			"kty": "EC", "kid": kid, "alg": "ES256", "use": "sig", "crv": "P-256",
			"x": b64(point[1:33]),
			"y": b64(point[33:]),
		}
	case ed25519.PublicKey:
		return map[string]any{
			"kty": "OKP", "kid": kid, "alg": "EdDSA", "use": "sig", "crv": "Ed25519",
			"x": b64(pub),
		}
	default:
		t.Fatalf("unsupported public key type %T", pub)
		return nil
	}
}

// signSAToken mints a token the way the kubelet would: signed with the
// cluster key, carrying the `kid` header the JWKS advertises.
func signSAToken(t *testing.T, km keyMaterial, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(signingMethod(km.alg), claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(km.private)
	if err != nil {
		t.Fatalf("sign SA token: %v", err)
	}
	return s
}

// saClaims is a faithful copy of a projected ServiceAccount token's claim
// set: nested `kubernetes.io` object, `sub: system:serviceaccount:<ns>:<sa>`,
// and the audience the producer projected the token with.
func saClaims(namespace, name string) jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss": testSAIssuer,
		"aud": []string{testSAAudience},
		"sub": "system:serviceaccount:" + namespace + ":" + name,
		"iat": now.Unix(),
		"exp": now.Add(1 * time.Hour).Unix(),
		"kubernetes.io": map[string]any{
			"namespace": namespace,
			"pod": map[string]any{
				"name": name + "-abc123",
				"uid":  "6a1b0e5a-0000-4000-8000-000000000001",
			},
			"serviceaccount": map[string]any{
				"name": name,
				"uid":  "6a1b0e5a-0000-4000-8000-000000000002",
			},
		},
	}
}

// saFixture wires a tenantauth extension to an in-process JWKS.
type saFixture struct {
	auth   *tenantAuth
	jwks   *fakeJWKS
	km     keyMaterial
	kid    string
	reader *sdkmetric.ManualReader
}

// newSAFixture builds a ServiceAccount-only gateway (unless the caller also
// sets static-PEM fields via `tweak`), with a real metrics SDK attached so
// every case can assert the rejection reason it produced.
func newSAFixture(t *testing.T, alg string, tweak func(c *Config)) saFixture {
	t.Helper()
	km := newKeys(t, alg)
	const kid = "cluster-key-1"
	fj := &fakeJWKS{doc: jwksDoc(t, map[string]keyMaterial{kid: km})}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	cfg := &Config{
		TenantClaim: defaultTenantClaim,
		ServiceAccount: &ServiceAccountConfig{
			Enabled:                true,
			Issuer:                 testSAIssuer,
			Audience:               testSAAudience,
			Algorithms:             []string{alg},
			NamespaceAsTenant:      true,
			JWKSMinRefreshInterval: -1, // no rate limit: tests drive refreshes directly
			fetchJWKS:              fj.fetch,
		},
	}
	if tweak != nil {
		tweak(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	a, err := newTenantAuth(cfg, mp)
	if err != nil {
		t.Fatalf("newTenantAuth: %v", err)
	}
	if err := a.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return saFixture{auth: a, jwks: fj, km: km, kid: kid, reader: reader}
}

// tenantOf drives one Authenticate call and returns the resolved tenant.
func (f saFixture) tenantOf(t *testing.T, token string) (string, error) {
	t.Helper()
	ctx, err := f.auth.Authenticate(context.Background(), bearer(token))
	if err != nil {
		return "", err
	}
	info := client.FromContext(ctx)
	if info.Auth == nil {
		t.Fatal("expected client.Info.Auth to be set")
	}
	tenant, _ := info.Auth.GetAttribute("tenant").(string)
	return tenant, nil
}

// mustReject asserts the token is refused AND counted under `reason` — a
// silent drop would be as bad as an accept.
func (f saFixture) mustReject(t *testing.T, token, reason string) {
	t.Helper()
	if _, err := f.auth.Authenticate(context.Background(), bearer(token)); err == nil {
		t.Fatal("expected the token to be rejected")
	}
	if got := rejectionCount(t, f.reader, reason); got != 1 {
		t.Fatalf("rejections{reason=%q} = %d, want 1", reason, got)
	}
}

// TestSA_ValidToken_TenantFromNamespace is the headline case: a projected
// ServiceAccount token verifies against the cluster JWKS and the tenant is
// the namespace claim the API server signed.
func TestSA_ValidToken_TenantFromNamespace(t *testing.T) {
	f := newSAFixture(t, "RS256", nil)
	token := signSAToken(t, f.km, f.kid, saClaims("agentloop", "collector"))
	tenant, err := f.tenantOf(t, token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if tenant != "agentloop" {
		t.Fatalf("tenant = %q, want %q (the namespace claim)", tenant, "agentloop")
	}
	if hasRejectionMetric(t, f.reader) {
		t.Fatal("expected NO rejection datapoints for a valid SA token")
	}
}

// TestSA_ValidToken_ES256AndEdDSA proves the JWKS path is not RSA-only: EC
// and OKP JWKs decode and verify too (a cluster may sign with either).
func TestSA_ValidToken_ES256AndEdDSA(t *testing.T) {
	for _, alg := range []string{"ES256", "EdDSA"} {
		t.Run(alg, func(t *testing.T) {
			f := newSAFixture(t, alg, nil)
			token := signSAToken(t, f.km, f.kid, saClaims("agentloop", "collector"))
			tenant, err := f.tenantOf(t, token)
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if tenant != "agentloop" {
				t.Fatalf("tenant = %q, want agentloop", tenant)
			}
		})
	}
}

// TestSA_TenantMap covers the arc-runners case from the issue: CI runners
// live in a namespace that is NOT their tenant, so an explicit
// ServiceAccount → tenant entry decides. The map wins over the namespace.
func TestSA_TenantMap(t *testing.T) {
	f := newSAFixture(t, "RS256", func(c *Config) {
		c.ServiceAccount.TenantMap = map[string]string{"arc-runners/gha-runner": "ci"}
	})
	token := signSAToken(t, f.km, f.kid, saClaims("arc-runners", "gha-runner"))
	tenant, err := f.tenantOf(t, token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if tenant != "ci" {
		t.Fatalf("tenant = %q, want %q (tenant_map must win over the namespace)", tenant, "ci")
	}
}

// TestSA_WrongAudience is the replay guard: a token the kubelet minted for
// the API server (or any other audience) must NOT be usable at the gateway,
// even though it is perfectly valid and signed by the cluster.
func TestSA_WrongAudience(t *testing.T) {
	f := newSAFixture(t, "RS256", nil)
	claims := saClaims("agentloop", "collector")
	claims["aud"] = []string{"https://kubernetes.default.svc.cluster.local"}
	f.mustReject(t, signSAToken(t, f.km, f.kid, claims), reasonBadAudience)
}

// TestSA_MissingAudience — no `aud` at all is as bad as the wrong one.
func TestSA_MissingAudience(t *testing.T) {
	f := newSAFixture(t, "RS256", nil)
	claims := saClaims("agentloop", "collector")
	delete(claims, "aud")
	f.mustReject(t, signSAToken(t, f.km, f.kid, claims), reasonBadAudience)
}

// TestSA_Expired — the kubelet refreshes projected tokens well before `exp`;
// a token that got past it anyway is refused, and counted, so a producer
// that stopped refreshing is loud rather than silently unauthenticated.
func TestSA_Expired(t *testing.T) {
	f := newSAFixture(t, "RS256", nil)
	claims := saClaims("agentloop", "collector")
	claims["iat"] = time.Now().Add(-2 * time.Hour).Unix()
	claims["exp"] = time.Now().Add(-1 * time.Hour).Unix()
	f.mustReject(t, signSAToken(t, f.km, f.kid, claims), reasonExpired)
}

// TestSA_WrongIssuer — a token from another cluster's API server (whose key
// somehow appeared in our JWKS) is still refused on `iss`.
func TestSA_WrongIssuer(t *testing.T) {
	f := newSAFixture(t, "RS256", nil)
	claims := saClaims("agentloop", "collector")
	claims["iss"] = "https://some-other-cluster.example"
	f.mustReject(t, signSAToken(t, f.km, f.kid, claims), reasonBadIssuer)
}

// TestSA_UnmappedServiceAccount_Rejected is the fail-closed core: the token
// is cryptographically perfect, but its identity maps to no tenant. It must
// be REJECTED, never defaulted onto some fallback tenant — a default here
// would let any pod in the cluster write into another tenant's rows.
func TestSA_UnmappedServiceAccount_Rejected(t *testing.T) {
	f := newSAFixture(t, "RS256", func(c *Config) {
		// namespace_as_tenant off: only the explicit map grants a tenant.
		c.ServiceAccount.NamespaceAsTenant = false
		c.ServiceAccount.TenantMap = map[string]string{"arc-runners/gha-runner": "ci"}
	})
	token := signSAToken(t, f.km, f.kid, saClaims("kube-system", "default"))
	f.mustReject(t, token, reasonUnmappedSA)
}

// TestSA_UnknownNamespace_Rejected covers the same fail-closed rule with the
// namespace allowlist: a pod in a namespace that is not a tenant namespace
// and has no map entry gets nothing.
func TestSA_UnknownNamespace_Rejected(t *testing.T) {
	f := newSAFixture(t, "RS256", func(c *Config) {
		c.ServiceAccount.Namespaces = []string{"agentloop", "sharedinbox"}
	})
	token := signSAToken(t, f.km, f.kid, saClaims("kube-system", "default"))
	f.mustReject(t, token, reasonUnmappedSA)
}

// TestSA_NoServiceAccountIdentity_Rejected: a token signed by the cluster
// key with the right audience but no ServiceAccount claims at all (e.g. a
// user token) resolves to no tenant, so it is refused.
func TestSA_NoServiceAccountIdentity_Rejected(t *testing.T) {
	f := newSAFixture(t, "RS256", nil)
	claims := jwt.MapClaims{
		"iss": testSAIssuer,
		"aud": []string{testSAAudience},
		"sub": "alice@example.com",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	f.mustReject(t, signSAToken(t, f.km, f.kid, claims), reasonUnmappedSA)
}

// TestSA_SmuggledTenantClaim proves the tenant is taken from the SIGNED
// ServiceAccount identity and nothing else. Here the producer is a pod in
// `sharedinbox` that stuffs `tenant: agentloop` into its own token payload —
// the claim is ignored, and it writes as `sharedinbox` or not at all. (The
// resource-attribute variant of this attack is overwritten one layer down;
// see tenanttagger's TestTraces_SpoofOverridden.)
func TestSA_SmuggledTenantClaim(t *testing.T) {
	f := newSAFixture(t, "RS256", nil)
	claims := saClaims("sharedinbox", "ci")
	claims["tenant"] = "agentloop" // attacker-chosen, must be ignored
	tenant, err := f.tenantOf(t, signSAToken(t, f.km, f.kid, claims))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if tenant != "sharedinbox" {
		t.Fatalf("tenant = %q, want %q — a `tenant` claim in the token body must never be honoured", tenant, "sharedinbox")
	}
}

// TestSA_UnknownKID_RejectedAfterRefresh: an unknown `kid` triggers exactly
// one JWKS refresh (the cluster may have rotated its key), and when the kid
// is still unknown afterwards the token is refused. An unknown key is never
// trusted.
func TestSA_UnknownKID_RejectedAfterRefresh(t *testing.T) {
	f := newSAFixture(t, "RS256", nil)
	before := f.jwks.count() // Start() warmed the cache: 1
	attacker := newKeys(t, "RS256")
	token := signSAToken(t, attacker, "attacker-key", saClaims("agentloop", "collector"))

	f.mustReject(t, token, reasonUnknownKID)

	if got := f.jwks.count() - before; got != 1 {
		t.Fatalf("JWKS fetches during the request = %d, want exactly 1 refresh attempt", got)
	}
}

// TestSA_KeyRotation_AcceptedAfterRefresh is the flip side: the cluster
// rotated its signing key, the new `kid` is unknown to the cache, the
// refresh picks it up and the token verifies. Rotation must not need a
// gateway restart.
func TestSA_KeyRotation_AcceptedAfterRefresh(t *testing.T) {
	f := newSAFixture(t, "RS256", nil)
	rotated := newKeys(t, "RS256")
	f.jwks.serve(jwksDoc(t, map[string]keyMaterial{"cluster-key-2": rotated}))

	token := signSAToken(t, rotated, "cluster-key-2", saClaims("agentloop", "collector"))
	tenant, err := f.tenantOf(t, token)
	if err != nil {
		t.Fatalf("Authenticate after key rotation: %v", err)
	}
	if tenant != "agentloop" {
		t.Fatalf("tenant = %q, want agentloop", tenant)
	}
}

// TestSA_RefreshRateLimited: a flood of tokens with bogus kids must not turn
// into a flood of API-server requests. Inside the refresh window (here: an
// hour, already consumed by Start's warmup) an unknown kid does NOT refetch
// — it is simply rejected. The refetch is an eventual-consistency mechanism
// for key rotation, not a per-request lookup an attacker can drive.
func TestSA_RefreshRateLimited(t *testing.T) {
	f := newSAFixture(t, "RS256", func(c *Config) {
		c.ServiceAccount.JWKSMinRefreshInterval = time.Hour
	})
	before := f.jwks.count()
	attacker := newKeys(t, "RS256")
	for range 5 {
		token := signSAToken(t, attacker, "attacker-key", saClaims("agentloop", "collector"))
		if _, err := f.auth.Authenticate(context.Background(), bearer(token)); err == nil {
			t.Fatal("expected unknown-kid token to be rejected")
		}
	}
	if got := f.jwks.count() - before; got != 0 {
		t.Fatalf("JWKS fetches for 5 unknown-kid tokens inside the refresh window = %d, want 0", got)
	}
}

// TestSA_BadSignature: right `kid`, wrong key. The signature is what binds
// the identity — knowing the kid buys an attacker nothing.
func TestSA_BadSignature(t *testing.T) {
	f := newSAFixture(t, "RS256", nil)
	attacker := newKeys(t, "RS256")
	token := signSAToken(t, attacker, f.kid, saClaims("agentloop", "collector"))
	f.mustReject(t, token, reasonBadSignature)
}

// TestSA_AlgNone pins the JWKS path against the classic unsigned-token
// attack, exactly as the static-PEM path is pinned.
func TestSA_AlgNone(t *testing.T) {
	f := newSAFixture(t, "RS256", nil)
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, saClaims("agentloop", "collector"))
	tok.Header["kid"] = f.kid
	s, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	if _, err := f.auth.Authenticate(context.Background(), bearer(s)); err == nil {
		t.Fatal("expected alg:none to be rejected on the JWKS path")
	}
}

// TestSA_AlgConfusion_HS256 is the attack the algorithm allowlist exists
// for: the JWKS is public, so an attacker can take the cluster's RSA public
// key and sign an HS256 token with it as the HMAC secret. HS* must never be
// accepted — the gateway holds public keys only.
func TestSA_AlgConfusion_HS256(t *testing.T) {
	f := newSAFixture(t, "RS256", nil)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, saClaims("agentloop", "collector"))
	tok.Header["kid"] = f.kid
	s, err := tok.SignedString(f.km.pubPEM) // public key as the HMAC secret
	if err != nil {
		t.Fatalf("sign HS256: %v", err)
	}
	if _, err := f.auth.Authenticate(context.Background(), bearer(s)); err == nil {
		t.Fatal("expected HS256 alg-confusion token to be rejected on the JWKS path")
	}
}

// TestSA_HMACAlgorithmRejectedByConfig closes the same door at config load:
// an operator cannot switch the JWKS path onto HS256 even deliberately.
func TestSA_HMACAlgorithmRejectedByConfig(t *testing.T) {
	for _, alg := range []string{"HS256", "none", "RS512"} {
		t.Run(alg, func(t *testing.T) {
			cfg := &Config{ServiceAccount: &ServiceAccountConfig{
				Enabled:           true,
				Issuer:            testSAIssuer,
				Algorithms:        []string{alg},
				NamespaceAsTenant: true,
			}}
			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected algorithm %q to be rejected", alg)
			}
		})
	}
}

// TestSA_AlgOutsideAllowedSet: the token's algorithm is asymmetric and
// otherwise fine, but the gateway pinned a different one. Rejected.
func TestSA_AlgOutsideAllowedSet(t *testing.T) {
	f := newSAFixture(t, "RS256", nil) // gateway accepts RS256 only
	ed := newKeys(t, "EdDSA")
	f.jwks.serve(jwksDoc(t, map[string]keyMaterial{"cluster-key-1": ed}))
	token := signSAToken(t, ed, "cluster-key-1", saClaims("agentloop", "collector"))
	if _, err := f.auth.Authenticate(context.Background(), bearer(token)); err == nil {
		t.Fatal("expected an EdDSA token to be rejected by an RS256-pinned gateway")
	}
}

// TestSA_JWKSUnavailable: the API server is unreachable, so no SA token can
// be verified. Fail-closed — and counted under its own reason so the
// operator can tell "the gateway cannot fetch keys" apart from "producers
// are sending bad tokens".
func TestSA_JWKSUnavailable(t *testing.T) {
	f := newSAFixture(t, "RS256", nil)
	// Wipe the warm cache by rotating to an empty JWKS, then break the
	// endpoint: the next unknown-kid lookup has nothing to fall back on.
	f.jwks.fail(errors.New("connection refused"))
	attacker := newKeys(t, "RS256")
	token := signSAToken(t, attacker, "some-other-kid", saClaims("agentloop", "collector"))
	f.mustReject(t, token, reasonJWKSUnavailable)
}

// TestSA_LegacyAndSubjectIdentity: the namespace/name are read from the
// nested projected-token claims, the legacy flat claims, or `sub` — all
// three are signed, so all three are safe sources.
func TestSA_LegacyAndSubjectIdentity(t *testing.T) {
	cases := map[string]func(jwt.MapClaims){
		"legacy_flat_claims": func(c jwt.MapClaims) {
			delete(c, "kubernetes.io")
			delete(c, "sub")
			c["kubernetes.io/serviceaccount/namespace"] = "agentloop"
			c["kubernetes.io/serviceaccount/service-account.name"] = "collector"
		},
		"subject_only": func(c jwt.MapClaims) {
			delete(c, "kubernetes.io")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSAFixture(t, "RS256", nil)
			claims := saClaims("agentloop", "collector")
			mutate(claims)
			tenant, err := f.tenantOf(t, signSAToken(t, f.km, f.kid, claims))
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if tenant != "agentloop" {
				t.Fatalf("tenant = %q, want agentloop", tenant)
			}
		})
	}
}

// TestSA_BothSourcesEnabled: with the SA source AND the static-PEM source
// configured, a projected token and a minted token both work, and each is
// still held to its own contract. This is the migration mode — in-cluster
// producers move to projected tokens while an external producer keeps its
// minted JWT.
func TestSA_BothSourcesEnabled(t *testing.T) {
	static := newKeys(t, "EdDSA")
	f := newSAFixture(t, "RS256", func(c *Config) {
		c.Issuer = "otelhouse-mint"
		c.Audience = "otelhouse-gateway"
		c.Algorithm = "EdDSA"
		c.PublicKeyPEM = string(static.pubPEM)
	})

	t.Run("sa_token", func(t *testing.T) {
		tenant, err := f.tenantOf(t, signSAToken(t, f.km, f.kid, saClaims("agentloop", "collector")))
		if err != nil {
			t.Fatalf("Authenticate SA token: %v", err)
		}
		if tenant != "agentloop" {
			t.Fatalf("tenant = %q, want agentloop", tenant)
		}
	})
	t.Run("static_pem_token_falls_back", func(t *testing.T) {
		tenant, err := f.tenantOf(t, signToken(t, static, baseClaims()))
		if err != nil {
			t.Fatalf("Authenticate minted token: %v", err)
		}
		if tenant != "alice" {
			t.Fatalf("tenant = %q, want alice (static-PEM fallback)", tenant)
		}
	})
	t.Run("garbage_rejected_by_both", func(t *testing.T) {
		if _, err := f.auth.Authenticate(context.Background(), bearer("not-a-jwt")); err == nil {
			t.Fatal("expected a garbage token to be rejected by both sources")
		}
	})
	t.Run("expired_sa_token_not_rescued_by_static_source", func(t *testing.T) {
		claims := saClaims("agentloop", "collector")
		claims["exp"] = time.Now().Add(-time.Hour).Unix()
		if _, err := f.auth.Authenticate(context.Background(), bearer(signSAToken(t, f.km, f.kid, claims))); err == nil {
			t.Fatal("expected an expired SA token to stay rejected with both sources enabled")
		}
	})
}

// TestSA_JWKSOverHTTPS exercises the real fetcher — TLS against a CA bundle
// on disk plus a bearer token file, the way the gateway talks to the
// in-cluster API server. The server is a loopback httptest server, so there
// is still no cluster and no outbound network.
func TestSA_JWKSOverHTTPS(t *testing.T) {
	km := newKeys(t, "RS256")
	const kid = "cluster-key-1"
	doc := jwksDoc(t, map[string]keyMaterial{kid: km})

	var gotAuth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	}))
	defer srv.Close()

	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.crt")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("gateway-sa-token\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	cfg := &Config{ServiceAccount: &ServiceAccountConfig{
		Enabled:           true,
		Issuer:            testSAIssuer,
		Audience:          testSAAudience,
		JWKSURL:           srv.URL + "/openid/v1/jwks",
		CAFile:            caFile,
		TokenFile:         tokenFile,
		Algorithms:        []string{"RS256"},
		NamespaceAsTenant: true,
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	a, err := newTenantAuth(cfg, nil)
	if err != nil {
		t.Fatalf("newTenantAuth: %v", err)
	}
	if err := a.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, err := a.Authenticate(context.Background(), bearer(signSAToken(t, km, kid, saClaims("agentloop", "collector"))))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if tenant, _ := client.FromContext(ctx).Auth.GetAttribute("tenant").(string); tenant != "agentloop" {
		t.Fatalf("tenant = %q, want agentloop", tenant)
	}
	if gotAuth != "Bearer gateway-sa-token" {
		t.Fatalf("JWKS request Authorization = %q, want the gateway's own SA token", gotAuth)
	}
}

// selfSignedCA returns a PEM-encoded certificate that signed nothing the
// tests serve — a CA bundle for a completely different authority.
func selfSignedCA(t *testing.T) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "not-the-cluster-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestSA_JWKSOverHTTPS_UntrustedCA proves the fetcher actually verifies the
// JWKS server's certificate: with a CA bundle from a different authority, no
// key is ever learned and every token is refused. A gateway that fetched its
// trust anchors over an unauthenticated channel would be trivially
// MITM-able into trusting an attacker's signing key.
func TestSA_JWKSOverHTTPS_UntrustedCA(t *testing.T) {
	km := newKeys(t, "RS256")
	const kid = "cluster-key-1"
	doc := jwksDoc(t, map[string]keyMaterial{kid: km})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(doc)
	}))
	defer srv.Close()

	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caFile, selfSignedCA(t), 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}

	cfg := &Config{ServiceAccount: &ServiceAccountConfig{
		Enabled:           true,
		Issuer:            testSAIssuer,
		Audience:          testSAAudience,
		JWKSURL:           srv.URL + "/openid/v1/jwks",
		CAFile:            caFile,
		TokenFile:         noTokenFile,
		Algorithms:        []string{"RS256"},
		NamespaceAsTenant: true,
	}}
	a, err := newTenantAuth(cfg, nil)
	if err != nil {
		t.Fatalf("newTenantAuth: %v", err)
	}
	// Start's warmup fails (untrusted CA) but must not block startup: an
	// unreachable API server may not crash-loop the gateway, and an empty key
	// cache rejects tokens rather than accepting them.
	if err := a.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := a.Authenticate(context.Background(), bearer(signSAToken(t, km, kid, saClaims("agentloop", "collector")))); err == nil {
		t.Fatal("expected every token to be rejected when the JWKS server's certificate is untrusted")
	}
}

// TestSA_ConfigValidate walks the ServiceAccount config guardrails. An
// operator mistake must be a startup error, not a gateway that quietly
// trusts the wrong thing.
func TestSA_ConfigValidate(t *testing.T) {
	valid := func() *Config {
		return &Config{ServiceAccount: &ServiceAccountConfig{
			Enabled:           true,
			Issuer:            testSAIssuer,
			Audience:          testSAAudience,
			NamespaceAsTenant: true,
		}}
	}
	t.Run("ok_serviceaccount_only", func(t *testing.T) {
		if err := valid().Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
	t.Run("missing_issuer", func(t *testing.T) {
		c := valid()
		c.ServiceAccount.Issuer = ""
		if err := c.Validate(); err == nil {
			t.Fatal("expected error when serviceaccount.issuer is unset")
		}
	})
	t.Run("no_identity_source_at_all", func(t *testing.T) {
		c := valid()
		c.ServiceAccount.Enabled = false
		if err := c.Validate(); err == nil {
			t.Fatal("expected error when neither identity source is enabled")
		}
	})
	t.Run("no_tenant_source", func(t *testing.T) {
		c := valid()
		c.ServiceAccount.NamespaceAsTenant = false // and no tenant_map
		if err := c.Validate(); err == nil {
			t.Fatal("expected error when no tenant source is configured")
		}
	})
	t.Run("tenant_map_key_shape", func(t *testing.T) {
		c := valid()
		c.ServiceAccount.TenantMap = map[string]string{"gha-runner": "ci"} // no namespace
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for a tenant_map key without <namespace>/<serviceaccount>")
		}
	})
	t.Run("tenant_map_empty_tenant", func(t *testing.T) {
		c := valid()
		c.ServiceAccount.TenantMap = map[string]string{"arc-runners/gha-runner": " "}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error for an empty tenant in tenant_map")
		}
	})
	t.Run("audience_defaults_to_gateway", func(t *testing.T) {
		c := valid()
		c.ServiceAccount.Audience = ""
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if got := c.ServiceAccount.audience(); got != defaultSAAudience {
			t.Fatalf("audience() = %q, want %q", got, defaultSAAudience)
		}
	})
}

// TestSA_FactoryDefaults locks in the defaults an operator inherits: the
// source is OFF unless enabled, but when enabled it already points at the
// in-cluster API server, requires the gateway audience, pins asymmetric
// algorithms and treats the namespace as the tenant.
func TestSA_FactoryDefaults(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	sa := cfg.ServiceAccount
	if sa == nil {
		t.Fatal("expected a ServiceAccount config block in the defaults")
	}
	if sa.Enabled {
		t.Fatal("the ServiceAccount source must be off until an operator enables it")
	}
	if sa.Audience != defaultSAAudience {
		t.Fatalf("default audience = %q, want %q", sa.Audience, defaultSAAudience)
	}
	if sa.JWKSURL != defaultSAJWKSURL {
		t.Fatalf("default jwks_url = %q, want %q", sa.JWKSURL, defaultSAJWKSURL)
	}
	if !sa.NamespaceAsTenant {
		t.Fatal("namespace_as_tenant must default to true")
	}
	for _, alg := range sa.Algorithms {
		if strings.HasPrefix(alg, "HS") || alg == "none" {
			t.Fatalf("default algorithms must never contain %q", alg)
		}
	}
	// The default config alone must not start a gateway: no identity source.
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected the bare default config to fail validation")
	}
}

// TestJWKS_ParseIgnoresUnusableKeys: a JWKS with an encryption key or an
// unknown key type must not poison the usable ones — and must not be
// silently treated as "no keys" when a good key is present.
func TestJWKS_ParseIgnoresUnusableKeys(t *testing.T) {
	km := newKeys(t, "RS256")
	good := jwkFor(t, "good", km)
	doc, err := json.Marshal(map[string]any{"keys": []any{
		map[string]any{"kty": "oct", "kid": "symmetric", "k": "c2VjcmV0"}, // never usable here
		map[string]any{"kty": "RSA", "kid": "broken", "n": "!!!", "e": "AQAB"},
		good,
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	keys, err := parseJWKS(doc)
	if err != nil {
		t.Fatalf("parseJWKS: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("parsed %d keys, want 1 (the RSA signing key)", len(keys))
	}
	if _, ok := keys["good"]; !ok {
		t.Fatalf("expected the usable key to be kept, got %v", keys)
	}
}
