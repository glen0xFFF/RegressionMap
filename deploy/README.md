# Deploying RegressionMap to glenox-server

Target: `glenox-server` (Ubuntu 24.04, Docker 29). Coolify already runs there. Its Traefik v3 container `coolify-proxy` owns ports 80/443 and is attached to the external Docker network `coolify`. Public hostname: `https://regressionmap.glenox.net`, proxied by Cloudflare with SSL mode "Full (strict)".

RegressionMap runs as one container, `regressionmap`. It is managed with Docker Compose from a git checkout in `/opt/regressionmap`, not as a Coolify resource. Do not also create a Coolify resource for the same hostname.

| File | Purpose |
| --- | --- |
| `deploy/Dockerfile` | Three stages: web build (node 24), Go build (static binaries), runtime (node 24 slim + git + tini, uid 10001). |
| `deploy/docker-compose.yml` | Service, hardening, volumes, Traefik labels. No host ports. |
| `deploy/entrypoint.sh` | Creates the demo fixture on first start, runs `keys ...`, or starts the server. |
| `deploy/.env.example` | Template for `deploy/.env`, which holds `MODEL_API_KEY` and, optionally, `WEBHOOK_SECRET`. |
| `.dockerignore` | Keeps `.git`, `node_modules`, `.data`, `demo-workspace`, `.env` and key files out of the build context. |

All commands below run on the server, from `/opt/regressionmap`, as a user in the `docker` group, unless the step says "workstation".

## 1. Get the code

First time:

```sh
sudo mkdir -p /opt/regressionmap
sudo chown "$USER": /opt/regressionmap
git clone <repository-url> /opt/regressionmap
cd /opt/regressionmap
```

Later (see also "Update"):

```sh
cd /opt/regressionmap
git pull --ff-only
```

## 2. Secrets: create deploy/.env (chmod 600)

`deploy/.env` holds `MODEL_API_KEY` and, optionally, `WEBHOOK_SECRET` (see "Enable webhooks" below). It is copied as a file with `scp`. Its values are never typed into a server command, never echoed, and never passed with `-e`. Never put them in `docker-compose.yml`: that file is committed.

On the **workstation**, build the env file from your key file, with permissions 600, without printing the key:

```sh
# POSIX shell (Git Bash, macOS, Linux). printf is a shell builtin, so the key is not in the process list.
umask 077
printf 'MODEL_API_KEY=%s\n' "$(tr -d '\r\n' < /path/to/your-model-key.txt)" > regressionmap.env
```

```powershell
# PowerShell
"MODEL_API_KEY=" + (Get-Content -Raw C:\path\to\your-model-key.txt).Trim() | Set-Content -NoNewline -Encoding ascii regressionmap.env
```

**Enable webhooks (optional).** Without `WEBHOOK_SECRET` the server answers every `notify_url` with `400 webhooks_disabled`. To enable completion webhooks, add a second line to the same file before you copy it.

- The secret must be at least 32 characters (Unicode characters, not bytes). A shorter one stops the server at startup with `WEBHOOK_SECRET must be at least 32 characters (it is the HMAC key of webhook signatures); unset it to disable webhooks`. With `restart: unless-stopped` the container then restarts in a loop, and the UI and API are down.
- It is used exactly as written: no quotes, no spaces. `openssl rand -hex 32` gives 64 hex characters.
- Receivers need the same value to check signatures. It is one secret for the whole server, so give it only to receivers you trust (see "CI and notifications" in the root `README.md`).
- Keep `webhook-secret.txt` where you keep the model key file. Every later replacement of `deploy/.env` needs it again.
- After the start (step 5), the startup log line shows `"webhooks_enabled":true`. The command is in step 7.

```sh
# POSIX shell, after the MODEL_API_KEY line above. Prints nothing.
umask 077
openssl rand -hex 32 > webhook-secret.txt
printf 'WEBHOOK_SECRET=%s\n' "$(tr -d '\r\n' < webhook-secret.txt)" >> regressionmap.env
```

