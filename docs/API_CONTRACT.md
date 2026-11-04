# RegressionMap API contract (v1)

Frozen contract between the Go backend (`backend/`) and the React UI (`web/`).
All bodies are JSON. Errors use `{"error": {"code": "string", "message": "string"}}` with a 4xx/5xx status.

The backend listens on `127.0.0.1:8787`. The Vite dev server proxies `/api` to it.

## GET /api/health

```json
{ "ok": true, "model_configured": false, "model_id": "muse-spark-1.3" }
```

`model_configured` is true only when the backend has a `MODEL_API_KEY`. The key itself is never returned.

## GET /api/repos

Repositories found directly under the configured workspace root (each must contain `.git`).

```json
{ "repos": [ { "name": "pagination-demo" } ] }
```

## GET /api/repos/{name}/commits

Newest first, at most 50.

```json
{ "commits": [ { "sha": "40-hex", "short": "7-hex", "subject": "string" } ] }
```

## GET /api/commands

The allowlist. The UI picks by `id`; it never sends a command string.

```json
{ "commands": [ { "id": "node-test", "argv": ["node", "--test"], "description": "Run node:test suite" } ] }
```

## GET /api/analyses

Newest first, at most 50, in-memory and persisted analyses combined.

```json
{ "analyses": [ { "id": "a…", "repo": "pagination-demo", "status": "done",
  "baseline": "40-hex", "current": "40-hex", "created_at": "RFC3339", "current_passed": false } ] }
```

`current_passed` is false while the current run has not finished.

## POST /api/analyses

Request:

```json
{ "repo": "pagination-demo", "baseline": "sha-or-short-sha", "command_id": "node-test" }
```

Response `202` with an Analysis (status `running`). Poll `GET /api/analyses/{id}` until status is `done` or `error`.

## GET /api/analyses/{id}

```jsonc
{
  "id": "a…",                     // opaque, matches ^a[0-9a-z-]{1,40}$
  "created_at": "RFC3339",
  "status": "running" | "done" | "error",
  "error": "string, only when status=error",
  "repo": "pagination-demo",
  "baseline": "40-hex",
  "current": "40-hex",            // HEAD at analysis time
  "command": { "id": "node-test", "argv": ["node", "--test"] },
  "baseline_run": TestRun,         // test run in a temp worktree at baseline
  "current_run": TestRun,          // test run in a temp worktree at HEAD
  "changed_files": [ { "path": "src/pagination.ts", "status": "M" } ],
  "evidence": [ Evidence ],
  "model": ModelResult,
  "verifications": [ Verification ]
}
```

`TestRun`:

```json
{ "exit_code": 1, "passed": false, "timed_out": false, "truncated": false,
  "duration_ms": 412, "stdout": "string", "stderr": "string" }
```

`passed` is `exit_code == 0 && !timed_out`. It comes from the process exit status, never from the model.

`Evidence`:

```jsonc
{ "id": "E1",
  "type": "test_failure" | "assertion" | "stack_frame" | "diff_hunk" | "source",
  "title": "string",
  "file": "src/pagination.ts",   // repo-relative, forward slashes; "" when not file-bound
  "line_start": 3,               // 0 when unknown (never invented)
  "line_end": 9,                 // 0 when unknown
  "excerpt": "exact text" }
```

`ModelResult`:

```jsonc
{ "status": "ok" | "disabled" | "error" | "invalid",
  "model_id": "muse-spark-1.3",
  "message": "human readable reason when not ok",
  "rejected": [ "hypothesis 2 cites unknown evidence id E99" ],  // items dropped by validation
  "result": {                      // present only when status=ok
    "summary": "string",
    "hypotheses": [ {
      "rank": 1, "title": "string",
      "file": "src/pagination.ts", "line_start": 4, "line_end": 4,
      "reason": "string",
      "supporting_evidence_ids": ["E1", "E4"],
      "counter_evidence_ids": [],
      "verification_suggestion": "string (text only, never executed)",
      "confidence_label": "low" | "medium" | "high" } ],
    "limitations": ["string"] } }
```

## POST /api/analyses/{id}/verify

Runs an allowlisted command in the repository's **working tree** (so an uncommitted fix is included).

Request — `approved` must be literally `true`, or the server returns `400 not_approved`:

```json
{ "command_id": "node-test", "approved": true }
```

Response `200` with a `Verification`:

```json
{ "command_id": "node-test", "argv": ["node", "--test"], "at": "RFC3339",
  "run": TestRun }
```

The verification is also appended to the analysis `verifications` list.

## Errors added by production hardening

| Status | `code` | When |
|---|---|---|
| 403 | `forbidden_host` | Host header is not an allowed host (default 127.0.0.1, localhost, [::1]). |
| 403 | `forbidden_origin` | Unsafe method with a foreign `Origin`, `Origin: null`, or `Sec-Fetch-Site` other than same-origin/none. |
| 415 | `unsupported_media_type` | POST without `Content-Type: application/json`. |
| 409 | `analysis_running` | An analysis of the same repository is already running. |
| 409 | `repo_busy` | Verification requested while the repository is being analysed or verified. |
| 429 | `too_many_analyses` | The global cap on concurrent analyses is reached. |
| 500 | `internal` | A handler panicked. |

