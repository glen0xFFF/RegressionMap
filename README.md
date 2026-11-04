# RegressionMap

Evidence-driven regression investigation for developers. Built for the Meta Global AI Developer Hackathon, track "Build for Your World".

A test passed at a known-good commit and fails now. RegressionMap runs the test at both revisions, collects the real failure output, the stack locations, and the changed code, gives each item a stable ID (E1, E2, ...), and asks a Meta model for ranked root-cause hypotheses that must cite those IDs. The developer then approves a verification rerun. The test runner decides pass or fail, not the model.

## What is implemented (verified on 2026-11-04, Windows 11, Go 1.27, Node 24)

- Go API (`backend/`) on `127.0.0.1:8787`, contract in `docs/API_CONTRACT.md`.
- Test runs at the baseline and at HEAD, each in a temporary `git worktree`, with a timeout and an output cap. Test runs, verification runs and git never receive the server's secrets or any variable whose name ends in `_KEY`, `_SECRET`, `_TOKEN` or `_PASSWORD`. See "Test environment".
- Evidence: node:test failure blocks (with actual and expected values), in-repo stack frames with the real source lines, diff hunks, and the current source of changed non-test files. Line numbers come from the test runner, the diff header, or the file. Likely secrets are redacted. A path segment that looks like a secret becomes `[REDACTED]` in the evidence `file` and `title`, for example `config/token=[REDACTED]/app.ts`. The list of changed files sent to the model is redacted the same way; the API's own `changed_files` keeps the real paths. The bundle has a size cap, and big input is cut, not refused. See "Limits".
- Meta model call: `POST https://api.meta.ai/v1/chat/completions`, model `muse-spark-1.3`, `response_format` `json_schema` with `strict: true` (per https://dev.meta.ai/docs/protocols/chat-completions and https://dev.meta.ai/docs/structured-output).
- Output validation. The system prompt states the limits: at most 5 hypotheses with unique ranks, the three text lengths below, and a summary that is not empty. Rejected hypotheses are shown as "rejected" with their reason, for example `hypothesis 2 ("…"): duplicate rank 1`. The rules, in the order applied:
  1. Unknown fields fail the parse. A missing `summary`, `hypotheses` or `limitations` field makes the result `invalid`, for example `model output did not match the schema: summary is missing`. An empty `summary` is accepted.
  2. A hypothesis is rejected when it cites no supporting evidence, cites an unknown evidence ID (supporting or counter), names a path that is empty, not in clean form, absolute, outside the repository or contains `:`, names a file that is not in the evidence, has an invalid line range (start below 1 or end before start; `0`–`0` means unknown and is allowed), has a range more than 3 lines away from every evidence span of that file (checked when the evidence has line numbers for the file), has a confidence label other than low/medium/high, has a rank below 1, or repeats the rank of an earlier hypothesis (the first one is kept).
  3. Only the 5 best-ranked hypotheses that remain are kept, in rank order. The others are rejected with `more than 5 hypotheses`.
  4. A `title` over 200 characters, a `reason` over 2000 or a `verification_suggestion` over 500 is cut to exactly its limit and ends with `…[truncated]`. The hypothesis is kept. The v3 build rejected it instead.
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
- API keys: requests from this machine are trusted. Every other `/api` request needs `Authorization: Bearer rm_live_…`, except `GET /api/health`. Only a SHA-256 hash of each key is stored. Keys have the scope `admin` or `analyze`, are created, listed and revoked from the UI's API Keys page or with `server keys create|list|revoke`, and are limited to `RATE_LIMIT_PER_HOUR` `POST /api/analyses` requests per hour, rejected requests included. See `docs/API_CONTRACT.md`.
- Deployment: `deploy/` holds a hardened Docker image and a Traefik compose file. See `deploy/README.md`.
- With `DATA_DIR`, analyses are saved as JSON files (atomic writes, mode 0600) and survive restarts. `GET /api/analyses` lists them.
- `WEB_DIR` lets the Go binary serve the built UI, so production is one process. Graceful shutdown on SIGINT/SIGTERM: running analyses are ended and their notifications get time to finish. See "Shutdown".
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
| `RATE_LIMIT_PER_HOUR` | `20` | `POST /api/analyses` requests per API key in a sliding hour. Requests the server rejects (`400`, `409`, `429 too_many_analyses`) count too. Local requests are not limited. |
| `MODEL_API_KEY` / `MODEL_API_KEY_FILE` | unset | Model key, or a file that contains only the key. It is never logged. |
| `MODEL_ID`, `MODEL_BASE_URL` | `muse-spark-1.3`, `https://api.meta.ai/v1` | |
| `WEB_DIR` | unset | Built UI (`web/dist`, must contain `index.html`), served at `/`. See "Production build". |
| `DATA_DIR` | unset | Persisted analyses (`analyses/*.json`) and API keys (`keys.json`, hashes only). |
| `LOG_FORMAT` | `text` | `text` or `json` (log/slog on stderr). With `json` every log line is JSON, except the line `regressionmap: <error>` printed when the server cannot start or exits with an error. In `json` lines, the message and every string value of an attribute have each character from U+0080 to U+009F, each Unicode format character (category Cf, for example U+202E) and U+2028/U+2029 replaced by the six-character text `\uXXXX` (four lowercase hex digits). A format character above U+FFFF, for example U+E0041, becomes its UTF-16 surrogate pair, two such texts (`\udb40\udc41`). The JSON encoder then escapes the backslash, so the raw line shows `\\u202e` and a JSON parser reads the six characters `\u202e`, not U+202E. An error value is replaced by its escaped error text only when that text holds such a character; otherwise it is written as before, as its error text or, for an error type with its own JSON encoding, as that encoding. Values of other kinds are written as before. The JSON encoder already escapes U+0000 to U+001F. `text` lines already quote these characters. |
| `WEBHOOK_SECRET` | unset | Enables `notify_url` (completion webhooks) and is the HMAC key of `json` deliveries. At least 32 characters (Unicode characters, not bytes), used exactly as set; a shorter value stops the server at startup. Unset: webhooks are disabled. It is never logged. See "CI and notifications". |
| `TEST_ENV_ALLOW` | unset | Comma-separated variable names that test and verification runs receive although the name rule withholds them. Git never receives them. See "Test environment". |

**Test environment.** Test runs, verification runs and git get the server's environment without `MODEL_API_KEY`, `MODEL_API_KEY_FILE`, `WEBHOOK_SECRET` and every variable whose name ends in `_KEY`, `_SECRET`, `_TOKEN` or `_PASSWORD`, in any letter case. If your tests need such a variable, name it in `TEST_ENV_ALLOW`, for example `TEST_ENV_ALLOW=DB_PASSWORD,STRIPE_TEST_KEY`. Listed names reach the baseline and HEAD test runs and verification runs. Git and its hooks never get them.

- Spaces around entries are trimmed and empty entries are ignored. Names match in any letter case. Naming a variable without such a suffix changes nothing.
- Each entry must be a variable name (letters, digits and `_`, not starting with a digit). Otherwise the server does not start: `TEST_ENV_ALLOW entry N is not a variable name: …`. The message gives the position, never the text, so a mistaken `NAME=value` entry does not print the value.
- `MODEL_API_KEY`, `MODEL_API_KEY_FILE` and `WEBHOOK_SECRET` can never be let through. Naming one stops the server at startup: `TEST_ENV_ALLOW must not name MODEL_API_KEY: the server's own secrets are never passed to test runs`.
- The startup line `RegressionMap API starting` carries `test_env_withheld`, the sorted names (never the values) of the variables in the server's environment that test runs do not get, and `test_env_allow`, the names from `TEST_ENV_ALLOW`. If a suite fails at both revisions with a missing credential, check `test_env_withheld` first.
- The rule goes by name only. A password inside a value, for example in `DATABASE_URL`, is still passed.

**Shutdown.** On Ctrl+C or SIGTERM the server:

1. stops taking requests and waits up to 10 s for open ones (log lines `shutting down`, then `stopped`);
2. ends every running analysis with status `error` and the error `analysis interrupted: the server stopped while it was running`, which stops its test processes and starts its completion notification;
3. waits up to 25 s more for running analyses and for every notification still being sent (log lines `draining`, then `drained`). If the 25 s run out, it logs the warning `drain incomplete` instead, with an `error` field that counts what was still open, for example `1 analysis run(s) still stopping, 1 notification(s) still being sent: context deadline exceeded`, and a `timeout` field (`25s`).

A stop takes at most about 35 s. A notification that is not finished by then is never retried: the URL lived in memory only. With `DATA_DIR`, the next process shows it as `failed` with `last_error` `delivery interrupted: the server stopped before it finished`; attempts made before the stop are not kept, so `attempts` shows 0. Without `DATA_DIR`, nothing survives a restart.

Demo flow: pick `pagination-demo`, pick the first commit as the baseline, start the analysis, read the hypotheses, then run `go run ./cmd/mkfixture -fix ../demo-workspace`, tick the approval box and run the verification. Reset with `git -C demo-workspace/pagination-demo checkout -- src/pagination.ts`.

## Tests

```sh
cd backend && gofmt -l . && go vet ./... && go test -count=1 ./...   # gofmt -l must print nothing
cd web && npx vitest run && npx tsc -b && npm run build
node --test "scripts/test/*.test.mjs"                                 # from the repository root
```

The backend tests need `git` and Node 24 on `PATH`, because the fixture tests run `node --test`.

The last line runs both CI scripts against a Node fake server. Quote the glob: Node does not search a directory argument. The sh cases use `sh` (dash on Ubuntu, Git Bash on Windows); `RM_TEST_SH=dash` picks another shell. The ps1 cases use `powershell.exe`, else `pwsh`. Without either, they are skipped with `neither powershell.exe nor pwsh is on PATH, so the ps1 cases cannot run here`. The run takes about a minute, because two cases wait the full 30 s of the notification wait.

CI (`.github/workflows/ci.yml`) runs the same checks on every push to `main` and on every pull request, on `ubuntu-latest` and `windows-latest`. The CI script tests run in the backend job, as the step "CI script tests". The gofmt check runs on Linux only. Windows runners check out with CRLF line endings, which gofmt would flag.

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

A failing CI run can start an analysis and get the result back in two ways: the CI script waits for the result and writes a summary, or the server calls a webhook when the analysis finishes. Webhooks are the v3 contract: see "Completion notifications (v3)" and "Review fixes to v3" in `docs/API_CONTRACT.md`.

**1. Get an `analyze` key.** On a server you run, create one with `server keys create --name ci --scope analyze` (it uses `DATA_DIR`; in the Docker image: `/app/regressionmap keys create --name ci --scope analyze`) or on the UI's API Keys page. The secret (`rm_live_…`) is shown once. Store it as a CI secret, for example `REGRESSIONMAP_KEY`. Do not use an `admin` key in CI. Each key may make `RATE_LIMIT_PER_HOUR` `POST /api/analyses` requests in a sliding hour. Requests the server rejects count too: `400 invalid_repo`, `400 invalid_baseline`, `409 analysis_running`, `429 too_many_analyses` and the webhook errors below. So CI runs with a wrong `REPO` or `BASELINE` use up the key's quota without starting an analysis. Over the limit the answer is `429 rate_limited` with `Retry-After`; a request refused that way does not count. For the hosted demo, the operator gives out the keys.

**2. Call the script.** `scripts/ci-analyze.sh` (POSIX sh, needs `curl` and `node`) and `scripts/ci-analyze.ps1` (Windows PowerShell 5.1 or PowerShell 7) take the same inputs and give the same output and exit codes. They start `POST /api/analyses`, poll every `POLL_SECONDS` (default 3 s), and print a markdown summary: status, baseline and HEAD PASS/FAIL from the exit codes, the ranked hypotheses with `file:line`, confidence and cited evidence IDs under the label "AI-generated hypotheses — not verified", and the number of rejected hypotheses. With `GITHUB_STEP_SUMMARY` set, the summary is also appended there.

```sh
export REGRESSIONMAP_URL=https://regressionmap.example.com   # http:// only for 127.0.0.1, localhost or [::1]
export REGRESSIONMAP_KEY=...                                  # from a CI secret; never on the command line
REPO=my-repo BASELINE=<good-sha> sh scripts/ci-analyze.sh
# optional: COMMAND_ID (default node-test), NOTIFY_URL, NOTIFY_FORMAT, TIMEOUT_SECONDS (600), POLL_SECONDS (3)
```

Inputs:

- `REGRESSIONMAP_URL`: `https://` to any host. Plain `http://` only when the host part is exactly `127.0.0.1`, `localhost` or `[::1]`, optionally followed by `:` and a port of digits. The host part is the text after the scheme up to the first `/`. It must not contain `@` or a backslash, also with `https://`. The scheme must be lowercase (`HTTPS://` is refused). With `http://`, the host part must be one of the three names above exactly as written, in lowercase (`http://LOCALHOST` is refused). An `https://` host is accepted in any letter case. Trailing slashes are removed.
- `TIMEOUT_SECONDS`: digits without a leading zero. `0` is allowed and times out after the first poll. `POLL_SECONDS`: digits above 0 without a leading zero.
- A bad URL or number is a usage error (exit 1) before anything is sent, for example `ci-analyze: REGRESSIONMAP_URL must not contain '@' or a backslash before its path` or `ci-analyze: TIMEOUT_SECONDS must be a whole number of seconds, without leading zeros`.

Output:

- Server text is untrusted. Whitespace collapses to one space, control characters are removed, and `::` and `##[` are broken with an invisible zero-width space. In the summary, inline text escapes `\`, `[`, `]` and HTML, and a backtick becomes `'`, so model text cannot add links, images or HTML. GitHub may still turn a bare URL into a link. A number that is not an integer prints as `?`, and a hypothesis that is not a JSON object is skipped.
- When `GITHUB_SHA` is set and the analysed HEAD is another commit, the line after the HEAD line reads ``- Warning: the server analysed `<sha>`, not `<sha>` (GITHUB_SHA): the server's clone may lag behind CI.`` The exit code does not change. On `pull_request` events `GITHUB_SHA` is the merge commit GitHub made, so the warning shows unless the server's clone has that commit.
- When the analysis finishes and its notification is still `pending`, the script polls every 2 s for up to 30 s more and prints the final state, for example `- Notification: delivered to hooks.example.com (json, 1 attempt(s))`. A notification that has not settled by then reads `still pending`. A run with a webhook can therefore take about 30 s longer. A failed request during this wait ends the wait and does not change the exit code.
- In GitHub Actions (`GITHUB_ACTIONS=true`), every block of output that carries server data is wrapped in `::stop-commands::<token>` and `::<token>::` on the same stream, with a new random 32-hex token each run, so server text cannot run workflow commands. The `GITHUB_STEP_SUMMARY` file never holds these lines, and local runs print no fence.
- API errors go to stderr as plain text, for example `ci-analyze: HTTP 401 unauthorized: a valid API key is required: Authorization: Bearer <key>`. Redirects are never followed: a 3xx prints its status, for example `ci-analyze: HTTP 302 error`. On `429` the script also prints `ci-analyze: Retry-After: <n> s`, or `not given` when the header is absent.

The key is read from the environment only. The shell script writes it to a private header file for curl, in a directory made with `umask 077` and `mktemp` under `RUNNER_TEMP`, else `TMPDIR`, else `/tmp`. The directory is removed on exit, also on HUP, INT, PIPE and TERM. Every curl call starts with `-q`, so no `.curlrc` is read, and passes `--proto`. The key is never in a process command line and never printed. `ci-analyze.ps1` decodes every response as UTF-8, so non-ASCII model text is right under Windows PowerShell 5.1, and each request times out after 30 s.

Exit codes: `0` analysis done (the tests can still fail: read the summary), `1` usage error, `2` analysis error, `3` timeout, `4` HTTP, network or auth error (prints the API error code and message). `docs/ci/github-actions-example.yml` is a workflow for your own repository: it runs the tests and, when they fail, downloads the script at a pinned commit and runs it.

**3. Webhooks.** Add `notify_url` (and optionally `notify_format`: `json`, the default, or `slack`) to `POST /api/analyses`, or set `NOTIFY_URL` / `NOTIFY_FORMAT` for the script. The server must have `WEBHOOK_SECRET` set, at least 32 characters (Unicode characters, not bytes; Docker: see "Enable webhooks" in `deploy/README.md`). Its startup log line shows `webhooks_enabled`. The hosted demo has webhooks disabled unless its operator sets `WEBHOOK_SECRET`. A server without it answers any `notify_url` with `400 webhooks_disabled`, and the script then exits 4 without starting an analysis.

The server checks the notification fields in this order:

1. `notify_url` is given and `WEBHOOK_SECRET` is not set: `400 webhooks_disabled`.
2. `notify_url` is given and breaks a URL rule (see "SSRF rules"): `400 invalid_notify_url`.
3. `notify_format` is given and is not exactly `json` or `slack` (`JSON` is refused): `400 bad_request`, `notify_format must be json or slack`, with or without `notify_url`.

An empty string counts as not given. A valid `notify_format` without `notify_url` is accepted and ignored.

When the analysis reaches `done` or `error`, the server sends one `POST` (`User-Agent: RegressionMap-Webhook/1`, 5 s timeout per attempt, up to 3 attempts after 1 s and 4 s, at most 2 minutes in all, retried only on network errors, 5xx and 429). A notification that cannot finish before the server stops is reported as `failed` and never retried (see "Shutdown").

- `json` bodies carry `event`, `id`, `repo`, `status`, `error`, `baseline`, `current`, `baseline_passed`, `current_passed`, `model_status`, `top_hypothesis` (or `null`), `created_at` and `finished_at`. `baseline_passed` and `current_passed` are `false` when that run did not happen, and `model_status` is `""` when the model step did not run, so read `status` and `error` first. The bodies are signed: `X-RegressionMap-Event: analysis.completed` and `X-RegressionMap-Signature: sha256=<hex HMAC-SHA256 of the exact body with WEBHOOK_SECRET>`.
- `slack` bodies are `{"text": "…"}`: one paragraph with the repo, the status, PASS, FAIL or `not run` for the baseline and HEAD, and the top hypothesis title with its `file:line` and confidence. It has no evidence and no source code and ends with "AI-generated hypothesis, not verified." Slack bodies are not signed; the token in the Slack URL protects them.

`GET /api/analyses/{id}` shows the delivery under `notification`: host, format, status (`pending`, `delivered` or `failed`), attempts, last status code and `last_error`. While the server is still sending, the status is `pending`, also when the analysis has left the in-memory list. `last_error` never holds the URL path or query, a resolved address, or control characters, and is at most 500 characters.

Treat a notify URL as a secret. The server never logs, returns or stores the full URL, and the scripts never print it. The server keeps it in memory only until the delivery finishes. The host is not secret: `notification.host` is shown to every key holder, stored in `DATA_DIR`, and logged when a delivery fails. The rest of the URL is the secret part, so put a receiver's credential in the path or query, never in the host name.

Verify a `json` webhook on the raw body, before parsing it:

```js
const crypto = require('node:crypto');
function verifySignature(rawBody, header, secret) { // rawBody: Buffer with the exact request body
  const want = Buffer.from('sha256=' + crypto.createHmac('sha256', secret).update(rawBody).digest('hex'));
  const got = Buffer.from(String(header || ''));
  return got.length === want.length && crypto.timingSafeEqual(got, want);
}
```

A valid signature proves only that the body came from this server. It does not prove that the delivery is for your analysis. `WEBHOOK_SECRET` is one secret per server: every receiver checks with the same value, and anyone who holds it can sign a body for any receiver of that server. Give it only to receivers you trust. Also, any key holder who knows your URL can make the server send you a validly signed delivery for their own analysis, and a captured body still verifies when it is sent again. So after the signature check, a receiver should:

- accept only an `id` that its own `POST /api/analyses` returned;
- process each `id` once (a retry sends the same bytes);
- reject a body whose `finished_at` is older than a few minutes.

Per-key secrets and a signed timestamp header are not in v3.

**SSRF rules.** A `notify_url` must be `https://`, at most 2048 characters (Unicode characters, not bytes), have a host, have no user info, and use port 443 or none. Otherwise the answer is `400 invalid_notify_url` with one of these messages: `notify_url is longer than 2048 characters`, `notify_url is not a valid URL`, `notify_url must use https://`, `notify_url must have a host`, `notify_url must not contain user info`, `notify_url must use port 443 or no port`.

The host must not contain a control character (U+0000 to U+001F, U+007F to U+009F), a Unicode format character (category Cf, for example U+200B or U+202E), a space or line or paragraph separator (for example U+00A0 or U+2028), invalid UTF-8, or the replacement character U+FFFD. The check cannot tell U+FFFD from invalid UTF-8, so a validly encoded U+FFFD (raw, or `%EF%BF%BD`) is refused too. The host is checked after percent-decoding, so `h%C2%85ooks.example.com` is refused like the raw form. Such a URL gets `400 invalid_notify_url` with `notify_url is not a valid URL`. This check comes right after the URL is parsed, before the `https://` check. Other non-ASCII letters stay allowed, so an internationalised host such as `bücher.example` is accepted.

At delivery time the server resolves the host and refuses to send the request if any resolved address is not public, for IPv4 and IPv6:

- loopback, private, link-local, CGNAT, multicast, unspecified and cloud-metadata addresses;
- the documentation ranges `192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24` and `2001:db8::/32`;
- IPv6 addresses that embed an IPv4 address: 6to4 (`2002::/16`), Teredo (`2001::/32`) and IPv4-translated (`::ffff:0:0:0/96`), whatever IPv4 address they carry.

An IPv4-mapped address (`::ffff:a.b.c.d`) is judged by the IPv4 address it carries. A refused host makes the notification `failed` after one attempt, with `last_error` `host does not resolve to a public address`. A name that does not exist or has no address gets the same answer. The address itself is never shown or logged. Other DNS errors, such as a timeout, read `resolve <host>: <error>` and are retried. The server connects only to an address that passed the check and checks it again at connect time, so a DNS rebind between the check and the connection cannot redirect it. It does not follow redirects and never uses `HTTP_PROXY` or `HTTPS_PROXY`.

**Hosted vs self-hosted.** The server analyses its own git clone under `WORKSPACE_ROOT`, at that clone's HEAD. It does not receive code from CI. The hosted demo contains only `pagination-demo`, so for any other repository it answers `400 invalid_repo`. To analyse your own repository, run a server yourself with a clone of it in `WORKSPACE_ROOT`, keep that clone at the commit that failed, and give the baseline as a commit that exists in the clone (otherwise `400 invalid_baseline`). Hosted bring-your-own-repo is a plan, not a feature: see `docs/ROADMAP-billing.md`.

## Limits

| Input | Limit | Above the limit |
| --- | --- | --- |
| Diff between baseline and HEAD | 8 MiB | The first 8 MiB, cut at a line boundary, are analysed. |
| Changed-file list | 1 MiB | Only the entries that fit whole are analysed. |
| A file read for evidence | 1 MiB | The first 1 MiB, cut after a complete line, is used. A stack frame past the cut shows `(source larger than 1 MiB, not read)`. |
| Test output | 256 KiB per stream | The rest is dropped and the run has `"truncated": true`. |
| Evidence excerpt | 3000 bytes | Cut, then a new line and `…[truncated]`. |
| Evidence title | 200 bytes | Cut on a character boundary, then `…[truncated]`. |
| Evidence bundle | 40000 bytes of excerpts | Items that do not fit are left out. |

The analysis continues after a cut. Each cut of the first three kinds adds one sentence to a `warnings` field on the analysis (omitted when empty), in this order:

- `changed-file list larger than 1 MiB: only the files that fit were analysed`
- `diff larger than 8 MiB: only the first 8 MiB were analysed`
- `N file(s) larger than 1 MiB were read only in part`, where N counts distinct files

Warnings never name a file. The model's input starts with a note that the server cut the input, and the system prompt asks the model to say so in `limitations`. The `source` item of a changed file read in part covers its complete lines and ends with the `…[truncated]` mark; if not one complete line fits, it has lines `0`–`0` and the excerpt `(source larger than 1 MiB, not read)`. `(source not readable)` still means the read failed. The UI does not show `warnings` yet: read them with `GET /api/analyses/{id}`.

A git failure during an analysis names its step: the error starts with `baseline worktree failed: `, `HEAD worktree failed: `, `changed-file list failed: ` or `diff failed: `, followed by git's own message. `baseline_run` and `current_run` are saved as soon as each run finishes, so a later git failure keeps them. Every git call waits at most 2 s after git exits for its output. If a hook leaves a background process that holds the output open, the call fails (`exec: WaitDelay expired before I/O complete`) instead of hanging. `GET /api/repos/{name}/commits` still answers `500 git_failed` when the `git log` output passes 1 MiB.

## Safety design

- No shell: git and test processes are started from argument arrays. Revisions must be `HEAD` or hex and are resolved to a full SHA before use.
- Repositories must be direct children of `WORKSPACE_ROOT`.
- Model output is data. Its `verification_suggestion` is shown as text and never executed.
- The API key is read on the backend only, never returned by `/api/health`, and scrubbed from error messages.
- Test runs, verification runs and git never receive the model key, `WEBHOOK_SECRET`, or variables with credential-like names (see "Test environment"). This is defence in depth, not a boundary: test code runs as the server's user.

## Limitations

Hypotheses are not proof of cause. The confidence label is a qualitative estimate from the model, not a calibrated probability. Evidence parsing targets node:test output; other runners produce a failure summary without parsed frames.

Path redaction can merge two files: changed files whose paths differ only in a part that looks like a secret show the same `[REDACTED]` path in the evidence. A URL in a model-written title can still show as a link in Slack.
