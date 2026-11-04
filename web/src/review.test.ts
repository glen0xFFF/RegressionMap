import { describe, expect, it } from "vitest";
import {
  LENS_ORDER,
  REVIEW_DISCLAIMER,
  VERDICT_LABEL,
  displayOrder,
  findingLocation,
  lensCounts,
  visibleFindings,
} from "./review";
import type { Review, ReviewFinding, ReviewLens, ReviewVerdict } from "./types";

const finding = (id: string, verdict: ReviewVerdict, o: Partial<ReviewFinding> = {}): ReviewFinding => ({
  id, lens: "code", severity: "high", title: `title ${id}`, detail: "", suggestion: "",
  file: "src/a.ts", line_start: 4, line_end: 9, evidence_ids: ["E2"],
  check: { verdict, reason: "", evidence_ids: [] }, ...o,
});

const review = (findings: ReviewFinding[], o: Partial<Review> = {}): Review => ({
  status: "ok", model_id: "muse-spark-1.3", message: "", check_status: "ok", check_message: "", summary: "",
  counts: { confirmed: 0, disputed: 0, refuted: 0, unchecked: 0 }, findings, rejected: [], ...o,
});

const ids = (fs: ReviewFinding[]) => fs.map((f) => f.id);

describe("review wording", () => {
  it("copies the labels and the disclaimer from the contract", () => {
    expect(REVIEW_DISCLAIMER).toBe("AI review: Muse wrote these findings and a second Muse pass checked them. Not verified by tests.");
    expect(VERDICT_LABEL).toEqual({
      confirmed: "Confirmed by Muse check",
      disputed: "Disputed by Muse check",
      refuted: "Refuted by Muse check",
      unchecked: "Not checked",
    });
    expect(LENS_ORDER).toEqual(["security", "code", "feature"]);
  });
});

describe("displayOrder", () => {
  it("displayOrder keeps confirmed, unchecked, disputed, refuted", () => {
    const fs = [
      finding("R1", "refuted"),
      finding("R2", "disputed"),
      finding("R3", "unchecked"),
      finding("R4", "confirmed"),
      finding("R5", "unchecked"),
    ];
    expect(ids(displayOrder(fs))).toEqual(["R4", "R3", "R5", "R2", "R1"]);
    expect(ids(fs)).toEqual(["R1", "R2", "R3", "R4", "R5"]);
  });
});

describe("visibleFindings", () => {
  it("visibleFindings hides refuted unless asked", () => {
    const rv = review([finding("R1", "refuted"), finding("R2", "confirmed"), finding("R3", "disputed")]);
    expect(ids(visibleFindings(rv, false))).toEqual(["R2", "R3"]);
    expect(ids(visibleFindings(rv, true))).toEqual(["R2", "R3", "R1"]);
    expect(visibleFindings({ ...rv, status: "error" }, true)).toEqual([]);
    expect(visibleFindings(null, true)).toEqual([]);
    expect(visibleFindings(undefined, false)).toEqual([]);
  });
});

describe("lensCounts", () => {
  it("lensCounts counts each lens", () => {
    const lens = (id: string, l: ReviewLens) => finding(id, "confirmed", { lens: l });
    expect(lensCounts([lens("R1", "security"), lens("R2", "code"), lens("R3", "code"), lens("R4", "feature")])).toEqual({
      security: 1, code: 2, feature: 1,
    });
    expect(lensCounts([])).toEqual({ security: 0, code: 0, feature: 0 });
  });
});

describe("findingLocation", () => {
  it("findingLocation gives the file and range, the file alone, or nothing", () => {
    expect(findingLocation(finding("R1", "confirmed"))).toBe("src/a.ts:L4-L9");
    expect(findingLocation(finding("R1", "confirmed", { line_start: 0, line_end: 0 }))).toBe("src/a.ts");
    expect(findingLocation(finding("R1", "confirmed", { file: "", line_start: 0, line_end: 0 }))).toBe("");
  });
});
