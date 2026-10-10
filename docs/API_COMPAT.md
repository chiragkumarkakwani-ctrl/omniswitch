# Agent API compatibility

OmniSwitch translates **POST /v1/responses** and **POST /v1/messages** into the
normal chat pipeline. Guardrails, client tool rules, routing and fallbacks,
cache isolation, budgets, and request logging remain in that pipeline. These
are stateless API translations, not a hosted conversation store. Clients must
resend conversation history and execute their own tools.

## Responses: supported request fields

| Field | Translation |
| --- | --- |
| `model`, `input` | Required model; input is a string or nonempty message/history item array. |
| Message items | `user`, `assistant`, `system`, or `developer` role; string content or content arrays. |
| `input_text`, `output_text` | Chat text; `output_text` lets clients replay assistant output items. |
| `input_image` | `image_url` (URL or data URI) and optional `detail` become a chat image part. No `file_id` lookup. |
| `instructions` | Prepended system message. |
| `function_call` history | `call_id`, `name`, and argument string become assistant tool calls. Adjacent calls share an assistant message. |
| `function_call_output` history | `call_id` becomes `tool_call_id`; string or text/image content becomes a tool message. |
| `tools` | Function tools: `name`, `description`, `parameters`, and `strict`. Other Responses tool kinds return 400. |
| `tool_choice` | `auto`, `none`, `required`, or `{type:"function", name:...}`. |
| `parallel_tool_calls` | Passed through as the chat parallel-tool option. |
| `max_output_tokens` | Positive value becomes `max_completion_tokens`. |
| `temperature`, `top_p` | Chat sampling fields. |
| `text.format` | `text`, `json_object`, or `json_schema` with `name`, `schema`, optional `description` and `strict`. |
| `reasoning.effort` | Chat `reasoning_effort`; adapter/model support still applies. |
| `metadata`, `user`, `stream` | Chat metadata, user, and streaming options. Streams request final usage. |
| `store` | Accepted but ignored; response reports `store:false`, and the chat request also sets `store:false`. |
| Unknown top-level fields | Preserved as chat passthrough fields; an upstream may reject fields it cannot support. |

`previous_response_id` (except null) returns HTTP **400** with
`error.code:"unsupported_parameter"` and `error.param:"previous_response_id"`.
The error explains that clients must resend history. Item references, file
lookups, hosted tools, background jobs, and other stateful Responses operations
are not implemented.

Blocking results include the response envelope and request options, `resp_`
IDs, status, output items, error/incomplete details, and usage. Text becomes an
assistant message; each tool call becomes a `function_call` with its original
`call_id` and complete argument string. A token-limit finish produces
`status:"incomplete"` and `incomplete_details.reason:"max_output_tokens"`.
Only the first completion choice is represented.

### Responses streaming

Named SSE events include increasing `sequence_number` values starting at zero:

1. `response.created`, `response.in_progress`.
2. `response.output_item.added` for each message or function call.
3. For text: `response.content_part.added`, `response.output_text.delta`,
   `response.output_text.done`, and `response.content_part.done`.
4. For tools: `response.function_call_arguments.delta` and
   `response.function_call_arguments.done`.
5. `response.output_item.done`, then `response.completed` or
   `response.incomplete`. Translation failures after headers produce
   `response.failed` instead of a success event.

Tool calls and text use stable output indices even when upstream calls
interleave. Completion waits for the upstream chat `[DONE]`, so a usage-only
chunk arriving after the finish chunk is not discarded. The Responses SSE
wire format itself does **not** include a `[DONE]` sentinel.

## Messages: supported request fields

| Field | Translation |
| --- | --- |
| `model`, `max_tokens`, `messages` | Required; positive output limit, nonempty user/assistant message array. |
| `system` | String or text blocks, joined as a system message. |
| Text content | String or `text` blocks. |
| Image blocks | `source.type:"url"`, or `base64` with `media_type` and `data`; converted to chat image parts. |
| Assistant `tool_use` | `id`, `name`, and object `input` become assistant tool calls. Text may coexist with calls. |
| User `tool_result` | `tool_use_id` and string/text-block content become tool messages, before other user content. `is_error:true` prefixes content with `[error] `. |
| `tools` | Client tools with `name`, optional `description`, and `input_schema`. |
| `tool_choice` | `auto`, `any` (chat `required`), or `tool` with `name`. `disable_parallel_tool_use` is inverted into `parallel_tool_calls`. |
| `stop_sequences` | Chat stop strings, also forwarded by the native Anthropic adapter. |
| `temperature`, `top_p` | Chat sampling fields, also forwarded by the native Anthropic adapter. |
| `top_k` | Only applied by the native Anthropic adapter, including after routing/fallback. Never forwarded to OpenAI-compatible upstreams. The `x-omniswitch-warnings` header explains this restriction. Cache entries remain distinct across values. |
| `metadata.user_id` | Chat `user`; native Anthropic metadata user ID. |
| `stream` | Messages SSE with final upstream usage requested. |

The official Anthropic SDK can authenticate with `x-api-key`; explicit
`Authorization` takes precedence. Middleware errors, including authentication
and rate-limit failures before the endpoint handler, use the Anthropic envelope:

```json
{"type":"error","error":{"type":"api_error","message":"..."}}
```

HTTP statuses remain those of the normal chat path. Error types reflect the
status: authentication, permission, rate-limit, invalid-request, API, or
upstream-overload error. Malformed upstream tool arguments return **502
`api_error`**, never a fabricated tool input. Arguments must decode to a JSON
object; empty, invalid, null, array, or scalar arguments fail. `{}` is valid.

