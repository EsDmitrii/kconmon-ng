import type { EChartsOption, LineSeriesOption, SeriesOption } from "echarts";
import { chartColors } from "./chart-theme";
import { stampShort, type Locale, type Translate } from "./i18n";
import { enT } from "./i18n/dict/annotations";
import type { Annotation, MaintenanceWindow } from "./types";

/**
 * It lives apart from components/annotations.tsx deliberately; EChart is mocked in every page test
 * (echarts.init reaches for a 2d canvas context jsdom does not implement and a real mount throws).
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

/** The server's own bound on a maintenance window's reason (docs/console-api.yaml's MaintenanceWindowRequest). */
export const MAINTENANCE_REASON_MAX = 512;

/** The maintenance overlay's series name — separate from the annotations one so
 *  an operator can switch the bands off from the legend without losing the
 *  notes, and vice versa. */
export const MAINTENANCE_SERIES_NAME = "Maintenance";

/** isInstant is the whole INSTANT/RANGE distinction, in one place: an absent `endAt` means a mark at a moment. */
export function isInstant(a: Annotation): boolean {
  return a.endAt === undefined || a.endAt === "" || Number.isNaN(Date.parse(a.endAt));
}

/**
 * annotationOverlaySeries builds ONE line series that carries every marker; markers have to hang
 * off a series — ECharts has no free-floating markLine.
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
  /* INSIDE the plot, and bounded.
     `position: "top"` draws the label ABOVE the band — outside the grid, where it overprinted the
     panel title and, for a long reason, ran off the right edge of the card with no way to read it.
     A marker label belongs in the area it marks; `overflow: "truncate"` keeps a long reason from
     doing the same thing inside. The whole text is in the row below the chart either way. */
  const emphasisLabel = {
    show: true,
    formatter: "{b}",
    color: colors.axis,
    fontSize: 11,
    width: 220,
    overflow: "truncate" as const,
  };

  return {
    name: ANNOTATION_SERIES_NAME,
    type: "line",
    // No points of its own: this series is a marker host. `data: []` also keeps
    // it out of the axis tooltip's value list, so hovering the chart still
    // reads the metrics rather than a phantom "Annotations: -".
    data: [],
    // A series with no explicit colour takes the next entry from ECharts' palette.
    itemStyle: { color: colors.other },
    lineStyle: { color: colors.other },
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
            emphasis: { label: { ...emphasisLabel, position: "insideTop" } },
            itemStyle: { color: colors.other, opacity: 0.14 },
            data: ranges,
          } satisfies LineSeriesOption["markArea"],
        }
      : {}),
  };
}

/**
 * withAnnotations appends the overlay to an option a page already built (curated-metrics.ts's
 * toSeriesOption, or any other).
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
 * maintenanceOverlaySeries is annotationOverlaySeries' SIBLING; a window with an unparseable edge
 * is SKIPPED rather than drawn with a NaN bound.
 */
export function maintenanceOverlaySeries(windows: MaintenanceWindow[], dark: boolean): LineSeriesOption | null {
  const colors = chartColors(dark ? "dark" : "light");
  const data: [{ name: string; xAxis: number }, { xAxis: number }][] = [];

  for (const span of mergeWindowSpans(windows)) {
    data.push([{ name: span.reasons.join(" · "), xAxis: span.start }, { xAxis: span.end }]);
  }

  if (data.length === 0) return null;

  return {
    name: MAINTENANCE_SERIES_NAME,
    type: "line",
    data: [],
    // Same finding as annotationOverlaySeries above, and the one the report actually caught.
    itemStyle: { color: colors.axis },
    lineStyle: { color: colors.axis },
    markArea: {
      label: { show: false },
      emphasis: {
        label: {
          show: true,
          formatter: "{b}",
          color: colors.axis,
          fontSize: 11,
          // Inside the band and bounded — see the annotation overlay's emphasisLabel.
          position: "insideTop",
          width: 220,
          overflow: "truncate",
        },
      },
      itemStyle: { color: colors.axis, opacity: 0.08, borderColor: colors.axis, borderWidth: 1, borderType: "dashed" },
      data,
    },
  };
}

/**
 * mergeWindowSpans folds overlapping maintenance windows into ONE band each.
 *
 * A markArea fill is translucent, and ECharts draws one per entry: two windows that overlap paint
 * the overlap twice, so the plot came out in two greys with a dashed seam down the middle — which
 * reads as a chart bug, not as "two maintenance windows overlap here". The union is one band, and
 * the reasons of everything inside it are joined for the hover label.
 *
 * Windows with an unparseable edge are SKIPPED rather than drawn with a NaN bound.
 */