A model `error` result message now states how many attempts were made, for example `model API returned HTTP 503 after 3 attempt(s): …`.

## Authentication and API keys (v2)

### Who needs a key

- **Local requests are trusted without a key.** A request is local only when all three hold: the TCP peer address is loopback, the Host header is an allowed loopback host, and there is no `X-Forwarded-For` / `Forwarded` header.
- **Every other request to `/api/*` needs `Authorization: Bearer <secret>`**, except `GET /api/health`, which stays public.
- Missing or invalid key: `401 {"error":{"code":"unauthorized",...}}` with header `WWW-Authenticate: Bearer`. A revoked key is invalid.

### Keys

- Secret format: `rm_live_` followed by 32 base62 characters. The secret is shown once, at creation, and is never stored: the server stores its SHA-256 hash and compares in constant time.
- Each key has `id` (`k_` + 10 base36 chars), `name` (1–64 printable chars), `scope`, `prefix` (first 12 chars of the secret), `created_at`, `last_used_at` (RFC3339 or null) and `revoked` (bool).
- Scopes:
  - `analyze`: every `/api/*` route except `/api/keys*`.
  - `admin`: everything.
- Keys live in `<DATA_DIR>/keys.json`, written atomically with mode 0600.

### GET /api/health (addition)

Adds `"auth_required": true|false`. It is true when the caller is not local.

### GET /api/me

```json
{ "mode": "local" }
{ "mode": "key", "key": { "id": "k_…", "name": "ci", "scope": "analyze", "prefix": "rm_live_ab12" } }
```

### GET /api/keys (admin or local)

```json
{ "keys": [ { "id": "k_…", "name": "ci", "scope": "analyze", "prefix": "rm_live_ab12",
  "created_at": "RFC3339", "last_used_at": null, "revoked": false } ] }
```

### POST /api/keys (admin or local)

Request `{ "name": "ci", "scope": "analyze" }`. Response `201`:

```json
{ "key": { …key… }, "secret": "rm_live_…" }
```

### DELETE /api/keys/{id} (admin or local)

Response `204`. Revoking is permanent. A key cannot revoke itself (`409 cannot_revoke_self`).

A caller with an `analyze` key gets `403 forbidden_scope` on all `/api/keys*` routes.

### Rate limit

`POST /api/analyses` is limited per key to `RATE_LIMIT_PER_HOUR` (default 20) in a sliding hour. Over the limit: `429 rate_limited` with a `Retry-After` header in seconds. Local requests are not limited.

### Server configuration (v2)

| Variable | Meaning |
|---|---|
| `ALLOWED_HOSTS` | Comma-separated extra hostnames accepted in Host/Origin, for example `regressionmap.glenox.net`. |
| `RATE_LIMIT_PER_HOUR` | Per-key analysis limit (default 20). |
| `REGRESSIONMAP_ALLOW_REMOTE=1` | Allows a non-loopback `ADDR`. It also requires `DATA_DIR` and at least one active `admin` key, or the server refuses to start. |

### CLI

`server keys create --name NAME --scope admin|analyze` prints the secret once on stdout. `server keys list` and `server keys revoke ID` also exist. All three use `DATA_DIR` and work while the server is stopped or running.

## Completion notifications (v3)

### Request

`POST /api/analyses` accepts two optional fields:

```json
{ "repo": "pagination-demo", "baseline": "03492ae", "command_id": "node-test",
  "notify_url": "https://hooks.example.com/…", "notify_format": "json" }
```

- `notify_format` is `json` (the default) or `slack`.
- The server must have `WEBHOOK_SECRET` set. Otherwise any `notify_url` is rejected with `400 webhooks_disabled`.
- `notify_url` must be `https://`, at most 2048 characters, have no user info, and use port 443 or none. Otherwise `400 invalid_notify_url`.
- At delivery time the host is resolved. Every resolved address must be public: no loopback, private, link-local, CGNAT, multicast, unspecified, or cloud-metadata addresses, for IPv4 and IPv6 alike. The connection is made only to an address that passed this check, so a DNS rebind between the check and the connection cannot redirect it. Redirects are not followed.
- **Treat a notify URL as a secret.** Slack webhook URLs contain a token. The full URL is never logged, never returned by the API, and never written to `DATA_DIR`. It lives in memory only until delivery finishes.

### Delivery

- **When:** once, after the analysis reaches `done` or `error`.
- **Request:** `POST`, `Content-Type: application/json`, `User-Agent: RegressionMap-Webhook/1`, with a 5 s timeout per attempt.
- **Retries:** up to 3 attempts, with backoff of 1 s and then 4 s. Only network errors and 5xx/429 responses are retried.
- **Signature (`json` format only):** header `X-RegressionMap-Signature: sha256=<hex HMAC-SHA256 of the exact body using WEBHOOK_SECRET>` and header `X-RegressionMap-Event: analysis.completed`.

`json` body:

