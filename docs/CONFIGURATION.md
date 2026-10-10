# Configuration

OmniSwitch can be configured with environment variables or a declarative config file.

## Gateway Config

Set `OMNISWITCH_CONFIG` to a YAML or JSON file:

```bash
OMNISWITCH_CONFIG=examples/gateway-config.yaml go run ./cmd/gateway
```

Environment variables are applied after the config file and override file values.

## Example

```yaml
apiVersion: omniswitch.dev/v1
kind: GatewayConfig

gateway:
  listen: ":8080"
  data_dir: ".omniswitch"
  auth: true
  cache_threshold: 0.95
  cache_ttl: 24h
  cache_scope: api_key
  log_payloads: false
  cors_origins: [https://app.example.com]
  circuit_breaker_failures: 5
  circuit_breaker_cooldown: 60s
  max_request_bytes: 10485760
  read_header_timeout: 5s
  read_timeout: 30s
  write_timeout: 0s
  idle_timeout: 60s

rate_limit:
  requests: 120
  window: 1m
  # Set to share quotas across gateway replicas.
  redis_url: redis://redis:6379/0
  prefix: omniswitch:ratelimit
  # false returns 503 if Redis is unavailable; true preserves availability.
  fail_open: false

authorization:
  rules:
    - name: member-model-restriction
      when: 'role == "member" && model == "production-model"'
      effect: deny
      message: This model requires an administrator key

identity:
  oidc:
    jwks_url: https://issuer.example.com/.well-known/jwks.json
    issuer: https://issuer.example.com/
    audience: omniswitch
    role_claim: roles
    workspace_claim: workspace_id
    organization_claim: organization_id
    cache_ttl: 5m

guardrails:
  actions:
    injection: deny
    pii: redact
  stream_buffer: true
  webhooks:
    - name: managed-moderation
      url: https://guardrails.example.com/v1/check
      stage: input
      action: deny
      headers:
        authorization: "Bearer ${GUARDRAIL_API_KEY}"
      timeout: 3s
      fail_open: false
  rules:
    - name: no-secret-marker
      stage: both
      pattern: '(?i)internal-secret'
      action: deny
      message: Sensitive content is not allowed

observability:
  otel_enabled: true
  otlp_endpoint: http://localhost:4318/v1/traces
  service_name: omniswitch-gateway
  insecure: true
  timeout: 10s
  prometheus_enabled: true

providers:
  - name: openai-prod
    type: openai
    api_key_env: OPENAI_API_KEY

  - name: ollama
    type: custom
    base_url: http://localhost:11434/v1
    models: [llama3.2]

mcp:
  enabled: true
  policy: policies/production-delete.yaml
  upstream: http://127.0.0.1:8090/mcp
  targets:
    - name: github
      upstream: http://127.0.0.1:8091/mcp
      policy: policies/production-delete.yaml
      headers:
        x-api-key: "${GITHUB_MCP_TOKEN}"
      # Forward an OIDC bearer token only; OmniSwitch API keys never leave the gateway.
      forward_bearer_token: false

    - name: filesystem
      transport: stdio
      command: npx
      args: ["-y", "@modelcontextprotocol/server-filesystem", "${WORKSPACE_ROOT}"]
      environment:
        LOG_LEVEL: warn

routes:
  canary-chat:
    fallbacks: ["@anthropic-prod"]
    max_retries: 2
    retry_backoff: 500ms
    retry_codes: [429, 500, 502, 503, 504]
    timeout: 30s
    default_params:
      temperature: 0.2
    override_params:
      max_tokens: 1000
    drop_params: [logprobs]
    variants:
      - provider: "@openai-prod"
        model: "@openai-prod/gpt-4o-mini"
        weight: 90
      - provider: anthropic
        model: claude-3-5-haiku-20241022
        weight: 10
        condition: 'model == "canary-chat"'
```

## Provider Accounts

Provider accounts create virtual providers without putting secrets in config.

```yaml
providers:
  - name: openai-prod
    type: openai
    api_key_env: OPENAI_API_KEY
```

This exposes models such as:

```text
@openai-prod/gpt-4o-mini
```

Supported provider types:

- `openai`
- `anthropic`
- `google`
- `groq`
- `azure` / `azure-openai` (deployment endpoint; `endpoint` and `api_version`)
- `bedrock` / `aws-bedrock` (region and AWS credential environment names)
- `cohere` (native rerank adapter, not chat)
- `custom`
- `inference` (OpenAI-compatible replicas; example below)

### OpenAI-compatible presets

Setting an environment variable enables these providers instantly - no config entry required. Each preset ships a default endpoint and a curated model list; a config `providers` entry with the same `type` can override `base_url` or `models`.

