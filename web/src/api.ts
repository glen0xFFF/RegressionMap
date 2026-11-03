import type { Analysis, Command, Commit, Health, Repo, Verification, VerifyRequest } from "./types";

export class ApiError extends Error {
  code: string;
  status: number;
  constructor(code: string, message: string, status: number) {
    super(message);
    this.name = "ApiError";
    this.code = code;
    this.status = status;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let res: Response;
  try {
    res = await fetch(path, {
      ...init,
      headers: { Accept: "application/json", ...(init?.body ? { "Content-Type": "application/json" } : {}) },
    });
  } catch {
    throw new ApiError(
      "network",
      "Cannot reach the RegressionMap backend at 127.0.0.1:8787. Is it running?",
      0,
    );
  }
  const text = await res.text();
  let body: unknown = undefined;
  try {
    body = text ? JSON.parse(text) : undefined;
  } catch {
    body = undefined;
  }
  if (!res.ok) {
    const err = (body as { error?: { code?: string; message?: string } } | undefined)?.error;
    if (err && typeof err.message === "string") {
      throw new ApiError(err.code ?? "error", err.message, res.status);
    }
    const msg =
      res.status >= 500
        ? `Backend error (HTTP ${res.status}). The backend may be down or unreachable.`
        : `Request failed (HTTP ${res.status}).`;
    throw new ApiError("http", msg, res.status);
  }
  if (body === undefined) {
    throw new ApiError("bad_response", "Backend returned a non-JSON response.", res.status);
  }
  return body as T;
}

const enc = encodeURIComponent;

export const api = {
  health: () => request<Health>("/api/health"),
  repos: () => request<{ repos: Repo[] }>("/api/repos"),
  commits: (repo: string) => request<{ commits: Commit[] }>(`/api/repos/${enc(repo)}/commits`),
  commands: () => request<{ commands: Command[] }>("/api/commands"),
  startAnalysis: (body: { repo: string; baseline: string; command_id: string }) =>
    request<Analysis>("/api/analyses", { method: "POST", body: JSON.stringify(body) }),
  getAnalysis: (id: string) => request<Analysis>(`/api/analyses/${enc(id)}`),
  verify: (id: string, body: VerifyRequest) =>
    request<Verification>(`/api/analyses/${enc(id)}/verify`, { method: "POST", body: JSON.stringify(body) }),
};

export function errorMessage(e: unknown): string {
  if (e instanceof Error) return e.message;
  return String(e);
}
