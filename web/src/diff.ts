import { classifyDiffLine, type DiffLineKind } from "./logic";
import type { ChangedFile, Evidence, Hypothesis } from "./types";

export interface DiffCounts {
  add: number;
  del: number;
}

/** Count added/removed lines in a unified-diff excerpt (file and hunk headers excluded). */
export function countDiffLines(excerpt: string): DiffCounts {
  let add = 0;
  let del = 0;
  for (const line of (excerpt ?? "").split("\n")) {
    const k = classifyDiffLine(line);
    if (k === "add") add++;
    else if (k === "del") del++;
  }
  return { add, del };
}

export interface DiffRow {
  kind: DiffLineKind;
  text: string;
  oldNo: number | null;
  newNo: number | null;
}

const HUNK_RE = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/;

/** Parse a diff excerpt into rows with old/new line numbers taken from the @@ header. */
export function parseHunk(excerpt: string): DiffRow[] {
  const lines = (excerpt ?? "").split("\n");
  if (lines.length > 1 && lines[lines.length - 1] === "") lines.pop();
  let oldNo: number | null = null;
  let newNo: number | null = null;
  const rows: DiffRow[] = [];
  for (const line of lines) {
    const kind = classifyDiffLine(line);
    if (kind === "hunk") {
      const m = HUNK_RE.exec(line);
      oldNo = m ? Number(m[1]) : null;
      newNo = m ? Number(m[2]) : null;
      rows.push({ kind, text: line, oldNo: null, newNo: null });
    } else if (kind === "meta" || line.startsWith("\\")) {
      rows.push({ kind: "meta", text: line, oldNo: null, newNo: null });
    } else if (kind === "add") {
      rows.push({ kind, text: line.slice(1), oldNo: null, newNo });
      if (newNo !== null) newNo++;
    } else if (kind === "del") {
      rows.push({ kind, text: line.slice(1), oldNo, newNo: null });
      if (oldNo !== null) oldNo++;
    } else {
      rows.push({ kind: "ctx", text: line.startsWith(" ") ? line.slice(1) : line, oldNo, newNo });
      if (oldNo !== null) oldNo++;
      if (newNo !== null) newNo++;
    }
  }
  return rows;
}

function diffHunks(evidence: Evidence[] | null | undefined): Evidence[] {
  return (evidence ?? []).filter((e) => e.type === "diff_hunk");
}

export interface ChangeRow extends DiffCounts {
  path: string;
  status: string;
  hunks: number;
}

// Go's \s in RE2 is ASCII whitespace only; JS's \s is wider, so it is spelt out.
const WS = "[\\t\\n\\f\\r ]";

/**
 * A copy of secretRes in backend/internal/evidence/evidence.go, in the same order.
 * Element [1] and the last one keep their first group ("${1}[REDACTED]" in Go).
 * The case-insensitive ones use the u flag, as Go folds case by Unicode simple
 * folding and counts runes, not UTF-16 units.
 */
const SECRET_RES: readonly [RegExp, string][] = [
  [/-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----/g, "[REDACTED]"],
  [new RegExp(`(bearer${WS}+)[A-Za-z0-9._\\-]{8,}`, "giu"), "$1[REDACTED]"],
  [/\bsk-[A-Za-z0-9_-]{16,}/g, "[REDACTED]"],
  [/\bAKIA[0-9A-Z]{16}\b/g, "[REDACTED]"],
  [/\bgh[pousr]_[A-Za-z0-9]{20,}/g, "[REDACTED]"],
  [new RegExp(`((?:api[_-]?key|secret|token|password|passwd)${WS}*[:=]${WS}*["']?)[^\\t\\n\\f\\r "']{4,}`, "giu"), "$1[REDACTED]"],
];

function redact(s: string): string {
  return SECRET_RES.reduce((acc, [re, by]) => acc.replace(re, by), s);
}

/**
 * The client copy of the server's RedactPath: each "/" segment is redacted on its
 * own, and Redact replaces the secret-looking part of a segment, which can be the
 * whole segment ("[REDACTED]") or only part of it ("token=[REDACTED]", "[REDACTED].ts").
 */
export function redactPath(p: string): string {
  return p.split("/").map(redact).join("/");
}

/**
 * Find the changed file an evidence path belongs to. The server redacts evidence
 * paths with RedactPath but keeps changed_files paths raw. An exact match wins;
 * otherwise the changed file whose redacted path equals the evidence path is used,
 * and only when exactly one changed file redacts to it.
 */
