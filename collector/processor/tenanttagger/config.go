// Package tenanttagger is an OpenTelemetry Collector processor that reads
// the authenticated tenant from client.Info (populated by the tenantauth
// extension) and stamps it onto every resource in the batch. It is
// fail-closed: a batch that arrives without a resolved tenant in the auth
// context is dropped instead of being written unlabelled or with a
// client-controlled tenant. This is what makes the ClickHouse row's
// ResourceAttributes['tenant'] the value from a signed claim, not from
// the payload.
package tenanttagger

import (
	"errors"
)

// Config configures the tenanttagger processor.
type Config struct {
	// AttributeKey is the resource attribute the processor writes the
	// tenant to. Defaults to "tenant" — matches the row-policy contract
	// in gitops#79.
	AttributeKey string `mapstructure:"attribute_key"`

	// AuthAttribute is the name of the attribute on client.Info.Auth the
	// processor reads to get the tenant. Must match the tenantauth
	// extension's tenant_claim. Defaults to "tenant".
	AuthAttribute string `mapstructure:"auth_attribute"`
}

const (
	defaultAttributeKey  = "tenant"
	defaultAuthAttribute = "tenant"
)

// Validate rejects an obviously misconfigured processor at load time.
func (c *Config) Validate() error {
	if c.AttributeKey == "" {
		return errors.New("tenanttagger: attribute_key must not be empty")
	}
	if c.AuthAttribute == "" {
		return errors.New("tenanttagger: auth_attribute must not be empty")
	}
	return nil
}

// ErrMissingTenant is returned when the batch arrives without a resolved
// tenant in the auth context. The processor returns this instead of
// silently emitting an unlabelled or client-labelled row — that's the
// "fail closed" invariant the acceptance tests assert.
var ErrMissingTenant = errors.New("tenanttagger: no authenticated tenant in client context")
