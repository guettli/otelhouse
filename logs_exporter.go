package otelhouse

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// LogExporter writes OTel log records to ClickHouse.
type LogExporter struct {
	conn  driver.Conn
	table string
}

// NewLogExporter opens a ClickHouse connection and returns a ready-to-use
// LogExporter. The Config{DSN,Table} type is reused from the trace exporter;
// Table defaults to "otel_logs".
func NewLogExporter(ctx context.Context, cfg Config) (*LogExporter, error) {
	if cfg.Table == "" {
		cfg.Table = "otel_logs"
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
	return &LogExporter{conn: conn, table: cfg.Table}, nil
}

// CreateLogSchema creates the logs table if it does not exist.
func (e *LogExporter) CreateLogSchema(ctx context.Context) error {
	return e.conn.Exec(ctx, logsSchemaSQL(e.table))
}

// Export sends a batch of log records to ClickHouse. Implements
// sdklog.Exporter.
func (e *LogExporter) Export(ctx context.Context, records []sdklog.Record) error {
	if len(records) == 0 {
		return nil
	}
	batch, err := e.conn.PrepareBatch(ctx, "INSERT INTO "+e.table)
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}
	for _, r := range records {
		ts := r.Timestamp()
		if ts.IsZero() {
			ts = r.ObservedTimestamp()
		}
		resAttrs := kvToMap(r.Resource().Attributes())
		svcName := resAttrs["service.name"]
		scope := r.InstrumentationScope()

		if err := batch.Append(
			ts,
			r.TraceID().String(),
			r.SpanID().String(),
			uint8(r.TraceFlags()),
			uint8(r.Severity()),
			r.SeverityText(),
			svcName,
			bodyToString(r.Body()),
			resAttrs,
			scope.Name,
			scope.Version,
			logKVToMap(r),
		); err != nil {
			return fmt.Errorf("append log record: %w", err)
		}
	}
	return batch.Send()
}

// ForceFlush is a no-op: each Export call commits its batch synchronously
// inside PrepareBatch/Send.
func (e *LogExporter) ForceFlush(_ context.Context) error {
	return nil
}

// Shutdown flushes and closes the ClickHouse connection. Implements
// sdklog.Exporter.
func (e *LogExporter) Shutdown(_ context.Context) error {
	return e.conn.Close()
}

// LogCount returns the number of stored log records with the given service
// name.
func (e *LogExporter) LogCount(ctx context.Context, serviceName string) (uint64, error) {
	row := e.conn.QueryRow(ctx, "SELECT count() FROM "+e.table+" WHERE ServiceName = ?", serviceName)
	var count uint64
	return count, row.Scan(&count)
}

// bodyToString renders a log.Value for the Body column. KindString passes the
// raw string through; other kinds use Value.String() (numbers, bools, slices
// and maps stringify deterministically; Empty becomes "<nil>").
func bodyToString(v log.Value) string {
	if v.Kind() == log.KindString {
		return v.AsString()
	}
	return v.String()
}

// logKVToMap walks the record's attributes into a map[string]string. Parallel
// to kvToMap for span/resource attributes.
func logKVToMap(r sdklog.Record) map[string]string {
	m := make(map[string]string, r.AttributesLen())
	r.WalkAttributes(func(kv log.KeyValue) bool {
		m[kv.Key] = bodyToString(kv.Value)
		return true
	})
	return m
}
