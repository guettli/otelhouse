package main

import (
	"context"
	"fmt"
	"os"

	"dagger.io/dagger"
)

// Pinned upstream component versions. Keep both tags identical so the
// telemetrygen we use to produce sample data is built from the same contrib
// tree as the Collector that ingests it. The clickhouseexporter metrics
// support is still alpha (#43), so this pin is the source of truth for the
// on-disk schema in ClickHouse.
const otelContribTag = "0.120.0"

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
	// /src/ci where the only Go module now lives; the Collector config under
	// collector/ is mounted into its own container further down.
	src := client.Host().Directory("..", dagger.HostDirectoryOpts{
		Exclude: []string{".git/"},
	})

	goMod := client.CacheVolume("otelhouse-go-mod")
	goBuild := client.CacheVolume("otelhouse-go-build")

	// Ephemeral ClickHouse for the integration harness. Credentials are set
	// explicitly because the image generates a random password for the default
	// user when no CLICKHOUSE_* env vars are given, which would make the
	// empty-password DSN fail with auth errors.
	clickhouse := client.Container().
		From("clickhouse/clickhouse-server:25.5").
		WithEnvVariable("CLICKHOUSE_USER", "test").
		WithEnvVariable("CLICKHOUSE_PASSWORD", "test").
		WithEnvVariable("CLICKHOUSE_DB", "test").
		WithExposedPort(9000).
		WithExposedPort(8123).
		AsService()

	// Upstream OTel Collector contrib build wired to the ClickHouse service.
	// The same `clickhouse` handle is reused below for the test container so
	// both containers share one ClickHouse instance and the data written here
	// is visible to the test queries.
	collector := client.Container().
		From("otel/opentelemetry-collector-contrib:"+otelContribTag).
		WithServiceBinding("clickhouse", clickhouse).
		WithMountedFile("/etc/otelcol/config.yaml", src.File("collector/config.yaml")).
		WithExposedPort(4317).
		WithExposedPort(4318).
		AsService(dagger.ContainerAsServiceOpts{
			Args:          []string{"--config=/etc/otelcol/config.yaml"},
			UseEntrypoint: true,
		})

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

	// Push sample metrics into the Collector via the upstream telemetrygen so
	// the integration test has data to assert against. Running this as a
	// one-shot container avoids adding a Go producer to the repo. Sync()
	// blocks until telemetrygen exits, by which time the Collector has the
	// metrics in flight; the test below polls for them to land.
	if _, err = client.Container().
		From("ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:"+otelContribTag).
		WithServiceBinding("collector", collector).
		WithExec(
			[]string{
				"metrics",
				"--otlp-endpoint", "collector:4317",
				"--otlp-insecure",
				"--metrics", "50",
			},
			dagger.ContainerWithExecOpts{UseEntrypoint: true},
		).Sync(ctx); err != nil {
		return fmt.Errorf("telemetrygen metrics: %w", err)
	}

	// Integration tests. The metrics test polls ClickHouse over HTTP for the
	// rows produced above; the Collector flush is asynchronous so the test
	// retries on its own deadline.
	if _, err = goBase.
		WithServiceBinding("clickhouse", clickhouse).
		WithEnvVariable("CLICKHOUSE_DSN", "clickhouse://test:test@clickhouse:9000/test").
		WithEnvVariable("CLICKHOUSE_HTTP_URL", "http://test:test@clickhouse:8123/").
		WithExec([]string{"go", "test", "-v", "-count=1", "./..."}).
		Sync(ctx); err != nil {
		return fmt.Errorf("go test: %w", err)
	}

	fmt.Println("All checks passed.")
	return nil
}
