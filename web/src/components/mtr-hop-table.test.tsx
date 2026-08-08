import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import type { Enrichment, MTRHop, PathSnapshot } from "@/lib/types";
import { fmtRttNs, hopTrendSeries, isPlaceholderHop, lossTier, TraceDetail, type TrendHistory } from "./mtr-hop-table";

// EChart is mocked for the same reason target-card.test.tsx mocks it:
// echarts.init() wants a 2d canvas context jsdom does not implement, so a real
// mount throws. The mock also CAPTURES the option, which is how the trend's
// two load-bearing claims — milliseconds, and a gap rather than a zero — are
// asserted on the data that would actually reach the chart.
const captured = vi.hoisted(() => ({ options: [] as TrendOption[] }));
vi.mock("@/components/echart", () => ({
  EChart: ({ option }: { option: TrendOption }) => {
    captured.options.push(option);
    return <div data-testid="echart" />;
  },
}));

/** The slice of the ECharts option this file asserts on. */
interface TrendOption {
  series?: { data?: [number, number | null][] }[];
}

const lastSeries = () => captured.options[captured.options.length - 1]?.series?.[0]?.data;

function hop(over: Partial<MTRHop> = {}): MTRHop {
  return { number: 1, ip: "10.0.0.1", hostname: "gw.internal", rttNs: 2_500_000, lossRatio: 0, ...over };
}

function snapshot(over: Partial<PathSnapshot> = {}): PathSnapshot {
  return {
    id: "11111111-1111-1111-1111-111111111111",
    sourceNode: "node-a",
    destination: "node-b",
    pathHash: "aaaaaaaaaaaa0000",
    hopCount: 2,
    hops: [hop(), hop({ number: 2, ip: "203.0.113.9", hostname: "edge", rttNs: 41_000_000, lossRatio: 0 })],
    firstSeen: "2026-08-05T10:00:00Z",
    lastSeen: "2026-08-06T10:00:00Z",
    traceCount: 5,
    ...over,
  };
}

function enrichment(over: Partial<Enrichment> = {}): Enrichment {
  return {
    ip: "203.0.113.9",
    rdns: "edge9.example.net",
    asn: 64500,
    provider: "Example Transit",
    geo: { country: "GB", city: "London", lat: 51.5, lon: -0.12 },
    resolvedAt: "2026-08-06T10:00:00Z",
    ...over,
  };
}

function renderDetail(snap: PathSnapshot, history?: TrendHistory) {
  return render(
    <ThemeProvider>
      <TraceDetail snapshot={snap} history={history} />
    </ThemeProvider>,
  );
}

/** The panel an expander controls, addressed the way a screen reader would:
 *  through aria-controls, so "inside the expanded row" is asserted rather than
 *  "somewhere on the page". */
function panelOf(expander: HTMLElement): HTMLElement {
  const id = expander.getAttribute("aria-controls");
  const panel = id ? document.getElementById(id) : null;
  if (!panel) throw new Error("expander controls no panel");
  return panel;
}

afterEach(() => {
  cleanup();
  captured.options.length = 0;
});

describe("fmtRttNs", () => {
  it("renders the ns wire convention as milliseconds and keeps sub-ms hops visible", () => {
    expect(fmtRttNs(2_500_000)).toBe("2.5ms");
    expect(fmtRttNs(400_000)).toBe("0.4ms");
    expect(fmtRttNs(undefined)).toBe("—");
  });
});

describe("isPlaceholderHop", () => {
  it("treats the tracer's own no-answer markers as address-less", () => {
    expect(isPlaceholderHop("*")).toBe(true);
    expect(isPlaceholderHop("")).toBe(true);
    expect(isPlaceholderHop("   ")).toBe(true);
    expect(isPlaceholderHop("10.0.0.1")).toBe(false);
  });
});

describe("lossTier", () => {
  it("uses the matrix's own 1% / 10% thresholds rather than new ones", () => {
    expect(lossTier(0)).toBe("ok");
    expect(lossTier(0.009)).toBe("ok");
    expect(lossTier(0.01)).toBe("warn");
    expect(lossTier(0.099)).toBe("warn");
    expect(lossTier(0.1)).toBe("bad");
    expect(lossTier(1)).toBe("bad");
  });
});

