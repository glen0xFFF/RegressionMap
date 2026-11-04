// scripts/ci-analyze.ps1 against the fake server: the shared cases, run with
// powershell.exe when present, else pwsh, else skipped with the reason shown.
// The parity cases run both scripts on the same answer and compare outputs.
import { describe, test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { defineCases } from './cases.mjs';
import { startServer } from './fake-server.mjs';
import {
  q,
  eq,
  repoRoot,
  psRunner,
  shRunner,
  analysis,
  notification,
  hostileTextAnalysis,
  hostileTypesAnalysis,
  printedSummary,
} from './harness.mjs';

const ps = psRunner();
const sh = shRunner();
defineCases(ps);

// Windows PowerShell 5.1 reads a .ps1 without a byte order mark as ANSI, so
// any non-ASCII byte in the script (a literal BOM in a regex, an em dash in a
// string) silently becomes other characters. Runs without PowerShell too.
test('ci-analyze.ps1 is plain ASCII', () => {
  const bytes = fs.readFileSync(path.join(repoRoot, 'scripts', 'ci-analyze.ps1'));
  const at = bytes.findIndex((b) => b > 0x7e || (b < 0x20 && b !== 0x0a && b !== 0x0d && b !== 0x09));
  const line = at < 0 ? 0 : bytes.subarray(0, at).toString('latin1').split('\n').length;
  assert.ok(at < 0, `byte 0x${at < 0 ? '' : bytes[at].toString(16)} at line ${line}`);
});

const fixtures = [
  ['a plain analysis', () => analysis({ notification: notification({ status: 'delivered', attempts: 1 }) })],
  ['hostile model text', hostileTextAnalysis],
  ['fields of the wrong type', hostileTypesAnalysis],
];

describe('parity: ci-analyze.sh and ci-analyze.ps1', ps.available ? { concurrency: 3 } : { skip: ps.skipReason }, () => {
  for (const [name, make] of fixtures) {
    test(`same exit code, stdout and job summary for ${name}`, async () => {
      const srv = await startServer({ gets: [{ status: 200, json: make() }] });
      try {
        const env = { GITHUB_ACTIONS: 'true', GITHUB_SHA: 'd'.repeat(40) };
        const [a, b] = await Promise.all([sh.run({ url: srv.url, env }), ps.run({ url: srv.url, env })]);
        eq(a.code, b.code, `sh ${a.debug}; ps1 ${b.debug}`);
        eq(b.summary, a.summary, `summary files differ: sh ${q(a.summary)}; ps1 ${q(b.summary)}`);
        eq(printedSummary(b.stdout), printedSummary(a.stdout), `stdout differs: sh ${a.debug}; ps1 ${b.debug}`);
      } finally {
        await srv.close();
      }
    });
  }
});
