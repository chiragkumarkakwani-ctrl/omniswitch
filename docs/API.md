# API Reference

OmniSwitch exposes OpenAI-compatible inference endpoints and OmniSwitch management endpoints.

## OpenAI-Compatible Gateway

### `POST /v1/chat/completions`

Accepts OpenAI-style chat completion requests.

Supported request fields, and how each is handled per adapter:

| Field | openai / azure / groq / custom (OpenAI-compatible) | anthropic | google |
|---|---|---|---|
| `model`, `messages`, `temperature`, `max_tokens`, `top_p`, `stream`, `stop` | forwarded | forwarded/translated | forwarded/translated |
| `tools`, `tool_choice` | forwarded | translated | translated |
| `message.name` (per-message) | forwarded | dropped (no Anthropic equivalent) | dropped (no Gemini equivalent) |
| OpenAI content arrays with `text` and `image_url` parts | forwarded | translated | translated |
| `response_format` (`text`, `json_object`, `json_schema`) | forwarded | translated to `output_config.format` (`json_schema`); any other `type` is rejected | translated to `responseMimeType`/`responseSchema`; any other `type` is rejected |
| `max_completion_tokens` | forwarded | translated to `max_tokens` (wins over `max_tokens` if both set) | translated to `maxOutputTokens` (wins over `max_tokens` if both set) |
| `parallel_tool_calls` | forwarded | translated to `tool_choice.disable_parallel_tool_use` | **rejected** (400 `unsupported_parameter`) |
| `stream_options.include_usage` | forwarded | honored: an extra chunk with empty `choices` and populated `usage` is emitted before `[DONE]` | honored: same synthesized final-chunk behavior |
| `store`, `metadata` | forwarded | ignored (platform bookkeeping only; no effect on generation, not forwarded upstream) | ignored (same as anthropic) |
| `n` (only `1` is safe) | forwarded | **rejected** if `n > 1` | **rejected** if `n > 1` |
| `seed` | forwarded | **rejected** | **rejected** |
| `presence_penalty`, `frequency_penalty` | forwarded | **rejected** | **rejected** |
| `logit_bias` | forwarded | **rejected** if non-empty | **rejected** if non-empty |
| `reasoning_effort` | forwarded | **rejected** if non-empty | **rejected** if non-empty |
| `service_tier` | forwarded | **rejected** if non-empty | **rejected** if non-empty |
| `user` | forwarded | dropped (no Anthropic equivalent) | dropped (no Gemini equivalent) |
| any other/future top-level OpenAI field (e.g. `prediction`, `modalities`, `audio`) | forwarded byte-for-byte (captured via an internal `Extra` map, not silently dropped) | dropped (adapter-specific request shape has no passthrough slot) | dropped (same) |

"Rejected" means the gateway returns HTTP 400 with an OpenAI-style error body
instead of silently ignoring the field or forwarding a request that would
behave differently than requested:

```json
{"error": {"message": "anthropic does not support the \"seed\" parameter; remove it or use an OpenAI-compatible provider", "type": "invalid_request_error", "param": "seed", "code": "unsupported_parameter"}}
```

This applies to both the non-streaming and streaming (`stream: true`) paths.
Requests to OpenAI-compatible upstreams (`openai`, `azure`, `groq`, `custom`,
and any preset built on `custom`) never hit this path: every field above,
plus any field OmniSwitch does not model by name, is forwarded verbatim.

Cache keys (exact-match cache) and guardrail evaluation see the full request:
the cache key is derived from the request's complete JSON encoding (so two
requests differing only in, say, `response_format` or `seed` never collide on
the same cached response), and guardrails evaluate the actual `messages`
content, which is unaffected by any of the fields above.

Headers:

- `Authorization: Bearer <omniswitch-api-key>` when `OMNISWITCH_AUTH=true`
- `x-omniswitch-provider`: force a provider or provider alias
- `x-omniswitch-config`: use a dynamic gateway config by name or ID
- `x-omniswitch-shadow-provider`: run a secondary provider asynchronously
- `x-omniswitch-trace-id`: group calls into a trace
- `x-omniswitch-session-id`: group calls into an agent session

