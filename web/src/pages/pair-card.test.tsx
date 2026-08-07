import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { resetNavigateForTest, setNavigateForTest } from "@/lib/api";
import { FakeSocket } from "@/lib/fake-websocket";
import { findLastRunForPair, PairCardPage, pairFromPath, pairScope, pairSeriesQuery } from "./pair-card";
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
  opts: { permissions?: string[]; runs?: unknown[]; runDetails?: Record<string, RunDetail>; onCreate?: (body: unknown) => Response } = {},
) {
  const { permissions = ["runs:create"], runs = [], runDetails = {}, onCreate } = opts;
  window.history.pushState({}, "", pathname);
  const createCalls: unknown[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    if (href.includes("/api/v1/version")) return Promise.resolve(json({ version: "1.6.0", commit: "x", capabilities: [] }));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody()));
    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.includes("/api/v1/matrix")) return Promise.resolve(json(matrixBody));
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
      results: [{ sourceNode: "node-a", destinationNode: "node-b", success: true, durationNs: 5, recordedAt: "t1" }],
    });
    const newer = runDetail("run-2", {
      createdAt: "2026-08-02T00:00:00Z",
      results: [{ sourceNode: "node-a", destinationNode: "node-b", success: false, durationNs: 9, error: "timeout", recordedAt: "t2" }],
    });
    // getRuns is newest-first, so the details array arrives newest-first too.
    expect(findLastRunForPair([newer, older], "node-a", "node-b")).toEqual({ run: newer, result: newer.results[0] });
  });

  it("does not match the reverse direction", () => {
    const run = runDetail("run-1", {
      results: [{ sourceNode: "node-b", destinationNode: "node-a", success: true, durationNs: 5, recordedAt: "t" }],
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
