import { HistoryIcon, KeyIcon, LogOutIcon, SearchIcon, SparkleIcon } from "./Icons";
import type { RecentEntry } from "../layout";
import type { Health, Loadable, Me } from "../types";

export type View = "analyze" | "history" | "keys";

/** Key management is offered to local callers and to admin-scope keys only. */
export function canManageKeys(me: Me | null | undefined): boolean {
  if (!me) return false;
  return me.mode === "local" || me.key.scope === "admin";
}

function fmtDate(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

export function Sidebar({
  view,
  onView,
  onNew,
  recent,
  onOpenRecent,
  health,
  activeId,
  me,
  onSignOut,
}: {
  view: View;
  onView: (v: View) => void;
  onNew: () => void;
  recent: RecentEntry[];
  onOpenRecent: (id: string) => void;
  health: Loadable<Health> | null;
  activeId?: string | null;
  me?: Me | null;
  /** Present only while an API key is stored. */
  onSignOut?: () => void;
}) {
  return (
    <aside className="sidebar">
      <div className="brand">
        <span className="logo mono" aria-hidden="true">
          rm
        </span>
        <span className="brand-name">RegressionMap</span>
      </div>

      <button type="button" className="btn-dark btn-block" onClick={onNew}>
        <SparkleIcon /> New Analysis
      </button>

      <nav className="nav">
        <button type="button" className={view === "analyze" ? "nav-item active" : "nav-item"} onClick={() => onView("analyze")}>
          <SearchIcon /> Analyze
        </button>
        <button type="button" className={view === "history" ? "nav-item active" : "nav-item"} onClick={() => onView("history")}>
          <HistoryIcon /> History
        </button>
        {canManageKeys(me) && (
          <button type="button" className={view === "keys" ? "nav-item active" : "nav-item"} onClick={() => onView("keys")}>
            <KeyIcon /> API Keys
          </button>
        )}
      </nav>

      <div className="section-label">RECENT</div>
      {recent.length === 0 ? (
        <p className="muted small pad-x">No analyses yet in this session.</p>
      ) : (
        <ul className="recent">
          {recent.map((r) => (
            <li key={r.id}>
              <button
                type="button"
                className={activeId === r.id ? "recent-item active" : "recent-item"}
                onClick={() => onOpenRecent(r.id)}
                title={`analysis ${r.id} · ${r.dot}`}
              >
                <span className={`dot dot-${r.dot}`} aria-hidden="true" />
                <span className="recent-repo">{r.repo}</span>
                <span className="recent-date muted">{fmtDate(r.startedAt)}</span>
              </button>
            </li>
          ))}
        </ul>
      )}

      <div className="sidebar-footer small">
        {!health || health.state === "loading" ? (
          <span className="muted">Connecting to backend…</span>
        ) : health.state === "error" ? (
          <span className="error" role="alert">
            <span className="dot dot-fail" aria-hidden="true" /> {health.error}
          </span>
        ) : health.data.model_configured ? (
          <span>
            <span className="dot dot-pass" aria-hidden="true" /> backend ok · {health.data.model_id}
          </span>
        ) : (
          <span>
            <span className="dot dot-pass" aria-hidden="true" /> backend ok ·{" "}
            <span className="warn">model not configured</span>
          </span>
        )}
        {(me?.mode === "key" || onSignOut) && (
          <div className="whoami">
            {me?.mode === "key" && (
              <span className="whoami-id" title={`API key ${me.key.name} (${me.key.scope})`}>
                <KeyIcon size={13} /> <span className="whoami-name">{me.key.name}</span>{" "}
                <code>{`${me.key.prefix}…`}</code>
              </span>
            )}
            {onSignOut && (
              <button type="button" className="btn-outline btn-sm" onClick={onSignOut}>
                <LogOutIcon size={13} /> Sign out
              </button>
            )}
          </div>
        )}
      </div>
    </aside>
  );
}
