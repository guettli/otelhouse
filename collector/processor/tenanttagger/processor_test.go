package tenanttagger

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/collector/client"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// noopMetrics builds an ingestMetrics whose counter goes into the OTel
// noop provider — the fast-path used by tests that only care about the
// processor's data-plane behaviour, not the observability signal. The
// metrics-specific assertions live in metrics_test.go.
func noopMetrics(t *testing.T) *ingestMetrics {
	t.Helper()
	m, err := newIngestMetrics(nil)
	if err != nil {
		t.Fatalf("newIngestMetrics: %v", err)
	}
	return m
}

// fakeAuth returns a client.AuthData that exposes exactly one attribute.
// The empty-string case is used to prove the fail-closed path: an Auth
// present but with an empty tenant is treated the same as no Auth at all.
type fakeAuth struct{ name, value string }

func (a fakeAuth) GetAttribute(name string) any {
	if name == a.name {
		return a.value
	}
	return nil
}
func (a fakeAuth) GetAttributeNames() []string { return []string{a.name} }

// ctxWithTenant simulates the context the tenantauth extension hands to
// downstream processors: a client.Info whose Auth resolves the tenant.
func ctxWithTenant(t string) context.Context {
	info := client.Info{Auth: fakeAuth{name: "tenant", value: t}}
	return client.NewContext(context.Background(), info)
}

// tracesWith builds a Traces batch whose resource attributes are seeded
// with the supplied map — the processor must PRESERVE non-tenant values
// while overwriting/inserting the tenant.
func tracesWith(seed map[string]string) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	attrs := rs.Resource().Attributes()
	for k, v := range seed {
		attrs.PutStr(k, v)
	}
	// One span so the resource actually has content the exporter would
	// forward. The processor operates on resource attrs only, but the
	// batch shape should be realistic.
	rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty().SetName("op")
	return td
}

func tenantAttr(t *testing.T, attrs pcommon.Map) string {
	t.Helper()
	v, ok := attrs.Get("tenant")
	if !ok {
		return ""
	}
	return v.Str()
}

// TestTraces_SpoofOverridden is the primary "cannot forge tenant" test.
// A client whose auth resolves to `alice` puts `tenant=evil` in the
// payload. The output must land on `alice` — the payload value is
// discarded.
func TestTraces_SpoofOverridden(t *testing.T) {
	cfg := Config{AttributeKey: "tenant", AuthAttribute: "tenant"}
	td := tracesWith(map[string]string{"tenant": "evil", "service.name": "foo"})
	out, err := processTraces(ctxWithTenant("alice"), cfg, noopMetrics(t), td)
	if err != nil {
		t.Fatalf("processTraces: %v", err)
	}
	attrs := out.ResourceSpans().At(0).Resource().Attributes()
	if got := tenantAttr(t, attrs); got != "alice" {
		t.Fatalf("tenant = %q, want alice", got)
	}
	// Non-tenant attribute must survive.
	sn, ok := attrs.Get("service.name")
	if !ok || sn.Str() != "foo" {
		t.Fatalf("service.name lost or mutated: got %v, ok=%v", sn.AsRaw(), ok)
	}
	// Only one "tenant" key must exist (guard against a subtle append-instead-of-overwrite bug).
	seen := 0
	attrs.Range(func(k string, _ pcommon.Value) bool {
		if k == "tenant" {
			seen++
		}
		return true
	})
	if seen != 1 {
		t.Fatalf("tenant key present %d times, want 1", seen)
	}
}

// TestTraces_NoClientTenant covers the common case — no client-supplied
// tenant, just the authenticated one stamped on. Ensures the code path
// works for well-behaved clients too.
func TestTraces_NoClientTenant(t *testing.T) {
	cfg := Config{AttributeKey: "tenant", AuthAttribute: "tenant"}
	td := tracesWith(map[string]string{"service.name": "foo"})
	out, err := processTraces(ctxWithTenant("alice"), cfg, noopMetrics(t), td)
	if err != nil {
		t.Fatalf("processTraces: %v", err)
	}
	if got := tenantAttr(t, out.ResourceSpans().At(0).Resource().Attributes()); got != "alice" {
		t.Fatalf("tenant = %q, want alice", got)
	}
}