```json
{ "event": "analysis.completed", "id": "a…", "repo": "pagination-demo", "status": "done",
  "error": "", "baseline": "40-hex", "current": "40-hex",
  "baseline_passed": true, "current_passed": false,
  "model_status": "ok",
  "top_hypothesis": { "rank": 1, "title": "…", "file": "src/pagination.ts",
                      "line_start": 7, "line_end": 13, "confidence_label": "high" },
  "created_at": "RFC3339", "finished_at": "RFC3339" }
```

`top_hypothesis` is `null` when the model produced no valid hypothesis.

`slack` body: `{ "text": "<one-paragraph summary>" }`. The summary contains the repo, the status, PASS/FAIL for the baseline and HEAD, and the top hypothesis title with its `file:line` and confidence. It always ends with "AI-generated hypothesis, not verified." It contains no evidence excerpts and no source code.

### Status on the analysis

`GET /api/analyses/{id}` adds `"notification": null` or:

```json
{ "host": "hooks.example.com", "format": "json",
  "status": "pending" | "delivered" | "failed",
  "attempts": 1, "last_status_code": 200, "last_error": "" }
```

Only the host is shown. `last_error` never contains the URL path or query.

## Review fixes to v3 (addendum, 2026-11-04)

A three-lens review (code, feature, SecOps) of the v3 build found the problems this addendum fixes. It was frozen before the fixes were built. It removes no v3 field. Where it changes behaviour, it says so.

### Notification request checks

`POST /api/analyses` checks the notification fields in this order:

1. `notify_url` is present and `WEBHOOK_SECRET` is not set: `400 webhooks_disabled`.
2. `notify_url` is present and breaks a URL rule: `400 invalid_notify_url`.
3. `notify_format` is present and is not `json` or `slack`: `400 bad_request`, with or without `notify_url`.

"At most 2048 characters" counts Unicode characters, not bytes.

### Address check (additions)

These addresses are also non-public, so the server never connects to them:

- IPv6 addresses that embed an IPv4 address: 6to4 (`2002::/16`), Teredo (`2001::/32`) and IPv4-translated (`::ffff:0:0:0/96`). An IPv4-mapped address (`::ffff:a.b.c.d`) is judged by the IPv4 address it carries.
- The documentation ranges `192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24` and `2001:db8::/32`.

### `last_error` and logs

- `last_error` never contains a resolved address. A host with no public address gives `host does not resolve to a public address`.
- Control characters (U+0000 to U+001F and U+007F to U+009F) are removed from `last_error` and from logged error text.
- Delivery failures go through the server's structured logger, so with `LOG_FORMAT=json` every log line is JSON.

### Shutdown

On SIGTERM or Ctrl+C the server:

1. stops accepting requests and waits up to 10 s for open requests, as before;
2. ends every running analysis with status `error` and the error `analysis interrupted: the server stopped while it was running`, which starts its notification;
3. waits up to 25 s more for deliveries that are still running.

A delivery that has not finished by then is reported as `failed` with `delivery interrupted: the server stopped before it finished`. It is not retried after a restart. The Docker deployment sets `stop_grace_period` to 40 s.

While this process is still delivering, GET shows the notification as `pending`, also when the analysis is no longer held in memory. The outcome of a finished delivery is always recorded, also when the analysis left memory during the delivery.

### Size limits and the new `warnings` field

| Input | Limit | What happens above it |
|---|---|---|
| Diff between baseline and HEAD | 8 MiB | The first 8 MiB, cut at a line boundary, are used. |
| Changed-file list | 1 MiB | The entries that fit are used. |
| A file read for evidence | 1 MiB | The first 1 MiB, cut at a line boundary, is used. A stack frame beyond the cut shows `(source larger than 1 MiB, not read)`. |

The analysis continues after a cut. Each cut adds one sentence to a new `warnings` field on the analysis, and the model input states that the input was cut:

```jsonc
"warnings": ["diff larger than 8 MiB: only the first 8 MiB were analysed"]   // omitted when empty
```

`baseline_run` and `current_run` are saved before the git steps, so a later git failure keeps them. A git failure names its step, for example `changed-file list failed: …`.

### Model input and output

- The `changed_files` list sent to the model uses the same path redaction as evidence: a secret-looking path segment becomes `[REDACTED]`. The API's own `changed_files` keeps the real paths.
- The system prompt states the output limits below.
- A `title` over 200 characters, a `reason` over 2000 or a `verification_suggestion` over 500 is cut to its limit and ends with `…[truncated]`. The hypothesis is kept. **Changed:** the v3 build rejected it.
- An empty `summary` no longer makes the result `invalid`. A missing `summary` field still does.

The complete validation, in the order applied:

1. A missing top-level field (`summary`, `hypotheses` or `limitations`) makes the result `invalid`.
2. A hypothesis is rejected, with its reason in `rejected`, when it cites no supporting evidence id, cites an unknown id, names an unsafe file or one that is not in the evidence, has an inverted range, has a range that is not within 3 lines of an evidence span for the same file (`0`–`0` is exempt), has an unknown confidence label, has a rank below 1, or repeats the rank of an earlier hypothesis (the first one is kept).
3. Only the 5 best-ranked hypotheses that remain are kept.

### Test environment

