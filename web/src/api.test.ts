import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "./api";

function mockFetch(impl: (url: string, init?: RequestInit) => Promise<Response>) {
  const fn = vi.fn(impl);
  vi.stubGlobal("fetch", fn);
  return fn;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("api", () => {
  it("parses contract error bodies into ApiError", async () => {
    mockFetch(async () =>
      new Response(JSON.stringify({ error: { code: "not_approved", message: "approval required" } }), {
        status: 400,
        headers: { "Content-Type": "application/json" },
      }),
    );
    await expect(api.health()).rejects.toMatchObject({ code: "not_approved", message: "approval required", status: 400 });
  });

  it("reports a clear message when the backend is unreachable", async () => {
    mockFetch(async () => {
      throw new TypeError("Failed to fetch");
    });
    const err = await api.health().catch((e) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err.code).toBe("network");
    expect(err.message).toMatch(/backend/i);
  });

  it("reports non-JSON 5xx (proxy error) as backend unreachable-ish error", async () => {
    mockFetch(async () => new Response("Bad Gateway", { status: 502 }));
    const err = await api.health().catch((e) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err.status).toBe(502);
  });

  it("POSTs verify with approved:true and encodes the analysis id", async () => {
    const fn = mockFetch(async () =>
      new Response(JSON.stringify({ command_id: "c", argv: [], at: "", run: {} }), { status: 200 }),
    );
    await api.verify("a/1", { command_id: "c", approved: true });
    const [url, init] = fn.mock.calls[0];
    expect(url).toBe("/api/analyses/a%2F1/verify");
    expect(init?.method).toBe("POST");
    expect(JSON.parse(String(init?.body))).toEqual({ command_id: "c", approved: true });
  });
});
