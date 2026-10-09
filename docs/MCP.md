# MCP gateway

Enable `mcp.enabled` and configure `mcp.targets` to expose remote MCP servers at
`POST /mcp`. HTTP/streamable HTTP and stdio transports share policy enforcement.
Tools appear as `target__tool`; calls to that name go to the named target.
OpenAPI-derived tools use the same discovery policy.

## Policy semantics

- `action: list` controls visibility in `tools/list`.
- `action: call` controls invocation in `tools/call`.
- `match.tool` uses the bare upstream tool name, with the `target__` prefix
  removed. For example, `ops__delete_repo` matches `delete_repo`, and
  `ops__github.delete_repo` matches `github.delete_repo`.
- Call checks still receive the supplied arguments as policy metadata and
  inferred resource attributes. Identity comes from gateway authentication.
- A hidden tool can still be called by name, but the call policy is evaluated.
  Hiding a tool is not a substitute for denying its invocation. A visible tool
  can be denied at call time based on arguments or authenticated identity.

Existing namespace/inferred-action policies remain enforced on calls as a
compatibility check. For example, a policy matching `tool: github` and
`action: delete` continues protecting `github.delete_repo`. New MCP policies
should use the bare name and `action: call` for invocation.

### Hide a destructive tool

```yaml
apiVersion: omniswitch.dev/v1
kind: Policy
metadata:
  name: hide-delete-repo
spec:
  match:
    tool: delete_repo
    action: list
  effect: deny
  reason: This tool is hidden from discovery.
```

For a broader pattern, use `policies/hide-destructive-tools.yaml`. Attach the
policy to the target's `policy` field. To prohibit calls as well, use a separate
call rule or a CEL rule matching both `list` and `call`.

## Discovery filtering and cache

Every upstream tool is evaluated against its target's policy engine before it
is returned. Policy evaluation errors hide the affected tool. Each target
listing emits one audit event with `metadata.hidden_tools`, including a zero
count when no tools are hidden; it does not emit an event per tool.

Upstream `tools/list` results are cached for 30 seconds per target. Filtering is
re-evaluated for each caller, including cache hits. Sessions, pagination
parameters, and delegated bearer credentials partition the cache. Closing the
MCP handler invalidates cached discovery results.

See [Configuration](CONFIGURATION.md#mcp-targets) for target fields and runtime
settings.
