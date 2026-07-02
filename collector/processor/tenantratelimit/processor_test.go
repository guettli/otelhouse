package tenantratelimit

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/collector/client"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeAuth mirrors the auth stub in the tenanttagger tests — an Auth that
// resolves exactly one attribute to a fixed value.
type fakeAuth struct{ name, value string }

func (a fakeAuth) GetAttribute(name string) any {
	if name == a.name {
		return a.value
	}
	return nil
}
func (a fakeAuth) GetAttributeNames() []string { return []string{a.name} }

func ctxWithTenant(t string) context.Context {
	info := client.Info{Auth: fakeAuth{name: "tenant", value: t}}
	return client.NewContext(context.Background(), info)
}

// tracesWithSpans builds a Traces batch whose single ResourceSpans has n
// spans — the count SpanCount() will report. The processor cares only
// about the count.
func tracesWithSpans(n int) ptrace.Traces {
	td := ptrace.NewTraces()
	spans := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans()
	for i := 0; i < n; i++ {
		spans.AppendEmpty().SetName("op")
	}
	return td
}

func logsWithRecords(n int) plog.Logs {
	ld := plog.NewLogs()
	recs := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords()
	for i := 0; i < n; i++ {
		recs.AppendEmpty().Body().SetStr("m")
	}
	return ld
}

// metricsWithGauges builds n single-point gauge metrics; DataPointCount()
// returns n.
func metricsWithGauges(n int) pmetric.Metrics {
	md := pmetric.NewMetrics()
	sm := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()
	for i := 0; i < n; i++ {
		m := sm.Metrics().AppendEmpty()
		m.SetName("m")
		m.SetEmptyGauge().DataPoints().AppendEmpty()
	}
	return md
}

// newTestLimiter wires a limiter against a noop meter and a fixed clock.
// Tests that want to inspect the counter use newTestLimiterWithReader.
func newTestLimiter(t *testing.T, cfg Config) *limiter {
	t.Helper()
	l, err := newLimiter(cfg, metricnoop.NewMeterProvider())
	if err != nil {
		t.Fatalf("newLimiter: %v", err)
	}
	fixed := time.Unix(0, 0)
	l.now = func() time.Time { return fixed }
	return l
}

// newTestLimiterWithReader wires a real SDK MeterProvider around a manual
// reader so the test can read the dropped counter's data points.
func newTestLimiterWithReader(t *testing.T, cfg Config) (*limiter, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	l, err := newLimiter(cfg, mp)
	if err != nil {
		t.Fatalf("newLimiter: %v", err)
	}
	fixed := time.Unix(0, 0)
	l.now = func() time.Time { return fixed }
	return l, reader
}

// droppedFor returns the counter value for the given tenant, or -1 if the
// counter has no series for that tenant.
func droppedFor(t *testing.T, reader *sdkmetric.ManualReader, tenant string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "otelhouse_gateway_ratelimit_dropped" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("counter data type = %T, want Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				v, ok := dp.Attributes.Value("tenant")
				if !ok || v.AsString() != tenant {
					continue
				}
				return dp.Value
			}
		}
	}
	return -1
}

func defaultCfg() Config {
	return Config{
		AuthAttribute: "tenant",
		Default:       RateLimit{Rate: 100, Burst: 100},
	}
}

// TestTraces_UnderLimitAdmitted — a batch that fits inside the bucket
// passes through unchanged.
func TestTraces_UnderLimitAdmitted(t *testing.T) {
	l := newTestLimiter(t, defaultCfg())
	td := tracesWithSpans(10)
	out, err := processTraces(ctxWithTenant("alice"), l, defaultCfg(), td)
	if err != nil {
		t.Fatalf("processTraces: %v", err)
	}
	if got := out.SpanCount(); got != 10 {
		t.Fatalf("out span count = %d, want 10", got)
	}
}

