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
	// Stands up the Collector + otelhouse-emit + otelhouse-api binary as
	// Dagger services and runs the e2e Go test that hits the API and
	// asserts the responses.
	if err = runE2E(ctx, client, clickhouse, clickhouseDSN, src, goBase); err != nil {
		return fmt.Errorf("e2e: %w", err)
	}

	fmt.Println("All checks passed.")
	return nil
}

// runE2E orchestrates the end-to-end harness: stand up the upstream OTel
// Collector wired to ClickHouse, drive sample OTLP traces/logs/metrics into
// it with the in-repo otelhouse-emit binary, build and run the otelhouse-api
// binary as a Dagger service, and run the Go e2e test that hits the API
// and asserts the responses.
//
// The Collector owns the schema via clickhouseexporter (create_schema: true).
// otelhouse contains no exporter code of its own — see issues #29 / #37.
func runE2E(
	ctx context.Context,
	client *dagger.Client,
	clickhouse *dagger.Service,
	clickhouseDSN string,
	src *dagger.Directory,
	goBase *dagger.Container,
) error {
	// Cap the whole e2e step. The pipeline previously hung at 6h on a
	// distroless-image issue; an explicit deadline turns a hang into a
	// timely, debuggable failure.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()

	logStep("collector: starting otel-collector-contrib service")
	collector := buildCollectorService(client, clickhouse, src)
	// Force the collector to start (and pass its port healthchecks) eagerly,
	// before any container binds to it. The previous CI run lazily started
	// the collector via the emitter's WithServiceBinding; the collector logged
	// "Everything is ready" but Dagger never moved on to building or running
	// the emitter, and the whole step died on the 20m deadline with a generic
	// "Post http://dagger/query: context deadline exceeded" — see #50.
	// Starting eagerly here means a stuck startup fails at this step with the
	// collector's own logs attached, instead of a silent hang in the emitter.
	collector, err := collector.Start(ctx)
	if err != nil {
		return fmt.Errorf("start collector: %w", err)
	}
	logStep("collector: service is up")

	logStep("emitter: building otelhouse-emit binary")
	emitter := buildEmitter(goBase)
	// Materialise the build before any service binding/emission, so a build
	// failure surfaces as a build failure (and the binary is cached for the
	// three subsequent emission containers).
	emitter, err = emitter.Sync(ctx)
	if err != nil {
		return fmt.Errorf("build emitter: %w", err)
	}

	logStep("emitter: driving OTLP traces/metrics/logs into collector")
	if err := runEmitter(ctx, emitter, collector); err != nil {
		return err
	}

	logStep("verify: polling clickhouse for rows in upstream tables")
	if err := verifyRows(ctx, client, clickhouse); err != nil {
		return err
	}

	logStep("api: building otelhouse-api and starting as service")
	api := buildAPIService(goBase, clickhouseDSN, clickhouse)
	// Same rationale as the collector: start the API eagerly so any startup
	// failure (e.g. DSN auth, port bind, ClickHouse connectivity) is reported
	// here rather than as an opaque hang in the e2e go test step below.
	api, err = api.Start(ctx)
	if err != nil {
		return fmt.Errorf("start api: %w", err)
	}
	logStep("api: service is up")

	logStep("e2e: running go test -tags e2e against the live API")
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
	logStep("e2e: all assertions passed")
	return nil
}

// logStep prints a timestamped progress marker to stderr so a hang in the
// Dagger pipeline can be pinpointed to a specific step from CI logs.
func logStep(msg string) {
	fmt.Fprintf(os.Stderr, "[e2e %s] %s\n", time.Now().UTC().Format(time.RFC3339), msg)
}

