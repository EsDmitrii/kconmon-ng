import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { resetNavigateForTest, setNavigateForTest } from "@/lib/api";
import { FakeSocket } from "@/lib/fake-websocket";
import { parseInvestigationParams } from "@/lib/investigation-sources";
import type { CheckDefinition, RunDetail, Schedule, Target } from "@/lib/types";
import {
  healthFromVector,
  runsTouchingTarget,
  TargetCardPage,
  targetDurationQuery,
  targetHealthQuery,
  targetIdFromPath,
} from "./target-card";

// EChart is mocked, not rendered: echarts.init() reaches for a 2d canvas
// context jsdom does not implement (no `canvas` package in devDependencies),
// and a real mount throws. Mocking it keeps the assertion this file actually
// cares about — a chart is rendered for a non-empty series and NOT rendered
// for an empty one — while leaving the option-building itself covered by
// lib/curated-metrics.test.ts.
vi.mock("@/components/echart", () => ({
  EChart: ({ className }: { className?: string }) => <div data-testid="echart" className={className} />,
}));

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

const problem = (status: number, title: string, detail?: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

function configBody(opts: { database?: boolean; prometheus?: boolean } = {}) {
  const { database = true, prometheus = true } = opts;
  return {
    auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
    anonymousBanner: false,
    controller: { configured: true },
    prometheus: { configured: prometheus },
    database: { configured: database },
  };
}

function meBody(permissions: string[]) {
  return {
    subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["operator"] },
    permissions,
  };
}

const TARGET_ID = "11111111-1111-1111-1111-111111111111";

const target: Target = {
  id: TARGET_ID,
  name: "edge-gw",
  kind: "host",
  address: "10.10.0.1",
  labels: { team: "net" },
  createdAt: "2026-08-01T00:00:00Z",
  updatedAt: "2026-08-01T00:00:00Z",
};

const definition: CheckDefinition = {
  id: "d-1",
  name: "edge-gw tcp",
  sourceSelection: "one-per-zone",
  destinationKind: "target",
  destinationTargetId: TARGET_ID,
  destinationAddress: "",
  checkType: "tcp",
  plane: "pod",
  params: {},
  enabled: true,
  createdAt: "2026-08-01T00:00:00Z",
  updatedAt: "2026-08-01T00:00:00Z",
};

const schedule: Schedule = {
  id: "s-1",
  definitionId: "d-1",
  kind: "interval",
  intervalNs: 60_000_000_000,
  runAt: null,
  enabled: true,
  lastFiredAt: null,
  nextFireAt: "2026-08-02T00:00:00Z",
  createdAt: "2026-08-01T00:00:00Z",
  updatedAt: "2026-08-01T00:00:00Z",
};

function runDetail(id: string, overrides: Partial<RunDetail> = {}): RunDetail {
  return {
    id,
    createdAt: "2026-08-01T00:00:00Z",
    status: "succeeded",
    type: "tcp",
    plane: "pod",
    initiatorKind: "user",
    initiatorId: "u1",
    pairTotal: 1,
    pairOk: 1,
    pairFailed: 0,
    spec: {},
    results: [],
    ...overrides,
  };
}

/** A spec snapshot the way checks.Spec marshals: exported Go field names, and
 *  a Destination's own lowercase json tags. */
function targetSpec(name: string, address = "10.10.0.1") {
  return {
    Sources: ["node-a"],
    Destinations: null,
    Type: "tcp",
    Plane: "pod",
    TypedDestinations: [{ kind: "target", name, address }],
    Timeout: 5_000_000_000,
  };
}

interface RenderOpts {
  permissions?: string[];
  database?: boolean;
  prometheus?: boolean;
  targetResponse?: Response;
  definitions?: CheckDefinition[];
  schedules?: Record<string, Schedule[]>;
  runs?: { id: string }[];
  runDetails?: Record<string, RunDetail>;
  rangeResult?: unknown[];
  healthResult?: unknown[];
  incidents?: unknown[];
}

