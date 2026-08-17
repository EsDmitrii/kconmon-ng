import { useMemo, useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import type * as echarts from "echarts";
import { ChevronDown, LineChart } from "lucide-react";
import { AnnotationBar, useAnnotations } from "@/components/annotations";
import { EChart } from "@/components/echart";
import { MaintenanceBar, useMaintenance } from "@/components/maintenance";
import { PageShell } from "@/components/page-shell";
import { Card } from "@/components/ui/card";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { useTheme } from "@/components/theme-provider";
import { promqlQueryRange } from "@/lib/api";
import { localeTag, useLocale, useT } from "@/lib/i18n";
import { exploreDict } from "@/lib/i18n/dict/explore";
import { GLOBAL_SCOPE } from "@/lib/annotations";
import {
  CURATED_CHARTS,
  chartTitle,
  resolveRangeToken,
  toSeriesOption,
  type CuratedChart,
  type PlotWindow,
} from "@/lib/curated-metrics";
import { useTimeContext } from "@/lib/timemachine";
import type { Annotation, MaintenanceWindow, PromResult } from "@/lib/types";

const EXPLORE_POLL_MS = 30_000;
// Datapoints per chart the step is sized for.
const TARGET_POINTS = 240;
const MIN_STEP_SECONDS = 15;

const RANGE_OPTIONS = [
  { value: "15m", label: "15m", seconds: 15 * 60 },
  { value: "1h", label: "1h", seconds: 60 * 60 },
  { value: "6h", label: "6h", seconds: 6 * 60 * 60 },
  { value: "24h", label: "24h", seconds: 24 * 60 * 60 },
] as const;

type RangeId = (typeof RANGE_OPTIONS)[number]["value"];

/* The compare panel's second axis of choice: how far back leg B is dragged. */
const SHIFT_OPTIONS = [
  { value: "", label: "No shift", seconds: 0 },
  { value: "1h", label: "1h", seconds: 60 * 60 },
  { value: "24h", label: "24h", seconds: 24 * 60 * 60 },
  { value: "7d", label: "7d", seconds: 7 * 24 * 60 * 60 },
] as const;

type ShiftId = (typeof SHIFT_OPTIONS)[number]["value"];

/** Which of the two things the panel compares A with. */
type CompareMode = "metric" | "self";

/* Leg B is drawn in A's own ramp colour, dimmed and dashed rather than given a palette of its own. */
const MUTED_OPACITY = 0.45;

// Rounds the raw range/TARGET_POINTS step up to the next multiple of
// MIN_STEP_SECONDS, so short ranges never ask Prometheus for a sub-15s step.
function stepSecondsFor(rangeSeconds: number): number {
  const raw = rangeSeconds / TARGET_POINTS;
  return Math.ceil(raw / MIN_STEP_SECONDS) * MIN_STEP_SECONDS;
}

/**
 * useExploreQuery anchors the window's END, and the Time Machine moves that
 * anchor: Live it is now, engaged it is `t`. The range picker keeps working
 * unchanged either way — 6h engaged means the six hours BEFORE t, which is what
 * an operator reading back to an incident actually wants, and is why `at`
 * replaces `end` rather than becoming a fourth range option.
 *
 * The poll goes off with it, for the same reason useTopology's does: a window
 * that ends at a fixed past instant returns the same series forever, and a
 * ticking refetch over it is a request per chart per 30s to redraw an identical
 * line.
 *
 * `shiftSeconds` is the compare panel's second leg: the SAME window dragged
 * that far into the past (end − shift, start − shift), which is why it drops
 * out of the query key at 0 — an unshifted compare leg then shares its key,
 * and therefore its request, with the curated card already asking for that
 * metric over that window.
 */
/**
 * exploreWindow is the [start, end] one leg asks Prometheus for, with the anchor
 * FLOORED onto the sample grid.
 *
 * The flooring is the whole of it. Prometheus emits a range query's samples at
 * start + k·step, so two legs anchored on two independent `new Date()` calls —
 * which is exactly what two queryFns firing milliseconds apart produce — get two
 * grids offset by that gap, and NO sample instant in common. ECharts' axis
 * tooltip collects the series that have a point at the hovered axis value, so at
 * the cursor only one leg was ever listed (owner report). Floored, both windows
 * land on the same instants by construction and the shifted leg re-timestamps
 * onto the current one sample for sample.
 *
 * Pure, and exported for exactly that reason: the alignment is a property to
 * prove, not a thing to eyeball on a chart.
 */
export function exploreWindow(
  anchorMs: number,
  rangeSeconds: number,
  shiftSeconds: number,
  stepSeconds: number,
): { start: Date; end: Date } {
  const stepMs = Math.max(stepSeconds, 1) * 1000;
  const aligned = Math.floor(anchorMs / stepMs) * stepMs;
  const end = aligned - shiftSeconds * 1000;
  return { start: new Date(end - rangeSeconds * 1000), end: new Date(end) };
}

function useExploreQuery(chart: CuratedChart, rangeSeconds: number, shiftSeconds = 0) {
  const { at } = useTimeContext();
  const base = at ? ["explore", chart.id, rangeSeconds, "at", at.toISOString()] : ["explore", chart.id, rangeSeconds];
  const query = useQuery({
    queryKey: shiftSeconds > 0 ? [...base, "shift", shiftSeconds] : base,
    queryFn: () => {
      const stepSeconds = stepSecondsFor(rangeSeconds);
      const { start, end } = exploreWindow((at ?? new Date()).getTime(), rangeSeconds, shiftSeconds, stepSeconds);
      const stepNs = stepSeconds * 1e9;
      // The curated queries carry lib/curated-metrics' RANGE_TOKEN where a Grafana panel would
      // carry $__range.
      return promqlQueryRange(resolveRangeToken(chart.query, rangeSeconds), start, end, stepNs);
    },
    refetchInterval: at ? false : EXPLORE_POLL_MS,
  });

  /* The span the AXIS shows, recomputed with each answer rather than once per
     mount: Live the anchor is `now`, and a frozen axis would leave every sample
     that arrived after mount hanging past its own maximum. Same pure function
     and the same flooring as the query itself, so the two agree by construction. */
  const window = useMemo(
    () => {
      /* The axis spans the window the READER asked for, unfloored. The QUERY floors
         its anchor onto the sample grid (exploreWindow, for the compare legs), but
         the annotation and maintenance bars fetch [anchor − range, anchor] exactly —
         so a floored axis left up to one step at each edge that the bar listed and
         no chart could draw, and ECharts drops a mark outside the extent rather
         than clamping it. */
      const anchorMs = (at ?? new Date()).getTime() - shiftSeconds * 1000;
      return { start: new Date(anchorMs - rangeSeconds * 1000), end: new Date(anchorMs) };
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps -- dataUpdatedAt IS the "new answer" signal
    [at, rangeSeconds, shiftSeconds, query.dataUpdatedAt],
  );

  return { query, window };
}

/** One leg of the compare panel: a curated query, its data, and its identity. */
export interface CompareLeg {
  chart: CuratedChart;
  /** Legend prefix — "A: TCP RTT p95…" or "A (24h earlier)". */
  label: string;
  data: PromResult;
  /** Milliseconds to add back to this leg's timestamps so it overlays A. */
  shiftMs?: number;
}

function legSeries(leg: CompareLeg, dark: boolean, muted: boolean): echarts.LineSeriesOption[] {
  const built = (toSeriesOption(leg.chart, leg.data, dark).series ?? []) as echarts.LineSeriesOption[];
  const shiftMs = leg.shiftMs ?? 0;
  return built.map((s) => ({
    ...s,
    name: `${leg.label} · ${s.name ?? ""}`,
    ...(muted
      ? {
          lineStyle: { ...s.lineStyle, type: "dashed" as const, opacity: MUTED_OPACITY },
          itemStyle: { opacity: MUTED_OPACITY },
        }
      : {}),
    data:
      shiftMs === 0
        ? s.data
        : ((s.data ?? []) as [number, number][]).map(([ts, v]): [number, number] => [ts + shiftMs, v]),
  }));
}

/** Merges two legs onto ONE pair of axes — A's, built by toSeriesOption exactly as the curated cards build theirs. */
export function toCompareOption(
  a: CompareLeg,
  b: CompareLeg | undefined,
  dark: boolean,
  window?: PlotWindow,
): echarts.EChartsOption {
  /* A's window for both: leg B is re-timestamped onto it in legSeries. */
  const base = toSeriesOption(a.chart, a.data, dark, window);
  return {
    ...base,
    series: [...legSeries(a, dark, false), ...(b ? legSeries(b, dark, true) : [])],
  };
}

function ChartSkeleton() {
  const t = useT(exploreDict);
  return (
    <div role="status" aria-live="polite" className="mt-3">
      <span className="sr-only">{t("chart.loading")}</span>
      <Skeleton className="h-[16.5rem] w-full" />
    </div>
  );
}

function ChartEmpty() {
  const t = useT(exploreDict);
  return (
    <div className="mt-3 flex h-[16.5rem] flex-col items-center justify-center gap-2 rounded-md bg-surface-2/40 text-center">
      <LineChart aria-hidden="true" className="size-5 text-muted-foreground" />
      <p className="text-xs text-muted-foreground">{t("chart.empty")}</p>
    </div>
  );
}

function ExploreCard({
  chart,
  rangeSeconds,
  dark,
  annotations,
  maintenance,
}: {
  chart: CuratedChart;
  rangeSeconds: number;
  dark: boolean;
  annotations: Annotation[];
  maintenance: MaintenanceWindow[];
}) {
  const t = useT(exploreDict);
  /* `chart.title` is the ENGLISH source field and the fixture tests read;
     chartTitle is the one place display language is decided. */
  const { locale } = useLocale();
  const { query, window } = useExploreQuery(chart, rangeSeconds);
  const { data, isPending, error } = query;
  const option = useMemo(
    () => (data ? toSeriesOption(chart, data, dark, window) : undefined),
    [chart, data, dark, window],
  );
  // promqlQueryRange resolves (rather than throws) for Prometheus's own error envelope (see
  // lib/api.ts).
  const queryError = data?.status === "error" ? (data.error ?? t("chart.queryFailed")) : undefined;
  const empty =
    data?.status === "success" &&
    (data.data?.resultType !== "matrix" || (data.data?.result ?? []).length === 0);

  return (
    <Card asChild interactive className="p-5">
      <section>
        <h2 className="text-sm font-semibold">{chartTitle(chart, locale)}</h2>
        {error ? (
          <p role="alert" className="mt-3 text-sm text-health-bad">{error.message}</p>
        ) : null}
        {queryError ? (
          <p role="alert" className="mt-3 text-sm text-health-bad">{queryError}</p>
        ) : null}
        {/* isPending, not isLoading — see the matrix surface. */}
        {isPending && !data ? <ChartSkeleton /> : null}
        {empty ? <ChartEmpty /> : null}
        {option && !empty && !queryError ? (
          <EChart
            option={option}
            annotations={annotations}
            maintenance={maintenance}
            dark={dark}
            className="mt-3 h-[16.5rem] w-full"
          />
        ) : null}
      </section>
    </Card>
  );
}

/* A native <select> dressed in kit tokens — same treatment as /live's type
   filter: the platform picker keeps its keyboard, mobile and screen-reader
   behaviour, only the closed face is restyled. */
function CompareSelect({
  label,
  value,
  onChange,
  disabled,
  children,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  children: ReactNode;
}) {
  return (
    <span className="relative inline-flex">
      <select
        aria-label={label}
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        className="h-9 appearance-none rounded-md bg-surface-2 py-1 pl-3 pr-8 text-sm text-foreground transition-colors duration-(--dur-fast) ease-(--ease) focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50"
      >
        {children}
      </select>
      <ChevronDown
        aria-hidden="true"
        className="pointer-events-none absolute right-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground"
      />
    </span>
  );
}

/**
 * isEmptyMatrix is "the query succeeded and matched nothing" — as opposed to "it failed" or "it has
 * not answered yet".
 */
function isEmptyMatrix(res: PromResult | undefined): boolean {
  return (
    res?.status === "success" && (res.data?.resultType !== "matrix" || (res.data?.result ?? []).length === 0)
  );
}

/*
 * It is only for the SHIFTED leg: a second METRIC coming back empty is a fact about that metric
 * over the visible window.
 */

/* Mounted only once a comparison is actually chosen — an idle panel must not cost a request. */
function CompareChart({
  chartA,
  chartB,
  labelA,
  labelB,
  shiftSeconds,
  shiftLabel,
  rangeSeconds,
  dark,
  annotations,
  maintenance,
}: {
  chartA: CuratedChart;
  chartB: CuratedChart | undefined;
  labelA: string;
  labelB: string;
  shiftSeconds: number;
  /** The preset's own label ("24h", "7d"), so the note below can say how far
   *  back the missing window was rather than printing seconds. Empty while no
   *  shift is chosen. */
  shiftLabel: string;
  rangeSeconds: number;
  dark: boolean;
  annotations: Annotation[];
  maintenance: MaintenanceWindow[];
}) {
  const t = useT(exploreDict);
  const legB = chartB ?? chartA;
  const a = useExploreQuery(chartA, rangeSeconds);
  const b = useExploreQuery(legB, rangeSeconds, shiftSeconds);

  const option = useMemo(() => {
    if (!a.query.data) return undefined;
    return toCompareOption(
      { chart: chartA, label: labelA, data: a.query.data },
      b.query.data
        ? { chart: legB, label: labelB, data: b.query.data, shiftMs: shiftSeconds * 1000 }
        : undefined,
      dark,
      a.window,
    );
  }, [a.query.data, a.window, b.query.data, chartA, legB, labelA, labelB, shiftSeconds, dark]);

  const error = a.query.error ?? b.query.error;
  const queryError = [a.query.data, b.query.data].find((d) => d?.status === "error");
  /* Nothing to draw at all — as opposed to "one leg is missing", which the
     legend already names, and which is still a chart worth looking at.
     The panel had no empty state: leg A answering with an empty matrix still
     produced an option, so it drew a labelled, gridded, entirely BLANK
     rectangle above five curated cards that were all honestly saying "no series
     returned for this range". Same fact, and the one panel whose contents the
     reader chose themselves was the only one that would not say it. */
  const nothingDrawn = ((option?.series ?? []) as unknown[]).length === 0;
  // The reference leg answered, and answered with nothing. Only meaningful for
  // a time shift — see shiftedLegEmptyNote.
  const shiftedLegEmpty = shiftSeconds > 0 && isEmptyMatrix(b.query.data);

  return (
    <>
      {error ? <p role="alert" className="mt-3 text-sm text-health-bad">{error.message}</p> : null}
      {queryError ? (
        <p role="alert" className="mt-3 text-sm text-health-bad">{queryError.error ?? t("chart.queryFailed")}</p>
      ) : null}
      {!option && !error ? <ChartSkeleton /> : null}
      {option && !queryError && nothingDrawn ? <ChartEmpty /> : null}
      {option && !queryError && !nothingDrawn ? (
        <EChart
          option={option}
          annotations={annotations}
          maintenance={maintenance}
          dark={dark}
          className="mt-3 h-[16.5rem] w-full"
        />
      ) : null}
      {/* Under the chart, not instead of it: leg A is real data and stays
          drawn. What the note removes is the silent reading that B is
          identical to it. */}
      {shiftedLegEmpty && !queryError ? (
        <p role="status" className="mt-2 text-xs leading-relaxed text-muted-foreground">
          {t("compare.shiftedEmpty", { shift: shiftLabel })}
        </p>
      ) : null}
    </>
  );
}

/** The compare panel: one extra chart above the curated grid that puts a second leg on A's axes. */
function ComparePanel({
  rangeSeconds,
  dark,
  annotations,
  maintenance,
}: {
  rangeSeconds: number;
  dark: boolean;
  annotations: Annotation[];
  maintenance: MaintenanceWindow[];
}) {
  const t = useT(exploreDict);
  const { locale } = useLocale();
  const [aId, setAId] = useState<string>(CURATED_CHARTS[0].id);
  const [bId, setBId] = useState<string>("");
  const [shiftId, setShiftId] = useState<ShiftId>("");
  /* The panel compares A with ONE of two things, and which one is now a control
     rather than a consequence. Choosing a shift used to silently retire the B
     metric and its lines — «куда делись ноды» — with a line of fine print under
     the chart as the only notice. */
  const [mode, setMode] = useState<CompareMode>("metric");

  const chartA = CURATED_CHARTS.find((c) => c.id === aId) ?? CURATED_CHARTS[0];
  const shift = SHIFT_OPTIONS.find((s) => s.value === shiftId) ?? SHIFT_OPTIONS[0];
  const shifted = mode === "self" && shift.seconds > 0;
  /* `bId` is KEPT while the reader is in self-shift mode rather than cleared:
     coming back must restore the metric they picked, not an empty select.
     Derived, not an effect: picking A as B too would draw the same line twice,
     so that selection simply reads as "none" until one of them moves. */
  const chartB = mode === "self" || bId === aId ? undefined : CURATED_CHARTS.find((c) => c.id === bId);
  const active = shifted || chartB !== undefined;
  const unitsDiffer = chartB !== undefined && chartB.unit !== chartA.unit;

  return (
    <Card asChild interactive className="p-5">
      <section aria-label={t("compare.title")}>
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
          <h2 className="text-sm font-semibold">{t("compare.title")}</h2>
          <CompareSelect label={t("compare.metricA")} value={aId} onChange={setAId}>
            {CURATED_CHARTS.map((c) => (
              <option key={c.id} value={c.id}>
                {chartTitle(c, locale)}
              </option>
            ))}
          </CompareSelect>
          <Segmented
            aria-label={t("compare.mode.aria")}
            options={[
              { value: "metric", label: t("compare.mode.metric") },
              { value: "self", label: t("compare.mode.self") },
            ]}
            value={mode}
            onChange={(v) => setMode(v as CompareMode)}
          />
          {/* ONE picker, in one place. The other is not rendered at all: a
              greyed-out neighbour is still a control the reader has to work out
              the state of, and working it out is what the fine print was for. */}
          {mode === "metric" ? (
            <CompareSelect label={t("compare.metricB")} value={bId} onChange={setBId}>
              <option value="">{t("compare.metricB.none")}</option>
              {CURATED_CHARTS.filter((c) => c.id !== aId).map((c) => (
                <option key={c.id} value={c.id}>
                  {chartTitle(c, locale)}
                </option>
              ))}
            </CompareSelect>
          ) : (
            <CompareSelect label={t("compare.shift")} value={shiftId} onChange={(v) => setShiftId(v as ShiftId)}>
              {SHIFT_OPTIONS.map((s) => (
                <option key={s.value} value={s.value}>
                  {s.value === "" ? t("compare.shift.none") : t("compare.shift.earlier", { label: s.label })}
                </option>
              ))}
            </CompareSelect>
          )}
        </div>

        {unitsDiffer ? (
          <p className="mt-2 text-xs text-muted-foreground">
            {t("compare.unitsDiffer", {
              unitB: chartB.unit === "ratio" ? t("compare.unitB.ratio") : t("compare.unitB.duration"),
              unitA: chartA.unit === "ratio" ? t("compare.unitA.ratio") : t("compare.unitA.seconds"),
            })}
          </p>
        ) : null}

        {active ? (
          <CompareChart
            chartA={chartA}
            chartB={chartB}
            /* In self-shift mode both legs ARE the same metric, so repeating its
               title twice says nothing; what distinguishes them is which clock
               each is on. The stroke is named too, because the two legs share a
               colour and a dashed line is the only other thing telling them
               apart — greyscale, colour-blindness and a printed screenshot all
               keep the words. */
            labelA={
              shifted ? t("compare.legA.now") : t("compare.legA", { title: chartTitle(chartA, locale) })
            }
            labelB={
              shifted
                ? t("compare.legB.earlier", { label: shift.label })
                : t("compare.legB", { title: chartB ? chartTitle(chartB, locale) : "" })
            }
            /* Gated on `shifted`, exactly as the two labels above are: the shift
               belongs to self-compare mode, and leaving it applied after a switch
               back to metric-B drew B from a window an hour or a week ago under a
               label that said "now". */
            shiftSeconds={shifted ? shift.seconds : 0}
            shiftLabel={shifted ? shift.label : ""}
            rangeSeconds={rangeSeconds}
            dark={dark}
            annotations={annotations}
            maintenance={maintenance}
          />
        ) : (
          <p className="mt-3 text-xs text-muted-foreground">{t("compare.idle")}</p>
        )}
      </section>
    </Card>
  );
}

export function ExplorePage() {
  const t = useT(exploreDict);
  const { locale } = useLocale();
  const { theme } = useTheme();
  const { at } = useTimeContext();
  const [rangeId, setRangeId] = useState<RangeId>("1h");
  const range = RANGE_OPTIONS.find((r) => r.value === rangeId) ?? RANGE_OPTIONS[1];
  /* Explore is a GLOBAL surface, and deliberately only that. */
  const { annotations, error: annotationsError, refresh } = useAnnotations(GLOBAL_SCOPE, range.seconds);
  /* The declared change windows, on exactly the same terms: global scope, the range picker's own window. */
  const {
    windows,
    error: maintenanceError,
    refresh: refreshMaintenance,
  } = useMaintenance(GLOBAL_SCOPE, range.seconds);

  return (
    <PageShell
      timeMachine
      title={t("title")}
      /* {at} lands INSIDE a translated sentence, so it takes that sentence's
         language — lib/i18n's localeTag. Computed here and passed in, never
         formatted by the dictionary (QA scope 2, finding #8). */
      description={at ? t("description.at", { at: at.toLocaleString(localeTag(locale)) }) : t("description")}
      actions={
        <Segmented
          aria-label={t("range.aria")}
          options={RANGE_OPTIONS.map((r) => ({ value: r.value, label: r.label }))}
          value={range.value}
          onChange={setRangeId}
        />
      }
    >
      <AnnotationBar
        scope={GLOBAL_SCOPE}
        annotations={annotations}
        error={annotationsError}
        onChanged={() => void refresh()}
        className="mt-0"
      />

      <MaintenanceBar
        scope={GLOBAL_SCOPE}
        windows={windows}
        error={maintenanceError}
        onChanged={() => void refreshMaintenance()}
        className="mt-0"
      />

      <ComparePanel
        rangeSeconds={range.seconds}
        dark={theme === "dark"}
        annotations={annotations}
        maintenance={windows}
      />

      <div className="grid gap-5 md:grid-cols-2">
        {CURATED_CHARTS.map((chart) => (
          <ExploreCard
            key={chart.id}
            chart={chart}
            rangeSeconds={range.seconds}
            dark={theme === "dark"}
            annotations={annotations}
            maintenance={windows}
          />
        ))}
      </div>
    </PageShell>
  );
}
