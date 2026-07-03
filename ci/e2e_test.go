//go:build e2e

// Package main: end-to-end test that asserts the full ingestion + query
// stack works. Run from inside the Dagger pipeline after the Collector has
// ingested sample telemetry and the otelhouse-api binary is up as a
// service.
//
// Gated by the e2e build tag so it is not picked up by the default
// `go test ./...` step in the same module (which runs without the API
// service binding).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

// pollTimeout caps how long the test waits for the Collector's batch
// processor to flush each signal to ClickHouse.
const pollTimeout = 60 * time.Second

type run struct {
	TraceID    string `json:"trace_id"`
	SpanCount  uint64 `json:"span_count"`
	StatusCode string `json:"status_code"`
}

type span struct {
	SpanID string `json:"span_id"`
	Name   string `json:"name"`
}

type traceResp struct {
	TraceID string `json:"trace_id"`
	Spans   []span `json:"spans"`
}

type logRecord struct {
	TraceID string `json:"trace_id"`
	Body    string `json:"body"`
}

func TestE2E_API(t *testing.T) {
	apiURL := os.Getenv("OTELHOUSE_API_URL")
	if apiURL == "" {
		t.Fatal("OTELHOUSE_API_URL not set; expected by the e2e harness in ci/main.go")
	}
	logsTraceID := os.Getenv("OTELHOUSE_E2E_LOG_TRACE_ID")
	if logsTraceID == "" {
		t.Fatal("OTELHOUSE_E2E_LOG_TRACE_ID not set; expected by the e2e harness in ci/main.go")
	}

	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout+30*time.Second)
	defer cancel()

	// /api/runs — poll until the Collector's batch processor has flushed
	// the trace rows. The Collector batches with a 1s timeout, so this
	// usually returns immediately, but we still wait to be robust against
	// slow CI runners.
	runs := pollRuns(ctx, t, apiURL)
	if len(runs) == 0 {
		t.Fatalf("/api/runs returned no runs after %s", pollTimeout)
	}
	first := runs[0]
	if first.TraceID == "" {
		t.Fatalf("/api/runs returned run with empty trace_id: %+v", first)
	}
	if first.SpanCount == 0 {
		t.Fatalf("/api/runs returned run with zero spans: %+v", first)
	}

	// /api/traces/{id} — the same trace, fetched directly.
	trace := getTrace(ctx, t, apiURL, first.TraceID)
	if trace.TraceID != first.TraceID {
		t.Fatalf("/api/traces returned trace_id %q, want %q", trace.TraceID, first.TraceID)
	}
	if len(trace.Spans) == 0 {
		t.Fatalf("/api/traces/%s returned no spans", first.TraceID)
	}

	// /api/logs?traceId=... — telemetrygen stamped every log record with
	// OTELHOUSE_E2E_LOG_TRACE_ID, so the API must return all of them.
	logs := pollLogs(ctx, t, apiURL, logsTraceID)
	if len(logs) == 0 {
		t.Fatalf("/api/logs?traceId=%s returned no logs after %s", logsTraceID, pollTimeout)
	}
	for _, lr := range logs {
		if lr.TraceID != logsTraceID {
			t.Fatalf("/api/logs returned log with trace_id %q, want %q", lr.TraceID, logsTraceID)
		}
	}
}

func pollRuns(ctx context.Context, t *testing.T, apiURL string) []run {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	var last []run
	for time.Now().Before(deadline) {
		var got []run
		if err := getJSON(ctx, apiURL+"/api/runs", &got); err != nil {
			t.Fatalf("GET /api/runs: %v", err)
		}
		if len(got) > 0 {
			return got
		}
		last = got
		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled while polling /api/runs: %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}
	t.Fatalf("/api/runs still empty after %s (last response: %v)", pollTimeout, last)
	return nil
}

func getTrace(ctx context.Context, t *testing.T, apiURL, traceID string) traceResp {
	t.Helper()
	var tr traceResp
	if err := getJSON(ctx, apiURL+"/api/traces/"+traceID, &tr); err != nil {
		t.Fatalf("GET /api/traces/%s: %v", traceID, err)
	}
	return tr
}

func pollLogs(ctx context.Context, t *testing.T, apiURL, traceID string) []logRecord {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	u := apiURL + "/api/logs?traceId=" + url.QueryEscape(traceID)
	for time.Now().Before(deadline) {
		var got []logRecord
		if err := getJSON(ctx, u, &got); err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		if len(got) > 0 {
			return got
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled while polling /api/logs: %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}
	return nil
}

func getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s -> %d: %s", u, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.Join(fmt.Errorf("decode %s", u), err)
	}
	return nil
}
