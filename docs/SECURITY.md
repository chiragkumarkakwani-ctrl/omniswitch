# Security and gateway API keys

See [the security reporting policy](../SECURITY.md) for private vulnerability
reports and [configuration](CONFIGURATION.md) for authentication, cache isolation,
logging, and policy settings. The gateway is self-hosted; operators must set up
TLS, restrict management access, and protect the data and policy directories.

## Key format and storage

Keys created by `POST /api/keys` use `sk-osw-` followed by **64 lowercase hex
characters**, from 32 cryptographically random bytes. The stored display prefix
is `sk-osw-` plus the first 6 hex characters and a Unicode ellipsis (`…`). The
raw key is returned at creation; subsequent key listings do not return it.

Authentication looks up a SHA-256 hash of the **entire bearer token**, not its
prefix. Existing `sk-sentinel-` keys keep working with their existing hashes;
no reissue or database rewrite is needed. Revoked/disabled and expired keys
remain rejected regardless of prefix. Prefixes are display/identification
conventions, not authentication credentials.

Keep keys in a secret manager or protected environment, not in source files,
URLs, browser-local storage, or logs. Use separate keys per workload and the
least-privileged role/workspace. Rotate exposed keys and review their usage.
Bootstrap keys are operator-provided credentials; production deployments
should explicitly enable authentication and provide a protected owner/admin
bootstrap key or configure OIDC.

## Secret-scanning patterns

Scan for **both** current and legacy generated credentials:

```regex
\bsk-osw-[0-9a-f]{64}\b
\bsk-sentinel-[0-9a-f]{64}\b
```

The repository's [`.gitleaks.toml`](../.gitleaks.toml) contains the gateway key
rules used by release CI. These patterns describe generated keys; separately
scan operator-provided bootstrap credentials and upstream provider secrets.
If a scan detects an actual key, revoke/rotate it rather than just deleting
its text from the current commit. Do not paste secret values into reports.

## Replay and logging boundaries

Policy replay accepts named files confined to `gateway.policy_dir`, or inline
policy bodies, not arbitrary server paths. Both candidate and optional baseline
policies are evaluated on the server. Request bodies are capped at 1 MiB and
batches at 1,000 requests. Keep the policy directory operator-writable only.

Payload logging defaults to off. If enabled, stored requests/responses may
contain personal data or credentials. Restrict data access and apply the
retention rules appropriate for your deployment. A lexical similarity cache
is not an embedding-based cache; keep tenant scoping enabled to avoid sharing
responses between callers.
