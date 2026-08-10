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

/** The cursor series' name. Named rather than anonymous so it can be switched
 *  off from the legend like any other series. */
export const CURSOR_SERIES_NAME = "Timeline cursor";

/**
 * cursorSeries is the timeline↔chart sync, in one markLine; null (nothing hovered) returns null
 * rather than an empty series.
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

/** withOverlays appends the cursor and the maintenance bands to an option a caller already built. */
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

/** deltaFromVectors is the matrix delta chip; two INSTANT evaluations rather than an average over the window. */
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
 * signalChartOption is this column's OWN option builder; it composes lib/curated-metrics.ts's
 * toSeriesOption — the series.
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
  id,
  title,
  unit,
  result,
  error,
  cursorAt,
  windows,
  annotations,
  emptyNote,
  refusal,
}: {
  /** The chart's IDENTITY, separate from its title since the title moved into
   *  the dictionary: it used to be derived from the English words, and an id
   *  that changes with the interface language is not an id. */
  id: string;
  title: string;
  unit: CuratedChart["unit"];
  result: PromResult | undefined;
  /** The REJECTION, as opposed to Prometheus's own error envelope below. */
  error?: Error | null;
  cursorAt: Date | null;
  windows: MaintenanceWindow[];
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
    () => (result ? signalChartOption(chart, result, dark, { cursorAt, windows }) : undefined),
    [chart, result, dark, cursorAt, windows],
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
 * SignalPanels is the right-hand column: the delta chip, the two charts and the cursor readout; the
 * cursor readout is DOM, not a chart tooltip, and deliberately.
 */
export function SignalPanels({
  scopeLabel,
  loss,
  lossError,
  rtt,
  rttError,
  delta,
  deltaError,
  cursorAt,
  windows,
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
  cursorAt: Date | null;
  windows: MaintenanceWindow[];
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
  const { locale } = useLocale();
  const tooWide = rangeTooWide ? t("chart.tooWide", { hours: PROMQL_MAX_RANGE_MS / 3_600_000 }) : undefined;
  return (
    <Card asChild className="p-5">
      <section aria-label={t("title")}>
        <div className="flex flex-wrap items-center gap-2">
          <h3 className="text-sm font-semibold">{t("title")}</h3>
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
          ) : (
            <>
              <span className="nums">{fmtPct(delta.before)}</span>
              <span aria-hidden="true" className="text-muted-foreground">
                →
              </span>
              <span className="nums">{fmtPct(delta.after)}</span>
              <Badge
                variant={delta.delta === null ? "unknown" : delta.delta > 0 ? "bad" : delta.delta < 0 ? "ok" : "neutral"}
              >
                {fmtSignedPct(delta.delta)}
              </Badge>
              <span className="text-[11px] text-muted-foreground">{t("delta.caption")}</span>
            </>
          )}
        </div>

        <p data-testid="signal-cursor" className="mt-2 text-[11px] text-muted-foreground">
          {/* The SAME stamp helper the timeline row's own clock uses, so the
              instant a reader is hovering reads identically in both places
              (QA scope 3, finding #18). */}
          {t("cursor", {
            at: cursorAt === null ? t("cursor.none") : stampClock(cursorAt, locale),
          })}
        </p>

        {gated ? (
          <p className="mt-3 text-xs leading-relaxed text-muted-foreground">{t("gated")}</p>
        ) : !promConfigured ? (
          <p className="mt-3 text-xs leading-relaxed text-muted-foreground">{t("promUnset")}</p>
        ) : (
          <>
            <SignalChart
              id="packet-loss"
              title={t("chart.loss")}
              unit="ratio"
              result={loss}
              error={lossError}
              cursorAt={cursorAt}
              windows={windows}
              annotations={annotations}
              emptyNote={t("chart.loss.empty")}
              refusal={tooWide}
            />
            <SignalChart
              id="rtt-p95"
              title={t("chart.rtt")}
              unit="seconds"
              result={rtt}
              error={rttError}
              cursorAt={cursorAt}
              windows={windows}
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
