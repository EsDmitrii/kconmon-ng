import { createContext, useContext, useState, type ReactNode } from "react";
import type * as echarts from "echarts";
import { capTooltipRows, type AxisTooltipRow } from "./chart-tooltip";

/**
 * chart-cursor.tsx — the shared time cursor, Grafana-style: hover one panel and
 * every other panel on the page draws a vertical line at the SAME INSTANT, so a
 * spike on one metric can be eyeballed against its neighbours.
 *
 * It is the Investigate page's mechanic, generalised. That page already sent an
 * instant from the timeline into its signal charts (investigation-timeline's
 * `onCursor`); this is the same idea with the wiring taken out of one page and
 * given to all of them — the timeline now publishes into the group, the charts
 * subscribe to it, and hovering a chart publishes too.
 *
 * Two decisions worth stating:
 *
 *   - it carries a TIMESTAMP, never a pixel. Two panels on a page rarely share
 *     an x-domain (Explore's compare leg is shifted 24h, MTR's loss strip spans
 *     the snapshot window), so every chart converts the instant with its own
 *     axis.
 *
 *   - it is NOT React state. A mousemove fires per pixel; routing that through
 *     setState would re-render a page of charts sixty times a second. The group
 *     is a plain pub/sub with one notification per animation frame, and the
 *     subscribers move a DOM line rather than re-rendering anything.
 */

/** Who published — the hovered chart skips its own echo. */
export type CursorSource = string;

export interface CursorGroup {
  /** Publish an instant (epoch ms) or null for "nothing hovered". */
  set(at: number | null, source: CursorSource): void;
  subscribe(fn: (at: number | null, source: CursorSource) => void): () => void;
  /** The last published instant, for a chart that mounts mid-hover. */
  current(): number | null;
  /**
   * Who published it. A chart LEAVING the page has to know whether the standing
   * cursor is its own before it clears it: a page of charts is not a fixed set
   * (Explore polls every thirty seconds and swaps compare legs on a click), and
   * clearing unconditionally meant any card unmounting wiped the crosshair off
   * every other panel with the mouse still resting on the one that drew it.
   */
  currentSource(): CursorSource;
}

/** How a frame is booked. Injected so a test can drive frames by hand. */
type Schedule = (fn: () => void) => unknown;

const defaultSchedule: Schedule = (fn) =>
  typeof requestAnimationFrame === "function" ? requestAnimationFrame(() => fn()) : setTimeout(fn, 16);

export function createCursorGroup(schedule: Schedule = defaultSchedule): CursorGroup {
  const subscribers = new Set<(at: number | null, source: CursorSource) => void>();
  let latest: number | null = null;
  let source: CursorSource = "";
  let queued = false;

  const flush = () => {
    queued = false;
    for (const fn of subscribers) {
      // One chart's failure must not strand the cursor on every other panel.
      try {
        fn(latest, source);
      } catch {
        /* ignored on purpose */
      }
    }
  };

  return {
    set(at, from) {
      latest = at;
      source = from;
      if (queued) return;
      queued = true;
      schedule(flush);
    },
    subscribe(fn) {
      subscribers.add(fn);
      return () => subscribers.delete(fn);
    },
    current: () => latest,
    currentSource: () => source,
  };
}

const CursorContext = createContext<CursorGroup | null>(null);

/**
 * ChartCursorProvider marks one sync group. PageShell mounts it, which makes "a
 * page" the scope without any page having to opt in.
 */
export function ChartCursorProvider({ children }: { children: ReactNode }) {
  const [group] = useState(() => createCursorGroup());
  return <CursorContext.Provider value={group}>{children}</CursorContext.Provider>;
}

/** null outside a provider: a chart mounted on its own is simply not synced. */
export function useChartCursor(): CursorGroup | null {
  return useContext(CursorContext);
}

/**
 * isTimeSeriesOption is the gate: a shared TIME cursor is meaningless on a chart
 * whose x-axis is a category (the MTR hop table's per-hop columns), so those
 * charts neither publish nor draw.
 */
export function isTimeSeriesOption(option: echarts.EChartsOption): boolean {
  const axis = Array.isArray(option.xAxis) ? option.xAxis[0] : option.xAxis;
  return (axis as { type?: string } | undefined)?.type === "time";
}

/* ── what a neighbour marks at the shared instant ───────────────────────── */

