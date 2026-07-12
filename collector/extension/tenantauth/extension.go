package tenantauth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"go.opentelemetry.io/collector/client"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension/auth"
	"go.opentelemetry.io/otel/metric"
)

// rejection is a verifier's "no", carrying the operator-visible reason
// label alongside the error returned to the client.
type rejection struct {
	reason string
	err    error
	// recognized reports that the token claimed THIS verifier's issuer, so
	// this verifier's reason is the interesting one when several verifiers
	// are enabled and all of them said no.
	recognized bool
}

// verifier is one identity source: it turns a bearer token into the tenant
// the bearer may write as, or a rejection. Implementations must never
// return an empty tenant with a nil rejection.
type verifier interface {
	name() string
	issuer() string
	verify(ctx context.Context, token string) (string, *rejection)
}

// tenantAuth is the auth.Server implementation. It verifies the request's
// Bearer token against every configured identity source (Kubernetes
// ServiceAccount tokens first, then static-PEM minted JWTs) and puts the
// resolved tenant into the request's client.Info so the tenanttagger
// processor can stamp it on every record.
//
// It is fail-closed by construction: the only way out with a tenant is a
// verifier returning one from a claim it cryptographically verified.
type tenantAuth struct {
	cfg       *Config
	verifiers []verifier
	claim     string
	metrics   *authMetrics
}

// newTenantAuth is exposed to the factory. Key material is parsed once at
// construction time so runtime auth calls are pure verifies. The
// MeterProvider is supplied by the collector via extension.Settings; a nil
// provider is treated as no-op so bare unit tests do not need to wire an
// SDK through.
func newTenantAuth(cfg *Config, mp metric.MeterProvider) (*tenantAuth, error) {
	m, err := newAuthMetrics(mp)
	if err != nil {
		return nil, err
	}
	t := &tenantAuth{cfg: cfg, claim: cfg.tenantClaim(), metrics: m}

	// Order matters: the ServiceAccount source is tried first, because
	// in-cluster producers are the default and their tokens rotate by
	// themselves. The static-PEM source is the documented fallback for
	// producers that are not in-cluster pods.
	if cfg.saEnabled() {
		sa, err := newServiceAccountVerifier(cfg.ServiceAccount)
		if err != nil {
			return nil, err
		}
		t.verifiers = append(t.verifiers, sa)
	}
	if cfg.staticEnabled() {
		st, err := newStaticVerifier(cfg)
		if err != nil {
			return nil, err
		}
		t.verifiers = append(t.verifiers, st)
	}
	if len(t.verifiers) == 0 {
		return nil, errors.New("tenantauth: no identity source configured")
	}
	return t, nil
}

// Start satisfies component.Component. Key material was parsed in
// newTenantAuth, so a bad key surfaces at extension construction. The JWKS
// is warmed here, best effort: an API server that is not reachable yet must
// not crash-loop the gateway, and an empty key cache rejects tokens rather
// than accepting them, so a failed warmup stays fail-closed.
func (t *tenantAuth) Start(ctx context.Context, _ component.Host) error {
	for _, v := range t.verifiers {
		if sa, ok := v.(*serviceAccountVerifier); ok {
			_ = sa.warmup(ctx)
		}
	}
	return nil
}

// Shutdown satisfies component.Component.
func (t *tenantAuth) Shutdown(_ context.Context) error { return nil }

// Authenticate is the hot path. It runs on every OTLP request, so it
// must be allocation-light and fail-fast on any invalid input.
//
// Contract on success: the returned context carries a client.Info whose
// Auth.GetAttribute("tenant") returns the resolved tenant. Downstream
// processors read exactly that — never a payload attribute.
//
// Contract on failure: a non-nil error and the ORIGINAL context, plus one
// increment of the rejections counter. The collector's auth interceptor
// maps a non-nil error to 401. No rejection is silent.
func (t *tenantAuth) Authenticate(ctx context.Context, sources map[string][]string) (context.Context, error) {
	token, err := extractBearer(sources)
	if err != nil {
		t.metrics.recordRejection(ctx, reasonMalformed)
		return ctx, err
	}

	var worst *rejection
	for _, v := range t.verifiers {
		tenant, rej := v.verify(ctx, token)
		if rej == nil {
			info := client.FromContext(ctx)
			info.Auth = tenantAuthData{tenant: tenant, claim: t.claim}
			return client.NewContext(ctx, info), nil
		}
		// Report the reason of the verifier the token actually belongs to
		// (its issuer matched). Otherwise the first rejection stands, so a
		// gateway with both sources enabled still reports something useful.
		if worst == nil || (!worst.recognized && rej.recognized) {
			worst = rej
		}
	}
	t.metrics.recordRejection(ctx, worst.reason)
	return ctx, worst.err
}

