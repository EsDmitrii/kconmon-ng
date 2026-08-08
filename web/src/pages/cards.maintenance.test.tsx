import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import { TimeMachineProvider } from "@/lib/timemachine";
import type { MaintenanceWindow } from "@/lib/types";
import { PairCardPage } from "./pair-card";
import { TargetCardPage } from "./target-card";

/**
 * The object cards' MAINTENANCE surface — the sibling of
 * cards.annotations.test.tsx, and the same one question: WHICH SCOPE does each
 * card ask for. Each asks for two (its own object's, plus the global one) and
 * neither leg may go out with the scope parameter absent, which would drag
 * every other object's declared windows onto this card.
 *
 * The EChart mock re-publishes BOTH overlays it was handed, so a page test can
 * see that the bands reached the chart while staying out of the business of how
 * they are drawn — that geometry is asserted in lib/annotations.test.ts, where
 * it is built.
 */
vi.mock("@/components/echart", () => ({
  EChart: ({ className, maintenance }: { className?: string; maintenance?: MaintenanceWindow[] }) => (
    <div data-testid="echart" className={className} data-maintenance={(maintenance ?? []).map((w) => w.id).join(",")} />
  ),
}));

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

const TARGET_ID = "11111111-1111-1111-1111-111111111111";

function win(over: Partial<MaintenanceWindow> = {}): MaintenanceWindow {
  return {
    id: "m-1",
    scope: "",
    startAt: "2026-08-01T11:30:00Z",
    endAt: "2026-08-01T11:45:00Z",
    reason: "switch upgrade",
    createdBy: "user:ada",
    createdAt: "2026-08-01T10:00:00Z",
    ...over,
  };
}

const topologyBody = {
  nodes: [{ name: "node-a", zone: "z1", ready: true }],
  agents: [{ id: "agent-1", nodeName: "node-a", podIP: "10.0.0.1", zone: "z1" }],
  timestamp: "t",
};

const matrixBody = {
  protocol: "tcp",
  plane: "pod",
  nodes: ["node-a", "node-b"],
  cells: [{ source: "node-a", destination: "node-b", failRatio: 0, rttP95: 1_000_000 }],
  timestamp: "t",
};

const configBody = {
  auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
  anonymousBanner: false,
  controller: { configured: true },
  prometheus: { configured: true },
  database: { configured: true },
};

const targetBody = {
  id: TARGET_ID,
  name: "edge-gw",
  kind: "host",
  address: "10.10.0.1",
  labels: {},
  createdAt: "2026-08-01T00:00:00Z",
  updatedAt: "2026-08-01T00:00:00Z",
};

interface StubOpts {
  permissions?: string[];
  byScope?: Record<string, MaintenanceWindow[]>;
}

