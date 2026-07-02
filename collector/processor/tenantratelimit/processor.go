package tenantratelimit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/collector/client"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/time/rate"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// limiter holds one token bucket per tenant plus the counter used to
// surface drops. Buckets are created lazily on first sight of a tenant
// so a large tenant list does not allocate at startup.
type limiter struct {
	cfg     Config
	dropped metric.Int64Counter

	// now is time.Now in production. Tests swap it out to drive the
	// bucket without real sleep.
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]*rate.Limiter
}

// newLimiter builds a limiter with the counter registered on the
// collector's meter provider. The counter name is
// otelhouse_gateway_ratelimit_dropped — the Prometheus exposition adds
// the `_total` suffix automatically for monotonic Sum instruments, so
// the scraped series is otelhouse_gateway_ratelimit_dropped_total.
func newLimiter(cfg Config, mp metric.MeterProvider) (*limiter, error) {
	meter := mp.Meter("github.com/guettli/otelhouse/collector/processor/tenantratelimit")
	dropped, err := meter.Int64Counter(
		"otelhouse_gateway_ratelimit_dropped",
		metric.WithDescription("Records dropped by the per-tenant rate limiter, by tenant."),
		metric.WithUnit("{record}"),
	)
	if err != nil {
		return nil, fmt.Errorf("tenantratelimit: create counter: %w", err)
	}
	return &limiter{
		cfg:     cfg,
		dropped: dropped,
		now:     time.Now,
		buckets: make(map[string]*rate.Limiter),
	}, nil
}

// bucketFor returns the tenant's token bucket, creating it on first use
// with the tenant's override (or the default if none).
func (l *limiter) bucketFor(tenant string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[tenant]; ok {
		return b
	}
	spec, ok := l.cfg.Overrides[tenant]
	if !ok {
		spec = l.cfg.Default
	}
	b := rate.NewLimiter(rate.Limit(spec.Rate), spec.effectiveBurst())
	l.buckets[tenant] = b
	return b
}

// admit charges the tenant's bucket for a batch of n records. When the
// bucket cannot cover the batch, admit records the drop on the counter
// and returns a gRPC RESOURCE_EXHAUSTED error so the OTLP receiver maps
// it to RESOURCE_EXHAUSTED / HTTP 429 back to the client.
func (l *limiter) admit(ctx context.Context, tenant string, n int) error {
	if n <= 0 {
		return nil
	}
	if l.bucketFor(tenant).AllowN(l.now(), n) {
		return nil
	}
	l.dropped.Add(ctx, int64(n),
		metric.WithAttributes(attribute.String("tenant", tenant)),
	)
	return status.Errorf(codes.ResourceExhausted,
		"tenantratelimit: tenant %q over rate limit, dropped %d records", tenant, n)
}

// tenantFromContext mirrors tenanttagger.tenantFromContext: the ONE place
// tenancy is read is client.Info. The processor never trusts the payload.
func tenantFromContext(ctx context.Context, authAttr string) (string, bool) {
	info := client.FromContext(ctx)
	if info.Auth == nil {
		return "", false
	}
	v := info.Auth.GetAttribute(authAttr)
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

func processTraces(ctx context.Context, l *limiter, cfg Config, td ptrace.Traces) (ptrace.Traces, error) {
	tenant, ok := tenantFromContext(ctx, cfg.AuthAttribute)
	if !ok {
		return td, ErrMissingTenant
	}
	if err := l.admit(ctx, tenant, td.SpanCount()); err != nil {
		return td, err
	}
	return td, nil
}

func processLogs(ctx context.Context, l *limiter, cfg Config, ld plog.Logs) (plog.Logs, error) {
	tenant, ok := tenantFromContext(ctx, cfg.AuthAttribute)
	if !ok {
		return ld, ErrMissingTenant
	}
	if err := l.admit(ctx, tenant, ld.LogRecordCount()); err != nil {
		return ld, err
	}
	return ld, nil
}

func processMetrics(ctx context.Context, l *limiter, cfg Config, md pmetric.Metrics) (pmetric.Metrics, error) {
	tenant, ok := tenantFromContext(ctx, cfg.AuthAttribute)
	if !ok {
		return md, ErrMissingTenant
	}
	if err := l.admit(ctx, tenant, md.DataPointCount()); err != nil {
		return md, err
	}
	return md, nil
}
