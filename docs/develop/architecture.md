# Architecture (internals)

This document explains the "why" and "how" of Hecatoncheires' internal design for human contributors. It complements — but does not replace — the machine-enforced rules in [`.claude/rules/`](../../.claude/rules/) and the project context in [`CLAUDE.md`](../../CLAUDE.md). For the developer documentation index, see [develop/README.md](./README.md).

## GraphQL DataLoader

The GraphQL layer uses a request-scoped DataLoader pattern (via
[`github.com/graph-gophers/dataloader/v7`](https://github.com/graph-gophers/dataloader))
to collapse N+1 fetches that arise when a list query renders sub-resolvers
for each row.

### Where it lives

- `pkg/controller/graphql/dataloader.go` — loader definitions, batch
  functions, request-context plumbing
- `pkg/cli/serve.go` — middleware that instantiates one
  `*DataLoaders` per HTTP request before invoking the gqlgen handler
- `pkg/controller/http/graphql_test.go` — the same per-request
  wiring on the test side, so resolver tests exercise the real
  batching path

### Why request-scoped

- The internal cache MUST NOT survive across requests. A loader that
  outlives one request would leak one user's view to another (private
  cases, restricted assignees) and break the multi-instance safety
  invariant in CLAUDE.md.
- `dataloader.NewBatchedLoader` is cheap; constructing seven loaders
  per request (`SlackUser`, `SlackChannelName`, `Action`, `Case`,
  three `ActionsByCase` scopes) is below noise on every CPU profile.
- Batching only collapses calls *inside* one batch tick anyway — the
  graph-gophers wait window is 16 ms by default — so a per-request
  loader is the longest meaningful scope.

### What gets batched

| Loader | Batch source | Solves N+1 on |
|---|---|---|
| `SlackUser` | `repo.SlackUser().GetByIDs(ctx, ids)` | `Case.reporter`, `Case.assignees`, `Case.channelUsers`, `Action.assignee`, `ActionEvent.actor` |
| `SlackChannelName` | `slackSvc.GetChannelNames(ctx, ids)` | `Case.slackChannelName` (the original Cases-page hotspot) |
| `Action` | `repo.Action().GetByIDs(ctx, ids)` | future Action sub-resolvers |
| `Case` | `repo.Case().GetByIDs(ctx, ids)` | `Action.case`, `Action.steps`, `Action.events`, `Action.messages`, `Action.stepProgress` |
| `ActiveActionsByCaseLoader` / `Archived` / `All` | `repo.Action().GetByCases(ctx, caseIDs, opts)` | `Case.actions` |

`SlackUser`, `Action`, and `Case` repositories all expose `GetByIDs`
returning a `map[K]*V`; missing IDs are silently absent (callers
distinguish "missing" from "found" themselves). The DataLoader batch
function fans those map results back out into per-key `Result` entries
in the order the dataloader supplied keys.

### Calling convention from resolvers

```go
// single load
user, err := loaders.SlackUser.Load(ctx, *obj.ReporterID)()

// many loads (returns []V, []error — per-key parallel arrays)
users, errs := loaders.SlackUser.LoadMany(ctx, obj.AssigneeIDs)()

// composite-key load (Case, Action, ActionsByCase)
c, err := loaders.Case.Load(ctx, MakeCaseKey(workspaceID, caseID))()
```

Each `Load` returns a `Thunk[V]`; calling the thunk is what actually
waits for the batch to fire. gqlgen runs sub-resolvers concurrently, so
the parallel `Load` calls all enqueue within the wait window and the
first thunk to be called triggers the single `GetByIDs` / batch fetch.

#### Handling missing keys

- `*SlackUser`: missing IDs return `Data: nil` (no error). The
  resolver decides: `Case.reporter` returns a field-level
  `ErrSlackUserNotInRepo` because the empty cell is the original
  bug; `Case.assignees` filters nils because `[SlackUser!]!`
  requires non-null elements; `Action.assignee` returns nil
  directly because the schema field is nullable.
- `*SlackChannelName`: returns `Data: nil` for IDs that the Slack
  service did not resolve. The resolver passes that through as a
  null GraphQL field.
- `Case` / `Action`: missing IDs return `Data: nil`. Resolvers that
  treat absence as "not visible to this requester" check for nil
  and return empty results (e.g. access-denied paths).

### Adding a new loader

1. Add a batch method on the relevant repository / service that
   takes a `[]K` and returns a `map[K]*V` (preferred — easier to
   reorder than a slice).
2. In `dataloader.go`:
   - Add a field on `DataLoaders`.
   - Wire it in `NewDataLoaders`.
   - Write a `buildXxxBatch` closure that:
     - Dedupes / normalises keys
     - Calls the repository once
     - Emits one `*dataloader.Result[V]` per input key in order,
       using `Data: nil` for legitimate "not found"
3. Replace per-row repository calls inside resolvers with
   `loaders.Xxx.Load(ctx, key)()` / `LoadMany(ctx, keys)()`.
4. Add (or extend) the regression test in
   `pkg/controller/graphql/dataloader_test.go` that wraps the real
   repository with a call counter and asserts the batch ran exactly
   once for the workload.

### Why we didn't keep the old "fake DataLoader"

Before this rewrite, `pkg/controller/graphql/dataloader.go` exposed
types named `SlackUserLoader`, `ActionLoader` etc. but each was just a
batch-fetch helper: `Load(ctx, ids)` made one repository call and
returned. Resolvers called it per row (`Load(ctx, []string{singleID})`)
because there was no debounce layer, so a 20-case list page issued 20
`SlackUser.GetByIDs` calls for reporter, 20 more for assignees, and 20
Slack API calls for channel names — even with caching on top. The
graph-gophers loader collapses each of those to one call per request.

## Workspace-scoped identity on the GraphQL wire

Most GraphQL clients — Apollo Client among them — normalize a response into a
cache keyed by `__typename` plus `id`, which assumes `id` is globally unique.
In this API it is not. Case and Action ids come from a per-workspace counter
(`counters/case/workspaces/{workspaceID}`, `pkg/repository/firestore/case.go`),
and Job ids are workspace-unique kebab-case keys from the workspace's TOML.
The first Case of every workspace is id 1.

So each of these types carries a `workspaceId` field, and its identity on the
wire is the pair `(workspaceId, id)`:

- `Case`, `Action`, `CaseRef`, `CaseJob`

**Every selection set that fetches one of these MUST also select
`workspaceId`.** The frontend keys its cache on the pair
(`frontend/src/graphql/cache.ts`); when the field is missing Apollo cannot
compute the key and silently degrades to not normalizing that object, so the
omission produces no error — only a stale-looking UI later.

### Why some types are deliberately *not* keyed

`FieldDefinition`, `FieldOption` and `ActionStatusDefinition` are configuration
value objects, not entities. Their `id` is a config key that is only meaningful
inside the enclosing configuration document, and the same id is reused both
across workspaces and across the two independent documents that carry them
(`FieldConfiguration.fields` for Cases vs `MemoConfiguration.fields` for Memos;
`fieldConfiguration.actionConfig` vs `caseStatusConfig`). No combination of
their fields identifies one, so adding `workspaceId` would not have been
enough — the frontend disables normalization for them instead.

### What went wrong before this was explicit

`Case` carried the workspace only as an internal Go field (`json:"-"`) used to
give sub-resolvers their scope, and never exposed it. The Home dashboard
aggregates open Cases across every workspace, so a single response could
contain `Case:12` twice; the second write replaced the first and both rows
rendered one workspace's title, assignees and `updatedAt` under the other's
(correct) workspace badge. The same collision made a cache-first return to a
previously visited workspace's Case list show another workspace's data.

## Agent thread session (internals)

The agent that responds to `@mention` in Slack threads treats each thread as
a long-running **Session** (`pkg/domain/model.Session`). The session ties a
Slack thread to either a Case (case-bound mode, when the channel is bound to
an existing Case) or to a draft-in-progress (open mode, when the bot is
mentioned in an unbound channel). It persists the gollem conversation history
so follow-up mentions can pick up where the previous turn left off, and
writes a Trace blob for every turn for diagnostics.

In case-bound mode the agent can edit the bound Case directly via the
`case__update_case` (title / description / custom fields), `case__assign` /
`case__unassign` (delta assignee changes), and a mode-specific "mark done" tool
— `case__update_case_status` for thread-mode workspaces (move to a closed board
status) or `case__close_case` for channel-mode cases (close OPEN -> CLOSED) —
the same tools the
event-driven Agent Jobs use. They funnel through `CaseUseCase.UpdateCase` /
`AssignCase` / `UnassignCase` / `UpdateCaseStatus` / `CloseCase`, so every entry point (Web
GraphQL, Slack modal, Job, mention agent) enforces the same validation,
including the SlackUser existence check on newly assigned users and user-typed
field values. Assignees are mutated only through the delta `AssignCase` /
`UnassignCase` path (never as a full-list replace on `UpdateCase`), so
concurrent edits cannot clobber one another.

Two turns never run concurrently on the same thread: every turn is spawned on
the agent runtime under the thread's **subject**, and the runtime admits one
live run per subject. A trigger that arrives while a run is live is refused
(`ErrSubjectBusy`) and the host posts the "already handling your previous
request" notice.

#### Case-mode invariants (enforced at the usecase boundary)

A Case is either **channel-mode** (`SlackThreadTS == ""`; dedicated channel) or
**thread-mode** (`SlackThreadTS != ""`; bound to a Slack thread, tracked by a
configurable board status / Kanban). Two invariants follow from this split and
are enforced at the **usecase boundary** — not just by withholding agent tools —
so every entry point (GraphQL, Slack, agent tools, eval) is covered uniformly:

- **Lifecycle path is mode-specific.** Thread-mode cases change lifecycle only
  by moving their board status (`UpdateCaseStatus`, which keeps `BoardStatus` and
  `Status` in sync); `CloseCase` / `ReopenCase` reject thread-mode cases
  (`ErrCaseThreadModeUseStatus`). Symmetrically, `UpdateCaseStatus` rejects
  channel-mode cases (no board status). The Web UI mirrors this: the
  close/reopen button shows only for channel-mode, the Kanban only for
  thread-mode.
- **Actions exist in both modes; their Slack notifications differ.** The
  configurable status attaches to the Case itself in thread mode, but a
  thread-mode Case still has Actions, created and managed from the Web UI. A
  thread-mode Case has no Action cards: creating an Action posts a one-line
  context-block reply in the Case thread, and every later notification (change,
  step, comment, archive) is a context-block reply in that same thread naming
  the Action, never broadcast and never folded into the channel notification
  slot. The routing lives in `resolveActionNotifyTarget` / `postActionNotice`
  (`pkg/usecase/action.go`), shared by the action, step and comment usecases.
  A thread-mode workspace has Actions only when it sets `[case] actions = true`
  (default off, so existing workspaces are unaffected): `CreateAction` and the
  reparent path reject a Case of a workspace without them
  (`ErrCaseActionsDisabled`), and the Web UI reads `actionsEnabled` to hide the
  section. On the agent side only the thread-case **mention** turn of such a
  workspace gets the action (`core__*`) tools: its scope sets
  `Scope.ThreadModeActions`, which its sub-agents inherit, and the kernel
  withholds the core toolset (`OmitCore`) from every other run there — create
  and materialize turns, Jobs (runtime and eval env) and the workspace agent,
  whose prompts are built around the Case board status.
- **Thread-mode Jobs embed the thread's recent messages in the system prompt.**
  Because a thread-mode Job has no Actions to anchor on, `JobRunner` loads the
  Case's recent Slack messages (`CaseMessageRepository.List`, bounded to the
  newest `recentMessageMaxCount` = 32 within `recentMessageWindow` = 24h of the
  run start, ordered oldest-first) and renders them in a dedicated system-prompt
  section. Each body is rune-truncated to `recentMessageTruncateRunes` = 140 with
  the original character count annotated when elided. Channel-mode Jobs skip this
  read entirely (the section is absent from their prompt).
