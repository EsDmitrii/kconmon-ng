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
import { GLOBAL_SCOPE } from "@/lib/annotations";
import { CURATED_CHARTS, toSeriesOption, type CuratedChart } from "@/lib/curated-metrics";
import { useTimeContext } from "@/lib/timemachine";
import type { Annotation, MaintenanceWindow, PromResult } from "@/lib/types";

const EXPLORE_POLL_MS = 30_000;
// Datapoints per chart the step is sized for; the server-side guard this
// stays under is the absolute range cap (promql.ErrRangeTooLarge — end-start
// vs MaxRange), not a range/step ratio — the largest range here (24h) is
// always within MaxRange (default 24h) regardless of step.
const TARGET_POINTS = 240;
const MIN_STEP_SECONDS = 15;

const RANGE_OPTIONS = [
  { value: "15m", label: "15m", seconds: 15 * 60 },
  { value: "1h", label: "1h", seconds: 60 * 60 },
  { value: "6h", label: "6h", seconds: 6 * 60 * 60 },
  { value: "24h", label: "24h", seconds: 24 * 60 * 60 },
] as const;

type RangeId = (typeof RANGE_OPTIONS)[number]["value"];

/* The compare panel's second axis of choice: how far back leg B is dragged.
   "" is the off position and keeps the panel in metric-B mode. */
const SHIFT_OPTIONS = [
  { value: "", label: "No shift", seconds: 0 },
  { value: "1h", label: "1h", seconds: 60 * 60 },
  { value: "24h", label: "24h", seconds: 24 * 60 * 60 },
  { value: "7d", label: "7d", seconds: 7 * 24 * 60 * 60 },
] as const;

type ShiftId = (typeof SHIFT_OPTIONS)[number]["value"];

