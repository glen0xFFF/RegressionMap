# Hackathon Submission — RegressionMap

> Draft. Every statement below was checked against the code or a real run on 2026-11-04 unless it is marked **TODO**. Fill the TODO items before submitting.

## Project name
RegressionMap

## Track
Build for Your World

## Short tagline
Evidence-driven regression investigation for developers.

## Problem
A test passed last week and fails today. To find out why, a developer has to read the test output, follow the stack trace, look through the diff, and decide which change is responsible. They do this by hand, and an unrelated change in the same range can hide the real one.

## Solution
RegressionMap takes a local Git repository, a known-good baseline commit, and an allowed test command. It then does the following:

1. Runs the tests at the baseline and at HEAD, each in a temporary `git worktree`, and records the real exit codes and output.
2. Collects evidence and gives each item a stable ID (E1, E2, …): failing assertions with actual and expected values, stack frames with the real source lines, diff hunks, and the current source of changed files.
3. Sends this bounded, redacted evidence bundle to a Meta model and asks for ranked root-cause hypotheses as strict JSON. Each hypothesis must cite evidence IDs and may list counter-evidence.
4. Validates the model output. A result with a missing `summary`, `hypotheses` or `limitations` field is invalid. A hypothesis is rejected, and shown as rejected with its reason, when it cites no evidence ID, cites an unknown ID, uses an unsafe path or names a file outside the evidence, gives an invalid line range or one more than 3 lines away from the evidence for that file, has an unknown confidence label or a rank below 1, or repeats an earlier rank. Only the 5 best-ranked hypotheses are kept. A title, reason or verification suggestion over its limit (200, 2000 and 500 characters) is cut and ends with `…[truncated]`.
5. Lets the developer apply a fix and approve a verification rerun. The test runner's exit code decides PASS or FAIL. The model never does.

## What makes it different
RegressionMap is not a chatbot. The tools collect the evidence deterministically, and the model only ranks and explains it. Every AI claim points to evidence the user can inspect. The UI labels the output "AI-generated hypotheses — not verified". The model's suggested verification is shown as text and is never executed.

## Meta technology
- **Model:** `muse-spark-1.3`
- **Access method:** OpenAI-compatible Chat Completions, `POST https://api.meta.ai/v1/chat/completions`, with `Authorization: Bearer $MODEL_API_KEY`. Source: https://dev.meta.ai/docs/protocols/chat-completions
- **Structured output:** `response_format` of type `json_schema` with `strict: true`. Source: https://dev.meta.ai/docs/structured-output
- **What the model does:** ranks plausible causes, explains each hypothesis using the cited evidence, lists counter-evidence, and states its limitations.
- **What stays deterministic:** git diff extraction, test execution, evidence collection and IDs, secret redaction, output validation, and the PASS/FAIL result of verification.
- **Live result observed on 2026-11-04 with the demo fixture:**
  - The baseline test passed and HEAD failed with exit code 1.
  - RegressionMap collected 9 evidence items.
  - The model ranked "Off-by-one in paginate slice end" in `src/pagination.ts` first, with high confidence, citing E1–E4, E6 and E9.
  - It ranked the unrelated `format.ts` change and the test edit as low and cited counter-evidence against both.
  - No hypotheses were rejected.
  - After the fix, the approved verification run exited with code 0.

## Safety
- No shell is involved. Git and test processes are started from argument arrays. Revisions must be `HEAD` or hex, and are resolved to a full SHA before use.
- Test commands come from an allowlist and are selected by ID. Each run has a timeout and an output cap.
- A verification run requires `"approved": true`. The Run button stays disabled until the user ticks the approval box.
- Repositories must be direct children of the configured workspace root.
- The API key is kept on the backend only. It is never returned to the browser and it is scrubbed from error messages.
- Test runs, verification runs and git never receive the model key, the webhook secret, or any variable whose name ends in `_KEY`, `_SECRET`, `_TOKEN` or `_PASSWORD`. An operator can let named variables through to test and verification runs with `TEST_ENV_ALLOW`, never to git and never the server's own secrets. The startup log lists the names, never the values, of the variables that test runs do not get.
- Completion webhooks are off unless the operator sets `WEBHOOK_SECRET` (at least 32 characters, counted as Unicode characters, not bytes). `json` deliveries are signed with HMAC-SHA256. The server sends them only over `https://` to hosts whose every address is public, checks the address again at connect time, follows no redirects, and never logs, returns or stores the full notify URL.
- Likely secrets in the evidence are redacted before the model request.

