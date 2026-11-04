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
5. Reviews the change between the baseline and HEAD with Muse (the **Muse review**, v4). Muse writes findings through three lenses: `code` (bugs, error handling, concurrency, resource leaks, broken edge cases), `feature` (whether the change does what its commit messages and tests say, missing cases, behaviour changes a user would notice) and `security` (injection, authentication and authorisation, secrets, unsafe input handling, SSRF, path traversal, unsafe deserialisation). Each finding must cite evidence IDs, including at least one diff hunk or source item. The evidence now also holds one `commit` item per commit in the range, newest first, at most 10. A second Muse call then tries to refute each finding, and the server validates both answers without a model. Each finding is labelled `Confirmed by Muse check`, `Disputed by Muse check`, `Refuted by Muse check` or `Not checked`. Refuted findings are hidden by default, and nothing is gated on findings. The review shows in the API, the web UI, the CI summary, the `json` webhook and the Slack text, which now ends with `AI-generated hypothesis and review, not verified.`
6. Lets the developer apply a fix and approve a verification rerun. The test runner's exit code decides PASS or FAIL. The model never does.

## What makes it different
RegressionMap is not a chatbot. The tools collect the evidence deterministically, and the model only ranks and explains it. Every AI claim points to evidence the user can inspect. The UI labels the output "AI-generated hypotheses — not verified". The model's suggested verification is shown as text and is never executed. Every surface that shows the Muse review carries the line `AI review: Muse wrote these findings and a second Muse pass checked them. Not verified by tests.`

## Meta technology
- **Model:** `muse-spark-1.3`
- **Access method:** OpenAI-compatible Chat Completions, `POST https://api.meta.ai/v1/chat/completions`, with `Authorization: Bearer $MODEL_API_KEY`. Source: https://dev.meta.ai/docs/protocols/chat-completions
- **Structured output:** `response_format` of type `json_schema` with `strict: true`. Source: https://dev.meta.ai/docs/structured-output
- **What the model does:** ranks plausible causes, explains each hypothesis using the cited evidence, lists counter-evidence, and states its limitations. In the Muse review it writes findings about the change and, in a second call, tries to refute each one. An analysis makes up to three model calls (hypotheses, review, check), one after another, with the same key.
- **What stays deterministic:** git diff extraction, commit messages, test execution, evidence collection and IDs, secret redaction, output validation of the hypotheses, the review and the check, and the PASS/FAIL result of verification.
- **Live result observed on 2026-11-04 with the demo fixture:**
  - The baseline test passed and HEAD failed with exit code 1.
  - RegressionMap collected 9 evidence items.
  - The model ranked "Off-by-one in paginate slice end" in `src/pagination.ts` first, with high confidence, citing E1–E4, E6 and E9.
  - It ranked the unrelated `format.ts` change and the test edit as low and cited counter-evidence against both.
  - No hypotheses were rejected.
  - After the fix, the approved verification run exited with code 0.
- **Live Muse review observed on 2026-11-04 on the hosted demo:** the review returned two findings, and the Muse check confirmed both. The `feature` finding cites the commit message: the commit says `refactor`, but the diff changes behaviour and the tests fail. Details are under "Live demo".

## Safety
- No shell is involved. Git and test processes are started from argument arrays. Revisions must be `HEAD` or hex, and are resolved to a full SHA before use.
- Test commands come from an allowlist and are selected by ID. Each run has a timeout and an output cap.
- A verification run requires `"approved": true`. The Run button stays disabled until the user ticks the approval box.
- Repositories must be direct children of the configured workspace root.
- The API key is kept on the backend only. It is never returned to the browser and it is scrubbed from error messages.
- Test runs, verification runs and git never receive the model key, the webhook secret, or any variable whose name ends in `_KEY`, `_SECRET`, `_TOKEN` or `_PASSWORD`. An operator can let named variables through to test and verification runs with `TEST_ENV_ALLOW`, never to git and never the server's own secrets. The startup log lists the names, never the values, of the variables that test runs do not get.
- Completion webhooks are off unless the operator sets `WEBHOOK_SECRET` (at least 32 characters, counted as Unicode characters, not bytes). `json` deliveries are signed with HMAC-SHA256. The server sends them only over `https://` to hosts whose every address is public, checks the address again at connect time, follows no redirects, and never logs, returns or stores the full notify URL.
- Likely secrets in the evidence are redacted before the model request. The review path uses the same redaction, commit messages included.
- Model-written text (titles, details, suggestions, reasons, the summary) is always shown as text: never as HTML, a Markdown link or image, a Slack link or a GitHub workflow command. In the CI summary, GitHub may still turn a bare URL into a link, and Markdown emphasis and headings in model text can still render.
- The hosted demo runs in public demo mode (`PUBLIC_DEMO=1`). A request without an API key gets only the `analyze` scope: it can list the demo repository, start and read analyses and run verifications, and API key management answers it with 403. A request that carries a key is checked as before. All keyless requests share one limit of `RATE_LIMIT_PER_HOUR` analyses an hour (20 on the hosted demo), and each analysis makes at most three model calls with the server's key. Webhooks are off on the hosted demo.

