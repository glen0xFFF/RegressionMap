// Types mirror docs/API_CONTRACT.md (v1). Keep in sync with the frozen contract.

export interface Health {
  ok: boolean;
  model_configured: boolean;
  model_id: string;
}

export interface Repo {
  name: string;
}

export interface Commit {
  sha: string;
  short: string;
  subject: string;
}

export interface Command {
  id: string;
  argv: string[];
  description: string;
}

export interface TestRun {
  exit_code: number;
  passed: boolean;
  timed_out: boolean;
  truncated: boolean;
  duration_ms: number;
  stdout: string;
  stderr: string;
}

export type EvidenceType =
  | "test_failure"
  | "assertion"
  | "stack_frame"
  | "diff_hunk"
  | "source";

export interface Evidence {
  id: string;
  type: EvidenceType;
  title: string;
  file: string;
  line_start: number;
  line_end: number;
  excerpt: string;
}

export type ConfidenceLabel = "low" | "medium" | "high";

export interface Hypothesis {
  rank: number;
  title: string;
  file: string;
  line_start: number;
  line_end: number;
  reason: string;
  supporting_evidence_ids: string[];
  counter_evidence_ids: string[];
  verification_suggestion: string;
  confidence_label: ConfidenceLabel;
}

export interface ModelResult {
  status: "ok" | "disabled" | "error" | "invalid";
  model_id: string;
  message?: string;
  rejected?: string[];
  result?: {
    summary: string;
    hypotheses: Hypothesis[];
    limitations: string[];
  };
}

export interface ChangedFile {
  path: string;
  status: string;
}

export interface Verification {
  command_id: string;
  argv: string[];
  at: string;
  run: TestRun;
}

export interface Analysis {
  id: string;
  status: "running" | "done" | "error";
  error?: string;
  repo: string;
  baseline: string;
  current: string;
  command: { id: string; argv: string[] };
  baseline_run?: TestRun | null;
  current_run?: TestRun | null;
  changed_files?: ChangedFile[] | null;
  evidence?: Evidence[] | null;
  model?: ModelResult | null;
  verifications?: Verification[] | null;
}

export interface VerifyRequest {
  command_id: string;
  approved: true;
}

export type Loadable<T> = { state: "loading" } | { state: "error"; error: string } | { state: "ok"; data: T };