// TestTraces_OverLimitRejected is THE acceptance test: a tenant over its
// bucket is refused loudly (RESOURCE_EXHAUSTED) and the counter records
// exactly the batch size that was dropped.
func TestTraces_OverLimitRejected(t *testing.T) {
	cfg := Config{
		AuthAttribute: "tenant",
		Default:       RateLimit{Rate: 10, Burst: 50},
	}
	l, reader := newTestLimiterWithReader(t, cfg)

	// First batch consumes the full burst; second one has no tokens left.
	if _, err := processTraces(ctxWithTenant("alice"), l, cfg, tracesWithSpans(50)); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	_, err := processTraces(ctxWithTenant("alice"), l, cfg, tracesWithSpans(50))
	if err == nil {
		t.Fatal("second batch: want error, got nil")
	}
	if s, ok := status.FromError(err); !ok || s.Code() != codes.ResourceExhausted {
		t.Fatalf("err code = %v, want ResourceExhausted (err=%v)", codes.Code(status.Code(err)), err)
	}
	if got := droppedFor(t, reader, "alice"); got != 50 {
		t.Fatalf("dropped counter = %d, want 50", got)
	}
}

// TestTraces_TenantsAreIsolated is the "one misbehaving tenant does not
// starve the others" property from the acceptance list.
func TestTraces_TenantsAreIsolated(t *testing.T) {
	cfg := Config{
		AuthAttribute: "tenant",
		Default:       RateLimit{Rate: 10, Burst: 10},
	}
	l, reader := newTestLimiterWithReader(t, cfg)

	// alice bursts and gets throttled.
	if _, err := processTraces(ctxWithTenant("alice"), l, cfg, tracesWithSpans(10)); err != nil {
		t.Fatalf("alice first: %v", err)
	}
	if _, err := processTraces(ctxWithTenant("alice"), l, cfg, tracesWithSpans(5)); err == nil {
		t.Fatal("alice second: want error, got nil")
	}

	// bob is unaffected — full bucket available.
	if _, err := processTraces(ctxWithTenant("bob"), l, cfg, tracesWithSpans(10)); err != nil {
		t.Fatalf("bob: want success while alice is throttled, got %v", err)
	}

	if got := droppedFor(t, reader, "alice"); got != 5 {
		t.Fatalf("alice dropped = %d, want 5", got)
	}
	if got := droppedFor(t, reader, "bob"); got != -1 {
		t.Fatalf("bob dropped = %d, want no counter series", got)
	}
}

// TestTraces_OverridesHonored — a tenant with an override uses that
// bucket, not the default.
func TestTraces_OverridesHonored(t *testing.T) {
	cfg := Config{
		AuthAttribute: "tenant",
		Default:       RateLimit{Rate: 1, Burst: 1},
		Overrides: map[string]RateLimit{
			"whale": {Rate: 1000, Burst: 5000},
		},
	}
	l := newTestLimiter(t, cfg)

	// Under the default this would fail on the first record after the
	// first one; under the override, 500 records still fit in the burst.
	if _, err := processTraces(ctxWithTenant("whale"), l, cfg, tracesWithSpans(500)); err != nil {
		t.Fatalf("whale under override: %v", err)
	}

	// A different tenant still uses the tiny default.
	if _, err := processTraces(ctxWithTenant("minnow"), l, cfg, tracesWithSpans(1)); err != nil {
		t.Fatalf("minnow burst=1 first: %v", err)
	}
	if _, err := processTraces(ctxWithTenant("minnow"), l, cfg, tracesWithSpans(1)); err == nil {
		t.Fatal("minnow burst=1 second: want error, got nil")
	}
}

// TestTraces_BatchBiggerThanBurst — a batch that exceeds burst can never
// succeed. Confirm the whole batch is rejected (not partially admitted)
// and the counter records the full batch.
func TestTraces_BatchBiggerThanBurst(t *testing.T) {
	cfg := Config{
		AuthAttribute: "tenant",
		Default:       RateLimit{Rate: 10, Burst: 10},
	}
	l, reader := newTestLimiterWithReader(t, cfg)
	_, err := processTraces(ctxWithTenant("alice"), l, cfg, tracesWithSpans(50))
	if err == nil {
		t.Fatal("want error for batch bigger than burst, got nil")
	}
	if s, ok := status.FromError(err); !ok || s.Code() != codes.ResourceExhausted {
		t.Fatalf("err code = %v, want ResourceExhausted (err=%v)", codes.Code(status.Code(err)), err)
	}
	if got := droppedFor(t, reader, "alice"); got != 50 {
		t.Fatalf("dropped = %d, want 50", got)
	}
}