- Test runs, verification runs and git never receive `MODEL_API_KEY`, `MODEL_API_KEY_FILE`, `WEBHOOK_SECRET`, or any variable whose name ends in `_KEY`, `_SECRET`, `_TOKEN` or `_PASSWORD`, in any letter case.
- New setting `TEST_ENV_ALLOW`: comma-separated variable names that test runs and verification runs receive anyway. Git never receives them. If it names `MODEL_API_KEY`, `MODEL_API_KEY_FILE` or `WEBHOOK_SECRET`, the server refuses to start.
- At startup the server logs the names, never the values, of the variables it withholds from test runs.

### Server configuration (v3)

| Variable | Meaning |
|---|---|
| `WEBHOOK_SECRET` | Enables `notify_url`. At least 32 characters; a shorter value stops the server at startup. When unset, webhooks are disabled. |
| `TEST_ENV_ALLOW` | Comma-separated variable names passed to test and verification runs despite the name rule above. |

### Clarifications (no behaviour change)

- **Rate limit:** every authenticated remote `POST /api/analyses` counts, including requests the handler rejects (`400`, `409`, `429 too_many_analyses`).
- **json body:** `baseline_passed` and `current_passed` are `false` when that run did not happen, and `model_status` is `""` when the model step did not run. Read `status` and `error` first.
- **Host:** `notification.host` is shown to every key holder and stored in `DATA_DIR`. The rest of the URL is the secret part.
- **Receivers:** a valid signature proves that the body came from this server. It does not prove that the delivery is for your analysis, because `WEBHOOK_SECRET` is one secret for the whole server. Accept only an `id` that your own `POST /api/analyses` returned, process each `id` once, and reject a `finished_at` older than a few minutes.

### Not in this addendum

Per-key webhook secrets and a signed timestamp header need a later contract version. The Slack text still shows URLs from model-written titles as links.

## Corrections of record to the v3 addendum (2026-11-04)

These entries correct or complete the addendum above after the final review of the v3 build. No line above is changed. The code already behaves as described here; for entries 3, 4 and 5 it does so from the same release as this section.

### 1. Model validation (corrects item 2 of "The complete validation")

Item 2 of the list in "Model input and output" reads in full:

2. A hypothesis is rejected, with its reason in `rejected`, when it cites no supporting evidence id, cites an unknown id, names an unsafe file or one that is not in the evidence, has a line range that starts below 1 or ends before it starts (`0`–`0` means unknown and is allowed), has a range that is not within 3 lines of an evidence span for the same file when the evidence has a known line range for that file, has an unknown confidence label, has a rank below 1, or repeats the rank of an earlier hypothesis (the first one is kept).

The evidence has a known line range for a file when at least one evidence item for that file has `line_start` above 0. Without one, the 3-line rule does not apply to that file. The two range reasons read `invalid line range <start>-<end>` and `line range <start>-<end> does not overlap any evidence for <file> (within 3 lines)`.

### 2. Plain-text log lines (completes "`last_error` and logs")

"With `LOG_FORMAT=json` every log line is JSON" means every line written through the server's logger. These lines stay plain text:

- `regressionmap: <error>`, printed on stderr when the server cannot start or exits with an error. Configuration errors are found before the logger exists, so this line cannot be JSON.
- In the Docker image, the container entrypoint's own `entrypoint: …` lines and, on a start where `/workspace/pagination-demo` does not exist yet, the lines the demo fixture builder prints: `created /workspace/pagination-demo`, `baseline <sha>`, `refactor <sha>` and `bug      <sha>`, or its error text if it fails.

### 3. Escaping in JSON log lines (new log rule)

With `LOG_FORMAT=json`, every string value the logger writes has each character from U+0080 to U+009F, each Unicode format character (general category Cf) and U+2028 and U+2029 written as the six-character escape `\uXXXX`, with four lowercase hex digits. This covers the message, attribute values of kind string, and error values, which are written as their error text. U+0000 to U+001F are escaped by the JSON encoder, as before. The `text` format does not change; it already quotes these characters.

### 4. Characters in the `notify_url` host (addition to the URL rules)

A `notify_url` whose host contains a control character (U+0000 to U+001F or U+007F to U+009F), a Unicode format character (category Cf), a white-space character, a space, line or paragraph separator (categories Zs, Zl and Zp), or invalid UTF-8 is refused with `400 invalid_notify_url` and the message `notify_url is not a valid URL`.

- The host is checked after percent-decoding, so the raw form (for example U+0085 in the JSON string) and the percent-encoded form (`%C2%85`) are both refused.
- Other non-ASCII letters stay allowed: an internationalised host such as `bücher.example` is accepted.
- The check runs right after the URL is parsed and before the `https://` check. The order of the request checks in "Notification request checks" does not change.

### 5. `WEBHOOK_SECRET` length (no rule change)

The minimum of 32 characters in "Server configuration (v3)" counts Unicode characters, not bytes, as the 2048-character limit of `notify_url` does. The error text stays `WEBHOOK_SECRET must be at least 32 characters (it is the HMAC key of webhook signatures); unset it to disable webhooks`. A release before this one counted bytes, so it accepted a shorter secret made of multibyte characters.

