import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { CAPABILITIES_POLL_MS, useCapabilities } from "./use-capabilities";

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function harness(capabilities?: string[]) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  if (capabilities !== undefined) {
    // Pre-seeding the cache makes the assertion synchronous, and pins the
    // ["version"] query key that use-matrix.ts and the Live page share.
    qc.setQueryData(["version"], { version: "1.6.0", commit: "abc123", capabilities });
  }
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>{children}</QueryClientProvider>
  );
  return { qc, wrapper };
}

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(json({ version: "1.6.0", commit: "abc123", capabilities: ["events"] })),
  );
});

afterEach(() => vi.unstubAllGlobals());

describe("useCapabilities", () => {
  it("polls /api/v1/version and reports realtime when events is advertised", async () => {
    const { wrapper } = harness();
    const { result } = renderHook(() => useCapabilities(), { wrapper });

    await waitFor(() => expect(result.current.realtime).toBe(true));
    expect(vi.mocked(fetch).mock.calls[0][0]).toBe("/api/v1/version");
  });

  it("reports no realtime for an empty capability list", () => {
    const { wrapper } = harness([]);
    const { result } = renderHook(() => useCapabilities(), { wrapper });
    expect(result.current.realtime).toBe(false);
  });

  it("reports no realtime for a controller/console that predates capability flags", () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    qc.setQueryData(["version"], { version: "1.5.0", commit: "abc123" });
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={qc}>{children}</QueryClientProvider>
    );
    const { result } = renderHook(() => useCapabilities(), { wrapper });
    expect(result.current.realtime).toBe(false);
  });

  it("polls on the repo's 15s cadence", () => {
    expect(CAPABILITIES_POLL_MS).toBe(15_000);
  });

  it("reports resolved only once an answer is in", async () => {
    vi.stubGlobal("fetch", vi.fn(() => new Promise(() => {})));
    const { wrapper } = harness();
    const { result } = renderHook(() => useCapabilities(), { wrapper });

    // No answer yet: `realtime === false` here means "we have not asked", and a
    // consumer that cannot tell the two apart warns on every cold load.
    expect(result.current.resolved).toBe(false);
    expect(result.current.realtime).toBe(false);
  });

  /**
   * `resolved` must be pending-shaped, not fetching-shaped. Every background
   * refetch (one per CAPABILITIES_POLL_MS, forever) is a fetch in flight over
   * data we already have, and a `resolved` built on isFetching would drop to
   * false on each one — flashing the Live page's skeleton over a working feed
   * four times a minute. Swapping the two would still pass every other test
   * here, so this pins it explicitly.
   */
  it("stays resolved across a background refetch", async () => {
    const { qc, wrapper } = harness(["events"]);
    const { result } = renderHook(() => useCapabilities(), { wrapper });
    expect(result.current.resolved).toBe(true);

    const inFlight = qc.refetchQueries({ queryKey: ["version"] });
    expect(qc.isFetching({ queryKey: ["version"] })).toBe(1);
    expect(result.current.resolved).toBe(true);

    await inFlight;
    expect(result.current.resolved).toBe(true);
    expect(result.current.realtime).toBe(true);
  });
});
