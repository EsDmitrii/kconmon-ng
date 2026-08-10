import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FakeSocket } from "@/lib/fake-websocket";
import type { Matrix, Protocol } from "@/lib/types";
import { MATRIX_POLL_MS, matrixTopic, useMatrix } from "./use-matrix";
import { resetWsClient } from "./use-ws-topic";

const polled: Matrix = {
  protocol: "tcp",
  plane: "pod",
  nodes: ["a"],
  cells: [],
  timestamp: "2026-07-28T10:00:00Z",
};
const pushed: Matrix = {
  protocol: "tcp",
  plane: "pod",
  nodes: ["a", "b"],
  cells: [{ source: "a", destination: "b", failRatio: 0 }],
  timestamp: "2026-07-28T10:00:05Z",
};

const udpPushed: Matrix = {
  protocol: "udp",
  plane: "pod",
  nodes: ["c"],
  cells: [],
  timestamp: "2026-07-28T10:00:09Z",
};

/**
 * The REST answer for a given protocol; the server echoes the protocol it was asked for, and this
 * test file leans.
 */
const polledFor = (protocol: string): Matrix => ({ ...polled, protocol });

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

const MATRIX_QUERY_HASH = JSON.stringify(["matrix", "tcp", "pod"]);

function setup(capabilities: string[]) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      const href = String(url);
      if (href.includes("/api/v1/version")) {
        return Promise.resolve(json({ version: "1.6.0", commit: "abc123", capabilities }));
      }
      const protocol = new URLSearchParams(href.split("?")[1] ?? "").get("protocol") ?? "tcp";
      return Promise.resolve(json(polledFor(protocol)));
    }),
  );
  // staleTime keeps the seeded ["version"] entry from refetching on mount: the
  // refetch would resolve after the test body has finished asserting, which
  // React reports as an update outside act().
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  qc.setQueryData(["version"], { version: "1.6.0", commit: "abc123", capabilities });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>{children}</QueryClientProvider>
  );
  return { qc, wrapper };
}

// The live refetchInterval the observer is actually running with. queryHash is
// the public, stable identity of a cache entry (JSON of the key), so this needs
// no QueryFilters generics.
function matrixRefetchInterval(qc: QueryClient): unknown {
  const entry = qc
    .getQueryCache()
    .getAll()
    .find((q) => q.queryHash === MATRIX_QUERY_HASH);
  return entry?.observers[0]?.options.refetchInterval;
}

function matrixFetchCount(): number {
  return vi.mocked(fetch).mock.calls.filter(([url]) => String(url).startsWith("/api/v1/matrix")).length;
}

beforeEach(() => {
  FakeSocket.reset();
  vi.stubGlobal("WebSocket", FakeSocket);
});

afterEach(() => {
  // Unmount BEFORE disposing the shared client: resetWsClient() calls close(),
  // which notifies state listeners, and a still-mounted hook would take that
  // setConnected(false) outside act().
  cleanup();
  resetWsClient();
  vi.unstubAllGlobals();
});

describe("matrixTopic", () => {
  it("mirrors the Go ws.MatrixTopic shape", () => {
    expect(matrixTopic("tcp")).toBe("matrix:tcp:pod");
    expect(matrixTopic("udp")).toBe("matrix:udp:pod");
    expect(matrixTopic("icmp")).toBe("matrix:icmp:pod");
  });
});

