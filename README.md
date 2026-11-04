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
- API keys: requests from this machine are trusted. Every other `/api` request needs `Authorization: Bearer rm_live_…`, except `GET /api/health`. Only a SHA-256 hash of each key is stored. Keys have the scope `admin` or `analyze`, are created, listed and revoked from the UI's API Keys page or with `server keys create|list|revoke`, and are limited to `RATE_LIMIT_PER_HOUR` analyses per hour. See `docs/API_CONTRACT.md`.
- Deployment: `deploy/` holds a hardened Docker image and a Traefik compose file. See `deploy/README.md`.
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
| `REGRESSIONMAP_ALLOW_REMOTE` | unset | Only `1` allows a non-loopback `ADDR`. It also requires `DATA_DIR` with at least one active `admin` API key, or the server refuses to start. |
| `ALLOWED_HOSTS` | unset | Comma-separated extra hostnames accepted in the Host and Origin headers, for example `regressionmap.glenox.net`. |
| `RATE_LIMIT_PER_HOUR` | `20` | Analyses per API key per hour. Local requests are not limited. |
| `MODEL_API_KEY` / `MODEL_API_KEY_FILE` | unset | Model key, or a file that contains only the key. It is never logged. |
| `MODEL_ID`, `MODEL_BASE_URL` | `muse-spark-1.3`, `https://api.meta.ai/v1` | |
| `WEB_DIR` | unset | Built UI (`web/dist`, must contain `index.html`), served at `/`. See "Production build". |
| `DATA_DIR` | unset | Persisted analyses (`analyses/*.json`) and API keys (`keys.json`, hashes only). |
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

## CI and notifications

A failing CI run can start an analysis and get the result back in two ways: the CI script waits for the result and writes a summary, or the server calls a webhook when the analysis finishes. Webhooks are the v3 contract: see "Completion notifications (v3)" in `docs/API_CONTRACT.md`.

**1. Get an `analyze` key.** On a server you run, create one with `server keys create --name ci --scope analyze` (it uses `DATA_DIR`; in the Docker image: `/app/regressionmap keys create --name ci --scope analyze`) or on the UI's API Keys page. The secret (`rm_live_…`) is shown once. Store it as a CI secret, for example `REGRESSIONMAP_KEY`. Do not use an `admin` key in CI. Each key may start `RATE_LIMIT_PER_HOUR` analyses per hour. For the hosted demo, the operator gives out the keys.

**2. Call the script.** `scripts/ci-analyze.sh` (POSIX sh, needs `curl` and `node`) and `scripts/ci-analyze.ps1` (Windows PowerShell 5.1) start `POST /api/analyses`, poll every 3 s, and print a markdown summary: status, baseline and HEAD PASS/FAIL from the exit codes, the ranked hypotheses with `file:line`, confidence and cited evidence IDs under the label "AI-generated hypotheses — not verified", and the number of rejected hypotheses. With `GITHUB_STEP_SUMMARY` set, the summary is also appended there.

```sh
export REGRESSIONMAP_URL=https://regressionmap.example.com   # http:// only for 127.0.0.1/localhost
export REGRESSIONMAP_KEY=...                                  # from a CI secret; never on the command line
REPO=my-repo BASELINE=<good-sha> sh scripts/ci-analyze.sh
# optional: COMMAND_ID (default node-test), NOTIFY_URL, NOTIFY_FORMAT, TIMEOUT_SECONDS (600), POLL_SECONDS (3)
```

The key is read from the environment only. The shell script writes it to a private header file (created with `umask 077` in a `mktemp` directory, deleted on exit) for curl, so it is never in a process command line and never printed. Exit codes: `0` analysis done (the tests can still fail: read the summary), `1` usage error, `2` analysis error, `3` timeout, `4` HTTP or auth error (prints the API error code and message; on `429 rate_limited` also the `Retry-After` value). `docs/ci/github-actions-example.yml` is a workflow for your own repository: it runs the tests and, when they fail, downloads the script at a pinned commit and runs it.

**3. Webhooks.** Add `notify_url` (and optionally `notify_format`: `json`, the default, or `slack`) to `POST /api/analyses`, or set `NOTIFY_URL` / `NOTIFY_FORMAT` for the script. The server must have `WEBHOOK_SECRET` set, otherwise it answers `400 webhooks_disabled`. When the analysis reaches `done` or `error`, the server sends one `POST` (`User-Agent: RegressionMap-Webhook/1`, 5 s timeout per attempt, up to 3 attempts after 1 s and 4 s, retried only on network errors, 5xx and 429). `json` bodies carry `event`, `id`, `repo`, `status`, the baseline and HEAD SHAs and PASS flags, `model_status` and `top_hypothesis` (or `null`). They are signed: `X-RegressionMap-Event: analysis.completed` and `X-RegressionMap-Signature: sha256=<hex HMAC-SHA256 of the exact body with WEBHOOK_SECRET>`. `slack` bodies are `{"text": "…"}`, a one-paragraph summary with no evidence and no source code, ending with "AI-generated hypothesis, not verified." Slack bodies are not signed; the token in the Slack URL protects them. `GET /api/analyses/{id}` shows the delivery under `notification` (host only, status, attempts). Treat a notify URL as a secret: the server never logs, returns or stores it, and the scripts never print it. Verify a `json` webhook on the raw body, before parsing it:

```js
const crypto = require('node:crypto');
function verifySignature(rawBody, header, secret) { // rawBody: Buffer with the exact request body
  const want = Buffer.from('sha256=' + crypto.createHmac('sha256', secret).update(rawBody).digest('hex'));
  const got = Buffer.from(String(header || ''));
  return got.length === want.length && crypto.timingSafeEqual(got, want);
}
```

**SSRF rules.** A `notify_url` must be `https://`, at most 2048 characters, have no user info, and use port 443 or none (`400 invalid_notify_url` otherwise). At delivery time the server resolves the host and refuses to send the request if any resolved address is not public: loopback, private, link-local, CGNAT, multicast, unspecified or cloud-metadata addresses, for IPv4 and IPv6. It connects only to an address that passed this check, so a DNS rebind between the check and the connection cannot redirect it, and it does not follow redirects.

**Hosted vs self-hosted.** The server analyses its own git clone under `WORKSPACE_ROOT`, at that clone's HEAD. It does not receive code from CI. The hosted demo contains only `pagination-demo`, so for any other repository it answers `400 invalid_repo`. To analyse your own repository, run a server yourself with a clone of it in `WORKSPACE_ROOT`, keep that clone at the commit that failed, and give the baseline as a commit that exists in the clone (otherwise `400 invalid_baseline`). Hosted bring-your-own-repo is a plan, not a feature: see `docs/ROADMAP-billing.md`.

## Safety design

- No shell: git and test processes are started from argument arrays. Revisions must be `HEAD` or hex and are resolved to a full SHA before use.
- Repositories must be direct children of `WORKSPACE_ROOT`.
- Model output is data. Its `verification_suggestion` is shown as text and never executed.
- The API key is read on the backend only, never returned by `/api/health`, and scrubbed from error messages.

## Limitations

Hypotheses are not proof of cause. The confidence label is a qualitative estimate from the model, not a calibrated probability. Evidence parsing targets node:test output; other runners produce a failure summary without parsed frames.
