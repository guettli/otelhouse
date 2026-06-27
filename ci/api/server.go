// Package api serves a small read-only HTTP API on top of the upstream
// OpenTelemetry Collector ClickHouse schema (otel_traces / otel_logs /
// otel_metrics_*). It is the data source for the otelhouse UI and the
// end-to-end Dagger harness in ci/main.go.
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

const (
	defaultRunsLimit = 100
	maxRunsLimit     = 1000
)

// hexIDPattern accepts the lowercase-hex trace and span ids the upstream
// OTel Collector ClickHouse exporter writes (32 chars for trace ids, 16
// for span ids). Anything else is rejected at the HTTP boundary.
var hexIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{1,32}$`)

// Config configures the API Server.
type Config struct {
	// DSN is the ClickHouse connection string in clickhouse-go's
	// clickhouse://user:password@host:port/database format.
	DSN string

	// TracesTable defaults to "otel_traces".
	TracesTable string

	// LogsTable defaults to "otel_logs".
	LogsTable string

	// DialTimeout caps the connection establishment timeout. Defaults to 30s.
	DialTimeout time.Duration
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
	if cfg.DialTimeout != 0 {
		opts.DialTimeout = cfg.DialTimeout
	}
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

// Close releases the underlying ClickHouse connection.
func (s *Server) Close() error {
	return s.conn.Close()
}

// Handler routes the three API endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runs", s.handleRuns)
	mux.HandleFunc("GET /api/traces/{id}", s.handleTrace)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	return mux
}

// Run is one Dagger pipeline run grouped by TraceId. StatusCode and Command
// are derived from the root span (ParentSpanId is the all-zero hex string in
// the upstream schema).
type Run struct {
	TraceID            string            `json:"trace_id"`
	ServiceName        string            `json:"service_name"`
	StartTime          time.Time         `json:"start_time"`
	EndTime            time.Time         `json:"end_time"`
	DurationNs         int64             `json:"duration_ns"`
	SpanCount          uint64            `json:"span_count"`
	StatusCode         string            `json:"status_code"`
	Command            string            `json:"command"`
	ResourceAttributes map[string]string `json:"resource_attributes"`
}

// Span is one row of a trace's span hierarchy.
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

// Trace bundles the spans of one TraceId, ordered by start time ascending so
// the response can be rendered as a waterfall without re-sorting.
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
		panic(errors.Join(errors.New("api: write json"), err))
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
