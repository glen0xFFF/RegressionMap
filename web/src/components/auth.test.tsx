import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { Sidebar, canManageKeys } from "./Sidebar";
import { SignIn } from "./SignIn";
import { KeysPage } from "./KeysPage";
import { VerifyPanel } from "./VerifyPanel";
import { clearStoredKey } from "../api";
import type { ApiKey, Me } from "../types";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  clearStoredKey();
});

function json(body: unknown, status = 200, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...headers } });
}

const SECRET = "rm_live_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345";

const sidebarProps = {
  view: "analyze" as const,
  onView: () => {},
  onNew: () => {},
  onOpenRecent: () => {},
  recent: [],
  health: { state: "ok" as const, data: { ok: true, model_configured: true, model_id: "m" } },
};

const adminMe: Me = { mode: "key", key: { id: "k_admin00001", name: "ops", scope: "admin", prefix: "rm_live_ad12" } };
const analyzeMe: Me = { mode: "key", key: { id: "k_ci00000001", name: "ci", scope: "analyze", prefix: "rm_live_ci12" } };

function navLabels() {
  return within(screen.getByRole("navigation"))
    .getAllByRole("button")
    .map((b) => b.textContent?.trim());
}

describe("Sidebar API Keys nav visibility", () => {
  it("is hidden without /api/me and for an analyze-scope key", () => {
    const { rerender } = render(<Sidebar {...sidebarProps} />);
    expect(navLabels()).toEqual(["Analyze", "History"]);
    rerender(<Sidebar {...sidebarProps} me={analyzeMe} />);
    expect(navLabels()).toEqual(["Analyze", "History"]);
  });

  it("is shown for local mode and for an admin key, and switches the view", () => {
    const onView = vi.fn();
    const { rerender } = render(<Sidebar {...sidebarProps} me={{ mode: "local" }} onView={onView} />);
    expect(navLabels()).toEqual(["Analyze", "History", "API Keys"]);
    rerender(<Sidebar {...sidebarProps} me={adminMe} onView={onView} />);
    const item = screen.getByRole("button", { name: "API Keys" });
    expect(item.querySelector("svg")).not.toBeNull();
    fireEvent.click(item);
    expect(onView).toHaveBeenCalledWith("keys");
  });

  it("offers the public demo identity no key management, no key identity and no sign-out", () => {
    const publicMe: Me = { mode: "public" };
    expect(canManageKeys(publicMe)).toBe(false);
    // Even with a sign-out handler passed, the footer offers none for the public identity.
    const onSignOut = vi.fn();
    render(<Sidebar {...sidebarProps} me={publicMe} onSignOut={onSignOut} />);
    expect(navLabels()).toEqual(["Analyze", "History"]);
    expect(screen.queryByRole("button", { name: /sign out/i })).not.toBeInTheDocument();
    expect(document.querySelector(".whoami")).toBeNull();
    expect(screen.getByText(/backend ok/)).toBeInTheDocument();
  });

  it("shows the signed-in key name and prefix and a Sign out action in the footer", () => {
    const onSignOut = vi.fn();
    render(<Sidebar {...sidebarProps} me={analyzeMe} onSignOut={onSignOut} />);
    expect(screen.getByText("ci")).toBeInTheDocument();
    expect(screen.getByText("rm_live_ci12…")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /sign out/i }));
    expect(onSignOut).toHaveBeenCalled();
  });
});

describe("SignIn", () => {
  it("uses a password input, shows 401 as invalid key, then signs in with a valid key", async () => {
    const fetchMock = vi.fn(async (_url: string, init?: RequestInit) => {
      const auth = (init?.headers as Record<string, string>).Authorization;
      return auth === `Bearer ${SECRET}` ? json(analyzeMe) : json({ error: { code: "unauthorized", message: "bad" } }, 401);
    });
    vi.stubGlobal("fetch", fetchMock);
    const onSignedIn = vi.fn();
    render(<SignIn onSignedIn={onSignedIn} />);
    const input = screen.getByLabelText("API key") as HTMLInputElement;
    expect(input.type).toBe("password");
    const btn = screen.getByRole("button", { name: "Sign in" });
    expect(btn).toBeDisabled();

    fireEvent.change(input, { target: { value: "rm_live_wrong" } });
    fireEvent.click(btn);
    expect(await screen.findByRole("alert")).toHaveTextContent("Invalid or revoked API key");
    expect(onSignedIn).not.toHaveBeenCalled();

    fireEvent.change(input, { target: { value: `  ${SECRET} ` } });
    fireEvent.click(btn);
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledWith(analyzeMe, SECRET));
    expect(fetchMock.mock.calls.every(([url]) => url === "/api/me")).toBe(true);
  });
});