describe("hopTrendSeries", () => {
  const s1 = snapshot({
    id: "s1",
    firstSeen: "2026-08-01T00:00:00Z",
    hops: [hop({ ip: "10.0.0.1", rttNs: 2_000_000 })],
  });
  // The middle route did NOT include 10.0.0.1 — the path changed.
  const s2 = snapshot({
    id: "s2",
    firstSeen: "2026-08-02T00:00:00Z",
    hops: [hop({ ip: "10.9.9.9", rttNs: 9_000_000 })],
  });
  const s3 = snapshot({
    id: "s3",
    firstSeen: "2026-08-03T00:00:00Z",
    hops: [hop({ ip: "10.0.0.1", rttNs: 4_500_000 })],
  });
  // The pane hands over the list newest-first, the way the API answers it.
  const newestFirst = [s3, s2, s1];

  it("plots oldest-first in milliseconds, not nanoseconds", () => {
    const series = hopTrendSeries(newestFirst, "10.0.0.1");

    expect(series.map(([t]) => t)).toEqual([
      Date.parse("2026-08-01T00:00:00Z"),
      Date.parse("2026-08-02T00:00:00Z"),
      Date.parse("2026-08-03T00:00:00Z"),
    ]);
    expect(series[0][1]).toBe(2);
    expect(series[2][1]).toBe(4.5);
  });

  it("leaves a GAP where the route did not include the hop — null, never zero", () => {
    const series = hopTrendSeries(newestFirst, "10.0.0.1");

    expect(series[1][1]).toBeNull();
    expect(series[1][1]).not.toBe(0);
  });

  it("is all gaps for an address none of the loaded paths carries", () => {
    expect(hopTrendSeries(newestFirst, "198.51.100.7").every(([, v]) => v === null)).toBe(true);
  });

  it("drops a snapshot whose firstSeen cannot be placed on a time axis", () => {
    expect(hopTrendSeries([snapshot({ firstSeen: "not-a-date" })], "10.0.0.1")).toEqual([]);
  });
});

describe("TraceDetail — enrichment row", () => {
  it("expands a hop and renders every enrichment field the map carries", () => {
    renderDetail(snapshot({ enrichment: { "203.0.113.9": enrichment() } }));

    const expander = screen.getByRole("button", { name: /hop 2/i });
    expect(expander).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(expander);
    expect(expander).toHaveAttribute("aria-expanded", "true");

    const panel = within(panelOf(expander));
    expect(panel.getByText("edge9.example.net")).toBeInTheDocument();
    expect(panel.getByText(/AS64500/)).toBeInTheDocument();
    expect(panel.getByText(/Example Transit/)).toBeInTheDocument();
    expect(panel.getByText(/London/)).toBeInTheDocument();
    expect(panel.getByText(/GB/)).toBeInTheDocument();
  });

  it("says nothing was recorded AND that enrichment may be off, inside the row, for an empty map", () => {
    renderDetail(snapshot({ enrichment: {} }));

    const expander = screen.getByRole("button", { name: /hop 1/i });
    fireEvent.click(expander);

    expect(within(panelOf(expander)).getByText(/no enrichment recorded/i)).toHaveTextContent(/may be disabled/i);
  });

  it("gives the same honest note when the response carried no enrichment field at all", () => {
    renderDetail(snapshot());

    fireEvent.click(screen.getByRole("button", { name: /hop 1/i }));

    expect(screen.getByText(/no enrichment recorded/i)).toBeInTheDocument();
  });

  it("gives a hop with SOME fields only the fields it has, with no note", () => {
    renderDetail(snapshot({ enrichment: { "10.0.0.1": { ip: "10.0.0.1", rdns: "gw.internal", resolvedAt: "x" } } }));

    fireEvent.click(screen.getByRole("button", { name: /hop 1/i }));

    expect(screen.getByText("gw.internal", { selector: "dd" })).toBeInTheDocument();
    expect(screen.queryByText(/no enrichment recorded/i)).not.toBeInTheDocument();
  });

  it("gives a placeholder hop no expander at all — there is no address to enrich", () => {
    renderDetail(snapshot({ hops: [hop({ number: 1, ip: "*", hostname: undefined, lossRatio: 1 })] }));

    expect(screen.queryByRole("button", { name: /hop 1/i })).not.toBeInTheDocument();
  });
});