- **OPEN-case creation is mode-decided by one funnel.** Both `CreateCase` and
  `SubmitDraft` (draft promotion) route through the shared `openInWorkspaceMode`
  funnel, so the mode decision is made once and every creation entry point stays
  consistent. In a thread-mode workspace both paths bind the new case to a
  freshly-posted **monitored-channel thread** and never provision a dedicated
  channel — draft submission included. Thread-mode activation **fails closed**
  (`ErrThreadModeSlackUnconfigured`) when the deployment has no Slack service or
  the workspace has no monitor channel, rather than silently falling back to a
  plain create; and a private case in thread mode is rejected
  (`ErrCasePrivateThreadModeUnsupported`), which on the `SubmitDraft` path rolls
  the draft back to DRAFT with no Slack binding.

### State persistence across turns

The turn lifecycle persists several pieces of state so that a follow-up
mention resumes where the previous turn left off:

- For new sessions, the full thread context is folded into the system
  prompt. For continuing sessions, only **unprocessed** thread messages
  (those with `ts > LastMentionTS` and `userID != botUserID`) are
  surfaced to the agent as user input.
- The agent runs against gollem with `WithHistoryRepository` so each LLM
  turn auto-persists to Cloud Storage. A trace.Recorder is also attached
  so the per-turn execution graph (LLM calls, tool calls, sub-agents) is
  captured.
