package api

import (
	"context"
	"fmt"
	"time"
)

// queryRuns returns up to `limit` distinct runs, newest first. A run is
// identified by its TraceId and projected with the service name, the
// earliest span start, the latest span end (start + duration), and the
// resource attributes of one representative span.
func (s *Server) queryRuns(ctx context.Context, limit int) ([]Run, error) {
	// Duration is stored as Int64 by this package and as UInt64 by the
	// upstream OTel Collector clickhouse exporter; toInt64 normalises both.
	query := fmt.Sprintf(`
SELECT
    TraceId,
    any(ServiceName)                                                   AS ServiceName,
    min(Timestamp)                                                     AS StartTime,
    max(toUnixTimestamp64Nano(Timestamp) + toInt64(Duration))          AS EndUnixNano,
    count()                                                            AS SpanCount,
    any(ResourceAttributes)                                            AS ResourceAttributes
FROM %s
GROUP BY TraceId
ORDER BY StartTime DESC
LIMIT ?
`, s.tracesTable)

	rows, err := s.conn.Query(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("query runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	runs := make([]Run, 0)
	for rows.Next() {
		var (
			run         Run
			endUnixNano int64
		)
		if err := rows.Scan(
			&run.TraceID,
			&run.ServiceName,
			&run.StartTime,
			&endUnixNano,
			&run.SpanCount,
			&run.ResourceAttributes,
		); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		run.EndTime = time.Unix(0, endUnixNano).UTC()
		run.StartTime = run.StartTime.UTC()
		run.DurationNs = endUnixNano - run.StartTime.UnixNano()
		if run.DurationNs < 0 {
			run.DurationNs = 0
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runs: %w", err)
	}
	return runs, nil
}

// queryTrace returns the full span hierarchy for a TraceId, ordered by
// start time so the caller can render a waterfall without re-sorting.
func (s *Server) queryTrace(ctx context.Context, traceID string) (Trace, error) {
	query := fmt.Sprintf(`
SELECT
    SpanId,
    ParentSpanId,
    SpanName,
    SpanKind,
    ServiceName,
    Timestamp,
    toInt64(Duration) AS Duration,
    StatusCode,
    StatusMessage,
    SpanAttributes
FROM %s
WHERE TraceId = ?
ORDER BY Timestamp ASC
`, s.tracesTable)

	rows, err := s.conn.Query(ctx, query, traceID)
	if err != nil {
		return Trace{}, fmt.Errorf("query trace: %w", err)
	}
	defer func() { _ = rows.Close() }()

	trace := Trace{TraceID: traceID, Spans: []Span{}}
	for rows.Next() {
		var sp Span
		if err := rows.Scan(
			&sp.SpanID,
			&sp.ParentSpanID,
			&sp.Name,
			&sp.Kind,
			&sp.ServiceName,
			&sp.StartTime,
			&sp.DurationNs,
			&sp.StatusCode,
			&sp.StatusMessage,
			&sp.SpanAttributes,
		); err != nil {
			return Trace{}, fmt.Errorf("scan span: %w", err)
		}
		sp.StartTime = sp.StartTime.UTC()
		trace.Spans = append(trace.Spans, sp)
	}
	if err := rows.Err(); err != nil {
		return Trace{}, fmt.Errorf("iterate trace: %w", err)
	}
	return trace, nil
}

// queryLogs returns log records carrying a given TraceId, ordered by
// timestamp ascending.
func (s *Server) queryLogs(ctx context.Context, traceID string) ([]LogRecord, error) {
	// SeverityNumber is UInt8 in this package's schema and in v0.110 of the
	// upstream Collector; newer Collector versions widen it to Int32. CAST
	// to UInt8 — OTLP severity is 1..24, so the narrow range never overflows.
	query := fmt.Sprintf(`
SELECT
    Timestamp,
    TraceId,
    SpanId,
    toUInt8(SeverityNumber) AS SeverityNumber,
    SeverityText,
    ServiceName,
    Body,
    LogAttributes
FROM %s
WHERE TraceId = ?
ORDER BY Timestamp ASC
`, s.logsTable)

	rows, err := s.conn.Query(ctx, query, traceID)
	if err != nil {
		return nil, fmt.Errorf("query logs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	logs := make([]LogRecord, 0)
	for rows.Next() {
		var lr LogRecord
		if err := rows.Scan(
			&lr.Timestamp,
			&lr.TraceID,
			&lr.SpanID,
			&lr.SeverityNumber,
			&lr.SeverityText,
			&lr.ServiceName,
			&lr.Body,
			&lr.LogAttributes,
		); err != nil {
			return nil, fmt.Errorf("scan log: %w", err)
		}
		lr.Timestamp = lr.Timestamp.UTC()
		logs = append(logs, lr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate logs: %w", err)
	}
	return logs, nil
}
