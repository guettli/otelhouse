package main

import (
	"context"
	"fmt"
	"os"

	"dagger.io/dagger"
)

// Pinned upstream component versions. Keep both tags identical so the
// telemetrygen producer is built from the same contrib tree as the
// Collector that ingests it.
const otelContribTag = "0.120.0"

// A fake AWS access key ID injected by the redaction test so it has a
// known-bad secret to push through the Collector. The literal matches
// the gitleaks `aws-access-token` rule (\b(?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16}\b)
// and is the IAM example string from the AWS documentation, not a real
// credential. Kept here so producer (this file) and assertion
// (ci/redaction_test.go) stay in sync.
const fakeAWSAccessKey = "AKIAIOSFODNN7EXAMPLE"

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
	// /src/ci where the only Go module now lives; the Collector configs
	// under collector/ are mounted into their own container further down.
	src := client.Host().Directory("..", dagger.HostDirectoryOpts{
		Exclude: []string{".git/"},
	})

	goMod := client.CacheVolume("otelhouse-go-mod")
	goBuild := client.CacheVolume("otelhouse-go-build")

	// Ephemeral ClickHouse for the integration harness. Credentials are set
	// explicitly because the image generates a random password for the
	// default user when no CLICKHOUSE_* env vars are given, which would make
	// the empty-password DSN fail with auth errors.
	clickhouse := client.Container().
		From("clickhouse/clickhouse-server:25.5").
		WithEnvVariable("CLICKHOUSE_USER", "test").
		WithEnvVariable("CLICKHOUSE_PASSWORD", "test").
		WithEnvVariable("CLICKHOUSE_DB", "test").
		WithExposedPort(9000).
		WithExposedPort(8123).
		AsService()

	// Upstream OTel Collector contrib build wired to the ClickHouse service.
	// Two --config flags are passed: the hand-written base config plus the
	// generated gitleaks redaction fragment. The Collector deep-merges them
	// at startup, which is how the pipelines in config.yaml resolve the
	// transform/redaction processor defined in redaction.yaml.
	collector := client.Container().
		From("otel/opentelemetry-collector-contrib:"+otelContribTag).
		WithServiceBinding("clickhouse", clickhouse).
		WithMountedFile("/etc/otelcol/config.yaml", src.File("collector/config.yaml")).
		WithMountedFile("/etc/otelcol/redaction.yaml", src.File("collector/redaction.yaml")).
		WithExposedPort(4317).
		WithExposedPort(4318).
		AsService(dagger.ContainerAsServiceOpts{
			Args: []string{
				"--config=/etc/otelcol/config.yaml",
				"--config=/etc/otelcol/redaction.yaml",
			},
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

	// Push one log carrying fakeAWSAccessKey in its body through the
	// Collector so the redaction integration test has a row to assert on.
	// Sync() blocks until telemetrygen exits; the Collector batch flush is
	// short (see collector/config.yaml) and the test polls regardless.
	if _, err = client.Container().
		From("ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:"+otelContribTag).
		WithServiceBinding("collector", collector).
		WithExec(
			[]string{
				"logs",
				"--otlp-endpoint", "collector:4317",
				"--otlp-insecure",
				"--logs", "1",
				"--body", "redaction-fixture " + fakeAWSAccessKey,
				"--otlp-attributes", `service.name="otelhouse-redaction-test"`,
			},
			dagger.ContainerWithExecOpts{UseEntrypoint: true},
		).Sync(ctx); err != nil {
		return fmt.Errorf("telemetrygen logs: %w", err)
	}

	// And one span carrying the same fake key in an attribute. Span
	// attributes go through replace_all_patterns(attributes, "value", ...)
	// in the transform processor, which is a different OTTL function from
	// the body case above — testing both keeps the YAML fragment honest.
	if _, err = client.Container().
		From("ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:"+otelContribTag).
		WithServiceBinding("collector", collector).
		WithExec(
			[]string{
				"traces",
				"--otlp-endpoint", "collector:4317",
				"--otlp-insecure",
				"--traces", "1",
				"--otlp-attributes", `service.name="otelhouse-redaction-test"`,
				"--telemetry-attributes", `aws.key="` + fakeAWSAccessKey + `"`,
			},
			dagger.ContainerWithExecOpts{UseEntrypoint: true},
		).Sync(ctx); err != nil {
		return fmt.Errorf("telemetrygen traces: %w", err)
	}

	// Integration tests. The redaction test polls ClickHouse over HTTP for
	// the rows produced above; the Collector flush is asynchronous so the
	// test retries on its own deadline.
	if _, err = goBase.
		WithServiceBinding("clickhouse", clickhouse).
		WithEnvVariable("CLICKHOUSE_DSN", "clickhouse://test:test@clickhouse:9000/test").
		WithEnvVariable("CLICKHOUSE_HTTP_URL", "http://test:test@clickhouse:8123/").
		WithEnvVariable("OTELHOUSE_FAKE_SECRET", fakeAWSAccessKey).
		WithExec([]string{"go", "test", "-v", "-count=1", "./..."}).
		Sync(ctx); err != nil {
		return fmt.Errorf("go test: %w", err)
	}

	fmt.Println("All checks passed.")
	return nil
}
