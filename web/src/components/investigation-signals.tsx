import { useEffect, useMemo, useState } from "react";
import type { EChartsOption, LineSeriesOption, SeriesOption } from "echarts";
import { EChart } from "@/components/echart";
import { useTheme } from "@/components/theme-provider";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { maintenanceOverlaySeries } from "@/lib/annotations";
import {
  TIMELINE_CURSOR_SOURCE,
  nearestInstant,
  readoutInstant,
  readoutSeries,
  useChartCursor,
  type ReadoutSeries,
} from "@/lib/chart-cursor";
import { ApiError } from "@/lib/api";
import { toSeriesOption, type CuratedChart } from "@/lib/curated-metrics";
import { stampClock, translate, useLocale, useT, type Translate } from "@/lib/i18n";
import { PROMQL_MAX_RANGE_MS } from "@/lib/investigation-sources";
import { signalsDict, type SignalsKey } from "@/lib/i18n/dict/signals";
import type { Annotation, MaintenanceWindow, PromResult } from "@/lib/types";

/** enT is the ENGLISH translator problemDetail defaults to — the wave's
 *  pattern for a pure helper, so a one-argument call reads what it always did. */
const enT: Translate<SignalsKey> = (key, vars) => translate(signalsDict, "en", key, vars);

/** problemDetail renders a rejected fetch as the sentence the SERVER wrote —
 *  a problem+json `detail` says which parameter was refused and why, which is
 *  the half an operator needs; the generic fallback is for a transport failure
 *  that carries no body at all. */
function problemDetail(error: Error, t: Translate<SignalsKey> = enT): string {
  if (error instanceof ApiError) return error.problem.detail ?? error.problem.title;
  return error.message === "" ? t("error.noBody") : error.message;
}

/** investigation-signals.tsx — the Investigate page's right-hand column: the scope's loss and RTT charts. */

/*
 * The cursor markLine that used to live here is gone, and the mechanic it served
 * is not: the timeline's instant now goes into the page's cursor group
 * (lib/chart-cursor.tsx), which draws ONE line on every chart of the page rather
 * than a private one on these two. That is also what made hovering a chart able
 * to move the cursor — a markLine rebuilt per mouse position would have meant a
 * setOption per frame.
 */