Streaming is available on this endpoint with `stream: true`. OmniSwitch's default
guardrail mode buffers stream output before emitting SSE so output policies can
be enforced.

### `POST /v1/responses`

Implements the practical text/messages subset of the OpenAI Responses API:
`model`, a string or message-array `input`, `instructions`, `temperature`,
`max_output_tokens`, and `top_p`. It uses the same routing, cache, budget,
guardrail, and logging path as chat completions. Responses streaming, tools,
background mode, and other advanced Responses features are not yet supported.

### `POST /v1/messages`

Accepts the core Anthropic Messages shape (`model`, `messages`, `system`,
`max_tokens`, `temperature`, `top_p`) and returns an Anthropic-style message
result through the common OmniSwitch path. Streaming and Anthropic tool use are
not yet supported.

### `POST /v1/embeddings`

Forwards OpenAI-compatible embedding requests (`model`, `input`, optional
`encoding_format`, `dimensions`, and `user`) to OpenAI or a compatible custom
provider. Input budget and guardrail checks apply; providers without an
embeddings implementation are skipped in a configured fallback chain.

### `POST /v1/rerank`

Accepts a common RAG rerank request shape (`model`, `query`, `documents`,
optional `top_n`, `return_documents`, `max_chunks_per_doc`, and `user`) and
routes it to providers that implement reranking. Custom OpenAI-compatible
providers forward rerank traffic to `/rerank`; fallback chains skip providers
without rerank support. Query and document text are inspected by input
guardrails before the provider call.

### `GET /v1/models`

Returns all models registered through provider adapters and provider catalog aliases.
For OpenAI, Anthropic, Google, and Groq, the advertised model list and the
per-1M-token pricing used for cost estimation and cost-optimal routing both
come from the consolidated model catalog at
`internal/provider/catalog/models.json` (see "Model catalog" in
[docs/CONFIGURATION.md](CONFIGURATION.md)).

## A2A Gateway

### `GET /.well-known/agent-card.json`

Publishes a public A2A v1 Agent Card for OmniSwitch's JSON-RPC interface. The
card advertises `/a2a` as a governed text inference agent and is cacheable by
A2A clients with `Cache-Control` and `ETag` validators.

### `POST /a2a`

Accepts A2A v1 JSON-RPC requests. `SendMessage` supports direct text message
responses by converting message text into the same chat-completions path used
by OpenAI-compatible clients. Provide the model with `metadata.model` or the
`x-omniswitch-model` header.

Supported A2A methods:

- `SendMessage`
- `GetExtendedAgentCard`

Task lifecycle methods, streaming, push notifications, and outbound A2A client
calls are not yet implemented.

## Management API

Send `Authorization: Bearer <gateway-api-key>` when authentication is enabled.
Every `/api/*` endpoint requires authentication except `GET /api/health`.
Viewer and member roles see only their key's logs, traces, decisions, metrics,
and budget usage; admin and owner roles see global data. Caller-supplied role,
key, and workspace headers do not change this scope. With authentication off,
the control plane runs as a synthetic owner; do not expose that mode publicly.

Invalid, expired, or disabled credentials return **401**. Insufficient role or
an authorization-policy denial returns **403**. API errors have this shape:

```json
{"error":{"message":"...","type":"...","code":"..."}}
```

Timestamps are RFC 3339 UTC; monetary values are numeric USD. Unless otherwise
noted, a `window` is `1h`, `6h`, `24h` (default), `7d`, or `30d`. Lists use
`limit=50` and `offset=0`; the HTTP page limit is 100. Larger positive limits
are clamped; zero/negative limits and negative offsets return 400.

### `GET /api/health`

Public reachability information for sign-in. This is not a provider readiness
probe.

### `GET /api/session`

