// Package tenantauth is an OpenTelemetry Collector extension that
// authenticates OTLP requests and resolves the tenant they are allowed to
// write as. Downstream processors read the tenant via client.FromContext
// and stamp it on every record — so the tenant a batch ends up tagged with
// is derived from a claim in a *verified* token that the client cannot
// forge, and any client-supplied tenant on the payload is overridden.
//
// # Why the tenant must come from a verified token
//
// Read isolation in the shared ClickHouse is enforced by row policies keyed
// on ResourceAttributes['tenant']. If a producer could pick its own tenant
// label — by setting a resource attribute, or by holding a shared secret
// that names someone else — it could write rows into any other tenant's
// view. The tenant label is therefore only ever taken from a cryptographic
// claim the gateway verified itself, and never defaulted when verification
// fails: an unresolvable batch is rejected, not written unlabelled.
//
// # Identity sources
//
// The extension supports two key sources; either or both may be enabled.
//
//  1. Kubernetes ServiceAccount tokens (preferred for in-cluster producers).
//     A projected ServiceAccount token is an RS256 JWT signed by the API
//     server. The extension verifies it against the cluster's JWKS
//     (/openid/v1/jwks, cached and refreshed on an unknown "kid"), requires
//     the token to have been projected with the gateway's audience
//     (audience: otelhouse-gateway), and derives the tenant from the
//     verified ServiceAccount identity: the namespace claim
//     (kubernetes.io/serviceaccount/namespace) by default, or an explicit
//     <namespace>/<serviceaccount> → tenant map for producers whose
//     namespace is not their tenant (e.g. CI runners in "arc-runners").
//     Identities that are neither mapped nor allowed as a namespace tenant
//     are REJECTED — never defaulted. The kubelet rotates these tokens, so
//     there is no secret to mint, distribute or expire.
//
//  2. Static-PEM minted JWTs (for any producer that is not an in-cluster
//     pod). The extension holds only the PUBLIC key; tokens are minted
//     elsewhere. One public key verifies every tenant's token, and the
//     tenant comes from the "tenant" claim.
//
// When both are enabled, a request is tried against the ServiceAccount
// verifier first and falls back to the static-PEM verifier.
//
// See ../../../docs/jwt-contract.md for the wire contract (claims, alg,
// iss/aud) both identity sources must satisfy.
package tenantauth

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Config configures the tenantauth extension.
//
// The top-level fields configure the static-PEM identity source (the
// original, hand-minted-JWT contract). The ServiceAccount block configures
// the Kubernetes ServiceAccount-token source. At least one must be enabled.
type Config struct {
	// Issuer is the exact string a static-PEM JWT's `iss` claim must equal.
	// Empty tokens or a mismatch fail validation. Only applies to the
	// static-PEM source; the ServiceAccount source has its own issuer.
	Issuer string `mapstructure:"issuer"`

	// Audience is the exact string a static-PEM JWT's `aud` claim must
	// equal. It identifies THIS gateway, so a token minted for a different
	// gateway cannot be replayed here.
	Audience string `mapstructure:"audience"`

	// Algorithm pins the JWS signing algorithm of the static-PEM source.
	// The extension refuses to accept a token whose `alg` header does not
	// equal this value — blocking `alg:none` and HS↔RS/ES/EdDSA
	// algorithm-confusion attacks.
	//
	// Supported: EdDSA (preferred), ES256, RS256.
	Algorithm string `mapstructure:"algorithm"`

	// PublicKeyPEM is the verification key in PEM form. Either this or
	// PublicKeyFile enables the static-PEM source — never a private key.
	PublicKeyPEM string `mapstructure:"public_key_pem"`

	// PublicKeyFile is a path to a PEM-encoded public key. Preferred when
	// the key is mounted from a Kubernetes Secret / ConfigMap.
	PublicKeyFile string `mapstructure:"public_key_file"`

	// TenantClaim is the JWT claim name the static-PEM source reads to
	// resolve the tenant. Defaults to "tenant" — matches the mint contract.
	// It is also the attribute name under which the resolved tenant is
	// exposed on client.Info.Auth for BOTH sources, so it must match
	// tenanttagger's `auth_attribute`.
	TenantClaim string `mapstructure:"tenant_claim"`

	// ServiceAccount configures verification of Kubernetes projected
	// ServiceAccount tokens against the cluster's JWKS. Nil / not enabled
	// means the source is off and only the static-PEM path is used.
	ServiceAccount *ServiceAccountConfig `mapstructure:"serviceaccount"`
}

