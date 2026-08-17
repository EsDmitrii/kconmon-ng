import { afterEach, describe, expect, it, vi } from "vitest";
import {
  ApiError,
  cancelRun,
  createSchedule,
  deleteSchedule,
  getConfig,
  getEvents,
  getMatrix,
  getMe,
  getTopology,
  getVersion,
  login,
  logout,
  promqlQuery,
  resetNavigateForTest,
  setNavigateForTest,
  updateSchedule,
} from "./api";

function mockFetch(status: number, body: unknown, contentType = "application/json") {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(
      new Response(JSON.stringify(body), { status, headers: { "Content-Type": contentType } }),
    ),
  );
}

afterEach(() => vi.unstubAllGlobals());

describe("api client", () => {
  it("parses topology", async () => {
    mockFetch(200, { nodes: [{ name: "n1", zone: "z", ready: true }], agents: [], timestamp: "t" });
    const topo = await getTopology();
    expect(topo.nodes[0].name).toBe("n1");
  });

  it("sends protocol and plane as query params", async () => {
    mockFetch(200, { protocol: "udp", plane: "pod", nodes: [], cells: [], timestamp: "t" });
    await getMatrix("udp");
    expect(vi.mocked(fetch).mock.calls[0][0]).toBe("/api/v1/matrix?protocol=udp&plane=pod");
  });

  it("throws ApiError on problem+json", async () => {
    mockFetch(503, { type: "about:blank", title: "prometheus not configured", status: 503 }, "application/problem+json");
    await expect(getMatrix("tcp")).rejects.toBeInstanceOf(ApiError);
  });

  it("returns Prometheus error envelopes instead of throwing", async () => {
    mockFetch(400, { status: "error", errorType: "bad_data", error: "parse error" });
    const res = await promqlQuery("up{");
    expect(res.status).toBe("error");
  });

  it("parses the version payload and its capability list", async () => {
    mockFetch(200, { version: "1.6.0", commit: "abc123", capabilities: ["events"] });
    const version = await getVersion();
    expect(version.capabilities).toEqual(["events"]);
    expect(vi.mocked(fetch).mock.calls[0][0]).toBe("/api/v1/version");
  });

  it("tolerates a version payload with no capabilities field", async () => {
    mockFetch(200, { version: "1.5.0", commit: "abc123" });
    const version = await getVersion();
    expect(version.capabilities).toBeUndefined();
  });

  it("parses the config payload", async () => {
    mockFetch(200, {
      auth: { mode: "anonymous", role: "admin" },
      anonymousBanner: true,
      controller: { configured: true },
      prometheus: { configured: true },
      database: { configured: false },
    });
    const config = await getConfig();
    expect(config.database.configured).toBe(false);
    expect(vi.mocked(fetch).mock.calls[0][0]).toBe("/api/v1/config");
  });
});

describe("getEvents", () => {
  function eventsPage(nextCursor = "") {
    return { events: [], nextCursor };
  }

  it("requests /api/v1/events with no query string when called with no args", async () => {
    mockFetch(200, eventsPage());
    await getEvents();
    expect(vi.mocked(fetch).mock.calls[0][0]).toBe("/api/v1/events");
  });

  it("builds one query key per field", async () => {
    mockFetch(200, eventsPage());
    await getEvents({
      types: ["check_observed"],
      scope: "node-a→node-b",
      from: new Date("2026-07-28T10:00:00.000Z"),
      to: new Date("2026-07-28T11:00:00.000Z"),
      limit: 50,
      cursor: "abc123",
    });
    const url = vi.mocked(fetch).mock.calls[0][0] as string;
    const qs = new URLSearchParams(url.split("?")[1]);
    expect(qs.getAll("type")).toEqual(["check_observed"]);
    expect(qs.get("scope")).toBe("node-a→node-b");
    expect(qs.get("from")).toBe("2026-07-28T10:00:00.000Z");
    expect(qs.get("to")).toBe("2026-07-28T11:00:00.000Z");
    expect(qs.get("limit")).toBe("50");
    expect(qs.get("cursor")).toBe("abc123");
  });

  it("repeats the type param once per requested type, in order", async () => {
    mockFetch(200, eventsPage());
    await getEvents({ types: ["check_observed", "mtr_completed", "mtr_triggered"] });
    const url = vi.mocked(fetch).mock.calls[0][0] as string;
    const qs = new URLSearchParams(url.split("?")[1]);
    expect(qs.getAll("type")).toEqual(["check_observed", "mtr_completed", "mtr_triggered"]);
  });

  it("omits keys for fields that were not supplied", async () => {
    mockFetch(200, eventsPage());
    await getEvents({ scope: "node-a" });
    const url = vi.mocked(fetch).mock.calls[0][0] as string;
    const qs = new URLSearchParams(url.split("?")[1]);
    expect(qs.has("type")).toBe(false);
    expect(qs.has("from")).toBe(false);
    expect(qs.has("to")).toBe(false);
    expect(qs.has("limit")).toBe(false);
    expect(qs.has("cursor")).toBe(false);
    expect(qs.get("scope")).toBe("node-a");
  });

  it("parses the returned page", async () => {
    mockFetch(200, {
      events: [
        {
          id: "1-1785276000000000000",
          seq: 1,
          type: "check_observed",
          severity: "info",
          scope: "node-a→node-b",
          timestamp: "2026-07-28T10:00:00Z",
          summary: "probe ok",
          details: {},
        },
      ],
      nextCursor: "next-page-token",
    });
    const page = await getEvents();
    expect(page.events).toHaveLength(1);
    expect(page.nextCursor).toBe("next-page-token");
  });

  it("throws ApiError on a 503 problem+json (history disabled)", async () => {
    mockFetch(
      503,
      { type: "about:blank", title: "event history not available", status: 503 },
      "application/problem+json",
    );
    await expect(getEvents()).rejects.toBeInstanceOf(ApiError);
  });

  it("throws ApiError on a 400 problem+json (malformed cursor/params)", async () => {
    mockFetch(400, { type: "about:blank", title: "invalid cursor", status: 400 }, "application/problem+json");
    await expect(getEvents({ cursor: "garbage" })).rejects.toBeInstanceOf(ApiError);
  });
});