- After the response is posted, `LastMentionTS` is updated to the current
  mention's TS so the next mention only ingests truly new chatter.

If the mention thread happens to live under an Action notification message
(matched via `Action.SlackMessageTS`), the session records the `ActionID`.

### Thread-mode case initialization (deferred, agent-driven)

In thread mode, case creation is initiated **only** by a post at the channel
root (a top-level message in the monitored channel). A **human** root post
always qualifies. A **bot-authored** root post qualifies only when the workspace
opts in via `[slack] accept_bot` (default off) — otherwise a
channel would spawn a Case for every bot notification. `isThreadCaseCreationTrigger`
rejects replies, edits, system events, and our own bot's posts; a
`bot_message`/`bot_id` post additionally requires the opt-in flag. This is
deliberate: in opted-in channels the case-creating signal is often an intake-form
app's relayed request.
The reporter is, as a rule, the post's author; only when the author is a bot
does `HandleThreadCaseCreation` fall back to resolving it from the first Slack
user mention in the body (the requester named in the form). When none is
present the reporter stays empty: thread-mode Cases are exempt from the
mandatory-reporter rule (`model.Case.ValidateNew` requires `ReporterID` only for
channel-mode Cases), so creation still proceeds and the GraphQL `reporter` field
resolves to null. A mention or a reply inside a thread that is not bound to a Case is
ignored — activity inside an arbitrary thread never starts a Case. A
channel-root post does **not** create a Case immediately, though:
`HandleThreadCaseCreation` runs the `threadcase`
plan-and-execute agent in
`ModeCreate`: it investigates (read-only search tools), may ask the reporter a
question (terminal `question` action → the turn ends and waits), and only
commits a Case once it produces a final `create` decision that passes full
field validation. The create turn runs `planexec.Run[CreateDecision]` with a
**validation-only host finalizer**: the planner declares completion with an
explicit `finalize` action, planexec generates + shape-validates the structured
`CreateDecision`, then runs the finalizer inside its final-output regeneration
loop. The finalizer validates the decision against the workspace field schema; a
schema-validation failure (e.g. a non-RFC3339 `due_date`, a missing required
field, an out-of-schema option) is fed back to the planner and the final output
regenerated (bounded by `finalOutputMaxRetry`), so the model corrects the value
in-loop instead of the turn dying with no feedback. The Case is then committed
**after** the turn via `Handler.Create` → `CaseUC.createThreadBoundCase`.
The two failure kinds take different paths on purpose: a field error is the
model's fault and model-fixable (fed back for regeneration), whereas a
persistence failure is an infrastructure error the model cannot repair by
re-emitting the same JSON — it is surfaced and the turn falls back rather than
wasting a regeneration cycle. On success the host posts a Block Kit summary; on
retry/budget exhaustion or a persistence failure it posts a fallback notice.

