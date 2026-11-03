import { RunBadge } from "./RunBadge";
import type { TestRun } from "../types";

export function TestRunView({ title, run }: { title: string; run: TestRun | null | undefined }) {
  if (!run) {
    return (
      <div className="run">
        <div className="run-head">
          <strong>{title}</strong> <span className="muted">no run recorded</span>
        </div>
      </div>
    );
  }
  const stdout = run.stdout ?? "";
  const stderr = run.stderr ?? "";
  return (
    <div className="run">
      <div className="run-head">
        <strong>{title}</strong>
        <RunBadge run={run} />
        <span className="mono">exit {run.exit_code}</span>
        <span className="muted">{run.duration_ms} ms</span>
        {run.timed_out && <span className="tag tag-warn">timed out</span>}
        {run.truncated && <span className="tag tag-warn">output truncated</span>}
      </div>
      <details>
        <summary>stdout ({stdout.length} chars)</summary>
        <pre className="output">{stdout || "(empty)"}</pre>
      </details>
      <details>
        <summary>stderr ({stderr.length} chars)</summary>
        <pre className="output">{stderr || "(empty)"}</pre>
      </details>
    </div>
  );
}
