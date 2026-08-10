import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RecentChanges } from "@/components/recent-changes";
import { ThemeProvider } from "@/components/theme-provider";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import { TimeMachineProvider } from "@/lib/timemachine";
import { NodeCardPage } from "./node-card";
import { PairCardPage } from "./pair-card";
import { TargetCardPage } from "./target-card";

/**
 * One pinned query per card family, plus the shared "Recent changes" rail: engaged, every read a
 * card makes is anchored at `t`.
 */

const AT = "2026-08-01T12:00:00Z";
const AT_MS = Date.parse(AT);
const AT_ISO = new Date(AT).toISOString();

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

const configBody = {
  auth: { mode: "anonymous", role: "admin", loginPath: "" },
  anonymousBanner: true,
  controller: { configured: true },
  prometheus: { configured: true },
  database: { configured: true },
};

const meBody = {
  subject: { kind: "user", id: "u", displayName: "u", groups: [], roles: ["admin"] },
  permissions: ["runs:create", "targets:read", "checks:read", "mtr:read"],
};

const targetBody = { id: "t-1", name: "edge-gw", kind: "host", address: "10.0.0.1", labels: {} };

interface Recorder {
  eventQueries: URLSearchParams[];
  instant: { query: string; time?: string }[];
  range: { query: string; start: string; end: string }[];
  topologyUrls: string[];
}

function stubFetch(): Recorder {
  const rec: Recorder = { eventQueries: [], instant: [], range: [], topologyUrls: [] };
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string, init?: RequestInit) => {
      const href = String(url);
      if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody));
      if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody));
      if (href.startsWith("/api/v1/topology")) {
        rec.topologyUrls.push(href);
        return Promise.resolve(
          json({ nodes: [{ name: "node-a", zone: "z1", ready: true }], agents: [], timestamp: AT, historical: true, asOf: AT }),
        );
      }
      if (href.startsWith("/api/v1/events")) {
        rec.eventQueries.push(new URLSearchParams(href.split("?")[1] ?? ""));
        return Promise.resolve(json({ events: [], nextCursor: "" }));
      }
      if (href.includes("/api/v1/promql/query_range")) {
        rec.range.push(JSON.parse(String(init?.body)) as { query: string; start: string; end: string });
        return Promise.resolve(json({ status: "success", data: { resultType: "matrix", result: [] } }));
      }
      if (href.includes("/api/v1/promql/query")) {
        rec.instant.push(JSON.parse(String(init?.body)) as { query: string; time?: string });
        return Promise.resolve(json({ status: "success", data: { resultType: "vector", result: [] } }));
      }
      if (href.startsWith("/api/v1/targets/")) return Promise.resolve(json(targetBody));
      if (href.startsWith("/api/v1/targets")) return Promise.resolve(json({ targets: [targetBody], nextCursor: "" }));
      if (href.startsWith("/api/v1/runs")) return Promise.resolve(json({ runs: [], nextCursor: "" }));
      if (href.startsWith("/api/v1/checks")) return Promise.resolve(json({ definitions: [], nextCursor: "" }));
      if (href.startsWith("/api/v1/schedules")) return Promise.resolve(json({ schedules: [], nextCursor: "" }));
      return Promise.resolve(json({}));
    }),
  );
  return rec;
}

