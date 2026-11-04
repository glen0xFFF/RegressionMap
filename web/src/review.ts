import { formatLines } from "./logic";
import type { Review, ReviewFinding, ReviewLens, ReviewVerdict } from "./types";

// Wording and display order of the Muse change review, copied from the v4
// section of docs/API_CONTRACT.md ("Display order and wording (every client)").

export const REVIEW_DISCLAIMER =
  "AI review: Muse wrote these findings and a second Muse pass checked them. Not verified by tests.";

export const VERDICT_LABEL: Record<ReviewVerdict, string> = {
  confirmed: "Confirmed by Muse check",
  disputed: "Disputed by Muse check",
  refuted: "Refuted by Muse check",
  unchecked: "Not checked",
};

export const LENS_ORDER: ReviewLens[] = ["security", "code", "feature"];

const VERDICT_RANK: Record<ReviewVerdict, number> = { confirmed: 0, unchecked: 1, disputed: 2, refuted: 3 };

/** The verdict of a finding; a missing or unknown one counts as unchecked. */
export function verdictOf(f: ReviewFinding): ReviewVerdict {
  const v = f.check?.verdict;
  return v !== undefined && Object.prototype.hasOwnProperty.call(VERDICT_RANK, v) ? v : "unchecked";
}

/** Confirmed, then unchecked, then disputed, then refuted; each group keeps the order of findings. */
export function displayOrder(fs: ReviewFinding[]): ReviewFinding[] {
  return fs
    .map((f, i) => ({ f, i }))
    .sort((a, b) => VERDICT_RANK[verdictOf(a.f)] - VERDICT_RANK[verdictOf(b.f)] || a.i - b.i)
    .map((x) => x.f);
}

/** The findings to show, in display order: none unless the review is ok, refuted ones only on request. */
export function visibleFindings(review: Review | null | undefined, showRefuted: boolean): ReviewFinding[] {
  if (!review || review.status !== "ok") return [];
  const ordered = displayOrder(review.findings ?? []);
  return showRefuted ? ordered : ordered.filter((f) => verdictOf(f) !== "refuted");
}

export function lensCounts(fs: ReviewFinding[]): Record<ReviewLens, number> {
  const counts: Record<ReviewLens, number> = { security: 0, code: 0, feature: 0 };
  for (const f of fs) if (Object.prototype.hasOwnProperty.call(counts, f.lens)) counts[f.lens]++;
  return counts;
}

/** "file:Lstart-Lend", the file alone for the range 0-0, or "" for a finding about no file. */
export function findingLocation(f: ReviewFinding): string {
  if (!f.file) return "";
  const l = formatLines(f.line_start, f.line_end);
  return l ? `${f.file}:${l}` : f.file;
}
