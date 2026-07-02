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
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"go.opentelemetry.io/collector/client"
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
	a, err := newTenantAuth(cfg)
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