Because a `question` ends the turn (a run cannot stay live for the minutes or
hours a Slack reply may take), the task can span multiple turns. A
pending question is answered through the question form's **Submit** interaction
(`HandleThreadCaseQuestionSubmit`), which resumes the create agent via
`runThreadCaseCreation` — free-text replies / mentions in the not-yet-a-case
thread are intentionally ignored. (`ResumeThreadCaseCreation` still drives this
resume directly, but in production it is reached only by the offline eval
harness.) The **same** thread Session (and therefore the same gollem history
key) is reused across the initial turn, any question/answer resume turns, and
the later case-bound mention turns — so the conversation history is one
continuous thread. The created case id is stamped onto the Session
(`Session.CaseID`) without changing `Session.ID`. See the agent runtime
vocabulary (turn / round / budget) in `.claude/rules/architecture.md`.

### Storage layout

Configurable via two CLI flags / environment variables:

| Flag | Env | Required | Purpose |
| --- | --- | --- | --- |
| `--cloud-storage-bucket` | `HECATONCHEIRES_CLOUD_STORAGE_BUCKET` | **yes** | Bucket holding History/Trace blobs |
| `--cloud-storage-prefix` | `HECATONCHEIRES_CLOUD_STORAGE_PREFIX` | no | Optional path prefix within the bucket |

