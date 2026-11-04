#!/bin/sh
# Start a RegressionMap analysis from CI, wait for it, and print a markdown
# summary. POSIX sh; needs curl and node (both are on GitHub-hosted runners).
#
#   REGRESSIONMAP_URL=https://regressionmap.example.com \
#   REGRESSIONMAP_KEY=rm_live_...   (an "analyze" key, from a CI secret) \
#   REPO=my-repo BASELINE=<good sha> sh scripts/ci-analyze.sh
#
# Environment:
#   REGRESSIONMAP_URL  required. https://, or http:// for 127.0.0.1/localhost/[::1] only.
#   REGRESSIONMAP_KEY  required. Read from the environment only. It is written to a
#                      private header file for curl and never put on a command line
#                      or printed.
#   REPO               required. Repository name under the server's WORKSPACE_ROOT.
#   BASELINE           required. Known-good commit (full or short SHA).
#   COMMAND_ID         allowlisted test command (default node-test).
#   NOTIFY_URL         optional https:// webhook for the completion notification.
#                      Treat it as a secret: it is never printed.
#   NOTIFY_FORMAT      optional: json (server default) or slack.
#   TIMEOUT_SECONDS    how long to wait (default 600).
#   POLL_SECONDS       poll interval (default 3).
#
# The summary goes to stdout and is appended to $GITHUB_STEP_SUMMARY when set.
#
# Exit codes:
#   0  analysis finished with status "done" (the tests may still fail; read the summary)
#   1  usage error (missing input, missing curl/node)
#   2  analysis finished with status "error"
#   3  timed out waiting (the analysis keeps running on the server)
#   4  HTTP, network or auth error (the API error code and message are printed)
set -eu
set +x # tracing would print the Authorization header line

say() { printf 'ci-analyze: %s\n' "$*" >&2; }
usage_error() { say "$*"; exit 1; }

[ -n "${REGRESSIONMAP_URL:-}" ] || usage_error "REGRESSIONMAP_URL is required"
[ -n "${REGRESSIONMAP_KEY:-}" ] || usage_error "REGRESSIONMAP_KEY is required (set it from a CI secret)"
[ -n "${REPO:-}" ] || usage_error "REPO is required"
[ -n "${BASELINE:-}" ] || usage_error "BASELINE is required"
COMMAND_ID="${COMMAND_ID:-node-test}"
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-600}"
POLL_SECONDS="${POLL_SECONDS:-3}"
export REPO BASELINE COMMAND_ID

case "$TIMEOUT_SECONDS" in '' | *[!0-9]*) usage_error "TIMEOUT_SECONDS must be a whole number" ;; esac
case "$POLL_SECONDS" in '' | *[!0-9]* | 0) usage_error "POLL_SECONDS must be a whole number above 0" ;; esac
command -v curl >/dev/null 2>&1 || usage_error "curl is required"
command -v node >/dev/null 2>&1 || usage_error "node is required"

base="${REGRESSIONMAP_URL%/}"
case "$base" in
  https://*) ;;
  http://127.0.0.1 | http://127.0.0.1:* | http://127.0.0.1/* | \
  http://localhost | http://localhost:* | http://localhost/* | \
  http://\[::1\] | http://\[::1\]:* | http://\[::1\]/*) ;;
  *) usage_error "REGRESSIONMAP_URL must start with https:// (plain http:// is accepted for 127.0.0.1, localhost and [::1] only)" ;;
esac

# Private scratch directory: files below are created mode 0600 and removed on exit.
umask 077
tmp="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/regressionmap.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

hdr="$tmp/auth.header"
printf 'Authorization: Bearer %s\n' "$REGRESSIONMAP_KEY" >"$hdr" # printf is a shell builtin: no argv
unset REGRESSIONMAP_KEY # child processes (curl, node) do not inherit it

helper="$tmp/helper.js"
cat >"$helper" <<'NODE'
'use strict';
const fs = require('fs');
const [cmd, ...args] = process.argv.slice(2);