// TestTraces_FailClosed_NoAuth is THE acceptance-critical test: no
// resolved tenant in the auth context → batch rejected, no output with an
// empty tenant. Guarantees no untagged row can reach ClickHouse.
func TestTraces_FailClosed_NoAuth(t *testing.T) {
	cfg := Config{AttributeKey: "tenant", AuthAttribute: "tenant"}
	td := tracesWith(map[string]string{"tenant": "evil"})
	_, err := processTraces(context.Background(), cfg, noopMetrics(t), td)
	if !errors.Is(err, ErrMissingTenant) {
		t.Fatalf("err = %v, want ErrMissingTenant", err)
	}
}

// TestTraces_FailClosed_EmptyAuthTenant — auth present but tenant is the
// empty string. Same failure mode.
func TestTraces_FailClosed_EmptyAuthTenant(t *testing.T) {
	cfg := Config{AttributeKey: "tenant", AuthAttribute: "tenant"}
	td := tracesWith(map[string]string{})
	_, err := processTraces(ctxWithTenant(""), cfg, noopMetrics(t), td)
	if !errors.Is(err, ErrMissingTenant) {
		t.Fatalf("err = %v, want ErrMissingTenant", err)
	}
}

// TestTraces_PreservesOtherAttrs is a targeted check that a batch with a
// realistic bundle of resource attrs comes out with all non-tenant keys
// unchanged. Not fully redundant with SpoofOverridden — here we assert
// the whole set, not just service.name.
func TestTraces_PreservesOtherAttrs(t *testing.T) {
	cfg := Config{AttributeKey: "tenant", AuthAttribute: "tenant"}
	seed := map[string]string{
		"service.name":    "svc",
		"service.version": "1.2.3",
		"host.name":       "node-42",
		"deployment.env":  "prod",
	}
	td := tracesWith(seed)
	out, err := processTraces(ctxWithTenant("alice"), cfg, noopMetrics(t), td)
	if err != nil {
		t.Fatalf("processTraces: %v", err)
	}
	attrs := out.ResourceSpans().At(0).Resource().Attributes()
	for k, want := range seed {
		v, ok := attrs.Get(k)
		if !ok || v.Str() != want {
			t.Fatalf("attr %q = %v (ok=%v), want %q", k, v.AsRaw(), ok, want)
		}
	}
}

// TestTraces_MultipleResourceSpans — a batch may carry several
// ResourceSpans (multiplex from a batching client). Every one of them
// must be stamped, not just the first.
func TestTraces_MultipleResourceSpans(t *testing.T) {
	cfg := Config{AttributeKey: "tenant", AuthAttribute: "tenant"}
	td := ptrace.NewTraces()
	for range [3]struct{}{} {
		rs := td.ResourceSpans().AppendEmpty()
		rs.Resource().Attributes().PutStr("tenant", "evil")
		rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty().SetName("op")
	}
	out, err := processTraces(ctxWithTenant("alice"), cfg, noopMetrics(t), td)
	if err != nil {
		t.Fatalf("processTraces: %v", err)
	}
	for i := 0; i < out.ResourceSpans().Len(); i++ {
		if got := tenantAttr(t, out.ResourceSpans().At(i).Resource().Attributes()); got != "alice" {
			t.Fatalf("rss[%d] tenant = %q, want alice", i, got)
		}
	}
}

// TestLogs_Stamped is the parallel check for the logs pipeline.
func TestLogs_Stamped(t *testing.T) {
	cfg := Config{AttributeKey: "tenant", AuthAttribute: "tenant"}
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("tenant", "evil")
	rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Body().SetStr("hi")
	out, err := processLogs(ctxWithTenant("alice"), cfg, noopMetrics(t), ld)
	if err != nil {
		t.Fatalf("processLogs: %v", err)
	}
	if got := tenantAttr(t, out.ResourceLogs().At(0).Resource().Attributes()); got != "alice" {
		t.Fatalf("tenant = %q, want alice", got)
	}
}