| Type | Environment variable | Default endpoint |
| --- | --- | --- |
| `mistral` | `MISTRAL_API_KEY` | `https://api.mistral.ai/v1` |
| `xai` | `XAI_API_KEY` | `https://api.x.ai/v1` |
| `deepseek` | `DEEPSEEK_API_KEY` | `https://api.deepseek.com/v1` |
| `together` | `TOGETHER_API_KEY` | `https://api.together.xyz/v1` |
| `fireworks` | `FIREWORKS_API_KEY` | `https://api.fireworks.ai/inference/v1` |
| `perplexity` | `PERPLEXITY_API_KEY` | `https://api.perplexity.ai` |
| `cerebras` | `CEREBRAS_API_KEY` | `https://api.cerebras.ai/v1` |
| `openrouter` | `OPENROUTER_API_KEY` | `https://openrouter.ai/api/v1` |

```yaml
providers:
  # Explicit account form (overrides preset defaults)
  - name: mistral-eu
    type: mistral
    api_key_env: MISTRAL_EU_KEY
    models: [mistral-large-latest]
```

`custom` connects any OpenAI-compatible endpoint. Use `base_url`, optional `models`, and optional `extra_headers`. Header values support environment expansion.

```yaml
providers:
  - name: azure-gpt4o
    type: custom
    base_url: https://YOUR_RESOURCE.openai.azure.com/openai/deployments/gpt-4o/chat/completions?api-version=2024-10-21
    api_key_env: AZURE_OPENAI_API_KEY
    extra_headers:
      api-key: "${AZURE_OPENAI_API_KEY}"
    models: [gpt-4o]
```

### Inference replicas (vLLM and Ollama)

`type: inference` registers a local provider without an upstream API key.
Use an OpenAI-compatible base URL ending in `/v1` for vLLM or Ollama. Each
replica can advertise its own `models`; omitting them lets it accept any model
when explicitly routed, but does not add entries to `/v1/models`.

```yaml
providers:
  - name: local-inference
    type: inference
    health_interval: 10s
    replicas:
      - url: http://vllm:8000/v1
        runtime: vllm
        models: [Qwen/Qwen2.5-7B-Instruct]
        priority: 0
      - url: http://ollama:11434/v1
        runtime: ollama
        models: [llama3.2]
        priority: 1
routes:
  local-chat:
    variants:
      - provider: local-inference
        model: Qwen/Qwen2.5-7B-Instruct
        weight: 1
```

Health probes run at startup and every `health_interval` (default `10s`,
positive durations required), with a 3-second timeout. vLLM probes `/health`;
Ollama probes `/api/tags`, relative to the base URL before `/v1`. A non-2xx
response or network error marks a replica unhealthy. Model-compatible replicas
are ranked by health, observed latency, queue length, and `priority` (lower
values preferred). Probes stop during shutdown. `InferenceBackend.ReplicaHealth()`
returns independent snapshots for the admin health API; `healthy` and
`last_check` are null until the first probe finishes.

## Model catalog

`/v1/models` and cost-based routing (the `cost-optimal` strategy) for OpenAI,
Anthropic, Google, and Groq are driven by a single consolidated catalog file,
embedded at build time via `go:embed`:
`internal/provider/catalog/models.json`.

Each entry records the model ID, `owned_by`, USD price per 1M input/output
tokens, a `source_url`, and the `verified_on` date it was last checked against
the provider's public pricing page. There is no live pricing fetch; updating
prices/models is a manual edit to this file (add a row, keep `verified_on`
current) followed by `go test ./internal/provider/...`. Bedrock and Cohere
also draw their default model lists from the catalog; Bedrock pricing then
matches by substring against the catalog's canonical Claude model IDs because
Bedrock model IDs vary by region/cross-region-inference-profile prefix.
Models not present in the catalog return zero estimated cost rather than a
guessed default. Mistral, DeepSeek, and xAI are reachable via `type: custom`
presets (see above) and have reference-only catalog rows under provider
`custom-preset-reference` for documentation purposes; they are not read by any
adapter.

## Observability

OmniSwitch can export gateway and provider spans to any OTLP-compatible backend.

```yaml
observability:
  otel_enabled: true
  otlp_endpoint: http://localhost:4318/v1/traces
  service_name: omniswitch-gateway
  headers:
    x-api-key: observability-secret
```

Setting `OMNISWITCH_OTEL_ENDPOINT` also enables tracing.

Set `prometheus_enabled: true` (the runtime default) to expose the lightweight
Prometheus endpoint at `GET /metrics`. When authentication is enabled, this
endpoint requires an authenticated viewer role or higher.

## Security and Runtime Limits

