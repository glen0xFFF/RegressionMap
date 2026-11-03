import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { VerifyPanel } from "./VerifyPanel";
import { HypothesesPanel } from "./HypothesesPanel";
import { RunBadge } from "./RunBadge";
import type { Command, Evidence, ModelResult, TestRun } from "../types";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const commands: Command[] = [{ id: "node-test", argv: ["node", "--test"], description: "Run node:test suite" }];

const passRun: TestRun = {
  exit_code: 0, passed: true, timed_out: false, truncated: false, duration_ms: 5, stdout: "ok 1", stderr: "",
};

describe("VerifyPanel", () => {
  it("shows exact argv, an unchecked approval box, and a disabled Run button", () => {
    render(<VerifyPanel analysisId="a1" commands={commands} defaultCommandId="node-test" verifications={[]} onVerified={() => {}} />);
    expect(screen.getByTestId("verify-argv")).toHaveTextContent('["node","--test"]');
    const box = screen.getByLabelText(/I approve running this command/i) as HTMLInputElement;
    expect(box.checked).toBe(false);
    expect(screen.getByRole("button", { name: /run verification/i })).toBeDisabled();
  });

  it("does not call the backend until approved; then posts approved:true", async () => {
    const fetchMock = vi.fn(async () =>
      new Response(JSON.stringify({ command_id: "node-test", argv: ["node", "--test"], at: "2026-01-01T00:00:00Z", run: passRun }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const onVerified = vi.fn();
    render(<VerifyPanel analysisId="a1" commands={commands} defaultCommandId="node-test" verifications={[]} onVerified={onVerified} />);

    fireEvent.click(screen.getByRole("button", { name: /run verification/i }));
    expect(fetchMock).not.toHaveBeenCalled();

    fireEvent.click(screen.getByLabelText(/I approve running this command/i));
    const btn = screen.getByRole("button", { name: /run verification/i });
    expect(btn).toBeEnabled();
    fireEvent.click(btn);

    await waitFor(() => expect(onVerified).toHaveBeenCalled());
    const calls = fetchMock.mock.calls as unknown as [string, RequestInit][];
    expect(calls[0][0]).toBe("/api/analyses/a1/verify");
    expect(JSON.parse(String(calls[0][1].body))).toEqual({ command_id: "node-test", approved: true });
    // approval resets after a run so each run needs fresh consent
    expect((screen.getByLabelText(/I approve running this command/i) as HTMLInputElement).checked).toBe(false);
  });

  it("shows backend error messages", async () => {
    vi.stubGlobal("fetch", vi.fn(async () =>
      new Response(JSON.stringify({ error: { code: "not_approved", message: "approval required" } }), { status: 400 }),
    ));
    render(<VerifyPanel analysisId="a1" commands={commands} defaultCommandId="node-test" verifications={[]} onVerified={() => {}} />);
    fireEvent.click(screen.getByLabelText(/I approve running this command/i));
    fireEvent.click(screen.getByRole("button", { name: /run verification/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent("approval required");
  });
});

describe("RunBadge", () => {
  it("shows FAIL when exit code non-zero even if passed=true", () => {
    render(<RunBadge run={{ ...passRun, exit_code: 2, passed: true }} />);
    expect(screen.getByText("FAIL")).toBeInTheDocument();
    expect(screen.queryByText("PASS")).not.toBeInTheDocument();
  });
});

const evidence: Evidence[] = [
  { id: "E1", type: "test_failure", title: "failing test", file: "", line_start: 0, line_end: 0, excerpt: "boom" },
];

describe("HypothesesPanel", () => {
  it("labels hypotheses as AI-generated and unverified, shows confidence as text, flags unknown ids", () => {
    const model: ModelResult = {
      status: "ok",
      model_id: "muse-spark-1.3",
      rejected: ["hypothesis 2 cites unknown evidence id E99"],
      result: {
        summary: "<b>not html</b>",
        hypotheses: [{
          rank: 1, title: "Off by one", file: "src/p.ts", line_start: 4, line_end: 4, reason: "because",
          supporting_evidence_ids: ["E1", "E7"], counter_evidence_ids: [], verification_suggestion: "run tests",
          confidence_label: "medium",
        }],
        limitations: ["only saw one file"],
      },
    };
    const onSelect = vi.fn();
    render(<HypothesesPanel model={model} evidence={evidence} onSelectEvidence={onSelect} />);
    expect(screen.getByText(/AI-generated hypotheses — not verified/)).toBeInTheDocument();
    expect(screen.getByText("medium")).toBeInTheDocument();
    expect(screen.queryByText(/%/)).not.toBeInTheDocument();
    expect(screen.getByText("only saw one file")).toBeInTheDocument();
    expect(screen.getByText("hypothesis 2 cites unknown evidence id E99")).toBeInTheDocument();
    // raw text, not HTML
    expect(screen.getByText("<b>not html</b>")).toBeInTheDocument();
    // unknown id flagged
    expect(screen.getByTitle(/unknown evidence id/i)).toHaveTextContent("E7");
    fireEvent.click(screen.getByRole("button", { name: "E1" }));
    expect(onSelect).toHaveBeenCalledWith("E1");
  });

  it("shows disabled state message", () => {
    render(<HypothesesPanel model={{ status: "disabled", model_id: "m" }} evidence={[]} onSelectEvidence={() => {}} />);
    expect(screen.getByText(/Meta model not configured — set MODEL_API_KEY on the backend/)).toBeInTheDocument();
  });

  it("shows invalid state with message and rejected list", () => {
    render(<HypothesesPanel model={{ status: "invalid", model_id: "m", message: "bad json", rejected: ["r1"] }} evidence={[]} onSelectEvidence={() => {}} />);
    expect(screen.getByText(/bad json/)).toBeInTheDocument();
    expect(screen.getByText("r1")).toBeInTheDocument();
  });

  it("shows error state with message", () => {
    render(<HypothesesPanel model={{ status: "error", model_id: "m", message: "timeout calling model" }} evidence={[]} onSelectEvidence={() => {}} />);
    expect(screen.getByText(/timeout calling model/)).toBeInTheDocument();
  });
});