export function changedFileFor(evidencePath: string, changedPaths: readonly string[]): string | null {
  // Exact first: a changed file whose name literally holds the marker (for example
  // cfg/token=[REDACTED]/a.ts) wins over another changed file that redacts to the
  // same path (cfg/token=abcd/a.ts), and both hunks then count for the literal one.
  if (changedPaths.includes(evidencePath)) return evidencePath;
  if (!evidencePath.includes("[REDACTED]")) return null;
  const fits = changedPaths.filter((p) => redactPath(p) === evidencePath);
  return fits.length === 1 ? fits[0] : null;
}

/** Walkthrough "Changes" table: one row per changed file; counts come only from diff_hunk evidence. */
export function changesTable(files: ChangedFile[] | null | undefined, evidence: Evidence[] | null | undefined): ChangeRow[] {
  const hunks = diffHunks(evidence);
  const paths = (files ?? []).map((f) => f.path);
  const owner = hunks.map((h) => changedFileFor(h.file, paths));
  return (files ?? []).map((f) => {
    const mine = hunks.filter((_, i) => owner[i] === f.path);
    const c = mine.reduce(
      (acc, h) => {
        const n = countDiffLines(h.excerpt);
        return { add: acc.add + n.add, del: acc.del + n.del };
      },
      { add: 0, del: 0 },
    );
    return { path: f.path, status: f.status, add: c.add, del: c.del, hunks: mine.length };
  });
}

export interface FileDiff extends DiffCounts {
  file: string;
  hunks: Evidence[];
}

export function groupDiffsByFile(evidence: Evidence[] | null | undefined): FileDiff[] {
  const map = new Map<string, FileDiff>();
  for (const h of diffHunks(evidence)) {
    let g = map.get(h.file);
    if (!g) {
      g = { file: h.file, hunks: [], add: 0, del: 0 };
      map.set(h.file, g);
    }
    const n = countDiffLines(h.excerpt);
    g.hunks.push(h);
    g.add += n.add;
    g.del += n.del;
  }
  return [...map.values()];
}

export interface Anchors {
  /** evidence id -> row index -> hypotheses to render directly under that row */
  rows: Map<string, Map<number, Hypothesis[]>>;
  /** file path -> hypotheses to render under the file header (no usable line) */
  fileLevel: Map<string, Hypothesis[]>;
}

/**
 * Map hypotheses to positions in the diff view. A hypothesis whose line_start (HEAD side)
 * falls inside a hunk of its file is anchored under that row; one with line 0 or a line
 * outside every hunk of its file goes under the file header. Files without hunks are ignored.
 */
export function anchorHypotheses(hyps: Hypothesis[] | null | undefined, evidence: Evidence[] | null | undefined): Anchors {
  const rows = new Map<string, Map<number, Hypothesis[]>>();
  const fileLevel = new Map<string, Hypothesis[]>();
  const hunks = diffHunks(evidence);
  const parsed = hunks.map((h) => ({ ev: h, rows: parseHunk(h.excerpt) }));
  const sorted = [...(hyps ?? [])].sort((a, b) => a.rank - b.rank);

  for (const h of sorted) {
    const mine = parsed.filter((p) => p.ev.file === h.file);
    if (!h.file || mine.length === 0) continue;
    let placed = false;
    if (h.line_start > 0) {
      const start = h.line_start;
      const end = Math.max(h.line_end || start, start);
      for (const p of mine) {
        const nums = p.rows.map((r) => r.newNo).filter((n): n is number => n !== null);
        if (nums.length === 0) continue;
        const min = Math.min(...nums);
        const max = Math.max(...nums);
        if (end < min || start > max) continue;
        // Prefer the last changed row inside the range. A deleted row sits at the
        // HEAD-side position of the next row that has a new line number.
        const eff = (i: number): number | null => {
          for (let j = i; j < p.rows.length; j++) if (p.rows[j].newNo !== null) return p.rows[j].newNo;
          for (let j = i - 1; j >= 0; j--) if (p.rows[j].newNo !== null) return p.rows[j].newNo! + 1;
          return null;
        };
        let idx = -1;
        p.rows.forEach((r, i) => {
          if (r.kind !== "add" && r.kind !== "del") return;
          const n = eff(i);
          if (n !== null && n >= start && n <= end) idx = i;
        });
        if (idx < 0) {
          p.rows.forEach((r, i) => {
            if (r.newNo !== null && r.newNo <= start) idx = i;
          });
        }
        if (idx < 0) continue;
        let m = rows.get(p.ev.id);
        if (!m) {
          m = new Map();
          rows.set(p.ev.id, m);
        }
        m.set(idx, [...(m.get(idx) ?? []), h]);
        placed = true;
        break;
      }
    }
    if (!placed) fileLevel.set(h.file, [...(fileLevel.get(h.file) ?? []), h]);
  }
  return { rows, fileLevel };
}
