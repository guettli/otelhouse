// Package tenantratelimit is an OpenTelemetry Collector processor that
// enforces a per-tenant ingest limit on the multi-tenant OTLP gateway. It
// reads the authenticated tenant from client.Info (populated by the
// tenantauth extension), consults a per-tenant token bucket, and rejects
// batches whose tenant is over its rate. Rejections surface as gRPC
// RESOURCE_EXHAUSTED / HTTP 429 back to the client and increment
// otelhouse_gateway_ratelimit_dropped{tenant} — no silent drop.
//
// The processor is keyed off the signed claim, not the payload, so a
// misbehaving tenant cannot forge a different identity to escape its
// bucket. Limits are in-memory and per-replica; with N gateway replicas
// the effective per-tenant ceiling is up to N × rate. A shared limiter
// (e.g. Redis) is left as a follow-up.
package tenantratelimit

import (
	"errors"
	"fmt"
)

// Config configures the tenantratelimit processor.
type Config struct {
	// AuthAttribute is the client.Info.Auth attribute the processor reads
	// to identify the tenant. Must match the tenantauth extension's
	// tenant_claim. Defaults to "tenant".
	AuthAttribute string `mapstructure:"auth_attribute"`

	// Default applies to any tenant not named in Overrides. Must be set
	// to a positive rate so an unlisted tenant still has a bound.
	Default RateLimit `mapstructure:"default"`

	// Overrides maps a tenant name to a per-tenant limit. A tenant not
	// listed uses Default.
	Overrides map[string]RateLimit `mapstructure:"overrides"`
}

// RateLimit is one token bucket: a sustained fill rate and a burst size.
// A batch consumes one token per record (span / log record / data
// point). A batch larger than Burst is rejected — the burst must be
// sized for the largest legitimate batch a producer will send.
type RateLimit struct {
	// Rate is the sustained fill rate in records/second. Must be > 0.
	Rate float64 `mapstructure:"rate"`

	// Burst is the maximum bucket depth. Must be > 0. When zero, it
	// defaults to Rate rounded up (a one-second burst allowance).
	Burst int `mapstructure:"burst"`
}

const defaultAuthAttribute = "tenant"

// defaultRate is the fill rate applied to a tenant with no override.
// Sized to be generous for well-behaved emitters (a Dagger pipeline emits
// well under a hundred spans/second on a normal run) while still bounding
// a hot loop.
const (
	defaultRate  = 1000.0
	defaultBurst = 2000
)

// Validate is invoked by the collector's confmap during config load.
func (c *Config) Validate() error {
	if c.AuthAttribute == "" {
		return errors.New("tenantratelimit: auth_attribute must not be empty")
	}
	if err := c.Default.validate("default"); err != nil {
		return err
	}
	for name, r := range c.Overrides {
		if name == "" {
			return errors.New("tenantratelimit: override tenant name must not be empty")
		}
		if err := r.validate(fmt.Sprintf("override %q", name)); err != nil {
			return err
		}
	}
	return nil
}

func (r RateLimit) validate(who string) error {
	if r.Rate <= 0 {
		return fmt.Errorf("tenantratelimit: %s rate must be > 0", who)
	}
	if r.Burst < 0 {
		return fmt.Errorf("tenantratelimit: %s burst must be >= 0", who)
	}
	return nil
}

// effectiveBurst returns the bucket depth actually used at runtime,
// applying the "zero means one-second's worth" default.
func (r RateLimit) effectiveBurst() int {
	if r.Burst > 0 {
		return r.Burst
	}
	b := int(r.Rate)
	if b < 1 {
		return 1
	}
	return b
}

// ErrMissingTenant is returned when a batch arrives without a resolved
// tenant in the auth context. The processor is fail-closed for the same
// reason tenanttagger is: no unauthenticated batch may reach the
// exporter. In a correctly wired pipeline tenanttagger drops those first,
// so this error is defence in depth.
var ErrMissingTenant = errors.New("tenantratelimit: no authenticated tenant in client context")
