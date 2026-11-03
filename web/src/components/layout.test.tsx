import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { Sidebar } from "./Sidebar";
import { MiddlePanel } from "./MiddlePanel";
import { RightPanel } from "./RightPanel";
import type { Analysis, Hypothesis, ModelResult, TestRun } from "../types";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const run = (exit_code: number, stdout = "", stderr = ""): TestRun => ({
  exit_code, passed: exit_code === 0, timed_out: false, truncated: false, duration_ms: 42, stdout, stderr,
});

function analysis(o: Partial<Analysis> = {}): Analysis {
  return {
    id: "a1", status: "done", repo: "pagination-demo", baseline: "b".repeat(40), current: "c".repeat(40),
    command: { id: "node-test", argv: ["node", "--test"] },
    baseline_run: run(0, "ok base"),
    current_run: run(1, "not ok 1 - page size", "AssertionError: expected 10"),
    changed_files: [{ path: "src/pagination.js", status: "M" }],
    evidence: [
      { id: "E1", type: "test_failure", title: "failing test", file: "", line_start: 0, line_end: 0, excerpt: "not ok 1" },
      { id: "E2", type: "diff_hunk", title: "hunk in pagination", file: "src/pagination.js", line_start: 3, line_end: 4, excerpt: "@@ -3 +3 @@\n-  a\n+  b" },
    ],
    model: null,
    verifications: [],
    ...o,
  };
}

describe("Sidebar", () => {
  const baseProps = {
    view: "analyze" as const,
    onView: () => {},
    onNew: () => {},
    onOpenRecent: () => {},
    recent: [
      { id: "a1", repo: "pagination-demo", dot: "fail" as const, startedAt: "2026-11-04T10:00:00.000Z" },
    ],
    health: { state: "ok" as const, data: { ok: true, model_configured: true, model_id: "muse-spark-1.3" } },
  };

  it("shows app name, New Analysis, only real nav items, recent list and health footer", () => {
    const onNew = vi.fn();
    const onOpenRecent = vi.fn();
    render(<Sidebar {...baseProps} onNew={onNew} onOpenRecent={onOpenRecent} />);
    expect(screen.getByText("RegressionMap")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /new analysis/i }));
    expect(onNew).toHaveBeenCalled();
    const nav = screen.getByRole("navigation");
    expect(within(nav).getAllByRole("button").map((b) => b.textContent?.trim())).toEqual(["Analyze", "History"]);
    expect(screen.queryByText(/settings|monitoring|team|api keys/i)).not.toBeInTheDocument();
    expect(screen.getByText("RECENT")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /pagination-demo/ }));
    expect(onOpenRecent).toHaveBeenCalledWith("a1");
    expect(screen.getByText(/backend ok · muse-spark-1.3/)).toBeInTheDocument();
  });

  it("shows model not configured and backend errors in footer", () => {
    const { rerender } = render(
      <Sidebar {...baseProps} health={{ state: "ok", data: { ok: true, model_configured: false, model_id: "m" } }} />,
    );
    expect(screen.getByText(/model not configured/)).toBeInTheDocument();
    rerender(<Sidebar {...baseProps} health={{ state: "error", error: "Cannot reach the RegressionMap backend" }} />);
    expect(screen.getByText(/Cannot reach the RegressionMap backend/)).toBeInTheDocument();
  });

  it("shows an empty recent state", () => {
    render(<Sidebar {...baseProps} recent={[]} />);
    expect(screen.getByText(/No analyses yet/)).toBeInTheDocument();
  });
});

