# Agent Tools

This is the **single source of truth** for the tools the LLM agent can call,
and — just as important — **which tools are available in which runtime
context**. If you are writing a Job prompt, an `[assist]` prompt, or a per-case
agent note and need to name a tool, confirm it here first. A tool referenced in
a prompt must be one that is actually wired into that prompt's context;
naming a tool the context does not expose silently does nothing.

> Quick map for prompt authors:
> - **Naming a tool in a Job prompt?** Read [Tools available by context](#tools-available-by-context) — Jobs get the Slack read tools (`slack__search_messages` / `slack__get_messages`), Notion (`notion__*`), and Jira (`jira_*`), but a *narrower* write palette than the interactive mention agent and still **no GitHub** read tools.
> - **Wondering whether a Job may close / delete / post anywhere?** Read [Guardrails](#guardrails).
> - **Wiring an external integration (Notion / GitHub / Jira) on / off?** See [Integrations](integrations/README.md); the tools light up automatically when the service is configured.

The names below are exactly what the LLM sees (e.g. `case__update_case`). They
are grouped by the package that defines them under `pkg/agent/tool/`.

## Tool catalogue

### Core / Action tools (`core`, `actionwriter`)

Manage the case's Action items (Kanban work items). Read tools are available
wherever the case has Actions; write tools split into a full set (interactive
mention agent) and a Job-safe subset.

| Tool | R/W | Purpose | Notes |
|------|-----|---------|-------|
| `core__list_actions` | R | List the case's actions. | Optional `include_archived` (default `false`). |
| `core__get_action` | R | Fetch one action by id. | Scoped to the case the run is pinned to: an action of another case is reported as **not found**, with the same wording as a missing one so the other case's existence is not confirmed. An action id is a small integer a model can guess, or be told to fetch by text it read. |
| `core__list_action_steps` | R | List the binary-state steps under an action. | |
| `core__search_referenceable_cases` | R | Search the target workspace of a `case_ref` / `multi_case_ref` field for a case id to reference. | Only wired when the workspace defines such a field. Private and draft cases are excluded. |
| `core__get_referenceable_cases` | R | Batch-fetch full details of referenced cases. | Same gating as above. Set the value itself via `case__update_case`'s `fields`. |
| `core__create_action` | W | Create a new action. | |
| `core__update_action` | W | Update an action's title / description / assignee. | Status changes go through `core__update_action_status`. |
| `core__update_action_status` | W | Move an action to another status. | Action status is independent of case status; this is **not** a case close. |
| `core__set_action_assignee` | W | Set / clear the action assignee (empty string clears). | |
| `core__add_action_step` | W | Add a step to an action. | |
| `core__set_action_step_done` | W | Mark a step done / undone (idempotent). | |
| `core__rename_action_step` | W | Rename a step. | |
| `core__archive_action` | W | Archive an action (hidden from views, recoverable). | **Interactive mention agent only — NOT exposed to Jobs.** |
| `core__unarchive_action` | W | Restore an archived action. | **Interactive mention agent only — NOT exposed to Jobs.** |
| `core__delete_action_step` | W | Delete a step. | **Interactive mention agent only — NOT exposed to Jobs.** |

There is no destructive "delete action" / "delete case" tool anywhere; the
archive lifecycle replaces deletion.

### Case writer tools (`casewriter`)

Edit the case the agent is bound to. Wired for the case-bound mention agent, for
the thread-case mention agent's sub-agents (the `case_write` toolset), and for
Jobs (both channel- and thread-mode). The set is **all-or-nothing**: there is no
status-only or assignee-only subset, so a user who asks a mention agent for any
case edit is never answered with "I lack that tool".

| Tool | R/W | Purpose | Notes |
|------|-----|---------|-------|
| `case__update_case` | W | Update title / description / custom field values. | Title and description are **full replacements** — review current values (shown in the system prompt) before overwriting. Cannot change status or assignees. Unknown field ids and type / option mismatches are rejected with a correctable error. |
| `case__assign` | W | Add assignee(s) by delta (set union). | Rejects user ids absent from the SlackUser store. |
| `case__unassign` | W | Remove assignee(s) by delta (set difference). | Does **not** reject unknown ids (a since-deleted user must stay removable). Applied atomically server-side, so concurrent edits never clobber. |
| `case__update_case_status` | W | Move the case to another board status (a closed status closes the case). | Thread-mode only — present when the workspace has a configured case status set (`CaseStatusSet`). The parameter enumerates the configured status ids. The result carries `previous_board_status` and `changed` alongside `board_status`, so a call that set the status the case already had is recognisable as a no-op; nothing else reports one, since the thread announcement is emitted only on a real change. **See [Guardrails](#guardrails): Jobs are instructed not to close; the human-driven mention agent may.** |
| `case__close_case` | W | Mark the case done by closing it (`OPEN` -> `CLOSED`); takes no parameters. | Channel-mode only — the counterpart of `case__update_case_status`, built when the workspace has **no** `CaseStatusSet`. Exactly one "mark done" tool is offered per mode. Closing an already-closed or draft case is rejected with a correctable error. |

### Workspace agent cross-case tools (`casemulti`)

Wired **only** into the [workspace agent](configuration.md#workspace-agent)
(a channel-mode workspace's `workspace_channel`, or a channel-root mention in a
thread-mode workspace's monitored channel). Unlike every other tool above —
which is pinned to a single Case at construction — these take **`case_id` as a
call-time argument**, so one turn can read and act across many Cases. Every call
is access-checked against the **mentioning user's** permissions (private Cases the
user cannot access are filtered from lists and rejected on direct access), and
writes are subject to the workspace agent's [write guardrail](#guardrails)
(nothing is changed unless the user explicitly asks).

The set differs by case mode, because a thread-mode workspace has no Actions and
closes a Case by moving it to a closed board status:

| Tool | R/W | Mode | Purpose | Notes |
|------|-----|------|---------|-------|
| `case__list_cases` | R | both | List cases in the workspace (optional `status` filter). | Cases the user cannot access are omitted. |
| `case__get_case` | R | both | Fetch one case by `case_id`. | Denied for a private case the user is not a member of. |
| `case__create_case` | W | both | Create a new case (channel mode: dedicated channel + invites + welcome; thread mode: a new thread in the monitored channel). | Reporter is the mentioning user. |
| `case__update_case` | W | both | Update a case's title / description / fields (`case_id`). | Cannot change assignees — use `case__assign` / `case__unassign`. |
| `case__assign` | W | both | Add assignee(s) to a case by delta (`case_id`, set union). | Rejects user ids absent from the SlackUser store. |
| `case__unassign` | W | both | Remove assignee(s) from a case by delta (`case_id`, set difference). | Unknown ids are not rejected (a since-deleted user must stay removable). |
| `case__close_case` | W | channel | Close a case (`case_id`). | Channel mode only — built when the workspace has **no** `CaseStatusSet`. |
| `case__update_case_status` | W | thread | Move a case to another board status (`case_id`, `status`); a closed status closes it. | Thread mode only — the counterpart of `case__close_case`. The `status` parameter enumerates the configured status ids. The result carries `previous_board_status` and `changed`, as in the case-bound tool above. Exactly one "mark done" tool is offered per mode. |
| `case__list_actions` | R | channel | List a case's actions (`case_id`). | Thread-mode workspaces manage no Actions, so none of the action tools are wired there. |
| `case__get_action` | R | channel | Fetch one action (`case_id`, `action_id`). | Verifies the action belongs to the case. |
| `case__create_action` | W | channel | Add an action to a case (`case_id`). | |
| `case__update_action` | W | channel | Update an action (`case_id`, `action_id`). | Change attributed to the mentioning user. |
| `case__update_action_status` | W | channel | Move an action to another status. | |
| `case__add_action_step` | W | channel | Add a step to an action. | |
| `case__set_action_step_done` | W | channel | Mark a step done / undone. | |

### Slack tools (`slack`, `slackpost`)

| Tool | R/W | Purpose | Notes |
|------|-----|---------|-------|
| `slack__search_messages` | R | Workspace-wide message search (`search.messages`). | Requires a Slack **User** OAuth token with `search:read`. See [slack.md](slack.md#user-token-scopes). Wired into the interactive / investigation contexts **and into Jobs** (both modes) when the User token is configured. |
| `slack__get_messages` | R | Bulk-fetch 1–10 messages with thread context (parallel, partial failure tolerated). `order: "latest"` keeps the thread root and the newest replies of a long thread instead of its first messages. | Wired into the interactive / investigation contexts **and into Jobs** (both modes) when a Slack service is configured. Reads via the User token when present, else via the bot if it is a channel member. Thread-mode Jobs use this to read their case thread first (the thread's `slack_thread_ts` is in the Job system prompt). |
| `slack__post_message` | W | Post a message to the case's Slack channel (supports `thread_ts`). | Used by the assist / mention flow, where the agent posts where it directs. Not suppressed by a Job's `quiet`. |
| `slack__post_to_case_channel` | W | Post a message to the case's bound channel. | **The only Slack *write* tool a Job gets** (Jobs also get the read tools above). The channel id is hard-pinned to `Case.SlackChannelID`; arbitrary channels are not reachable. Wired only when a Slack service is configured and the case has a bound channel. Both `serve` and `tick` must be given a Slack bot token — a sweep executes the runs it dispatches, so a `tick` without one leaves this tool unbuilt. The same holds for every other integration on the Job palette (Notion, GitHub, Jira, WebFetch): see [cli.md](cli.md#tick). |

**Both Slack read tools are size-bounded.** `--slack-tool-max-text-size` cuts one
message's `text`, and `--slack-tool-max-result-size` bounds the combined size of
the messages one call returns; the response then reports `text_truncated` per
message and `returned` / `omitted` / `truncated` for the call, so a partial
result is not read as a complete one. The bounds exist because `count` is a tool
parameter the model fills in, while per-message length is bounded by nothing at
all, and a sub-agent re-sends its accumulated tool results on every later turn.
See [cli.md](cli.md#slack-agent-tool-limits) for the defaults and the response
fields.

**A planner is only offered toolsets that resolve to a tool.** The palettes above
are the *ceiling* on what a host may advertise, not what it does advertise: before
each Spawn, a plan-execute host filters its palette through
`agentkernel.ToolSetProbe`, which builds the same resolver the tool factory uses
and drops every id that would yield nothing. So a deployment without Notion never
sees `notion` in a plan, and a case with no Slack channel never sees `slack_post`.
Without this the planner assigns a task a toolset its sub-agent never receives,
and the run dies on `unknown tool` — which is exactly what happened when
`slack__post_to_case_channel` was advertised on a deployment that built no poster.
When you add a toolset whose tools are conditional, make sure the condition is
visible to `ToolSetResolver.Has`.

**A planner's own tools only read.** The planner of a plan-execute agent (Jobs
with the `planexec` strategy, the thread-case agent, the workspace agent, the
case-draft agent) may call tools itself before it decides, but it is given only
the read tools of its palette: `core_ro` for the action tools, `slack_ro` for
Slack, `memo_ro` (`memo__list_memos` / `memo__get_memo`), `case_multi_ro`
(`case__list_cases` / `case__get_case` and the action reads), the read-only
integrations, and the knowledge read tools. Every write — a post, a case or
action edit, a memo or knowledge change — is done by a sub-agent whose task was
assigned the write toolset. A write the planner calls anyway is answered with
`unknown tool` and does not run. `memo_ro` and `case_multi_ro` are never offered
to a planner as task toolsets. When you add a toolset, classify it in
`agent.ReadOnlyToolSetIDs`: an unclassified id fails the planner's claim rather
than reaching it with its writers.

**Sub-agents are told the ids their tools are pinned to.** A plan-execute run
spawns one sub-agent per planned task, and that sub-agent's system prompt is
built from the planner's task text — not from the host's. Its tools, however,
are pinned to the run's subject. So every plan-execute host passes the run's
identifiers (`workspace_id`, `case_id`, `slack_channel_id`, `slack_thread_ts`)
as a context block rendered into each sub-agent's prompt. Without it a task told
to "read the case thread" has to invent a channel id and a timestamp, and
`slack__get_messages` rejects the call. When you add a tool that takes an
identifier the host already knows, put that identifier in the block
(`agent.TaskContext`) rather than relying on the planner to copy it into the
task description.

### Knowledge tools (`knowledge`)

Workspace-wide shared Knowledge (semantic + keyword searchable). Tags are
first-class entities referenced by id; use `knowledge__list_tags` to discover
existing tag ids before creating entries or new tags. Read tools are always
offered; write tools are gated (see notes).

| Tool | R/W | Purpose | Notes |
|------|-----|---------|-------|
| `knowledge__search_knowledge` | R | Search workspace knowledge (semantic + keyword, optional `tag_ids` filter). Results carry `tag_ids`. | |
| `knowledge__get_knowledge` | R | Fetch a knowledge entry (title, Markdown claim, `tag_ids`) by id. | |
| `knowledge__list_tags` | R | List all tags in use; returns objects with `id` and `name` (not plain strings). Call this first before creating tags or knowledge entries. | |
| `knowledge__create_tag` | W | Create a new tag; returns its `id`. Must call `knowledge__list_tags` first to avoid creating duplicates. | Write is **withheld while the agent runs against a PRIVATE case**. |
| `knowledge__update_tag` | W | Rename an existing tag by `id`. | Same private-case gating. |
| `knowledge__delete_tag` | W | Delete a tag by `id`. Succeeds only when no knowledge entry references it. | Same private-case gating. |
| `knowledge__create_knowledge` | W | Create a knowledge entry (Markdown claim, `tag_ids` of pre-existing tags — at least one required). | Same private-case gating. |
| `knowledge__update_knowledge` | W | Update a knowledge entry's title / claim / `tag_ids` (full replacement of tag list; omit to preserve). | Same private-case gating. |

### Memo tools (`memo`)

Per-case memos. Wired only when the workspace defines a `[memo]` section with
at least one memo field.

| Tool | R/W | Purpose |
|------|-----|---------|
| `memo__list_memos` | R | List one page of the case's memos, newest first. Archived memos are never returned. `limit` (default 10, max 50) and `offset` (default 0) page the result; the response carries `offset`, `total_count`, `returned_count`, `has_more`. Optional creation-time window `created_after` / `created_before` (RFC3339). |
| `memo__get_memo` | R | Fetch a memo by id. |
| `memo__apply_memo_changes` | W | Apply every memo change in one call: `creates` (title + field values), `updates` (title / fields, omit to preserve; at least one required), and `archives` (ids to soft-delete, recoverable). Up to 50 entries per call. |

There is deliberately no per-memo write tool. Each tool call is one LLM round
trip and every round trip re-sends the whole message history, so writing memos
one at a time cost (number of mutations × context size). Batching them removes
that multiplier.

Entries are applied in order — every create, then every update, then every
archive — and independently: the response carries a per-entry outcome
(`results[]` with `op` / `index` / `ok` and either `memo` or `error`, plus
`applied` / `failed` counts). A failed write is additionally reported through
`errutil.Handle`; a malformed argument is not, since the model can repair it by
re-sending the call.

Whether a bad entry costs only itself depends on where it is caught. gollem
validates the arguments against the tool schema *before* the tool runs, and
rejects the whole call on the first violation it finds:

| Failure | Effect |
|---------|--------|
| Missing `title` / `memo_id` / `field_id`, an empty archive id, an update with neither `title` nor `fields`, an unknown field id or option value, a missing memo, an inaccessible case, a failed write | Only that entry fails; every other entry is still applied |
| A value whose **type** does not match the schema (`creates` not an array, a numeric `title`, a non-string archive id) | The whole call fails as a tool error and the model has to re-send it |
| Every array absent or empty, or more than 50 entries in total | The whole call fails as a tool error |

Requiredness is enforced while parsing each entry rather than through the
schema's `Required` flag, which is what keeps the first row per-entry: a nested
`Required` violation would make gollem reject the batch as a whole.

### Notion tools (`notion`)

Wired when `HECATONCHEIRES_NOTION_API_TOKEN` is set. See [Integrations → Notion](integrations/notion.md).
Available in the investigation / interactive contexts **and in Jobs** (both modes).

| Tool | R/W | Purpose |
|------|-----|---------|
| `notion__search` | R | Search Notion pages and databases shared with the integration (title match). |
| `notion__get_page` | R | Retrieve a page's content as Notion-flavored Markdown. Page ids only — a database id belongs to `notion__get_database`. |
| `notion__get_database` | R | Describe a database — its data sources and every column's name and type — and list the rows it holds, so a `database` search hit can be read too. |
| `notion__search_database` | R | Search one database's rows: keywords ANDed across the properties you name, property-value conditions, ordering, and the column values to return. |

### GitHub tools (`github`)

Wired when the GitHub App flags are set. See [Integrations → GitHub](integrations/github.md).
Investigation / interactive contexts only — **not** wired into Jobs.

The repositories these tools can read are exactly those the GitHub App
installation covers. GitHub answers 404 both for a repository that does not
exist and for one the installation cannot see, so a lookup that 404s is
re-checked against the repository itself: when the repository is out of reach
the tool says so and names the owner and repository it tried, instead of
reporting the issue, PR, file, or ref as missing. The agent needs the two apart
— it varies the number after "not found", but has to correct the owner when the
repository is unreachable.

| Tool | R/W | Purpose |
|------|-----|---------|
| `github__search` | R | Search issues / PRs with GitHub search syntax (`repo:`, `is:open`, `author:`, `label:`, …). Up to 50 hits. |
| `github__get_issue` | R | Fetch an issue (not a PR) with body, labels, and comments. |
| `github__get_pull_request` | R | Fetch a PR with body, comments, reviews; optional `include_files=true` adds the diff. |
| `github__get_file` | R | Fetch a file's content at any ref. UTF-8 text only; capped at 1 MB. |
| `github__list_commits` | R | List commits with optional `path` / `author` / `since` / `until` filters. |

### Jira tools (`jira`)

Read-only Jira Cloud integration (`gollem-dev/tools/jira`, wrapped by
`pkg/agent/tool/jira`). Wired when `--jira-base-url` / `--jira-email` /
`--jira-api-token` (or the matching `HECATONCHEIRES_JIRA_*` env vars) are all
set. See [Integrations → Jira](integrations/jira.md).
Available in every context, including Jobs — unlike GitHub, which is
interactive/investigation only.

| Tool | R/W | Purpose |
|------|-----|---------|
| `jira_list_projects` | R | List projects accessible to the account (id, key, name, type, lead), with pagination. |
| `jira_search_issues` | R | Search issues with JQL; a `project` argument is spliced into the JQL via AND. Returns key, summary, status, type, assignee, priority, updated time. |
| `jira_get_issues` | R | Fetch one or more issues by key/id in a single batch (up to 100); descriptions and optional comments are rendered to Markdown. |

### Web fetch tool (`webfetch`)

| Tool | R/W | Purpose | Notes |
|------|-----|---------|-------|
| `webfetch` | R | Fetch an HTTP(S) URL and return it as Markdown. | Blocks non-public IPs and screens the result for indirect prompt injection before returning it. Wired when a web-fetch client is configured. The HTTP status is returned rather than raised as an error. If extraction yields no text, the tool returns an empty `result` with the HTTP status without calling the analyzer. |

An `application/pdf` response is sent to the screening model as a document
rather than extracted to text, so the same call screens it and renders it to
Markdown. Two consequences for an operator:

- A PDF has its own size cap (`--webfetch-max-pdf-size`, default 2 MiB) because
  it needs one an order of magnitude above the text cap. A PDF over the cap is
  **refused** rather than truncated — a truncated PDF keeps its `%PDF-`
  signature, so the model would read a broken file as a complete one.
- Three failures on this path are logged at `INFO` and **not** reported to
  Sentry: a media type the tool cannot read, a PDF over the cap, and a body that
  claims `application/pdf` but is not a PDF. The URL was chosen by the model, so
  none of them is an operator's defect. See [operations.md](operations.md) §
  What is not reported.

Any other media type is still rejected, including a PDF a server labels
`application/octet-stream` — the tool does not sniff a body's contents.

### Planner metadata tools (`wsmeta`)

Used **only** by the proposal (case-draft) flow — not by Jobs, the mention
agent, or the other hosts' sub-agents. Every other host already knows which
workspace it runs in and hands that workspace's schema to its tools directly;
the draft flow is the one that must choose.

The planner calls them itself, before it decides anything: a plan-execute
planner may make read-only tool calls, each as its own transition, and only then
emits its plan. A planned task may also request the `wsmeta` toolset.

| Tool | R/W | Purpose |
|------|-----|---------|
| `list_workspaces` | R | List id / name / description of all registered workspaces. |
| `get_workspace` | R | Fetch a workspace's identity, full field schema (with option metadata), and sources. The planner must call this before proposing a case so it uses exact field / option ids. |

## Tools available by context

The agent runs in several contexts, and **each wires a different subset**. This
matrix is the answer to "can my Job call `slack__get_messages`?" (yes) or "can
the mention agent close a case?" (yes — see [Guardrails](#guardrails)).

| Tool group | Mention agent (channel-mode case) | `assist` batch | Job — channel-mode | Job — thread-mode | Thread-case investigation | Proposal sub-agent (case draft) |
|------------|:---:|:---:|:---:|:---:|:---:|:---:|
| `core` read + Actions (`actionwriter`) | ✓ (full, incl. archive / delete-step) | ✓ (full) | ✓ (Job subset, no archive / delete-step) | — (thread mode has no Actions) | — | read-only `core__list_actions` / `core__get_action` |
| `case__*` (casewriter) | ✓ | — | ✓ | ✓ | ✓ (mention turns only; no case exists on a create turn) | — (no case exists yet) |
| `slack__search_messages`, `slack__get_messages` (read) | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `slack__post_message` | — | ✓ (pinned to the case channel) | — | — | — | — |
| `slack__post_to_case_channel` | — | — | ✓ | ✓ | — | — |
| `notion__*` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `github__*` | ✓ | ✓ | — | — | ✓ | ✓ |
| `jira_*` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `webfetch` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `knowledge__*` (incl. tag CRUD) | ✓ (write if case is non-private) | — | ✓ (write if case is non-private) | ✓ (write if case is non-private) | read-only | read-only |
| `memo__*` | ✓ (if memos enabled) | — | ✓ (if memos enabled) | ✓ (if memos enabled) | — | — |
| `wsmeta` | — | — | — | — | — | ✓ (and the planner itself) |

Notes:

- **"Mention agent"** is the case-bound agent that runs when a human @-mentions
  the bot in a channel-mode case channel (`pkg/usecase/agent/casebound`). It
  gets the widest palette because a human is in the loop.
- **`assist`** is the batch pass over every open case (`hecatoncheires assist`).
  Its palette is the action tools plus Slack read **and post**: posting its
  findings into the case channel is the whole point of the command, so unlike
  the mention agent it gets `slack__post_message` as a tool it may call at will.
  It has no case-editing, memo, or knowledge tools. A ✓ above still requires the
  integration to be wired, and the `assist` command wires only the Slack **bot**
  token today — so Notion / GitHub / Jira / web fetch and the User-token Slack
  search resolve to nothing there in practice. (Source of truth:
  `agent.KnownToolSetIDsAssist`.)
- **Jobs** (`[[job]]`, both `simple` and `planexec` strategy) run **unattended**.
  They get case + action writes, knowledge, memo, web fetch, the Slack read
  tools (`slack__search_messages` / `slack__get_messages`), Notion read tools,
  Jira read tools, and a single channel-pinned Slack *post* tool — but a
  narrower *write* palette than the mention agent (no archive / delete-step)
  and **no GitHub** read tools. Jira is the one integration wired into Jobs
  that GitHub is not: it carries no repository-scoped write surface to worry
  about, so it follows the default "non-Action tools go everywhere" rule
  instead of GitHub's carved-out exception.
  The Slack read tools let a thread-mode Job read its own case thread before
  acting (the thread's `slack_thread_ts` is supplied in the Job system prompt),
  which is exactly why they are wired: a Job told to "read the thread first"
  must have a tool to do so, or it will misuse the post tool instead. (Source of
  truth for Job wiring: `buildJobTools` in `pkg/cli/job_runtime.go`.)
- **Thread-mode** workspaces have no Actions, so the whole `core` / Action
  surface is absent there.
- **Proposal sub-agents** are read-only investigators: no case exists yet, so the
  draft is materialised by the host. **Thread-case mention sub-agents and Job
  (`planexec`) sub-agents may use write tools** to carry out the requested change
  once the planner has gathered enough context — the planner assigns the write
  toolset (`case_write` for thread-case) to a dedicated task. This is gated by
  `RunRequest.AllowSubAgentWrites` (true for Jobs and for thread-case mention
  turns, false for thread-case *create* turns); the resolver still bounds which
  tools each sub-agent can physically call, so the flag and the tool set always
  agree.
- A thread-case mention turn can change case content two ways — `case__update_case`
  inside the loop, or the terminal `materialize` decision. Since `materialize`
  replaces title and description wholesale, the system prompt instructs the
  planner to pick one path per turn. That is a **prompt-only** guardrail.

## Guardrails

Some restrictions are enforced in **code** (the tool simply isn't wired, so the
agent cannot call it) and some are enforced only by the **system prompt** (the
tool exists, but the agent is instructed not to use it a certain way). Knowing
which is which matters: a prompt-only guardrail is a strong instruction, not a
hard lock.

| Restriction | Applies to | Enforcement |
|-------------|-----------|-------------|
| **A Job will not close a case.** Closing a case is a human-only decision. | Jobs (both strategies) | **Prompt only.** `case__update_case_status` can technically set any configured status, including a closed one; the Job system prompt instructs the agent not to. The interactive mention agent (human-initiated) *may* move a case to a closed status. |
| **No deleting cases.** | All agent contexts | No delete tool exists anywhere (archive replaces delete). |
| **A Job will not archive actions or delete action steps.** | Jobs | **Code.** `core__archive_action` / `core__unarchive_action` / `core__delete_action_step` are not wired into the Job palette. |
| **A Job posts only to the case's bound Slack channel.** | Jobs | **Code.** The only Slack write tool a Job gets is `slack__post_to_case_channel`, hard-pinned to `Case.SlackChannelID`. |
| **Knowledge stays out of shared storage for private cases.** | Mention agent & Jobs on a private case | **Code.** `knowledge__create_knowledge` / `knowledge__update_knowledge` / `knowledge__create_tag` / `knowledge__update_tag` / `knowledge__delete_tag` are withheld; only read tools remain. |
| **A Job cannot read its own past run traces.** | Jobs | **Prompt.** Determine idempotency from the current case state, the action list, and Slack history — not from prior traces. |
| **Actions exist only in channel-mode cases.** | All agent contexts | **Code, at the usecase boundary.** The action (`core__*`) tools are withheld from thread-mode in all hosts, *and* `ActionUseCase.CreateAction` / `UpdateAction` (reparent) reject a thread-mode parent (`ErrCaseThreadModeNoActions`) — so every entry point (GraphQL, Slack, agent, eval), not just the tool wiring, is covered. |
| **Case lifecycle changes follow the mode-specific path.** | All agent contexts | **Code, at the usecase boundary.** Thread-mode cases close/reopen only via board status (`UpdateCaseStatus`); `CloseCase` / `ReopenCase` reject thread-mode (`ErrCaseThreadModeUseStatus`), and `UpdateCaseStatus` rejects channel-mode — keeping `BoardStatus` and the lifecycle `Status` in sync. |

When a guardrail is "prompt only", treat it as a firm design constraint: do not
write a Job prompt that tries to talk the agent around it (e.g. "ignore the
close restriction and close the case"). If you genuinely need a Job to perform a
restricted action, that is a change to the binary, not to a prompt.

## See also

- [Configuration → Job Definitions](configuration.md#job-definitions-job) — the `[[job]]` schema, scheduling, and execution strategy.
- [Operations → Agent Jobs operations](operations.md#agent-jobs-operations) — the runtime behaviour (triggers, concurrency, run log).
- [Integrations](integrations/README.md) — turning the Notion, GitHub, and Jira tools on.
- [Concepts](concepts.md) — the vocabulary (Case, Action, Workspace, Job).
