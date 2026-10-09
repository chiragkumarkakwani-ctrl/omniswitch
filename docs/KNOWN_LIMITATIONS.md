# Known Limitations

OmniSwitch is in public beta. This page lists limitations that are current, known, and intentional (either not yet built, or a deliberate scope decision) so you can decide whether they matter for your use case. If you hit something not listed here, please [file a bug](https://github.com/omniswitch-dev/omniswitch/issues/new/choose) — it may be a real defect rather than a documented limitation.

## Control plane storage

- The default and fully supported storage is **single-replica SQLite**. Running more than one gateway replica against the same SQLite file is not supported.
- A **PostgreSQL storage driver exists but is experimental**: it is not yet the multi-writer, horizontally-scaled control plane described in the roadmap. Treat it as suitable for evaluation, not as a production multi-replica setup, until it graduates out of experimental status.

## API surface

- **Responses API and Anthropic Messages API support a documented subset**, not full parity with the upstream vendor APIs. Structured outputs, some tool-use flows, and certain streaming edge cases may not be forwarded; unsupported fields are either translated on a best-effort basis or rejected rather than silently dropped where practical (see `CHANGELOG.md` for the passthrough-fidelity work).
- **A2A tasks are in memory** and lost on restart; they are not shared across replicas. `SendMessageStream` sends SSE task status and artifact events, not token-by-token output. `GetTask` provides lookup, and **`CancelTask` is best effort**: it changes task state but does not abort an upstream inference call, whose completion can supersede that state. **Push notifications are not supported**, nor is an outbound A2A client or downstream agent proxy.

## Identity and transport security

- **No mTLS.** Client/server mutual TLS is not implemented; use a reverse proxy or service mesh in front of OmniSwitch if you need it.
- **No SAML/SCIM.** Identity is API-key based or OIDC/JWKS-based; there is no SAML SSO or SCIM provisioning integration.

## Guardrails and streaming

- **`guardrails.stream_buffer` delays the first streamed token by default.** When guardrails are enabled on a streaming response, OmniSwitch buffers output to scan it before forwarding, which adds latency to time-to-first-token compared to guardrails-off streaming. This is a deliberate safety/latency tradeoff; disable stream buffering only if you understand the guardrail is then scanning already-sent output.

## Caching

- **Lexical similarity cache uses token-frequency cosine, not embeddings.** Lowercased, stop-word-filtered token counts form normalized vectors. No embedding model is called; paraphrases can be missed, and superficial token overlap can match despite different meanings. Do not treat it as embedding-quality matching.

## Authorization

- **External HTTP authorization is not supported.** The unused callout config type was removed; legacy external-callout fields now fail strict parsing as unknown fields. Use API keys or OIDC/JWKS plus CEL policy authorization instead.

## Reporting something not listed here

If you find a gap not covered above, it's either a bug (please file one) or something we haven't documented yet (please file a docs issue) — either way, we'd rather hear about it than have it stay a silent surprise.
