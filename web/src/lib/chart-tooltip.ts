import type * as echarts from "echarts";

/**
 * chart-tooltip.ts — why every chart tooltip in this console used to be cut off,
 * and the arithmetic that stops it.
 *
 * ECharts places its tooltip beside the cursor and then "refixes" it against
 * api.getWidth()/getHeight() — the CHART's own box, not the window. A worst-5
 * panel is half a grid column wide and its tooltip is five pair names wide, so
 * the refix could only move the box to a NEGATIVE offset: out of the panel, past
 * the Card, and off the left of the screen, which is exactly the "…nmon-prod-m03"
 * the owner photographed. `confine: true` would have fixed the overflow by
 * squeezing the tooltip into a box too small to hold it; the answer is to clamp
 * against the VIEWPORT instead, and to lift the element out of the panel so no
 * ancestor's overflow can clip what we placed.
 */

/** Distance kept between the cursor and the tooltip's near edge. */
export const GAP = 12;

/** How close to the window's edge the tooltip may sit. */
export const VIEWPORT_MARGIN = 8;

export interface HostRect {
  left: number;
  top: number;
  width: number;
  height: number;
}

export interface TooltipPlacement {
  /** The cursor, in the chart host's own coordinates. */
  point: [number, number];
  /** The tooltip's measured [width, height]. */
  content: [number, number];
  /** The chart host's box in VIEWPORT coordinates. */
  host: HostRect;
  viewport: { width: number; height: number };
}

function clamp(value: number, min: number, max: number): number {
  return max < min ? min : Math.min(Math.max(value, min), max);
}

/**
 * placeTooltip answers where the tooltip goes, host-relative, under three rules
 * in order: stay whole inside the window, prefer the side of the cursor with
 * room, and never sit on top of the point being read.
 */
export function placeTooltip({ point, content, host, viewport }: TooltipPlacement): [number, number] {
  const [px, py] = point;
  const [w, h] = content;

  /* The window's limits, expressed host-relative so every comparison below is
     in one coordinate system. */
  const minX = VIEWPORT_MARGIN - host.left;
  const maxX = viewport.width - VIEWPORT_MARGIN - w - host.left;
  const minY = VIEWPORT_MARGIN - host.top;
  const maxY = viewport.height - VIEWPORT_MARGIN - h - host.top;

  const right = px + GAP;
  const left = px - GAP - w;
  // Flip before shifting: a tooltip beside the cursor reads better than one
  // dragged along the window's edge.
  const x = right <= maxX ? right : left >= minX ? left : clamp(right, minX, maxX);

  // Vertically centred on the cursor, which keeps a tall tooltip's rows either
  // side of the value being read.
  let y = clamp(py - h / 2, minY, maxY);

  /* Only reachable when the tooltip had to be SHIFTED horizontally — beside the
     cursor it can never overlap. Moving it off the cursor's row keeps the point
     itself, and the axis line under it, visible. */
  const coversPoint = px >= x && px <= x + w && py >= y && py <= y + h;
  if (coversPoint) {
    const below = py + GAP;
    const above = py - GAP - h;
    y = below <= maxY ? below : above >= minY ? above : y;
  }

  return [x, y];
}

/* ── how many rows a tooltip may claim ───────────────────────────────────── */

/**
 * The owner's report: `up` on the PromQL console matches ninety-nine series, and
 * hovering the chart produced a tooltip that tried to name every one of them and
 * covered the whole screen.
 *
 * A tooltip is a GLANCE. Grafana's answer is the right one and it is what this
 * takes: the rows nearest what the cursor is pointing at, then an honest line
 * saying how many were left out — with the full listing living where a full
 * listing belongs, in the Table and Raw views under the chart. It also disposes
 * of the placement layer's one degenerate case above: a tooltip taller than the
 * viewport had nowhere legal to go and got clamped to the top edge regardless.
 */
export const TOOLTIP_ROW_CAP = 10;

