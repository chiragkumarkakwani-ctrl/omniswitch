# Observability

OmniSwitch ships with SQLite-backed logs, traces, sessions, and feedback, plus
two standard egress paths for external observability platforms.

## OpenTelemetry (traces)

The chat pipeline (including `/v1/responses` and `/v1/messages`) creates a
server request span and client spans for router provider attempts. Request and
attempt span names are `chat <model>`; malformed requests without a known model
use `chat unknown`. Cached completions still have a request span but do not
make a provider attempt. Embeddings, local moderations, and rerank currently
record metrics and request metadata, not gateway OTel request spans.

Tracer names are `omniswitch/gateway` and `omniswitch/router`. The resource has
`service.namespace=omniswitch`, the configured `service.name` (default
`omniswitch-gateway`), and standard `telemetry.sdk.name`,
`telemetry.sdk.language`, and `telemetry.sdk.version` attributes. Additional
resource attributes can come from `OTEL_RESOURCE_ATTRIBUTES`.

Export via OTLP/HTTP:

```yaml
observability:
  otlp_endpoint: https://otel.yourcompany.com:4318
  service_name: omniswitch-prod
  headers:
    authorization: "Bearer ${OTEL_TOKEN}"
  insecure: false
  timeout: 10s
```

Environment equivalents: `OMNISWITCH_OTEL_ENDPOINT`, `OMNISWITCH_OTEL_HEADERS`,
`OMNISWITCH_OTEL_SERVICE_NAME`, `OMNISWITCH_OTEL_ENABLED`,
`OMNISWITCH_OTEL_INSECURE`, `OMNISWITCH_OTEL_TIMEOUT`.

### Span attribute reference

Attributes describe the normalized chat request and the final known response.
Optional request parameters are emitted only when supplied or set by route
transforms. Response/provider attributes are emitted when known, including
cached responses and completions denied by output guardrails. For streaming,
usage comes from the provider's reported usage, and finish reasons are ordered
by choice index. Prompts, completions, tool arguments, and credentials are not
included in span attributes.

| GenAI attribute | Meaning |
| --- | --- |
| `gen_ai.operation.name` | `chat` on requests and provider attempts |
| `gen_ai.provider.name` | Provider type when known, otherwise provider account name; `semantic-cache` for a cache hit |
| `gen_ai.system` | Same value as provider name; compatibility alias retained for v0.2.0 only |
| `gen_ai.request.model` | Model requested after route parameter transforms |
| `gen_ai.response.model` | Model returned by the completion, or resolved provider metadata |
| `gen_ai.request.max_tokens` | Effective limit; `max_completion_tokens` takes precedence over `max_tokens` |
| `gen_ai.request.temperature` | Effective request temperature |
| `gen_ai.request.top_p` | Effective request top-p |
| `gen_ai.response.finish_reasons` | Array of non-empty completion finish reasons |
| `gen_ai.response.id` | Completion ID |
| `gen_ai.usage.input_tokens` | Reported prompt/input tokens |
| `gen_ai.usage.output_tokens` | Reported completion/output tokens |

Provider attempts emit operation, provider (and the legacy system alias),
request model, and resolved response model; response ID, finish reasons,
request parameters, and token usage are on the gateway request span.

| Gateway attribute | Meaning |
| --- | --- |
| `http.request.method` | HTTP method |
| `http.route` | Original API path, including compatibility endpoints |
| `omniswitch.request_id` | Generated gateway request ID |
| `omniswitch.trace_id` | Gateway correlation ID (separate from the OTel trace ID) |
| `omniswitch.session_id` | Optional session correlation ID |
| `omniswitch.user_id` | Optional user correlation ID |
| `omniswitch.config` | Selected dynamic configuration, when requested |
| `omniswitch.decision` | `ALLOW` or `DENY` |
| `omniswitch.decision_reason` | Budget or guardrail denial reason |
| `omniswitch.guardrail.type` | Input guardrail that denied the request |
| `omniswitch.guardrail.fallback_target` | Input guardrail's selected fallback target |
| `omniswitch.cache_hit` | Whether a cached completion was used |
| `omniswitch.usage.cost_usd` | Estimated upstream cost; also on provider attempts |
| `omniswitch.attempt` | Zero-based router retry index (provider attempts only) |
| `omniswitch.fallback` | Whether the router selected a fallback (provider attempts only) |

These earlier LLM attributes remain for compatibility:

| Attribute | Where |
| --- | --- |
| `llm.request.model` | Requests and provider attempts |
| `llm.request.stream` | Requests and provider attempts |
| `llm.provider` | Provider attempts; account name |
| `llm.response.provider` | Requests and provider attempts; account name |
| `llm.response.model` | Requests and provider attempts |
| `llm.usage.prompt_tokens` | Requests |
| `llm.usage.completion_tokens` | Requests |
| `llm.usage.total_tokens` | Requests |

Failed provider calls and malformed requests may also have `exception` events
with standard `exception.type` and `exception.message` event attributes.

