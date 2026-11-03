# RegressionMap

Evidence-driven regression investigation for developers. Built for the Meta Global AI Developer Hackathon, track "Build for Your World".

A test passed at a known-good commit and fails now. RegressionMap runs the test at both revisions, collects the real failure output, the stack locations, and the changed code, gives each item a stable ID (E1, E2, ...), and asks a Meta model for ranked root-cause hypotheses that must cite those IDs. The developer then approves a verification rerun. The test runner decides pass or fail, not the model.

## What is implemented (verified on 2026-11-04, Windows 11, Go 1.27, Node 24)

- Go API (`backend/`) on `127.0.0.1:8787`, contract in `docs/API_CONTRACT.md`.
- Test runs at the baseline and at HEAD, each in a temporary `git worktree`, with a timeout and an output cap. `MODEL_API_KEY` is removed from the child process environment.
- Evidence: node:test failure blocks (with actual and expected values), in-repo stack frames with the real source lines, diff hunks, and the current source of changed non-test files. Line numbers come from the test runner, the diff header, or the file. Likely secrets are redacted. The bundle has a size cap.
- Meta model call: `POST https://api.meta.ai/v1/chat/completions`, model `muse-spark-1.3`, `response_format` `json_schema` with `strict: true` (per https://dev.meta.ai/docs/protocols/chat-completions and https://dev.meta.ai/docs/structured-output).
- Output validation: unknown fields fail the parse. Each hypothesis is dropped if it cites an unknown evidence ID, names a file not in the evidence, has a path with `..` or an absolute path, has an inverted line range, or has a confidence label other than low/medium/high. Dropped items are shown as "rejected".
- If the key is missing, the model step shows a "disabled" state and the rest of the analysis still works.
- Verification: allowlisted command only (`node --test`), chosen by ID, run in the repository working tree, and only when the request contains `"approved": true`. The UI keeps the Run button disabled until the approval box is ticked.
- React UI (`web/`): repo and baseline selectors, test runs with PASS/FAIL from exit code only, changed files, evidence list, hypotheses labelled "AI-generated hypotheses — not verified", verification panel.
- Demo fixture: `go run ./cmd/mkfixture <dir>` creates `pagination-demo`, a small original TypeScript repo. Commit 1 passes. Commit 2 changes an unrelated formatter. Commit 3 introduces an off-by-one bug in `paginate`. `-fix` writes the known fix into the working tree.

Licence: MIT (see `LICENSE`).

Production hardening (also verified 2026-11-04):

- Every request passes a middleware chain: access log (method, path, status, duration; never headers or bodies), panic recovery, security headers with a strict Content-Security-Policy, and Host/Origin checks that block DNS rebinding and cross-site requests. POST requests must be `application/json`.
- The server refuses to listen on a non-loopback address unless `REGRESSIONMAP_ALLOW_REMOTE=1` is set.
- Model calls retry on HTTP 429, 5xx and network errors with exponential backoff and jitter, honour `Retry-After`, and never retry other 4xx responses.
- One running analysis per repository (409), a global cap on concurrent analyses (429), a bounded in-memory job list, a whole-analysis timeout, and stale worktree cleanup at startup.
- With `DATA_DIR`, analyses are saved as JSON files (atomic writes, mode 0600) and survive restarts. `GET /api/analyses` lists them.
- `WEB_DIR` lets the Go binary serve the built UI, so production is one process. Graceful shutdown on SIGINT/SIGTERM.
- GitHub Actions CI runs the Go and web suites on Ubuntu and Windows.

Not implemented: other test runners than `node --test`, ESLint.

## Run it

One command (creates `demo-workspace/` with the fixture if missing, starts the API with `go run` and Vite):

```sh
scripts/dev.sh                                               # macOS / Linux / Git Bash
powershell -ExecutionPolicy Bypass -File scripts\dev.ps1     # Windows
```

The scripts never read a key file themselves. To enable the model step, set `MODEL_API_KEY` or `MODEL_API_KEY_FILE` in your own environment first. The backend reads it.

Or step by step:

```sh
# 1. demo repository
cd backend
go run ./cmd/mkfixture ../demo-workspace

# 2. API (key stays on the backend; never commit it)
WORKSPACE_ROOT=../demo-workspace MODEL_API_KEY_FILE=/path/to/key.txt go run ./cmd/server

# 3. UI
cd ../web
npm install
npm run dev        # http://localhost:5173, proxies /api to 127.0.0.1:8787
```

Server environment:

| Variable | Default | Notes |
| --- | --- | --- |
| `WORKSPACE_ROOT` | (required) | Must be an existing directory. Repositories are its direct children. |
| `ADDR` | `127.0.0.1:8787` | Must be a loopback address (`127.x`, `::1`, `localhost`). Otherwise the server refuses to start. |
| `REGRESSIONMAP_ALLOW_REMOTE` | unset | Only `1` allows a non-loopback `ADDR`. A warning is logged, because the API has no authentication. |
| `MODEL_API_KEY` / `MODEL_API_KEY_FILE` | unset | Model key, or a file that contains only the key. It is never logged. |
| `MODEL_ID`, `MODEL_BASE_URL` | `muse-spark-1.3`, `https://api.meta.ai/v1` | |
| `WEB_DIR` | unset | Built UI (`web/dist`, must contain `index.html`), served at `/`. See "Production build". |
| `DATA_DIR` | unset | Reserved for persisted state. |
| `LOG_FORMAT` | `text` | `text` or `json` (log/slog on stderr). |

The server stops cleanly on Ctrl+C or SIGTERM: in-flight requests get up to 10 s to finish.

Demo flow: pick `pagination-demo`, pick the first commit as the baseline, start the analysis, read the hypotheses, then run `go run ./cmd/mkfixture -fix ../demo-workspace`, tick the approval box and run the verification. Reset with `git -C demo-workspace/pagination-demo checkout -- src/pagination.ts`.

## Tests

```sh
cd backend && gofmt -l . && go vet ./... && go test -count=1 ./...   # gofmt -l must print nothing
cd web && npx vitest run && npx tsc -b && npm run build
```

The backend tests need `git` and Node 24 on `PATH`, because the fixture tests run `node --test`.

CI (`.github/workflows/ci.yml`) runs the same checks on every push to `main` and on every pull request, on `ubuntu-latest` and `windows-latest`. The gofmt check runs on Linux only. Windows runners check out with CRLF line endings, which gofmt would flag.

## Production build

```sh
scripts/build.sh                                             # or: powershell -ExecutionPolicy Bypass -File scripts\build.ps1
```

This runs `npm ci && npm run build` in `web/` and `go build` into `backend/bin/regressionmap` (`.exe` on Windows). One process then serves the UI and the API on one port:

```sh
WORKSPACE_ROOT=./demo-workspace WEB_DIR=./web/dist backend/bin/regressionmap
# open http://127.0.0.1:8787
```

With `WEB_DIR` set:

- `/api/*` goes to the API.
- Other GET/HEAD requests are served from `WEB_DIR`. A path that is not a file returns `index.html` (SPA fallback).
- Other methods outside `/api` return 405.
- Files are opened through `os.Root`, so `..` and symlinks cannot reach outside `WEB_DIR`.

## Safety design

- No shell: git and test processes are started from argument arrays. Revisions must be `HEAD` or hex and are resolved to a full SHA before use.
- Repositories must be direct children of `WORKSPACE_ROOT`.
- Model output is data. Its `verification_suggestion` is shown as text and never executed.
- The API key is read on the backend only, never returned by `/api/health`, and scrubbed from error messages.

## Limitations

Hypotheses are not proof of cause. The confidence label is a qualitative estimate from the model, not a calibrated probability. Evidence parsing targets node:test output; other runners produce a failure summary without parsed frames.