```powershell
# PowerShell, after the MODEL_API_KEY line above (make webhook-secret.txt with openssl rand -hex 32, for example in Git Bash)
Add-Content -NoNewline -Encoding ascii regressionmap.env ("`nWEBHOOK_SECRET=" + (Get-Content -Raw C:\path\to\webhook-secret.txt).Trim())
```

Still on the **workstation**, copy it and delete the local copy:

```sh
scp regressionmap.env glenox-server:/opt/regressionmap/deploy/.env
rm regressionmap.env          # PowerShell: Remove-Item regressionmap.env
```

On the **server**, fix the permissions and check the file without printing the value:

```sh
chmod 600 deploy/.env
stat -c '%a %U %n' deploy/.env               # expect: 600 <you> deploy/.env
grep -c '^MODEL_API_KEY=.' deploy/.env       # expect: 1 (prints a count, not the key)
LC_ALL=C grep -c '^WEBHOOK_SECRET=.\{32,\}$' deploy/.env   # webhooks only; expect: 1 (a count, not the secret)
```

The `WEBHOOK_SECRET` check counts bytes (`LC_ALL=C`), while the server counts Unicode characters. For the ASCII hex secret that `openssl rand -hex 32` makes, bytes and characters are the same, so the check agrees with the server. For a secret with non-ASCII characters it does not: such a secret of fewer than 32 characters can still print 1, and the server then refuses to start.

`deploy/.env` is ignored by git (`.gitignore`: `.env`) and by the Docker build (`.dockerignore`). To run without the model step, copy `deploy/.env.example` to `deploy/.env` and leave the value empty. Compose refuses to start without the file.

**Rotate the webhook secret.** There is one secret at a time. Make a new `webhook-secret.txt` and give the new value to every receiver; a receiver can accept either value while you switch. Then replace `deploy/.env` as above, with both lines, and recreate the container as in "Rotate the model key" (step 7). Deliveries after the restart are signed with the new value. A receiver that checks only the old value rejects them.

## 3. Build

```sh
docker network inspect coolify --format '{{.Name}}'   # must print: coolify
docker compose -f deploy/docker-compose.yml build
docker image tag regressionmap:local "regressionmap:$(git rev-parse --short HEAD)"
```

## 4. Create the admin key BEFORE the first start

With `REGRESSIONMAP_ALLOW_REMOTE=1` (set in the image), the server refuses to start unless `DATA_DIR` is set and at least one active `admin` key exists. Create it with a one-off container. It uses the same `regressionmap-data` volume:

```sh
docker compose -f deploy/docker-compose.yml run --rm -l traefik.enable=false \
  regressionmap keys create --name admin --scope admin
```

The secret (`rm_live_` + 32 characters) is printed **once** on stdout. Put it into your password manager right away. Do not redirect it into a file on the server. Only its SHA-256 hash is stored, in `/data/keys.json` (mode 0600). `-l traefik.enable=false` keeps Traefik from routing traffic to the one-off container.

## 5. Start

```sh
docker compose -f deploy/docker-compose.yml up -d
docker compose -f deploy/docker-compose.yml ps                       # STATUS should become "(healthy)"
docker inspect --format '{{.State.Health.Status}}' regressionmap     # expect: healthy
```

On the first start the entrypoint creates the demo repository `/workspace/pagination-demo` in the `regressionmap-workspace` volume.

Traefik only routes to the container once it is healthy (the image healthcheck polls every 2 s during the first 30 s).

## 6. Verify

```sh
curl -sI https://regressionmap.glenox.net
curl https://regressionmap.glenox.net/api/health
```

Expected:

- `curl -sI` prints `HTTP/2 200` (the UI), with the security headers (`content-security-policy`, ...). Cloudflare adds its own headers.
- `/api/health` returns `{"ok":true,...,"auth_required":false}`, because `docker-compose.yml` sets `PUBLIC_DEMO=1` (see "Public demo mode" in step 7). Without `PUBLIC_DEMO=1`, `auth_required` is true, because requests through Traefik are not local.
- Without a key, the API answers as the public demo identity. A bad key is still refused, and key management stays closed:

```sh
curl -s -o /dev/null -w '%{http_code}\n' https://regressionmap.glenox.net/api/repos    # expect: 200 (401 without PUBLIC_DEMO=1)
curl -s -o /dev/null -w '%{http_code}\n' -H 'Authorization: Bearer rm_live_invalid' https://regressionmap.glenox.net/api/repos   # expect: 401
curl -s -o /dev/null -w '%{http_code}\n' https://regressionmap.glenox.net/api/keys     # expect: 403 (401 without PUBLIC_DEMO=1)
```

- With a key (read it from a file or a prompt, not from the command line):

```sh
read -rs RM_KEY && curl -s -H "Authorization: Bearer $RM_KEY" https://regressionmap.glenox.net/api/me; unset RM_KEY
```

- Plain HTTP redirects to HTTPS: `curl -sI http://regressionmap.glenox.net` shows a redirect to `https://` (301 from Cloudflare "Always Use HTTPS", or 302 from the `regressionmap-redirect` middleware).

