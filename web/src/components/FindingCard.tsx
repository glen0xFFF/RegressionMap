import { EvidenceChips, Suggestion } from "./ReviewComment";
import { confidenceClass } from "../layout";
import { VERDICT_LABEL, findingLocation, verdictOf } from "../review";
import type { Evidence, ReviewFinding } from "../types";

// All model text is rendered as React text nodes (escaped), never as HTML.

/** One Muse review finding, written out like a review comment. */
export function FindingCard({
  finding: f,
  evidence,
  onSelectEvidence,
  compact = false,
}: {
  finding: ReviewFinding;
  evidence: Evidence[];
  onSelectEvidence: (id: string) => void;
  compact?: boolean;
}) {
  const verdict = verdictOf(f);
  const loc = findingLocation(f);
  const reason = f.check?.reason ?? "";
  return (
    <article className={`comment finding finding-${verdict}${compact ? " comment-compact" : ""}`}>
      <div className="avatar avatar-review" aria-hidden="true">
        MR
      </div>
      <div className="comment-main">
        <div className="comment-head">
          <span className="author">Muse review</span>
          <span className="rank-badge">{f.id}</span>
          <span className="lens-chip">{f.lens}</span>
          <span className={`verdict verdict-${verdict}`}>{VERDICT_LABEL[verdict]}</span>
          <span className="conf">
            <span className="sr-only">severity </span>
            <span className={confidenceClass(f.severity)}>{f.severity}</span>
          </span>
        </div>
        {loc && <code className="loc-chip">{loc}</code>}
        <div className="hyp-title finding-title">{f.title}</div>
        {!compact && f.detail && <p className="hyp-reason">{f.detail}</p>}
        {reason && (
          <div className="check-reason small">
            <span>{`Muse check: ${reason}`}</span>
            {(f.check.evidence_ids?.length ?? 0) > 0 && (
              <>
                {" "}
                <EvidenceChips ids={f.check.evidence_ids} evidence={evidence} onSelect={onSelectEvidence} />
              </>
            )}
          </div>
        )}
        <div className="cites small">
          <span className="muted">Evidence </span>
          <EvidenceChips ids={f.evidence_ids} evidence={evidence} onSelect={onSelectEvidence} />
        </div>
        {!compact && f.suggestion && <Suggestion text={f.suggestion} label="Suggested fix" />}
      </div>
    </article>
  );
}