/** One row ECharts hands an `axis`-trigger formatter. */
export interface AxisTooltipRow {
  /** ECharts' own coloured dot, already HTML. */
  marker?: string;
  seriesName?: string;
  axisValueLabel?: string;
  /** [x, y] on a time axis; a bare number on a category one. */
  value?: unknown;
}

/** rowValue is the y a row carries, or null when it carries nothing numeric. */
export function rowValue(row: AxisTooltipRow): number | null {
  const raw = Array.isArray(row.value) ? row.value[row.value.length - 1] : row.value;
  /* Number(null) is 0 and Number("") is 0, and a gap in a series is neither a
     zero nor a reading — it must sort as "no value", not as the smallest one. */
  if (raw === null || raw === undefined || raw === "") return null;
  const n = typeof raw === "number" ? raw : Number(raw);
  return Number.isFinite(n) ? n : null;
}

/**
 * capTooltipRows picks the rows worth showing and counts the rest.
 *
 * With a cursor value — what the pointer is actually over on the y-axis — the
 * order is PROXIMITY, because the series under the cursor is the one being
 * asked about. Without one it is value-descending, which is the next most
 * useful reading of "what is going on here". A row with no numeric value sorts
 * last either way rather than being dropped silently.
 *
 * Under the cap nothing is reordered at all: the caller's series order is the
 * chart's legend order, and shuffling ten rows would cost more than it buys.
 *
 * Generic in the row so a caller with a RICHER row keeps it: lib/chart-cursor.tsx's
 * neighbour readout ranks rows that also carry the series index and the sample
 * they were read from, and it must get those back rather than a bare projection.
 */
export function capTooltipRows<T extends AxisTooltipRow>(
  rows: readonly T[],
  cursor: number | null,
  cap: number = TOOLTIP_ROW_CAP,
): { rows: T[]; hidden: number } {
  if (rows.length <= cap) return { rows: [...rows], hidden: 0 };
  const rank = (row: T): number => {
    const v = rowValue(row);
    if (v === null) return Number.POSITIVE_INFINITY;
    return cursor === null ? -v : Math.abs(v - cursor);
  };
  const ordered = rows
    .map((row, i) => ({ row, i, rank: rank(row) }))
    /* The index breaks ties, so two series sitting on the same value keep the
       order the chart drew them in instead of swapping on every mousemove. */
    .sort((a, b) => a.rank - b.rank || a.i - b.i)
    .map((entry) => entry.row);
  return { rows: ordered.slice(0, cap), hidden: rows.length - cap };
}

/**
 * NO_VALUE is the console's glyph for "there is no reading here", already the
 * convention in components/investigation-signals.tsx. Exported because
 * lib/curated-metrics.ts's formatters print the same thing for the same reason,
 * and two surfaces disagreeing about how a hole looks is its own bug.
 */
export const NO_VALUE = "—";

/**
 * tooltipValueText decides what a row's value READS AS, and its whole job is the
 * distinction the bubble used to lose.
 *
 * A gap in a series is not a zero. `Number(null)` is 0, so a hole under the
 * cursor printed `0.0ms` — a perfectly plausible latency that never happened.
 * And `histogram_quantile` over an empty window is NaN, which reached the same
 * row as the literal text "NaNms". Both are "no reading", and both say so.
 *
 * The formatter is only ever handed something a formatter can honestly work on;
 * a non-numeric label (the MTR hop strip's category rows) still goes through
 * verbatim, because there the text IS the value.
 *
 * Exported because the shared cursor's neighbour readout prints values too, and
 * "what a value reads as" is one rule for this console, not one per surface.
 */
export function tooltipValueText(raw: unknown, valueFormatter?: (value: unknown) => string): string {
  // `'-'` is ECharts' own empty-slot marker; null/undefined is a hole in a
  // series; "" is a cell that arrived with nothing in it.
  if (raw === null || raw === undefined || raw === "" || raw === "-") return NO_VALUE;
  if (typeof raw === "number" && !Number.isFinite(raw)) return NO_VALUE;
  return valueFormatter ? valueFormatter(raw) : String(raw);
}

