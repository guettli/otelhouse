// Command otelhouse-emit drives a small amount of synthetic OTel traffic
// into an OTLP/gRPC endpoint. It exists for the end-to-end harness in
// ci/main.go: a local replacement for `telemetrygen` that uses only the
// OTel Go SDK and so builds in a few seconds, instead of the multi-minute
// `go install` of opentelemetry-collector-contrib's telemetrygen.
//
// Usage: otelhouse-emit -signal {traces|metrics|logs} -endpoint host:port [-count N] [-trace-id hex] [-span-id hex]
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/resource"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
	"go.opentelemetry.io/otel/trace"
)

const defaultServiceName = "otelhouse-emit"

func main() {
	var (
		signal   = flag.String("signal", "", "one of: traces, metrics, logs")
		endpoint = flag.String("endpoint", "", "OTLP/gRPC endpoint (host:port, no scheme)")
		count    = flag.Int("count", 20, "number of records to emit")
		traceID  = flag.String("trace-id", "", "32-hex TraceID stamped on every emitted log record (logs signal only)")
		spanID   = flag.String("span-id", "", "16-hex SpanID stamped on every emitted log record (logs signal only)")
	)
	flag.Parse()

	if *signal == "" || *endpoint == "" {
		fmt.Fprintln(os.Stderr, "otelhouse-emit: -signal and -endpoint are required")
		flag.Usage()
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(defaultServiceName)),
	)
	if err != nil {
		fail("build resource: %v", err)
	}

	switch *signal {
	case "traces":
		if err := emitTraces(ctx, *endpoint, *count, res); err != nil {
			fail("emit traces: %v", err)
		}
	case "metrics":
		if err := emitMetrics(ctx, *endpoint, *count, res); err != nil {
			fail("emit metrics: %v", err)
		}
	case "logs":
		if err := emitLogs(ctx, *endpoint, *count, *traceID, *spanID, res); err != nil {
			fail("emit logs: %v", err)
		}
	default:
		fail("unknown -signal %q (want traces|metrics|logs)", *signal)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "otelhouse-emit: "+format+"\n", args...)
	os.Exit(1)
}

func emitTraces(ctx context.Context, endpoint string, count int, res *resource.Resource) error {
	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return fmt.Errorf("trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	defer func() { _ = tp.Shutdown(ctx) }()

	tracer := tp.Tracer("otelhouse-emit")
	for i := 0; i < count; i++ {
		// One root span per iteration with a single child, so /api/runs
		// returns distinct runs and /api/traces/:id has a real hierarchy.
		ctx, parent := tracer.Start(ctx, fmt.Sprintf("parent-%d", i))
		_, child := tracer.Start(ctx, fmt.Sprintf("child-%d", i))
		child.SetAttributes(attribute.Int("iter", i))
		child.End()
		parent.End()
	}
	return tp.ForceFlush(ctx)
}

func emitMetrics(ctx context.Context, endpoint string, count int, res *resource.Resource) error {
	exp, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithInsecure(),
	)
	if err != nil {
		return fmt.Errorf("metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp,
			sdkmetric.WithInterval(50*time.Millisecond),
		)),
		sdkmetric.WithResource(res),
	)
	defer func() { _ = mp.Shutdown(ctx) }()

	meter := mp.Meter("otelhouse-emit")
	gauge, err := meter.Int64Gauge("otelhouse.emit.iter")
	if err != nil {
		return fmt.Errorf("create gauge: %w", err)
	}
	for i := 0; i < count; i++ {
		gauge.Record(ctx, int64(i))
	}
	return mp.ForceFlush(ctx)
}

func emitLogs(ctx context.Context, endpoint string, count int, traceIDHex, spanIDHex string, res *resource.Resource) error {
	exp, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(endpoint),
		otlploggrpc.WithInsecure(),
	)
	if err != nil {
		return fmt.Errorf("log exporter: %w", err)
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp,
			sdklog.WithExportInterval(50*time.Millisecond),
		)),
		sdklog.WithResource(res),
	)
	defer func() { _ = lp.Shutdown(ctx) }()

	logger := lp.Logger("otelhouse-emit")

	spanCtx, err := buildSpanContext(traceIDHex, spanIDHex)
	if err != nil {
		return fmt.Errorf("invalid -trace-id/-span-id: %w", err)
	}
	logCtx := trace.ContextWithSpanContext(ctx, spanCtx)

	for i := 0; i < count; i++ {
		var rec otellog.Record
		rec.SetTimestamp(time.Now())
		rec.SetObservedTimestamp(time.Now())
		rec.SetSeverity(otellog.SeverityInfo)
		rec.SetSeverityText("INFO")
		rec.SetBody(otellog.StringValue(fmt.Sprintf("iter %d", i)))
		rec.AddAttributes(otellog.Int("iter", i))
		logger.Emit(logCtx, rec)
	}
	return lp.ForceFlush(ctx)
}

// buildSpanContext returns a SpanContext stamped with the supplied trace/span
// ids. Empty inputs return a zero context, which leaves the log record's
// TraceID/SpanID fields unset.
func buildSpanContext(traceIDHex, spanIDHex string) (trace.SpanContext, error) {
	if traceIDHex == "" && spanIDHex == "" {
		return trace.SpanContext{}, nil
	}
	tidBytes, err := hex.DecodeString(traceIDHex)
	if err != nil || len(tidBytes) != 16 {
		return trace.SpanContext{}, fmt.Errorf("trace id must be 32 hex chars (got %q)", traceIDHex)
	}
	sidBytes, err := hex.DecodeString(spanIDHex)
	if err != nil || len(sidBytes) != 8 {
		return trace.SpanContext{}, fmt.Errorf("span id must be 16 hex chars (got %q)", spanIDHex)
	}
	var tid trace.TraceID
	copy(tid[:], tidBytes)
	var sid trace.SpanID
	copy(sid[:], sidBytes)
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid,
		SpanID:  sid,
	}), nil
}
