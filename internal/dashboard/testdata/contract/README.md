# Console endpoint fixtures

These are contract examples and development-harness data, not shipped console data. The frontend contains no sample numbers or credentials. Each method/path has its own response fixture; the server remains the authorization and scoping boundary. Arrays may be empty, nullable timestamps may be null, and optional existing fields may be omitted. The examples intentionally include denied/limited requests, a provider retry and fallback, hidden tools, near-budget usage, and automatic retention.

`logs_export.json` is a **CSV metadata sidecar**, not a JSON API response. Assert the content type, filename prefix, 10,000-row cap, and absence of payloads instead of JSON-decoding an export. `chat_completions.json` describes the blocking result; streaming uses the usual `data: <chat.completion.chunk>` records and `[DONE]`, with `x-omniswitch-trace-id` in the response header.

## Response field inventory

| Method and path | Fixture | Response fields |
|---|---|---|
| `GET /api/session` | `session.json` | `{authenticated, auth_enabled, auth_method, key_id, name, key_prefix, role, workspace_id, organization_id, expires_at, is_bootstrap, gateway: {version, commit, storage, listen, mcp_enabled, log_payloads}}` |
| `GET /api/health` | `health.json` | `{status, time}` |
| `GET /api/logs` | `logs.json` | `{logs: [{id, timestamp, trace_id, session_id, provider, model, api_key_id, status, input_tokens, output_tokens, total_tokens, cached_tokens, reasoning_tokens, latency_ms, cost, cost_source, endpoint, decision, decision_reason, cached, metadata}], total, limit, offset}` |
| `GET /api/logs/facets` | `logs_facets.json` | `{providers: [], models: [], statuses: [], endpoints: []}` |
| `GET /api/logs/export?format=csv` | `logs_export.json` | `{content_type, filename_prefix, max_rows, columns: []}` |
| `GET /api/metrics/timeseries?window=&bucket=` | `metrics_timeseries.json` | `{window, bucket, buckets: [{start, requests, allowed, denied, limited, errors, cache_hits, tokens, cost, avg_latency_ms}]}` |
| `GET /api/metrics/latency?window=` | `metrics_latency.json` | `{count, avg_ms, p50_ms, p95_ms, p99_ms}` |
| `GET /api/guardrails/events` | `guardrails_events.json` | `{events: [{id, timestamp, type, action, matched, details, request_id, trace_id, model, provider, api_key_id}], total, limit, offset}` |
| `GET /api/guardrails/summary` | `guardrails_summary.json` | `{matched, by_action: {deny, redact, warn, log, fallback, reroute, retry}, by_type: [{type, count}]}` |
| `GET /api/traces/{id}` | `traces.json` | `{trace_id, logs: [{id, timestamp, trace_id, session_id, provider, model, api_key_id, status, input_tokens, output_tokens, total_tokens, cached_tokens, reasoning_tokens, latency_ms, cost, cost_source, endpoint, decision, decision_reason, cached, metadata}], guardrail_events: [{id, timestamp, type, action, matched, details, request_id, trace_id, model, provider, api_key_id}], shadow_logs: [], feedback: [{id, trace_id, timestamp, score, rating, comment}]}` |
| `POST /api/feedback` | `feedback_create.json` | `{feedback: {id, trace_id, timestamp, score, rating, comment}}` |
| `GET /api/mcp/targets` | `mcp_targets.json` | `{enabled, targets: [{name, transport, enabled, connected, tool_count, visible, hidden, last_error, policy_path, token_exchange: {configured, token_url_host, audience}}]}` |
| `GET /api/mcp/tools?target=&as_key=` | `mcp_tools.json` | `{principal: {key_id, workspace_id, role}, tools: [{target, name, namespaced_name, description, list: {allowed, rule, reason}, call: {allowed, rule, reason, evaluated_without_arguments}}]}` |
| `GET /api/mcp/decisions` | `mcp_decisions.json` | `{decisions: [{timestamp, decision_id, agent_id, tool, action, target, decision, rule, reason, latency_ms, trace_id}], total}` |
| `GET /.well-known/agent-card.json` | `agent_card.json` | `{name, description, version, supportedInterfaces: [{url, protocolBinding, protocolVersion}], capabilities: {streaming, pushNotifications, extendedAgentCard}, defaultInputModes: [], defaultOutputModes: [], securitySchemes: {omniswitchBearer: {httpAuthSecurityScheme: {scheme}}}, security: [{omniswitchBearer: []}], skills: [{id, name, description, tags: [], inputModes: [], outputModes: []}]}` |
| `GET /api/providers/health` | `providers_health.json` | `{providers: [{name, type, circuit: {state, failures, threshold, open_until, last_failure}, requests, avg_latency_ms, error_rate, cost}]}` |
| `POST /api/providers/{name}/circuit/reset` | `providers_circuit_reset.json` | `{name, circuit: {state}}` |
| `GET /api/catalog` | `catalog.json` | `{verified_on, models: [{provider, id, input_per_million, output_per_million, source_url}]}` |
| `GET /api/keys` | `keys.json` | `{keys: [{id, name, key_prefix, workspace_id, role, created_at, rate_limit, budget_usd, monthly_cost_budget, monthly_token_budget, enabled, requests_per_minute, tokens_per_minute, usd_per_day, last_used_at}]}` |
| `POST /api/keys` | `keys_create.json` | `{key, api_key: {id, name, key_prefix, workspace_id, role, created_at, rate_limit, budget_usd, monthly_cost_budget, monthly_token_budget, enabled, requests_per_minute, tokens_per_minute, usd_per_day, last_used_at}}` |
| `PATCH /api/keys?id=` | `keys_update.json` | `{id, name, key_prefix, workspace_id, role, created_at, rate_limit, budget_usd, monthly_cost_budget, monthly_token_budget, enabled, requests_per_minute, tokens_per_minute, usd_per_day, last_used_at}` |
| `DELETE /api/keys?id=` | `keys_delete.json` | `{status}` |
| `GET /api/keys/usage?window=month` | `keys_usage.json` | `{period: {start, end}, keys: [{key_id, cost_usd, tokens, cost_budget, token_budget, cost_pct, token_pct, state}]}` |
| `GET /api/limits/usage?key_id=` | `limits_usage.json` | `{policies: [{name, scope, unit, limit, window, used, remaining, resets_in_seconds, source}]}` |
| `GET /api/virtual-keys` | `virtual_keys.json` | `{virtual_keys: [{id, name, provider_type, provider_name, created_at, updated_at, enabled}]}` |
| `POST /api/virtual-keys` | `virtual_keys_create.json` | `{id, name, provider_type, provider_name, created_at, updated_at, enabled}` |
| `POST /api/virtual-keys/rotate` | `virtual_keys_rotate.json` | `{status}` |
| `DELETE /api/virtual-keys?name=` | `virtual_keys_delete.json` | `{status}` |
| `GET /api/orgs` | `orgs.json` | `{organizations: [{id, name, created_at}]}` |
| `POST /api/orgs` | `orgs_create.json` | `{id, name, created_at}` |
| `GET /api/workspaces` | `workspaces.json` | `{workspaces: [{id, organization_id, name, created_at}]}` |
| `POST /api/workspaces` | `workspaces_create.json` | `{id, organization_id, name, created_at}` |
| `GET /api/users` | `users.json` | `{users: [{id, email, name, created_at}]}` |
| `POST /api/users` | `users_create.json` | `{id, email, name, created_at}` |
| `GET /api/workspace-members?workspace_id=` | `workspace_members.json` | `{members: [{workspace_id, user_id, role, created_at}]}` |
| `POST /api/workspace-members` | `workspace_members_create.json` | `{workspace_id, user_id, role, created_at}` |
| `GET /api/retention` | `retention.json` | `{policy: {request_logs, payloads, guardrail_events, shadow_requests, limited_and_denied}, sweep_interval_seconds, payload_logging, last_run: {started_at, finished_at, deleted: {request_logs, guardrail_events, shadow_requests, semantic_cache}, payloads_cleared, error}, next_run}` |
| `POST /api/retention/sweep` | `retention_sweep.json` | `{policy: {request_logs, payloads, guardrail_events, shadow_requests, limited_and_denied}, sweep_interval_seconds, payload_logging, last_run: {started_at, finished_at, deleted: {request_logs, guardrail_events, shadow_requests, semantic_cache}, payloads_cleared, error}, next_run}` |
| `GET /api/config` | `config.json` | `{version, commit, config_path, auth_enabled, gateway: {listen, auth_enabled, cache_threshold, cache_ttl, cache_scope, log_payloads, cors_origins: [], max_request_bytes, max_upload_bytes, read_header_timeout, read_timeout, write_timeout, idle_timeout, circuit_breaker_failures, circuit_breaker_cooldown, shadow_provider}, rate_limit: {requests, window, backend, fail_open, policies: [{name, scope, unit, limit, window, match, action}]}, identity: {api_keys_enabled, oidc: {enabled, issuer, audience, role_claim, workspace_claim, organization_claim, cache_ttl}}, guardrails: {stream_buffer, actions: {pii, injection, sql_injection, toxic, code_leakage}, rule_count, rules: [{name, stage, action}], tool_rule_count, tool_rules: [], webhook_count, webhooks: [], llm_judge_count, llm_judges: [], moderation_api}, providers: {names: [], model_count}, routes: [{model, provider, strategy, fallbacks: [], max_retries, retry_backoff, retry_codes: [], timeout, shadow_provider, default_params: {}, override_params: {}, drop_params: [], variants: []}], mcp: {enabled, targets: [{name, transport, enabled, policy_path, token_exchange: {configured, token_url_host, audience}}]}, a2a: {enabled, agent_card_path, jsonrpc_path, methods: [], streaming, streaming_mode, task_lifecycle, task_store, cancel, push_notifications}, retention: {request_logs, payloads, guardrail_events, shadow_requests, limited_and_denied}, pricing: {catalog_verified_on, catalog_file, overrides}, storage: {driver, schema_version, pending_migrations}, metrics: {public}}` |
| `GET /api/config/raw` | `config_raw.json` | `{path, format, body}` |
| `POST /api/config/raw` | `config_raw_save.json` | `{saved, path}` |
| `POST /api/config/validate` | `config_validate.json` | `{valid, errors: []}` |
| `GET /api/configs` | `configs.json` | `{configs: [{id, name, description, format, body, created_at, updated_at, enabled}]}` |
| `POST /api/configs` | `configs_create.json` | `{id, name, description, format, body, created_at, updated_at, enabled}` |
| `PATCH /api/configs?id=` | `configs_update.json` | `{enabled}` |
| `DELETE /api/configs?id=` | `configs_delete.json` | `{deleted}` |
| `GET /api/version` | `version.json` | `{version, commit, built}` |
| `GET /api/prompts` | `prompts.json` | `{prompts: [{id, name, version, template, variables: [], created_at, updated_at}]}` |
| `POST /api/prompts` | `prompts_create.json` | `{id, name, version, template, variables: [], created_at, updated_at}` |
| `GET /api/prompts/versions?name=` | `prompts_versions.json` | `{prompts: [{id, name, version, template, variables: [], created_at, updated_at}]}` |
| `POST /api/prompts/render` | `prompts_render.json` | `{rendered}` |
| `GET /v1/models` | `models.json` | `{object, data: [{id, object, created, owned_by}]}` |
| `POST /v1/chat/completions` | `chat_completions.json` | `{id, object, created, model, choices: [{index, message: {role, content}, finish_reason}], usage: {prompt_tokens, completion_tokens, total_tokens}}` |
| `POST /api/evals/policy` | `evals_policy.json` | `{total, allowed, denied, errors, results: [{index, request: {agent: {id, department, role}, tool: {name}, action: {name}, resource: {type, name, environment}, session: {id}, metadata: {}}, decision: {id, allowed, rule, reason, evaluation_time, trace: [{rule, expression, effect, matched}]}}]}` |

