package main

import (
	"context"
	"fmt"
	"os"

	"dagger.io/dagger"
)

// Pinned upstream OTel Collector contrib + telemetrygen versions. Bump
// together so the collector schema (created by the clickhouseexporter on
// first insert) and the emitter stay in sync.
const otelCollectorVersion = "0.114.0"

// The otel-collector-contrib image is built FROM scratch and ships neither
// /etc/passwd nor /etc/group. Dagger's container runtime refuses to exec
// into a UID/GID it can't resolve, so we inject minimal root entries and
// run as root — these are ephemeral CI containers on a private Dagger
// service network with no security surface.
const (
	distrolessRootPasswd = "root:x:0:0:root:/:/sbin/nologin\n"
	distrolessRootGroup  = "root:x:0:\n"
)

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
	// Stands up the Collector + telemetrygen + otelhouse-api binary as
	// Dagger services and runs the e2e Go test that hits the API and
	// asserts the responses.
	if err = runE2E(ctx, client, clickhouse, clickhouseDSN, src, goMod, goBuild, goBase); err != nil {
		return fmt.Errorf("e2e: %w", err)
	}

	fmt.Println("All checks passed.")
	return nil
}

// runE2E orchestrates the end-to-end harness: stand up the upstream OTel
// Collector wired to ClickHouse, drive sample OTLP traces/logs/metrics into
// it with telemetrygen, build and run the otelhouse-api binary as a Dagger
// service, and run the Go e2e test that hits the API and asserts the
// responses.
//
// The Collector owns the schema via clickhouseexporter (create_schema: true).
// otelhouse contains no exporter code of its own — see issues #29 / #37.
func runE2E(
	ctx context.Context,
	client *dagger.Client,
	clickhouse *dagger.Service,
	clickhouseDSN string,
	src *dagger.Directory,
	goMod, goBuild *dagger.CacheVolume,
	goBase *dagger.Container,
) error {
	collector := buildCollectorService(client, clickhouse, src)

	telemetrygen := buildTelemetrygen(client, goMod, goBuild)
	if err := runTelemetrygen(ctx, telemetrygen, collector); err != nil {
		return err
	}

	if err := verifyRows(ctx, client, clickhouse); err != nil {
		return err
	}

	api := buildAPIService(goBase, clickhouseDSN, clickhouse)

	// Run the e2e Go test. The test is gated by the `e2e` build tag, so
	// passing -tags e2e here picks it up. The fixed log TraceID is exposed
	// to the test so it can query /api/logs?traceId=... deterministically.
	if _, err := goBase.
		WithServiceBinding("clickhouse", clickhouse).
		WithServiceBinding("otelhouse-api", api).
		WithEnvVariable("OTELHOUSE_API_URL", "http://otelhouse-api:8080").
		WithEnvVariable("OTELHOUSE_E2E_LOG_TRACE_ID", e2eLogsTraceID).
		WithExec([]string{
			"go", "test", "-v", "-count=1", "-tags", "e2e", "-run", "TestE2E", "./...",
		}).
		Sync(ctx); err != nil {
		return fmt.Errorf("e2e test: %w", err)
	}
	return nil
}

// buildCollectorService stands up the upstream otel-collector-contrib image
// as a Dagger service, wired to the ClickHouse service via service binding
// and configured by ci/otel-collector-config.yaml.
func buildCollectorService(
	client *dagger.Client,
	clickhouse *dagger.Service,
	src *dagger.Directory,
) *dagger.Service {
	collectorImage := fmt.Sprintf("otel/opentelemetry-collector-contrib:%s", otelCollectorVersion)
	return client.Container().
		From(collectorImage).
		// See distrolessRootPasswd: the upstream image is FROM scratch and
		// has no /etc/passwd or /etc/group, so neither UID 0 nor GID 0 can
		// be resolved.
		WithNewFile("/etc/passwd", distrolessRootPasswd).
		WithNewFile("/etc/group", distrolessRootGroup).
		WithUser("0").
		WithServiceBinding("clickhouse", clickhouse).
		// The YAML reads these via ${env:...} so credentials stay defined
		// once in this file.
		WithEnvVariable("CLICKHOUSE_HOST", "clickhouse").
		WithEnvVariable("CLICKHOUSE_DB", clickhouseDB).
		WithEnvVariable("CLICKHOUSE_USER", clickhouseUser).
		WithEnvVariable("CLICKHOUSE_PASSWORD", clickhousePassword).
		WithFile("/etc/otelcol/config.yaml", src.File("ci/otel-collector-config.yaml")).
		WithExposedPort(4317).
		WithExposedPort(4318).
		// UseEntrypoint prepends the image's ENTRYPOINT (/otelcol-contrib)
		// — without it, Dagger tries to exec "--config=..." as a binary.
		WithExec(
			[]string{"--config=/etc/otelcol/config.yaml"},
			dagger.ContainerWithExecOpts{UseEntrypoint: true},
		).
		AsService()
}