// ServiceAccountConfig configures the Kubernetes ServiceAccount-token
// identity source: verification against the cluster JWKS plus the mapping
// from a verified ServiceAccount identity onto a tenant.
type ServiceAccountConfig struct {
	// Enabled turns the ServiceAccount source on.
	Enabled bool `mapstructure:"enabled"`

	// Issuer is the exact string a ServiceAccount token's `iss` claim must
	// equal — the cluster's OIDC issuer (`kubectl get --raw
	// /.well-known/openid-configuration | jq .issuer`, typically
	// "https://kubernetes.default.svc.cluster.local"). Required: an
	// implicit default would be a silent security-relevant guess.
	Issuer string `mapstructure:"issuer"`

	// Audience is the exact audience producers must project their token
	// with (`serviceAccountToken.audience`). A token minted for the API
	// server (the default audience) must NOT be replayable at the gateway,
	// so this is always enforced. Defaults to "otelhouse-gateway".
	Audience string `mapstructure:"audience"`

	// JWKSURL is the cluster's JWKS endpoint. Defaults to the in-cluster
	// API server address.
	JWKSURL string `mapstructure:"jwks_url"`

	// CAFile is the CA bundle used to reach JWKSURL. Defaults to the
	// in-cluster ServiceAccount CA. Only needs overriding outside a cluster
	// (tests).
	CAFile string `mapstructure:"ca_file"`

	// TokenFile is the bearer token presented to the API server when
	// fetching the JWKS. Some clusters allow anonymous access to
	// /openid/v1/jwks, others do not; defaults to the gateway's own
	// projected ServiceAccount token. It is re-read on every fetch, so
	// kubelet rotation is picked up. Set to "-" to send no bearer token.
	TokenFile string `mapstructure:"token_file"`

	// Algorithms is the closed set of signing algorithms accepted for
	// ServiceAccount tokens. Defaults to RS256, ES256, EdDSA (Kubernetes
	// signs with RS256 today; ES256 clusters exist). HS* and `none` are
	// rejected at config-validation time — the gateway only ever holds
	// public keys.
	Algorithms []string `mapstructure:"algorithms"`

	// JWKSMinRefreshInterval rate-limits JWKS refetches triggered by an
	// unknown `kid`, so a flood of bogus tokens cannot turn into a flood of
	// API-server requests. Defaults to 30s; a negative value disables the
	// rate limit (tests only).
	JWKSMinRefreshInterval time.Duration `mapstructure:"jwks_min_refresh_interval"`

	// NamespaceAsTenant derives the tenant from the verified
	// `kubernetes.io/serviceaccount/namespace` claim when the identity is
	// not in TenantMap. This is the 1:1 namespace-per-tenant model.
	// Defaults to true (set by the factory).
	NamespaceAsTenant bool `mapstructure:"namespace_as_tenant"`

	// Namespaces optionally restricts which namespaces NamespaceAsTenant
	// accepts. Empty means "any namespace may write as itself". A namespace
	// that is not listed here and has no TenantMap entry is REJECTED.
	Namespaces []string `mapstructure:"namespaces"`

	// TenantMap maps an explicit ServiceAccount identity to a tenant, for
	// producers whose namespace is not their tenant. Keys are
	// "<namespace>/<serviceaccount>", e.g.
	//
	//	tenant_map:
	//	  arc-runners/gha-runner: ci
	//
	// A TenantMap entry always wins over NamespaceAsTenant.
	TenantMap map[string]string `mapstructure:"tenant_map"`

	// fetchJWKS overrides the HTTP fetch of the JWKS document. Unexported:
	// it exists so unit tests can serve a JWKS in-process without a cluster.
	// Production always goes through the HTTP client built from
	// JWKSURL/CAFile/TokenFile.
	fetchJWKS jwksFetcher
}

const (
	defaultTenantClaim  = "tenant"
	defaultSAAudience   = "otelhouse-gateway"
	defaultSAJWKSURL    = "https://kubernetes.default.svc/openid/v1/jwks"
	defaultSACAFile     = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	defaultSATokenFile  = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // a mount path, not a credential
	defaultSAMinRefresh = 30 * time.Second

	// noTokenFile disables sending a bearer token with the JWKS fetch.
	noTokenFile = "-"
)

// defaultSAAlgorithms is what Kubernetes clusters actually sign with, minus
// anything symmetric.
func defaultSAAlgorithms() []string { return []string{"RS256", "ES256", "EdDSA"} }

// supportedAlgorithms is the closed set of asymmetric signing algorithms
// the extension accepts. HMAC (HS*) is deliberately absent: the gateway
// only holds public keys, so an HMAC token could never be legitimately
// verified here, and allowing HS* opens the well-known alg-confusion
// downgrade where an attacker signs an HS256 token using the RSA public
// key as the HMAC secret.
var supportedAlgorithms = map[string]struct{}{
	"EdDSA": {},
	"ES256": {},
	"RS256": {},
}

