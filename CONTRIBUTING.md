# Contributing to Hecatoncheires

This guide covers setting up a development environment, building, testing, and
the checks a change must pass before it is opened as a pull request. For how
the code is organized, start with [docs/develop/](docs/develop/README.md) and
[Architecture](docs/develop/architecture.md).

## Development environment

- Go 1.26.9+ (the version in the `go` directive of `go.mod`)
- Node.js 22.22+ (react-router v8 requires it)
- pnpm through Corepack
- Docker, for the Firestore emulator used by the test suite

### pnpm via Corepack

The pnpm version is pinned in `frontend/package.json` (`packageManager` field).
Enable Corepack once on your machine and it installs the pinned pnpm
automatically:

```bash
corepack enable
```

Do NOT install pnpm globally with `npm install -g pnpm` — that bypasses the pin
and is the most common cause of the lockfile being unexpectedly rewritten when
you run e2e or build commands.

If you intentionally want to update dependencies, run `pnpm install` inside
`frontend/` on its own and commit the resulting `pnpm-lock.yaml` change.
Scripted installs (CI, the e2e runner, the Dockerfile) use `--frozen-lockfile`
and fail fast if the lockfile is out of sync rather than silently rewriting it.

## Building

| What | Command |
|---|---|
| Generate GraphQL code from the schema in `graphql/` | `task graphql` |
| Build the frontend into `frontend/dist` (embedded into the Go binary) | `pnpm install --frozen-lockfile && pnpm run build` in `frontend/` |
| Run the frontend dev server with hot reload | `pnpm run dev` in `frontend/` |
| Build the container image | `docker build -t hecatoncheires .` |

To run the server locally, follow [Getting Started](docs/getting_started.md).

## Testing

### Go tests

```bash
task test:firestore
```

This starts a Firestore emulator in Docker, runs `go test ./...` against it, and
removes the container afterwards. A bare `go test ./...` fails unless an emulator
is listening on `127.0.0.1:28615`, because the repository tests always exercise
the Firestore implementation. See
[Getting Started → Repository tests and the Firestore emulator](docs/getting_started.md#repository-tests-and-the-firestore-emulator)
for the details and the Apple Silicon note.

### Frontend tests

Inside `frontend/`:

```bash
pnpm test   # Vitest unit tests
pnpm lint   # ESLint (also enforces the keyboard / IME input rules)
```

### End-to-end tests

The Playwright suite runs against a memory-backed server in Chromium. Install
the browser once before the first run:

```bash
cd frontend
pnpm install --frozen-lockfile
pnpm exec playwright install chromium
cd ..
```

Then run the suite from the repository root:

```bash
task test:e2e
```

This builds the backend, starts it on a free local port, runs the tests, and
stops the server (`frontend/scripts/e2e.sh`). It
installs dependencies with `--frozen-lockfile` and refuses to rewrite
`pnpm-lock.yaml`. Other modes are pnpm scripts inside `frontend/`:

```bash
pnpm run test:e2e:ui       # interactive UI mode
pnpm run test:e2e:headed   # show the browser
pnpm run test:e2e:debug    # debug mode
pnpm run test:e2e:report   # open the last report
```

To run the suite against a server you started yourself:

1. Start the backend in memory mode with the same workspace configs the suite
   expects (`frontend/scripts/e2e.sh` loads these three):
   ```bash
   go run . serve \
     --repository-backend=memory \
     --config=frontend/e2e/fixtures/config.test.toml \
     --config=frontend/e2e/fixtures/config.review.test.toml \
     --config=frontend/e2e/fixtures/extra-workspaces \
     --no-auth=U000000000 \
     --addr=:8080
   ```
2. In another terminal:
   ```bash
   cd frontend
   BASE_URL=http://localhost:8080 pnpm run test:e2e
   ```

E2E tests also run in GitHub Actions on every push that changes `frontend/`,
`pkg/`, or `graphql/` (`.github/workflows/e2e.yml`); test results and
screenshots are uploaded as artifacts when they fail.

## Checks before opening a pull request

Run all of these and make sure they pass:

```bash
go fmt ./...
go vet ./...
golangci-lint run ./...
gosec -exclude-generated -quiet ./...
opa test .goast
goast test
task test:firestore
```

If you changed anything under `frontend/`, also run `pnpm test` and `pnpm lint`
in `frontend/` and `task test:e2e` from the repository root; all three must
pass. The goast policies in `.goast/` enforce many of the project's Go
conventions mechanically; see [.goast/README.md](.goast/README.md) for the policy
catalog.

## Commits and pull requests

- Write commit messages, pull-request titles, and pull-request descriptions in
  English.
- Use a one-line [Semantic Commit](https://www.conventionalcommits.org/) message:
  `<type>: <subject>`, where `type` is one of `feat`, `fix`, `refactor`, `test`,
  `docs`, `chore`, `ci`, `style`, `perf`.
- Keep pull-request titles under 70 characters and put the explanation in the
  description.
- Update the documentation under `docs/` together with any change to features,
  configuration, flags, environment variables, or required permissions.
- When a pull request changes what a screen displays, add a `## Screenshots`
  section with one image per changed state, labelled with the screen, state,
  language, theme, and viewport width. Capture them against the server
  `frontend/scripts/e2e.sh` starts, filled with placeholder data written for the
  capture. Never show data from a real environment, from the issue or report
  that motivated the change, or from the change's own tests. See
  [.claude/rules/pr-screenshots.md](.claude/rules/pr-screenshots.md) and
  [.claude/skills/capture-screenshots/SKILL.md](.claude/skills/capture-screenshots/SKILL.md).
