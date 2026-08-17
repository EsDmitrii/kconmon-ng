import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import type { Enrichment, MTRHop, PathSnapshot } from "@/lib/types";
import {
  fmtLossPct,
  fmtRttNs,
  hopList,
  hopTrendSeries,
  isPlaceholderHop,
  lossTier,
  shortHash,
  TraceDetail,
  trendExtent,
  type TrendHistory,
} from "./mtr-hop-table";

// EChart is mocked for the same reason target-card.test.tsx mocks it; the mock also CAPTURES the
// option, which is how the trend's two load-bearing claims.
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
  xAxis?: { min?: number; max?: number };
  yAxis?: { axisLabel?: { formatter?: (v: number) => string } };
}

const lastSeries = () => captured.options[captured.options.length - 1]?.series?.[0]?.data;
const lastOption = () => captured.options[captured.options.length - 1];

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

  /* QA scope 4, finding #14: a same-node first hop answers in tens of
     microseconds, and one decimal of a millisecond printed it as "0.0ms" —
     a measurement erased rather than reported. */
  it("switches to microseconds below 0.1ms rather than rounding a real hop to zero", () => {
    expect(fmtRttNs(22_000)).toBe("22µs");
    expect(fmtRttNs(1_500)).toBe("2µs");
    // The boundary stays in milliseconds: 0.1ms has a decimal that holds it.
    expect(fmtRttNs(100_000)).toBe("0.1ms");
    expect(fmtRttNs(99_999)).toBe("100µs");
  });

  it("keeps a genuine ZERO in milliseconds — 0µs would read as a measurement", () => {
    expect(fmtRttNs(0)).toBe("0.0ms");
  });

  /* Hostile QA: a JSON null divided by 1e6 is 0, so an ABSENT reading printed
     "0.0ms" — a hop reported as answering in no time at all. Everything that is
     not a finite number is an absence, and the table already has a mark for
     one. */
  it("prints the absence mark for anything that is not a finite number", () => {
    expect(fmtRttNs(null as unknown as number)).toBe("—");
    expect(fmtRttNs("fast" as unknown as number)).toBe("—");
    expect(fmtRttNs(Number.POSITIVE_INFINITY)).toBe("—");
    expect(fmtRttNs(Number.NaN)).toBe("—");
  });

  /* The microsecond floor reads the MAGNITUDE, so a negative reading — the
     wire's nonsense, not the console's — keeps its sign instead of being
     rounded into "-0.0ms". */
  it("does not round a negative reading into a signed zero", () => {
    expect(fmtRttNs(-4_000)).toBe("-4µs");
    expect(fmtRttNs(-2_500_000)).toBe("-2.5ms");
  });
});

describe("fmtLossPct", () => {
  it("renders the ratio as whole percent", () => {
    expect(fmtLossPct(0)).toBe("0%");
    expect(fmtLossPct(0.1)).toBe("10%");
    expect(fmtLossPct(1)).toBe("100%");
  });

  /* Hostile QA: an absent lossRatio rendered "NaN%" in red, and a ratio of 12
     rendered "1200%". The schema says 0..1; anything else is a payload this
     server did not write. */
  it("says nothing rather than NaN, and stays inside the range the schema promises", () => {
    expect(fmtLossPct(undefined)).toBe("—");
    expect(fmtLossPct(null as unknown as number)).toBe("—");
    expect(fmtLossPct(-0.5)).toBe("0%");
    expect(fmtLossPct(12)).toBe("100%");
  });
});

describe("hopList", () => {
  it("passes a real list through and answers an empty one for everything else", () => {
    const hops = [hop()];
    expect(hopList(hops)).toBe(hops);
    expect(hopList(null)).toEqual([]);
    expect(hopList(undefined)).toEqual([]);
    expect(hopList("nope" as unknown as MTRHop[])).toEqual([]);
  });
});

describe("shortHash", () => {
  it("keeps twelve characters, and answers an empty string for a hash that never arrived", () => {
    expect(shortHash("aaaaaaaaaaaa0000")).toBe("aaaaaaaaaaaa");
    expect(shortHash(undefined as unknown as string)).toBe("");
  });
});

