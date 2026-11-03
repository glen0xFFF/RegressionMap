import { HypothesesPanel } from "./HypothesesPanel";
import { SparkleIcon } from "./Icons";
import { VerifyPanel } from "./VerifyPanel";
import type { Analysis, Command, Verification } from "../types";

export function RightPanel({
  analysis: a,
  elapsed,
  commands,
  onSelectEvidence,
  onVerified,
}: {
  analysis: Analysis | null;
  elapsed: string | null;
  commands: Command[];
  onSelectEvidence: (id: string) => void;
  onVerified: (v: Verification) => void;
}) {
  return (
    <aside className="right scroll">
      <div className="right-head">
        <span className="accent">
          <SparkleIcon />
        </span>
        <h2>AI Analysis</h2>
        {elapsed && <span className="pill pill-time">{`${elapsed} analysis time`}</span>}
      </div>

      {!a ? (
        <div className="placeholder muted">
          Run an analysis to see ranked root-cause hypotheses here. Every claim cites evidence ids you can check.
        </div>
      ) : a.status === "running" ? (
        <div className="placeholder muted">Analysis running… hypotheses appear when the tests and the model call finish.</div>
      ) : (
        <>
          <HypothesesPanel
            model={a.model}
            evidence={a.evidence ?? []}
            changedFiles={a.changed_files}
            onSelectEvidence={onSelectEvidence}
          />
          <VerifyPanel
            analysisId={a.id}
            commands={commands}
            defaultCommandId={a.command?.id ?? commands[0]?.id ?? ""}
            verifications={a.verifications ?? []}
            onVerified={onVerified}
          />
        </>
      )}
    </aside>
  );
}
