import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import App from "./App";
import { KEY_STORAGE, clearStoredKey, getStoredKey, setStoredKey } from "./api";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  clearStoredKey();
  try {
    window.sessionStorage.clear();
  } catch {
    /* ignore */
  }
});

function json(body: unknown, status = 200, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...headers } });
}

const GOOD = "rm_live_GOODGOODGOODGOODGOODGOODGOODGOOD";
const SECRET = "rm_live_NEWNEWNEWNEWNEWNEWNEWNEWNEWNEWNE";
const ciMe = { mode: "key", key: { id: "k_ci00000001", name: "ci", scope: "analyze", prefix: GOOD.slice(0, 12) } };

const common = (url: string, init?: RequestInit): Response | null => {
  if (url === "/api/repos") return json({ repos: [{ name: "pagination-demo" }] });
  if (url === "/api/repos/pagination-demo/commits")
    return json({ commits: [{ sha: "b".repeat(40), short: "bbbbbbb", subject: "baseline" }] });
  if (url === "/api/commands") return json({ commands: [{ id: "node-test", argv: ["node", "--test"], description: "d" }] });
  void init;
  return null;
};

/** A remote server: everything except /api/health needs the GOOD key. */
function remoteBackend() {
  const fn = vi.fn(async (url: string, init?: RequestInit) => {
    const auth = (init?.headers as Record<string, string> | undefined)?.Authorization;
    if (url === "/api/health") return json({ ok: true, model_configured: true, model_id: "m", auth_required: true });
    if (auth !== `Bearer ${GOOD}`)
      return json({ error: { code: "unauthorized", message: "missing or invalid API key" } }, 401, { "WWW-Authenticate": "Bearer" });
    if (url === "/api/me") return json(ciMe);
    return common(url, init) ?? json({ error: { code: "not_found", message: "nope" } }, 404);
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

describe("App auth", () => {
  it("shows a sign-in card when auth is required, rejects a bad key, then loads the app with a good one", async () => {
    const fn = remoteBackend();
    render(<App />);
    const input = (await screen.findByLabelText("API key")) as HTMLInputElement;
    expect(input.type).toBe("password");
    expect(screen.queryByTestId("analyze-btn")).not.toBeInTheDocument();

    fireEvent.change(input, { target: { value: "rm_live_bad" } });
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByText(/Invalid or revoked API key/)).toBeInTheDocument();
    expect(getStoredKey()).toBeNull();

    fireEvent.change(screen.getByLabelText("API key"), { target: { value: GOOD } });
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));

    const start = await screen.findByTestId("analyze-btn");
    await waitFor(() => expect(start).toBeEnabled());
    expect(window.sessionStorage.getItem(KEY_STORAGE)).toBe(GOOD);
    // Only the prefix and name are shown; the key never appears in the page or a URL.
    expect(screen.getByText(`${GOOD.slice(0, 12)}…`)).toBeInTheDocument();
    expect(document.body.innerHTML).not.toContain(GOOD);
    expect(fn.mock.calls.some(([url]) => String(url).includes("rm_live"))).toBe(false);
    // analyze-scope key: no API Keys nav item
    expect(within(screen.getByRole("navigation")).queryByRole("button", { name: "API Keys" })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /sign out/i }));
    expect(await screen.findByLabelText("API key")).toBeInTheDocument();
    expect(getStoredKey()).toBeNull();
    expect(window.sessionStorage.getItem(KEY_STORAGE)).toBeNull();
  });

  it("drops a revoked stored key on 401 and asks to sign in again", async () => {
    setStoredKey("rm_live_REVOKEDREVOKEDREVOKEDREVOKEDRE");
    remoteBackend();
    render(<App />);
    expect(await screen.findByLabelText("API key")).toBeInTheDocument();
    expect(getStoredKey()).toBeNull();
  });

  it("works with sessionStorage throwing", async () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("SecurityError");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("SecurityError");
    });
    remoteBackend();
    render(<App />);
    fireEvent.change(await screen.findByLabelText("API key"), { target: { value: GOOD } });
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    const start = await screen.findByTestId("analyze-btn");
    await waitFor(() => expect(start).toBeEnabled());
  });
});

function localBackend(analysisResponse?: () => Response) {
  const fn = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === "/api/health") return json({ ok: true, model_configured: true, model_id: "m", auth_required: false });
    if (url === "/api/me") return json({ mode: "local" });
    if (url === "/api/keys" && !init?.method) return json({ keys: [] });
    if (url === "/api/keys" && init?.method === "POST")
      return json(
        {
          key: { id: "k_new0000001", name: "ci", scope: "analyze", prefix: SECRET.slice(0, 12), created_at: "2026-11-04T10:00:00Z", last_used_at: null, revoked: false },
          secret: SECRET,
        },
        201,
      );
    if (url === "/api/analyses" && init?.method === "POST" && analysisResponse) return analysisResponse();
    return common(url, init) ?? json({ error: { code: "not_found", message: "nope" } }, 404);
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

/**
 * A hosted demo with PUBLIC_DEMO=1: a request without a key runs as the public demo identity,
 * and any key is refused. refuseAfterMs holds each refusal back, so that /api/me can go out
 * with the stored key before the first 401 for it arrives.
 */
