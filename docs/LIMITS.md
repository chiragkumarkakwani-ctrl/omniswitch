# Request, token, and spend limits

OmniSwitch applies every matching policy, not just the first policy. Policies
can share a counter across a key, workspace, organization, model, or the whole
gateway. Configure them in `rate_limit.policies`; see
[CONFIGURATION.md](CONFIGURATION.md) for the complete field inventory.

## Defaults and per-key limits

The gateway retains its existing default of 120 requests per minute, or the
configured `rate_limit.requests` and `rate_limit.window`. A positive legacy
key `rate_limit` overrides that default for the key. These policies are named
`default-requests`.

Positive key columns add independent policies:

| Key field | Policy | Window |
| --- | --- | --- |
| `requests_per_minute` | `key-requests-per-minute` | 1 minute |
| `tokens_per_minute` | `key-tokens-per-minute` (`total_tokens`) | 1 minute |
| `usd_per_day` | `key-usd-per-day` | 24 hours |

Zero disables each additional key policy; it does not disable the default
request policy. A new RPM ceiling is additive, not a replacement for the
legacy default quota. Authentication loads the key on each request, so key
edits take effect on the next request without a config reload. Authenticated
key/workspace/organization identity comes from authentication, not caller
quota headers. With authentication disabled, the legacy header/IP fallback
remains available; it is not a tenant security boundary.

The four implicit names above are reserved. Explicit policy names must be
unique. Request/token limits are positive integers; USD limits are positive,
finite dollar amounts. Supported units are `requests`, `input_tokens`,
`output_tokens`, `total_tokens`, and `usd`. Policy windows accept Go durations
or a `d` suffix, from 1 second to 30 days; `1d` means exactly 24 hours.

## Matching and windows

`match.key_id`, `match.workspace_id`, and `match.model` are optional globs,
combined with AND. An omitted selector matches all applicable identities;
`*` and `?` can match slashes in model IDs. Scopes without the required trusted
identity do not acquire a counter.

Request policies match the submitted model and run before routing. A bounded
JSON prefix inspection preserves the request body; models beyond that prefix
are checked after decoding, still before routing. Token/USD policies use the
resolved upstream model, stripping gateway account prefixes. Scope `model`
shares a bucket for that model; scope `global` shares one across the gateway.
Other scopes use their authenticated identity IDs. Filtered policies still
aggregate all matching traffic within their selected scope.

Both backends use fixed windows aligned to the Unix epoch. A 24-hour window
therefore resets at UTC midnight, not 24 hours after the first request.
Changing a limit, action, or selector with the same name/scope/unit/window
keeps current usage. Renaming a policy or changing its window creates a new
counter. Counters expire automatically.

## Reservation and reconciliation

Chat (blocking and streaming), embeddings, and rerank reserve token/USD usage
immediately before upstream I/O. Routing variants, aliases, retries, and
fallbacks use the selected provider/model rather than reselecting a route for
pricing. A policy rejection stops the route; fallback cannot bypass it.

The estimator uses text bytes divided by four, rounded up, plus message
framing and tool/schema text. Chat reserves the requested output maximum
(`max_completion_tokens` before `max_tokens`), or 256 tokens when unspecified.
Embedding token arrays use their length; rerank includes query and document
text. These are approximations, especially for images and non-English text.

Reported upstream usage reconciles each reservation once. Cache reads/writes
are included in input totals; reasoning/thinking tokens are included in output
totals, never added twice. Actual usage may exceed the estimate and can take a
counter past its ceiling; the gateway cannot undo already-consumed upstream
work. Later requests are rejected until capacity returns. This is not an
invoice-exact spending guarantee.

Successful responses without usable upstream usage retain the conservative
reservation instead of refunding it. Their request logs show cost zero and
`cost_source: none`, not a claim of free service. Failed attempts release their
unused token/USD reservation. Request checks and token/USD checks are separate
atomic phases: a later token/spend rejection still counts as a request when
the earlier request check passed.

