import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import { NodeCardPage, nodeHealth, nodeNameFromPath } from "./node-card";

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

function configBody(databaseConfigured = true) {
  return {
    auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
    anonymousBanner: false,
    controller: { configured: true },
    prometheus: { configured: true },
    database: { configured: databaseConfigured },
  };
}

const topologyBody = {
  nodes: [
    { name: "node-a", zone: "z1", ready: true },
    { name: "node-b", zone: "z2", ready: true },
  ],
  agents: [{ id: "agent-1", nodeName: "node-a", podIP: "10.0.0.1", zone: "z1" }],
  timestamp: "t",
};

const matrixBody = {
  protocol: "tcp",
  plane: "pod",
  nodes: ["node-a", "node-b"],
  cells: [
    { source: "node-a", destination: "node-b", failRatio: 0.02, rttP95: 1_500_000 },
    { source: "node-b", destination: "node-a", failRatio: 0 },
  ],
  timestamp: "t",
};

function renderPage(pathname = "/nodes/node-a", opts: { runs?: unknown[] } = {}) {
  window.history.pushState({}, "", pathname);
  const fetchMock = vi.fn((url: string) => {
    const href = String(url);
    if (href.includes("/api/v1/version")) return Promise.resolve(json({ version: "1.6.0", commit: "x", capabilities: [] }));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody()));
    if (href.includes("/api/v1/topology")) return Promise.resolve(json(topologyBody));
    if (href.includes("/api/v1/matrix")) return Promise.resolve(json(matrixBody));
    if (href.startsWith("/api/v1/events")) return Promise.resolve(json({ events: [], nextCursor: "" }));
    if (href.startsWith("/api/v1/runs")) return Promise.resolve(json({ runs: opts.runs ?? [], nextCursor: "" }));
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <NodeCardPage />
    </QueryClientProvider>,
  );
  return { ...utils, fetchMock, qc };
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

describe("nodeNameFromPath", () => {
  it("extracts the node name after the prefix", () => {
    expect(nodeNameFromPath("/nodes/node-a")).toBe("node-a");
  });

  it("round-trips a node name that needs URL encoding", () => {
    const name = "weird node/name äöü→x";
    const encoded = `/nodes/${encodeURIComponent(name)}`;
    expect(nodeNameFromPath(encoded)).toBe(name);
  });

  it("returns empty for a path with no node segment", () => {
    expect(nodeNameFromPath("/nodes/")).toBe("");
    expect(nodeNameFromPath("/topology")).toBe("");
  });
});

describe("nodeHealth", () => {
  const cells = [
    { source: "node-a", destination: "node-b", failRatio: 0.02 },
    { source: "node-a", destination: "node-c", failRatio: 0 },
  ];

  it("uses the worst outbound fail ratio, excluding self-cells", () => {
    const h = nodeHealth(true, cells, "node-a");
    expect(h.tier).toBe("warn");
    expect(h.percent).toBeCloseTo(98, 5);
  });

  it("overrides to bad when the node is not ready, regardless of ratio", () => {
    expect(nodeHealth(false, [{ source: "node-a", destination: "node-b", failRatio: 0 }], "node-a").tier).toBe("bad");
  });

  it("reads unknown with no outbound data", () => {
    expect(nodeHealth(true, [], "node-a")).toEqual({ percent: null, tier: "unknown" });
  });
});

describe("NodeCardPage", () => {
  it("renders identity and health from stubbed topology + matrix", async () => {
    renderPage("/nodes/node-a");

    // Identity: title is the node name, zone from topology, agent from topology.
    expect(screen.getByRole("heading", { name: "node-a", level: 1 })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("Zone z1")).toBeInTheDocument());
    expect(screen.getByText("agent-1")).toBeInTheDocument();
    expect(screen.getByText("10.0.0.1")).toBeInTheDocument();

    // Health: worst outbound fail ratio for node-a is 2% -> Degraded, 98.0% healthy.
    expect(screen.getByText("Degraded")).toBeInTheDocument();
    expect(screen.getByText("98.0% healthy")).toBeInTheDocument();

    // Per-destination breakdown row.
    expect(screen.getByText("node-b")).toBeInTheDocument();
    expect(screen.getByText("2.0%")).toBeInTheDocument();
  });

  it("requests the recent-changes rail scoped to exactly the node name", async () => {
    const { fetchMock } = renderPage("/nodes/node-a");
    await waitFor(() =>
      expect(fetchMock.mock.calls.some((c) => String(c[0]).startsWith("/api/v1/events"))).toBe(true),
    );
    const call = fetchMock.mock.calls.find((c) => String(c[0]).startsWith("/api/v1/events"));
    const url = new URL(String(call?.[0]), "http://localhost");
    expect(url.searchParams.get("scope")).toBe("node-a");
  });
});
