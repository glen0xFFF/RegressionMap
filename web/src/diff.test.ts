import { describe, expect, it } from "vitest";
import { anchorFindings, anchorHypotheses, changesTable, countDiffLines, groupDiffsByFile, parseHunk, redactPath } from "./diff";
import type { Evidence, Hypothesis, ReviewFinding } from "./types";

const HUNK = [
  "@@ -3,4 +3,5 @@ function page(items, size) {",
  "   const start = 0;",
  "-  const end = size;",
  "+  const end = size + 1;",
  "+  // extra",
  "   return items.slice(start, end);",
  " }",
].join("\n");

describe("countDiffLines", () => {
  it("counts + and - lines, ignoring file headers and hunk headers", () => {
    expect(countDiffLines(HUNK)).toEqual({ add: 2, del: 1 });
    expect(countDiffLines("--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b")).toEqual({ add: 1, del: 1 });
    expect(countDiffLines("")).toEqual({ add: 0, del: 0 });
  });
});

describe("parseHunk", () => {
  it("assigns old/new line numbers from the hunk header", () => {
    const rows = parseHunk(HUNK);
    expect(rows[0]).toMatchObject({ kind: "hunk", oldNo: null, newNo: null });
    expect(rows[1]).toMatchObject({ kind: "ctx", oldNo: 3, newNo: 3, text: "  const start = 0;" });
    expect(rows[2]).toMatchObject({ kind: "del", oldNo: 4, newNo: null, text: "  const end = size;" });
    expect(rows[3]).toMatchObject({ kind: "add", oldNo: null, newNo: 4, text: "  const end = size + 1;" });
    expect(rows[4]).toMatchObject({ kind: "add", oldNo: null, newNo: 5 });
    expect(rows[5]).toMatchObject({ kind: "ctx", oldNo: 5, newNo: 6 });
    expect(rows[6]).toMatchObject({ kind: "ctx", oldNo: 6, newNo: 7 });
  });

  it("leaves line numbers null when there is no parsable hunk header", () => {
    const rows = parseHunk("+a\n-b");
    expect(rows.map((r) => [r.kind, r.oldNo, r.newNo])).toEqual([
      ["add", null, null],
      ["del", null, null],
    ]);
  });

  it("handles single-line hunk headers without counts", () => {
    const rows = parseHunk("@@ -7 +9 @@\n-x\n+y");
    expect(rows[1]).toMatchObject({ kind: "del", oldNo: 7 });
    expect(rows[2]).toMatchObject({ kind: "add", newNo: 9 });
  });
});

const ev = (id: string, file: string, excerpt: string, type: Evidence["type"] = "diff_hunk"): Evidence => ({
  id, type, title: id, file, line_start: 0, line_end: 0, excerpt,
});