/** Series names come from PromQL label values, so they are somebody else's text. */
function escapeHtml(value: string): string {
  return value
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

/**
 * renderAxisTooltip builds the bubble: a header, the kept rows, and the overflow
 * line. The markers are ECharts' own HTML and go through as they are; everything
 * else is escaped.
 */
export function renderAxisTooltip(
  rows: readonly AxisTooltipRow[],
  hidden: number,
  header: string,
  more: (count: number) => string,
  valueFormatter?: (value: unknown) => string,
): string {
  const body = rows
    .map((row) => {
      const raw = Array.isArray(row.value) ? row.value[row.value.length - 1] : row.value;
      const shown = tooltipValueText(raw, valueFormatter);
      return (
        `<div style="display:flex;align-items:center;gap:8px;line-height:1.5">${row.marker ?? ""}` +
        `<span style="flex:1 1 auto;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;max-width:22rem">` +
        `${escapeHtml(row.seriesName ?? "")}</span>` +
        `<span style="font-weight:600">${escapeHtml(shown)}</span></div>`
      );
    })
    .join("");
  const tail =
    hidden > 0 ? `<div style="margin-top:4px;opacity:0.7">${escapeHtml(more(hidden))}</div>` : "";
  return `<div style="font-weight:600;margin-bottom:4px">${escapeHtml(header)}</div>${body}${tail}`;
}

/** The size argument ECharts hands a `position` callback. */
interface PositionSize {
  contentSize: [number, number];
  viewSize: [number, number];
}

/**
 * clampTooltipOption returns the option with its tooltip taught to place itself.
 * A chart that declared no tooltip is returned UNTOUCHED — identity included, so
 * the caller's memo still holds.
 */
export function clampTooltipOption(
  option: echarts.EChartsOption,
  host: () => HTMLElement | null,
): echarts.EChartsOption {
  const tooltip = option.tooltip;
  if (!tooltip || Array.isArray(tooltip)) return option;

  return {
    ...option,
    tooltip: {
      ...tooltip,
      /* Out of the Card, whose overflow was the other half of the clipping.
         With the element on <body> the position below is still expressed in the
         chart's coordinates — ECharts transforms it. */
      appendTo: "body",
      confine: false,
      position: (point: number[], _params: unknown, _dom: unknown, _rect: unknown, size: PositionSize) => {
        const el = host();
        const box = el?.getBoundingClientRect();
        return placeTooltip({
          point: [point[0], point[1]],
          content: size.contentSize,
          /* Before the first measurement — and in a test DOM where every box is
             zero — the chart's own view size is the only honest bound there is. */
          host: box ?? { left: 0, top: 0, width: size.viewSize[0], height: size.viewSize[1] },
          viewport:
            typeof window === "undefined"
              ? { width: size.viewSize[0], height: size.viewSize[1] }
              : { width: window.innerWidth, height: window.innerHeight },
        });
      },
    },
  };
}

/**
 * sharedTooltipOption is what components/echart.tsx actually applies: the
 * placement above, plus the two things every chart in the console now gets.
 *
 *   - a CROSS axis pointer on the chart under the mouse. ECharts only draws its
 *     pointer on the hovered chart, so this is by construction the hovered one:
 *     vertical for the instant, horizontal for the value, with the axis label
 *     pill that makes the y readable without counting gridlines. The OTHER
 *     panels keep the vertical-only line lib/chart-cursor.tsx draws for them —
 *     a horizontal line across a panel with a different y-axis would be a lie.
 *
 *   - a capped row list, so a query matching ninety-nine series cannot produce a
 *     tooltip taller than the window.
 *
 * A chart that declared no tooltip is returned UNTOUCHED, identity included.
 */
/**
 * axisValueFormatter is the chart's OWN way of writing a y value — its
 * `tooltip.valueFormatter` first, then its y-axis label formatter.
 *
 * The cross's y pill printed the raw number: `0.00811` beside an axis whose own
 * ticks read `8.1ms` (owner screenshot). The pill is a reading of the same axis,
 * so it takes the same formatter rather than a second opinion. Undefined when
 * the chart formats nothing — the PromQL console's axis is deliberately raw, and
 * a pill inventing units there would be worse than the bare number.
 *
 * Exported for the neighbour readout: a value read off the RTT panel must print
 * in the RTT panel's OWN units, and this is already the function that knows how
 * that chart writes a y. Asking it twice is how the pill and the readout stay
 * incapable of disagreeing.
 */
export function axisValueFormatter(option: echarts.EChartsOption): ((value: unknown) => string) | undefined {
  const tooltip = option.tooltip;
  const fromTooltip =
    tooltip && !Array.isArray(tooltip)
      ? (tooltip.valueFormatter as ((value: unknown) => string) | undefined)
      : undefined;
  if (fromTooltip) return fromTooltip;

  const yAxis = option.yAxis;
  /* A multi-axis chart has no single "the y value", so it is left alone. */
  if (!yAxis || Array.isArray(yAxis)) return undefined;
  const label = (yAxis as { axisLabel?: { formatter?: unknown } }).axisLabel;
  return typeof label?.formatter === "function"
    ? (value: unknown) => String((label.formatter as (v: unknown) => unknown)(value))
    : undefined;
}

/** withPointerLabel teaches the Y axis's own pointer to print the pill properly. */
function withPointerLabel(option: echarts.EChartsOption): Partial<echarts.EChartsOption> {
  const format = axisValueFormatter(option);
  const yAxis = option.yAxis;
  if (!format || !yAxis || Array.isArray(yAxis)) return {};
  const axis = yAxis as Record<string, unknown>;
  const pointer = (axis.axisPointer ?? {}) as Record<string, unknown>;
  return {
    /* On the Y AXIS rather than on the tooltip: the tooltip's own axisPointer
       label governs BOTH pills, and running a value formatter over the x pill
       would turn an instant into a latency. */
    yAxis: {
      ...axis,
      axisPointer: {
        ...pointer,
        label: { ...((pointer.label ?? {}) as object), formatter: (p: { value: unknown }) => format(p.value) },
      },
    } as echarts.EChartsOption["yAxis"],
  };
}

export function sharedTooltipOption(
  option: echarts.EChartsOption,
  host: () => HTMLElement | null,
  opts: { cursorValue: () => number | null; more: (count: number) => string; cap?: number },
): echarts.EChartsOption {
  const tooltip = option.tooltip;
  if (!tooltip || Array.isArray(tooltip)) return option;

  const pointer = (tooltip.axisPointer ?? {}) as Record<string, unknown>;
  const valueFormatter = tooltip.valueFormatter as ((value: unknown) => string) | undefined;

  const withCross: echarts.EChartsOption = {
    ...option,
    ...withPointerLabel(option),
    tooltip: {
      ...tooltip,
      /* A cross draws with crossStyle, not lineStyle, so each surface's own grid
         colour is carried across rather than dropped for a default. The cast is
         the seam: `pointer` came in as an index signature because the surfaces
         build it freely, and ECharts' own option type is what it goes back as. */
      axisPointer: {
        ...pointer,
        type: "cross",
        crossStyle: pointer.crossStyle ?? pointer.lineStyle,
      } as NonNullable<echarts.TooltipComponentOption["axisPointer"]>,
      /* Only an axis trigger produces a list of series; an item trigger is one
         point and has nothing to cap. */
      ...(tooltip.trigger === "axis"
        ? {
            formatter: (params: unknown) => {
              const list = (Array.isArray(params) ? params : [params]) as AxisTooltipRow[];
              const { rows, hidden } = capTooltipRows(list, opts.cursorValue(), opts.cap);
              return renderAxisTooltip(rows, hidden, list[0]?.axisValueLabel ?? "", opts.more, valueFormatter);
            },
          }
        : {}),
    },
  };

  return clampTooltipOption(withCross, host);
}