function renderCard(node: React.ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TimeMachineProvider>{node}</TimeMachineProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
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

describe("RecentChanges engaged at t", () => {
  it("bounds the rail with to=t", async () => {
    window.history.pushState({}, "", `/nodes/node-a?at=${AT}`);
    const rec = stubFetch();
    renderCard(<RecentChanges scope="node-a" />);
    await waitFor(() => expect(rec.eventQueries.length).toBeGreaterThan(0));
    expect(rec.eventQueries[0].get("scope")).toBe("node-a");
    expect(rec.eventQueries[0].get("to")).toBe(AT_ISO);
  });

  it("states the cut where the rows are", async () => {
    window.history.pushState({}, "", `/nodes/node-a?at=${AT}`);
    stubFetch();
    renderCard(<RecentChanges scope="node-a" />);
    await screen.findByText(`up to ${new Date(AT).toLocaleString()}`);
  });

  it("stops listening to the socket, so the present cannot trickle in", async () => {
    window.history.pushState({}, "", `/nodes/node-a?at=${AT}`);
    stubFetch();
    renderCard(<RecentChanges scope="node-a" />);
    await screen.findByText("No recent changes.");
    expect(FakeSocket.instances).toHaveLength(0);
  });

  it("sends no bound and still subscribes while live", async () => {
    window.history.pushState({}, "", "/nodes/node-a");
    const rec = stubFetch();
    renderCard(<RecentChanges scope="node-a" />);
    await waitFor(() => expect(rec.eventQueries.length).toBeGreaterThan(0));
    expect(rec.eventQueries[0].get("to")).toBeNull();
    await waitFor(() => expect(FakeSocket.instances).toHaveLength(1));
  });
});

describe("NodeCardPage engaged at t", () => {
  it("reads topology at ?at= and dates the header from the server's asOf", async () => {
    window.history.pushState({}, "", `/nodes/node-a?at=${AT}`);
    const rec = stubFetch();
    renderCard(<NodeCardPage />);
    await screen.findByText(new RegExp(`state as of ${escapeRe(new Date(AT).toLocaleString())}`));
    expect(rec.topologyUrls[0]).toContain("at=");
  });
});

describe("PairCardPage engaged at t", () => {
  it("ends the RTT window at t rather than at now", async () => {
    window.history.pushState({}, "", `/pairs/node-a/node-b?at=${AT}`);
    const rec = stubFetch();
    renderCard(<PairCardPage />);
    await waitFor(() => expect(rec.range.length).toBeGreaterThan(0));
    const pair = rec.range.find((b) => b.query.includes("destination_node=\"node-b\""));
    expect(pair).toBeDefined();
    expect(Date.parse(pair?.end ?? "")).toBe(AT_MS);
    expect(AT_MS - Date.parse(pair?.start ?? "")).toBe(60 * 60 * 1000);
  });

  it("labels the chart with the window it actually drew", async () => {
    window.history.pushState({}, "", `/pairs/node-a/node-b?at=${AT}`);
    stubFetch();
    renderCard(<PairCardPage />);
    await screen.findByText(new RegExp(`hour ending ${escapeRe(new Date(AT).toLocaleString())}`));
  });
});

describe("TargetCardPage engaged at t", () => {
  it("evaluates the header's instant health query AT t", async () => {
    window.history.pushState({}, "", `/targets/t-1?at=${AT}`);
    const rec = stubFetch();
    renderCard(<TargetCardPage />);
    await waitFor(() => expect(rec.instant.length).toBeGreaterThan(0));
    expect(rec.instant[0].time).toBe(AT_ISO);
  });

  it("says the header is state as of t", async () => {
    window.history.pushState({}, "", `/targets/t-1?at=${AT}`);
    stubFetch();
    renderCard(<TargetCardPage />);
    await screen.findByText(`External probe target — state as of ${new Date(AT).toLocaleString()}`);
  });

  it("sends no time at all while live", async () => {
    window.history.pushState({}, "", "/targets/t-1");
    const rec = stubFetch();
    renderCard(<TargetCardPage />);
    await waitFor(() => expect(rec.instant.length).toBeGreaterThan(0));
    expect(rec.instant[0].time).toBeUndefined();
  });

  /* The honest line is the only fix that is not a milestone. */
  it("says the config panel is shown as of NOW, because it is (#4)", async () => {
    window.history.pushState({}, "", `/targets/t-1?at=${AT}`);
    stubFetch();
    renderCard(<TargetCardPage />);
    const notice = await screen.findByTestId("checks-tm-notice");
    expect(notice).toHaveTextContent("Target configuration is shown as of now — only the probe series time-travel.");
  });

  it("disclaims nothing while live — there is nothing to disclaim (#4)", async () => {
    window.history.pushState({}, "", "/targets/t-1");
    stubFetch();
    renderCard(<TargetCardPage />);
    await screen.findByText("External probe target");
    expect(screen.queryByTestId("checks-tm-notice")).toBeNull();
  });
});

function escapeRe(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

const AFTER = { id: "run-after", createdAt: "2026-08-01T12:30:00Z", startedAt: "2026-08-01T12:30:05Z" };
const BEFORE = { id: "run-before", createdAt: "2026-08-01T11:00:00Z", startedAt: "2026-08-01T11:00:05Z" };

function runSummary(r: { id: string; createdAt: string; startedAt: string }) {
  return {
    ...r,
    status: "succeeded", type: "tcp", plane: "pod",
    initiatorKind: "user", initiatorId: "u",
    pairTotal: 1, pairOk: 1, pairFailed: 0,
  };
}

function runDetail(r: { id: string; createdAt: string; startedAt: string }) {
  return {
    ...runSummary(r),
    spec: {},
    results: [
      {
        sourceNode: "node-a",
        destinationNode: "node-b",
        success: true,
        durationNs: 1_000_000,
        recordedAt: r.startedAt,
      },
    ],
  };
}

/** Answers the run list with both runs and each detail with its own body. */
function stubRuns(): { detailIds: string[] } {
  const detailIds: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      const href = String(url);
      if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody));
      if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody));
      if (href.startsWith("/api/v1/topology")) {
        return Promise.resolve(json({ nodes: [{ name: "node-a", zone: "z1", ready: true }], agents: [], timestamp: AT }));
      }
      if (href.startsWith("/api/v1/events")) return Promise.resolve(json({ events: [], nextCursor: "" }));
      if (href.includes("/api/v1/promql/query_range")) {
        return Promise.resolve(json({ status: "success", data: { resultType: "matrix", result: [] } }));
      }
      if (href.includes("/api/v1/promql/query")) {
        return Promise.resolve(json({ status: "success", data: { resultType: "vector", result: [] } }));
      }
      const detail = /^\/api\/v1\/runs\/(.+)$/.exec(href);
      if (detail) {
        detailIds.push(detail[1]);
        return Promise.resolve(json(runDetail(detail[1] === AFTER.id ? AFTER : BEFORE)));
      }
      if (href.startsWith("/api/v1/runs")) {
        return Promise.resolve(json({ runs: [runSummary(AFTER), runSummary(BEFORE)], nextCursor: "" }));
      }
      return Promise.resolve(json({}));
    }),
  );
  return { detailIds };
}

