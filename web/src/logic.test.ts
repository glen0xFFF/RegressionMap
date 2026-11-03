import { describe, expect, it } from "vitest";
import {
  buildVerifyRequest,
  classifyDiffLine,
  formatLines,
  resolveEvidenceIds,
  runBadge,
  shouldPoll,
} from "./logic";
import type { Evidence, TestRun } from "./types";

function run(overrides: Partial<TestRun> = {}): TestRun {
  return {
    exit_code: 0,
    passed: true,
    timed_out: false,
    truncated: false,
    duration_ms: 10,
    stdout: "",
    stderr: "",
    ...overrides,
  };
}

describe("runBadge", () => {
  it("is PASS only when exit_code is 0 and not timed out", () => {
    expect(runBadge(run({ exit_code: 0, timed_out: false }))).toBe("PASS");
  });

  it("is FAIL for non-zero exit code even if passed flag claims true", () => {
    expect(runBadge(run({ exit_code: 1, passed: true }))).toBe("FAIL");
  });

  it("is FAIL when timed out even with exit code 0", () => {
    expect(runBadge(run({ exit_code: 0, timed_out: true, passed: true }))).toBe("FAIL");
  });

  it("ignores the passed flag entirely (exit code is the source of truth)", () => {
    expect(runBadge(run({ exit_code: 0, timed_out: false, passed: false }))).toBe("PASS");
  });

  it("is FAIL for negative exit codes (signals)", () => {
    expect(runBadge(run({ exit_code: -1 }))).toBe("FAIL");
  });

  it("is UNKNOWN when there is no run", () => {
    expect(runBadge(null)).toBe("UNKNOWN");
    expect(runBadge(undefined)).toBe("UNKNOWN");
  });

  it("is UNKNOWN when exit_code is not a number", () => {
    expect(runBadge({ ...run(), exit_code: "0" as unknown as number })).toBe("UNKNOWN");
  });
});

describe("resolveEvidenceIds", () => {
  const evidence: Evidence[] = [
    { id: "E1", type: "test_failure", title: "t", file: "", line_start: 0, line_end: 0, excerpt: "x" },
    { id: "E2", type: "diff_hunk", title: "d", file: "a.ts", line_start: 1, line_end: 2, excerpt: "+a" },
  ];

  it("resolves known ids to their evidence item", () => {
    const r = resolveEvidenceIds(["E2", "E1"], evidence);
    expect(r).toHaveLength(2);
    expect(r[0]).toEqual({ id: "E2", known: true, evidence: evidence[1] });
    expect(r[1].known).toBe(true);
  });

  it("flags unknown ids", () => {
    const r = resolveEvidenceIds(["E1", "E99"], evidence);
    expect(r[1]).toEqual({ id: "E99", known: false, evidence: undefined });
  });

  it("handles missing lists", () => {
    expect(resolveEvidenceIds(undefined, evidence)).toEqual([]);
    expect(resolveEvidenceIds(["E1"], null)).toEqual([{ id: "E1", known: false, evidence: undefined }]);
  });
});

describe("buildVerifyRequest", () => {
  it("refuses to build a body without approval", () => {
    expect(() => buildVerifyRequest("node-test", false)).toThrow(/approv/i);
  });

  it("refuses non-boolean truthy approval", () => {
    expect(() => buildVerifyRequest("node-test", "yes" as unknown as boolean)).toThrow();
  });

  it("refuses an empty command id", () => {
    expect(() => buildVerifyRequest("", true)).toThrow();
  });

  it("builds a body with approved literally true after approval", () => {
    expect(buildVerifyRequest("node-test", true)).toEqual({ command_id: "node-test", approved: true });
  });
});

describe("classifyDiffLine", () => {
  it("classifies added, removed, hunk header and context lines", () => {
    expect(classifyDiffLine("+foo")).toBe("add");
    expect(classifyDiffLine("-foo")).toBe("del");
    expect(classifyDiffLine("@@ -1,2 +1,2 @@")).toBe("hunk");
    expect(classifyDiffLine(" foo")).toBe("ctx");
    expect(classifyDiffLine("+++ b/file")).toBe("meta");
    expect(classifyDiffLine("--- a/file")).toBe("meta");
  });
});

describe("formatLines", () => {
  it("formats line ranges and never invents unknown lines", () => {
    expect(formatLines(3, 9)).toBe("L3-L9");
    expect(formatLines(4, 4)).toBe("L4");
    expect(formatLines(0, 0)).toBe("");
    expect(formatLines(5, 0)).toBe("L5");
  });
});

describe("shouldPoll", () => {
  it("polls only while running", () => {
    expect(shouldPoll({ status: "running" })).toBe(true);
    expect(shouldPoll({ status: "done" })).toBe(false);
    expect(shouldPoll({ status: "error" })).toBe(false);
    expect(shouldPoll(null)).toBe(false);
  });
});