describe("MiddlePanel", () => {
  const noop = () => {};
  const props = { highlight: null, onClear: noop, onAnalyze: noop, canAnalyze: true, busy: false, form: <div>form</div> };

  it("shows empty state without an analysis", () => {
    render(<MiddlePanel {...props} analysis={null} />);
    expect(screen.getByText(/Pick a repository and baseline, then Analyze/)).toBeInTheDocument();
  });

  it("Output tab shows current run stdout+stderr and a baseline/HEAD strip from exit codes", () => {
    render(<MiddlePanel {...props} analysis={analysis()} />);
    expect(screen.getByTestId("terminal")).toHaveTextContent("not ok 1 - page size");
    expect(screen.getByTestId("terminal")).toHaveTextContent("AssertionError: expected 10");
    const strip = screen.getByTestId("run-strip");
    expect(within(strip).getByText("PASS")).toBeInTheDocument();
    expect(within(strip).getByText("FAIL")).toBeInTheDocument();
    expect(strip).toHaveTextContent("exit 1");
  });

  it("Diff tab shows only diff hunks; Evidence tab shows all evidence", () => {
    render(<MiddlePanel {...props} analysis={analysis()} />);
    fireEvent.click(screen.getByRole("tab", { name: "Diff" }));
    expect(screen.getByText("hunk in pagination")).toBeInTheDocument();
    expect(screen.queryByText("failing test")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("tab", { name: "Evidence" }));
    expect(screen.getByText("failing test")).toBeInTheDocument();
    expect(screen.getByText("hunk in pagination")).toBeInTheDocument();
  });

  it("Diff tab renders PR-style diff with the model's hypothesis anchored inline", () => {
    const model: ModelResult = {
      status: "ok", model_id: "m",
      result: { summary: "", limitations: [], hypotheses: [hyp(1, { line_start: 3, line_end: 3, title: "Anchored hyp" })] },
    };
    const { container } = render(<MiddlePanel {...props} analysis={analysis({ model })} />);
    fireEvent.click(screen.getByRole("tab", { name: "Diff" }));
    expect(screen.getByTestId("file-header-src/pagination.js")).toBeInTheDocument();
    const comment = container.querySelector(".inline-comment") as HTMLElement;
    expect(within(comment).getByText("Anchored hyp")).toBeInTheDocument();
  });

  it("switches to the Evidence tab when an evidence id is highlighted", () => {
    const { rerender, container } = render(<MiddlePanel {...props} analysis={analysis()} />);
    rerender(<MiddlePanel {...props} analysis={analysis()} highlight="E1" />);
    expect(screen.getByRole("tab", { name: "Evidence" })).toHaveAttribute("aria-selected", "true");
    expect(container.querySelector("#evidence-E1")).toHaveClass("ev-hl");
  });

  it("Analyze and Clear buttons call handlers; Analyze respects canAnalyze", () => {
    const onAnalyze = vi.fn();
    const onClear = vi.fn();
    const { rerender } = render(<MiddlePanel {...props} onAnalyze={onAnalyze} onClear={onClear} analysis={analysis()} />);
    fireEvent.click(screen.getByRole("button", { name: /analyze/i }));
    fireEvent.click(screen.getByRole("button", { name: /clear/i }));
    expect(onAnalyze).toHaveBeenCalledTimes(1);
    expect(onClear).toHaveBeenCalledTimes(1);
    rerender(<MiddlePanel {...props} canAnalyze={false} onAnalyze={onAnalyze} onClear={onClear} analysis={null} />);
    expect(screen.getByRole("button", { name: /analyze/i })).toBeDisabled();
  });
});

const hyp = (rank: number, o: Partial<Hypothesis> = {}): Hypothesis => ({
  rank, title: `Hypothesis ${rank}`, file: "src/pagination.js", line_start: 3, line_end: 4, reason: `reason ${rank}`,
  supporting_evidence_ids: ["E1"], counter_evidence_ids: [], verification_suggestion: `check ${rank}`,
  confidence_label: rank === 1 ? "high" : "low", ...o,
});

describe("RightPanel", () => {
  const commands = [{ id: "node-test", argv: ["node", "--test"], description: "Run tests" }];
  const okModel: ModelResult = {
    status: "ok", model_id: "muse-spark-1.3",
    result: { summary: "the summary", hypotheses: [hyp(2), hyp(1)], limitations: ["lim"] },
  };

  it("shows a placeholder without an analysis", () => {
    render(<RightPanel analysis={null} elapsed={null} commands={commands} onSelectEvidence={() => {}} onVerified={() => {}} />);
    expect(screen.getByText(/AI Analysis/)).toBeInTheDocument();
    expect(screen.getByText(/Run an analysis to see/)).toBeInTheDocument();
    expect(screen.queryByText(/analysis time/)).not.toBeInTheDocument();
  });

  it("shows elapsed pill, root cause card, other hypotheses, copy button, and verify card", async () => {
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    const onSelect = vi.fn();
    render(
      <RightPanel analysis={analysis({ model: okModel })} elapsed="12s" commands={commands} onSelectEvidence={onSelect} onVerified={() => {}} />,
    );
    expect(screen.getByText("12s analysis time")).toBeInTheDocument();
    expect(screen.getByText(/AI-generated hypotheses — not verified/)).toBeInTheDocument();

    const root = screen.getByTestId("root-cause");
    expect(within(root).getByText("Root Cause")).toBeInTheDocument();
    expect(within(root).getByText("Hypothesis 1")).toBeInTheDocument();
    expect(within(root).getByText("src/pagination.js:L3-L4")).toBeInTheDocument();
    expect(within(root).getByText("high")).toHaveClass("pill-high");

    const others = screen.getByTestId("other-hypotheses");
    expect(within(others).getByText("Hypothesis 2")).toBeInTheDocument();
    expect(within(others).queryByText("Hypothesis 1")).not.toBeInTheDocument();
    fireEvent.click(within(others).getByRole("button", { name: /copy suggestion/i }));
    expect(writeText).toHaveBeenCalledWith("check 2");

    fireEvent.click(within(root).getByRole("button", { name: "E1" }));
    expect(onSelect).toHaveBeenCalledWith("E1");

    expect(screen.getByText("lim")).toBeInTheDocument();
    expect(screen.getByText("Verify fix")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /run verification/i })).toBeDisabled();
  });

  it("renders disabled model state as a card", () => {
    render(
      <RightPanel analysis={analysis({ model: { status: "disabled", model_id: "m" } })} elapsed={null} commands={commands} onSelectEvidence={() => {}} onVerified={() => {}} />,
    );
    expect(screen.getByText(/Meta model not configured — set MODEL_API_KEY on the backend/)).toBeInTheDocument();
    expect(screen.getByText("Verify fix")).toBeInTheDocument();
  });
});
