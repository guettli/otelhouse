package tenanttagger

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// meterScopeName follows the OTel convention: the import path of the
// instrumented package.
const meterScopeName = "github.com/guettli/otelhouse/collector/processor/tenanttagger"

// Signal label values on the ingest records counter. Kept as constants so
// the enum is grep-able from dashboards → code.
const (
	signalTraces  = "traces"
	signalLogs    = "logs"
	signalMetrics = "metrics"
)

// ingestMetrics owns the observability signal for per-tenant ingest volume.
// The tenanttagger runs on every batch after tenantauth resolved the tenant,
// so this is the natural point to attribute records to a tenant.
type ingestMetrics struct {
	records metric.Int64Counter
}

// newIngestMetrics builds the counter against the supplied MeterProvider.
// A nil provider is treated as the noop provider so tests / minimal setups
// do not need to plumb one in.
func newIngestMetrics(mp metric.MeterProvider) (*ingestMetrics, error) {
	if mp == nil {
		mp = noop.NewMeterProvider()
	}
	meter := mp.Meter(meterScopeName)
	// The Prometheus exporter appends `_total` to monotonic sums, so the
	// registered instrument name is the acceptance-criteria metric name
	// minus that suffix.
	c, err := meter.Int64Counter(
		"otelhouse_gateway_ingest_records",
		metric.WithDescription("OTLP records tagged by the tenanttagger processor, labelled by tenant and signal."),
		metric.WithUnit("{record}"),
	)
	if err != nil {
		return nil, fmt.Errorf("tenanttagger: create ingest records counter: %w", err)
	}
	return &ingestMetrics{records: c}, nil
}

// recordIngest attributes n records to (tenant, signal). n<=0 is a no-op —
// an empty batch is a legitimate arrival, and emitting a zero increment
// would still allocate a datapoint the operator does not need to see.
func (m *ingestMetrics) recordIngest(ctx context.Context, tenant, signal string, n int64) {
	if n <= 0 {
		return
	}
	m.records.Add(ctx, n, metric.WithAttributes(
		attribute.String("tenant", tenant),
		attribute.String("signal", signal),
	))
}