If the certificate is not issued, check the proxy log: `docker logs coolify-proxy 2>&1 | grep -i -E 'acme|regressionmap'`. With Cloudflare proxying, an HTTP-01 challenge fails when Cloudflare rewrites `http://` to `https://` before it reaches Traefik. Fixes: turn off "Always Use HTTPS" for `/.well-known/acme-challenge/*`, or switch the record to "DNS only" until the first certificate is issued, then back to "Proxied". I could not check which challenge type the `letsencrypt` resolver in coolify-proxy uses.

## 7. API keys: list, create, rotate, revoke

While the server runs, use `exec` (same `DATA_DIR`, same user):

```sh
docker compose -f deploy/docker-compose.yml exec regressionmap /app/regressionmap keys list
docker compose -f deploy/docker-compose.yml exec regressionmap /app/regressionmap keys create --name ci --scope analyze
docker compose -f deploy/docker-compose.yml exec regressionmap /app/regressionmap keys revoke k_xxxxxxxxxx
```

With an admin key, the same can be done over HTTPS: `GET/POST /api/keys`, `DELETE /api/keys/{id}` (see `docs/API_CONTRACT.md`, "Authentication and API keys (v2)").

- `analyze` keys can use every `/api/*` route except `/api/keys*`. Give these to people and CI.
- `admin` keys can do everything. Keep one, in a password manager.
- Rotate a key: create the new key, switch the client to it, then `keys revoke` the old id. Revoking is permanent.
- Rotate the admin key the same way. Keep at least one active admin key, or the server will not start after the next restart.
- `POST /api/analyses` is limited to `RATE_LIMIT_PER_HOUR` (20, set in `docker-compose.yml`) requests per key in a sliding hour. Requests the server rejects (`400`, `409`, `429 too_many_analyses`) count too.

Rotate the **model key**: replace `deploy/.env` as in step 2, then:

```sh
docker compose -f deploy/docker-compose.yml up -d --force-recreate
```

The file is replaced whole. If webhooks are on, add the `WEBHOOK_SECRET` line again. Otherwise the server restarts with webhooks disabled, and every request with a `notify_url` gets `400 webhooks_disabled`. After the restart, check the startup log line:

```sh
docker compose -f deploy/docker-compose.yml logs regressionmap | grep -o '"webhooks_enabled":[a-z]*' | tail -n 1   # expect: "webhooks_enabled":true
```

### Public demo mode (`PUBLIC_DEMO`)

`docker-compose.yml` sets `PUBLIC_DEMO=1`, so the hosted demo needs no sign-in. A request to `/api/*` without an `Authorization` header runs as the public demo identity, which has the `analyze` scope: it can list repositories, commits and commands, start and read analyses, and run verifications. `GET /api/health` reports `"auth_required":false`, `GET /api/me` answers `{"mode":"public"}`, and the UI shows no sign-in card, no API Keys page and no Sign out. A request that sends a key is checked as before, so a bad or revoked key still gets `401`. `/api/keys*` answers the public identity with `403 forbidden_scope`: key management still needs an admin key, or `docker exec` as above.

