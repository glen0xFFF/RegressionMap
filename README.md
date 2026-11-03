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

Not implemented: other test runners than `node --test`, screenshots, ESLint, a production build served by the Go server.

## Run it

```sh
# 1. demo repository
cd backend
go run ./cmd/mkfixture ../demo-workspace

# 2. API (key stays on the backend; never commit it)
WORKSPACE_ROOT=../demo-workspace MODEL_API_KEY_FILE=/path/to/key.txt go run ./cmd/server
#   or MODEL_API_KEY=... ; optional MODEL_ID, MODEL_BASE_URL, ADDR

# 3. UI
cd ../web
npm install
npm run dev        # http://localhost:5173, proxies /api to 127.0.0.1:8787
```

Demo flow: pick `pagination-demo`, pick the first commit as the baseline, start the analysis, read the hypotheses, then run `go run ./cmd/mkfixture -fix ../demo-workspace`, tick the approval box and run the verification. Reset with `git -C demo-workspace/pagination-demo checkout -- src/pagination.ts`.

## Tests

```sh
cd backend && go vet ./... && go test ./...
cd web && npx vitest run && npx tsc -b && npm run build
```

## Safety design

- No shell: git and test processes are started from argument arrays. Revisions must be `HEAD` or hex and are resolved to a full SHA before use.
- Repositories must be direct children of `WORKSPACE_ROOT`.
- Model output is data. Its `verification_suggestion` is shown as text and never executed.
- The API key is read on the backend only, never returned by `/api/health`, and scrubbed from error messages.

## Limitations

Hypotheses are not proof of cause. The confidence label is a qualitative estimate from the model, not a calibrated probability. Evidence parsing targets node:test output; other runners produce a failure summary without parsed frames.
