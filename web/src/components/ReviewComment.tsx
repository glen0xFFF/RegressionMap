import { useState } from "react";
import { CopyIcon } from "./Icons";
import { confidenceClass } from "../layout";
import { formatLines, resolveEvidenceIds } from "../logic";
import type { Evidence, Hypothesis } from "../types";

// All model text is rendered as React text nodes (escaped), never as HTML.

export function EvidenceChips({
  ids,
  evidence,
  onSelect,
}: {
  ids: string[] | null | undefined;
  evidence: Evidence[];
  onSelect: (id: string) => void;
}) {
  const resolved = resolveEvidenceIds(ids, evidence);
  if (resolved.length === 0) return <span className="muted small">none</span>;
  return (
    <>
      {resolved.map((r) =>
        r.known ? (
          <button key={r.id} type="button" className="chip" title={r.evidence?.title} onClick={() => onSelect(r.id)}>
            {r.id}
          </button>
        ) : (
          <span key={r.id} className="chip chip-unknown" title="unknown evidence id — not in this analysis">
            {r.id}
          </span>
        ),
      )}
    </>
  );
}

export function hypothesisLocation(h: Hypothesis): string {
  if (!h.file) return "";
  const l = formatLines(h.line_start, h.line_end);
  return l ? `${h.file}:${l}` : h.file;
}

export function Suggestion({ text, label = "Suggested verification" }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  async function copy() {
    try {
      await navigator.clipboard?.writeText(text);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      /* clipboard unavailable */
    }
  }
  return (
    <details className="suggest">
      <summary>{label}</summary>
      <div className="suggest-box">
        <code className="suggest-text">{text}</code>
        <button type="button" className="icon-btn" aria-label="Copy suggestion" title={copied ? "Copied" : "Copy (text only — never executed)"} onClick={copy}>
          <CopyIcon size={14} />
        </button>
      </div>
      <div className="muted small">Text only — RegressionMap never executes model suggestions.</div>
    </details>
  );
}

export function ReviewComment({
  hypothesis: h,
  evidence,
  onSelectEvidence,
  compact = false,
  heading,
}: {
  hypothesis: Hypothesis;
  evidence: Evidence[];
  onSelectEvidence: (id: string) => void;
  compact?: boolean;
  heading?: React.ReactNode;
}) {
  const loc = hypothesisLocation(h);
  return (
    <article className={compact ? "comment comment-compact" : "comment"}>
      <div className="avatar" aria-hidden="true">
        RM
      </div>
      <div className="comment-main">
        <div className="comment-head">
          <span className="author">RegressionMap analysis</span>
          <span className="rank-badge">#{h.rank}</span>
          {heading}
          <span className="conf">
            <span className="sr-only">confidence </span>
            <span className={confidenceClass(h.confidence_label)}>{h.confidence_label}</span>
          </span>
        </div>
        {loc && <code className="loc-chip">{loc}</code>}
        <div className="hyp-title">{h.title}</div>
        <p className="hyp-reason">{h.reason}</p>
        <div className="cites small">
          <span className="muted">Evidence </span>
          <EvidenceChips ids={h.supporting_evidence_ids} evidence={evidence} onSelect={onSelectEvidence} />
          {(h.counter_evidence_ids?.length ?? 0) > 0 && (
            <>
              <span className="muted"> · Counter </span>
              <EvidenceChips ids={h.counter_evidence_ids} evidence={evidence} onSelect={onSelectEvidence} />
            </>
          )}
        </div>
        {h.verification_suggestion && !compact && <Suggestion text={h.verification_suggestion} />}
      </div>
    </article>
  );
}
