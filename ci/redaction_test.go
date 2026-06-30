package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRedactionScrubsSecrets verifies the gitleaks-derived transform
// processor in collector/redaction.yaml rewrites a known-bad secret to its
// REDACTED:<rule-id> placeholder before the row lands in ClickHouse.
//
// ci/main.go drives a telemetrygen container that pushes one log carrying
// OTELHOUSE_FAKE_SECRET in its body and one span carrying it in an
// attribute; both signals flow through the transform/redaction processor
// before the clickhouseexporter writes them. The test below polls until
// each row appears, then asserts the original literal is gone and the
// REDACTED placeholder is present — i.e. it checks both halves of "the
// pipeline ran and the redaction took effect" rather than just one.
//
// The test skips when CLICKHOUSE_HTTP_URL is unset so `go test ./...` from
// a bare checkout — without the Dagger services — stays a no-op.
func TestRedactionScrubsSecrets(t *testing.T) {
	base := os.Getenv("CLICKHOUSE_HTTP_URL")
	secret := os.Getenv("OTELHOUSE_FAKE_SECRET")
	if base == "" || secret == "" {
		t.Skip("CLICKHOUSE_HTTP_URL or OTELHOUSE_FAKE_SECRET not set; run via `make test`")
	}

	const (
		service     = "otelhouse-redaction-test"
		placeholder = "REDACTED:aws-access-token"
	)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	t.Run("log body", func(t *testing.T) {
		sql := fmt.Sprintf(
			`SELECT Body FROM otel_logs WHERE ServiceName = '%s' ORDER BY Timestamp DESC LIMIT 1`,
			service)
		row := waitForRow(ctx, t, base, sql)
		if strings.Contains(row, secret) {
			t.Fatalf("log body still contains the unredacted secret: %q", row)
		}
		if !strings.Contains(row, placeholder) {
			t.Fatalf("log body missing %q placeholder, got %q", placeholder, row)
		}
	})

	t.Run("span attribute", func(t *testing.T) {
		// SpanAttributes is a Map(String,String); ClickHouse renders it
		// as a TSV-friendly literal when selected directly, which is all
		// we need for substring assertions.
		sql := fmt.Sprintf(
			`SELECT toString(SpanAttributes) FROM otel_traces WHERE ServiceName = '%s' ORDER BY Timestamp DESC LIMIT 1`,
			service)
		row := waitForRow(ctx, t, base, sql)
		if strings.Contains(row, secret) {
			t.Fatalf("span attributes still contain the unredacted secret: %q", row)
		}
		if !strings.Contains(row, placeholder) {
			t.Fatalf("span attributes missing %q placeholder, got %q", placeholder, row)
		}
	})
}

// waitForRow polls until the SQL returns a non-empty row or ctx expires.
// Failure mode for "row never appears" is reported as a fatal test error
// with the last error from ClickHouse, so debugging starts from the actual
// HTTP/SQL response rather than a context-deadline message.
func waitForRow(ctx context.Context, t *testing.T, base, sql string) string {
	t.Helper()
	var (
		last    error
		row     string
		attempt int
	)
	for {
		attempt++
		raw, err := queryScalar(ctx, base, sql)
		if err == nil && strings.TrimSpace(raw) != "" {
			row = strings.TrimSpace(raw)
			t.Logf("row arrived after %d poll(s): %s", attempt, truncate(row, 200))
			return row
		}
		if err != nil {
			last = err
		} else {
			last = fmt.Errorf("empty result")
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for row (last error: %v): %v", last, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// queryScalar runs a SELECT against ClickHouse's HTTP interface and returns
// the raw response body. HTTP keeps the test dependency-free — no driver
// to vendor — at the cost of being limited to scalar/TSV-style results,
// which is all the assertions need.
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
