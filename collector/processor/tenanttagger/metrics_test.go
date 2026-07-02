package tenanttagger

import (
	"context"
	"testing"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newTestMetrics builds an ingestMetrics backed by a real SDK MeterProvider
// paired with a manual reader — the pattern the extension side uses too,
// so both suites are consistent.
func newTestMetrics(t *testing.T) (*ingestMetrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := newIngestMetrics(mp)
	if err != nil {
		t.Fatalf("newIngestMetrics: %v", err)
	}
	return m, reader
}

// ingestCount returns the value of the ingest counter for the (tenant,
// signal) datapoint or 0 if the series was not emitted.
func ingestCount(t *testing.T, reader *sdkmetric.ManualReader, tenant, signal string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "otelhouse_gateway_ingest_records" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %q is %T, want metricdata.Sum[int64]", m.Name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				tv, _ := dp.Attributes.Value(attribute.Key("tenant"))
				sv, _ := dp.Attributes.Value(attribute.Key("signal"))
				if tv.AsString() == tenant && sv.AsString() == signal {
					return dp.Value
				}
			}
		}
	}
	return 0
}

// tracesN builds a Traces batch with n spans under one ResourceSpans, so
// SpanCount() == n. Used to prove the counter reflects record volume, not
// batch count.
func tracesN(n int) ptrace.Traces {
	td := ptrace.NewTraces()
	ss := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty()
	for i := 0; i < n; i++ {
		ss.Spans().AppendEmpty().SetName("op")
	}
	return td
}

func logsN(n int) plog.Logs {
	ld := plog.NewLogs()
	sl := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty()
	for i := 0; i < n; i++ {
		sl.LogRecords().AppendEmpty().Body().SetStr("hi")
	}
	return ld
}

// metricsN builds a Metrics batch with n Gauge datapoints — DataPointCount()
// counts data points, not metric definitions, so we cover the same n=count
// semantics as traces/logs.
func metricsN(n int) pmetric.Metrics {
	md := pmetric.NewMetrics()
	sm := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()
	g := sm.Metrics().AppendEmpty()
	g.SetName("m")
	gauge := g.SetEmptyGauge()
	for i := 0; i < n; i++ {
		gauge.DataPoints().AppendEmpty().SetIntValue(int64(i))
	}
	return md
}

// TestIngest_PerTenantPerSignal is the acceptance test: the counter
// increments per (tenant, signal) with the right value. Two tenants under
// load must not blur into one series.
func TestIngest_PerTenantPerSignal(t *testing.T) {
	cfg := Config{AttributeKey: "tenant", AuthAttribute: "tenant"}
	m, reader := newTestMetrics(t)

	// Alice: 5 traces, 3 logs, 7 metrics data points.
	if _, err := processTraces(ctxWithTenant("alice"), cfg, m, tracesN(5)); err != nil {
		t.Fatalf("processTraces alice: %v", err)
	}
	if _, err := processLogs(ctxWithTenant("alice"), cfg, m, logsN(3)); err != nil {
		t.Fatalf("processLogs alice: %v", err)
	}
	if _, err := processMetrics(ctxWithTenant("alice"), cfg, m, metricsN(7)); err != nil {
		t.Fatalf("processMetrics alice: %v", err)
	}
	// Bob: 2 traces. Should NOT bleed into alice's series.
	if _, err := processTraces(ctxWithTenant("bob"), cfg, m, tracesN(2)); err != nil {
		t.Fatalf("processTraces bob: %v", err)
	}

	if got := ingestCount(t, reader, "alice", "traces"); got != 5 {
		t.Errorf("alice traces = %d, want 5", got)
	}
	if got := ingestCount(t, reader, "alice", "logs"); got != 3 {
		t.Errorf("alice logs = %d, want 3", got)
	}
	if got := ingestCount(t, reader, "alice", "metrics"); got != 7 {
		t.Errorf("alice metrics = %d, want 7", got)
	}
	if got := ingestCount(t, reader, "bob", "traces"); got != 2 {
		t.Errorf("bob traces = %d, want 2", got)
	}
	// Series that were never touched should be 0 — confirms the counter
	// does not accidentally emit against tenants that never sent anything.
	if got := ingestCount(t, reader, "bob", "logs"); got != 0 {
		t.Errorf("bob logs = %d, want 0", got)
	}
}

// TestIngest_AccumulatesAcrossBatches — a single tenant sending multiple
// batches must sum, not overwrite. Guards against a subtle "record last
// batch size" bug.
func TestIngest_AccumulatesAcrossBatches(t *testing.T) {
	cfg := Config{AttributeKey: "tenant", AuthAttribute: "tenant"}
	m, reader := newTestMetrics(t)
	for _, n := range []int{4, 6, 10} {
		if _, err := processTraces(ctxWithTenant("alice"), cfg, m, tracesN(n)); err != nil {
			t.Fatalf("processTraces: %v", err)
		}
	}
	if got := ingestCount(t, reader, "alice", "traces"); got != 20 {
		t.Fatalf("alice traces = %d, want 20 (4+6+10)", got)
	}
}

// TestIngest_FailClosedDoesNotIncrement — a batch that hits the fail-closed
// path (no resolved tenant) must NOT increment the counter. Emitting under
// an empty or client-supplied tenant would defeat the whole "tenant comes
// from the signed claim" contract, so the counter has to inherit it too.
func TestIngest_FailClosedDoesNotIncrement(t *testing.T) {
	cfg := Config{AttributeKey: "tenant", AuthAttribute: "tenant"}
	m, reader := newTestMetrics(t)
	if _, err := processTraces(context.Background(), cfg, m, tracesN(5)); err == nil {
		t.Fatal("expected ErrMissingTenant")
	}
	if got := ingestCount(t, reader, "", "traces"); got != 0 {
		t.Fatalf("empty-tenant series = %d, want 0", got)
	}
	// Also confirm no traces datapoints exist for any tenant.
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			if mm.Name != "otelhouse_gateway_ingest_records" {
				continue
			}
			if sum, ok := mm.Data.(metricdata.Sum[int64]); ok && len(sum.DataPoints) > 0 {
				t.Fatalf("expected no datapoints on fail-closed path, got %d", len(sum.DataPoints))
			}
		}
	}
}

// TestIngest_EmptyBatchNoZeroDatapoint — a legitimate empty batch is a
// no-op for the counter. Emitting a zero increment would still allocate a
// series the operator does not need to see and pollutes cardinality.
func TestIngest_EmptyBatchNoZeroDatapoint(t *testing.T) {
	cfg := Config{AttributeKey: "tenant", AuthAttribute: "tenant"}
	m, reader := newTestMetrics(t)
	td := ptrace.NewTraces() // no ResourceSpans → SpanCount == 0
	if _, err := processTraces(ctxWithTenant("alice"), cfg, m, td); err != nil {
		t.Fatalf("processTraces: %v", err)
	}
	if got := ingestCount(t, reader, "alice", "traces"); got != 0 {
		t.Fatalf("alice traces = %d, want 0 (empty batch should not emit)", got)
	}
}
