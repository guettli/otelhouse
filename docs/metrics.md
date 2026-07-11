# Gateway metrics

The multi-tenant OTLP gateway emits Prometheus metrics on the collector's
own telemetry endpoint so operators can watch per-tenant ingest, spot silent
auth failures (missed rotation), and see when the exporter is falling
behind.

The metrics below are the otelhouse-specific ones. The stock collector
telemetry (queue sizes, retry counts, exporter drops from
`clickhouseexporter`, receiver accepted/refused counters, etc.) is emitted
by the upstream components unchanged and surfaces on the same scrape
endpoint.

## Custom metrics

| Metric                                          | Type    | Labels                    | Emitted by          | Purpose |
| ----------------------------------------------- | ------- | ------------------------- | ------------------- | ------- |
| `otelhouse_gateway_auth_rejections_total`       | Counter | `reason`                  | `tenantauth`        | Count OTLP requests refused by the JWT auth layer, categorised by why. |
| `otelhouse_gateway_ingest_records_total`        | Counter | `tenant`, `signal`        | `tenanttagger`      | Count records that flowed through the processor, attributed to the signed tenant. |
| `otelhouse_gateway_ratelimit_dropped_total`     | Counter | `tenant`                  | `tenantratelimit`   | Count records rejected because the tenant was over its ingest rate limit. |

All three counters follow the OTel Prometheus convention: the instruments are
registered as `otelhouse_gateway_*` monotonic Sums and the SDK exposes them as
`<name>_total` on the scrape endpoint. No counter is emitted until its first
increment, so a fresh gateway with no traffic reports no series (the operator
sees "no data" rather than a misleading zero).

### `otelhouse_gateway_auth_rejections_total{reason=...}`

Incremented once per OTLP request the `tenantauth` extension rejects. The
`reason` label is one of:

| Reason           | Meaning                                                                 |
| ---------------- | ----------------------------------------------------------------------- |
| `expired`        | JWT `exp` is in the past. **The rotation loop missed the TTL.** Alert on any nonzero rate here.
| `bad_signature`  | Signature did not verify — includes `alg:none`, alg-confusion, wrong key, tampered token.
| `bad_issuer`     | JWT `iss` did not match the gateway's configured issuer.
| `bad_audience`   | JWT `aud` did not match the gateway's configured audience.
| `missing_tenant` | Token verified cryptographically but had no (or empty) tenant claim.
| `malformed`      | Anything else: no `Authorization` header, wrong scheme, empty bearer, unparseable JWT, missing `exp`.

A healthy gateway reports zero for every reason. In practice `expired` is
the load-bearing one — the operator's rotation job (gitops#89) re-mints
tokens ahead of `exp`, so any nonzero `expired` rate means a tenant is
about to go silent. Wire an ntfy alert on `rate(otelhouse_gateway_auth_rejections_total{reason="expired"}[5m]) > 0`
(same shape as gitops's `cron-diff-alert.sh`).

### `otelhouse_gateway_ingest_records_total{tenant,signal}`

Incremented by the `tenanttagger` processor after it has resolved the
tenant from `client.Info`. The count is the natural OTel "record" per
signal:

| Signal    | Increment source           |
| --------- | -------------------------- |
| `traces`  | `Traces.SpanCount()`       |
| `logs`    | `Logs.LogRecordCount()`    |
| `metrics` | `Metrics.DataPointCount()` |

Fail-closed inheritance: a batch that arrives without a resolved tenant
returns `ErrMissingTenant` and does **not** increment the counter. The
tenant label always comes from the signed claim — never from the payload,
never blank.

Empty batches (`SpanCount() == 0` etc.) are no-ops for the counter so an
idle tenant does not create a phantom zero series.

### `otelhouse_gateway_ratelimit_dropped_total{tenant}`

Incremented by the `tenantratelimit` processor by the number of records in a
batch it rejects because the tenant's token bucket could not cover it. The
batch is **not** silently discarded: the processor returns gRPC
`RESOURCE_EXHAUSTED`, which the OTLP receiver maps to `RESOURCE_EXHAUSTED` /
HTTP `429`, so the client sees the rejection and can retry.

Like the ingest counter, the `tenant` label always comes from the signed claim.
Buckets are in-memory per replica, so the counter is per replica too — sum
across replicas before comparing against a configured limit.

A nonzero rate means either a tenant is genuinely over-sending, or its limit is
set too low. It is a dashboard/warn signal, not a page.

### Stock `clickhouseexporter` telemetry

`clickhouseexporter` publishes the usual exporter helper counters on the
same MeterProvider — queue size, retry count, send-failed, dropped
records. They surface unchanged on the scrape endpoint (e.g.
`otelcol_exporter_queue_size`, `otelcol_exporter_send_failed_spans`).
Watch these to catch backpressure or persistent ClickHouse failures.

## Enabling the scrape

Metrics are emitted through the collector's own MeterProvider, which
means the operator turns them on with the standard
`service.telemetry.metrics` block in the gateway config. The gitops
deploy pins:

```yaml
service:
  telemetry:
    metrics:
      readers:
        - pull:
            exporter:
              prometheus:
                host: 0.0.0.0
                port: 8888
```

which exposes every metric described above at `http://<gateway>:8888/metrics`.

## Alerting

The operator-critical alerts are:

1. `otelhouse_gateway_auth_rejections_total{reason="expired"}` — nonzero
   rate → the rotation loop failed for at least one tenant. Loudly page.
2. `sum by (tenant) (rate(otelhouse_gateway_ingest_records_total[10m]))
   == 0` for a tenant that was previously nonzero → tenant went silent
   (network, agent crash, expired token). Warn, don't page.

Worth a dashboard panel and a warn (not a page):
`sum by (tenant) (rate(otelhouse_gateway_ratelimit_dropped_total[10m])) > 0`
— a tenant is losing data to its rate limit; either it is over-sending or its
limit needs raising.

Every other rejection reason usually means an attacker or a
misconfigured client, not an operator emergency — track them on a
dashboard rather than paging on them.
