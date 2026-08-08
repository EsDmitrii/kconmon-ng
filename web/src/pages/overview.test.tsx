import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { OverviewPage, fmtAge, summarize } from "./overview";
import type { Matrix, Topology } from "@/lib/types";

const matrix: Matrix = {
  protocol: "tcp",
  plane: "pod",
  nodes: ["a", "b", "c"],
  cells: [
    { source: "a", destination: "b", failRatio: null }, // unmeasured — excluded
    { source: "a", destination: "c", failRatio: 0.005 }, // healthy
    { source: "b", destination: "a", failRatio: 0.02 }, // degraded
    { source: "b", destination: "c", failRatio: 0.15, rttP95: 3_000_000 }, // failing
    { source: "c", destination: "a", failRatio: 0.5, rttP95: 9_000_000 }, // failing
  ],
  timestamp: "2026-01-01T00:00:00Z",
};

const topo: Topology = {
  nodes: [
    { name: "a", zone: "z1", ready: true },
    { name: "b", zone: "z1", ready: false },
  ],
  agents: [],
  timestamp: "2026-01-01T00:00:00Z",
};

describe("summarize", () => {
  it("counts failing/degraded/total, excluding null failRatio", () => {
    const s = summarize(matrix, topo);
    expect(s.pairsTotal).toBe(4); // the null cell is excluded
    expect(s.pairsFailing).toBe(2); // 0.15, 0.5
    expect(s.pairsDegraded).toBe(1); // 0.02
  });

  it("returns the top 5 problem pairs ordered by failRatio desc", () => {
    const many: Matrix = {
      ...matrix,
      cells: [0.5, 0.4, 0.3, 0.2, 0.15, 0.12, 0.02].map((r, i) => ({
        source: `s${i}`,
        destination: `d${i}`,
        failRatio: r,
      })),
    };
    const s = summarize(many);
    expect(s.worstPairs).toHaveLength(5);
    expect(s.worstPairs.map((c) => c.failRatio)).toEqual([0.5, 0.4, 0.3, 0.2, 0.15]);
  });

  it("falls back to matrix.nodes when topology is absent", () => {
    const s = summarize(matrix);
    expect(s.totalNodes).toBe(3);
    expect(s.readyNodes).toBe(3);
  });

  it("prefers topology node counts when present", () => {
    const s = summarize(matrix, topo);
    expect(s.totalNodes).toBe(2);
    expect(s.readyNodes).toBe(1);
  });
});

afterEach(() => vi.unstubAllGlobals());

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <OverviewPage />
    </QueryClientProvider>,
  );
}

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

describe("OverviewPage", () => {
  it("shows the ready-nodes tile as readyNodes/totalNodes from the API", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) =>
        Promise.resolve(String(url).includes("/topology") ? json(topo) : json(matrix)),
      ),
    );
    renderPage();
    expect(await screen.findByText("1/2")).toBeInTheDocument();
    expect(screen.getByText("Nodes ready")).toBeInTheDocument();
  });
});

/* ── M6: the two panels that replaced the placeholders (Decision 9) ─────── */

function meBody(permissions: string[]) {
  return { subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["viewer"] }, permissions };
}

function configBody(databaseConfigured: boolean) {
  return {
    auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
    anonymousBanner: false,
    controller: { configured: true },
    prometheus: { configured: true },
    database: { configured: databaseConfigured },
  };
}

const ALL_READS = ["incidents:read", "events:read"];

function incidentRow(over: Record<string, unknown> = {}) {
  return {
    id: "inc-1",
    title: "Loss between node-a and node-b",
    scope: "node-a→node-b",
    fromAt: "2026-01-01T00:00:00Z",
    status: "open",
    notes: "",
    pinned: [],
    createdBy: "user:ada",
    createdAt: "2026-01-01T00:00:00Z",
    ...over,
  };
}

function eventRow(over: Record<string, unknown> = {}) {
  return {
    id: "e-1",
    seq: 1,
    type: "topology_changed",
    severity: "warn",
    scope: "node-a→node-b",
    timestamp: "2026-01-01T00:05:00Z",
    summary: "node-b NotReady",
    details: null,
    ...over,
  };
}

interface PanelOptions {
  permissions?: string[];
  database?: boolean;
  incidents?: unknown[];
  events?: unknown[];
}

function renderOverview(opts: PanelOptions = {}) {
  const { permissions = ALL_READS, database = true, incidents = [], events = [] } = opts;
  const urls: string[] = [];
  const fetchMock = vi.fn((url: string) => {
    const href = String(url);
    urls.push(href);
    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody(database)));
    if (href.includes("/api/v1/topology")) return Promise.resolve(json(topo));
    if (href.startsWith("/api/v1/incidents")) return Promise.resolve(json({ incidents, nextCursor: "" }));
    if (href.startsWith("/api/v1/events")) return Promise.resolve(json({ events, nextCursor: "" }));
    return Promise.resolve(json(matrix));
  });
  vi.stubGlobal("fetch", fetchMock);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <OverviewPage />
    </QueryClientProvider>,
  );
  return { ...utils, urls, fetchMock };
}

