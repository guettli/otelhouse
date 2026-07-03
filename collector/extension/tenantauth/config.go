// Package tenantauth is an OpenTelemetry Collector extension that
// authenticates OTLP requests with per-tenant signed JWTs and resolves the
// bound tenant into the auth context. Downstream processors read the tenant
// via client.FromContext and stamp it on records — so the tenant a batch
// ends up tagged with is derived from a signed claim the client cannot
// forge, and any client-supplied tenant on the payload is overridden.
//
// See ../../../docs/jwt-contract.md for the wire contract (claims, alg,
// iss/aud) the gitops mint job must produce.
package tenantauth

import (
	"errors"
	"fmt"
	"os"

	"github.com/golang-jwt/jwt/v5"
)

// Config configures the tenantauth extension.
//
// The extension only ever holds a PUBLIC key; JWTs are minted elsewhere
// (per-tenant secret in gitops). One public key verifies every tenant's
// token — adding a tenant is a pure gitops action, not a gateway change.
type Config struct {
	// Issuer is the exact string the JWT `iss` claim must equal. Empty
	// tokens or a mismatch fail validation.
	Issuer string `mapstructure:"issuer"`

	// Audience is the exact string the JWT `aud` claim must equal. It
	// identifies THIS gateway, so a token minted for a different gateway
	// cannot be replayed here.
	Audience string `mapstructure:"audience"`

	// Algorithm pins the JWS signing algorithm. The extension refuses to
	// accept a token whose `alg` header does not equal this value —
	// blocking `alg:none` and HS↔RS/ES/EdDSA algorithm-confusion attacks.
	//
	// Supported: EdDSA (preferred), ES256, RS256.
	Algorithm string `mapstructure:"algorithm"`

	// PublicKeyPEM is the verification key in PEM form. Either this or
	// PublicKeyFile must be set — never a private key.
	PublicKeyPEM string `mapstructure:"public_key_pem"`

	// PublicKeyFile is a path to a PEM-encoded public key. Preferred when
	// the key is mounted from a Kubernetes Secret / ConfigMap.
	PublicKeyFile string `mapstructure:"public_key_file"`

	// TenantClaim is the JWT claim name the extension reads to resolve the
	// tenant. Defaults to "tenant" — matches the gitops mint contract.
	TenantClaim string `mapstructure:"tenant_claim"`
}

const defaultTenantClaim = "tenant"

// supportedAlgorithms is the closed set of asymmetric signing algorithms
// the extension accepts. HMAC (HS*) is deliberately absent: the gateway
// only holds a public key, so an HMAC token could never be legitimately
// verified here, and allowing HS* opens the well-known alg-confusion
// downgrade where an attacker signs an HS256 token using the RSA public
// key as the HMAC secret.
var supportedAlgorithms = map[string]struct{}{
	"EdDSA": {},
	"ES256": {},
	"RS256": {},
}

// Validate is invoked by the collector's confmap during config load.
func (c *Config) Validate() error {
	if c.Issuer == "" {
		return errors.New("tenantauth: issuer must be set")
	}
	if c.Audience == "" {
		return errors.New("tenantauth: audience must be set")
	}
	if c.Algorithm == "" {
		return errors.New("tenantauth: algorithm must be set")
	}
	if _, ok := supportedAlgorithms[c.Algorithm]; !ok {
		return fmt.Errorf("tenantauth: unsupported algorithm %q (want one of EdDSA, ES256, RS256)", c.Algorithm)
	}
	if c.PublicKeyPEM == "" && c.PublicKeyFile == "" {
		return errors.New("tenantauth: one of public_key_pem or public_key_file must be set")
	}
	if c.PublicKeyPEM != "" && c.PublicKeyFile != "" {
		return errors.New("tenantauth: public_key_pem and public_key_file are mutually exclusive")
	}
	return nil
}

// loadPEM returns the PEM bytes the config points at, whether inline or on
// disk. Called at extension Start() so a bad path fails startup, not the
// first request.
func (c *Config) loadPEM() ([]byte, error) {
	if c.PublicKeyPEM != "" {
		return []byte(c.PublicKeyPEM), nil
	}
	data, err := os.ReadFile(c.PublicKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read public_key_file %q: %w", c.PublicKeyFile, err)
	}
	return data, nil
}

// parsePublicKey turns a PEM-encoded key into the concrete key type
// golang-jwt expects for the pinned algorithm.
func parsePublicKey(pemBytes []byte, alg string) (any, error) {
	switch alg {
	case "EdDSA":
		return jwt.ParseEdPublicKeyFromPEM(pemBytes)
	case "ES256":
		return jwt.ParseECPublicKeyFromPEM(pemBytes)
	case "RS256":
		return jwt.ParseRSAPublicKeyFromPEM(pemBytes)
	default:
		return nil, fmt.Errorf("tenantauth: unsupported algorithm %q", alg)
	}
}

func (c *Config) tenantClaim() string {
	if c.TenantClaim == "" {
		return defaultTenantClaim
	}
	return c.TenantClaim
}