### v0.2.0 tracing migration

Update collector rules, dashboards, and saved queries from `sentinel.*` to
`omniswitch.*`. The gateway suffixes are unchanged; router
`sentinel.retry_attempt` becomes `omniswitch.attempt`, and `sentinel.fallback`
becomes `omniswitch.fallback`. `llm.usage.cost_usd` becomes
`omniswitch.usage.cost_usd`. The legacy tracer names `sentinel/gateway` and
`sentinel/router` and resource namespace `sentinel` are replaced, not aliased.
Use the new `gen_ai.*` attributes for portable queries. `gen_ai.system` remains
for one release to support older backends; migrate it to `gen_ai.provider.name`
before v0.3.0.

### Langfuse

Langfuse ingests OTLP traces natively. Point the OTLP endpoint at your
Langfuse region (`https://cloud.langfuse.com/...` or self-hosted) and set the
basic-auth header pair from your Langfuse keys:

```bash
export OMNISWITCH_OTEL_ENABLED=true
export OMNISWITCH_OTEL_ENDPOINT=https://cloud.langfuse.com/api/public/otel/v1/traces
# base64("pk-...:sk-...")
export OMNISWITCH_OTEL_HEADERS="authorization=Basic <b64>"
export OMNISWITCH_OTEL_SERVICE_NAME=omniswitch
```

Traces appear in Langfuse with provider latency, guardrail decisions, and cost
attributes attached to spans.

### Jaeger / Grafana Tempo

Any OTLP/HTTP collector works. For local development:

```bash
docker run --rm -p 4318:4318 -p 16686:16686 jaegertracing/all-in-one:latest
export OMNISWITCH_OTEL_ENABLED=true
export OMNISWITCH_OTEL_ENDPOINT=http://localhost:4318
export OMNISWITCH_OTEL_INSECURE=true
```

## Prometheus (metrics)

`GET /metrics` exposes Prometheus text format 0.0.4 from a concurrency-safe
in-process registry. No metrics are reconstructed from SQLite/Postgres request
logs: counters start at zero after a process restart and never roll off a
24-hour window. Use `rate()` or `increase()` and aggregate across instances for
traffic and spend over a time range. Retention, log deletion, and log-write
failures do not change these counters.

The endpoint is enabled by default (`OMNISWITCH_PROMETHEUS_ENABLED=true`, or
`observability.prometheus_enabled: true`). It sits behind the gateway's normal
authentication, authorization, and rate limiting. When gateway authentication
is enabled, the scrape token needs a viewer role or higher:

```yaml
scrape_configs:
  - job_name: omniswitch
    static_configs:
      - targets: ["omniswitch:8080"]
    authorization:
      type: Bearer
      credentials_file: /run/secrets/omniswitch_metrics_token
```

On a trusted monitoring network, `OMNISWITCH_METRICS_PUBLIC=true` (or
`observability.prometheus.public: true`) mounts **only `/metrics`** outside the
middleware chain. The default is `false`. Other API paths still require their
normal credentials. `OMNISWITCH_PROMETHEUS_ENABLED=false` disables the endpoint
even when public access is configured. The read-only config summary exposes
`metrics.public`, which is true only when public scraping is active.

### Metric reference

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `omniswitch_requests_total` | Counter | `endpoint`, `provider`, `model`, `status` | Completed API requests, counted once even through Responses/Messages translation |
| `omniswitch_request_duration_seconds` | Histogram | `endpoint`, `provider` | End-to-end duration, including streaming, middleware, guardrails, cache, provider calls, translation, and logging |
| `omniswitch_tokens_total` | Counter | `provider`, `model`, `direction` | Reported upstream input/output tokens, excluding cache hits |
| `omniswitch_cost_usd_total` | Counter | `provider`, `model` | Estimated upstream cost in USD, excluding cache hits |
| `omniswitch_guardrail_events_total` | Counter | `type`, `action` | Triggered input/output guardrail results, including non-blocking actions and local moderation checks |
| `omniswitch_rate_limit_rejections_total` | Counter | `policy` | Rejections recorded by the rate/spend limiter integration hook |
| `omniswitch_provider_circuit_state` | Gauge | `provider` | Scrape-time state: `0` closed, `1` half-open, `2` open |
| `omniswitch_build_info` | Gauge | `version`, `commit` | One sample of value `1` for this binary's build |
| `omniswitch_metrics_series_overflow_total` | Counter | `family` | Metric observations folded into the family's overflow series |

- `endpoint` comes from the original request path: `chat_completions`,
  `responses`, `messages`, `embeddings`, `moderations`, or `rerank`. Images,
  audio, MCP, A2A, admin APIs, health probes, and scrapes are not included.
- `status` is `success` (2xx/3xx), `denied` (402/403), `limited` (429), or
  `error` (other statuses). Auth and rate-limit middleware failures are counted
  by the binary's outer metrics middleware. Embedders using endpoint handlers
  alone count only requests that reach those handlers; use `MetricsMiddleware`
  outside your auth chain to include middleware failures.
