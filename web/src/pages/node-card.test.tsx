import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import { parseInvestigationParams } from "@/lib/investigation-sources";
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

function renderPage(
  pathname = "/nodes/node-a",
  opts: {
    runs?: unknown[];
    permissions?: string[];
    incidents?: unknown[];
    topology?: unknown;
    matrix?: unknown;
  } = {},
) {
  window.history.pushState({}, "", pathname);
  const fetchMock = vi.fn((url: string) => {
    const href = String(url);
    if (href.includes("/api/v1/version")) return Promise.resolve(json({ version: "1.6.0", commit: "x", capabilities: [] }));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody()));
    if (href.includes("/api/v1/auth/me")) {
      return Promise.resolve(
        json({ subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: [] }, permissions: opts.permissions ?? [] }),
      );
    }
    if (href.includes("/api/v1/topology")) return Promise.resolve(json(opts.topology ?? topologyBody));
    if (href.includes("/api/v1/matrix")) return Promise.resolve(json(opts.matrix ?? matrixBody));
    if (href.startsWith("/api/v1/maintenance")) return Promise.resolve(json({ windows: [], nextCursor: "" }));
    if (href.startsWith("/api/v1/annotations")) return Promise.resolve(json({ annotations: [], nextCursor: "" }));
    if (href.startsWith("/api/v1/incidents")) return Promise.resolve(json({ incidents: opts.incidents ?? [], nextCursor: "" }));
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
    expect(nodeHealth(true, [], "node-a")).toEqual({ percent: null, tier: "unknown", scored: 0, total: 0 });
  });

  /* QA round 2, finding #1: a node whose pairs have simply never failed. */
  it("does NOT read a measured-but-unscored node as No data", () => {
    const measured = [{ source: "node-a", destination: "node-b", failRatio: null, rttP95: 2_200_000 }];
    expect(nodeHealth(true, measured, "node-a")).toEqual({ percent: null, tier: "ok", scored: 0, total: 1 });
  });

  it("takes the tier from packet loss when no failure ratio was reported", () => {
    const lossy = [{ source: "node-a", destination: "node-b", failRatio: null, rttP95: 1e6, lossRatio: 0.2 }];
    const h = nodeHealth(true, lossy, "node-a");
    expect(h.tier).toBe("bad");
    expect(h.percent).toBeCloseTo(80, 5);
  });

  it("still reads unknown when nothing measured the node at all", () => {
    const silent = [{ source: "node-a", destination: "node-b", failRatio: null }];
    expect(nodeHealth(true, silent, "node-a")).toEqual({ percent: null, tier: "unknown", scored: 0, total: 1 });
  });

  /* QA scope 2, finding #3 — the figure and the evidence behind it. */
  it("reports how many of the node's pairs actually produced the figure", () => {
    const nine = Array.from({ length: 9 }, (_, i) => ({
      source: "node-a",
      destination: `node-${i}`,
      failRatio: i === 0 ? 0 : null,
      ...(i === 0 ? { rttP95: 1e6 } : {}),
    }));
    const h = nodeHealth(true, nine, "node-a");
    expect(h.percent).toBeCloseTo(100, 5);
    expect(h.scored).toBe(1);
    expect(h.total).toBe(9);
  });

  it("counts a self-cell in neither half", () => {
    const withSelf = [
      { source: "node-a", destination: "node-a", failRatio: 0 },
      { source: "node-a", destination: "node-b", failRatio: 0.02 },
    ];
    expect(nodeHealth(true, withSelf, "node-a")).toMatchObject({ scored: 1, total: 1 });
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

  // QA scope 2 #21: `scope=node-a` is an EQUALITY filter.
  it("requests the recent-changes rail with the pair-aware scopeNode filter", async () => {
    const { fetchMock } = renderPage("/nodes/node-a");
    await waitFor(() =>
      expect(fetchMock.mock.calls.some((c) => String(c[0]).startsWith("/api/v1/events"))).toBe(true),
    );
    const call = fetchMock.mock.calls.find((c) => String(c[0]).startsWith("/api/v1/events"));
    const url = new URL(String(call?.[0]), "http://localhost");
    expect(url.searchParams.get("scopeNode")).toBe("node-a");
    expect(url.searchParams.get("scope")).toBeNull();
  });
});

/* ── QA scope 2, findings #3–#6, #14, #18, #21 ───────────────────────────── */

