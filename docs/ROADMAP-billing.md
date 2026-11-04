# Paid subscriptions: roadmap

> **Status: PLAN. NOT IMPLEMENTED.** Nothing in this document exists in the code yet. No prices, limits or providers are decided. Every number below is an example that marks the shape of a decision someone still has to make.

## Where we are today (implemented)

- One server, one operator. Keys are created by the operator (`server keys create`, or the UI's API Keys page), stored as SHA-256 hashes in `DATA_DIR/keys.json`, and limited per key by `RATE_LIMIT_PER_HOUR` with a sliding-hour limiter.
- Analyses run against git clones the operator placed under `WORKSPACE_ROOT`. Tests run in a temporary `git worktree` on the server host, with a timeout and an output cap, but **not in a sandbox**.
- Analyses are JSON files in `DATA_DIR`. There are no users, organisations, plans or payments.
- CI (`scripts/ci-analyze.sh`) and completion webhooks (`notify_url`, v3 contract) already work for a self-hosted server.

## Plans (examples only)

| | Free | Pro | Team |
|---|---|---|---|
| Who | one person trying it | one developer | a team sharing repositories |
| Analyses per month | e.g. 30 | e.g. 500 | e.g. 3,000 pooled |
| Concurrent analyses | 1 | e.g. 2 | e.g. 5 |
| Connected repositories | e.g. 1 public | e.g. 10 | e.g. 50 |
| Seats | 1 | 1 | e.g. 10, more as add-on |
| API keys | e.g. 1 | e.g. 5 | e.g. 25 |
| Analysis retention | e.g. 7 days | e.g. 30 days | e.g. 90 days |
| Webhooks | yes | yes | yes |
| Price | free | **not decided** | **not decided** |

Open decisions: prices, whether limits are hard stops or soft overage, annual billing, and tax handling (for example Stripe Tax).

## Self-serve signup (email magic link)

- No passwords. The user enters an email address. The server sends a link with a single-use token (32 random bytes, valid for e.g. 15 minutes). Only a hash of the token is stored, and the comparison runs in constant time.
- Rate limits per email address and per IP on link requests. The response is the same whether or not the address exists.
- The first login creates an organisation on the Free plan, with the user as owner. The user can then create `analyze` keys for that organisation in the UI. `admin` keys stay operator-only.
- Sessions use an HttpOnly, Secure, SameSite=Lax cookie. The existing Host/Origin checks and CSP stay in place.

## Payments (Stripe)

- **Checkout:** an "Upgrade" button creates a Stripe Checkout Session in `subscription` mode for the organisation's Stripe customer. The server never sees card data.
- **Customer Portal:** a "Manage billing" link opens the Stripe Customer Portal for plan changes, payment methods, invoices and cancellation.
- **Webhooks** (one endpoint, signature checked against the raw body with the `Stripe-Signature` header and the endpoint secret, with timestamp tolerance):
  - `checkout.session.completed`: link the Stripe customer and subscription to the organisation.
  - `customer.subscription.updated`: apply the new plan and status (`active`, `past_due`, `canceled`, …).
  - `customer.subscription.deleted`: drop the organisation to Free at period end.
  - `invoice.payment_failed`: mark as `past_due`, email the owner, keep a grace period (e.g. 7 days) before limits fall to Free.
- **Idempotency:** store each Stripe `event.id` in a `stripe_events` table with a unique constraint, and process the event in the same database transaction that records it. A repeated delivery is acknowledged with 200 and does nothing. Events can arrive out of order, so the handler re-reads the subscription from the Stripe API and applies its current state rather than trusting the event order. Outgoing Stripe API calls send an `Idempotency-Key`.
- **Entitlements** are derived only from the stored subscription state, never from what the browser says after the Checkout redirect.

## Usage metering

- Count per organisation and per key: analyses started, analyses finished, verification runs, model calls.
- **Reuse the existing per-key rate limiter** for bursts (requests per hour). Add a monthly quota per organisation, checked and incremented in Postgres in the same transaction that accepts `POST /api/analyses`, so two concurrent requests cannot both take the last unit.
- Only analyses the server accepted count. An analysis that fails because of the server (not because of the user's tests) is refunded to the quota.
- Over the quota, the API returns an error with a stable code (for example `quota_exceeded`). That is a new error code, so it needs a contract change (a v4 section in `docs/API_CONTRACT.md`) before it is built.
- The UI shows usage against the plan limits. Usage is recorded locally; reporting usage to Stripe is only needed if overage billing is chosen.

## Bring your own repository (GitHub App)

- A GitHub App instead of OAuth tokens with the `repo` scope. Least privilege: **Contents: read** and **Metadata: read**, on repositories the user selects at installation. Write permissions (for example Checks, to post results on a commit) only later, as an opt-in, and only if needed.
- The server stores the installation id, never a long-lived token. For each analysis it requests an installation token (valid for one hour), restricted to that one repository, and fetches only the commits it needs.
- The clone exists only for the run and is deleted with the sandbox. Nothing from the repository is kept except the analysis result (evidence excerpts) under the retention rule below.
- Uninstalling the app (installation webhook) disconnects the repositories and stops new analyses.

## Per-run sandbox (hard prerequisite)

Running a stranger's test suite on the server host is remote code execution by design. **No paid plan, and no hosted bring-your-own-repo, before this exists:**

- One throwaway container (or microVM) per analysis, created for the run and destroyed after it. Nothing is reused between runs or customers.
- The repository is mounted **read-only**. Worktrees and build output go to a size-limited scratch volume inside the sandbox.
- **No network** except the model API endpoint, through an egress allowlist. The test command itself gets no network. The model key is never inside the container that runs user code: the model call is made outside it, or through a proxy that adds the key.
- CPU, memory, process-count, disk and wall-clock limits. Non-root user, no added capabilities, seccomp profile, read-only root filesystem.
- No secrets in the sandbox environment (the existing removal of `MODEL_API_KEY` from child processes stays as a second layer).
- Open question: dependency installation (`npm ci`) needs network. Options are a pre-fetch step outside the sandbox or a read-only package cache. This needs a decision.

## Data storage

- Move from JSON files in `DATA_DIR` to **Postgres**: `orgs`, `users`, `memberships`, `api_keys` (hashes only), `magic_link_tokens` (hashes only), `subscriptions`, `stripe_events`, `usage`, `github_installations`, `analyses`.
- Migrations are forward-only. Encrypted storage at rest, daily backups with a tested restore.
- Notify URLs keep the v3 rule: they are never written to the database.

## Privacy

- Analyses **send code to the model provider**: failure output, stack frames with source lines, diff hunks and the current source of changed files. The signup page and the terms must say so plainly. The provider's own retention and training terms must be checked and stated; they are not verified here.
- Secret redaction before the model call is best effort, not a guarantee. Users should not run analyses on code that contains live credentials.
- Retention: analyses (including evidence excerpts) are deleted after the plan's retention period, and on request. Deleting an organisation deletes its analyses, keys and installations.
- We do not train models on customer code.
- Needed before charging: a privacy policy, terms of service, a list of subprocessors (hosting, Stripe, the model provider, the email provider) and a data processing agreement for business customers.

## Phased order

1. **Accounts, no money.** Postgres, organisations, magic-link signup, per-organisation keys, monthly quota on the Free plan. Hosted analyses stay limited to the operator's demo repositories.
2. **Sandbox.** Per-run container with the limits above, plus tests that prove the limits (no network, read-only mount, timeout kill).
3. **Bring your own repository** as a free, invite-only beta: GitHub App, installation tokens, clones deleted after each run.
4. **Billing in Stripe test mode.** Checkout, Customer Portal, all four webhooks with idempotency and out-of-order handling, entitlement enforcement, dunning emails.
5. **Charge real money**, only after this list is complete:
   - [ ] Per-run sandbox in production, with tests for each limit.
   - [ ] GitHub App with read-only permissions, no stored long-lived tokens.
   - [ ] Postgres with backups and a restore that was actually tested.
   - [ ] Quota enforcement under concurrent requests.
   - [ ] Stripe webhooks verified, idempotent, replay-tested in test mode.
   - [ ] Privacy policy, terms, subprocessor list, retention job running.
   - [ ] An external security review of the sandbox and the auth flows.
   - [ ] Prices and limits decided by the owner and written down.
