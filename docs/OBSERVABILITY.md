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

Enable `/metrics` on the gateway port:

```bash
export OMNISWITCH_PROMETHEUS_ENABLED=true
curl -s localhost:8080/metrics | head
```

Scrape config:

```yaml
scrape_configs:
  - job_name: omniswitch
    static_configs:
      - targets: ["omniswitch.internal:8080"]
```

## Built-in trace waterfall

The dashboard renders per-request waterfalls from span data recorded in the
`request_logs.metadata` column (`_spans`): budget checks, input guardrails,
cache lookup, provider calls, and output guardrails with durations. No external
system required.
