import { useEffect, useState, type ReactNode } from "react";
import { DiffView } from "./DiffView";
import { EvidenceList } from "./EvidenceList";
import { RunBadge } from "./RunBadge";
import { ClearIcon, PlayIcon } from "./Icons";
import { diffEvidence } from "../layout";
import { evidenceDomId } from "../logic";
import type { Analysis, TestRun } from "../types";

type Tab = "output" | "diff" | "evidence";
const TABS: { id: Tab; label: string }[] = [
  { id: "output", label: "Output" },
  { id: "diff", label: "Diff" },
  { id: "evidence", label: "Evidence" },
];

function RunSummary({ label, run }: { label: string; run: TestRun | null | undefined }) {
  return (
    <span className="run-sum">
      <span className="run-sum-label">{label}</span>
      <RunBadge run={run} />
      {run ? (
        <>
          <span className="mono">exit {run.exit_code}</span>
          <span className="muted">{run.duration_ms} ms</span>
          {run.timed_out && <span className="pill pill-medium">timed out</span>}
          {run.truncated && <span className="pill pill-low">truncated</span>}
        </>
      ) : (
        <span className="muted">no run</span>
      )}
    </span>
  );
}

export function MiddlePanel({
  analysis: a,
  highlight,
  highlightSeq = 0,
  form,
  onAnalyze,
  onClear,
  canAnalyze,
  busy,
  error,
  onSelectEvidence = () => {},
}: {
  analysis: Analysis | null;
  highlight: string | null;
  highlightSeq?: number;
  form: ReactNode;
  onAnalyze: () => void;
  onClear: () => void;
  canAnalyze: boolean;
  busy: boolean;
  error?: ReactNode;
  onSelectEvidence?: (id: string) => void;
}) {
  const [tab, setTab] = useState<Tab>("output");

  // Clicking an evidence chip switches to the Evidence tab...
  useEffect(() => {
    if (highlight) setTab("evidence");
  }, [highlight, highlightSeq]);
  // ...and scrolls the item into view once it is rendered.
  useEffect(() => {
    if (highlight && tab === "evidence") {
      document.getElementById(evidenceDomId(highlight))?.scrollIntoView?.({ behavior: "smooth", block: "center" });
    }
  }, [highlight, highlightSeq, tab]);

  const evidence = a?.evidence ?? [];
  const diffs = diffEvidence(evidence);
  const cur = a?.current_run;
  const running = a?.status === "running";

  let body: ReactNode;
  if (!a) {
    body = <div className="empty-state">Pick a repository and baseline, then Analyze.</div>;
  } else if (tab === "output") {
    body = cur ? (
      <pre className="terminal" data-testid="terminal">
        {`$ ${a.command?.argv?.join(" ") ?? ""}\n`}
        {cur.stdout}
        {cur.stderr && <span className="term-err">{cur.stderr}</span>}
        {!cur.stdout && !cur.stderr && "(no output)"}
      </pre>
    ) : (
      <div className="empty-state">{running ? "Running tests at baseline and HEAD…" : "No HEAD test run recorded."}</div>
    );
  } else if (tab === "diff") {
    body =
      running && diffs.length === 0 ? (
        <div className="empty-state">Waiting for results…</div>
      ) : (
        <DiffView
          evidence={evidence}
          hypotheses={a.model?.status === "ok" ? (a.model.result?.hypotheses ?? []) : []}
          highlightId={highlight}
          onSelectEvidence={onSelectEvidence}
        />
      );
  } else {
    body =
      running && evidence.length === 0 ? (
        <div className="empty-state">Waiting for results…</div>
      ) : (
        <EvidenceList evidence={evidence} highlightId={highlight} />
      );
  }

  return (
    <section className="middle">
      <div className="topbar">
        <div className="segmented" role="tablist" aria-label="Analysis views">
          {TABS.map((t) => (
            <button
              key={t.id}
              type="button"
              role="tab"
              aria-selected={tab === t.id}
              className={tab === t.id ? "seg active" : "seg"}
              onClick={() => setTab(t.id)}
            >
              {t.label}
            </button>
          ))}
        </div>
        <div className="form-compact">{form}</div>
      </div>

      {a && (
        <div className="run-strip" data-testid="run-strip">
          <RunSummary label="Baseline" run={a.baseline_run} />
          <RunSummary label="HEAD" run={a.current_run} />
          <span className={`status status-${a.status}`}>{a.status}</span>
        </div>
      )}
      {a?.status === "error" && (
        <div className="banner banner-error" role="alert">
          Analysis failed: {a.error || "unknown error"}
        </div>
      )}
      {error}

      <div className="content scroll">{body}</div>

      <div className="bottombar">
        <button type="button" className="btn-primary" onClick={onAnalyze} disabled={!canAnalyze} data-testid="analyze-btn">
          <PlayIcon size={14} /> {busy ? "Analyzing…" : "Analyze"}
        </button>
        <button type="button" className="btn-outline" onClick={onClear} disabled={busy}>
          <ClearIcon size={14} /> Clear
        </button>
      </div>
    </section>
  );
}
