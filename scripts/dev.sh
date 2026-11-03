#!/usr/bin/env bash
# Development mode: Go API on 127.0.0.1:8787 (go run) + Vite on :5173.
#
#   scripts/dev.sh
#
# WORKSPACE_ROOT defaults to ./demo-workspace; the demo fixture is created
# there if it is missing. The model key is read by the backend only from
# MODEL_API_KEY or MODEL_API_KEY_FILE in YOUR environment; this script never
# reads, prints or stores it. Without a key the model step shows "disabled".
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ -z "${WORKSPACE_ROOT:-}" ]; then
  export WORKSPACE_ROOT="$REPO/demo-workspace"
  if [ ! -d "$WORKSPACE_ROOT/pagination-demo" ]; then
    echo "creating demo fixture in $WORKSPACE_ROOT"
    (cd "$REPO/backend" && go run ./cmd/mkfixture "$WORKSPACE_ROOT")
  fi
fi

if [ -n "${MODEL_API_KEY:-}" ] || [ -n "${MODEL_API_KEY_FILE:-}" ]; then
  echo "model key: provided via environment"
else
  echo "model key: not set (export MODEL_API_KEY or MODEL_API_KEY_FILE to enable hypotheses)"
fi

if [ ! -d "$REPO/web/node_modules" ]; then
  (cd "$REPO/web" && npm ci)
fi

# Run the backend in its own process group so it (and the binary `go run`
# starts) can be stopped together.
set -m
(cd "$REPO/backend" && exec go run ./cmd/server) &
BACKEND_PID=$!
set +m

cleanup() {
  kill -TERM -- "-$BACKEND_PID" 2>/dev/null || kill -TERM "$BACKEND_PID" 2>/dev/null || true
  wait "$BACKEND_PID" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "API  http://127.0.0.1:8787   workspace=$WORKSPACE_ROOT"
echo "UI   http://localhost:5173"
cd "$REPO/web"
npm run dev
