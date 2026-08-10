import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, createMaintenance, deleteMaintenance, getMaintenance } from "./api";
import type { MaintenanceWindow } from "./types";

/**
 * The maintenance half of lib/api.ts; `scope` is the annotations three-state (absent /
 * present-but-empty / a value).
 */

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

const problem = (status: number, title: string, detail?: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

const window_: MaintenanceWindow = {
  id: "m-1",
  scope: "",
  startAt: "2026-08-01T11:30:00Z",
  endAt: "2026-08-01T12:30:00Z",
  reason: "switch upgrade",
  createdBy: "user:ada",
  createdAt: "2026-08-01T10:00:00Z",
};

const emptyPage = { windows: [], nextCursor: "" };

function stubFetch(impl: (url: string, init?: RequestInit) => Response) {
  const fn = vi.fn((url: string, init?: RequestInit) => Promise.resolve(impl(String(url), init)));
  vi.stubGlobal("fetch", fn);
  return fn;
}

function queryOf(fn: ReturnType<typeof stubFetch>, index = 0): URLSearchParams {
  const url = String(fn.mock.calls[index][0]);
  return new URLSearchParams(url.split("?")[1] ?? "");
}

beforeEach(() => {
  document.cookie = "csrf=tok-123";
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.cookie = "csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT";
});

describe("getMaintenance", () => {
  it("omits scope entirely when the caller passed none — every scope", async () => {
    const fetchMock = stubFetch(() => json(emptyPage));
    await getMaintenance({ from: new Date("2026-08-01T11:00:00Z"), to: new Date("2026-08-01T12:00:00Z") });
    expect(queryOf(fetchMock).has("scope")).toBe(false);
  });

  it("sends a PRESENT-BUT-EMPTY scope for the global-only listing", async () => {
    const fetchMock = stubFetch(() => json(emptyPage));
    await getMaintenance({ scope: "" });
    expect(String(fetchMock.mock.calls[0][0])).toBe("/api/v1/maintenance?scope=");
  });

  it("bounds the listing with RFC3339 instants", async () => {
    const fetchMock = stubFetch(() => json(emptyPage));
    await getMaintenance({ scope: "node-a", from: new Date("2026-08-01T11:00:00Z"), to: new Date("2026-08-01T12:00:00Z") });
    const qs = queryOf(fetchMock);
    expect(qs.get("scope")).toBe("node-a");
    expect(qs.get("from")).toBe("2026-08-01T11:00:00.000Z");
    expect(qs.get("to")).toBe("2026-08-01T12:00:00.000Z");
  });
});

describe("createMaintenance", () => {
  it("POSTs the window and reads the created row back", async () => {
    const fetchMock = stubFetch(() => json(window_, { status: 201 }));
    const created = await createMaintenance({
      scope: "node-a→node-b",
      startAt: "2026-08-01T11:30:00Z",
      endAt: "2026-08-01T12:30:00Z",
      reason: "switch upgrade",
    });
    expect(created.id).toBe("m-1");
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toBe("/api/v1/maintenance");
    expect((init as RequestInit).method).toBe("POST");
    expect(JSON.parse(String((init as RequestInit).body))).toEqual({
      scope: "node-a→node-b",
      startAt: "2026-08-01T11:30:00Z",
      endAt: "2026-08-01T12:30:00Z",
      reason: "switch upgrade",
    });
  });

  it("surfaces the server's 422 as an ApiError carrying the problem detail", async () => {
    stubFetch(() => problem(422, "unprocessable", "endAt must be after startAt"));
    await expect(
      createMaintenance({ scope: "", startAt: "2026-08-01T12:30:00Z", endAt: "2026-08-01T11:30:00Z", reason: "x" }),
    ).rejects.toMatchObject({ problem: { detail: "endAt must be after startAt" } });
  });
});

describe("deleteMaintenance", () => {
  it("DELETEs the id and resolves on the 204", async () => {
    const fetchMock = stubFetch(() => new Response(null, { status: 204 }));
    await deleteMaintenance("m-1");
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toBe("/api/v1/maintenance/m-1");
    expect((init as RequestInit).method).toBe("DELETE");
  });

  it("escapes the id rather than pasting it into the path", async () => {
    const fetchMock = stubFetch(() => new Response(null, { status: 204 }));
    await deleteMaintenance("a/b c");
    expect(String(fetchMock.mock.calls[0][0])).toBe("/api/v1/maintenance/a%2Fb%20c");
  });

  it("rejects with an ApiError when the row is already gone", async () => {
    stubFetch(() => problem(404, "not found", "no such maintenance window"));
    await expect(deleteMaintenance("gone")).rejects.toBeInstanceOf(ApiError);
  });
});