function clearCsrfCookie() {
  document.cookie = "csrf=; path=/; expires=Thu, 01 Jan 1970 00:00:00 GMT";
}

describe("apiFetch: credentials + CSRF (task-19-brief.md)", () => {
  afterEach(clearCsrfCookie);

  it("sends credentials: same-origin on every request", async () => {
    mockFetch(200, { nodes: [], agents: [], timestamp: "t" });
    await getTopology();
    const [, init] = vi.mocked(fetch).mock.calls[0];
    expect(init?.credentials).toBe("same-origin");
  });

  it("echoes the csrf cookie as X-CSRF-Token on a mutation", async () => {
    document.cookie = "csrf=tok-xyz; path=/";
    mockFetch(200, { status: "success", data: { resultType: "vector", result: [] } });
    await promqlQuery("up");
    const [, init] = vi.mocked(fetch).mock.calls[0];
    expect(new Headers(init?.headers).get("X-CSRF-Token")).toBe("tok-xyz");
  });

  it("omits X-CSRF-Token on a GET even when the cookie is set", async () => {
    document.cookie = "csrf=tok-xyz; path=/";
    mockFetch(200, { nodes: [], agents: [], timestamp: "t" });
    await getTopology();
    const [, init] = vi.mocked(fetch).mock.calls[0];
    expect(new Headers(init?.headers).has("X-CSRF-Token")).toBe(false);
  });

  it("omits X-CSRF-Token on a mutation when no csrf cookie is set", async () => {
    mockFetch(200, { status: "success", data: { resultType: "vector", result: [] } });
    await promqlQuery("up");
    const [, init] = vi.mocked(fetch).mock.calls[0];
    expect(new Headers(init?.headers).has("X-CSRF-Token")).toBe(false);
  });
});

describe("apiFetch: 401 redirect (task-19-brief.md)", () => {
  afterEach(() => {
    window.history.pushState({}, "", "/");
    resetNavigateForTest();
  });

  it("a 401 redirects to /login with the current path as returnTo", async () => {
    window.history.pushState({}, "", "/matrix?foo=bar");
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    mockFetch(401, { type: "about:blank", title: "authentication required", status: 401 }, "application/problem+json");
    await expect(getMe()).rejects.toBeInstanceOf(ApiError);
    expect(navigateSpy).toHaveBeenCalledWith("/login?returnTo=%2Fmatrix%3Ffoo%3Dbar");
  });

  /* The owner's report: an unauthenticated hit on the root produced
     /login?returnTo=%2F — a parameter that asks for the place the login page
     already goes when nobody asks for anything. */
  it("writes NO query at all when the target is the root", async () => {
    window.history.pushState({}, "", "/");
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    mockFetch(401, { type: "about:blank", title: "authentication required", status: 401 }, "application/problem+json");
    await expect(getMe()).rejects.toBeInstanceOf(ApiError);
    expect(navigateSpy).toHaveBeenCalledWith("/login");
  });

  it("still writes one for the root WITH a query, which is not the same place", async () => {
    window.history.pushState({}, "", "/?protocol=udp");
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    mockFetch(401, { type: "about:blank", title: "authentication required", status: 401 }, "application/problem+json");
    await expect(getMe()).rejects.toBeInstanceOf(ApiError);
    expect(navigateSpy).toHaveBeenCalledWith("/login?returnTo=%2F%3Fprotocol%3Dudp");
  });

  it("a 403 does not redirect", async () => {
    window.history.pushState({}, "", "/matrix");
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    mockFetch(
      403,
      { type: "about:blank", title: "permission denied", status: 403, detail: "missing permission: mtr:run" },
      "application/problem+json",
    );
    await expect(getMe()).rejects.toBeInstanceOf(ApiError);
    expect(navigateSpy).not.toHaveBeenCalled();
  });

  it("does not loop when already on /login", async () => {
    window.history.pushState({}, "", "/login");
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    mockFetch(401, { type: "about:blank", title: "authentication required", status: 401 }, "application/problem+json");
    await expect(getMe()).rejects.toBeInstanceOf(ApiError);
    expect(navigateSpy).not.toHaveBeenCalled();
  });

  it("a 401 from POST /api/v1/auth/login itself does not redirect (that's a bad-password answer, not a session loss)", async () => {
    window.history.pushState({}, "", "/login");
    const navigateSpy = vi.fn();
    setNavigateForTest(navigateSpy);
    mockFetch(401, { type: "about:blank", title: "invalid credentials", status: 401 }, "application/problem+json");
    await expect(login("ada", "wrong")).rejects.toBeInstanceOf(ApiError);
    expect(navigateSpy).not.toHaveBeenCalled();
  });
});