describe("isPlaceholderHop", () => {
  it("treats the tracer's own no-answer markers as address-less", () => {
    expect(isPlaceholderHop("*")).toBe(true);
    expect(isPlaceholderHop("")).toBe(true);
    expect(isPlaceholderHop("   ")).toBe(true);
    expect(isPlaceholderHop("10.0.0.1")).toBe(false);
  });

  /* A hop with no `ip` key at all is the same absence, and .trim() on the
     undefined it leaves behind is a page-killing TypeError. */
  it("treats a missing address as an absence rather than throwing on it", () => {
    expect(isPlaceholderHop(undefined as unknown as string)).toBe(true);
    expect(isPlaceholderHop(null as unknown as string)).toBe(true);
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

  /* A loss number that is not a number is an UNKNOWN hop, not a failing one;
     without the guard every threshold below was false and the row came out red
     next to a cell reading "—". */
  it("does not paint an unknown hop as a failing one", () => {
    expect(lossTier(undefined as unknown as number)).toBe("ok");
    expect(lossTier(Number.NaN)).toBe("ok");
  });
});

describe("TraceDetail — payloads this server would not send", () => {
  it("renders a snapshot whose hops field is not a list at all", () => {
    renderDetail(snapshot({ hops: null as unknown as MTRHop[] }));
    expect(screen.getByRole("table", { name: "Hops" })).toBeTruthy();
    expect(document.body.textContent).not.toMatch(/NaN|undefined/);
  });

  it("draws the silence mark for a hop with no address, not an empty cell", () => {
    renderDetail(snapshot({ hopCount: 1, hops: [hop({ ip: "", hostname: "" })] }));
    const cells = within(screen.getByRole("table", { name: "Hops" })).getAllByRole("cell");
    expect(cells.map((c) => c.textContent)).toContain("*");
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

/**
 * A pair with ONE stored path drew a single symbol marooned at the left edge of whatever "nice"
 * interval ECharts picked.
 */
describe("trendExtent", () => {
  const t = (iso: string) => Date.parse(iso);

  it("gives a lone sample an hour either side rather than a degenerate axis", () => {
    const at = t("2026-08-01T12:00:00Z");
    expect(trendExtent([[at, 3]])).toEqual({ min: at - 3_600_000, max: at + 3_600_000 });
  });

  it("pads a real extent by 5% at each end, so the end symbols clear the axis", () => {
    const lo = t("2026-08-01T00:00:00Z");
    const hi = t("2026-08-01T10:00:00Z");
    const pad = (hi - lo) * 0.05;
    expect(trendExtent([[lo, 1], [hi, 2]])).toEqual({ min: lo - pad, max: hi + pad });
  });

  it("measures only the points that were MEASURED — a gap has no position to pin to", () => {
    const lo = t("2026-08-01T00:00:00Z");
    const mid = t("2026-08-02T00:00:00Z");
    const hi = t("2026-08-03T00:00:00Z");
    // The route dropped the hop on the last snapshot; the axis must not
    // stretch to cover a measurement that never happened.
    expect(trendExtent([[lo, 1], [mid, 2], [hi, null]])?.max).toBeLessThan(hi);
  });

  it("answers undefined with nothing to measure, so the caller leaves the axis alone", () => {
    expect(trendExtent([])).toBeUndefined();
    expect(trendExtent([[1, null]])).toBeUndefined();
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

/* QA scope 4, finding #6: a long rDNS name pushed RTT and Loss off the right
   edge of the card, and the card clipped them without a word. */
describe("TraceDetail — horizontal overflow", () => {
  const LONG = "edge-router-09.transit.lon.eu-west.example-networks.internal";

  it("keeps RTT and Loss on screen next to a long name", () => {
    renderDetail(snapshot({ hops: [hop({ hostname: LONG })] }));

    // The columns it used to push out are still rendered.
    expect(screen.getByText("Loss")).toBeInTheDocument();
    expect(screen.getByText("RTT")).toBeInTheDocument();
  });

  it("says nothing about scrolling while nothing is off-screen", () => {
    renderDetail(snapshot());
    // jsdom lays nothing out, so scrollWidth === clientWidth === 0: the
    // affordance must be driven by real overflow, not drawn unconditionally.
    expect(screen.queryByRole("note")).not.toBeInTheDocument();
  });

  it("offers the hint once the table really does run past its card", () => {
    const { container } = renderDetail(snapshot());
    const scroller = container.querySelector(".overflow-x-auto") as HTMLElement;
    Object.defineProperty(scroller, "scrollWidth", { value: 800, configurable: true });
    Object.defineProperty(scroller, "clientWidth", { value: 400, configurable: true });

    fireEvent.scroll(scroller);
    expect(screen.getByRole("note")).toHaveTextContent(/scroll sideways/i);
  });
});

/* «hostname обрезается»: `10-244-4-21.kconmon-kconmon-ng-…` clipped mid-token
   while the column beside it stood half empty. The cap that did it — a hard
   14rem on the cell — was the fix for QA scope 4 #6 (a long name shoving RTT and
   Loss off the card) applied to the wrong column: the name is the widest thing
   in the row and the ONLY thing here that is not a fixed-shape field. */
describe("TraceDetail — the hostname column", () => {
  /* The owner's own address, at the length a Kubernetes pod rDNS actually
     reaches. Pinned by count so a future trim of the fixture cannot quietly
     weaken what this file claims. */
  const POD_RDNS = "10-244-4-21.kconmon-kconmon-ng-agents.kconmon.svc.cluster.local";

  it("is a 63-character name, whole in the DOM and whole in the title", () => {
    expect(POD_RDNS).toHaveLength(63);
    renderDetail(snapshot({ hops: [hop({ hostname: POD_RDNS })] }));

    // Present as TEXT, not merely as a tooltip: nothing elides it in the markup.
    const cell = screen.getByText(POD_RDNS);
    expect(cell).toHaveAttribute("title", POD_RDNS);
    expect(cell.textContent).toBe(POD_RDNS);
  });

  /* The layout contract, pinned so the columns cannot be inverted again. The
     table is FIXED — without that, a single unbreakable 63-character token
     widens its own column and shoves the numerics out, which is the bug the
     14rem cap was reaching for. Given fixed layout, whoever has no width takes
     whatever is left, and that has to be Hostname. */
  it("gives every column but Hostname a pinned width, and Hostname the rest", () => {
    const { container } = renderDetail(snapshot());

    const table = container.querySelector("table") as HTMLTableElement;
    expect(table.className).toMatch(/\btable-fixed\b/);

    /* Order AND pinned-ness in one assertion: exactly one `false`, on the name.
       Address is a fixed-width mono field; RTT and Loss are narrow numerics. */
    const cols = [...container.querySelectorAll("colgroup col")];
    expect(cols.map((c) => [c.getAttribute("data-col"), /(?:^|\s)w-/.test(c.className)])).toEqual([
      ["expand", true],
      ["number", true],
      ["address", true],
      ["hostname", false],
      ["rtt", true],
      ["loss", true],
    ]);
  });

  // The inversion guard proper: the cap and the clip may not come back.
  it("neither caps nor clips the name cell", () => {
    renderDetail(snapshot({ hops: [hop({ hostname: POD_RDNS })] }));

    const cell = screen.getByText(POD_RDNS);
    expect(cell.className).not.toMatch(/\btruncate\b/);
    expect(cell.className).not.toMatch(/max-w-/);
    // It wraps INSIDE its column instead — a name on two lines is readable, a
    // name cut mid-token is not.
    expect(cell).toHaveClass("break-all");
  });

  // A hop with no name still reads as a hole rather than as empty space.
  it("keeps the em dash for a hop with no name, and no title on it", () => {
    renderDetail(snapshot({ hops: [hop({ hostname: "" })] }));

    const cell = screen.getAllByText("—")[0];
    expect(cell).not.toHaveAttribute("title");
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

  it("pins the x-axis to the data's own padded extent", () => {
    renderDetail(current, history());

    fireEvent.click(screen.getByRole("button", { name: /trend for 10\.0\.0\.1/i }));

    const lo = Date.parse("2026-08-01T00:00:00Z");
    const hi = Date.parse("2026-08-03T00:00:00Z");
    expect(lastOption()?.xAxis?.min).toBe(lo - (hi - lo) * 0.05);
    expect(lastOption()?.xAxis?.max).toBe(hi + (hi - lo) * 0.05);
  });

  it("gives a single-sample trend a ±1h window rather than an axis of one instant", () => {
    renderDetail(current, history({ snapshots: [current], traceTotal: 2 }));

    fireEvent.click(screen.getByRole("button", { name: /trend for 10\.0\.0\.1/i }));

    const at = Date.parse("2026-08-03T00:00:00Z");
    expect(lastOption()?.xAxis?.max! - lastOption()?.xAxis?.min!).toBe(2 * 60 * 60 * 1000);
    expect(lastOption()?.xAxis?.min).toBe(at - 60 * 60 * 1000);
  });

  it("labels the y-axis with the console's ONE millisecond rule, not a private toFixed(1)", () => {
    renderDetail(current, history());

    fireEvent.click(screen.getByRole("button", { name: /trend for 10\.0\.0\.1/i }));

    const fmt = lastOption()?.yAxis?.axisLabel?.formatter;
    expect(fmt?.(2.5)).toBe("2.5ms");
    expect(fmt?.(123.4)).toBe("123ms");
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

/* the pane in Russian Everything above renders with no <LocaleProvider>, which lib/i18n defines as English. */

describe("TraceDetail — Russian", () => {
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

  function renderRu(history: TrendHistory) {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    render(
      <LocaleProvider>
        <ThemeProvider>
          <TraceDetail snapshot={current} history={history} />
        </ThemeProvider>
      </LocaleProvider>,
    );
  }

  afterEach(() => {
    // vitest.setup.ts backs localStorage with ONE Map per test FILE.
    localStorage.removeItem(LOCALE_STORAGE_KEY);
  });

  it("names the table, its columns and the snapshot's own three figures", () => {
    renderRu({ snapshots: [current, older], hasOlder: false, traceTotal: 5 });

    expect(screen.getByRole("table", { name: "Хопы" })).toBeInTheDocument();
    for (const column of ["Адрес", "Имя хоста", "Потери"]) {
      expect(screen.getByRole("columnheader", { name: column })).toBeInTheDocument();
    }
    expect(screen.getByText("Впервые виден")).toBeInTheDocument();
    expect(screen.getByText("Трассировок")).toBeInTheDocument();
  });

  it("picks the FEW form for two paths", () => {
    renderRu({ snapshots: [current, older], hasOlder: true, traceTotal: 5 });
    fireEvent.click(screen.getByRole("button", { name: "Тренд RTT для 10.0.0.1" }));
    expect(screen.getByText(/^Тренд покрывает загруженные здесь 2 пути\./)).toBeInTheDocument();
  });

  it("picks the ONE form for a single path, and for a trace count ending in 1", () => {
    renderRu({ snapshots: [current], hasOlder: false, traceTotal: 21 });
    fireEvent.click(screen.getByRole("button", { name: "Тренд RTT для 10.0.0.1" }));
    // «1 путь», not «1 пути»; «21 трассировка», not «21 трассировок».
    expect(screen.getByText(/1 путь \(2 из 21 трассировка у этой пары\)/)).toBeInTheDocument();
  });

  it("picks the MANY form for a trace count in the teens and above four", () => {
    renderRu({ snapshots: [current, older], hasOlder: false, traceTotal: 40 });
    fireEvent.click(screen.getByRole("button", { name: "Тренд RTT для 10.0.0.1" }));
    expect(screen.getByText(/\(5 из 40 трассировок у этой пары\)/)).toBeInTheDocument();
  });

  it("translates the enrichment note, keeping the config key an operator would grep for", () => {
    renderRu({ snapshots: [current], hasOlder: false, traceTotal: 2 });
    fireEvent.click(screen.getByRole("button", { name: "Обогащение для хопа 1, 10.0.0.1" }));
    const note = screen.getByText(/обогащения не записано/);
    expect(note).toHaveTextContent("console.mtr.enrichment.enabled");
  });
});
