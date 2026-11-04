#!/bin/sh
# Start a RegressionMap analysis from CI, wait for it, and print a markdown
# summary. POSIX sh; needs curl and node (both are on GitHub-hosted runners).
#
#   REGRESSIONMAP_URL=https://regressionmap.example.com \
#   REGRESSIONMAP_KEY=rm_live_...   (an "analyze" key, from a CI secret) \
#   REPO=my-repo BASELINE=<good sha> sh scripts/ci-analyze.sh
#
# Environment:
#   REGRESSIONMAP_URL  required. https://, or http:// for 127.0.0.1, localhost or
#                      [::1] only (optionally :port). No user information ('@')
#                      and no backslash before the path.
#   REGRESSIONMAP_KEY  required. Read from the environment only. It is written to a
#                      private header file for curl and never put on a command line
#                      or printed.
#   REPO               required. Repository name under the server's WORKSPACE_ROOT.
#   BASELINE           required. Known-good commit (full or short SHA).
#   COMMAND_ID         allowlisted test command (default node-test).
#   NOTIFY_URL         optional https:// webhook for the completion notification.
#                      Treat it as a secret: it is never printed.
#   NOTIFY_FORMAT      optional: json (server default) or slack.
#   TIMEOUT_SECONDS    how long to wait (default 600; digits, no leading zero).
#   POLL_SECONDS       poll interval (default 3; digits above 0, no leading zero).
#   GITHUB_SHA         set by GitHub Actions. When the analysed HEAD differs, the
#                      summary warns that the server's clone may lag behind CI.
#   GITHUB_ACTIONS     set by GitHub Actions. When it is "true", output that carries
#                      server data is fenced with a random stop-commands token.
#
# The summary goes to stdout and is appended to $GITHUB_STEP_SUMMARY when set.
# Server text is untrusted: control characters are removed, '::' and '##[' are
# broken, and in the summary links, images and HTML are escaped.
#
# Exit codes:
#   0  analysis finished with status "done" (the tests may still fail; read the summary)
#   1  usage error (missing or invalid input, missing curl/node)
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

# No leading zeros: sh arithmetic would read 010 as octal and reject 08.
case "$TIMEOUT_SECONDS" in '' | *[!0-9]* | 0?*) usage_error "TIMEOUT_SECONDS must be a whole number of seconds, without leading zeros" ;; esac
case "$POLL_SECONDS" in '' | *[!0-9]* | 0*) usage_error "POLL_SECONDS must be a whole number above 0, without leading zeros" ;; esac
command -v curl >/dev/null 2>&1 || usage_error "curl is required"
command -v node >/dev/null 2>&1 || usage_error "node is required"

# The URL rule (the same as in ci-analyze.ps1): https:// to any host, or plain
# http:// when the authority is exactly 127.0.0.1, localhost or [::1] with an
# optional :port. The authority (up to the first '/') must not hold '@' (user
# information, which would move the real host after it) or a backslash.
url_rule="REGRESSIONMAP_URL must start with https:// (plain http:// is accepted for 127.0.0.1, localhost and [::1] only)"
base="$REGRESSIONMAP_URL"
while :; do case "$base" in */) base="${base%/}" ;; *) break ;; esac; done
case "$base" in
  https://*) authority="${base#https://}" proto='=https' ;;
  http://*) authority="${base#http://}" proto='=http' ;;
  *) usage_error "$url_rule" ;;
esac
authority="${authority%%/*}"
case "$authority" in
  '') usage_error "REGRESSIONMAP_URL has no host" ;;
  *@* | *\\*) usage_error "REGRESSIONMAP_URL must not contain '@' or a backslash before its path" ;;
esac
if [ "$proto" = '=http' ]; then
  case "$authority" in
    127.0.0.1:*) port="${authority#127.0.0.1:}" ;;
    localhost:*) port="${authority#localhost:}" ;;
    '[::1]:'*) port="${authority#'[::1]:'}" ;;
    127.0.0.1 | localhost | '[::1]') port=80 ;;
    *) usage_error "$url_rule" ;;
  esac
  case "$port" in '' | *[!0-9]*) usage_error "$url_rule" ;; esac
