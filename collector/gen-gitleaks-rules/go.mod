// Standalone module so the generator's runtime deps (none, currently) do not
// bleed into the ci/ module. Run by hand — see ../README in the parent
// directory or the package doc in main.go.
module github.com/guettli/otelhouse/collector/gen-gitleaks-rules

go 1.26.3
