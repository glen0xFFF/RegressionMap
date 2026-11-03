#!/bin/sh
# RegressionMap container entrypoint (runs under tini as uid 10001).
#
#   (no arguments)      start the server
#   keys create|list|revoke ...
#                       run the API key CLI against DATA_DIR and exit
#
# Nothing here writes git config: safe.directory is not weakened. git accepts
# the repositories because they are owned by the user running the server.
set -eu

uid="$(id -u)"

# API key CLI: does not touch the workspace. Its stdout carries the secret,
# so nothing else is printed to stdout before it.
if [ "$#" -gt 0 ] && [ "$1" = "keys" ]; then
  exec /app/regressionmap "$@"
fi

if [ "$#" -gt 0 ]; then
  echo "entrypoint: unknown command '$1' (use no arguments, or: keys create|list|revoke ...)" >&2
  exit 2
fi

if [ ! -w /workspace ]; then
  echo "entrypoint: /workspace is not writable by uid $uid; the volume must be owned by 10001:10001" >&2
  exit 1
fi

# Demo-only workspace: create the fixture on first start.
if [ ! -d /workspace/pagination-demo ]; then
  echo "entrypoint: creating demo fixture in /workspace" >&2
  /app/mkfixture /workspace >&2
fi

# git refuses repositories owned by another uid ("dubious ownership").
# Report that clearly instead of failing later inside an analysis.
for d in /workspace/*/; do
  [ -d "$d" ] || continue
  owner="$(stat -c %u "$d")"
  if [ "$owner" != "$uid" ]; then
    echo "entrypoint: WARNING $d is owned by uid $owner, not $uid; git will refuse it. Fix the volume ownership (chown -R 10001:10001)." >&2
  fi
done

exec /app/regressionmap
