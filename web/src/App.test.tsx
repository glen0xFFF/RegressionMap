import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import App from "./App";
import type { Analysis, ModelResult, Review, ReviewFinding } from "./types";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  window.sessionStorage.clear();
});

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const failRun = { exit_code: 1, passed: false, timed_out: false, truncated: false, duration_ms: 3, stdout: "not ok 1", stderr: "" };
const passRun = { ...failRun, exit_code: 0, passed: true, stdout: "ok 1" };

const doneAnalysis: Analysis = {
  id: "a1",
  status: "done",
  repo: "pagination-demo",
  baseline: "b".repeat(40),
  current: "c".repeat(40),
  command: { id: "node-test", argv: ["node", "--test"] },
  baseline_run: passRun,
  current_run: failRun,
  changed_files: [{ path: "src/pagination.ts", status: "M" }],
  evidence: [{ id: "E1", type: "diff_hunk", title: "hunk", file: "src/pagination.ts", line_start: 3, line_end: 4, excerpt: "+x" }],
  model: { status: "disabled", model_id: "muse-spark-1.3" },
  verifications: [],
};

describe("App", () => {
  it("shows a clear message when the backend is down", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => {
      throw new TypeError("Failed to fetch");
    }));
    render(<App />);
    expect((await screen.findAllByText(/Cannot reach the RegressionMap backend/)).length).toBeGreaterThan(0);
  });

  it("starts an analysis, polls while running, and renders results with model disabled", async () => {
    let polls = 0;
    const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
      if (url === "/api/health") return json({ ok: true, model_configured: false, model_id: "muse-spark-1.3" });
      if (url === "/api/repos") return json({ repos: [{ name: "pagination-demo" }] });
      if (url === "/api/repos/pagination-demo/commits")
        return json({ commits: [{ sha: "b".repeat(40), short: "bbbbbbb", subject: "baseline" }] });
      if (url === "/api/commands")
        return json({ commands: [{ id: "node-test", argv: ["node", "--test"], description: "Run node:test suite" }] });
      if (url === "/api/analyses" && init?.method === "POST") {
        expect(JSON.parse(String(init.body))).toEqual({ repo: "pagination-demo", baseline: "b".repeat(40), command_id: "node-test" });
        return json({ ...doneAnalysis, status: "running", baseline_run: null, current_run: null, evidence: [], model: null }, 202);
      }
      if (url === "/api/analyses/a1") {
        polls++;
        return json(polls < 2 ? { ...doneAnalysis, status: "running", model: null } : doneAnalysis);
      }
      return json({ error: { code: "not_found", message: "nope" } }, 404);
    });
    vi.stubGlobal("fetch", fetchMock);
    render(<App />);

    const start = await screen.findByTestId("analyze-btn");
    await waitFor(() => expect(start).toBeEnabled());
    fireEvent.click(start);

    expect(await screen.findByText(/Meta model not configured/, {}, { timeout: 4000 })).toBeInTheDocument();
    expect(polls).toBeGreaterThanOrEqual(2);
    expect(screen.getByText("src/pagination.ts")).toBeInTheDocument();
    // baseline PASS, current FAIL from exit codes
    expect(screen.getAllByText("PASS").length).toBeGreaterThan(0);
    expect(screen.getAllByText("FAIL").length).toBeGreaterThan(0);
    expect(screen.getByLabelText(/I approve running this command/i)).not.toBeChecked();
    // three-pane shell
    expect(screen.getByText("AI Analysis")).toBeInTheDocument();
    expect(screen.getByText(/analysis time$/)).toBeInTheDocument();
    const recent = screen.getByRole("button", { name: /pagination-demo/ });
    expect(recent.querySelector(".dot-fail")).not.toBeNull();
    // Clear empties the view; History lists the session analysis
    fireEvent.click(screen.getByRole("button", { name: /clear/i }));
    expect(screen.getByText(/Pick a repository and baseline, then Analyze/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "History" }));
    expect(screen.getByTestId("history")).toHaveTextContent("a1");
  });

  it("review appears when the poll finishes", async () => {
    let polls = 0;
    vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
      if (url === "/api/analyses" && init?.method === "POST")
        return json({ ...doneAnalysis, status: "running", baseline_run: null, current_run: null, evidence: [], model: null, review: null }, 202);
      if (url === "/api/analyses/a1") {
        polls++;
        return json(
          polls < 2
            ? { ...doneAnalysis, status: "running", model: okModel, review: null }
            : { ...doneAnalysis, model: okModel, review: review([confirmed, refuted]) },
        );
      }
      return setup(url);
    }));
    render(<App />);

    const start = await screen.findByTestId("analyze-btn");
    await waitFor(() => expect(start).toBeEnabled());
    fireEvent.click(start);

    expect(await screen.findByText("Muse review running…", {}, { timeout: 4000 })).toBeInTheDocument();
    expect(screen.getByText("end index off by one")).toBeInTheDocument();
    expect(screen.queryByText("Verify fix")).not.toBeInTheDocument();

    expect(await screen.findByText("Last page drops one item", {}, { timeout: 4000 })).toBeInTheDocument();
    expect(polls).toBeGreaterThanOrEqual(2);
    expect(screen.queryByText("Muse review running…")).not.toBeInTheDocument();
    expect(screen.queryByText("Refuted worry")).not.toBeInTheDocument();
    expect(screen.getByText("1 confirmed")).toHaveClass("recent-confirmed");
  });

  it("the refuted toggle starts off and resets when another analysis opens", async () => {
    window.sessionStorage.setItem(
      "regressionmap.recent",
      JSON.stringify([
        { id: "a1", repo: "pagination-demo", dot: "fail", startedAt: "2026-11-04T10:00:00.000Z", confirmed: 1 },
        { id: "a2", repo: "other-demo", dot: "fail", startedAt: "2026-11-04T09:00:00.000Z" },
      ]),
    );
    const other = { ...refuted, id: "R1", title: "Other refuted" };
    vi.stubGlobal("fetch", vi.fn(async (url: string) => {
      if (url === "/api/analyses/a1") return json({ ...doneAnalysis, model: okModel, review: review([confirmed, refuted]) });
      if (url === "/api/analyses/a2") return json({ ...doneAnalysis, id: "a2", repo: "other-demo", model: okModel, review: review([other]) });
      return setup(url);
    }));
    render(<App />);

    fireEvent.click(await screen.findByRole("button", { name: /pagination-demo/ }));
    expect(await screen.findByText("Last page drops one item")).toBeInTheDocument();
    expect(screen.queryByText("Refuted worry")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Show 1 refuted" }));
    expect(screen.getByText("Refuted worry")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Hide 1 refuted" })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /other-demo/ }));
    expect(await screen.findByRole("button", { name: "Show 1 refuted" })).toBeInTheDocument();
    expect(screen.queryByText("Other refuted")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Show 1 refuted" }));
    expect(screen.getByText("Other refuted")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /pagination-demo/ }));
    expect(await screen.findByText("Last page drops one item")).toBeInTheDocument();
    expect(screen.queryByText("Refuted worry")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Show 1 refuted" })).toBeInTheDocument();
  });
});