fi

# Private scratch directory: files below are created mode 0600 and removed on
# exit, also when the script is stopped by HUP, INT, PIPE or TERM.
umask 077
tmp="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/regressionmap.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 141' PIPE
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
  let out = s;
  for (const x of secrets) if (x) out = out.split(x).join('[redacted]');
  return out;
}

// Server data is untrusted. Text fields are printed only when they are
// strings, numbers only when they are safe integers ('?' otherwise), and
// objects only when they are plain objects.
const str = (s) => (typeof s === 'string' ? s : '');
const obj = (x) => (x !== null && typeof x === 'object' && !Array.isArray(x) ? x : null);
const int = (x) => (Number.isSafeInteger(x) ? String(x) : '?');
// One plain line: secrets redacted, whitespace collapsed, control characters
// (U+0000-U+001F, U+007F-U+009F) removed, at most max characters, and '::'
// and '##[' broken with a zero-width space so that the Actions runner never
// reads a workflow command in it. For stderr.
const ZW = String.fromCharCode(0x200b); // zero-width space
const ELLIPSIS = String.fromCharCode(0x2026);
function plain(s, max = 300) {
  // JavaScript's \s lacks U+0085 and .NET's lacks U+FEFF: both scripts use
  // the union, so they collapse the same characters.
  let t = redact(str(s)).replace(/[\s\u0085]+/g, ' ').replace(/[\u0000-\u001f\u007f-\u009f]/g, '');
  t = redact(t.replace(/ {2,}/g, ' ').replace(/^ | $/g, ''));
  const cp = Array.from(t);
  if (cp.length > max) t = cp.slice(0, max - 1).join('') + ELLIPSIS;
  return t.replace(/:(?=:)/g, ':' + ZW).replace(/#(?=#\[)/g, '#' + ZW);
}
// Inside a code span nothing is interpreted; only a backtick would end it.
const code = (s, max) => plain(s, max).replace(/`/g, "'");
// Inline Markdown: no links, images, HTML or code spans.
const md = (s, max) =>
  plain(s, max).replace(/[\\[\]]/g, '\\$&')
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/`/g, "'");
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
} else if (cmd === 'token') {
  process.stdout.write(require('crypto').randomBytes(16).toString('hex'));
} else if (cmd === 'error') {
  // error HTTP_CODE BODY_FILE HEADERS_FILE: plain-text lines for stderr
  const [status, bodyFile, headFile] = args;
  const err = obj((obj(readJSON(bodyFile)) || {}).error) || {};
  let line = 'HTTP ' + status + ' ' + (plain(err.code, 80) || 'error');
  const msg = plain(err.message);
  if (msg) line += ': ' + msg;
  const out = ['ci-analyze: ' + line];
  if (status === '429') {
    let ra = '';
    try {
      const m = /^retry-after:[ \t]*(.*?)[ \t\r]*$/im.exec(fs.readFileSync(headFile, 'utf8'));
      if (m) ra = m[1];
    } catch (e) { /* no headers */ }
    out.push('ci-analyze: Retry-After: ' + (/^[0-9]{1,15}$/.test(ra) ? ra + ' s' : plain(ra, 40) || 'not given'));
  }
  console.log(out.join('\n'));
} else if (cmd === 'field') {
  // field FILE id|status  -> validated value, exit 1 when invalid
  const a = obj(readJSON(args[0])) || {};
  const v = a[args[1]];
  const ok = typeof v === 'string' &&
    (args[1] === 'id' ? /^a[0-9a-z-]{1,40}$/.test(v) : ['running', 'done', 'error'].includes(v));
  if (!ok) process.exit(1);
  process.stdout.write(v);
} else if (cmd === 'pending') {
  // pending FILE -> exit 0 when the notification is still pending
  const n = obj((obj(readJSON(args[0])) || {}).notification);
  process.exit(n && n.status === 'pending' ? 0 : 1);
} else if (cmd === 'summary') {
  const a = obj(readJSON(args[0])) || {};
  const short = (s) => code(str(s).slice(0, 7), 7);
  const run = (r) => {
    r = obj(r);
    if (!r) return 'not run';
    if (r.timed_out === true) return 'FAIL (timed out)';
    return (r.exit_code === 0 ? 'PASS' : 'FAIL') + ' (exit ' + int(r.exit_code) + ')';
  };
  const loc = (h) => {
    const f = code(h.file, 200);
    if (!f) return '';
    const ls = h.line_start;
    const le = h.line_end;
    if (!Number.isSafeInteger(ls)) return '`' + f + ':?`';
    if (ls <= 0) return '`' + f + '`';
    if (!Number.isSafeInteger(le)) return '`' + f + ':' + ls + '-?`';
    if (le > ls) return '`' + f + ':' + ls + '-' + le + '`';
    return '`' + f + ':' + ls + '`';
  };
  const ids = (xs) => (Array.isArray(xs) ? xs : [])
    .filter((x) => typeof x === 'string').map((x) => md(x, 12)).filter(Boolean).join(', ');
  const o = [];
  o.push('## RegressionMap analysis', '');
  o.push('- Repository: `' + code(a.repo, 100) + '`');
  o.push('- Analysis: `' + code(a.id, 60) + '`');
  o.push('- Status: **' + md(a.status, 20) + '**');
  if (a.status === 'error') o.push('- Error: ' + md(a.error, 500));
  o.push('- Baseline `' + short(a.baseline) + '`: ' + run(a.baseline_run));
  o.push('- HEAD `' + short(a.current) + '`: ' + run(a.current_run));
  // The server tests its own clone: say so when that is not the commit CI ran.
  const want = str(process.env.GITHUB_SHA).trim().toLowerCase();
  const got = str(a.current).trim().toLowerCase();
  if (want && got) {
    const k = Math.min(want.length, got.length);
    if (k < 7 || want.slice(0, k) !== got.slice(0, k)) {
      o.push('- Warning: the server analysed `' + short(got) + '`, not `' + short(want) +
        "` (GITHUB_SHA): the server's clone may lag behind CI.");
    }
  }
  const m = obj(a.model);
  if (m) {
    const mid = code(m.model_id, 60);
    const msg = md(m.message);
    o.push('- Model: ' + md(m.status, 20) + (mid ? ' (`' + mid + '`)' : '') + (msg ? ' — ' + msg : ''));
  }
  const n = obj(a.notification);
  if (n) {
    const st = n.status === 'pending' ? 'still pending' : md(n.status, 20);
    const le = md(n.last_error, 200);
    o.push('- Notification: ' + st + ' to ' + md(n.host, 120) + ' (' + md(n.format, 10) + ', ' +
      int(n.attempts) + ' attempt(s)' + (le ? ', ' + le : '') + ')');
  }
  if (m) {
    o.push('', '### AI-generated hypotheses — not verified', '');
    o.push('The test runner decides PASS or FAIL. These are ranked guesses that cite evidence IDs from the analysis; check them before acting.', '');
    const result = obj(m.result);
    const hs = m.status === 'ok' && result && Array.isArray(result.hypotheses) ? result.hypotheses.filter(obj) : [];
    if (hs.length === 0) o.push('No valid hypotheses.');
    for (const h of hs) {
      let line = int(h.rank) + '. **' + md(h.title, 200) + '**';
      const l = loc(h);
      if (l) line += ' — ' + l;
      line += ' — confidence: ' + md(h.confidence_label, 10);
      line += ' — evidence: ' + (ids(h.supporting_evidence_ids) || 'none');
      if (ids(h.counter_evidence_ids)) line += ' — counter-evidence: ' + ids(h.counter_evidence_ids);
      o.push(line);
      const reason = md(h.reason, 500);
      if (reason) o.push('   ' + reason);
    }
    o.push('', 'Rejected by validation: ' + (Array.isArray(m.rejected) ? m.rejected.length : 0));
  }
  // The Muse review, only when review is an object. Lens and verdict values
  // count only as exact strings. Refuted findings are only counted; the other
  // findings are listed confirmed, then unchecked, then disputed.
  const rv = obj(a.review);
  if (rv) {
    o.push('', '### Muse review', '');
    if (rv.status !== 'ok') {
      const msg = md(rv.message, 300);
      o.push('Muse review not available: ' + md(rv.status, 20) + (msg ? ' — ' + msg : ''));
    } else {
      const found = Array.isArray(rv.findings) ? rv.findings.filter(obj) : [];
      const check = (f) => obj(f.check) || {};
      const lens = (x) => found.filter((f) => f.lens === x).length;
      const verdict = (x) => found.filter((f) => check(f).verdict === x);
      o.push('Lenses: security ' + lens('security') + ', code ' + lens('code') + ', feature ' + lens('feature') +
        '. Verdicts: ' + verdict('confirmed').length + ' confirmed, ' + verdict('unchecked').length + ' unchecked, ' +
        verdict('disputed').length + ' disputed, ' + verdict('refuted').length + ' refuted.');
      if (rv.check_status === 'error' || rv.check_status === 'invalid') {
        const cm = md(rv.check_message, 300);
        o.push('Muse check not available: ' + rv.check_status + (cm ? ' — ' + cm : ''));
      }
      o.push('');
      // Each listed finding goes with the contract label of the verdict it
      // matched exactly, so no model text reaches the verdict slot.
      const withLabel = (fs, label) => fs.map((f) => ({ f, label }));
      const shown = [
        ...withLabel(verdict('confirmed'), 'Confirmed by Muse check'),
        ...withLabel(verdict('unchecked'), 'Not checked'),
        ...withLabel(verdict('disputed'), 'Disputed by Muse check'),
      ];
      if (shown.length === 0) o.push('No confirmed, unchecked or disputed findings.');
      for (const { f, label } of shown) {
        let line = '- ' + md(f.id, 12) + ' [' + md(f.lens, 12) + ' · ' + md(f.severity, 12) + ' · ' +
          label + '] **' + md(f.title, 200) + '**';
        const l = loc(f);
        if (l) line += ' — ' + l;
        line += ' — evidence: ' + (ids(f.evidence_ids) || 'none');
        o.push(line);
        const detail = md(f.detail, 300);
        if (detail) o.push('  ' + detail);
        const reason = md(check(f).reason, 300);
        if (reason) o.push('  Muse check: ' + reason);
      }
      o.push('', 'Refuted by Muse check (not shown): ' + verdict('refuted').length);
      o.push('Rejected by validation: ' + (Array.isArray(rv.rejected) ? rv.rejected.length : 0));
    }
    o.push('', 'AI review: Muse wrote these findings and a second Muse pass checked them. Not verified by tests.');
  }
  console.log(o.join('\n'));
} else {
  process.exit(64);
}
NODE
export RM_HDR="$hdr" RM_REQ="$tmp/request.json"

