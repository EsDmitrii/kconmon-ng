import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { resetNavigateForTest, setNavigateForTest } from "@/lib/api";
import { FakeSocket } from "@/lib/fake-websocket";
import { parseInvestigationParams, scopeFilterValue } from "@/lib/investigation-sources";
import {
  findLastRunForPair,
  knownNodes,
  PairCardPage,
  pairFromPath,
  pairScope,
  pairSeriesQuery,
  unknownPairEndpoints,
} from "./pair-card";
import type { RunDetail } from "@/lib/types";

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

function meBody(permissions: string[]) {
  return {
    subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["operator"] },
    permissions,
  };
}

const matrixBody = {
  protocol: "tcp",
  plane: "pod",
  nodes: ["node-a", "node-b"],
  cells: [
    { source: "node-a", destination: "node-b", failRatio: 0.02, rttP95: 1_500_000 },
    { source: "node-b", destination: "node-a", failRatio: 0.5, rttP95: 3_000_000 },
  ],
  timestamp: "t",
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

function renderPage(
  pathname = "/pairs/node-a/node-b",
  opts: {
    permissions?: string[];
    runs?: unknown[];
    runDetails?: Record<string, RunDetail>;
    onCreate?: (body: unknown) => Response;
    incidents?: unknown[];
    topology?: unknown;
  } = {},
) {
  const { permissions = ["runs:create"], runs = [], runDetails = {}, onCreate, incidents = [] } = opts;
  window.history.pushState({}, "", pathname);
  const createCalls: unknown[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    if (href.includes("/api/v1/version")) return Promise.resolve(json({ version: "1.6.0", commit: "x", capabilities: [] }));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody()));
    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.includes("/api/v1/matrix")) return Promise.resolve(json(matrixBody));
    if (href.includes("/api/v1/topology")) {
      return Promise.resolve(
        json(
          opts.topology ?? {
            nodes: [
              { name: "node-a", zone: "z1", ready: true },
              { name: "node-b", zone: "z2", ready: true },
            ],
            agents: [],
            timestamp: "t",
          },
        ),
      );
    }
    if (href.startsWith("/api/v1/incidents")) return Promise.resolve(json({ incidents, nextCursor: "" }));
    if (href.startsWith("/api/v1/events")) return Promise.resolve(json({ events: [], nextCursor: "" }));
    if (href === "/api/v1/runs" && method === "POST") {
      const body: unknown = JSON.parse(String(init?.body ?? "{}"));
      createCalls.push(body);
      if (onCreate) return Promise.resolve(onCreate(body));
      return Promise.resolve(json({ id: "run-xyz", status: "pending", pairTotal: 1, wsTopic: "run:run-xyz" }, { status: 202 }));
    }
    if (href.startsWith("/api/v1/runs/")) {
      const id = href.slice("/api/v1/runs/".length);
      const detail = runDetails[id];
      if (detail) return Promise.resolve(json(detail));
      return Promise.resolve(json(runDetail(id)));
    }
    if (href.startsWith("/api/v1/runs")) return Promise.resolve(json({ runs, nextCursor: "" }));
    if (href.includes("/api/v1/promql/query_range")) return Promise.resolve(json({ status: "success", data: { resultType: "matrix", result: [] } }));
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  const navigateSpy = vi.fn();
  setNavigateForTest(navigateSpy);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <PairCardPage />
      </ThemeProvider>
    </QueryClientProvider>,
  );
  return { ...utils, fetchMock, createCalls, qc, navigateSpy };
}

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

describe("pairFromPath", () => {
  it("splits source and destination on the first slash after the prefix", () => {
    expect(pairFromPath("/pairs/node-a/node-b")).toEqual({ source: "node-a", destination: "node-b" });
  });

  it("round-trips names that need URL encoding, including ones containing a literal slash", () => {
    const source = "ns/pod-1";
    const destination = "weird äöü→name";
    const encoded = `/pairs/${encodeURIComponent(source)}/${encodeURIComponent(destination)}`;
    expect(pairFromPath(encoded)).toEqual({ source, destination });
  });

  it("returns empty strings for a malformed path", () => {
    expect(pairFromPath("/pairs/only-one-segment")).toEqual({ source: "", destination: "" });
  });
});

describe("pairScope", () => {
  it("uses U+2192, not a hyphen-arrow", () => {
    expect(pairScope("node-a", "node-b")).toBe("node-a→node-b");
    expect(pairScope("node-a", "node-b")).not.toBe("node-a->node-b");
  });
});