## New work completed during the hackathon
The work was built from an empty repository on 2026-11-04, inside the Oct 26 – Nov 4, 2026 window.
- `95a14f5` — Go API, evidence collection, Meta model integration with schema validation, approval-gated verification, demo fixture generator, API contract, README, MIT licence. `go test ./...` passes for all 7 packages.
- `b5a95c4` — React UI: three-pane layout, pull-request style diff with each hypothesis shown as an inline comment, walkthrough and changes table, approval-gated verification card. 76 vitest tests pass.
- `fadabf2` — Production hardening: Host/Origin checks against DNS rebinding and cross-site requests, security headers, model retries with backoff, per-repository and global job limits, persisted analyses, single-binary serving of the UI, graceful shutdown, GitHub Actions CI.
- `397c193` — Frozen API contract for API keys, remote mode and rate limits.
- `bf73db2` — API keys (hashed, admin/analyze scopes, per-key rate limit, UI page and CLI), remote mode, hardened Docker image and Traefik deployment.
- `2476892`, `0a351fc` — Frozen v3 contract and its build: completion webhooks (signed `json` and Slack), CI scripts for GitHub Actions in sh and PowerShell, and hardening.
- `2a49002` to `c6f005d` — v3 review fixes, frozen in the contract first (`2a49002`), then built in lanes and merged (`ed0ed82` to `792aacd`). `c6f005d` was deployed.
- `8e512d6`, `74f3fdf`, `4f6ae12` — Design, frozen contract and implementation plan of the Muse change review (v4).
- `77734fc`, `f4c7045`, `b196ccb`, `cab97b5` — Muse change review, built test-first in four lanes: commit messages as evidence, the review and check calls with strict schemas and server-side validation, the review in the `json` webhook and the Slack text, the review panel with findings inline in the diff, and the review in the CI summary.
- `5928301`, `71f3cdc`, `953c644`, `da97b16` — Documentation of the review, and tests for the gaps the lane verifiers found.
- `db96cef`, `f6b2c0d` — Public demo mode, frozen in the contract first: the hosted demo runs without sign-in.
- `2c810bf`, `3895b0f` — Corrections of record from the final review: secrets are redacted from the whole commit message before its title is taken, a model answer cut off mid-body is a failed call, and the CI summary uses the verdict labels.
- `7c1efcc` — Every analysis runs the Muse review and the Muse check after the hypotheses, under a deadline 15 s before the job deadline. Deployed on 2026-11-04.

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
- Muse review findings are not proof. A finding confirmed by the Muse check is supported by the cited evidence in a second Muse pass, not shown by a test.
- Repository text, commit messages included, can steer the review: it can hide problems or invent them. The evidence rules, the check call and the labels limit this. They do not remove it.
- The review makes an analysis take longer, because its model calls run one after another. The hypotheses are saved and shown as soon as they exist.
- Without `DATA_DIR`, analyses are kept only in memory and are lost when the server restarts. With `DATA_DIR` (as in the hosted demo, see `deploy/Dockerfile`), they are saved to disk and survive restarts.
- In public demo mode all visitors share one hourly analysis limit, and requests the server rejects count too, so one visitor can use it up for everyone for up to an hour.

## Live demo
https://regressionmap.glenox.net

No sign-in and no API key are needed. The hosted demo runs in public demo mode (`PUBLIC_DEMO=1`): a request without a key runs with the `analyze` scope, so a visitor can open the page, pick `pagination-demo` with its first commit as the baseline, start an analysis, and read the hypotheses and the Muse review. All visitors share one limit of 20 analyses per hour. API key management still needs an admin key.

