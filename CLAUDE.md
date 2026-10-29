# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Hecatoncheires is an AI-native project/case management platform built with Go and React. It provides a customizable GraphQL API with support for AI-powered analysis and automation. The system allows defining custom fields via TOML configuration files, making it adaptable to various use cases including security risk management, task management, and incident tracking.

## Common Development Commands

### Building and Testing
- `task` - Run default task (GraphQL code generation)
- `task build` - Build the complete application (frontend + backend)
- `task build:frontend` - Build frontend only
- `task graphql` - Generate GraphQL code from schema
- `task run` - Build and run the server
- `task dev:frontend` - Run frontend development server
- `go build` - Build the main binary
- `go test ./...` - Run all tests
- `go test ./pkg/path/to/package` - Run tests for specific package
- `task lint:frontend` - Run ESLint on the frontend (or `pnpm lint` inside `frontend/`)

### Code Generation
- `go tool gqlgen generate` - Generate GraphQL resolvers and types from schema
- Mock generation planned for future when more interfaces are defined

## Important Development Guidelines

### Error Handling
- Use `github.com/m-mizutani/goerr/v2` for error handling
- Must wrap errors with `goerr.Wrap` to maintain error context
- Add helpful variables with `goerr.V` for debugging
- **NEVER silently swallow errors** — returning a default/empty value while discarding the error (e.g., `return emptyResult, nil` in an `if err != nil` block) is strictly prohibited. Errors MUST always be propagated to the caller via `goerr.Wrap` or returned directly. This applies to ALL contexts including GraphQL resolvers — do not justify swallowing errors with "graceful degradation" or "it's just auxiliary data". If an operation fails, the caller must know about it
- **NEVER check error messages using `strings.Contains(err.Error(), ...)`**
- **ALWAYS use `errors.Is(err, targetErr)` or `errors.As(err, &target)` for error type checking**
- Error discrimination must be done by error types, not by parsing error messages
- **Non-fatal errors (errors that don't require rollback or propagation) MUST be handled via `errutil.Handle(ctx, err, "description")`** — never use raw `logger.Error` or describe error handling as "log only". `errutil.Handle` is the project's standard mechanism for non-fatal error handling

### Logging
- **Never call `slog.Info()`, `slog.Error()`, `slog.Debug()`, `slog.Warn()` or other global slog logger functions directly.** Always obtain a logger via `logging.From(ctx)` from `pkg/utils/logging/`
- Attribute constructors (`slog.String()`, `slog.Any()`, `slog.Int64()`, etc.) are fine — use them as-is

### Resource Cleanup
- **ALWAYS use `safe.Close(ctx, closer)` from `pkg/utils/safe`** to close `io.Closer` resources
- **NEVER use `_ = x.Close()` or bare `x.Close()`** — use `safe.Close` instead for nil-safe, error-logged cleanup

### Background Goroutines
- Background goroutines launch via `pkg/utils/async.Dispatch` / `RunParallel`, never raw `go func(){...}()`. Those helpers wrap panic recovery, logger context propagation, and error reporting
- Tests that exercise async tails must call `async.Wait()` before asserting on side effects — do not rely on `time.Sleep`

### Implementation Completeness
- **NEVER leave incomplete implementations, TODOs, or placeholder code**
- **NEVER skip implementation because it's complex or lengthy**
- **ALWAYS complete the full implementation in one go**
- If a task seems too complex, break it down into smaller steps, but complete ALL steps
- Long code is acceptable — incomplete code is NOT

### Multi-Instance Safety (Stateless Design)
- **The application is designed to run as multiple concurrent instances** (horizontal scaling). Any design that assumes single-instance will break in production
- **NEVER hold cross-request state in process memory.** State that must survive across separate requests, goroutines that originated elsewhere, or instance boundaries MUST be persisted to a shared backend (Firestore / GCS / Pub/Sub)
- **Allowed in-memory state**: only within a single continuous processing flow (e.g. variables within one HTTP request, one goroutine's local variables, one WebSocket connection's live buffer for the duration of that connection). As soon as the flow ends, the state must be gone or persisted
- **Forbidden patterns**:
  - In-memory registry/map keyed by ID that other requests look up (e.g. `map[SessionID]*Handler` at package level)
  - Singleton caches of business data without a shared backend
  - Cross-goroutine coordination via channels at package scope

### Testing Best Practices
- ALWAYS write tests for ALL code you create. This is NON-NEGOTIABLE.
- Writing code without tests is UNACCEPTABLE.
- Use standard Go testing package
- Use Memory repository from `pkg/repository/memory` for repository testing
- Test both memory and firestore implementations when applicable
- Every function, method, and handler MUST have corresponding tests
- Tests must be written BEFORE declaring the task complete

### Code Visibility
- Do not expose unnecessary methods, variables and types
- Use `export_test.go` to expose items needed only for testing
- **NEVER place default values inside internal/private functions**
  - Default values should be controlled at the caller's level (e.g., CLI flags, configuration)
  - Internal functions should receive all necessary parameters from their callers
  - This ensures configurability and avoids hidden magic values

## Architecture

### Core Structure
The application follows Domain-Driven Design (DDD) with clean architecture:

- `pkg/cli/` - CLI commands and configuration
- `pkg/controller/` - Interface adapters
  - `graphql/` - GraphQL resolvers
  - `http/` - HTTP server and routing
- `pkg/domain/` - Domain layer
  - `interfaces/` - Repository and service interfaces
  - `model/` - Domain models (IoC data structures)
- `pkg/repository/` - Data persistence implementations
  - `firestore/` - Firestore backend
  - `memory/` - In-memory backend (testing/development)
- `pkg/agent/` - The agent runtime, independent of any usecase
  - `kernel/` - Builds the agentkit `Kernel` every host spawns onto: the agent registry, the per-agent tool factory (`ToolDeps`), the `Scope` carried as Process metadata, and the claim middleware (trace archive + run timeline + side-effect guard). `agentkernel.Serve` is the only permitted way to run the worker.
  - `budget/` - `budget.Config` → `agentkit.Limiter`: the per-Process step and token ceilings. See `.claude/rules/architecture.md` § Budget.
  - `react/` - The generic single-loop Strategy (tool calls until the model answers), used for the case-channel agent and the shared task sub-agent.
  - `runtrace/` - Turns LLM / tool call boundaries into the `JobRunLog` / `JobRunEvent` records the run-detail UI reads.
- `pkg/usecase/` - Application use cases orchestrating domain operations
  - `agent/planexec/` - The plan-and-execute Strategy (plan → sub-agents → replan → terminal output). Shared by `agent/threadcase`, `agent/proposal` and `agent/job`. There is no in-process runner beside it any more: every host, the eval harness included, spawns onto the agentkit runtime.
  - `agent/proposal/` - Case-draft (proposal) agent host: spawns the turn and applies its draft / question from the run's completion handler (`Host`).
  - `agent/threadcase/` - Thread-mode case host (creation and mention turns).
  - `agent/casebound/` / `agent/wsagent/` - Case-channel and workspace-channel mention hosts.
  - `agent/job/` - Job execution layer. Every Job runs on the durable runtime (`pkg/usecase/job/durable.go`) — `serve`, `tick` and the eval harness alike. `SingleLoopJobExecutor` remains as the in-process fallback for a deployment with no LLM configured (where there is no runtime to spawn onto), and is wired only in that case (`inProcessExecutors`). The Job agent's tool set is assembled in `buildJobTools()` (`pkg/cli/job_runtime.go`). **When adding a new agent tool, wire it into every host that needs it — including this Job path — not just the mention/assist path; non-Action tools default to both channel- and thread-mode.** See `.claude/rules/architecture.md` § "Agent tool wiring (host coverage)".
  - `eval/` - Offline eval harness (`hecatoncheires eval`): runs scenario files through a workflow driver, judges the produced artifact with an LLM checklist, and dumps diagnostics. See `docs/eval.md`.
- `pkg/utils/` - Shared utilities (logging, etc.)
- `frontend/` - React TypeScript application
- `graphql/` - GraphQL schema definitions

### Key Components

#### GraphQL API
- Schema-first design using gqlgen
- GraphQL playground available at `/graphiql` (configurable)
- Type-safe resolvers in `pkg/controller/graphql/`

#### Frontend
- React with TypeScript
- Vite for development and building
- pnpm for package management (faster and more efficient than npm)
- Apollo Client for GraphQL integration
- Embedded into Go binary via `//go:embed`
- Development mode: Hot reload on port 5173
- Production mode: Served from embedded files

##### pnpm version & lockfile policy
- The pnpm version is pinned in `frontend/package.json` (`packageManager` field). Use Corepack (`corepack enable`) so the local pnpm matches the pin; do NOT install pnpm globally
- All non-interactive entry points (CI, `frontend/scripts/e2e.sh`, the Dockerfile) MUST install with `--frozen-lockfile`. Never invoke a bare `pnpm install` from a script — it silently rewrites `pnpm-lock.yaml` on version/peer drift
- `pnpm-lock.yaml` is updated only by an explicit, manual `pnpm install` inside `frontend/`. If `--frozen-lockfile` fails, investigate the drift (pnpm version mismatch, deliberate `package.json` change) — do not just re-run with `pnpm install` to "fix" it

##### Frontend CSS Styling Guidelines
**NEVER hardcode color values, spacing, or sizes in CSS files.** Always use CSS variables defined in `frontend/src/styles/global.css`.

**Colors - Always use semantic variables:**
- Borders: `var(--border-default)`, `var(--border-light)`, `var(--border-medium)`, `var(--border-hover)`, `var(--border-strong)`
- Backgrounds: `var(--bg-paper)`, `var(--bg-subtle)`, `var(--bg-muted)`, `var(--bg-highlight)`
- Text: `var(--text-heading)`, `var(--text-body)`, `var(--text-muted)`, `var(--text-label)`
- Status: `var(--color-error)`, `var(--color-success)`, `var(--color-warning)`, `var(--color-info)`
- Primary: `var(--color-primary)`, `var(--color-primary-light)`, `var(--color-primary-dark)`

**Spacing - Always use spacing variables:**
- `var(--spacing-xs)` (4px), `var(--spacing-sm)` (8px), `var(--spacing-md-sm)` (12px)
- `var(--spacing-md-lg)` (14px), `var(--spacing-md)` (16px), `var(--spacing-lg)` (24px)
- `var(--spacing-xl)` (32px), `var(--spacing-xxl)` (48px)

**Units - Use rem for responsiveness:**
- Convert pixel values to rem (1rem = 16px)
- Examples: `20px` → `1.25rem`, `10px` → `0.625rem`
- Exception: 1px borders can remain as px

**Bad examples (DO NOT DO THIS):**
```css
border: 1px solid #E5E7EB;  /* Hardcoded color */
padding: 14px 16px;         /* Hardcoded spacing */
right: 20px;                /* Hardcoded size */
```

**Good examples:**
```css
border: 1px solid var(--border-default);
padding: var(--spacing-md-lg) var(--spacing-md);
right: 1.25rem;
```

##### Keyboard & IME Input — MANDATORY
**Any keyboard handler that triggers a destructive action on Enter (save, submit, mode change, navigation) MUST guard against IME composition** using `isImeComposing` from `frontend/src/utils/keyboard.ts`. CJK users press Enter to confirm IME conversions — un-guarded handlers silently corrupt their input. Never write `if (e.key === 'Enter') { ...side effect... }` without the guard. See `.claude/rules/frontend-keyboard-input.md` for full details.

##### Internationalization (i18n) — MANDATORY
**All user-facing text in both frontend and backend MUST use the i18n system. Hardcoding strings is prohibited.**

**Frontend (React/TypeScript):**
- Translation keys are defined in `frontend/src/i18n/keys.ts` as an `as const` object
- Translations: `frontend/src/i18n/en.ts` (English), `frontend/src/i18n/ja.ts` (Japanese)
- Both files use `Record<MsgKey, string>` — missing keys cause compile errors
- Use `useTranslation()` hook in components: `const { t } = useTranslation()`
- Usage: `t('keyName')` or `t('keyName', { param: value })` for interpolation
- When adding new UI text: add the key to `keys.ts`, then add translations to both `en.ts` and `ja.ts`

**Backend (Go / Slack UI):**
- Translation keys are iota constants (`MsgKey`) in `pkg/i18n/i18n.go`
- Translations are Go arrays in `pkg/i18n/messages.go` — `init()` panics on missing entries
- Call `i18n.T(ctx, i18n.MsgKeyName, args...)` directly (package-level function)
- Language is detected from Slack user locale and stored in context via `i18n.ContextWithLang(ctx, lang)`
- Default language is configured via `--default-lang` CLI flag / `HECATONCHEIRES_DEFAULT_LANG` env var
- When adding new Slack messages: add a `MsgKey` constant, add entries to both `messagesEN` and `messagesJA` arrays

#### Storage Backends
- **Firestore**: Production-ready persistent storage
- **Memory**: In-memory storage for testing and development
- Repository pattern allows easy switching between backends
- Interface defined in `pkg/domain/interfaces/`

##### Firestore Index Policy
- **CRITICAL: Firestore index updates are PROHIBITED in principle**
- When implementing new queries or batch operations:
  - Use existing indexes whenever possible
  - For batch operations, prefer parallel individual queries over complex queries requiring new indexes
  - If a feature absolutely requires a new index, consult with the team first
  - Example: Instead of `Where("case_id", "in", caseIDs).OrderBy(...)` (needs index), use parallel `Where("case_id", "==", caseID).OrderBy(...)` calls
- Test queries locally to ensure they work with existing indexes before deployment
- This policy prevents index management overhead and ensures queries remain simple and maintainable

##### Firestore Naming Policy
- **NEVER use underscore-joined (`_`) naming to encode multiple semantics into a single document/collection name**
  - Bad: `risk_case_counter`, `tenant1_cases`, `prefix_collectionName`
  - This pattern is fragile (ambiguous parsing if IDs contain underscores) and hard to maintain
- **Use Firestore subcollections to represent hierarchical relationships**
  - Good: `tenants/{tenantID}/counters/case` instead of `counters/risk_case_counter`
  - Subcollections naturally express parent-child relationships and are the idiomatic Firestore pattern
- The existing `collectionPrefix` mechanism is a legacy pattern; for new features, prefer subcollections

### Application Modes
- `serve` - HTTP server mode with GraphQL API and frontend

### Future Features (Planned)
The following features are planned but not yet implemented:
- Enhanced AI-powered case analysis and assessment
- Advanced search and query capabilities with full-text search
- Dashboard analytics and visualizations
- Export and integration features (CSV, JSON, webhooks)
- Additional source integrations (GitHub, Jira, etc.)
- Real-time collaboration features
- Advanced workflow automation

## Configuration

The application is configured via CLI flags or environment variables:

- `HECATONCHEIRES_ADDR` - HTTP server address (default: `:8080`)
- `HECATONCHEIRES_GRAPHIQL` - Enable GraphiQL playground (default: `true`)
- `HECATONCHEIRES_CLOUD_STORAGE_BUCKET` - Cloud Storage bucket for agent History/Trace persistence (required when Slack is wired). See `docs/develop/architecture.md` § Agent thread session.
- `HECATONCHEIRES_CLOUD_STORAGE_PREFIX` - Optional object key prefix within the Cloud Storage bucket
- `HECATONCHEIRES_DASHBOARD_STALE_THRESHOLD` - Age after which an open Case with no update is flagged as "stalled" on the home dashboard (default: `336h` = 14 days; `0` disables). See `docs/cli.md`.
- `HECATONCHEIRES_HOME_MESSAGE_LLM_*` - Optional dedicated LLM for the home dashboard greeting (`_PROVIDER`, `_MODEL`, `_OPENAI_API_KEY`, `_CLAUDE_API_KEY`, `_GEMINI_PROJECT_ID`, `_GEMINI_LOCATION`). Falls back to the main LLM (`HECATONCHEIRES_LLM_*`) when unset; greeting is disabled if neither is configured. See `docs/cli.md`.
- `HECATONCHEIRES_JOB_MAX_CONCURRENCY` - Maximum number of **scheduled** Agent Job runs executing concurrently across the whole deployment (default: `1`; `0` disables the limit). Enforced via Firestore execution slots (`jobSlots/{index}`), so it holds across instances; set the same value on `serve` and `tick`. A run that finds no free slot is skipped and retried on the next tick. See `docs/operations.md` § Deployment-wide concurrency limit.
- `HECATONCHEIRES_SLACK_TOOL_MAX_TEXT_SIZE` / `HECATONCHEIRES_SLACK_TOOL_MAX_RESULT_SIZE` - Size bounds applied to the Slack read tools' results (defaults: `4096` bytes per message text, `32768` bytes per call; `0` disables a bound). One pair covers both `slack__search_messages` and `slack__get_messages`. Registered on `serve`, `tick`, `assist` and `eval`. See `docs/cli.md` § Slack agent tool limits.
- `HECATONCHEIRES_MCP` - Enable the read-only MCP endpoint at `/mcp` (default: `false`). Requires `HECATONCHEIRES_POLICY`. See `docs/mcp.md`.
- `HECATONCHEIRES_POLICY` - Rego policy file/directory path(s) authorizing MCP requests (`data.auth.mcp`). Required when MCP is enabled.
- `HECATONCHEIRES_MCP_ENV` - Allow-list of environment variable names exposed to the Rego policy as `input.env`.
- Logger configuration (format, level, output destination)
- Sentry (optional) - `HECATONCHEIRES_SENTRY_DSN` enables Sentry error reporting via `errutil.Handle`. Companion vars: `HECATONCHEIRES_SENTRY_ENV`, `HECATONCHEIRES_SENTRY_RELEASE`. See `docs/operations.md` § Observability (Sentry).
- BigQuery export - the `export` subcommand full-refreshes each workspace's data into BigQuery (Storage Write API into a per-run staging table, then `CREATE OR REPLACE TABLE ... AS SELECT` onto the destination; the destination's previous schema is never consulted). No new flags/env vars; the destination and workspace→dataset mapping live in the `[export]` section of the `--global-config` file. See `docs/export.md`.

