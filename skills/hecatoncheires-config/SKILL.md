---
name: hecatoncheires-config
description: Author a Hecatoncheires workspace configuration TOML (config.toml). Use whenever the user wants to set up a new workspace, add or change custom fields, define case/action status workflows, wire Slack (channel or thread mode), configure the memo or assist features, or add event-driven/scheduled Jobs — for any project, task, incident, risk, or case-management use case, even when they don't name "config.toml" explicitly.
---

# Author a Hecatoncheires workspace config

Produce a valid workspace configuration `.toml` for Hecatoncheires. One file
defines one `[workspace]` plus its optional sections (`[labels]`, `[[fields]]`,
`[slack]`, `[case]`, `[action]`, `[memo]`, `[assist]`, `[[job]]`, …). The server
loads it at startup via `--config` / `HECATONCHEIRES_CONFIG`.

This skill describes only the **principles** of authoring a config. It
deliberately does **not** restate the concrete schema — the key names, field
types, id patterns, status colors, validation rules, and template variables all
evolve, and a copy here would silently drift out of sync with the running
binary. Always read the authoritative, always-current source before you write
keys.

**Where these docs live.** This skill is published through a plugin marketplace
and is usually installed into *some other* repository, where no local `docs/`
exists — so the doc paths below are not relative paths to read blindly. Resolve
each one this way:

- When you are working inside the `m-mizutani/hecatoncheires` repository itself,
  read the local copy under `docs/` — it matches your working tree (and may be
  ahead of `main`).
- Otherwise, fetch the canonical published version from GitHub (default branch
  `main`) with your web-fetch tool. If you cannot reach it (offline, fetch
  blocked), say so and ask the user for the current reference rather than
  inventing keys from memory.

The sources below are listed **in the order you need them**, each named by its
`docs/<file>` path and canonical URL. Read the one that answers your current
question — don't guess which file holds a fact:

| You need to know… | Read |
|-------------------|------|
| Any TOML section / field / id rule / validation | `docs/configuration.md` (read first) |
| **The exact name of an agent tool**, and **whether a Job can call it** | `docs/agent_tools.md` |
| How a scheduled Job fires (per-case, UTC cron, open-cases-only) and the Job guardrails | `docs/configuration.md` § *Job Definitions* (Events and scheduling / Guardrails) |
| Slack channel vs thread mode, scopes, id formats | `docs/slack.md` |
| How the config file is loaded + the `validate` command | `docs/cli.md` |
| Turning Notion / GitHub / Jira on (the *enable* side, not the tool list) | `docs/integrations/` |

- **Configuration reference** — `docs/configuration.md` /
  <https://github.com/m-mizutani/hecatoncheires/blob/main/docs/configuration.md>.
  The complete reference for every section, every field type, every id/pattern
  rule, and the validation list. Also owns the `[[job]]` schema, the scheduled
  execution model, and the Job guardrails. Treat it as ground truth over
  anything you remember. Read this first.
- **Agent tools** — `docs/agent_tools.md` /
  <https://github.com/m-mizutani/hecatoncheires/blob/main/docs/agent_tools.md>.
  **The single source of truth for tool names and which context exposes them.**
  Read it before you put any tool name in a Job or `[assist]` prompt. A Job's
  palette is narrower than the interactive agent's — notably Jobs get **no**
  Slack search, Notion, or GitHub tools — so a plausible-sounding name from
  another context will silently do nothing.
- **Slack** — `docs/slack.md` /
  <https://github.com/m-mizutani/hecatoncheires/blob/main/docs/slack.md>.
  Channel mode vs thread mode, scopes, and the channel/team id formats
  referenced by `[slack]`.
- **CLI** — `docs/cli.md` /
  <https://github.com/m-mizutani/hecatoncheires/blob/main/docs/cli.md>.
  How the file is loaded (`--config`, directory recursion) and the `validate`
  command used in the final step.
- **Integrations** — `docs/integrations/README.md` /
  <https://github.com/m-mizutani/hecatoncheires/blob/main/docs/integrations/README.md>.
  How to *enable* Notion, GitHub, and Jira (tokens, App setup), one page per
  service under `docs/integrations/`. For the tool **names**
  and their availability, use `docs/agent_tools.md` instead.

## How to work

Don't dump a TOML file on the first message. Configs encode someone's actual
workflow, so converge on intent first, then write, then prove it loads.

