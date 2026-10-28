# Configuration Guide

Hecatoncheires is configured through a combination of a TOML configuration file and CLI flags (or environment variables). This guide is the complete reference for the `config.toml` file.

## Configuration File (config.toml)

The application requires a TOML configuration file at startup. This file defines custom fields for cases and display labels for entities.

- Default path: `./config.toml`
- Override with `--config` flag or `HECATONCHEIRES_CONFIG` environment variable
- The file **must exist** at startup; a missing file causes an error

### Basic Structure

```toml
# Workspace configuration (required)
[workspace]
id = "risk"
name = "Risk Management"

# Entity display labels (optional)
[labels]
case = "Risk"

# Custom field definitions
[[fields]]
id = "severity"
name = "Severity"
type = "select"
required = true
options = [
  { id = "high", name = "High" },
  { id = "low", name = "Low" },
]

# Slack integration (optional)
[slack]
channel_prefix = "risk"

# AI assist configuration (optional)
[assist]
prompt = "Check action deadlines and follow up on pending items."
language = "Japanese"
```

---

## Workspace Section

The `[workspace]` section defines the workspace's identity and is **required** in each configuration file.

```toml
[workspace]
id = "risk"
name = "Risk Management"
emoji = "🛡️"   # optional; mutually exclusive with color
language = "ja"  # optional; "en" or "ja"
```

### Properties

