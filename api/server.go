package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Default LIMIT applied to /api/runs when the caller doesn't pass ?limit=.
const defaultRunsLimit = 100

// Maximum LIMIT accepted on /api/runs. Clamps oversized requests so a buggy
// client can't force ClickHouse to scan the whole trace table.
const maxRunsLimit = 1000

// hexIDPattern is a permissive validator for trace and span ids: both
// the upstream OpenTelemetry Collector ClickHouse exporter and this package
// store them as lowercase hex strings (32 chars for trace ids, 16 for span
// ids), but we accept any non-empty hex run up to 32 chars so callers can
// also pass shorter ids if a future schema change makes that meaningful.
var hexIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{1,32}$`)

// Config configures the API Server.
type Config struct {
	// DSN is the ClickHouse connection string.
	// Format: clickhouse://[user[:password]@]host[:port]/database
	DSN string

	// TracesTable is the ClickHouse table to query for traces.
	// Defaults to "otel_traces".
	TracesTable string

	// LogsTable is the ClickHouse table to query for logs.
	// Defaults to "otel_logs".
	LogsTable string

	// DialTimeout caps how long the driver waits to establish a connection.
	// Defaults to 30s (the clickhouse-go default).
	DialTimeout time.Duration

	// ReadTimeout caps how long the driver waits for a query response. A
	// zero value (the default) defers to the clickhouse-go default.
	ReadTimeout time.Duration

	// MaxOpenConns caps the number of open connections in the pool. A zero
	// value (the default) defers to the clickhouse-go default.
	MaxOpenConns int

	// MaxIdleConns caps the number of idle connections in the pool. A zero
	// value (the default) defers to the clickhouse-go default.
	MaxIdleConns int

	// Compression enables LZ4 block compression on the wire. A
	// compression= query parameter on the DSN takes precedence if set.
	Compression bool
}

func (c *Config) applyDefaults() {
	if c.TracesTable == "" {
		c.TracesTable = "otel_traces"
	}
	if c.LogsTable == "" {
		c.LogsTable = "otel_logs"
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = 30 * time.Second
	}
}

// Server holds the ClickHouse connection and serves the API endpoints.
type Server struct {
	conn        driver.Conn
	tracesTable string
	logsTable   string
}

// New opens a ClickHouse connection and returns a ready-to-use Server.
func New(ctx context.Context, cfg Config) (*Server, error) {
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
	return &Server{conn: conn, tracesTable: cfg.TracesTable, logsTable: cfg.LogsTable}, nil
}

// NewWithConn builds a Server around an existing ClickHouse connection.
// Useful in tests that already opened a connection via another exporter, or
// when the caller wants to share a pool across components.
func NewWithConn(conn driver.Conn, tracesTable, logsTable string) *Server {
	if tracesTable == "" {
		tracesTable = "otel_traces"
	}
	if logsTable == "" {
		logsTable = "otel_logs"
	}
	return &Server{conn: conn, tracesTable: tracesTable, logsTable: logsTable}
}

// Close releases the underlying ClickHouse connection.
func (s *Server) Close() error {
	return s.conn.Close()
}

// Handler returns an [http.Handler] that routes the three API endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runs", s.handleRuns)
	mux.HandleFunc("GET /api/traces/{id}", s.handleTrace)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	return mux
}

// applyConnOptions mirrors the helper on the otelhouse package: copies the
// typed connection fields onto opts, leaving zero values for the driver's
// own defaults to take effect, and never overwrites a compression preset
// from the DSN.
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

// Run represents one Dagger pipeline run grouped by TraceId.
type Run struct {
	TraceID            string            `json:"trace_id"`
	ServiceName        string            `json:"service_name"`
	StartTime          time.Time         `json:"start_time"`
	EndTime            time.Time         `json:"end_time"`
	DurationNs         int64             `json:"duration_ns"`
	SpanCount          uint64            `json:"span_count"`
	ResourceAttributes map[string]string `json:"resource_attributes"`
}

// Span is one row of a trace's span hierarchy. The shape is tailored to the
// Gantt/waterfall use case: start time + duration is enough to draw a bar,
// parent_span_id is enough to nest bars, and the human-facing fields are
// included verbatim.
type Span struct {
	SpanID         string            `json:"span_id"`
	ParentSpanID   string            `json:"parent_span_id"`
	Name           string            `json:"name"`
	Kind           string            `json:"kind"`
	ServiceName    string            `json:"service_name"`
	StartTime      time.Time         `json:"start_time"`
	DurationNs     int64             `json:"duration_ns"`
	StatusCode     string            `json:"status_code"`
	StatusMessage  string            `json:"status_message"`
	SpanAttributes map[string]string `json:"span_attributes"`
}

// Trace bundles the spans of one TraceId. Spans are ordered by start time
// ascending so the response can be rendered as a waterfall without
// re-sorting on the client.
type Trace struct {
	TraceID string `json:"trace_id"`
	Spans   []Span `json:"spans"`
}

// LogRecord is one row from the logs table associated with a TraceId.
type LogRecord struct {
	Timestamp      time.Time         `json:"timestamp"`
	TraceID        string            `json:"trace_id"`
	SpanID         string            `json:"span_id"`
	SeverityNumber uint8             `json:"severity_number"`
	SeverityText   string            `json:"severity_text"`
	ServiceName    string            `json:"service_name"`
	Body           string            `json:"body"`
	LogAttributes  map[string]string `json:"log_attributes"`
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	limit := defaultRunsLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		if n > maxRunsLimit {
			n = maxRunsLimit
		}
		limit = n
	}

	runs, err := s.queryRuns(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !hexIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid trace id")
		return
	}

	trace, err := s.queryTrace(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(trace.Spans) == 0 {
		writeError(w, http.StatusNotFound, "trace not found")
		return
	}
	writeJSON(w, http.StatusOK, trace)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("traceId")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing traceId")
		return
	}
	if !hexIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid traceId")
		return
	}

	logs, err := s.queryLogs(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, logs)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	if err := enc.Encode(payload); err != nil {
		// The status is already written, so we cannot recover; log via the
		// default logger by panicking with errors.Join so net/http surfaces
		// the failure as a 500 to the client side of the connection.
		panic(errors.Join(errors.New("api: write json"), err))
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
