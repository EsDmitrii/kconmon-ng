import { useMemo } from "react";
import type { EChartsOption, LineSeriesOption, SeriesOption } from "echarts";
import { EChart } from "@/components/echart";
import { useTheme } from "@/components/theme-provider";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { maintenanceOverlaySeries } from "@/lib/annotations";
import { chartColors } from "@/lib/chart-theme";
import { ApiError } from "@/lib/api";
import { formatSeconds, toSeriesOption, type CuratedChart } from "@/lib/curated-metrics";
import type { Annotation, MaintenanceWindow, PromResult } from "@/lib/types";

/** problemDetail renders a rejected fetch as the sentence the SERVER wrote —
 *  a problem+json `detail` says which parameter was refused and why, which is
 *  the half an operator needs; the generic fallback is for a transport failure
 *  that carries no body at all. */
function problemDetail(error: Error): string {
  if (error instanceof ApiError) return error.problem.detail ?? error.problem.title;
  return error.message === "" ? "The query could not be run." : error.message;
}

/**
 * investigation-signals.tsx — the Investigate page's right-hand column: the
 * scope's loss and RTT charts, the timeline cursor drawn across both of them,
 * and the matrix delta chip.
 *
 * It is PRESENTATIONAL. Every fetch the panels render lives in
 * pages/investigate.tsx, because the same loss/RTT range responses are also
 * what lib/investigation.ts's thresholdCrossings derives the timeline's
 * threshold rows from — fetching them twice (once for the chart, once for the
 * timeline) would let the picture and the rows disagree about the very series
 * they are both describing.
 *
 * The cursor builder below is exported and unit-tested directly, for the reason
 * lib/annotations.ts already documents: EChart is mocked in every page test
 * (echarts.init needs a 2d canvas context jsdom does not implement), so marker
 * geometry can only be asserted where it is BUILT.
 *
 * The maintenance bands are NOT built here any more. M6 Task 9 lifted them into
 * lib/annotations.ts (maintenanceOverlaySeries), beside the annotation overlay
 * they are modelled on, because the pair/target cards and Explore draw the same
 * bands — this file just composes them below.
 */

/** The cursor series' name. Named rather than anonymous so it can be switched
 *  off from the legend like any other series. */
export const CURSOR_SERIES_NAME = "Timeline cursor";

/**
 * cursorSeries is the timeline↔chart sync, in one markLine.
 *
 * Hovering a timeline row hands this the row's instant and the same vertical
 * line appears on every signal chart, which is the whole point of the pane:
 * "the route changed HERE" and "loss started THERE" have to be readable as one
 * picture. Null (nothing hovered) returns null rather than an empty series, so
 * the caller's memoised option keeps its identity and ECharts is not asked to
 * re-render for a marker that is not there.
 *
 * Markers must hang off a series — ECharts has no free-floating markLine — so
 * this is an empty line series whose only job is to host one.
 */
export function cursorSeries(at: Date | null, dark: boolean): LineSeriesOption | null {
  if (at === null) return null;
  const ms = at.getTime();
  if (!Number.isFinite(ms)) return null;
  const colors = chartColors(dark ? "dark" : "light");
  return {
    name: CURSOR_SERIES_NAME,
    type: "line",
    data: [],
    markLine: {
      symbol: "none",
      silent: true,
      animation: false,
      label: { show: false },
      lineStyle: { color: colors.axis, type: "solid", width: 1, opacity: 0.85 },
      data: [{ xAxis: ms }],
    },
  };
}

/** withOverlays appends the cursor and the maintenance bands to an option a
 *  caller already built, WITHOUT mutating it (pages memoise their options, and
 *  a mutation would be invisible to that memo) and returning the same object
 *  when there is nothing to add. The bands come from the SHARED builder every
 *  other surface draws (lib/annotations.ts's maintenanceOverlaySeries), so the
 *  Investigate charts and a pair card cannot disagree about what a declared
 *  window looks like. */
export function withOverlays(
  option: EChartsOption,
  opts: { cursorAt: Date | null; windows: MaintenanceWindow[]; dark: boolean },
): EChartsOption {
  const extra = [maintenanceOverlaySeries(opts.windows, opts.dark), cursorSeries(opts.cursorAt, opts.dark)].filter(
    (s): s is LineSeriesOption => s !== null,
  );
  if (extra.length === 0) return option;
  const existing: SeriesOption[] = Array.isArray(option.series)
    ? (option.series as SeriesOption[])
    : option.series
      ? [option.series as SeriesOption]
      : [];
  return { ...option, series: [...existing, ...extra] };
}

export interface MatrixDelta {
  before: number | null;
  after: number | null;
  delta: number | null;
}

