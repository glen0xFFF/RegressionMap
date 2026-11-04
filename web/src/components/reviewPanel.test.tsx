import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { ReviewPanel } from "./ReviewPanel";
import { FindingCard } from "./FindingCard";
import { REVIEW_DISCLAIMER } from "../review";
import type { Evidence, Review, ReviewFinding, ReviewVerdict } from "../types";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const HUNK = "@@ -5,3 +5,3 @@\n const a = 1;\n-  return items.slice(start, end);\n+  return items.slice(start, end - 1);";

const evidence: Evidence[] = [
  { id: "E1", type: "test_failure", title: "failing test", file: "", line_start: 0, line_end: 0, excerpt: "not ok" },
  { id: "E2", type: "diff_hunk", title: "hunk", file: "src/pagination.ts", line_start: 5, line_end: 7, excerpt: HUNK },
  { id: "E3", type: "source", title: "source", file: "src/pagination.ts", line_start: 1, line_end: 20, excerpt: "x" },
];

const finding = (id: string, verdict: ReviewVerdict, o: Partial<ReviewFinding> = {}): ReviewFinding => ({
  id, lens: "code", severity: "medium", title: `title ${id}`, detail: `detail ${id}`, suggestion: "",
  file: "src/pagination.ts", line_start: 6, line_end: 6, evidence_ids: ["E2"],
  check: verdict === "unchecked" ? { verdict, reason: "", evidence_ids: [] } : { verdict, reason: `reason ${id}`, evidence_ids: ["E2"] },
  ...o,
});

const review = (o: Partial<Review> = {}): Review => ({
  status: "ok", model_id: "muse-spark-1.3", message: "", check_status: "ok", check_message: "", summary: "",
  counts: { confirmed: 0, disputed: 0, refuted: 0, unchecked: 0 }, findings: [], rejected: [], ...o,
});

const panel = (rv: Review | null | undefined, o: { running?: boolean; showRefuted?: boolean; onToggleRefuted?: () => void } = {}) => (
  <ReviewPanel
    review={rv}
    running={o.running ?? false}
    evidence={evidence}
    showRefuted={o.showRefuted ?? false}
    onToggleRefuted={o.onToggleRefuted ?? (() => {})}
    onSelectEvidence={() => {}}
  />
);

