// Package api implements an HTTP API server that queries Dagger traces and
// logs from ClickHouse and serves them to a frontend.
//
// The server exposes three read-only JSON endpoints:
//
//   - GET /api/runs                 list of distinct runs (one per TraceId)
//   - GET /api/traces/{id}          full span hierarchy for a single TraceId
//   - GET /api/logs?traceId={id}    log records matching a TraceId
//
// The endpoints query the schemas produced by either the upstream
// OpenTelemetry Collector ClickHouse exporter (used by the docker-compose
// pipeline in this repo) or by this package's own trace and log exporters:
// the columns referenced (TraceId, SpanId, ParentSpanId, SpanName, SpanKind,
// ServiceName, Timestamp, Duration, StatusCode, StatusMessage,
// ResourceAttributes, SpanAttributes for traces; Timestamp, TraceId, SpanId,
// SeverityNumber, SeverityText, ServiceName, Body, LogAttributes for logs)
// are common to both.
//
// Wire a [*Server] into the standard library [net/http]:
//
//	srv, err := api.New(ctx, api.Config{DSN: "clickhouse://localhost:9000/otel"})
//	if err != nil { ... }
//	defer srv.Close()
//	http.ListenAndServe(":8080", srv.Handler())
package api
