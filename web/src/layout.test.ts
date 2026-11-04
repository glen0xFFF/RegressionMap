import { describe, expect, it } from "vitest";
import {
  confidenceClass,
  diffEvidence,
  formatElapsed,
  recentDot,
  splitHypotheses,
  upsertRecent,
  type RecentEntry,
} from "./layout";
import type { Analysis, Evidence, Hypothesis, Review, TestRun } from "./types";

const run = (exit_code: number, timed_out = false): TestRun => ({
  exit_code, passed: exit_code === 0, timed_out, truncated: false, duration_ms: 1, stdout: "", stderr: "",
});

function analysis(o: Partial<Analysis> = {}): Analysis {
  return {
    id: "a1", status: "done", repo: "pagination-demo", baseline: "b", current: "c",
    command: { id: "node-test", argv: ["node", "--test"] }, ...o,
  };
}

describe("recentDot", () => {
  it("is red when the current run fails by exit code (ignores passed flag)", () => {
    expect(recentDot(analysis({ current_run: { ...run(1), passed: true } }))).toBe("fail");
  });
  it("is green only when current run exit code 0 and not timed out", () => {
    expect(recentDot(analysis({ current_run: run(0) }))).toBe("pass");
    expect(recentDot(analysis({ current_run: run(0, true) }))).toBe("fail");
  });
  it("is running while running, error on analysis error, unknown without run", () => {
    expect(recentDot(analysis({ status: "running" }))).toBe("running");
    expect(recentDot(analysis({ status: "error" }))).toBe("error");
    expect(recentDot(analysis({ current_run: null }))).toBe("unknown");
  });
});

describe("upsertRecent", () => {
  it("prepends new analyses and updates existing ones in place", () => {
    const t = "2026-11-04T10:00:00.000Z";
    let list: RecentEntry[] = [];
    list = upsertRecent(list, analysis({ id: "a1", status: "running" }), t);
    list = upsertRecent(list, analysis({ id: "a2", status: "running" }), t);
    expect(list.map((e) => e.id)).toEqual(["a2", "a1"]);
    list = upsertRecent(list, analysis({ id: "a1", status: "done", current_run: run(1) }), "ignored");
    expect(list.map((e) => e.id)).toEqual(["a2", "a1"]);
    expect(list[1]).toEqual({ id: "a1", repo: "pagination-demo", dot: "fail", startedAt: t });
  });
  it("upsertRecent records the confirmed count", () => {
    const t = "2026-11-04T10:00:00.000Z";
    const rv = (confirmed: number): Review => ({
      status: "ok", model_id: "muse-spark-1.3", message: "", check_status: "ok", check_message: "", summary: "",
      counts: { confirmed, disputed: 0, refuted: 1, unchecked: 0 }, findings: [], rejected: [],
    });
    let list = upsertRecent([], analysis({ id: "a1", status: "running", review: null }), t);
    expect(list[0]).toStrictEqual({ id: "a1", repo: "pagination-demo", dot: "running", startedAt: t });
    list = upsertRecent(list, analysis({ id: "a1", current_run: run(1), review: rv(2) }), "ignored");
    expect(list[0]).toStrictEqual({ id: "a1", repo: "pagination-demo", dot: "fail", startedAt: t, confirmed: 2 });
    list = upsertRecent(list, analysis({ id: "a2", current_run: run(0), review: rv(0) }), t);
    expect(list[0]).toStrictEqual({ id: "a2", repo: "pagination-demo", dot: "pass", startedAt: t, confirmed: 0 });
    list = upsertRecent(list, analysis({ id: "a3", current_run: run(0) }), t);
    expect(list[0]).toStrictEqual({ id: "a3", repo: "pagination-demo", dot: "pass", startedAt: t });
  });
  it("caps the list at 20 entries", () => {
    let list: RecentEntry[] = [];
    for (let i = 0; i < 25; i++) list = upsertRecent(list, analysis({ id: `a${i}` }), "t");
    expect(list).toHaveLength(20);
    expect(list[0].id).toBe("a24");
  });
});

describe("formatElapsed", () => {
  it("formats measured elapsed seconds", () => {
    expect(formatElapsed(1000, 13400)).toBe("12s");
    expect(formatElapsed(0, 400)).toBe("<1s");
    expect(formatElapsed(0, 125_000)).toBe("2m 5s");
  });
  it("returns null when unknown or invalid", () => {
    expect(formatElapsed(null, 100)).toBeNull();
    expect(formatElapsed(100, null)).toBeNull();
    expect(formatElapsed(200, 100)).toBeNull();
  });
});

const h = (rank: number): Hypothesis => ({
  rank, title: `h${rank}`, file: "", line_start: 0, line_end: 0, reason: "", supporting_evidence_ids: [],
  counter_evidence_ids: [], verification_suggestion: "", confidence_label: "low",
});

describe("splitHypotheses", () => {
  it("picks the lowest rank as root cause and orders the rest", () => {
    const { root, others } = splitHypotheses([h(3), h(1), h(2)]);
    expect(root?.rank).toBe(1);
    expect(others.map((x) => x.rank)).toEqual([2, 3]);
  });
  it("handles empty / missing lists", () => {
    expect(splitHypotheses(undefined)).toEqual({ root: null, others: [] });
    expect(splitHypotheses([])).toEqual({ root: null, others: [] });
  });
});

describe("diffEvidence", () => {
  it("keeps only diff_hunk items", () => {
    const ev: Evidence[] = [
      { id: "E1", type: "test_failure", title: "", file: "", line_start: 0, line_end: 0, excerpt: "" },
      { id: "E2", type: "diff_hunk", title: "", file: "a", line_start: 0, line_end: 0, excerpt: "+" },
    ];
    expect(diffEvidence(ev).map((e) => e.id)).toEqual(["E2"]);
    expect(diffEvidence(null)).toEqual([]);
  });
});

describe("confidenceClass", () => {
  it("maps labels to pill classes, unknown to low", () => {
    expect(confidenceClass("high")).toBe("pill pill-high");
    expect(confidenceClass("medium")).toBe("pill pill-medium");
    expect(confidenceClass("low")).toBe("pill pill-low");
    expect(confidenceClass("bogus" as never)).toBe("pill pill-low");
  });
});
