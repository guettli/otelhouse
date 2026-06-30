package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"dagger.io/dagger"
)

// Pinned upstream OTel Collector contrib image. Drives the schema the
// clickhouseexporter creates on first insert; bump when the schema changes.
const otelCollectorVersion = "0.114.0"

// ClickHouse credentials used by every component in the harness (server,
// collector exporter, query containers). Centralised here so the YAML stays
// generic and consumes them via ${env:...}.
const (
	clickhouseUser     = "test"
	clickhousePassword = "test"
	clickhouseDB       = "test"
)

func main() {
	ctx := context.Background()
	if err := pipeline(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func pipeline(ctx context.Context) error {
	client, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		return fmt.Errorf("dagger connect: %w", err)
	}
	defer func() { _ = client.Close() }()

	// Mount the whole repo (minus .git/). The Go checks run from /src/ci
	// where the only Go module lives; the harness reads the collector
	// config from the same tree.
	src := client.Host().Directory("..", dagger.HostDirectoryOpts{
		Exclude: []string{".git/"},
	})

	goMod := client.CacheVolume("otelhouse-go-mod")
	goBuild := client.CacheVolume("otelhouse-go-build")

	// Ephemeral ClickHouse for the integration harness. Credentials are set
	// explicitly because the image generates a random password for the
	// default user when no CLICKHOUSE_* env vars are given, which makes the
	// empty-password DSN fail with auth errors.
	clickhouse := client.Container().
		From("clickhouse/clickhouse-server:25.5").
		WithEnvVariable("CLICKHOUSE_USER", clickhouseUser).
		WithEnvVariable("CLICKHOUSE_PASSWORD", clickhousePassword).
		WithEnvVariable("CLICKHOUSE_DB", clickhouseDB).
		WithExposedPort(9000).
		WithExposedPort(8123).
		AsService()

	clickhouseDSN := fmt.Sprintf(
		"clickhouse://%s:%s@clickhouse:9000/%s",
		clickhouseUser, clickhousePassword, clickhouseDB,
	)

	goBase := client.Container().
		From("golang:1.26-alpine").
		WithMountedCache("/go/pkg/mod", goMod).
		WithMountedCache("/root/.cache/go-build", goBuild).
		WithMountedDirectory("/src", src).
		WithWorkdir("/src/ci")

	// gofmt — scan the entire tree so any future Go file outside ci/ is
	// covered too.
	if _, err = goBase.
		WithExec([]string{"sh", "-c",
			`out=$(gofmt -l /src); if [ -n "$out" ]; then echo "unformatted: $out" >&2; exit 1; fi`,
		}).Sync(ctx); err != nil {
		return fmt.Errorf("gofmt: %w", err)
	}

	// go vet
	if _, err = goBase.WithExec([]string{"go", "vet", "./..."}).Sync(ctx); err != nil {
		return fmt.Errorf("go vet: %w", err)
	}

	// golangci-lint
	lintCtr := client.Container().
		From("golangci/golangci-lint:v2.12.2-alpine").
		WithMountedDirectory("/src", src).
		WithWorkdir("/src/ci")
	if _, err = lintCtr.WithExec([]string{"golangci-lint", "run", "./..."}).Sync(ctx); err != nil {
		return fmt.Errorf("golangci-lint: %w", err)
	}

	// go build
	if _, err = goBase.WithExec([]string{"go", "build", "./..."}).Sync(ctx); err != nil {
		return fmt.Errorf("go build: %w", err)
	}

	// Unit-style tests against the live ClickHouse service. The e2e test
	// in ci/e2e_test.go is gated behind the `e2e` build tag so it is NOT
	// picked up here — it runs in runE2E below, against the full
	// Collector+API stack.
	if _, err = goBase.
		WithServiceBinding("clickhouse", clickhouse).
		WithEnvVariable("CLICKHOUSE_DSN", clickhouseDSN).
		WithExec([]string{"go", "test", "-v", "-count=1", "./..."}).
		Sync(ctx); err != nil {
		return fmt.Errorf("go test: %w", err)
	}

	// End-to-end test: Dagger → OTLP → Collector → ClickHouse → API.
	if err = runE2E(ctx, client, clickhouse, clickhouseDSN, src, goBase); err != nil {
		return fmt.Errorf("e2e: %w", err)
	}

	fmt.Println("All checks passed.")
	return nil
}

// runE2E orchestrates the end-to-end harness inside ONE Dagger container:
// the upstream OTel Collector binary, the in-repo otelhouse-emit and
// otelhouse-api binaries, and the Go e2e test all run as local processes
// against a single inbound ClickHouse service binding. The Collector owns
// the ClickHouse schema via clickhouseexporter (create_schema: true);
// otelhouse contains no exporter code of its own (see #29 / #37).
//
// Why one container, not three Dagger services chained together:
// previous CI runs (#50) showed that running the collector — or any
// container that itself has a WithServiceBinding — as a Dagger service
// hangs the entire step on the deadline (~20m) on Dagger v0.21.7, even
// with ExperimentalSkipHealthcheck on every exposed port. The collector
// boots and logs "Everything is ready", but Service.Start never returns.
// Running collector and API as background processes inside one container
// removes the chained service binding entirely and uses only the
// ClickHouse service binding, which is the same pattern the earlier
// `go test ./...` step already exercises successfully.
func runE2E(
	ctx context.Context,
	client *dagger.Client,
	clickhouse *dagger.Service,
	clickhouseDSN string,
	src *dagger.Directory,
	goBase *dagger.Container,
) error {
	// Cap the whole e2e step. With everything inline this finishes in well
	// under a minute; the deadline only exists so an unexpected hang turns
	// into a timely, debuggable failure.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	logStep("e2e: extracting otel-collector-contrib binary from upstream image")
	collectorImage := fmt.Sprintf("otel/opentelemetry-collector-contrib:%s", otelCollectorVersion)
	collectorBin := client.Container().From(collectorImage).File("/otelcol-contrib")

	logStep("e2e: running all-in-one harness container")
	if _, err := goBase.
		WithFile("/usr/local/bin/otelcol-contrib", collectorBin).
		WithFile("/etc/otelcol/config.yaml", src.File("ci/otel-collector-config.yaml")).
		WithServiceBinding("clickhouse", clickhouse).
		// The collector YAML reads these via ${env:...}.
		WithEnvVariable("CLICKHOUSE_HOST", "clickhouse").
		WithEnvVariable("CLICKHOUSE_DB", clickhouseDB).
		WithEnvVariable("CLICKHOUSE_USER", clickhouseUser).
		WithEnvVariable("CLICKHOUSE_PASSWORD", clickhousePassword).
		WithEnvVariable("CLICKHOUSE_DSN", clickhouseDSN).
		WithEnvVariable("OTELHOUSE_E2E_LOG_TRACE_ID", e2eLogsTraceID).
		WithEnvVariable("OTELHOUSE_E2E_LOG_SPAN_ID", e2eLogsSpanID).
		WithEnvVariable("OTELHOUSE_API_URL", "http://127.0.0.1:8080").
		WithExec([]string{"sh", "-c", e2eScript}).
		Sync(ctx); err != nil {
		return fmt.Errorf("e2e harness: %w", err)
	}
	logStep("e2e: all assertions passed")
	return nil
}

// logStep prints a timestamped progress marker to stderr so the pipeline's
// position can be read straight off CI logs.
func logStep(msg string) {
	fmt.Fprintf(os.Stderr, "[e2e %s] %s\n", time.Now().UTC().Format(time.RFC3339), msg)
}

// e2eLogsTraceID is the constant TraceID the local emitter stamps onto
// every generated log record. The e2e test reads this and queries
// /api/logs?traceId=e2eLogsTraceID, so the assertion does not have to
// discover the id at runtime by querying ClickHouse for a random row.
const e2eLogsTraceID = "1234567890abcdef1234567890abcdef"

// e2eLogsSpanID pairs with e2eLogsTraceID.
const e2eLogsSpanID = "1234567890abcdef"

// e2eScript is the busybox-sh script that runs inside the single Dagger
// e2e container. It builds the in-repo emitter and API, runs the upstream
// collector and the API as background processes, emits traces/metrics/logs
// over OTLP, and finally runs the Go e2e test that hits the API.
//
// We deliberately don't use telemetrygen: `go install
// github.com/open-telemetry/opentelemetry-collector-contrib/cmd/telemetrygen`
// pulls the whole contrib tree and timed out the workflow on a cold cache.
const e2eScript = `set -eu

echo "[e2e-sh] building otelhouse-emit and otelhouse-api"
go build -o /usr/local/bin/otelhouse-emit ./cmd/otelhouse-emit
go build -o /usr/local/bin/otelhouse-api  ./cmd/otelhouse-api

mkdir -p /tmp/e2e

echo "[e2e-sh] starting otel-collector-contrib (background)"
/usr/local/bin/otelcol-contrib --config=/etc/otelcol/config.yaml \
  > /tmp/e2e/collector.log 2>&1 &
COLLECTOR_PID=$!

cleanup() {
  status=$?
  echo "[e2e-sh] cleaning up (exit=$status)"
  kill "$COLLECTOR_PID" 2>/dev/null || true
  if [ -n "${API_PID:-}" ]; then kill "$API_PID" 2>/dev/null || true; fi
  if [ "$status" -ne 0 ]; then
    echo "=== collector.log ==="
    cat /tmp/e2e/collector.log 2>/dev/null || true
    echo "=== api.log ==="
    cat /tmp/e2e/api.log 2>/dev/null || true
  fi
}
trap cleanup EXIT

echo "[e2e-sh] waiting for collector to log 'Everything is ready'"
for i in $(seq 1 60); do
  if grep -q "Everything is ready" /tmp/e2e/collector.log 2>/dev/null; then
    break
  fi
  sleep 1
done
if ! grep -q "Everything is ready" /tmp/e2e/collector.log 2>/dev/null; then
  echo "[e2e-sh] collector did not become ready in 60s" >&2
  exit 1
fi
echo "[e2e-sh] collector is ready"

echo "[e2e-sh] emitting OTLP traces, metrics and logs"
/usr/local/bin/otelhouse-emit -signal traces  -endpoint 127.0.0.1:4317 -count 20
/usr/local/bin/otelhouse-emit -signal metrics -endpoint 127.0.0.1:4317 -count 20
/usr/local/bin/otelhouse-emit -signal logs    -endpoint 127.0.0.1:4317 -count 20 \
  -trace-id "$OTELHOUSE_E2E_LOG_TRACE_ID" -span-id "$OTELHOUSE_E2E_LOG_SPAN_ID"

# Collector batch processor flushes every 1s; give it a brief grace period
# before the API queries the upstream tables.
echo "[e2e-sh] waiting for collector to flush rows to clickhouse"
sleep 3

echo "[e2e-sh] starting otelhouse-api (background)"
/usr/local/bin/otelhouse-api -addr 127.0.0.1:8080 -dsn "$CLICKHOUSE_DSN" \
  > /tmp/e2e/api.log 2>&1 &
API_PID=$!

echo "[e2e-sh] waiting for API to answer /api/runs"
for i in $(seq 1 30); do
  if wget -q -O /dev/null "$OTELHOUSE_API_URL/api/runs" 2>/dev/null; then
    break
  fi
  sleep 1
done
if ! wget -q -O /dev/null "$OTELHOUSE_API_URL/api/runs" 2>/dev/null; then
  echo "[e2e-sh] api did not become ready in 30s" >&2
  exit 1
fi
echo "[e2e-sh] api is ready"

echo "[e2e-sh] running go test -tags e2e"
go test -v -count=1 -tags e2e -run TestE2E ./...
echo "[e2e-sh] all assertions passed"
`