describe("useMatrix without realtime", () => {
  it("leaves M1 polling exactly as it was and opens no socket", async () => {
    const { qc, wrapper } = setup([]);
    const { result } = renderHook(() => useMatrix("tcp"), { wrapper });

    await waitFor(() => expect(result.current.data).toEqual(polled));
    expect(matrixRefetchInterval(qc)).toBe(MATRIX_POLL_MS);
    expect(result.current.live).toBe(false);
    expect(FakeSocket.instances).toHaveLength(0);
  });

  it("actually keeps polling — the interval fires", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const { wrapper } = setup([]);
      const { result } = renderHook(() => useMatrix("tcp"), { wrapper });
      await waitFor(() => expect(result.current.data).toEqual(polled));
      expect(matrixFetchCount()).toBe(1);

      await act(async () => {
        await vi.advanceTimersByTimeAsync(2 * MATRIX_POLL_MS);
      });
      expect(matrixFetchCount()).toBeGreaterThan(1);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("useMatrix with realtime", () => {
  it("still paints first from getMatrix(), then stops polling and takes pushed snapshots", async () => {
    const { qc, wrapper } = setup(["events"]);
    const { result } = renderHook(() => useMatrix("tcp"), { wrapper });

    // First paint is still the REST call — the socket has nothing yet, and
    // polling stays armed until it actually reaches open.
    await waitFor(() => expect(result.current.data).toEqual(polled));
    expect(matrixFetchCount()).toBe(1);
    expect(matrixRefetchInterval(qc)).toBe(MATRIX_POLL_MS);
    expect(result.current.live).toBe(false);

    expect(FakeSocket.instances).toHaveLength(1);
    act(() => {
      FakeSocket.last().emitOpen();
    });
    expect(FakeSocket.last().sent).toEqual([`{"action":"subscribe","topic":"${matrixTopic("tcp")}"}`]);
    expect(matrixRefetchInterval(qc)).toBe(false);
    expect(result.current.live).toBe(true);

    act(() => {
      FakeSocket.last().emitEnvelope({ topic: matrixTopic("tcp"), type: "snapshot", seq: 1, data: pushed });
    });

    await waitFor(() => expect(result.current.data).toEqual(pushed));
    // The pushed snapshot lands in the SAME query key, so MatrixPage is unchanged.
    expect(qc.getQueryData(["matrix", "tcp", "pod"])).toEqual(pushed);
    expect(matrixFetchCount()).toBe(1);
  });

  it("really does silence the interval — no refetch across two poll periods", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const { wrapper } = setup(["events"]);
      const { result } = renderHook(() => useMatrix("tcp"), { wrapper });
      await waitFor(() => expect(result.current.data).toEqual(polled));
      act(() => {
        FakeSocket.last().emitOpen();
      });
      expect(result.current.live).toBe(true);
      expect(matrixFetchCount()).toBe(1);

      await act(async () => {
        await vi.advanceTimersByTimeAsync(2 * MATRIX_POLL_MS);
      });
      expect(matrixFetchCount()).toBe(1);
    } finally {
      vi.useRealTimers();
    }
  });

  // The capability says the ingester is healthy; it cannot see a CSP denial or
  // an Upgrade-stripping proxy on the browser side. Trusting it alone would
  // freeze the matrix on its first paint forever.
  it("keeps polling armed when the capability is advertised but the socket never opens", async () => {
    const { qc, wrapper } = setup(["events"]);
    const { result } = renderHook(() => useMatrix("tcp"), { wrapper });

    await waitFor(() => expect(result.current.data).toEqual(polled));
    expect(FakeSocket.instances).toHaveLength(1);
    expect(FakeSocket.last().readyState).toBe(0); // dialled, never established
    expect(matrixRefetchInterval(qc)).toBe(MATRIX_POLL_MS);
    expect(result.current.live).toBe(false);
  });

  it("re-arms polling when a live socket drops", async () => {
    const { qc, wrapper } = setup(["events"]);
    const { result } = renderHook(() => useMatrix("tcp"), { wrapper });

    await waitFor(() => expect(result.current.data).toEqual(polled));
    act(() => {
      FakeSocket.last().emitOpen();
    });
    expect(matrixRefetchInterval(qc)).toBe(false);

    act(() => {
      FakeSocket.last().emitClose();
    });
    expect(result.current.live).toBe(false);
    expect(matrixRefetchInterval(qc)).toBe(MATRIX_POLL_MS);
  });

  it("subscribes to the topic of the selected protocol only", async () => {
    const { wrapper } = setup(["events"]);
    const { result } = renderHook(() => useMatrix("udp"), { wrapper });
    await waitFor(() => expect(result.current.data).toEqual(polledFor("udp")));
    act(() => {
      FakeSocket.last().emitOpen();
    });
    expect(FakeSocket.last().sent).toEqual(['{"action":"subscribe","topic":"matrix:udp:pod"}']);
  });

  // Regression: a pushed TCP snapshot must never be written into the UDP key
  // and rendered under the UDP label while the REST call for UDP is in flight.
  it("never seeds another protocol's key with the snapshot it is still holding", async () => {
    const { qc, wrapper } = setup(["events"]);
    const { result, rerender } = renderHook(({ p }: { p: Protocol }) => useMatrix(p), {
      wrapper,
      initialProps: { p: "tcp" as Protocol },
    });

    await waitFor(() => expect(result.current.data).toEqual(polled));
    act(() => {
      FakeSocket.last().emitOpen();
      FakeSocket.last().emitEnvelope({ topic: matrixTopic("tcp"), type: "snapshot", seq: 1, data: pushed });
    });
    await waitFor(() => expect(result.current.data).toEqual(pushed));

    // Switch protocol. Synchronously — no awaiting, this is exactly the window
    // the bug lived in — the UDP key must be untouched and the hook must admit
    // it has nothing rather than serve TCP numbers.
    rerender({ p: "udp" });
    expect(qc.getQueryData(["matrix", "udp", "pod"])).toBeUndefined();
    expect(result.current.data).toBeUndefined();
    expect(FakeSocket.last().sent).toEqual([
      '{"action":"subscribe","topic":"matrix:tcp:pod"}',
      '{"action":"unsubscribe","topic":"matrix:tcp:pod"}',
      '{"action":"subscribe","topic":"matrix:udp:pod"}',
    ]);

    // The UDP key fills from REST, exactly as it would with realtime off…
    await waitFor(() => expect(result.current.data).toEqual(polledFor("udp")));

    // …and a real UDP frame is accepted normally, with TCP's entry left intact.
    act(() => {
      FakeSocket.last().emitEnvelope({
        topic: matrixTopic("udp"),
        type: "snapshot",
        seq: 1,
        data: udpPushed,
      });
    });
    await waitFor(() => expect(result.current.data).toEqual(udpPushed));
    expect(qc.getQueryData(["matrix", "udp", "pod"])).toEqual(udpPushed);
    expect(qc.getQueryData(["matrix", "tcp", "pod"])).toEqual(pushed);
  });
});