const keyCI: ApiKey = {
  id: "k_ci00000001",
  name: "ci",
  scope: "analyze",
  prefix: "rm_live_ci12",
  created_at: "2026-11-01T10:00:00Z",
  last_used_at: null,
  revoked: false,
};
const keyOld: ApiKey = {
  id: "k_old0000001",
  name: "old-laptop",
  scope: "admin",
  prefix: "rm_live_ol12",
  created_at: "2026-10-01T10:00:00Z",
  last_used_at: "2026-10-02T10:00:00Z",
  revoked: true,
};

function keysBackend(opts: { revokeStatus?: number; listStatus?: number } = {}) {
  const created: ApiKey = {
    id: "k_new0000001",
    name: "deploy",
    scope: "admin",
    prefix: SECRET.slice(0, 12),
    created_at: "2026-11-04T10:00:00Z",
    last_used_at: null,
    revoked: false,
  };
  const fn = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === "/api/keys" && !init?.method) {
      if (opts.listStatus === 403) return json({ error: { code: "forbidden_scope", message: "admin scope required" } }, 403);
      return json({ keys: [keyCI, keyOld] });
    }
    if (url === "/api/keys" && init?.method === "POST") return json({ key: created, secret: SECRET }, 201);
    if (url.startsWith("/api/keys/") && init?.method === "DELETE") {
      if (opts.revokeStatus === 409) return json({ error: { code: "cannot_revoke_self", message: "cannot revoke self" } }, 409);
      return new Response(null, { status: 204 });
    }
    return json({ error: { code: "not_found", message: "nope" } }, 404);
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