// staticEnabled reports whether the static-PEM identity source is on. It is
// keyed off key material rather than a flag so pre-existing configs keep
// working verbatim.
func (c *Config) staticEnabled() bool {
	return c.PublicKeyPEM != "" || c.PublicKeyFile != ""
}

// saEnabled reports whether the Kubernetes ServiceAccount source is on.
func (c *Config) saEnabled() bool {
	return c.ServiceAccount != nil && c.ServiceAccount.Enabled
}

// Validate is invoked by the collector's confmap during config load.
func (c *Config) Validate() error {
	if c.PublicKeyPEM != "" && c.PublicKeyFile != "" {
		return errors.New("tenantauth: public_key_pem and public_key_file are mutually exclusive")
	}
	if !c.staticEnabled() && !c.saEnabled() {
		return errors.New("tenantauth: no identity source configured: set serviceaccount.enabled (Kubernetes ServiceAccount tokens) and/or one of public_key_pem / public_key_file (static-PEM JWTs)")
	}
	if c.staticEnabled() {
		if err := c.validateStatic(); err != nil {
			return err
		}
	}
	if c.saEnabled() {
		if err := c.ServiceAccount.validate(); err != nil {
			return err
		}
	}
	return nil
}

// validateStatic checks the static-PEM source's own required fields. They
// are only required when that source is enabled, so a ServiceAccount-only
// gateway needs no PEM-era config at all.
func (c *Config) validateStatic() error {
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
	return nil
}

// validate checks the ServiceAccount source. Anything that could make the
// gateway accept an identity it should not — an unpinned algorithm, a
// missing audience, no tenant source — is a startup error, never a runtime
// surprise.
func (s *ServiceAccountConfig) validate() error {
	if s.Issuer == "" {
		return errors.New("tenantauth: serviceaccount.issuer must be set (the cluster's OIDC issuer, e.g. https://kubernetes.default.svc.cluster.local)")
	}
	if s.audience() == "" {
		return errors.New("tenantauth: serviceaccount.audience must be set")
	}
	if s.jwksURL() == "" {
		return errors.New("tenantauth: serviceaccount.jwks_url must be set")
	}
	for _, alg := range s.algorithms() {
		if _, ok := supportedAlgorithms[alg]; !ok {
			return fmt.Errorf("tenantauth: serviceaccount: unsupported algorithm %q (want one of EdDSA, ES256, RS256)", alg)
		}
	}
	if !s.NamespaceAsTenant && len(s.TenantMap) == 0 {
		return errors.New("tenantauth: serviceaccount: no tenant source: enable namespace_as_tenant and/or set tenant_map")
	}
	for k, v := range s.TenantMap {
		ns, name, ok := strings.Cut(k, "/")
		if !ok || strings.TrimSpace(ns) == "" || strings.TrimSpace(name) == "" {
			return fmt.Errorf("tenantauth: serviceaccount.tenant_map key %q must have the form <namespace>/<serviceaccount>", k)
		}
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("tenantauth: serviceaccount.tenant_map[%q] is empty; an identity maps to a non-empty tenant or is left out (unmapped identities are rejected)", k)
		}
	}
	for _, ns := range s.Namespaces {
		if strings.TrimSpace(ns) == "" {
			return errors.New("tenantauth: serviceaccount.namespaces contains an empty entry")
		}
	}
	return nil
}

func (s *ServiceAccountConfig) audience() string {
	if s.Audience == "" {
		return defaultSAAudience
	}
	return s.Audience
}

func (s *ServiceAccountConfig) jwksURL() string {
	if s.JWKSURL == "" {
		return defaultSAJWKSURL
	}
	return s.JWKSURL
}

func (s *ServiceAccountConfig) caFile() string {
	if s.CAFile == "" {
		return defaultSACAFile
	}
	return s.CAFile
}

func (s *ServiceAccountConfig) tokenFile() string {
	if s.TokenFile == "" {
		return defaultSATokenFile
	}
	return s.TokenFile
}

func (s *ServiceAccountConfig) algorithms() []string {
	if len(s.Algorithms) == 0 {
		return defaultSAAlgorithms()
	}
	return s.Algorithms
}

func (s *ServiceAccountConfig) minRefreshInterval() time.Duration {
	switch {
	case s.JWKSMinRefreshInterval == 0:
		return defaultSAMinRefresh
	case s.JWKSMinRefreshInterval < 0:
		return 0
	default:
		return s.JWKSMinRefreshInterval
	}
}

// loadPEM returns the PEM bytes the config points at, whether inline or on
// disk. Called at extension construction so a bad path fails startup, not
// the first request.
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
