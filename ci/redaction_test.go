//go:build e2e

// End-to-end proof that the Collector redacts secrets out of telemetry before
// it reaches ClickHouse (#46). Unlike TestE2E_Store, which reads back rows the
// otelhouse-emit binary produced, this test both writes and reads: it emits a
// synthetic-but-rule-matching secret straight at the Collector's OTLP endpoint
// and then asserts, against ClickHouse, that the stored rows carry the
// REDACTED marker and never the original literal.
//
// The redaction rules come from the gitleaks rule pack, expanded into a stock
// `transform` processor by collector/gen-gitleaks-rules and committed as
// collector/redaction.yaml; ci/main.go wires that file into the harness
// Collector ahead of `batch` on the logs and traces pipelines.
//
// Gated by the e2e build tag, like the rest of this file's siblings, so the
// default `go test ./...` step (which has no running Collector) skips it.
package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/guettli/otelhouseview/otelstore"
)

// secretLiteral is a fake AWS access-key ID: it matches the gitleaks
// aws-access-token rule (AKIA followed by 16 base32 chars) but is not a real
// credential — the test owns it, so nothing sensitive is checked into the
// repo. If it survives to ClickHouse unredacted, redaction is broken.
const secretLiteral = "AKIAROTATE4FAKEKEY23"

// redactedMarker is what transform/redaction replaces an aws-access-token
// match with (REDACTED:<ruleID>).
const redactedMarker = "REDACTED:aws-access-token"

// redactionServiceName isolates this test's rows from every other signal in
// the shared ClickHouse so the assertions query exactly what they emitted.
const redactionServiceName = "otelhouse-redaction-e2e"

// secretAttrKey is the span/log attribute the test hides the secret in.
const secretAttrKey = "secret.token"

// logBodyWithSecret carries the secret with no secret-ish keyword ("key",
// "token", ...) around it, so only the aws-access-token rule matches and the
// stored body is the deterministic "<prefix>REDACTED:aws-access-token<suffix>"
// rather than a cascade of overlapping generic-rule replacements.
const logBodyWithSecret = "found " + secretLiteral + " here"

func TestE2E_Redaction(t *testing.T) {
	dsn := os.Getenv("OTELHOUSE_E2E_CLICKHOUSE_DSN")
	if dsn == "" {
		t.Fatal("OTELHOUSE_E2E_CLICKHOUSE_DSN not set; expected by the e2e harness in ci/main.go")
	}
	endpoint := os.Getenv("OTELHOUSE_E2E_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "127.0.0.1:4317"
	}

	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout+30*time.Second)
	defer cancel()

	if err := emitSecretTelemetry(ctx, endpoint); err != nil {
		t.Fatalf("emit secret-bearing telemetry to %s: %v", endpoint, err)
	}

	store, err := otelstore.OpenClickHouse(ctx, dsn)
	if err != nil {
		t.Fatalf("otelstore.OpenClickHouse(%q): %v", dsn, err)
	}
	defer func() { _ = store.Close() }()

	// Log body: the secret must be gone and the redaction marker present.
	body := pollScalar(ctx, t, store, "log body",
		`SELECT Body FROM otel_logs WHERE ServiceName = ? ORDER BY Timestamp DESC LIMIT 1`,
		redactionServiceName)
	assertRedacted(t, "otel_logs.Body", body)

	// Span attribute value: same guarantee for a value carried in an
	// attribute rather than a body. With no keyword around a bare literal the
	// attribute redacts to exactly the marker.
	// secretAttrKey is a compile-time constant the test owns, so it is safe to
	// inline into the map subscript rather than bind it (a `?` placeholder
	// inside SpanAttributes[...] is needlessly driver-specific).
	attr := pollScalar(ctx, t, store, "span attribute",
		`SELECT SpanAttributes['`+secretAttrKey+`'] FROM otel_traces WHERE ServiceName = ? ORDER BY Timestamp DESC LIMIT 1`,
		redactionServiceName)
	assertRedacted(t, "otel_traces.SpanAttributes['"+secretAttrKey+"']", attr)
	if attr != redactedMarker {
		t.Fatalf("span attribute redacted to %q, want exactly %q", attr, redactedMarker)
	}
}

// assertRedacted is the core guarantee: the raw secret never reached
// ClickHouse and the redaction marker did.
func assertRedacted(t *testing.T, where, got string) {
	t.Helper()
	if strings.Contains(got, secretLiteral) {
		t.Fatalf("%s still contains the raw secret %q (value %q): redaction did not run", where, secretLiteral, got)
	}
	if !strings.Contains(got, redactedMarker) {
		t.Fatalf("%s is missing the redaction marker %q (value %q)", where, redactedMarker, got)
	}
}

// emitSecretTelemetry sends one log (secret in the body) and one span (secret
// in an attribute) to the Collector, both tagged with redactionServiceName,
// and flushes so the rows are on their way before the poll begins.
func emitSecretTelemetry(ctx context.Context, endpoint string) error {
	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(redactionServiceName)))
	if err != nil {
		return err
	}

	logExp, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(endpoint), otlploggrpc.WithInsecure())
	if err != nil {
		return err
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
		sdklog.WithResource(res),
	)
	defer func() { _ = lp.Shutdown(ctx) }()
	var rec otellog.Record
	rec.SetTimestamp(time.Now())
	rec.SetObservedTimestamp(time.Now())
	rec.SetSeverity(otellog.SeverityInfo)
	rec.SetBody(otellog.StringValue(logBodyWithSecret))
	rec.AddAttributes(otellog.String(secretAttrKey, secretLiteral))
	lp.Logger("otelhouse-redaction-e2e").Emit(ctx, rec)
	if err := lp.ForceFlush(ctx); err != nil {
		return err
	}

	traceExp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint), otlptracegrpc.WithInsecure())
	if err != nil {
		return err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	defer func() { _ = tp.Shutdown(ctx) }()
	_, span := tp.Tracer("otelhouse-redaction-e2e").Start(ctx, "redaction-probe")
	span.SetAttributes(attribute.String(secretAttrKey, secretLiteral))
	span.End()
	return tp.ForceFlush(ctx)
}

// pollScalar runs a single-column, single-row query until it returns a row or
// pollTimeout elapses — the Collector batches with a 1s timeout, so the row
// usually lands quickly, but CI runners can be slow.
func pollScalar(ctx context.Context, t *testing.T, store *otelstore.ClickHouseStore, what, query string, args ...any) string {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	for time.Now().Before(deadline) {
		var got string
		err := store.DB().QueryRowContext(ctx, query, args...).Scan(&got)
		if err == nil {
			return got
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("query %s: %v", what, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled while polling for %s: %v", what, ctx.Err())
		case <-time.After(time.Second):
		}
	}
	t.Fatalf("no %s row for service %q after %s: redacted telemetry never reached ClickHouse", what, redactionServiceName, pollTimeout)
	return ""
}