`gateway.cache_scope` controls cache sharing: `api_key` (default), `workspace`,
`organization`, or `global`. Organization scope is derived from the
authenticated key's persisted workspace mapping, not a caller-provided header.
Use `global` only for public, tenant-independent traffic. `log_payloads` defaults to `false`, so SQLite request logs retain
metadata and usage but not raw prompts or outputs.

`cors_origins` is an explicit browser allow-list. Leaving it empty disables
cross-origin browser access. `max_request_bytes`, `read_header_timeout`,
`read_timeout`, `write_timeout`, and `idle_timeout` map to request and server
limits. The write timeout defaults to `0s` so long-running SSE streams are not
cut off; set a positive value only when streaming is not required.

`gateway.storage` selects the store backend: `sqlite` (default) or `postgres`.
When set to `postgres`, `gateway.storage_url` (a `postgres://` or
`postgresql://` DSN) is required and config validation rejects the file
without it. The `OMNISWITCH_DATABASE_URL` environment variable always takes
precedence over `gateway.storage_url`, so an operator can override storage
without editing the config file.

`gateway.alerts` sends threshold-based notifications (for example, elevated
error rates) to a webhook and/or Slack incoming webhook URL:

```yaml
gateway:
  alerts:
    webhook_url: https://hooks.example.com/omniswitch
    slack_url: https://hooks.slack.com/services/...
    thresholds: [0.5, 0.9]
```

`webhook_url` and `slack_url` must be absolute `http://` or `https://` URLs
when set; `thresholds` are fractions in `(0, 1]`. Alerts reload alongside
guardrails when `OMNISWITCH_CONFIG` hot-reload is enabled.

Out-of-process HTTP authorization is not supported; the unused external
callout config type has been removed. Use `authorization.rules` (CEL) instead.
Unknown fields fail strict YAML/JSON parsing.

## Authorization and Rate Limits

`authorization.rules` are CEL policies evaluated after API-key authentication.
They receive `method`, `path`, `model`, `api_key_id`, `workspace_id`,
`organization_id`, `role`, `subject`, and `claims`. A matching `deny` always
wins. When any `allow` rule exists, a request must also match at least one
allow rule. This composes with the built-in role checks for the control plane.

`rate_limit` controls the default request quota. Every API key can still set
its own quota in the control plane. Without `redis_url`, the gateway uses a
local sliding window, suitable for one replica. With Redis, OmniSwitch uses an
atomic fixed-window counter shared by all replicas. The startup check fails
closed by default if Redis cannot be reached; `fail_open: true` lets traffic
continue if Redis later becomes unavailable.

## OIDC Workload Identity

`identity.oidc` accepts JWTs signed by the public keys at `jwks_url`, alongside
locally issued API keys. It supports RSA, ECDSA, and Ed25519 signing keys and
verifies expiration, issuer, and audience when those values are configured.
The JWT `sub` becomes the request subject and quota/cache key; role, workspace,
and organization claims are mapped with the configurable claim names. JWT
claims are also available to authorization CEL rules as `subject` and
`claims`. Supplying a JWKS URL enables gateway authentication automatically.

When `auth: true` is used on an empty database, set
`OMNISWITCH_BOOTSTRAP_API_KEY` for the first process start. OmniSwitch stores only
its SHA-256 hash and creates the `bootstrap-admin` owner key. Store the secret
in an environment-injection or secret-manager mechanism, then rotate it into a
normal key and remove the bootstrap variable. `OMNISWITCH_BOOTSTRAP_ROLE` may be
`owner` (default) or `admin`; `OMNISWITCH_BOOTSTRAP_WORKSPACE` is optional.

## Routing

Routes support direct providers, ordered `fallbacks`, weighted `variants`, a
CEL `condition` on a variant (`model` and `prompt` variables), retry count and
backoff, retryable HTTP status codes, per-attempt timeouts, a shadow provider,
and request shaping. `default_params` fills absent values,
`override_params` always wins, and `drop_params` is applied last. The current
request-shaping fields are `model`, `temperature`, `max_tokens`, `top_p`,
`stream`, and `stop`.

## Guardrails

Built-in input/output checks cover PII, prompt injection, SQL patterns, toxic
content, and code leakage. `guardrails.actions` can set their action; regex
rules can run at `input`, `output`, or `both` stages. All file-configurable
checks accept `deny`, `redact`, `warn`, and `log`. Regex rules also accept
`action: fallback` with a required `fallback: provider` or
`fallback: provider/model` target; the gateway reroutes instead of rejecting. A target with another action is
rejected rather than silently ignored. Every trigger produces a structured
guardrail audit event; `deny` and `redact` are enforced in the local request
path. The runtime engine also has retry/reroute actions, but the file
validator does not accept those actions or expose all their fields.
`stream_buffer: true` (default) buffers an SSE response before emitting it,
allowing output enforcement. Turning it off makes streamed output enforcement
a trusted-provider trade-off.

