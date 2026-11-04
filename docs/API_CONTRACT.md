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