# In GitHub Actions, every block of output that carries server data is fenced
# with a random token, so the runner reads no workflow command in it.
tok=''
if [ "${GITHUB_ACTIONS:-}" = true ]; then tok="$(node "$helper" token)"; fi
untrusted() {
  if [ -n "$tok" ]; then
    printf '::stop-commands::%s\n%s\n::%s::\n' "$tok" "$1" "$tok"
  else
    printf '%s\n' "$1"
  fi
}
say_untrusted() { untrusted "ci-analyze: $*" >&2; }

node "$helper" request "$tmp/request.json"
unset NOTIFY_URL # in the private request file now; not inherited by curl

# api METHOD PATH [BODY_FILE]: sets HTTP_CODE, response body in $tmp/resp.json.
# Returns 1 when curl fails (its exit status in CURL_STATUS). -q must stay the
# first argument: it stops curl from reading a .curlrc.
api() {
  rm -f "$tmp/resp.json" "$tmp/resp.headers"
  if [ $# -ge 3 ]; then
    HTTP_CODE="$(curl -q -sS --proto "$proto" --max-time 30 -X "$1" -H @"$hdr" -H 'Accept: application/json' \
      -H 'Content-Type: application/json' --data-binary @"$3" \
      -D "$tmp/resp.headers" -o "$tmp/resp.json" -w '%{http_code}' "$base$2")" || {
      CURL_STATUS=$?
      return 1
    }
  else
    HTTP_CODE="$(curl -q -sS --proto "$proto" --max-time 30 -X "$1" -H @"$hdr" -H 'Accept: application/json' \
      -D "$tmp/resp.headers" -o "$tmp/resp.json" -w '%{http_code}' "$base$2")" || {
      CURL_STATUS=$?
      return 1
    }
  fi
}
curl_failed() {
  say "$1 $2 failed: curl exit $CURL_STATUS"
  exit 4
}
http_error() {
  untrusted "$(node "$helper" error "$HTTP_CODE" "$tmp/resp.json" "$tmp/resp.headers")" >&2
  exit 4
}
emit() {
  untrusted "$1"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then printf '%s\n' "$1" >>"$GITHUB_STEP_SUMMARY"; fi
}

api POST /api/analyses "$tmp/request.json" || curl_failed POST /api/analyses
case "$HTTP_CODE" in 2??) ;; *) http_error ;; esac
id="$(node "$helper" field "$tmp/resp.json" id)" || {
  say "the server returned no valid analysis id"
  exit 4
}
say_untrusted "started analysis $id of $REPO (baseline $BASELINE, command $COMMAND_ID)"