describe("cancelRun", () => {
  afterEach(clearCsrfCookie);

  it("POSTs the cancel subresource with no body and resolves on the 204", async () => {
    document.cookie = "csrf=tok-abc; path=/";
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(null, { status: 204 })));
    await expect(cancelRun("run 1/2")).resolves.toBeUndefined();
    const [url, init] = vi.mocked(fetch).mock.calls[0];
    // The id is encoded, not interpolated raw -- it comes off a URL path.
    expect(url).toBe("/api/v1/runs/run%201%2F2/cancel");
    expect(init?.method).toBe("POST");
    expect(init?.body).toBeUndefined();
    expect(new Headers(init?.headers).get("X-CSRF-Token")).toBe("tok-abc");
  });

  it("rejects with ApiError on a 404 rather than swallowing it", async () => {
    mockFetch(404, { type: "about:blank", title: "run not found", status: 404 }, "application/problem+json");
    await expect(cancelRun("nope")).rejects.toBeInstanceOf(ApiError);
  });
});

describe("schedule writes", () => {
  afterEach(clearCsrfCookie);

  it("createSchedule POSTs the body verbatim", async () => {
    mockFetch(201, { id: "s-1" });
    await createSchedule({ definitionId: "d-1", kind: "interval", intervalNs: 30_000_000_000, enabled: true });
    const [url, init] = vi.mocked(fetch).mock.calls[0];
    expect(url).toBe("/api/v1/schedules");
    expect(init?.method).toBe("POST");
    expect(JSON.parse(String(init?.body))).toEqual({
      definitionId: "d-1",
      kind: "interval",
      intervalNs: 30_000_000_000,
      enabled: true,
    });
  });

  it("updateSchedule PUTs to the row's own URL", async () => {
    mockFetch(200, { id: "s-1" });
    await updateSchedule("s-1", { definitionId: "d-1", kind: "continuous", enabled: false });
    const [url, init] = vi.mocked(fetch).mock.calls[0];
    expect(url).toBe("/api/v1/schedules/s-1");
    expect(init?.method).toBe("PUT");
  });

  it("deleteSchedule resolves on the 204 (no JSON parse of an empty body)", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(null, { status: 204 })));
    await expect(deleteSchedule("s-1")).resolves.toBeUndefined();
    const [url, init] = vi.mocked(fetch).mock.calls[0];
    expect(url).toBe("/api/v1/schedules/s-1");
    expect(init?.method).toBe("DELETE");
  });
});

describe("auth endpoints", () => {
  afterEach(clearCsrfCookie);

  it("getMe parses the subject and permissions", async () => {
    mockFetch(200, {
      subject: { kind: "user", id: "u1", displayName: "Ada Lovelace", groups: ["g1"], roles: ["viewer"] },
      permissions: ["mtr:run"],
    });
    const me = await getMe();
    expect(me.subject.displayName).toBe("Ada Lovelace");
    expect(me.permissions).toEqual(["mtr:run"]);
    expect(vi.mocked(fetch).mock.calls[0][0]).toBe("/api/v1/auth/me");
  });

  it("login posts JSON and resolves on a 204", async () => {
    // 204 is a null-body status: the Response constructor throws if handed a
    // body alongside it, unlike mockFetch's other JSON.stringify(body) calls.
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(null, { status: 204 })));
    await expect(login("ada", "secret")).resolves.toBeUndefined();
    const [url, init] = vi.mocked(fetch).mock.calls[0];
    expect(url).toBe("/api/v1/auth/login");
    expect(init?.method).toBe("POST");
    expect(init?.body).toBe(JSON.stringify({ username: "ada", password: "secret" }));
  });

  it("login rejects with ApiError on a 401", async () => {
    mockFetch(401, { type: "about:blank", title: "invalid credentials", status: 401 }, "application/problem+json");
    await expect(login("ada", "wrong")).rejects.toBeInstanceOf(ApiError);
  });

  it("logout POSTs and echoes CSRF, resolving on a 204", async () => {
    document.cookie = "csrf=tok-abc; path=/";
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(null, { status: 204 })));
    await expect(logout()).resolves.toBeUndefined();
    const [url, init] = vi.mocked(fetch).mock.calls[0];
    expect(url).toBe("/api/v1/auth/logout");
    expect(new Headers(init?.headers).get("X-CSRF-Token")).toBe("tok-abc");
  });
});
