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
)

// tenantAuth is the auth.Server implementation. It verifies a Bearer JWT
// against the pinned algorithm + configured issuer + audience and puts the
// resolved tenant into the request's client.Info so the tenanttagger
// processor can stamp it on every record.
type tenantAuth struct {
	cfg    *Config
	key    any
	parser *jwt.Parser
	claim  string
}

// newTenantAuth is exposed to the factory. It parses the PEM key once at
// construction time so runtime auth calls are pure verifies.
func newTenantAuth(cfg *Config) (*tenantAuth, error) {
	pem, err := cfg.loadPEM()
	if err != nil {
		return nil, err
	}
	key, err := parsePublicKey(pem, cfg.Algorithm)
	if err != nil {
		return nil, fmt.Errorf("tenantauth: parse public key: %w", err)
	}
	parser := jwt.NewParser(
		// Pin the algorithm — refuses `alg:none` and blocks HS↔RS/ES/EdDSA
		// confusion. golang-jwt still calls our keyfunc, so we ALSO
		// re-check the header algorithm there as defence in depth.
		jwt.WithValidMethods([]string{cfg.Algorithm}),
		jwt.WithIssuer(cfg.Issuer),
		jwt.WithAudience(cfg.Audience),
		jwt.WithExpirationRequired(),
	)
	return &tenantAuth{
		cfg:    cfg,
		key:    key,
		parser: parser,
		claim:  cfg.tenantClaim(),
	}, nil
}

// Start satisfies component.Component. Nothing to do; the key was parsed
// in newTenantAuth so a bad key surfaces at extension construction, not
// on the first request.
func (t *tenantAuth) Start(_ context.Context, _ component.Host) error { return nil }

// Shutdown satisfies component.Component.
func (t *tenantAuth) Shutdown(_ context.Context) error { return nil }

// Authenticate is the hot path. It runs on every OTLP request, so it
// must be allocation-light and fail-fast on any invalid input.
//
// Contract on success: the returned context carries a client.Info whose
// Auth.GetAttribute("tenant") returns the tenant claim string. Downstream
// processors read exactly that.
//
// Contract on failure: a non-nil error and the ORIGINAL context. The
// collector's auth interceptor maps a non-nil error to 401.
func (t *tenantAuth) Authenticate(ctx context.Context, sources map[string][]string) (context.Context, error) {
	token, err := extractBearer(sources)
	if err != nil {
		return ctx, err
	}
	claims := jwt.MapClaims{}
	// keyfunc is called AFTER the parser has already refused any alg not
	// in WithValidMethods, but we re-verify header.alg here so a future
	// jwt-go bug can't relax that guarantee behind our back.
	parsed, err := t.parser.ParseWithClaims(token, claims, func(tok *jwt.Token) (any, error) {
		if tok.Method == nil || tok.Method.Alg() != t.cfg.Algorithm {
			return nil, fmt.Errorf("unexpected signing method %q", tok.Header["alg"])
		}
		return t.key, nil
	})
	if err != nil {
		return ctx, fmt.Errorf("tenantauth: verify token: %w", err)
	}
	if !parsed.Valid {
		return ctx, errors.New("tenantauth: token invalid")
	}
	tenant, err := extractTenant(claims, t.claim)
	if err != nil {
		return ctx, err
	}
	info := client.FromContext(ctx)
	info.Auth = tenantAuthData{tenant: tenant, claim: t.claim}
	return client.NewContext(ctx, info), nil
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
// claim is exposed; the raw token is discarded so it cannot leak downstream.
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
)