function publicBackend(refuseAfterMs = 0) {
  const fn = vi.fn(async (url: string, init?: RequestInit) => {
    const auth = (init?.headers as Record<string, string> | undefined)?.Authorization;
    if (url === "/api/health") return json({ ok: true, model_configured: true, model_id: "m", auth_required: false });
    if (auth !== undefined) {
      if (refuseAfterMs > 0) await new Promise((r) => setTimeout(r, refuseAfterMs));
      return json({ error: { code: "unauthorized", message: "missing or invalid API key" } }, 401, { "WWW-Authenticate": "Bearer" });
    }
    if (url === "/api/me") return json({ mode: "public" });
    if (url.startsWith("/api/keys"))
      return json({ error: { code: "forbidden_scope", message: "the public demo cannot manage API keys; use an admin key" } }, 403);
    if (url === "/api/analyses" && init?.method === "POST")
      return json({ error: { code: "analysis_running", message: "analysis already running" } }, 409);
    return common(url, init) ?? json({ error: { code: "not_found", message: "nope" } }, 404);
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

describe("App public demo", () => {
  it("shows no sign-in card, no API Keys item, no key identity and no sign-out, and the analyze view is usable", async () => {
    const fn = publicBackend();
    render(<App />);
    const start = await screen.findByTestId("analyze-btn");
    await waitFor(() => expect(start).toBeEnabled());
    await waitFor(() => expect(fn.mock.calls.some(([url]) => url === "/api/me")).toBe(true));
    // Let the {"mode":"public"} answer render.
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });

    expect(screen.getByTestId("analyze-btn")).toBeEnabled();
    expect(screen.queryByTestId("signin")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("API key")).not.toBeInTheDocument();
    expect(within(screen.getByRole("navigation")).queryByRole("button", { name: "API Keys" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /sign out/i })).not.toBeInTheDocument();
    expect(document.querySelector(".whoami")).toBeNull();
    expect(screen.getByText(/backend ok/)).toBeInTheDocument();

    // Starting an analysis reaches the API without a key.
    fireEvent.click(screen.getByTestId("analyze-btn"));
    expect(await screen.findByRole("alert")).toHaveTextContent(/already running/);
    expect(fn.mock.calls.some(([url, init]) => url === "/api/analyses" && init?.method === "POST")).toBe(true);
    expect(fn.mock.calls.every(([, init]) => (init?.headers as Record<string, string> | undefined)?.Authorization === undefined)).toBe(true);
    expect(fn.mock.calls.some(([url]) => String(url).startsWith("/api/keys"))).toBe(false);
  });

  it.each([
    ["before /api/me goes out with it", 0],
    ["after /api/me went out with it", 50],
  ])("drops a stored key refused %s and carries on as the public demo without a sign-in card", async (_when, refuseAfterMs) => {
    const STALE = "rm_live_REVOKEDREVOKEDREVOKEDREVOKEDRE";
    const authOf = (init?: RequestInit) => (init?.headers as Record<string, string> | undefined)?.Authorization;
    setStoredKey(STALE);
    const fn = publicBackend(refuseAfterMs);
    render(<App />);

    const start = await screen.findByTestId("analyze-btn");
    await waitFor(() => expect(start).toBeEnabled());
    // Let every held-back refusal arrive and render.
    await act(async () => {
      await new Promise((r) => setTimeout(r, refuseAfterMs + 50));
    });

    // The stored key was sent and checked, and the 401 for it dropped it.
    expect(fn.mock.calls.some(([, init]) => authOf(init) === `Bearer ${STALE}`)).toBe(true);
    expect(getStoredKey()).toBeNull();
    expect(window.sessionStorage.getItem(KEY_STORAGE)).toBeNull();
    // The last load of each resource went out without a key.
    for (const path of ["/api/me", "/api/repos", "/api/commands"]) {
      const calls = fn.mock.calls.filter(([url]) => url === path);
      const last = calls[calls.length - 1];
      expect(last, path).toBeDefined();
      expect(authOf(last?.[1]), path).toBeUndefined();
    }
    expect(screen.getByTestId("analyze-btn")).toBeEnabled();
    expect(screen.queryByTestId("signin")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("API key")).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(within(screen.getByRole("navigation")).queryByRole("button", { name: "API Keys" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /sign out/i })).not.toBeInTheDocument();
    expect(document.querySelector(".whoami")).toBeNull();
  });
});

describe("App API Keys view", () => {
  it("is reachable in local mode; the secret is gone after navigating away and back", async () => {
    localBackend();
    render(<App />);
    fireEvent.click(await screen.findByRole("button", { name: "API Keys" }));
    expect(await screen.findByTestId("keys-page")).toBeInTheDocument();
    // Full-width view: no AI Analysis pane next to it.
    expect(screen.queryByText("AI Analysis")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /sign out/i })).not.toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Key name"), { target: { value: "ci" } });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));
    expect(await screen.findByTestId("new-secret")).toHaveTextContent(SECRET);

    fireEvent.click(screen.getByRole("button", { name: "Analyze" }));
    expect(screen.queryByTestId("keys-page")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "API Keys" }));
    await screen.findByTestId("keys-page");
    expect(screen.queryByTestId("new-secret")).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain(SECRET);
  });
});

describe("App analysis start errors", () => {
  async function startAndGetAlert() {
    render(<App />);
    const start = await screen.findByTestId("analyze-btn");
    await waitFor(() => expect(start).toBeEnabled());
    fireEvent.click(start);
    return screen.findByRole("alert");
  }

  it("shows 429 rate_limited with Retry-After", async () => {
    localBackend(() => json({ error: { code: "rate_limited", message: "rate limit exceeded" } }, 429, { "Retry-After": "42" }));
    expect(await startAndGetAlert()).toHaveTextContent("Rate limit reached for this API key. Retry after 42 s.");
  });

  it("shows 409 analysis_running clearly", async () => {
    localBackend(() => json({ error: { code: "analysis_running", message: "analysis already running" } }, 409));
    expect(await startAndGetAlert()).toHaveTextContent(/already running/);
  });
});