function stubFetch(opts: StubOpts = {}) {
  const {
    permissions = ["maintenance:read", "maintenance:write", "targets:read", "checks:read"],
    byScope = {},
  } = opts;
  const maintenanceQueries: URLSearchParams[] = [];
  const createBodies: unknown[] = [];
  const deleted: string[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    if (href.includes("/api/v1/version")) return Promise.resolve(json({ version: "1.8.0", commit: "x", capabilities: [] }));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody));
    if (href.includes("/api/v1/auth/me")) {
      return Promise.resolve(
        json({ subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: [] }, permissions }),
      );
    }
    if (href.startsWith("/api/v1/maintenance/") && method === "DELETE") {
      deleted.push(decodeURIComponent(href.slice("/api/v1/maintenance/".length)));
      return Promise.resolve(new Response(null, { status: 204 }));
    }
    if (href.startsWith("/api/v1/maintenance") && method === "POST") {
      createBodies.push(JSON.parse(String(init?.body ?? "{}")));
      return Promise.resolve(json(win({ id: "new" }), { status: 201 }));
    }
    if (href.startsWith("/api/v1/maintenance")) {
      const qs = new URLSearchParams(href.split("?")[1] ?? "");
      maintenanceQueries.push(qs);
      const key = qs.has("scope") ? (qs.get("scope") as string) : " ";
      return Promise.resolve(json({ windows: byScope[key] ?? [], nextCursor: "" }));
    }
    if (href.startsWith("/api/v1/annotations")) return Promise.resolve(json({ annotations: [], nextCursor: "" }));
    if (href.includes("/api/v1/topology")) return Promise.resolve(json(topologyBody));
    if (href.includes("/api/v1/matrix")) return Promise.resolve(json(matrixBody));
    if (href.startsWith("/api/v1/events")) return Promise.resolve(json({ events: [], nextCursor: "" }));
    if (href.startsWith("/api/v1/targets/")) return Promise.resolve(json(targetBody));
    if (href.startsWith("/api/v1/checks")) return Promise.resolve(json({ definitions: [], nextCursor: "" }));
    if (href.startsWith("/api/v1/schedules")) return Promise.resolve(json({ schedules: [], nextCursor: "" }));
    if (href.startsWith("/api/v1/runs")) return Promise.resolve(json({ runs: [], nextCursor: "" }));
    if (href.includes("/api/v1/promql/query_range")) {
      // A non-empty series so the chart actually mounts and can be asked which
      // windows it was handed; an empty one takes the "no series" branch.
      return Promise.resolve(
        json({
          status: "success",
          data: { resultType: "matrix", result: [{ metric: { protocol: "tcp" }, values: [[1785283200, "0.01"]] }] },
        }),
      );
    }
    if (href.includes("/api/v1/promql")) {
      return Promise.resolve(json({ status: "success", data: { resultType: "vector", result: [] } }));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  return { fetchMock, maintenanceQueries, createBodies, deleted };
}