describe("NodeCardPage — the header figure and the evidence under it", () => {
  it("states the denominator when the figure rests on part of the pairs", async () => {
    renderPage("/nodes/node-a");
    // matrixBody: node-a has ONE outbound cell, and it is scored — so the
    // figure covers everything there is and needs no qualifier.
    await waitFor(() => expect(screen.getByTestId("node-health-percent")).toHaveTextContent("98.0% healthy"));
    expect(screen.getByTestId("node-health-percent")).not.toHaveTextContent("scored");
  });

  it("qualifies the figure when most of the node's pairs produced no ratio", async () => {
    const partial = {
      protocol: "tcp",
      plane: "pod",
      nodes: ["node-a", "node-b", "node-c"],
      cells: [
        { source: "node-a", destination: "node-b", failRatio: 0, rttP95: 1e6 },
        // Measured by nothing at all: it counts in the denominator and not in
        // the numerator, which is precisely the gap the header now discloses.
        { source: "node-a", destination: "node-c", failRatio: null },
      ],
      timestamp: "t",
    };
    renderPage("/nodes/node-a", { matrix: partial });
    await waitFor(() =>
      expect(screen.getByTestId("node-health-percent")).toHaveTextContent("100.0% healthy · 1 of 2 pairs scored"),
    );
  });
});

describe("NodeCardPage — the per-destination breakdown", () => {
  it("links every destination row at the pair card, URL-encoding both halves", async () => {
    renderPage("/nodes/node-a");
    const link = await screen.findByRole("link", { name: "node-b" });
    expect(link).toHaveAttribute("href", "/pairs/node-a/node-b");
  });

  it("grows a packet-loss column when the cells carry loss, so the header reconciles", async () => {
    const lossy = {
      protocol: "udp",
      plane: "pod",
      nodes: ["node-a", "node-b"],
      cells: [{ source: "node-a", destination: "node-b", failRatio: null, rttP95: 1e6, lossRatio: 0.2 }],
      timestamp: "t",
    };
    renderPage("/nodes/node-a", { matrix: lossy });
    // The header says Failing off 20% loss; the table now SHOWS that 20%.
    await waitFor(() => expect(screen.getByText("Failing")).toBeInTheDocument());
    expect(screen.getByRole("columnheader", { name: "Packet loss" })).toBeInTheDocument();
    expect(screen.getByText("20.0%")).toBeInTheDocument();
  });

  it("has no loss column at all on a protocol whose cells carry none", async () => {
    renderPage("/nodes/node-a");
    await waitFor(() => expect(screen.getByRole("columnheader", { name: "Fail ratio" })).toBeInTheDocument());
    expect(screen.queryByRole("columnheader", { name: "Packet loss" })).toBeNull();
  });

  it("says 'no fail data' rather than an em-dash for a lazy failure counter", async () => {
    const silentCounter = {
      protocol: "tcp",
      plane: "pod",
      nodes: ["node-a", "node-b"],
      cells: [{ source: "node-a", destination: "node-b", failRatio: null, rttP95: 1e6 }],
      timestamp: "t",
    };
    renderPage("/nodes/node-a", { matrix: silentCounter });
    // The matrix's own two readings, on the card that used one glyph for both.
    expect(await screen.findByText("no fail data")).toBeInTheDocument();
    expect(screen.queryByText("no data")).toBeNull();
  });

  it("keeps 'no data' for a destination nothing measured", async () => {
    const unprobed = {
      protocol: "tcp",
      plane: "pod",
      nodes: ["node-a", "node-b"],
      cells: [{ source: "node-a", destination: "node-b", failRatio: null }],
      timestamp: "t",
    };
    renderPage("/nodes/node-a", { matrix: unprobed });
    expect(await screen.findByText("no data")).toBeInTheDocument();
  });
});

describe("NodeCardPage — an empty podIP", () => {
  it("draws the em-dash for the historical shape's empty string, not a blank cell", async () => {
    const emptyIP = {
      nodes: [{ name: "node-a", zone: "z1", ready: true }],
      agents: [{ id: "agent-1", nodeName: "node-a", podIP: "", zone: "z1" }],
      timestamp: "t",
    };
    renderPage("/nodes/node-a", { topology: emptyIP });
    await waitFor(() => expect(screen.getByText("Pod IP")).toBeInTheDocument());
    const cell = screen.getByText("Pod IP").parentElement?.querySelector("dd");
    expect(cell).toHaveTextContent("—");
  });
});

