package api_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/guettli/otelhouse"
	"github.com/guettli/otelhouse/api"
)

// TestServer_RoundTrip seeds traces and logs through the otelhouse exporters
// and verifies the API endpoints return the expected data.
//
// The endpoints query the same schema otelhouse writes to, which mirrors the
// upstream Collector clickhouse exporter for the columns referenced here, so
// this test doubles as coverage for the Collector-backed pipeline produced by
// docker-compose.yml.
func TestServer_RoundTrip(t *testing.T) {
	dsn := os.Getenv("CLICKHOUSE_DSN")
	if dsn == "" {
		t.Skip("CLICKHOUSE_DSN not set; run via Dagger CI or set it manually")
	}

	ctx := context.Background()

	// Each test run uses its own tables to avoid cross-talk with other
	// integration tests landing in the shared CLICKHOUSE_DSN. Suffix is
	// process-pid + unix-nano to be unique within a single CI run.
	suffix := time.Now().UnixNano()
	tracesTable := otelTable("api_test_traces_", suffix)
	logsTable := otelTable("api_test_logs_", suffix)

	traceExp, err := otelhouse.New(ctx, otelhouse.Config{DSN: dsn, Table: tracesTable})
	if err != nil {
		t.Fatalf("otelhouse.New: %v", err)
	}
	t.Cleanup(func() {
		if err := traceExp.Shutdown(ctx); err != nil {
			t.Errorf("trace shutdown: %v", err)
		}
	})
	if err := traceExp.CreateSchema(ctx); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}

	logExp, err := otelhouse.NewLogExporter(ctx, otelhouse.Config{DSN: dsn, Table: logsTable})
	if err != nil {
		t.Fatalf("otelhouse.NewLogExporter: %v", err)
	}
	t.Cleanup(func() {
		if err := logExp.Shutdown(ctx); err != nil {
			t.Errorf("log shutdown: %v", err)
		}
	})
	if err := logExp.CreateLogSchema(ctx); err != nil {
		t.Fatalf("CreateLogSchema: %v", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("dagger"),
			semconv.ServiceVersion("0.15.0"),
			attribute.String("dagger.engine.host", "ci-host"),
		),
	)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(traceExp),
		sdktrace.WithResource(res),
	)
	t.Cleanup(func() {
		if err := tp.Shutdown(ctx); err != nil {
			t.Errorf("tracer provider shutdown: %v", err)
		}
	})

	tracer := tp.Tracer("dagger/engine")
	ctxRun, root := tracer.Start(ctx, "do",
		trace.WithAttributes(
			attribute.String("dagger.op", "do"),
			attribute.String("dagger.cmd", "dagger call test"),
		),
	)
	rootSC := root.SpanContext()
	_, child := tracer.Start(ctxRun, "build",
		trace.WithAttributes(attribute.String("dagger.op", "build")),
	)
	childSC := child.SpanContext()
	child.End()
	root.End()

	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(logExp)),
		sdklog.WithResource(res),
	)
	t.Cleanup(func() {
		if err := lp.Shutdown(ctx); err != nil {
			t.Errorf("logger provider shutdown: %v", err)
		}
	})

	logger := lp.Logger("dagger/engine")
	// Two log records that share the trace's context, one INFO and one ERROR,
	// so the /api/logs endpoint has something to order by timestamp.
	for i, body := range []string{"start", "fail"} {
		var r log.Record
		r.SetTimestamp(time.Now().Add(time.Duration(i) * time.Millisecond))
		switch body {
		case "start":
			r.SetSeverity(log.SeverityInfo)
			r.SetSeverityText("INFO")
		case "fail":
			r.SetSeverity(log.SeverityError)
			r.SetSeverityText("ERROR")
		}
		r.SetBody(log.StringValue(body))
		logger.Emit(trace.ContextWithSpanContext(ctx, rootSC), r)
	}

	srv, err := api.New(ctx, api.Config{
		DSN:         dsn,
		TracesTable: tracesTable,
		LogsTable:   logsTable,
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("api close: %v", err)
		}
	})

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	traceID := rootSC.TraceID().String()

	t.Run("runs lists the seeded trace", func(t *testing.T) {
		var runs []api.Run
		getJSON(t, ts.URL+"/api/runs", &runs)

		var got *api.Run
		for i := range runs {
			if runs[i].TraceID == traceID {
				got = &runs[i]
				break
			}
		}
		if got == nil {
			t.Fatalf("trace %s not in /api/runs (got %d runs)", traceID, len(runs))
		}
		if got.ServiceName != "dagger" {
			t.Errorf("ServiceName = %q, want %q", got.ServiceName, "dagger")
		}
		if got.SpanCount != 2 {
			t.Errorf("SpanCount = %d, want 2", got.SpanCount)
		}
		if got.ResourceAttributes["service.name"] != "dagger" {
			t.Errorf(`ResourceAttributes["service.name"] = %q, want "dagger"`,
				got.ResourceAttributes["service.name"])
		}
		if got.StartTime.IsZero() {
			t.Error("StartTime is zero")
		}
		if got.DurationNs <= 0 {
			t.Errorf("DurationNs = %d, want > 0", got.DurationNs)
		}
		if got.Command != "dagger call test" {
			t.Errorf("Command = %q, want %q", got.Command, "dagger call test")
		}
		// StatusCode is "Unset" for unended-error spans, but the root here
		// finished normally — accept the OK/Unset variants the OTel SDK
		// produces and that ClickHouse stores verbatim.
		switch got.StatusCode {
		case "STATUS_CODE_UNSET", "Unset", "STATUS_CODE_OK", "Ok":
			// ok
		default:
			t.Errorf("StatusCode = %q, want Unset/Ok variant", got.StatusCode)
		}
	})

	t.Run("trace returns the span hierarchy", func(t *testing.T) {
		var trc api.Trace
		getJSON(t, ts.URL+"/api/traces/"+traceID, &trc)

		if trc.TraceID != traceID {
			t.Errorf("TraceID = %q, want %q", trc.TraceID, traceID)
		}
		if len(trc.Spans) != 2 {
			t.Fatalf("len(Spans) = %d, want 2", len(trc.Spans))
		}

		var rootSpan, childSpan *api.Span
		for i := range trc.Spans {
			switch trc.Spans[i].Name {
			case "do":
				rootSpan = &trc.Spans[i]
			case "build":
				childSpan = &trc.Spans[i]
			}
		}
		if rootSpan == nil || childSpan == nil {
			t.Fatalf("missing root or child span: %+v", trc.Spans)
		}
		if rootSpan.ParentSpanID != strings.Repeat("0", 16) {
			t.Errorf("root ParentSpanID = %q, want all-zero", rootSpan.ParentSpanID)
		}
		if childSpan.ParentSpanID != rootSC.SpanID().String() {
			t.Errorf("child ParentSpanID = %q, want root SpanID %q",
				childSpan.ParentSpanID, rootSC.SpanID().String())
		}
		if childSpan.SpanID != childSC.SpanID().String() {
			t.Errorf("child SpanID = %q, want %q", childSpan.SpanID, childSC.SpanID().String())
		}
		if rootSpan.DurationNs <= 0 {
			t.Errorf("root DurationNs = %d, want > 0", rootSpan.DurationNs)
		}
		if rootSpan.SpanAttributes["dagger.op"] != "do" {
			t.Errorf(`root attribute["dagger.op"] = %q, want "do"`,
				rootSpan.SpanAttributes["dagger.op"])
		}
	})

	t.Run("trace 404 on unknown id", func(t *testing.T) {
		unknown := hex.EncodeToString(make([]byte, 16)) // 32 hex zeros
		resp, err := http.Get(ts.URL + "/api/traces/" + unknown)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("trace 400 on malformed id", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/traces/not-hex!")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("logs filters by traceId and orders by timestamp", func(t *testing.T) {
		var logs []api.LogRecord
		getJSON(t, ts.URL+"/api/logs?traceId="+traceID, &logs)

		if len(logs) != 2 {
			t.Fatalf("len(logs) = %d, want 2 (got=%+v)", len(logs), logs)
		}
		for _, lr := range logs {
			if lr.TraceID != traceID {
				t.Errorf("TraceID = %q, want %q", lr.TraceID, traceID)
			}
			if lr.ServiceName != "dagger" {
				t.Errorf("ServiceName = %q, want %q", lr.ServiceName, "dagger")
			}
		}
		if logs[0].Body != "start" || logs[1].Body != "fail" {
			t.Errorf("body order = [%q, %q], want [start, fail]", logs[0].Body, logs[1].Body)
		}
		if logs[0].SeverityText != "INFO" || logs[1].SeverityText != "ERROR" {
			t.Errorf("severity order = [%q, %q], want [INFO, ERROR]",
				logs[0].SeverityText, logs[1].SeverityText)
		}
	})

	t.Run("logs 400 on missing traceId", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/logs")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("runs limit query parameter is honoured", func(t *testing.T) {
		var runs []api.Run
		getJSON(t, ts.URL+"/api/runs?limit=1", &runs)
		if len(runs) != 1 {
			t.Errorf("len(runs) = %d, want 1", len(runs))
		}
	})

	t.Run("runs limit rejects invalid input", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/runs?limit=abc")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

// otelTable composes a ClickHouse-safe table name from a base and a suffix.
// ClickHouse identifiers can contain letters, digits and underscores; we
// keep the form simple to avoid quoting in CREATE/SELECT statements.
func otelTable(prefix string, suffix int64) string {
	return prefix + decimal(suffix)
}

func decimal(n int64) string {
	if n == 0 {
		return "0"
	}
	if n < 0 {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}
