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

## RFC 8693 token exchange (beta)

A target can exchange the gateway's authenticated OIDC bearer token for a
short-lived downstream token. The authorization server must support RFC 8693
and the requested scope/audience/resource. Configure OIDC authentication and an
OAuth client that is permitted to exchange these subject tokens. Confidential
clients use `client_secret_post`. Optional actor tokens support delegated
on-behalf-of deployments.

### Worked example

Inject `MCP_CLIENT_SECRET` and, if using the actor fields below, `MCP_ACTOR_TOKEN`
through your deployment's secret manager. These values are not stored in the
YAML. Remove the actor fields when your authorization server does not need them.

```yaml
apiVersion: omniswitch.dev/v1
kind: GatewayConfig
gateway:
  auth: true
identity:
  oidc:
    jwks_url: https://auth.example.com/.well-known/jwks.json
    issuer: https://auth.example.com
    audience: omniswitch
mcp:
  enabled: true
  targets:
    - name: ops
      upstream: https://mcp.example.com/mcp
      policy: policies/hide-destructive-tools.yaml
      token_exchange:
        token_url: https://auth.example.com/oauth/token
        client_id: omniswitch-gateway
        client_secret_env: MCP_CLIENT_SECRET
        subject_token_type: urn:ietf:params:oauth:token-type:jwt
        requested_token_type: urn:ietf:params:oauth:token-type:access_token
        scope: tools.read tools.invoke
        audience: mcp-ops
        resource: https://mcp.example.com
        actor_token_env: MCP_ACTOR_TOKEN
        actor_token_type: urn:ietf:params:oauth:token-type:jwt
```

Send an OIDC bearer token accepted by the gateway to `POST /mcp`, listing tools
or calling a target-prefixed name such as `ops__search`. The gateway posts an
`application/x-www-form-urlencoded` exchange with
`grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, the validated
`subject_token`, configured type fields, client credentials, and optional
scope/audience/resource/actor fields. It then sends
`Authorization: Bearer <exchanged token>` to the target. The subject token is
not forwarded to that target.

Token URLs require HTTPS; localhost and loopback addresses permit HTTP for
local testing. Exchange cannot be combined with `forward_bearer_token`, and
requires an HTTP/streamable HTTP target. Gateway API keys are not exchanged;
requests without a verified OIDC subject fail closed. The outgoing exchange
uses the request's cancellation context, has a ten-second timeout, and does
not follow token-endpoint redirects.

Exchanged tokens are cached per target and subject until their declared expiry,
with a safety margin. Responses without a positive `expires_in` are not cached.
Closing the handler clears the token cache. Client secrets and actor tokens are
resolved from named environment variables; literal `client_secret` or
`actor_token` configuration fields are rejected. Exchange errors do not include
authorization-server response bodies or credential-bearing URLs.

The `/api/config` target summary exposes
`token_exchange: {configured, token_url_host, audience}`. It does not expose
client/actor credentials, subject tokens, exchanged tokens, or the full token
URL. Configured does not imply that the authorization server has accepted a
request. MCP target changes take effect after a gateway restart.

See [Configuration](CONFIGURATION.md#mcp-targets) for every target field and
runtime setting.