/**
 * The owner asked twice, so the vertical line alone was the wrong answer twice.
 * On Investigate → Signals (Packet loss above, RTT p95 below) and on Explore he
 * hovers one panel to read the SAME INSTANT on its neighbours, and a bare line
 * tells him only where in time he already knew he was.
 *
 * So a neighbour marks its own curve instead: at the shared instant, a dot on
 * each series' own point, in that series' own colour. The NUMBERS stay with the
 * panel under the pointer, which already has a tooltip listing them — the third
 * report on this mechanic was that a box on every neighbour covers the very
 * curves it annotates, and reading one panel should not mean reading six boxes.
 *
 * What a neighbour still does NOT draw is the horizontal half of the cross. The
 * panels' y-axes are different quantities — a 53.9% height carried down from the
 * loss panel lands on a millisecond value on the RTT panel and means nothing
 * there. The dot is the honest replacement for it: not "the pointer is at this
 * height", but "this series' sample is HERE".
 *
 * Everything below is PURE. The drawing lives in components/echart.tsx and runs
 * inside the group's one animation frame; keeping the arithmetic out of the
 * component is what lets the honesty rules be pinned by tests that never mount
 * a chart.
 */

/**
 * How many series a neighbour's box may name.
 *
 * Five, not the tooltip's ten. The tooltip belongs to the panel being pointed
 * at and may take room; this box sits on a panel the reader is NOT looking at,
 * often an h-40 signal strip 160px tall, and ten rows would cover the data it
 * is supposed to be annotating. Five is also exactly the worst-5 panel the
 * owner named, so its every series gets a row.
 */
export const READOUT_ROW_CAP = 5;

/** One series of a chart, reduced to what a readout needs. */
export interface ReadoutSeries {
  /** The legend name, verbatim; "" for a series that declared none. */
  name: string;
  /** The series' own colour, or null when the chart never declared one. */
  color: string | null;
  /** Ascending by time, holes already dropped. */
  points: readonly (readonly [number, number])[];
  /** This series' own median sampling interval; 0 when it has none. */
  step: number;
}

/** A reading a series ACTUALLY has, with the instant it was taken at. */
export interface ReadoutSample {
  t: number;
  v: number;
}

/** One row of a neighbour's box: the series, the sample, and where it came from. */
export interface ReadoutRow extends AxisTooltipRow {
  /** The series' index in the chart's own option, which is also ECharts' own. */
  index: number;
  series: ReadoutSeries;
  sample: ReadoutSample;
}

/** An instant is a number of ms, a Date, or a string a time axis parses. */
function toInstant(raw: unknown): number | null {
  if (typeof raw === "number") return Number.isFinite(raw) ? raw : null;
  if (raw instanceof Date) return Number.isFinite(raw.getTime()) ? raw.getTime() : null;
  if (typeof raw === "string") {
    const parsed = Date.parse(raw);
    return Number.isFinite(parsed) ? parsed : null;
  }
  return null;
}

/**
 * A reading, or null for a HOLE — and the distinction is the same one
 * lib/chart-tooltip.ts's tooltipValueText makes. `Number(null)` is 0, and a gap
 * in a loss series drawn as a dot on the axis reads as "no loss at all", which
 * is the opposite of what it means.
 */
function toReading(raw: unknown): number | null {
  if (typeof raw === "number") return Number.isFinite(raw) ? raw : null;
  if (typeof raw === "string" && raw.trim() !== "") {
    const n = Number(raw);
    return Number.isFinite(n) ? n : null;
  }
  return null;
}

function median(values: number[]): number {
  if (values.length === 0) return 0;
  const sorted = [...values].sort((a, b) => a - b);
  const mid = sorted.length >> 1;
  return sorted.length % 2 === 1 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2;
}

/**
 * The series' own sampling interval, as the MEDIAN gap rather than the mean: a
 * fifteen-minute hole in an otherwise 60s series drags a mean to 140s and would
 * have the readout reaching two minutes for a value. The median is what the
 * series is actually scraped at.
 */
function medianStep(points: readonly (readonly [number, number])[]): number {
  const gaps: number[] = [];
  for (let i = 1; i < points.length; i++) {
    const gap = points[i][0] - points[i - 1][0];
    if (gap > 0) gaps.push(gap);
  }
  return median(gaps);
}

/** The point forms ECharts accepts on a time axis: `[t, v]` and `{ value: [t, v] }`. */
function toPoint(item: unknown): [number, number] | null {
  const pair = Array.isArray(item) ? item : (item as { value?: unknown } | null)?.value;
  if (!Array.isArray(pair) || pair.length < 2) return null;
  const t = toInstant(pair[0]);
  const v = toReading(pair[pair.length - 1]);
  return t === null || v === null ? null : [t, v];
}

function declaredColor(entry: Record<string, unknown>): string | null {
  const own = entry.color;
  if (typeof own === "string") return own;
  for (const key of ["itemStyle", "lineStyle"] as const) {
    const style = entry[key] as { color?: unknown } | undefined;
    if (typeof style?.color === "string") return style.color;
  }
  return null;
}

/**
 * readoutSeries reduces a chart's option to the series a neighbour can read.
 *
 * The OPTION is the source on purpose — it is the same object the chart was
 * drawn from, so a value here cannot disagree with the line on screen, and
 * reading it costs nothing per frame (this runs once per setOption, not once
 * per mousemove).
 *
 * A series with no points at all drops out entirely, which is exactly what
 * removes lib/annotations.ts's two overlay series: they carry `data: []` and
 * exist only to host a markLine.
 */
