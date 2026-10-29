# CLI Reference

This document is the complete reference for the `hecatoncheires` command-line interface: its subcommands, their flags, and the environment variables they read.

All flags can also be set via environment variables. Environment variables use the prefix `HECATONCHEIRES_` with uppercase, underscore-separated names (e.g., `--log-level` becomes `HECATONCHEIRES_LOG_LEVEL`).

CLI flags take precedence over environment variables.

Subcommands:

- [`serve`](#serve) — start the HTTP server (GraphQL API + frontend + Slack webhooks).
- [`assist`](#assist) — run the AI assist agent across open cases.
- [`migrate`](#migrate) — manage Firestore indexes.
- [`validate`](#validate) — validate configuration files and optionally check DB consistency.
- [`diagnosis`](#diagnosis) — one-shot data inspection / repair jobs.
- [`tick`](#tick) — run a single sweep over scheduled Agent Jobs.
- [`eval`](./eval.md) — run offline scenario-based evaluation of LLM workflows (see [eval.md](./eval.md)).
- [`export`](./export.md) — full-refresh the current workspace data into BigQuery (see [export.md](./export.md)).

For TOML configuration topics (workspace definitions, field schemas, the `[assist]` section, etc.), see [configuration.md](./configuration.md).

---

## Global Flags

Available for all commands.

| Flag | Alias | Env Var | Default | Description |
|------|-------|---------|---------|-------------|
| `--log-level` | `-l` | `HECATONCHEIRES_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `--log-format` | `-f` | `HECATONCHEIRES_LOG_FORMAT` | `console` | Log format: `console`, `json` |
| `--log-output` | `-o` | `HECATONCHEIRES_LOG_OUTPUT` | `stdout` | Log output: `stdout`, `stderr`, `-`, or a file path |
| `--log-quiet` | `-q` | `HECATONCHEIRES_LOG_QUIET` | `false` | Quiet mode (disables all log output) |
| `--log-stacktrace` | `-s` | `HECATONCHEIRES_LOG_STACKTRACE` | `true` | Show stacktrace in console format |

---

## `serve`

The `serve` command (alias: `s`) starts the HTTP server.

| Flag | Env Var | Default | Required | Description |
|------|---------|---------|----------|-------------|
| `--addr` | `HECATONCHEIRES_ADDR` | `:8080` | No | HTTP server address and port |
| `--base-url` | `HECATONCHEIRES_BASE_URL` | - | Yes\* | Application base URL (e.g., `https://your-domain.com`). No trailing slash |
| `--graphiql` | `HECATONCHEIRES_GRAPHIQL` | `true` | No | Enable GraphiQL playground at `/graphiql` |
| `--config` | `HECATONCHEIRES_CONFIG` | `./config.toml` | No | Path to TOML configuration file |
| `--global-config` | `HECATONCHEIRES_GLOBAL_CONFIG` | - | No | Paths to deployment-wide config files/directories (TOML) holding `[[workspace_group]]` definitions. Unset leaves workspace groups dormant. See [configuration.md](./configuration.md#global-configuration-workspace-groups) |
| `--firestore-project-id` | `HECATONCHEIRES_FIRESTORE_PROJECT_ID` | - | Yes | Google Cloud Firestore project ID |
| `--firestore-database-id` | `HECATONCHEIRES_FIRESTORE_DATABASE_ID` | `(default)` | No | Firestore database ID |
| `--notion-api-token` | `HECATONCHEIRES_NOTION_API_TOKEN` | - | No | Notion API token for Source integration |
| `--default-lang` | `HECATONCHEIRES_DEFAULT_LANG` | `en` | No | Language (`en`, `ja`) of Slack messages and agent replies when neither the workspace's `[workspace] language` nor the Slack user's locale applies. A workspace `language` takes priority over the user's locale. See [configuration.md](./configuration.md#workspace-language) |
| `--no-auth` | `HECATONCHEIRES_NO_AUTH` | - | No | Slack user ID for no-auth mode (development only) |
| `--slack-client-id` | `HECATONCHEIRES_SLACK_CLIENT_ID` | - | Yes\* | Slack OAuth client ID |
| `--slack-client-secret` | `HECATONCHEIRES_SLACK_CLIENT_SECRET` | - | Yes\* | Slack OAuth client secret |
| `--slack-bot-token` | `HECATONCHEIRES_SLACK_BOT_TOKEN` | - | No\*\* | Slack Bot User OAuth Token (`xoxb-...`) |
| `--slack-user-oauth-token` | `HECATONCHEIRES_SLACK_USER_OAUTH_TOKEN` | - | No | Slack User OAuth Token for admin API (`xoxp-...`, required for cross-workspace channel connect in Enterprise Grid) |
| `--slack-signing-secret` | `HECATONCHEIRES_SLACK_SIGNING_SECRET` | - | No\*\*\* | Slack signing secret for webhook verification |
| `--slack-notification-slot-duration` | `HECATONCHEIRES_NOTIFICATION_SLOT_DURATION` | `1h` | No | Rolling window during which Action/Step change notifications are aggregated into a single editable channel message. Set `0` to disable aggregation (legacy `reply_broadcast` per event). See [user_guide.md](./user_guide.md) |
| `--slack-tool-max-text-size` | `HECATONCHEIRES_SLACK_TOOL_MAX_TEXT_SIZE` | `4096` | No | Maximum bytes of a single Slack message's `text` in an agent tool result. A longer message is cut on a rune boundary and its entry carries `text_truncated: true`. Applies to both `slack__search_messages` and `slack__get_messages`. `0` disables the bound. See [Slack agent tool limits](#slack-agent-tool-limits) |
| `--slack-tool-max-result-size` | `HECATONCHEIRES_SLACK_TOOL_MAX_RESULT_SIZE` | `32768` | No | Maximum combined bytes of the messages one Slack agent tool call returns. Messages past the cap are dropped and reported in the response. Applies to both Slack read tools. `0` disables the bound. See [Slack agent tool limits](#slack-agent-tool-limits) |
| `--github-app-id` | `HECATONCHEIRES_GITHUB_APP_ID` | - | No | GitHub App ID for GitHub Source integration |
| `--github-app-installation-id` | `HECATONCHEIRES_GITHUB_APP_INSTALLATION_ID` | - | No | GitHub App Installation ID |
| `--github-app-private-key` | `HECATONCHEIRES_GITHUB_APP_PRIVATE_KEY` | - | No | GitHub App private key (PEM string or file path) |
| `--webfetch-enabled` | `HECATONCHEIRES_WEBFETCH_ENABLED` | `true` | No | Enable the agent `webfetch` tool. Built only when an LLM client is also configured (the LLM screens fetched content for indirect prompt injection). Connections to non-public IPs are blocked (SSRF guard) |
| `--webfetch-timeout` | `HECATONCHEIRES_WEBFETCH_TIMEOUT` | `10` | No | `webfetch` HTTP request timeout in seconds |
| `--webfetch-max-size` | `HECATONCHEIRES_WEBFETCH_MAX_SIZE` | `262144` | No | `webfetch` maximum response body size in bytes (excess is truncated). Default 256 KiB keeps a single fetch within model context / cost limits. Does not apply to a PDF response — see the next row |
| `--webfetch-max-pdf-size` | `HECATONCHEIRES_WEBFETCH_MAX_PDF_SIZE` | `2097152` | No | `webfetch` maximum size in bytes of an `application/pdf` response. A larger PDF is **refused**, not truncated (a truncated PDF is a broken file the model would read as a complete one). Default 2 MiB covers the guideline / advisory PDFs an agent cites while bounding cost — a PDF page runs roughly 1.5k–3k tokens. `0` disables PDF reading; a PDF fetch then fails naming the disabled cap |
| `--llm-model` | `HECATONCHEIRES_LLM_MODEL` | - | No\*\*\*\* | Default model: the reference name of an `[[llm_model]]` entry in `--global-config` (its `alias`, or its `model` when it declares none). Empty disables AI features. See [Model definitions](#model-definitions) |
| `--llm-openai-api-key` | `HECATONCHEIRES_LLM_OPENAI_API_KEY` | - | No\*\*\*\* | OpenAI API key (required for a model whose `provider` is `openai`) |
| `--llm-claude-api-key` | `HECATONCHEIRES_LLM_CLAUDE_API_KEY` | - | No\*\*\*\* | Anthropic Claude API key (for a `claude` model reached through Anthropic directly) |
| `--llm-gemini-project-id` | `HECATONCHEIRES_LLM_GEMINI_PROJECT_ID` | - | No\*\*\*\* | Google Cloud project ID (Gemini, or Claude via Vertex AI) |
| `--llm-gemini-location` | `HECATONCHEIRES_LLM_GEMINI_LOCATION` | `global` | No | Google Cloud location for Gemini / Claude on Vertex AI |
| `--embedding-gemini-project-id` | `HECATONCHEIRES_EMBEDDING_GEMINI_PROJECT_ID` | - | Cond. | Google Cloud project ID for the Gemini embedding client. Required whenever `--llm-model` is set |
| `--embedding-gemini-location` | `HECATONCHEIRES_EMBEDDING_GEMINI_LOCATION` | `global` | No | Google Cloud location for the Gemini embedding client |
| `--embedding-model` | `HECATONCHEIRES_EMBEDDING_MODEL` | `gemini-embedding-2` | No | Gemini embedding model name |
| `--dashboard-stale-threshold` | `HECATONCHEIRES_DASHBOARD_STALE_THRESHOLD` | `336h` (14d) | No | Age after which an open Case with no update is flagged as "stalled" on the home dashboard. `0` disables the stalled flag |
| `--home-message-llm-provider` | `HECATONCHEIRES_HOME_MESSAGE_LLM_PROVIDER` | - | No | Dedicated LLM provider (`openai`/`claude`/`gemini`) for the home dashboard greeting. Empty falls back to `--llm-provider`; if neither is set the greeting is disabled |
| `--home-message-llm-model` | `HECATONCHEIRES_HOME_MESSAGE_LLM_MODEL` | - | No | Model name for the home greeting LLM (provider default if empty) |
| `--home-message-llm-openai-api-key` | `HECATONCHEIRES_HOME_MESSAGE_LLM_OPENAI_API_KEY` | - | No | OpenAI API key (when `--home-message-llm-provider=openai`) |
| `--home-message-llm-claude-api-key` | `HECATONCHEIRES_HOME_MESSAGE_LLM_CLAUDE_API_KEY` | - | No | Anthropic Claude API key (when `--home-message-llm-provider=claude`, direct access) |
| `--home-message-llm-gemini-project-id` | `HECATONCHEIRES_HOME_MESSAGE_LLM_GEMINI_PROJECT_ID` | - | No | Google Cloud project ID (Gemini, or Claude via Vertex AI) for the home greeting LLM |
| `--home-message-llm-gemini-location` | `HECATONCHEIRES_HOME_MESSAGE_LLM_GEMINI_LOCATION` | `global` | No | Google Cloud location for the home greeting LLM |
| `--cloud-storage-bucket` | `HECATONCHEIRES_CLOUD_STORAGE_BUCKET` | - | Yes\*\*\*\*\* | Cloud Storage bucket holding agent thread session History/Trace blobs. See [develop/architecture.md](./develop/architecture.md#agent-thread-session-internals) |
| `--cloud-storage-prefix` | `HECATONCHEIRES_CLOUD_STORAGE_PREFIX` | - | No | Optional object key prefix within the Cloud Storage bucket |
| `--sentry-dsn` | `HECATONCHEIRES_SENTRY_DSN` | - | No | Sentry DSN. Setting a non-empty value enables Sentry error reporting via `errutil.Handle`. See [operations.md](./operations.md) |
| `--sentry-env` | `HECATONCHEIRES_SENTRY_ENV` | - | No | Sentry environment tag (e.g., `production`, `staging`) |
| `--sentry-release` | `HECATONCHEIRES_SENTRY_RELEASE` | - | No | Sentry release identifier (e.g., commit SHA) |
| `--mcp` | `HECATONCHEIRES_MCP` | `false` | No | Enable the MCP (Model Context Protocol) endpoint at `/mcp`. Requires `--policy`. See [mcp.md](./mcp.md) |
| `--policy` | `HECATONCHEIRES_POLICY` | - | Cond. | Path(s) to Rego policy files or directories used to authorize MCP requests (`data.auth.mcp`). Repeatable. **Required** when `--mcp` is set |
| `--mcp-env` | `HECATONCHEIRES_MCP_ENV` | - | No | Names of environment variables to expose to the Rego policy as `input.env` (allow-list). Repeatable |
| `--job-max-concurrency` | `HECATONCHEIRES_JOB_MAX_CONCURRENCY` | `1` | No | Maximum number of **scheduled** Agent Job runs executing concurrently across the whole deployment. Set the same value on every instance (including `tick`). `0` disables the limit. See [operations.md](./operations.md) |
| `--agent-max-steps` | `HECATONCHEIRES_AGENT_MAX_STEPS` | `128` | No | Maximum committed transitions one agent run may execute, sub-agents included. See [Agent runtime budgets](#agent-runtime-budgets) |
| `--agent-default-budget-usd` | `HECATONCHEIRES_AGENT_DEFAULT_BUDGET_USD` | - | No | USD one agent run may spend, sub-agents included, before it is told to conclude — not a hard ceiling; see [Agent runtime budgets](#agent-runtime-budgets). Overrides `[agent] default_budget_usd` in `--global-config`; a Job's `budget_usd` overrides both. Unset falls back to the document, then to `2.0` |
| `--agent-task-max-steps` | `HECATONCHEIRES_AGENT_TASK_MAX_STEPS` | `48` | No | Maximum committed transitions one sub-agent may execute |
| `--agent-task-max-input-tokens` | `HECATONCHEIRES_AGENT_TASK_MAX_INPUT_TOKENS` | `100000` | No | Maximum input tokens one sub-agent may consume |
| `--agent-task-max-output-tokens` | `HECATONCHEIRES_AGENT_TASK_MAX_OUTPUT_TOKENS` | `20000` | No | Maximum output tokens one sub-agent may produce |
| `--agent-budget-notice-ratio` | `HECATONCHEIRES_AGENT_BUDGET_NOTICE_RATIO` | `0.9` | No | Fraction of any ceiling an agent may spend before it is told to conclude. What is left is the reserve it concludes on (see the budget section below for what it does there). Must be greater than 0 and less than 1 |
| `--agent-worker-concurrency` | `HECATONCHEIRES_AGENT_WORKER_CONCURRENCY` | `8` | No | Maximum agent transitions this instance drives at once |
| `--agent-worker-poll-concurrency` | `HECATONCHEIRES_AGENT_WORKER_POLL_CONCURRENCY` | `2` | No | Number of parallel poll loops looking for runnable agent processes |
| `--agent-worker-lease` | `HECATONCHEIRES_AGENT_WORKER_LEASE` | `120s` | No | How long a worker holds a claimed agent process before another instance may reclaim it |
| `--agent-worker-poll-interval` | `HECATONCHEIRES_AGENT_WORKER_POLL_INTERVAL` | `2s` | No | How often a worker polls for runnable agent processes |

### Slack agent tool limits

`slack__search_messages` and `slack__get_messages` return message text straight
into the model context, and the number of results is chosen by the model
(`count` is a tool parameter) rather than by the operator. Two flags bound what
one call may inject; both apply to **both** tools, so an operator configures one
budget for Slack reads rather than one per tool.

| Bound | Flag | What it does |
|---|---|---|
| Per message | `--slack-tool-max-text-size` | Cuts one message's `text` at the byte limit, on a rune boundary. The entry then carries `text_truncated: true`. |
| Per call | `--slack-tool-max-result-size` | Stops admitting messages once their combined JSON size reaches the limit. |

Every response reports what survived, so a partial result is never mistaken for
a complete one:

- `slack__search_messages` returns `returned`, `omitted` and `truncated`
  alongside `total` (which keeps reporting what Slack matched, not what fit).
- `slack__get_messages` returns `omitted` and `truncated` at the top level, and
  repeats `omitted` on each target that lost messages, so the agent knows which
  thread it is missing. The per-call budget is shared across the targets of one
  call and charged in target order, so the same request returns the same
  messages every time. Which messages a thread gives up depends on its `order`
  parameter: the default `oldest` keeps the start of the thread and drops the
  newest replies, while `latest` keeps the thread root and the newest replies
  and drops the oldest ones. Use `latest` when the agent must see what was said
  last (for example a progress check deciding whether someone already replied).

Setting either flag to `0` disables that bound; a negative value is rejected at
startup rather than read as "disabled". The two are independent: the per-message
cap is what catches an outlier long message, while the per-call cap is what
bounds a call returning many short ones. Both matter because a sub-agent re-sends
its accumulated tool results on every later turn, so an unbounded call is paid
for repeatedly.

The first message of a call is always returned, even when it alone exceeds
`--slack-tool-max-result-size`. Dropping it would leave the agent an empty result
it cannot get past — asking for fewer results returns the same oversized message
first again. This only arises when the per-message cap is disabled and the
per-call one is not; with both at their defaults a single entry cannot exceed the
per-call budget.

The same flags are registered on `serve`, `tick`, `assist` and `eval`, so a
scheduled run and a scenario see the same bounds a mention turn does.

### Model definitions

Which models this deployment may use, and what each costs, is declared once in a
`--global-config` file as `[[llm_model]]` entries. Nothing else may name a model:
`--llm-model` picks the default one by reference name, and a Job's `llm_model`
picks its own. A reference name that no entry defines fails at startup and in
`hecatoncheires validate`.

Prices are written in **USD per 1M tokens**, the unit every provider publishes,
and are what the money budget below is measured against. The full field
reference, the reference-name rules and the per-provider credentials are in
[configuration.md](./configuration.md#model-definitions-llm_model).

Requests to an `openai` model set neither `reasoning_effort` nor `verbosity`, so
the model's own defaults apply — both for `--llm-model` / a Job's `llm_model` and
for the home greeting LLM. Earlier releases sent `reasoning_effort="minimal"` and
`verbosity="low"` on every request; on a reasoning model the defaults can spend
more reasoning tokens and produce longer output, so a run on such a model may
reach its money budget (below) sooner than before.

### Agent runtime budgets

An agent run is a durable process: each transition (one LLM call, or one tool
call) is checkpointed before the next one starts, and any instance can pick the
process up from the last checkpoint. The two tiers of a run are bounded by
different quantities:

- **A root run** — what a mention, a Job or an assist pass starts — is bounded by
  **money** (`--agent-default-budget-usd`, `[agent] default_budget_usd`, or a
  Job's `budget_usd`) and by **steps** (`--agent-max-steps`).
- **A sub-agent** — one planned task inside a run — is bounded by **steps** and
  **tokens** (`--agent-task-max-*`), and by the money allowance its parent gave
  it.

The root ceiling is money because a token is not a unit of cost: one Job may run
on a model twenty times dearer than another's, so any token figure is right for
one of them and wrong for the other. Its step ceiling is not a spend limit — it
exists so a run that never terminates is stopped even while it costs almost
nothing. The task tier keeps token ceilings on top of its money allowance: they
bound one investigation's share of a turn.

Each generate is priced by the four token counts the provider reports — uncached
input, output, cache read and cache write — at the rate of the model the run
actually used. A cache read is charged at its discount and a cache write at its
premium, which is why `[[llm_model]]` carries four prices rather than one.

Every ceiling is cumulative over the whole run, sub-agents included: a
sub-agent's usage is added to its parent when it finishes, so the ceiling on a
run covers everything it spawned. Read the two step tiers together —
`--agent-task-max-steps` bounds ONE investigation, `--agent-max-steps` bounds the
planner plus all of them. The default pair (128 root, 48 task) affords the
planner's own work plus roughly two sub-agents at their full allowance, or
several modest ones.

Because a sub-agent's usage arrives in one addition when it finishes, a run can
cross a ceiling by a whole sub-agent's worth at once; the reported figure is then
past the ceiling rather than at it.
Crossing `--agent-budget-notice-ratio` of the budget or the step ceiling puts the
run into its **reserve** — by default the last tenth. What the rest of the run is
depends on the agent's strategy.

- **An agent that works in a single loop** (the channel-mode case agent, the
  assist agent, a Job with the simple strategy, and every sub-agent) makes two
  moves. It is first told that this turn is its final tool call and that it must
  not write its result yet, because its side effects (updating a case, posting a
  message, recording an action) happen through its own tool calls and nowhere
  else. The call that carries that tool's result back tells it the opposite: call
  nothing more, and write its result now, briefly. So the reserve has to cover two
  generates and one tool call. The first instruction cannot force the agent to
  call a tool: an agent that writes its result instead simply ends the run there.
- **A plan-and-execute agent** (the thread-mode case agent, the workspace agent,
  the case-draft agent, and a Job with the planexec strategy) starts no further
  sub-agent and makes one move: it writes its final output from what the run has
  done so far. Its planner can only read — every write in such a run is done by a
  sub-agent — so there is no tool call left for it to make, and a new sub-agent
  could spend more than the reserve holds.

The call that may call no more tools is sent to the LLM provider with tool calls
disabled. If the provider returns a tool call anyway, the call is still run and
answered, because leaving it unanswered would make the provider reject every
later request in the run.

**Spending the budget does not stop the run.** Past the ceiling the agent is told
that its budget is gone and makes the same moves; the run then ends itself.
This matters because a sub-agent's whole spend is added to its parent in one write
when the sub-agent finishes: a run regularly passes from under the notice
threshold to past the budget in a single jump, with no earlier warning, and
stopping it there would end a turn whose sub-agents had already done the work with
nothing recorded about it.

**So a budget bounds what a run may spend before it must conclude — it does not
bound what you are charged.** Three things land on top of it:

- the overshoot already committed when the budget was first observed as spent.
  Because of the fold above, the first verdict a run gets about its money can
  already be well past the figure — the run this behaviour was written for
  reported `$2.31` against a `$2.00` budget.
- the reserve's calls: two for an agent that works in a single loop, one for a
  plan-and-execute agent.
- everything up to the step or token ceiling, if the model keeps calling tools on
  a call sent with tool calls disabled (`tool_choice: none`). This happens only if
  the LLM provider returns a tool call despite that setting.

**Each sub-agent's share is decided by the planner.** When it plans a round, the
planner is told what the run has left and states an amount for every task; the
runtime rejects the plan unless each amount is positive, no single amount exceeds
what is left, and they add up to no more than what is left. Each sub-agent is then
given exactly its stated share, so the heavy task can have the larger one — which
an even split could not do.

What this bounds is the round's **allocations**, not its spend. Two things sit
between the two: the figure is read before the planning call that itself costs
something, and a sub-agent that crosses its share is told to conclude rather than
stopped, exactly as the run is. `--agent-max-steps` and `--agent-task-max-*`
remain the only hard bounds.

A sub-agent that spends its share is told to conclude, exactly as the run is: it
makes its final tool call and reports. A task given far too little therefore
returns a thin answer rather than failing.

What still stops a run outright is a **step or token** ceiling
(`--agent-max-steps`, `--agent-task-max-*`), and a run whose budget or model price
cannot be resolved at all. Size `--agent-max-steps` accordingly: it is the real
upper bound on a run that will not end itself.

**Check the ratio against your tightest ceiling, not against the money one.** The
ratio applies to every ceiling, and the sub-agent input-token ceiling is where a
tenth is thinnest: at the default `--agent-task-max-input-tokens` of 100000 the
reserve is 10000 input tokens, and each generate re-sends the whole conversation
rather than the last turn of it. A sub-agent whose conversation has grown past
about ten thousand tokens therefore crosses its ceiling on the reserve's first
generate and is stopped before the tool call — the run ends the way it did before
the reserve existed. Raise `--agent-task-max-input-tokens`, or lower this ratio,
if your sub-agents run long enough for that.

A run whose budget or model price cannot be resolved is stopped rather than run
unbounded. Startup validation makes that unreachable for a configured
deployment; reaching it means a run's metadata names something this build cannot
price.

### A crashed run is not resumed

If the instance driving a run dies mid-transition, the run fails; it is not
picked up and re-run from its last checkpoint. That is deliberate. A transition
performs its effect and is checkpointed afterwards, so re-running one that died
in between would repeat an effect that already happened — a second Action, a
second Slack post. Failing is what the previous runtime did with a crashed turn,
so this is not a regression, and the user gets the usual "couldn't finish this
turn" reply. A run that merely returns an *error* is still retried normally;
this applies only to a claim that vanished.

The step defaults are derived from the loop bounds the previous agent runtime
used. **The task token defaults are not derived from measurement** — the previous
runtime counted no tokens — so they are a starting point. Review them against
the usage your deployment actually records before tightening them.

### Migrating from the token-based agent budget

Three settings were removed when the root ceiling became money. An environment
variable for a flag that no longer exists is **silently ignored**, so a
deployment that keeps them will run with defaults it did not choose:

| Removed | Do this instead |
|---|---|
| `HECATONCHEIRES_LLM_PROVIDER` | Declare the model as an `[[llm_model]]` entry (the `provider` lives there) and set `HECATONCHEIRES_LLM_MODEL` to its reference name |
| `HECATONCHEIRES_AGENT_MAX_INPUT_TOKENS` | Set `HECATONCHEIRES_AGENT_DEFAULT_BUDGET_USD`, or `[agent] default_budget_usd` |
| `HECATONCHEIRES_AGENT_MAX_OUTPUT_TOKENS` | Same as above |

`--llm-model` was previously an optional model name; it is now a reference name
into `[[llm_model]]` and is what enables the AI features at all. A deployment
that used a provider's default model must now name that model explicitly, so its
price is known.

\* Required for OAuth mode. Alternatively, use `--no-auth` for development.

\*\* When used with `--no-auth`, the given user ID is validated against Slack; without it, `--no-auth` runs as a placeholder test user. Also enables user avatar display and Slack user refresh worker.

\*\*\* Required only to enable Slack webhook integration. Without this, webhook endpoints are not registered.

\*\*\*\* `--llm-model` is optional for `serve` (AI features will be disabled if unset). When set, it must name an `[[llm_model]]` entry, and that entry's `provider` decides which credentials become required:
- `openai` → `--llm-openai-api-key`
- `claude` → either `--llm-claude-api-key` (direct Anthropic API) **or** `--llm-gemini-project-id` (Vertex AI). The two are mutually exclusive.
- `gemini` → `--llm-gemini-project-id` and `--llm-gemini-location`

The same rule applies to every model a Job names: its client is built at startup, so its credentials must be present then. Declaring a model that no Job and no `--llm-model` names costs nothing — no client is built for it.

The embedding client is configured separately and is **required whenever LLM is enabled** (`--llm-model` set on `serve`, or always for `assist`). It is reserved for upcoming similarity-search features; the wiring is preserved so callers can keep the same flags through the redesign. The default model is `gemini-embedding-2`; the dimension is fixed at 768. Application Default Credentials must be authorized for the project. Without `--llm-model`, `serve` runs in a degraded mode that does not need the embedder either.

\*\*\*\*\* Required whenever `--slack-bot-token` is configured. The agent that responds to Slack mentions persists per-thread conversation History and execution Trace into the bucket so follow-up mentions can resume the session. The service account needs **Storage Object Admin** on the bucket.

The prefix for auto-created Slack channel names is not a CLI flag: it is configured per workspace via the `[slack] channel_prefix` key in the TOML configuration file, and defaults to the workspace ID when unset. See [configuration.md](./configuration.md#slack-section).

See [Authentication Modes](#authentication-modes) below for the two supported authentication configurations.

---

## `assist`

The `assist` command (alias: `a`) runs the AI assist agent for all open cases across workspaces. It requires an LLM provider, an embedding client, and a Slack bot token.

| Flag | Env Var | Default | Required | Description |
|------|---------|---------|----------|-------------|
| `--slack-bot-token` | `HECATONCHEIRES_SLACK_BOT_TOKEN` | - | Yes | Slack Bot Token for sending notifications |
| `--workspace` | `HECATONCHEIRES_ASSIST_WORKSPACE` | - | No | Target workspace ID (if empty, process all workspaces) |
| `--log-count` | `HECATONCHEIRES_ASSIST_LOG_COUNT` | `7` | No | Number of recent assist logs to include in system prompt |
| `--message-count` | `HECATONCHEIRES_ASSIST_MESSAGE_COUNT` | `50` | No | Number of recent Slack messages to include in system prompt |
| `--config` | `HECATONCHEIRES_CONFIG` | `./config.toml` | No | Paths to configuration files or directories (TOML). Can be specified multiple times |
| `--repository-backend` | `HECATONCHEIRES_REPOSITORY_BACKEND` | `firestore` | No | Repository backend type (`firestore` or `memory`) |
| `--firestore-project-id` | `HECATONCHEIRES_FIRESTORE_PROJECT_ID` | - | Cond. | Firestore Project ID (required when using firestore backend) |
| `--firestore-database-id` | `HECATONCHEIRES_FIRESTORE_DATABASE_ID` | - | No | Firestore Database ID |
| `--global-config` | `HECATONCHEIRES_GLOBAL_CONFIG` | - | Yes | Paths to global config files or directories holding the `[[llm_model]]` definitions. Required for `assist`: the model `--llm-model` names must be defined |
| `--llm-model` | `HECATONCHEIRES_LLM_MODEL` | - | Yes | Reference name of an `[[llm_model]]` entry. Required for `assist` |
| `--llm-openai-api-key` | `HECATONCHEIRES_LLM_OPENAI_API_KEY` | - | Cond. | OpenAI API key (required for a model whose `provider` is `openai`) |
| `--llm-claude-api-key` | `HECATONCHEIRES_LLM_CLAUDE_API_KEY` | - | Cond. | Anthropic Claude API key (for a `claude` model reached through Anthropic directly) |
| `--llm-gemini-project-id` | `HECATONCHEIRES_LLM_GEMINI_PROJECT_ID` | - | Cond. | Google Cloud project ID (Gemini, or Claude via Vertex AI) |
| `--llm-gemini-location` | `HECATONCHEIRES_LLM_GEMINI_LOCATION` | `global` | No | Google Cloud location for Gemini / Claude on Vertex AI (e.g. `global`, `us-central1`) |
| `--embedding-gemini-project-id` | `HECATONCHEIRES_EMBEDDING_GEMINI_PROJECT_ID` | - | Yes | Google Cloud project ID for the Gemini embedding client |
| `--embedding-gemini-location` | `HECATONCHEIRES_EMBEDDING_GEMINI_LOCATION` | `global` | No | Google Cloud location for the Gemini embedding client (e.g. `global`, `us-central1`) |
| `--embedding-model` | `HECATONCHEIRES_EMBEDDING_MODEL` | `gemini-embedding-2` | No | Gemini embedding model name |

`assist` also accepts every `--agent-*` flag listed under [`serve`](#serve); they
bound the assist agent's run the same way. See [Agent runtime budgets](#agent-runtime-budgets).

It accepts the Slack tool flags (`--slack-tool-max-*`) too. The assist agent gets
`slack__get_messages` from the bot token alone, so its Slack reads are bounded
exactly as `serve`'s are — see [Slack agent tool limits](#slack-agent-tool-limits).

The `[assist]` TOML section (prompt, language) is documented in [configuration.md](./configuration.md).

### How an assist pass runs

`assist` is one foreground pass. It spawns one agent run per open case, drives
the agent worker itself until every run has finished, writes each run's
`AssistLog`, and exits — so the command's exit is what tells its scheduler the
pass is over.

Its agent runs are held **in the process, not in Firestore**, unlike `serve`'s.
The assist agent exists only inside this command, so a run left in the shared
store would be picked up by a `serve` instance that cannot resolve it and would
be failed outright. The consequence is that an interrupted `assist` pass is not
resumable: the next invocation starts over, which is what it did before as well.

---

## `migrate`

The `migrate` command (alias: `m`) manages Firestore indexes.

| Flag | Env Var | Default | Required | Description |
|------|---------|---------|----------|-------------|
| `--firestore-project-id` | `HECATONCHEIRES_FIRESTORE_PROJECT_ID` | - | Yes | Google Cloud Firestore project ID |
| `--firestore-database-id` | `HECATONCHEIRES_FIRESTORE_DATABASE_ID` | `(default)` | No | Firestore database ID |
| `--dry-run` | - | `false` | No | Preview migration changes without applying |

Operational depth (when to run a migration, emulator usage, index policy) lives in [operations.md](./operations.md).

---

## `validate`

The `validate` command (alias: `v`) validates configuration files and optionally checks DB consistency.

| Flag | Env Var | Default | Required | Description |
|------|---------|---------|----------|-------------|
| `--config` | `HECATONCHEIRES_CONFIG` | `./config.toml` | No | Paths to configuration files or directories (TOML). Can be specified multiple times |
| `--global-config` | `HECATONCHEIRES_GLOBAL_CONFIG` | - | No | Paths to deployment-wide config files/directories (TOML). Validated when present: workspace group members must reference known workspaces, `[[llm_model]]` entries must be well-formed and uniquely named, `[agent]` must carry a usable budget, and every `[[job]]` `llm_model` must name a defined model |
| `--repository-backend` | `HECATONCHEIRES_REPOSITORY_BACKEND` | `firestore` | No | Repository backend type (`firestore` or `memory`) |
| `--firestore-project-id` | `HECATONCHEIRES_FIRESTORE_PROJECT_ID` | - | Cond. | Firestore Project ID (required when using firestore backend) |
| `--firestore-database-id` | `HECATONCHEIRES_FIRESTORE_DATABASE_ID` | - | No | Firestore Database ID |
| `--check-db` | `HECATONCHEIRES_CHECK_DB` | `false` | No | Perform database consistency check |

When `--check-db` is not specified, only the configuration files are validated and the DB consistency check is skipped.

The Job-to-model cross-check needs both documents: the Jobs come from `--config`
and the definitions from `--global-config`, so it runs only when both are given.
It is the same check `serve` performs at startup, available without deploying.

A workspace's `[authz]` policy is compiled and trial-evaluated here exactly as
`serve` does at startup (see
[configuration.md § Authorization Section](./configuration.md#authorization-section-authz)):
a policy that does not compile, or that errors on either sample input, fails
validation. `POST /api/validate/db` does not read policy files — a submitted
document has no directory of its own — so over HTTP only the shape of
`[authz] policy` (at least one non-empty path) is checked.

### What `--check-db` checks

The DB consistency check answers one question: has a configuration change left
existing data inconsistent with the configuration now in force? It reads every
Case, Action and Memo of every registered workspace and reports the mismatches.
**It never writes** — repairing data is the job of `diagnosis` subcommands.

| Reported as | Applies to | Detects |
|-------------|-----------|---------|
| `field_value` | Case and Memo field values | A stored value that violates its declared type: a `select` / `multi-select` option id the definition no longer lists, a `date` that is not RFC3339, a `number` holding a string, a `case_ref` that is not a numeric id, a `text` value that does not fit the field's `semantic` (e.g. a channel name in a `slack_channel_id` field; `expected` is then `text (<semantic>)`), a `text` value that does not match the field's `validation.pattern` (`expected` is then `text (pattern=<pattern>)`, or `text (<semantic>, pattern=<pattern>)` when the field also has a semantic), and the equivalent for every other field type |
| `field_type_mismatch` | Case and Memo field values | A stored value whose recorded type no longer matches the schema. This catches a type change whose values still happen to fit — `text` → `markdown` are both strings — which `field_value` alone cannot see. Values written before the type was recorded (empty type) are not comparable and are skipped |
| `case_ref_missing` | Case field values | A `case_ref` / `multi_case_ref` value pointing at a Case that does not exist in the field's configured `reference_workspace`. This is what surfaces after `reference_workspace` is repointed |
| `board_status_invalid` | thread-mode Cases | A `BoardStatus` that is empty or is not one of the `[[case.status]]` ids. Such a Case appears in no Kanban column |
| `lifecycle_status_mismatch` | thread-mode Cases | A Case whose lifecycle status (`OPEN` / `CLOSED`) disagrees with whether its `BoardStatus` is listed under `[case] closed`. This is what surfaces after a status is added to or removed from `closed` |
| `action_status_invalid` | Actions | A `Status` that is not one of the `[[action.status]]` ids. Archived Actions are included, because they stay visible in the Case history |
| `archived_case_not_closed` | all Cases | An archived Case whose lifecycle status is not `CLOSED`. Every write path enforces the invariant (archiving refuses a non-closed Case; reopen and board-status changes refuse an archived one), so a finding means the document came from somewhere else — a data import, a manual edit, or a lost race between archive and reopen. It matters because such a Case is filtered out of the Cases list, the board, the dashboard and the Job scan at once, and cannot be found without this check. Unlike the two rows above it is not gated on `CaseMode`: the invariant holds in channel mode too |

Each row of output is one **group**, not one entity: a configuration change
affects a whole workspace uniformly, so occurrences sharing the same check and
field id collapse into a single line carrying `count` (how many entities) and
`sample` (the lowest-id entity, e.g. `case:42` / `action:42/7` / `memo:42/<id>`).
`expected`, `actual` and `message` describe that sample only.

### What `--check-db` deliberately does NOT check

These are consequences of editing the configuration that the project accepts.
They are **not detected** — which is different from not implemented:

- **A field definition removed from the config.** Values stored under a field id
  the schema no longer defines are left alone; only fields whose definition still
  exists are checked. Status ids are the exception, not an oversight: a
  `[[case.status]]` / `[[action.status]]` id that is removed or renamed **is**
  reported by `board_status_invalid` / `action_status_invalid`, because the Case
  or Action keeps pointing at a column or state that no longer exists.
- **A `required` field with no stored value.** `required` is enforced when
  writing. Adding `required = true` later would otherwise flag every Case that
  predates the change.
- **`[[job]]`, Source and Tag references.** Sources and Tags are stored entities
  rather than configuration, and a `JobRun` left behind by a deleted `[[job]]`
  falls under the removed-definition rule above.
- **A Job's `llm_model` and `budget_usd`.** Neither is stored on an entity, so
  there is nothing persisted to reconcile: an undefined model reference is
  reported by `hecatoncheires validate` itself (without `--check-db`) and refused
  at startup. A run record naming a model the configuration no longer declares is
  a historical fact about that run, not an inconsistency.
- **`[slack] allow_existing_channel`.** The setting only decides whether case
  creation may link an existing channel; nothing stored records which way a
  channel was obtained. Turning it off leaves already linked Cases consistent.
  Two Cases sharing one `SlackChannelID` (possible when two creation requests
  for the same channel race) is not detected either. The per-user channel lists
  the existing-channel picker stores (`slackMemberChannels`) are not checked:
  they hold what Slack returned, no configuration value, and the picker
  refetches a list older than 10 minutes.
- **`[authz]` workspace policies.** A policy decides who may access a workspace
  at request time; no stored entity records that decision or any value derived
  from the policy. Changing the policy therefore cannot leave persisted data
  inconsistent, and there is nothing for `--check-db` to reconcile.
- **The workspace ids recorded on a home-screen greeting.** Each stored greeting
  keeps the workspaces its user could access when it was generated, and it is
  reused only while that set matches the user's current access exactly. A
  workspace removed or renamed in the config therefore just makes the greeting
  stop matching, and a new one is generated. The stale id is never read as a
  reference, so there is no inconsistency to report.
- **Whether a referenced Case may be referenced.** `case_ref_missing` checks
  existence only. Privacy and draft state gate references when they are written;
  applying that here would flag references that were legitimate at write time.
- **Agent processes.** The `agentProcesses` documents that back an in-flight
  agent run carry no configuration-derived values — their workspace and case ids
  are references, and everything else is runtime state (strategy state, metrics,
  lease). There is nothing a configuration edit can leave inconsistent there, so
  they are out of scope by construction rather than by choice.
- **Action comments.** An `ActionComment`
  (`actions/{id}/comments/{commentId}`) holds an author id, a Markdown body and
  timestamps — no field values, no status id, nothing derived from the TOML. A
  configuration edit cannot leave one inconsistent, so like agent processes they
  are out of scope by construction.

The check reads the data as it finds it and does not take a snapshot, so a
workspace being written to concurrently can yield a count that is already stale.
Re-run it if the numbers matter.

### Running the same check over HTTP

The `serve` server exposes the identical check at `POST /api/validate/db`, with
one difference: the configuration is taken from the request instead of from the
running process, so a candidate config change can be checked before it is
deployed. The report comes back as JSON. See
[operations.md § DB consistency check over HTTP](./operations.md#db-consistency-check-over-http).

Configuration validation parses every Go `text/template` prompt the config supplies — each `[[job]]` `prompt` / `prompt_file` and every Slack `welcome_messages` entry — with the same template dialect the runtime renders with. A malformed template (an unbalanced `{{ ... }}` action, an unknown function) fails `validate` up-front instead of only erroring the first time the Job runs or a case is created. This is a parse check: it proves the template compiles, not that a specific field reference resolves against a live case (that is exercised at render time).

---

## `diagnosis`

The `diagnosis` command (alias: `d`) groups one-shot data inspection / repair jobs. Each
sub-subcommand is a self-contained job; the umbrella itself takes no flags.

Operational runbook depth for these jobs lives in [operations.md](./operations.md).

### `diagnosis fix-unsent-action`

Re-posts Slack messages for Actions whose initial Slack post never reached
Slack. The job sweeps every workspace in the registry, finds Actions with an
empty `SlackMessageTS`, and replays the post via the unified
`ActionUseCase.PostSlackMessageToAction` entry point. Repeat runs are safe:
already-posted Actions are skipped.

```bash
hecatoncheires diagnosis fix-unsent-action \
  --config=./config.toml \
  --slack-bot-token=xoxb-... \
  --firestore-project-id=...
```

| Flag | Env Var | Default | Required | Description |
|------|---------|---------|----------|-------------|
| `--config` | `HECATONCHEIRES_CONFIG` | `./config.toml` | Yes | Workspace configuration file |
| `--base-url` | `HECATONCHEIRES_BASE_URL` | - | No | Base URL used to render the action's WebUI link inside the Slack message |
| `--default-lang` | `HECATONCHEIRES_DEFAULT_LANG` | `en` | No | Default language for the Slack message text (`en`, `ja`), used when the workspace sets no `[workspace] language` |
| `--slack-bot-token` | `HECATONCHEIRES_SLACK_BOT_TOKEN` | - | Yes | Slack Bot Token used to post the recovery messages |
| `--firestore-project-id` | `HECATONCHEIRES_FIRESTORE_PROJECT_ID` | - | Cond. | Required when using the Firestore backend |
| `--firestore-database-id` | `HECATONCHEIRES_FIRESTORE_DATABASE_ID` | `(default)` | No | Firestore database ID |
| Sentry flags | see [operations.md](./operations.md) | - | No | Same flags as `serve` for error reporting |

The job logs a final summary line:

```
fix-unsent-action complete total=N fixed=X skipped=Y failed=Z
```

- `Total` — Actions found with an empty `SlackMessageTS`
- `Fixed` — Successfully posted; timestamp persisted
- `Skipped` — Documented skip conditions (parent Case has no Slack channel,
  the Action was already posted by a concurrent run, or the row was deleted
  during the sweep)
- `Failed` — Unexpected errors. Each is reported via `errutil.Handle` so it
  reaches the configured error sink (Sentry / log); the sweep continues
  past failures so a single bad row never blocks the rest

---

## `tick`

The `tick` command runs a single sweep over scheduled Agent Jobs and dispatches due ones. The same logic backs `POST /hooks/tick`; wire it to Cloud Scheduler (or any cron).

The dispatched runs execute on the same agent runtime `serve` uses, and the sweep drives that runtime itself: **the command exits once every run it dispatched has finished**, so a scheduled sweep does not depend on a `serve` instance being up. That is why it takes the Cloud Storage and `--agent-*` flags below.

| Flag | Env Var | Default | Required | Description |
|------|---------|---------|----------|-------------|
| `--config` | `HECATONCHEIRES_CONFIG` | `./config.toml` | No | Paths to configuration files or directories (TOML). Can be specified multiple times |
| `--repository-backend` | `HECATONCHEIRES_REPOSITORY_BACKEND` | `firestore` | No | Repository backend type (`firestore` or `memory`) |
| `--firestore-project-id` | `HECATONCHEIRES_FIRESTORE_PROJECT_ID` | - | Cond. | Firestore Project ID (required when using firestore backend) |
| `--firestore-database-id` | `HECATONCHEIRES_FIRESTORE_DATABASE_ID` | - | No | Firestore Database ID |
| `--global-config` | `HECATONCHEIRES_GLOBAL_CONFIG` | - | Cond. | Paths to global config files or directories holding the `[[llm_model]]` definitions and `[agent]`. Required whenever `--llm-model` is set |
| `--llm-model` | `HECATONCHEIRES_LLM_MODEL` | - | No | Reference name of an `[[llm_model]]` entry (empty disables AI features) |
| `--llm-openai-api-key` | `HECATONCHEIRES_LLM_OPENAI_API_KEY` | - | Cond. | OpenAI API key (required for a model whose `provider` is `openai`) |
| `--llm-claude-api-key` | `HECATONCHEIRES_LLM_CLAUDE_API_KEY` | - | Cond. | Anthropic Claude API key (for a `claude` model reached through Anthropic directly) |
| `--llm-gemini-project-id` | `HECATONCHEIRES_LLM_GEMINI_PROJECT_ID` | - | Cond. | Google Cloud project ID (Gemini, or Claude via Vertex AI) |
| `--llm-gemini-location` | `HECATONCHEIRES_LLM_GEMINI_LOCATION` | `global` | No | Google Cloud location for Gemini / Claude on Vertex AI (e.g. `global`, `us-central1`) |
| `--job-max-concurrency` | `HECATONCHEIRES_JOB_MAX_CONCURRENCY` | `1` | No | Maximum number of scheduled Agent Job runs executing concurrently across the whole deployment. Must match the value given to `serve`. `0` disables the limit |
| `--cloud-storage-bucket` | `HECATONCHEIRES_CLOUD_STORAGE_BUCKET` | - | Cond. | Cloud Storage bucket holding the runs' conversation and trace archive. Required whenever an LLM provider is configured — without it a sweep cannot record the runs it dispatches |
| `--cloud-storage-prefix` | `HECATONCHEIRES_CLOUD_STORAGE_PREFIX` | - | No | Optional object key prefix within the bucket |
| `--base-url` | `HECATONCHEIRES_BASE_URL` | - | No | Base URL of the web UI. Slack messages a dispatched run posts (e.g. an Action notification) link back to it; without it the link is dropped |
| `--slack-bot-token` | `HECATONCHEIRES_SLACK_BOT_TOKEN` | - | Cond. | Slack Bot User OAuth Token. Required whenever an LLM provider is configured: it is what gives the dispatched runs their Slack read tools and `slack__post_to_case_channel`, the only way an unattended run reports its result |
| `--slack-user-oauth-token` | `HECATONCHEIRES_SLACK_USER_OAUTH_TOKEN` | - | No | Slack User OAuth Token. Enables `slack__search_messages` (`search:read`) and lets `slack__get_messages` read public channels the bot has not joined (`channels:history`) |
| `--slack-notification-slot-duration` | `HECATONCHEIRES_NOTIFICATION_SLOT_DURATION` | `1h` | No | Rolling window for aggregating channel-side change notifications. Set the same value as `serve` so a case updated by a scheduled run notifies the way it does elsewhere |
| `--notion-api-token` | `HECATONCHEIRES_NOTION_API_TOKEN` | - | No | Notion API token. Enables the `notion__*` agent tools |
| `--embedding-gemini-project-id` | `HECATONCHEIRES_EMBEDDING_GEMINI_PROJECT_ID` | - | Cond. | Required whenever an LLM provider is configured (same rule as `serve`). The knowledge tools' similarity search runs on this embedder |

`tick` also accepts every `--agent-*` flag listed under [`serve`](#serve), plus the
Jira (`--jira-*`), WebFetch (`--webfetch-*`) and Slack tool
(`--slack-tool-max-*`) flags documented there. See
[Agent runtime budgets](#agent-runtime-budgets) and
[Slack agent tool limits](#slack-agent-tool-limits). Set the Slack tool limits to
the same values as `serve`, or a scheduled run reads Slack under different bounds
than a mention turn does.

**Configure the integrations here, not only on `serve`.** The sweep executes the
runs it dispatches, so the Job agent's tools are built from *this* process's
clients. An integration left unconfigured is not offered to the planner at all
(the palette is derived per run from what actually resolved), so the runs simply
proceed without that capability. The command logs a warning at startup for each
one, because a sweep silently running Notion-blind or Jira-blind Jobs is almost
never what the operator intended.

Slack and the LLM are required **together**: `tick` refuses to start with one and
not the other. A sweep with an LLM dispatches agent runs, and Slack is the only
way an unattended run reports anything — without it the runs would mutate cases
and tell nobody.

Two families of `serve` flags are deliberately *not* accepted here:

- **Slack OAuth client id/secret and the signing secret.** They authenticate
  inbound HTTP requests; a sweep serves no endpoints, so asking a cron deployment
  for them would spread credentials it can never use.
- **GitHub App (`--github-*`).** A sweep runs only Job agents, and the Job tool
  palette withholds the `github` toolset from an unattended run. A GitHub client
  configured here would be one no Job can reach.

Operational depth (scheduling cadence, relationship to `POST /hooks/tick`, the
concurrency limit, what the sweep waits for) lives in
[operations.md](./operations.md).

---

## Authentication Modes

The serve command supports two authentication modes:

**OAuth Mode (Production)**
```bash
hecatoncheires serve \
  --base-url=https://your-domain.com \
  --slack-client-id=YOUR_CLIENT_ID \
  --slack-client-secret=YOUR_CLIENT_SECRET \
  --slack-bot-token=xoxb-YOUR_BOT_TOKEN \
  --firestore-project-id=YOUR_PROJECT_ID
```

**No-Auth Mode (Development)**
```bash
hecatoncheires serve \
  --no-auth=U1234567890 \
  --slack-bot-token=xoxb-YOUR_BOT_TOKEN \
  --firestore-project-id=YOUR_PROJECT_ID
```

`--slack-bot-token` is optional here: with it, `--no-auth` validates the user ID against Slack; without it, a placeholder test user is used. If `--slack-client-id`/`--slack-client-secret` are also provided, `--no-auth` takes precedence and they are ignored.

---

## `export`

The `export` command full-refreshes the current data of every configured
workspace into BigQuery — one dataset per workspace, one table per entity
(`cases` / `actions` / `memos` / `knowledge` / `tags`). It reuses the repository
and `--config` / `--global-config` flags (no new flags); the BigQuery
destination and the workspace→dataset mapping live in the `[export]` section of
the global config. See [export.md](./export.md) for the configuration, exported
schema, full-refresh semantics, and required IAM.

```bash
hecatoncheires export \
  --global-config ./global.toml \
  --config ./workspaces/ \
  --firestore-project-id YOUR_PROJECT_ID
```

---

## See Also

- [configuration.md](./configuration.md) — TOML configuration (workspaces, field schemas, the `[assist]` section).
- [export.md](./export.md) — BigQuery export: `[export]` config, exported schema, full-refresh semantics, IAM.
- [deployment.md](./deployment.md) — deployment topology and runtime requirements.
- [operations.md](./operations.md) — operational runbooks for `migrate`, `diagnosis`, and `tick`, plus Sentry / observability.
- [integrations/](./integrations/README.md) — Notion, GitHub, and Jira integrations.
- [slack.md](./slack.md) — Slack app setup and OAuth scopes.
