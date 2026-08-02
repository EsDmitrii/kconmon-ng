import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { LineChart } from "lucide-react";
import { EChart } from "@/components/echart";
import { PageShell } from "@/components/page-shell";
import { Card } from "@/components/ui/card";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { useTheme } from "@/components/theme-provider";
import { promqlQueryRange } from "@/lib/api";
import { CURATED_CHARTS, toSeriesOption, type CuratedChart } from "@/lib/curated-metrics";

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

// Rounds the raw range/TARGET_POINTS step up to the next multiple of
// MIN_STEP_SECONDS, so short ranges never ask Prometheus for a sub-15s step.
function stepSecondsFor(rangeSeconds: number): number {
  const raw = rangeSeconds / TARGET_POINTS;
  return Math.ceil(raw / MIN_STEP_SECONDS) * MIN_STEP_SECONDS;
}

function useExploreQuery(chart: CuratedChart, rangeSeconds: number) {
  return useQuery({
    queryKey: ["explore", chart.id, rangeSeconds],
    queryFn: () => {
      const end = new Date();
      const start = new Date(end.getTime() - rangeSeconds * 1000);
      const stepNs = stepSecondsFor(rangeSeconds) * 1e9;
      return promqlQueryRange(chart.query, start, end, stepNs);
    },
    refetchInterval: EXPLORE_POLL_MS,
  });
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

function ExploreCard({ chart, rangeSeconds, dark }: { chart: CuratedChart; rangeSeconds: number; dark: boolean }) {
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
          <EChart option={option} className="mt-3 h-[16.5rem] w-full" />
        ) : null}
      </section>
    </Card>
  );
}

export function ExplorePage() {
  const { theme } = useTheme();
  const [rangeId, setRangeId] = useState<RangeId>("1h");
  const range = RANGE_OPTIONS.find((r) => r.value === rangeId) ?? RANGE_OPTIONS[1];

  return (
    <PageShell
      title="Explore"
      description="Curated metric charts across TCP/UDP/ICMP/DNS, recomputed from Prometheus every 30s."
      actions={
        <Segmented
          aria-label="Time range"
          options={RANGE_OPTIONS.map((r) => ({ value: r.value, label: r.label }))}
          value={range.value}
          onChange={setRangeId}
        />
      }
    >
      <div className="grid gap-5 md:grid-cols-2">
        {CURATED_CHARTS.map((chart) => (
          <ExploreCard key={chart.id} chart={chart} rangeSeconds={range.seconds} dark={theme === "dark"} />
        ))}
      </div>
    </PageShell>
  );
}
