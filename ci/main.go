package main

import (
	"context"
	"fmt"
	"os"

	"dagger.io/dagger"
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
	defer client.Close()

	// Repo root is one level up from ci/.
	src := client.Host().Directory("..", dagger.HostDirectoryOpts{
		Exclude: []string{".git/", "ci/"},
	})

	goMod := client.CacheVolume("otelhouse-go-mod")
	goBuild := client.CacheVolume("otelhouse-go-build")

	// ClickHouse service used for integration tests.
	// Credentials are set explicitly because the image's entrypoint generates
	// a random password for the default user when no CLICKHOUSE_* env vars are
	// provided, which makes the empty-password DSN fail with auth errors.
	// Dagger's OTel traces can also be forwarded here by setting
	// OTEL_EXPORTER_OTLP_ENDPOINT to a receiver backed by the otelhouse exporter.
	clickhouse := client.Container().
		From("clickhouse/clickhouse-server:25.5").
		WithEnvVariable("CLICKHOUSE_USER", "test").
		WithEnvVariable("CLICKHOUSE_PASSWORD", "test").
		WithEnvVariable("CLICKHOUSE_DB", "test").
		WithExposedPort(9000).
		WithExposedPort(8123).
		AsService()

	goBase := client.Container().
		From("golang:1.26-alpine").
		WithMountedCache("/go/pkg/mod", goMod).
		WithMountedCache("/root/.cache/go-build", goBuild).
		WithMountedDirectory("/src", src).
		WithWorkdir("/src")

	// gofmt
	if _, err = goBase.
		WithExec([]string{"sh", "-c",
			`out=$(gofmt -l .); if [ -n "$out" ]; then echo "unformatted: $out" >&2; exit 1; fi`,
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
		WithWorkdir("/src")
	if _, err = lintCtr.WithExec([]string{"golangci-lint", "run", "./..."}).Sync(ctx); err != nil {
		return fmt.Errorf("golangci-lint: %w", err)
	}

	// go build
	if _, err = goBase.WithExec([]string{"go", "build", "./..."}).Sync(ctx); err != nil {
		return fmt.Errorf("go build: %w", err)
	}

	// Integration tests with a live ClickHouse instance.
	// The service hostname is "clickhouse" inside the Dagger network.
	// Setting OTEL_EXPORTER_OTLP_ENDPOINT here would route the test container's
	// own OTel output into ClickHouse via the exporter under test.
	if _, err = goBase.
		WithServiceBinding("clickhouse", clickhouse).
		WithEnvVariable("CLICKHOUSE_DSN", "clickhouse://test:test@clickhouse:9000/test").
		WithExec([]string{"go", "test", "-v", "-count=1", "./..."}).
		Sync(ctx); err != nil {
		return fmt.Errorf("go test: %w", err)
	}

	fmt.Println("All checks passed.")
	return nil
}