Validates the current identity and returns `authenticated`, `auth_enabled`,
`auth_method`, `key_id`, `name`, `key_prefix`, `role`, `workspace_id`,
`organization_id`, nullable `expires_at`, and `is_bootstrap`. The `gateway`
object contains `version`, `commit`, `storage`, `listen`, `mcp_enabled`, and
`log_payloads`. It never includes the bearer secret, stored hash, key metadata,
or OIDC claims. `key_prefix` is the stored display value, not a secret.
Responses use `Cache-Control: no-store`.

Auth-off sessions report role `owner`, method `none`, and empty identity IDs.
OIDC sessions use the verified subject as the display name; only the local
`bootstrap-admin` API key sets `is_bootstrap`.

`OMNISWITCH_BOOTSTRAP_API_KEY` creates that key only when absent. A restart does
not re-enable a disabled bootstrap key or reset its name, role, expiry,
workspace, limits, or accrued spend. Changing the configured secret rotates
only the stored hash. Create a personal administrator key before disabling the
bootstrap key.

### `GET /api/logs`

Returns `{logs, total, limit, offset}`. Supported filters:

| Parameter | Meaning |
|---|---|
| `window` | Relative range; default `24h` |
| `since`, `until` | RFC 3339 bounds overriding either relative bound; `since` inclusive, `until` exclusive |
| `provider`, `model`, `trace_id`, `session_id`, `endpoint` | Exact value |
| `api_key_id` | Exact key filter, admin/owner only; non-admin use returns 403 |
| `status` | `success`, `error`, `denied`, `limited`; repeat the parameter or comma-separate values |
| `decision` | `ALLOW` or `DENY` |
| `cached` | Boolean cache-result filter |
| `q` | Case-insensitive literal search over model, trace, session, and error text, not payloads |
| `sort` | `time_desc` (default), `time_asc`, or `latency_desc`, with deterministic ID ties |
| `limit`, `offset` | Pagination |

Ranges must be positive and no longer than 366 days. Text filters are limited
to 256 bytes of valid UTF-8 without NUL characters. Malformed URL encoding is
rejected. `%` and `_` in `q` are literal characters, not SQL wildcards.

Endpoint names are `chat_completions`, `responses`, `messages`, `embeddings`,
`moderations`, `rerank`, `images_generations`, `audio_transcriptions`,
`audio_speech`, `mcp`, and `a2a`. Logs also include `cost_source`
(`catalog`, `override`, or `none`), `cached_tokens`, and `reasoning_tokens`.
Legacy missing metadata is normalized to the JSON string `"{}"`.

Raw `request_body` and `response_body` are stored only when `log_payloads: true`
is explicitly enabled. Existing stored payloads may remain until retention
clears them; turning logging off does not retroactively remove them.

### `GET /api/logs/facets?window=24h`

Returns sorted `{providers, models, statuses, endpoints}` arrays for the
caller's scope and window, capped at 200 distinct values per facet. This is a
window-wide options query, not pagination of the request list.

### `GET /api/logs/export?format=csv`

Admin/owner only. Uses the same filters, order, and offset as `/api/logs` and
exports at most **10,000 rows** (or a smaller supplied positive `limit`).
Returns `text/csv; charset=utf-8`, `Cache-Control: no-store`, and an attachment
named `omniswitch-requests-<UTC timestamp>.csv`.

Columns are `id,timestamp,trace_id,session_id,provider,model,api_key_id,endpoint,
status,decision,input_tokens,output_tokens,total_tokens,cached_tokens,
reasoning_tokens,latency_ms,cost,cost_source,cached`. Payloads are excluded by
the database query; metadata, error text, and guardrail details are not exported.
Text starting with `=`, `+`, `-`, or `@` after whitespace is prefixed with an
apostrophe to prevent spreadsheet formula interpretation.

### `GET /api/metrics?window=24h`

Returns scoped request, decision, error, token, latency, and cost aggregates.

### `GET /api/metrics/providers?window=24h`