**Cost.** Anyone who can reach `https://regressionmap.glenox.net` can start analyses, and each analysis makes up to three model calls with the server's `MODEL_API_KEY`. Only these limits bound that cost:

- one rate-limit bucket that every request without a key shares: `RATE_LIMIT_PER_HOUR` (20) `POST /api/analyses` per sliding hour, then `429 rate_limited` with `Retry-After`. Requests the server rejects count too. Each key keeps its own bucket, so key holders are not blocked when the public bucket is used up;
- the job limits: one analysis or verification per repository at a time, and at most two analyses running at once.

Verifications run the allowlisted test command in the container. They make no model call and are not rate limited.

**Check.** The startup log line carries `"public_demo":true`, after a `WARNING: PUBLIC_DEMO=1` line:

```sh
docker compose -f deploy/docker-compose.yml logs regressionmap | grep -o '"public_demo":[a-z]*' | tail -n 1   # expect: "public_demo":true
```

**Turn it off.** Remove the `- PUBLIC_DEMO=1` line from `deploy/docker-compose.yml`, then:

```sh
docker compose -f deploy/docker-compose.yml up -d
```

Compose recreates the container because its environment changed. The log then shows `"public_demo":false`, `/api/health` reports `"auth_required":true`, and `/api/repos` without a key gets `401` again. Any value other than `1` also leaves the mode off. Commit the removal to the repository as well, so that the next update (step 8) does not bring the line back.

## 8. Update

```sh
cd /opt/regressionmap
git rev-parse HEAD > "$HOME/.regressionmap.prev-sha"   # the commit you can return to
cat "$HOME/.regressionmap.prev-sha"                      # must print a 40-character SHA; stop here if not
git pull --ff-only
docker image tag regressionmap:local regressionmap:prev # keep the running image for rollback
docker compose -f deploy/docker-compose.yml build
docker image tag regressionmap:local "regressionmap:$(git rev-parse --short HEAD)"
docker compose -f deploy/docker-compose.yml up -d
docker inspect --format '{{.State.Health.Status}}' regressionmap
curl https://regressionmap.glenox.net/api/health
```

The previous SHA goes to `$HOME/.regressionmap.prev-sha`, not next to the checkout: `/opt` is owned by root, and only `/opt/regressionmap` was handed to you in step 1. Run the update and the rollback as the same user, so both use the same `$HOME`.

The volumes (`regressionmap-data` with keys and saved analyses, `regressionmap-workspace` with the demo repository) are kept across updates.

## 9. Rollback

```sh
cd /opt/regressionmap
PREV_SHA="$(cat "$HOME/.regressionmap.prev-sha")"
test -n "$PREV_SHA" && git checkout "$PREV_SHA"          # compose file that matches the old image
git rev-parse HEAD                                       # must equal $PREV_SHA; stop here if not
docker image tag regressionmap:prev regressionmap:local
docker compose -f deploy/docker-compose.yml up -d --no-build --force-recreate
docker inspect --format '{{.State.Health.Status}}' regressionmap
```

Return to `main` later with `git checkout main && git pull --ff-only`. Every build is also tagged `regressionmap:<short-sha>` (`docker image ls regressionmap`). To roll back further, tag that image as `regressionmap:local` and run the same `up` command.

**Never roll back to an image older than the v2 authentication change.** Such an image has no API keys. With `REGRESSIONMAP_ALLOW_REMOTE=1` it would serve the API on the internet without authentication. If v2 itself is broken, stop the service instead: `docker compose -f deploy/docker-compose.yml stop`.

## 10. Logs and status

```sh
docker compose -f deploy/docker-compose.yml logs -f --tail=200       # JSON lines (LOG_FORMAT=json)
docker compose -f deploy/docker-compose.yml ps
docker inspect --format '{{json .State.Health}}' regressionmap
docker stats --no-stream regressionmap
```