/* Leg B is drawn in A's own ramp colour, dimmed and dashed rather than given a
   palette of its own: two unrelated colour families on one pair of axes read as
   ten independent series, and the point of the panel is that B is the
   REFERENCE and A is the subject. */
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
function useExploreQuery(chart: CuratedChart, rangeSeconds: number, shiftSeconds = 0) {
  const { at } = useTimeContext();
  const base = at ? ["explore", chart.id, rangeSeconds, "at", at.toISOString()] : ["explore", chart.id, rangeSeconds];
  return useQuery({
    queryKey: shiftSeconds > 0 ? [...base, "shift", shiftSeconds] : base,
    queryFn: () => {
      const anchor = at ?? new Date();
      const end = new Date(anchor.getTime() - shiftSeconds * 1000);
      const start = new Date(end.getTime() - rangeSeconds * 1000);
      const stepNs = stepSecondsFor(rangeSeconds) * 1e9;
      return promqlQueryRange(chart.query, start, end, stepNs);
    },
    refetchInterval: at ? false : EXPLORE_POLL_MS,
  });
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

/**
 * Merges two legs onto ONE pair of axes — A's, built by toSeriesOption exactly
 * as the curated cards build theirs, so the compare chart cannot drift from the
 * charts below it.
 *
 * A shifted leg gets its shift added BACK to every timestamp: "compare with 24h
 * earlier" means overlaid on the visible window, not drawn a day off the left
 * edge of it. The legend label is what tells you which line is which instant.
 *
 * B is optional so leg A paints the moment it arrives rather than waiting on a
 * shifted window that may be a slower query.
 */
export function toCompareOption(a: CompareLeg, b: CompareLeg | undefined, dark: boolean): echarts.EChartsOption {
  const base = toSeriesOption(a.chart, a.data, dark);
  return {
    ...base,
    series: [...legSeries(a, dark, false), ...(b ? legSeries(b, dark, true) : [])],
  };
}

function ChartSkeleton() {
  return (
    <div role="status" aria-live="polite" className="mt-3">
      <span className="sr-only">Loading chart…</span>
      <Skeleton className="h-[16.5rem] w-full" />
    </div>
  );
}

function ChartEmpty() {
  return (
    <div className="mt-3 flex h-[16.5rem] flex-col items-center justify-center gap-2 rounded-md bg-surface-2/40 text-center">
      <LineChart aria-hidden="true" className="size-5 text-muted-foreground" />
      <p className="text-xs text-muted-foreground">
        No series returned for this range — try a longer window above.
      </p>
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
  const { data, isLoading, error } = useExploreQuery(chart, rangeSeconds);
  const option = useMemo(() => (data ? toSeriesOption(chart, data, dark) : undefined), [chart, data, dark]);
  // promqlQueryRange resolves (rather than throws) for Prometheus's own error
  // envelope (see lib/api.ts), so a query-level failure surfaces via
  // data.status, not react-query's `error`.
  const queryError = data?.status === "error" ? (data.error ?? "query failed") : undefined;
  const empty =
    data?.status === "success" &&
    (data.data?.resultType !== "matrix" || (data.data?.result ?? []).length === 0);

  return (
    <Card asChild interactive className="p-5">
      <section>
        <h2 className="text-sm font-semibold">{chart.title}</h2>
        {error ? (
          <p role="alert" className="mt-3 text-sm text-health-bad">{error.message}</p>
        ) : null}
        {queryError ? (
          <p role="alert" className="mt-3 text-sm text-health-bad">{queryError}</p>
        ) : null}
        {isLoading && !data ? <ChartSkeleton /> : null}
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

/* Mounted only once a comparison is actually chosen — an idle panel must not
   cost a request. Both legs go through useExploreQuery, so Time Machine
   anchoring, stepping and polling are the page's, not a second implementation
   of them. */
function CompareChart({
  chartA,
  chartB,
  labelA,
  labelB,
  shiftSeconds,
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
  rangeSeconds: number;
  dark: boolean;
  annotations: Annotation[];
  maintenance: MaintenanceWindow[];
}) {
  const legB = chartB ?? chartA;
  const a = useExploreQuery(chartA, rangeSeconds);
  const b = useExploreQuery(legB, rangeSeconds, shiftSeconds);

  const option = useMemo(() => {
    if (!a.data) return undefined;
    return toCompareOption(
      { chart: chartA, label: labelA, data: a.data },
      b.data ? { chart: legB, label: labelB, data: b.data, shiftMs: shiftSeconds * 1000 } : undefined,
      dark,
    );
  }, [a.data, b.data, chartA, legB, labelA, labelB, shiftSeconds, dark]);

  const error = a.error ?? b.error;
  const queryError = [a.data, b.data].find((d) => d?.status === "error");

  return (
    <>
      {error ? <p role="alert" className="mt-3 text-sm text-health-bad">{error.message}</p> : null}
      {queryError ? (
        <p role="alert" className="mt-3 text-sm text-health-bad">{queryError.error ?? "query failed"}</p>
      ) : null}
      {!option && !error ? <ChartSkeleton /> : null}
      {option && !queryError ? (
        <EChart
          option={option}
          annotations={annotations}
          maintenance={maintenance}
          dark={dark}
          className="mt-3 h-[16.5rem] w-full"
        />
      ) : null}
    </>
  );
}

/**
 * The compare panel: one extra chart above the curated grid that puts a second
 * leg on A's axes. Two mutually exclusive modes — a second curated metric, or a
 * time shift of A against its own past — because "B is UDP loss AND B is
 * yesterday's A" has no single honest reading.
 *
 * There is deliberately NO free-form PromQL box here. Writing a query is
 * /console's job; Explore stays a curated surface, and a text field would make
 * this page a worse copy of one that already exists.
 */
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
  const [aId, setAId] = useState<string>(CURATED_CHARTS[0].id);
  const [bId, setBId] = useState<string>("");
  const [shiftId, setShiftId] = useState<ShiftId>("");

  const chartA = CURATED_CHARTS.find((c) => c.id === aId) ?? CURATED_CHARTS[0];
  const shift = SHIFT_OPTIONS.find((s) => s.value === shiftId) ?? SHIFT_OPTIONS[0];
  const shifted = shift.seconds > 0;
  // Derived, not an effect: picking A as B too would draw the same line twice,
  // so that selection simply reads as "none" until one of them moves.
  const chartB = shifted || bId === aId ? undefined : CURATED_CHARTS.find((c) => c.id === bId);
  const active = shifted || chartB !== undefined;
  const unitsDiffer = chartB !== undefined && chartB.unit !== chartA.unit;

  return (
    <Card asChild interactive className="p-5">
      <section aria-label="Compare">
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
          <h2 className="text-sm font-semibold">Compare</h2>
          <CompareSelect label="Metric A" value={aId} onChange={setAId}>
            {CURATED_CHARTS.map((c) => (
              <option key={c.id} value={c.id}>
                {c.title}
              </option>
            ))}
          </CompareSelect>
          <span className="text-xs text-muted-foreground">with</span>
          <CompareSelect label="Compare with metric" value={bId} onChange={setBId} disabled={shifted}>
            <option value="">No second metric</option>
            {CURATED_CHARTS.filter((c) => c.id !== aId).map((c) => (
              <option key={c.id} value={c.id}>
                {c.title}
              </option>
            ))}
          </CompareSelect>
          <span className="text-xs text-muted-foreground">or itself</span>
          <CompareSelect label="Compare with earlier" value={shiftId} onChange={(v) => setShiftId(v as ShiftId)}>
            {SHIFT_OPTIONS.map((s) => (
              <option key={s.value} value={s.value}>
                {s.value === "" ? s.label : `${s.label} earlier`}
              </option>
            ))}
          </CompareSelect>
        </div>

        {shifted ? (
          <p className="mt-2 text-xs text-muted-foreground">
            A time shift compares A with itself, so the second-metric picker is off while one is chosen. The earlier leg
            is drawn over the same window, dimmed and dashed.
          </p>
        ) : null}
        {unitsDiffer ? (
          <p className="mt-2 text-xs text-muted-foreground">
            B is a {chartB.unit === "ratio" ? "ratio" : "duration"} on A&apos;s {chartA.unit} axis — read its shape, not
            its height.
          </p>
        ) : null}

        {active ? (
          <CompareChart
            chartA={chartA}
            chartB={chartB}
            labelA={`A: ${chartA.title}`}
            labelB={shifted ? `A (${shift.label} earlier)` : `B: ${chartB?.title ?? ""}`}
            shiftSeconds={shift.seconds}
            rangeSeconds={rangeSeconds}
            dark={dark}
            annotations={annotations}
            maintenance={maintenance}
          />
        ) : (
          <p className="mt-3 text-xs text-muted-foreground">
            Pick a second metric or an earlier window to overlay a reference leg on A&apos;s axes. Nothing is queried
            until you do.
          </p>
        )}
      </section>
    </Card>
  );
}

export function ExplorePage() {
  const { theme } = useTheme();
  const { at } = useTimeContext();
  const [rangeId, setRangeId] = useState<RangeId>("1h");
  const range = RANGE_OPTIONS.find((r) => r.value === rangeId) ?? RANGE_OPTIONS[1];
  /* Explore is a GLOBAL surface, and deliberately only that: these five charts
     are fleet-wide curated aggregates with no object identity at all — "TCP RTT
     p95, worst 5 pairs" is not about node-a, and hanging node-a's private note
     across it would be a mark whose subject the chart cannot show. So the page
     reads (and writes) scope "" only, and an operator who wants a note on one
     node puts it on that node's card.

     One hook for the whole page rather than one per card: all five charts share
     the range picker's window, so they share the request and the markers. */
  const { annotations, error: annotationsError, refresh } = useAnnotations(GLOBAL_SCOPE, range.seconds);
  /* The declared change windows, on exactly the same terms (M6 Task 9): global
     scope, the range picker's own window, one hook for the whole page. A
     fleet-wide upgrade is precisely the context these five aggregates are
     missing when they all bend at once. */
  const {
    windows,
    error: maintenanceError,
    refresh: refreshMaintenance,
  } = useMaintenance(GLOBAL_SCOPE, range.seconds);

  return (
    <PageShell
      title="Explore"
      description={
        at
          ? `Curated metric charts across TCP/UDP/ICMP/DNS, ending ${at.toLocaleString()} — the range below is measured back from there.`
          : "Curated metric charts across TCP/UDP/ICMP/DNS, recomputed from Prometheus every 30s."
      }
      actions={
        <Segmented
          aria-label="Time range"
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