Returns `{providers:[...]}` with scoped per-provider request counts, average
latency, error rate, and cost. Honors the same window values as other metrics.

### `GET /api/metrics/timeseries?window=24h&bucket=1h`

Returns `{window,bucket,buckets:[...]}`. Each bucket has `start`, `requests`,
`allowed`, `denied`, `limited`, `errors`, `cache_hits`, `tokens`, `cost`, and
`avg_latency_ms`. Empty buckets contain zeroes. Buckets align to Unix UTC
boundaries; first/last buckets may represent a partial range.

Default bucket sizes: `1h` → `5m`, `6h` → `15m`, `24h` → `1h`, `7d` → `6h`,
`30d` → `1d`. An override accepts a positive whole-second Go duration or `1d`,
no longer than 30 days. More than 200 aligned buckets returns 400 rather than
silently changing the requested resolution.

### `GET /api/metrics/latency?window=24h`

Returns `{count,avg_ms,p50_ms,p95_ms,p99_ms}`. Percentiles use nearest rank over
ordered recorded latency values, not interpolation or Prometheus histograms.
An empty range returns numeric zeroes.

### `GET /api/traces/{id}?window=30d`

Returns `{trace_id,logs,guardrail_events,shadow_logs,feedback,spans}`. The default
trace window is **30 days**; other supported windows are accepted. Unknown or
out-of-scope traces return 404. Logs add `key_name`; their `metadata` remains a
JSON string. Optional persisted `metadata._spans` are also returned in the
ordered top-level `spans` array. No stage timing is inferred from log timestamps.

Measured stages include auth, input/output guardrails, route resolution,
cache lookup, individual provider attempts, retry backoff, and applied
fallback/reroute decisions. A stream's provider attempt ends when its channel
finishes, not when the channel is acquired. A request recorder holds at most
256 stages; actual truncation sets `metadata._spans_truncated=true`.

Trace retrieval is time-bounded and capped at 500 request logs and 100 each of
guardrail events, shadow logs, and feedback. Non-admin ownership is established
through request IDs before related records are read, even when keys share a
caller-supplied trace ID. Feedback linked only by a trace ID is admin-only.

### `GET /api/guardrails/events`

Returns `{events,total,limit,offset}`. Accepts `window`, exact `type`, `action`,
`trace_id`, and pagination. Events join request logs to add `trace_id`, `model`,
`provider`, and `api_key_id`. Details are bounded to 256 Unicode characters;
recognizable API-key, bearer-token, credential-assignment, and email patterns
are redacted. This is pattern-based redaction, not a general DLP guarantee.

### `GET /api/guardrails/summary?window=24h`

Returns exact matched/action totals and at most 200 grouped type rows:
`{matched,by_action:{deny,redact,warn,log,fallback,reroute,retry},by_type:[{type,count}]}`.
Zero-action counts are present. Non-admins only see events joined to their key.

### `GET /api/mcp/targets`

Returns `{enabled,targets:[...]}` including configured disabled targets.
Each target reports name, transport, enabled/connected status, tool counts,
visible/hidden counts for the caller, safe `last_error`, `openapi_spec`,
`forward_bearer_token`, and `token_exchange:{configured,token_url_host,audience}`.
Policy paths are admin-only. Headers, transport commands, upstream URLs,
client secrets, and actor/access tokens are not returned.

Connection state comes from bounded `tools/list` discovery, not a guarantee
that a subsequent tool call will succeed. Native discovery is bounded to
1 MiB, 1,000 tools, and a per-target deadline; incomplete paginated discovery
is not reported as a complete inventory. Known OpenAPI tools remain inspectable
when native discovery fails, with the target explicitly marked disconnected.

### `GET /api/mcp/tools?target=<name>&as_key=<key-id>`

