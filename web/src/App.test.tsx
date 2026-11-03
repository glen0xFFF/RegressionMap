import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import App from "./App";
import type { Analysis } from "./types";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
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
});
