import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, createAnnotation, deleteAnnotation, listAnnotations } from "./api";
import type { Annotation } from "./types";

/** The annotations half of lib/api.ts, in its own file rather than appended to api.test.ts. */

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

const problem = (status: number, title: string, detail?: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

const annotation: Annotation = {
  id: "a-1",
  startAt: "2026-08-01T11:30:00Z",
  scope: "",
  text: "rolled the gateway",
  createdBy: "user:ada",
  createdAt: "2026-08-01T11:30:01Z",
};

const emptyPage = { annotations: [], nextCursor: "" };

function stubFetch(impl: (url: string, init?: RequestInit) => Response) {
  const fn = vi.fn((url: string, init?: RequestInit) => Promise.resolve(impl(String(url), init)));
  vi.stubGlobal("fetch", fn);
  return fn;
}

/** The one query string the call under test produced. */
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

describe("listAnnotations", () => {
  it("omits scope entirely when the caller passed none — every scope", async () => {
    const fetchMock = stubFetch(() => json(emptyPage));
    await listAnnotations({ from: new Date("2026-08-01T11:00:00Z"), to: new Date("2026-08-01T12:00:00Z") });
    const url = String(fetchMock.mock.calls[0][0]);
    expect(url.startsWith("/api/v1/annotations?")).toBe(true);
    expect(queryOf(fetchMock).has("scope")).toBe(false);
  });

  it("sends a PRESENT-BUT-EMPTY scope for the global-only listing", async () => {
    const fetchMock = stubFetch(() => json(emptyPage));
    await listAnnotations({ scope: "" });
    const url = String(fetchMock.mock.calls[0][0]);
    expect(url).toBe("/api/v1/annotations?scope=");
    const qs = queryOf(fetchMock);
    expect(qs.has("scope")).toBe(true);
    expect(qs.get("scope")).toBe("");
  });

  it("sends an exact scope unchanged, arrow and all", async () => {
    const fetchMock = stubFetch(() => json(emptyPage));
    await listAnnotations({ scope: "node-a→node-b" });
    expect(queryOf(fetchMock).get("scope")).toBe("node-a→node-b");
  });

  it("bounds the window with RFC3339 from/to", async () => {
    const fetchMock = stubFetch(() => json(emptyPage));
    await listAnnotations({ from: new Date("2026-08-01T11:00:00Z"), to: new Date("2026-08-01T12:00:00Z") });
    const qs = queryOf(fetchMock);
    expect(qs.get("from")).toBe("2026-08-01T11:00:00.000Z");
    expect(qs.get("to")).toBe("2026-08-01T12:00:00.000Z");
  });

  it("passes limit and cursor through, and omits them when unset", async () => {
    const fetchMock = stubFetch(() => json(emptyPage));
    await listAnnotations({ limit: 50, cursor: "c1" });
    expect(queryOf(fetchMock).get("limit")).toBe("50");
    expect(queryOf(fetchMock).get("cursor")).toBe("c1");
    await listAnnotations({});
    expect(String(fetchMock.mock.calls[1][0])).toBe("/api/v1/annotations");
  });

  it("returns the page body", async () => {
    stubFetch(() => json({ annotations: [annotation], nextCursor: "next" }));
    const page = await listAnnotations({ scope: "" });
    expect(page.annotations).toHaveLength(1);
    expect(page.annotations[0].text).toBe("rolled the gateway");
    expect(page.nextCursor).toBe("next");
  });

  it("surfaces problem+json as an ApiError", async () => {
    stubFetch(() => problem(503, "database unavailable", "console.database.mode is unset"));
    await expect(listAnnotations({ scope: "" })).rejects.toBeInstanceOf(ApiError);
  });
});

describe("createAnnotation", () => {
  it("POSTs the body verbatim with the CSRF header attached", async () => {
    const fetchMock = stubFetch(() => json(annotation, { status: 201 }));
    await createAnnotation({ startAt: "2026-08-01T11:30:00Z", scope: "", text: "rolled the gateway" });
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/annotations");
    expect(init?.method).toBe("POST");
    expect(JSON.parse(String(init?.body))).toEqual({
      startAt: "2026-08-01T11:30:00Z",
      scope: "",
      text: "rolled the gateway",
    });
    expect(new Headers(init?.headers).get("X-CSRF-Token")).toBe("tok-123");
  });

  it("carries endAt only when the caller supplied one", async () => {
    const fetchMock = stubFetch(() => json(annotation, { status: 201 }));
    await createAnnotation({
      startAt: "2026-08-01T11:30:00Z",
      endAt: "2026-08-01T11:45:00Z",
      scope: "node-a",
      text: "drain",
    });
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({
      startAt: "2026-08-01T11:30:00Z",
      endAt: "2026-08-01T11:45:00Z",
      scope: "node-a",
      text: "drain",
    });
  });

  it("answers the created annotation", async () => {
    stubFetch(() => json(annotation, { status: 201 }));
    await expect(createAnnotation({ startAt: annotation.startAt, scope: "", text: annotation.text })).resolves.toEqual(
      annotation,
    );
  });

  it("surfaces a 403 as an ApiError rather than resolving", async () => {
    stubFetch(() => problem(403, "forbidden", "annotations:write required"));
    await expect(createAnnotation({ startAt: annotation.startAt, scope: "", text: "x" })).rejects.toBeInstanceOf(
      ApiError,
    );
  });
});

describe("deleteAnnotation", () => {
  it("DELETEs the id-scoped path and resolves on 204", async () => {
    const fetchMock = stubFetch(() => new Response(null, { status: 204 }));
    await expect(deleteAnnotation("a-1")).resolves.toBeUndefined();
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/annotations/a-1");
    expect(init?.method).toBe("DELETE");
    expect(new Headers(init?.headers).get("X-CSRF-Token")).toBe("tok-123");
  });

  it("encodes the id", async () => {
    const fetchMock = stubFetch(() => new Response(null, { status: 204 }));
    await deleteAnnotation("a/1");
    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/annotations/a%2F1");
  });

  it("surfaces a 404 as an ApiError", async () => {
    stubFetch(() => problem(404, "not found"));
    await expect(deleteAnnotation("nope")).rejects.toBeInstanceOf(ApiError);
  });
});
