import { DiffExcerpt } from "./DiffExcerpt";
import { evidenceDomId, formatLines } from "../logic";
import type { Evidence } from "../types";

export function EvidenceList({ evidence, highlightId }: { evidence: Evidence[]; highlightId: string | null }) {
  if (evidence.length === 0) {
    return <p className="muted">No evidence items were collected.</p>;
  }
  return (
    <ol className="evidence">
      {evidence.map((e) => {
        const lines = formatLines(e.line_start, e.line_end);
        return (
          <li key={e.id} id={evidenceDomId(e.id)} className={highlightId === e.id ? "ev ev-hl" : "ev"}>
            <div className="ev-head">
              <span className="chip chip-static">{e.id}</span>
              <span className="tag">{e.type}</span>
              <strong>{e.title}</strong>
              {e.file && <span className="mono muted">{lines ? `${e.file}:${lines}` : e.file}</span>}
            </div>
            {e.type === "diff_hunk" ? <DiffExcerpt text={e.excerpt} /> : <pre className="output">{e.excerpt}</pre>}
          </li>
        );
      })}
    </ol>
  );
}
