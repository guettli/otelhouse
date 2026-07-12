package tenantauth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// meterScopeName follows the OTel convention: the import path of the
// instrumented package.
const meterScopeName = "github.com/guettli/otelhouse/collector/extension/tenantauth"

// Rejection reason constants — used as the `reason` label on the auth
// rejections counter. These are the operator-visible categories the issue
// asked us to expose; add a new one here rather than inventing labels ad
// hoc at the increment site (otherwise dashboards drift).
const (
	reasonExpired       = "expired"
	reasonBadSignature  = "bad_signature"
	reasonMissingTenant = "missing_tenant"
	reasonBadIssuer     = "bad_issuer"
	reasonBadAudience   = "bad_audience"
	reasonMalformed     = "malformed"

	// Kubernetes ServiceAccount-token source.

	// reasonUnknownKID: the token's `kid` is not in the cluster JWKS, even
	// after a refresh — a forged token, or one from another cluster.
	reasonUnknownKID = "unknown_kid"
	// reasonJWKSUnavailable: the cluster JWKS could not be fetched or
	// parsed, so no ServiceAccount token can be verified right now. This is
	// a gateway-side outage, not a producer error — alert on it.
	reasonJWKSUnavailable = "jwks_unavailable"
	// reasonUnmappedSA: the token verified, but its ServiceAccount identity
	// maps to no tenant. Fail-closed: never defaulted to a fallback tenant.
	reasonUnmappedSA = "unmapped_serviceaccount"
)

// authMetrics owns the observability signals emitted by the tenantauth
// extension. The counter is the operator's only view into which tenants
// are failing auth and why — a silently-expired token is now loud.
type authMetrics struct {
	rejections metric.Int64Counter
}

// newAuthMetrics constructs the counter against the supplied MeterProvider.
// A nil provider is treated as the noop provider so tests and any bare
// construction path do not need to plumb a real SDK through.
func newAuthMetrics(mp metric.MeterProvider) (*authMetrics, error) {
	if mp == nil {
		mp = noop.NewMeterProvider()
	}
	meter := mp.Meter(meterScopeName)
	// The Prometheus exporter appends `_total` to monotonic sums, so the
	// registered instrument name is the acceptance-criteria metric name
	// minus that suffix.
	c, err := meter.Int64Counter(
		"otelhouse_gateway_auth_rejections",
		metric.WithDescription("OTLP requests rejected by the tenantauth extension, labelled by reason."),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, fmt.Errorf("tenantauth: create auth rejections counter: %w", err)
	}
	return &authMetrics{rejections: c}, nil
}

// recordRejection increments the counter with the supplied reason label.
// Kept as a method so the increment sites stay noise-free.
func (m *authMetrics) recordRejection(ctx context.Context, reason string) {
	m.rejections.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// classifyJWTError maps a golang-jwt error to one of the exposed reason
// labels. Order matters: expired/issuer/audience are all wrapped in
// ErrTokenInvalidClaims, so we check the specific sentinels first and fall
// through to `malformed` for anything else the parser refused (alg:none,
// alg confusion, wrong-method, corrupted encoding, ...).
func classifyJWTError(err error) string {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return reasonExpired
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		return reasonBadIssuer
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return reasonBadAudience
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return reasonBadSignature
	case errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
		// A token with NO `aud` (or no `iss`) is the same operator-visible
		// failure as one with the wrong value — a producer that did not
		// project its token for this gateway. golang-jwt distinguishes
		// "missing" from "mismatched" only in the message, so match on that;
		// anything else required-but-missing (exp, ...) stays `malformed`.
		switch {
		case strings.Contains(err.Error(), "aud claim is required"):
			return reasonBadAudience
		case strings.Contains(err.Error(), "iss claim is required"):
			return reasonBadIssuer
		default:
			return reasonMalformed
		}
	default:
		return reasonMalformed
	}
}
