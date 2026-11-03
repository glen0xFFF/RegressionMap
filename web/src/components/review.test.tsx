import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { ReviewComment } from "./ReviewComment";
import { DiffView } from "./DiffView";
import { HypothesesPanel } from "./HypothesesPanel";
import type { Evidence, Hypothesis, ModelResult } from "../types";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const HUNK = "@@ -3,2 +3,2 @@\n   const start = 0;\n-  const end = size;\n+  const end = size + 1;";

const evidence: Evidence[] = [
  { id: "E1", type: "test_failure", title: "failing test", file: "", line_start: 0, line_end: 0, excerpt: "not ok" },
  { id: "E2", type: "diff_hunk", title: "hunk", file: "src/pagination.js", line_start: 3, line_end: 4, excerpt: HUNK },
];

const hyp = (rank: number, o: Partial<Hypothesis> = {}): Hypothesis => ({
  rank, title: `Hypothesis ${rank}`, file: "src/pagination.js", line_start: 4, line_end: 4, reason: `reason ${rank}`,
  supporting_evidence_ids: ["E2"], counter_evidence_ids: [], verification_suggestion: `check ${rank}`,
  confidence_label: "medium", ...o,
});

describe("ReviewComment", () => {
  it("renders as a review comment with initials, author, location chip, pill, body and evidence chips", () => {
    const onSelect = vi.fn();
    render(<ReviewComment hypothesis={hyp(1)} evidence={evidence} onSelectEvidence={onSelect} />);
    expect(screen.getByText("RM")).toBeInTheDocument();
    expect(screen.getByText("RegressionMap analysis")).toBeInTheDocument();
    expect(screen.getByText("src/pagination.js:L4")).toHaveClass("loc-chip");
    expect(screen.getByText("medium")).toHaveClass("pill-medium");
    expect(screen.getByText("reason 1")).toBeInTheDocument();
    expect(screen.getByText("Suggested verification")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "E2" }));
    expect(onSelect).toHaveBeenCalledWith("E2");
  });

  it("copies the verification suggestion as text", () => {
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    render(<ReviewComment hypothesis={hyp(1)} evidence={evidence} onSelectEvidence={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: /copy suggestion/i }));
    expect(writeText).toHaveBeenCalledWith("check 1");
  });
});

describe("DiffView", () => {
  it("renders file header with +/- counts, line numbers, row colours", () => {
    const { container } = render(<DiffView evidence={evidence} hypotheses={[]} highlightId={null} onSelectEvidence={() => {}} />);
    const header = screen.getByTestId("file-header-src/pagination.js");
    expect(header).toHaveTextContent("src/pagination.js");
    expect(header).toHaveTextContent("+1");
    expect(header).toHaveTextContent("−1");
    const add = container.querySelector(".drow-add")!;
    expect(add).toHaveTextContent("const end = size + 1;");
    expect(within(add as HTMLElement).getByTestId("new-no")).toHaveTextContent("4");
    expect(container.querySelector(".drow-del")).toHaveTextContent("const end = size;");
    expect(screen.queryByText("failing test")).not.toBeInTheDocument();
  });

  it("anchors an inline comment directly under the hypothesis line and 0/0 ones under the file header", () => {
    const { container } = render(
      <DiffView
        evidence={evidence}
        hypotheses={[hyp(1), hyp(2, { line_start: 0, line_end: 0, title: "file level" })]}
        highlightId={null}
        onSelectEvidence={() => {}}
      />,
    );
    const addRow = container.querySelector(".drow-add")!;
    const next = addRow.nextElementSibling as HTMLElement;
    expect(next).toHaveClass("inline-comment");
    expect(within(next).getByText("Hypothesis 1")).toBeInTheDocument();
    const fileLevel = screen.getByTestId("file-comments-src/pagination.js");
    expect(within(fileLevel).getByText("file level")).toBeInTheDocument();
  });

  it("shows an empty state without diff hunks", () => {
    render(<DiffView evidence={[evidence[0]]} hypotheses={[]} highlightId={null} onSelectEvidence={() => {}} />);
    expect(screen.getByText(/No diff hunks/)).toBeInTheDocument();
  });
});

describe("HypothesesPanel walkthrough", () => {
  it("shows walkthrough summary, a Changes table with counts from evidence, and ranked/rejected header", () => {
    const model: ModelResult = {
      status: "ok", model_id: "muse-spark-1.3", rejected: ["dropped one"],
      result: { summary: "Off-by-one in page end.", hypotheses: [hyp(1), hyp(2)], limitations: [] },
    };
    render(
      <HypothesesPanel
        model={model}
        evidence={evidence}
        changedFiles={[{ path: "src/pagination.js", status: "M" }]}
        onSelectEvidence={() => {}}
      />,
    );
    expect(screen.getByText(/AI-generated hypotheses — not verified/)).toBeInTheDocument();
    const walk = screen.getByTestId("walkthrough");
    expect(within(walk).getByText("Walkthrough")).toBeInTheDocument();
    expect(within(walk).getByText("Off-by-one in page end.")).toBeInTheDocument();
    const row = within(walk).getByText("src/pagination.js").closest("tr")!;
    expect(row).toHaveTextContent("M");
    expect(row).toHaveTextContent("+1");
    expect(row).toHaveTextContent("−1");
    expect(screen.getByText("Hypotheses: 2 ranked · 1 rejected")).toBeInTheDocument();
  });
});
