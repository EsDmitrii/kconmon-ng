import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FakeSocket } from "@/lib/fake-websocket";
import type { RunDetail, RunProgressFrame } from "@/lib/types";
import { isTerminalRunStatus, mergeRunPairs, RUN_POLL_MS, runTopic, useRun } from "./use-run";
import { resetWsClient } from "./use-ws-topic";

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

function setup(capabilities: string[], run: RunDetail) {
  const fetchMock = vi.fn((url: string) => {
    const href = String(url);
    if (href.includes("/api/v1/version")) {
      return Promise.resolve(json({ version: "1.6.0", commit: "abc123", capabilities }));
    }
    if (href.startsWith("/api/v1/runs/")) {
      return Promise.resolve(json(run));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  qc.setQueryData(["version"], { version: "1.6.0", commit: "abc123", capabilities });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>{children}</QueryClientProvider>
  );
  return { qc, wrapper, fetchMock };
}

function runFetchCount(fetchMock: ReturnType<typeof vi.fn>): number {
  return fetchMock.mock.calls.filter(([url]) => String(url).startsWith("/api/v1/runs/")).length;
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

describe("runTopic", () => {
  it("mirrors ws.RunTopic's own canonical format", () => {
    expect(runTopic("abc-123")).toBe("run:abc-123");
  });
});

describe("isTerminalRunStatus", () => {
  it("treats succeeded/failed/partial as terminal, pending/running/undefined as not", () => {
    expect(isTerminalRunStatus("succeeded")).toBe(true);
    expect(isTerminalRunStatus("failed")).toBe(true);
    expect(isTerminalRunStatus("partial")).toBe(true);
    expect(isTerminalRunStatus("pending")).toBe(false);
    expect(isTerminalRunStatus("running")).toBe(false);
    expect(isTerminalRunStatus(undefined)).toBe(false);
  });

  it("treats cancelled as terminal -- a cancelled run is finished, not paused", () => {
    expect(isTerminalRunStatus("cancelled")).toBe(true);
  });
});

describe("mergeRunPairs", () => {
  const frame = (over: Partial<RunProgressFrame> = {}): RunProgressFrame => ({
    runId: RUN_ID,
    source: "a",
    destination: "b",
    state: "dispatched",
    completed: 0,
    total: 2,
    ...over,
  });

  it("renders a pair the socket has seen but REST has not caught up to yet", () => {
    const frames = new Map([["a\0b", frame()]]);
    const rows = mergeRunPairs([], frames);
    expect(rows).toEqual([{ source: "a", destination: "b", state: "dispatched", success: undefined, durationNs: undefined, error: undefined }]);
  });

  it("a socket frame and a polled result for the SAME pair render once, REST wins", () => {
    const frames = new Map([["a\0b", frame({ state: "succeeded", success: true, durationNs: 5 })]]);
    const rows = mergeRunPairs(
      [{ sourceNode: "a", destinationNode: "b", success: true, durationNs: 9, recordedAt: "t", sampleSeq: 0 }],
      frames,
    );
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ source: "a", destination: "b", state: "succeeded", durationNs: 9 });
  });

  it("keeps pairs from both sources when they name different pairs", () => {
    const frames = new Map([["c\0d", frame({ source: "c", destination: "d" })]]);
    const rows = mergeRunPairs(
      [{ sourceNode: "a", destinationNode: "b", success: true, durationNs: 1, recordedAt: "t", sampleSeq: 0 }],
      frames,
    );
    expect(rows).toHaveLength(2);
  });
});

describe("useRun with realtime off", () => {
  it("still completes the run from polling alone, opening no socket at all", async () => {
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
                  pairOk: 2,
                  results: [
                    { sourceNode: "a", destinationNode: "b", success: true, durationNs: 1, recordedAt: "t", sampleSeq: 0 },
                    { sourceNode: "b", destinationNode: "a", success: true, durationNs: 1, recordedAt: "t", sampleSeq: 0 },
                  ],
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
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={qc}>{children}</QueryClientProvider>
    );

    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const { result } = renderHook(() => useRun(RUN_ID), { wrapper });

      await waitFor(() => expect(result.current.run?.status).toBe("running"));
      expect(result.current.live).toBe(false);
      expect(FakeSocket.instances).toHaveLength(0);

      // Once the next poll answers terminal, the page must reflect it with
      // no socket ever having been involved.
      terminal = true;
      await act(async () => {
        await vi.advanceTimersByTimeAsync(RUN_POLL_MS + 100);
      });

      expect(result.current.run?.status).toBe("succeeded");
      expect(result.current.pairs).toHaveLength(2);
      expect(result.current.live).toBe(false);
      expect(FakeSocket.instances).toHaveLength(0);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("useRun and a cancelled run", () => {
  it("stops polling once the status is cancelled -- the run-fetch count plateaus", async () => {
    let status = "running";
    const fetchMock = vi.fn((url: string) => {
      const href = String(url);
      if (href.includes("/api/v1/version")) return Promise.resolve(json({ version: "1.6.0", commit: "x", capabilities: [] }));
      if (href.startsWith("/api/v1/runs/")) return Promise.resolve(json(runBody({ status })));
      return Promise.resolve(json({}));
    });
    vi.stubGlobal("fetch", fetchMock);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    qc.setQueryData(["version"], { version: "1.6.0", commit: "x", capabilities: [] });
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={qc}>{children}</QueryClientProvider>
    );

    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const { result } = renderHook(() => useRun(RUN_ID), { wrapper });
      await waitFor(() => expect(result.current.run?.status).toBe("running"));

      // While it is running the poll is alive -- the control half of this
      // test, so the plateau below cannot pass by the timer never firing.
      const whileRunning = runFetchCount(fetchMock);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(RUN_POLL_MS + 100);
      });
      expect(runFetchCount(fetchMock)).toBeGreaterThan(whileRunning);

      status = "cancelled";
      await act(async () => {
        await vi.advanceTimersByTimeAsync(RUN_POLL_MS + 100);
      });
      await waitFor(() => expect(result.current.run?.status).toBe("cancelled"));

      const settled = runFetchCount(fetchMock);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(RUN_POLL_MS * 3);
      });
      expect(runFetchCount(fetchMock)).toBe(settled);
    } finally {
      vi.useRealTimers();
    }
  });

  it("opens no socket for a run that is already cancelled on first load", async () => {
    const { wrapper } = setup(["events"], runBody({ status: "cancelled" }));
    const { result } = renderHook(() => useRun(RUN_ID), { wrapper });

    await waitFor(() => expect(result.current.run?.status).toBe("cancelled"));
    expect(result.current.live).toBe(false);
    expect(FakeSocket.instances).toHaveLength(0);
  });
});

