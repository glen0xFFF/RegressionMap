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
