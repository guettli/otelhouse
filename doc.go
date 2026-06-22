// Package otelhouse exports OpenTelemetry traces, metrics and log records to
// ClickHouse.
//
// Spans land in a single MergeTree table (default "otel_traces"); metrics land
// in five tables keyed by aggregation type (default prefix "otel_metrics"
// → "otel_metrics_gauge", "_sum", "_histogram", "_exponential_histogram",
// "_summary"); log records land in a single table (default "otel_logs"). The
// trace and metric schemas mirror the OpenTelemetry Collector contrib
// ClickHouse exporter so Grafana dashboards built for that exporter work
// against tables written by this package. The log schema is native to
// sdklog.Record and is not drop-in compatible with the Collector's logs table.
//
// # DSN format
//
//	clickhouse://[user[:password]@]host[:port]/database[?param=value...]
//
// # Constructors
//
// Each signal has its own exporter and constructor:
//
//   - [New] returns an [*Exporter] that implements
//     go.opentelemetry.io/otel/sdk/trace.SpanExporter.
//   - [NewMetricExporter] returns a [*MetricExporter] that implements
//     go.opentelemetry.io/otel/sdk/metric.Exporter.
//   - [NewLogExporter] returns a [*LogExporter] that implements
//     go.opentelemetry.io/otel/sdk/log.Exporter.
//
// See the package-level examples for end-to-end wiring of each provider.
package otelhouse