Returns `{principal:{key_id,workspace_id,role},tools:[...]}`. Tools include
`target`, `name`, `namespaced_name`, `description`, `list:{allowed,rule,reason}`,
and `call:{allowed,rule,reason,evaluated_without_arguments:true}`. Non-admins
never receive hidden tools; admins receive explanations for hidden tools too.
The mere presence of `as_key` requires admin/owner; empty values return 400,
unknown keys/targets return 404. With `as_key`, policy sees the stored key's
role and workspace rather than browser-supplied identity headers.

This is a dry run: no tool invocation and no audit event. Call policy uses
empty arguments and cannot promise the outcome of a real argument-bearing
call. Argument-dependent evaluation failures deny with an explicit explanation.

### `GET /api/mcp/decisions?window=24h`

Returns `{decisions,total}` with normal pagination and key scoping. Decisions
include timestamp, decision ID, agent, tool, action, target, decision, rule,
reason, latency, and trace ID from persisted MCP audit metadata. Missing legacy
metadata stays empty rather than inventing an agent, rule, or target.

### `GET /api/providers/health?window=24h`

Returns sorted `{providers:[...]}` with `name`, `type`, `circuit`, `requests`,
`avg_latency_ms`, `error_rate`, and `cost`. Traffic aggregates are key-scoped;
the circuit state is global to the current process. Circuit snapshots include
`state`, `failures`, `threshold`, `cooldown_s`, nullable `open_until`, and
nullable `last_failure`, without advancing the breaker state machine.
Inference `replicas` are present only when a probe source is available;
unprobed health/timestamps remain null, never fabricated.

### `POST /api/providers/{name}/circuit/reset`

Admin/owner only. Clears the registered provider's failure history and returns
`{name,circuit:{state:"closed"}}`. Unknown providers return 404. Does not reset
another gateway process's circuit state.

### `GET /metrics`

Prometheus exposition is separate from SQL window aggregates. See
[Observability](OBSERVABILITY.md) for authentication, public opt-in, and metric
family semantics.

### `GET /api/providers`

Returns registered provider names and exposed models.

### `GET /api/config`

Returns a redacted runtime configuration posture summary for authenticated
operators, including `version`, `commit`, and `auth_enabled`, gateway settings,
rate-limit backend, identity mode, authorization rule names/effects, guardrail
rule/webhook names, provider counts, route summaries, MCP posture, and A2A
capabilities. `config_path` and MCP `policy_path` fields are admin-only.
It does not return bootstrap keys, provider credentials, target headers, webhook
URLs, provider base URLs, Redis URLs, or policy expressions.

### `POST /api/config/validate`

Admin/owner only. Accepts `{format:"yaml"|"json",body:"..."}` and returns
`{valid,errors:[{line,message}]}`. Invalid config returns HTTP 200 with
`valid:false`; malformed request envelopes return 400. Syntax errors have a
source line when known; semantic errors use line 0, not a guessed line.
Uses the same gateway parser and semantic checks as raw-save. Does not write
or apply config. Body size is at most 1 MiB; the JSON envelope is at most 2 MiB.
Unknown properties and trailing JSON/YAML documents are rejected.

### `GET /api/config/raw`, `POST /api/config/raw`

GET retains viewer access to the **raw configured file**, returning
`{path,format,body}` with `no-store`, not the redacted posture summary. Do not
put secrets directly in that file when granting read access to workload keys;
use supported environment references. No attached file returns 404.

POST requires admin/owner and the same input as validation. Invalid config
returns 400 with `{valid:false,errors:[...]}` and leaves the file unchanged.
The format must match the configured extension. A valid save uses a
same-directory atomic replacement, preserves existing permissions (0600 for
a new file), and returns `{saved:true,path}`. It writes only the configured
path; request-supplied paths are rejected. Saving does not promise that every
restart-required setting is immediately applied; see configuration reload rules.

### `POST /api/virtual-keys`

Stores an encrypted provider credential and exposes it as a virtual provider on next gateway startup.

