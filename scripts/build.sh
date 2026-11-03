#!/usr/bin/env bash
# Production build: web/dist + a single Go binary that serves the API and UI.
#
#   scripts/build.sh
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "== web: npm ci && npm run build"
(cd "$REPO/web" && npm ci && npm run build)

echo "== backend: go build"
EXE="$(cd "$REPO/backend" && go env GOEXE)"
BIN="$REPO/backend/bin/regressionmap$EXE"
(cd "$REPO/backend" && go build -trimpath -o "$BIN" ./cmd/server)

cat <<EOF

Built:
  $REPO/web/dist
  $BIN

Run it (UI and API on one port, http://127.0.0.1:8787):
  WORKSPACE_ROOT=$REPO/demo-workspace WEB_DIR=$REPO/web/dist $BIN

Set MODEL_API_KEY or MODEL_API_KEY_FILE in your environment to enable the
model step. Optional: LOG_FORMAT=json, ADDR=127.0.0.1:PORT, DATA_DIR=...
EOF
