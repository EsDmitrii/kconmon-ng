import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FakeSocket } from "@/lib/fake-websocket";
import { TimeMachineProvider } from "@/lib/timemachine";
import { MATRIX_POLL_MS, useMatrix } from "./use-matrix";
import { resetWsClient } from "./use-ws-topic";

/** useMatrix's THIRD path: engaged, the grid is rebuilt from PromQL at `t` and both live transports go quiet. */

const AT = "2026-08-01T12:00:00Z";

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

const vectorBody = {
  status: "success",
  data: {
    resultType: "vector",
    result: [{ metric: { source_node: "a", destination_node: "b" }, value: [1785276000, "0.25"] }],
  },
};

/** Advertises realtime so the socket WOULD open if anything still asked it to
 *  — the point of the "no socket" case below is that nothing does. */
function stubFetch() {
  const urls: string[] = [];
  const fetchMock = vi.fn((url: string) => {
    const href = String(url);
    urls.push(href);
    if (href.includes("/api/v1/version")) {
      return Promise.resolve(json({ version: "1.7.0", commit: "abc", capabilities: ["events"] }));
    }
    if (href.includes("/api/v1/promql/query")) return Promise.resolve(json(vectorBody));
    return Promise.resolve(json({ protocol: "tcp", plane: "pod", nodes: [], cells: [], timestamp: AT }));
  });
  vi.stubGlobal("fetch", fetchMock);
  return { urls, fetchMock };
}

function renderMatrix(qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
  return {
    qc,
    ...renderHook(() => useMatrix("tcp"), {
      wrapper: ({ children }) => (
        <QueryClientProvider client={qc}>
          <TimeMachineProvider>{children}</TimeMachineProvider>
        </QueryClientProvider>
      ),
    }),
  };
}

beforeEach(() => {
  FakeSocket.reset();
  vi.stubGlobal("WebSocket", FakeSocket);
  window.history.pushState({}, "", `/matrix?at=${AT}`);
});

afterEach(() => {
  cleanup();
  resetWsClient();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
});

describe("useMatrix engaged at t", () => {
  it("never touches GET /api/v1/matrix — that endpoint is live-only by design", async () => {
    const { urls } = stubFetch();
    const { result } = renderMatrix();
    await waitFor(() => expect(result.current.data).toBeDefined());
    expect(urls.filter((u) => u.startsWith("/api/v1/matrix"))).toEqual([]);
  });

  it("evaluates the matrix's own PromQL at t instead", async () => {
    const bodies: { query: string; time?: string }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string, init?: RequestInit) => {
        const href = String(url);
        if (href.includes("/api/v1/promql/query")) {
          bodies.push(JSON.parse(String(init?.body)) as { query: string; time?: string });
          return Promise.resolve(json(vectorBody));
        }
        return Promise.resolve(json({ version: "1.7.0", commit: "abc", capabilities: ["events"] }));
      }),
    );
    const { result } = renderMatrix();
    await waitFor(() => expect(result.current.data).toBeDefined());
    expect(bodies).toHaveLength(2); // TCP: fail + rtt, no packet-loss series
    // promqlQuery's own serialisation (toISOString, milliseconds) — unchanged
    // by this task. The instant is the same second `?at=` carries; only the
    // rendering differs, and Go's time.RFC3339 parse accepts both.
    for (const b of bodies) expect(b.time).toBe(new Date(AT).toISOString());
    expect(result.current.data?.cells[0]).toMatchObject({ source: "a", destination: "b", failRatio: 0.25 });
  });

  it("opens no websocket at all — a pushed frame is by definition now", async () => {
    stubFetch();
    const { result } = renderMatrix();
    await waitFor(() => expect(result.current.data).toBeDefined());
    expect(FakeSocket.instances).toHaveLength(0);
    expect(result.current.live).toBe(false);
  });

  it("does not poll", async () => {
    vi.useFakeTimers();
    const { fetchMock } = stubFetch();
    renderMatrix();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    const promqlCalls = () =>
      fetchMock.mock.calls.filter(([u]) => String(u).includes("/api/v1/promql/query")).length;
    const first = promqlCalls();
    expect(first).toBe(2);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(MATRIX_POLL_MS * 4);
    });
    expect(promqlCalls()).toBe(first);
  });

  it("writes into a per-instant cache key, leaving the live entry alone", async () => {
    stubFetch();
    const { result, qc } = renderMatrix();
    await waitFor(() => expect(result.current.data).toBeDefined());
    expect(qc.getQueryData(["matrix", "tcp", "pod"])).toBeUndefined();
    expect(qc.getQueryData(["matrix", "tcp", "pod", "at", AT])).toBeDefined();
  });
});