function renderAt(pathname: string, node: React.ReactNode) {
  window.history.pushState({}, "", pathname);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TimeMachineProvider>{node}</TimeMachineProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

/** The scopes a surface asked for, as WIRE STATES rather than values: "absent"
 *  is a different REQUEST from "=" (present-but-empty). */
async function scopesAsked(queries: URLSearchParams[]): Promise<string[]> {
  await waitFor(() => expect(queries.length).toBeGreaterThanOrEqual(2));
  return [...new Set(queries.map((qs) => (qs.has("scope") ? `=${qs.get("scope")}` : "absent")))].sort();
}

function spanSecondsOf(qs: URLSearchParams): number {
  return (Date.parse(qs.get("to") ?? "") - Date.parse(qs.get("from") ?? "")) / 1000;
}

beforeEach(() => {
  FakeSocket.reset();
  vi.stubGlobal("WebSocket", FakeSocket);
});

afterEach(() => {
  cleanup();
  resetWsClient();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
});

describe("PairCardPage maintenance", () => {
  it("asks for the pair scope — the U+2192 arrow — and the global one", async () => {
    const { maintenanceQueries } = stubFetch();
    renderAt("/pairs/node-a/node-b", <PairCardPage />);
    expect(await scopesAsked(maintenanceQueries)).toEqual(["=", "=node-a→node-b"]);
  });

  it("bounds the fetch to the chart's own hour", async () => {
    const { maintenanceQueries } = stubFetch();
    renderAt("/pairs/node-a/node-b", <PairCardPage />);
    await waitFor(() => expect(maintenanceQueries.length).toBeGreaterThan(0));
    expect(spanSecondsOf(maintenanceQueries[0])).toBe(3600);
  });

  it("hands the bands to the chart as an overlay of their own", async () => {
    stubFetch({ byScope: { "node-a→node-b": [win({ id: "p", scope: "node-a→node-b", reason: "link maintenance" })] } });
    renderAt("/pairs/node-a/node-b", <PairCardPage />);
    await waitFor(() => expect(screen.getByTestId("echart").getAttribute("data-maintenance")).toBe("p"));
  });

  it("declares a window against the pair scope, fixed", async () => {
    const { createBodies } = stubFetch();
    renderAt("/pairs/node-a/node-b", <PairCardPage />);
    fireEvent.click(await screen.findByRole("button", { name: "＋ maintenance" }));
    fireEvent.change(await screen.findByLabelText("Reason"), { target: { value: "link maintenance" } });
    fireEvent.click(screen.getByRole("button", { name: "Create maintenance window" }));
    await waitFor(() => expect(createBodies).toHaveLength(1));
    expect((createBodies[0] as { scope: string }).scope).toBe("node-a→node-b");
  });

  it("hides create entirely for a subject with read but no write", async () => {
    stubFetch({ permissions: ["maintenance:read", "targets:read", "checks:read"] });
    renderAt("/pairs/node-a/node-b", <PairCardPage />);
    await screen.findByTestId("maintenance-bar");
    expect(screen.queryByRole("button", { name: "＋ maintenance" })).toBeNull();
  });

  it("keeps create visible but disabled while the Time Machine is engaged", async () => {
    stubFetch();
    renderAt("/pairs/node-a/node-b?at=2026-08-01T09:00:00Z", <PairCardPage />);
    expect(await screen.findByRole("button", { name: "＋ maintenance" })).toBeDisabled();
  });

  it("makes ZERO requests and shows no bar without maintenance:read", async () => {
    const { maintenanceQueries } = stubFetch({ permissions: ["annotations:read", "targets:read", "checks:read"] });
    renderAt("/pairs/node-a/node-b", <PairCardPage />);
    // The annotation bar still mounts, which is the proof the card itself has
    // rendered and "zero" is not "not yet".
    await screen.findByTestId("annotation-bar");
    expect(maintenanceQueries).toEqual([]);
    expect(screen.queryByTestId("maintenance-bar")).toBeNull();
  });

  it("keeps the two overlays as separate bars — a note is not a declared window", async () => {
    stubFetch();
    renderAt("/pairs/node-a/node-b", <PairCardPage />);
    await screen.findByTestId("maintenance-bar");
    expect(screen.getByTestId("annotation-bar")).toBeTruthy();
  });
});

describe("TargetCardPage maintenance", () => {
  it("asks for the target's NAME as its scope, plus the global one", async () => {
    const { maintenanceQueries } = stubFetch();
    renderAt(`/targets/${TARGET_ID}`, <TargetCardPage />);
    fireEvent.click(await screen.findByRole("radio", { name: "History" }));
    expect(await scopesAsked(maintenanceQueries)).toEqual(["=", "=edge-gw"]);
  });

  it("bounds the fetch to the History chart's hour", async () => {
    const { maintenanceQueries } = stubFetch();
    renderAt(`/targets/${TARGET_ID}`, <TargetCardPage />);
    fireEvent.click(await screen.findByRole("radio", { name: "History" }));
    await waitFor(() => expect(maintenanceQueries.length).toBeGreaterThan(0));
    expect(spanSecondsOf(maintenanceQueries[0])).toBe(3600);
  });

  it("hands the bands to the History chart", async () => {
    stubFetch({ byScope: { "edge-gw": [win({ id: "t", scope: "edge-gw", reason: "provider window" })] } });
    renderAt(`/targets/${TARGET_ID}`, <TargetCardPage />);
    fireEvent.click(await screen.findByRole("radio", { name: "History" }));
    await waitFor(() => expect(screen.getByTestId("echart").getAttribute("data-maintenance")).toBe("t"));
  });

  it("declares a window against the target name, fixed, and lists it for deletion", async () => {
    const { createBodies, deleted } = stubFetch({
      byScope: { "edge-gw": [win({ id: "doomed", scope: "edge-gw", reason: "wrong day" })] },
    });
    renderAt(`/targets/${TARGET_ID}`, <TargetCardPage />);
    fireEvent.click(await screen.findByRole("radio", { name: "History" }));

    fireEvent.click(await screen.findByRole("button", { name: "＋ maintenance" }));
    fireEvent.change(await screen.findByLabelText("Reason"), { target: { value: "provider window" } });
    fireEvent.click(screen.getByRole("button", { name: "Create maintenance window" }));
    await waitFor(() => expect(createBodies).toHaveLength(1));
    expect((createBodies[0] as { scope: string }).scope).toBe("edge-gw");

    fireEvent.click(await screen.findByRole("button", { name: "Delete maintenance window: wrong day" }));
    await waitFor(() => expect(deleted).toEqual(["doomed"]));
  });
});