## Additional input and nullability contracts

- Logs filters: `window`, `since`, `until`, `provider`, `model`, `endpoint`, comma-separated `status`, `api_key_id` (admin), `trace_id`, `session_id`, `decision`, `cached`, `q`, `sort`, `limit`, `offset`. Export sends the same filters with `format=csv`, not payloads.
- `logs[].metadata` is stored JSON text. `_spans[]` carries `name`, optional `provider`, `start`, `duration_ms`, `status`, and optional `metadata` (`attempt`, `fallback`, `http_status`, `error`, `from`, `to`, `strategy`, `providers`, `reason`, guardrail details). The client also accepts an optional parsed top-level `spans` array; it never invents attempt timings.
- `session.expires_at`, provider circuit `open_until` / `last_failure`, key `last_used_at`, and retention `last_run` / `next_run` may be null. `workspace_id`, `organization_id`, and provider credentials `base_url` may be empty or omitted according to the existing structs.
- Key create/patch inputs: `name`, `role`, `workspace_id`, `enabled` (patch), `rate_limit`, `budget_usd`, `monthly_cost_budget`, `monthly_token_budget`, `expires_in` (create), `requests_per_minute`, `tokens_per_minute`, `usd_per_day`. Zero budgets and per-key limits mean unlimited. Creation alone returns `key`; the UI destroys it when its reveal dialog closes.
- Config validation/raw-save: `{format, body}`. Validation may return `{valid:false, errors:[{line, message}]}` (or a standard 400 error envelope). Saved-config create: `{id?, name, description, format, body, enabled}`; patch only toggles `enabled`.
- Policy replay: `{policies:[{name,body}], baseline_policies?:[{name,body}], requests:[ToolRequest]}`. Uploaded records, not request metadata, supply arguments and context. The console computes decision diffs with separate calls if no backend baseline extension is present. Never accept a browser-selected arbitrary server path.
- Prompt create: `{name, template}`; render: `{prompt_id, variables}`. Versions return the same `prompts` shape. Provider credentials create/rotate take masked `provider_key` inputs and never echo them.
- Feedback: `{trace_id, score, rating, comment}`; scores are -1, 0, 1. Workspaces/orgs/users/memberships use the existing create structs. No delete control exists for those resources.
- New empty arrays remain honest empty states. `cost_source=none` displays **Unpriced**, not $0. A2A qualifiers come from config, and published capabilities from the actual agent card; task state is in memory, cancellation is best effort, and streaming means SSE task events.

## Call budgets and polling

Overview uses five data calls per render/poll: timeseries, latency, guardrail summary, provider health, and recent logs. Aggregate cards come from the buckets rather than a sixth metrics call. The boot identity check is separate. Other views make at most five initial data calls; secondary admin tabs fetch only when opened. Only Overview and Providers poll, every 15 seconds while visible.
