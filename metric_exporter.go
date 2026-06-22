package otelhouse

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// MetricExporter writes OTel metrics to ClickHouse, one table per data type
// (gauge, sum, histogram, exponential histogram, summary).
type MetricExporter struct {
	conn          driver.Conn
	prefix        string
	retentionDays int
}

// MetricConfig configures the MetricExporter.
type MetricConfig struct {
	// DSN is the ClickHouse connection string.
	// Format: clickhouse://[user[:password]@]host[:port]/database
	DSN string

	// Prefix is the ClickHouse table prefix. Defaults to "otel_metrics".
	// Tables created are <Prefix>_gauge, <Prefix>_sum, <Prefix>_histogram,
	// <Prefix>_exponential_histogram and <Prefix>_summary.
	Prefix string

	// RetentionDays controls the TTL clause baked into CreateSchema for every
	// metric sub-table. A zero value (the default) means 180 days; a positive
	// value uses that many days; a negative value (e.g. -1) omits the TTL
	// clause entirely so retention can be managed out-of-band. Only applies
	// to newly created tables: CreateSchema uses CREATE TABLE IF NOT EXISTS
	// and will not modify the TTL of an existing table.
	RetentionDays int
}

// applyDefaults fills zero-valued MetricConfig fields with their defaults.
// Pure: no I/O, safe to call from unit tests.
func (c *MetricConfig) applyDefaults() {
	if c.Prefix == "" {
		c.Prefix = "otel_metrics"
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = 180
	}
}

// NewMetricExporter opens a ClickHouse connection and returns a ready-to-use
// MetricExporter.
func NewMetricExporter(ctx context.Context, cfg MetricConfig) (*MetricExporter, error) {
	cfg.applyDefaults()
	opts, err := clickhouse.ParseDSN(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &MetricExporter{conn: conn, prefix: cfg.Prefix, retentionDays: cfg.RetentionDays}, nil
}

// CreateSchema creates the five metric tables if they do not exist.
func (e *MetricExporter) CreateSchema(ctx context.Context) error {
	for suffix, stmt := range metricsSchemaSQL(e.prefix, e.retentionDays) {
		warnIfTableExists(ctx, e.conn, e.prefix+"_"+suffix)
		if err := e.conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("create %s_%s: %w", e.prefix, suffix, err)
		}
	}
	return nil
}

// Temporality returns the default cumulative temporality for all instrument
// kinds, matching the OpenTelemetry Collector contrib ClickHouse exporter.
func (e *MetricExporter) Temporality(k sdkmetric.InstrumentKind) metricdata.Temporality {
	return sdkmetric.DefaultTemporalitySelector(k)
}

// Aggregation returns the SDK's default aggregation for the given instrument
// kind.
func (e *MetricExporter) Aggregation(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(k)
}

// Export writes a batch of metric data to ClickHouse, fanning the data points
// out to the table that matches their aggregation type.
func (e *MetricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	resAttrs := kvToMap(rm.Resource.Attributes())
	resSchema := rm.Resource.SchemaURL()
	svcName := resAttrs["service.name"]

	batches, err := newMetricBatches(ctx, e.conn, e.prefix)
	if err != nil {
		return err
	}
	defer batches.closeAll()

	for _, sm := range rm.ScopeMetrics {
		scopeAttrs := kvToMap(sm.Scope.Attributes.ToSlice())
		for _, m := range sm.Metrics {
			if err := batches.appendMetric(svcName, resAttrs, resSchema, sm.Scope, scopeAttrs, m); err != nil {
				return err
			}
		}
	}

	return batches.send()
}

// ForceFlush is a no-op: each Export call commits its batches synchronously.
func (e *MetricExporter) ForceFlush(_ context.Context) error {
	return nil
}

// Shutdown closes the underlying ClickHouse connection.
func (e *MetricExporter) Shutdown(_ context.Context) error {
	return e.conn.Close()
}

// MetricCount returns the number of rows in the named metric sub-table for the
// given service. table must be one of "gauge", "sum", "histogram",
// "exponential_histogram" or "summary".
func (e *MetricExporter) MetricCount(ctx context.Context, serviceName, table string) (uint64, error) {
	full := e.prefix + "_" + table
	row := e.conn.QueryRow(ctx, "SELECT count() FROM "+full+" WHERE ServiceName = ?", serviceName)
	var count uint64
	return count, row.Scan(&count)
}
