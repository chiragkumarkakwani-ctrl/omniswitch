# Command Line

The `omniswitch` binary is both the gateway server and its own CLI. Running
it with no arguments is equivalent to `omniswitch serve`.

```
Usage:
  omniswitch [command]

Commands:
  serve             Start the gateway server (default when no command is given)
  validate-config   Validate a gateway config file and exit (defaults to $OMNISWITCH_CONFIG)
  migrate           Inspect or apply schema migrations (status, up [--dry-run])
  version           Print version information
  help              Show this help text

Flags:
  -h, --help        Show this help text
  -v, --version     Print version information
```

None of `--help`, `--version`, `validate-config`, or `migrate` start the HTTP server or
bind a port, so they are safe to run in scripts, health checks, or CI even
while a gateway instance is already listening on the configured address.

## `omniswitch serve`

Starts the gateway. Configuration comes from environment variables and,
optionally, a config file pointed to by `OMNISWITCH_CONFIG` (see
[`docs/CONFIGURATION.md`](CONFIGURATION.md)). At minimum, set one provider
API key:

```bash
OPENAI_API_KEY=sk-... omniswitch serve
```

The process starts listening on `OMNISWITCH_LISTEN` (default `:8080`) and
runs until it receives `SIGINT` or `SIGTERM`, at which point it stops
accepting new connections, drains in-flight requests (including open SSE
streams) for up to `OMNISWITCH_SHUTDOWN_TIMEOUT` (default `30s`), and exits
`0`.

## `omniswitch validate-config [path]`

Loads and validates a gateway config file without starting the server.
Useful in CI or a pre-deploy step to catch a bad config before a rollout.

```bash
omniswitch validate-config examples/gateway-config.yaml
# OK: examples/gateway-config.yaml

omniswitch validate-config ./broken.yaml
# invalid config ./broken.yaml: <reason>
# exit code 1
```

If no path is given, it falls back to `OMNISWITCH_CONFIG`. If neither is set,
it prints a usage error and exits `2`.

## `omniswitch migrate status`

Prints the storage driver, current schema version, latest version, and number of
pending migrations. It never creates or alters schema tables, even when startup
migrations are enabled. Example output:

```text
Driver: sqlite
Current version: 0
Latest version: 4
Pending migrations: 4
```

## `omniswitch migrate up [--dry-run]`

Applies pending migrations in order using the same transactional migrator as
`serve`. `--dry-run` lists their version numbers and names without applying them
or creating the migration ledger. Repeated `up` calls are idempotent. Schema
version 4 adds retention indexes, including partial indexes for payload-bearing
rows and independently retained log classes. Existing row data is unchanged.

```bash
omniswitch migrate status
omniswitch migrate up --dry-run
omniswitch migrate up
```

Both commands load `$OMNISWITCH_CONFIG` and use the same storage settings as
`serve`: `OMNISWITCH_DATA` overrides `gateway.data_dir`, and
`OMNISWITCH_DATABASE_URL` overrides `gateway.storage_url`. A `postgres://` or
`postgresql://` URL selects PostgreSQL; otherwise SQLite uses `omniswitch.db`
in the data directory. Commands do not start the gateway or bind its HTTP port.
Opening the store can move legacy `sentinel.db` / `sentinel_cache.db` filenames
and their WAL/SHM sidecars, even for status or dry-run; this does not change the
schema. Existing new filenames are never overwritten.

**Before upgrading, stop old gateway processes and back up the SQLite database
and sidecars, or run `pg_dump` for PostgreSQL.** For controlled rollouts, set
`gateway.migrate_on_start: false` (or `OMNISWITCH_MIGRATE_ON_START=false`), run
`migrate status` / `migrate up --dry-run`, and then explicitly run `migrate up`.
Readiness stays HTTP 503 while migrations are pending; restart afterward if the
gateway deferred bootstrap-key initialization while the schema was pending.

Retention is independent of migrations and enabled by default. The first sweep
runs after one hour: request logs older than 30 days and logged payloads older
than 7 days are removed unless the respective retention policies are `0`.
See [configuration](CONFIGURATION.md#data-retention) for all five policies and
the admin-only manual sweep endpoint.

## `omniswitch version` / `omniswitch --version` / `omniswitch -v`

Prints the build version, commit, and build date, then exits `0`. Useful for
confirming what an installer or Docker image shipped:

```bash
omniswitch version
# omniswitch version v0.2.0-beta (commit abcdef1, built 2026-09-11T00:00:00Z)
```

## `omniswitch help` / `omniswitch --help` / `omniswitch -h`

Prints the usage text above and exits `0`.

## Unknown commands

Any other first argument prints an error to stderr followed by the usage
text, and exits `1`.

## Health endpoints

These are HTTP endpoints on the running gateway, not CLI subcommands, but
they are the operational counterpart to this CLI and are documented here for
discoverability:

- `GET /healthz` — liveness. Always returns `200 {"status":"ok"}` once the
  process is up; it does not touch the store or provider registry. Point a
  process supervisor's liveness probe here.
- `GET /readyz` — readiness. Returns `200 {"status":"ready"}` when the store
  is reachable and at least one provider is registered (or
  `OMNISWITCH_ALLOW_NO_PROVIDERS=true` is set), otherwise `503
  {"status":"not ready","reasons":[...]}`. Point a load balancer's readiness
  probe here.

Both endpoints are always unauthenticated and bypass `gateway.auth`, rate
limiting, and CORS, so they are safe for infrastructure health checks that
cannot present an API key.

## The policy CLI (`omniswitch-policy`)

A separate binary, `omniswitch-policy`, ships alongside `omniswitch` in
release archives and Homebrew. It manages MCP tool-call policy files (the CEL
rules read by `mcp.policy` / `OMNISWITCH_MCP_POLICY`) independently of the
gateway process. Run `omniswitch-policy --help` for its own usage.
