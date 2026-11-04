import { Fragment } from "react";
import { FindingCard } from "./FindingCard";
import { ReviewComment } from "./ReviewComment";
import { anchorFindings, anchorHypotheses, groupDiffsByFile, parseHunk } from "../diff";
import { evidenceDomId } from "../logic";
import type { Evidence, Hypothesis, ReviewFinding } from "../types";

const SIGN: Record<string, string> = { add: "+", del: "-", ctx: " ", hunk: "", meta: "" };

/**
 * PR-style diff of diff_hunk evidence with AI comments anchored on the referenced lines:
 * the hypotheses first, then the Muse review findings (already filtered and in display order).
 */
export function DiffView({
  evidence,
  hypotheses,
  findings = [],
  highlightId,
  onSelectEvidence,
}: {
  evidence: Evidence[];
  hypotheses: Hypothesis[];
  findings?: ReviewFinding[];
  highlightId: string | null;
  onSelectEvidence: (id: string) => void;
}) {
  const groups = groupDiffsByFile(evidence);
  if (groups.length === 0) return <div className="empty-state">No diff hunks between baseline and HEAD.</div>;
  const anchors = anchorHypotheses(hypotheses, evidence);
  const found = anchorFindings(findings, evidence);

  return (
    <div className="diffview">
      {groups.map((g) => {
        const fileComments = anchors.fileLevel.get(g.file) ?? [];
        const fileFindings = found.fileLevel.get(g.file) ?? [];
        return (
          <div key={g.file} className="file-diff">
            <div className="file-header" data-testid={`file-header-${g.file}`}>
              <code className="file-path">{g.file || "(unknown file)"}</code>
              <span className="counts mono">
                <span className="c-add">+{g.add}</span> <span className="c-del">−{g.del}</span>
              </span>
            </div>
            {fileComments.length + fileFindings.length > 0 && (
              <div className="file-comments" data-testid={`file-comments-${g.file}`}>
                {fileComments.map((h, i) => (
                  <div key={`${h.rank}-${i}`} className="inline-comment">
                    <ReviewComment hypothesis={h} evidence={evidence} onSelectEvidence={onSelectEvidence} compact />
                  </div>
                ))}
                {fileFindings.map((f, i) => (
                  <div key={`${f.id}-${i}`} className="inline-comment inline-finding">
                    <FindingCard finding={f} evidence={evidence} onSelectEvidence={onSelectEvidence} compact />
                  </div>
                ))}
              </div>
            )}
            {g.hunks.map((e) => {
              const rows = parseHunk(e.excerpt);
              const rowAnchors = anchors.rows.get(e.id);
              const rowFindings = found.rows.get(e.id);
              return (
                <div key={e.id} id={evidenceDomId(e.id)} className={highlightId === e.id ? "hunk ev-hl" : "hunk"}>
                  <div className="hunk-title small">
                    <span className="chip chip-static">{e.id}</span> <span className="muted">{e.title}</span>
                  </div>
                  <div className="drows mono" role="table" aria-label={`diff ${e.id}`}>
                    {rows.map((r, i) => (
                      <Fragment key={i}>
                        <div className={`drow drow-${r.kind}`} role="row">
                          <span className="ln" data-testid="old-no">
                            {r.oldNo ?? ""}
                          </span>
                          <span className="ln" data-testid="new-no">
                            {r.newNo ?? ""}
                          </span>
                          <span className="sign">{SIGN[r.kind]}</span>
                          <span className="code">{r.text || " "}</span>
                        </div>
                        {rowAnchors?.get(i)?.map((h, j) => (
                          <div key={`c-${i}-${j}`} className="inline-comment">
                            <ReviewComment hypothesis={h} evidence={evidence} onSelectEvidence={onSelectEvidence} compact />
                          </div>
                        ))}
                        {rowFindings?.get(i)?.map((f, j) => (
                          <div key={`f-${i}-${j}`} className="inline-comment inline-finding">
                            <FindingCard finding={f} evidence={evidence} onSelectEvidence={onSelectEvidence} compact />
                          </div>
                        ))}
                      </Fragment>
                    ))}
                  </div>
                </div>
              );
            })}
          </div>
        );
      })}
    </div>
  );
}
