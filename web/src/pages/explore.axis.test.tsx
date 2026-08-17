import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { TimeMachineProvider } from "@/lib/timemachine";
import { ExplorePage } from "./explore";

/*
The range presets looked like they only redrew the curves: 6h and 24h produced the same axis,
because ECharts fit it to the data and this stand's Prometheus holds a few hours (owner report).
The axis is now the span that was ASKED for, so a window with no samples in it is visible as a
window with no samples in it.
*/

vi.mock("@/components/echart", () => ({
  EChart: ({ option, className }: { option?: { xAxis?: { min?: number; max?: number } }; className?: string }) => (
    <div
      data-testid="echart"
      className={className}
      data-xmin={String(option?.xAxis?.min ?? "")}
      data-xmax={String(option?.xAxis?.max ?? "")}
    />
  ),
}));

const NOW = new Date("2026-08-01T12:00:00Z");
const HOUR = 60 * 60 * 1000;

/* Three hours of samples, whatever window is asked for — the stand's own shape. */
const SAMPLES: [number, string][] = [
  [(NOW.getTime() - 3 * HOUR) / 1000, "0.1"],
  [(NOW.getTime() - HOUR) / 1000, "0.2"],
  [NOW.getTime() / 1000, "0.15"],
];

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(NOW);
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) =>
      Promise.resolve(
        new Response(
          JSON.stringify(
            String(url).includes("/api/v1/promql/query_range")
              ? { status: "success", data: { resultType: "matrix", result: [{ metric: { protocol: "tcp" }, values: SAMPLES }] } }
              : {},
          ),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      ),
    ),
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

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

/** The span of the first curated card's axis, in hours. */
async function axisHours(): Promise<number> {
  const chart = (await screen.findAllByTestId("echart"))[0];
  const min = Number(chart.getAttribute("data-xmin"));
  const max = Number(chart.getAttribute("data-xmax"));
  expect(Number.isFinite(min) && Number.isFinite(max)).toBe(true);
  return (max - min) / HOUR;
}

describe("Explore's time axis", () => {
  it("spans the picked range, not the span of the data that came back", async () => {
    renderPage();
    // The page opens on 1h; the fixture carries three hours of samples, so an
    // axis fitted to the DATA would be wider than the window.
    await waitFor(async () => expect(await axisHours()).toBeCloseTo(1, 1));

    fireEvent.click(screen.getByRole("radio", { name: "6h" }));
    await waitFor(async () => expect(await axisHours()).toBeCloseTo(6, 1));

    fireEvent.click(screen.getByRole("radio", { name: "24h" }));
    await waitFor(async () => expect(await axisHours()).toBeCloseTo(24, 1));
  });

  it("ends at now, so the newest sample sits at the right edge", async () => {
    renderPage();
    const chart = (await screen.findAllByTestId("echart"))[0];
    await waitFor(() => expect(Number(chart.getAttribute("data-xmax"))).toBeGreaterThan(0));
    // Floored onto the step grid by exploreWindow, so "now" within one step.
    expect(NOW.getTime() - Number(chart.getAttribute("data-xmax"))).toBeLessThan(60_000);
  });
});