describe("useRun with realtime on", () => {
  it("renders progress from socket frames while REST is still behind", async () => {
    const { wrapper } = setup(["events"], runBody({ status: "running" }));
    const { result } = renderHook(() => useRun(RUN_ID), { wrapper });

    await waitFor(() => expect(result.current.run?.status).toBe("running"));
    expect(FakeSocket.instances).toHaveLength(1);
    act(() => FakeSocket.last().emitOpen());
    expect(FakeSocket.last().sent).toEqual([`{"action":"subscribe","topic":"${runTopic(RUN_ID)}"}`]);
    expect(result.current.live).toBe(true);

    act(() => {
      FakeSocket.last().emitEnvelope({
        topic: runTopic(RUN_ID),
        type: "event",
        seq: 1,
        data: { runId: RUN_ID, source: "a", destination: "b", state: "dispatched", completed: 0, total: 2 },
      });
    });

    await waitFor(() => expect(result.current.pairs).toHaveLength(1));
    expect(result.current.pairs[0]).toMatchObject({ source: "a", destination: "b", state: "dispatched" });
  });

  it("unsubscribes and stops treating the socket as live on the TypeClosed control frame", async () => {
    const { wrapper } = setup(["events"], runBody({ status: "running" }));
    const { result } = renderHook(() => useRun(RUN_ID), { wrapper });

    await waitFor(() => expect(result.current.run?.status).toBe("running"));
    act(() => FakeSocket.last().emitOpen());
    expect(result.current.live).toBe(true);

    act(() => {
      FakeSocket.last().emitEnvelope({ topic: runTopic(RUN_ID), type: "closed", seq: 5, data: {} });
    });

    await waitFor(() => expect(result.current.live).toBe(false));
    // Pins the no-reconnect-loop claim: `socketEnabled` going false must actually tear down the
    // subscription over the wire (WsClient.unsubscribe, lib/ws.ts).
    expect(FakeSocket.last().sent).toContain(JSON.stringify({ action: "unsubscribe", topic: runTopic(RUN_ID) }));
  });

  it("falls back to polling when the topic subscribe is rejected (registry full / old run)", async () => {
    const { wrapper } = setup(["events"], runBody({ status: "running" }));
    const { result } = renderHook(() => useRun(RUN_ID), { wrapper });

    await waitFor(() => expect(result.current.run?.status).toBe("running"));
    act(() => FakeSocket.last().emitOpen());
    act(() => {
      FakeSocket.last().emitEnvelope({ topic: runTopic(RUN_ID), type: "error", seq: 0, data: "unknown topic" });
    });

    await waitFor(() => expect(result.current.live).toBe(false));
  });
});

describe("useRun permalink guarantee", () => {
  it("a direct load of an already-finished run renders from the REST payload alone -- no socket", async () => {
    const { wrapper } = setup(
      ["events"],
      runBody({
        status: "succeeded",
        pairOk: 1,
        results: [{ sourceNode: "a", destinationNode: "b", success: true, durationNs: 4, recordedAt: "t", sampleSeq: 0 }],
      }),
    );
    const { result } = renderHook(() => useRun(RUN_ID), { wrapper });

    await waitFor(() => expect(result.current.run?.status).toBe("succeeded"));
    expect(result.current.pairs).toHaveLength(1);
    expect(result.current.live).toBe(false);
    expect(FakeSocket.instances).toHaveLength(0);
  });
});

describe("useRun with an unknown run id", () => {
  it("reports notFound rather than staying an infinite spinner", async () => {
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
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={qc}>{children}</QueryClientProvider>
    );

    const { result } = renderHook(() => useRun("nope"), { wrapper });

    await waitFor(() => expect(result.current.notFound).toBe(true));
    expect(result.current.isLoading).toBe(false);

    const before = runFetchCount(fetchMock);
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(RUN_POLL_MS + 100);
      });
    } finally {
      vi.useRealTimers();
    }
    // No repeated polling against a 404 that a timer cannot fix.
    expect(runFetchCount(fetchMock)).toBe(before);
  });
});
