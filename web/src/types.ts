// Types mirror docs/API_CONTRACT.md (v1 + v2 auth + v4 review). Keep in sync with the frozen contract.

export interface Health {
  ok: boolean;
  model_configured: boolean;
  model_id: string;
  /** v2: true when the caller is not local and must send an API key. v4: false for every caller with PUBLIC_DEMO=1. */
  auth_required?: boolean;
}

export type KeyScope = "analyze" | "admin";

/** Public view of an API key. The secret is never part of it. */
export interface ApiKey {
  id: string;
  name: string;
  scope: KeyScope;
  prefix: string;
  created_at: string;
  last_used_at: string | null;
  revoked: boolean;
}

export type Me =
  | { mode: "local" }
  | { mode: "key"; key: Pick<ApiKey, "id" | "name" | "scope" | "prefix"> }
  /** v4: the public demo identity (PUBLIC_DEMO=1): analyze scope, no key. */
  | { mode: "public" };

export interface CreatedKey {
  key: ApiKey;
  /** Shown once, never stored by the UI. */
  secret: string;
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
  | "source"
  | "commit";

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

// v4: the Muse change review (docs/API_CONTRACT.md, "Muse change review").
export type ReviewLens = "code" | "feature" | "security";
export type ReviewSeverity = "high" | "medium" | "low";
export type ReviewVerdict = "confirmed" | "disputed" | "refuted" | "unchecked";

export interface ReviewCheck {
  verdict: ReviewVerdict;
  reason: string;
  evidence_ids: string[];
}

export interface ReviewFinding {
  id: string;
  lens: ReviewLens;
  severity: ReviewSeverity;
  title: string;
  detail: string;
  suggestion: string;
  file: string;
  line_start: number;
  line_end: number;
  evidence_ids: string[];
  check: ReviewCheck;
}

export interface ReviewCounts {
  confirmed: number;
  disputed: number;
  refuted: number;
  unchecked: number;
}

export interface Review {
  status: "ok" | "disabled" | "error" | "invalid" | "skipped";
  model_id: string;
  message: string;
  check_status: "ok" | "error" | "invalid" | "skipped";
  check_message: string;
  summary: string;
  counts: ReviewCounts;
  findings: ReviewFinding[];
  rejected: string[];
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
  /** v4: null while the review chain runs; absent for an analysis stored before v4. */
  review?: Review | null;
  verifications?: Verification[] | null;
}

export interface VerifyRequest {
  command_id: string;
  approved: true;
}

export type Loadable<T> = { state: "loading" } | { state: "error"; error: string } | { state: "ok"; data: T };