// TestTraces_FailClosedNoAuth — no tenant in context → ErrMissingTenant,
// same fail-closed invariant as tenanttagger.
func TestTraces_FailClosedNoAuth(t *testing.T) {
	l := newTestLimiter(t, defaultCfg())
	_, err := processTraces(context.Background(), l, defaultCfg(), tracesWithSpans(1))
	if !errors.Is(err, ErrMissingTenant) {
		t.Fatalf("err = %v, want ErrMissingTenant", err)
	}
}

// TestTraces_FailClosedEmptyTenant — auth present but empty string tenant
// hits the same failure mode.
func TestTraces_FailClosedEmptyTenant(t *testing.T) {
	l := newTestLimiter(t, defaultCfg())
	_, err := processTraces(ctxWithTenant(""), l, defaultCfg(), tracesWithSpans(1))
	if !errors.Is(err, ErrMissingTenant) {
		t.Fatalf("err = %v, want ErrMissingTenant", err)
	}
}

// TestLogs_OverLimit parity check for the logs pipeline.
func TestLogs_OverLimit(t *testing.T) {
	cfg := Config{
		AuthAttribute: "tenant",
		Default:       RateLimit{Rate: 5, Burst: 5},
	}
	l, reader := newTestLimiterWithReader(t, cfg)
	if _, err := processLogs(ctxWithTenant("alice"), l, cfg, logsWithRecords(5)); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := processLogs(ctxWithTenant("alice"), l, cfg, logsWithRecords(3)); err == nil {
		t.Fatal("want error on second batch")
	}
	if got := droppedFor(t, reader, "alice"); got != 3 {
		t.Fatalf("dropped = %d, want 3", got)
	}
}

// TestMetrics_OverLimit parity check for the metrics pipeline. Counts
// data points, not metric definitions.
func TestMetrics_OverLimit(t *testing.T) {
	cfg := Config{
		AuthAttribute: "tenant",
		Default:       RateLimit{Rate: 5, Burst: 5},
	}
	l, reader := newTestLimiterWithReader(t, cfg)
	if _, err := processMetrics(ctxWithTenant("alice"), l, cfg, metricsWithGauges(5)); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := processMetrics(ctxWithTenant("alice"), l, cfg, metricsWithGauges(3)); err == nil {
		t.Fatal("want error on second batch")
	}
	if got := droppedFor(t, reader, "alice"); got != 3 {
		t.Fatalf("dropped = %d, want 3", got)
	}
}

// TestLogs_FailClosed and TestMetrics_FailClosed keep the fail-closed
// invariant symmetric across signals.
func TestLogs_FailClosed(t *testing.T) {
	l := newTestLimiter(t, defaultCfg())
	if _, err := processLogs(context.Background(), l, defaultCfg(), logsWithRecords(1)); !errors.Is(err, ErrMissingTenant) {
		t.Fatalf("err = %v, want ErrMissingTenant", err)
	}
}
func TestMetrics_FailClosed(t *testing.T) {
	l := newTestLimiter(t, defaultCfg())
	if _, err := processMetrics(context.Background(), l, defaultCfg(), metricsWithGauges(1)); !errors.Is(err, ErrMissingTenant) {
		t.Fatalf("err = %v, want ErrMissingTenant", err)
	}
}

// TestBucketRefill — after enough simulated time passes the tenant gets
// its bucket back. The clock is virtualised so the test does not sleep.
func TestBucketRefill(t *testing.T) {
	cfg := Config{
		AuthAttribute: "tenant",
		Default:       RateLimit{Rate: 10, Burst: 10},
	}
	l := newTestLimiter(t, cfg)
	base := time.Unix(0, 0)
	current := base
	l.now = func() time.Time { return current }

	// Empty the bucket.
	if _, err := processTraces(ctxWithTenant("alice"), l, cfg, tracesWithSpans(10)); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, err := processTraces(ctxWithTenant("alice"), l, cfg, tracesWithSpans(1)); err == nil {
		t.Fatal("bucket should be empty")
	}

	// Advance a full second — rate=10 refills the entire bucket.
	current = base.Add(time.Second)
	if _, err := processTraces(ctxWithTenant("alice"), l, cfg, tracesWithSpans(10)); err != nil {
		t.Fatalf("after refill: %v", err)
	}
}