// TestLogs_FailClosed — the fail-closed invariant must hold for logs.
func TestLogs_FailClosed(t *testing.T) {
	cfg := Config{AttributeKey: "tenant", AuthAttribute: "tenant"}
	ld := plog.NewLogs()
	ld.ResourceLogs().AppendEmpty()
	if _, err := processLogs(context.Background(), cfg, noopMetrics(t), ld); !errors.Is(err, ErrMissingTenant) {
		t.Fatalf("err = %v, want ErrMissingTenant", err)
	}
}

// TestMetrics_Stamped is the parallel check for the metrics pipeline.
func TestMetrics_Stamped(t *testing.T) {
	cfg := Config{AttributeKey: "tenant", AuthAttribute: "tenant"}
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("tenant", "evil")
	rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty().SetName("m")
	out, err := processMetrics(ctxWithTenant("alice"), cfg, noopMetrics(t), md)
	if err != nil {
		t.Fatalf("processMetrics: %v", err)
	}
	if got := tenantAttr(t, out.ResourceMetrics().At(0).Resource().Attributes()); got != "alice" {
		t.Fatalf("tenant = %q, want alice", got)
	}
}

// TestMetrics_FailClosed — the fail-closed invariant must hold for metrics.
func TestMetrics_FailClosed(t *testing.T) {
	cfg := Config{AttributeKey: "tenant", AuthAttribute: "tenant"}
	md := pmetric.NewMetrics()
	md.ResourceMetrics().AppendEmpty()
	if _, err := processMetrics(context.Background(), cfg, noopMetrics(t), md); !errors.Is(err, ErrMissingTenant) {
		t.Fatalf("err = %v, want ErrMissingTenant", err)
	}
}

// TestCustomKeys — a deployment that renames the resource attribute (say
// to `x-tenant-id`) must still work, and the auth attribute lookup must
// respect the configured name too. Guards against hard-coded "tenant".
func TestCustomKeys(t *testing.T) {
	cfg := Config{AttributeKey: "x-tenant-id", AuthAttribute: "org"}
	info := client.Info{Auth: fakeAuth{name: "org", value: "acme"}}
	ctx := client.NewContext(context.Background(), info)
	td := tracesWith(map[string]string{"x-tenant-id": "evil"})
	out, err := processTraces(ctx, cfg, noopMetrics(t), td)
	if err != nil {
		t.Fatalf("processTraces: %v", err)
	}
	v, ok := out.ResourceSpans().At(0).Resource().Attributes().Get("x-tenant-id")
	if !ok || v.Str() != "acme" {
		t.Fatalf("x-tenant-id = %v (ok=%v), want acme", v.AsRaw(), ok)
	}
}

// TestConfig_Validate — smoke check that startup rejects empty keys.
func TestConfig_Validate(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		c := &Config{AttributeKey: "tenant", AuthAttribute: "tenant"}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
	t.Run("empty_attribute_key", func(t *testing.T) {
		c := &Config{AttributeKey: "", AuthAttribute: "tenant"}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("empty_auth_attribute", func(t *testing.T) {
		c := &Config{AttributeKey: "tenant", AuthAttribute: ""}
		if err := c.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})
}

// TestFactoryDefaults — the ocb-integration surface. Default config must
// match the documented gitops contract (attribute name "tenant").
func TestFactoryDefaults(t *testing.T) {
	f := NewFactory()
	cfg := f.CreateDefaultConfig().(*Config)
	if cfg.AttributeKey != "tenant" || cfg.AuthAttribute != "tenant" {
		t.Fatalf("defaults = %+v, want tenant/tenant", cfg)
	}
}
