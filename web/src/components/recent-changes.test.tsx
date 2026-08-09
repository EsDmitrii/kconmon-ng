import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import type { LiveEvent } from "@/lib/types";
import { matchesScope, RecentChanges } from "./recent-changes";

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

function configBody(databaseConfigured: boolean) {
  return {
    auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
    anonymousBanner: false,
    controller: { configured: true },
    prometheus: { configured: true },
    database: { configured: databaseConfigured },
  };
}

function event(overrides: Partial<LiveEvent> = {}): LiveEvent {
  return {
    id: "1-1000",
    seq: 1,
    type: "check_observed",
    severity: "info",
    scope: "node-a",
    timestamp: "2026-08-06T10:00:00Z",
    summary: "check observed",
    details: null,
    ...overrides,
  };
}

function renderRail(scope: string, opts: { databaseConfigured?: boolean; events?: LiveEvent[] } = {}) {
  return renderRailWith({ scope }, opts);
}

function renderRailWith(
  props: { scope: string; scopeNode?: undefined } | { scope?: undefined; scopeNode: string },
  opts: { databaseConfigured?: boolean; events?: LiveEvent[] } = {},
) {
  const { databaseConfigured = true, events = [] } = opts;
  const fetchMock = vi.fn((url: string) => {
    const href = String(url);
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody(databaseConfigured)));
    if (href.startsWith("/api/v1/events")) return Promise.resolve(json({ events, nextCursor: "" }));
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <RecentChanges {...props} />
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
});

describe("RecentChanges", () => {
  it("requests /api/v1/events with the exact pinned scope string, arrow and all", async () => {
    const { fetchMock } = renderRail("node-a→node-b");
    await waitFor(() =>
      expect(fetchMock.mock.calls.some((c) => String(c[0]).startsWith("/api/v1/events"))).toBe(true),
    );
    const call = fetchMock.mock.calls.find((c) => String(c[0]).startsWith("/api/v1/events"));
    const url = new URL(String(call?.[0]), "http://localhost");
    // The exact string, decoded back off the wire — U+2192, never "->".
    expect(url.searchParams.get("scope")).toBe("node-a→node-b");
    expect(url.searchParams.get("limit")).toBe("50");
  });

  it("prepends a live event on the socket whose scope matches exactly", async () => {
    renderRail("node-a");
    await screen.findByText("No recent changes.");

    act(() => FakeSocket.last().emitOpen());
    act(() => {
      FakeSocket.last().emitEnvelope({
        topic: "live",
        type: "event",
        seq: 1,
        data: event({ id: "2-2000", scope: "node-a", summary: "matching scope event" }),
      });
    });

    expect(await screen.findByText("matching scope event")).toBeInTheDocument();
  });

  it("does not render a live event for a different scope", async () => {
    renderRail("node-a");
    await waitFor(() => expect(screen.getByText("No recent changes.")).toBeInTheDocument());

    act(() => FakeSocket.last().emitOpen());
    act(() => {
      FakeSocket.last().emitEnvelope({
        topic: "live",
        type: "event",
        seq: 1,
        data: event({ id: "3-3000", scope: "node-a2", summary: "unrelated scope event" }),
      });
    });

    // Give the (non-existent) update a chance to land, then assert it never did.
    await new Promise((r) => setTimeout(r, 0));
    expect(screen.queryByText("unrelated scope event")).not.toBeInTheDocument();
    expect(screen.getByText("No recent changes.")).toBeInTheDocument();
  });

  it("with database.configured === false renders the degraded note and makes no history request", async () => {
    const { fetchMock } = renderRail("node-a", { databaseConfigured: false });
    expect(await screen.findByText(/history requires a database/i)).toBeInTheDocument();
    expect(fetchMock.mock.calls.some((c) => String(c[0]).startsWith("/api/v1/events"))).toBe(false);
  });

  it("renders history rows returned from the REST scrollback", async () => {
    renderRail("node-a", { events: [event({ id: "9-9000", summary: "history row" })] });
    expect(await screen.findByText("history row")).toBeInTheDocument();
  });
});

/* ── the pair-aware half (QA scope 2 #21) ─────────────────────────────────── */

describe("matchesScope", () => {
  it("with a scopeNode admits the bare scope and both sides of a pair", () => {
    expect(matchesScope("node-a", "", "node-a")).toBe(true);
    expect(matchesScope("node-a→node-b", "", "node-a")).toBe(true);
    expect(matchesScope("node-c→node-a", "", "node-a")).toBe(true);
  });

  it("with a scopeNode matches whole halves, never substrings", () => {
    expect(matchesScope("node-ax→node-b", "", "node-a")).toBe(false);
    expect(matchesScope("node-b→x-node-a", "", "node-a")).toBe(false);
    expect(matchesScope("node-ab", "", "node-a")).toBe(false);
    expect(matchesScope("node-c→node-d", "", "node-a")).toBe(false);
  });

  it("with a scope stays exact equality — a pair card is pinned to one edge", () => {
    expect(matchesScope("node-a→node-b", "node-a→node-b", "")).toBe(true);
    expect(matchesScope("node-a", "node-a→node-b", "")).toBe(false);
    expect(matchesScope("node-a→node-b", "node-a", "")).toBe(false);
  });
});

describe("RecentChanges pinned by scopeNode", () => {
  it("sends ?scopeNode= and never the mutually exclusive ?scope=", async () => {
    const { fetchMock } = renderRailWith({ scopeNode: "node-a" });
    await waitFor(() =>
      expect(fetchMock.mock.calls.some((c) => String(c[0]).startsWith("/api/v1/events"))).toBe(true),
    );
    const call = fetchMock.mock.calls.find((c) => String(c[0]).startsWith("/api/v1/events"));
    const url = new URL(String(call?.[0]), "http://localhost");
    expect(url.searchParams.get("scopeNode")).toBe("node-a");
    expect(url.searchParams.get("scope")).toBeNull();
  });

  it("admits a live pair-scoped event naming the node — the half a reload used to reveal", async () => {
    renderRailWith({ scopeNode: "node-a" });
    await screen.findByText("No recent changes.");

    act(() => FakeSocket.last().emitOpen());
    act(() => {
      FakeSocket.last().emitEnvelope({
        topic: "live",
        type: "event",
        seq: 1,
        data: event({ id: "4-4000", scope: "node-a→node-b", summary: "tcp check node-a→node-b failed" }),
      });
    });

    expect(await screen.findByText("tcp check node-a→node-b failed")).toBeInTheDocument();
  });
});