| Property | Type | Required | Description |
|----------|------|----------|-------------|
| `id` | string | **Yes** | Unique workspace identifier. Must match `^[a-z0-9]+(-[a-z0-9]+)*$` and be at most 63 characters |
| `name` | string | No | Display name for the workspace. Defaults to `id` if omitted |
| `emoji` | string | No | Badge glyph shown in the workspace selector and switcher. Mutually exclusive with `color`. Up to 16 runes |
| `color` | string | No | Badge background color as a `#RRGGBB` hex code (6-digit only). Mutually exclusive with `emoji` |
| `language` | string | No | Output language of this workspace's Slack messages and agent replies: `en` or `ja`. Takes priority over each Slack user's locale. Unset keeps the per-user locale. See [Workspace Language](#workspace-language) |

### Workspace Language

`language` fixes the language of everything the workspace writes to Slack:
messages, modals and ephemeral notices, the language the agents are told to
answer in, and the output of Jobs and `assist`. The language is chosen in this
order:

| Order | Source | Used when |
|-------|--------|-----------|
| 1 | `[workspace] language` | The workspace sets it. The Slack user's locale is not consulted |
| 2 | The Slack user's locale (`users.info`) | The workspace sets no `language` |
| 3 | `--default-lang` / `HECATONCHEIRES_DEFAULT_LANG` | Neither of the above is available, e.g. a scheduled Job with no workspace `language` |

For example, with `language = "ja"` a user whose Slack locale is `en-US` gets
Japanese replies; with `language` unset the same user gets English.

- **Accepted values** are the language codes `en` and `ja`, case-sensitive.
  `""` is the same as leaving the key out. Any other value (`"JA"`, `"ja-JP"`,
  `"Japanese"`, `"fr"`) fails configuration loading with
  `workspace language must be one of: en, ja`; `serve` does not start, and
  `validate` and `POST /api/validate/db` report the same error.
- **Not the same format as `[assist] language`.** That key takes a free-form
  language name (`"Japanese"`) and is used only in the assist prompt. When both
  are set, `[assist] language` decides the assist prompt's language; when only
  `[workspace] language` is set, the assist prompt follows it.
- **Before a workspace is known** the user's locale still applies: the
  workspace picker of the slash command, and a mention-created case draft until
  its agent has chosen a workspace.
- **The web UI is not affected.** It follows the browser's language, and pages
  such as the dashboard span several workspaces.
- A change takes effect on restart, like every other workspace setting. An
  agent run keeps the language it started with.

### Workspace Badge (emoji / color)

Each workspace is shown as a small badge in the workspace selector and the
breadcrumb switcher. You can control its appearance with **either** `emoji`
**or** `color` — they are mutually exclusive, and setting both fails validation
at startup.

- **`emoji`** — renders the emoji on a neutral background (e.g. `emoji = "🛡️"`).
- **`color`** — renders the workspace's initials on a gradient derived from the
  given `#RRGGBB` color (e.g. `color = "#2cb38d"`). Only the 6-digit hex form is
  accepted; `#fff` and named colors are rejected.
- **Neither** — the initials are shown on an automatically assigned color that
  is derived deterministically from the workspace `id`. The same workspace
  always gets the same color regardless of how many workspaces exist or their
  order, so the selector and switcher stay consistent.

### Workspace ID Format

Workspace IDs must follow the same format as field IDs:

- Pattern: `^[a-z0-9]+(-[a-z0-9]+)*$`
- Maximum length: 63 characters
- Must be unique across all workspaces

| Example | Valid | Reason |
|---------|-------|--------|
| `risk` | Yes | Simple lowercase |
| `security-team` | Yes | Hyphen-separated |
| `workspace-123` | Yes | With numbers |
| `MyWorkspace` | **No** | Uppercase not allowed |
| `workspace_1` | **No** | Underscores not allowed |
| `workspace.name` | **No** | Dots not allowed |

---

## Labels

The `[labels]` section customizes entity display names in the UI:

```toml
[labels]
case = "Risk"        # Default: "Case"
```

| Key | Default | Description |
|-----|---------|-------------|
| `case` | `Case` | Display name for the primary entity |

If omitted or empty, the default values are used.

---

## Field Definitions

Each field is defined as an element of the `[[fields]]` array:

```toml
[[fields]]
id = "severity"
name = "Severity"
type = "select"
required = true
description = "Overall severity assessment"
```

### Properties

| Property | Type | Required | Description |
|----------|------|----------|-------------|
| `id` | string | **Yes** | Unique identifier. Must match `^[a-z][a-z0-9_]*$` |
| `name` | string | **Yes** | Display name shown in the UI |
| `type` | string | **Yes** | Field type (see [Field Types](#field-types)) |
| `required` | boolean | No | Whether the field is required (default: `false`) |
| `description` | string | No | Help text shown in the UI |
| `reference_workspace` | string | Case-reference only | Target workspace ID whose Cases this field references. **Required** for `case_ref` / `multi_case_ref`, and rejected for every other type. Must name a configured workspace (self-reference allowed) |
| `semantic` | string | No | How a `text` value is interpreted (see [Text field semantics](#text-field-semantics)). Rejected for every other type and for an undefined semantic |
| `validation` | table | No | Constraints on the shape of the value. Its only key is `pattern`, a regular expression the whole `text` value must match (see [Text field pattern](#text-field-pattern)). `pattern` is rejected for every other type and when it is not a valid Go RE2 expression. Other keys inside `validation` are ignored |

### Field ID Format

Field IDs must start with a lowercase letter and consist of lowercase letters,
digits, and underscores:

- Pattern: `^[a-z][a-z0-9_]*$`
- Must be unique across all fields in the configuration

> **Breaking change (Apr 2026)**: Field IDs and Option IDs no longer accept
> hyphens (`-`). The new pattern aligns with Go identifier rules so that
> Slack welcome message templates can reference custom field values via dot
> notation (e.g., `{{.Fields.risk_level}}`). Existing deployments that store
> hyphenated FieldIDs in Firestore must migrate the data themselves; the
> application provides no automatic migration. Workspace IDs are unaffected
> and continue to follow the legacy hyphen-separated format.

| Example | Valid | Reason |
|---------|-------|--------|
| `category` | Yes | Simple lowercase |
| `risk_level` | Yes | Underscore-separated |
| `my_field_123` | Yes | With numbers |
| `risk-level` | **No** | Hyphens are no longer allowed |
| `MyField` | **No** | Uppercase not allowed |
| `1category` | **No** | Cannot start with a digit |
| `field.name` | **No** | Dots not allowed |
| `_leading` | **No** | Cannot start with underscore |

---

## Field Types

### `text`

Single-line text input.

```toml
[[fields]]
id = "description"
name = "Description"
type = "text"
```

A `text` field may set `semantic` to say what its value refers to; see
[Text field semantics](#text-field-semantics). It may also set
`validation.pattern` to restrict the value to a regular expression; see
[Text field pattern](#text-field-pattern).

### `markdown`

Multi-line Markdown text. Stored as a plain string like `text`, but the Web UI
renders it as Markdown when displaying it, and the editor offers a Write /
Preview toggle. On the Case detail page a Markdown field is not listed in the
sidebar: it gets its own section in the main column, below the description,
headed by the field name, where the value is rendered as Markdown and edited in
place (click to edit) — long text stays readable at full width. In Slack
modals a Markdown field is edited as a multi-line plain-text input (raw
Markdown source — Slack has no preview element). Available for both Case fields (`[[fields]]`) and memo fields
(`[[memo.fields]]`).

```toml
[[fields]]
id = "runbook"
name = "Runbook"
type = "markdown"
```

### `number`

Numeric input.

```toml
[[fields]]
id = "score"
name = "Risk Score"
type = "number"
```

### `select`

Single selection from a predefined list of options. Requires at least one option.

```toml
[[fields]]
id = "priority"
name = "Priority"
type = "select"
required = true
options = [
  { id = "high", name = "High" },
  { id = "medium", name = "Medium" },
  { id = "low", name = "Low" },
]
```

### `multi-select`

Multiple selections from a predefined list of options. Requires at least one option.

```toml
[[fields]]
id = "tags"
name = "Tags"
type = "multi-select"
options = [
  { id = "urgent", name = "Urgent" },
  { id = "review_needed", name = "Review Needed" },
]
```

### `user`

Single Slack user reference. Rendered with user avatar in the UI. Requires Slack integration to be configured.

```toml
[[fields]]
id = "assignee"
name = "Assignee"
type = "user"
```

### `multi-user`

Multiple Slack user references. Requires Slack integration.

```toml
[[fields]]
id = "stakeholders"
name = "Stakeholders"
type = "multi-user"
```

### `date`

Date picker input.

```toml
[[fields]]
id = "deadline"
name = "Deadline"
type = "date"
```

### `url`

URL input with validation.

```toml
[[fields]]
id = "reference"
name = "Reference URL"
type = "url"
```

### `case_ref`

Reference to a single Case in another (or the same) workspace, selected through
a searchable dropdown. The target workspace is fixed by the `reference_workspace`
property (required); only Cases in that workspace can be referenced.

```toml
[[fields]]
id = "related_incident"
name = "Related Incident"
type = "case_ref"
reference_workspace = "incident-response"
```

### `multi_case_ref`

Reference to multiple Cases in the workspace named by `reference_workspace`
(required). Same rules as `case_ref`, but stores a list.

```toml
[[fields]]
id = "related_cases"
name = "Related Cases"
type = "multi_case_ref"
reference_workspace = "incident-response"
```

> **Private Cases are never referenceable.** A private Case in the target
> workspace is excluded from the picker, hidden from the agent search/detail
> tools, and rejected when set as a value — regardless of who is editing. Draft
> Cases are likewise not referenceable. `reference_workspace` may name the
> field's own workspace (self-reference is allowed), and is validated at startup
> against the set of configured workspaces.
>
> **Case-reference fields cannot be `required`.** The Slack case-creation modal
> has no element for a searchable cross-workspace case picker, so a required
> case reference would be un-fillable from Slack. Set them via the Web UI or
> agent tools after the Case exists; config load rejects `required = true` on
> these types.

### Summary

| Type | Description | Requires Options | Requires `reference_workspace` |
|------|-------------|-----------------|-----------------|
| `text` | Single-line text input | No | No |
| `markdown` | Multi-line Markdown text (rendered in the Web UI) | No | No |
| `number` | Numeric input | No | No |
| `select` | Single selection from options | **Yes** | No |
| `multi-select` | Multiple selections from options | **Yes** | No |
| `user` | Single Slack user reference | No | No |
| `multi-user` | Multiple Slack user references | No | No |
| `date` | Date picker | No | No |
| `url` | URL input | No | No |
| `case_ref` | Single Case reference in another workspace | No | **Yes** |
| `multi_case_ref` | Multiple Case references in another workspace | No | **Yes** |

### Text field semantics

`semantic` tells Hecatoncheires what a `text` value refers to. The value is
still stored and edited as plain text; the semantic adds validation on every
write path and extra information wherever the value is shown. It is available
for both Case fields (`[[fields]]`) and memo fields (`[[memo.fields]]`), and is
rejected at config load on any other type or when the name is not defined.

```toml
[[fields]]
id = "notify_channel"
name = "Notification channel"
type = "text"
semantic = "slack_channel_id"
```

| Semantic | Accepted value | Web UI | Slack messages | Agent prompts |
|----------|----------------|--------|----------------|---------------|
| `slack_channel_id` | `C` or `G` followed by uppercase letters and digits (e.g. `C0123456789`). `C` is a public channel or a private channel created since March 2021; `G` is an older private channel or a multi-person DM ([Slack docs](https://docs.slack.dev/apis/web-api/using-the-conversations-api/)). An empty value is accepted. A channel name (`#general`), a `<#C...>` mention, and a direct-message ID starting with `D` are rejected. Slack may change a `G` ID to `C` when the channel is shared with another organization; update the stored value if that happens | The ID stays as the field value; a line under it shows the channel name linked to `https://slack.com/archives/<ID>`. When the name cannot be resolved (Slack not configured, a private channel the bot is not in, a deleted channel, or a stored value that is not a channel ID) the line says so as an error ("Couldn't resolve the name") and carries no link | Rendered as `<#ID>`, so Slack shows the channel name | The field is listed with `semantic=slack_channel_id` and a description of the expected value |

Every agent that can write a field value is told the field's semantic and the
expected value shape: the thread-mode case agent, the case-channel agent (Case
and memo fields), the workspace-channel agent, the Job agent (Case and memo
fields) and the case-draft agent (through `get_workspace`). The read-only MCP
tool `hecaton_list_workspaces` also returns it in the field schema.

The channel name is looked up with `conversations.info` — the same call the
Case's own `slackChannelName` uses — so no additional Slack scope is needed.

**Adding a semantic to an existing field.** Values stored before the semantic
was set are left untouched and keep displaying as before. Writing a new value
to the field is validated; updating other fields of the same Case is not
blocked. Run `hecatoncheires validate --check-db` to list stored values that do
not fit: they are reported as `field_value` with `expected` set to
`text (slack_channel_id)`. Removing `semantic` restores the previous behaviour.

### Text field pattern

`validation.pattern` restricts a `text` value to a regular expression. It is
available for both Case fields (`[[fields]]`) and memo fields
(`[[memo.fields]]`). Every write path checks it: the Web UI, Slack, the agents'
tools and thread-mode case creation. A value that does not match is rejected
and nothing is saved.

```toml
[[fields]]
id = "ticket_id"
name = "Ticket ID"
type = "text"
description = "Ticket ID in the external tracker, e.g. SEC-1234"
validation = { pattern = '[A-Z]{2,5}-[0-9]+' }
```

| Value | Result | Why |
|-------|--------|-----|
| `SEC-1234` | Accepted | The whole value matches |
| `sec-1234` | Rejected | Lowercase letters do not match `[A-Z]` |
| `See SEC-1234` | Rejected | Only part of the value matches |
| (empty) | Accepted | An empty value is not checked. Set `required = true` to require a value |

How the pattern is applied:

- **The whole value must match.** The pattern is matched as `^(?:pattern)$`,
  as the HTML `pattern` attribute does. You do not need to write `^` and `$`;
  writing them changes nothing. To accept a value that only contains a match,
  say so explicitly, e.g. `.*SEC-[0-9]+.*`. Note that this differs from JSON
  Schema's `pattern`, which accepts a partial match.
- **Go RE2 syntax.** Matching time grows linearly with the length of the value.
  Backreferences (`\1`) and lookaround (`(?=…)`, `(?!…)`) are not available, and
  a pattern that uses them is rejected at config load. See the
  [RE2 syntax reference](https://github.com/google/re2/wiki/Syntax).
- **Multi-line values.** By default `.` does not match a newline and `$` matches
  only at the end of the value. Add flags such as `(?s)` yourself when you need
  otherwise.
- **An empty value is always accepted**, so setting a pattern does not make an
  optional field required.
- **With `semantic`.** Both can be set on the same field, and a value must pass
  both. The semantic is checked first, and only its error is reported when the
  value breaks both.
- **Only for `text`.** A `pattern` on any other type is rejected at config load.

When a value is rejected, the error names the field and the pattern. For the
example above, saving `sec-1234` returns `BAD_USER_INPUT` from GraphQL with
this message, and an agent's tool call receives the same text:

```
field "ticket_id": text value must match the pattern [A-Z]{2,5}-[0-9]+ (the whole value must match; Go RE2 syntax): text value does not match the field pattern
```

**Writing the pattern in TOML.** Use a literal string (single quotes), because
regular expressions often contain backslashes. In a basic string (double
quotes) each backslash must be doubled, and `"\d"` is an invalid TOML escape
that fails before the pattern is checked:

```toml
# These two lines are the same pattern, INC-\d+
validation = { pattern = 'INC-\d+' }
validation = { pattern = "INC-\\d+" }

# TOML decode error
validation = { pattern = "INC-\d+" }
```

`validation` can be written in any of these forms; they are equivalent:

```toml
# One-line inline table
[[fields]]
id = "ticket_id"
name = "Ticket ID"
type = "text"
validation = { pattern = '[A-Z]{2,5}-[0-9]+' }

# Multi-line inline table (TOML 1.1; a trailing comma is allowed)
[[fields]]
id = "ticket_id"
name = "Ticket ID"
type = "text"
validation = {
  pattern = '[A-Z]{2,5}-[0-9]+',
}

# Sub-table. It belongs to the [[fields]] entry just above it, so write it
# after that entry's other keys.
[[fields]]
id = "ticket_id"
name = "Ticket ID"
type = "text"

[fields.validation]
pattern = '[A-Z]{2,5}-[0-9]+'
```

Keys other than `pattern` inside `validation` are ignored, as unknown keys are
everywhere else in the workspace config. For example,
`validation = { max_length = 120 }` loads without error and checks nothing.

More examples:

```toml
# Memo field holding a CVE identifier.
# Accepts CVE-2024-3094; rejects CVE-24-3094 and cve-2024-3094.
[[memo.fields]]
id = "cve"
name = "CVE"
type = "text"
description = "CVE identifier this memo is about"
validation = { pattern = 'CVE-[0-9]{4}-[0-9]{4,}' }

# Combined with a semantic: slack_channel_id alone accepts IDs starting with
# C or G; the pattern rejects the G ones. "#general" fails the semantic and
# reports only the semantic error.
[[fields]]
id = "notify_channel"
name = "Notification channel"
type = "text"
semantic = "slack_channel_id"
validation = { pattern = 'C[0-9A-Z]+' }
```

These configurations fail to load. Startup, `hecatoncheires validate` and
`POST /api/validate/db` report the field ID and the pattern:

```toml
# pattern on a type other than text
[[fields]]
id = "score"
name = "Score"
type = "number"
validation = { pattern = '[0-9]+' }

# not valid RE2 (backreferences are not supported)
[[fields]]
id = "code"
name = "Code"
type = "text"
validation = { pattern = '(a)\1' }
```

Every agent that can write a field value is given the pattern and the rule
that the whole value must match: the thread-mode case agent, the case-channel
agent (Case and memo fields), the workspace-channel agent and the Job agent
(Case and memo fields) in their system prompts, and the case-draft agent
through the `pattern` and `pattern_hint` keys of `get_workspace`. The read-only
MCP tool `hecaton_list_workspaces` returns the same two keys in its field
schema. The Web UI does not check the pattern before saving; it shows the error
the server returns.

**Adding a pattern to an existing field.** Values stored before the pattern was
set are left untouched. Writing a new value to the field is validated; updating
other fields of the same Case is not blocked. Run
`hecatoncheires validate --check-db` to list stored values that do not match:
they are reported as `field_value` with `expected` set to
`text (pattern=[A-Z]{2,5}-[0-9]+)`, or `text (slack_channel_id, pattern=C[0-9A-Z]+)`
when the field also has a semantic. Removing `pattern` restores the previous
behaviour.

---

## Options (for select / multi-select)

Fields of type `select` or `multi-select` must define at least one option using the `options` property.

### Option Properties

| Property | Type | Required | Description |
|----------|------|----------|-------------|
| `id` | string | **Yes** | Unique identifier within the field. Same format as field ID |
| `name` | string | **Yes** | Display name |
| `description` | string | No | Description of this option (surfaced in the UI's `(?)` help popover and option-hover tooltip) |
| `metadata` | table | No | Arbitrary key-value pairs |

Option IDs must be unique within their parent field and follow the same format as field IDs (`^[a-z][a-z0-9_]*$`).

`description` lives at the top level — it is a first-class part of the option contract, surfaced directly in the UI. The previous shape that placed `description` (or `color`) inside `metadata` is no longer supported; migrate to top-level `description`.

### Metadata

The `metadata` property allows attaching arbitrary key-value data to an option. Values can be strings, numbers, or booleans.

```toml
[[fields]]
id = "severity"
name = "Severity"
type = "select"
description = "Incident severity. Score (1–5) lives in metadata."
options = [
  { id = "critical", name = "Critical", description = "Requires immediate executive attention", metadata = { score = 5, escalation_required = true } },
  { id = "low", name = "Low", description = "Minimal impact", metadata = { score = 1 } },
]
```

Use cases for metadata:
- **Scoring**: Attach numeric scores for risk calculation
- **Workflow**: Flag options that trigger specific behaviors

For human-readable text shown in the UI, use top-level `description` — never store it under `metadata`.

---

## Memo Section

The optional `[memo]` section enables **case-scoped memos**: a per-Case memory
where humans and agents record facts, observations, hypotheses, and decisions
while working a Case. A memo always has an `id` and a `title`; everything else
is a custom field defined here, using the exact same field schema as case
`[[fields]]` (same types, options, and validation rules).

When `[memo]` is omitted the memo feature is disabled for the workspace: the
WebUI hides the Memos tab and agents are given no memo tools.

```toml
[memo]
# The "strong definition" of what this memo records. Shown in the WebUI and
# embedded into the agent system prompt so the agent knows what to write down.
description = """
This memo is the investigation memory for an incident case. Record facts,
observations, hypotheses, and decisions so later agent runs build on them.
Mark unverified guesses with a confidence so they are not mistaken for facts.
"""

# Memo custom fields. Identical schema to [[fields]] (id / name / type /
# required / description / options).
[[memo.fields]]
id = "memo_type"
name = "Type"
type = "select"
required = true
description = "Whether this memory is a verified fact or an inference."
options = [
  { id = "fact", name = "Fact", description = "Verified, backed by evidence." },
  { id = "observation", name = "Observation", description = "Read from logs/data." },
  { id = "hypothesis", name = "Hypothesis", description = "Unverified inference." },
  { id = "decision", name = "Decision", description = "A recorded decision." },
]

[[memo.fields]]
id = "body"
name = "Body"
type = "text"
description = "Free-form details, evidence, and next steps."

[[memo.fields]]
id = "evidence"
name = "Evidence"
type = "url"
description = "Link to the primary source (log, dashboard, PR)."
```

### Properties

| Property            | Description                                                            |
|---------------------|------------------------------------------------------------------------|
| `description`       | The strong definition of the memo, embedded into the agent system prompt and shown in the WebUI. |
| `[[memo.fields]]`   | Memo custom field definitions. Same schema and validation as `[[fields]]`. |

### Behavior

- **WebUI**: a "Memos" tab on the Case detail page lets users list, view,
  create, edit, and archive memos. Deletion is a soft delete (archive) and is
  restorable.
- **Agents**: every agent running in a Case context is given memo tools
  (`memo__list_memos`, `memo__get_memo`, `memo__apply_memo_changes`), scoped to
  that Case. Create and update are validated against the memo field schema
  exactly like the WebUI path (required fields enforced, unknown field ids
  rejected).
- **`memo__apply_memo_changes`**: the one and only write tool. It takes
  `creates`, `updates` and `archives` together (up to 50 entries in total), so
  an agent commits its whole set of memo changes in a single call instead of one
  call per memo — every call is an LLM round trip that re-sends the whole
  message history, so per-memo writes cost (number of mutations × context size).
  - Entries are applied in order — every create, then every update, then every
    archive — and independently: the response carries each entry's outcome in
    `results[]` plus `applied` / `failed` counts, so one bad entry does not
    discard the rest.
  - The exception is a value whose **type** does not match the tool schema
    (`creates` not an array, a numeric `title`): that is rejected before any
    entry is applied and the whole call fails. See
    [agent_tools.md](agent_tools.md) for the full per-entry vs whole-call split.
  - An update entry must carry at least one of `title` / `fields`.
- **`memo__list_memos`**: returns one page of the Case's memos, newest first.
  - **Archived memos are never returned.** Archiving is how an agent drops a
    memory, so the list tool cannot read it back; `memo__get_memo` still
    resolves an archived memo for a caller that already holds its id, and the
    WebUI can restore it.
  - **Paging**: `limit` defaults to 10 and is capped at 50 (a larger value is
    capped, not rejected; a value below 1 falls back to the default). `offset`
    defaults to 0 and skips that many of the newest matching memos. The response
    carries `offset` (the value actually applied), `total_count` (matches before
    `limit`/`offset`), `returned_count`, and `has_more` — pass
    `offset + returned_count` back as the next `offset` to read the next page.
    An `offset` at or beyond `total_count` yields an empty page rather than an
    error. Because paging is offset-based, memos created between two calls shift
    the list and a memo can repeat across pages.
  - **Creation-time window**: `created_after` (RFC3339, inclusive) and
    `created_before` (RFC3339, exclusive). Omitting a bound — or passing an empty
    string, which agents do in place of omitting it — leaves that side unbounded;
    any other value that is not RFC3339 is rejected. The same empty-string
    tolerance applies to `limit` and `offset`. The agent combines the bounds with
    the current time its run was given to ask for e.g. the memos of the last 7
    days (see `docs/develop/architecture.md` § Current time in agent prompts for
    where each host states it).
  - The page size is deliberately small: a tool response stays in the agent's
    message history for the rest of the run and is re-sent on every later model
    call, so a full dump is paid for repeatedly.
- **System prompt**: the `description` and the memo field schema are injected
  into the agent's system prompt, along with the id + title of up to 20 of the
  Case's active memos (and the total count when there are more). Full content is
  fetched on demand via the memo tools.

---

## Slack Section

The `[slack]` section customizes Slack integration settings. This section is optional.

```toml
[slack]
channel_prefix = "risk"
```

### Properties

| Property | Type | Required | Default | Description |
|----------|------|----------|---------|-------------|
| `channel_prefix` | string | No | workspace ID | Prefix for auto-created Slack channel names (channel mode only) |
| `mode` | string | No | `channel` | Case binding mode: `channel` (one Case per dedicated channel) or `thread` (one Case per thread in a monitored channel) |
| `channel` | string | Conditional | — | The monitored Slack channel ID (e.g. `C0123456789`). **Required when `mode = "thread"`.** Use the channel **ID**, not the name |
| `trigger` | string | No | `instant` | Thread mode only. What starts a Case: `instant` (every channel-root post) or `mention` (only an @mention of the bot — at the channel root or inside a thread that has no Case yet). Ignored in channel mode (startup warning). See [Case trigger](#case-trigger-thread-mode). |
| `accept_bot` | bool | No | `false` | Thread mode only. When `true`, **bot-authored** events also start a Case (a channel-root post in `instant` mode, an @mention in `mention` mode) — e.g. an intake-form app's relayed request. When `false`, only human-authored events start a Case, so the channel is not flooded with a Case per bot notification. |
| `reaction` | string | No | — | Thread mode only. Emoji name (with or without surrounding colons, e.g. `incident` or `:incident:`) that starts a Case when added to a message. Must be **unique across workspaces**. Setting it in channel mode is a config error. Requires the `reactions:read` scope and the `reaction_added` event subscription; the bot must be a member of any channel where the emoji is used. See [Reaction-triggered case creation](slack.md#reaction-triggered-case-creation). |
| `workspace_channel` | string | No | — | **Channel mode only.** A workspace-level shared channel ID (e.g. `C0123456789`) where the cross-case **workspace agent** runs. Parallels `channel` (the thread-mode monitored channel) but is a distinct role. Must be **unique across workspaces** (and must not collide with any workspace's monitored `channel`). Setting it in thread mode is a config error — there the monitored channel already hosts the agent. The bot must be a member. See [Workspace agent](#workspace-agent). |
| `allow_existing_channel` | bool | No | `false` | **Channel mode only.** When `true`, the Web case-creation form lets the user link the new case to an existing Slack channel instead of creating one. Ignored in thread mode (startup warning). Requires the `channels:join` bot scope. See [Linking an existing channel](#linking-an-existing-channel). |

When a case is created (channel mode), Hecatoncheires can automatically create a Slack channel with the naming pattern: `{channel_prefix}-{case_number}`.

If `channel_prefix` is not specified, the workspace ID is used as the default prefix.

**Thread mode:** When `mode = "thread"`, `channel_prefix`, `[slack.invite]`, and
`welcome_messages` are ignored (no dedicated channel is created). The monitored
`channel` must exist, the bot must be a member of it, and the app must subscribe
to the `message.channels` (and `message.groups` for private channels) events. A
`[case.status]` section is required in thread mode — see
[Case Section](#case-section-thread-mode). See also
[Slack Integration → Thread mode](slack.md#thread-mode-monitored-channel).

### Linking an existing channel

With `allow_existing_channel = true`, the **New case** form of the Web UI shows a
**Slack channel** choice: *Create a new channel* (the default, the behaviour
without this setting) or *Use an existing channel*. The choice appears only when
the workspace is in channel mode and the server has Slack configured.

- **One channel per case, one case per channel.** A channel already linked to a
  case in *any* workspace is rejected when the case is created. Two requests for
  the same channel at almost the same moment can both succeed; this is not
  detected or repaired.
- **Which channels are offered.** Channels the user has joined and the bot can
  see, minus every workspace's monitored `channel` / `workspace_channel` and
  channels already linked to a case. A private channel is listed only when the
  bot is a member — the bot cannot join a private channel itself, so invite it
  first.
- **Bot membership.** If the bot is not in the chosen public channel, it joins
  it (`conversations.join`) when the case is created. If the join fails, the
  case is not created. The join happens before the user's membership is
  checked, so a request that fails that last check leaves the bot in the
  channel.
- **Privacy follows the channel.** A private channel makes the case private.
  The private toggle is hidden; the GraphQL `createCase` input must omit
  `isPrivate` when `existingSlackChannelID` is set.
- **Not available for drafts.** *Save as draft* is disabled while an existing
  channel is chosen; submitting a draft always creates a new channel.
- **Renaming.** Changing the case title still renames the channel. Slack only
  lets the channel creator, a Workspace Admin or a Channel Manager rename a
  channel, so for a channel the bot did not create the rename usually fails and
  the title change is rejected. Keep the title of such a case unchanged.
- Messages posted to the channel before it was linked are not imported.

Turning the setting off later does not affect cases already linked.

### Workspace agent

The **workspace agent** is *not* bound to any single Case: when the bot is
@mentioned it can read across, and act on, **every Case the mentioning user can
access** — answer questions, create a Case, update or move an existing one, all
from one place. Each mention thread is one continuing conversation.

Where it runs depends on the case mode.

**Channel mode** — in a separate, optional workspace-level channel:

```toml
[slack]
mode = "channel"
workspace_channel = "C0123456789"   # the bot must be a member; channel ID, not name

# Optional custom instruction for the workspace agent.
[slack.workspace_agent]
prompt = "This workspace tracks security risks. Prefer concise Japanese replies."
# prompt_file = "prompts/ws_agent.md"   # or load from a file (relative to this config)
```

**Thread mode** — in the monitored channel itself, on a channel-root @mention. No
`workspace_channel` is involved (setting one in thread mode is a config error):

```toml
[slack]
mode = "thread"
channel = "C_CASES"     # the monitored channel; a root @mention here runs the agent
trigger = "mention"

[slack.workspace_agent]
prompt = "Prefer concise Japanese replies."
```

- `workspace_channel` is **channel mode only** and must be **unique** across all
  workspaces (it must not equal another workspace's `workspace_channel` or
  monitored `channel`). Omit it to leave the channel-mode agent dormant.
- `[slack.workspace_agent]` configures the agent and is valid in **both** modes.
  `prompt` / `prompt_file` are optional and **mutually exclusive** (both set is a
  config error); `prompt_file` is resolved relative to the config file's
  directory and behaves like a Job `prompt_file`. In channel mode the section
  requires `workspace_channel`; in thread mode it stands alone.
- **Thread mode differences:** a thread-mode workspace has no Actions, so the
  agent gets no Action tools, and it finishes a Case by moving it to a closed
  board status (`case__update_case_status`) rather than with a separate close
  tool. In thread mode the agent runs on a channel-root mention **instead of**
  starting a Case there; Cases come from an @mention inside a thread. See
  [Slack Integration → Thread mode](slack.md#thread-mode-monitored-channel).
- **Write safety:** the workspace agent has broad, cross-case write access, so it
  is held to a strict guardrail — it **never** creates/updates/closes/reassigns
  anything unless your message explicitly asks for that specific change; by
  default it investigates and reports. Your `prompt` adds domain context but
  **cannot relax** this guardrail.
- **Access control:** the agent acts with the **mentioning user's** permissions —
  it can only read or change Cases that user is allowed to access, so private
  Cases stay invisible to non-members. For the same reason it runs only for a
  mention authored by a Slack user; an app-authored mention (`accept_bot`) has no
  identity to act on behalf of and never reaches the agent.

### Auto-Invite (`[slack.invite]`)

The `[slack.invite]` subsection configures automatic invitation of users and user group members to Slack channels when a case is created.

```toml
[slack.invite]
users = ["U12345678", "U87654321"]
groups = ["S0614TZR7", "@security-response"]
```

| Property | Type | Required | Default | Description |
|----------|------|----------|---------|-------------|
| `users` | string[] | No | `[]` | Slack user IDs to automatically invite to case channels |
| `groups` | string[] | No | `[]` | Slack user group IDs or `@`-prefixed handle names to resolve and invite |

**Group resolution:**
- If a group identifier starts with `@` (e.g., `@security-response`), it is treated as a handle name and resolved to a group ID via the `usergroups.list` API
- Otherwise (e.g., `S0614TZR7`), it is treated as a Slack user group ID and used directly
- Members of each group are fetched via `usergroups.users.list` and added to the invite list
- Group resolution failures are logged but do not prevent case creation
- Duplicate users (across direct users, group members, creator, and assignees) are automatically deduplicated

**Required bot scope:** `usergroups:read` (in addition to existing scopes)

### Welcome Messages (`welcome_messages`)

After the Slack channel is created and initial members are invited, Hecatoncheires can post a sequence of welcome messages to the channel. Each entry is a Go [`text/template`](https://pkg.go.dev/text/template) string that is rendered against the newly created Case.

```toml
[slack]
welcome_messages = [
  "<@{{.Case.ReporterID}}> Created Case `{{.Case.Title}}`.",
  """\
:rotating_light: Highlights
- Status: {{.Case.Status}}
- Severity: {{.Fields.severity.name}} ({{.Fields.severity.id}})
- Tags: {{range $i, $t := .Fields.tags.items}}{{if $i}}, {{end}}{{$t.name}}{{end}}
- Reporter: <@{{.Case.ReporterID}}>
- Assignees: {{range $i, $a := .Case.AssigneeIDs}}{{if $i}}, {{end}}<@{{$a}}>{{end}}
- Detail: {{.URL}}""",
]
```

**Behavior**

- Messages are sent in the order they appear in the array, after channel creation, member invitation, and bookmark addition.
- Templates are parsed at startup; an invalid template aborts the application start with a configuration error.
- A template that produces an empty string at runtime is skipped (useful for conditional messages with `{{if ...}}...{{end}}`).
- A failure to render or post a single message is logged via `errutil.Handle` and does **not** roll back case creation; subsequent messages still attempt to post.

**Available template variables**

| Variable | Type | Notes |
|----------|------|-------|
| `.Case.ID` | int64 | Case sequence number |
| `.Case.Title` | string | Case title |
| `.Case.Description` | string | Case description |
| `.Case.Status` | CaseStatus | Normalized status string |
| `.Case.ReporterID` | string | Slack user ID of the reporter |
| `.Case.AssigneeIDs` | []string | Slack user IDs of assignees |
| `.Case.SlackChannelID` | string | The freshly-created channel ID |
| `.Case.IsPrivate` | bool | Whether the case is private |
| `.Case.CreatedAt` | time.Time | Creation timestamp (UTC) |
| `.Workspace.ID` | string | Workspace identifier |
| `.Workspace.Name` | string | Workspace display name |
| `.Fields` | map[string]map[string]any | Custom field values keyed by Field ID — each entry exposes `id` and `name` (and `items` for multi-select) |
| `.URL` | string | Web UI Case URL when `--base-url` is configured (otherwise empty) |

**Field value structure**

Each Field is exposed as a sub-map with the following sub-keys:

| Sub-key | Description |
|---------|-------------|
| `id` | Raw stored value. For `select`, the option ID. For `multi-select`, an array of option IDs. For `text`/`number`/`date`/`url`/`user`/`multi-user`, the raw stored value. |
| `name` | Display name. For `select`/`multi-select`, the Option `name` from the schema (falls back to the ID when not found). For other field types, mirrors `id`. |
| `items` | `multi-select` only. Slice of `{id, name}` maps for iteration in templates. |

Examples:

```text
Severity: {{.Fields.severity.name}}                        ← "High"
Severity ID: {{.Fields.severity.id}}                        ← "high"
Tags: {{range $i, $t := .Fields.tags.items}}{{if $i}}, {{end}}{{$t.name}}{{end}}
Note: {{.Fields.note.id}}                                   ← bare text value
```

Slack mrkdwn syntax such as `<@USER_ID>` and `<#CHANNEL_ID>` is rendered as-is and expanded by Slack at delivery time.

### Private Case Channels

When a case is created with the **Private** flag enabled, the associated Slack channel is created as a **private channel** instead of a public one. This ensures that only invited members can view the channel content.

Private cases also track channel membership:
- Channel member IDs are stored on the case and used for access control — only channel members can view the case details, actions, and assist logs associated with a private case.
- Members can be synced from the Slack channel via the **Sync** button on the case detail page or through the `syncCaseChannelUsers` GraphQL mutation.
- Bot users are automatically filtered out from the member list.

---

## Assist Section

The `[assist]` section configures the AI-powered case assistance feature. This section is optional.

```toml
[assist]
prompt = "Check action deadlines and follow up on pending items."
language = "Japanese"
```

### Properties

| Property | Type | Required | Description |
|----------|------|----------|-------------|
| `prompt` | string | No | Custom prompt for the AI assistant when analyzing cases |
| `language` | string | No | Preferred language for AI responses as a free-form name (e.g., "Japanese", "English"). Overrides `[workspace] language` for the assist prompt only; when unset, the assist prompt follows `[workspace] language` |

The `assist` command runs an AI agent that periodically reviews open cases and provides insights, reminders, and suggestions via Slack.

**Important:** If the `prompt` field is empty or the `[assist]` section is omitted, the workspace will be skipped during assist command execution.

### Usage

The assist feature requires:
- A model declared as an `[[llm_model]]` entry in a global config file (see
  [Model definitions](#model-definitions-llm_model)) and named with
  `--llm-model`, plus the credentials that entry's `provider` requires.
- Slack Bot Token (via `--slack-bot-token`)

Declare the model once:

```toml
# global.toml — pass via --global-config

[[llm_model]]
alias    = "assist"
provider = "gemini"
model    = "gemini-3.7-flash"
input_usd_per_mtok  = 0.75
output_usd_per_mtok = 3.75
```

Run the assist command (Gemini on Vertex AI example):

```bash
hecatoncheires assist \
  --slack-bot-token=xoxb-YOUR_BOT_TOKEN \
  --global-config=./global.toml \
  --llm-model=assist \
  --llm-gemini-project-id=YOUR_GCP_PROJECT \
  --llm-gemini-location=global \
  --workspace=risk
```

With an OpenAI model (`provider = "openai"` in the entry):

```bash
hecatoncheires assist \
  --slack-bot-token=xoxb-YOUR_BOT_TOKEN \
  --global-config=./global.toml \
  --llm-model=assist \
  --llm-openai-api-key=sk-YOUR_OPENAI_KEY \
  --workspace=risk
```

With Claude on Vertex AI (`provider = "claude"` in the entry):

```bash
hecatoncheires assist \
  --slack-bot-token=xoxb-YOUR_BOT_TOKEN \
  --global-config=./global.toml \
  --llm-model=assist \
  --llm-gemini-project-id=YOUR_GCP_PROJECT \
  --llm-gemini-location=us-east5 \
  --workspace=risk
```

The AI agent will:
1. Retrieve recent assist logs and Slack messages for context
2. Analyze open cases in the specified workspace
3. Execute the custom prompt to generate insights
4. Post findings to the case's Slack channel

### Agent tool registry (Slack mention & assist)

> **The full tool catalogue — every tool name, what it does, and which runtime
> context (Job vs. interactive mention agent) exposes it — lives in
> [Agent Tools](agent_tools.md).** Read it before naming a tool in any prompt;
> the palette a Job gets is narrower than the interactive agent's. The table
> below is only a quick reference for *which optional service enables which
> tool group* for the interactive mention / assist agent.

| Tool group | Enabled by |
|------------|------------|
| `core__*` (actions) + `case__*` (case edits) | Always (a case context exists). |
| `slack__search_messages` | `HECATONCHEIRES_SLACK_USER_OAUTH_TOKEN` with the `search:read` scope. See [docs/slack.md](slack.md#user-token-scopes). |
| `slack__get_messages`, `slack__post_message` | `HECATONCHEIRES_SLACK_BOT_TOKEN`. |
| `notion__search`, `notion__get_page`, `notion__get_database`, `notion__search_database` | `HECATONCHEIRES_NOTION_API_TOKEN`. See [Integrations → Notion](integrations/notion.md). |
| `github__*` | The `--github-app-*` flags. See [Integrations → GitHub](integrations/github.md). |
| `jira_*` | The `--jira-*` flags (`HECATONCHEIRES_JIRA_BASE_URL` / `_EMAIL` / `_API_TOKEN`). See [Integrations → Jira](integrations/jira.md). |
| `webfetch` | A configured web-fetch client. |
| `knowledge__*` | Always (write is withheld on private cases). |
| `memo__*` | A `[memo]` section with at least one memo field defined. |

---

## Action Section

The `[action]` section is **optional**. When omitted, the workspace inherits a built-in default set of action statuses (`BACKLOG`, `TODO`, `IN_PROGRESS`, `BLOCKED`, `COMPLETED`) so that data written before configurable statuses keeps working unchanged. Define this section to tailor the action workflow to your team.

```toml
[action]
initial = "queued"            # Required when [action] is present
closed  = ["done", "cancelled"]   # Optional, defaults to []

[[action.status]]
id = "queued"
name = "Queued"
description = "Awaiting triage"
color = "idle"                 # See "Color values" below
emoji = "📋"

[[action.status]]
id = "doing"
name = "Doing"
color = "active"
emoji = "▶️"

[[action.status]]
id = "done"
name = "Done"
color = "success"
emoji = "✅"
```

### `[action]` keys

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| `initial` | string | **Yes** | ID of the status assigned to newly created actions. Must match one of the `[[action.status]]` IDs. |
| `closed` | string[] | No | IDs treated as "closed" for filtering / completion checks. Each ID must match a defined status. Defaults to `[]`. |

### `[[action.status]]` keys

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| `id` | string | **Yes** | Stable identifier. Must match `^[A-Za-z0-9]+([_-][A-Za-z0-9]+)*$` and be unique within the workspace. Stored in the database as the action's status. |
| `name` | string | **Yes** | Display label. Write it in whichever language fits the workspace — there is no localization layer. |
| `description` | string | No | Free-form description (currently unused in the UI; reserved for tooltips). |
| `color` | string | No | UI color. See "Color values" below. Defaults to the neutral `idle` preset. |
| `emoji` | string | No | Single emoji used in Slack messages and web UI badges. |

### Color values

The `color` field accepts **two and only two** kinds of value. Any other string fails validation at startup.

#### 1. Semantic preset name (recommended)

Each preset expresses the **kind** of state, not a specific named status. Pick the preset that matches your status's intent — preset names are matched case-insensitively.

| Preset | Intended meaning | Suggested for statuses like |
|--------|------------------|-----------------------------|
| `idle` | Not started yet, queued, reserved | `backlog`, `queued`, `new` |
| `active` | Work in progress, someone is on it | `in_progress`, `doing`, `wip` |
| `waiting` | Waiting on someone else (review / external response / dependency) | `in_review`, `waiting_for_user`, `qa` |
| `paused` | Intentionally on hold | `on_hold`, `parked` |
| `attention` | Needs attention / something off-track | `needs_input`, `flagged` |
| `blocked` | Cannot proceed; something is preventing progress | `blocked`, `dependency_failed` |
| `success` | Completed in the desired way | `done`, `resolved`, `shipped` |
| `neutral_done` | Closed without value judgment (won't fix, expired, withdrawn) | `cancelled`, `wont_fix`, `expired` |
| `failure` | Closed in an undesired way (failed, rejected, abandoned) | `failed`, `rejected`, `lost` |

Presets resolve to CSS variables (`--action-status-<preset>`) on the frontend and follow the active light/dark theme.

#### 2. Hex color code

When no preset captures what you need, supply an absolute color as `#RRGGBB` or `#RGB`. Example: `"#5EAEDC"`, `"#abc"`. Hex codes do **not** follow the theme, so pick a color that reads on both light and dark backgrounds.

#### Not supported (will fail validation)

- CSS variable references like `"var(--ok)"` — the supported entry point is the preset list above.
- CSS color keywords like `"red"`, `"blue"`.
- Function notations: `rgb(...)`, `rgba(...)`, `hsl(...)`, `oklch(...)`.
- Any string that is neither a preset name nor `#RRGGBB`/`#RGB`.

#### Examples

| Value | Result |
|-------|--------|
| `"success"` | ✅ Preset, theme-aware green |
| `"WAITING"` | ✅ Case-insensitive preset match |
| `"#5EAEDC"` | ✅ Absolute hex |
| `"#abc"` | ✅ Short hex |
| (omitted) | ✅ Falls back to `idle` |
| `"var(--ok)"` | ❌ Validation error |
| `"red"` | ❌ Validation error |
| `"rgb(255,0,0)"` | ❌ Validation error |

---

## Case Section (thread mode)

The `[case]` section configures the status set that attaches to **Cases** in
thread mode (`[slack] mode = "thread"`). It is **required** for thread-mode
workspaces and **ignored** (with a startup warning) in channel mode, where the
configurable status set lives on Actions instead (`[action]`).

The shape mirrors `[action]` exactly — the same keys, the same color values, and
the same validation rules apply (see [Action Section](#action-section)).

```toml
[slack]
mode = "thread"
channel = "C0123456789"   # the monitored channel ID

[case]
initial = "triage"            # Required: status assigned to a newly created Case
closed  = ["done", "wontfix"] # IDs treated as closed (closes the Case via lifecycle sync)

[[case.status]]
id = "triage"
name = "Triage"
color = "active"
emoji = "🩺"

[[case.status]]
id = "investigating"
name = "Investigating"
color = "waiting"

[[case.status]]
id = "done"
name = "Done"
color = "success"
emoji = "✅"
```

The Kanban board renders one column per `[[case.status]]` for thread-mode
workspaces; dragging a Case into a `closed` column closes the Case. The
investigation agent can also move a Case to a closed status when a mention
indicates the issue is resolved.

### Case trigger (thread mode)

`[slack] trigger` selects **what starts a Case** in a monitored channel:

- **`instant`** (default): every channel-root post starts a Case. This is the
  original thread-mode behaviour — good for a dedicated intake channel where each
  post is a request. As a recovery path, an @mention inside a thread that has
  **no Case yet** also starts one (seeded by the whole thread) — so a thread whose
  root never became a Case (a message subtype or a bot post that `instant`
  creation skipped, or a thread predating the bot) can still be turned into a Case
  by mentioning the bot in it.
- **`mention`**: a Case is started **only when the bot is @mentioned**, either in
  a channel-root message or inside a thread that has no Case yet. A plain post
  (no mention) is left alone. Use this in a busy, shared channel where only some
  conversations should become Cases. The mention text (and, for an in-thread
  mention, the surrounding thread) seeds the initialization agent, so the Case is
  created from the same context a human would have read.

In **both** modes an @mention inside a thread that has **no Case yet** starts a
Case, and an @mention inside a thread that **already** has a Case runs the
investigation agent (respond / update / close), not creation. The `accept_bot`
gate applies to whichever event drives creation: a bot-authored channel-root post
(`instant`) or a bot-authored @mention (either mode). The one instant-only
difference is the channel root: in `instant` the channel-root **message** drives
root creation, so a channel-root @mention there is not a separate trigger. A
follow-up @mention on a still-Case-less thread resumes the in-flight
initialization (superseding a pending question) rather than starting a second
turn; the agent runtime serialises concurrent triggers on the thread's subject.

### Case agent prompts (`[case.prompts]`)

Thread-mode case **initialization** is agent-driven and is started by the
configured [case trigger](#case-trigger-thread-mode) — a channel-root post
(`instant`) or an @mention (`mention`). The trigger may be authored by a human
**or** by an integration bot — e.g. an intake-form app that relays a request on a
person's behalf. **Bot-authored triggers are opt-in**: they start a Case only when
the workspace sets `[slack] accept_bot = true` (default `false`, so a
channel is not flooded with a Case per bot notification); human triggers always
start a Case. The Case **reporter** is, as a
rule, the trigger's author. Only when the trigger is bot-authored (so it has no
human author) is the reporter taken from the first Slack user mention in the body
(typically the requester named in the form); if it names no user, the
reporter is left **empty** — a thread-mode Case is allowed to have no reporter,
so creation still proceeds. When such a trigger arrives, the bot does **not**
create a Case immediately.
Instead it runs a plan-and-execute agent that investigates
(all read-only search tools are available), may ask the reporter to clarify
intent (a question form posted to the thread), and only commits a Case once it
can fill a valid title, description, and every **required** custom field. If
validation fails — a required field missing, or a value outside the allowed
options — the agent is told what is wrong and tries again, all bounded by the
turn's budget and step ceiling. A pending question is answered by submitting the
question form (not by a free-text reply), and every question on it must be answered
(see [user_guide.md](./user_guide.md#question-form-every-question-must-be-answered)).
When the agent cannot conclude, it posts
a "couldn't conclude" notice. On success the bot posts a Block Kit
summary of the created Case.

The optional `[case.prompts]` sub-table injects workspace-specific guidance into
this agent. Today only the `create` key (the initialization agent) is consumed;
`mention` / `close` are reserved for future phases.

```toml
[case.prompts]
create = """
For security incidents, always set the severity field and capture the affected
service in the description before creating the case.
"""
```

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| `create` | string | No | Appended to the case-initialization agent's system prompt as "Workspace-specific instructions". Capped at 16384 bytes; startup fails if exceeded. |

---

## Job Definitions (`[[job]]`)

Agent Jobs let workspace administrators declaratively wire LLM-powered automation to Case lifecycle events and periodic ticks. Each Job is defined in the workspace TOML, listens to one or more events, and runs the Plan-and-Execute agent runtime with a fixed system-prompt structure and a curated tool palette (read-only + writer).

```toml
# A minimal lifecycle Job.
[[job]]
id = "summarize_on_create"
name = "Auto-summarize on creation"
description = "Summarize a new case and post the summary to Slack."
events.case = { on = ["created"] }
prompt = """
Summarize this case in three lines or fewer and post it to the Slack channel
bound to the case via slack__post_to_case_channel.
"""

# A multi-trigger Job: fires on lifecycle events AND every hour.
[[job]]
id = "watch"
events.case = { on = ["created", "closed"] }
events.scheduled = { every = "1h" }
prompt = "Take any appropriate action..."

# A cron-based scheduled Job.
[[job]]
id = "daily_digest"
events.scheduled = { cron = "0 9 * * *" }  # 09:00 UTC every day
prompt = "Post a status digest to the case Slack channel."
```

### Fields

| Field         | Type     | Required | Notes |
|---------------|----------|----------|-------|
| `id`          | string   | yes      | Workspace-unique, snake_case (`^[a-z0-9]+(_[a-z0-9]+)*$`). |
| `name`        | string   | no       | Human-readable label for logs. |
| `description` | string   | no       | Free-form description for operators. |
| `prompt`      | string   | (\*\*)   | Inline prompt. Go `text/template`. Has access to `.Case`, `.Workspace`, `.Event`. |
| `prompt_file` | string   | (\*\*)   | Path to a file holding the prompt, resolved relative to this config file's directory. Use it when the prompt is too long to inline comfortably. |
| `disabled`    | bool     | no       | Defaults to `false` (= active). Set `true` to temporarily disable. |
| `quiet`       | bool     | no       | Defaults to `false`. Set `true` to suppress the operational Slack session log (see *Session log* below). |
| `strategy`    | string   | no       | `"simple"` (default) or `"planexec"`. See *Execution strategy* below. |
| `interactive` | bool     | no       | Defaults to `false`. Set `true` to let the Job pause mid-run and ask the user a question in Slack, resuming when they answer. **Requires `strategy = "planexec"`.** See *Interactive Jobs* below. |
| `reflection`  | bool     | no       | Defaults to `false`. Set `true` to run a post-execution reflection pass after a successful run. See *Reflection* below. |
| `llm_model`   | string   | no       | Reference name of an `[[llm_model]]` entry in the global config (its `alias`, or its `model`). Empty uses the deployment's default model. A name no entry defines fails at startup and in `validate`. See *Model definitions* below. |
| `budget_usd`  | float    | no       | Amount in USD one run of this Job may spend, sub-agents included, before it is told to conclude. It is **not** a bound on what the run is charged — see [cli.md](./cli.md#agent-runtime-budgets) for the three things spent on top of it. Omitted (or `0`) uses the deployment's default budget. See *Model definitions* below. |
| `events.case` | table    | (\*)     | `on = ["created" \| "closed", ...]`. Always an array. |
| `events.scheduled` | table | (\*)   | Exactly one of `every = "1h"` or `cron = "0 9 * * *"`. |

(\*\*) Exactly one of `prompt` or `prompt_file` must be set; supplying both, or neither, fails at config load time.

### Prompt source (`prompt` vs `prompt_file`)

Provide the prompt either inline via `prompt` or from an external file via
`prompt_file` — never both. `prompt_file` is resolved **relative to the
directory of the config file that declares the Job**, so a prompt can live
next to its workspace TOML and grow without bloating it:

```toml
[[job]]
id = "deep_investigation_on_create"
prompt_file = "prompts/deep_investigation.md"  # relative to this config.toml
events.case = { on = ["created"] }
```

The file's contents are loaded at startup and behave exactly like an inline
`prompt` (Go `text/template` with access to `.Case`, `.Workspace`, `.Event`);
trailing whitespace is trimmed. A missing or empty file fails loudly at load
time.

### Execution strategy

The `strategy` field selects which runtime drives the Job's LLM loop.
Defaults to `simple`, which is the v1 behaviour: a single
`gollem.Agent.Execute` call with the configured tool set. Set it to
`planexec` to opt into the plan-and-execute runtime — a planner LLM emits
a list of parallel sub-agent tasks, the runtime fans them out, replans
with the observations, and finishes with a plain-text summary. A Job's
sub-agents may perform the deliverable action themselves (e.g. post the
result to the case channel via `slack__post_to_case_channel`) once the
planner has gathered enough context — they are not restricted to
observation-only investigation. As a rule the planner investigates first
and dispatches the write as a later task, but it may write immediately
when the action is self-evident.

```toml
[[job]]
id = "deep_investigation_on_create"
prompt = "Investigate the case from every angle and summarise findings."
strategy = "planexec"
events.case = { on = ["created"] }
```

| Strategy | When to use |
|----------|-------------|
| `simple` (default) | Single-step actions: post a digest, set a status, send a Slack reply. The Job's prompt is a direct instruction the agent executes in one ReAct loop. |
| `planexec` | Multi-step investigations: pull context from several sources, cross-reference, and produce a structured summary. Each round runs up to 5 sub-agent tasks in parallel; how many rounds a turn gets is decided by its budget and step ceiling rather than by a round count. See [cli.md](./cli.md#agent-runtime-budgets). |

### Interactive Jobs (`interactive`)

By default a Job runs **unattended** — it never waits for a human. Set
`interactive = true` to let the Job **pause to ask the user a question** when
the planner needs clarification, then resume once the user answers.

```toml
[[job]]
id = "triage_on_create"
prompt = "Investigate the case and ask the reporter if anything is unclear."
strategy = "planexec"   # required for interactive
interactive = true
events.case = { on = ["created"] }
```

How it works:

1. When the planner decides it needs input, the run **suspends**: a Block Kit
   question form is posted into the Case's Slack thread, and the run is
   recorded as *awaiting input* (its lease is released so it does not block
   other work).
2. The user answers the form and clicks **Submit**. Every question must be
   answered; a Submit that leaves one blank keeps the run suspended and tells
   the submitter which questions are blank (see
   [user_guide.md](./user_guide.md#question-form-every-question-must-be-answered)).
3. The run **resumes** under the same conversation history — the planner sees
   everything it already gathered plus the answer, and continues to a
   conclusion (it may ask again if needed).

Constraints and behaviour:

- **`strategy = "planexec"` is mandatory.** Setting `interactive = true` on a
  `simple` Job (or with no `strategy`, which defaults to `simple`) fails at
  config load — the single-loop runtime has no question mechanism.
- A question is posted into the Case's Slack thread. An interactive Job whose
  Case has no Slack thread to ask in fails the run loudly rather than dropping
  the question.
- While a run is awaiting input, the scheduler does **not** start a new run for
  the same Job × Case — the suspended run owns the slot until it is answered.
- The question form is **not** suppressed by `quiet`: `quiet` only silences the
  operational session-log markers, whereas a question is a deliberate
  interaction with the user.
- An interactive Job's run **stays open** across the wait, so its budget and step
  ceiling are not granted fresh when the human answers: the whole exchange is one
  run against one set of ceilings. Size `budget_usd` for the exchange, not for the
  first turn of it.

Interactive Jobs require the agent conversation history to be persisted in a
shared backend (Cloud Storage) so a resume — which may be handled by a
different instance — can continue the conversation. This is already the case
in any deployment with Slack wired (see `HECATONCHEIRES_CLOUD_STORAGE_BUCKET`).

### Reflection

When `reflection = true`, a successful run is followed by a **reflection pass**:
a knowledge-only agent inherits the run's full conversation (via the run
history) and curates the workspace's shared **Knowledge** and **Tags** — adding
durable, non-obvious learnings, augmenting or correcting existing entries, and
keeping the tag vocabulary tidy. It only has the knowledge / tag tools
(`knowledge__*`); it cannot touch the case, actions, or Slack.

```toml
[[job]]
id = "investigate_and_learn"
prompt = "Investigate the case and post a summary."
strategy = "planexec"
reflection = true
events.case = { on = ["closed"] }
```

Reflection is **best-effort and never affects the run's outcome**: if it fails,
the failure is reported through the error tracker but the Job still counts as
SUCCESS. It works with both strategies. It is **skipped** for:

- failed runs (only successful runs are reflected on),
- **private cases** — their contents must not leak into the workspace-wide
  shared knowledge.

The reflection agent's LLM and tool calls are recorded in the run's event
timeline under the `reflection` phase, so you can inspect what it learned.

Notes:

- `planexec` Jobs run **unattended** — they cannot ask the operator a
  question mid-run. Use `simple` if your Job is interactive in spirit
  but happens to need a small amount of planning; the planexec
  Question feature is reserved for the proposal mode.
- `planexec` Jobs surface their per-phase progress through the same
  JobRunLog event trail as `simple`, so the Cases UI shows you the
  plan, sub-agent activity, and final summary in order.
- The JobRunLog `executorKind` field is recorded as `single_loop`
  (simple) or `plan_execute` (planexec). The Cases UI renders the
  Run row with a `planexec` chip when the Job ran under the
  plan-and-execute runtime.

(\*) At least one of `events.case` / `events.scheduled` must be present.

### Events and scheduling

A Job subscribes to one or both event domains, and the runtime fires **one
invocation per `(job, case)` pair** — never one invocation for a whole
workspace. This per-case model is the single most important thing to internalise
when writing a Job prompt: your prompt runs against **one case at a time**, with
that case already loaded into the system prompt as `.Case`. **Do not** write a
prompt that tries to enumerate or loop over cases — the agent has no tool to
list other cases, and it doesn't need one.

**`events.case`** — fires the moment a case lifecycle transition is persisted:

| `on` value | Fires when |
|------------|-----------|
| `created`  | A new (published) case is created. |
| `closed`   | A case is moved to a closed status. |

**`events.scheduled`** — a periodic sweep (driven by `hecatoncheires tick` or
`POST /hooks/tick`) decides which Jobs are due. Exactly one of:

| Key | Format | Meaning |
|-----|--------|---------|
| `every` | Go duration string (`"24h"`, `"6h"`, `"30m"`, `"1h30m"`) | Fire when at least this long has elapsed since the last successful run for that `(job, case)`. |
| `cron`  | 5-field cron expression (`min hour day-of-month month day-of-week`), **UTC** | Fire on the cron schedule. No seconds field and no `@`-descriptors (`@hourly` etc.). |

Key facts that are easy to get wrong:

- **Scheduled Jobs run only against OPEN cases** — cases that are still open and
  published. Closed (`CLOSED`) and draft cases are skipped entirely, so a daily
  digest Job will simply stop firing for a case once it closes.
- **Cron is always UTC.** To fire at 08:30 Asia/Tokyo (JST = UTC+9), write
  `"30 23 * * *"`. There is no per-Job timezone setting.
- **The first scheduled run is always due**, regardless of `every` / `cron`,
  because there is no prior run to measure against.
- A Job may subscribe to **both** domains at once (e.g. react to `created` *and*
  sweep `every = "1h"`); each matching event produces its own invocation.

The operational mechanics of the sweep — concurrency leasing, duplicate
suppression, the `tick` CLI vs. the HTTP hook — are covered in
[Operations → Agent Jobs operations](operations.md#agent-jobs-operations).

### Inspecting a Case's Jobs (Web UI)

A Case's **Agent** page (`/ws/{workspace}/cases/{id}/agent`) surfaces an
**Automated Jobs** section — between the *Settings* and *Run history*
sections — that lists the enabled Jobs which can fire against that Case,
with their trigger conditions, execution strategy, and (on click) the full
prompt. It is read-only; Job definitions are managed in the workspace TOML.

What appears there mirrors the firing rules above:

- **Case-lifecycle Jobs** (`events.case`) are always listed, regardless of
  the Case's current status.
- **Scheduled Jobs** (`events.scheduled`) are listed only while the Case is
  **OPEN**, because the scheduled sweep skips `CLOSED`/draft cases.
- **Disabled Jobs** (`disabled = true`) never appear.

The page is backed by the `caseJobs(workspaceId, caseId): [CaseJob!]!`
GraphQL query. Like the run-history queries it enforces private-case access
control: a caller who is not a member of a private Case's channel is refused.

A single run's detail page also reports **what that run spent** and **which model
it ran on**, beside its duration and trigger. The cost is priced when the run
ends, at the rate then configured for that model, so it is the same figure the
run's budget was judged against; correcting a price afterwards does not restate
it. A run whose recorded cost is zero shows an em dash rather than `$0.00`, so a
fraction of a cent is not confused with nothing; note that zero covers two cases
the page cannot tell apart — a run that finished before costs were tracked, and a
run that ended before it reached the model at all. The same two values are
exported to BigQuery as `cost_nano_usd` and `model` (see
[export.md](./export.md)), where the scope of what the figure covers is stated in
full.
`CaseJob.trigger.schedule.everySeconds` reports `every` as whole seconds and
`cron` reports the original cron expression (the two are mutually exclusive).

### Running a Job by hand (Web UI)

Each row of the **Automated Jobs** list carries a **Run** button that starts
that Job against the Case immediately, without waiting for its trigger
condition. The run is identical to an event-driven one — same prompt, same
tools, same Slack session log (subject to `quiet`), same reflection pass —
and appears in the *Run history* table below, where the **Trigger** column
reads `Manual`.

Rules worth knowing:

- **Only the Jobs shown in the list can be started.** The button and the API
  share one definition of "runnable for this Case", so a disabled Job, or a
  scheduled-only Job on a `CLOSED` case, cannot be started by hand either.
- **One run at a time per `(job, case)`.** While a run holds the lease — or
  while an interactive run is waiting on a question the user has not answered
  yet — a second Run press is refused with a conflict error instead of
  queueing. A question left unanswered past the timeout no longer blocks it:
  the next run recovers the abandoned one, exactly as an event-driven trigger
  does.
- **The button returns as soon as the run is accepted**, not when it
  finishes. The page then polls the run history until the run reaches its
  terminal stage.
- **The trigger reason reaching the agent differs.** The system prompt's
  *Trigger reason* section says the run was started by hand and names the
  operator. A prompt that branches on `{{ .Event.CaseLifecycle }}` sees an
  empty value on a manual run, because no lifecycle transition occurred —
  write such a prompt to tolerate that, or keep it out of the prompt.

The button is backed by the
`triggerCaseJob(workspaceId, caseId, jobId): Boolean!` GraphQL mutation. It
enforces the same private-case access control as `caseJobs` and reports
refusals through `extensions.code`: `FORBIDDEN` (not a member of a private
Case), `NOT_FOUND` (unknown Case, or a Job that is not runnable for it) and
`CONFLICT` (a run is already in flight).

### Session log

Each Job run posts a minimal operational log to the Case's Slack channel so
operators can see the agent working in real time. The log is intentionally
sparse — a starting marker, a few progress lines, and a completion / failure
marker — not a play-by-play of every LLM call.

- **Starting marker** — when the run begins, the runtime posts a
  `starting... <job_id>` message. This message roots the run's **session
  thread**: all subsequent log lines for that run are replies to it.
  - *Channel-mode Cases* (the Case owns a dedicated channel): the marker is
    a new channel-root message; progress and completion replies thread under
    it.
  - *Thread-mode Cases* (the Case lives in a Slack thread): the marker is a
    reply in the Case thread, and the Case thread doubles as the session
    thread (Slack nests only one level).
- **Progress lines** — the first time the run executes each distinct tool, a
  single line is posted to the session thread. Repeat calls to the same tool
  stay silent, so the log stays short.
- **Completion / failure marker** — when the run finishes, a success or
  failure marker (with the error text on failure) is posted to the session
  thread.

The agent's own `slack__post_message` tool is **not** part of the session
log: it remains a deliberate agent action, posted where the agent directs
(channel root in channel-mode Cases), and is never suppressed by `quiet`.

Set `quiet = true` to disable the entire session log (starting marker,
progress lines, and completion / failure marker) for a Job — useful for
high-frequency or purely background Jobs whose Slack chatter would be noise.
The run still executes and records its full trace in the Cases UI. The
session log also no-ops automatically when the deployment has no Slack
service wired (e.g. the scheduled-tick CLI) or when the Case has no bound
Slack channel.

### System prompt

The runtime constructs a structured system prompt every invocation. The
contents are fixed by the runtime — Job authors only control
`prompt` (the user message).

| Section                  | Content |
|--------------------------|---------|
| Role                     | Agent role and tone (fixed text). |
| Current time             | The turn's execution start time in UTC RFC3339, so the agent has an absolute "now" to reason about recency and elapsed time. Omitted only when the runtime did not supply a timestamp. |
| Workspace                | `id`, `name`, `description`, and the custom field schema. Each field's `id`, `name`, `type`, `required` flag (when true), `description` (when set), and — for `select` / `multi-select` — every option's `id`, `name`, `description`, and freeform metadata pairs are emitted so the agent knows the field's constraints and the meaning of each option ID. |
| Board statuses           | The workspace's case board statuses (`id`, `name`, `description`, and a `(closed)` marker), rendered only when the workspace defines a case status set. This is the vocabulary `case__update_case_status` accepts; the case's *current* status is the `board_status` line of the Case section. |
| Case                     | The persisted state of the current case: `id`, `title`, `description`, lifecycle `status`, `board_status` (thread-mode only — a channel-mode case has no board, so the line is omitted), `is_test`, `is_private`, `archived_at`, `reporter`, `assignees`, `channel_user_ids`, `slack_channel_id`, `slack_thread_ts`, `created_at`, `updated_at`, and `field_values`. `is_test`, `is_private`, `archived_at` and `channel_user_ids` are always rendered, using `(empty)` when unset, because the agent has no tool to read the case back and cannot otherwise tell an unset value from a withheld line. `field_values` carries one line per field the workspace schema declares, in schema order — an unset field renders `(empty)` rather than being skipped — followed by any stored value whose field the schema no longer declares. `select` / `multi-select` entries are rendered as `id: <raw> (<option name>, ...)` so the agent can map raw option IDs back to their human-readable label without re-consulting the schema. Unknown option IDs fall back to the raw value. `access_denied` (runtime-only) and `request_key` (Slack modal de-duplication) are deliberately excluded. |
| Per-case operator notes  | Rendered only when the Case has `AgentAdditionalPrompt` set (see below). |
| Actions                  | Existing non-archived actions, for de-duplication. |
| Trigger condition        | The Job's declared subscription (events.case / events.scheduled). |
| Trigger reason           | The concrete event that fired this invocation (timestamps, actor, lifecycle / cron tick). |
| Guardrails               | Fixed restrictions the runtime injects. See [Guardrails](#guardrails) below. |
| Tools                    | gollem auto-injects the resolved tool list — see [Agent Tools](agent_tools.md). |

### Guardrails

The runtime injects a fixed set of restrictions into every Job's system prompt.
These are **not** configurable from the TOML, and a Job prompt cannot grant
itself more than they allow (a per-case *Additional prompt* cannot either — it
is extra context, not extra capability):

- **A Job will not close a case.** Closing is a human-only decision. The agent
  is told not to move a case to a closed status, even though
  `case__update_case_status` could technically do it. (The interactive,
  human-initiated mention agent *may* close a case — this restriction is
  specific to unattended Jobs.)
- **A Job cannot delete cases, archive actions, or delete action steps.**
- **A Job posts only to the case's own bound Slack channel** — never to an
  arbitrary channel.
- **A Job cannot read its own past run traces;** it judges idempotency from the
  current case state, action list, and Slack history. So a "don't repeat
  yourself" prompt must lean on the visible case state, not on memory of prior
  runs.

Which of these are enforced in code (the tool is absent) versus only by the
prompt (the tool exists but the agent is instructed not to use it that way) is
spelled out in [Agent Tools → Guardrails](agent_tools.md#guardrails). The
distinction matters: a prompt-only guardrail is a firm instruction, so do not
write a Job prompt that tries to argue the agent out of it.

### Per-case agent customisation

The Case Agent page (`/ws/{ws}/cases/{id}/agent`) lets operators attach
two Case-scoped knobs that the runtime applies on top of the TOML Job
definition:

- **Additional prompt**: a Markdown snippet (max 16,384 bytes) that is
  rendered into the `Per-case operator notes` section of the system
  prompt. It does **not** replace the TOML prompt or the Guardrails —
  treat it as extra context, not as a way to grant new capabilities.
- **Source allowlist** (`AgentSourceIDs`): when empty, every enabled
  Workspace Source remains in play. When non-empty, Source-aware tools
  MUST narrow themselves to the listed IDs. Unknown / deleted IDs are
  silently skipped at use time so a Source toggled off later does not
  invalidate the stored selection.

Both values round-trip through the `updateCaseAgentSettings` GraphQL
mutation. Drafts cannot carry agent settings (they have no Slack
channel and no Job runs against them).

---

## Authorization Section (`[authz]`)

The optional `[authz]` section restricts which users may use a workspace. It
names one or more Rego policy files; the policy decides, per user, whether that
user may access this workspace at all.

```toml
[authz]
policy = ["policies/authz"]   # files or directories, relative to this config file
```

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| `policy` | array of strings | yes (when the section is present) | Rego files or directories. A directory is read recursively and every `.rego` file in it is loaded. A relative path is resolved against the directory of the config file that declares it; an absolute path is used as is. |

A workspace **without** an `[authz]` section allows every user, which is the
behavior of a deployment that predates this section.

### Writing the policy

The policy must declare `package authz` and a boolean `allow` rule. The
application evaluates the whole `data.authz` document and reads only `allow`:

- `allow` is `true` → the user may access the workspace.
- `allow` is `false` or undefined (no `allow` rule matched) → denied.
- Other rules in the package are ignored.

The policy receives this `input`:

| Field | Description |
|-------|-------------|
| `input.workspace.id` | Workspace ID (`[workspace] id`) |
| `input.workspace.name` | Workspace name (`[workspace] name`) |
| `input.user.id` | Slack user ID of the user being checked (e.g. `U01234567`) |
| `input.user.email` | Email address of the Slack user |
| `input.user.name` | Slack handle |
| `input.user.display_name` | Slack display name, falling back to the real name |

Every `input.user` field except `id` comes from the Slack user list the server
already synchronises into its database (every 10 minutes, and at startup). No
Slack API call is made to answer an authorization question. A user who is not
in that list yet — someone who joined Slack after the last synchronisation —
is evaluated with `email`, `name` and `display_name` set to `""`, so a policy
that matches on those fields denies them until the next synchronisation.

Example: allow members of one email domain, plus a fixed list of Slack users.

```rego
package authz

allow if endswith(input.user.email, "@example.com")

allow if input.user.id in {"U0123ABCD", "U0456EFGH"}
```

### Where the policy is enforced

The decision applies to every path on which a human user acts on the
workspace:

- **Web UI / GraphQL** — the workspace list shows only permitted workspaces,
  and every operation that names a denied workspace is answered with the
  `FORBIDDEN` error code. `GET /api/workspaces` requires a signed-in session
  when authentication is enabled and lists only permitted workspaces. The
  home-screen greeting is reused (for up to an hour) only while the user's
  permitted workspaces are exactly those it was generated for, so a revoked
  workspace stops appearing in it as soon as the decision below expires.
- **Slack** — mentions, messages, reactions, slash commands, modals, buttons
  and select menus in a denied workspace are not processed. The user is told so
  in a message only they can see (an ephemeral message, or a modal when the
  interaction opened one). When a mention or slash command must choose a
  workspace, only permitted workspaces are offered.
- **Agent** — the workspace-listing tools list only the workspaces the
  requesting user may access.

The policy is **not** applied to:

- operations with no human user behind them: scheduled Jobs, `tick`,
  `export`, and messages posted by bots;
- the MCP endpoint (`/mcp`), which has its own policy (see
  [MCP Server](mcp.md)).

### Decision caching and failure handling

- A decision is cached for **60 seconds** per server instance, keyed on the
  Slack user. A policy or Slack-profile change therefore takes effect within a
  minute; each instance keeps its own cache. A user who is not in the synced
  Slack user list yet is not cached, so their decision updates as soon as the
  next synchronisation stores them.
- If a decision cannot be made — the policy errors at evaluation time, or the
  Slack user list cannot be read — access is **denied** and nothing is cached.

### Startup check

At startup each workspace's policy is compiled and evaluated once against two
sample inputs: a user with a synced Slack record, and a user without one (all
fields but `id` empty). Startup fails if the policy does not compile or if
either evaluation returns an error — for example, `allow` is not a boolean, or
two rules assign `allow` conflicting values. The check verifies only that the
policy evaluates; it does not check what the policy decides.

---

## Validation Rules

The configuration file is validated at startup. The following rules are enforced:

| Rule | Error |
|------|-------|
| Configuration file must exist at the specified path | `ErrConfigNotFound` |
| All field IDs must match `^[a-z][a-z0-9_]*$` | `ErrInvalidFieldID` |
| All field names must be non-empty | `ErrMissingName` |
| Field type must be one of the 8 supported types | `ErrInvalidFieldType` |
| Field IDs must be unique across the entire configuration | `ErrDuplicateFieldID` |
| `select` and `multi-select` fields must have at least one option | `ErrMissingOptions` |
| All option IDs must match the same pattern as field IDs | `ErrInvalidFieldID` |
| All option names must be non-empty | `ErrMissingName` |
| Option IDs must be unique within their parent field | `ErrDuplicateOptionID` |
| `validation.pattern` is set only on `text` fields | `ErrUnexpectedPattern` |
| `validation.pattern` is a valid Go RE2 regular expression | `ErrInvalidPattern` |
| Slack welcome message templates must parse | `ErrInvalidWelcomeMessage` |
| Action status IDs must match `^[A-Za-z0-9]+([_-][A-Za-z0-9]+)*$`, be at most 32 characters, and be unique within the workspace | (action status validation) |
| `[action] initial` must reference a defined `[[action.status]] id` | (action status validation) |
| Each entry in `[action] closed` must reference a defined `[[action.status]] id` | (action status validation) |
| `[[action.status]] color` must be a preset name or `#RRGGBB` / `#RGB` | (action status validation) |
| `[authz] policy` must list at least one path, and no empty path | `ErrAuthzPolicyEmpty` |
| `[authz]` policy files must exist and compile | (policy compile error) |
| `[authz]` policy must evaluate without error against the startup sample inputs | `ErrAuthzPolicyTrialFailed` |

If any validation fails, the application exits with a descriptive error message including the field ID and context.

---

## Complete Example

The following example configures Hecatoncheires as a security risk management system. See also [examples/config.toml](../examples/config.toml).

```toml
# Workspace configuration (required)
[workspace]
id = "risk"
name = "Risk Management"

# Customize entity labels to match your domain
[labels]
case = "Risk"

# Slack integration (optional)
[slack]
channel_prefix = "risk"

# Auto-invite users and groups to case channels (optional)
[slack.invite]
users = ["U12345678"]
groups = ["@security-response"]

# AI assist configuration (optional)
[assist]
prompt = "Check action deadlines and follow up on pending items. Remind the team of overdue tasks."
language = "Japanese"

# Category field: multi-select
[[fields]]
id = "category"
name = "Category"
type = "multi-select"
required = true
description = "Risk classification. Multiple values may apply."
options = [
  { id = "data_breach", name = "Data Breach", description = "Risk of personal or confidential information leakage" },
  { id = "system_failure", name = "System Failure", description = "Risk of system or service downtime and failures" },
]

# Likelihood field: select with scoring (score lives in metadata)
[[fields]]
id = "likelihood"
name = "Likelihood"
type = "select"
required = true
description = "How likely the risk is to materialise."
options = [
  { id = "low", name = "Low", description = "Unlikely to occur (a few times per year)", metadata = { score = 2 } },
  { id = "high", name = "High", description = "Likely to occur (about once per week)", metadata = { score = 4 } },
]

# Simple fields without options
[[fields]]
id = "specific_impact"
name = "Specific Impact"
type = "text"

[[fields]]
id = "deadline"
name = "Deadline"
type = "date"

[[fields]]
id = "reference_url"
name = "Reference URL"
type = "url"
```

---

## Global Configuration

Everything above lives in a **workspace** config file passed via `--config`,
where the rule is **one TOML file = one workspace**. Some settings, however,
span workspaces rather than belonging to any single one. Those live in a
separate **global** config file passed via `--global-config`
(`HECATONCHEIRES_GLOBAL_CONFIG`).

The global config holds **workspace groups**, the **`[export]`** configuration
(see [export.md](./export.md)), the **model definitions** every agent may use,
and the deployment-wide **`[agent]`** settings.

- A global config file MUST NOT contain a `[workspace]` section, and a
  workspace file MUST NOT contain `[[workspace_group]]`. Do not place the
  global config under a `--config` path.
- `--global-config` accepts one or more file/directory paths (directories are
  walked for `.toml`), just like `--config`.
- Sections that are a **set** — `[[workspace_group]]`, `[[llm_model]]` — may be
  spread across several files; only a repeated identifier is a conflict. Sections
  that configure the deployment as a whole — `[export]`, `[agent]` — must be
  declared in exactly one file.

### Workspace Groups

A workspace group bundles multiple workspaces for organization and navigation.
Grouping is **orthogonal**: which group(s) a workspace belongs to never changes
the workspace's own behavior, and groups are entirely optional. Unset
`--global-config` leaves workspace groups dormant; the GraphQL `workspaceGroups`
query returns an empty list.

### `[[workspace_group]]` keys

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| `id` | string | **Yes** | Group identifier. Must match `^[a-z0-9]+(-[a-z0-9]+)*$` and be at most 63 characters. Unique across all `--global-config` files. Independent from workspace IDs (a group and a workspace may share a name) |
| `name` | string | No | Display name. Defaults to `id` when omitted |
| `description` | string | No | Human-readable description |
| `members` | array of string | No | Workspace IDs in this group, in declared order. May be empty. Each ID must reference a workspace defined under `--config`; an unknown member fails validation at startup. Duplicates within one group are rejected. A workspace may appear in multiple groups |

### Example

```toml
# global.toml — pass via --global-config (NOT --config)

[[workspace_group]]
id          = "operations"
name        = "Operations"
description = "Day-to-day operational workspaces."
members     = ["risk", "recruit"]

# A workspace may belong to more than one group.
[[workspace_group]]
id          = "risk-review"
name        = "Risk Review"
members     = ["risk"]
```

Run with both inputs:

```bash
hecatoncheires serve \
  --config ./examples/workspaces/ \
  --global-config ./examples/global.toml
```

See `examples/global.toml` for a complete example.

### Model definitions (`[[llm_model]]`)

Which models this deployment may use — and what each costs — is declared here,
and **only** here. A Job's `llm_model` and the `--llm-model` flag both name one
of these entries by reference name; a name no entry defines is refused at startup
and by `hecatoncheires validate`.

```toml
# global.toml

[agent]
# What one agent run may spend when neither the Job nor the command line says.
# Optional; omitted falls back to the built-in $2.00.
default_budget_usd = 3.0

[[llm_model]]
provider = "claude"
model    = "claude-opus-5"        # no alias → the reference name is "claude-opus-5"
input_usd_per_mtok       = 5.0
output_usd_per_mtok      = 25.0
cache_read_usd_per_mtok  = 0.5
cache_write_usd_per_mtok = 6.25
thinking                 = "adaptive"   # optional, claude only
thinking_display         = "summarized" # optional, requires thinking
effort                   = "high"       # optional, claude only

[[llm_model]]
alias    = "cheap"                # the reference name is "cheap"
provider = "gemini"
model    = "gemini-3.7-flash"
input_usd_per_mtok      = 0.75
output_usd_per_mtok     = 3.75
cache_read_usd_per_mtok = 0.075
```

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| `alias` | string | No | The name Jobs and `--llm-model` refer to this entry by. Omitted, the reference name is `model`. Must match `^[A-Za-z0-9][A-Za-z0-9._@-]*$` and be at most 100 characters, and must be unique across every `--global-config` file |
| `provider` | string | **Yes** | `openai`, `claude` or `gemini`. It decides which credentials the entry needs (see below) |
| `model` | string | **Yes** | The model name handed to that provider |
| `input_usd_per_mtok` | float | **Yes** | Price of one million uncached input tokens, in USD. Must be positive |
| `output_usd_per_mtok` | float | **Yes** | Price of one million generated tokens, in USD. Must be positive |
| `cache_read_usd_per_mtok` | float | No | Price of one million tokens served from the prompt cache. Defaults to `0` |
| `cache_write_usd_per_mtok` | float | No | Price of one million tokens written to the prompt cache. Defaults to `0`. Only Claude reports cache writes; the value is never charged for other providers |
| `thinking` | string | No | `claude` only. Sent as `thinking.type` on every request. The only accepted value is `adaptive`. Omitted, no `thinking` is sent and the model's default applies |
| `thinking_display` | string | No | `claude` only, and only together with `thinking`. Sent as `thinking.display`: `summarized` returns a summary of the model's thinking, `omitted` returns thinking blocks with empty text. Omitted, no `display` is sent and the API's default applies |
| `effort` | string | No | `claude` only. Sent as `output_config.effort`: `low`, `medium`, `high`, `xhigh` or `max`. Omitted, no `effort` is sent and the model's default applies |

**`thinking`, `thinking_display` and `effort` are checked for their value set
only.** A value outside the lists above, one of these keys on an `openai` or
`gemini` entry, or `thinking_display` without `thinking` fails at startup.
Whether a particular model accepts a value is not checked here: a model that does
not support it rejects every request with a `400`, and that error is returned as
the run's failure. The key names follow the request fields they are sent as, so a
value the API adds to one of those fields later needs no new key.

**A `claude` entry reached through Vertex AI sends no `output_config.format`.**
Where the agent asks for a JSON reply, the response schema is stated in a user
message instead, so the API does not constrain the reply to the schema. A
`claude` entry reached with `--llm-claude-api-key` still sends the schema as
`output_config.format`.

**Write the prices from the provider's own pricing page**, in dollars per 1M
tokens — the unit those pages use. Two rules follow from what the figure is for:

- **A model whose price varies by prompt length: write the HIGHER tier.**
  Under-charging lets a run spend past its budget; over-charging only ends a run
  a little early, with whatever it found so far.
- **A price of zero is refused.** A model priced at nothing has an unbounded
  budget, which is the failure the money ceiling exists to prevent. So is a
  positive figure so small it rounds away — usually a per-token price written
  where a per-million one belongs.

**Credentials stay on the command line**, not in this file: `openai` needs
`--llm-openai-api-key`, `gemini` needs `--llm-gemini-project-id` and
`--llm-gemini-location`, and `claude` needs either `--llm-claude-api-key` (direct
Anthropic API) or `--llm-gemini-project-id` (Vertex AI) — the two are mutually
exclusive. A client is built at startup for the default model and for every model
an enabled Job names, so those credentials must be present then. Declaring a
model nobody names costs nothing.

### Agent settings (`[agent]`)

| Key | Type | Required | Description |
|-----|------|----------|-------------|
| `default_budget_usd` | float | No | What one agent run may spend, sub-agents included, before it is told to conclude, when the Job names no `budget_usd`. Not a hard ceiling — see [cli.md](./cli.md#agent-runtime-budgets). Must not be negative |

The effective budget for a run is resolved in this order, narrowest first:

1. The Job's own `budget_usd`.
2. `--agent-default-budget-usd` / `HECATONCHEIRES_AGENT_DEFAULT_BUDGET_USD`.
3. `[agent] default_budget_usd` in this file.
4. The built-in `2.0` USD.

The deployment-wide intent belongs in this document, next to the model prices it
is spent against; the flag exists so one environment can be raised temporarily
without editing it. The startup log reports the figure in force and which of the
four it came from.

---

## See Also

- [CLI Flags & Environment Variables](cli.md) — server flags, environment variables, and the diagnosis command
- [Integrations](integrations/README.md) — Notion, GitHub, and Jira
- [Operations](operations.md) — observability (Sentry) and operational guidance
- [Slack](slack.md) — Slack app setup, scopes, and authentication
- [User Guide](user_guide.md) — drafts, import, action steps, case-draft, and agent Jobs usage