describe("KeysPage", () => {
  it("lists keys with scope, prefix…, created, last used or never, and status", async () => {
    keysBackend();
    render(<KeysPage me={{ mode: "local" }} />);
    const rowCI = (await screen.findByText("ci")).closest("tr")!;
    expect(within(rowCI).getByText("analyze")).toHaveClass("pill");
    expect(within(rowCI).getByText("rm_live_ci12…")).toBeInTheDocument();
    expect(within(rowCI).getByText("never")).toBeInTheDocument();
    expect(within(rowCI).getByText("Active")).toBeInTheDocument();
    expect(within(rowCI).getByRole("button", { name: /revoke/i })).toBeInTheDocument();
    const rowOld = screen.getByText("old-laptop").closest("tr")!;
    expect(within(rowOld).getByText("Revoked")).toBeInTheDocument();
    expect(within(rowOld).queryByText("never")).not.toBeInTheDocument();
    expect(within(rowOld).queryByRole("button", { name: /revoke/i })).not.toBeInTheDocument();
  });

  it("creates a key and shows the secret once, with copy, warning and a placeholder curl example", async () => {
    const fn = keysBackend();
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    const { unmount } = render(<KeysPage me={{ mode: "local" }} />);
    await screen.findByText("ci");
    const create = screen.getByRole("button", { name: "Create" });
    expect(create).toBeDisabled();
    fireEvent.change(screen.getByLabelText("Key name"), { target: { value: "deploy" } });
    fireEvent.change(screen.getByLabelText("Scope"), { target: { value: "admin" } });
    fireEvent.click(create);

    const box = await screen.findByTestId("new-secret");
    expect(box).toHaveTextContent(SECRET);
    expect(box).toHaveTextContent("Copy this key now. You will not be able to see it again.");
    const post = fn.mock.calls.find(([, init]) => init?.method === "POST")!;
    expect(JSON.parse(String(post[1]?.body))).toEqual({ name: "deploy", scope: "admin" });

    const curl = screen.getByTestId("curl-example");
    expect(curl).toHaveTextContent(`${window.location.origin}/api/analyses`);
    expect(curl).toHaveTextContent("Authorization: Bearer <YOUR_KEY>");
    expect(curl.textContent).not.toContain(SECRET);
    // The secret appears exactly once on the page.
    expect(document.body.textContent!.split(SECRET).length - 1).toBe(1);

    fireEvent.click(within(box).getByRole("button", { name: /copy/i }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(SECRET));
    expect(await within(box).findByText(/copied/i)).toBeInTheDocument();

    // The new key is listed by name and prefix only.
    const row = screen.getByText("deploy").closest("tr")!;
    expect(within(row).getByText(`${SECRET.slice(0, 12)}…`)).toBeInTheDocument();

    // Navigating away and back: the secret is gone.
    unmount();
    render(<KeysPage me={{ mode: "local" }} />);
    await screen.findByText("ci");
    expect(screen.queryByTestId("new-secret")).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain(SECRET);
  });

  it("revokes only after an inline confirm step (no window.confirm)", async () => {
    const fn = keysBackend();
    const confirmSpy = vi.fn(() => true);
    vi.stubGlobal("confirm", confirmSpy);
    vi.stubGlobal("alert", vi.fn());
    render(<KeysPage me={{ mode: "local" }} />);
    const row = (await screen.findByText("ci")).closest("tr")!;
    fireEvent.click(within(row).getByRole("button", { name: /revoke/i }));
    expect(fn.mock.calls.some(([, init]) => init?.method === "DELETE")).toBe(false);
    expect(within(row).getByText(/permanent/i)).toBeInTheDocument();
    fireEvent.click(within(row).getByRole("button", { name: /cancel/i }));
    expect(within(row).queryByText(/permanent/i)).not.toBeInTheDocument();

    fireEvent.click(within(row).getByRole("button", { name: /revoke/i }));
    fireEvent.click(within(row).getByRole("button", { name: /confirm revoke/i }));
    await waitFor(() => expect(within(row).getByText("Revoked")).toBeInTheDocument());
    const del = fn.mock.calls.find(([, init]) => init?.method === "DELETE")!;
    expect(del[0]).toBe("/api/keys/k_ci00000001");
    expect(within(row).queryByRole("button", { name: /revoke/i })).not.toBeInTheDocument();
    expect(confirmSpy).not.toHaveBeenCalled();
  });

  it("shows cannot_revoke_self and forbidden_scope messages", async () => {
    keysBackend({ revokeStatus: 409 });
    render(<KeysPage me={{ mode: "local" }} />);
    const row = (await screen.findByText("ci")).closest("tr")!;
    fireEvent.click(within(row).getByRole("button", { name: /revoke/i }));
    fireEvent.click(within(row).getByRole("button", { name: /confirm revoke/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/cannot revoke the key you are signed in with/i);
    expect(within(row).getByText("Active")).toBeInTheDocument();
    cleanup();

    keysBackend({ listStatus: 403 });
    render(<KeysPage me={analyzeMe} />);
    expect(await screen.findByRole("alert")).toHaveTextContent(/admin scope/i);
  });
});

describe("VerifyPanel busy and rate-limit errors", () => {
  const props = {
    analysisId: "a1",
    commands: [{ id: "node-test", argv: ["node", "--test"], description: "d" }],
    defaultCommandId: "node-test",
    verifications: [],
    onVerified: () => {},
  };

  it("shows repo_busy as a clear inline message", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => json({ error: { code: "repo_busy", message: "busy" } }, 409)));
    render(<VerifyPanel {...props} />);
    fireEvent.click(screen.getByLabelText(/I approve running this command/i));
    fireEvent.click(screen.getByRole("button", { name: /run verification/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/being analysed or verified/i);
  });

  it("shows rate_limited with Retry-After", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => json({ error: { code: "rate_limited", message: "slow down" } }, 429, { "Retry-After": "9" })),
    );
    render(<VerifyPanel {...props} />);
    fireEvent.click(screen.getByLabelText(/I approve running this command/i));
    fireEvent.click(screen.getByRole("button", { name: /run verification/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Rate limit reached for this API key. Retry after 9 s.");
  });
});
