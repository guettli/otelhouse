package otelhouse_test

import (
	"context"
	"log"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/guettli/otelhouse"
)

// ExampleNew wires the trace exporter into an SDK TracerProvider. The DSN
// points at a stock ClickHouse server; in production set it to your cluster.
// No // Output: line is included on purpose: the example compiles but is
// skipped by `go test`, so godoc stays meaningful without requiring a live
// database.
func ExampleNew() {
	ctx := context.Background()

	exp, err := otelhouse.New(ctx, otelhouse.Config{
		DSN: "clickhouse://localhost:9000/default",
	})
	if err != nil {
		log.Fatalf("otelhouse.New: %v", err)
	}

	if err := exp.CreateSchema(ctx); err != nil {
		log.Fatalf("CreateSchema: %v", err)
	}

	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
	defer func() { _ = tp.Shutdown(ctx) }()

	// Register tp with otel.SetTracerProvider in real applications, then use
	// tp.Tracer(...) to emit spans.
	_ = tp
}

// ExampleNewMetricExporter wires the metric exporter into an SDK
// MeterProvider via a PeriodicReader.
func ExampleNewMetricExporter() {
	ctx := context.Background()

	exp, err := otelhouse.NewMetricExporter(ctx, otelhouse.MetricConfig{
		DSN: "clickhouse://localhost:9000/default",
	})
	if err != nil {
		log.Fatalf("otelhouse.NewMetricExporter: %v", err)
	}

	if err := exp.CreateSchema(ctx); err != nil {
		log.Fatalf("CreateSchema: %v", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
	)
	defer func() { _ = mp.Shutdown(ctx) }()

	// Register mp with otel.SetMeterProvider in real applications, then use
	// mp.Meter(...) to record measurements.
	_ = mp
}

// ExampleNewLogExporter wires the log exporter into an SDK LoggerProvider via
// a BatchProcessor.
func ExampleNewLogExporter() {
	ctx := context.Background()

	exp, err := otelhouse.NewLogExporter(ctx, otelhouse.Config{
		DSN: "clickhouse://localhost:9000/default",
	})
	if err != nil {
		log.Fatalf("otelhouse.NewLogExporter: %v", err)
	}

	if err := exp.CreateLogSchema(ctx); err != nil {
		log.Fatalf("CreateLogSchema: %v", err)
	}

	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
	)
	defer func() { _ = lp.Shutdown(ctx) }()

	// Register lp with go.opentelemetry.io/otel/log/global.SetLoggerProvider
	// in real applications, then use lp.Logger(...) to emit log records.
	_ = lp
}
