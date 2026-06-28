package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMetricsIngested verifies the upstream clickhouseexporter wrote the
// telemetrygen-produced metrics into ClickHouse. The Dagger pipeline pushes
// metrics through the Collector before this test runs, so by the time the
// test starts the data is in flight; the loop polls because the Collector
// flushes asynchronously and the exporter creates the table on the first
// write.
//
// The test skips when CLICKHOUSE_HTTP_URL is unset so `go test ./...` from a
// bare checkout — without the Dagger services — stays a no-op.
func TestMetricsIngested(t *testing.T) {
	base := os.Getenv("CLICKHOUSE_HTTP_URL")
	if base == "" {
		t.Skip("CLICKHOUSE_HTTP_URL not set; run via `make test`")
	}

	// telemetrygen's default resource sets service.name="telemetrygen".
	const service = "telemetrygen"

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	countSQL := fmt.Sprintf(
		"SELECT count() FROM otel_metrics_sum WHERE ServiceName = '%s'", service)
	if err := waitFor(ctx, 2*time.Second, func() error {
		raw, err := queryScalar(ctx, base, countSQL)
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("parse count %q: %w", raw, err)
		}
		if n == 0 {
			return fmt.Errorf("no rows yet")
		}
		t.Logf("otel_metrics_sum has %d rows for service=%s", n, service)
		return nil
	}); err != nil {
		t.Fatalf("waiting for metrics: %v", err)
	}

	// Sanity check: the row(s) carry a non-empty MetricName, so we know the
	// pipeline shape is right rather than just that *something* landed.
	nameSQL := fmt.Sprintf(
		"SELECT any(MetricName) FROM otel_metrics_sum WHERE ServiceName = '%s'", service)
	name, err := queryScalar(ctx, base, nameSQL)
	if err != nil {
		t.Fatalf("read MetricName: %v", err)
	}
	if strings.TrimSpace(name) == "" {
		t.Fatalf("rows for service %q have empty MetricName", service)
	}
	t.Logf("sample MetricName for service=%s: %s", service, strings.TrimSpace(name))
}

// waitFor retries fn every `every` interval until it succeeds or ctx expires.
func waitFor(ctx context.Context, every time.Duration, fn func() error) error {
	var last error
	for {
		if err := fn(); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last attempt: %v)", ctx.Err(), last)
		case <-time.After(every):
		}
	}
}

// queryScalar runs a single-value SELECT against ClickHouse's HTTP interface
// and returns the raw response body. Using HTTP keeps the test dependency-free
// — no ClickHouse Go driver to vendor — at the cost of being limited to
// scalar/TSV-style results, which is all the assertions need.
func queryScalar(ctx context.Context, base, sql string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("query", sql)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}