describe("NodeCardPage — the protocol in the URL", () => {
  afterEach(() => window.history.replaceState({}, "", "/"));

  it("opens on the protocol a shared link named", async () => {
    renderPage("/nodes/node-a?protocol=icmp");
    await waitFor(() => expect(screen.getByRole("radio", { name: "ICMP" })).toBeChecked());
  });

  it("REPLACES the protocol on a switch, leaving one history entry", async () => {
    renderPage("/nodes/node-a");
    await waitFor(() => expect(screen.getByRole("radio", { name: "TCP" })).toBeChecked());
    const before = window.history.length;
    fireEvent.click(screen.getByRole("radio", { name: "UDP" }));
    expect(new URLSearchParams(window.location.search).get("protocol")).toBe("udp");
    expect(window.history.length).toBe(before);
  });

  it("normalises a protocol this console cannot probe", async () => {
    renderPage("/nodes/node-a?protocol=sctp");
    await waitFor(() => expect(new URLSearchParams(window.location.search).get("protocol")).toBe("tcp"));
  });
});

describe("NodeCardPage — maintenance", () => {
  it("mounts the same bar the pair and target cards carry, scoped to this node", async () => {
    const { fetchMock } = renderPage("/nodes/node-a", { permissions: ["maintenance:read"] });
    expect(await screen.findByTestId("maintenance-bar")).toBeInTheDocument();
    const scopes = fetchMock.mock.calls
      .map((c) => String(c[0]))
      .filter((h) => h.startsWith("/api/v1/maintenance"))
      .map((h) => new URL(h, "http://localhost").searchParams.get("scope"));
    expect(scopes).toContain("node-a");
  });

  it("shows no bar at all without maintenance:read", async () => {
    renderPage("/nodes/node-a");
    await waitFor(() => expect(screen.getByText("Annotations")).toBeInTheDocument());
    expect(screen.queryByTestId("maintenance-bar")).toBeNull();
  });
});

/* ── M6 Task 8: the Investigate entry point + the related-incidents rail ── */

const nodeIncident = (over: Record<string, unknown> = {}) => ({
  id: "inc-1",
  title: "node-a keeps flapping",
  scope: "node-a",
  fromAt: "2026-01-01T00:00:00Z",
  status: "open",
  notes: "",
  pinned: [],
  createdBy: "user:ada",
  createdAt: "2026-01-01T00:00:00Z",
  ...over,
});

describe("NodeCardPage — Investigate entry point", () => {
  it("links to an investigation of THIS node that parseInvestigationParams reads back", async () => {
    renderPage("/nodes/node-a");
    const link = await screen.findByRole("link", { name: "Investigate" });
    const href = link.getAttribute("href") ?? "";
    expect(href.startsWith("/investigate?")).toBe(true);

    const p = parseInvestigationParams(href.slice(href.indexOf("?")), new Date());
    expect(p.kind).toBe("node");
    expect(p.a).toBe("node-a");
    expect(p.to.getTime() - p.from.getTime()).toBe(60 * 60 * 1000);
  });

  it("URL-encodes a node name that needs it", async () => {
    const name = "ns/pod a";
    renderPage(`/nodes/${encodeURIComponent(name)}`);
    const links = await screen.findAllByRole("link", { name: "Investigate" });
    const href = links[0].getAttribute("href") ?? "";
    expect(parseInvestigationParams(href.slice(href.indexOf("?")), new Date()).a).toBe(name);
  });
});

describe("NodeCardPage — open incidents rail", () => {
  it("shows only the incidents whose scope IS this node", async () => {
    renderPage("/nodes/node-a", {
      permissions: ["incidents:read"],
      incidents: [nodeIncident(), nodeIncident({ id: "inc-2", title: "somewhere else", scope: "node-b" })],
    });

    const rail = await screen.findByRole("complementary", { name: "Open incidents" });
    const rows = await within(rail).findAllByTestId("related-incident");
    expect(rows).toHaveLength(1);
    expect(within(rows[0]).getByRole("link", { name: "node-a keeps flapping" }).getAttribute("href")).toBe(
      "/investigate?incident=inc-1",
    );
  });

  it("without incidents:read: a muted line and ZERO requests", async () => {
    const { fetchMock } = renderPage("/nodes/node-a", { permissions: [] });

    expect(await screen.findByText(/incidents need incidents:read/i)).toBeInTheDocument();
    await waitFor(() => expect(fetchMock.mock.calls.some((c) => String(c[0]).includes("/api/v1/matrix"))).toBe(true));
    expect(fetchMock.mock.calls.some((c) => String(c[0]).startsWith("/api/v1/incidents"))).toBe(false);
  });

  it("says so when no open incident names this node", async () => {
    renderPage("/nodes/node-a", { permissions: ["incidents:read"], incidents: [nodeIncident({ scope: "node-b" })] });
    expect(await screen.findByText(/no open incident names this object/i)).toBeInTheDocument();
  });
});