`guardrails.webhooks` adds an opt-in connector for a managed or in-house
moderation service. OmniSwitch sends `{stage, text, messages}` and expects
`{triggered, message, details}` or `{allowed}`. The configured `action`
controls enforcement; `fail_open: false` (the safer default) treats an
unavailable webhook as a guardrail trigger, while `true` ignores its failure.

## MCP Targets

The legacy `upstream` is a default HTTP MCP upstream. `targets` adds federated
HTTP MCP servers. `tools/list` combines allowed target tools under stable
`target__tool` names, and `tools/call` dispatches the prefixed tool to the
matching upstream with the target's policy and configured headers. HTTP
responses with `text/event-stream` are streamed through without gateway
buffering, so streamable HTTP and SSE-capable MCP servers work through the same
endpoint. Set `forward_bearer_token: true` only for targets that require
OAuth/OIDC delegation: it forwards the authenticated OIDC bearer token and
never forwards a local OmniSwitch API key. Stdio targets use a persistent
newline-delimited JSON-RPC child process and serialize calls per target.

### OpenAPI-to-MCP conversion

Set `openapi_spec` on a target to expose an OpenAPI 3.x document's operations
as synthetic MCP tools (local path or URL). Operation IDs become tool names;
path parameters are substituted, remaining arguments become the JSON body
(GET/DELETE: query string), and every call passes through the target policy,
audit trail, and headers before plain HTTP execution.

```yaml
mcp:
  targets:
    - name: pets-api
      upstream: https://petstore.example.com   # REST base URL
      openapi_spec: ./specs/petstore.yaml      # or https://...
      headers:
        authorization: "Bearer ${PETSTORE_TOKEN}"
```

Distributed trace context propagates into `params._meta.traceparent`
(SEP-414) on forwarded and OpenAPI-executed calls, so downstream MCP servers
can continue the gateway's OTel trace. A2A is not an MCP target type; it is
served separately through public Agent Card discovery and `/a2a` JSON-RPC.

## Policy replay

`POST /api/evals/policy` accepts 1–1,000 uploaded tool requests in a JSON body
of at most 1 MiB (an oversized body returns `413`). There are no dataset or
experiment CRUD handlers. Named policies are bare `.cel`, `.yaml`, or `.yml`
filenames inside `gateway.policy_dir`, default `policies`, relative to the
process working directory. `OMNISWITCH_POLICY_DIR` overrides this setting.
Absolute paths, nested paths, traversal, and symlinks escaping this directory
are rejected. Keep this dedicated directory writable only by the operator.

```yaml
gateway:
  policy_dir: /etc/omniswitch/policies
```

Use `policies: [{"name":"production-delete.yaml"}]` for a server-side file.
Use `policies: [{"name":"candidate.cel","body":"action.name == \"delete\""}]`
for inline CEL; a `.yaml`/`.yml` name selects an inline Policy document instead.
The legacy arbitrary-path `policy_paths` field is rejected. The combined
candidate/baseline policy bodies also have a 1 MiB limit, including file bodies,
and at most 1,000 policies may be supplied.

Optional `baseline_policies` uses the same name/body shape and confinement.
The server evaluates both sets against the same uploaded `requests`. The
response keeps `total`, `allowed`, `denied`, `errors`, and `results`, and adds
`baseline` plus `diff` (`changed`, `allowed_to_denied`, `denied_to_allowed`,
and per-index `changes` with before/after decisions). Diff compares allowed,
matched rule, reason, and evaluation errors, not generated IDs, timing, or
trace hashes. No inference calls or persistent datasets are involved.

## A2A capabilities

A2A is enabled at `/a2a` with public Agent Card discovery at
`/.well-known/agent-card.json`. Canonical methods are `SendMessage`,
`SendMessageStream`, `GetTask`, `CancelTask`, and `GetExtendedAgentCard`.
There is no separate A2A config switch. `/api/config` reports:

```json
{"enabled":true,"streaming":true,"streaming_mode":"sse_task_events","task_lifecycle":true,"task_store":"in_memory","cancel":"best_effort","push_notifications":false,"methods":["SendMessage","SendMessageStream","GetTask","CancelTask","GetExtendedAgentCard"]}
```

SSE sends task status/artifact events, not token deltas. Tasks are in memory
and are lost on restart (not shared across replicas). New tasks trigger pruning
of records older than one hour.
Cancellation is best effort: it updates state without aborting upstream
inference, and completion can supersede the canceled state. No push
notifications or downstream A2A proxy are implemented. The Agent Card uses
camelCase capability qualifiers for the same boundaries.