The access log records method, path, status and duration. It never records headers or bodies, so bearer keys do not reach the log. Docker keeps at most 3 × 10 MB of logs (`json-file`, `max-size`/`max-file`).

Every line the server logs is JSON. The exceptions are these plain-text lines:

- `regressionmap: <error>`, which the server prints when it cannot start (for example after a too-short `WEBHOOK_SECRET`) or exits with an error;
- the entrypoint's own `entrypoint: …` lines;
- on a start where `/workspace/pagination-demo` does not exist yet (the first start with a new volume), the four lines of the demo fixture builder that the entrypoint runs: `created /workspace/pagination-demo`, `baseline <sha>`, `refactor <sha>` and `bug      <sha>`. If the builder fails, it prints its error as plain text instead and the container exits.

In the JSON lines, the message and every string value of an attribute have each character from U+0080 to U+009F, each Unicode format character (category Cf, for example U+202E) and U+2028/U+2029 replaced by the six-character text `\uXXXX` (four lowercase hex digits). A format character above U+FFFF, for example U+E0041, becomes its UTF-16 surrogate pair, two such texts (`\udb40\udc41`). The JSON encoder then escapes the backslash, so the raw line shows `\\u202e` and a JSON parser reads the six characters `\u202e`, not U+202E. An error value is replaced by its escaped error text only when that text holds such a character; otherwise it is written as before, as its error text or, for an error type with its own JSON encoding, as that encoding. A string inside a group, or one that a LogValuer returns, is escaped the same way. Values of other kinds, such as lists, maps and structs, are written as before. The JSON encoder already escapes U+0000 to U+001F.

A failed webhook delivery is a `WARN` line with the message `notification failed` and the fields `analysis`, `host`, `attempts`, `status_code` and `error`. It never holds the URL path or query.

**Stop.** `docker compose ... stop`, `up -d` after an update, and `--force-recreate` send SIGTERM. The server then waits up to 10 s for open requests, ends every running analysis with status `error` (`analysis interrupted: the server stopped while it was running`), and waits up to 25 s more for running analyses and webhooks still being sent. The log shows `shutting down` and `draining`, then `drained`. If the 25 s run out, it shows the warning `drain incomplete` instead, with an `error` field that counts what was still open, for example `1 analysis run(s) still stopping, 1 notification(s) still being sent: context deadline exceeded`, and a `timeout` field (`25s`). `stop_grace_period: 40s` gives it that time before Docker kills the container. An interrupted analysis is not resumed: start it again after the restart. A webhook that could not finish shows `failed` after the restart, with `delivery interrupted: the server stopped before it finished`, and is never retried.

Backup of keys and saved analyses (`keys.json` holds hashes only, not secrets):

```sh
docker run --rm -v regressionmap-data:/data:ro -v "$PWD":/backup debian:bookworm-slim \
  tar czf /backup/regressionmap-data.tgz -C /data .
```

## Security model

