import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
