//go:build e2e

// Package main: end-to-end test that asserts the full ingestion stack works.
// Run from inside the Dagger pipeline after the Collector has ingested sample
// telemetry, and asserts against ClickHouse directly through the shared read
// library github.com/guettli/otelhouseview/otelstore — the same library the
// otelhouseview service and UI read with, so a green e2e means the rows this
// repo's write path lands are exactly the rows the reader can see.
//
// This repo owns the write path only; there is no query API here any more.
//
// Gated by the e2e build tag so it is not picked up by the default
// `go test ./...` step in the same module (which runs without the seeded
// Collector data).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/guettli/otelhouseview/otelstore"
)

// pollTimeout caps how long the test waits for the Collector's batch
// processor to flush each signal to ClickHouse. The Collector batches with a
// 1s timeout, so the rows usually land immediately, but we still poll to stay
// robust against slow CI runners — that is the whole reason this test polls.
const pollTimeout = 60 * time.Second

// logRecord is the projection of otel_logs this test asserts on.
type logRecord struct {
	TraceID string
	Body    string
}

func TestE2E_Store(t *testing.T) {
	dsn := os.Getenv("OTELHOUSE_E2E_CLICKHOUSE_DSN")
	if dsn == "" {
		t.Fatal("OTELHOUSE_E2E_CLICKHOUSE_DSN not set; expected by the e2e harness in ci/main.go")
	}
	logsTraceID := os.Getenv("OTELHOUSE_E2E_LOG_TRACE_ID")
	if logsTraceID == "" {
		t.Fatal("OTELHOUSE_E2E_LOG_TRACE_ID not set; expected by the e2e harness in ci/main.go")
	}

	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout+30*time.Second)
	defer cancel()

	store, err := otelstore.OpenClickHouse(ctx, dsn)
	if err != nil {
		t.Fatalf("otelstore.OpenClickHouse(%q): %v", dsn, err)
	}
	defer func() { _ = store.Close() }()

	// ListTraces — poll until the Collector's batch processor has flushed the
	// span rows.
	traces := pollTraces(ctx, t, store)
	if len(traces) == 0 {
		t.Fatalf("ListTraces returned no traces after %s: the emitted spans never reached ClickHouse", pollTimeout)
	}
	first := traces[0]
	if first.TraceID == "" {
		t.Fatalf("ListTraces returned a trace with an empty trace id: %+v", first)
	}

	// GetTrace — the same trace, fetched by id, must carry its spans.
	trace, err := store.GetTrace(ctx, first.TraceID)
	if err != nil {
		if errors.Is(err, otelstore.ErrNotFound) {
			t.Fatalf("GetTrace(%s): trace listed by ListTraces cannot be fetched by id", first.TraceID)
		}
		t.Fatalf("GetTrace(%s): %v", first.TraceID, err)
	}
	if trace.TraceID != first.TraceID {
		t.Fatalf("GetTrace returned trace id %q, want %q", trace.TraceID, first.TraceID)
	}
	if len(trace.Spans) == 0 {
		t.Fatalf("GetTrace(%s) returned no spans", first.TraceID)
	}
	if _, ok := trace.Root(); !ok {
		t.Fatalf("GetTrace(%s) returned %d spans but no root span", first.TraceID, len(trace.Spans))
	}

	// Logs — otelhouse-emit stamped every emitted log record with
	// OTELHOUSE_E2E_LOG_TRACE_ID, so every row for that trace id must come
	// back carrying it.
	//
	// This one query does not go through Store.GetTrace: otelstore's Trace is
	// span-anchored (GetTrace reports ErrNotFound when the trace has no spans)
	// and the seeded log trace deliberately has no spans of its own. DB() is
	// the library's documented escape hatch for exactly this — a caller-owned
	// read-only query on the store's own connection.
	logs := pollLogs(ctx, t, store, logsTraceID)
	if len(logs) == 0 {
		t.Fatalf("no log records for trace id %s after %s: the emitted logs never reached ClickHouse", logsTraceID, pollTimeout)
	}
	for _, lr := range logs {
		if lr.TraceID != logsTraceID {
			t.Fatalf("log record carries trace id %q, want %q (body %q)", lr.TraceID, logsTraceID, lr.Body)
		}
	}
}

// pollTraces calls ListTraces until it returns at least one trace or
// pollTimeout elapses.
func pollTraces(ctx context.Context, t *testing.T, store *otelstore.ClickHouseStore) []otelstore.TraceSummary {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	for time.Now().Before(deadline) {
		got, err := store.ListTraces(ctx, 10)
		if err != nil {
			t.Fatalf("ListTraces: %v", err)
		}
		if len(got) > 0 {
			return got
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled while polling ListTraces: %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}
	return nil
}

// pollLogs reads the log rows for traceID until at least one shows up or
// pollTimeout elapses.
func pollLogs(ctx context.Context, t *testing.T, store *otelstore.ClickHouseStore, traceID string) []logRecord {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	for time.Now().Before(deadline) {
		got, err := logsForTrace(ctx, store, traceID)
		if err != nil {
			t.Fatalf("query logs for trace id %s: %v", traceID, err)
		}
		if len(got) > 0 {
			return got
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled while polling for logs of trace id %s: %v", traceID, ctx.Err())
		case <-time.After(time.Second):
		}
	}
	return nil
}

func logsForTrace(ctx context.Context, store *otelstore.ClickHouseStore, traceID string) ([]logRecord, error) {
	rows, err := store.DB().QueryContext(ctx,
		`SELECT TraceId, Body FROM otel_logs WHERE TraceId = ? ORDER BY Timestamp`, traceID)
	if err != nil {
		return nil, fmt.Errorf("select otel_logs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []logRecord
	for rows.Next() {
		var lr logRecord
		if err := rows.Scan(&lr.TraceID, &lr.Body); err != nil {
			return nil, fmt.Errorf("scan otel_logs row: %w", err)
		}
		out = append(out, lr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate otel_logs rows: %w", err)
	}
	return out, nil
}