/** firstSample reads the one number out of an instant vector. Prometheus's own
 *  error envelope RESOLVES rather than throws (lib/api.ts's handle), an empty
 *  vector is a legitimate "nothing measured", and NaN/+Inf arrive as strings —
 *  all three answer null, never 0. */
function firstSample(res: PromResult | undefined): number | null {
  if (!res || res.status !== "success" || res.data?.resultType !== "vector") return null;
  const entry = (res.data.result ?? [])[0] as { value?: [number, string] } | undefined;
  const raw = entry?.value?.[1];
  if (raw === undefined) return null;
  const v = Number(raw);
  return Number.isFinite(v) ? v : null;
}

/**
 * deltaFromVectors is the matrix delta chip: the scope's fail ratio evaluated
 * at the START of the window and again at its END, and the signed difference.
 *
 * Two INSTANT evaluations rather than an average over the window, because the
 * question the chip answers is "is this worse than it was when the window
 * opened?" — and the value at `from` is, by the rate window's own construction,
 * the state of the PRECEDING window. A null on either side keeps the delta null:
 * "we could not measure one end" is not "nothing changed".
 */
export function deltaFromVectors(before: PromResult | undefined, after: PromResult | undefined): MatrixDelta {
  const b = firstSample(before);
  const a = firstSample(after);
  return { before: b, after: a, delta: b === null || a === null ? null : a - b };
}

function fmtPct(v: number | null): string {
  return v === null ? "—" : `${(v * 100).toFixed(1)}%`;
}

function fmtSignedPct(v: number | null): string {
  if (v === null) return "—";
  const pct = v * 100;
  return `${pct >= 0 ? "+" : "−"}${Math.abs(pct).toFixed(1)} pp`;
}

/**
 * signalChartOption is this column's OWN option builder (QA round 3, findings
 * #13 and #14).
 *
 * It composes lib/curated-metrics.ts's toSeriesOption — the series, the colours
 * and the tooltip are one builder for the whole console, and forking them here
 * would let a curated chart and a signal chart draw the same numbers
 * differently. What it then states EXPLICITLY is the two axis treatments this
 * narrow column depends on, rather than inheriting them silently:
 *
 *   x — `hideOverlap`, so a 20rem-wide time axis thins its ticks out instead of
 *       smearing every stamp into an unreadable band (#13).
 *   y — the ADAPTIVE millisecond formatter (curated-metrics' formatSeconds), so
 *       an RTT axis stepping fractions of a millisecond stops printing the same
 *       label three times (#14).
 *
 * Stating them here is the point of the function. These panels are 24rem wide
 * against the curated grid's full page, so they hit the collision cases first
 * and hardest; a future width or density change to the shared builder must not
 * be able to quietly un-fix this column, and a test pins the option THIS
 * function returns.
 */
export function signalChartOption(
  chart: CuratedChart,
  result: PromResult,
  dark: boolean,
  overlays: { cursorAt: Date | null; windows: MaintenanceWindow[] },
): EChartsOption {
  const base = toSeriesOption(chart, result, dark);
  const withAxes: EChartsOption = {
    ...base,
    xAxis: { ...(base.xAxis as object), axisLabel: { ...(base.xAxis as { axisLabel?: object }).axisLabel, hideOverlap: true } },
    yAxis: {
      ...(base.yAxis as object),
      axisLabel: {
        ...(base.yAxis as { axisLabel?: object }).axisLabel,
        formatter: chart.unit === "seconds" ? (value: number) => formatSeconds(value) : undefined,
      },
    },
  } as EChartsOption;
  return withOverlays(withAxes, { ...overlays, dark });
}