1. **Understand the use case.** What does one "case" represent in their world
   (a risk, an incident, a ticket, a candidate)? That single answer drives the
   entity label, the field set, and the status workflow. Ask if it isn't clear.

2. **Read the schema.** Open `docs/configuration.md` (and `docs/slack.md` if
   Slack is involved) and confirm the current keys, field types, and id rules
   before proposing anything. Do not trust memory for patterns like field-id
   syntax — they have changed before.

3. **Propose a plan in prose, not TOML.** Summarize the workspace identity, the
   entity label, the Slack mode, and a table of proposed fields (id, name,
   type, required, options). Let the user correct it. Iterating on a readable
   plan is far cheaper than iterating on TOML.

4. **Write the file** once the plan is confirmed, following
   `docs/configuration.md` exactly. Confirm the output path with the user
   (default `./config.toml`).

5. **Validate before declaring done.** Run `hecatoncheires validate
   --config <path>` and fix every error it reports. A config that hasn't been
   loaded by the real validator is not finished — startup validation is strict
   and rejects subtle mistakes (bad id patterns, dangling status references,
   unparseable templates) that look fine to the eye. If you cannot invoke the
   binary (sandbox, no build) but have the source checked out, do **not**
   silently downgrade to eyeballing the file against the rules list — exercise
   the same code path the validator uses, `config.LoadWorkspaceConfigs` in
   `pkg/cli/config`, from a throwaway test or snippet. The loader is the ground
   truth; a manual rule walkthrough is only a last resort, and say so plainly
   when that is all you could do.

To check how a Job or prompt change behaves on past cases, use the
[`hecatoncheires-build-scenario` skill](https://github.com/ubie-oss/hecatoncheires/blob/main/skills/hecatoncheires-build-scenario/SKILL.md)
to compare the current and candidate configurations with the same scenarios
and checks.

## Principles that outlast the schema

These hold regardless of which keys the current docs define:

- **Model the domain, not the tool.** Fields and statuses should mirror how the
  team already talks about the work. A field nobody fills in is worse than no
  field. Prefer a small, required-where-it-matters set over an exhaustive one.

- **Build only what was asked.** Don't add sections, fields, Jobs, or an
  `[assist]` block the user didn't request just because they're available —
  every extra knob is something they have to understand and maintain. If you
  think an addition genuinely helps, suggest it in prose and let them opt in.
  Likewise, don't invent identifiers you can't ground: tool names referenced in
  a Job or assist prompt must be real **and exposed in that context** — confirm
  each against `docs/agent_tools.md` (which lists every tool and which context
  wires it), not from memory or a plausible-sounding guess. A Job naming a tool
  it doesn't have just does nothing.

- **Ids are contracts; names are cosmetics.** An `id` is referenced by
  templates, queries, stored data, and other configs — renaming it later breaks
  those references, so choose it deliberately and let it be boring. A `name`
  (display label) can be reworded freely. When unsure whether a string is an id
  or a label, check `docs/configuration.md` for which key it is.

- **Choices the user must pick = `select`/`multi-select`; free observations =
  `text`.** Reach for an enumerated field only when the set of answers is known
  and stable, because each option id is itself a contract. Put machine-meaning
  (scores, escalation flags) in option `metadata`, and human-meaning in
  `description` — never fold one into the other.

- **Required means "the case is invalid without it."** Marking everything
  required pushes friction onto every case creation and tempts junk values. Set
  a field required only when an empty value is genuinely meaningless.

- **Mode dictates which sections are mandatory.** Thread mode and channel mode
  pull in different required sections (e.g. a monitored channel, a case status
  workflow). Decide the Slack mode early because it changes what the rest of the
  file must contain — and let `docs/configuration.md` / `docs/slack.md` tell you
  exactly which sections each mode requires.

- **Templates are validated at load, not at use.** Welcome messages and Job
  prompts are Go `text/template` strings parsed eagerly at startup, and they
  reference fields by id. A typo'd field reference or unclosed action fails the
  whole load — which is why step 5's `validate` run is non-negotiable.

- **One workspace per file, composed by directory.** Multiple workspaces are
  separate files loaded together; cross-workspace references (e.g. a case
  reference field) require the target workspace id to exist among the loaded
  set. Keep each file self-describing.

## When you change the running schema

If you are editing the Go config loader (`pkg/cli/config/`) and add, rename, or
remove a key, the source of truth is `docs/configuration.md` — update it in the
same change so this skill and every reader keep pointing at accurate keys. This
skill intentionally holds no schema to update; the docs do.