describe("pairSeriesQuery", () => {
  it("references only allowed metric names and both peer labels", () => {
    const q = pairSeriesQuery("node-a", "node-b");
    expect(q).toContain("kconmon_ng_tcp_total_duration_seconds_bucket");
    expect(q).toContain("kconmon_ng_udp_rtt_seconds_bucket");
    expect(q).toContain("kconmon_ng_icmp_rtt_seconds_bucket");
    expect(q).toContain('source_node="node-a"');
    expect(q).toContain('destination_node="node-b"');
  });

  it("escapes quotes and backslashes in node names", () => {
    const q = pairSeriesQuery('a"b', "c\\d");
    expect(q).toContain('source_node="a\\"b"');
    expect(q).toContain('destination_node="c\\\\d"');
  });
});

describe("findLastRunForPair", () => {
  it("returns the newest matching run and its result row", () => {
    const older = runDetail("run-1", {
      createdAt: "2026-08-01T00:00:00Z",
      results: [{ sourceNode: "node-a", destinationNode: "node-b", success: true, durationNs: 5, recordedAt: "t1", sampleSeq: 0 }],
    });
    const newer = runDetail("run-2", {
      createdAt: "2026-08-02T00:00:00Z",
      results: [{ sourceNode: "node-a", destinationNode: "node-b", success: false, durationNs: 9, error: "timeout", recordedAt: "t2", sampleSeq: 0 }],
    });
    // getRuns is newest-first, so the details array arrives newest-first too.
    expect(findLastRunForPair([newer, older], "node-a", "node-b")).toEqual({ run: newer, result: newer.results[0] });
  });

  it("does not match the reverse direction", () => {
    const run = runDetail("run-1", {
      results: [{ sourceNode: "node-b", destinationNode: "node-a", success: true, durationNs: 5, recordedAt: "t", sampleSeq: 0 }],
    });
    expect(findLastRunForPair([run], "node-a", "node-b")).toBeUndefined();
  });
});

describe("PairCardPage", () => {
  it("renders both directions' fail ratios from the matrix", async () => {
    renderPage("/pairs/node-a/node-b");
    await waitFor(() => expect(screen.getByText("2.0%")).toBeInTheDocument());
    expect(screen.getByText("50.0%")).toBeInTheDocument();
  });

  it("requests the recent-changes rail scoped to the exact pair string", async () => {
    const { fetchMock } = renderPage("/pairs/node-a/node-b");
    await waitFor(() =>
      expect(fetchMock.mock.calls.some((c) => String(c[0]).startsWith("/api/v1/events"))).toBe(true),
    );
    const call = fetchMock.mock.calls.find((c) => String(c[0]).startsWith("/api/v1/events"));
    const url = new URL(String(call?.[0]), "http://localhost");
    expect(url.searchParams.get("scope")).toBe("node-a→node-b");
  });

  it('"Run check" posts a one-pair run and navigates to its permalink', async () => {
    const { createCalls, navigateSpy } = renderPage("/pairs/node-a/node-b", { permissions: ["runs:create"] });
    fireEvent.click(await screen.findByRole("radio", { name: "Diagnostics" }));
    const button = await screen.findByRole("button", { name: "Run check" });
    fireEvent.click(button);

    await waitFor(() => expect(navigateSpy).toHaveBeenCalledWith("/diagnostics/runs/run-xyz"));
    expect(createCalls).toEqual([{ type: "tcp", plane: "pod", sources: ["node-a"], destinations: ["node-b"] }]);
  });

  it("hides the Run check action without runs:create", async () => {
    renderPage("/pairs/node-a/node-b", { permissions: [] });
    fireEvent.click(await screen.findByRole("radio", { name: "Diagnostics" }));
    await screen.findByText("Last run for this pair");
    expect(screen.queryByRole("button", { name: "Run check" })).not.toBeInTheDocument();
  });
});

/* ── M6 Task 8: the Investigate entry point + the related-incidents rail ── */

/* ── QA scope 2, finding #7: two endpoints, validated ────────────────────── */

const TOPO = {
  nodes: [{ name: "node-a", zone: "z1", ready: true }],
  agents: [{ id: "a-1", nodeName: "node-agent", podIP: "10.0.0.1", zone: "z1" }],
  timestamp: "t",
};

