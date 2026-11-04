// The cases both CI scripts must pass, with the same inputs and the same
// expected output. ci-analyze-sh.test.mjs and ci-analyze-ps1.test.mjs run them.
import { describe, test } from 'node:test';
import assert from 'node:assert/strict';
import { startServer, freePort, ID } from './fake-server.mjs';
import {
  KEY,
  NOTIFY,
  ZW,
  q,
  eq,
  deq,
  has,
  lacks,
  lines,
  workflowMarkers,
  assertNoControlChars,
  fenced,
  analysis,
  hypothesis,
  notification,
  printedSummary,
  BASE_SUMMARY,
  HYPOTHESES_HEADING,
  hostileTextAnalysis,
  HOSTILE_TEXT_SUMMARY,
  hostileTypesAnalysis,
  HOSTILE_TYPES_SUMMARY,
} from './harness.mjs';

const done = (over) => ({ status: 200, json: analysis(over) });
const err = (status, code, message, headers) => ({ status, json: { error: { code, message } }, headers });

async function withServer(spec, fn) {
  const srv = await startServer(spec);
  try {
    return await fn(srv);
  } finally {
    await srv.close();
  }
}

const URL_RULE =
  'ci-analyze: REGRESSIONMAP_URL must start with https:// (plain http:// is accepted for 127.0.0.1, localhost and [::1] only)';
const URL_AT = "ci-analyze: REGRESSIONMAP_URL must not contain '@' or a backslash before its path";