function renderPage(pathname = `/targets/${TARGET_ID}`, opts: RenderOpts = {}) {
  const {
    permissions = ["targets:read", "checks:read"],
    database = true,
    prometheus = true,
    targetResponse,
    definitions = [],
    schedules = {},
    runs = [],
    runDetails = {},
    rangeResult = [],
    healthResult = [],
    incidents = [],
  } = opts;
  window.history.pushState({}, "", pathname);
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    if (href.includes("/api/v1/version")) {
      return Promise.resolve(json({ version: "1.6.0", commit: "x", capabilities: [] }));
    }
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody({ database, prometheus })));
    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.startsWith("/api/v1/incidents")) return Promise.resolve(json({ incidents, nextCursor: "" }));
    if (href.startsWith("/api/v1/events")) return Promise.resolve(json({ events: [], nextCursor: "" }));
    if (href.startsWith("/api/v1/targets/")) {
      return Promise.resolve(targetResponse ?? json(target));
    }
    if (href.startsWith("/api/v1/checks")) return Promise.resolve(json({ definitions, nextCursor: "" }));
    if (href.startsWith("/api/v1/schedules")) {
      const definitionId = new URL(href, "http://localhost").searchParams.get("definitionId") ?? "";
      return Promise.resolve(json({ schedules: schedules[definitionId] ?? [], nextCursor: "" }));
    }
    if (href.startsWith("/api/v1/runs/")) {
      const id = href.slice("/api/v1/runs/".length);
      return Promise.resolve(json(runDetails[id] ?? runDetail(id)));
    }
    if (href.startsWith("/api/v1/runs")) return Promise.resolve(json({ runs, nextCursor: "" }));
    if (href.includes("/api/v1/promql/query_range") && method === "POST") {
      return Promise.resolve(json({ status: "success", data: { resultType: "matrix", result: rangeResult } }));
    }
    if (href.includes("/api/v1/promql/query") && method === "POST") {
      return Promise.resolve(json({ status: "success", data: { resultType: "vector", result: healthResult } }));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  const navigateSpy = vi.fn();
  setNavigateForTest(navigateSpy);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TargetCardPage />
      </ThemeProvider>
    </QueryClientProvider>,
  );
  return { ...utils, fetchMock, qc, navigateSpy };
}

const called = (fetchMock: { mock: { calls: unknown[][] } }, prefix: string) =>
  fetchMock.mock.calls.some((c) => String(c[0]).startsWith(prefix));

beforeEach(() => {
  FakeSocket.reset();
  vi.stubGlobal("WebSocket", FakeSocket);
});

afterEach(() => {
  cleanup();
  resetWsClient();
  vi.unstubAllGlobals();
  resetNavigateForTest();
  window.history.pushState({}, "", "/");
});

describe("targetIdFromPath", () => {
  it("reads the id straight off the pathname", () => {
    expect(targetIdFromPath(`/targets/${TARGET_ID}`)).toBe(TARGET_ID);
  });

  it("returns empty for the list route and for an unrelated path", () => {
    expect(targetIdFromPath("/targets")).toBe("");
    expect(targetIdFromPath("/targets/")).toBe("");
    expect(targetIdFromPath("/nodes/node-a")).toBe("");
  });

  it("decodes percent-escapes and falls back to the raw remainder when malformed", () => {
    expect(targetIdFromPath(`/targets/${encodeURIComponent("a b")}`)).toBe("a b");
    expect(targetIdFromPath("/targets/%E0%A4%A")).toBe("%E0%A4%A");
  });
});

describe("targetDurationQuery / targetHealthQuery", () => {
  it("scope every series to this target through the external metric family", () => {
    expect(targetDurationQuery("edge-gw")).toContain("kconmon_ng_external_duration_seconds_bucket");
    expect(targetDurationQuery("edge-gw")).toContain('target="edge-gw"');
    expect(targetHealthQuery("edge-gw")).toContain("kconmon_ng_external_results_total");
    expect(targetHealthQuery("edge-gw")).toContain('result="success"');
  });

  it("escapes quotes and backslashes in a target name", () => {
    expect(targetDurationQuery('a"b')).toContain('target="a\\"b"');
    expect(targetHealthQuery("c\\d")).toContain('target="c\\\\d"');
  });
});

describe("healthFromVector", () => {
  it("turns a success ratio into a percentage and a tier", () => {
    expect(healthFromVector({ status: "success", data: { resultType: "vector", result: [{ metric: {}, value: [1, "1"] }] } })).toEqual({
      percent: 100,
      tier: "ok",
    });
    expect(healthFromVector({ status: "success", data: { resultType: "vector", result: [{ metric: {}, value: [1, "0.95"] }] } })).toEqual({
      percent: 95,
      tier: "warn",
    });
    expect(healthFromVector({ status: "success", data: { resultType: "vector", result: [{ metric: {}, value: [1, "0.5"] }] } })).toEqual({
      percent: 50,
      tier: "bad",
    });
  });

  it("reads an empty vector, an error envelope and NaN as no data rather than 0%", () => {
    expect(healthFromVector({ status: "success", data: { resultType: "vector", result: [] } })).toEqual({
      percent: null,
      tier: "unknown",
    });
    expect(healthFromVector({ status: "error", error: "boom" })).toEqual({ percent: null, tier: "unknown" });
    expect(healthFromVector(undefined)).toEqual({ percent: null, tier: "unknown" });
    expect(
      healthFromVector({ status: "success", data: { resultType: "vector", result: [{ metric: {}, value: [1, "NaN"] }] } }),
    ).toEqual({ percent: null, tier: "unknown" });
  });
});