## Testing

Test files follow Go conventions (`*_test.go`). The codebase includes:
- Unit tests for individual components
- Integration tests with repository implementations
- Repository tests use both memory and firestore backends for verification

## Restrictions and Rules

### Directory

- When you are mentioned about `tmp` directory, you SHOULD NOT see `/tmp`. You need to check `./tmp` directory from root of the repository.

### Exposure policy

In principle, do not trust developers who use this library from outside

- Do not export unnecessary methods, structs, and variables
- Assume that exposed items will be changed. Never expose fields that would be problematic if changed
- Use `export_test.go` for items that need to be exposed for testing purposes

### Documentation
- **When adding new features, changing APIs, or adding new dependencies/scopes, ALWAYS update the relevant documentation** (`docs/` directory)
- This includes but is not limited to: new Slack scopes, new environment variables, new configuration options, new API endpoints, changed behavior
- Documentation updates are part of the implementation, not an afterthought — include them in specs and implementation plans from the start
- If a feature requires external setup (e.g., adding OAuth scopes in Slack App settings), document the required steps

### Eval tool catalog

- When you add or remove an agent tool that an eval scenario can reference (the
  `[tools.*]` keys / tool-usage checks), you MUST update the eval tool catalog
  so the catalog, the `hecatoncheires eval --list-tools` output, the scenario
  validator, and the authoring skill stay in sync.