Blocking results include `msg_` IDs, assistant role, model, text and `tool_use`
blocks, stop reason/sequence, and input/output usage. Finish reasons map to
`end_turn`, `max_tokens`, `tool_use`, or `stop_sequence`. The actual matched
stop sequence is unavailable in the chat model; it remains null rather than
being guessed.

### Messages streaming

The stream sends `message_start`, `content_block_start`,
`content_block_delta`, `content_block_stop`, `message_delta`, and
`message_stop`. Text deltas use `text_delta`; tool arguments use
`input_json_delta` with `partial_json` fragments. Blocks start in contiguous
index order even when arguments for a later parallel call arrive first.
Final `message_delta.usage` carries cumulative input/output totals.

The native Anthropic adapter maps content-block indices to dense chat tool
indices, so text before or between tools cannot create empty, phantom calls.
Initial tool input is used only when the upstream sends no JSON argument deltas;
explicit `{}` is preserved, and missing or malformed input is not replaced.

Idle, open streams send `ping` events every 15 seconds. Pings begin only after
the chat pipeline approves SSE: they do not prematurely turn a buffered
guardrail denial or upstream failure into HTTP 200. The writer implements
`http.Flusher`, preserves partial SSE writes, and stops its keepalive timer
when the request completes.

The default guardrail-buffered mode validates complete tool arguments before
sending headers and can return HTTP 502. If operators disable stream buffering,
HTTP status cannot change after headers: a late malformed argument produces an
SSE `error` containing `api_error`, without a successful `message_stop`.

## Usage and other boundaries

- The current foundation `provider.Usage` exposes prompt, completion, and total
  tokens only. Responses cached/reasoning detail counters and Messages cache
  read/creation counters currently report zero, meaning **detail unavailable**,
  not a measured zero. No such provider fields are invented by this package.
- **P2b integration:** connect its real cached/reasoning fields in
  `internal/compat/responseUsage` and `messageUsage`. Cache-creation tokens must
  stay unavailable until an adapter actually supplies that separate count.
- Cached and redacted streams retain tool calls, exact text whitespace, and
  final usage; they are synthetic playback rather than live token generation.
- Strict tool/schema options reach OpenAI-compatible adapters, but schema
  enforcement and reasoning support remain provider/model dependent. The
  gateway validates Messages argument JSON shape, not arbitrary tool schemas.
- Extended thinking, provider cache-control blocks, server tools, document/file
  blocks, and other Messages-specific features are not translated. Unknown
  top-level Messages fields are not forwarded. Unsupported content-block and
  tool-choice kinds return 400.
- Translation tests use synthetic, reference-derived fixtures. SDK checks use
  a local mock upstream, not billable live models or a guarantee about every
  model/provider combination.

## Official SDK checks

Checked **2026-10-09**, Python **3.12.3**, Go **1.24.13**:

| Official SDK | API | Mode | Result |
| --- | --- | --- | --- |
| `openai==3.28.0` | Responses | Blocking | PASS: two-turn conversation, two function calls and outputs, final answer. |
| `openai==3.28.0` | Responses | Streaming | PASS: official `responses.stream()` accumulation, argument deltas/done, ordered sequence numbers, final answer. |
| `anthropic==1.13.0` | Messages | Blocking | PASS: two-turn conversation, two tool-use/results, final answer. |
| `anthropic==1.13.0` | Messages | Streaming | PASS: official `messages.stream()` accumulation, interleaved parallel JSON input deltas, final answer. |

All four flows checked IDs, complete tool arguments, stop/status fields, and
final usage (11 input, 7 output; Responses total 18). The gateway ran with auth
enabled and guardrail buffering enabled; the upstream was OpenAI-compatible.
Additional official SDK checks passed for stateless-history rejection (400)
and malformed Messages tool arguments (502 in blocking and buffered streaming).
Anthropic 1.13.0 no longer has a `top_k` keyword argument, so that request field
was sent through the official SDK's `extra_body` option. Every started gateway
and mock process was stopped after the checks.

### Repeat locally

The test helpers are under `internal/gateway/testdata/compat`. Create a virtual
environment outside the checkout and install the pinned SDKs there:

```sh
venv_dir=$(mktemp -d /var/tmp/osw-sdk-venv.XXXXXX)
python3 -m venv "$venv_dir"
"$venv_dir/bin/python" -m pip install openai==3.28.0 anthropic==1.13.0
COMPAT_SDK_PYTHON="$venv_dir/bin/python" \
  bash internal/gateway/testdata/compat/run_sdk_checks.sh
```

The runner builds the gateway, creates disposable authenticated config/data,
starts the gateway and mock on loopback ports 18150/18151, runs SDK assertions,
and stops both processes and removes its temporary data with an exit trap.
`COMPAT_GATEWAY_PORT` and `COMPAT_UPSTREAM_PORT` can override those two ports.
No production credential or external model is required.

For a Python distribution without `ensurepip`, install its venv package, or
create the venv with `--without-pip` and bootstrap SDKs using the host pip's
`--python "$venv_dir/bin/python"` option. Do not install these checks' packages
into the system Python.

## Reference sources

Public references checked on 2026-10-09:

- https://platform.openai.com/docs/api-reference/responses/create
- https://developers.openai.com/api/reference/resources/responses/streaming-events
- https://platform.openai.com/docs/guides/function-calling?api-mode=responses
- https://platform.claude.com/docs/en/api/messages
- https://platform.claude.com/docs/en/build-with-claude/streaming
- https://platform.claude.com/docs/en/api/errors

Each request/result/SSE fixture records its public source and verification date.
Pure table/golden tests live in `internal/compat`; mock-upstream golden and
two-turn acceptance tests also exercise the real handlers in `internal/gateway`.
Native Anthropic HTTP regressions additionally cover text followed by two tools,
including empty input, through both APIs with buffering enabled and disabled.
