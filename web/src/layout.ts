import { runBadge } from "./logic";
import type { Analysis, ConfidenceLabel, Evidence, Hypothesis } from "./types";

export type RecentDot = "pass" | "fail" | "running" | "error" | "unknown";

export interface RecentEntry {
  id: string;
  repo: string;
  dot: RecentDot;
  startedAt: string;
}

/** Status dot for the sidebar. Derived from the current run's exit code only. */
export function recentDot(a: Analysis): RecentDot {
  if (a.status === "running") return "running";
  if (a.status === "error") return "error";
  const b = runBadge(a.current_run);
  if (b === "PASS") return "pass";
  if (b === "FAIL") return "fail";
  return "unknown";
}

const MAX_RECENT = 20;

/** Insert or update an analysis in the client-side recent list (newest first). */
export function upsertRecent(list: RecentEntry[], a: Analysis, startedAt: string): RecentEntry[] {
  const idx = list.findIndex((e) => e.id === a.id);
  if (idx >= 0) {
    const next = list.slice();
    next[idx] = { ...next[idx], repo: a.repo, dot: recentDot(a) };
    return next;
  }
  return [{ id: a.id, repo: a.repo, dot: recentDot(a), startedAt }, ...list].slice(0, MAX_RECENT);
}

/** Human-readable elapsed time between two epoch-ms timestamps; null when unknown. */
export function formatElapsed(startMs: number | null, endMs: number | null): string | null {
  if (startMs == null || endMs == null || endMs < startMs) return null;
  const ms = endMs - startMs;
  if (ms < 1000) return "<1s";
  const total = Math.round(ms / 1000);
  if (total < 60) return `${total}s`;
  return `${Math.floor(total / 60)}m ${total % 60}s`;
}

export function splitHypotheses(hyps: Hypothesis[] | null | undefined): { root: Hypothesis | null; others: Hypothesis[] } {
  const sorted = [...(hyps ?? [])].sort((a, b) => a.rank - b.rank);
  return { root: sorted[0] ?? null, others: sorted.slice(1) };
}

export function diffEvidence(ev: Evidence[] | null | undefined): Evidence[] {
  return (ev ?? []).filter((e) => e.type === "diff_hunk");
}

export function confidenceClass(label: ConfidenceLabel): string {
  const known = label === "high" || label === "medium" || label === "low" ? label : "low";
  return `pill pill-${known}`;
}