Object layout under the bucket:

```
{prefix}/v1/sessions/{sessionID}/history.json
{prefix}/v1/traces/{sessionID}/{traceID}.json
```

- `sessionID` = `Session.ID` (UUIDv7).
- `traceID` = the `ts` of the mention message that triggered the turn —
  one trace per mention.

The `serve` command refuses to start when the bucket flag is unset.

Session metadata (workspace, case, thread TS, action linkage, last mention
TS, pending question, optional draft binding) is stored in Firestore keyed
by Slack channel + thread TS:

```
slack_channels/{channelID}/sessions/{threadTS}
```

The same Session row is used by both modes — case-bound mention agent
(`pkg/usecase/agent/casebound`) and open-mode draft agent
(`pkg/usecase/agent/draft`). Mode is discriminated at lookup time:
`Session.IsCaseBound()` returns true when `CaseID != 0`.

No new Firestore composite indexes are required; lookups are direct
document fetches.

### Required IAM

The service account that runs the application needs read/write access to
the configured Cloud Storage bucket. The least-privilege role is
**Storage Object Admin** scoped to the bucket (or the prefix if you split
buckets across environments). `Storage Object Viewer` alone is
insufficient — Save mutates objects on every LLM turn.

### Reading the artifacts

History blobs are gollem `History` JSON (`github.com/gollem-dev/gollem` v0.26+
format, version 3). They can be loaded back into a Go process via
`gollem.HistoryRepository.Load(ctx, sessionID)`.

Trace blobs are gollem `trace.Trace` JSON. The `metadata.labels` map
includes:

- `session_id` — `AgentSession.ID`
- `workspace_id`, `case_id`, `thread_ts`, `action_id` — domain identifiers
- `trigger_mention_ts` — the Slack TS that triggered this turn