```json
{
  "name": "azure-prod",
  "provider_type": "custom",
  "provider_name": "azure-prod",
  "base_url": "https://YOUR_RESOURCE.openai.azure.com/openai/deployments/gpt-4o/chat/completions?api-version=2024-10-21",
  "provider_key": "real-provider-secret",
  "metadata": {
    "auth_header": "api-key",
    "models": "gpt-4o,gpt-4o-mini"
  }
}
```

The response redacts `provider_key`. Use `OMNISWITCH_VAULT_KEY` to keep stored credentials decryptable across restarts.

### `GET /api/virtual-keys`

Lists virtual provider keys without decrypted provider credentials.

### `POST /api/virtual-keys/rotate`

Rotates the encrypted provider credential for an existing virtual key.

```json
{
  "name": "azure-prod",
  "provider_key": "new-real-provider-secret"
}
```

### `DELETE /api/virtual-keys?name=<virtual_key_name>`

Revokes a virtual key without deleting its audit history.

### `GET /api/feedback`

Lists feedback entries.

Query parameters:

- `limit`
- `trace_id`
- `request_id`

### `POST /api/feedback`

Records human feedback.

```json
{
  "trace_id": "trace_123",
  "score": 1,
  "rating": "up",
  "comment": "Useful response",
  "user_id": "user_123",
  "metadata": {
    "screen": "chat"
  }
}
```

`score` must be `-1`, `0`, or `1`.

### `POST /api/keys`

Admin/owner only. Creates a gateway client key and returns
`{key,api_key}`. The raw `key` is shown **only once**; only its hash is stored.

```json
{
  "name": "production-app",
  "workspace_id": "ws_123",
  "role": "member",
  "rate_limit": 120,
  "monthly_cost_budget": 50,
  "monthly_token_budget": 1000000,
  "requests_per_minute": 120,
  "tokens_per_minute": 20000,
  "usd_per_day": 5,
  "expires_in": "720h"
}
```

`name` is required and trimmed; role defaults to `viewer`. Omitted legacy
`rate_limit` defaults to 60; explicit zero is preserved. Per-key limit/budget
zeroes mean no additional limit, not exemption from configured global policies.
Integer limits must be 0…2147483647, USD values finite/nonnegative, and a supplied
`expires_in` a positive Go duration. Names/workspaces must be valid text without
NUL characters, bounded to 256 bytes. Key request envelopes are limited to
16 KiB; unknown fields and multiple JSON objects return 400.

### `GET /api/keys`

Returns `{keys:[...]}` without secret/hash material. Non-admins receive only
their own stored key; verified identities with no local key receive an empty
list. Admins may filter by `workspace_id`. Rows include the stored display
`key_prefix`, role/workspace, budgets, `requests_per_minute`, `tokens_per_minute`,
`usd_per_day`, and nullable `last_used_at`.

### `PATCH /api/keys?id=<key-id>`

Admin/owner only. Atomically updates supplied fields and returns the updated
key. Writable fields: `name`, `enabled`, `rate_limit`, `budget_usd`,
`monthly_cost_budget`, `monthly_token_budget`, `role`, `workspace_id`,
`requests_per_minute`, `tokens_per_minute`, `usd_per_day`. Omitted/null fields
are unchanged; an empty update returns 400. Unknown/protected properties are
rejected. Missing keys return 404.

PATCH never resets the secret/hash, display prefix, expiry, creation/last-use
timestamps, or accrued spend. If legacy `budget_usd` and `monthly_cost_budget`
are equal, changing just one keeps their duplicate alias in sync; independently
configured unequal budgets remain independent.

### `GET /api/keys/usage?window=month`

Returns `{period:{start,end},keys:[...]}` for the UTC calendar month, with usage
measured from month start through now and `end` at the next month boundary.
Includes zero-traffic keys; viewer/member scope is their own key. Other windows
return 400. Rows contain `key_id`, `cost_usd`, `tokens`, `cost_budget`,
`token_budget`, `cost_pct`, `token_pct`, and `state`. Ratios are fractions
(0.8 = 80%), not percentage integers. State is `exceeded` at ≥1, `near` at ≥0.8,
otherwise `ok`, considering either configured budget.

