import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { EvidenceList } from "./EvidenceList";
import type { Evidence } from "../types";

afterEach(cleanup);

const evidence: Evidence[] = [
  { id: "E1", type: "test_failure", title: "failing test", file: "", line_start: 0, line_end: 0, excerpt: "AssertionError" },
  {
    id: "E2", type: "diff_hunk", title: "hunk", file: "src/p.ts", line_start: 3, line_end: 9,
    excerpt: "@@ -3,2 +3,2 @@\n-  return a;\n+  return a + 1;\n context",
  },
];

describe("EvidenceList", () => {
  it("renders an empty state", () => {
    render(<EvidenceList evidence={[]} highlightId={null} />);
    expect(screen.getByText(/No evidence items/)).toBeInTheDocument();
  });

  it("renders diff hunks with +/- line classes and highlights the selected item", () => {
    const { container } = render(<EvidenceList evidence={evidence} highlightId="E2" />);
    expect(container.querySelector("#evidence-E2")).toHaveClass("ev-hl");
    expect(container.querySelector("#evidence-E1")).not.toHaveClass("ev-hl");
    expect(screen.getByText("+  return a + 1;", { normalizer: (s) => s })).toHaveClass("dl-add");
    expect(screen.getByText("-  return a;", { normalizer: (s) => s })).toHaveClass("dl-del");
    expect(screen.getByText("@@ -3,2 +3,2 @@")).toHaveClass("dl-hunk");
    expect(screen.getByText("src/p.ts:L3-L9")).toBeInTheDocument();
  });
});
