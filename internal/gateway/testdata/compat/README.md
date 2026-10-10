# Reference-derived compatibility fixtures

These are synthetic examples authored from the public API references, not live
model captures. Each JSON fixture records its source URL and the date checked.

- `request_*`: API request and the complete expected canonical chat request.
- `response_*`: canonical chat response and the complete expected API response.
- `stream_*`: canonical chat SSE payloads (including the `[DONE]` sentinel) and
  the complete expected named event list. Usage follows the finish chunk on
  purpose; completion must not discard that final usage-only chunk.

The interleaved-tool fixtures allocate output/block indices as calls or text
first appear, not by raw upstream tool index. They cover two independent calls,
fragmented arguments, text after tools, and completed empty JSON input.

References checked 2026-10-09:

- https://platform.openai.com/docs/api-reference/responses/create
- https://platform.openai.com/docs/api-reference/responses-streaming
- https://platform.openai.com/docs/guides/function-calling?api-mode=responses
- https://platform.claude.com/docs/en/api/messages
- https://platform.claude.com/docs/en/build-with-claude/streaming
- https://platform.claude.com/docs/en/api/errors

## Official SDK helpers

`mock_upstream.py` serves a local OpenAI-compatible two-turn tool upstream.
`sdk_check.py` uses official OpenAI/Anthropic clients and their stream accumulators.
`run_sdk_checks.sh` builds/starts an authenticated gateway and the mock, runs the
checks, and cleans up both processes. Set `COMPAT_SDK_PYTHON` to a virtual
environment interpreter containing the SDKs. See `docs/API_COMPAT.md` for pins,
results, and the supported subset.