describe("OverviewPage — Open incidents (Decision 9)", () => {
  it("lists the open incidents newest-first, each one a permalink into incident mode", async () => {
    renderOverview({ incidents: [incidentRow(), incidentRow({ id: "inc-2", title: "Target flapping", scope: "" })] });

    const panel = await screen.findByRole("region", { name: "Open incidents" });
    const rows = await within(panel).findAllByTestId("open-incident");
    expect(rows).toHaveLength(2);
    expect(within(rows[0]).getByRole("link", { name: "Loss between node-a and node-b" }).getAttribute("href")).toBe(
      "/investigate?incident=inc-1",
    );
    // "" is the GLOBAL scope, and the chip says so rather than showing nothing.
    expect(within(rows[1]).getByText("global")).toBeInTheDocument();
  });

  it("asks for exactly the open ones, five of them", async () => {
    const { urls } = renderOverview({ incidents: [incidentRow()] });
    await screen.findByTestId("open-incident");

    const call = urls.find((u) => u.startsWith("/api/v1/incidents"));
    expect(call).toContain("status=open");
    expect(call).toContain("limit=5");
  });

  it("says there are none rather than rendering an empty box", async () => {
    renderOverview({ incidents: [] });
    expect(await screen.findByText(/no open incidents/i)).toBeInTheDocument();
  });

  it("without incidents:read: one muted line and ZERO requests", async () => {
    const { urls } = renderOverview({ permissions: ["events:read"] });

    expect(await screen.findByText(/open incidents need incidents:read/i)).toBeInTheDocument();
    await waitFor(() => expect(urls.some((u) => u.startsWith("/api/v1/events"))).toBe(true));
    expect(urls.some((u) => u.startsWith("/api/v1/incidents"))).toBe(false);
  });

  it("without a database: the one-line database note and ZERO requests", async () => {
    const { urls } = renderOverview({ database: false });

    expect((await screen.findAllByText(/set console\.database\.mode/i)).length).toBeGreaterThan(0);
    await waitFor(() => expect(urls.some((u) => u.includes("/api/v1/config"))).toBe(true));
    expect(urls.some((u) => u.startsWith("/api/v1/incidents"))).toBe(false);
  });
});

describe("OverviewPage — Recent events (Decision 9)", () => {
  it("renders the newest ten in the Live feed's own vocabulary, and links to Live", async () => {
    renderOverview({ events: [eventRow(), eventRow({ id: "e-2", summary: "agent restarted", severity: "info" })] });

    const panel = await screen.findByRole("region", { name: "Recent events" });
    const rows = await within(panel).findAllByTestId("overview-event");
    expect(rows).toHaveLength(2);
    expect(within(rows[0]).getByText("node-b NotReady")).toBeInTheDocument();
    expect(within(rows[0]).getByText("warn")).toBeInTheDocument();
    expect(within(panel).getByRole("link", { name: /open Live/i }).getAttribute("href")).toBe("/live");
  });

  it("asks for ten", async () => {
    const { urls } = renderOverview({ events: [eventRow()] });
    await screen.findByTestId("overview-event");
    expect(urls.find((u) => u.startsWith("/api/v1/events"))).toContain("limit=10");
  });

  it("without events:read: one muted line and ZERO requests", async () => {
    const { urls } = renderOverview({ permissions: ["incidents:read"] });

    expect(await screen.findByText(/fleet events need events:read/i)).toBeInTheDocument();
    await waitFor(() => expect(urls.some((u) => u.startsWith("/api/v1/incidents"))).toBe(true));
    expect(urls.some((u) => u.startsWith("/api/v1/events"))).toBe(false);
  });

  it("says nothing has happened rather than looking broken", async () => {
    renderOverview({ events: [] });
    expect(await screen.findByText(/nothing has happened yet/i)).toBeInTheDocument();
  });
});

describe("OverviewPage — the alerts placeholder is untouched (M7)", () => {
  it("still says firing alerts arrive with a later milestone", async () => {
    renderOverview();
    expect(await screen.findByText("Firing alerts")).toBeInTheDocument();
    expect(screen.getByText(/arrives with a later milestone \(alertmanager wiring\)/i)).toBeInTheDocument();
  });
});

describe("fmtAge", () => {
  it("coarsens: seconds, minutes, hours, then days", () => {
    const now = new Date("2026-01-02T00:00:00Z");
    expect(fmtAge("2026-01-01T23:59:30Z", now)).toBe("30s");
    expect(fmtAge("2026-01-01T23:30:00Z", now)).toBe("30m");
    expect(fmtAge("2026-01-01T00:00:00Z", now)).toBe("24h");
    expect(fmtAge("2025-12-28T00:00:00Z", now)).toBe("5d");
  });

  it("is a dash for an unparseable instant, never NaN", () => {
    expect(fmtAge("never", new Date())).toBe("—");
  });
});