function SignalChart({
  title,
  unit,
  result,
  error,
  cursorAt,
  windows,
  annotations,
  emptyNote,
}: {
  title: string;
  unit: CuratedChart["unit"];
  result: PromResult | undefined;
  /** The REJECTION, as opposed to Prometheus's own error envelope below (QA
   *  round 3, finding #2). A 4xx from the guarded proxy — an inverted range,
   *  a refused expression, a proxy that is down — throws out of the fetch, so
   *  `result` stays undefined and this pane used to render nothing at all: a
   *  heading over 160px of dead space, indistinguishable from still loading. */
  error?: Error | null;
  cursorAt: Date | null;
  windows: MaintenanceWindow[];
  annotations: Annotation[];
  emptyNote: string;
}) {
  const { theme } = useTheme();
  const dark = theme === "dark";
  const chart = useMemo<CuratedChart>(() => ({ id: title, title, unit, query: "" }), [title, unit]);
  const option = useMemo(
    () => (result ? signalChartOption(chart, result, dark, { cursorAt, windows }) : undefined),
    [chart, result, dark, cursorAt, windows],
  );

  // promqlQueryRange RESOLVES Prometheus's own error envelope rather than
  // throwing, so a query-level failure shows up in the body, not as a rejection.
  const envelopeError = result?.status === "error" ? (result.error ?? "query failed") : undefined;
  /* Both failure shapes render as ONE line under the heading. The rejection is
     named first because it is the one that leaves no result to describe. */
  const problem = error ? problemDetail(error) : envelopeError;
  const empty =
    result?.status === "success" && (result.data?.resultType !== "matrix" || (result.data?.result ?? []).length === 0);

  return (
    <section aria-label={title} className="mt-4 first:mt-0">
      <h4 className="text-xs font-medium text-muted-foreground">{title}</h4>
      {problem ? (
        <p role="alert" className="mt-1 text-xs leading-relaxed text-health-bad">
          {problem}
        </p>
      ) : null}
      {empty && !problem ? <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{emptyNote}</p> : null}
      {option && !empty && !problem ? (
        <EChart option={option} annotations={annotations} dark={dark} className="mt-1 h-40 w-full" />
      ) : null}
    </section>
  );
}

/**
 * SignalPanels is the right-hand column: the delta chip, the two charts and the
 * cursor readout.
 *
 * The cursor readout is DOM, not a chart tooltip, and deliberately: the canvas
 * marker cannot be read by a screen reader, cannot be focused, and is invisible
 * to every test in this repo. A one-line readout says which instant the panes
 * are currently agreeing on, in text.
 */
export function SignalPanels({
  scopeLabel,
  loss,
  lossError,
  rtt,
  rttError,
  delta,
  cursorAt,
  windows,
  annotations,
  promConfigured,
  gated,
}: {
  scopeLabel: string;
  loss: PromResult | undefined;
  /** The loss range query's REJECTION (finding #2). */
  lossError?: Error | null;
  rtt: PromResult | undefined;
  /** The RTT range query's REJECTION (finding #2). */
  rttError?: Error | null;
  delta: MatrixDelta;
  cursorAt: Date | null;
  windows: MaintenanceWindow[];
  annotations: Annotation[];
  promConfigured: boolean;
  /** True when the subject holds no promql:query — the panes render their own
   *  muted line and NOTHING is fetched (the timeline's source list carries the
   *  same statement; this one keeps the empty column from looking broken). */
  gated: boolean;
}) {
  return (
    <Card asChild className="p-5">
      <section aria-label="Signals">
        <div className="flex flex-wrap items-center gap-2">
          <h3 className="text-sm font-semibold">Signals</h3>
          <span className="text-xs text-muted-foreground">{scopeLabel}</span>
        </div>

        <div data-testid="matrix-delta" className="mt-3 flex flex-wrap items-center gap-2 text-xs">
          <span className="text-muted-foreground">Fail ratio</span>
          <span className="nums">{fmtPct(delta.before)}</span>
          <span aria-hidden="true" className="text-muted-foreground">
            →
          </span>
          <span className="nums">{fmtPct(delta.after)}</span>
          <Badge variant={delta.delta === null ? "unknown" : delta.delta > 0 ? "bad" : delta.delta < 0 ? "ok" : "neutral"}>
            {fmtSignedPct(delta.delta)}
          </Badge>
          <span className="text-[11px] text-muted-foreground">window start vs window end</span>
        </div>

        <p data-testid="signal-cursor" className="mt-2 text-[11px] text-muted-foreground">
          Cursor {cursorAt === null ? "— no row hovered" : cursorAt.toLocaleTimeString()}
        </p>

        {gated ? (
          <p className="mt-3 text-xs leading-relaxed text-muted-foreground">
            The loss and RTT panes read Prometheus through the guarded proxy, which needs promql:query. Nothing was
            requested.
          </p>
        ) : !promConfigured ? (
          <p className="mt-3 text-xs leading-relaxed text-muted-foreground">
            Prometheus is not configured for this console — set console.prometheus.address to draw the scope's signals.
            The timeline above does not depend on it.
          </p>
        ) : (
          <>
            <SignalChart
              title="Packet loss"
              unit="ratio"
              result={loss}
              error={lossError}
              cursorAt={cursorAt}
              windows={windows}
              annotations={annotations}
              emptyNote="No loss series for this scope in the window — nothing is probing it, or the samples have not been scraped yet."
            />
            <SignalChart
              title="RTT p95"
              unit="seconds"
              result={rtt}
              error={rttError}
              cursorAt={cursorAt}
              windows={windows}
              annotations={annotations}
              emptyNote="No RTT series for this scope in the window."
            />
          </>
        )}
      </section>
    </Card>
  );
}
