package tenantauth

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newTestAuthWithMeter is the metrics-aware sibling of newAuth: it builds a
// tenantauth extension whose MeterProvider is a real SDK provider wired to a
// manual reader. Tests call Collect on the reader to inspect the counter
// after driving a request through Authenticate.
func newTestAuthWithMeter(t *testing.T, km keyMaterial) (*tenantAuth, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
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
	a, err := newTenantAuth(cfg, mp)
	if err != nil {
		t.Fatalf("newTenantAuth: %v", err)
	}
	return a, reader
}

// rejectionCount returns the value of the auth-rejections counter for the
// (reason=<reason>) datapoint. Returns 0 when the datapoint is absent — the
// SDK does not emit a series until it has been touched, so "no signal" is
// the expected reading for a reason the test did not trigger.
func rejectionCount(t *testing.T, reader *sdkmetric.ManualReader, reason string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "otelhouse_gateway_auth_rejections" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %q is %T, want metricdata.Sum[int64]", m.Name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				if v, _ := dp.Attributes.Value(attribute.Key("reason")); v.AsString() == reason {
					return dp.Value
				}
			}
		}
	}
	return 0
}

// hasRejectionMetric returns true if the counter has been emitted at all —
// used for happy-path assertions where we want to prove NO datapoint exists.
func hasRejectionMetric(t *testing.T, reader *sdkmetric.ManualReader) bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "otelhouse_gateway_auth_rejections" {
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					return false
				}
				return len(sum.DataPoints) > 0
			}
		}
	}
	return false
}

// TestMetrics_ValidTokenDoesNotIncrement locks in the acceptance criterion
// "a good token does not increment the counter" — the operator needs a clean
// baseline of zero rejections when everything is healthy.
func TestMetrics_ValidTokenDoesNotIncrement(t *testing.T) {
	km := newKeys(t, "EdDSA")
	a, reader := newTestAuthWithMeter(t, km)
	token := signToken(t, km, baseClaims())
	if _, err := a.Authenticate(context.Background(), bearer(token)); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if hasRejectionMetric(t, reader) {
		t.Fatal("expected NO auth rejection datapoints after a valid token")
	}
}

// TestMetrics_RejectionReasons walks each JWT failure mode and asserts the
// counter increments with the matching `reason` label. This is the primary
// acceptance test — a dashboard filtering on any of these reasons must
// actually see a signal.
func TestMetrics_RejectionReasons(t *testing.T) {
	km := newKeys(t, "EdDSA")
	other := newKeys(t, "EdDSA")

	// each case is a factory that drives one Authenticate call which must
	// map to the named reason.
	cases := []struct {
		name   string
		reason string
		run    func(t *testing.T, a *tenantAuth)
	}{
		{
			name:   "expired",
			reason: reasonExpired,
			run: func(t *testing.T, a *tenantAuth) {
				c := baseClaims()
				c["iat"] = time.Now().Add(-2 * time.Hour).Unix()
				c["exp"] = time.Now().Add(-1 * time.Hour).Unix()
				_, _ = a.Authenticate(context.Background(), bearer(signToken(t, km, c)))
			},
		},
		{
			name:   "bad_signature",
			reason: reasonBadSignature,
			run: func(t *testing.T, a *tenantAuth) {
				_, _ = a.Authenticate(context.Background(), bearer(signToken(t, other, baseClaims())))
			},
		},
		{
			name:   "bad_issuer",
			reason: reasonBadIssuer,
			run: func(t *testing.T, a *tenantAuth) {
				c := baseClaims()
				c["iss"] = "not-the-real-mint"
				_, _ = a.Authenticate(context.Background(), bearer(signToken(t, km, c)))
			},
		},
		{
			name:   "bad_audience",
			reason: reasonBadAudience,
			run: func(t *testing.T, a *tenantAuth) {
				c := baseClaims()
				c["aud"] = "some-other-gateway"
				_, _ = a.Authenticate(context.Background(), bearer(signToken(t, km, c)))
			},
		},
		{
			name:   "missing_tenant",
			reason: reasonMissingTenant,
			run: func(t *testing.T, a *tenantAuth) {
				c := baseClaims()
				delete(c, "tenant")
				_, _ = a.Authenticate(context.Background(), bearer(signToken(t, km, c)))
			},
		},
		{
			name:   "malformed_missing_header",
			reason: reasonMalformed,
			run: func(t *testing.T, a *tenantAuth) {
				_, _ = a.Authenticate(context.Background(), map[string][]string{})
			},
		},
		{
			// alg:none is refused by WithValidMethods, which surfaces as
			// jwt.ErrTokenSignatureInvalid — the same bucket a forged token
			// lands in. That matches operator intuition: both are "someone
			// tried to defeat the signature," not "the mint forgot a field."
			name:   "bad_signature_alg_none",
			reason: reasonBadSignature,
			run: func(t *testing.T, a *tenantAuth) {
				tok := jwt.NewWithClaims(jwt.SigningMethodNone, baseClaims())
				s, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
				if err != nil {
					t.Fatalf("sign none: %v", err)
				}
				_, _ = a.Authenticate(context.Background(), bearer(s))
			},
		},
		{
			name:   "malformed_not_bearer",
			reason: reasonMalformed,
			run: func(t *testing.T, a *tenantAuth) {
				_, _ = a.Authenticate(context.Background(), map[string][]string{
					"Authorization": {"Basic Zm9vOmJhcg=="},
				})
			},
		},
		{
			name:   "malformed_empty_token",
			reason: reasonMalformed,
			run: func(t *testing.T, a *tenantAuth) {
				_, _ = a.Authenticate(context.Background(), map[string][]string{
					"Authorization": {"Bearer "},
				})
			},
		},
		{
			name:   "malformed_garbage_bearer",
			reason: reasonMalformed,
			run: func(t *testing.T, a *tenantAuth) {
				_, _ = a.Authenticate(context.Background(), bearer("not-a-jwt"))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, reader := newTestAuthWithMeter(t, km)
			tc.run(t, a)
			if got := rejectionCount(t, reader, tc.reason); got != 1 {
				t.Fatalf("rejections{reason=%q} = %d, want 1", tc.reason, got)
			}
		})
	}
}

// TestMetrics_NoOpMeterProvider proves the nil-MeterProvider path stays
// silent (no panic, no error) — this is what the tests and any lightweight
// caller rely on to skip plumbing an SDK through.
func TestMetrics_NoOpMeterProvider(t *testing.T) {
	km := newKeys(t, "EdDSA")
	a, err := newTenantAuth(&Config{
		Issuer:       "otelhouse-mint",
		Audience:     "otelhouse-gateway",
		Algorithm:    km.alg,
		PublicKeyPEM: string(km.pubPEM),
		TenantClaim:  defaultTenantClaim,
	}, metric.MeterProvider(nil))
	if err != nil {
		t.Fatalf("newTenantAuth: %v", err)
	}
	// Any Authenticate call must not panic; the counter simply goes nowhere.
	_, _ = a.Authenticate(context.Background(), map[string][]string{})
	if _, err := a.Authenticate(context.Background(), bearer(signToken(t, km, baseClaims()))); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
}