Use these labels to slice traces in any downstream observability tool.

Agents that run on the agentkit runtime write **one trace object per claim**,
not per turn, because a durable run is picked up and put down many times and
`trace.Repository.Save` overwrites by id — a per-run id would let a later,
partial claim replace a complete earlier archive. Their labels differ
accordingly: `session_id` carries the **root Process id** (the identifier that
spans a whole run), the Slack session id moves to `slack_session_id`, and
`process_id`, `agent`, `job_id` and `job_run_id` are added. See
`claimTraceMetadata` in `pkg/agent/kernel/middleware.go`.

The agent tools available within these sessions are described in
[Configuration](../configuration.md#agent-tool-registry-slack-mention--assist).
They share the same GitHub App installation as the Source pipeline.

### Mention runs on the case agent page

The Cloud Storage trace above is the durable, full-fidelity artifact. In
addition, every post-creation mention turn is recorded as a queryable
`JobRunLog` + `JobRunEvent` trail in Firestore so it appears on the case
agent page (`/ws/{workspace}/cases/{id}/agent`) alongside scheduled and
lifecycle Job runs, through the same `caseJobRunLogs` read path.

- Both mention hosts — `casebound` (channel-mode) and `threadcase`
  `ModeMention` (thread-mode) — record via `pkg/agent/runtrace`, the same
  machinery the Job runner uses. On agentkit a run outlives the request that
  started it, so both open the log with `runtrace.Open` after Spawn and close it
  with `runtrace.FinishRun` from the completion handler; the pre-agentkit path
  used `runtrace.Recorder` + `runtrace.Handler` inside the turn instead.
- Mention runs are not configured Jobs, so each mention turn gets its own
  fresh per-turn JobID and is tagged `EventType = model.EventTypeMention`;
  the page shows a localized "Mention" label (resolved from the eventType, not
  the opaque JobID).
- **Token and call totals for an agentkit-hosted run come from
  `Process.Metrics`**, not from a trace handler: the run's transitions are
  spread across claims and possibly instances, so only the Process row
  accumulates the whole total.
- **The per-call event timeline works for agentkit-hosted runs too.** Each claim
  opens a `runtrace.Handler` alongside the archive recorder, and `Sequence` is
  allocated by the repository inside each write — so a run that moves between
  instances, or a resumed turn, keeps appending to one ordered timeline. This is
  why there is no in-process sequence counter any more: two claims would both
  start at 1.
- Creation-time turns (`threadcase` `ModeCreate`) are excluded — only
  mentions in an already-created case are listed.

## LLM prompt caching

Every LLM call this codebase makes opts into gollem's prompt cache. There is no
CLI flag or environment variable — it is on unconditionally, because it is a pure
cost optimization with no behavioural difference in the model's output.

It is applied in two places, because there are two ways a call is made:

- **Agent runs** go through agentkit, which builds the gollem session itself, so
  no host can hand `gollem.WithPromptCache` to a client. `promptCacheMiddleware`
  (`pkg/agent/kernel/middleware.go`) appends `gollem.WithSessionPromptCache(true)`
  to every `GenerateRequest` instead — one registration on the Kernel covers every
  agent, and there is exactly one place a Kernel is built.
- **Standalone sessions** outside the agent runtime (the webfetch analyze call,
  the assist log summary, the eval harness) pass the gollem option directly where
  they build the session.

Losing the first of those is what issue #266 reported: the agentkit migration
dropped the per-host `gollem.WithPromptCache(true)` calls, and the cache hit rate
on scheduled Job runs went from ~80% of input tokens to zero.

What it changes, per provider:

- **Claude** — gollem marks the stable prefix (system prompt, tool
  definitions) and the growing conversation tail with ephemeral
  `cache_control` breakpoints. The system prompt and tools become cache hits
  from the second call onward, and the moving tail breakpoint means each new
  turn pays full price only for the newly appended tokens. This is the big
  win for the plan-and-execute loop, where the planner is re-invoked once per
  round against a system prompt and tool set that never change.
- **OpenAI / Gemini** — the flag does not alter the request at all; both
  providers cache automatically on their side. Cache usage is still reported.

Two caveats worth knowing when reading token numbers:

- Claude only caches prefixes above a model-specific minimum (1024 or 4096
  tokens depending on the model). Marking anything shorter is a silent no-op
  on the API side — no error, the cache counters just stay `0`. Short-lived
  agents (the webfetch analyze session, the assist log summary) will
  therefore usually show no cache activity, which is expected rather than a
  misconfiguration.
- Claude cache entries use the default 5-minute TTL and gollem does not
  expose a knob for it, so a thread that goes quiet for longer re-pays for
  the prefix on its next turn.

`gollem.Response` reports `CacheCreationInputToken` (tokens written to the
cache; Claude only) and `CacheReadInputToken` (tokens served from it).
`InputToken` remains the *total* input including the cached prefix, so the
existing token-budget accounting stays correct whether or not a cache hit
occurred.

## Current time in agent prompts

An agent has no clock. Unless a run is told the date, it resolves "today", "by
tomorrow" and "last week" against whatever its training suggests — a case created
from a Slack thread was told the due date was "today" and recorded one a month in
the past. Every agent host therefore states the turn's start time, as an RFC3339
UTC string, and instructs the model to resolve relative dates against it.

**The first user message is where it belongs**, and two things push it there.

| host | where the time is stated |
| --- | --- |
| `threadcase` (thread-mode create / materialize / mention) | first user message (`agent.PlannerMessage`) |
| `proposal` (case draft) | first user message (`agent.PlannerMessage`) |
| `wsagent` (workspace-channel mention) | first user message (`agent.PlannerMessage`) |
| `casebound` (channel-mode case mention) | system prompt |
| `job` (Agent Job) | system prompt (`PromptInputs.Now`) |
| `assist` | system prompt (`prompts/assist_system.md`) |
| planexec sub-agents (every planexec host) | task context (`agent.TaskContext.Now`) |

The first reason is a requirement, and it applies to `threadcase` and
`proposal`. A host that passes `agentkit.WithInheritedHistory` starts its next
turn from the previous turn's messages, and the system prompt is **not** part of
that history — it is handed to each Generate call as a session option and rebuilt
for every turn. A system-prompt-only time would leave the inherited messages with
nothing saying when they were written. The section is therefore put at the top of
each turn's first user message, and it states that the latest such block is the
current one.

The second is a preference, and it is why `wsagent` does the same although it
inherits nothing. The system prompt and the tool definitions are the prefix
Claude's prompt cache matches on (see § LLM prompt caching), so a value that
changes every turn puts the whole system block back on the bill each time.
Keeping the time out of it leaves that prefix byte-identical from one turn to the
next.

`casebound`, `job` and `assist` predate this and still state it in their system
prompt. They inherit no history, so this is a cache cost rather than a
correctness problem; a new host should follow the three above.

Sub-agents never receive the host's system prompt or user input
(`buildSubAgentSystemPrompt` builds them from the task text plus the task
context), so they are told through `agent.TaskContext`. planexec's direct child
gets both (`Input.UserInput` plus the task context), and the terminal call reads
the planner's own conversation. Each host passes the same instant everywhere, so
a planner and its sub-agents cannot disagree about what "today" means inside one
turn — pinned per host by the `...TellsThePlannerAndItsSubAgentsOneTime` tests.

**Everything is UTC.** The codebase has no timezone configuration (cron schedules
are UTC too), so a conversation in a zone ahead of UTC can have the agent resolve
"today" to the previous date during that zone's early hours.

## Assignee ranking cache

The WebUI assignee pickers order their candidates by how often each user is
assigned in the current workspace (`frequentAssigneeIDs`). The ordering data has
to come from somewhere, and the constraint that shaped the design is that a
picker opens on **every** case-detail view: the query must not scan the Case
collection.

### Storage

One document per workspace at `workspaces/{workspaceID}/rankings/assignee`,
holding `model.AssigneeRanking` directly (no struct tags, so the Firestore field
names are the Go ones):

| Field        | Meaning                                                      |
|--------------|--------------------------------------------------------------|
| `WorkspaceID` | The workspace. Also the document's parent path segment.     |
| `UserIDs`     | Ranked Slack user IDs, most assigned first, at most 12.     |
| `ComputedAt`  | When `UserIDs` was produced. Zero value = never computed.   |

It is derived data. Deleting it costs one recompute and nothing else.

### Read path and refresh

`CaseUseCase.ListFrequentAssignees` (`pkg/usecase/assignee_ranking.go`) first
rejects a `workspaceID` the `WorkspaceRegistry` does not know
(`model.ErrWorkspaceNotFound`, which the GraphQL error mapper already reports as
`NOT_FOUND`). The other workspace-scoped reads can skip that check because a
bogus id simply returns nothing — but this one *writes* on a cold cache, and
Firestore creates `workspaces/{anything}/rankings/assignee` even when no such
workspace exists, so an unchecked id would let a caller leave a junk document
behind per id it invents. The registry is in-process configuration, so the check
costs no read.

It then reads the document and returns `UserIDs` as-is. If `ComputedAt` is older than one hour — or
the document does not exist — it dispatches `refreshAssigneeRanking` through
`async.Dispatch` and **still returns what it had**, which on a cold cache is an
empty slice. So the request path is one document read, always; the caller never
waits for a scan.

The refresh calls the existing `Case().List(ctx, workspaceID)` and counts
assignments in Go. It deliberately does **not** add a bounded projection query
(`OrderBy("UpdatedAt").Select(...).Limit(N)`): that would need a new repository
method, two backend implementations, and a projection model, to bound work that
already runs at most once per hour per workspace and outside the request. The
whole (non-`DRAFT`) case set is read instead.

Two consequences of keeping it this simple:

- **No refresh exclusion.** Several instances crossing the freshness boundary at
  once each recompute; `Set` is last-write-wins and the results are
  near-identical, so a claim document and its expiry handling would buy nothing
  but a handful of saved reads per hour.
- **An empty result still stores `ComputedAt`.** A workspace where nothing
  qualifies must not be re-scanned on every request.

### Private cases are filtered in Go, not in the query

`rankAssignees` skips `IsPrivate` cases so the shared ranking cannot leak who
works inside them. The filter lives in Go rather than the query because
`Where("IsPrivate", "==", false)` combined with any `OrderBy` requires a
composite index, which `.claude/rules/firestore.md` forbids.

### Client side

`useAssigneeCandidates` (`frontend/src/hooks/useAssigneeCandidates.ts`) is the
single supply point for every picker's user list: it pairs `slackUsers` with
`frequentAssigneeIDs` and runs `orderAssigneeCandidates`
(`frontend/src/utils/assignees.ts`), which puts ranked users first and sorts the
rest by display name. The ranking is treated as optional throughout — while it
loads, when it errors, and before a workspace is resolved, the list degrades to
display-name order rather than blocking the picker.

## See Also

- [develop/README.md](./README.md) — developer documentation index
- [User Guide](../user_guide.md) — the user-facing agent thread lifecycle and available agent tools
- [Configuration](../configuration.md) — TOML field definitions and the agent tool registry
- [CLI](../cli.md) — CLI flags and environment variables
- [Integrations](../integrations/README.md) — Notion, GitHub, and Jira integration setup
- [Operations](../operations.md) — Sentry and observability
- [`CLAUDE.md`](../../CLAUDE.md) and [`.claude/rules/`](../../.claude/rules/) — enforced project rules
