import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, KEY_STORAGE, api, clearStoredKey, errorMessage, getStoredKey, onUnauthorized, setStoredKey } from "./api";

function json(body: unknown, status = 200, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...headers } });
}

function mockFetch(impl: (url: string, init?: RequestInit) => Promise<Response>) {
  const fn = vi.fn(impl);
  vi.stubGlobal("fetch", fn);
  return fn;
}

function authHeader(init?: RequestInit): string | undefined {
  return (init?.headers as Record<string, string> | undefined)?.Authorization;
}

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  clearStoredKey();
});

describe("api key storage and auth header", () => {
  it("sends no Authorization header when no key is stored", async () => {
    const fn = mockFetch(async () => json({ ok: true }));
    await api.health();
    expect(authHeader(fn.mock.calls[0][1])).toBeUndefined();
  });

  it("sends Authorization: Bearer <key> on every request once a key is stored", async () => {
    setStoredKey("rm_live_abcdefghijklmnopqrstuvwxyz012345");
    expect(window.sessionStorage.getItem(KEY_STORAGE)).toBe("rm_live_abcdefghijklmnopqrstuvwxyz012345");
    const fn = mockFetch(async () => json({ repos: [], commands: [] }));
    await api.repos();
    await api.commands();
    await api.startAnalysis({ repo: "r", baseline: "b", command_id: "c" });
    for (const [url, init] of fn.mock.calls) {
      expect(authHeader(init)).toBe("Bearer rm_live_abcdefghijklmnopqrstuvwxyz012345");
      expect(url).not.toMatch(/rm_live/);
    }
    // POST still carries the JSON content type
    expect((fn.mock.calls[2][1]?.headers as Record<string, string>)["Content-Type"]).toBe("application/json");
  });

  it("clearStoredKey removes the key and the header", async () => {
    setStoredKey("rm_live_x");
    clearStoredKey();
    expect(getStoredKey()).toBeNull();
    expect(window.sessionStorage.getItem(KEY_STORAGE)).toBeNull();
    const fn = mockFetch(async () => json({ ok: true }));
    await api.health();
    expect(authHeader(fn.mock.calls[0][1])).toBeUndefined();
  });

  it("keeps working (in memory) when sessionStorage throws", async () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("SecurityError");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("QuotaExceeded");
    });
    vi.spyOn(Storage.prototype, "removeItem").mockImplementation(() => {
      throw new Error("SecurityError");
    });
    expect(getStoredKey()).toBeNull();
    expect(() => setStoredKey("rm_live_mem")).not.toThrow();
    expect(getStoredKey()).toBe("rm_live_mem");
    const fn = mockFetch(async () => json({ ok: true }));
    await api.health();
    expect(authHeader(fn.mock.calls[0][1])).toBe("Bearer rm_live_mem");
    expect(() => clearStoredKey()).not.toThrow();
    expect(getStoredKey()).toBeNull();
  });

  it("me(key) validates a candidate key without storing it or notifying 401 listeners", async () => {
    const listener = vi.fn();
    const off = onUnauthorized(listener);
    const fn = mockFetch(async () => json({ error: { code: "unauthorized", message: "invalid key" } }, 401));
    const err = await api.me("rm_live_candidate").catch((e) => e);
    off();
    expect(err).toBeInstanceOf(ApiError);
    expect(err.status).toBe(401);
    expect(authHeader(fn.mock.calls[0][1])).toBe("Bearer rm_live_candidate");
    expect(fn.mock.calls[0][0]).toBe("/api/me");
    expect(listener).not.toHaveBeenCalled();
    expect(getStoredKey()).toBeNull();
  });
});

describe("401 handling", () => {
  it("notifies unauthorized listeners on 401 from any call", async () => {
    const listener = vi.fn();
    const off = onUnauthorized(listener);
    mockFetch(async () => json({ error: { code: "unauthorized", message: "missing key" } }, 401, { "WWW-Authenticate": "Bearer" }));
    const err = await api.repos().catch((e) => e);
    expect(err).toMatchObject({ code: "unauthorized", status: 401 });
    expect(listener).toHaveBeenCalledTimes(1);
    expect(listener).toHaveBeenCalledWith(null);
    setStoredKey("rm_live_sent");
    await api.repos().catch(() => {});
    expect(listener).toHaveBeenLastCalledWith("rm_live_sent");
    expect(listener).toHaveBeenCalledTimes(2);
    off();
    await api.repos().catch(() => {});
    expect(listener).toHaveBeenCalledTimes(2);
  });
});

describe("keys endpoints", () => {
  it("lists, creates and revokes keys (DELETE 204 has no body)", async () => {
    const key = { id: "k_abcdefghij", name: "ci", scope: "analyze", prefix: "rm_live_ab12", created_at: "2026-11-04T10:00:00Z", last_used_at: null, revoked: false };
    const fn = mockFetch(async (url, init) => {
      if (url === "/api/keys" && !init?.method) return json({ keys: [key] });
      if (url === "/api/keys" && init?.method === "POST") return json({ key, secret: "rm_live_s" }, 201);
      if (url === "/api/keys/k_abcdefghij" && init?.method === "DELETE") return new Response(null, { status: 204 });
      return json({ error: { code: "not_found", message: "nope" } }, 404);
    });
    expect((await api.keys()).keys).toEqual([key]);
    expect(await api.createKey({ name: "ci", scope: "analyze" })).toEqual({ key, secret: "rm_live_s" });
    expect(JSON.parse(String(fn.mock.calls[1][1]?.body))).toEqual({ name: "ci", scope: "analyze" });
    await expect(api.revokeKey("k_abcdefghij")).resolves.toBeUndefined();
  });
});

describe("error messages", () => {
  it("reads Retry-After into the error and mentions it for rate_limited", async () => {
    mockFetch(async () => json({ error: { code: "rate_limited", message: "rate limit exceeded" } }, 429, { "Retry-After": "120" }));
    const err = await api.startAnalysis({ repo: "r", baseline: "b", command_id: "c" }).catch((e) => e);
    expect(err).toMatchObject({ code: "rate_limited", status: 429, retryAfter: 120 });
    const msg = errorMessage(err);
    expect(msg).toMatch(/rate limit/i);
    expect(msg).toMatch(/120 s/);
  });

  it("explains analysis_running, repo_busy, too_many_analyses, forbidden_scope and cannot_revoke_self", () => {
    expect(errorMessage(new ApiError("analysis_running", "busy", 409))).toMatch(/already running/i);
    expect(errorMessage(new ApiError("repo_busy", "busy", 409))).toMatch(/being analysed or verified/i);
    expect(errorMessage(new ApiError("too_many_analyses", "cap", 429, 5))).toMatch(/5 s/);
    expect(errorMessage(new ApiError("forbidden_scope", "no", 403))).toMatch(/admin scope/i);
    expect(errorMessage(new ApiError("cannot_revoke_self", "no", 409))).toMatch(/signed in with/i);
    expect(errorMessage(new ApiError("unauthorized", "no", 401))).toMatch(/Invalid or revoked API key/);
  });
});