describe("knownNodes", () => {
  it("unions the Kubernetes nodes, the agents' own nodes, the matrix list and both ends of a cell", () => {
    const known = knownNodes(TOPO, matrixBody);
    expect([...(known ?? [])].sort()).toEqual(["node-a", "node-agent", "node-b"]);
  });

  it("counts an agent's node even off-cluster, where `nodes` is the empty half", () => {
    expect(knownNodes({ nodes: [], agents: TOPO.agents, timestamp: "t" }, undefined)).toEqual(
      new Set(["node-agent"]),
    );
  });

  it("answers null while NOBODY has answered — not-yet is not no-such-node", () => {
    expect(knownNodes(undefined, undefined)).toBeNull();
  });
});

describe("unknownPairEndpoints", () => {
  it("names the half the fleet does not report", () => {
    expect(unknownPairEndpoints(new Set(["node-a"]), "node-a", "nope")).toEqual(["nope"]);
  });

  it("names both when neither is known", () => {
    expect(unknownPairEndpoints(new Set(["node-a"]), "x", "y")).toEqual(["x", "y"]);
  });

  it("says nothing for a pair the fleet reports", () => {
    expect(unknownPairEndpoints(new Set(["node-a", "node-b"]), "node-a", "node-b")).toEqual([]);
  });

  it("judges nothing without an inventory to judge against", () => {
    expect(unknownPairEndpoints(null, "node-a", "nope")).toEqual([]);
    expect(unknownPairEndpoints(new Set(), "node-a", "nope")).toEqual([]);
  });
});

describe("PairCardPage — an endpoint the fleet does not report", () => {
  it("renders the not-found state instead of a working card", async () => {
    renderPage("/pairs/node-a/there-is-no-such-node");
    expect(await screen.findByRole("heading", { name: "No such pair", level: 1 })).toBeInTheDocument();
    expect(screen.getByText(/no node called “there-is-no-such-node”/)).toBeInTheDocument();
  });

  it("offers NO writes on it — the whole point of the finding", async () => {
    renderPage("/pairs/node-a/there-is-no-such-node", {
      permissions: ["runs:create", "annotations:write", "maintenance:read", "maintenance:write"],
    });
    await screen.findByRole("heading", { name: "No such pair", level: 1 });
    expect(screen.queryByTestId("maintenance-bar")).toBeNull();
    expect(screen.queryByRole("button", { name: /annotate/i })).toBeNull();
    expect(screen.queryByRole("button", { name: "Run check" })).toBeNull();
  });

  it("still renders the card for a pair only the AGENTS know about", async () => {
    renderPage("/pairs/node-a/node-agent", { topology: TOPO });
    expect(await screen.findByRole("heading", { name: "node-a → node-agent" })).toBeInTheDocument();
  });
});

describe("PairCardPage — Investigate entry point", () => {
  it("links to a PAIR investigation joined by the same separator pairScope uses", async () => {
    renderPage("/pairs/node-a/node-b", { permissions: [] });
    const href = (await screen.findByRole("link", { name: "Investigate" })).getAttribute("href") ?? "";

    const p = parseInvestigationParams(href.slice(href.indexOf("?")), new Date());
    expect(p.kind).toBe("pair");
    expect(p.a).toBe("node-a");
    expect(p.b).toBe("node-b");
    // The very string the events feed and an incident's scope are matched on.
    expect(scopeFilterValue({ kind: p.kind, a: p.a, b: p.b })).toBe(pairScope("node-a", "node-b"));
  });
});

describe("PairCardPage — open incidents rail", () => {
  it("filters the open list to this pair's own scope string", async () => {
    const row = (over: Record<string, unknown>) => ({
      id: "inc-1",
      title: "loss on the pair",
      scope: pairScope("node-a", "node-b"),
      fromAt: "2026-01-01T00:00:00Z",
      status: "open",
      notes: "",
      pinned: [],
      createdBy: "user:ada",
      createdAt: "2026-01-01T00:00:00Z",
      ...over,
    });
    renderPage("/pairs/node-a/node-b", {
      permissions: ["incidents:read"],
      incidents: [row({}), row({ id: "inc-2", title: "the reverse direction", scope: pairScope("node-b", "node-a") })],
    });

    const rail = await screen.findByRole("complementary", { name: "Open incidents" });
    const rows = await within(rail).findAllByTestId("related-incident");
    expect(rows).toHaveLength(1);
    expect(within(rows[0]).getByRole("link", { name: "loss on the pair" })).toBeInTheDocument();
  });
});