describe("changesTable", () => {
  it("one row per changed file with status and +/- counts from diff_hunk evidence only", () => {
    const rows = changesTable(
      [{ path: "src/p.js", status: "M" }, { path: "README.md", status: "A" }],
      [ev("E1", "src/p.js", HUNK), ev("E2", "src/p.js", "@@ -20 +21 @@\n-q"), ev("E3", "src/p.js", "+not a diff", "source")],
    );
    expect(rows).toEqual([
      { path: "src/p.js", status: "M", add: 2, del: 2, hunks: 2 },
      { path: "README.md", status: "A", add: 0, del: 0, hunks: 0 },
    ]);
  });
  it("handles missing inputs", () => {
    expect(changesTable(null, null)).toEqual([]);
  });
  it("joins a hunk whose path has a [REDACTED] segment to the one changed file it fits", () => {
    const rows = changesTable(
      [{ path: "packages/sk-components-library/src/button.ts", status: "M" }, { path: "src/p.js", status: "M" }],
      [ev("E1", "packages/[REDACTED]/src/button.ts", HUNK), ev("E2", "src/p.js", "-q")],
    );
    expect(rows).toEqual([
      { path: "packages/sk-components-library/src/button.ts", status: "M", add: 2, del: 1, hunks: 1 },
      { path: "src/p.js", status: "M", add: 0, del: 1, hunks: 1 },
    ]);
  });
  it("joins a path with two redacted segments only to the changed file that redacts to it", () => {
    const rows = changesTable(
      [
        { path: "a/AKIAABCDEFGHIJKLMNOP/b/ghp_abcdefghijklmnopqrstuvwxyz0123/c.ts", status: "M" },
        { path: "a/AKIAABCDEFGHIJKLMNOP/b/c.ts", status: "M" },
        { path: "a/AKIAABCDEFGHIJKLMNOP/b/ghp_abcdefghijklmnopqrstuvwxyz0123/extra/c.ts", status: "A" },
      ],
      [ev("E1", "a/[REDACTED]/b/[REDACTED]/c.ts", "+x\n+y")],
    );
    expect(rows.map((r) => r.hunks)).toEqual([1, 0, 0]);
    expect(rows[0]).toMatchObject({ add: 2, del: 0 });
  });
  it("joins a hunk whose path segment was redacted only in part", () => {
    const files = [
      { path: "config/token=supersecretvalue123/app.ts", status: "M" },
      { path: "sk-abcdefghijklmnopqrstuv.ts", status: "A" },
      { path: "lib/api_key=abcdef/x.ts", status: "M" },
      { path: "src/other.ts", status: "M" },
    ];
    const rows = changesTable(files, [
      ev("E1", "config/token=[REDACTED]/app.ts", "+x"),
      ev("E2", "[REDACTED].ts", "+y"),
      ev("E3", "lib/api_key=[REDACTED]/x.ts", "-z"),
    ]);
    expect(rows.map((r) => r.hunks)).toEqual([1, 1, 1, 0]);
  });
  it("redacts each segment on its own, so a redacted path keeps the changed path's segments", () => {
    const rows = changesTable(
      [{ path: "cfg/token=abcd/efgh/x.ts", status: "M" }, { path: "src/sk-abcdefghijklmnopqrstuv/a.ts", status: "M" }],
      [ev("E1", "cfg/token=[REDACTED]", "+x"), ev("E2", "src/[REDACTED]", "+y"), ev("E3", "cfg/token=[REDACTED]/efgh/x.ts", "-z")],
    );
    expect(rows).toEqual([
      { path: "cfg/token=abcd/efgh/x.ts", status: "M", add: 0, del: 1, hunks: 1 },
      { path: "src/sk-abcdefghijklmnopqrstuv/a.ts", status: "M", add: 0, del: 0, hunks: 0 },
    ]);
  });
  it("does not give a redacted hunk to a changed file whose segment the server would not redact", () => {
    const rows = changesTable([{ path: "src/lib/a.ts", status: "M" }], [ev("E1", "src/[REDACTED]/a.ts", "+x")]);
    expect(rows[0].hunks).toBe(0);
  });
  it("gives the hunk to no file when the redacted path fits more than one changed file", () => {
    const rows = changesTable(
      [{ path: "keys/AKIAAAAAAAAAAAAAAAAA/x.ts", status: "M" }, { path: "keys/AKIABBBBBBBBBBBBBBBB/x.ts", status: "M" }],
      [ev("E1", "keys/[REDACTED]/x.ts", "+x")],
    );
    expect(rows.map((r) => r.hunks)).toEqual([0, 0]);
  });
  it("prefers an exact path match over a redacted match", () => {
    const rows = changesTable(
      [{ path: "docs/[REDACTED]/a.md", status: "M" }, { path: "docs/secret/a.md", status: "M" }],
      [ev("E1", "docs/[REDACTED]/a.md", "+x"), ev("E2", "docs/secret/a.md", "-y")],
    );
    expect(rows).toEqual([
      { path: "docs/[REDACTED]/a.md", status: "M", add: 1, del: 0, hunks: 1 },
      { path: "docs/secret/a.md", status: "M", add: 0, del: 1, hunks: 1 },
    ]);
  });
});

describe("redactPath", () => {
  // The cases of TestRedactPathMatchesBuildFile in backend/internal/evidence/evidence_test.go,
  // plus the sk- package name and the bearer rule.
  it("redacts a path the way the server's RedactPath does", () => {
    const cases: Record<string, string> = {
      "src/app.ts": "src/app.ts",
      "config/token=supersecretvalue123/app.ts": "config/token=[REDACTED]/app.ts",
      "AKIAABCDEFGHIJKLMNOP/x.ts": "[REDACTED]/x.ts",
      "keys/ghp_abcdefghijklmnopqrstuvwxyz0123/k.ts": "keys/[REDACTED]/k.ts",
      "sk-abcdefghijklmnopqrstuv.ts": "[REDACTED].ts",
      "packages/sk-components-library/src/button.ts": "packages/[REDACTED]/src/button.ts",
      "lib/Bearer abcdefgh.ts/x": "lib/Bearer [REDACTED]/x",
      "": "",
    };
    for (const [raw, want] of Object.entries(cases)) expect(redactPath(raw), raw).toBe(want);
  });
});

describe("groupDiffsByFile", () => {
  it("groups diff_hunk evidence by file preserving first-seen order", () => {
    const groups = groupDiffsByFile([ev("E1", "b.js", "+x"), ev("E2", "a.js", "-y"), ev("E3", "b.js", "+z"), ev("E4", "", "x", "test_failure")]);
    expect(groups.map((g) => [g.file, g.hunks.map((h) => h.id), g.add, g.del])).toEqual([
      ["b.js", ["E1", "E3"], 2, 0],
      ["a.js", ["E2"], 0, 1],
    ]);
  });
});

const hyp = (rank: number, file: string, line_start: number, line_end = line_start): Hypothesis => ({
  rank, title: `h${rank}`, file, line_start, line_end, reason: "", supporting_evidence_ids: [],
  counter_evidence_ids: [], verification_suggestion: "", confidence_label: "low",
});