- The single source of truth is `ToolCatalog()` in `pkg/usecase/eval/runner.go`
  (with the tool-name constants in `pkg/usecase/eval/toolsim`). Update it there;
  `--list-tools` and the scenario validator read from it. Also refresh the tool
  list in `skills/hecatoncheires-build-scenario/SKILL.md` and `docs/eval.md`.
- If the tool's client is concrete (not an interface), note whether it is
  simulatable or live-only in the catalog entry (e.g. `github_search` is
  live-only in v1).

### DB consistency checks (`validate --check-db`)

`hecatoncheires validate --check-db` is what tells an operator whether a
configuration change has left existing data inconsistent. It only covers what
`UseCases.ValidateDB` (`pkg/usecase/validate.go`) explicitly checks, so every
new configurable item silently starts out unchecked.

- **Whenever you add or change a configuration item that persisted data
  refers to, you MUST review `ValidateDB` and either add the corresponding
  check or record why none is needed.** This applies to: a new field type or
  field constraint, a new status set or status attribute, a new TOML section
  whose values are stored on an entity, and a new persisted model that carries
  config-derived values (the way `Case` / `Action` / `Memo` do).
- The same obligation applies in reverse: when you add a **persisted model**
  that stores config-derived values, decide whether `ValidateDB` must walk it,
  and say so in the spec — not after the fact.