It runs on the author's server as a hardened Docker container behind Traefik, with a Let's Encrypt certificate, through Cloudflare. Live since 2026-11-04.

v4 live checks, measured on 2026-11-04 after the v4 deploy:

- On the server, the checkout `/opt/regressionmap` runs `7c1efcc` (`7c1efcc38846933142fbeeed78e8547fe61a2fe7`). The container `regressionmap` runs the image `regressionmap:local`, also tagged `regressionmap:7c1efcc`. It was started at 2026-11-04T15:31:34Z, reports healthy, and has a stop timeout of 40 s.
- The startup log line shows `"public_demo":true`, `"webhooks_enabled":false`, `"rate_limit_per_hour":20`, `"model_configured":true` and `"test_env_withheld":"MODEL_API_KEY"`, after the warning that public demo mode is on.
- From outside the server, without a key: `GET /api/health` returns `{"auth_required":false,"model_configured":true,"model_id":"muse-spark-1.3","ok":true}`, `GET /api/me` returns `{"mode":"public"}`, `GET /api/repos` returns 200 with `pagination-demo`, and `GET /api/keys` returns 403 `forbidden_scope`. The page returns 200, and plain HTTP answers `HTTP/1.1 301 Moved Permanently` to HTTPS.
- One analysis, started from outside the server without a key, ended `done` in 59 s:
  - the baseline passed and HEAD exited with code 1;
  - 11 evidence items were collected, among them two commit items, E10 and E11, one per commit in the range;
  - the hypotheses were shown about 26 s after the start, while the review was still running;
  - the model returned `ok` with 0 rejected hypotheses: rank 1, high confidence, "Off-by-one in paginate: inclusive end used with exclusive slice" in `src/pagination.ts` (lines 7–13), citing E1–E4, E6 and E9; the `format.ts` change and the test edit were ranked low, with counter-evidence;
  - the Muse review returned `ok` with two findings and 0 rejected, and the Muse check returned `ok`: both findings are `Confirmed by Muse check`, and the list row shows `{"confirmed":2,"disputed":0,"refuted":0,"unchecked":0}`;
  - the `code` finding, high: "Off-by-one in paginate: exclusive slice end with -1 drops last item" in `src/pagination.ts` (lines 7–13);
  - the `feature` finding, high: "Pagination change breaks expected pageSize behavior despite refactor label", citing the commit item E10 (`refactor: clarify page end index`) next to the diff and the failing tests;
  - the `security` lens reported no finding.

Earlier checks, kept for the record. Until the v4 deploy, every `/api` call except the health check needed an API key.

- v2, measured on 2026-11-04 against the public URL: `/` returned 200, and plain HTTP redirected to HTTPS (301). `GET /api/health` returned `{"auth_required":true,"model_configured":true,"model_id":"muse-spark-1.3","ok":true}`. `GET /api/repos` returned 401 without a key and with an invalid key. One analysis with an admin key completed: the baseline passed and HEAD exited with code 1, 9 evidence items were collected, and the model returned `ok` with 0 rejected hypotheses. Rank 1, with high confidence, was "Off-by-one in paginate slice end index in src/pagination.ts" (lines 7–13), citing E1–E4, E6 and E9. The unrelated `format.ts` change and the test edit were ranked low.
- v3, measured on 2026-11-04 after the v3 deploy: the checkout ran `c6f005d` (`c6f005d2e6470ae609622c1463f5596ae4373982`). The container ran the image `regressionmap:local`, also tagged `regressionmap:c6f005d`; it was deployed at 2026-11-04T11:23:11Z, reported healthy, and had a stop timeout of 40 s. `GET /api/health` returned `{"auth_required":true,"model_configured":true,"model_id":"muse-spark-1.3","ok":true}`, `GET /api/repos` without a key returned 401, and plain HTTP answered 301. The startup log line showed `"webhooks_enabled":false`, `"rate_limit_per_hour":20` and `"test_env_withheld":"MODEL_API_KEY"`.

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
- [x] The live demo URL loads over HTTPS and needs no sign-in: an analysis without a key completed on the hosted demo (measured 2026-11-04, after the v4 deploy).
- [x] No judges' key is needed: the hosted demo runs in public demo mode (`PUBLIC_DEMO=1`).
- [ ] The demo video is uploaded, and the link opens without requesting access.
- [ ] All claims still match the code at the final commit.
- [ ] All team members are added.
- [ ] The official form confirms that the submission went through.
