import { useCallback, useEffect, useRef, useState } from "react";
import { api, clearStoredKey, errorMessage, getStoredKey, onUnauthorized, setStoredKey } from "./api";
import { formatElapsed, upsertRecent, type RecentEntry } from "./layout";
import { shouldPoll } from "./logic";
import { MiddlePanel } from "./components/MiddlePanel";
import { RightPanel } from "./components/RightPanel";
import { Sidebar, canManageKeys, type View } from "./components/Sidebar";
import { SignIn } from "./components/SignIn";
import { KeysPage } from "./components/KeysPage";
import type { Analysis, Command, Commit, Health, Loadable, Me, Repo, Verification } from "./types";

function useLoad<T>(fn: (() => Promise<T>) | null, deps: unknown[]): [Loadable<T> | null, () => void] {
  const [val, setVal] = useState<Loadable<T> | null>(null);
  const [tick, setTick] = useState(0);
  useEffect(() => {
    if (!fn) {
      setVal(null);
      return;
    }
    let cancelled = false;
    setVal({ state: "loading" });
    fn().then(
      (data) => !cancelled && setVal({ state: "ok", data }),
      (e) => !cancelled && setVal({ state: "error", error: errorMessage(e) }),
    );
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, tick]);
  return [val, () => setTick((t) => t + 1)];
}

const POLL_MS = 1000;
const RECENT_KEY = "regressionmap.recent";

// Per-tab convenience only: the backend is the source of truth for analyses.
function loadRecent(): RecentEntry[] {
  try {
    const raw = window.sessionStorage.getItem(RECENT_KEY);
    const parsed = raw ? JSON.parse(raw) : [];
    return Array.isArray(parsed) ? parsed : [];
  } catch {
    return [];
  }
}
function saveRecent(list: RecentEntry[]) {
  try {
    window.sessionStorage.setItem(RECENT_KEY, JSON.stringify(list));
  } catch {
    /* storage unavailable */
  }
}