- Keep the check catalog in `docs/cli.md` (§ `validate`) in sync with
  `ValidateDB`, including the items deliberately left unchecked and the reason.
  A reader must be able to tell "not detected" from "not implemented".
- Deliberate exclusions currently in force: a **field definition removed from
  config** leaves its stored values orphaned, and that is accepted, not
  reported; missing values for `required` fields are likewise not reported. Do
  not reintroduce either without asking.
- Not every check is config-driven. `archived_case_not_closed` reports an
  archived Case whose lifecycle status is not `CLOSED` — an invariant every
  write path already enforces. It is checked anyway because a Case in that
  state is filtered out of the Cases list, the board, the dashboard and the
  Job scan at once, leaving an operator no way to find it. Apply the same
  test to a new invariant: if breaking it makes an entity invisible
  everywhere, `ValidateDB` is what surfaces it.
- Note the asymmetry: a **status id** removed or renamed IS reported
  (`board_status_invalid` / `action_status_invalid`), because the Case or Action
  keeps pointing at a Kanban column or state that no longer exists. Only field
  definitions fall under the exclusion above.
- `ValidateDB` is read-only. It reports; it never repairs. Repair jobs belong
  under the `diagnosis` subcommand.
- The check has **two entry points** and both must keep working: the CLI
  (`validate --check-db`, against the config the process loaded) and
  `POST /api/validate/db` on `serve` (against workspace TOML supplied in the
  request, via `UseCases.ValidateDBWithConfig`). A new check needs nothing extra
  for the HTTP path — it shares the usecase — but the JSON response shape in
  `pkg/controller/http/validate.go` names each `kind`, so a new `kind` belongs in
  the endpoint's documented catalog (`docs/operations.md` § DB consistency check
  over HTTP) as well as in `docs/cli.md`.