### 6. Analysis fields, evidence and warnings (completes the addendum)

- **`finished_at`:** `GET /api/analyses/{id}` carries `finished_at` (RFC 3339, UTC) once the analysis has finished. It is set in the same update that moves the status from `running` to `done` or `error`, and is omitted while the analysis runs. An analysis that a previous process left `running`, and that is read back from `DATA_DIR` as `error`, has no `finished_at`. Rows of `GET /api/analyses` do not carry it.
- **Evidence `file`:** each path segment goes through the evidence secret redaction on its own, so the part of a segment that looks like a secret becomes `[REDACTED]` and the rest of the path stays, for example `config/token=[REDACTED]/app.ts`.
- **Evidence `title`:** goes through the same secret redaction, and a copy of the item's path inside the title shows the redacted path. The title is then capped at 200 bytes, cut on a character boundary, and ends with `…[truncated]` when it was cut. The mark is added after the cut and is not counted in the 200 bytes.
- **Warnings:** each size limit that cuts the input adds one sentence, at most one per limit, in this order:
  - `changed-file list larger than 1 MiB: only the files that fit were analysed`
  - `diff larger than 8 MiB: only the first 8 MiB were analysed`
  - `<N> file(s) larger than 1 MiB were read only in part`: one sentence for all files read in part together, where N counts distinct paths.

  Warnings never name a file.

### 7. Deliveries cut off by a stop (completes "Shutdown")

A delivery still running when the 25 s drain ends is not finished by this process: the process exits without recording an outcome.

- With `DATA_DIR`, the stored analysis keeps the notification as it was when the analysis finished: `status` `pending`, `attempts` 0, `last_status_code` 0 and `last_error` `""`. The next process shows it as `failed` with `last_error` `delivery interrupted: the server stopped before it finished`. `attempts` and `last_status_code` stay 0, also when attempts were made before the stop. The next process never retries the delivery, because the URL was held in memory only.
- Without `DATA_DIR`, nothing of the analysis remains after the restart.

### 8. Escaped log text, the U+FFFD host character and the secret count (completes entries 3, 4 and 5)

These points complete entries 3, 4 and 5. They change no rule. The code already behaves this way, from the same release as those entries.

- **Entry 3, what a reader gets:** each escape is six characters of text inside the string value: a backslash, `u` and four lowercase hex digits. The JSON encoder escapes that backslash, so the raw line shows `\\u202e` and a JSON parser returns the six characters `\u202e`, not U+202E.
- **Entry 3, characters above U+FFFF:** a format character above U+FFFF, for example the tag character U+E0041, is written as its UTF-16 surrogate pair, two such escapes: `\udb40\udc41`.
- **Entry 3, error values:** an error value is replaced by its escaped error text only when that text holds a character entry 3 names. Otherwise it is written as before: as its error text or, for an error type with its own JSON encoding, as that encoding. Values of kinds other than string and error, and any text inside them, are written as before.
- **Entry 4, U+FFFD:** the host check cannot tell invalid UTF-8 from the replacement character U+FFFD, so a host that holds a validly encoded U+FFFD, raw or percent-encoded as `%EF%BF%BD`, is refused too, with `400 invalid_notify_url` and `notify_url is not a valid URL`.
- **Entry 5, bytes that are not UTF-8:** each byte of `WEBHOOK_SECRET` that is not part of a valid UTF-8 sequence counts as one character.

### 9. Strings inside groups and LogValuer values (corrects entry 8)

Entry 8 says that values of kinds other than string and error, and any text inside them, are written as before. That is not complete. The handler applies the escaping of entry 3 to each attribute inside a group and to the value a LogValuer resolves to, so a string inside a group, or a string that a LogValuer returns, is escaped like any other string value. Lists, maps and structs are written as before. The server logs no groups and no LogValuer values, so no line the server writes changes.

## Muse change review (v4, 2026-11-04)

Every analysis also reviews the change between the baseline and HEAD. Muse (the configured `MODEL_ID`, `muse-spark-1.3` by default, called with the same key as the hypotheses) writes findings through three lenses. A second Muse call then tries to refute each finding. The server checks both answers without a model. Nothing in the review is verified: in RegressionMap, "verified" stays reserved for the approval-gated test rerun, whose exit code decides PASS or FAIL.

This section was frozen before any v4 code. It removes no v1, v2 or v3 field. It changes one v3 behaviour, the Slack text, and says so where it does.

### Lenses

- `code`: bugs, error handling, concurrency, resource leaks, broken edge cases.
- `feature`: whether the change does what its commit messages and tests say, missing cases, behaviour changes a user would notice.
- `security`: injection, authentication and authorisation, secrets, unsafe input handling, SSRF, path traversal, unsafe deserialisation.

### When the review runs