Usage sums recorded `ALLOW` request rows; it is not a live reservation/limit
snapshot. Effective cost budget uses positive `budget_usd` before
`monthly_cost_budget`. Unpriced traffic and separately accrued spend are not
invented in these recorded totals.

### `DELETE /api/keys?id=<key-id>`

Admin/owner only. Disables an API key rather than exposing or deleting its
secret. Subsequent authentication with that key returns 401.

### `POST /api/orgs`

Creates an organization.

```json
{
  "name": "Acme",
  "metadata": {
    "tier": "enterprise"
  }
}
```

### `GET /api/orgs`

Lists organizations.

### `POST /api/workspaces`

Creates a workspace inside an organization.

```json
{
  "organization_id": "org_123",
  "name": "Production"
}
```

### `GET /api/workspaces`

Lists workspaces. Use `organization_id` to filter.

### `POST /api/users`

Creates or updates a user.

```json
{
  "email": "ada@example.com",
  "name": "Ada Lovelace"
}
```

### `GET /api/users`

Lists users.

### `POST /api/workspace-members`

Creates or updates a workspace membership.

```json
{
  "workspace_id": "ws_123",
  "user_id": "user_123",
  "role": "admin"
}
```

### `GET /api/workspace-members`

Lists workspace memberships. Use `workspace_id` to filter.

### `POST /api/prompts`

Creates a prompt template. Reusing an existing `name` creates the next version.

### `GET /api/prompts`

Lists prompt templates.

### `GET /api/prompts/versions?name=<prompt_name>`

Lists all versions for a prompt name, newest version first.

### `POST /api/prompts/render`

Renders a prompt template with variables.

### `POST /api/evals/policy`

Replays a batch of tool requests against one or more OmniSwitch policy files.

```json
{
  "policy_paths": ["policies/production-delete.yaml"],
  "requests": [
    {
      "agent": {"id": "coder", "department": "engineering"},
      "tool": {"name": "github"},
      "action": {"name": "delete"},
      "resource": {
        "type": "repo",
        "name": "payments",
        "environment": "production"
      }
    }
  ]
}
```

The response includes aggregate `allowed`, `denied`, and `errors` counts plus each decision trace.

## Moderation

### `POST /v1/moderations`

Provides an OpenAI-compatible moderation response using OmniSwitch's configured
local input guardrails. `input` accepts a string or an array of strings. Each
result includes `flagged`, `categories`, and `category_scores`; no request text
is sent to an external moderation provider.

## MCP Gateway

### `POST /mcp`

Evaluates MCP JSON-RPC `tools/call` requests through OmniSwitch policy and forwards allowed requests upstream. With `mcp.targets` configured, `tools/list` federates tools from each target under `target__tool` names and `tools/call` dispatches the prefixed name to its owning target. Target headers are configured server-side and OmniSwitch credentials are not forwarded upstream.

### `POST /v1/mcp/tools/call`

Alias for the MCP tool-call gateway.

The MCP gateway supports JSON-RPC over HTTP, including streaming
`text/event-stream` responses from streamable HTTP/SSE-capable upstreams.
Targets can opt into OIDC bearer-token delegation with
`forward_bearer_token: true`; local OmniSwitch API keys are never forwarded.
It supports persistent stdio targets as well as HTTP and streamable HTTP. It
does not yet implement OpenAPI conversion. A2A is exposed separately through
the `/.well-known/agent-card.json` and `/a2a` endpoints.

## Roles and Scopes

When `OMNISWITCH_AUTH=true`, API-key roles are `viewer`, `member`, `admin`, and
`owner`. All valid keys can call inference and MCP. Viewers can read dashboard
data (scoped to their own key); members can change prompts and run policy evals;
admins/owners can mutate keys, virtual keys, and organization/workspace data.
Admins and owners see global dashboard aggregates, while other roles are
limited to their key's logs and metrics.