- The HTTP path parses request-supplied documents with an empty
  `config.WorkspaceConfigSource.BaseDir` on purpose: that keeps the parse off the
  filesystem, so a submitted `prompt_file` is never read. Do not "fix" it by
  handing it a base directory — that turns an unauthenticated endpoint into an
  arbitrary file read.

### Check

When making changes, before finishing the task, always:
- **WRITE TESTS FIRST - This is MANDATORY, not optional**
- Run `go vet ./...`, `go fmt ./...` to format the code
- Run `golangci-lint run ./...` to check lint error
- Run `gosec -exclude-generated -quiet ./...` to check security issue
- Run `opa test .goast` and `goast test` to verify the goast policies (many `CLAUDE.md` conventions — no `slog.*`/`fmt.Print*`, `err.Error()` string matching, `firestore:"..."` tags, raw `x.Close()`, external test packages, etc. — are mechanically enforced here; see `.goast/README.md` for the policy catalog)
- Run `zenv go test ./...` to ensure ALL tests pass
- **NEVER run `go build` to verify code.** Use `go vet ./...` instead to check for compile errors
- **MANDATORY whenever any file under `frontend/` changes**:
  - Run `pnpm test` in `frontend/` to execute Vitest unit tests
  - Run `pnpm lint` in `frontend/` to execute ESLint
  - Both MUST pass before declaring the task complete. Do not skip lint
    even for "trivial" changes — the keyboard / IME policy is enforced
    here and silent regressions are exactly what lint is for
