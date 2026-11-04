// scripts/ci-analyze.sh against the fake server: the shared cases, plus the
// cases that only apply to the sh script (curl configuration, signals).
import { describe, test } from 'node:test';
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { defineCases } from './cases.mjs';
import { startServer, ID } from './fake-server.mjs';
import { SH, KEY, q, eq, deq, has, lacks, shRunner, repoRoot, analysis } from './harness.mjs';

const runner = shRunner();
defineCases(runner);

const forward = (p) => (process.platform === 'win32' ? p.replace(/\\/g, '/') : p);

describe(`ci-analyze.sh only (${SH})`, { concurrency: 3 }, () => {
  test('a curlrc with "verbose" is ignored, so the Authorization header is never printed', async () => {
    const srv = await startServer({ gets: [{ status: 200, json: analysis() }] });
    try {
      const r = await runner.run({
        url: srv.url,
        prepare: ({ dir, p }) => {
          const home = path.join(dir, 'planted');
          fs.mkdirSync(home);
          for (const name of ['.curlrc', '_curlrc']) fs.writeFileSync(path.join(home, name), 'verbose\n');
          return { CURL_HOME: p(home), HOME: p(home) };
        },
      });
      eq(r.code, 0, r.debug);
      lacks(r.stderr, '> Authorization', r.debug);
      lacks(r.stderr, '> POST', r.debug);
    } finally {
      await srv.close();
    }
  });

  // The script is started in the background by a wrapper sh, which waits
  // until the private directory exists, then sends the signal with the
  // shell's own kill (on Windows a Node kill would not be a POSIX signal).
  for (const [sig, expected] of [
    ['HUP', 129],
    ['PIPE', 141],
    ['TERM', 143],
  ]) {
    test(`SIG${sig} removes the private directory that holds the key (exit ${expected})`, async () => {
      const srv = await startServer({ gets: [{ status: 200, json: { id: ID, status: 'running' } }] });
      const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rm-ci-sig-'));
      try {
        const tmp = path.join(dir, 'tmp');
        fs.mkdirSync(tmp);
        const wrapper = [
          '"$RM_SH" "$RM_SCRIPT" >"$RM_OUT" 2>"$RM_ERR" &',
          'pid=$!',
          'n=0',
          'while :; do',
          '  set -- "$TMPDIR"/regressionmap.*/request.json',
          '  [ -f "$1" ] && break',
          '  n=$((n + 1)); if [ "$n" -gt 300 ]; then kill "$pid"; echo "no private directory"; exit 99; fi',
          '  sleep 0.1',
          'done',
          'sleep 1',
          `kill -${sig} "$pid"`,
          'wait "$pid"',
          'echo "rc=$?"',
        ].join('\n');
        const env = { ...process.env };
        for (const k of Object.keys(env)) if (/^(GITHUB_|RUNNER_|REGRESSIONMAP_|NOTIFY_)/i.test(k)) delete env[k];
        Object.assign(env, {
          RM_SH: SH,
          RM_SCRIPT:forward(path.join(repoRoot, 'scripts', 'ci-analyze.sh')),
          RM_OUT: forward(path.join(dir, 'out')),
          RM_ERR: forward(path.join(dir, 'err')),
          TMPDIR: forward(tmp),
          REGRESSIONMAP_URL: srv.url,
          REGRESSIONMAP_KEY: KEY,
          REPO: 'my-repo',
          BASELINE: '1111111',
          POLL_SECONDS: '1',
          TIMEOUT_SECONDS: '120',
        });
        const out = await new Promise((resolve, reject) => {
          const child = spawn(SH, ['-c', wrapper], { env, stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true });
          let text = '';
          child.stdout.on('data', (c) => (text += c));
          child.stderr.on('data', (c) => (text += c));
          const timer = setTimeout(() => child.kill(), 60000);
          child.on('error', reject);
          child.on('close', () => {
            clearTimeout(timer);
            resolve(text);
          });
        });
        const errText = fs.existsSync(path.join(dir, 'err')) ? fs.readFileSync(path.join(dir, 'err'), 'utf8') : '';
        has(out, `rc=${expected}`, `wrapper ${q(out)}; script stderr ${q(errText)}`);
        deq(fs.readdirSync(tmp), [], `the private directory was left behind; wrapper ${q(out)}`);
        lacks(out + errText, KEY);
      } finally {
        await srv.close();
        fs.rmSync(dir, { recursive: true, force: true });
      }
    });
  }
});
