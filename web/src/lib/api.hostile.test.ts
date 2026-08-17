import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, getEvents, getIncidents } from "@/lib/api";

/*
 * The transport under hostile answers. Everything below is a shape the browser
 * can actually be handed — a nil Go slice, a proxy's HTML error page, a
 * connection cut mid-body — and none of it may reach a page as a raw JS
 * exception or as a null where types.ts promised an array.
 */

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const raw = (body: string, status: number, contentType: string) =>
  new Response(body, { status, headers: { "Content-Type": contentType } });

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("getEvents under a nil-slice body", () => {
  /* pages/live.tsx feeds page.events straight into pushEvents, which reads
     .length on it. A null there is a TypeError inside the load, and the feed
     reports "failed to load event history" for a request that in fact
     succeeded. */
  it("folds a null events array to []", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(json({ events: null, nextCursor: "" }))));
    const page = await getEvents();
    expect(page.events).toEqual([]);
  });

  /* A null nextCursor is never "" , so /live's `exhausted` check stays false
     forever: "Load older" keeps offering a walk that re-fetches page one. */
  it("folds a null nextCursor to the empty string the pagination reads", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(json({ events: [], nextCursor: null }))));
    expect((await getEvents()).nextCursor).toBe("");
  });

  it("leaves a real page alone", async () => {
    const event = {
      id: "1-1",
      seq: 1,
      type: "check_observed",
      severity: "info",
      scope: "a→b",
      timestamp: "2026-01-01T00:00:00Z",
      summary: "s",
      details: {},
    };
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(json({ events: [event], nextCursor: "c" }))));
    const page = await getEvents();
    expect(page.events).toHaveLength(1);
    expect(page.nextCursor).toBe("c");
  });
});

describe("getIncidents under a nil-slice body", () => {
  it("folds a null incidents array to []", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(json({ incidents: null, nextCursor: null }))));
    const page = await getIncidents();
    expect(page.incidents).toEqual([]);
    expect(page.nextCursor).toBe("");
  });
});

describe("handle() keeps the HTTP status when the body is not the JSON it claims", () => {
  /* An ingress or a sidecar answering an HTML error page under a JSON
     Content-Type used to throw the JSON parser's own SyntaxError, and the 502
     — the only fact worth reporting — was lost with it. */
  it("reports the status rather than the parser's complaint on a 502 with an HTML body", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.resolve(raw("<html><body>502 Bad Gateway</body></html>", 502, "application/json"))),
    );
    await expect(getEvents()).rejects.toSatisfy(
      (e: unknown) => e instanceof ApiError && e.problem.status === 502,
    );
  });

  it("reports a 500 whose body is empty", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(raw("", 500, "application/json"))));
    await expect(getEvents()).rejects.toSatisfy(
      (e: unknown) => e instanceof ApiError && e.problem.status === 500,
    );
  });

  /* A 200 whose body was cut in half is still a failed read, and a page that
     catches ApiError must be given one rather than a bare SyntaxError whose
     message ("Unexpected end of JSON input") it would print at the operator. */
  it("turns a truncated 200 body into an ApiError, not a raw SyntaxError", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(raw('{"events":[', 200, "application/json"))));
    await expect(getEvents()).rejects.toSatisfy((e: unknown) => e instanceof ApiError);
  });

  it("still surfaces a real problem+json verbatim", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() =>
        Promise.resolve(
          raw(
            JSON.stringify({ type: "about:blank", title: "nope", status: 503, detail: "history is off" }),
            503,
            "application/problem+json",
          ),
        ),
      ),
    );
    await expect(getEvents()).rejects.toSatisfy(
      (e: unknown) => e instanceof ApiError && e.problem.detail === "history is off",
    );
  });
});
