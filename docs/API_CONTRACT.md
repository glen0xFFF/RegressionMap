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

## POST /api/analyses

Request:

```json
{ "repo": "pagination-demo", "baseline": "sha-or-short-sha", "command_id": "node-test" }
```

Response `202` with an Analysis (status `running`). Poll `GET /api/analyses/{id}` until status is `done` or `error`.

## GET /api/analyses/{id}

```jsonc
{
  "id": "a1",
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
