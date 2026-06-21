package otelhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Exporter writes OTel spans to ClickHouse.
type Exporter struct {
	conn  driver.Conn
	table string
}

// Config configures the Exporter.
type Config struct {
	// DSN is the ClickHouse connection string.
	// Format: clickhouse://[user[:password]@]host[:port]/database
	DSN string

	// Table is the ClickHouse table name. Defaults to "otel_traces".
	Table string
}

// New opens a ClickHouse connection and returns a ready-to-use Exporter.
func New(ctx context.Context, cfg Config) (*Exporter, error) {
	if cfg.Table == "" {
		cfg.Table = "otel_traces"
	}
	opts, err := clickhouse.ParseDSN(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Exporter{conn: conn, table: cfg.Table}, nil
}

// CreateSchema creates the traces table if it does not exist.
func (e *Exporter) CreateSchema(ctx context.Context) error {
	return e.conn.Exec(ctx, schemaSQL(e.table))
}

// ExportSpans sends a batch of spans to ClickHouse. Implements sdktrace.SpanExporter.
func (e *Exporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}
	batch, err := e.conn.PrepareBatch(ctx, "INSERT INTO "+e.table)
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}
	for _, s := range spans {
		resAttrs := kvToMap(s.Resource().Attributes())
		svcName := resAttrs["service.name"]

		evTimes := make([]time.Time, len(s.Events()))
		evNames := make([]string, len(s.Events()))
		evAttrs := make([]map[string]string, len(s.Events()))
		for i, ev := range s.Events() {
			evTimes[i] = ev.Time
			evNames[i] = ev.Name
			evAttrs[i] = kvToMap(ev.Attributes)
		}

		lkTraceIDs := make([]string, len(s.Links()))
		lkSpanIDs := make([]string, len(s.Links()))
		lkStates := make([]string, len(s.Links()))
		lkAttrs := make([]map[string]string, len(s.Links()))
		for i, l := range s.Links() {
			lkTraceIDs[i] = l.SpanContext.TraceID().String()
			lkSpanIDs[i] = l.SpanContext.SpanID().String()
			lkStates[i] = l.SpanContext.TraceState().String()
			lkAttrs[i] = kvToMap(l.Attributes)
		}

		if err := batch.Append(
			s.StartTime(),
			s.SpanContext().TraceID().String(),
			s.SpanContext().SpanID().String(),
			s.Parent().SpanID().String(),
			s.SpanContext().TraceState().String(),
			s.Name(),
			s.SpanKind().String(),
			svcName,
			resAttrs,
			s.InstrumentationScope().Name,
			s.InstrumentationScope().Version,
			kvToMap(s.Attributes()),
			s.EndTime().Sub(s.StartTime()).Nanoseconds(),
			s.Status().Code.String(),
			s.Status().Description,
			evTimes, evNames, evAttrs,
			lkTraceIDs, lkSpanIDs, lkStates, lkAttrs,
		); err != nil {
			return fmt.Errorf("append span %s: %w", s.SpanContext().SpanID(), err)
		}
	}
	return batch.Send()
}

// Shutdown flushes and closes the ClickHouse connection. Implements sdktrace.SpanExporter.
func (e *Exporter) Shutdown(_ context.Context) error {
	return e.conn.Close()
}

// SpanCount returns the number of stored spans with the given service name.
func (e *Exporter) SpanCount(ctx context.Context, serviceName string) (uint64, error) {
	row := e.conn.QueryRow(ctx, "SELECT count() FROM "+e.table+" WHERE ServiceName = ?", serviceName)
	var count uint64
	return count, row.Scan(&count)
}

func kvToMap(attrs []attribute.KeyValue) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		m[string(kv.Key)] = kv.Value.AsString()
	}
	return m
}
