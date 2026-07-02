package tenantauth

import (
	"context"
	"errors"
	"fmt"

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
	default:
		return reasonMalformed
	}
}