/* ── QA round 2, findings #3, #4 and #16: the Agent identity panel ───────── */

/** Renders the card with a topology answer of the caller's choosing — a
 *  problem+json response for the failure cases, a body for the rest. */
function renderWithTopology(topology: { status: number; body: unknown }, pathname = "/nodes/node-a") {
  window.history.pushState({}, "", pathname);
  const fetchMock = vi.fn((url: string) => {
    const href = String(url);
    if (href.includes("/api/v1/version")) return Promise.resolve(json({ version: "1.6.0", commit: "x", capabilities: [] }));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody()));
    if (href.includes("/api/v1/auth/me")) {
      return Promise.resolve(json({ subject: { kind: "user", id: "u1" }, permissions: [] }));
    }
    if (href.includes("/api/v1/topology")) {
      return Promise.resolve(
        new Response(JSON.stringify(topology.body), {
          status: topology.status,
          headers: { "Content-Type": topology.status === 200 ? "application/json" : "application/problem+json" },
        }),
      );
    }
    if (href.includes("/api/v1/matrix")) return Promise.resolve(json(matrixBody));
    if (href.startsWith("/api/v1/incidents")) return Promise.resolve(json({ incidents: [], nextCursor: "" }));
    if (href.startsWith("/api/v1/events")) return Promise.resolve(json({ events: [], nextCursor: "" }));
    if (href.startsWith("/api/v1/runs")) return Promise.resolve(json({ runs: [], nextCursor: "" }));
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <NodeCardPage />
    </QueryClientProvider>,
  );
}

describe("NodeCardPage — identity when the topology query FAILED", () => {
  it("says what the server said instead of drawing four em-dashes", async () => {
    renderWithTopology({
      status: 422,
      body: {
        type: "about:blank",
        title: "instant outside retention",
        status: 422,
        detail: "at is older than console.database.retentionDays (30d)",
      },
    });
    const line = await screen.findByTestId("identity-problem");
    expect(line).toHaveTextContent("console.database.retentionDays");
    expect(screen.queryByText("Zone")).not.toBeInTheDocument();
    expect(screen.queryByText("Pod IP")).not.toBeInTheDocument();
  });

  it("keeps the em-dashes for a SUCCESSFUL topology that simply lacks the node", async () => {
    renderWithTopology({ status: 200, body: { nodes: [], agents: [], timestamp: "t" } }, "/nodes/node-zzz");
    await screen.findByText("Zone");
    expect(screen.queryByTestId("identity-problem")).toBeNull();
    expect(screen.getAllByText("—").length).toBeGreaterThanOrEqual(4);
  });
});

describe("NodeCardPage — zone from a topology with agents but no nodes", () => {
  it("reads the zone off the node's own AGENT entry", async () => {
    renderWithTopology({
      status: 200,
      body: {
        nodes: [],
        agents: [{ id: "agent-1", nodeName: "node-a", podIP: "10.0.0.1", zone: "z9" }],
        timestamp: "t",
      },
    });
    await waitFor(() => expect(screen.getByText("z9")).toBeInTheDocument());
    expect(screen.getByText("Zone z9")).toBeInTheDocument();
  });

  it("leaves readiness an em-dash, and says where that fact would have come from", async () => {
    renderWithTopology({
      status: 200,
      body: {
        nodes: [],
        agents: [{ id: "agent-1", nodeName: "node-a", podIP: "10.0.0.1", zone: "z9" }],
        timestamp: "t",
      },
    });
    await waitFor(() => expect(screen.getByText("z9")).toBeInTheDocument());
    const ready = screen.getByText("Ready").parentElement?.querySelector("dd");
    expect(ready).toHaveTextContent("—");
    expect(ready?.getAttribute("title")).toBe("node readiness comes from the Kubernetes node informer");
  });
});

describe("NodeCardPage — the header percentage", () => {
  it("drops the '— healthy' line entirely when there is no percentage to state", async () => {
    renderWithTopology({
      status: 200,
      body: { nodes: [{ name: "node-zzz", zone: "z1", ready: true }], agents: [], timestamp: "t" },
    }, "/nodes/node-zzz");
    // node-zzz has no matrix cells at all: No data, and nothing else.
    await screen.findByText("No data");
    expect(screen.queryByText(/healthy/)).toBeNull();
  });
});
