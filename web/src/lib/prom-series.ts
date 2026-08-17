import { DEFAULT_PAGE_SIZE } from "./pagination";

/**
 * prom-series.ts — what actually tells two PromQL series apart.
 *
 * The owner's rejection of the Console's results: `up` on the stand matches 86
 * series, and every surface that named one printed its WHOLE label set —
 * `{__name__="up", container="agent", endpoint="http", i…` — cut off mid-label
 * by whatever box it was in. Eighty-six identical prefixes are not an identity,
 * and the characters that would have distinguished them were exactly the ones
 * the ellipsis ate.
 *
 * The rule here is Grafana Explore's, and it is the only honest one: a label
 * whose value is the SAME on every series in the result distinguishes nothing.
 * It is said once, above the listing, and dropped from every row. What is left
 * is the smallest string that still identifies a series — `up{pod="…-5nkfd"}`.
 *
 * Nothing is thrown away: `full`/`fullText` carry every label the series
 * actually has, which is what the raw table's row expand and its `title` show.
 *
 * Arithmetic only — no React, no DOM, no fetch.
 */

/** `__name__` is Prometheus's metric name, not a label an operator wrote. */
const NAME_LABEL = "__name__";

export interface SeriesIdentity {
  /** The metric's own name, or "" when the result carries none. */
  name: string;
  /** ONLY the labels that differ across the result, ascending by key. */
  distinguishing: Array<[string, string]>;
  /** `name{k="v"}` — what a legend row, a tooltip row and a table cell print. */
  text: string;
  /** Every label this series carries, ascending by key. */
  full: Array<[string, string]>;
  /** The same, rendered — the row's `title` and its expanded line. */
  fullText: string;
}

export interface SeriesIdentitySet {
  /** Index-aligned with the input, so a caller can zip it against its own rows. */
  series: SeriesIdentity[];
  /** The labels EVERY series shares, stated once instead of on every row. */
  shared: Array<[string, string]>;
  /** The metric name when the whole result agrees on one, else "". */
  sharedName: string;
  /** `sharedName{shared}` — the one line above the listing. */
  sharedText: string;
}

/** render turns a name and a label list into the one string shape this page uses. */
function render(name: string, labels: ReadonlyArray<readonly [string, string]>): string {
  if (labels.length === 0) return name === "" ? "{}" : name;
  const body = labels.map(([k, v]) => `${k}="${v}"`).join(", ");
  return `${name}{${body}}`;
}

function sortedEntries(metric: Record<string, string>): Array<[string, string]> {
  return Object.entries(metric)
    .filter(([k]) => k !== NAME_LABEL)
    .sort(([a], [b]) => a.localeCompare(b));
}

/**
 * seriesIdentities computes the minimal distinguishing identity of every series
 * in one result, plus the part they all share.
 *
 * A label counts as SHARED only when every series carries it AND every value
 * agrees: a label one series lacks is itself a difference, and dropping it would
 * make two rows read alike.
 */
export function seriesIdentities(metrics: readonly Record<string, string>[]): SeriesIdentitySet {
  if (metrics.length === 0) return { series: [], shared: [], sharedName: "", sharedText: "" };

  const keys = [...new Set(metrics.flatMap((m) => Object.keys(m)))].filter((k) => k !== NAME_LABEL).sort();
  const sharedKeys = keys.filter((k) => {
    const first = metrics[0][k];
    return first !== undefined && metrics.every((m) => m[k] === first);
  });
  const sharedKeySet = new Set(sharedKeys);

  const names = new Set(metrics.map((m) => m[NAME_LABEL] ?? ""));
  /* One metric name across the whole result is part of what they SHARE, so it
     is hoisted out of the rows; a mixed result keeps the name on every row,
     because there it is the main thing telling them apart. */
  const uniformName = names.size === 1 ? [...names][0] : "";

  const build = (minimal: boolean): SeriesIdentity[] =>
    metrics.map((metric) => {
      const full = sortedEntries(metric);
      const name = metric[NAME_LABEL] ?? "";
      const distinguishing = minimal ? full.filter(([k]) => !sharedKeySet.has(k)) : full;
      return { name, distinguishing, text: render(name, distinguishing), full, fullText: render(name, full) };
    });

  let series = build(true);
  /* Two series that agree on every label cannot be told apart by a minimal
     identity, and two legend rows reading `up` would be a worse lie than a long
     string. Prometheus does not emit this; a recording rule or a fixture can. */
  if (metrics.length > 1 && new Set(series.map((s) => s.text)).size < series.length) {
    series = build(false);
  }

  const shared = sharedKeys.map((k) => [k, metrics[0][k]] as [string, string]);
  return {
    series,
    shared,
    sharedName: uniformName,
    sharedText: shared.length === 0 && uniformName === "" ? "" : render(uniformName, shared),
  };
}