Live streams ask the upstream for usage even when the client did not request
usage chunks; unrequested usage is not forwarded. Reconciliation uses a
bounded context independent of client cancellation. Headers already sent on a
live stream describe its reservation; `/api/limits/usage` shows the reconciled
counter afterwards. Blocking and buffered responses can publish reconciled
headers. Late completion adjusts only the original window, never the next one.
An uncertain backend adjustment is not retried, avoiding a double delta.

Semantic cache hits count their returned logical token usage but reserve no
new upstream USD. Media integrations use the same reservation API and the
cited image/duration/character price helpers. Unknown sizes, qualities, prices,
or audio durations remain unpriced; request quotas still apply. Speech pricing
expects Unicode character counts. External surcharges, time/context tiers,
and other provider fees require appropriate operator pricing overlays.

## Backends and failures

Without Redis, counters are mutex-protected in memory and last for one process.
They are not shared between replicas and reset on restart. Redis counters use
namespaced Lua operations: each phase checks all its charges before writing,
so a refusal never partially charges that phase. Adjustments preserve TTLs and
do not recreate expired windows. USD counters use millionths of a dollar,
rounding reservations and limits upward to that precision.

Use the same prefix and policy configuration on every replica. This backend
uses a standalone/single-primary Redis client, not Redis Cluster or Sentinel
discovery. Configure Redis persistence separately if needed. An unavailable
backend fails closed with HTTP 503 before upstream I/O by default.
`rate_limit.fail_open: true` explicitly allows traffic during backend failures;
usage cannot be guaranteed during that interval. Config validation errors are
not treated as backend outages and are never silently accepted.

`action: log` counts usage and logs an exceeded ceiling but does not reject.
Log-only policies do not restrict the response headers.

## Response headers and errors

Every `/v1/*` response carries these six headers, including early errors:

```text
x-ratelimit-limit-requests
x-ratelimit-remaining-requests
x-ratelimit-reset-requests
x-ratelimit-limit-tokens
x-ratelimit-remaining-tokens
x-ratelimit-reset-tokens
```

They describe the most restrictive applicable request and token buckets.
Token headers consider input, output, and total-token policies. Reset values
are durations such as `1s` or `6m0s`, not epoch timestamps, following
https://developers.openai.com/api/docs/guides/rate-limits . No matching bucket
(or an error before authentication) uses `0`, `0`, and `0s`; this is an
absence marker, not a zero-capacity policy. USD policies additionally provide
`x-omniswitch-{limit,remaining,reset}-usd`. CORS exposes these headers.

A rejecting policy returns 429 and an integer-seconds `Retry-After`:

```json
{"error":{"type":"rate_limit_exceeded","code":"total_tokens_limit_exceeded","message":"policy key-tokens-per-minute: 1000 total_tokens per 1m exceeded","param":null}}
```

The code is `<unit>_limit_exceeded`. Messages uses the Anthropic
`{"type":"error","error":{"type":"rate_limit_error","message":"..."}}`
envelope. One `limited` audit row records `decision: DENY`,
`decision_reason: rate_limit:<policy>`, and the original endpoint, without
request/response bodies. The centralized rejection hook is the integration
point for `omniswitch_rate_limit_rejections_total{policy}`.

Existing calendar-month budgets are separate controls and continue returning
402. They are not replaced by the daily/window-based USD policies.

## Inspection and reload

- `GET /api/limits/usage?key_id=<id>&model=<model>` returns current policy usage,
  remaining capacity, reset seconds, and the policy source (`key` or `config`).
  `key_id` defaults to the caller. Viewer/member callers can inspect only their
  own key; admin/owner callers can select another key. Shared workspace,
  organization, model, and global buckets affecting that key are included.
- `GET /api/catalog` returns effective embedded/file/override catalog rows and
  pricing provenance. It does not fetch live prices.
- `GET /api/config` includes live policies and safe pricing metadata, never a
  Redis connection URL or credentials.

Policy and pricing arrays reload when the gateway config file changes. Invalid
config or catalog data leaves the previous live policies/catalog untouched.
Removing those arrays clears them. Relative `pricing.catalog_file` paths are
resolved beside the gateway config file and validated at load time. If only
that external file changes, touch the gateway config file or restart to reload
it. Changing Redis URL/prefix requires a restart.