describe("TraceDetail — loss column", () => {
  it("colours loss with the matrix's health tokens and leaves a clean hop quiet", () => {
    renderDetail(
      snapshot({
        hops: [
          hop({ number: 1, ip: "10.0.0.1", lossRatio: 0 }),
          hop({ number: 2, ip: "10.0.0.2", lossRatio: 0.05 }),
          hop({ number: 3, ip: "10.0.0.3", lossRatio: 0.4 }),
        ],
      }),
    );

    expect(screen.getByText("0%")).toHaveClass("text-muted-foreground");
    expect(screen.getByText("5%")).toHaveClass("text-health-warn");
    expect(screen.getByText("40%")).toHaveClass("text-health-bad");
  });
});

describe("TraceDetail — per-hop trend", () => {
  const older = snapshot({
    id: "older",
    firstSeen: "2026-08-01T00:00:00Z",
    traceCount: 3,
    hops: [hop({ ip: "10.0.0.1", rttNs: 2_000_000 })],
  });
  const current = snapshot({
    id: "11111111-1111-1111-1111-111111111111",
    firstSeen: "2026-08-03T00:00:00Z",
    traceCount: 2,
    hops: [hop({ ip: "10.0.0.1", rttNs: 4_000_000 })],
  });

  function history(over: Partial<TrendHistory> = {}): TrendHistory {
    return { snapshots: [current, older], hasOlder: false, traceTotal: 5, ...over };
  }

  it("charts the hop's RTT across the loaded paths only when asked", () => {
    renderDetail(current, history());

    expect(screen.queryByTestId("echart")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /trend for 10\.0\.0\.1/i }));

    expect(screen.getByTestId("echart")).toBeInTheDocument();
    expect(lastSeries()).toEqual([
      [Date.parse("2026-08-01T00:00:00Z"), 2],
      [Date.parse("2026-08-03T00:00:00Z"), 4],
    ]);
  });

  it("gaps the snapshots the hop is missing from instead of drawing a zero", () => {
    const rerouted = snapshot({
      id: "rerouted",
      firstSeen: "2026-08-02T00:00:00Z",
      hops: [hop({ ip: "10.9.9.9", rttNs: 9_000_000 })],
    });
    renderDetail(current, history({ snapshots: [current, rerouted, older] }));

    fireEvent.click(screen.getByRole("button", { name: /trend for 10\.0\.0\.1/i }));

    expect(lastSeries()?.[1]).toEqual([Date.parse("2026-08-02T00:00:00Z"), null]);
  });

  it("says the trend is partial when older paths of the pair have not been loaded", () => {
    renderDetail(current, history({ hasOlder: true }));

    fireEvent.click(screen.getByRole("button", { name: /trend for 10\.0\.0\.1/i }));

    expect(screen.getByText(/load older/i)).toBeInTheDocument();
  });

  it("says the trend is partial when the loaded paths cover fewer than the pair's traces", () => {
    renderDetail(current, history({ traceTotal: 40 }));

    fireEvent.click(screen.getByRole("button", { name: /trend for 10\.0\.0\.1/i }));

    expect(screen.getByText(/5 of the pair's 40 traces/i)).toBeInTheDocument();
  });

  it("claims nothing about coverage when the loaded paths ARE the pair's whole history", () => {
    renderDetail(current, history());

    fireEvent.click(screen.getByRole("button", { name: /trend for 10\.0\.0\.1/i }));

    expect(screen.queryByText(/load older/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/of the pair's/i)).not.toBeInTheDocument();
  });

  it("offers no trend for a placeholder hop, and none at all without a loaded history", () => {
    renderDetail(snapshot({ hops: [hop({ number: 1, ip: "*", hostname: undefined })] }), history());
    expect(screen.queryByRole("button", { name: /trend for/i })).not.toBeInTheDocument();

    cleanup();
    renderDetail(current);
    expect(screen.queryByRole("button", { name: /trend for/i })).not.toBeInTheDocument();
  });

  it("closes the trend when its own toggle is pressed again", () => {
    renderDetail(current, history());

    const toggle = screen.getByRole("button", { name: /trend for 10\.0\.0\.1/i });
    fireEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-pressed", "true");
    fireEvent.click(toggle);

    expect(screen.queryByTestId("echart")).not.toBeInTheDocument();
  });
});
