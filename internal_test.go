package otelhouse

import (
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// TestKVToMap exercises kvToMap across the attribute types listed in the issue.
//
// kvToMap currently renders each value via attribute.Value.AsString(), which only
// returns the underlying text for STRING-typed values; other typed values yield
// the empty string. These tests pin that contract so a future intentional change
// (e.g. switching to Value.String() to also render ints/bools/floats/slices) is
// visible as a test diff rather than a silent behavioral shift in the rows
// written to ClickHouse.
func TestKVToMap(t *testing.T) {
	tests := []struct {
		name string
		in   []attribute.KeyValue
		want map[string]string
	}{
		{
			name: "empty",
			in:   nil,
			want: map[string]string{},
		},
		{
			name: "string",
			in:   []attribute.KeyValue{attribute.String("k", "v")},
			want: map[string]string{"k": "v"},
		},
		{
			name: "int64",
			in:   []attribute.KeyValue{attribute.Int64("n", 42)},
			want: map[string]string{"n": ""},
		},
		{
			name: "bool",
			in:   []attribute.KeyValue{attribute.Bool("b", true)},
			want: map[string]string{"b": ""},
		},
		{
			name: "float64",
			in:   []attribute.KeyValue{attribute.Float64("f", 1.5)},
			want: map[string]string{"f": ""},
		},
		{
			name: "string slice",
			in:   []attribute.KeyValue{attribute.StringSlice("xs", []string{"a", "b"})},
			want: map[string]string{"xs": ""},
		},
		{
			name: "mixed",
			in: []attribute.KeyValue{
				attribute.String("svc", "dagger"),
				attribute.Int64("n", 7),
				attribute.Bool("cached", true),
			},
			want: map[string]string{
				"svc":    "dagger",
				"n":      "",
				"cached": "",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kvToMap(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d (got=%v)", len(got), len(tt.want), got)
			}
			for k, w := range tt.want {
				if g, ok := got[k]; !ok || g != w {
					t.Errorf("key %q = %q (present=%v), want %q", k, g, ok, w)
				}
			}
		})
	}
}

// TestKVToMap_ServiceName mirrors the ExportSpans lookup at exporter.go:66,
// which reads "service.name" out of the resource-attribute map after kvToMap.
func TestKVToMap_ServiceName(t *testing.T) {
	attrs := []attribute.KeyValue{
		semconv.ServiceName("svc-x"),
		semconv.ServiceVersion("0.1.0"),
		attribute.String("dagger.engine.host", "localhost"),
	}
	m := kvToMap(attrs)
	if got := m["service.name"]; got != "svc-x" {
		t.Errorf(`m["service.name"] = %q, want "svc-x"`, got)
	}
	if got := m["service.version"]; got != "0.1.0" {
		t.Errorf(`m["service.version"] = %q, want "0.1.0"`, got)
	}
}

func TestConfigApplyDefaults(t *testing.T) {
	tests := []struct {
		name string
		in   Config
		want Config
	}{
		{
			name: "empty table defaults to otel_traces and 180-day retention",
			in:   Config{},
			want: Config{Table: "otel_traces", RetentionDays: 180},
		},
		{
			name: "explicit table is preserved",
			in:   Config{Table: "custom_traces"},
			want: Config{Table: "custom_traces", RetentionDays: 180},
		},
		{
			name: "empty DSN is not invented",
			in:   Config{Table: "t"},
			want: Config{Table: "t", RetentionDays: 180},
		},
		{
			name: "DSN is preserved",
			in:   Config{DSN: "clickhouse://localhost:9000/db"},
			want: Config{DSN: "clickhouse://localhost:9000/db", Table: "otel_traces", RetentionDays: 180},
		},
		{
			name: "explicit retention is preserved",
			in:   Config{RetentionDays: 30},
			want: Config{Table: "otel_traces", RetentionDays: 30},
		},
		{
			name: "negative retention sentinel survives defaulting",
			in:   Config{RetentionDays: -1},
			want: Config{Table: "otel_traces", RetentionDays: -1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in
			got.applyDefaults()
			if got != tt.want {
				t.Errorf("applyDefaults = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestMetricConfigApplyDefaults(t *testing.T) {
	tests := []struct {
		name string
		in   MetricConfig
		want MetricConfig
	}{
		{
			name: "empty prefix defaults to otel_metrics and 180-day retention",
			in:   MetricConfig{},
			want: MetricConfig{Prefix: "otel_metrics", RetentionDays: 180},
		},
		{
			name: "explicit prefix is preserved",
			in:   MetricConfig{Prefix: "custom_metrics"},
			want: MetricConfig{Prefix: "custom_metrics", RetentionDays: 180},
		},
		{
			name: "DSN is preserved",
			in:   MetricConfig{DSN: "clickhouse://localhost:9000/db"},
			want: MetricConfig{DSN: "clickhouse://localhost:9000/db", Prefix: "otel_metrics", RetentionDays: 180},
		},
		{
			name: "explicit retention is preserved",
			in:   MetricConfig{RetentionDays: 30},
			want: MetricConfig{Prefix: "otel_metrics", RetentionDays: 30},
		},
		{
			name: "negative retention sentinel survives defaulting",
			in:   MetricConfig{RetentionDays: -1},
			want: MetricConfig{Prefix: "otel_metrics", RetentionDays: -1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in
			got.applyDefaults()
			if got != tt.want {
				t.Errorf("applyDefaults = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSchemaSQL(t *testing.T) {
	const table = "unit_test_traces"
	sql := schemaSQL(table, 180)

	if sql == "" {
		t.Fatal("schemaSQL returned empty string")
	}
	if !strings.Contains(sql, table) {
		t.Errorf("schemaSQL does not contain table name %q:\n%s", table, sql)
	}

	wantSubstrings := []string{
		"CREATE TABLE IF NOT EXISTS",
		"MergeTree()",
		"ORDER BY",
		"PARTITION BY toDate(Timestamp)",
		"ServiceName",
		"ResourceAttributes",
		"SpanAttributes",
		"EventAttributes",
		"LinkAttributes",
		"TTL toDateTime(Timestamp) + toIntervalDay(180)",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(sql, want) {
			t.Errorf("schemaSQL missing substring %q", want)
		}
	}

	// Idempotent: same input → same output.
	if schemaSQL(table, 180) != sql {
		t.Error("schemaSQL is not deterministic for the same input")
	}
}

func TestSchemaSQL_Retention(t *testing.T) {
	const table = "unit_test_traces"
	tests := []struct {
		name        string
		days        int
		mustContain []string
		mustNot     []string
	}{
		{
			name:        "default 180 days",
			days:        180,
			mustContain: []string{"TTL toDateTime(Timestamp)", "toIntervalDay(180)"},
		},
		{
			name:        "custom 30 days",
			days:        30,
			mustContain: []string{"TTL toDateTime(Timestamp)", "toIntervalDay(30)"},
			mustNot:     []string{"toIntervalDay(180)"},
		},
		{
			name:    "negative omits TTL",
			days:    -1,
			mustNot: []string{"TTL ", "toIntervalDay("},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql := schemaSQL(table, tt.days)
			for _, s := range tt.mustContain {
				if !strings.Contains(sql, s) {
					t.Errorf("schemaSQL(%d) missing %q:\n%s", tt.days, s, sql)
				}
			}
			for _, s := range tt.mustNot {
				if strings.Contains(sql, s) {
					t.Errorf("schemaSQL(%d) unexpectedly contains %q:\n%s", tt.days, s, sql)
				}
			}
		})
	}
}

func TestMetricsSchemaSQL_Retention(t *testing.T) {
	const prefix = "unit_test_metrics"
	tests := []struct {
		name        string
		days        int
		mustContain []string
		mustNot     []string
	}{
		{
			name:        "default 180 days",
			days:        180,
			mustContain: []string{"TTL toDateTime(TimeUnix)", "toIntervalDay(180)"},
		},
		{
			name:        "custom 30 days",
			days:        30,
			mustContain: []string{"TTL toDateTime(TimeUnix)", "toIntervalDay(30)"},
			mustNot:     []string{"toIntervalDay(180)"},
		},
		{
			name:    "negative omits TTL",
			days:    -1,
			mustNot: []string{"TTL ", "toIntervalDay("},
		},
	}
	wantSuffixes := []string{"gauge", "sum", "histogram", "exponential_histogram", "summary"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmts := metricsSchemaSQL(prefix, tt.days)
			for _, suffix := range wantSuffixes {
				sql, ok := stmts[suffix]
				if !ok {
					t.Fatalf("metricsSchemaSQL missing %q statement", suffix)
				}
				for _, s := range tt.mustContain {
					if !strings.Contains(sql, s) {
						t.Errorf("%s_%s(%d) missing %q:\n%s", prefix, suffix, tt.days, s, sql)
					}
				}
				for _, s := range tt.mustNot {
					if strings.Contains(sql, s) {
						t.Errorf("%s_%s(%d) unexpectedly contains %q:\n%s", prefix, suffix, tt.days, s, sql)
					}
				}
			}
		})
	}
}