/**
 * LEGEND_MAX_SERIES is where the in-chart legend stops being a legend.
 *
 * ECharts' scroll legend does not wrap past the room it has: at 86 series it
 * became "1/86" — one name between two arrows, a control for reading labels one
 * at a time, which is what the owner rejected. Past this many series the RAW
 * TABLE below the chart is the legend, and it is a better one: it lists every
 * series, ten to a page, with the same identities and their values.
 *
 * The number is the product's own page size on purpose. Whenever the legend is
 * drawn, the raw table's first page holds exactly the same series, so the two
 * readings of the result cannot disagree about what is on screen.
 */
export const LEGEND_MAX_SERIES = DEFAULT_PAGE_SIZE;

/** showLegend: draw one only while it can be read at a glance. */
export function showLegend(seriesCount: number): boolean {
  return seriesCount > 0 && seriesCount <= LEGEND_MAX_SERIES;
}

/* ── colour, which is the other half of telling two series apart ─────────── */

/**
 * The owner on the Console's Chart tab: «в консоли снова все полосочки серые».
 *
 * They were. lib/chart-theme.ts's seriesColor assigns the five validated hues
 * and folds EVERYTHING after them into `other` — which is `--chart-axis`, the
 * very grey the axis is drawn in. That is the right rule where it was written:
 * the curated charts are `topk(5)`, so a sixth line there really is an also-ran.
 * The Console has no topk. `up` matches 86 series, and 81 lines plus 81 table
 * swatches came out identical axis grey — at which point the swatch no longer
 * ties a row to a line, and the plot is a grey smear.
 *
 * paletteColor is the Console's rule: the same five hues, RE-LIT one lap at a
 * time. Lightness moves a fraction of the way toward a ceiling/floor rather
 * than by a fixed step, so no two laps of one hue can clamp into each other.
 *
 * It does not pretend colour can carry 86 identities — after four laps it
 * repeats, and the raw table below the plot is what tells those apart. What it
 * does guarantee is the ten series on the reader's page, which is exactly the
 * number the legend and the table's first page hold.
 */
const HSL_TRIPLET = /^hsl\(\s*([\d.]+)\s*,\s*([\d.]+)%\s*,\s*([\d.]+)%\s*\)$/i;
/** How far toward LIGHT_CEIL (positive) or DARK_FLOOR (negative) each lap moves. */
const LAPS = [0, 0.45, -0.45, 0.75];
const LIGHT_CEIL = 84;
const DARK_FLOOR = 24;

export function paletteColor(base: readonly string[], index: number): string {
  if (base.length === 0) return "";
  const i = Number.isFinite(index) ? Math.max(0, Math.floor(index)) : 0;
  const color = base[i % base.length];
  const lap = LAPS[Math.floor(i / base.length) % LAPS.length];
  if (lap === 0) return color;

  const parsed = HSL_TRIPLET.exec(color);
  /* A token that is not the documented hsl() triplet is returned untouched: a
     re-lit `hsl(NaN, NaN%, NaN%)` is not a colour, and zrender would drop the
     series to black. */
  if (!parsed) return color;
  const [, hue, saturation, lightness] = parsed;
  const l = Number(lightness);
  const shifted = lap > 0 ? l + (LIGHT_CEIL - l) * lap : l + (l - DARK_FLOOR) * lap;
  return `hsl(${hue}, ${saturation}%, ${Math.round(shifted)}%)`;
}
