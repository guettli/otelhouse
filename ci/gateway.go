package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"dagger.io/dagger"
)

// gatewayImageRepo is the canonical registry path for the gateway image.
// gitops#82 deploys from here.
const gatewayImageRepo = "ghcr.io/guettli/otelhouse-gateway"

// buildGatewayImage runs ocb against collector/builder-config.yaml inside a
// Go builder container and assembles the resulting binary into a minimal
// distroless runtime image. The image contains only the binary and the
// distroless base — no config, no keys, no build tools. The collector
// config is supplied at deploy time via a k8s ConfigMap (gitops#82) so no
// tenant material is ever baked in.
func buildGatewayImage(
	client *dagger.Client,
	src *dagger.Directory,
	goMod, goBuild *dagger.CacheVolume,
) *dagger.Container {
	builder := client.Container().
		From("golang:1.26-alpine").
		// ocb generates a Go program that pulls in the components listed
		// in builder-config.yaml; some indirect module downloads need git.
		WithExec([]string{"apk", "add", "--no-cache", "git"}).
		WithMountedCache("/go/pkg/mod", goMod).
		WithMountedCache("/root/.cache/go-build", goBuild).
		// CGO_ENABLED=0 forces a fully static binary so it runs on
		// distroless/static (no libc, no dynamic loader).
		WithEnvVariable("CGO_ENABLED", "0").
		// Pin ocb to otelCollectorVersion — the same version the component
		// modules in builder-config.yaml are pinned to (that manifest has no
		// otelcol_version key; builder v0.114.0 removed it), so the builder
		// and the components it wires together stay in lockstep.
		WithExec([]string{"go", "install",
			"go.opentelemetry.io/collector/cmd/builder@v" + otelCollectorVersion}).
		WithMountedDirectory("/src", src).
		WithWorkdir("/src/collector").
		// dist.output_path (./_build) and dist.name (otelhouse-gateway)
		// in builder-config.yaml determine where ocb writes the binary.
		// Copy it out of the mount so .File() below reads from the
		// container's own filesystem, not the mount overlay.
		WithExec([]string{"/go/bin/builder", "--config=builder-config.yaml"}).
		WithExec([]string{"cp", "/src/collector/_build/otelhouse-gateway", "/otelhouse-gateway"})

	binary := builder.File("/otelhouse-gateway")

	return client.Container().
		From("gcr.io/distroless/static-debian12:nonroot").
		WithFile("/otelhouse-gateway", binary).
		// Config is deliberately NOT baked in — it is supplied at deploy
		// time via a k8s ConfigMap so no keys or secrets ever land in
		// the image.
		WithEntrypoint([]string{"/otelhouse-gateway"})
}

// publishGatewayImage pushes the assembled gateway image to
// ghcr.io/guettli/otelhouse-gateway. The primary tag is the pinned
// collector/exporter version — the source of truth for the ClickHouse
// schema — so image ↔ schema stay traceable.
//
// Runs only when this CI job is a push to main or a tag push. On PR
// builds the function is a no-op. When it should publish but the
// registry credentials are missing, it fails loudly rather than
// silently skipping.
func publishGatewayImage(ctx context.Context, client *dagger.Client, ctr *dagger.Container) error {
	ref := os.Getenv("GITHUB_REF")
	event := os.Getenv("GITHUB_EVENT_NAME")
	isMainPush := event == "push" && ref == "refs/heads/main"
	isTagPush := strings.HasPrefix(ref, "refs/tags/")
	if !isMainPush && !isTagPush {
		fmt.Fprintf(os.Stderr, "gateway: publish skipped (event=%q ref=%q)\n", event, ref)
		return nil
	}

	user, token := os.Getenv("GHCR_USER"), os.Getenv("GHCR_TOKEN")
	if user == "" || token == "" {
		// Fail loud: the CI job is meant to publish but has no creds.
		return fmt.Errorf("gateway: GHCR_USER and GHCR_TOKEN must be set on main/tag pushes")
	}

	authed := ctr.WithRegistryAuth("ghcr.io", user, client.SetSecret("ghcr-token", token))

	// The pinned collector/exporter version is the schema source of truth
	// (see collector/builder-config.yaml). Making it the primary tag keeps
	// image ↔ schema traceable. An immutable per-commit tag lets gitops
	// deploys pin to a specific build without racing a later main merge.
	tags := []string{otelCollectorVersion}
	if sha := os.Getenv("GITHUB_SHA"); sha != "" {
		tags = append(tags, otelCollectorVersion+"-"+shortSHA(sha))
	}
	if isTagPush {
		tags = append(tags, strings.TrimPrefix(ref, "refs/tags/"))
	}

	for _, tag := range tags {
		full := gatewayImageRepo + ":" + tag
		fmt.Fprintf(os.Stderr, "gateway: publishing %s\n", full)
		if _, err := authed.Publish(ctx, full); err != nil {
			return fmt.Errorf("gateway: publish %s: %w", full, err)
		}
	}
	return nil
}

func shortSHA(sha string) string {
	if len(sha) < 7 {
		return sha
	}
	return sha[:7]
}