export function mergeWindowSpans(
  windows: MaintenanceWindow[],
): { start: number; end: number; reasons: string[] }[] {
  const parsed = windows
    .map((w) => ({ start: Date.parse(w.startAt), end: Date.parse(w.endAt), reason: w.reason }))
    .filter((w) => !Number.isNaN(w.start) && !Number.isNaN(w.end))
    .sort((a, b) => a.start - b.start);

  const out: { start: number; end: number; reasons: string[] }[] = [];
  for (const w of parsed) {
    const last = out[out.length - 1];
    // `<=` so two windows that merely touch (one ends exactly where the next begins) also read as
    // one uninterrupted period, which is what they are.
    if (last && w.start <= last.end) {
      last.end = Math.max(last.end, w.end);
      if (w.reason && !last.reasons.includes(w.reason)) last.reasons.push(w.reason);
      continue;
    }
    out.push({ start: w.start, end: w.end, reasons: w.reason ? [w.reason] : [] });
  }
  return out;
}

/**
 * withMaintenance is withAnnotations for the bands; same contract in every respect that matters to
 * a caller: never mutates.
 */
export function withMaintenance(option: EChartsOption, windows: MaintenanceWindow[], dark: boolean): EChartsOption {
  const overlay = maintenanceOverlaySeries(windows, dark);
  if (!overlay) return option;
  const existing: SeriesOption[] = Array.isArray(option.series)
    ? (option.series as SeriesOption[])
    : option.series
      ? [option.series as SeriesOption]
      : [];
  return { ...option, series: [...existing, overlay] };
}

/**
 * mergeAnnotations folds the per-scope listings a surface fetches (its own scope, plus the global
 * ones) into one list; ordering is oldest-first on (startAt, id): a total order.
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

/** FrozenWindow is the range a surface is PINNED to — the Investigate page's
 *  committed from/to. Absent everywhere else: a live chart re-fetches a moving
 *  window, so anything created lands in it by the next poll. */
export interface FrozenWindow {
  from: Date;
  to: Date;
}

/**
 * outsideWindowNote is the one line a create form shows when what it just stored will not appear in
 * the list it was created from; everywhere else the created row simply APPEARS after the refresh.
 */
export function outsideWindowNote(
  start: Date,
  end: Date | null,
  frozen: FrozenWindow | undefined,
  t: Translate<"created.outsideWindow"> = enT,
  locale: Locale = "en",
): string | null {
  if (frozen === undefined) return null;
  const from = frozen.from.getTime();
  const to = frozen.to.getTime();
  if (!Number.isFinite(from) || !Number.isFinite(to)) return null;
  const startMs = start.getTime();
  const endMs = end === null ? startMs : end.getTime();
  if (Number.isNaN(startMs) || Number.isNaN(endMs)) return null;
  if (startMs <= to && endMs >= from) return null;
  /* The stamp lands inside a translated sentence, so it takes that sentence's
     language — lib/i18n's stampShort, never a bare toLocale* with an options bag
     (QA scope 3, finding #7: an options bag renders WORDS, and words follow the
     interface). The DAY comes with it now: a frozen window can be last Tuesday's,
     and a bare clock said "ends 01:00 PM" without saying which one. */
  return t("created.outsideWindow", { ends: stampShort(frozen.to, locale) });
}

/**
 * defaultStartIn is the instant a create form should OPEN on.
 *
 * NOW, while the surface is live or while now already falls inside the frozen
 * window being listed. Otherwise `spanSeconds` before that window's END — a form
 * that defaults to now inside a window over last Tuesday stores something the
 * list it was created from can never show, and every single create from the
 * Investigate page was therefore born "outside this window" (QA scope 3,
 * finding #5). Clamped up to the window's start so a span longer than the window
 * still begins inside it.
 */
export function defaultStartIn(now: Date, frozen: FrozenWindow | undefined, spanSeconds = 0): Date {
  if (frozen === undefined) return now;
  const from = frozen.from.getTime();
  const to = frozen.to.getTime();
  if (!Number.isFinite(from) || !Number.isFinite(to) || from > to) return now;
  const nowMs = now.getTime();
  if (nowMs >= from && nowMs <= to) return now;
  return new Date(Math.max(from, to - spanSeconds * 1000));
}

/** mergeMaintenanceWindows is mergeAnnotations for the windows: same two legs
 *  (the surface's own scope and the global one), same de-duplication by id, and
 *  the same total (startAt, id) order so two renders of one fetch never
 *  disagree about which of two simultaneous windows comes first. */
export function mergeMaintenanceWindows(...lists: MaintenanceWindow[][]): MaintenanceWindow[] {
  const byId = new Map<string, MaintenanceWindow>();
  for (const list of lists) for (const w of list) byId.set(w.id, w);
  return [...byId.values()].sort((a, b) => {
    const delta = Date.parse(a.startAt) - Date.parse(b.startAt);
    if (delta !== 0 && !Number.isNaN(delta)) return delta;
    return a.id.localeCompare(b.id);
  });
}
