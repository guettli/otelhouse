# The Dagger pipeline in ci/ is the single source of truth for tests.
# `make test` runs locally exactly what CI runs, so a green local run implies a
# green CI run. Do not add a second test path (e.g. docker-compose) — see
# https://github.com/guettli/otelhouse/issues/33.
#
# The pipeline needs a reachable Dagger engine. Point the Dagger SDK at a
# remote engine by exporting _EXPERIMENTAL_DAGGER_RUNNER_HOST before running;
# it is inherited by `go run` below.

.PHONY: test ci
test ci:
	cd ci && go run .