// Secrets that must never be printed, even if a server echoed them.
const secrets = [];
try {
  const m = /^Authorization: Bearer (.+)$/m.exec(fs.readFileSync(process.env.RM_HDR, 'utf8'));
  if (m) secrets.push(m[1].trim());
} catch (e) { /* no header file */ }
try {
  const r = JSON.parse(fs.readFileSync(process.env.RM_REQ, 'utf8'));
  if (r.notify_url) secrets.push(r.notify_url);
} catch (e) { /* no request yet */ }
function redact(s) {
  let out = String(s);
  for (const x of secrets) if (x) out = out.split(x).join('[redacted]');
  return out;
}
// Model output is untrusted text: one line, no HTML, no code spans, bounded length.
function md(s, max = 300) {
  let t = redact(s == null ? '' : s).replace(/[\r\n\t]+/g, ' ').replace(/\s+/g, ' ').trim();
  t = t.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/`/g, "'");
  return t.length > max ? t.slice(0, max - 1) + '…' : t;
}
function readJSON(file) {
  try { return JSON.parse(fs.readFileSync(file, 'utf8')); } catch (e) { return null; }
}

if (cmd === 'request') {
  const e = process.env;
  const o = { repo: e.REPO, baseline: e.BASELINE, command_id: e.COMMAND_ID };
  if (e.NOTIFY_URL) {
    o.notify_url = e.NOTIFY_URL;
    if (e.NOTIFY_FORMAT) o.notify_format = e.NOTIFY_FORMAT;
  }
  fs.writeFileSync(args[0], JSON.stringify(o));
} else if (cmd === 'error') {
  // error HTTP_CODE BODY_FILE HEADERS_FILE
  const [code, bodyFile, headFile] = args;
  const body = readJSON(bodyFile);
  const err = body && body.error ? body.error : {};
  let line = 'HTTP ' + code + ' ' + md(err.code || 'error', 80);
  if (err.message) line += ': ' + md(err.message);
  console.log('ci-analyze: ' + line);
  if (code === '429') {
    let ra = '';
    try {
      const m = /^retry-after:\s*(.+?)\s*$/im.exec(fs.readFileSync(headFile, 'utf8'));
      if (m) ra = m[1];
    } catch (e) { /* no headers */ }
    console.log('ci-analyze: Retry-After: ' + (ra ? md(ra, 40) + ' s' : 'not given'));
  }
} else if (cmd === 'field') {
  // field FILE id|status  -> validated value, exit 1 when invalid
  const a = readJSON(args[0]) || {};
  const v = a[args[1]];
  const ok = args[1] === 'id' ? /^a[0-9a-z-]{1,40}$/.test(v || '')
    : ['running', 'done', 'error'].includes(v);
  if (!ok) process.exit(1);
  process.stdout.write(v);
} else if (cmd === 'summary') {
  const a = readJSON(args[0]) || {};
  const short = (s) => md(String(s || '').slice(0, 7), 7);
  const run = (r) => {
    if (!r) return 'not run';
    if (r.timed_out) return 'FAIL (timed out)';
    return (r.exit_code === 0 ? 'PASS' : 'FAIL') + ' (exit ' + r.exit_code + ')';
  };
  const loc = (h) => {
    const f = md(h.file, 200);
    if (!f) return '';
    if (!(h.line_start > 0)) return '`' + f + '`';
    if (h.line_end > h.line_start) return '`' + f + ':' + h.line_start + '-' + h.line_end + '`';
    return '`' + f + ':' + h.line_start + '`';
  };
  const ids = (xs) => (Array.isArray(xs) ? xs : []).map((x) => md(x, 12)).join(', ');
  const o = [];
  o.push('## RegressionMap analysis', '');
  o.push('- Repository: `' + md(a.repo, 100) + '`');
  o.push('- Analysis: `' + md(a.id, 60) + '`');
  o.push('- Status: **' + md(a.status, 20) + '**');
  if (a.status === 'error') o.push('- Error: ' + md(a.error, 500));
  o.push('- Baseline `' + short(a.baseline) + '`: ' + run(a.baseline_run));
  o.push('- HEAD `' + short(a.current) + '`: ' + run(a.current_run));
  const m = a.model || null;
  if (m) {
    o.push('- Model: ' + md(m.status, 20) + (m.model_id ? ' (`' + md(m.model_id, 60) + '`)' : '') +
      (m.message ? ' — ' + md(m.message) : ''));
  }
  const n = a.notification;
  if (n) {
    o.push('- Notification: ' + md(n.status, 20) + ' to ' + md(n.host, 120) + ' (' + md(n.format, 10) + ', ' +
      Number(n.attempts || 0) + ' attempt(s)' + (n.last_error ? ', ' + md(n.last_error, 200) : '') + ')');
  }
  if (m) {
    o.push('', '### AI-generated hypotheses — not verified', '');
    o.push('The test runner decides PASS or FAIL. These are ranked guesses that cite evidence IDs from the analysis; check them before acting.', '');
    const hs = m.status === 'ok' && m.result && Array.isArray(m.result.hypotheses) ? m.result.hypotheses : [];
    if (hs.length === 0) o.push('No valid hypotheses.');
    for (const h of hs) {
      let line = Number(h.rank) + '. **' + md(h.title, 200) + '**';
      const l = loc(h);
      if (l) line += ' — ' + l;
      line += ' — confidence: ' + md(h.confidence_label, 10);
      line += ' — evidence: ' + (ids(h.supporting_evidence_ids) || 'none');
      if (ids(h.counter_evidence_ids)) line += ' — counter-evidence: ' + ids(h.counter_evidence_ids);
      o.push(line);
      if (h.reason) o.push('   ' + md(h.reason, 500));
    }
    o.push('', 'Rejected by validation: ' + (Array.isArray(m.rejected) ? m.rejected.length : 0));
  }
  console.log(o.join('\n'));
} else {
  process.exit(64);
}
NODE
export RM_HDR="$hdr" RM_REQ="$tmp/request.json"

node "$helper" request "$tmp/request.json"
unset NOTIFY_URL # in the private request file now; not inherited by curl

# api METHOD PATH [BODY_FILE]: sets HTTP_CODE; response body in $tmp/resp.json.
api() {
  rm -f "$tmp/resp.json" "$tmp/resp.headers"
  if [ $# -ge 3 ]; then
    HTTP_CODE="$(curl -sS --max-time 30 -X "$1" -H @"$hdr" -H 'Accept: application/json' \
      -H 'Content-Type: application/json' --data-binary @"$3" \
      -D "$tmp/resp.headers" -o "$tmp/resp.json" -w '%{http_code}' "$base$2")" || {
      say "$1 $2 failed: curl exit $?"
      exit 4
    }
  else
    HTTP_CODE="$(curl -sS --max-time 30 -X "$1" -H @"$hdr" -H 'Accept: application/json' \
      -D "$tmp/resp.headers" -o "$tmp/resp.json" -w '%{http_code}' "$base$2")" || {
      say "$1 $2 failed: curl exit $?"
      exit 4
    }
  fi
}
http_error() {
  node "$helper" error "$HTTP_CODE" "$tmp/resp.json" "$tmp/resp.headers" >&2
  exit 4
}
emit() {
  printf '%s\n' "$1"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then printf '%s\n' "$1" >>"$GITHUB_STEP_SUMMARY"; fi
}

api POST /api/analyses "$tmp/request.json"
case "$HTTP_CODE" in 2??) ;; *) http_error ;; esac
id="$(node "$helper" field "$tmp/resp.json" id)" || {
  say "the server returned no valid analysis id"
  exit 4
}
say "started analysis $id of $REPO (baseline $BASELINE, command $COMMAND_ID)"

deadline=$(($(date +%s) + TIMEOUT_SECONDS))
while :; do
  sleep "$POLL_SECONDS"
  api GET "/api/analyses/$id"
  [ "$HTTP_CODE" = 200 ] || http_error
  status="$(node "$helper" field "$tmp/resp.json" status)" || {
    say "unexpected analysis status from the server"
    exit 4
  }
  [ "$status" = running ] || break
  if [ "$(date +%s)" -ge "$deadline" ]; then
    say "timed out after ${TIMEOUT_SECONDS}s waiting for analysis $id (it keeps running on the server)"
    emit "RegressionMap: timed out after ${TIMEOUT_SECONDS}s waiting for analysis \`$id\`."
    exit 3
  fi
done

emit "$(node "$helper" summary "$tmp/resp.json")"
[ "$status" = done ] || exit 2
exit 0
