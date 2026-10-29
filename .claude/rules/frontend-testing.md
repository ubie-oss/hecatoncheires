---
paths:
  - "frontend/**/*"
---

# Frontend Testing Requirements

## Vitest Unit Tests

**CRITICAL: When modifying or adding frontend logic (utility functions, hooks, parsers, etc.), you MUST write corresponding Vitest unit tests.**

### Requirements

- **New utility functions**: Create `*.test.ts` file alongside the source file (e.g., `notion.ts` → `notion.test.ts`)
- **Modified logic**: Update affected unit tests to reflect the changes
- **Test framework**: Use Vitest with `import { describe, it, expect } from 'vitest'`

### Test Execution

**MANDATORY: After any frontend code change, you MUST run Vitest unit
tests, ESLint, AND E2E tests.**

- Run `pnpm test` in `frontend/` to execute Vitest unit tests
- Run `pnpm lint` in `frontend/` to execute ESLint
- Run `task test:e2e` to execute Playwright E2E tests
- All three MUST pass before considering the task complete

The lint step is non-negotiable: it enforces the keyboard / IME handling
policy (see `.claude/rules/frontend-keyboard-input.md`). Skipping it
silently re-introduces the IME bug class this rule was created to
prevent.

### Test File Locations (Unit Tests)

- Unit test files live alongside their source files: `src/utils/notion.test.ts`, `src/hooks/useXxx.test.ts`, etc.
- Configuration: `vite.config.ts` (test section)

## Playwright E2E Tests

**CRITICAL: When modifying or adding frontend components/pages, you MUST also update or add corresponding Playwright E2E tests.**

### Requirements

- **New components/pages**: Create corresponding Page Object classes in `frontend/e2e/pages/` and test cases in `frontend/e2e/tests/`
- **Component modifications**: Update affected test cases to reflect the changes
- **UI changes**: Update selectors in Page Object classes to match new DOM structure, text, or attributes
- **Form/interaction changes**: Update test cases to verify the new behavior
- **Modal/dialog/toast additions**: Include accessibility attributes (`role`, `aria-*`) and update tests

### Test File Locations (E2E)

- **Page Objects**: `frontend/e2e/pages/`
- **Test Cases**: `frontend/e2e/tests/`
- **Fixtures**: `frontend/e2e/fixtures/`

### E2E Quality Bar — reproduce the real use case (NON-NEGOTIABLE)

An E2E test MUST exercise the feature's **actual user journey end-to-end and
assert observable outcomes**. A spec that only navigates to a page and asserts
it rendered (`isPageLoaded()` / "title is visible" and nothing else) is **not an
acceptable E2E test** — it is a smoke check that passes even when the feature is
completely broken. Do not write page-load-only specs and call the work done.

The e2e harness (`frontend/scripts/e2e.sh`) boots a **real backend** with an
in-memory repository and a seeded `test` workspace. So data-mutating flows
(create / edit / delete) genuinely work and **must be driven through the UI**.
"Creating data needs the backend / external services, so I only check the page
loads" is NOT a valid excuse — it is only valid for flows that truly require an
external third party (Slack/Notion/GitHub API), and even then you must state
which external dependency forced the limitation in a comment.

For every feature you add or change, the E2E spec must cover, at minimum, the
**primary journey** and its observable result. For CRUD-style features that means
walking the full loop through the UI:

1. **Create** via the form/dialog, then assert it appears in the list/detail with
   the values you entered (not just that the form submitted).
2. **Read / filter / search** — assert the right rows appear and the wrong ones
   are excluded.
3. **Update** — change a field, persist, **reload**, and assert it round-tripped
   to the backend.
4. **Delete** — remove it and assert it is gone from the list.

Also cover the feature-specific behavior that motivated the change (e.g. a new
control's effect, a guard/validation, a state transition) — assert the behavior,
not merely that the control is present.

- **Model spec**: `frontend/e2e/tests/case.spec.ts` — full create → list →
  detail → edit → close/reopen → paginate/filter journeys with outcome
  assertions. New specs should match this depth.
- **Anti-pattern**: a spec whose every assertion is `isPageLoaded()` (the old
  `source.spec.ts` shape). If you find yourself writing only that, you have not
  finished the E2E.
- Add `data-testid` to the interactive controls your journey drives so Page
  Object selectors stay stable (e.g. the tag editor's suggestion / create
  options), rather than matching on volatile text/structure.

### Checking the screen in a browser

Passing E2E tests do not show how the changed screen looks. Before completing
a change to what a screen displays, capture the changed states against the
server `frontend/scripts/e2e.sh` starts and look at them, following the
`capture-screenshots` skill. The same images go into the PR body
(`.claude/rules/pr-screenshots.md`), and they show dummy data only.

### Best Practices

- Use Page Object Pattern to keep tests maintainable
- Avoid `waitForTimeout()` - rely on Playwright's auto-waiting
- Use `.first()` on locators to avoid strict mode violations when multiple matches exist
- Add accessibility attributes to interactive elements for better test reliability
- Use `main h1` instead of just `h1` to target content area specifically