function setup(url: string): Response {
  if (url === "/api/health") return json({ ok: true, model_configured: true, model_id: "muse-spark-1.3" });
  if (url === "/api/repos") return json({ repos: [{ name: "pagination-demo" }] });
  if (url === "/api/repos/pagination-demo/commits")
    return json({ commits: [{ sha: "b".repeat(40), short: "bbbbbbb", subject: "baseline" }] });
  if (url === "/api/commands")
    return json({ commands: [{ id: "node-test", argv: ["node", "--test"], description: "Run node:test suite" }] });
  return json({ error: { code: "not_found", message: "nope" } }, 404);
}

const okModel: ModelResult = {
  status: "ok",
  model_id: "muse-spark-1.3",
  result: {
    summary: "",
    limitations: [],
    hypotheses: [
      {
        rank: 1, title: "end index off by one", file: "src/pagination.ts", line_start: 3, line_end: 4, reason: "r",
        supporting_evidence_ids: ["E1"], counter_evidence_ids: [], verification_suggestion: "", confidence_label: "high",
      },
    ],
  },
};

const confirmed: ReviewFinding = {
  id: "R1", lens: "code", severity: "high", title: "Last page drops one item", detail: "d", suggestion: "",
  file: "src/pagination.ts", line_start: 3, line_end: 3, evidence_ids: ["E1"],
  check: { verdict: "confirmed", reason: "E1 shows it", evidence_ids: ["E1"] },
};
const refuted: ReviewFinding = {
  ...confirmed, id: "R2", severity: "low", title: "Refuted worry",
  check: { verdict: "refuted", reason: "E1 shows otherwise", evidence_ids: ["E1"] },
};

function review(findings: ReviewFinding[]): Review {
  const n = (v: string) => findings.filter((f) => f.check.verdict === v).length;
  return {
    status: "ok", model_id: "muse-spark-1.3", message: "", check_status: "ok", check_message: "", summary: "",
    counts: { confirmed: n("confirmed"), disputed: n("disputed"), refuted: n("refuted"), unchecked: n("unchecked") },
    findings, rejected: [],
  };
}