**Network.** No port is published on the host (`ports:` is absent). The only way in is Cloudflare → `coolify-proxy` (TLS, Let's Encrypt certificate, so "Full (strict)" works) → the `coolify` network → container port 8080. A host firewall change is not needed, and a misconfigured firewall cannot expose the API directly. Other containers on the `coolify` network can reach `regressionmap:8080`. They are not local callers (their peer address is not loopback), so they need a key like everyone else.

**Authentication.** Requests through Traefik carry `X-Forwarded-For` and a non-loopback peer, so they are never "local": every `/api/*` call except `GET /api/health` needs `Authorization: Bearer rm_live_...`. With `PUBLIC_DEMO=1`, which this deployment sets, a call without that header runs as the public demo identity (`analyze` scope) instead; see "Public demo mode" in step 7. Keys are stored as SHA-256 hashes and compared in constant time. `ALLOWED_HOSTS=regressionmap.glenox.net` is the only extra name accepted in Host/Origin, which keeps DNS-rebinding and cross-site protection on. Only processes inside the container (the healthcheck, `docker exec`) are local, and whoever can run `docker exec` already controls the host.

**Container hardening.**

- Runs as uid/gid 10001 (`app`), never root. Binaries and UI files are owned by root and read-only for the app user.
- `read_only: true`: the root filesystem cannot be written. Writable places are only `/data` and `/workspace` (named volumes) and `/tmp` (tmpfs, 256 MB, `nosuid,nodev,noexec`), where git worktrees and test runs go.
- `cap_drop: [ALL]` and `no-new-privileges:true`: no Linux capabilities, no privilege gain through setuid binaries.
- `pids_limit: 256`, `mem_limit: 1g`, `cpus: 1.0`: a runaway test or fork bomb cannot take the host down. Each test run also has a timeout and an output cap.
- `tini` is PID 1: signals are forwarded (graceful shutdown on `docker stop`, within `stop_grace_period: 40s`; see step 10) and zombie processes from test runs are reaped.
- No secrets in the image. `MODEL_API_KEY` and, when set, `WEBHOOK_SECRET` come from `deploy/.env` (mode 600) at run time. They are visible to `docker inspect`, so only to people with Docker access (who are root-equivalent anyway).
- Child environment. Test runs, verification runs and git never receive `MODEL_API_KEY`, `MODEL_API_KEY_FILE`, `WEBHOOK_SECRET`, or any variable whose name ends in `_KEY`, `_SECRET`, `_TOKEN` or `_PASSWORD`, in any letter case. `TEST_ENV_ALLOW` can let named variables through to test and verification runs, never to git and never the three server secrets. This deployment does not set it: the demo tests need no credentials. The startup log line carries `test_env_withheld`, the names (never the values) of the variables test runs do not get. This is defence in depth, not a boundary: see "Demo-only workspace".
- git configuration is not weakened. There is no `safe.directory '*'`. The workspace repositories are owned by uid 10001, the same user that runs git. The entrypoint warns if a repository has a different owner.

**Demo-only workspace.** The API runs `node --test` (the only allowlisted command) on repositories in `/workspace`. Test code is arbitrary code: it runs as the same user as the server and could read what that user can read, including `/data` and the server's own process environment. Therefore `/workspace` holds only the generated `pagination-demo` fixture. Do not mount real repositories, host directories or the Docker socket into this container. The API has no endpoint to upload or change code. An analyze key, and the public demo identity, can only start analyses and verifications of repositories that already exist there. The container has outbound internet access, because the model call goes to `https://api.meta.ai`.

## What was not verified

These files were written and checked statically (`sh -n` on the entrypoint, label and variable review). They were not built or run: Docker was not available where they were written. Before relying on them, check on the server:

- That the base image tags exist: `node:24-bookworm-slim`, `golang:1.27-bookworm`. If `golang:1.27-bookworm` is missing, use `golang:1.27` or `golang:1-bookworm` (it must be Go 1.27 or newer; `GOTOOLCHAIN=local` stops the build otherwise).
- That the v2 server (`ALLOWED_HOSTS`, `RATE_LIMIT_PER_HOUR`, `keys` subcommand, admin-key check at startup) is in the commit you build. The names follow `docs/API_CONTRACT.md`.
- How the `letsencrypt` resolver of coolify-proxy issues certificates behind Cloudflare (see step 6).
- That a `WEBHOOK_SECRET` line in `deploy/.env` reaches the server through `env_file`: the startup log should show `"webhooks_enabled":true` (step 7).
- That `PUBLIC_DEMO=1` in the environment list reaches the server: the startup log should show `"public_demo":true`, and a request to `/api/repos` without a key should get `200` (steps 6 and 7).
- That `stop_grace_period: 40s` lets the drain finish: after `docker compose -f deploy/docker-compose.yml stop`, the log should show `drained`, or the warning `drain incomplete` with its `error` and `timeout` fields (see "Stop" in step 10), before the container exits.
