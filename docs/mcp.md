# MCP Server

Hecatoncheires can expose a read-only [Model Context Protocol](https://modelcontextprotocol.io/)
(MCP) endpoint so that AI clients (Claude and other MCP-capable hosts) can read
Workspaces, Cases, and Actions. The endpoint is served over **Streamable HTTP**
on the same HTTP server as the GraphQL API and Slack webhooks, and every tool
call is authorized using a verified Slack identity or a Rego policy.

## Enabling

The endpoint is disabled by default. For user access, configure `[mcp]` in a
deployment-wide TOML file and enable the endpoint with `--mcp`:

```toml
[mcp]
identity_header = "X-User-ID-Token"
google_audience = "example.apps.googleusercontent.com"
```

```bash
hecatoncheires serve --mcp --global-config ./global.toml
```

Set `google_audience` to the OAuth client ID used to issue ID tokens for the MCP
service. The client must send the user's Google ID token on every request in
`identity_header`, as a raw JWT or as `Bearer <JWT>`. Header names are
case-insensitive. The server checks Google's signature, issuer, audience,
expiration, and verified email. It accepts Gmail and Google Workspace email
addresses that Google can vouch for. See [Google's ID token verification guide](https://developers.google.com/identity/gsi/web/guides/verify-google-id-token).

The verified email must exactly match one record in the synced Slack user
directory. Enable Slack user synchronization with the `users:read` and
`users:read.email` scopes and keep the directory current. A missing or ambiguous
match is denied. The Slack user then gets the same workspace access as in the
Web UI. Workspace listings are filtered and all Case and Action tools check the
target workspace before reading it.

To retain policy-only access for existing integrations, omit `[mcp]` and
supply at least one Rego policy with `--policy`:

```bash
hecatoncheires serve \
  --mcp \
  --policy ./policies/mcp.rego \
  --mcp-env MCP_TOKEN
```

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--mcp` | `HECATONCHEIRES_MCP` | `false` | Enable the `/mcp` endpoint |
| `--policy` | `HECATONCHEIRES_POLICY` | - | Rego policy file(s) or directory(ies). Repeatable. Required for policy-only access |
| `--mcp-env` | `HECATONCHEIRES_MCP_ENV` | - | Names of environment variables exposed to the policy as `input.env` (allow-list). Repeatable |

When `--mcp` is set, `serve` requires a valid `[mcp]` section or `--policy`.
An invalid `[mcp]` section fails startup even when a policy is supplied.

The endpoint is mounted at `POST/GET /mcp`. Point your MCP client's
Streamable HTTP transport at `https://your-host/mcp`.

## Tools

All tools are read-only and prefixed with `hecaton_` so they stay namespaced in
a client that aggregates several MCP servers.

| Tool | Input | Returns |
|------|-------|---------|
| `hecaton_list_workspaces` | _(none)_ | Accessible workspaces with details: `id`, `name`, `description`, `emoji`, `color`, `case_mode`, `action_statuses`, `case_statuses`, `field_schema` (each field: `id`, `name`, `type`, and — when the field sets one — `semantic` plus `semantic_hint`, an English description of the expected value; when the field sets `validation.pattern`, `pattern` plus `pattern_hint`, which states that the whole value must match the pattern) |
| `hecaton_list_cases` | `workspace_id` (required), `status` (optional: `DRAFT`/`OPEN`/`CLOSED`) | Case summaries (`id`, `title`, `status`, `board_status`, `reporter_id`, `assignee_ids`, `created_at`, `updated_at`) |
| `hecaton_get_cases` | `workspace_id` (required), `ids` (required, `[]int`) | Full case details (summary fields plus `description`, `slack_channel_id`, `slack_thread_ts`, `field_values`, `agent_source_ids`) |
| `hecaton_list_actions` | `workspace_id` (required), `case_id` (optional), `include_archived` (optional `bool`) | Action details (`id`, `case_id`, `title`, `description`, `assignee_id`, `status`, `due_date`, `archived_at`, `slack_message_ts`, timestamps) |
| `hecaton_get_actions` | `workspace_id` (required), `ids` (required, `[]int`) | Action details |

### Private cases are never exposed

Private Cases — and **every Action beneath them** — are never returned over
MCP, regardless of channel membership. This is stricter than the Web/Slack
behaviour (where a member can view a private Case). Specifically:

- `hecaton_list_cases` omits private Cases entirely.
- `hecaton_get_cases` silently omits any requested private Case (and any
  non-existent ID), so the response never reveals that such a Case exists.
- `hecaton_list_actions` omits Actions whose parent Case is private; passing a
  private `case_id` returns an empty list.
- `hecaton_get_actions` silently omits Actions whose parent Case is private.

### Archived cases are not listed

`hecaton_list_cases` returns only the active archive slice. A Case that has been
archived (see [Archiving a Case](./user_guide.md#archiving-a-case)) is left out
regardless of the `status` argument, and the tool exposes no argument to ask for
it — the read surface stays what the team currently works on.

`hecaton_get_cases` is unaffected: fetching a known id still returns the Case,
with `archived_at` absent from the summary fields. Use the GraphQL
`cases(filter: ARCHIVED)` query when an archived Case has to be enumerated.

## Authorization (Rego)

When `--policy` is supplied, tool calls must pass the Rego entrypoint
**`data.auth.mcp`**. The policy receives an `input` document and must return an
object with an `allow` boolean and an optional `user` string.

In verified identity mode, the MCP policy can further restrict calls that pass
the workspace's [`[authz]` policy](./configuration.md#authorization-section-authz).
In policy-only mode, `data.auth.mcp` determines access for the integration.
Use `input.tool.workspace_id` to restrict workspace-scoped calls and gate
`hecaton_list_workspaces` separately if the client should not list all workspaces.

### Policy input

```json
{
  "req": {
    "method": "POST",
    "path": "/mcp",
    "header": { "Authorization": ["Bearer ..."] }
  },
  "env": { "MCP_TOKEN": "..." },
  "tool": {
    "name": "hecaton_list_cases",
    "workspace_id": "security",
    "args": { "workspace_id": "security", "status": "OPEN" }
  }
}
```

- `req` — the inbound HTTP request (method, path, headers). The request body is
  not included; the tool call is surfaced separately via `tool`.
- `env` — only the environment variables named with `--mcp-env`.
- `tool` — the tool name, the target `workspace_id` (when the tool takes one),
  and the full argument map, so a policy can authorize per tool / workspace /
  argument.
- `slack_user_id` — the caller's resolved Slack user ID in verified identity
  mode. The configured identity header is removed from `req.header` before
  evaluation.

### Policy output

```json
{ "allow": true, "user": "U0123456789" }
```

- `allow` (boolean, required) — gates the call. When false, the tool returns an
  authorization error and no data is read.
- `user` (string, optional) — the Slack user ID the request acts as in
  policy-only mode. In verified identity mode, the authenticated Slack user
  always remains the caller.

### Example policy

```rego
package auth.mcp

default allow := false

# Allow when the Authorization header carries the shared MCP token supplied
# via --mcp-env MCP_TOKEN.
allow if {
	input.req.header.Authorization[0] == sprintf("Bearer %s", [input.env.MCP_TOKEN])
}

# Act as a fixed Slack user.
user := "U0123456789" if {
	input.req.header.Authorization[0] == sprintf("Bearer %s", [input.env.MCP_TOKEN])
}

# You can also authorize per tool or workspace, e.g. allow only listing:
# allow if { input.tool.name == "hecaton_list_workspaces" }
```

Policies are compiled once at startup; a malformed policy makes `serve` fail
immediately rather than at the first request.

## Security notes

- **Credentials are kept out of logs.** The identity header is consumed by
  MCP authentication. The `input.env` allow-list is tagged for
  redaction and the `Authorization` header is redacted by the logger's masq
  configuration, so neither appears in logs even at debug level.
- **Stateless transport.** The Streamable HTTP transport runs stateless: each
  request carries its own authorization and no cross-request session state is
  held in process memory, so the endpoint is safe behind a horizontally-scaled
  deployment.
- **Use HTTPS** to protect credentials in transit.
