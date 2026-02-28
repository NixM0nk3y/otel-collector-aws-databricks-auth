# AWS Federated Databricks OTel Authenticator

A custom OpenTelemetry Collector extension that provides AWS-federated authentication for forwarding telemetry to a [Databricks Unity Catalog OTLP endpoint](https://docs.databricks.com/en/observability/open-telemetry.html).

## Overview

Databricks exposes an OTLP-compatible ingestion endpoint that writes spans, logs, and metrics directly into Unity Catalog Delta tables. Authenticating to this endpoint requires a Databricks bearer token on every HTTP request. This project provides a custom OTel Collector authenticator extension (`databricksauth`) that acquires and injects that token automatically — using AWS IAM federation — so downstream pipelines never handle credentials directly.

## Auth Flow

In production the extension uses the ECS task role to prove its AWS identity to Databricks via the OAuth 2.0 Token Exchange grant (RFC 8693):

```
ECS task role
  └─► aws-sdk-go-v2 LoadDefaultConfig  (picks up ECS container credentials automatically)
        └─► sts.GetWebIdentityToken(audience="AwsTokenExchange", signingAlg="RS256")
              └─► AWS-signed JWT
                    └─► POST https://<workspace>/oidc/v1/token
                          grant_type=urn:ietf:params:oauth:grant-type:token-exchange
                          subject_token=<aws-jwt>
                          subject_token_type=urn:ietf:params:oauth:token-type:jwt
                          client_id=<sp-client-id>
                          scope=all-apis
                          └─► {access_token, expires_in}  (TTL ~1h)
                                └─► Authorization: Bearer <token>  (injected per-request)
```

Tokens are cached and refreshed transparently (5 min before expiry by default). Concurrent refresh requests are coalesced via singleflight — only one exchange hits the Databricks OIDC endpoint regardless of request concurrency.

## Architecture

```
                    ┌─────────────────────────────────────────────────┐
                    │           otelcol-databricks (custom binary)     │
                    │                                                   │
 OTLP (gRPC/HTTP)  │  ┌──────────┐   ┌───────────┐   ┌────────────┐  │   OTLP/HTTP
──────────────────►│  │ receiver │──►│ processor │──►│  exporter  │──┼──────────────► Databricks
   :4317 / :4318   │  │  (otlp)  │   │  (batch)  │   │ (otlphttp) │  │  /api/2.0/otel
                   │  └──────────┘   └───────────┘   └─────┬──────┘  │
                   │                                        │ auth    │
                   │                               ┌────────▼───────┐ │
                   │                               │ databricksauth │ │
                   │                               │  extension     │ │
                   │                               │                │ │
                   │                               │ RoundTripper   │ │
                   │                               │ injects:       │ │
                   │                               │ Authorization: │ │
                   │                               │ Bearer <token> │ │
                   │                               └────────────────┘ │
                   └─────────────────────────────────────────────────-┘
```

### Extension: `databricksauth`

Located at `extension/databricksauthextension/`, this is a standalone Go module implementing the OTel Collector `extensionauth.HTTPClient` interface. The `otlphttp` exporter calls `RoundTripper()`, which wraps the base transport to inject `Authorization: Bearer <token>` on every outbound request.

The extension supports two mutually exclusive modes selected implicitly by config:

| Config field set                 | Mode                                                                          |
| -------------------------------- | ----------------------------------------------------------------------------- |
| `token`                          | **Static** — token injected directly; no AWS calls. Use for local dev.        |
| `sp_client_id` + `workspace_url` | **Federation** — AWS→Databricks token exchange on first request, then cached. |

```
extension/databricksauthextension/
├── go.mod            # standalone Go module
├── config.go         # Config struct + Validate()
├── factory.go        # NewFactory(), component type "databricksauth"
├── extension.go      # Start(), RoundTripper, bearerRoundTripper
└── token.go          # AWSTokenProvider interface, STSTokenProvider, tokenCache
```

## Project Structure

```
build/
  builder-config.yaml       # ocb component manifest; includes local extension via path:
dist/                       # built collector binary (git-ignored)
extension/
  databricksauthextension/  # custom authenticator extension
    go.mod
    config.go
    factory.go
    extension.go
    token.go
    config_test.go
    token_test.go
    extension_test.go
test/
  config.yaml               # local dev config (debug exporter only, no auth)
  databricks-config.yaml    # Databricks config (uses databricksauth extension)
Makefile
.env.example
```

## Tooling

| Tool                                                                                                         | Version  | Purpose                                                  |
| ------------------------------------------------------------------------------------------------------------ | -------- | -------------------------------------------------------- |
| [ocb](https://github.com/open-telemetry/opentelemetry-collector/tree/main/cmd/builder)                       | v0.146.x | Builds a custom OTel Collector from a component manifest |
| [telemetrygen](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/cmd/telemetrygen) | v0.146.x | Generates synthetic OTLP traffic for local testing       |

## Commands

### Build the collector

Compiles the custom collector binary using ocb. The extension is compiled in via the `path:` directive in `build/builder-config.yaml` — no registry publishing required.

```bash
make collector/build
```

Output binary: `./dist/otelcol-databricks`

### Run locally (debug only)

Starts the collector with `test/config.yaml`. Exposes gRPC on `:4317` and HTTP on `:4318`. All telemetry is logged to stdout via the `debug` exporter. No credentials required.

```bash
make collector/run
```

### Run with Databricks forwarding (federation mode)

Starts the collector with `test/databricks-config.yaml`. Copy `.env.example` to `.env` and fill in the values:

```bash
cp .env.example .env
# fill in DATABRICKS_HOST, DATABRICKS_SP_CLIENT_ID, DATABRICKS_UC_* values
make collector/run/databricks
```

The `databricksauth` extension performs the AWS→Databricks token exchange automatically using the ambient ECS task role (or whichever AWS credential source is configured). No token appears in any config file or environment variable — `DATABRICKS_SP_CLIENT_ID` is just the OAuth app's client ID, not a secret.

### Run with a static token (local dev / fallback)

For environments without ECS/AWS credentials, configure a static token directly:

```yaml
# test/databricks-config.yaml (local override)
extensions:
  databricksauth:
    token: "${env:DATABRICKS_TOKEN}"
```

`token` and `sp_client_id` are mutually exclusive; `Validate()` returns an error if both or neither are set.

### Send test traffic

With a collector running locally, use `telemetrygen` to push synthetic data:

```bash
make test/traffic/traces    # 5s of trace spans → localhost:4317
make test/traffic/metrics   # 5s of metrics     → localhost:4317
make test/traffic/logs      # 5s of log records  → localhost:4317
```

### Run extension tests

```bash
cd extension/databricksauthextension
go test ./...
```

Tests cover config validation, token cache behaviour (cache hit, expiry, concurrent singleflight, expired-token safety), error propagation, and the full RoundTripper pipeline — all without real AWS or Databricks credentials.

## Configuration Reference

```yaml
extensions:
  databricksauth:
    # --- Federation mode (production) ---
    workspace_url: "https://<workspace>.azuredatabricks.net"  # required with sp_client_id
    sp_client_id: "<databricks-sp-oauth-client-id>"           # Databricks SP OAuth app
    expiry_buffer: 5m                                         # refresh this long before expiry (default: 5m)

    # --- Static mode (local dev) ---
    # token: "<databricks-pat-or-sp-token>"                   # mutually exclusive with sp_client_id
```

## Databricks Setup

### 1. Create Unity Catalog tables

Run the following SQL in Databricks to create the three Delta tables. Replace `<catalog>`, `<schema>`, and `<table_prefix>` with your own values — the same prefix must be used for all three tables.

<details>
<summary>Spans</summary>

```sql
CREATE TABLE <catalog>.<schema>.<table_prefix>_otel_spans (
  trace_id STRING,
  span_id STRING,
  trace_state STRING,
  parent_span_id STRING,
  flags INT,
  name STRING,
  kind STRING,
  start_time_unix_nano LONG,
  end_time_unix_nano LONG,
  attributes MAP<STRING, STRING>,
  dropped_attributes_count INT,
  events ARRAY<STRUCT<
    time_unix_nano: LONG,
    name: STRING,
    attributes: MAP<STRING, STRING>,
    dropped_attributes_count: INT
  >>,
  dropped_events_count INT,
  links ARRAY<STRUCT<
    trace_id: STRING,
    span_id: STRING,
    trace_state: STRING,
    attributes: MAP<STRING, STRING>,
    dropped_attributes_count: INT,
    flags: INT
  >>,
  dropped_links_count INT,
  status STRUCT<
    message: STRING,
    code: STRING
  >,
  resource STRUCT<
    attributes: MAP<STRING, STRING>,
    dropped_attributes_count: INT
  >,
  resource_schema_url STRING,
  instrumentation_scope STRUCT<
    name: STRING,
    version: STRING,
    attributes: MAP<STRING, STRING>,
    dropped_attributes_count: INT
  >,
  span_schema_url STRING
) USING DELTA
TBLPROPERTIES ('otel.schemaVersion' = 'v1')
```

</details>

<details>
<summary>Logs</summary>

```sql
CREATE TABLE <catalog>.<schema>.<table_prefix>_otel_logs (
  event_name STRING,
  trace_id STRING,
  span_id STRING,
  time_unix_nano LONG,
  observed_time_unix_nano LONG,
  severity_number STRING,
  severity_text STRING,
  body STRING,
  attributes MAP<STRING, STRING>,
  dropped_attributes_count INT,
  flags INT,
  resource STRUCT<
    attributes: MAP<STRING, STRING>,
    dropped_attributes_count: INT
  >,
  resource_schema_url STRING,
  instrumentation_scope STRUCT<
    name: STRING,
    version: STRING,
    attributes: MAP<STRING, STRING>,
    dropped_attributes_count: INT
  >,
  log_schema_url STRING
) USING DELTA
TBLPROPERTIES ('otel.schemaVersion' = 'v1')
```

</details>

<details>
<summary>Metrics</summary>

```sql
CREATE TABLE <catalog>.<schema>.<table_prefix>_otel_metrics (
  name STRING,
  description STRING,
  unit STRING,
  metric_type STRING,
  gauge STRUCT<
    start_time_unix_nano: LONG,
    time_unix_nano: LONG,
    value: DOUBLE,
    exemplars: ARRAY<STRUCT<
      time_unix_nano: LONG,
      value: DOUBLE,
      span_id: STRING,
      trace_id: STRING,
      filtered_attributes: MAP<STRING, STRING>
    >>,
    attributes: MAP<STRING, STRING>,
    flags: INT
  >,
  sum STRUCT<
    start_time_unix_nano: LONG,
    time_unix_nano: LONG,
    value: DOUBLE,
    exemplars: ARRAY<STRUCT<
      time_unix_nano: LONG,
      value: DOUBLE,
      span_id: STRING,
      trace_id: STRING,
      filtered_attributes: MAP<STRING, STRING>
    >>,
    attributes: MAP<STRING, STRING>,
    flags: INT,
    aggregation_temporality: STRING,
    is_monotonic: BOOLEAN
  >,
  histogram STRUCT<
    start_time_unix_nano: LONG,
    time_unix_nano: LONG,
    count: LONG,
    sum: DOUBLE,
    bucket_counts: ARRAY<LONG>,
    explicit_bounds: ARRAY<DOUBLE>,
    exemplars: ARRAY<STRUCT<
      time_unix_nano: LONG,
      value: DOUBLE,
      span_id: STRING,
      trace_id: STRING,
      filtered_attributes: MAP<STRING, STRING>
    >>,
    attributes: MAP<STRING, STRING>,
    flags: INT,
    min: DOUBLE,
    max: DOUBLE,
    aggregation_temporality: STRING
  >,
  exponential_histogram STRUCT<
    attributes: MAP<STRING, STRING>,
    start_time_unix_nano: LONG,
    time_unix_nano: LONG,
    count: LONG,
    sum: DOUBLE,
    scale: INT,
    zero_count: LONG,
    positive_bucket: STRUCT<offset: INT, bucket_counts: ARRAY<LONG>>,
    negative_bucket: STRUCT<offset: INT, bucket_counts: ARRAY<LONG>>,
    flags: INT,
    exemplars: ARRAY<STRUCT<
      time_unix_nano: LONG,
      value: DOUBLE,
      span_id: STRING,
      trace_id: STRING,
      filtered_attributes: MAP<STRING, STRING>
    >>,
    min: DOUBLE,
    max: DOUBLE,
    zero_threshold: DOUBLE,
    aggregation_temporality: STRING
  >,
  summary STRUCT<
    start_time_unix_nano: LONG,
    time_unix_nano: LONG,
    count: LONG,
    sum: DOUBLE,
    quantile_values: ARRAY<STRUCT<quantile: DOUBLE, value: DOUBLE>>,
    attributes: MAP<STRING, STRING>,
    flags: INT
  >,
  metadata MAP<STRING, STRING>,
  resource STRUCT<
    attributes: MAP<STRING, STRING>,
    dropped_attributes_count: INT
  >,
  resource_schema_url STRING,
  instrumentation_scope STRUCT<
    name: STRING,
    version: STRING,
    attributes: MAP<STRING, STRING>,
    dropped_attributes_count: INT
  >,
  metric_schema_url STRING
) USING DELTA
TBLPROPERTIES ('otel.schemaVersion' = 'v1')
```

</details>

### 2. Grant permissions

Grant the following to the service principal used for token exchange (the one identified by `sp_client_id`):

```sql
GRANT USE_CATALOG ON CATALOG <catalog> TO `<service-principal>`;
GRANT USE_SCHEMA  ON SCHEMA  <catalog>.<schema> TO `<service-principal>`;

GRANT MODIFY, SELECT ON TABLE <catalog>.<schema>.<table_prefix>_otel_spans   TO `<service-principal>`;
GRANT MODIFY, SELECT ON TABLE <catalog>.<schema>.<table_prefix>_otel_logs    TO `<service-principal>`;
GRANT MODIFY, SELECT ON TABLE <catalog>.<schema>.<table_prefix>_otel_metrics TO `<service-principal>`;
```

## Databricks OTLP Endpoint

The collector forwards to these endpoints (one per signal type, appended automatically by the `otlphttp` exporter):

```text
https://<workspace>.cloud.databricks.com/api/2.0/otel/v1/traces
https://<workspace>.cloud.databricks.com/api/2.0/otel/v1/logs
https://<workspace>.cloud.databricks.com/api/2.0/otel/v1/metrics
```

Required headers per request:

| Header                       | Value                                                     |
| ---------------------------- | --------------------------------------------------------- |
| `Content-Type`               | `application/x-protobuf`                                  |
| `Authorization`              | `Bearer <token>` — injected by `databricksauth` extension |
| `X-Databricks-UC-Table-Name` | `<catalog>.<schema>.<prefix>_otel_<type>`                 |

## References

- [OTel Collector Builder (ocb)](https://github.com/open-telemetry/opentelemetry-collector/tree/main/cmd/builder)
- [Building a custom authenticator extension](https://opentelemetry.io/docs/collector/extend/custom-component/extension/authenticator/)
- [Databricks OTel ingestion docs](https://docs.databricks.com/en/observability/open-telemetry.html)
- [OAuth 2.0 Token Exchange (RFC 8693)](https://datatracker.ietf.org/doc/html/rfc8693)
- [bearertokenauthextension](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/extension/bearertokenauthextension) — reference implementation