export function readoutSeries(option: echarts.EChartsOption): ReadoutSeries[] {
  const raw = option.series;
  const list: unknown[] = Array.isArray(raw) ? raw : raw ? [raw] : [];

  const built = list
    .map((entry) => {
      const record = (entry ?? {}) as Record<string, unknown>;
      const data = Array.isArray(record.data) ? record.data : [];
      const points = data
        .map(toPoint)
        .filter((p): p is [number, number] => p !== null)
        .sort((a, b) => a[0] - b[0]);
      return {
        name: typeof record.name === "string" ? record.name : "",
        color: declaredColor(record),
        points,
        step: medianStep(points),
      } satisfies ReadoutSeries;
    })
    .filter((series) => series.points.length > 0);

  /* A one-point series has no interval of its own — the 15m window over a
     five-minute metric that lib/curated-metrics.ts draws with symbols on. It
     borrows the chart's, because the panel's other series DO know how often
     this chart is sampled. With nothing to borrow it reads only an exact hit. */
  const chartStep = median(built.map((s) => s.step).filter((step) => step > 0));
  return built.map((series) => (series.step > 0 ? series : { ...series, step: chartStep }));
}

/**
 * nearestSample answers what a series reads at an instant — or refuses to.
 *
 * The shared instant comes from a PIXEL on another chart, so it essentially
 * never coincides with a sample. Interpolating between the two it falls between
 * would put a number on screen that the series does not contain, which is the
 * one thing this must not do. So it snaps to the nearer real sample and hands
 * back THAT sample's own timestamp, which is what lets the box say which
 * reading it is showing.
 *
 * The whole refusal rule is ONE sentence: the nearest sample must be within one
 * of this series' own sampling steps. That single bound does every job asked of
 * it, which is why there is no second constant here — an earlier draft carried a
 * separate "is this bracket a hole" test and its own staleness factor, and the
 * test at the EDGE of a hole caught it refusing an instant sitting ten seconds
 * from a real sample. On a regular grid the nearest point is never more than
 * half a step away, so the readout is continuous; inside a hole it goes quiet
 * the moment a whole scrape's worth of time separates the cursor from anything
 * real; past the end of a series it reads the last sample for one more step and
 * then stops. A reading pulled from up to a step away is still a reading the
 * series HAS, and the dot drawn on the SAMPLE rather than on the cursor line is
 * what says how far away it was taken.
 */
export function nearestSample(series: ReadoutSeries, at: number): ReadoutSample | null {
  const points = series.points;
  if (points.length === 0 || !Number.isFinite(at)) return null;

  let lo = 0;
  let hi = points.length - 1;
  if (at <= points[lo][0]) hi = lo;
  else if (at >= points[hi][0]) lo = hi;
  else {
    while (hi - lo > 1) {
      const mid = (lo + hi) >> 1;
      if (points[mid][0] <= at) lo = mid;
      else hi = mid;
    }
  }

  const near = at - points[lo][0] <= points[hi][0] - at ? lo : hi;
  const away = Math.abs(points[near][0] - at);
  /* `away === 0` is what keeps a series with no derivable step readable at all:
     it has nothing to be judged against, so only landing ON it counts. */
  if (away > series.step && away !== 0) return null;
  return { t: points[near][0], v: points[near][1] };
}

/**
 * pickReadoutRows decides which of a neighbour's series get a dot.
 *
 * It ranks with lib/chart-tooltip.ts's own capTooltipRows — the same helper, one
 * fewer rule in this console. There is no cursor VALUE to rank by here (the
 * pointer is on another panel entirely), so the ranking is the value-descending
 * one that helper already falls back to.
 *
 * Then it puts the survivors back into the chart's own series order. Which
 * series deserve a dot can change as the cursor travels; the order they are
 * DRAWN in must not, or a panel nobody is pointing at reshuffles itself under
 * the reader's eye.
 *
 * `hidden` counts what the cap dropped. Nothing renders it any more — the box
 * that carried a "+n more" tail is gone — but the number is still the honest
 * output of a capped ranking, and lib/chart-cursor.test.tsx pins it.
 */
export function pickReadoutRows(
  model: readonly ReadoutSeries[],
  at: number | null,
  cap: number = READOUT_ROW_CAP,
): { rows: ReadoutRow[]; hidden: number } {
  if (at === null) return { rows: [], hidden: 0 };

  const candidates: ReadoutRow[] = [];
  model.forEach((series, index) => {
    const sample = nearestSample(series, at);
    if (sample === null) return;
    candidates.push({ index, series, sample, seriesName: series.name, value: [sample.t, sample.v] });
  });

  const { rows, hidden } = capTooltipRows(candidates, null, cap);
  return { rows: rows.sort((a, b) => a.index - b.index), hidden };
}
