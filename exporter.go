package otelhouse

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Exporter writes OTel spans to ClickHouse.
type Exporter struct {
	conn          driver.Conn
	table         string
	retentionDays int
}

// Config configures the Exporter.
type Config struct {
	// DSN is the ClickHouse connection string.
	// Format: clickhouse://[user[:password]@]host[:port]/database
	DSN string

	// Table is the ClickHouse table name. Defaults to "otel_traces".
	Table string

	// RetentionDays controls the TTL clause baked into CreateSchema. A zero
	// value (the default) means 180 days; a positive value uses that many
	// days; a negative value (e.g. -1) omits the TTL clause entirely so
	// retention can be managed out-of-band. Only applies to newly created
	// tables: CreateSchema uses CREATE TABLE IF NOT EXISTS and will not
	// modify the TTL of an existing table.
	RetentionDays int

	// DialTimeout caps how long the driver waits to establish a connection.
	// Defaults to 30s (the clickhouse-go default).
	DialTimeout time.Duration

	// ReadTimeout caps how long the driver waits for a query response. A
	// zero value (the default) defers to the clickhouse-go default.
	ReadTimeout time.Duration

	// MaxOpenConns caps the number of open connections in the pool. A zero
	// value (the default) defers to the clickhouse-go default (MaxIdleConns
	// + 5).
	MaxOpenConns int

	// MaxIdleConns caps the number of idle connections in the pool. A zero
	// value (the default) defers to the clickhouse-go default (5).
	MaxIdleConns int

	// Compression enables LZ4 block compression on the wire. Defaults to
	// false to preserve current behavior. When true, the constructor sets
	// clickhouse.Options.Compression to &clickhouse.Compression{Method:
	// clickhouse.CompressionLZ4} unless the DSN already specified a
	// compression= query parameter (in which case the DSN wins).
	Compression bool
}

// applyDefaults fills zero-valued Config fields with their defaults.
// Pure: no I/O, safe to call from unit tests.
func (c *Config) applyDefaults() {
	if c.Table == "" {
		c.Table = "otel_traces"
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = 180
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = 30 * time.Second
	}
}

// applyConnOptions mutates opts with the typed connection fields. Zero
// values are skipped so the driver's own defaults apply. A pre-existing
// opts.Compression (set by a compression= DSN query parameter) is never
// overwritten — the DSN wins.
func applyConnOptions(opts *clickhouse.Options, dial, read time.Duration, maxOpen, maxIdle int, compress bool) {
	if dial != 0 {
		opts.DialTimeout = dial
	}
	if read != 0 {
		opts.ReadTimeout = read
	}
	if maxOpen != 0 {
		opts.MaxOpenConns = maxOpen
	}
	if maxIdle != 0 {
		opts.MaxIdleConns = maxIdle
	}
	if compress && opts.Compression == nil {
		opts.Compression = &clickhouse.Compression{Method: clickhouse.CompressionLZ4}
	}
}

// New opens a ClickHouse connection and returns a ready-to-use Exporter.
func New(ctx context.Context, cfg Config) (*Exporter, error) {
	cfg.applyDefaults()
	opts, err := clickhouse.ParseDSN(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	applyConnOptions(opts, cfg.DialTimeout, cfg.ReadTimeout, cfg.MaxOpenConns, cfg.MaxIdleConns, cfg.Compression)
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Exporter{conn: conn, table: cfg.Table, retentionDays: cfg.RetentionDays}, nil
}

// CreateSchema creates the traces table if it does not exist.
func (e *Exporter) CreateSchema(ctx context.Context) error {
	warnIfTableExists(ctx, e.conn, e.table)
	return e.conn.Exec(ctx, schemaSQL(e.table, e.retentionDays))
}

// warnIfTableExists emits a one-line log when the named table already exists,
// to flag that a freshly configured RetentionDays will not be applied: the
// CREATE TABLE IF NOT EXISTS path leaves the existing TTL untouched.
func warnIfTableExists(ctx context.Context, conn driver.Conn, table string) {
	var exists uint8
	row := conn.QueryRow(ctx, "EXISTS TABLE "+table)
	if err := row.Scan(&exists); err != nil {
		return
	}
	if exists == 1 {
		log.Printf("otelhouse: table %s already exists; RetentionDays only applies to newly created tables", table)
	}
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
