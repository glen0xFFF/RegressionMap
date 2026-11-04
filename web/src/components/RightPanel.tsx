import { HypothesesPanel } from "./HypothesesPanel";
import { SparkleIcon } from "./Icons";
import { ReviewPanel } from "./ReviewPanel";
import { VerifyPanel } from "./VerifyPanel";
import type { Analysis, Command, Verification } from "../types";

export function RightPanel({
  analysis: a,
  elapsed,
  commands,
  onSelectEvidence,
  onVerified,
  showRefuted,
  onToggleRefuted,
}: {
  analysis: Analysis | null;
  elapsed: string | null;
  commands: Command[];
  onSelectEvidence: (id: string) => void;
  onVerified: (v: Verification) => void;
  showRefuted: boolean;
  onToggleRefuted: () => void;
}) {
  const running = a?.status === "running";
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
      ) : running && !a.model ? (
        <div className="placeholder muted">Analysis running… hypotheses appear when the tests and the model call finish.</div>
      ) : (
        // While the review chain runs, the hypotheses already exist: show them, and
        // keep the verify card back until the analysis has ended.
        <>
          <HypothesesPanel
            model={a.model}
            evidence={a.evidence ?? []}
            changedFiles={a.changed_files}
            onSelectEvidence={onSelectEvidence}
          />
          <ReviewPanel
            review={a.review}
            running={running}
            evidence={a.evidence ?? []}
            showRefuted={showRefuted}
            onToggleRefuted={onToggleRefuted}
            onSelectEvidence={onSelectEvidence}
          />
          {!running && (
            <VerifyPanel
              analysisId={a.id}
              commands={commands}
              defaultCommandId={a.command?.id ?? commands[0]?.id ?? ""}
              verifications={a.verifications ?? []}
              onVerified={onVerified}
            />
          )}
        </>
      )}
    </aside>
  );
}
