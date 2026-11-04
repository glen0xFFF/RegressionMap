import type {
  Analysis,
  ApiKey,
  Command,
  Commit,
  CreatedKey,
  Health,
  KeyScope,
  Me,
  Repo,
  Verification,
  VerifyRequest,
} from "./types";

export class ApiError extends Error {
  code: string;
  status: number;
  /** Seconds from a Retry-After header, when the server sent one. */
  retryAfter?: number;
  constructor(code: string, message: string, status: number, retryAfter?: number) {
    super(message);
    this.name = "ApiError";
    this.code = code;
    this.status = status;
    if (retryAfter !== undefined) this.retryAfter = retryAfter;
  }
}

// ---------- API key storage ----------
// The key lives in sessionStorage (per tab) and, as a fallback when storage
// throws (private mode, blocked site data), in module memory. It is only ever
// sent in the Authorization header: never logged, never put in a URL.

export const KEY_STORAGE = "regressionmap.key";
let memoryKey: string | null = null;

export function getStoredKey(): string | null {
  try {
    const v = window.sessionStorage.getItem(KEY_STORAGE);
    if (v) return v;
  } catch {
    /* storage unavailable: fall back to memory */
  }
  return memoryKey;
}

export function setStoredKey(key: string): void {
  memoryKey = key;
  try {
    window.sessionStorage.setItem(KEY_STORAGE, key);
  } catch {
    /* storage unavailable: memory only */
  }
}

export function clearStoredKey(): void {
  memoryKey = null;
  try {
    window.sessionStorage.removeItem(KEY_STORAGE);
  } catch {
    /* storage unavailable */
  }
}

// ---------- 401 notification ----------

/** Receives the key the failed request sent (null when it sent none). */
type Listener = (sentKey: string | null) => void;
const unauthorizedListeners = new Set<Listener>();

/** Subscribe to 401 responses from any call. Returns an unsubscribe function. */
export function onUnauthorized(fn: Listener): () => void {
  unauthorizedListeners.add(fn);
  return () => {
    unauthorizedListeners.delete(fn);
  };
}

function parseRetryAfter(v: string | null): number | undefined {
  if (!v) return undefined;
  const t = v.trim();
  if (/^\d+$/.test(t)) return Number(t);
  const at = Date.parse(t);
  if (Number.isNaN(at)) return undefined;
  return Math.max(0, Math.ceil((at - Date.now()) / 1000));
}

interface RequestOpts {
  /** Use this key instead of the stored one, and do not notify 401 listeners. */
  key?: string;
  /** The response has no body (204). */
  empty?: boolean;
}

async function request<T>(path: string, init?: RequestInit, opts: RequestOpts = {}): Promise<T> {
  const key = opts.key ?? getStoredKey() ?? null;
  const headers: Record<string, string> = { Accept: "application/json" };
  if (init?.body) headers["Content-Type"] = "application/json";
  if (key) headers.Authorization = `Bearer ${key}`;
  let res: Response;
  try {
    res = await fetch(path, { ...init, headers });
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
    if (res.status === 401 && opts.key === undefined) unauthorizedListeners.forEach((fn) => fn(key));
    const retryAfter = parseRetryAfter(res.headers.get("Retry-After"));
    const err = (body as { error?: { code?: string; message?: string } } | undefined)?.error;
    if (err && typeof err.message === "string") {
      throw new ApiError(err.code ?? "error", err.message, res.status, retryAfter);
    }
    const msg =
      res.status >= 500
        ? `Backend error (HTTP ${res.status}). The backend may be down or unreachable.`
        : `Request failed (HTTP ${res.status}).`;
    throw new ApiError(res.status === 401 ? "unauthorized" : "http", msg, res.status, retryAfter);
  }
  if (opts.empty) return undefined as T;
  if (body === undefined) {
    throw new ApiError("bad_response", "Backend returned a non-JSON response.", res.status);
  }
  return body as T;
}

const enc = encodeURIComponent;

export const api = {
  health: () => request<Health>("/api/health"),
  /** With `key`, validates that candidate key instead of the stored one. */
  me: (key?: string) => request<Me>("/api/me", undefined, key === undefined ? {} : { key }),
  repos: () => request<{ repos: Repo[] }>("/api/repos"),
  commits: (repo: string) => request<{ commits: Commit[] }>(`/api/repos/${enc(repo)}/commits`),
  commands: () => request<{ commands: Command[] }>("/api/commands"),
  startAnalysis: (body: { repo: string; baseline: string; command_id: string }) =>
    request<Analysis>("/api/analyses", { method: "POST", body: JSON.stringify(body) }),
  getAnalysis: (id: string) => request<Analysis>(`/api/analyses/${enc(id)}`),
  verify: (id: string, body: VerifyRequest) =>
    request<Verification>(`/api/analyses/${enc(id)}/verify`, { method: "POST", body: JSON.stringify(body) }),
  keys: () => request<{ keys: ApiKey[] }>("/api/keys"),
  createKey: (body: { name: string; scope: KeyScope }) =>
    request<CreatedKey>("/api/keys", { method: "POST", body: JSON.stringify(body) }),
  revokeKey: (id: string) => request<void>(`/api/keys/${enc(id)}`, { method: "DELETE" }, { empty: true }),
};

function retryHint(e: ApiError): string {
  return e.retryAfter !== undefined ? ` Retry after ${e.retryAfter} s.` : "";
}

export function errorMessage(e: unknown): string {
  if (e instanceof ApiError) {
    switch (e.code) {
      case "unauthorized":
        return "Invalid or revoked API key.";
      case "rate_limited":
        // Local requests are never rate limited, so without a key this is
        // the one bucket that the public demo identity shares.
        return getStoredKey() === null
          ? `The public demo has reached its shared analysis rate limit.${retryHint(e)}`
          : `Rate limit reached for this API key.${retryHint(e)}`;
      case "too_many_analyses":
        return `Too many analyses are running on the server right now.${retryHint(e)}`;
      case "analysis_running":
        return "An analysis of this repository is already running. Wait for it to finish, then try again.";
      case "repo_busy":
        return "The repository is being analysed or verified right now. Wait for it to finish, then try again.";
      case "forbidden_scope":
        return "This API key does not have the admin scope needed to manage API keys.";
      case "cannot_revoke_self":
        return "You cannot revoke the key you are signed in with.";
    }
    return e.message;
  }
  if (e instanceof Error) return e.message;
  return String(e);
}