## Provider Vault

Set `OMNISWITCH_VAULT_KEY` before creating virtual keys so encrypted provider credentials remain decryptable after restart.

```bash
curl -X POST http://localhost:8080/api/virtual-keys \
  -H "Content-Type: application/json" \
  -d '{
    "name": "azure-prod",
    "provider_type": "custom",
    "provider_name": "azure-prod",
    "base_url": "https://YOUR_RESOURCE.openai.azure.com/openai/deployments/gpt-4o/chat/completions?api-version=2024-10-21",
    "provider_key": "real-provider-secret",
    "metadata": {"auth_header": "api-key", "models": "gpt-4o"}
  }'
```

## Environment Variables

- `OMNISWITCH_LISTEN`
- `OMNISWITCH_DATA`
- `OMNISWITCH_DATABASE_URL` - a `postgres://` or `postgresql://` DSN enables PostgreSQL for the persistent store: keys, logs, budgets, saved configuration, and response cache. The driver remains experimental; shared persistence does not make A2A tasks or runtime health shared or establish production active-active readiness. When unset (or not a postgres URL), SQLite in the data directory is used.
- `OMNISWITCH_AUTH`
- `OMNISWITCH_BOOTSTRAP_API_KEY`
- `OMNISWITCH_BOOTSTRAP_WORKSPACE`
- `OMNISWITCH_BOOTSTRAP_ROLE`
- `OMNISWITCH_CACHE_THRESHOLD`
- `OMNISWITCH_CACHE_TTL`
- `OMNISWITCH_CACHE_SCOPE`
- `OMNISWITCH_LOG_PAYLOADS`
- `OMNISWITCH_CORS_ORIGINS`
- `OMNISWITCH_GUARDRAIL_STREAM_BUFFER`
- `OMNISWITCH_MAX_REQUEST_BYTES`
- `OMNISWITCH_READ_HEADER_TIMEOUT`
- `OMNISWITCH_READ_TIMEOUT`
- `OMNISWITCH_WRITE_TIMEOUT`
- `OMNISWITCH_IDLE_TIMEOUT`
- `OMNISWITCH_CIRCUIT_BREAKER_FAILURES`
- `OMNISWITCH_CIRCUIT_BREAKER_COOLDOWN`
- `OMNISWITCH_RATE_LIMIT_REQUESTS`
- `OMNISWITCH_RATE_LIMIT_WINDOW`
- `OMNISWITCH_RATE_LIMIT_REDIS_URL`
- `OMNISWITCH_RATE_LIMIT_PREFIX`
- `OMNISWITCH_RATE_LIMIT_FAIL_OPEN`
- `OMNISWITCH_OIDC_JWKS_URL`
- `OMNISWITCH_OIDC_ISSUER`
- `OMNISWITCH_OIDC_AUDIENCE`
- `OMNISWITCH_OIDC_ROLE_CLAIM`
- `OMNISWITCH_OIDC_WORKSPACE_CLAIM`
- `OMNISWITCH_OIDC_ORGANIZATION_CLAIM`
- `OMNISWITCH_OIDC_CACHE_TTL`
- `OMNISWITCH_SHADOW_PROVIDER`
- `OMNISWITCH_AB_TEST`
- `OMNISWITCH_OTEL_ENABLED`
- `OMNISWITCH_OTEL_ENDPOINT`
- `OMNISWITCH_OTEL_SERVICE_NAME`
- `OMNISWITCH_OTEL_HEADERS`
- `OMNISWITCH_OTEL_INSECURE`
- `OMNISWITCH_OTEL_TIMEOUT`
- `OMNISWITCH_PROMETHEUS_ENABLED`
- `OMNISWITCH_VAULT_KEY`
- `OMNISWITCH_MCP_ENABLED` - defaults to `false`. When enabled without `OMNISWITCH_MCP_POLICY` (or `mcp.policy`) set, the gateway falls back to a conservative embedded default policy rather than failing to start.
- `OMNISWITCH_MCP_POLICY`
- `OMNISWITCH_MCP_UPSTREAM`
- `OMNISWITCH_SHUTDOWN_TIMEOUT` - defaults to `30s`. On SIGINT/SIGTERM, `omniswitch serve` stops accepting new connections, waits up to this long for in-flight requests (including SSE streams) to finish, then exits `0`.
- `OMNISWITCH_ALLOW_NO_PROVIDERS` - defaults to `false`. When `false`, `GET /readyz` reports not-ready if no provider is registered; set to `true` for local smoke-testing without any provider configured.
- `OPENAI_API_KEY`
- `ANTHROPIC_API_KEY`
- `GOOGLE_API_KEY`
- `GROQ_API_KEY`

