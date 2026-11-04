import { describe, expect, it } from "vitest";
import { redactPath } from "./diff";
import entries from "./testdata/redact-path-parity.json";

// The golden fixture is shared with backend/internal/evidence/redactpath_parity_test.go.
// Every "want" is written by the server's RedactPath, never by hand, so this fails when
// the client copy of secretRes in diff.ts drifts from the server's; the Go test fails
// when the server drifts from the fixture.
const REGEN = "cd backend && go test ./internal/evidence -run TestRedactPathParity -update";

describe("redactPath parity with the server's RedactPath", () => {
  it("has enough entries to guard the copy", () => {
    expect(entries.length).toBeGreaterThanOrEqual(45);
  });

  it("gives the server's output for every fixture entry", () => {
    const diffs = entries
      .map((e) => ({ in: e.in, want: e.want, got: redactPath(e.in) }))
      .filter((d) => d.got !== d.want);
    expect(
      diffs,
      `redactPath in web/src/diff.ts differs from the server's RedactPath; make it match, or if the server changed on purpose regenerate the fixture with: ${REGEN}`,
    ).toEqual([]);
  });
});