describe("runsTouchingTarget", () => {
  it("keeps runs whose spec names this target and drops the rest", () => {
    const mine = runDetail("run-1", { spec: targetSpec("edge-gw") });
    const other = runDetail("run-2", { spec: targetSpec("other-gw") });
    const nodeRun = runDetail("run-3", { spec: { Sources: ["node-a"], Destinations: ["node-b"], Type: "tcp" } });
    expect(runsTouchingTarget([mine, other, nodeRun], "edge-gw").map((r) => r.id)).toEqual(["run-1"]);
  });

  it("does not treat an ad-hoc destination that happens to share the name as this target", () => {
    const adhoc = runDetail("run-4", {
      spec: { TypedDestinations: [{ kind: "adhoc", name: "edge-gw", address: "edge-gw" }] },
    });
    expect(runsTouchingTarget([adhoc], "edge-gw")).toEqual([]);
  });

  it("survives a spec shape it does not recognise", () => {
    expect(runsTouchingTarget([runDetail("r", { spec: null }), runDetail("r2", { spec: "nope" })], "edge-gw")).toEqual([]);
  });
});

describe("TargetCardPage", () => {
  it("renders name, kind and address on a cold load of a bookmarked /targets/{id}", async () => {
    renderPage();
    expect(await screen.findByRole("heading", { name: "edge-gw" })).toBeInTheDocument();
    expect(screen.getByText("host")).toBeInTheDocument();
    expect(screen.getByText("10.10.0.1")).toBeInTheDocument();
  });

  it("shows an honest not-found card for an unknown id instead of crashing", async () => {
    renderPage(`/targets/${TARGET_ID}`, { targetResponse: problem(404, "not found", "no such target") });
    expect(await screen.findByText("This target does not exist")).toBeInTheDocument();
    expect(screen.queryByRole("radiogroup", { name: "Tab" })).not.toBeInTheDocument();
  });

  it("says the database is required and fires no target or event request when it is disabled", async () => {
    const { fetchMock } = renderPage(`/targets/${TARGET_ID}`, { database: false });
    await screen.findByText(/stored in the database/i);
    await waitFor(() => expect(called(fetchMock, "/api/v1/config")).toBe(true));
    expect(called(fetchMock, "/api/v1/targets")).toBe(false);
    expect(called(fetchMock, "/api/v1/events")).toBe(false);
    expect(called(fetchMock, "/api/v1/checks")).toBe(false);
  });

  it("explains the missing permission and fires no target request without targets:read", async () => {
    const { fetchMock } = renderPage(`/targets/${TARGET_ID}`, { permissions: [] });
    expect(await screen.findByText(/Requires the targets:read permission/)).toBeInTheDocument();
    expect(called(fetchMock, "/api/v1/targets")).toBe(false);
  });

  it("pins the recent-changes rail to the target's own name", async () => {
    const { fetchMock } = renderPage();
    await waitFor(() => expect(called(fetchMock, "/api/v1/events")).toBe(true));
    const call = fetchMock.mock.calls.find((c) => String(c[0]).startsWith("/api/v1/events"));
    const url = new URL(String(call?.[0]), "http://localhost");
    expect(url.searchParams.get("scope")).toBe("edge-gw");
  });

  it("lists the definitions pointing at this target and each one's schedules", async () => {
    const { fetchMock } = renderPage(`/targets/${TARGET_ID}`, {
      definitions: [definition],
      schedules: { "d-1": [schedule] },
    });
    expect(await screen.findByText("edge-gw tcp")).toBeInTheDocument();
    expect(await screen.findByText(/every 1m/)).toBeInTheDocument();
    const checksCall = fetchMock.mock.calls.find((c) => String(c[0]).startsWith("/api/v1/checks"));
    expect(new URL(String(checksCall?.[0]), "http://localhost").searchParams.get("targetId")).toBe(TARGET_ID);
  });

  it("says so when no definition points at this target", async () => {
    renderPage();
    expect(await screen.findByText(/No check definition points at this target/i)).toBeInTheDocument();
  });

  it("renders an honest note instead of an empty-axis chart when the series is empty", async () => {
    renderPage(`/targets/${TARGET_ID}`, { rangeResult: [] });
    fireEvent.click(await screen.findByRole("radio", { name: "History" }));
    expect(await screen.findByText(/No external probe series for this target/i)).toBeInTheDocument();
    expect(screen.queryByTestId("echart")).not.toBeInTheDocument();
  });

  it("renders the chart when the PromQL proxy returns a series", async () => {
    renderPage(`/targets/${TARGET_ID}`, {
      rangeResult: [{ metric: { source_node: "node-a" }, values: [[1, "0.004"]] }],
    });
    fireEvent.click(await screen.findByRole("radio", { name: "History" }));
    expect(await screen.findByTestId("echart")).toBeInTheDocument();
    expect(screen.queryByText(/No external probe series for this target/i)).not.toBeInTheDocument();
  });

  it("queries the range proxy for the external duration series of this target", async () => {
    const { fetchMock } = renderPage();
    fireEvent.click(await screen.findByRole("radio", { name: "History" }));
    await waitFor(() =>
      expect(fetchMock.mock.calls.some((c) => String(c[0]).includes("/api/v1/promql/query_range"))).toBe(true),
    );
    const call = fetchMock.mock.calls.find((c) => String(c[0]).includes("/api/v1/promql/query_range"));
    const body = JSON.parse(String((call?.[1] as RequestInit | undefined)?.body ?? "{}")) as { query: string };
    expect(body.query).toContain("kconmon_ng_external_duration_seconds_bucket");
    expect(body.query).toContain('target="edge-gw"');
  });

  it("says the history tab has nothing behind it when Prometheus is not configured, and asks it nothing", async () => {
    const { fetchMock } = renderPage(`/targets/${TARGET_ID}`, { prometheus: false });
    fireEvent.click(await screen.findByRole("radio", { name: "History" }));
    expect(await screen.findByText(/Prometheus is not configured/i)).toBeInTheDocument();
    expect(fetchMock.mock.calls.some((c) => String(c[0]).includes("/api/v1/promql"))).toBe(false);
  });

  it("filters the runs tab down to runs whose spec names this target", async () => {
    renderPage(`/targets/${TARGET_ID}`, {
      runs: [{ id: "run-1" }, { id: "run-2" }],
      runDetails: {
        "run-1": runDetail("run-1", { spec: targetSpec("edge-gw") }),
        "run-2": runDetail("run-2", { spec: targetSpec("other-gw") }),
      },
    });
    fireEvent.click(await screen.findByRole("radio", { name: "Runs" }));
    expect(await screen.findByRole("link", { name: "run-1" })).toHaveAttribute("href", "/diagnostics/runs/run-1");
    expect(screen.queryByRole("link", { name: "run-2" })).not.toBeInTheDocument();
  });

  it("keeps the card read-only: no create/edit/delete affordance even with every write permission", async () => {
    renderPage(`/targets/${TARGET_ID}`, {
      permissions: ["targets:read", "targets:write", "checks:read", "checks:write", "schedules:write"],
      definitions: [definition],
      schedules: { "d-1": [schedule] },
    });
    await screen.findByRole("heading", { name: "edge-gw" });
    expect(screen.queryByRole("button", { name: /edit/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /delete/i })).not.toBeInTheDocument();
  });

  it("explains the missing checks:read permission on the checks tab", async () => {
    renderPage(`/targets/${TARGET_ID}`, { permissions: ["targets:read"] });
    expect(await screen.findByText(/Requires the checks:read permission/)).toBeInTheDocument();
  });

  it("renders a not-found card when the URL carries no id at all", async () => {
    const { fetchMock } = renderPage("/targets/");
    expect(await screen.findByText("This link is missing a target id.")).toBeInTheDocument();
    expect(called(fetchMock, "/api/v1/targets")).toBe(false);
  });
});