## Command Line and Health Checks

See [`docs/CLI.md`](CLI.md) for `omniswitch serve`, `omniswitch validate-config`,
and `omniswitch version`. `GET /healthz` (liveness) and `GET /readyz`
(readiness: store reachability and provider registration) are always
unauthenticated, independent of `gateway.auth`, and safe to point Kubernetes
or load-balancer health checks at.

## Supported YAML keys

This inventory is checked against every YAML tag in the config structs by
`internal/gatewayconfig/docs_test.go`. The reverse check parses each documented
key with strict field checking. `[]` means a list item; `*` means a named map
entry. Header names, model aliases, parameter names, and environment names are
map values/keys, not additional schema fields. Durations use strings such as
`1s`, `5m`, and `24h`. Provider-specific fields apply to the adapter noted below.

| Key | Meaning / default |
| --- | --- |
| `apiVersion` | Schema version: `omniswitch.dev/v1`. |
| `kind` | Document kind: `GatewayConfig`. |
| `gateway` | Listener, storage, cache, and safety settings. |
| `gateway.listen` | HTTP listen address; default `:8080`. |
| `gateway.data_dir` | SQLite data directory; default current directory. |
| `gateway.policy_dir` | Dedicated named-policy replay directory; default `policies`. |
| `gateway.storage` | Validation hint: `sqlite` or `postgres`; `postgres` requires `storage_url`. Driver selection follows `storage_url`. |
| `gateway.storage_url` | PostgreSQL DSN for the persistent store, including keys, logs, budgets, saved config, and response cache; empty uses SQLite. PostgreSQL is experimental; A2A tasks and runtime health remain process-local. |
| `gateway.auth` | Require API-key/OIDC authentication; OIDC configuration enables auth. |
| `gateway.cache_threshold` | Lexical similarity cache cosine threshold; default `0.95`. |
| `gateway.cache_ttl` | Exact/lexical response cache lifetime; default `24h`. |
| `gateway.cache_scope` | Tenant boundary: `api_key` (default), `workspace`, `organization`, or explicit shared `global`. |
| `gateway.log_payloads` | Persist request/response bodies; default `false`. |
| `gateway.cors_origins` | Allowed browser origins; wildcard is rejected with auth enabled. |
| `gateway.circuit_breaker_failures` | Failures before opening a provider circuit; default `5`. |
| `gateway.circuit_breaker_cooldown` | Circuit reset interval; default `60s`. |
| `gateway.max_request_bytes` | Maximum JSON request body bytes; default `10485760`. |
| `gateway.read_header_timeout` | HTTP header timeout; default `5s`. |
| `gateway.read_timeout` | HTTP read timeout; default `30s`. |
| `gateway.write_timeout` | HTTP write timeout; default `0s` to avoid truncating streams. |
| `gateway.idle_timeout` | HTTP keep-alive idle timeout; default `60s`. |
| `gateway.shadow_provider` | Background chat mirror provider; its response is not returned to the caller. |
| `gateway.alerts` | Budget-threshold notification destinations. |
| `gateway.alerts.webhook_url` | Generic budget notification HTTP(S) endpoint. |
| `gateway.alerts.slack_url` | Slack incoming-webhook HTTP(S) endpoint. |
| `gateway.alerts.thresholds` | Budget fractions in `(0, 1]`; an empty list emits no threshold alerts. |
| `observability` | OTLP trace export and Prometheus settings. |
| `observability.otel_enabled` | Enable OTLP traces; setting an endpoint also enables export. |
| `observability.otlp_endpoint` | OTLP/HTTP trace collector URL. |
| `observability.service_name` | OTel service name; default `omniswitch-gateway`. |
| `observability.headers` | Collector request headers; environment-expanded at startup. |
| `observability.insecure` | Allow insecure OTLP transport. |
| `observability.timeout` | OTLP export timeout; default `10s`. |
| `observability.prometheus_enabled` | Expose the `/metrics` endpoint. |
| `providers` | Named upstream accounts or inference replicas. |
| `providers[].name` | Registry/account name; native account models can be addressed as `@name/model`. |
| `providers[].type` | Native adapter, OpenAI-compatible preset, `custom`, or `inference`. |
| `providers[].api_key_env` | Environment variable containing an upstream key; adapter default applies when omitted. |
| `providers[].base_url` | Custom/preset OpenAI-compatible base URL; also Azure endpoint fallback. |
| `providers[].endpoint` | Azure OpenAI deployment endpoint (takes precedence over `base_url`). |
| `providers[].api_version` | Azure OpenAI API version. |
| `providers[].region` | AWS Bedrock region. |
| `providers[].aws_access_key_env` | AWS access-key environment name; default `AWS_ACCESS_KEY_ID`. |
| `providers[].aws_secret_key_env` | AWS secret-key environment name; default `AWS_SECRET_ACCESS_KEY`. |
| `providers[].aws_session_token_env` | Optional AWS session-token environment name; default `AWS_SESSION_TOKEN`. |
| `providers[].guardrail_id` | AWS Bedrock guardrail identifier. |
| `providers[].guardrail_version` | AWS Bedrock guardrail version. |
| `providers[].models` | Advertised models for custom/preset, Azure, or Bedrock accounts; other native adapters use their catalogs. Inference uses replica models. |
| `providers[].extra_headers` | Custom/preset server-side request headers; values support environment expansion. |
| `providers[].replicas` | Inference replicas (required for `type: inference`). |
| `providers[].replicas[].url` | Runtime base URL; use `/v1` for vLLM/Ollama OpenAI-compatible chat. |
| `providers[].replicas[].runtime` | `vllm`/`ollama` use OpenAI-compatible chat; `tgi` uses non-streaming `/generate`. Default `openai`; Triton chat is unsupported. |
| `providers[].replicas[].models` | Accepted model names; empty accepts any explicitly routed model. |
| `providers[].replicas[].priority` | Replica score penalty; lower nonnegative values preferred. |
| `providers[].health_interval` | Inference probe interval; positive duration, default `10s`. |
| `identity` | Workload identity verification. |
| `identity.oidc` | OIDC JWT verification settings. |
| `identity.oidc.jwks_url` | JWKS HTTP(S) endpoint used to verify JWT signatures. |
| `identity.oidc.issuer` | Required JWT issuer when configured. |
| `identity.oidc.audience` | Required JWT audience when configured. |
| `identity.oidc.role_claim` | Claim containing the gateway role; default `role`. |
| `identity.oidc.workspace_claim` | Workspace claim name; default `workspace_id`. |
| `identity.oidc.organization_claim` | Organization claim name; default `organization_id`. |
| `identity.oidc.cache_ttl` | JWKS cache lifetime; default `5m`. |
| `authorization` | CEL request authorization. |
| `authorization.rules` | Ordered allow/deny CEL rules; a deny takes precedence. |
| `authorization.rules[].name` | Rule label for audit/error context. |
| `authorization.rules[].when` | CEL predicate over identity, method, path, and model. |
| `authorization.rules[].effect` | `allow` or `deny`; allow rules require at least one match. |
| `authorization.rules[].message` | Denial response explanation. |
| `rate_limit` | Request quota backend and window. |
| `rate_limit.requests` | Request count per quota window; default `120`. |
| `rate_limit.window` | Positive quota window; default `1m`. |
| `rate_limit.redis_url` | Optional Redis URL for coordinated fixed windows; otherwise local sliding windows. |
| `rate_limit.prefix` | Redis quota-key namespace; default `omniswitch:ratelimit`. |
| `rate_limit.fail_open` | Allow requests on Redis failure; default `false` returns `503`. |
| `guardrails` | Local rules, tool checks, and remote checks. |
| `guardrails.actions` | Built-in check name to action (`deny`, `redact`, `warn`, `log`). |
| `guardrails.rules` | Text regex checks. |
| `guardrails.rules[].name` | Rule/audit label. |
| `guardrails.rules[].stage` | `input`, `output`, or `both`; default `both`. |
| `guardrails.rules[].pattern` | Go-compatible regular expression. |
| `guardrails.rules[].action` | `deny`, `redact`, `warn`, `log`, or `fallback`; default `deny`. |
| `guardrails.rules[].message` | Explanation when the pattern matches. |
| `guardrails.rules[].fallback` | Reroute target: `provider` or `provider/model`, with a nonempty provider name; required with `action: fallback` and rejected with other actions. |
| `guardrails.tool_rules` | Tool-name/argument regex checks. |
| `guardrails.tool_rules[].name` | Tool rule/audit label. |
| `guardrails.tool_rules[].stage` | `input`, `output`, or `both`; default `output`. |
| `guardrails.tool_rules[].tool_name` | Regex over tool/function name. |
| `guardrails.tool_rules[].arguments_pattern` | Regex over serialized tool arguments. |
| `guardrails.tool_rules[].action` | `deny`, `redact`, `warn`, or `log`; default `deny`. |
| `guardrails.tool_rules[].message` | Explanation for a matching tool call. |
| `guardrails.webhooks` | External JSON moderation checks. |
| `guardrails.webhooks[].name` | Webhook/audit label. |
| `guardrails.webhooks[].url` | JSON moderation HTTP(S) endpoint. |
| `guardrails.webhooks[].stage` | `input`, `output`, or `both`; default `both`. |
| `guardrails.webhooks[].action` | Action when the service reports triggered; default `deny`. |
| `guardrails.webhooks[].headers` | Server-side HTTP headers; environment-expanded. |
| `guardrails.webhooks[].timeout` | Remote-check timeout; default `3s`. |
| `guardrails.webhooks[].fail_open` | Allow on check errors/timeouts; default `false`. |
| `guardrails.webhooks[].max_retries` | Additional attempts on network errors/5xx; default `0` (4xx and decisions are final). |
| `guardrails.llm_judges` | OpenAI-compatible chat-based moderation checks. |
| `guardrails.llm_judges[].name` | Judge/audit label. |
| `guardrails.llm_judges[].url` | OpenAI-compatible chat-completions HTTP(S) URL. |
| `guardrails.llm_judges[].model` | Moderation model name. |
| `guardrails.llm_judges[].stage` | `input`, `output`, or `both`; default `both`. |
| `guardrails.llm_judges[].action` | Action when the judge reports triggered; default `deny`. |
| `guardrails.llm_judges[].system_prompt` | Optional override for the JSON moderation system prompt. |
| `guardrails.llm_judges[].api_key_env` | Environment variable containing the judge API key. |
| `guardrails.llm_judges[].headers` | Judge request headers; environment-expanded. |
| `guardrails.llm_judges[].timeout` | Judge timeout; default `10s`. |
| `guardrails.llm_judges[].fail_open` | Allow on judge errors/timeouts; default `false`. |
| `guardrails.stream_buffer` | Buffer SSE before output enforcement; default `true`. |
| `mcp` | MCP proxy targets and policies. |
| `mcp.enabled` | Mount MCP proxy routes; default `false`. |
| `mcp.policy` | Default CEL/Policy file for tool calls. |
| `mcp.upstream` | Legacy single HTTP MCP upstream; use `targets` for federation. |
| `mcp.targets` | Named HTTP, stdio, or OpenAPI targets. |
| `mcp.targets[].name` | Target label and federated tool prefix. |
| `mcp.targets[].transport` | `http`/`streamable_http` (default) or `stdio`. |
| `mcp.targets[].upstream` | HTTP MCP or OpenAPI REST base URL. |
| `mcp.targets[].command` | Executable for a stdio target. |
| `mcp.targets[].args` | Stdio command arguments; environment-expanded. |
| `mcp.targets[].environment` | Stdio environment additions; values environment-expanded. |
| `mcp.targets[].policy` | Target policy file; overrides the MCP default. |
| `mcp.targets[].headers` | Server-side HTTP headers; environment-expanded. |
| `mcp.targets[].forward_bearer_token` | Forward OIDC Bearer tokens; default `false`. Gateway API keys are never forwarded. |
| `mcp.targets[].openapi_spec` | OpenAPI YAML/JSON file or HTTP(S) URL, converted to policy-gated MCP tools. |
| `mcp.targets[].enabled` | Enable this target; default `true`. |
| `routes` | Model/alias names mapped to routing rules. |
| `routes.*.strategy` | `direct`, `fallback`, `round-robin`, `least-latency`, or `cost-optimal`; omitted infers direct/fallback. |
| `routes.*.provider` | Primary registry provider/account. |
| `routes.*.fallbacks` | Ordered fallback providers/accounts. |
| `routes.*.max_retries` | Additional retry attempts; default `0`. |
| `routes.*.retry_backoff` | Retry delay duration; default `200ms`. |
| `routes.*.retry_codes` | Restrict retries to these HTTP status codes. Empty retries errors other than canceled contexts or unsupported parameters. |
| `routes.*.timeout` | Per-attempt duration; omitted uses caller/provider timeout. |
| `routes.*.shadow_provider` | Route-specific background chat mirror. |
| `routes.*.default_params` | Request parameters supplied when the caller omits them. |
| `routes.*.override_params` | Request parameters forced by the route. |
| `routes.*.drop_params` | Request fields removed before upstream forwarding. |
| `routes.*.variants` | Weighted, optionally conditional provider/model alternatives. |
| `routes.*.variants[].name` | Optional label shown in configuration summaries; not used to choose a variant or in request-routing metadata. |
| `routes.*.variants[].provider` | Registry provider/account for the variant. |
| `routes.*.variants[].model` | Upstream model override. |
| `routes.*.variants[].weight` | Positive integer selection weight. |
| `routes.*.variants[].condition` | Optional CEL predicate for variant eligibility. |