- Verify test coverage for your changes - EVERY new function/method MUST be tested

### Language

All comment and character literal in source code must be in English

### Pull Requests

- PR titles and descriptions (body) must be written in English
- Commit messages must be written in English
- **Commit messages must be a single line.** No body paragraphs. State the change in one sentence. Explanation goes in the PR description, not the commit
- Follow Semantic Commit format: `<type>: <subject>` (types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`, `ci`, `style`, `perf`)
- Keep PR titles short (under 70 characters); use the body for details
- **Do NOT include `Co-Authored-By:` trailers in commit messages.** No AI/tool attribution lines. The commit body and PR body should also stay free of "Generated with Claude" style notes
- **A PR that changes what a screen displays has a `## Screenshots` section**, captured with the `capture-screenshots` skill. **Screenshots show dummy data only, whatever the change is: never use data derived from the change or from the instruction to create what a screenshot shows.** See `.claude/rules/pr-screenshots.md`

### Repository Write Discipline (NON-NEGOTIABLE)

Background: a Firestore `caseRepository.Create` was rebuilding the
persisted struct field-by-field and silently dropped `ReporterID`
when that field was later added to `model.Case`. Every Slack-
originated case lost its reporter; the UI showed empty Reporter
cells indistinguishable from "no reporter recorded". The bug
survived because (a) the memory repo round-tripped via a full
struct copy and (b) the Firestore tests skipped without
`FIRESTORE_PROJECT_ID`. See `.claude/rules/architecture.md` →
"Repository write contract" for the full rules.

The shortlist:

- **NO field-by-field copy** inside a repository. The shape of
  `Create` is `Validate → assign storage ID on the caller's
  pointer → Set(ctx, x) → return x`. The shape of `Update` is
  `Validate → existence check → Set(ctx, x) → return x`. Nothing
  is rebuilt.
- **NO mirror "doc" types, NO `firestore:"..."` struct tags, NO
  converter functions** (`toXxxDoc` / `fromXxxDoc`). Persist
  `*model.X` directly via `Set(ctx, x)` and `DataTo(&x)`.
- **`time.Now()` does not belong in repository writes.** The
  caller (usecase) owns CreatedAt / UpdatedAt. Repos that stamp
  timestamps force every caller through one clock and override
  whatever the caller passed in.
- **Every persisted model needs `Validate()`** enforcing required
  identity fields (ReporterID, CreatorID, etc.). Repositories
  MUST call it before every write so a handler bug that forgot to
  inject the reporter fails loudly at the first write. (Scoped
  exception: `Case.ValidateNew` requires `ReporterID` only for
  channel-mode Cases; thread-mode Cases created from a bot intake
  post that names no human may have an empty reporter. Keep such
  relaxations narrowly scoped and documented at the check.)

### Repository Tests

The Firestore implementation MUST be exercised — not skipped —
because the memory repo's full struct copy hides bugs that only
appear on the Firestore Create path. Run the Firestore tests
against the emulator in CI and locally.

- Test files should have `package {name}_test`. Do not use same package name
- Test file name convention is: `xyz.go` → `xyz_test.go`. Other test file names (e.g., `xyz_e2e_test.go`) are not allowed.
- Repository Tests Location:
  - NEVER create test files in `pkg/repository/firestore/` or `pkg/repository/memory/` subdirectories
  - ALL repository tests MUST be placed directly in `pkg/repository/*_test.go`
  - Use `runRepositoryTest()` helper to test against both memory and firestore implementations
- Repository Tests Best Practices:
  - Always use random IDs (e.g., using `time.Now().UnixNano()`) to avoid test conflicts
  - Never use hardcoded IDs like "msg-001", "user-001" as they cause test failures when running in parallel
  - **Round-trip every persisted field exhaustively.** A Create test that asserts only `Title` and `ID` cannot catch a repo that drops `ReporterID`; the round-trip must read every field back through `Get` and assert each one.
  - Compare expected values properly - don't just check if something exists, verify it matches what was saved
  - For timestamp comparisons, use tolerance (e.g., `< time.Second`) to account for storage precision
- Test Skip Policy:
  - **NEVER use `t.Skip()` for anything other than missing environment variables**
  - If a test requires infrastructure (like Firestore index), fix the infrastructure, don't skip the test
  - If a feature is not implemented, write the code, don't skip the test
  - The only acceptable skip pattern: checking for missing environment variables at the beginning of a test

### Test File Checklist (Use this EVERY time)
Before creating or modifying tests:
1. ✓ Is there a corresponding source file for this test file?
2. ✓ Does the test file name match exactly? (`xyz.go` → `xyz_test.go`)
3. ✓ Are all tests for a source file in ONE test file?
4. ✓ No standalone feature/e2e/integration test files?
5. ✓ For repository tests: placed in `pkg/repository/*_test.go`, NOT in firestore/ or memory/ subdirectories?