- `provider` is the configured provider/account name. Cache hits use
  `semantic-cache`; local moderations use `omniswitch`. An empty provider/model
  means the request ended before those values were resolved. Model labels use
  the returned model when known, otherwise the resolved/requested model.
- `direction` is `input` or `output`. Usage and cost count each successful
  upstream completion, including ones later denied or retried by output
  guardrails. Successful background shadow completions also add usage and cost,
  but do not add logical request or duration samples. One logical request is
  still counted once. Cache hits do not increment upstream tokens or cost, and
  local moderation has no upstream usage. Streaming token totals come from
  reported provider usage, not text length estimates. Cost uses the gateway's
  current pricing estimator.
- `type` and `action` are the triggered guardrail result's type and configured
  action. Metrics do not depend on audit persistence. Multiple choices or
  moderation inputs can trigger multiple events.
- `policy` is the rejecting limiter policy name. Limiters must call
  `telemetry.DefaultMetrics.RecordRateLimitRejection(policy)` once per rejection.
- Circuits must register a concurrency-safe owned snapshot with
  `telemetry.DefaultMetrics.RegisterCircuitStateSource(func() map[string]int)`.
  The hook runs on every scrape, outside the registry lock, and omits invalid
  states. Removed providers do not leave stale gauge samples.

The duration histogram has cumulative inclusive bounds (seconds): `0.005`,
`0.01`, `0.025`, `0.05`, `0.1`, `0.25`, `0.5`, `1`, `2.5`, `5`, `10`, `30`,
`60`, `120`, and `+Inf`. Exposition includes
`omniswitch_request_duration_seconds_bucket` (additional `le` label), `_sum`,
and `_count`. Unobserved families expose HELP/TYPE lines but no label samples;
rate-limit and circuit samples require their integration hooks. Labels are
escaped and output is deterministically ordered. Request IDs and API keys are
never metric labels.

### Label cardinality limit

Each family has a hard cap of **2,000 distinct label sets**, including one
reserved overflow series: up to 1,999 normally labeled sets plus the overflow
set. The cap is fixed in `maxMetricSeriesPerFamily`, not a runtime setting.
Counter and histogram families retain their existing label sets until process
restart. After reaching the limit, every update to a new label set goes into a
single series where **every family label value is `other`**, for example:

```text
omniswitch_requests_total{endpoint="other",provider="other",model="other",status="other"} 42
```

No valid counter increment or histogram observation is dropped. Histogram
overflow preserves bucket, sum, and count updates; its generated `le` labels
keep their normal fixed bounds. The cap applies to endpoint/provider groups,
not individual bucket samples. `other` is reserved: a literal all-`other`
label set shares the overflow series.

`omniswitch_metrics_series_overflow_total{family}` increments for **each metric
update folded into overflow**, not once per distinct rejected label set.
For example, one usage report can produce separate input-token, output-token,
and cost overflow events. Its `family` values are fixed metric names, so this
counter cannot introduce caller-controlled cardinality. HELP text also
announces the cap. To detect loss of label-level detail:

```promql
sum by (family) (rate(omniswitch_metrics_series_overflow_total[5m])) > 0
```

Overflow preserves aggregate totals but loses provider/model/status/direction
breakdowns. Queries filtered to normal label values must include or separately
monitor the `other` series; overflow token samples combine input and output
usage under `direction="other"`.

Scrape-time circuit gauges use the same cap, selecting retained provider names
in sorted order on each snapshot. Their `provider="other"` sample reports the
highest state among folded providers (open takes precedence over half-open and
closed). Each folded snapshot entry increments the overflow counter on each
scrape. Removed providers and obsolete overflow states still disappear from
subsequent snapshots. Build information continues to replace its single label
set rather than accumulating history.

### v0.2.0 metrics migration

The old `/metrics` rolling 24-hour SQL sums were not true counters. The new
`omniswitch_requests_total`, `omniswitch_tokens_total`, and
`omniswitch_cost_usd_total` have the labels above and process-lifetime semantics.
Update dashboards/alerts to use rates or increases and appropriate label sums.
The former `omniswitch_requests_allowed_total`,
`omniswitch_requests_denied_total`, and `omniswitch_requests_errors_total` are
replaced by `omniswitch_requests_total` with the corresponding `success`,
`denied`, and `error` status labels. The old
`omniswitch_request_latency_ms_average` is replaced by the seconds histogram;
compute an average with `rate(..._sum[5m]) / rate(..._count[5m])` or a quantile
with `histogram_quantile()`. `omniswitch_cache_hits_total` is replaced by
request counts with `provider="semantic-cache"`. SQL-based `/api/metrics`
continues to serve its existing dashboard contract separately.

## Built-in trace waterfall

The dashboard renders per-request waterfalls from span data recorded in the
`request_logs.metadata` column (`_spans`): budget checks, input guardrails,
cache lookup, provider calls, and output guardrails with durations. No external
system required.