describe("ReviewPanel", () => {
  it("ReviewPanel shows each status", () => {
    const notOk: [Review["status"], string, string, string][] = [
      [
        "disabled",
        "Meta model not configured: MODEL_API_KEY is not set on the backend.",
        "card-warn",
        "Muse review not available: Meta model not configured: MODEL_API_KEY is not set on the backend.",
      ],
      ["error", "review stopped at the analysis deadline", "card-error", "Muse review failed: review stopped at the analysis deadline"],
      ["invalid", "no finding passed validation", "card-error", "Muse review was rejected as invalid: no finding passed validation"],
      [
        "skipped",
        "the analysis ended before the change could be reviewed",
        "card-warn",
        "Muse review skipped: the analysis ended before the change could be reviewed",
      ],
    ];
    for (const [status, message, cls, text] of notOk) {
      const rejected = status === "invalid" ? ['finding 1 ("t"): cites unknown evidence id E99'] : [];
      render(panel(review({ status, message, check_status: "skipped", check_message: "no review to check", rejected })));
      expect(screen.getByRole("heading", { name: "Muse review" })).toBeInTheDocument();
      expect(screen.getByText(text)).toHaveClass("card", cls);
      expect(screen.getByText(REVIEW_DISCLAIMER)).toBeInTheDocument();
      expect(screen.queryByText(/Muse check/)).not.toBeInTheDocument();
      expect(screen.queryByText("No findings.")).not.toBeInTheDocument();
      if (status === "invalid") {
        expect(screen.getByText("Rejected by validation (1)")).toBeInTheDocument();
        expect(screen.getByText('finding 1 ("t"): cites unknown evidence id E99')).toBeInTheDocument();
      }
      cleanup();
    }

    render(panel(review({ findings: [finding("R1", "confirmed")], counts: { confirmed: 1, disputed: 0, refuted: 0, unchecked: 0 } })));
    expect(screen.getByRole("heading", { name: "Muse review" })).toBeInTheDocument();
    expect(screen.getByText("title R1")).toBeInTheDocument();
    expect(screen.getByText(REVIEW_DISCLAIMER)).toBeInTheDocument();
    expect(screen.queryByText(/^Muse review (not available|failed|was rejected|skipped)/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /refuted/ })).not.toBeInTheDocument();
  });

  it("ReviewPanel shows an unknown status as failed", () => {
    for (const status of ["toString", "constructor", "later"]) {
      render(panel(review({ status: status as Review["status"], message: "msg" })));
      expect(screen.getByText("Muse review failed: msg")).toHaveClass("card", "card-error");
      expect(screen.queryByText(/^undefined/)).not.toBeInTheDocument();
      expect(screen.getByText(REVIEW_DISCLAIMER)).toBeInTheDocument();
      cleanup();
    }
  });

  it("ReviewPanel shows running and older analyses", () => {
    const { rerender } = render(panel(null, { running: true }));
    expect(screen.getByRole("heading", { name: "Muse review" })).toBeInTheDocument();
    expect(screen.getByText("Muse review running…")).toBeInTheDocument();
    rerender(panel(undefined, { running: false }));
    expect(screen.getByText("No Muse review for this analysis.")).toBeInTheDocument();
    expect(screen.queryByText("Muse review running…")).not.toBeInTheDocument();
    rerender(panel(null, { running: false }));
    expect(screen.getByText("No Muse review for this analysis.")).toBeInTheDocument();
  });

  it("ReviewPanel shows counts, no findings and rejections", () => {
    render(
      panel(
        review({
          check_status: "skipped", check_message: "no finding to check", summary: "clean change",
          rejected: ['finding 1 ("t"): cites no evidence'],
        }),
      ),
    );
    expect(screen.getByText("No findings.")).toBeInTheDocument();
    expect(screen.getByText("clean change")).toBeInTheDocument();
    expect(screen.getByText("Security 0 · Code 0 · Feature 0")).toBeInTheDocument();
    expect(screen.getByText("0 confirmed · 0 not checked · 0 disputed · 0 refuted")).toBeInTheDocument();
    expect(screen.getByText("Rejected by validation (1)")).toBeInTheDocument();
    expect(screen.getByText('finding 1 ("t"): cites no evidence')).toBeInTheDocument();
    expect(screen.queryByText(/Muse check skipped/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /refuted/ })).not.toBeInTheDocument();
  });

  it("ReviewPanel orders by verdict and hides refuted", () => {
    const rv = review({
      counts: { confirmed: 1, disputed: 1, refuted: 1, unchecked: 2 },
      findings: [
        finding("R1", "refuted", { lens: "security" }),
        finding("R2", "disputed"),
        finding("R3", "unchecked", { lens: "feature" }),
        finding("R4", "confirmed"),
        finding("R5", "unchecked"),
      ],
    });
    const onToggle = vi.fn();
    const { container, rerender } = render(panel(rv, { onToggleRefuted: onToggle }));
    const titles = () => [...container.querySelectorAll(".finding-title")].map((e) => e.textContent);
    expect(titles()).toEqual(["title R4", "title R3", "title R5", "title R2"]);
    expect(screen.queryByText("title R1")).not.toBeInTheDocument();
    expect(screen.getByText("Security 1 · Code 3 · Feature 1")).toBeInTheDocument();
    expect(screen.getByText("1 confirmed · 2 not checked · 1 disputed · 1 refuted")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Show 1 refuted" }));
    expect(onToggle).toHaveBeenCalledTimes(1);

    rerender(panel(rv, { onToggleRefuted: onToggle, showRefuted: true }));
    expect(titles()).toEqual(["title R4", "title R3", "title R5", "title R2", "title R1"]);
    const refuted = screen.getByText("title R1").closest("article") as HTMLElement;
    expect(refuted).toHaveClass("finding-refuted");
    expect(within(refuted).getByText("Refuted by Muse check")).toHaveClass("verdict", "verdict-refuted");
    fireEvent.click(screen.getByRole("button", { name: "Hide 1 refuted" }));
    expect(onToggle).toHaveBeenCalledTimes(2);
  });

  it("the refuted toggle names its action and has no pressed state", () => {
    const rv = review({ counts: { confirmed: 0, disputed: 0, refuted: 1, unchecked: 0 }, findings: [finding("R1", "refuted")] });
    const { rerender } = render(panel(rv));
    expect(screen.getByRole("button", { name: "Show 1 refuted" })).not.toHaveAttribute("aria-pressed");
    rerender(panel(rv, { showRefuted: true }));
    expect(screen.getByRole("button", { name: "Hide 1 refuted" })).not.toHaveAttribute("aria-pressed");
  });

  it("ReviewPanel keeps the toggle and not No findings when every finding is refuted", () => {
    const rv = review({
      counts: { confirmed: 0, disputed: 0, refuted: 2, unchecked: 0 },
      findings: [finding("R1", "refuted"), finding("R2", "refuted", { lens: "security" })],
    });
    const { container } = render(panel(rv));
    expect(screen.queryByText("No findings.")).not.toBeInTheDocument();
    expect(container.querySelectorAll(".finding-title")).toHaveLength(0);
    expect(screen.getByText("0 confirmed · 0 not checked · 0 disputed · 2 refuted")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Show 2 refuted" })).toBeInTheDocument();
  });

  it("ReviewPanel shows findings the diff view cannot anchor", () => {
    // src/util.ts has a source item and no diff_hunk; the first finding is about no file.
    const withSource: Evidence[] = [
      ...evidence,
      { id: "E4", type: "source", title: "util source", file: "src/util.ts", line_start: 1, line_end: 30, excerpt: "y" },
    ];
    const rv = review({
      counts: { confirmed: 1, disputed: 0, refuted: 0, unchecked: 1 },
      findings: [
        finding("R1", "confirmed", { title: "No file finding", file: "", line_start: 0, line_end: 0 }),
        finding("R2", "unchecked", { title: "Source only finding", file: "src/util.ts", line_start: 3, line_end: 4, evidence_ids: ["E2", "E4"] }),
      ],
    });
    const { container } = render(
      <ReviewPanel review={rv} running={false} evidence={withSource} showRefuted={false} onToggleRefuted={() => {}} onSelectEvidence={() => {}} />,
    );
    expect([...container.querySelectorAll(".finding-title")].map((e) => e.textContent)).toEqual([
      "No file finding",
      "Source only finding",
    ]);
    const noFile = screen.getByText("No file finding").closest("article") as HTMLElement;
    expect(noFile.querySelector(".loc-chip")).toBeNull();
    const sourceOnly = screen.getByText("Source only finding").closest("article") as HTMLElement;
    expect(within(sourceOnly).getByText("src/util.ts:L3-L4")).toHaveClass("loc-chip");
  });

  it("review text renders as text", () => {
    const hostile = finding("R1", "confirmed", {
      title: "<img src=x onerror=alert(1)>",
      detail: "[a](http://evil.example)",
      suggestion: "<script>alert(1)</script>",
      check: { verdict: "confirmed", reason: "<b>x</b>", evidence_ids: ["E2"] },
    });
    const { container } = render(
      panel(
        review({
          summary: "<a href='http://evil.example'>s</a>",
          findings: [hostile],
          counts: { confirmed: 1, disputed: 0, refuted: 0, unchecked: 0 },
          rejected: ['finding 2 ("<img src=y>"): cites no evidence'],
        }),
      ),
    );
    expect(screen.getByText("<img src=x onerror=alert(1)>")).toBeInTheDocument();
    expect(screen.getByText("[a](http://evil.example)")).toBeInTheDocument();
    expect(screen.getByText("Muse check: <b>x</b>")).toBeInTheDocument();
    expect(screen.getByText("<a href='http://evil.example'>s</a>")).toBeInTheDocument();
    expect(screen.getByText('finding 2 ("<img src=y>"): cites no evidence')).toBeInTheDocument();
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("a[href]")).toBeNull();
    expect(container.querySelector("b")).toBeNull();
    expect(container.querySelector("script")).toBeNull();
  });

  it("check problems are shown", () => {
    const findings = [finding("R1", "unchecked"), finding("R2", "unchecked", { lens: "security" })];
    const counts = { confirmed: 0, disputed: 0, refuted: 0, unchecked: 2 };
    const { rerender } = render(
      panel(review({ check_status: "error", check_message: "check stopped at the analysis deadline", findings, counts })),
    );
    expect(screen.getByText("Muse check error: check stopped at the analysis deadline")).toHaveClass("card", "card-warn");
    expect(screen.getAllByText("Not checked")).toHaveLength(2);
    rerender(panel(review({ check_status: "invalid", check_message: "model output did not match the schema: x", findings, counts })));
    expect(screen.getByText("Muse check invalid: model output did not match the schema: x")).toHaveClass("card-warn");
  });
});