describe("anchorHypotheses", () => {
  const hunks = [ev("E1", "src/p.js", HUNK), ev("E2", "src/q.js", "@@ -1 +1 @@\n-a\n+b")];

  it("anchors a hypothesis under the row whose new line number equals line_start", () => {
    const a = anchorHypotheses([hyp(1, "src/p.js", 4)], hunks);
    expect(a.rows.get("E1")?.get(3)?.map((h) => h.rank)).toEqual([1]);
    expect(a.fileLevel.get("src/p.js") ?? []).toEqual([]);
  });

  it("anchors on the final context row of the hunk", () => {
    // line 6 is a context row at index 5; line 7 at index 6
    const a = anchorHypotheses([hyp(2, "src/p.js", 7)], hunks);
    expect(a.rows.get("E1")?.get(6)?.map((h) => h.rank)).toEqual([2]);
  });

  it("puts 0/0 hypotheses and out-of-range lines under the file header", () => {
    const a = anchorHypotheses([hyp(1, "src/p.js", 0, 0), hyp(2, "src/p.js", 99)], hunks);
    expect(a.fileLevel.get("src/p.js")?.map((h) => h.rank)).toEqual([1, 2]);
    expect(a.rows.get("E1")?.size ?? 0).toBe(0);
  });

  it("ignores hypotheses for files without diff hunks and orders anchors by rank", () => {
    const a = anchorHypotheses([hyp(3, "src/q.js", 1), hyp(1, "src/q.js", 1), hyp(2, "other.js", 1)], hunks);
    expect(a.rows.get("E2")?.get(2)?.map((h) => h.rank)).toEqual([1, 3]);
    expect(a.fileLevel.get("other.js")).toBeUndefined();
  });

  it("anchors a ranged hypothesis under the last changed row inside its range", () => {
    // range 3-7 covers the whole hunk; changed rows are 2 (del), 3 (+ L4), 4 (+ L5)
    const a = anchorHypotheses([hyp(1, "src/p.js", 3, 7)], hunks);
    expect(a.rows.get("E1")?.get(4)?.map((h) => h.rank)).toEqual([1]);
  });

  it("anchors on a deleted row when only a deletion falls in the range", () => {
    const delOnly = [ev("E9", "d.js", "@@ -1,3 +1,2 @@\n a\n-b\n c")];
    const a = anchorHypotheses([hyp(1, "d.js", 1, 2)], delOnly);
    expect(a.rows.get("E9")?.get(2)?.map((h) => h.rank)).toEqual([1]);
  });

  it("handles missing hypotheses", () => {
    const a = anchorHypotheses(undefined, hunks);
    expect(a.rows.size).toBe(0);
    expect(a.fileLevel.size).toBe(0);
  });
});

const fnd = (id: string, file: string, line_start: number, line_end = line_start): ReviewFinding => ({
  id, lens: "code", severity: "high", title: id, detail: "", suggestion: "", file, line_start, line_end,
  evidence_ids: [], check: { verdict: "confirmed", reason: "", evidence_ids: [] },
});

describe("anchorFindings", () => {
  const hunks = [ev("E1", "src/p.js", HUNK), ev("E2", "src/q.js", "@@ -1 +1 @@\n-a\n+b")];

  it("anchorFindings places findings on their rows", () => {
    const h = anchorHypotheses([hyp(1, "src/p.js", 7)], hunks);
    const f = anchorFindings([fnd("R1", "src/p.js", 7)], hunks);
    expect([...(h.rows.get("E1")?.keys() ?? [])]).toEqual([6]);
    expect([...(f.rows.get("E1")?.keys() ?? [])]).toEqual([6]);
    expect(f.rows.get("E1")?.get(6)?.map((x) => x.id)).toEqual(["R1"]);
    expect(f.fileLevel.size).toBe(0);
  });

  it("anchorFindings keeps the given order and puts unplaced lines under the file header", () => {
    const f = anchorFindings(
      [fnd("R3", "src/q.js", 1), fnd("R1", "src/q.js", 1), fnd("R2", "src/p.js", 0, 0), fnd("R4", "src/p.js", 99)],
      hunks,
    );
    expect(f.rows.get("E2")?.get(2)?.map((x) => x.id)).toEqual(["R3", "R1"]);
    expect(f.fileLevel.get("src/p.js")?.map((x) => x.id)).toEqual(["R2", "R4"]);
  });

  it("anchorFindings skips findings without a diff hunk", () => {
    const withSource = [...hunks, ev("E3", "src/s.js", "const s = 1;", "source")];
    const f = anchorFindings([fnd("R1", "src/s.js", 1), fnd("R2", "", 0, 0), fnd("R3", "src/s.js", 0, 0)], withSource);
    expect(f.rows.size).toBe(0);
    expect(f.fileLevel.size).toBe(0);
  });

  it("anchorFindings handles missing findings and evidence", () => {
    expect(anchorFindings(undefined, hunks).rows.size).toBe(0);
    expect(anchorFindings(null, null).fileLevel.size).toBe(0);
  });
});
