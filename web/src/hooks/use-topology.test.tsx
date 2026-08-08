import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { TimeMachineProvider } from "@/lib/timemachine";
import { TOPOLOGY_POLL_MS, useTopology } from "./use-topology";

/**
 * Both halves of useTopology live here: the live path (unchanged since M1 —
 * these cases exist so a Time Machine change that broke it would be caught by
 * THIS file rather than by a page test three layers up) and the engaged path.
 *
 * The Time Machine is driven the way the app drives it: through the URL. The
 * provider reads `?at=` on mount (readAtFromLocation), so pushing a location
 * before render is the whole setup — no mocked context, no test-only setter,
 * and therefore no way for these tests to agree with an implementation that a
 * real shared link would disagree with.
 */

const AT = "2026-08-01T12:00:00Z";
/** What the request must carry: URLSearchParams-style encoding of the same
 *  RFC 3339 instant `?at=` holds, colons percent-encoded. */
const AT_ENCODED = "2026-08-01T12%3A00%3A00Z";

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

const liveBody = { nodes: [{ name: "a", zone: "z", ready: true }], agents: [], timestamp: AT };

const historicalBody = {
  nodes: [],
  agents: [],
  timestamp: AT,
  historical: true,
  asOf: AT,
  eventsFolded: 3,
  unfoldableEvents: 417,
  truncated: false,
};

function stub(body: unknown) {
  const urls: string[] = [];
  const fetchMock = vi.fn((url: string) => {
    urls.push(String(url));
    return Promise.resolve(json(body));
  });
  vi.stubGlobal("fetch", fetchMock);
  return { urls, fetchMock };
}

function renderTopology() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderHook(() => useTopology(), {
    wrapper: ({ children }) => (
      <QueryClientProvider client={qc}>
        <TimeMachineProvider>{children}</TimeMachineProvider>
      </QueryClientProvider>
    ),
  });
}

beforeEach(() => {
  window.history.pushState({}, "", "/topology");
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("useTopology while live", () => {
  it("asks for the plain endpoint with no at param at all", async () => {
    const { urls } = stub(liveBody);
    const { result } = renderTopology();
    await waitFor(() => expect(result.current.data).toBeDefined());
    expect(urls).toEqual(["/api/v1/topology"]);
  });

  // Fake timers go in BEFORE render on purpose: react-query arms the
  // refetchInterval while the query mounts, and a timer armed against the real
  // clock is invisible to a fake one installed afterwards — the poll would then
  // look switched off in a test whose whole point is that it is not.
  it("keeps polling every TOPOLOGY_POLL_MS", async () => {
    vi.useFakeTimers();
    const { fetchMock } = stub(liveBody);
    renderTopology();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    const afterFirst = fetchMock.mock.calls.length;
    expect(afterFirst).toBe(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(TOPOLOGY_POLL_MS + 100);
    });
    expect(fetchMock.mock.calls.length).toBeGreaterThan(afterFirst);
  });
});

describe("useTopology engaged at t", () => {
  it("asks for ?at= carrying the instant formatted exactly as the URL holds it", async () => {
    window.history.pushState({}, "", `/topology?at=${AT}`);
    const { urls } = stub(historicalBody);
    const { result } = renderTopology();
    await waitFor(() => expect(result.current.data).toBeDefined());
    expect(urls).toEqual([`/api/v1/topology?at=${AT_ENCODED}`]);
    // Decoding must yield the instant itself, not a mangled one — this is the
    // string a shared link and the request have to agree on.
    expect(new URL(urls[0], "http://x").searchParams.get("at")).toBe(AT);
  });

  it("truncates sub-second precision rather than letting it reach the wire", async () => {
    window.history.pushState({}, "", "/topology?at=2026-08-01T12:00:00.987Z");
    const { urls } = stub(historicalBody);
    const { result } = renderTopology();
    await waitFor(() => expect(result.current.data).toBeDefined());
    expect(new URL(urls[0], "http://x").searchParams.get("at")).toBe(AT);
  });

  it("does not poll — a past instant cannot change, so a refetch can only cost", async () => {
    vi.useFakeTimers();
    window.history.pushState({}, "", `/topology?at=${AT}`);
    const { fetchMock } = stub(historicalBody);
    renderTopology();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(fetchMock.mock.calls.length).toBe(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(TOPOLOGY_POLL_MS * 4);
    });
    expect(fetchMock.mock.calls.length).toBe(1);
  });

  it("exposes the fold's honesty fields so a page can explain an empty answer", async () => {
    window.history.pushState({}, "", `/topology?at=${AT}`);
    stub(historicalBody);
    const { result } = renderTopology();
    await waitFor(() => expect(result.current.data).toBeDefined());
    expect(result.current.data).toMatchObject({
      historical: true,
      asOf: AT,
      eventsFolded: 3,
      unfoldableEvents: 417,
      truncated: false,
      nodes: [],
    });
  });

  it("caches per instant, so the live entry is never overwritten by a historical answer", async () => {
    window.history.pushState({}, "", `/topology?at=${AT}`);
    stub(historicalBody);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { result } = renderHook(() => useTopology(), {
      wrapper: ({ children }) => (
        <QueryClientProvider client={qc}>
          <TimeMachineProvider>{children}</TimeMachineProvider>
        </QueryClientProvider>
      ),
    });
    await waitFor(() => expect(result.current.data).toBeDefined());
    expect(qc.getQueryData(["topology"])).toBeUndefined();
    expect(qc.getQueryData(["topology", "at", AT])).toMatchObject({ historical: true });
  });
});
