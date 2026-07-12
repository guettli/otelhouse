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

// createDefaultConfig returns the defaults an operator's config is merged
// into. Neither identity source is enabled by default — no key material, no
// serviceaccount.enabled — so a config that names the extension without
// configuring an identity source fails Validate() rather than starting a
// gateway that trusts nobody (or, worse, everybody).
func createDefaultConfig() component.Config {
	return &Config{
		Algorithm:   "EdDSA",
		TenantClaim: defaultTenantClaim,
		ServiceAccount: &ServiceAccountConfig{
			Enabled:                false,
			Audience:               defaultSAAudience,
			JWKSURL:                defaultSAJWKSURL,
			CAFile:                 defaultSACAFile,
			TokenFile:              defaultSATokenFile,
			Algorithms:             defaultSAAlgorithms(),
			JWKSMinRefreshInterval: defaultSAMinRefresh,
			// One namespace per tenant is the tenancy model; an explicit
			// tenant_map entry overrides it per ServiceAccount.
			NamespaceAsTenant: true,
		},
	}
}

func createExtension(_ context.Context, set extension.Settings, cfg component.Config) (extension.Extension, error) {
	return newTenantAuth(cfg.(*Config), set.MeterProvider)
}