describe("FindingCard", () => {
  const f = finding("R1", "confirmed", {
    severity: "high",
    file: "src/pagination.ts",
    line_start: 7,
    line_end: 7,
    detail: "The last page loses its final item.",
    suggestion: "Use end, not end - 1.",
    evidence_ids: ["E2"],
    check: { verdict: "confirmed", reason: "E2 shows the slice end moved by one", evidence_ids: ["E3"] },
  });

  it("FindingCard shows severity as a word and the verdict label", () => {
    const onSelect = vi.fn();
    render(<FindingCard finding={f} evidence={evidence} onSelectEvidence={onSelect} />);
    expect(screen.getByText("MR")).toBeInTheDocument();
    expect(screen.getByText("Muse review")).toHaveClass("author");
    expect(screen.getByText("high")).toHaveClass("pill", "pill-high");
    expect(screen.getByText("code")).toHaveClass("lens-chip");
    expect(screen.getByText("Confirmed by Muse check")).toHaveClass("verdict", "verdict-confirmed");
    expect(screen.getByText("Muse check: E2 shows the slice end moved by one")).toBeInTheDocument();
    expect(screen.getByText("src/pagination.ts:L7")).toHaveClass("loc-chip");
    expect(screen.getByText("title R1")).toHaveClass("finding-title");
    expect(screen.getByText("The last page loses its final item.")).toBeInTheDocument();
    expect(screen.getByText("Suggested fix")).toBeInTheDocument();
    expect(screen.queryByText("Suggested verification")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "E2" }));
    expect(onSelect).toHaveBeenCalledWith("E2");
    fireEvent.click(screen.getByRole("button", { name: "E3" }));
    expect(onSelect).toHaveBeenCalledWith("E3");
  });

  it("FindingCard copies the suggested fix as text", () => {
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    render(<FindingCard finding={f} evidence={evidence} onSelectEvidence={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: /copy suggestion/i }));
    expect(writeText).toHaveBeenCalledWith("Use end, not end - 1.");
  });

  it("FindingCard in compact mode leaves out the detail and the suggestion", () => {
    render(<FindingCard finding={f} evidence={evidence} onSelectEvidence={() => {}} compact />);
    expect(screen.getByText("title R1")).toBeInTheDocument();
    expect(screen.getByText("Confirmed by Muse check")).toBeInTheDocument();
    expect(screen.getByText("Muse check: E2 shows the slice end moved by one")).toBeInTheDocument();
    expect(screen.queryByText("The last page loses its final item.")).not.toBeInTheDocument();
    expect(screen.queryByText("Suggested fix")).not.toBeInTheDocument();
  });

  it("FindingCard shows a check reason that cites nothing without chips", () => {
    const { container } = render(
      <FindingCard
        finding={{ ...f, check: { verdict: "disputed", reason: "Unlikely with page sizes under 1000.", evidence_ids: [] } }}
        evidence={evidence}
        onSelectEvidence={() => {}}
      />,
    );
    expect(screen.getByText("Disputed by Muse check")).toHaveClass("verdict-disputed");
    expect(container.querySelector(".check-reason")).toHaveTextContent(/^Muse check: Unlikely with page sizes under 1000\.$/);
  });

  it("FindingCard shows no location for a finding about no file", () => {
    const { container } = render(
      <FindingCard finding={{ ...f, file: "", line_start: 0, line_end: 0 }} evidence={evidence} onSelectEvidence={() => {}} />,
    );
    expect(container.querySelector(".loc-chip")).toBeNull();
  });
});