/** withOverlays appends the maintenance bands to an option a caller already built. */
export function withOverlays(
  option: EChartsOption,
  opts: { windows: MaintenanceWindow[]; dark: boolean },
): EChartsOption {
  const extra = [maintenanceOverlaySeries(opts.windows, opts.dark)].filter(
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

/** deltaFromVectors is the matrix delta chip; two INSTANT evaluations rather than an average over the window. */
export function deltaFromVectors(before: PromResult | undefined, after: PromResult | undefined): MatrixDelta {
  const b = firstSample(before);
  const a = firstSample(after);
  return { before: b, after: a, delta: b === null || a === null ? null : a - b };
}

function fmtPct(v: number | null): string {
  return v === null ? "—" : `${(v * 100).toFixed(1)}%`;
}

/* "pp" is percentage POINTS, and it is a word: the Russian interface said "pp" too. The rounding
   guard is the other half — a delta of -0.0004 rendered as "−0.0 pp", which reads as a decrease
   that did not happen. */
function fmtSignedPct(v: number | null, unit = "pp"): string {
  if (v === null) return "—";
  const pct = v * 100;
  const rounded = Number(Math.abs(pct).toFixed(1));
  const sign = rounded === 0 ? "±" : pct >= 0 ? "+" : "−";
  return `${sign}${rounded.toFixed(1)} ${unit}`;
}

/** The window BOTH charts are pinned to, so loss and RTT share one x-axis. */
export interface SignalWindow {
  from: Date;
  to: Date;
}

/**
 * The name lib/curated-metrics.ts's builder gives a series whose metric carries
 * no labels — which is every series here, since both queries aggregate the
 * scope down to one. A legend reading "series" under "Packet loss" names
 * nothing, so the chart's own name replaces it; a series that DID keep a label
 * keeps its own name.
 */
const UNNAMED_SERIES = "series";

/**
 * signalChartOption is this column's OWN option builder; it composes lib/curated-metrics.ts's
 * toSeriesOption — the series.
 *
 * Two things it adds to the shared builder's answer, both about this column and
 * neither about the series: the window the reader asked for as the axis span
 * (so loss and RTT line up, and a window with no samples still draws as a
 * window rather than vanishing), and the chart's own series name where the
 * builder had none. The pills and the tooltip are lib/chart-tooltip.ts's,
 * applied at the shared chart mount, and are not restated here.
 */
export function signalChartOption(
  chart: CuratedChart,
  result: PromResult,
  dark: boolean,
  overlays: { windows: MaintenanceWindow[]; window?: SignalWindow; seriesName?: string },
): EChartsOption {
  const span = overlays.window;
  const base = toSeriesOption(chart, result, dark, span ? { start: span.from, end: span.to } : undefined);
  const named: SeriesOption[] = (Array.isArray(base.series) ? (base.series as SeriesOption[]) : []).map((s) =>
    overlays.seriesName !== undefined && s.name === UNNAMED_SERIES ? { ...s, name: overlays.seriesName } : s,
  );
  const withAxes: EChartsOption = {
    ...base,
    series: named,
    xAxis: { ...(base.xAxis as object), axisLabel: { ...(base.xAxis as { axisLabel?: object }).axisLabel, hideOverlap: true } },
    /* No yAxis override. toSeriesOption already installs the unit's own
       formatter — formatSeconds for seconds, formatRatio for ratios — and the
       override that re-stated the first case set `undefined` for the second,
       which ECharts reads as "no formatter at all": the packet-loss axis then
       printed 0.01 beside a tooltip saying 1.0%. */
  } as EChartsOption;
  return withOverlays(withAxes, { windows: overlays.windows, dark });
}

/**
 * snapModel is what the readout snaps to: every sample the two charts draw,
 * in lib/chart-cursor.tsx's own series shape. Built from the Prometheus
 * matrices directly rather than from a chart option, so it is one memo per
 * fetch and does not wait for a chart to mount.
 */
export function snapModel(...results: (PromResult | undefined)[]): ReadoutSeries[] {
  const series = results.flatMap((res) => {
    if (!res || res.status !== "success" || res.data?.resultType !== "matrix" || !Array.isArray(res.data.result)) return [];
    return (res.data.result as { values?: [number, string][] }[]).map((entry) => ({
      data: (entry.values ?? []).map(([ts, v]) => [ts * 1000, Number(v)]),
    }));
  });
  return readoutSeries({ series } as EChartsOption);
}

function SignalChart({
  id,
  title,
  seriesName,
  unit,
  result,
  error,
  windows,
  span,
  annotations,
  emptyNote,
  refusal,
}: {
  /** The chart's IDENTITY, separate from its title since the title moved into
   *  the dictionary: it used to be derived from the English words, and an id
   *  that changes with the interface language is not an id. */
  id: string;
  title: string;
  /** What the legend and the tooltip call the one series — see UNNAMED_SERIES. */
  seriesName: string;
  unit: CuratedChart["unit"];
  result: PromResult | undefined;
  /** The REJECTION, as opposed to Prometheus's own error envelope below. */
  error?: Error | null;
  windows: MaintenanceWindow[];
  /** The investigated window, pinned as the axis span on both charts. */
  span?: SignalWindow;
  annotations: Annotation[];
  emptyNote: string;
  /** OUR OWN refusal, decided before anything was fetched, so it outranks both
   *  failure shapes below: there is no rejection and no envelope to describe
   *  when the request was never sent. */
  refusal?: string;
}) {
  const t = useT(signalsDict);
  const { theme } = useTheme();
  const dark = theme === "dark";
  const chart = useMemo<CuratedChart>(() => ({ id, title, unit, query: "" }), [id, title, unit]);
  const option = useMemo(
    () => (result ? signalChartOption(chart, result, dark, { windows, window: span, seriesName }) : undefined),
    [chart, result, dark, windows, span, seriesName],
  );

  // promqlQueryRange RESOLVES Prometheus's own error envelope rather than
  // throwing, so a query-level failure shows up in the body, not as a rejection.
  const envelopeError = result?.status === "error" ? (result.error ?? t("error.queryFailed")) : undefined;
  /* Both failure shapes render as ONE line under the heading. The rejection is
     named first because it is the one that leaves no result to describe. */
  const problem = refusal ?? (error ? problemDetail(error, t) : envelopeError);
  const empty =
    result?.status === "success" && (result.data?.resultType !== "matrix" || (result.data?.result ?? []).length === 0);

  return (
    <section aria-label={title} className="mt-4 first:mt-0">
      <h3 className="text-xs font-medium text-muted-foreground">{title}</h3>
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
 * CursorReadout says, in words, which instant the page's time cursor is on. It
 * is DOM rather than a chart tooltip deliberately: a canvas marker cannot be
 * focused or read aloud.
 *
 * It subscribes to the cursor group instead of taking a prop, so that hovering a
 * CHART moves it as readily as hovering a timeline row does — and so that the
 * mousemove behind it re-renders one paragraph rather than the Investigate page.
 * The state is the FORMATTED string, which means React bails out of most frames
 * of a drag: the clock only ticks once a second.
 *
 * What it prints while a chart is hovered is the instant the chart's own axis
 * pointer snapped to — the sample the tooltip beside it is describing — not
 * the pixel under the mouse. The group carries the pixel's instant, and the
 * shared chart mount owns the pointer (its pill, its tooltip), so the snap is
 * computed here from the same samples the charts draw: `snap` is those
 * samples, lib/chart-cursor.tsx's nearestInstant is the snap, and its
 * readoutInstant is the rule that decides when it applies.
 */
function CursorReadout({ snap }: { snap: readonly ReadoutSeries[] }) {
  const t = useT(signalsDict);
  const { locale } = useLocale();
  const group = useChartCursor();
  const [text, setText] = useState(() => t("cursor.none"));

  useEffect(() => {
    if (!group) return;
    const show = () => {
      const raw = group.current();
      const fromChart = group.currentSource() !== TIMELINE_CURSOR_SOURCE;
      const snapped = raw === null || !fromChart ? null : nearestInstant(snap, raw);
      const at = readoutInstant(raw, fromChart, snapped);
      setText(at === null ? t("cursor.none") : stampClock(new Date(at), locale));
    };
    show();
    return group.subscribe(show);
  }, [group, snap, locale, t]);

  return (
    <p data-testid="signal-cursor" className="mt-2 text-[11px] text-muted-foreground">
      {/* The SAME stamp helper the timeline row's own clock uses, so the instant
          a reader is hovering reads identically in both places (QA scope 3,
          finding #18). */}
      {t("cursor", { at: text })}
    </p>
  );
}

/**
 * SignalPanels is the right-hand column: the delta chip, the two charts and the shared cursor.
 */
export function SignalPanels({
  scopeLabel,
  loss,
  lossError,
  rtt,
  rttError,
  delta,
  deltaError,
  windows,
  span,
  annotations,
  promConfigured,
  gated,
  rangeTooWide,
}: {
  scopeLabel: string;
  loss: PromResult | undefined;
  /** The loss range query's REJECTION (finding #2). */
  lossError?: Error | null;
  rtt: PromResult | undefined;
  /** The RTT range query's REJECTION (finding #2). */
  rttError?: Error | null;
  delta: MatrixDelta;
  /** The fail-ratio pair's REJECTION (QA scope 3, finding #1). Without it the
   *  chip printed "0.0% → 0.0% · +0.0 pp" over two requests that never came
   *  back — a figure, in the place a figure lives, describing nothing. */
  deltaError?: Error | null;
  windows: MaintenanceWindow[];
  /** The investigated window. Both charts pin their x-axis to it, so the loss
   *  and RTT panels share one span whatever each series happens to cover. */
  span?: SignalWindow;
  annotations: Annotation[];
  promConfigured: boolean;
  /** True when the subject holds no promql:query — the panes render their own
   *  muted line and NOTHING is fetched (the timeline's source list carries the
   *  same statement; this one keeps the empty column from looking broken). */
  gated: boolean;
  /** True when the window is wider than one query_range may be, in which case
   *  the two range queries were NOT fetched and the charts say the bound
   *  themselves — the delta chip is unaffected, its two evaluations are INSTANT
   *  queries and carry no range at all. */
  rangeTooWide: boolean;
}) {
  const t = useT(signalsDict);
  const tooWide = rangeTooWide ? t("chart.tooWide", { hours: PROMQL_MAX_RANGE_MS / 3_600_000 }) : undefined;
  /* Every sample the two charts draw, for the readout to snap to. */
  const snap = useMemo(() => snapModel(loss, rtt), [loss, rtt]);
  /* Which edge has no sample, said in one muted sentence. "— → 0.0% —" was
     three dashes and a figure, and none of the four said which end was missing. */
  const missing =
    delta.before === null && delta.after === null
      ? t("delta.noSample.both")
      : delta.before === null
        ? t("delta.noSample.start")
        : delta.after === null
          ? t("delta.noSample.end")
          : null;
  return (
    <Card asChild className="p-5">
      <section aria-label={t("title")}>
        <div className="flex flex-wrap items-center gap-2">
          <h2 className="type-section">{t("title")}</h2>
          <span className="text-xs text-muted-foreground">{scopeLabel}</span>
        </div>

        <div data-testid="matrix-delta" className="mt-3 flex flex-wrap items-center gap-2 text-xs">
          <span className="text-muted-foreground">{t("delta.failRatio")}</span>
          {deltaError ? (
            /* No numbers at all when the two evaluations were refused. A "—"
               would be honest about the VALUE and silent about the reason, and
               the reason is the half an operator can act on. */
            <span role="alert" className="leading-relaxed text-health-bad">
              {problemDetail(deltaError, t)}
            </span>
          ) : missing !== null ? (
            <span data-testid="matrix-delta-missing" className="type-meta">
              {missing}
            </span>
          ) : (
            <>
              <span className="nums">{fmtPct(delta.before)}</span>
              <span aria-hidden="true" className="text-muted-foreground">
                →
              </span>
              <span className="nums">{fmtPct(delta.after)}</span>
              <Badge variant={delta.delta === null ? "unknown" : delta.delta > 0 ? "bad" : delta.delta < 0 ? "ok" : "neutral"}>
                {fmtSignedPct(delta.delta, t("delta.unit"))}
              </Badge>
              <span className="text-[11px] text-muted-foreground">{t("delta.caption")}</span>
            </>
          )}
        </div>

        <CursorReadout snap={snap} />

        {gated ? (
          <p className="mt-3 text-xs leading-relaxed text-muted-foreground">{t("gated")}</p>
        ) : !promConfigured ? (
          <p className="mt-3 text-xs leading-relaxed text-muted-foreground">{t("promUnset")}</p>
        ) : (
          <>
            <SignalChart
              id="packet-loss"
              title={t("chart.loss")}
              seriesName={t("series.loss")}
              unit="ratio"
              result={loss}
              error={lossError}
              windows={windows}
              span={span}
              annotations={annotations}
              emptyNote={t("chart.loss.empty")}
              refusal={tooWide}
            />
            <SignalChart
              id="rtt-p95"
              title={t("chart.rtt")}
              seriesName={t("series.rtt")}
              unit="seconds"
              result={rtt}
              error={rttError}
              windows={windows}
              span={span}
              annotations={annotations}
              emptyNote={t("chart.rtt.empty")}
              refusal={tooWide}
            />
          </>
        )}
      </section>
    </Card>
  );
}
