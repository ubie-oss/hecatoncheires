# Screenshots in Pull Requests

This rule applies to every change, not only to files under `frontend/`: a
backend change that alters what a screen shows also changes the screen.

## When a pull request needs screenshots

When a pull request changes what a screen displays — a component, page, style,
i18n string, or a backend change that alters the data or state a screen shows —
its body has a `## Screenshots` section with the captures used to check the
change in a browser:

- One image per changed state (for example: empty list, list with rows, form
  with a validation error). Show the state the change is about, not only the
  page's initial view.
- The line above each image names the screen, the state, the UI language
  (`en` / `ja`), the theme, and the viewport width.
- When the change alters text, capture both `en` and `ja`, because a key that
  exists in both files can still overflow or read wrongly in one of them.
- When a later commit changes a captured screen, capture it again and replace
  the affected images in the body, so the body always shows the head commit. Do
  not leave outdated images in place or append new ones below them.
- Attach images to the pull request body. Do not post them as comments and do
  not commit them to the repository.

Use the [capture-screenshots skill](../skills/capture-screenshots/SKILL.md) for
the procedure. A screenshot does not replace the E2E test the change needs
(`.claude/rules/frontend-testing.md`).

## Dummy data only (ABSOLUTE)

This repository is public, and every image attached to a pull request is
published with it. **Every screenshot shows dummy data only, whatever the
change is** — a bug fix, a new feature, a copy change, a refactor. There is no
exception for "the value looks harmless" or "it is only a test environment".

### Where the data may come from

- The server is the one `frontend/scripts/e2e.sh` starts: the memory
  repository, the workspace configs in `frontend/e2e/fixtures/`, and
  `--no-auth=U000000000`. It starts empty and its data is discarded when the
  script exits.
- Every value the capture enters (case titles, descriptions, field values,
  action titles, comments, memo text, knowledge, user names) is a generic
  placeholder written for the capture itself: `Sample case 1`,
  `Example action`, `alice` / `bob`, `example.com`, synthetic IDs.
- Data is created through the UI or the GraphQL API of that server, as the E2E
  tests do. When a state cannot be reached that way (a Slack-backed member
  list, an access-denied case), rewrite the response with `page.route`, as
  `frontend/e2e/tests/private-case.spec.ts` does, and fill it with
  placeholders. State in the pull request body which response was rewritten.

### What must never be used to create or display capture data

- **Data derived from the change itself**: values that appear in the diff or
  motivated it — strings in new or changed tests, the reproduction data of the
  bug being fixed, example values in the new code, comments, or documentation.
- **Data derived from the instruction**: anything the request supplied or
  pointed to — titles, names, user or channel IDs, message text, field values,
  URLs, log lines, error messages, issue or ticket text, and any screenshot or
  file the user shared. This holds even when the value is paraphrased,
  shortened, or translated.
- Any server other than the one above: `task run`, a dev server with a real
  workspace config, Firestore (including the emulator holding someone's data),
  a deployed environment, or a real Slack workspace.

The placeholder must not hint at the real case either: a fix for a bug seen on
a case about a specific incident is captured with `Sample case 1`, not with a
renamed version of that incident.

If the state the change is about cannot be shown with placeholders, do not
fall back to real data. State in the pull request body which state was not
captured and why.

### Before attaching

Open every image and check that it contains nothing but placeholders, fixture
workspace names, and UI text. Discard an image that shows anything else and
capture it again.
