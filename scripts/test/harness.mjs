// Runs scripts/ci-analyze.sh and scripts/ci-analyze.ps1 against the fake
// server with a controlled environment, and holds the shared fixtures and
// assertions. This module only exports functions: importing it starts nothing.
import { spawn, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import assert from 'node:assert/strict';
import { ID } from './fake-server.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
export const repoRoot = path.resolve(here, '..', '..');
const shScript = path.join(repoRoot, 'scripts', 'ci-analyze.sh');
const psScript = path.join(repoRoot, 'scripts', 'ci-analyze.ps1');

export const KEY = 'rm_live_TESTKEY_0123456789abcdef0123456789';
export const NOTIFY = 'https://hooks.example.com/services/T000/B000/NOTIFYSECRET0123';
export const ZW = '\u200b';

const isWindows = process.platform === 'win32';
// Git Bash on Windows takes C:/x/y paths; backslashes are escapes in sh.
const forward = (p) => (isWindows ? p.replace(/\\/g, '/') : p);

// The sh binary: sh by default (bash in Git Bash on Windows, dash on Ubuntu).
// RM_TEST_SH=dash runs the same cases under another shell.
export const SH = process.env.RM_TEST_SH || 'sh';

// Variables that would change what the scripts do; every run starts without
// them, so the CI job's own GITHUB_* values never leak into a case.
const OWN = /^(REGRESSIONMAP_|GITHUB_|RUNNER_|NOTIFY_|ACTIONS_|CURL_HOME$|REPO$|BASELINE$|COMMAND_ID$|TIMEOUT_SECONDS$|POLL_SECONDS$)/i;

function cleanEnv() {
  const env = {};
  for (const [k, v] of Object.entries(process.env)) if (!OWN.test(k)) env[k] = v;
  return env;
}

// Text for a failure message: the test key hidden, control characters
// escaped and workflow command markers broken, so a failing case cannot
// inject commands into the log of the CI job that runs these tests. All
// comparisons go through eq/deq/assert.ok with such a message, because
// assert.equal would print the raw actual and expected values.
export function q(s) {
  return JSON.stringify(String(s).split(KEY).join('<TEST KEY>'))
    .replace(/::/g, ':' + ZW + ':')
    .replace(/##\[/g, '#' + ZW + '#[');
}
export function eq(actual, expected, debug = '') {
  assert.ok(actual === expected, `expected ${q(expected)}, got ${q(actual)} ${debug}`);
}
export function deq(actual, expected, debug = '') {
  eq(JSON.stringify(actual), JSON.stringify(expected), debug);
}

function findPowerShell() {
  for (const exe of ['powershell.exe', 'pwsh']) {
    const r = spawnSync(exe, ['-NoProfile', '-NonInteractive', '-Command', '$PSVersionTable.PSVersion.ToString()'], {
      encoding: 'utf8',
      timeout: 60000,
      windowsHide: true,
    });
    if (r.status === 0) return { exe, version: r.stdout.trim() };
  }
  return null;
}

export function shRunner() {
  return {
    kind: 'sh',
    label: `sh: ${SH}`,
    available: true,
    run: (opts) => runScript('sh', [SH, [forward(shScript)]], opts),
  };
}

let psFound;
export function psRunner() {
  if (psFound === undefined) psFound = findPowerShell();
  if (!psFound) {
    return {
      kind: 'ps1',
      label: 'ps1',
      available: false,
      skipReason: 'neither powershell.exe nor pwsh is on PATH, so the ps1 cases cannot run here',
    };
  }
  const args = ['-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', psScript];
  return {
    kind: 'ps1',
    label: `ps1: ${psFound.exe} ${psFound.version}`,
    available: true,
    run: (opts) => runScript('ps1', [psFound.exe, args], opts),
  };
}

// Runs one script. `env` overrides the defaults (undefined removes a
// variable); `prepare(dir)` may create files in the run's private directory
// and return more overrides. Resolves to { code, stdout, stderr, summary, ms }
// and throws when the key was printed or (sh) a temporary file was left.
async function runScript(kind, [cmd, args], { url, env = {}, prepare, timeoutMs = 120000 } = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rm-ci-test-'));
  const p = kind === 'sh' ? forward : (x) => x;
  try {
    const tmp = path.join(dir, 'tmp');
    const curlHome = path.join(dir, 'curlhome');
    fs.mkdirSync(tmp);
    fs.mkdirSync(curlHome);
    const summaryFile = path.join(dir, 'summary.md');
    fs.writeFileSync(summaryFile, 'PRE-EXISTING LINE\n');
    const e = cleanEnv();
    Object.assign(e, {
      REGRESSIONMAP_URL: url,
      REGRESSIONMAP_KEY: KEY,
      REPO: 'my-repo',
      BASELINE: '1111111',
      POLL_SECONDS: '1',
      GITHUB_STEP_SUMMARY: p(summaryFile),
      TMPDIR: p(tmp),
      CURL_HOME: p(curlHome),
    });
    const extra = prepare ? prepare({ dir, p }) : {};
    for (const [k, v] of Object.entries({ ...env, ...extra })) {
      if (v === undefined) delete e[k];
      else e[k] = v;
    }
    const started = Date.now();
    const result = await new Promise((resolve, reject) => {
      const child = spawn(cmd, args, { env: e, stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true });
      const out = [];
      const err = [];
      child.stdout.on('data', (c) => out.push(c));
      child.stderr.on('data', (c) => err.push(c));
      const timer = setTimeout(() => child.kill(), timeoutMs);
      child.on('error', (x) => {
        clearTimeout(timer);
        reject(x);
      });
      child.on('close', (code, signal) => {
        clearTimeout(timer);
        resolve({
          code,
          signal,
          stdout: Buffer.concat(out).toString('utf8'),
          stderr: Buffer.concat(err).toString('utf8'),
        });
      });
    });
    result.ms = Date.now() - started;
    result.summary = fs.readFileSync(summaryFile, 'utf8');
    result.debug = `exit ${result.code}; stdout ${q(result.stdout)}; stderr ${q(result.stderr)}`;
    for (const [name, text] of [
      ['stdout', result.stdout],
      ['stderr', result.stderr],
      ['summary', result.summary],
    ]) {
      assert.ok(!text.includes(KEY), `the API key was printed to ${name}: ${result.debug}`);
    }
    if (kind === 'sh') {
      const left = fs.readdirSync(tmp);
      assert.ok(left.length === 0, `temporary files were left behind: ${q(left.join(', '))}; ${result.debug}`);
    }
    return result;
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

// Lines as the runner sees them (CRLF from PowerShell on Windows included).
export const lines = (s) => s.split(/\r?\n/);

export function has(text, needle, debug = '') {
  assert.ok(text.includes(needle), `expected ${q(needle)} in ${q(text)} ${debug}`);
}
export function lacks(text, needle, debug = '') {
  assert.ok(!text.includes(needle), `did not expect ${q(needle)} in ${q(text)} ${debug}`);
}

// No line may be read as a workflow command: after leading whitespace, no
// line starts with '::' (the runner trims it before matching), and no line
// holds the legacy '##[' form anywhere. The script's own stop-commands
// markers are the only exception; they are returned in order.
export function workflowMarkers(text, debug = '') {
  const markers = [];
  for (const line of lines(text)) {
    const t = line.replace(/^[\s\u0085]+/, '');
    assert.ok(!t.includes('##['), `a line holds '##[': ${q(line)} ${debug}`);
    if (!t.startsWith('::')) continue;
    assert.ok(/^::stop-commands::[0-9a-f]{32}$/.test(t) || /^::[0-9a-f]{32}::$/.test(t), `a line starts with '::': ${q(line)} ${debug}`);
    markers.push(t);
  }
  return markers;
}

// The script prints only newlines (and CR before them from PowerShell); no
// other control character from U+0000 to U+001F or U+007F to U+009F.
export function assertNoControlChars(text, debug = '') {
  const m = /[\u0000-\u0009\u000b-\u000c\u000e-\u001f\u007f-\u009f]|\r(?!\n)/.exec(text);
  assert.ok(!m, `control character ${m && q(m[0])} in ${q(text)} ${debug}`);
}

// When GITHUB_ACTIONS is true, all lines that carry server data sit between
// '::stop-commands::<token>' and '::<token>::'. Returns the lines in between.
export function fenced(text, debug = '') {
  const ls = lines(text).filter((l) => l !== '');
  const first = /^::stop-commands::([0-9a-f]{32})$/.exec(ls[0] || '');
  assert.ok(first, `the output does not start with a stop-commands line: ${debug}`);
  const token = first[1];
  assert.ok(ls[ls.length - 1] === `::${token}::`, `the output does not end with the resume line: ${debug}`);
  return { token, inner: ls.slice(1, -1) };
}

export function hypothesis(over = {}) {
  return {
    rank: 1,
    title: 'Off-by-one in paginate — café …',
    file: 'src/pagination.ts',
    line_start: 7,
    line_end: 13,
    reason: 'The end index is exclusive',
    supporting_evidence_ids: ['E1', 'E4'],
    counter_evidence_ids: [],
    verification_suggestion: 'run the pagination tests',
    confidence_label: 'high',
    ...over,
  };
}

export function analysis(over = {}) {
  return {
    id: ID,
    repo: 'my-repo',
    status: 'done',
    created_at: '2026-11-04T10:00:00Z',
    baseline: '1111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
    current: '2222222bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
    command: { id: 'node-test', argv: ['node', '--test'] },
    baseline_run: { exit_code: 0, passed: true, timed_out: false, truncated: false, duration_ms: 12 },
    current_run: { exit_code: 1, passed: false, timed_out: false, truncated: false, duration_ms: 15 },
    changed_files: [],
    evidence: [],
    model: {
      status: 'ok',
      model_id: 'test-model',
      message: '',
      rejected: ['hypothesis 3: cites unknown evidence id E99'],
      result: {
        summary: 's',
        hypotheses: [
          hypothesis(),
          hypothesis({
            rank: 2,
            title: 'Formatter change',
            file: 'src/format.ts',
            line_start: 4,
            line_end: 4,
            reason: 'Unlikely',
            supporting_evidence_ids: ['E2'],
            counter_evidence_ids: ['E1'],
            confidence_label: 'low',
          }),
        ],
        limitations: [],
      },
    },
    verifications: [],
    notification: null,
    review: null,
    ...over,
  };
}

// One finding of the Muse review, as GET /api/analyses/{id} sends it.
export function finding(over = {}) {
  return {
    id: 'R1',
    lens: 'security',
    severity: 'high',
    title: 'Token compared with ==',
    detail: 'Timing leak in the token check.',
    suggestion: 'Compare the tokens in constant time.',
    file: 'src/auth.ts',
    line_start: 12,
    line_end: 12,
    evidence_ids: ['E3'],
    check: { verdict: 'confirmed', reason: 'E3 shows a plain == comparison.', evidence_ids: ['E3'] },
    ...over,
  };
}

// The check of a finding without a valid check.
export const unchecked = () => ({ verdict: 'unchecked', reason: '', evidence_ids: [] });

// A review with one finding per verdict. The refuted one (R2) is only counted.
// The ids follow contract rule 5: severity (high, medium, low), then lens
// (security, code, feature), so the low code finding R3 comes before the low
// feature finding R4.
export function review(over = {}) {
  return {
    status: 'ok',
    model_id: 'test-model',
    message: '',
    check_status: 'ok',
    check_message: '',
    summary: 'One timing leak.',
    counts: { confirmed: 1, disputed: 1, refuted: 1, unchecked: 1 },
    findings: [
      finding(),
      finding({
        id: 'R2',
        lens: 'code',
        severity: 'medium',
        title: 'Cache never cleared',
        detail: 'The map grows with every call.',
        suggestion: '',
        file: 'src/cache.ts',
        line_start: 3,
        line_end: 5,
        evidence_ids: ['E4'],
        check: { verdict: 'refuted', reason: 'E4 clears the map on every call.', evidence_ids: ['E4'] },
      }),
      finding({
        id: 'R3',
        lens: 'code',
        severity: 'low',
        title: 'Empty page not handled',
        detail: '',
        suggestion: '',
        file: '',
        line_start: 0,
        line_end: 0,
        evidence_ids: ['E2'],
        check: unchecked(),
      }),
      finding({
        id: 'R4',
        lens: 'feature',
        severity: 'low',
        title: 'Possible overflow',
        detail: '',
        suggestion: '',
        file: 'src/pagination.ts',
        line_start: 7,
        line_end: 13,
        evidence_ids: ['E2'],
        check: { verdict: 'disputed', reason: 'Unlikely with page sizes under 1000.', evidence_ids: ['E2'] },
      }),
    ],
    rejected: ['finding 5 ("Rename x"): cites unknown evidence id E99'],
    ...over,
  };
}

export const REVIEW_DISCLAIMER = 'AI review: Muse wrote these findings and a second Muse pass checked them. Not verified by tests.';

// The review section as the scripts append it to the summary: `body` is the
// lines between the heading and the disclaimer.
export const reviewSection = (body) => ['', '', '### Muse review', '', ...body, '', REVIEW_DISCLAIMER].join('\n');

// The review section for review() with no overrides.
export const REVIEW_SECTION = reviewSection([
  'Lenses: security 1, code 2, feature 1. Verdicts: 1 confirmed, 1 unchecked, 1 disputed, 1 refuted.',
  '',
  '- R1 [security · high · Confirmed by Muse check] **Token compared with ==** — `src/auth.ts:12` — evidence: E3',
  '  Timing leak in the token check.',
  '  Muse check: E3 shows a plain == comparison.',
  '- R3 [code · low · Not checked] **Empty page not handled** — evidence: E2',
  '- R4 [feature · low · Disputed by Muse check] **Possible overflow** — `src/pagination.ts:7-13` — evidence: E2',
  '  Muse check: Unlikely with page sizes under 1000.',
  '',
  'Refuted by Muse check (not shown): 1',
  'Rejected by validation: 1',
]);

export const notification = (over = {}) => ({
  host: 'hooks.example.com',
  format: 'json',
  status: 'pending',
  attempts: 0,
  last_status_code: 0,
  last_error: '',
  ...over,
});

export const HYPOTHESES_HEADING = [
  '',
  '### AI-generated hypotheses — not verified',
  '',
  'The test runner decides PASS or FAIL. These are ranked guesses that cite evidence IDs from the analysis; check them before acting.',
  '',
];

// The complete summary for analysis() with no overrides.
export const BASE_SUMMARY = [
  '## RegressionMap analysis',
  '',
  '- Repository: `my-repo`',
  '- Analysis: `a-test-1`',
  '- Status: **done**',
  '- Baseline `1111111`: PASS (exit 0)',
  '- HEAD `2222222`: FAIL (exit 1)',
  '- Model: ok (`test-model`)',
  ...HYPOTHESES_HEADING,
  '1. **Off-by-one in paginate — café …** — `src/pagination.ts:7-13` — confidence: high — evidence: E1, E4',
  '   The end index is exclusive',
  '2. **Formatter change** — `src/format.ts:4` — confidence: low — evidence: E2 — counter-evidence: E1',
  '   Unlikely',
  '',
  'Rejected by validation: 1',
].join('\n');

// Model text that tries every injection the scripts must neutralise.
export function hostileTextAnalysis() {
  return analysis({
    repo: 'rep`o<b>::x',
    model: {
      status: 'ok',
      model_id: 'm::1',
      message: 'note ::set-output name=a::b',
      rejected: [],
      result: {
        summary: 's',
        limitations: [],
        hypotheses: [
          hypothesis({
            title:
              'Fix [click here](https://evil.example/fix) ![t](https://evil.example/t.png) <img src=x onerror=alert(1)> \u001b[32mPASS\u001b[0m ##[error]legacy & more \\[x]',
            reason: '::add-mask::FAIL',
          }),
          hypothesis({
            rank: 2,
            title: '::warning file=a.js::spoofed',
            reason: '::error title=x::y\r\n::notice::z\u0007\u009b\u0085end',
            file: 'src/b.ts',
            line_start: 0,
            line_end: 0,
            supporting_evidence_ids: ['E2'],
          }),
        ],
      },
    },
  });
}

export const HOSTILE_TEXT_SUMMARY = [
  '## RegressionMap analysis',
  '',
  "- Repository: `rep'o<b>:" + ZW + ':x`',
  '- Analysis: `a-test-1`',
  '- Status: **done**',
  '- Baseline `1111111`: PASS (exit 0)',
  '- HEAD `2222222`: FAIL (exit 1)',
  '- Model: ok (`m:' + ZW + ':1`) — note :' + ZW + ':set-output name=a:' + ZW + ':b',
  ...HYPOTHESES_HEADING,
  '1. **Fix \\[click here\\](https://evil.example/fix) !\\[t\\](https://evil.example/t.png) &lt;img src=x onerror=alert(1)&gt; \\[32mPASS\\[0m #' +
    ZW +
    '#\\[error\\]legacy &amp; more \\\\\\[x\\]** — `src/pagination.ts:7-13` — confidence: high — evidence: E1, E4',
  '   :' + ZW + ':add-mask:' + ZW + ':FAIL',
  '2. **:' + ZW + ':warning file=a.js:' + ZW + ':spoofed** — `src/b.ts` — confidence: high — evidence: E2',
  '   :' + ZW + ':error title=x:' + ZW + ':y :' + ZW + ':notice:' + ZW + ':z end',
  '',
  'Rejected by validation: 0',
].join('\n');

// Fields that should be numbers but are not, and hypotheses that are not
// objects. An honest server never sends these.
export function hostileTypesAnalysis() {
  return analysis({
    current_run: { exit_code: '1)\n::error title=Injected::x\n<img src="https://evil.example/p.png"> (', timed_out: false },
    notification: notification({ status: 'delivered', attempts: '3', last_status_code: 200 }),
    model: {
      status: 'ok',
      model_id: 'test-model',
      message: '',
      rejected: [],
      result: {
        summary: 's',
        limitations: [],
        hypotheses: [
          null,
          5,
          'str',
          [1],
          {
            rank: 'x\n::error::y',
            title: 't',
            file: 'f.js',
            line_start: '\t5',
            line_end: '\n::notice::z',
            confidence_label: 'high',
            supporting_evidence_ids: ['E1'],
            counter_evidence_ids: [],
            reason: 'r',
          },
          {
            rank: 2.5,
            title: 'u',
            file: 'g.js',
            line_start: 3,
            line_end: '9',
            confidence_label: 'low',
            supporting_evidence_ids: ['E2'],
            counter_evidence_ids: [],
            reason: '',
          },
        ],
      },
    },
  });
}

export const HOSTILE_TYPES_SUMMARY = [
  '## RegressionMap analysis',
  '',
  '- Repository: `my-repo`',
  '- Analysis: `a-test-1`',
  '- Status: **done**',
  '- Baseline `1111111`: PASS (exit 0)',
  '- HEAD `2222222`: FAIL (exit ?)',
  '- Model: ok (`test-model`)',
  '- Notification: delivered to hooks.example.com (json, ? attempt(s))',
  ...HYPOTHESES_HEADING,
  '?. **t** — `f.js:?` — confidence: high — evidence: E1',
  '   r',
  '?. **u** — `g.js:3-?` — confidence: low — evidence: E2',
  '',
  'Rejected by validation: 0',
].join('\n');

// Review text that tries the injections the review section must neutralise:
// workflow commands, a Markdown link and image, HTML, a backtick, a newline.
const HOSTILE_REVIEW_TEXT =
  '::error::pwned ##[group]x [a](http://evil.example) ![i](http://evil.example/i.png) <script> `b`\nnext';

export function hostileReviewAnalysis() {
  return analysis({
    review: review({
      counts: { confirmed: 1, disputed: 0, refuted: 0, unchecked: 0 },
      findings: [
        finding({
          title: HOSTILE_REVIEW_TEXT,
          detail: HOSTILE_REVIEW_TEXT,
          file: 'src/' + HOSTILE_REVIEW_TEXT + '.ts',
          check: { verdict: 'confirmed', reason: HOSTILE_REVIEW_TEXT, evidence_ids: ['E3'] },
        }),
      ],
      rejected: [],
    }),
  });
}

// HOSTILE_REVIEW_TEXT as inline Markdown (md) and inside a code span (code).
const HOSTILE_REVIEW_MD =
  ':' + ZW + ':error:' + ZW + ':pwned #' + ZW +
  "#\\[group\\]x \\[a\\](http://evil.example) !\\[i\\](http://evil.example/i.png) &lt;script&gt; 'b' next";
const HOSTILE_REVIEW_CODE =
  ':' + ZW + ':error:' + ZW + ':pwned #' + ZW + "#[group]x [a](http://evil.example) ![i](http://evil.example/i.png) <script> 'b' next";

export const HOSTILE_REVIEW_SUMMARY =
  BASE_SUMMARY +
  reviewSection([
    'Lenses: security 1, code 0, feature 0. Verdicts: 1 confirmed, 0 unchecked, 0 disputed, 0 refuted.',
    '',
    '- R1 [security · high · Confirmed by Muse check] **' + HOSTILE_REVIEW_MD + '** — `src/' + HOSTILE_REVIEW_CODE + '.ts:12` — evidence: E3',
    '  ' + HOSTILE_REVIEW_MD,
    '  Muse check: ' + HOSTILE_REVIEW_MD,
    '',
    'Refuted by Muse check (not shown): 0',
    'Rejected by validation: 0',
  ]);

// Review text over every cap of the review section that the output shows.
// A cut value keeps max - 1 characters and ends with an ellipsis. The listed
// finding keeps the verdict "confirmed", so that it is listed; its line shows
// the contract label, never the verdict text, so the verdict has no cap to
// show. The check_status of a failed check is printed only when it is exactly
// "error" or "invalid", so it has no cap to show.
const CUT = '…';

export function cappedReviewAnalysis() {
  return analysis({
    review: review({
      counts: { confirmed: 1, disputed: 0, refuted: 0, unchecked: 0 },
      findings: [
        finding({
          id: 'R' + '1'.repeat(20),
          lens: 'l'.repeat(20),
          severity: 's'.repeat(20),
          title: 't'.repeat(250),
          detail: 'd'.repeat(400),
          file: 'f'.repeat(250),
          evidence_ids: ['E' + '7'.repeat(20), 'E' + '8'.repeat(20)],
          check: { verdict: 'confirmed', reason: 'r'.repeat(400), evidence_ids: ['E3'] },
        }),
      ],
      rejected: [],
    }),
  });
}

export const CAPPED_REVIEW_SUMMARY =
  BASE_SUMMARY +
  reviewSection([
    'Lenses: security 0, code 0, feature 0. Verdicts: 1 confirmed, 0 unchecked, 0 disputed, 0 refuted.',
    '',
    // id 12; lens, severity 12 each; title 200; file 200; evidence ids 12 each
    '- R' + '1'.repeat(10) + CUT +
      ' [' + 'l'.repeat(11) + CUT + ' · ' + 's'.repeat(11) + CUT + ' · Confirmed by Muse check]' +
      ' **' + 't'.repeat(199) + CUT + '**' +
      ' — `' + 'f'.repeat(199) + CUT + ':12`' +
      ' — evidence: E' + '7'.repeat(10) + CUT + ', E' + '8'.repeat(10) + CUT,
    // detail 300; check reason 300
    '  ' + 'd'.repeat(299) + CUT,
    '  Muse check: ' + 'r'.repeat(299) + CUT,
    '',
    'Refuted by Muse check (not shown): 0',
    'Rejected by validation: 0',
  ]);

// A review that is not ok, over the status cap (20) and the message cap (300).
export function cappedStatusAnalysis() {
  return analysis({
    review: review({
      status: 'x'.repeat(30),
      message: 'm'.repeat(400),
      check_status: 'skipped',
      check_message: 'no review to check',
      counts: { confirmed: 0, disputed: 0, refuted: 0, unchecked: 0 },
      findings: [],
      rejected: [],
    }),
  });
}

export const CAPPED_STATUS_SUMMARY =
  BASE_SUMMARY + reviewSection(['Muse review not available: ' + 'x'.repeat(19) + CUT + ' — ' + 'm'.repeat(299) + CUT]);

// A failed check, over the check message cap (300).
export function cappedCheckAnalysis() {
  return analysis({
    review: review({
      check_status: 'error',
      check_message: 'c'.repeat(400),
      counts: { confirmed: 0, disputed: 0, refuted: 0, unchecked: 1 },
      findings: [finding({ check: unchecked() })],
      rejected: [],
    }),
  });
}

export const CAPPED_CHECK_SUMMARY =
  BASE_SUMMARY +
  reviewSection([
    'Lenses: security 1, code 0, feature 0. Verdicts: 0 confirmed, 1 unchecked, 0 disputed, 0 refuted.',
    'Muse check not available: error — ' + 'c'.repeat(299) + CUT,
    '',
    '- R1 [security · high · Not checked] **Token compared with ==** — `src/auth.ts:12` — evidence: E3',
    '  Timing leak in the token check.',
    '',
    'Refuted by Muse check (not shown): 0',
    'Rejected by validation: 0',
  ]);

// The summary as the script printed it: stdout without the stop-commands
// lines, with CRLF folded to LF and the trailing newline removed.
export function printedSummary(stdout) {
  return lines(stdout)
    .filter((l) => !/^::stop-commands::[0-9a-f]{32}$/.test(l) && !/^::[0-9a-f]{32}::$/.test(l))
    .join('\n')
    .replace(/\n+$/, '');
}
