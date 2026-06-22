// Command otelhouse-api serves the HTTP API for querying Dagger traces and
// logs from a ClickHouse instance populated by either the upstream
// OpenTelemetry Collector ClickHouse exporter or this package's own
// exporters.
//
// Configuration is read from flags; sensible defaults match the
// docker-compose pipeline in the repo root.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/guettli/otelhouse/api"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dsn := flag.String("dsn", "clickhouse://otel:otel@localhost:9000/otel", "ClickHouse DSN")
	tracesTable := flag.String("traces-table", "otel_traces", "ClickHouse traces table")
	logsTable := flag.String("logs-table", "otel_logs", "ClickHouse logs table")
	readTimeout := flag.Duration("read-timeout", 30*time.Second, "HTTP server read timeout")
	writeTimeout := flag.Duration("write-timeout", 30*time.Second, "HTTP server write timeout")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := api.New(ctx, api.Config{
		DSN:         *dsn,
		TracesTable: *tracesTable,
		LogsTable:   *logsTable,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "otelhouse-api: connect: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = srv.Close() }()

	httpSrv := &http.Server{
		Addr:         *addr,
		Handler:      srv.Handler(),
		ReadTimeout:  *readTimeout,
		WriteTimeout: *writeTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("otelhouse-api: listening on %s (clickhouse=%s)", *addr, *dsn)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		log.Printf("otelhouse-api: shutdown requested")
	case err := <-errCh:
		if err != nil {
			fmt.Fprintf(os.Stderr, "otelhouse-api: serve: %v\n", err)
			os.Exit(1)
		}
		return
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintf(os.Stderr, "otelhouse-api: shutdown: %v\n", err)
		os.Exit(1)
	}
}