describe("NodeCardPage Diagnostics engaged at t", () => {
  it("never renders a run that started after the viewed instant", async () => {
    window.history.pushState({}, "", `/nodes/node-a?at=${AT}`);
    const { detailIds } = stubRuns();
    renderCard(<NodeCardPage />);
    fireEvent.click(await screen.findByRole("radio", { name: "Diagnostics" }));
    await screen.findByText("run-before");
    expect(screen.queryByText("run-after")).toBeNull();
    // And it does not even cost a detail request.
    expect(detailIds).not.toContain("run-after");
  });

  it("states the time bound alongside the page bound it already admitted", async () => {
    window.history.pushState({}, "", `/nodes/node-a?at=${AT}`);
    stubRuns();
    renderCard(<NodeCardPage />);
    fireEvent.click(await screen.findByRole("radio", { name: "Diagnostics" }));
    expect(await screen.findByText(/only runs started at or before the viewed instant/)).toBeInTheDocument();
  });

  it("lists both while Live, and says nothing about an instant", async () => {
    window.history.pushState({}, "", "/nodes/node-a");
    stubRuns();
    renderCard(<NodeCardPage />);
    fireEvent.click(await screen.findByRole("radio", { name: "Diagnostics" }));
    await screen.findByText("run-after");
    expect(screen.getByText("run-before")).toBeInTheDocument();
    expect(screen.queryByText(/viewed instant/)).toBeNull();
  });
});

describe("PairCardPage Diagnostics engaged at t", () => {
  it("calls the newest run AS OF t the last run, not the newest run now", async () => {
    window.history.pushState({}, "", `/pairs/node-a/node-b?at=${AT}`);
    stubRuns();
    renderCard(<PairCardPage />);
    fireEvent.click(await screen.findByRole("radio", { name: "Diagnostics" }));
    await screen.findByRole("link", { name: "run-before" });
    expect(screen.queryByRole("link", { name: "run-after" })).toBeNull();
  });
});