## New work completed during the hackathon
The work was built from an empty repository on 2026-11-04, inside the Oct 26 – Nov 4, 2026 window.
- `95a14f5` — Go API, evidence collection, Meta model integration with schema validation, approval-gated verification, demo fixture generator, API contract, README, MIT licence. `go test ./...` passes for all 7 packages.
- `b5a95c4` — React UI: three-pane layout, pull-request style diff with each hypothesis shown as an inline comment, walkthrough and changes table, approval-gated verification card. 76 vitest tests pass.
- `fadabf2` — Production hardening: Host/Origin checks against DNS rebinding and cross-site requests, security headers, model retries with backoff, per-repository and global job limits, persisted analyses, single-binary serving of the UI, graceful shutdown, GitHub Actions CI.
- `397c193` — Frozen API contract for API keys, remote mode and rate limits.
- `bf73db2` — API keys (hashed, admin/analyze scopes, per-key rate limit, UI page and CLI), remote mode, hardened Docker image and Traefik deployment.

## Demo flow
1. Run `go run ./cmd/mkfixture ../demo-workspace` to create `pagination-demo`. Commit 1 passes. Commit 2 makes an unrelated formatting change. Commit 3 introduces an off-by-one bug.
2. In the UI, pick `pagination-demo` and the first commit as the baseline, then start the analysis.
3. Show the baseline PASS (exit 0) and the HEAD FAIL (exit 1) with the real assertion: `actual: [ 1, 2 ]`, `expected: [ 1, 2, 3 ]`.
4. Show the evidence list and the ranked hypotheses with their evidence citations. Point out that the unrelated change is ranked low and has counter-evidence.
5. Run `go run ./cmd/mkfixture -fix ../demo-workspace`, tick the approval box, and run the verification. Show the real PASS with exit code 0.

## Limitations
- The hypotheses rank plausible causes. They do not prove causation.
- The confidence label is a qualitative estimate, not a calibrated probability.
- Evidence parsing is built for `node --test` output. Other runners would produce a failure summary without parsed stack frames.
- Only `node --test` is on the command allowlist in this version.
- Without `DATA_DIR`, analyses are kept only in memory and are lost when the server restarts. With `DATA_DIR` (as in the hosted demo, see `deploy/Dockerfile`), they are saved to disk and survive restarts.

## Live demo
https://regressionmap.glenox.net

Live since 2026-11-04. It runs on the author's server as a hardened Docker container behind Traefik, with a Let's Encrypt certificate, through Cloudflare. Checks measured on 2026-11-04 against the public URL:

- `https://regressionmap.glenox.net/` returns 200. Plain HTTP redirects to HTTPS (301).
- `GET /api/health` returns `{"auth_required":true,"model_configured":true,"model_id":"muse-spark-1.3","ok":true}`.
- `GET /api/repos` without a key returns 401. With an invalid key it also returns 401.
- One analysis with an admin key completed:
  - the baseline passed and HEAD exited with code 1;
  - 9 evidence items were collected;
  - the model returned `ok` with 0 rejected hypotheses;
  - rank 1, high confidence: "Off-by-one in paginate slice end index in src/pagination.ts" (lines 7–13), citing E1–E4, E6 and E9;
  - the unrelated `format.ts` change and the test edit were ranked low.

Visitors need an API key, because every `/api` call except the health check requires `Authorization: Bearer`. **TODO:** create a judges' key with the `analyze` scope (`docker compose -f deploy/docker-compose.yml exec regressionmap /app/regressionmap keys create --name judges --scope analyze` on the server) and give it to the judges through the submission form, never in this file.

On the hosted instance, the verification step runs the tests on the demo repository as it is. Visitors cannot apply the fix there, so verification shows FAIL. The full fix-then-PASS flow is in the demo video and works locally.

## Public repository
https://github.com/glen0xFFF/RegressionMap

## Three-minute demo video
**TODO:** paste the public or unlisted video URL once it has been uploaded.

## Team
**TODO:** list every team member. The repository owner is glen0xFFF (Dev Glenox).

## Final pre-submit checklist
- [x] The repository is public. Anonymous HTTP 200 on 2026-11-04.
- [x] The Meta model integration is real and has been run live.
- [x] The UI commit is pushed and its hash is added above.
- [x] The live demo URL loads over HTTPS. The admin key works (measured 2026-11-04).
- [ ] A judges' `analyze` key is created and given through the submission form.
- [ ] The demo video is uploaded, and the link opens without requesting access.
- [ ] All claims still match the code at the final commit.
- [ ] All team members are added.
- [ ] The official form confirms that the submission went through.
