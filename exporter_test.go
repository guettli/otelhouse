package otelhouse_test

import (
	"context"
	"os"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/guettli/otelhouse"
)

// TestExporter_DaggerLikeTrace sends a trace that mimics a Dagger CI pipeline execution
// and verifies the spans are queryable in ClickHouse.
//
// In production, set OTEL_EXPORTER_OTLP_ENDPOINT to a receiver backed by this
// exporter and Dagger will automatically route its own execution traces to ClickHouse.
func TestExporter_DaggerLikeTrace(t *testing.T) {
	dsn := os.Getenv("CLICKHOUSE_DSN")
	if dsn == "" {
		t.Skip("CLICKHOUSE_DSN not set; run via Dagger CI or set it manually")
	}

	ctx := context.Background()

	exp, err := otelhouse.New(ctx, otelhouse.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := exp.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	if err := exp.CreateSchema(ctx); err != nil {
		t.Fatalf("CreateSchema: %v", err)
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

	// WithSyncer exports spans synchronously on span.End(), so no ForceFlush needed.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
		sdktrace.WithResource(res),
	)
	t.Cleanup(func() { tp.Shutdown(ctx) })

	tracer := tp.Tracer("dagger/engine", trace.WithInstrumentationVersion("0.15.0"))

	// Root span: the top-level "do" operation Dagger emits per pipeline run.
	ctx, root := tracer.Start(ctx, "do",
		trace.WithAttributes(
			attribute.String("dagger.op", "do"),
			attribute.Bool("dagger.cached", false),
		),
	)

	// Child: build step (cache hit).
	_, build := tracer.Start(ctx, "build",
		trace.WithAttributes(
			attribute.String("dagger.op", "build"),
			attribute.Bool("dagger.cached", true),
			attribute.String("dagger.image", "golang:1.26-alpine"),
		),
	)
	build.AddEvent("cache-hit", trace.WithAttributes(attribute.String("layer", "go-mod")))
	build.End()

	// Child: lint step.
	_, lint := tracer.Start(ctx, "lint",
		trace.WithAttributes(
			attribute.String("dagger.op", "exec"),
			attribute.String("dagger.cmd", "golangci-lint run ./..."),
		),
	)
	lint.End()

	// Child: test step.
	_, testSpan := tracer.Start(ctx, "test",
		trace.WithAttributes(
			attribute.String("dagger.op", "exec"),
			attribute.String("dagger.cmd", "go test ./..."),
		),
	)
	testSpan.AddEvent("clickhouse-ready")
	testSpan.End()

	root.End()

	// Verify all four spans landed in ClickHouse.
	count, err := exp.SpanCount(ctx, "dagger")
	if err != nil {
		t.Fatalf("SpanCount: %v", err)
	}
	if count < 4 {
		t.Errorf("want >= 4 spans in ClickHouse, got %d", count)
	}
}
