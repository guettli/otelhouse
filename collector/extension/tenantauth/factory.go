package tenantauth

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension"
)

// typeStr identifies the extension in the collector's config.
var typeStr = component.MustNewType("tenantauth")

// NewFactory returns an extension.Factory for use in a custom collector
// distribution built with ocb.
func NewFactory() extension.Factory {
	return extension.NewFactory(
		typeStr,
		createDefaultConfig,
		createExtension,
		component.StabilityLevelAlpha,
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		Algorithm:   "EdDSA",
		TenantClaim: defaultTenantClaim,
	}
}

func createExtension(_ context.Context, _ extension.Settings, cfg component.Config) (extension.Extension, error) {
	return newTenantAuth(cfg.(*Config))
}