1. The hypothesis call runs first, as before. Its result is saved as soon as it exists. While the review runs, `GET /api/analyses/{id}` shows `status` `running`, the `model` result, and `review` `null`.
2. The review call runs next, whatever the hypothesis call returned. When the review keeps at least one finding, the check call runs after it. The three model calls run one after another and never overlap.
3. The analysis becomes `done` when the review chain has ended. A review problem only sets `review.status` or `review.check_status`. It never changes the analysis `status` or `error`. The webhook is still delivered once, at the end.
4. The review chain has its own deadline, 15 s before the job deadline:
   - If the review call is still running at that point, `review.status` is `error` with the message `review stopped at the analysis deadline`.
   - If the review call has finished and the check call is still running, `review.status` stays `ok`, `check_status` is `error` with `check stopped at the analysis deadline`, and every finding is `unchecked`.
   - If that point has already passed when the hypothesis call returns, the review does not start, and `review.status` is `error` with `no time was left for the review`.
5. Without a model key, `review.status` is `disabled` with the hypothesis text `Meta model not configured: MODEL_API_KEY is not set on the backend.` No call is made.
6. If the analysis ends before its evidence exists (for example after a git failure), `review.status` is `skipped` with the message `the analysis ended before the change could be reviewed`.
7. If the analysis ends as `error` after its evidence exists (the job deadline during the hypothesis call, or a shutdown), a review that has finished is kept. A review that has not finished gets `review.status` `error` with `the analysis ended before the review finished`. A check that has not finished gets `check_status` `error` with the same message, and every finding is `unchecked`. Without a model key the review is `disabled`, as in rule 5.
8. An analysis that was still running when the server stopped without a drain is loaded as `error` at the next start, as before. Its `review` stays `null`.

### New evidence type `commit`

The evidence `type` enum adds `commit`: one item per commit that HEAD has and the baseline does not have (`git log <baseline>..<HEAD>`), newest first, at most 10.

- `file` is `""`. `line_start` and `line_end` are `0`.
- `title` is `commit <first 7 characters of the sha>: <first line of the message>`. It goes through the evidence secret redaction and the 200-byte title cap.
- `excerpt` is the whole commit message without trailing white space. It goes through the evidence secret redaction. A message over 1000 bytes is cut on a character boundary and ends with the evidence cut mark (a line break and `…[truncated]`).
- Commits are added newest first. The first commit whose excerpt would bring the `commit` excerpts above 4000 bytes is left out, and so is every older one.
- The `commit` items count against the 40,000-byte evidence budget. Their space is set aside before the other items are added, so a large diff cannot push them out. They come after every other item, so the other items keep the ids they had before v4.
- If git cannot list the commit messages, the analysis goes on without `commit` items, and `warnings` gets `commit messages could not be read: the evidence holds no commit items`. If git's output for them is larger than 1 MiB, only the commits that fit are used, and `warnings` gets `commit messages larger than 1 MiB: only the commits that fit were read`.
- The hypothesis call receives them too, and a hypothesis may cite them.

### `review` on `GET /api/analyses/{id}`

`GET /api/analyses/{id}` adds `review`. It is `null` for an analysis stored before v4, and while the review chain has not ended.

```jsonc
"review": {
  "status": "ok" | "disabled" | "error" | "invalid" | "skipped",
  "model_id": "muse-spark-1.3",
  "message": "",                       // why, when status is not ok; "" when it is ok
  "check_status": "ok" | "error" | "invalid" | "skipped",
  "check_message": "",                 // why, when check_status is not ok; "" when it is ok
  "summary": "string",
  "counts": { "confirmed": 2, "disputed": 1, "refuted": 1, "unchecked": 0 },
  "findings": [ {
    "id": "R1",
    "lens": "code" | "feature" | "security",
    "severity": "high" | "medium" | "low",
    "title": "string", "detail": "string",
    "suggestion": "string (text only, never executed)",
    "file": "src/x.ts", "line_start": 4, "line_end": 9,
    "evidence_ids": ["E3", "E7"],
    "check": { "verdict": "confirmed" | "disputed" | "refuted" | "unchecked",
               "reason": "string", "evidence_ids": ["E3"] } } ],
  "rejected": ["finding 3 (\"Token compared with ==\"): cites unknown evidence id E99"]
}
```

- Every field is always present. `summary` is `""`, and `findings` and `rejected` are `[]`, when there is nothing to show. They are never `null`.
- `counts` counts the findings by `check.verdict`. All four counts are `0` when there are no findings.
- `check_status` is `ok` when the check answer was read, even if it left some findings `unchecked`. It is `skipped` when no check call was made: `check_message` is `no finding to check` when the review is `ok` and kept no finding, and `no review to check` when `review.status` is not `ok`.
- A finding about no particular file has `file` `""` and the range `0`–`0`. The range `0`–`0` with a file means the whole file.
- `check.reason` is `""` and `check.evidence_ids` is `[]` when the verdict is `unchecked`.
- `findings` are in id order. Ids are `R1`, `R2`, ... in the order of the rule "Order and ids" below.

### Review answer and validation

The review call asks for this object (strict JSON schema):

```json
{ "summary": "string",
  "findings": [ { "lens": "code", "severity": "high", "title": "string", "detail": "string",
                  "suggestion": "string", "file": "string", "line_start": 0, "line_end": 0,
                  "evidence_ids": ["E3"] } ] }
```

