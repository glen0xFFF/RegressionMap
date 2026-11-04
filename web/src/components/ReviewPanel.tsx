import { useId } from "react";
import { FindingCard } from "./FindingCard";
import { REVIEW_DISCLAIMER, lensCounts, verdictOf, visibleFindings } from "../review";
import type { Evidence, Review } from "../types";

// All model text below is rendered as React text nodes (escaped), never as HTML.

// fallback: the words HypothesesPanel uses for a failure line that has no message.
const NOT_OK: Record<Exclude<Review["status"], "ok">, { cls: string; lead: string; fallback: string }> = {
  disabled: { cls: "card card-warn", lead: "Muse review not available", fallback: "no details" },
  error: { cls: "card card-error", lead: "Muse review failed", fallback: "unknown error" },
  invalid: { cls: "card card-error", lead: "Muse review was rejected as invalid", fallback: "no details" },
  skipped: { cls: "card card-warn", lead: "Muse review skipped", fallback: "no details" },
};

function Rejected({ items }: { items?: string[] | null }) {
  if (!items || items.length === 0) return null;
  return (
    <details className="group" open>
      <summary>Rejected by validation ({items.length})</summary>
      <div className="card card-warn">
        <ul>
          {items.map((r, i) => (
            <li key={i}>{r}</li>
          ))}
        </ul>
      </div>
    </details>
  );
}

function Body({
  review: rv,
  evidence,
  showRefuted,
  onToggleRefuted,
  onSelectEvidence,
}: {
  review: Review;
  evidence: Evidence[];
  showRefuted: boolean;
  onToggleRefuted: () => void;
  onSelectEvidence: (id: string) => void;
}) {
  if (rv.status !== "ok") {
    // An unknown status, even an inherited key such as "toString", reads as failed.
    const s = Object.prototype.hasOwnProperty.call(NOT_OK, rv.status) ? NOT_OK[rv.status] : NOT_OK.error;
    return (
      <>
        <div className={s.cls}>{`${s.lead}: ${rv.message || s.fallback}`}</div>
        <Rejected items={rv.rejected} />
      </>
    );
  }

  const all = rv.findings ?? [];
  const lens = lensCounts(all);
  const c = rv.counts ?? { confirmed: 0, disputed: 0, refuted: 0, unchecked: 0 };
  const refuted = all.filter((f) => verdictOf(f) === "refuted").length;
  const shown = visibleFindings(rv, showRefuted);
  return (
    <>
      {rv.summary && <p className="summary">{rv.summary}</p>}
      <div className="review-counts small">
        <div>{`Security ${lens.security} · Code ${lens.code} · Feature ${lens.feature}`}</div>
        <div>{`${c.confirmed} confirmed · ${c.unchecked} not checked · ${c.disputed} disputed · ${c.refuted} refuted`}</div>
      </div>
      {(rv.check_status === "error" || rv.check_status === "invalid") && (
        <div className="card card-warn">{`Muse check ${rv.check_status}: ${rv.check_message || "no details"}`}</div>
      )}
      {all.length === 0 ? (
        <div className="card muted">No findings.</div>
      ) : (
        <div className="findings">
          {shown.map((f) => (
            <div key={f.id} className="card">
              <FindingCard finding={f} evidence={evidence} onSelectEvidence={onSelectEvidence} />
            </div>
          ))}
        </div>
      )}
      {refuted > 0 && (
        // The label names the action ("Show" / "Hide"), so no aria-pressed: a pressed
        // state on a label that changes would give a screen reader two signals.
        <button type="button" className="btn-outline btn-sm refuted-toggle" onClick={onToggleRefuted}>
          {showRefuted ? `Hide ${refuted} refuted` : `Show ${refuted} refuted`}
        </button>
      )}
      <Rejected items={rv.rejected} />
    </>
  );
}

/** The "Muse review" section of the right panel. */
export function ReviewPanel({
  review,
  running,
  evidence,
  showRefuted,
  onToggleRefuted,
  onSelectEvidence,
}: {
  review: Review | null | undefined;
  running: boolean;
  evidence: Evidence[];
  showRefuted: boolean;
  onToggleRefuted: () => void;
  onSelectEvidence: (id: string) => void;
}) {
  const headingId = useId();
  return (
    <section className="review-panel" data-testid="review-panel" aria-labelledby={headingId}>
      <div className="review-head">
        <h3 id={headingId} className="review-h">
          Muse review
        </h3>
        {review?.model_id && <span className="muted small">{review.model_id}</span>}
      </div>
      {!review ? (
        <div className="card muted">{running ? "Muse review running…" : "No Muse review for this analysis."}</div>
      ) : (
        <>
          <Body
            review={review}
            evidence={evidence}
            showRefuted={showRefuted}
            onToggleRefuted={onToggleRefuted}
            onSelectEvidence={onSelectEvidence}
          />
          <div className="ai-label">{REVIEW_DISCLAIMER}</div>
        </>
      )}
    </section>
  );
}
