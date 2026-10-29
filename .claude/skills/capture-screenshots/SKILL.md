---
name: capture-screenshots
description: Check a screen change in a browser against a locally built hecatoncheires server filled with dummy data, capture screenshots of the changed states, and attach them to the pull request body. Use when a pull request changes what a screen displays, as .claude/rules/pr-screenshots.md requires, or when asked to check the UI, take screenshots, or confirm a change on the real screen.
---

# Capturing screenshots for a pull request

[pr-screenshots.md](../../rules/pr-screenshots.md) decides when a pull request
needs screenshots and which data they may show. This skill is the procedure:
drive the changed screen against the server built from the branch, capture each
changed state with dummy data, check the images, and put them in the pull
request body.

**Read the "Dummy data only" section of the rule before writing the capture
script.** Every value the script enters is a placeholder written for the
capture. Never copy a value from the diff, from the tests the change adds, from
the bug being fixed, or from anything the user's request supplied.

## Where it runs

Capture through `frontend/scripts/e2e.sh`, not `task run` or `pnpm dev`. The
script builds the frontend and the backend from the working tree, starts the
server with the memory repository, the workspace configs in
`frontend/e2e/fixtures/`, and `--no-auth=U000000000` on a free port, and hands
its arguments to `playwright test`. That gives a capture three properties a dev
server does not:

- The server holds no data until the capture creates it, so nothing outside the
  capture can appear on the screen.
- The workspaces are the fixture ones (`test`, `review`, the `extra-workspaces`
  fillers), whose names and fields are already dummy values.
- The data is discarded when the script exits, so it needs no cleanup.

A capture is not a test. Keep the scripts out of `frontend/e2e/tests/`: put them
under `frontend/pr-screenshots/`, which is gitignored. The behaviour the images
show still needs its E2E test.

## Before starting

- Install Chromium once if it is missing: `pnpm exec playwright install chromium`
  in `frontend/` (CONTRIBUTING.md § End-to-end tests).
- `gh` must support `--attach` on `gh pr create` / `gh pr edit`; check with
  `gh pr edit --help`.

## 1. Write the Playwright configuration

`frontend/pr-screenshots/capture.config.ts`:

```ts
import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  testMatch: '*.capture.ts',
  outputDir: './out',
  workers: 1,
  timeout: 120_000,
  reporter: [['list']],
  use: {
    ...devices['Desktop Chrome'],
    baseURL: process.env.BASE_URL,
    viewport: { width: 1280, height: 800 },
    locale: 'en-US',
    actionTimeout: 10_000,
  },
});
```

Use the viewport width the change is about and state it in the pull request
body.

## 2. Write the capture script

`frontend/pr-screenshots/<name>.capture.ts`. Reuse the Page Objects in
`frontend/e2e/pages/` and the constants in `frontend/e2e/fixtures/testData.ts`,
so the capture drives the screen the same way the E2E tests do:

```ts
import { test, expect } from '@playwright/test';
import { CaseListPage } from '../e2e/pages/CaseListPage';
import { CaseFormPage } from '../e2e/pages/CaseFormPage';
import { TEST_WORKSPACE_ID } from '../e2e/fixtures/testData';

// Placeholders written for this capture. Never derived from the change or the request.
const SAMPLE_CASES = [
  { title: 'Sample case 1', description: 'Placeholder description.', category: 'bug' },
  { title: 'Sample case 2', description: 'Placeholder description.', category: 'task' },
];

for (const lang of ['en', 'ja'] as const) {
  test(`case list with rows (${lang})`, async ({ page }) => {
    // The UI language is read from localStorage (frontend/src/i18n/index.tsx).
    await page.addInitScript((l) => localStorage.setItem('hecatoncheires-lang', l), lang);

    const list = new CaseListPage(page);
    const form = new CaseFormPage(page);
    await list.navigate(TEST_WORKSPACE_ID);
    await list.waitForTableLoad();

    for (const c of SAMPLE_CASES) {
      await list.clickNewCaseButton();
      await form.createCase({
        title: c.title,
        description: c.description,
        customFields: { category: c.category },
      });
    }

    // Assert the state the image is meant to show before capturing it.
    await list.navigate(TEST_WORKSPACE_ID);
    await expect(page.getByText('Sample case 2')).toBeVisible();
    // Relative to frontend/, where Playwright runs.
    await page.screenshot({ path: `pr-screenshots/case-list-rows-${lang}.png`, fullPage: true });
  });
}
```

- **Data.** Create it through the UI or the GraphQL API of the harness server,
  with placeholders only. Every capture starts from an empty server, so create
  everything the state needs in the same script. Do not add seed data to
  `frontend/e2e/fixtures/` for a capture.
- **States the harness cannot reach.** A state that depends on Slack, an LLM, or
  another external service cannot be produced by the harness. Rewrite the
  GraphQL response with `page.route` as `frontend/e2e/tests/private-case.spec.ts`
  does, fill the rewritten fields with placeholders, and state in the pull
  request body which operation was rewritten.
- **Elements.** Use `getByTestId` first, then `getByRole` or `getByLabel`. When an
  element cannot be found reliably, add a `data-testid` in the component and use
  it from the Page Object.
- **Results.** Assert each state with `expect` before `page.screenshot`, so a
  passing run shows the screen reached that state and the image is not the only
  evidence of it. Wait with `expect`, never with `waitForTimeout`.
- **Theme.** The default theme is light. When the change touches colours or the
  design tokens in `frontend/src/styles/global.css`, also capture the dark theme
  and label it.

## 3. Capture

From the repository root:

```sh
./frontend/scripts/e2e.sh --config=pr-screenshots/capture.config.ts
```

The config path is relative to `frontend/`, where the script runs Playwright.
The images land in `frontend/pr-screenshots/`.

## 4. Check the images

Read every PNG with the Read tool and confirm:

- It shows the state the pull request body will describe, not a loading
  spinner, an error, or only the page's initial view.
- Every value on it is a placeholder from the capture script, a fixture
  workspace or field name, or UI text. Nothing on it comes from the change, the
  request, or a real environment (`.claude/rules/pr-screenshots.md`).

Capture again if either check fails.

## 5. Put them in the pull request body

`--body-file` replaces the whole body. Write the complete body — the
description and `## Screenshots` — to `frontend/pr-screenshots/body.md`, beside
the images. For an existing pull request, start from its current body
(`gh pr view <number> --json body --jq .body`) and change only the
`## Screenshots` section. Refer to each image by the same `./<file>` path that
`--attach` receives; `gh` uploads each file and rewrites the reference to the
uploaded URL.

```markdown
## Screenshots

Case list with two cases, en, light theme, 1280px wide.

![Case list with two cases (en)](./case-list-rows-en.png)

Case list with two cases, ja, light theme, 1280px wide.

![Case list with two cases (ja)](./case-list-rows-ja.png)
```

Run `gh` from `frontend/pr-screenshots/` so the `./<file>` paths resolve:

```sh
gh pr create --title "<title>" --body-file body.md --attach ./case-list-rows-en.png --attach ./case-list-rows-ja.png
# or, for an existing pull request
gh pr edit <number> --body-file body.md --attach ./case-list-rows-en.png --attach ./case-list-rows-ja.png
```

Then read the body back with `gh pr view <number> --json body --jq .body` and
confirm that no `](./` reference remains. `gh` still updates the pull request
when an upload fails but exits non-zero, so a remaining reference is a failed
upload.

When a later commit changes a captured screen, capture it again and replace the
images with `gh pr edit --body-file`, as the rule requires.