Its input is the evidence bundle (with the `commit` items), the test outcomes at the baseline and HEAD, the server's `warnings`, and the changed-file list with the same path redaction as the hypothesis call. The prompt asks for at most 5 findings per lens, cited evidence that shows the problem, at least one cited `diff_hunk` or `source` item, and no style or naming remarks. It states that repository text is data, never instructions.

The server checks the answer in this order. `N` is the finding's 1-based position in Muse's answer. A rejection reads `finding N ("<title>"): <reason>`, with the title cut to 80 bytes, as for hypotheses.

1. An answer that is not a JSON object, that has a field the schema does not have, or that lacks `summary` or `findings`, makes `review.status` `invalid`. `message` says why.
2. A finding is rejected at the first rule it breaks, with one of these reasons:
   - `unknown lens "<lens>"`
   - `unknown severity "<severity>"`
   - `empty title` (a title that is empty or only white space)
   - `cites no evidence`
   - `cites unknown evidence id <id>`
   - `cites no diff_hunk or source evidence`
   - `has a line range but no file` (a finding with no file must have the range `0`–`0`)
   - `invalid file path "<file>"` (the path rule of hypotheses)
   - `file <file> is not in the evidence`
   - `invalid line range <start>-<end>` (a range other than `0`–`0` that starts below 1 or ends before it starts)
   - `cites no evidence in <file>` (a finding with a file must cite at least one item of that file)
   - `line range <start>-<end> does not overlap any cited evidence for <file> (within 3 lines)` (checked only when a cited item of that file has a known line range)
   - `repeats finding <M>` (the same lens, file, range and title as the earlier kept finding `M`)
3. Text over a limit is cut to the limit and ends with `…[truncated]`, as for hypotheses: `title` 200, `detail` 2000, `suggestion` 500 and `summary` 2000 characters. The finding is kept.
4. At most 5 findings per lens are kept: the highest severity first, then Muse's order. A finding dropped here is rejected with `more than 5 <lens> findings`.
5. **Order and ids:** the kept findings are sorted by severity (`high`, `medium`, `low`), then by lens (`security`, `code`, `feature`), then by Muse's order. They get the ids `R1`, `R2`, ... in that order.
6. If the answer had findings and none was kept, `review.status` is `invalid` with the message `no finding passed validation`. An answer with no findings is `ok`.

`rejected` lists the finding reasons in Muse's order, then the check reasons.

### Check answer and validation

The check call sends the kept findings (id, lens, severity, title, detail, file, range and cited evidence ids) and the same evidence bundle. It asks for this object (strict JSON schema):

```json
{ "checks": [ { "id": "R1", "verdict": "confirmed", "reason": "string", "evidence_ids": ["E3"] } ] }
```

The prompt asks Muse to try to refute each finding using only the evidence: `confirmed` when the cited evidence shows the problem, `disputed` when the evidence is ambiguous or the problem is unlikely in practice, `refuted` when the evidence shows that the problem does not exist.

1. An answer that is not a JSON object, that has a field the schema does not have, or that lacks `checks`, makes `check_status` `invalid`. Every finding is `unchecked`.
2. A check for an id that is not a kept finding is ignored. Only the first check for a finding counts. Later checks for the same finding are ignored.
3. A check is rejected when its verdict is unknown or it cites an unknown evidence id. The rejection reads `check for <id>: unknown verdict "<verdict>"` or `check for <id>: cites unknown evidence id <evidence id>`.
4. `reason` is cut at 1000 characters with the same mark.
5. A finding without a valid check is `unchecked`. If the check call fails, `check_status` is `error`, `check_message` says why, and every finding is `unchecked`.

### `review_counts` on `GET /api/analyses`

Each row adds `review_counts`: the `counts` object of its review, or `null` when the analysis has no review.

### Display order and wording (every client)

- The section is called **Muse review**. Where a verdict is written out, the labels are `Confirmed by Muse check`, `Disputed by Muse check`, `Refuted by Muse check` and `Not checked`.
- Every surface that shows the review carries this line: `AI review: Muse wrote these findings and a second Muse pass checked them. Not verified by tests.`
- **Display order:** confirmed, then unchecked, then disputed, then refuted findings. Within each group they keep the order of `findings`. Refuted findings are hidden by default: the API always includes them, the web UI shows them behind a toggle, and the CI summary and the Slack text only count them.
- Severity is shown as a word, not only as a colour.
- Model-written text (titles, details, suggestions, reasons, the summary) is always shown as text: never as HTML, a Markdown link or image, a Slack link or a GitHub workflow command.

### Webhooks (v4)

The `json` body adds `review` as its last field. It is `null` when the analysis has no review:

```json
"review": { "status": "ok", "confirmed": 2, "disputed": 1, "refuted": 0, "unchecked": 0,
            "top_finding": { "id": "R1", "lens": "security", "severity": "high", "title": "…",
                             "file": "src/x.ts", "line_start": 4, "line_end": 9,
                             "verdict": "confirmed" } }
```

`top_finding` is the first confirmed finding in display order, else the first unchecked one, else `null`. The `review` object carries no detail, suggestion, reason, evidence or summary. The other body fields do not change.

**Changed:** the `slack` text.