// TestConfig_Validate — startup rejects obviously broken config so a
// misconfigured deployment fails to load instead of silently letting
// traffic through.
func TestConfig_Validate(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		c := &Config{AuthAttribute: "tenant", Default: RateLimit{Rate: 1, Burst: 1}}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
	t.Run("empty_auth_attribute", func(t *testing.T) {
		c := &Config{Default: RateLimit{Rate: 1, Burst: 1}}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("zero_default_rate", func(t *testing.T) {
		c := &Config{AuthAttribute: "tenant"}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("negative_burst", func(t *testing.T) {
		c := &Config{AuthAttribute: "tenant", Default: RateLimit{Rate: 1, Burst: -1}}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("bad_override_rate", func(t *testing.T) {
		c := &Config{
			AuthAttribute: "tenant",
			Default:       RateLimit{Rate: 1, Burst: 1},
			Overrides:     map[string]RateLimit{"alice": {Rate: 0}},
		}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("empty_override_key", func(t *testing.T) {
		c := &Config{
			AuthAttribute: "tenant",
			Default:       RateLimit{Rate: 1, Burst: 1},
			Overrides:     map[string]RateLimit{"": {Rate: 1, Burst: 1}},
		}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})
}

// TestFactoryDefaults — the ocb-integration surface. Defaults must be
// safe (positive rate) and match the AuthAttribute convention.
func TestFactoryDefaults(t *testing.T) {
	f := NewFactory()
	cfg := f.CreateDefaultConfig().(*Config)
	if cfg.AuthAttribute != "tenant" {
		t.Fatalf("AuthAttribute = %q, want tenant", cfg.AuthAttribute)
	}
	if cfg.Default.Rate <= 0 || cfg.Default.Burst <= 0 {
		t.Fatalf("default rate/burst = %+v, want positive", cfg.Default)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
}

// TestEffectiveBurst_ZeroFallback — a caller who leaves Burst unset gets
// a one-second's-worth allowance instead of an unusable zero bucket.
func TestEffectiveBurst_ZeroFallback(t *testing.T) {
	if got := (RateLimit{Rate: 10}).effectiveBurst(); got != 10 {
		t.Fatalf("burst = %d, want 10 (from Rate fallback)", got)
	}
	if got := (RateLimit{Rate: 0.5}).effectiveBurst(); got != 1 {
		t.Fatalf("burst = %d, want 1 (min fallback)", got)
	}
	if got := (RateLimit{Rate: 10, Burst: 42}).effectiveBurst(); got != 42 {
		t.Fatalf("burst = %d, want 42 (explicit)", got)
	}
}

// TestCustomAuthAttribute — a deployment that renames the auth attribute
// (say to "org") must still read the tenant. Guards against a hard-coded
// "tenant" lookup.
func TestCustomAuthAttribute(t *testing.T) {
	cfg := Config{AuthAttribute: "org", Default: RateLimit{Rate: 10, Burst: 10}}
	l := newTestLimiter(t, cfg)
	info := client.Info{Auth: fakeAuth{name: "org", value: "acme"}}
	ctx := client.NewContext(context.Background(), info)
	if _, err := processTraces(ctx, l, cfg, tracesWithSpans(1)); err != nil {
		t.Fatalf("custom auth attribute: %v", err)
	}
}

// TestAdmit_ZeroCountNoOp — an empty batch should not consume tokens or
// increment the counter. Empty batches are legal on the wire (e.g. an
// SDK flushing an empty queue) and we should not turn them into 429s.
func TestAdmit_ZeroCountNoOp(t *testing.T) {
	cfg := Config{AuthAttribute: "tenant", Default: RateLimit{Rate: 1, Burst: 1}}
	l, reader := newTestLimiterWithReader(t, cfg)
	if _, err := processTraces(ctxWithTenant("alice"), l, cfg, tracesWithSpans(0)); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	// A follow-up single record still fits — the empty batch did not
	// drain the bucket.
	if _, err := processTraces(ctxWithTenant("alice"), l, cfg, tracesWithSpans(1)); err != nil {
		t.Fatalf("after empty batch: %v", err)
	}
	if got := droppedFor(t, reader, "alice"); got != -1 {
		t.Fatalf("dropped counter unexpectedly present: %d", got)
	}
}

// TestNewLimiter_NoopMeter — construction against a noop meter provider
// (used when the collector has no telemetry configured) succeeds.
func TestNewLimiter_NoopMeter(t *testing.T) {
	var mp metric.MeterProvider = metricnoop.NewMeterProvider()
	if _, err := newLimiter(defaultCfg(), mp); err != nil {
		t.Fatalf("newLimiter: %v", err)
	}
}
