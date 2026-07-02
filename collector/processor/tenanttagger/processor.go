package tenanttagger

import (
	"context"

	"go.opentelemetry.io/collector/client"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// tenantFromContext returns the authenticated tenant string from the
// batch's client.Info, or false if it is absent. This is the ONE place
// the processor reads tenancy from — never from the payload — so the
// fail-closed guarantee is enforceable by a single check.
func tenantFromContext(ctx context.Context, authAttr string) (string, bool) {
	info := client.FromContext(ctx)
	if info.Auth == nil {
		return "", false
	}
	v := info.Auth.GetAttribute(authAttr)
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// stampAttrs deletes any client-supplied value at attrKey (defence in
// depth against a client that puts `tenant=other` in the payload) and
// writes the authenticated tenant. All existing non-tenant attributes
// are preserved.
func stampAttrs(attrs pcommon.Map, attrKey, tenant string) {
	attrs.Remove(attrKey)
	attrs.PutStr(attrKey, tenant)
}

// processTraces mutates the batch in place. Returning td unmodified on
// the error path is fine — the collector's consumer chain drops the
// batch when the error is non-nil.
func processTraces(ctx context.Context, cfg Config, td ptrace.Traces) (ptrace.Traces, error) {
	tenant, ok := tenantFromContext(ctx, cfg.AuthAttribute)
	if !ok {
		return td, ErrMissingTenant
	}
	rss := td.ResourceSpans()
	for i := 0; i < rss.Len(); i++ {
		stampAttrs(rss.At(i).Resource().Attributes(), cfg.AttributeKey, tenant)
	}
	return td, nil
}

func processLogs(ctx context.Context, cfg Config, ld plog.Logs) (plog.Logs, error) {
	tenant, ok := tenantFromContext(ctx, cfg.AuthAttribute)
	if !ok {
		return ld, ErrMissingTenant
	}
	rls := ld.ResourceLogs()
	for i := 0; i < rls.Len(); i++ {
		stampAttrs(rls.At(i).Resource().Attributes(), cfg.AttributeKey, tenant)
	}
	return ld, nil
}

func processMetrics(ctx context.Context, cfg Config, md pmetric.Metrics) (pmetric.Metrics, error) {
	tenant, ok := tenantFromContext(ctx, cfg.AuthAttribute)
	if !ok {
		return md, ErrMissingTenant
	}
	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		stampAttrs(rms.At(i).Resource().Attributes(), cfg.AttributeKey, tenant)
	}
	return md, nil
}
