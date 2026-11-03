import { runBadge } from "../logic";
import type { TestRun } from "../types";

/** Badge derived from exit code only (see runBadge). */
export function RunBadge({ run }: { run: TestRun | null | undefined }) {
  const b = runBadge(run);
  return <span className={`badge badge-${b.toLowerCase()}`}>{b}</span>;
}
