# JWT contract for the multi-tenant OTLP gateway

This document is the wire contract between the gitops mint job (which
issues per-instance JWTs) and the `tenantauth` extension in the otelhouse
gateway (which verifies them). Anything not written down here is not
guaranteed to work — treat it as the source of truth when editing either
side.

Related tracker: [#53](https://github.com/guettli/otelhouse/issues/53) and
gitops epic [guettli/gitops#73](https://github.com/guettli/gitops/issues/73).

## Signing algorithm

The gateway is pinned to ONE asymmetric signing algorithm at deploy time
via `algorithm:` in the extension config. Supported values:

| Value   | Notes                                       |
| ------- | ------------------------------------------- |
| `EdDSA` | Preferred (Ed25519). Small keys, fast.      |
| `ES256` | ECDSA P-256. Use if operator standard.      |
| `RS256` | RSA 2048+. Widest tooling support.          |

The extension refuses `alg:none` and refuses any HMAC algorithm (`HS256`
etc.) — the gateway holds only the **public** key, so an HS-family token
could never legitimately verify here, and allowing it opens the classic
HS↔RS/ES/EdDSA algorithm-confusion downgrade (attacker signs an HS256
token using the RSA public key as the HMAC secret).

The mint job MUST set the JWT `alg` header to exactly the value the
gateway is configured with. A mismatch → 401.

## Required claims

Every token MUST carry the following. Missing OR mismatching any of them
→ 401:

| Claim    | Type     | Meaning                                                   |
| -------- | -------- | --------------------------------------------------------- |
| `tenant` | string   | Non-empty tenant id. Bound to this token by the signature. |
| `iss`    | string   | Issuer id. Must equal the gateway's configured `issuer`.  |
| `aud`    | string   | Audience id. Must equal the gateway's configured `audience`. |
| `iat`    | number   | Issued-at (unix seconds). Standard use.                   |
| `exp`    | number   | Expiration (unix seconds). Must be in the future.         |

`tenant` is the only per-instance claim — every other required claim is a
fixed property of the gateway. Adding a tenant is therefore a mint-side
action: sign a new token with a new `tenant` value, no gateway change.

The `tenant` value ends up on every ingested record as
`ResourceAttributes['tenant']` and is the join key for the row policies
in gitops#79.

## Header

Only `alg` and `typ` (`typ: JWT`) are inspected. Other headers (`kid`,
`x5t`, ...) are accepted but ignored — key rotation is a config change
in this iteration, not a `kid`-driven lookup.

## Transport

Sent as an HTTP `Authorization: Bearer <token>` header on every OTLP
request (gRPC and HTTP). The gateway's OTLP receiver forwards the header
to the `tenantauth` extension via the standard `include_metadata: true`
mechanism.

## Verification steps (all must pass, else 401)

1. Header `alg` equals the configured algorithm — no `alg:none`, no HMAC.
2. Signature verifies against the configured public key.
3. `exp` is in the future.
4. `iss` equals the configured issuer.
5. `aud` equals the configured audience.
6. `tenant` claim is present, is a string, and is non-empty after trimming
   whitespace.

On success, the resolved tenant is attached to the request's
`client.Info` auth context under the attribute name `tenant`. The
`tenanttagger` processor reads exactly that.

## Fail-closed downstream

Even if a token verifies but somehow arrives at the processor with an
empty tenant (should be impossible after the checks above), the
`tenanttagger` processor drops the batch. No row is ever emitted with an
empty or client-supplied tenant. This is enforced in code, tested in
`processor/tenanttagger/processor_test.go`, and asserted end-to-end in
the Dagger test that pushes unauthenticated data and asserts the database
stays empty.

## Example token payload (decoded)

```json
{
  "iss": "otelhouse-mint",
  "aud": "otelhouse-gateway",
  "tenant": "agentloop-42",
  "iat": 1751500000,
  "exp": 1751586400
}
```

The signature is over the standard JWS Compact Serialization:
`base64url(header) + '.' + base64url(payload) + '.' + base64url(sig)`.

## Config on the mint side (gitops#79)

The mint job needs, in addition to the private key:

- `iss` — the same string the gateway is configured with.
- `aud` — the same string the gateway is configured with.
- `alg` — must be one of the three supported algorithms and match the
  gateway config.
- Per-instance: the `tenant` string.
- Reasonable `exp` (recommendation: 7 days, refresh from the mint job).

Renewing tokens is a pure minting concern; the gateway does no
long-lived caching, so a rolled key or a rotated token takes effect on
the next request.