/* ── M6 Task 8: the Investigate entry point + the related-incidents rail ── */

describe("TargetCardPage — Investigate entry point", () => {
  it("links to a TARGET investigation carrying the target's NAME, not its id", async () => {
    renderPage();
    const href = (await screen.findByRole("link", { name: "Investigate" })).getAttribute("href") ?? "";

    const p = parseInvestigationParams(href.slice(href.indexOf("?")), new Date());
    expect(p.kind).toBe("target");
    expect(p.a).toBe("edge-gw");
    expect(href).not.toContain(TARGET_ID);
    expect(p.to.getTime() - p.from.getTime()).toBe(60 * 60 * 1000);
  });
});

describe("TargetCardPage — open incidents rail", () => {
  it("filters the open list to incidents scoped to this target's name", async () => {
    const row = (over: Record<string, unknown>) => ({
      id: "inc-1",
      title: "edge-gw unreachable",
      scope: "edge-gw",
      fromAt: "2026-01-01T00:00:00Z",
      status: "open",
      notes: "",
      pinned: [],
      createdBy: "user:ada",
      createdAt: "2026-01-01T00:00:00Z",
      ...over,
    });
    renderPage(`/targets/${TARGET_ID}`, {
      permissions: ["targets:read", "incidents:read"],
      incidents: [row({}), row({ id: "inc-2", title: "a node's problem", scope: "node-a" })],
    });

    const rail = await screen.findByRole("complementary", { name: "Open incidents" });
    const rows = await within(rail).findAllByTestId("related-incident");
    expect(rows).toHaveLength(1);
    expect(within(rows[0]).getByRole("link", { name: "edge-gw unreachable" }).getAttribute("href")).toBe(
      "/investigate?incident=inc-1",
    );
  });
});