// buildTelemetrygen builds the telemetrygen CLI from source in a regular
// golang:alpine container.
//
// We avoid the published distroless image (ghcr.io/.../telemetrygen): it is
// a multi-arch OCI index with attestation manifests, and Dagger v0.21.7
// hangs indefinitely trying to resolve it. Building from source via
// `go install` is a few seconds with the cache mounts and gives us a normal
// Alpine runtime (real /etc/passwd, shell, PATH).
func buildTelemetrygen(
	client *dagger.Client,
	goMod, goBuild *dagger.CacheVolume,
) *dagger.Container {
	pkg := fmt.Sprintf(
		"github.com/open-telemetry/opentelemetry-collector-contrib/cmd/telemetrygen@v%s",
		otelCollectorVersion,
	)
	return client.Container().
		From("golang:1.26-alpine").
		WithMountedCache("/go/pkg/mod", goMod).
		WithMountedCache("/root/.cache/go-build", goBuild).
		WithEnvVariable("CGO_ENABLED", "0").
		WithExec([]string{"go", "install", pkg})
}

// E2ELogsTraceID is the constant TraceID telemetrygen stamps onto every
// generated log record. The e2e test in ci/e2e_test.go reads this and
// queries /api/logs?traceId=E2ELogsTraceID, so the assertion does not have
// to discover the id at runtime by querying ClickHouse for a random row.
const e2eLogsTraceID = "1234567890abcdef1234567890abcdef"

// e2eLogsSpanID pairs with e2eLogsTraceID — telemetrygen requires a valid
// SpanID alongside the TraceID flag.
const e2eLogsSpanID = "1234567890abcdef"

// runTelemetrygen drives sample OTLP traces, metrics and logs into the
// Collector. telemetrygen blocks until it has sent the requested count
// over OTLP and received the receiver-level ack from the collector; the
// batch processor (1s timeout) then flushes to ClickHouse asynchronously,
// which is what verifyRows and the e2e test poll for.
//
// Traces and metrics use telemetrygen's default random ids. Logs are
// stamped with a fixed TraceID/SpanID so the e2e test can assert
// /api/logs?traceId=... returns deterministic content.
func runTelemetrygen(
	ctx context.Context,
	telemetrygen *dagger.Container,
	collector *dagger.Service,
) error {
	emissions := []struct {
		signal string
		extra  []string
	}{
		{"traces", nil},
		{"metrics", nil},
		{"logs", []string{
			"--trace-id", e2eLogsTraceID,
			"--span-id", e2eLogsSpanID,
		}},
	}
	for _, e := range emissions {
		args := []string{
			"/go/bin/telemetrygen", e.signal,
			"--otlp-endpoint", "otelcol:4317",
			"--otlp-insecure",
			"--" + e.signal, "20",
		}
		args = append(args, e.extra...)
		if _, err := telemetrygen.
			WithServiceBinding("otelcol", collector).
			WithExec(args).
			Sync(ctx); err != nil {
			return fmt.Errorf("telemetrygen %s: %w", e.signal, err)
		}
	}
	return nil
}

// verifyRows polls ClickHouse until each upstream-schema table expected for
// the signals we emitted has at least one row, or fails after ~30s.
// telemetrygen defaults to Gauge metrics, so we check otel_metrics_gauge
// specifically.
func verifyRows(ctx context.Context, client *dagger.Client, clickhouse *dagger.Service) error {
	script := fmt.Sprintf(`set -eu
tables="otel_traces otel_logs otel_metrics_gauge"
for t in $tables; do
  count=0
  for i in $(seq 1 30); do
    count=$(clickhouse-client --host=clickhouse --user=%s --password=%s --database=%s --query="SELECT count() FROM $t" 2>/dev/null || echo 0)
    if [ "$count" -gt 0 ]; then
      echo "$t: $count rows"
      break
    fi
    sleep 1
  done
  if [ "$count" -eq 0 ]; then
    echo "no rows in $t after 30s" >&2
    exit 1
  fi
done`, clickhouseUser, clickhousePassword, clickhouseDB)

	if _, err := client.Container().
		From("clickhouse/clickhouse-server:25.5").
		WithServiceBinding("clickhouse", clickhouse).
		WithExec([]string{"sh", "-c", script}).
		Sync(ctx); err != nil {
		return fmt.Errorf("verify rows: %w", err)
	}
	return nil
}

// buildAPIService builds the otelhouse-api binary inside goBase and exposes
// it on :8080 as a Dagger service pointed at the ClickHouse binding.
func buildAPIService(
	goBase *dagger.Container,
	clickhouseDSN string,
	clickhouse *dagger.Service,
) *dagger.Service {
	return goBase.
		WithExec([]string{
			"go", "build",
			"-o", "/usr/local/bin/otelhouse-api",
			"./cmd/otelhouse-api",
		}).
		WithServiceBinding("clickhouse", clickhouse).
		WithExposedPort(8080).
		WithExec([]string{
			"/usr/local/bin/otelhouse-api",
			"-addr", ":8080",
			"-dsn", clickhouseDSN,
		}).
		AsService()
}
