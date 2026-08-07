import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import type { RunDetail } from "@/lib/types";
import { runIdFromPath, RunDetailPage } from "./run-detail";

const RUN_ID = "run-1";

function runBody(overrides: Partial<RunDetail> = {}): RunDetail {
  return {
    id: RUN_ID,
    createdAt: "2026-07-28T10:00:00Z",
    status: "running",
    type: "tcp",
    plane: "pod",
    initiatorKind: "user",
    initiatorId: "u1",
    pairTotal: 2,
    pairOk: 0,
    pairFailed: 0,
    spec: {},
    results: [],
    ...overrides,
  };
}

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function renderPage(capabilities: string[], run: RunDetail, runId = RUN_ID) {
  window.history.pushState({}, "", `/diagnostics/runs/${runId}`);
  const fetchMock = vi.fn((url: string) => {
    const href = String(url);
    if (href.includes("/api/v1/version")) return Promise.resolve(json({ version: "1.6.0", commit: "x", capabilities }));
    if (href.startsWith("/api/v1/runs/")) return Promise.resolve(json(run));
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  qc.setQueryData(["version"], { version: "1.6.0", commit: "x", capabilities });
  const utils = render(
    <QueryClientProvider client={qc}>
      <RunDetailPage />
    </QueryClientProvider>,
  );
  return { ...utils, fetchMock, qc };
}

function renderNotFound(runId = "nope") {
  window.history.pushState({}, "", `/diagnostics/runs/${runId}`);
  const fetchMock = vi.fn((url: string) => {
    const href = String(url);
    if (href.includes("/api/v1/version")) return Promise.resolve(json({ version: "1.6.0", commit: "x", capabilities: [] }));
    if (href.startsWith("/api/v1/runs/")) {
      return Promise.resolve(
        new Response(JSON.stringify({ type: "about:blank", title: "run not found", status: 404 }), {
          status: 404,
          headers: { "Content-Type": "application/problem+json" },
        }),
      );
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  qc.setQueryData(["version"], { version: "1.6.0", commit: "x", capabilities: [] });
  return render(
    <QueryClientProvider client={qc}>
      <RunDetailPage />
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

describe("runIdFromPath", () => {
  it("extracts the id after the permalink prefix", () => {
    expect(runIdFromPath("/diagnostics/runs/abc-123")).toBe("abc-123");
    expect(runIdFromPath("/diagnostics")).toBe("");
  });
});

describe("RunDetailPage", () => {
  it("renders progress from socket frames", async () => {
    renderPage(["events"], runBody({ status: "running" }));

    // Wait for the run to have actually loaded (not just the page shell) --
    // the socket only opens once the first REST response is in, see
    // use-run.ts's socketEnabled doc comment.
    await screen.findByText("running");
    act(() => FakeSocket.last().emitOpen());
    act(() => {
      FakeSocket.last().emitEnvelope({
        topic: "run:run-1",
        type: "event",
        seq: 1,
        data: { runId: RUN_ID, source: "node-a", destination: "node-b", state: "dispatched", completed: 0, total: 2 },
      });
    });

    await waitFor(() => expect(screen.getByText("node-a")).toBeInTheDocument());
    expect(screen.getByText("node-b")).toBeInTheDocument();
    expect(screen.getByText("dispatched")).toBeInTheDocument();
  });

  it("still completes with the socket disabled -- polling alone drives it to a terminal state", async () => {
    window.history.pushState({}, "", `/diagnostics/runs/${RUN_ID}`);
    let terminal = false;
    const fetchMock = vi.fn((url: string) => {
      const href = String(url);
      if (href.includes("/api/v1/version")) return Promise.resolve(json({ version: "1.6.0", commit: "x", capabilities: [] }));
      if (href.startsWith("/api/v1/runs/")) {
        return Promise.resolve(
          json(
            terminal
              ? runBody({
                  status: "succeeded",
                  pairOk: 1,
                  results: [{ sourceNode: "node-a", destinationNode: "node-b", success: true, durationNs: 12, recordedAt: "t" }],
                })
              : runBody({ status: "running" }),
          ),
        );
      }
      return Promise.resolve(json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    qc.setQueryData(["version"], { version: "1.6.0", commit: "x", capabilities: [] });

    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      render(
        <QueryClientProvider client={qc}>
          <RunDetailPage />
        </QueryClientProvider>,
      );

      await waitFor(() => expect(screen.getByText("running")).toBeInTheDocument());
      expect(FakeSocket.instances).toHaveLength(0);

      terminal = true;
      await act(async () => {
        await vi.advanceTimersByTimeAsync(5_100);
      });

      // "succeeded" shows twice (the run's status badge and this pair's own
      // state badge).
      expect(screen.getAllByText("succeeded").length).toBeGreaterThan(0);
      expect(screen.getByText("node-a")).toBeInTheDocument();
      expect(FakeSocket.instances).toHaveLength(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it("a socket frame and a polled result for the same pair render once, not twice", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "running",
        results: [],
      }),
    );

    await screen.findByText("running");
    act(() => FakeSocket.last().emitOpen());
    act(() => {
      FakeSocket.last().emitEnvelope({
        topic: "run:run-1",
        type: "event",
        seq: 1,
        data: { runId: RUN_ID, source: "node-a", destination: "node-b", state: "succeeded", success: true, completed: 1, total: 1 },
      });
    });
    await waitFor(() => expect(screen.getAllByText("node-a")).toHaveLength(1));
    expect(screen.getAllByText("node-b")).toHaveLength(1);
  });

  it("a direct load of a finished run's permalink renders from the REST payload alone", async () => {
    renderPage(
      ["events"],
      runBody({
        status: "succeeded",
        pairOk: 1,
        results: [{ sourceNode: "node-a", destinationNode: "node-b", success: true, durationNs: 3, recordedAt: "t" }],
      }),
    );

    // "succeeded" shows twice (the run's own status badge, and this one
    // pair's state badge) -- both are the REST payload rendering correctly.
    await waitFor(() => expect(screen.getAllByText("succeeded").length).toBeGreaterThan(0));
    expect(screen.getByText("node-a")).toBeInTheDocument();
    // Already terminal on first paint -- no socket is ever opened for it.
    expect(FakeSocket.instances).toHaveLength(0);
  });

  it("an unknown run id renders a not-found state rather than an infinite spinner", async () => {
    renderNotFound("nope");

    expect(await screen.findByText(/this run does not exist/i)).toBeInTheDocument();
    expect(screen.queryByRole("status", { name: /loading run/i })).not.toBeInTheDocument();
    expect(screen.queryByText(/loading run/i)).not.toBeInTheDocument();
  });
});
