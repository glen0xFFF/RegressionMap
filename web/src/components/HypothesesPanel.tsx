import { AlertCircleIcon, CheckCircleIcon } from "./Icons";
import { ReviewComment } from "./ReviewComment";
import { changesTable } from "../diff";
import { splitHypotheses } from "../layout";
import type { ChangedFile, Evidence, ModelResult } from "../types";

// All model text below is rendered as React text nodes (escaped), never as HTML.

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

function Walkthrough({
  summary,
  changedFiles,
  evidence,
}: {
  summary?: string;
  changedFiles?: ChangedFile[] | null;
  evidence: Evidence[];
}) {
  const rows = changesTable(changedFiles, evidence);
  if (!summary && rows.length === 0) return null;
  return (
    <div className="card" data-testid="walkthrough">
      <h3 className="card-h">Walkthrough</h3>
      {summary ? <p className="summary">{summary}</p> : <p className="muted small">No model summary.</p>}
      {rows.length > 0 && (
        <>
          <h4>Changes</h4>
          <table className="changes">
            <thead>
              <tr>
                <th>File</th>
                <th>What changed</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.path}>
                  <td>
                    <code>{r.path}</code>
                  </td>
                  <td className="mono small">
                    <span className={`fs fs-${r.status.charAt(0)}`}>{r.status}</span>{" "}
                    {r.hunks > 0 ? (
                      <>
                        <span className="c-add">+{r.add}</span> <span className="c-del">−{r.del}</span>
                      </>
                    ) : (
                      <span className="muted">no hunk collected</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </div>
  );
}

export function HypothesesPanel({
  model,
  evidence,
  changedFiles,
  onSelectEvidence,
}: {
  model: ModelResult | null | undefined;
  evidence: Evidence[];
  changedFiles?: ChangedFile[] | null;
  onSelectEvidence: (id: string) => void;
}) {
  const label = (
    <div className="ai-label">
      {model?.model_id
        ? `AI-generated hypotheses — not verified · ${model.model_id}`
        : "AI-generated hypotheses — not verified"}
    </div>
  );

  if (!model || model.status !== "ok") {
    let card: React.ReactNode;
    if (!model) card = <div className="card muted">No model result yet.</div>;
    else if (model.status === "disabled")
      card = (
        <div className="card card-warn">
          Meta model not configured — set MODEL_API_KEY on the backend. Test runs and evidence are still shown.
          {model.message && <p className="muted small">{model.message}</p>}
        </div>
      );
    else if (model.status === "error")
      card = <div className="card card-error">Model call failed: {model.message || "unknown error"}</div>;
    else card = <div className="card card-error">Model output was rejected as invalid: {model.message || "no details"}</div>;
    return (
      <div className="hyp-panel">
        {label}
        {card}
        <Walkthrough changedFiles={changedFiles} evidence={evidence} />
        <Rejected items={model?.rejected} />
      </div>
    );
  }

  const result = model.result;
  const { root, others } = splitHypotheses(result?.hypotheses);
  const ranked = (root ? 1 : 0) + others.length;
  const rejected = model.rejected?.length ?? 0;

  return (
    <div className="hyp-panel">
      {label}
      <Walkthrough summary={result?.summary} changedFiles={changedFiles} evidence={evidence} />

      <div className="hyp-count">{`Hypotheses: ${ranked} ranked · ${rejected} rejected`}</div>

      {!root ? (
        <div className="card muted">The model returned no hypotheses.</div>
      ) : (
        <details className="group" open>
          <summary>
            <span className="icon-red">
              <AlertCircleIcon size={14} />
            </span>{" "}
            Root cause
          </summary>
          <div className="card card-root" data-testid="root-cause">
            <ReviewComment
              hypothesis={root}
              evidence={evidence}
              onSelectEvidence={onSelectEvidence}
              heading={<h3 className="root-h">Root Cause</h3>}
            />
          </div>
        </details>
      )}

      {others.length > 0 && (
        <details className="group" open>
          <summary>
            <CheckCircleIcon size={14} /> Other hypotheses ({others.length})
          </summary>
          <div className="others" data-testid="other-hypotheses">
            {others.map((h, i) => (
              <div key={`${h.rank}-${i}`} className="card">
                <ReviewComment hypothesis={h} evidence={evidence} onSelectEvidence={onSelectEvidence} />
              </div>
            ))}
          </div>
        </details>
      )}

      <details className="group" open>
        <summary>Limitations</summary>
        <div className="card">
          {result?.limitations && result.limitations.length > 0 ? (
            <ul>
              {result.limitations.map((l, i) => (
                <li key={i}>{l}</li>
              ))}
            </ul>
          ) : (
            <p className="muted small">None stated by the model.</p>
          )}
        </div>
      </details>
      <Rejected items={model.rejected} />
    </div>
  );
}
