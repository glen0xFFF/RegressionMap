# syntax=docker/dockerfile:1
#
# RegressionMap production image. Build context is the repository root:
#
#   docker compose -f deploy/docker-compose.yml build
#   (or: docker build -f deploy/Dockerfile -t regressionmap:local .)
#
# No secret is baked into the image. MODEL_API_KEY comes from deploy/.env at
# run time; API keys live in the /data volume.

# ---------------------------------------------------------------- web (UI)
FROM node:24-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

# ---------------------------------------------------------- go (binaries)
# go.mod declares go 1.27.0. GOTOOLCHAIN=local makes the build fail instead
# of downloading a toolchain if this tag ever resolves to an older Go.
FROM golang:1.27-bookworm AS go
WORKDIR /src/backend
COPY backend/go.mod ./
COPY backend/ ./
RUN GOTOOLCHAIN=local CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/regressionmap ./cmd/server \
 && GOTOOLCHAIN=local CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/mkfixture ./cmd/mkfixture

# ---------------------------------------------------------------- runtime
# Node is required at run time: the allowlisted test command is `node --test`.
FROM node:24-bookworm-slim AS runtime

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates tini \
 && rm -rf /var/lib/apt/lists/*

# Non-root user. /data and /workspace are owned by it so that fresh named
# volumes inherit that ownership (git refuses repositories owned by another
# user, and we do not weaken safe.directory).
RUN groupadd --system --gid 10001 app \
 && useradd --system --uid 10001 --gid 10001 --home-dir /home/app --create-home --shell /usr/sbin/nologin app \
 && mkdir -p /app/web /data /workspace \
 && chown 10001:10001 /data /workspace \
 && chmod 0700 /data \
 && chmod 0755 /workspace

# Binaries and UI are owned by root and not writable by the app user.
COPY --from=go /out/regressionmap /app/regressionmap
COPY --from=go /out/mkfixture /app/mkfixture
COPY --from=web /src/web/dist/ /app/web/
COPY --chmod=0755 deploy/entrypoint.sh /app/entrypoint.sh

ENV WEB_DIR=/app/web \
    DATA_DIR=/data \
    WORKSPACE_ROOT=/workspace \
    ADDR=0.0.0.0:8080 \
    REGRESSIONMAP_ALLOW_REMOTE=1 \
    LOG_FORMAT=json \
    HOME=/home/app

USER 10001:10001
WORKDIR /app
EXPOSE 8080
VOLUME ["/data", "/workspace"]

# Local request (loopback peer, Host 127.0.0.1, no X-Forwarded-For); exits 1
# on any non-2xx status, network error or after 4 s.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --start-interval=2s --retries=3 \
  CMD ["node", "-e", "fetch('http://127.0.0.1:8080/api/health',{headers:{Host:'127.0.0.1'},signal:AbortSignal.timeout(4000)}).then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"]

ENTRYPOINT ["tini", "--", "/app/entrypoint.sh"]