- The hypothesis title, each finding title and each location sit inside an inline code span, with backticks removed from the string. Inside a code span Slack makes no links. `&`, `<` and `>` are still escaped. An empty title becomes the plain word `untitled`. A location is the file, plus `:<line_start>` when `line_start` is above 0.
- After the hypothesis part, the text adds one sentence about the review:
  - `Muse review: <counts>.` lists the counts above 0 in display order, followed by ``Top: [<lens>, <severity>] `<title>` (`<location>`).`` for `top_finding`. The `Top:` part is left out when `top_finding` is `null`, and the location in brackets is left out when the finding has no file.
  - `Muse review: no findings.` when the review is `ok` with no findings.
  - `Muse review: not available (<status>).` when `review.status` is not `ok`.
  - There is no review sentence when the analysis has no review.
- It ends with `AI-generated hypothesis and review, not verified.` instead of `AI-generated hypothesis, not verified.`
- Example: ``RegressionMap analysis of pagination-demo finished with status done. Baseline PASS, HEAD FAIL. Top hypothesis: `end index off by one` (`src/pagination.ts:7`, confidence high). Muse review: 1 confirmed, 1 disputed. Top: [code, high] `Last page drops one item` (`src/pagination.ts:7`). AI-generated hypothesis and review, not verified.``
- This closes the item "The Slack text still shows URLs from model-written titles as links" of the v3 addendum.

### Security notes

- Everything the review path sends to the model goes through the same redaction as the hypothesis path, commit messages included.
- Repository text, commit messages included, can steer the review: it can hide problems or invent them. The evidence rules, the check call and the labels limit this. They do not remove it.
- Nothing a model writes is executed, followed or rendered as markup.

### Not in v4

Gating on findings (exit codes and analysis statuses do not change), rerunning a review on demand, dismissing or editing findings, a pull-request description as input for the feature lens, reviews of analyses stored before v4, and another model or provider.

## Public demo mode (v4, 2026-11-04)

Optional. It lets a hosted demo run without the sign-in step. It is off unless the operator turns it on.

### Server configuration

- `PUBLIC_DEMO=1` turns it on. Any other value, or no value, leaves it off.
- The startup log line adds `"public_demo": true|false`. When it is on, the server also logs a warning that `/api` requests without a key run as the public demo identity.

### Who needs a key (change)

With `PUBLIC_DEMO=1`, a request to `/api/*` that is not local and carries no `Authorization` header is served as the public demo identity, which has the `analyze` scope. A request that carries an `Authorization` header is checked as before: a malformed, unknown or revoked key still gets `401 unauthorized`. Local requests do not change.

### What the public demo identity can do

- Everything the `analyze` scope allows: list repositories, commits, commands and analyses, start and read analyses, and run verifications.
- `GET /api/keys`, `POST /api/keys` and `DELETE /api/keys/{id}` answer `403 forbidden_scope`, as for an `analyze` key.
- `POST /api/analyses`: all public demo requests share one rate-limit bucket of `RATE_LIMIT_PER_HOUR` per sliding hour. Over the limit: `429 rate_limited` with `Retry-After`, as for a key.

### GET /api/health (change)

With `PUBLIC_DEMO=1`, `auth_required` is `false` for every caller, because a caller that is not local can use the API without a key.

### GET /api/me (addition)

The public demo identity gets:

```json
{ "mode": "public" }
```

### Web UI

When `GET /api/health` says `auth_required` `false`, the UI shows no sign-in card. For `{ "mode": "public" }` it offers no API key management and no sign-out. A key stored in the tab is still sent and checked.

### Security notes

- Anyone who can reach the server can start analyses and verifications. Each analysis makes up to three model calls with the server's key, so the model cost is bounded by the shared rate limit and the job limits only.
- The existing limits still apply: one analysis or verification per repository at a time, and at most two analyses running at once.
- API key management still needs an admin key or a local request.

## Corrections of record to the v4 section (2026-11-04)

The final review of the v4 build found these points. Each entry corrects or completes a rule above.

### 1. Ids of the other evidence items (corrects "New evidence type `commit`")

The `commit` items come after every other item, so the other items keep their order. They keep the ids they had before v4 only while the budget has room for all of them: when the space set aside for the `commit` items leaves out an item that v3 would have kept, every later item gets a lower id.

### 2. Secrets that start on the first line of a commit message (completes "New evidence type `commit`")

The secret redaction runs on the whole message before the title takes its first line. A secret that starts on the first line and ends on a later one is therefore removed from the title as well as from the excerpt.

### 3. A model answer that is cut off (completes "When the review runs", rules 4 and 7, and check rule 5)

A model call whose response body cannot be read to the end has failed; it is not an invalid answer. When the deadline or a cancellation ends the read, the call ends as `error` with the same timeout or cancel message as a request that got no response, so rules 4 and 7 apply. Any other read error is retried like a transport error. This holds for the hypothesis, review and check calls.

### 4. Verdicts in the CI summary (completes "Display order and wording")

In the CI summary, the verdict inside each finding line's brackets is written out with the labels: `Confirmed by Muse check`, `Not checked` or `Disputed by Muse check`.
