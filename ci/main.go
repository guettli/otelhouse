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

// ClickHouse credentials used by every component in the harness (the server,
// the collector exporter, query containers). Centralised here so the YAML
// stays generic and consumes them via ${env:...}.
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

	// Mount the whole repo (minus .git/). The Go checks below run from
	// /src/ci where the only Go module now lives; the collector harness
	// reads ci/otel-collector-config.yaml from the same tree.
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

	// Integration tests. The ci/ module currently has no _test.go files, so
	// this is a vacuous pass; the ClickHouse service binding stays wired up
	// so future Go-side tests can reach the same service.
	if _, err = goBase.
		WithServiceBinding("clickhouse", clickhouse).
		WithEnvVariable("CLICKHOUSE_DSN", fmt.Sprintf(
			"clickhouse://%s:%s@clickhouse:9000/%s",
			clickhouseUser, clickhousePassword, clickhouseDB,
		)).
		WithExec([]string{"go", "test", "-v", "-count=1", "./..."}).
		Sync(ctx); err != nil {
		return fmt.Errorf("go test: %w", err)
	}

	// Ingestion backbone: stand up the upstream OTel Collector with the
	// clickhouseexporter, emit OTLP traces/metrics/logs, and verify the
	// rows land in the upstream-schema tables.
	if err = runCollectorHarness(ctx, client, clickhouse, src); err != nil {
		return fmt.Errorf("collector harness: %w", err)
	}

	fmt.Println("All checks passed.")
	return nil
}

// runCollectorHarness brings up an OTel Collector (contrib) wired to the
// already-running ClickHouse service, drives sample OTLP telemetry into it
// with telemetrygen, and asserts the rows land in the upstream schema
// (otel_traces, otel_logs, otel_metrics_gauge).
//
// The Collector owns the schema via clickhouseexporter (create_schema: true).
// otelhouse contains no exporter code of its own — see issues #29 / #37.
func runCollectorHarness(
	ctx context.Context,
	client *dagger.Client,
	clickhouse *dagger.Service,
	src *dagger.Directory,
) error {
	collectorImage := fmt.Sprintf("otel/opentelemetry-collector-contrib:%s", otelCollectorVersion)
	telemetrygenImage := fmt.Sprintf(
		"ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:v%s",
		otelCollectorVersion,
	)

	collector := client.Container().
		From(collectorImage).
		// The upstream collector image declares USER 10001 but that UID
		// is missing from /etc/passwd in the base layer, and Dagger
		// refuses to exec into an unresolvable user. Run as root — this
		// is an ephemeral CI container with no security surface, and the
		// collector itself does no privileged work.
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
		WithExec([]string{"--config=/etc/otelcol/config.yaml"}).
		AsService()

	// telemetrygen blocks until it has sent the requested count over OTLP
	// and received the receiver-level ack from the collector. The batch
	// processor (1s timeout) then flushes to ClickHouse asynchronously,
	// which is what the polling loop in verifyRows waits on.
	emissions := []struct {
		subcommand string
		countFlag  string
	}{
		{"traces", "--traces"},
		{"metrics", "--metrics"},
		{"logs", "--logs"},
	}
	for _, e := range emissions {
		if _, err := client.Container().
			From(telemetrygenImage).
			// The upstream telemetrygen image declares USER 10001, but
			// that UID is absent from /etc/passwd in the base image and
			// Dagger refuses to exec into an unresolvable user. Run as
			// root — this is an ephemeral CI container with no security
			// surface.
			WithUser("0").
			WithServiceBinding("otelcol", collector).
			WithExec([]string{
				"telemetrygen", e.subcommand,
				"--otlp-endpoint", "otelcol:4317",
				"--otlp-insecure",
				e.countFlag, "20",
			}).
			Sync(ctx); err != nil {
			return fmt.Errorf("telemetrygen %s: %w", e.subcommand, err)
		}
	}

	return verifyRows(ctx, client, clickhouse)
}

// verifyRows polls ClickHouse until each of the upstream-schema tables
// expected for the signals we emitted has at least one row, or fails after
// ~30s. telemetrygen defaults to Gauge metrics, so we check
// otel_metrics_gauge specifically.
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