export default function App() {
  // Bumped on sign-in and sign-out so every load runs again with the new key.
  const [authEpoch, setAuthEpoch] = useState(0);
  const [authNeeded, setAuthNeeded] = useState(false);
  const [hasKey, setHasKey] = useState(() => getStoredKey() !== null);
  const [me, setMe] = useState<Me | null>(null);

  // Any 401 for the key we currently hold (or for no key) means: sign in.
  // A late 401 for a request sent before the current key was stored is ignored.
  useEffect(
    () =>
      onUnauthorized((sent) => {
        if (sent !== getStoredKey()) return;
        clearStoredKey();
        setHasKey(false);
        setMe(null);
        setAuthNeeded(true);
      }),
    [],
  );

  const [health, reloadHealth] = useLoad<Health>(api.health, [authEpoch]);
  const [repos, reloadRepos] = useLoad<Repo[]>(() => api.repos().then((r) => r.repos ?? []), [authEpoch]);
  const [commands, reloadCommands] = useLoad<Command[]>(() => api.commands().then((r) => r.commands ?? []), [authEpoch]);

  // Who am I? Decides the sign-in card and the API Keys nav item.
  useEffect(() => {
    if (health?.state !== "ok") return;
    if (health.data.auth_required && !getStoredKey()) {
      setMe(null);
      setAuthNeeded(true);
      return;
    }
    let cancelled = false;
    api.me().then(
      (m) => {
        if (cancelled) return;
        setMe(m);
        setAuthNeeded(false);
      },
      () => {
        /* 401 is handled by onUnauthorized; an older backend without /api/me just has no identity */
      },
    );
    return () => {
      cancelled = true;
    };
  }, [health]);

  const [repo, setRepo] = useState("");
  const [baseline, setBaseline] = useState("");
  const [commandId, setCommandId] = useState("");
  const [commits] = useLoad<Commit[]>(repo ? () => api.commits(repo).then((r) => r.commits ?? []) : null, [repo, authEpoch]);

  useEffect(() => {
    if (repos?.state === "ok" && !repo && repos.data.length > 0) setRepo(repos.data[0].name);
  }, [repos, repo]);
  useEffect(() => {
    if (commands?.state === "ok" && !commandId && commands.data.length > 0) setCommandId(commands.data[0].id);
  }, [commands, commandId]);
  useEffect(() => {
    if (commits?.state === "ok") {
      // Default baseline: second-newest commit (newest is usually HEAD).
      const list = commits.data;
      if (!list.some((c) => c.sha === baseline)) setBaseline(list[1]?.sha ?? list[0]?.sha ?? "");
    } else {
      setBaseline("");
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [commits]);

  const [view, setView] = useState<View>("analyze");
  const [analysis, setAnalysis] = useState<Analysis | null>(null);
  const [starting, setStarting] = useState(false);
  const [startError, setStartError] = useState<string | null>(null);
  const [pollError, setPollError] = useState<string | null>(null);
  const [highlight, setHighlight] = useState<string | null>(null);
  const [highlightSeq, setHighlightSeq] = useState(0);
  const highlightTimer = useRef<number | undefined>(undefined);
  const [recent, setRecent] = useState<RecentEntry[]>(loadRecent);
  const startTimes = useRef(new Map<string, number>());
  const [elapsed, setElapsed] = useState<Record<string, string>>({});
  const repoSelect = useRef<HTMLSelectElement>(null);

  // Keep the session "recent" list and elapsed time in sync with the analysis.
  useEffect(() => {
    if (!analysis) return;
    setRecent((list) => {
      const next = upsertRecent(list, analysis, new Date().toISOString());
      saveRecent(next);
      return next;
    });
    if (analysis.status === "done" && !elapsed[analysis.id]) {
      const t = formatElapsed(startTimes.current.get(analysis.id) ?? null, Date.now());
      if (t) setElapsed((m) => ({ ...m, [analysis.id]: t }));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [analysis]);

  // Poll every second while running.
  useEffect(() => {
    if (!analysis || !shouldPoll(analysis)) return;
    const id = analysis.id;
    let cancelled = false;
    const t = window.setTimeout(async () => {
      try {
        const next = await api.getAnalysis(id);
        if (!cancelled) {
          setPollError(null);
          setAnalysis(next);
        }
      } catch (e) {
        if (!cancelled) {
          setPollError(errorMessage(e));
          setAnalysis((a) => (a ? { ...a } : a)); // schedule another attempt
        }
      }
    }, POLL_MS);
    return () => {
      cancelled = true;
      window.clearTimeout(t);
    };
  }, [analysis]);

  async function start() {
    setStartError(null);
    setPollError(null);
    setHighlight(null);
    setView("analyze");
    setStarting(true);
    const t0 = Date.now();
    try {
      const a = await api.startAnalysis({ repo, baseline, command_id: commandId });
      startTimes.current.set(a.id, t0);
      setAnalysis(a);
    } catch (e) {
      setStartError(errorMessage(e));
    } finally {
      setStarting(false);
    }
  }

  function clear() {
    setAnalysis(null);
    setStartError(null);
    setPollError(null);
    setHighlight(null);
  }

  function newAnalysis() {
    clear();
    setView("analyze");
    window.setTimeout(() => repoSelect.current?.focus(), 0);
  }

  async function openRecent(id: string) {
    setView("analyze");
    setStartError(null);
    setPollError(null);
    setHighlight(null);
    try {
      setAnalysis(await api.getAnalysis(id));
    } catch (e) {
      setStartError(`Could not load analysis ${id}: ${errorMessage(e)}`);
    }
  }

  function signedIn(m: Me, key: string) {
    setStoredKey(key);
    setHasKey(true);
    setMe(m);
    setAuthNeeded(false);
    setAuthEpoch((n) => n + 1);
  }

  function signOut() {
    clearStoredKey();
    setHasKey(false);
    setMe(null);
    clear();
    setView("analyze");
    setAuthEpoch((n) => n + 1);
  }

  const selectEvidence = useCallback((id: string) => {
    setHighlight(id);
    setHighlightSeq((n) => n + 1);
    window.clearTimeout(highlightTimer.current);
    highlightTimer.current = window.setTimeout(() => setHighlight(null), 2500);
  }, []);

  const onVerified = useCallback((v: Verification) => {
    setAnalysis((a) => {
      if (!a) return a;
      const list = a.verifications ?? [];
      if (list.some((x) => x.at === v.at && x.command_id === v.command_id)) return a;
      return { ...a, verifications: [...list, v] };
    });
  }, []);

  const busy = starting || analysis?.status === "running";
  const canStart = !busy && !!repo && !!baseline && !!commandId && repos?.state === "ok" && commands?.state === "ok";
  const commandList = commands?.state === "ok" ? commands.data : [];
  const selectedCmd = commandList.find((c) => c.id === commandId);

  const form = (
    <>
      <label className="sel" title="Repository">
        <span className="sel-label">repo</span>
        <select ref={repoSelect} value={repo} onChange={(e) => setRepo(e.target.value)} disabled={busy || repos?.state !== "ok" || repos.data.length === 0}>
          {repos?.state === "loading" && <option value="">Loading…</option>}
          {repos?.state === "error" && <option value="">unavailable</option>}
          {repos?.state === "ok" && repos.data.length === 0 && <option value="">no repositories</option>}
          {repos?.state === "ok" &&
            repos.data.map((r) => (
              <option key={r.name} value={r.name}>
                {r.name}
              </option>
            ))}
        </select>
      </label>
      <label className="sel" title="Baseline commit (known good)">
        <span className="sel-label">baseline</span>
        <select value={baseline} onChange={(e) => setBaseline(e.target.value)} disabled={busy || commits?.state !== "ok" || commits.data.length === 0}>
          {!commits && <option value="">select repo</option>}
          {commits?.state === "loading" && <option value="">Loading…</option>}
          {commits?.state === "error" && <option value="">unavailable</option>}
          {commits?.state === "ok" && commits.data.length === 0 && <option value="">no commits</option>}
          {commits?.state === "ok" &&
            commits.data.map((c) => (
              <option key={c.sha} value={c.sha}>
                {c.short} {c.subject}
              </option>
            ))}
        </select>
      </label>
      <label className="sel" title={selectedCmd ? `argv: ${JSON.stringify(selectedCmd.argv)}` : "Test command"}>
        <span className="sel-label">command</span>
        <select value={commandId} onChange={(e) => setCommandId(e.target.value)} disabled={busy || commands?.state !== "ok" || commandList.length === 0}>
          {commands?.state === "loading" && <option value="">Loading…</option>}
          {commands?.state === "error" && <option value="">unavailable</option>}
          {commands?.state === "ok" && commandList.length === 0 && <option value="">no commands</option>}
          {commandList.map((c) => (
            <option key={c.id} value={c.id}>
              {c.id}
            </option>
          ))}
        </select>
      </label>
      {selectedCmd && <code className="argv-hint">{JSON.stringify(selectedCmd.argv)}</code>}
    </>
  );

  const loadErrors = [repos, commands, commits].filter((l): l is { state: "error"; error: string } => l?.state === "error");
  const banner = (
    <>
      {loadErrors.length > 0 && (
        <div className="banner banner-error" role="alert">
          {loadErrors[0].error}{" "}
          <button
            type="button"
            className="btn-outline btn-sm"
            onClick={() => {
              reloadHealth();
              reloadRepos();
              reloadCommands();
            }}
          >
            Retry
          </button>
        </div>
      )}
      {startError && (
        <div className="banner banner-error" role="alert">
          {startError}
        </div>
      )}
      {pollError && <div className="banner banner-warn">Polling failed, retrying: {pollError}</div>}
    </>
  );

  return (
    <div className="shell">
      <Sidebar
        view={view}
        onView={setView}
        onNew={newAnalysis}
        recent={recent}
        onOpenRecent={openRecent}
        health={health}
        activeId={analysis?.id ?? null}
        me={me}
        onSignOut={hasKey ? signOut : undefined}
      />

      {authNeeded ? (
        <section className="middle wide" data-testid="signin">
          <div className="topbar">
            <h2 className="topbar-title">Sign in</h2>
          </div>
          <SignIn onSignedIn={signedIn} />
        </section>
      ) : view === "keys" && canManageKeys(me) ? (
        <KeysPage me={me} />
      ) : view === "history" ? (
        <section className="middle">
          <div className="topbar">
            <h2 className="topbar-title">History</h2>
          </div>
          <div className="content scroll" data-testid="history">
            {recent.length === 0 ? (
              <div className="empty-state">No analyses started in this browser session yet.</div>
            ) : (
              <table className="changes history-table">
                <thead>
                  <tr>
                    <th>Analysis</th>
                    <th>Repository</th>
                    <th>HEAD tests</th>
                    <th>Started</th>
                  </tr>
                </thead>
                <tbody>
                  {recent.map((r) => (
                    <tr key={r.id} className="clickable" onClick={() => openRecent(r.id)}>
                      <td>
                        <code>{r.id}</code>
                      </td>
                      <td>{r.repo}</td>
                      <td>
                        <span className={`dot dot-${r.dot}`} aria-hidden="true" /> {r.dot}
                      </td>
                      <td className="muted small">{new Date(r.startedAt).toLocaleString()}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </section>
      ) : (
        <MiddlePanel
          analysis={analysis}
          highlight={highlight}
          highlightSeq={highlightSeq}
          form={form}
          onAnalyze={start}
          onClear={clear}
          canAnalyze={canStart}
          busy={busy}
          error={banner}
          onSelectEvidence={selectEvidence}
        />
      )}

      {!authNeeded && !(view === "keys" && canManageKeys(me)) && (
        <RightPanel
          analysis={analysis}
          elapsed={analysis ? (elapsed[analysis.id] ?? null) : null}
          commands={commandList}
          onSelectEvidence={selectEvidence}
          onVerified={onVerified}
        />
      )}
    </div>
  );
}
