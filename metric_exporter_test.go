package otelhouse_test

import (
	"context"
	"os"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/guettli/otelhouse"
)

// TestMetricExporter_DaggerLikeMetrics records one of each supported
// instrument kind with attributes a Dagger pipeline would emit and verifies
// the data points land in the correct ClickHouse metric table.
func TestMetricExporter_DaggerLikeMetrics(t *testing.T) {
	dsn := os.Getenv("CLICKHOUSE_DSN")
	if dsn == "" {
		t.Skip("CLICKHOUSE_DSN not set; run via Dagger CI or set it manually")
	}

	ctx := context.Background()

	exp, err := otelhouse.NewMetricExporter(ctx, otelhouse.MetricConfig{DSN: dsn})
	if err != nil {
		t.Fatalf("NewMetricExporter: %v", err)
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
			semconv.ServiceName("dagger-metrics"),
			semconv.ServiceVersion("0.15.0"),
			attribute.String("dagger.engine.host", "localhost"),
		),
	)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
	)
	t.Cleanup(func() {
		if err := mp.Shutdown(ctx); err != nil {
			t.Errorf("meter provider shutdown: %v", err)
		}
	})

	meter := mp.Meter("dagger/engine", metric.WithInstrumentationVersion("0.15.0"))

	// Sum (counter): number of builds executed.
	builds, err := meter.Int64Counter("dagger.builds",
		metric.WithDescription("Total builds executed"),
		metric.WithUnit("{build}"),
	)
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	builds.Add(ctx, 3, metric.WithAttributes(attribute.String("dagger.op", "build")))

	// Gauge (observable): cache size at collection time.
	if _, err := meter.Int64ObservableGauge("dagger.cache.size",
		metric.WithDescription("Engine layer cache size"),
		metric.WithUnit("By"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(42, metric.WithAttributes(attribute.String("dagger.cache", "layer")))
			return nil
		}),
	); err != nil {
		t.Fatalf("Int64ObservableGauge: %v", err)
	}

	// Histogram: step duration.
	stepDur, err := meter.Float64Histogram("dagger.step.duration",
		metric.WithDescription("Pipeline step wall time"),
		metric.WithUnit("s"),
	)
	if err != nil {
		t.Fatalf("Float64Histogram: %v", err)
	}
	stepDur.Record(ctx, 0.12, metric.WithAttributes(attribute.String("dagger.op", "exec")))
	stepDur.Record(ctx, 0.34, metric.WithAttributes(attribute.String("dagger.op", "exec")))

	// Collect from the SDK and export to ClickHouse.
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	if err := exp.Export(ctx, &rm); err != nil {
		t.Fatalf("Export: %v", err)
	}

	for _, tc := range []struct {
		table string
		want  uint64
	}{
		{"sum", 1},
		{"gauge", 1},
		{"histogram", 1},
	} {
		got, err := exp.MetricCount(ctx, "dagger-metrics", tc.table)
		if err != nil {
			t.Fatalf("MetricCount(%s): %v", tc.table, err)
		}
		if got < tc.want {
			t.Errorf("table %s: want >= %d rows, got %d", tc.table, tc.want, got)
		}
	}
}
