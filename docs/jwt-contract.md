# Token contract for the multi-tenant OTLP gateway

This document is the wire contract between a producer and the `tenantauth`
extension in the otelhouse gateway. Anything not written down here is not
guaranteed to work — treat it as the source of truth when editing either
side.

The gateway accepts **two identity sources**:

| Source | Who it is for | Token minted by | Verified against | Tenant comes from |
| ------ | ------------- | --------------- | ---------------- | ----------------- |
| **Kubernetes ServiceAccount token** (default) | in-cluster producers | the kubelet, rotated automatically | the cluster JWKS (`/openid/v1/jwks`) | the ServiceAccount identity in the signed claims |
| **Static-PEM minted JWT** (fallback) | anything that is not an in-cluster pod | an operator's mint job, using a private key held in gitops | a static public key in the gateway config | the `tenant` claim |

Either or both may be enabled. When both are on, a request is tried against
the ServiceAccount verifier first and falls back to the static-PEM verifier.

Related trackers: [#53](https://github.com/guettli/otelhouse/issues/53),
[#73](https://github.com/guettli/otelhouse/issues/73), and gitops epic
[guettli/gitops#73](https://github.com/guettli/gitops/issues/73).

## Why the tenant MUST come from a verified token

The tenant is not metadata. It is the **authorization boundary**.

Every ingested record is stamped with `ResourceAttributes['tenant']`, and the
ClickHouse row policies bound to each per-tenant `<tenant>_ro` user filter on
exactly that value. So the tenant label decides *whose data a row is*:

- If a producer could set the tenant itself — as an OTLP **resource
  attribute** on the payload, as a header, or through any other
  unauthenticated channel — it could write rows that appear inside **any
  other tenant's** Grafana. Read isolation for every other tenant would be
  gone, and nothing would look wrong, because from the database's point of
  view those rows would simply *be* that tenant's rows.
- The same is true of a *shared* bearer secret: whoever holds it **is** that
  tenant. That is not hypothetical — CI holding agentloop's minted JWT
  recorded `guettli/sharedinbox` spans as `tenant: agentloop`.

Therefore:

1. The tenant is only ever derived from a claim in a token the gateway
   **cryptographically verified itself**.
2. Any client-supplied `tenant` resource attribute on the payload is
   **overwritten**, never honoured (`tenanttagger`).
3. A batch whose tenant cannot be derived from a verified claim is
   **dropped** — never written unlabelled, never defaulted to a fallback
   tenant. Fail closed.

An unmapped-but-perfectly-signed identity is rejected for the same reason: a
default tenant would be a place where somebody else's data lands.

---

## Source 1: Kubernetes ServiceAccount tokens (default, in-cluster)

A projected ServiceAccount token is an RS256 JWT signed by the API server,
whose signing keys are published at the cluster's JWKS endpoint. The kubelet
mints and rotates it. There is **no secret to distribute and no expiry
cliff** — this is the preferred source for every in-cluster producer.

### Producer setup

Project a token with the gateway's audience, and read it from the file the
kubelet keeps fresh:

```yaml
volumes:
  - name: otlp-token
    projected:
      sources:
        - serviceAccountToken:
            audience: otelhouse-gateway
            expirationSeconds: 3600
            path: token
```

```yaml
containers:
  - name: app
    volumeMounts:
      - name: otlp-token
        mountPath: /var/run/secrets/otelhouse
        readOnly: true
    env:
      - name: OTEL_EXPORTER_OTLP_ENDPOINT
        value: http://otelhouse-gateway.otelhouse.svc:4317
```

and build `OTEL_EXPORTER_OTLP_HEADERS` from the projected file:

```sh
export OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer $(cat /var/run/secrets/otelhouse/token)"
```

Re-read the file periodically (or restart on rotation): the kubelet refreshes
the token well before `exp`, and the gateway rejects an expired one.

> The `audience:` line is **not optional**. A token projected with the
> default audience (the API server) is rejected by the gateway — see below.

### Required claims

The kubelet produces all of these; a producer never writes them by hand.
Missing or mismatching any of them → 401.

| Claim | Type | Meaning |
| ----- | ---- | ------- |
| `iss` | string | The cluster's OIDC issuer. Must equal the gateway's `serviceaccount.issuer` (typically `https://kubernetes.default.svc.cluster.local`). |
| `aud` | string[] | Must contain the gateway's `serviceaccount.audience` — `otelhouse-gateway`. |
| `exp` | number | Expiry. Required and enforced. |
| `kubernetes.io.namespace` | string | The producer's namespace — the default tenant source. |
| `kubernetes.io.serviceaccount.name` | string | The producer's ServiceAccount — the key into the SA→tenant map. |
| `sub` | string | `system:serviceaccount:<namespace>:<name>`. Read as a fallback when the nested claims are absent. (Legacy Secret-based tokens carry the flat `kubernetes.io/serviceaccount/namespace` claim instead; that is read too.) |

### Audience: why it is enforced

Every pod in the cluster has a default ServiceAccount token mounted — minted
for the **API server's** audience. If the gateway did not validate `aud`,
that token would be replayable at the gateway, and *any* pod in the cluster
could write telemetry as its namespace's tenant without ever opting in.
Requiring `aud: otelhouse-gateway` means a producer must have **deliberately
projected a token for the gateway** — a Pod-spec change an operator can see
and review.

### Deriving the tenant

From the **verified** claims only, in this order:

1. **Explicit map.** If `serviceaccount.tenant_map` has an entry for
   `<namespace>/<serviceaccount>`, that tenant wins. This is for producers
   whose namespace is not their tenant — e.g. GitHub Actions runners:

   ```yaml
   tenant_map:
     arc-runners/gha-runner: ci
   ```

2. **Namespace as tenant.** Otherwise, if `namespace_as_tenant` is on (the
   default) and the optional `namespaces` allowlist permits it, the tenant is
   the `kubernetes.io/serviceaccount/namespace` claim. This is the 1:1
   namespace-per-tenant model.

3. **Otherwise: rejected** — 401, counted as
   `otelhouse_gateway_auth_rejections_total{reason="unmapped_serviceaccount"}`.
   No default, no fallback tenant.

### Key handling (JWKS)

- The gateway fetches the cluster JWKS from `serviceaccount.jwks_url`
  (default `https://kubernetes.default.svc/openid/v1/jwks`) over TLS
  validated against `serviceaccount.ca_file` (default
  `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt`), presenting its own
  ServiceAccount token from `serviceaccount.token_file` (default
  `/var/run/secrets/kubernetes.io/serviceaccount/token`, re-read per fetch so
  kubelet rotation is picked up). Set `token_file: "-"` to send none.
- Keys are cached by `kid`. A token with an unknown `kid` triggers **one**
  JWKS refresh (rate-limited by `jwks_min_refresh_interval`, default `30s`,
  so bogus kids cannot be turned into a flood of API-server requests). If the
  `kid` is still unknown afterwards, the token is **rejected**
  (`reason="unknown_kid"`). Cluster key rotation needs no gateway restart.
- If the JWKS cannot be fetched, no ServiceAccount token verifies:
  `reason="jwks_unavailable"`. Fail closed — an unreachable API server never
  turns into an accepted token.

### Gateway config

```yaml
extensions:
  tenantauth:
    serviceaccount:
      enabled: true
      issuer: https://kubernetes.default.svc.cluster.local
      audience: otelhouse-gateway          # what producers must project
      jwks_url: https://kubernetes.default.svc/openid/v1/jwks
      ca_file: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
      token_file: /var/run/secrets/kubernetes.io/serviceaccount/token
      algorithms: [RS256, ES256, EdDSA]    # never HS*, never none
      jwks_min_refresh_interval: 30s
      namespace_as_tenant: true            # tenant = namespace claim
      namespaces: []                       # optional allowlist; empty = any
      tenant_map:                          # explicit identity → tenant
        arc-runners/gha-runner: ci
```

`serviceaccount.issuer` is required: the gateway will not guess which cluster
it trusts.

---

## Source 2: Static-PEM minted JWT (anything that is not an in-cluster pod)

Unchanged from the original contract, and still fully supported. The gateway
holds only the **public** key; tokens are minted elsewhere (per-tenant secret
in gitops). One public key verifies every tenant's token, so adding a tenant
is a pure mint-side action.

### Signing algorithm

Pinned to ONE asymmetric algorithm at deploy time via `algorithm:`:

| Value   | Notes                                       |
| ------- | ------------------------------------------- |
| `EdDSA` | Preferred (Ed25519). Small keys, fast.      |
| `ES256` | ECDSA P-256. Use if operator standard.      |
| `RS256` | RSA 2048+. Widest tooling support.          |

The mint job MUST set the JWT `alg` header to exactly the configured value.
A mismatch → 401.

### Required claims

| Claim    | Type     | Meaning                                                   |
| -------- | -------- | --------------------------------------------------------- |
| `tenant` | string   | Non-empty tenant id. Bound to this token by the signature. |
| `iss`    | string   | Issuer id. Must equal the gateway's configured `issuer`.  |
| `aud`    | string   | Audience id. Must equal the gateway's configured `audience`. |
| `iat`    | number   | Issued-at (unix seconds). Standard use.                   |
| `exp`    | number   | Expiration (unix seconds). Must be in the future.         |

Example payload (decoded):

```json
{
  "iss": "agentloop-operator",
  "aud": "otelhouse-gateway",
  "tenant": "agentloop-42",
  "iat": 1751500000,
  "exp": 1754092000
}
```

The claim the tenant is read from is configurable (`tenant_claim`, default
`tenant`). It is also the attribute name under which the resolved tenant is
exposed on `client.Info.Auth` — for **both** sources — so it must match
`tenanttagger`'s `auth_attribute`.

### Gateway config

```yaml
extensions:
  tenantauth:
    issuer: agentloop-operator
    audience: otelhouse-gateway
    algorithm: EdDSA
    public_key_file: /etc/otelhouse/jwt-public.pem   # or: public_key_pem
    tenant_claim: tenant
```

Tokens are short-lived; re-minting before `exp` is an operator concern
(gitops#89). The gateway does no long-lived caching, so a rolled key or a
rotated token takes effect on the next request.

---

## Algorithms: what is never accepted

Both sources refuse `alg: none` and **every HMAC algorithm** (`HS256`, …).
The gateway holds only public keys, so an HS-family token could never
legitimately verify here — and accepting one opens the classic
HS↔RS/ES/EdDSA **algorithm-confusion** downgrade, where an attacker signs an
HS256 token using the *public* key (which, for the JWKS source, is
published!) as the HMAC secret. The allowlist is enforced at config-load time
— an operator cannot switch HMAC on — and again at key lookup.

## Transport

Sent as an HTTP `Authorization: Bearer <token>` header on every OTLP request
(gRPC and HTTP). The gateway's OTLP receiver forwards the header to the
`tenantauth` extension via the standard `include_metadata: true` mechanism.

## Verification steps (all must pass, else 401)

For a **ServiceAccount token**:

1. Header `alg` is in the configured allowlist — no `alg:none`, no HMAC.
2. The `kid` resolves to a key in the cluster JWKS (refreshing once if unknown).
3. The signature verifies against that key, and the key's type matches `alg`.
4. `exp` is present and in the future.
5. `iss` equals `serviceaccount.issuer`.
6. `aud` contains `serviceaccount.audience` (`otelhouse-gateway`).
7. The claims carry a ServiceAccount identity, and that identity maps to a
   tenant via `tenant_map` or `namespace_as_tenant`.

For a **static-PEM JWT**:

1. Header `alg` equals the configured algorithm — no `alg:none`, no HMAC.
2. The signature verifies against the configured public key.
3. `exp` is present and in the future.
4. `iss` equals the configured issuer.
5. `aud` equals the configured audience.
6. The `tenant` claim is present, is a string, and is non-empty after
   trimming whitespace.

On success, the resolved tenant is attached to the request's `client.Info`
auth context under the attribute name `tenant`. The `tenanttagger` processor
reads exactly that — and nothing else.

Every rejection increments
`otelhouse_gateway_auth_rejections_total{reason=...}`; the reason labels are
documented in [metrics.md](metrics.md). No request is dropped without being
counted.

## Fail-closed downstream

Even if a token verifies but somehow arrives at the processor with an empty
tenant (impossible after the checks above), the `tenanttagger` processor
drops the batch. No row is ever emitted with an empty or client-supplied
tenant. This is enforced in code, tested in
`processor/tenanttagger/processor_test.go` (`TestTraces_SpoofOverridden`,
`TestTraces_FailClosed_NoAuth`), and asserted end-to-end in the Dagger test
that pushes unauthenticated data and asserts the database stays empty.
