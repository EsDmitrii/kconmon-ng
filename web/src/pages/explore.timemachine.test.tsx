import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { TimeMachineProvider } from "@/lib/timemachine";
import { ExplorePage } from "./explore";

/** /explore engaged: the range picker keeps working, anchored BACK from `t` rather than from now. */

const AT = "2026-08-01T12:00:00Z";
const AT_MS = Date.parse(AT);

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

interface RangeBody {
  query: string;
  start: string;
  end: string;
  step: number;
}

function stubFetch() {
  const bodies: RangeBody[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    if (String(url).includes("/api/v1/promql/query_range")) {
      bodies.push(JSON.parse(String(init?.body)) as RangeBody);
      return Promise.resolve(json({ status: "success", data: { resultType: "matrix", result: [] } }));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);
  return { bodies, fetchMock };
}

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TimeMachineProvider>
          <ExplorePage />
        </TimeMachineProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  window.history.pushState({}, "", `/explore?at=${AT}`);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
});

describe("ExplorePage engaged at t", () => {
  it("ends every window at t rather than at now", async () => {
    const { bodies } = stubFetch();
    renderPage();
    await waitFor(() => expect(bodies.length).toBeGreaterThan(0));
    for (const b of bodies) expect(Date.parse(b.end)).toBe(AT_MS);
  });

  it("measures the selected range BACKWARDS from t, so the picker still means what it says", async () => {
    const { bodies } = stubFetch();
    renderPage();
    await waitFor(() => expect(bodies.length).toBeGreaterThan(0));
    // Default range is 1h.
    expect(AT_MS - Date.parse(bodies[0].start)).toBe(60 * 60 * 1000);

    bodies.length = 0;
    fireEvent.click(screen.getByRole("radio", { name: "6h" }));
    await waitFor(() => expect(bodies.length).toBeGreaterThan(0));
    expect(Date.parse(bodies[0].end)).toBe(AT_MS);
    expect(AT_MS - Date.parse(bodies[0].start)).toBe(6 * 60 * 60 * 1000);
  });

  it("stops polling — a window that ends at a fixed instant redraws an identical line", async () => {
    vi.useFakeTimers();
    const { bodies } = stubFetch();
    renderPage();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    const first = bodies.length;
    expect(first).toBeGreaterThan(0);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(120_000);
    });
    expect(bodies.length).toBe(first);
  });

  it("says in the header what the range is measured back from", async () => {
    stubFetch();
    renderPage();
    await screen.findByText(/the range below is measured back from there/);
  });
});

describe("ExplorePage while live", () => {
  it("still ends at now", async () => {
    window.history.pushState({}, "", "/explore");
    const { bodies } = stubFetch();
    const before = Date.now();
    renderPage();
    await waitFor(() => expect(bodies.length).toBeGreaterThan(0));
    const end = Date.parse(bodies[0].end);
    /* "Now", floored onto the sample grid: exploreWindow aligns the anchor so
       the compare panel's two legs share sample instants, which puts the end at
       most one step behind the wall clock and never ahead of it. The 15s step is
       the smallest this page ever asks for, so it is the whole slack. */
    expect(end).toBeGreaterThanOrEqual(before - 15_000);
    expect(end).toBeLessThanOrEqual(Date.now());
  });
});
