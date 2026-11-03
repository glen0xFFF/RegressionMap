import type { Analysis, Evidence, TestRun, VerifyRequest } from "./types";

export type Badge = "PASS" | "FAIL" | "UNKNOWN";

/**
 * Pass/fail badge for a test run. Derived ONLY from the process exit status:
 * PASS iff exit_code === 0 and the run did not time out. The `passed` field and
 * any model output are deliberately ignored.
 */
export function runBadge(run: TestRun | null | undefined): Badge {
  if (!run || typeof run.exit_code !== "number" || Number.isNaN(run.exit_code)) {
    return "UNKNOWN";
  }
  return run.exit_code === 0 && run.timed_out !== true ? "PASS" : "FAIL";
}

export interface ResolvedEvidence {
  id: string;
  known: boolean;
  evidence: Evidence | undefined;
}

/** Resolve evidence ids cited by a hypothesis; unknown ids are flagged (known=false). */
export function resolveEvidenceIds(
  ids: string[] | null | undefined,
  evidence: Evidence[] | null | undefined,
): ResolvedEvidence[] {
  if (!ids) return [];
  const byId = new Map((evidence ?? []).map((e) => [e.id, e]));
  return ids.map((id) => {
    const ev = byId.get(id);
    return { id, known: ev !== undefined, evidence: ev };
  });
}

/**
 * Build the verify request body. Throws unless the user explicitly approved
 * (approved must be the boolean `true`).
 */
export function buildVerifyRequest(commandId: string, approved: boolean): VerifyRequest {
  if (approved !== true) {
    throw new Error("Verification requires explicit approval");
  }
  if (!commandId) {
    throw new Error("A command must be selected");
  }
  return { command_id: commandId, approved: true };
}

export type DiffLineKind = "add" | "del" | "hunk" | "meta" | "ctx";

export function classifyDiffLine(line: string): DiffLineKind {
  if (line.startsWith("+++") || line.startsWith("---")) return "meta";
  if (line.startsWith("@@")) return "hunk";
  if (line.startsWith("+")) return "add";
  if (line.startsWith("-")) return "del";
  if (line.startsWith("diff ") || line.startsWith("index ")) return "meta";
  return "ctx";
}

/** Format a line range; 0 means unknown and is never shown as a line number. */
export function formatLines(start: number, end: number): string {
  if (!start) return "";
  if (!end || end === start) return `L${start}`;
  return `L${start}-L${end}`;
}

export function shouldPoll(a: Pick<Analysis, "status"> | null | undefined): boolean {
  return !!a && a.status === "running";
}

export function evidenceDomId(id: string): string {
  return `evidence-${id}`;
}
