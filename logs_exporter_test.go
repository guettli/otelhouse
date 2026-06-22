package otelhouse_test

import (
	"context"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/guettli/otelhouse"
)

// TestLogExporter_DaggerLikeLogs emits logs that mimic a Dagger CI pipeline
// run and verifies they are queryable in ClickHouse. Mirrors
// TestExporter_DaggerLikeTrace.
func TestLogExporter_DaggerLikeLogs(t *testing.T) {
	dsn := os.Getenv("CLICKHOUSE_DSN")
	if dsn == "" {
		t.Skip("CLICKHOUSE_DSN not set; run via Dagger CI or set it manually")
	}

	ctx := context.Background()

	exp, err := otelhouse.NewLogExporter(ctx, otelhouse.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("NewLogExporter: %v", err)
	}
	t.Cleanup(func() {
		if err := exp.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	if err := exp.CreateLogSchema(ctx); err != nil {
		t.Fatalf("CreateLogSchema: %v", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("dagger"),
			semconv.ServiceVersion("0.15.0"),
			attribute.String("dagger.engine.host", "localhost"),
		),
	)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}

	// SimpleProcessor exports synchronously on every Emit, so no flush wait
	// is needed before the count query below.
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)),
		sdklog.WithResource(res),
	)
	t.Cleanup(func() {
		if err := lp.Shutdown(ctx); err != nil {
			t.Errorf("LoggerProvider.Shutdown: %v", err)
		}
	})

	logger := lp.Logger("dagger/engine", log.WithInstrumentationVersion("0.15.0"))

	// 1. INFO: pipeline start, no trace context.
	{
		var r log.Record
		r.SetTimestamp(time.Now())
		r.SetSeverity(log.SeverityInfo)
		r.SetSeverityText("INFO")
		r.SetBody(log.StringValue("pipeline started"))
		r.AddAttributes(log.String("dagger.op", "do"))
		logger.Emit(ctx, r)
	}

	// 2. WARN: cache hit.
	{
		var r log.Record
		r.SetTimestamp(time.Now())
		r.SetSeverity(log.SeverityWarn)
		r.SetSeverityText("WARN")
		r.SetBody(log.StringValue("build step served from cache"))
		r.AddAttributes(
			log.String("dagger.op", "build"),
			log.Bool("dagger.cached", true),
		)
		logger.Emit(ctx, r)
	}

	// 3. ERROR: command failure carrying a trace/span context.
	{
		traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
		if err != nil {
			t.Fatalf("TraceIDFromHex: %v", err)
		}
		spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
		if err != nil {
			t.Fatalf("SpanIDFromHex: %v", err)
		}
		sc := trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     spanID,
			TraceFlags: trace.FlagsSampled,
			Remote:     true,
		})
		errCtx := trace.ContextWithSpanContext(ctx, sc)

		var r log.Record
		r.SetTimestamp(time.Now())
		r.SetSeverity(log.SeverityError)
		r.SetSeverityText("ERROR")
		r.SetBody(log.StringValue("exec failed: golangci-lint run ./..."))
		r.AddAttributes(
			log.String("dagger.op", "exec"),
			log.String("dagger.cmd", "golangci-lint run ./..."),
			log.Int64("exit_code", 1),
		)
		logger.Emit(errCtx, r)
	}

	count, err := exp.LogCount(ctx, "dagger")
	if err != nil {
		t.Fatalf("LogCount: %v", err)
	}
	if count < 3 {
		t.Errorf("want >= 3 log records in ClickHouse, got %d", count)
	}
}