export function defineCases(runner) {
  const opts = runner.available ? { concurrency: 3 } : { skip: runner.skipReason };
  describe(`ci-analyze (${runner.label})`, opts, () => {
    // ---- exit codes ------------------------------------------------------
    test('exit 0: a done analysis prints the summary and appends it to GITHUB_STEP_SUMMARY', async () => {
      await withServer({ gets: [{ status: 200, json: { id: ID, status: 'running' } }, done()] }, async (srv) => {
        const r = await runner.run({ url: srv.url });
        eq(r.code, 0, r.debug);
        eq(printedSummary(r.stdout), BASE_SUMMARY, `stdout differs: ${r.debug}`);
        eq(r.summary, 'PRE-EXISTING LINE\n' + BASE_SUMMARY + '\n', `summary file differs: ${q(r.summary)}`);
        has(r.stderr, 'ci-analyze: started analysis a-test-1 of my-repo (baseline 1111111, command node-test)', r.debug);
        const post = srv.requests[0];
        eq(post.method, 'POST');
        eq(post.headers.authorization, 'Bearer ' + KEY);
        deq(JSON.parse(post.body), { repo: 'my-repo', baseline: '1111111', command_id: 'node-test' });
        eq(srv.state.gets, 2, r.debug);
        // Local runs (GITHUB_ACTIONS unset) carry no stop-commands lines.
        deq(workflowMarkers(r.stdout + r.stderr, r.debug), []);
      });
    });

    test('exit 1: a missing input is a usage error and nothing is sent', async () => {
      await withServer({ gets: [done()] }, async (srv) => {
        const r = await runner.run({ url: srv.url, env: { REPO: undefined } });
        eq(r.code, 1, r.debug);
        eq(r.stderr.trim(), 'ci-analyze: REPO is required', r.debug);
        eq(srv.state.connections, 0, r.debug);
      });
    });

    test('exit 2: an analysis that ends with status error prints its error', async () => {
      const failed = { status: 'error', error: 'baseline run failed: git worktree add exited 128', model: null, current_run: null };
      await withServer({ gets: [done(failed)] }, async (srv) => {
        const r = await runner.run({ url: srv.url });
        eq(r.code, 2, r.debug);
        const printed = lines(printedSummary(r.stdout));
        deq(
          printed.slice(4, 8),
          [
            '- Status: **error**',
            '- Error: baseline run failed: git worktree add exited 128',
            '- Baseline `1111111`: PASS (exit 0)',
            '- HEAD `2222222`: not run',
          ],
          r.debug,
        );
      });
    });

    test('exit 3: an analysis still running at TIMEOUT_SECONDS times out', async () => {
      await withServer({ gets: [{ status: 200, json: { id: ID, status: 'running' } }] }, async (srv) => {
        const r = await runner.run({ url: srv.url, env: { TIMEOUT_SECONDS: '0' } });
        eq(r.code, 3, r.debug);
        eq(printedSummary(r.stdout), 'RegressionMap: timed out after 0s waiting for analysis `a-test-1`.', r.debug);
        has(r.stderr, 'ci-analyze: timed out after 0s waiting for analysis a-test-1 (it keeps running on the server)', r.debug);
      });
    });

    test('exit 4: a 401 prints the status and the API error as plain text', async () => {
      const post = err(401, 'unauthorized', 'a valid API key is required: Authorization: Bearer <key>', { 'WWW-Authenticate': 'Bearer' });
      await withServer({ post }, async (srv) => {
        const r = await runner.run({ url: srv.url });
        eq(r.code, 4, r.debug);
        eq(r.stderr.trim(), 'ci-analyze: HTTP 401 unauthorized: a valid API key is required: Authorization: Bearer <key>', r.debug);
        eq(r.stdout, '', r.debug);
      });
    });

    test('exit 4: a redirect is not followed and reports its HTTP status', async () => {
      const post = { status: 302, raw: '', headers: { Location: 'http://127.0.0.1:9/elsewhere' } };
      await withServer({ post }, async (srv) => {
        const r = await runner.run({ url: srv.url });
        eq(r.code, 4, r.debug);
        eq(r.stderr.trim(), 'ci-analyze: HTTP 302 error', r.debug);
        eq(srv.requests.length, 1, r.debug);
      });
    });

    test('exit 4: an unreachable server is a network error', async () => {
      const port = await freePort();
      const r = await runner.run({ url: `http://127.0.0.1:${port}` });
      eq(r.code, 4, r.debug);
      has(r.stderr, 'ci-analyze: POST /api/analyses failed: ', r.debug);
    });

    test('exit 4: a 404 while polling prints the API error', async () => {
      await withServer({ gets: [err(404, 'not_found', 'analysis not found')] }, async (srv) => {
        const r = await runner.run({ url: srv.url });
        eq(r.code, 4, r.debug);
        has(r.stderr, 'ci-analyze: HTTP 404 not_found: analysis not found', r.debug);
      });
    });

    test('exit 4: a server that echoes the key and the notify URL gets both redacted', async () => {
      const post = err(400, 'bad_request', `you sent Bearer ${KEY} and ${NOTIFY}`);
      await withServer({ post }, async (srv) => {
        const r = await runner.run({ url: srv.url, env: { NOTIFY_URL: NOTIFY, NOTIFY_FORMAT: 'slack' } });
        eq(r.code, 4, r.debug);
        eq(r.stderr.trim(), 'ci-analyze: HTTP 400 bad_request: you sent Bearer [redacted] and [redacted]', r.debug);
        lacks(r.stderr, 'NOTIFYSECRET', r.debug);
        deq(JSON.parse(srv.requests[0].body), {
          repo: 'my-repo',
          baseline: '1111111',
          command_id: 'node-test',
          notify_url: NOTIFY,
          notify_format: 'slack',
        });
      });
    });

    test('exit 4: a 429 prints its Retry-After value', async () => {
      const post = err(429, 'rate_limited', 'analysis rate limit for this key reached; retry later', { 'Retry-After': '1234' });
      await withServer({ post }, async (srv) => {
        const r = await runner.run({ url: srv.url });
        eq(r.code, 4, r.debug);
        deq(
          lines(r.stderr.trim()),
          ['ci-analyze: HTTP 429 rate_limited: analysis rate limit for this key reached; retry later', 'ci-analyze: Retry-After: 1234 s'],
          r.debug,
        );
      });
    });

    test('exit 4: a 429 without Retry-After says so', async () => {
      const post = err(429, 'too_many_analyses', 'at most 2 analyses can run at once');
      await withServer({ post }, async (srv) => {
        const r = await runner.run({ url: srv.url });
        eq(r.code, 4, r.debug);
        deq(
          lines(r.stderr.trim()),
          ['ci-analyze: HTTP 429 too_many_analyses: at most 2 analyses can run at once', 'ci-analyze: Retry-After: not given'],
          r.debug,
        );
      });
    });

    test('exit 4: an analysis id that is not exactly a[0-9a-z-]{1,40} is refused', async () => {
      // A trailing newline passes a regex anchored with a .NET '$'.
      const post = { status: 202, json: { id: ID + '\n', status: 'running' } };
      await withServer({ post, gets: [done()] }, async (srv) => {
        const r = await runner.run({ url: srv.url });
        eq(r.code, 4, r.debug);
        eq(r.stderr.trim(), 'ci-analyze: the server returned no valid analysis id', r.debug);
        eq(srv.requests.length, 1, r.debug);
      });
    });

    // ---- REGRESSIONMAP_URL rule -----------------------------------------
    const rejected = [
      ['userinfo before a loopback host', (port) => `http://localhost:${port}@127.0.0.1:${port}`, URL_AT],
      ['userinfo before a non-loopback host name', (port) => `http://127.0.0.1:x@0x7f000001:${port}`, URL_AT],
      ['userinfo with https', (port) => `https://user:pass@127.0.0.1:${port}`, URL_AT],
      ['a backslash in the authority', (port) => `http://localhost:${port}\\@127.0.0.1:${port}`, URL_AT],
      ['http to a non-loopback host', (port) => `http://0x7f000001:${port}`, URL_RULE],
      ['http to a loopback look-alike', (port) => `http://127.0.0.1.example:${port}`, URL_RULE],
      ['a port with a non-digit', (port) => `http://127.0.0.1:${port}x`, URL_RULE],
      ['an empty port', () => 'http://localhost:', URL_RULE],
      ['an upper-case scheme', (port) => `HTTP://127.0.0.1:${port}`, URL_RULE],
      ['an upper-case loopback name', (port) => `http://LOCALHOST:${port}`, URL_RULE],
      ['another scheme', (port) => `ftp://127.0.0.1:${port}`, URL_RULE],
      // curl would read this as https://127.0.0.1:<port>; the rule sees no host.
      ['no host', (port) => `https:///127.0.0.1:${port}`, 'ci-analyze: REGRESSIONMAP_URL has no host'],
    ];
    for (const [name, make, message] of rejected) {
      test(`exit 1: REGRESSIONMAP_URL with ${name} is refused before any connection`, async () => {
        await withServer({ gets: [done()] }, async (srv) => {
          const r = await runner.run({ url: make(srv.port) });
          eq(r.code, 1, r.debug);
          eq(r.stderr.trim(), message, r.debug);
          eq(srv.state.connections, 0, r.debug);
        });
      });
    }

    for (const [name, make] of [
      ['http://localhost:<port>', (port) => `http://localhost:${port}`],
      ['http://127.0.0.1:<port>/ (trailing slash)', (port) => `http://127.0.0.1:${port}/`],
      ['http://127.0.0.1:<port>// (trailing slashes)', (port) => `http://127.0.0.1:${port}//`],
    ]) {
      test(`exit 0: plain http to loopback is accepted: ${name}`, async () => {
        await withServer({ gets: [done()] }, async (srv) => {
          const r = await runner.run({ url: make(srv.port) });
          eq(r.code, 0, r.debug);
          eq(srv.requests[0].url, '/api/analyses', r.debug);
        });
      });
    }

    test('exit 0: plain http to [::1] is accepted', async (t) => {
      let srv;
      try {
        srv = await startServer({ gets: [done()], host: '::1' });
      } catch (e) {
        t.skip(`no IPv6 loopback here (${e.code})`);
        return;
      }
      try {
        const r = await runner.run({ url: `http://[::1]:${srv.port}` });
        eq(r.code, 0, r.debug);
      } finally {
        await srv.close();
      }
    });

    test('exit 4: https passes the URL rule (the TLS handshake then fails against the plain fake server)', async () => {
      await withServer({ gets: [done()] }, async (srv) => {
        const r = await runner.run({ url: `https://127.0.0.1:${srv.port}` });
        eq(r.code, 4, r.debug);
        assert.ok(srv.state.connections >= 1, `no connection was made: ${r.debug}`);
        eq(srv.requests.length, 0, r.debug);
      });
    });

    // ---- leading zeros ---------------------------------------------------
    for (const [name, value, message] of [
      ['TIMEOUT_SECONDS', '08', 'ci-analyze: TIMEOUT_SECONDS must be a whole number of seconds, without leading zeros'],
      ['TIMEOUT_SECONDS', '010', 'ci-analyze: TIMEOUT_SECONDS must be a whole number of seconds, without leading zeros'],
      ['TIMEOUT_SECONDS', '00', 'ci-analyze: TIMEOUT_SECONDS must be a whole number of seconds, without leading zeros'],
      ['POLL_SECONDS', '0', 'ci-analyze: POLL_SECONDS must be a whole number above 0, without leading zeros'],
      ['POLL_SECONDS', '00', 'ci-analyze: POLL_SECONDS must be a whole number above 0, without leading zeros'],
      ['POLL_SECONDS', '05', 'ci-analyze: POLL_SECONDS must be a whole number above 0, without leading zeros'],
    ]) {
      test(`exit 1: ${name}=${value} is a usage error`, async () => {
        await withServer({ gets: [done()] }, async (srv) => {
          const r = await runner.run({ url: srv.url, env: { [name]: value } });
          eq(r.code, 1, r.debug);
          eq(r.stderr.trim(), message, r.debug);
          eq(srv.state.connections, 0, r.debug);
        });
      });
    }

    // ---- untrusted text --------------------------------------------------
    test('model text cannot add workflow commands, links, images, HTML or control characters', async () => {
      await withServer({ gets: [{ status: 200, json: hostileTextAnalysis() }] }, async (srv) => {
        const r = await runner.run({ url: srv.url });
        eq(r.code, 0, r.debug);
        eq(printedSummary(r.stdout), HOSTILE_TEXT_SUMMARY, `stdout differs: ${r.debug}`);
        eq(r.summary, 'PRE-EXISTING LINE\n' + HOSTILE_TEXT_SUMMARY + '\n', `summary file differs: ${q(r.summary)}`);
        for (const text of [r.stdout, r.stderr, r.summary]) {
          deq(workflowMarkers(text, r.debug), []);
          assertNoControlChars(text, r.debug);
        }
      });
    });

    test('in GitHub Actions, output that carries server data is fenced with a random stop-commands token', async () => {
      await withServer({ gets: [{ status: 200, json: hostileTextAnalysis() }] }, async (srv) => {
        const r = await runner.run({ url: srv.url, env: { GITHUB_ACTIONS: 'true' } });
        eq(r.code, 0, r.debug);
        const out = fenced(r.stdout, r.debug);
        eq(out.inner.join('\n'), HOSTILE_TEXT_SUMMARY.replace(/\n\n+/g, '\n'), r.debug);
        const err = fenced(r.stderr, r.debug);
        deq(err.inner, ['ci-analyze: started analysis a-test-1 of my-repo (baseline 1111111, command node-test)'], r.debug);
        deq(workflowMarkers(r.stdout, r.debug), ['::stop-commands::' + out.token, `::${out.token}::`]);
        // The job summary is Markdown, not a log: no stop-commands lines there.
        eq(r.summary, 'PRE-EXISTING LINE\n' + HOSTILE_TEXT_SUMMARY + '\n', `summary file differs: ${q(r.summary)}`);
      });
    });

    test('an API error message on stderr is plain text: no HTML entities, no commands, no control characters', async () => {
      const post = err(400, 'bad_request', '::error::x\n##[warning]y <b>&amp; café\u001b[31m');
      await withServer({ post }, async (srv) => {
        const r = await runner.run({ url: srv.url, env: { GITHUB_ACTIONS: 'true' } });
        eq(r.code, 4, r.debug);
        const e = fenced(r.stderr, r.debug);
        deq(e.inner, [`ci-analyze: HTTP 400 bad_request: :${ZW}:error:${ZW}:x #${ZW}#[warning]y <b>&amp; café[31m`], r.debug);
        assertNoControlChars(r.stderr, r.debug);
      });
    });

    // ---- numeric fields and hypothesis shapes ------------------------------
    test('fields that should be integers print "?" when they are not, and non-object hypotheses are skipped', async () => {
      await withServer({ gets: [{ status: 200, json: hostileTypesAnalysis() }] }, async (srv) => {
        const r = await runner.run({ url: srv.url });
        eq(r.code, 0, r.debug);
        eq(printedSummary(r.stdout), HOSTILE_TYPES_SUMMARY, `stdout differs: ${r.debug}`);
        deq(workflowMarkers(r.stdout + r.stderr + r.summary, r.debug), []);
        lacks(r.summary, '<img', r.debug);
      });
    });

    test('hypotheses [null] gives "No valid hypotheses." and a complete summary', async () => {
      const a = analysis();
      a.model.result.hypotheses = [null];
      await withServer({ gets: [{ status: 200, json: a }] }, async (srv) => {
        const r = await runner.run({ url: srv.url });
        eq(r.code, 0, r.debug);
        const expected = [
          ...lines(BASE_SUMMARY).slice(0, 8),
          ...HYPOTHESES_HEADING,
          'No valid hypotheses.',
          '',
          'Rejected by validation: 1',
        ].join('\n');
        eq(printedSummary(r.stdout), expected, r.debug);
      });
    });

    // ---- notification ----------------------------------------------------
    test('a pending notification is polled every 2 s until it settles', async () => {
      const gets = [
        done({ notification: notification() }),
        done({ notification: notification() }),
        done({ notification: notification({ status: 'delivered', attempts: 1, last_status_code: 200 }) }),
      ];
      await withServer({ gets }, async (srv) => {
        const r = await runner.run({ url: srv.url });
        eq(r.code, 0, r.debug);
        has(r.stdout, '\n- Notification: delivered to hooks.example.com (json, 1 attempt(s))\n', r.debug);
        has(r.stderr, 'ci-analyze: the analysis finished; waiting up to 30s for its notification to be delivered', r.debug);
        eq(srv.state.gets, 3, r.debug);
        const at = srv.requests.filter((x) => x.method === 'GET').map((x) => x.at);
        for (let i = 1; i < at.length; i++) assert.ok(at[i] - at[i - 1] >= 1800, `GETs ${at[i] - at[i - 1]} ms apart: ${r.debug}`);
      });
    });

    test('a notification that stays pending is reported as still pending after about 30 s', async () => {
      await withServer({ gets: [done({ status: 'error', error: 'boom', notification: notification() })] }, async (srv) => {
        const r = await runner.run({ url: srv.url });
        eq(r.code, 2, r.debug);
        has(r.stdout, '\n- Notification: still pending to hooks.example.com (json, 0 attempt(s))\n', r.debug);
        assert.ok(r.ms >= 29000 && r.ms < 50000, `ran ${r.ms} ms: ${r.debug}`);
        assert.ok(srv.state.gets >= 12 && srv.state.gets <= 17, `${srv.state.gets} GETs: ${r.debug}`);
      });
    });

    test('a settled notification is printed without waiting', async () => {
      const n = notification({ status: 'failed', attempts: 3, last_status_code: 503, last_error: 'receiver returned HTTP 503' });
      await withServer({ gets: [done({ notification: n })] }, async (srv) => {
        const r = await runner.run({ url: srv.url });
        eq(r.code, 0, r.debug);
        has(r.stdout, '\n- Notification: failed to hooks.example.com (json, 3 attempt(s), receiver returned HTTP 503)\n', r.debug);
        eq(srv.state.gets, 1, r.debug);
        lacks(r.stderr, 'waiting up to 30s', r.debug);
      });
    });

    // ---- HEAD check ------------------------------------------------------
    test('a warning is printed when the analysed HEAD is not GITHUB_SHA; the exit code stays 0', async () => {
      await withServer({ gets: [done()] }, async (srv) => {
        const r = await runner.run({ url: srv.url, env: { GITHUB_SHA: 'c'.repeat(40) } });
        eq(r.code, 0, r.debug);
        const printed = lines(printedSummary(r.stdout));
        const head = printed.indexOf('- HEAD `2222222`: FAIL (exit 1)');
        assert.ok(head > 0, r.debug);
        eq(
          printed[head + 1],
          "- Warning: the server analysed `2222222`, not `ccccccc` (GITHUB_SHA): the server's clone may lag behind CI.",
          r.debug,
        );
        has(r.summary, "the server's clone may lag behind CI", r.debug);
      });
    });

    test('no warning when GITHUB_SHA is the analysed HEAD', async () => {
      await withServer({ gets: [done()] }, async (srv) => {
        const r = await runner.run({ url: srv.url, env: { GITHUB_SHA: analysis().current.toUpperCase() } });
        eq(r.code, 0, r.debug);
        eq(printedSummary(r.stdout), BASE_SUMMARY, r.debug);
      });
    });

    // ---- encoding ----------------------------------------------------------
    test('UTF-8 in an error body reaches stderr intact', async () => {
      const post = err(400, 'invalid_repo', 'dépôt « café » — introuvable …');
      await withServer({ post }, async (srv) => {
        const r = await runner.run({ url: srv.url });
        eq(r.code, 4, r.debug);
        eq(r.stderr.trim(), 'ci-analyze: HTTP 400 invalid_repo: dépôt « café » — introuvable …', r.debug);
      });
    });

    test('hypothesis titles keep their order and ranks', async () => {
      const a = analysis();
      a.model.result.hypotheses = [hypothesis({ rank: 3, title: 'third' }), hypothesis({ rank: 1, title: 'first' })];
      await withServer({ gets: [{ status: 200, json: a }] }, async (srv) => {
        const r = await runner.run({ url: srv.url });
        eq(r.code, 0, r.debug);
        const ranks = lines(printedSummary(r.stdout)).filter((l) => /^\d+\. \*\*/.test(l));
        deq(
          ranks.map((l) => l.slice(0, l.indexOf('**', 5) + 2)),
          ['3. **third**', '1. **first**'],
          r.debug,
        );
      });
    });
  });
}