// buildCollectorService stands up the upstream otel-collector-contrib binary
// as a Dagger service, wired to the ClickHouse service via service binding
// and configured by ci/otel-collector-config.yaml.
//
// The binary is extracted from the upstream distroless image and run on top
// of alpine. The distroless image (FROM scratch) is unusable as a Dagger
// service in Dagger v0.21.7: the collector boots and serves on its ports,
// but Dagger's service-readiness probe never reports the ports as ready and
// every dependent WithServiceBinding hangs until the context expires.
// Putting the same binary on a normal Linux base image makes the service
// behave like any other Dagger service.
func buildCollectorService(
	client *dagger.Client,
	clickhouse *dagger.Service,
	src *dagger.Directory,
) *dagger.Service {
	collectorImage := fmt.Sprintf("otel/opentelemetry-collector-contrib:%s", otelCollectorVersion)
	collectorBin := client.Container().From(collectorImage).File("/otelcol-contrib")

	return client.Container().
		From("alpine:3.20").
		WithFile("/usr/local/bin/otelcol-contrib", collectorBin).
		WithFile("/etc/otelcol/config.yaml", src.File("ci/otel-collector-config.yaml")).
		WithServiceBinding("clickhouse", clickhouse).
		// The YAML reads these via ${env:...} so credentials stay defined
		// once in this file.
		WithEnvVariable("CLICKHOUSE_HOST", "clickhouse").
		WithEnvVariable("CLICKHOUSE_DB", clickhouseDB).
		WithEnvVariable("CLICKHOUSE_USER", clickhouseUser).
		WithEnvVariable("CLICKHOUSE_PASSWORD", clickhousePassword).
		// Only the OTLP gRPC port is consumed by the emitter; exposing the
		// HTTP port too (4318) adds an extra Dagger TCP healthcheck for no
		// benefit and was a candidate root cause for the #50 hang.
		WithExposedPort(4317).
		WithExec([]string{
			"/usr/local/bin/otelcol-contrib",
			"--config=/etc/otelcol/config.yaml",
		}).
		AsService()
}

// e2eLogsTraceID is the constant TraceID the local emitter stamps onto
// every generated log record. The e2e test reads this and queries
// /api/logs?traceId=e2eLogsTraceID, so the assertion does not have to
// discover the id at runtime by querying ClickHouse for a random row.
const e2eLogsTraceID = "1234567890abcdef1234567890abcdef"

// e2eLogsSpanID pairs with e2eLogsTraceID.
const e2eLogsSpanID = "1234567890abcdef"

// buildEmitter compiles the in-repo otelhouse-emit binary inside goBase.
//
// We deliberately do not use the upstream telemetrygen here: `go install
// github.com/open-telemetry/opentelemetry-collector-contrib/cmd/telemetrygen`
// pulls the entire opentelemetry-collector-contrib dependency tree (every
// receiver, processor and exporter in contrib), which takes 10+ minutes on
// a cold CI cache and timed out the workflow. The local emitter under
// ci/cmd/otelhouse-emit uses only the OTel Go SDK and OTLP exporters and
// shares the goBase Go module cache that the gofmt / vet / build / test
// steps already populated.
func buildEmitter(goBase *dagger.Container) *dagger.Container {
	return goBase.WithExec([]string{
		"go", "build",
		"-o", "/usr/local/bin/otelhouse-emit",
		"./cmd/otelhouse-emit",
	})
}

// runEmitter drives sample OTLP traces, metrics and logs into the Collector.
// The emitter blocks until it has sent the requested count over OTLP and
// flushed; the Collector's batch processor (1s timeout) then writes to
// ClickHouse asynchronously, which is what verifyRows and the e2e test poll
// for.
//
// Traces and metrics use the OTel SDK's default random ids. Logs are stamped
// with a fixed TraceID/SpanID so the e2e test can assert
// /api/logs?traceId=... returns deterministic content.
func runEmitter(
	ctx context.Context,
	emitter *dagger.Container,
	collector *dagger.Service,
) error {
	emissions := []struct {
		signal string
		extra  []string
	}{
		{"traces", nil},
		{"metrics", nil},
		{"logs", []string{
			"-trace-id", e2eLogsTraceID,
			"-span-id", e2eLogsSpanID,
		}},
	}
	for _, e := range emissions {
		args := []string{
			"/usr/local/bin/otelhouse-emit",
			"-signal", e.signal,
			"-endpoint", "otelcol:4317",
			"-count", "20",
		}
		args = append(args, e.extra...)
		if _, err := emitter.
			WithServiceBinding("otelcol", collector).
			WithExec(args).
			Sync(ctx); err != nil {
			return fmt.Errorf("emit %s: %w", e.signal, err)
		}
	}
	return nil
}

// verifyRows polls ClickHouse until each upstream-schema table expected for
// the signals we emitted has at least one row, or fails after ~30s. The
// emitter records gauge metrics, so we check otel_metrics_gauge specifically.
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