// staticVerifier is the original identity source: a JWT minted elsewhere
// and verified against a static public key, with the tenant carried in a
// configurable claim. It stays the documented answer for any producer that
// is not an in-cluster pod.
type staticVerifier struct {
	cfg    *Config
	key    any
	parser *jwt.Parser
	claim  string
}

func newStaticVerifier(cfg *Config) (*staticVerifier, error) {
	pem, err := cfg.loadPEM()
	if err != nil {
		return nil, err
	}
	key, err := parsePublicKey(pem, cfg.Algorithm)
	if err != nil {
		return nil, fmt.Errorf("tenantauth: parse public key: %w", err)
	}
	return &staticVerifier{
		cfg: cfg,
		key: key,
		parser: jwt.NewParser(
			// Pin the algorithm — refuses `alg:none` and blocks HS↔RS/ES/EdDSA
			// confusion. golang-jwt still calls our keyfunc, so we ALSO
			// re-check the header algorithm there as defence in depth.
			jwt.WithValidMethods([]string{cfg.Algorithm}),
			jwt.WithIssuer(cfg.Issuer),
			jwt.WithAudience(cfg.Audience),
			jwt.WithExpirationRequired(),
		),
		claim: cfg.tenantClaim(),
	}, nil
}

func (s *staticVerifier) name() string   { return "static_pem" }
func (s *staticVerifier) issuer() string { return s.cfg.Issuer }

func (s *staticVerifier) verify(_ context.Context, token string) (string, *rejection) {
	claims := jwt.MapClaims{}
	parsed, err := s.parser.ParseWithClaims(token, claims, func(tok *jwt.Token) (any, error) {
		if tok.Method == nil || tok.Method.Alg() != s.cfg.Algorithm {
			return nil, fmt.Errorf("unexpected signing method %q", tok.Header["alg"])
		}
		return s.key, nil
	})
	if err != nil {
		return "", &rejection{
			reason:     classifyJWTError(err),
			err:        fmt.Errorf("tenantauth: verify token: %w", err),
			recognized: issuerMatches(claims, s.cfg.Issuer),
		}
	}
	if parsed == nil || !parsed.Valid {
		return "", &rejection{reason: reasonMalformed, err: errors.New("tenantauth: token invalid")}
	}
	tenant, err := extractTenant(claims, s.claim)
	if err != nil {
		return "", &rejection{reason: reasonMissingTenant, err: err, recognized: true}
	}
	return tenant, nil
}

// extractBearer pulls the token out of the standard Authorization header.
// Header names are case-insensitive; sources may deliver them in either
// canonical or lower form depending on the transport.
func extractBearer(sources map[string][]string) (string, error) {
	var values []string
	for k, v := range sources {
		if strings.EqualFold(k, "authorization") {
			values = v
			break
		}
	}
	if len(values) == 0 {
		return "", errors.New("tenantauth: missing Authorization header")
	}
	raw := values[0]
	// Case-insensitive Bearer prefix per RFC 6750.
	const prefix = "Bearer "
	if len(raw) < len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
		return "", errors.New("tenantauth: Authorization header is not a Bearer token")
	}
	tok := strings.TrimSpace(raw[len(prefix):])
	if tok == "" {
		return "", errors.New("tenantauth: empty bearer token")
	}
	return tok, nil
}

// extractTenant returns the tenant claim as a non-empty string or an
// error. A JWT that verifies cryptographically but carries no tenant
// still fails auth — the extension never resolves an empty tenant, so
// the fail-closed guarantee in tenanttagger holds by construction.
func extractTenant(claims jwt.MapClaims, name string) (string, error) {
	v, ok := claims[name]
	if !ok {
		return "", fmt.Errorf("tenantauth: token missing %q claim", name)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("tenantauth: %q claim is not a string", name)
	}
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("tenantauth: %q claim is empty", name)
	}
	return s, nil
}

// tenantAuthData is a tiny client.AuthData implementation. Only the tenant
// is exposed; the raw token is discarded so it cannot leak downstream.
type tenantAuthData struct {
	tenant string
	claim  string
}

func (d tenantAuthData) GetAttribute(name string) any {
	if name == d.claim {
		return d.tenant
	}
	return nil
}

func (d tenantAuthData) GetAttributeNames() []string { return []string{d.claim} }

// compile-time interface checks.
var (
	_ auth.Server     = (*tenantAuth)(nil)
	_ client.AuthData = tenantAuthData{}
	_ verifier        = (*staticVerifier)(nil)
	_ verifier        = (*serviceAccountVerifier)(nil)
)