deadline=$(($(date +%s) + TIMEOUT_SECONDS))
while :; do
  sleep "$POLL_SECONDS"
  api GET "/api/analyses/$id" || curl_failed GET "/api/analyses/$id"
  [ "$HTTP_CODE" = 200 ] || http_error
  status="$(node "$helper" field "$tmp/resp.json" status)" || {
    say "unexpected analysis status from the server"
    exit 4
  }
  [ "$status" = running ] || break
  if [ "$(date +%s)" -ge "$deadline" ]; then
    say_untrusted "timed out after ${TIMEOUT_SECONDS}s waiting for analysis $id (it keeps running on the server)"
    emit "RegressionMap: timed out after ${TIMEOUT_SECONDS}s waiting for analysis \`$id\`."
    exit 3
  fi
done

# The notification is sent when the analysis finishes, so its result often
# lands a few seconds later. Wait up to 30 s more for it, every 2 s. A failed
# GET here ends the wait but not the run: the analysis result is known.
cp "$tmp/resp.json" "$tmp/final.json"
if node "$helper" pending "$tmp/final.json"; then
  say "the analysis finished; waiting up to 30s for its notification to be delivered"
  wait_end=$(($(date +%s) + 30))
  while [ "$(date +%s)" -lt "$wait_end" ]; do
    sleep 2
    if ! api GET "/api/analyses/$id"; then
      say "stopped waiting for the notification: curl exit $CURL_STATUS"
      break
    fi
    if [ "$HTTP_CODE" != 200 ]; then
      say "stopped waiting for the notification: HTTP $HTTP_CODE"
      break
    fi
    if ! node "$helper" field "$tmp/resp.json" status >/dev/null; then
      say "stopped waiting for the notification: unexpected analysis status from the server"
      break
    fi
    cp "$tmp/resp.json" "$tmp/final.json"
    node "$helper" pending "$tmp/final.json" || break
  done
fi

emit "$(node "$helper" summary "$tmp/final.json")"
[ "$status" = done ] || exit 2
exit 0
