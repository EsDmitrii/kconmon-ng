import type { EChartsOption, LineSeriesOption, SeriesOption } from "echarts";
import { chartColors } from "./chart-theme";
import type { Annotation } from "./types";

/**
 * annotations.ts — the PURE half of M5's annotation overlay: scope constants,
 * list folding, and the ECharts option builder. No React, no fetch.
 *
 * It lives apart from components/annotations.tsx deliberately. EChart is mocked
 * in every page test (echarts.init reaches for a 2d canvas context jsdom does
 * not implement and a real mount throws), so the marker geometry can only be
 * asserted where it is built rather than where it is drawn — and that is this
 * file, unit-tested directly in annotations.test.ts.
 */

/** GLOBAL_SCOPE is "" — a REAL value on this API, not a missing one. See
 *  lib/types.ts's AnnotationQuery for the three states GET reads out of it. */
export const GLOBAL_SCOPE = "";

/** The server's own bound on annotation text (docs/console-api.yaml). Mirrored
 *  here so the create form can stop a doomed 422 at the textarea. */
export const ANNOTATION_TEXT_MAX = 1024;

/** The overlay series' name. It appears in the legend on purpose: an operator
 *  reading a busy chart can click it off, and a nameless series would be a
 *  blank legend entry rather than no entry at all. */
export const ANNOTATION_SERIES_NAME = "Annotations";

/**
 * isInstant is the whole INSTANT/RANGE distinction, in one place: an absent
 * `endAt` means a mark at a moment (plan Decision 10 — "NULL means instant"),
 * never a span that is still open. A present-but-unparseable endAt degrades to
 * an instant rather than to a band with a NaN edge, which ECharts would render
 * as an area reaching to the end of the axis.
 */
export function isInstant(a: Annotation): boolean {
  return a.endAt === undefined || a.endAt === "" || Number.isNaN(Date.parse(a.endAt));
}

/**
 * annotationOverlaySeries builds ONE line series that carries every marker.
 *
 * Markers have to hang off a series — ECharts has no free-floating markLine —
 * so this is an empty line series whose only job is to host them. Instants
 * become markLine entries at their startAt; spans become markArea entries
 * between startAt and endAt. The text rides along as each item's `name`, shown
 * on hover via the emphasis label rather than at rest: a chart with a dozen
 * marks would otherwise be unreadable behind its own annotations.
 *
 * Returns null when there is nothing to draw, so callers can skip the series
 * entirely instead of appending an empty one.
 */
export function annotationOverlaySeries(annotations: Annotation[], dark: boolean): LineSeriesOption | null {
  const colors = chartColors(dark ? "dark" : "light");
  const instants: { name: string; xAxis: number }[] = [];
  const ranges: [{ name: string; xAxis: number }, { xAxis: number }][] = [];

  for (const a of annotations) {
    const start = Date.parse(a.startAt);
    // An unparseable start has no position on a time axis at all — drawing it
    // at NaN puts a line nowhere and a band everywhere.
    if (Number.isNaN(start)) continue;
    if (isInstant(a)) {
      instants.push({ name: a.text, xAxis: start });
      continue;
    }
    ranges.push([{ name: a.text, xAxis: start }, { xAxis: Date.parse(a.endAt as string) }]);
  }

  if (instants.length === 0 && ranges.length === 0) return null;

  const label = { show: false } as const;
  const emphasisLabel = { show: true, formatter: "{b}", color: colors.axis, fontSize: 11 };

  return {
    name: ANNOTATION_SERIES_NAME,
    type: "line",
    // No points of its own: this series is a marker host. `data: []` also keeps
    // it out of the axis tooltip's value list, so hovering the chart still
    // reads the metrics rather than a phantom "Annotations: -".
    data: [],
    ...(instants.length > 0
      ? {
          markLine: {
            symbol: "none",
            label,
            emphasis: { label: { ...emphasisLabel, position: "insideEndTop" } },
            lineStyle: { color: colors.other, type: "dashed", width: 1, opacity: 0.9 },
            data: instants,
          } satisfies LineSeriesOption["markLine"],
        }
      : {}),
    ...(ranges.length > 0
      ? {
          markArea: {
            label,
            emphasis: { label: { ...emphasisLabel, position: "top" } },
            itemStyle: { color: colors.other, opacity: 0.14 },
            data: ranges,
          } satisfies LineSeriesOption["markArea"],
        }
      : {}),
  };
}

/**
 * withAnnotations appends the overlay to an option a page already built
 * (curated-metrics.ts's toSeriesOption, or any other), without touching what is
 * already there — the caller's object is never mutated, because pages memoise
 * their options and a mutation would be invisible to that memo.
 *
 * Returns the SAME object when there is nothing to overlay, so a memo keyed on
 * identity does not invalidate for an empty annotation list.
 */
export function withAnnotations(option: EChartsOption, annotations: Annotation[], dark: boolean): EChartsOption {
  const overlay = annotationOverlaySeries(annotations, dark);
  if (!overlay) return option;
  const existing: SeriesOption[] = Array.isArray(option.series)
    ? (option.series as SeriesOption[])
    : option.series
      ? [option.series as SeriesOption]
      : [];
  return { ...option, series: [...existing, overlay] };
}

/**
 * mergeAnnotations folds the per-scope listings a surface fetches (its own
 * scope, plus the global ones) into one list.
 *
 * De-duplication by id matters because a GLOBAL surface fetches `?scope=` and a
 * scoped one fetches both — and the same global mark can legitimately arrive
 * from both legs of a future caller. Ordering is oldest-first on
 * (startAt, id): a total order, so two renders of the same data never disagree
 * about which of two simultaneous marks comes first.
 */
export function mergeAnnotations(...lists: Annotation[][]): Annotation[] {
  const byId = new Map<string, Annotation>();
  for (const list of lists) for (const a of list) byId.set(a.id, a);
  return [...byId.values()].sort((a, b) => {
    const delta = Date.parse(a.startAt) - Date.parse(b.startAt);
    if (delta !== 0 && !Number.isNaN(delta)) return delta;
    return a.id.localeCompare(b.id);
  });
}
