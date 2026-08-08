import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { AnnotationBar, useAnnotations } from "@/components/annotations";
import { EChart } from "@/components/echart";
import { PageShell } from "@/components/page-shell";
import { RecentChanges } from "@/components/recent-changes";
import { useTheme } from "@/components/theme-provider";
import { Badge, type BadgeProps } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { useAuth } from "@/hooks/use-auth";
import { useMatrix } from "@/hooks/use-matrix";
import { ApiError, createRun, getRun, getRuns, goTo, promqlQueryRange } from "@/lib/api";
import { toSeriesOption, type CuratedChart } from "@/lib/curated-metrics";
import { useTimeContext, useWritesDisabled } from "@/lib/timemachine";
import type { MatrixCell, RunDetail, RunResult } from "@/lib/types";
import { escapeLabelValue } from "@/lib/utils";

const PAIR_PATH_PREFIX = "/pairs/";

/**
 * pairFromPath reads {source, destination} straight off
 * window.location.pathname, the same convention run-detail.tsx's
 * runIdFromPath and node-card.tsx's nodeNameFromPath already use for the
 * same reasons (plain-render testability, correctness on a cold bookmarked
 * load). The two segments are separated by a literal "/", so only the FIRST
 * slash after the prefix is the separator — encodeURIComponent always
 * escapes a literal "/" inside either name to "%2F", so a raw "/" here can
 * only be the one the route itself inserted between source and destination.
 */
export function pairFromPath(pathname: string): { source: string; destination: string } {
  if (!pathname.startsWith(PAIR_PATH_PREFIX)) return { source: "", destination: "" };
  const rest = pathname.slice(PAIR_PATH_PREFIX.length);
  const slash = rest.indexOf("/");
  if (slash === -1) return { source: "", destination: "" };
  const rawSource = rest.slice(0, slash);
  const rawDestination = rest.slice(slash + 1);
  try {
    return { source: decodeURIComponent(rawSource), destination: decodeURIComponent(rawDestination) };
  } catch {
    return { source: rawSource, destination: rawDestination };
  }
}

/** pairScope mirrors internal/console/events/live_event.go's own pairScope
 * exactly -- U+2192, never a hyphen-arrow. This is the one string the
 * RecentChanges rail below is pinned to. */
export function pairScope(source: string, destination: string): string {
  return `${source}→${destination}`;
}

type Tier = "ok" | "warn" | "bad" | "unknown";

const TIER_VARIANT: Record<Tier, NonNullable<BadgeProps["variant"]>> = {
  ok: "ok",
  warn: "warn",
  bad: "bad",
  unknown: "unknown",
};

function tierOf(cell: MatrixCell | undefined): Tier {
  if (!cell || cell.failRatio === null) return "unknown";
  if (cell.failRatio < 0.01) return "ok";
  if (cell.failRatio < 0.1) return "warn";
  return "bad";
}

function fmtFail(ratio: number): string {
  return `${(100 * ratio).toFixed(1)}%`;
}

function fmtDuration(ns?: number): string {
  return ns === undefined ? "—" : `${(ns / 1e6).toFixed(0)}ms`;
}

function fmtTime(ts?: string): string {
  if (!ts) return "—";
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? ts : d.toLocaleString();
}

/** DirectionStat renders one directed leg's fail ratio as a labelled badge --
 * the pair card's header shows BOTH legs (src→dst and dst→src) side by side,
 * since a pair's two directions can and do disagree. */
function DirectionStat({ label, cell }: { label: string; cell?: MatrixCell }) {
  const tier = tierOf(cell);
  return (
    <span className="flex items-center gap-1.5 text-xs">
      <span className="text-muted-foreground">{label}</span>
      <Badge variant={TIER_VARIANT[tier]} dot>
        {cell?.failRatio == null ? "no data" : fmtFail(cell.failRatio)}
      </Badge>
    </span>
  );
}

/**
 * pairSeriesQuery mirrors curated-metrics.ts's own fail-rate query shape:
 * three protocols' RTT p95 requested as one PromQL query, each tagged with a
 * synthetic `protocol` label via label_replace so curated-metrics.ts's own
 * toSeriesOption renders this exactly like any curated chart. Metric names
 * and the peer label set (source_node, destination_node) are verified
 * against docs/metrics.md.
 */
export function pairSeriesQuery(source: string, destination: string): string {
  const sel = `source_node="${escapeLabelValue(source)}",destination_node="${escapeLabelValue(destination)}"`;
  const p95 = (metric: string) => `histogram_quantile(0.95, sum by (le) (rate(${metric}{${sel}}[5m])))`;
  return (
    "sum by (protocol) (" +
    `label_replace(${p95("kconmon_ng_tcp_total_duration_seconds_bucket")}, "protocol", "tcp", "", "") or ` +
    `label_replace(${p95("kconmon_ng_udp_rtt_seconds_bucket")}, "protocol", "udp", "", "") or ` +
    `label_replace(${p95("kconmon_ng_icmp_rtt_seconds_bucket")}, "protocol", "icmp", "", "")` +
    ")"
  );
}

const PAIR_RANGE_SECONDS = 60 * 60;
const PAIR_TARGET_POINTS = 120;
const PAIR_MIN_STEP_SECONDS = 15;

function PairOverviewTab({ source, destination }: { source: string; destination: string }) {
  const { theme } = useTheme();
  /* The pair's own scope is the SAME string the RecentChanges rail and the
     controller's live events use — pairScope's U+2192, never a hyphen-arrow.
     Getting it wrong here would file notes under a scope nothing else in the
     console ever reads. Global marks come along for the ride (useAnnotations
     fetches both legs), because a fleet-wide event is exactly the context a
     single pair's chart is missing. */
  const scope = pairScope(source, destination);
  const { annotations, error: annotationsError, refresh } = useAnnotations(scope, PAIR_RANGE_SECONDS);
  const chart = useMemo<CuratedChart>(
    () => ({ id: "pair-rtt", title: "RTT p95 by protocol", unit: "seconds", query: pairSeriesQuery(source, destination) }),
    [source, destination],
  );
  // Engaged, the window ends at `t` rather than now — "state as of t" for a
  // chart means the hour BEFORE t, not the hour before this render.
  const { at } = useTimeContext();
  const { data, isLoading, error } = useQuery({
    queryKey: at
      ? ["pair-series", source, destination, "at", at.toISOString()]
      : ["pair-series", source, destination],
    queryFn: () => {
      const end = at ?? new Date();
      const start = new Date(end.getTime() - PAIR_RANGE_SECONDS * 1000);
      const stepSeconds =
        Math.ceil(PAIR_RANGE_SECONDS / PAIR_TARGET_POINTS / PAIR_MIN_STEP_SECONDS) * PAIR_MIN_STEP_SECONDS;
      return promqlQueryRange(chart.query, start, end, stepSeconds * 1e9);
    },
  });
  const option = useMemo(() => (data ? toSeriesOption(chart, data, theme === "dark") : undefined), [chart, data, theme]);
  // promqlQueryRange resolves (rather than throws) for Prometheus's own error
  // envelope -- see lib/api.ts's `handle` -- so a query-level failure surfaces
  // via data.status, not react-query's `error`.
  const queryError = data?.status === "error" ? (data.error ?? "query failed") : undefined;
  const empty = data?.status === "success" && (data.data?.resultType !== "matrix" || (data.data?.result ?? []).length === 0);

  return (
    <Card asChild className="p-5">
      <section>
        <h3 className="text-sm font-semibold">
          RTT p95 by protocol {at ? `(hour ending ${at.toLocaleString()})` : "(last hour)"}
        </h3>
        {error ? (
          <p role="alert" className="mt-3 text-sm text-health-bad">
            {error.message}
          </p>
        ) : null}
        {queryError ? (
          <p role="alert" className="mt-3 text-sm text-health-bad">
            {queryError}
          </p>
        ) : null}
        {isLoading && !data ? <Skeleton className="mt-3 h-64 w-full" /> : null}
        {empty ? (
          <p className="mt-3 text-xs text-muted-foreground">
            {at
              ? "No series returned for this pair in the hour before that instant."
              : "No series returned for this pair in the last hour."}
          </p>
        ) : null}
        {option && !empty && !queryError ? (
          <EChart option={option} annotations={annotations} dark={theme === "dark"} className="mt-3 h-64 w-full" />
        ) : null}
        <AnnotationBar
          scope={scope}
          annotations={annotations}
          error={annotationsError}
          onChanged={() => void refresh()}
        />
      </section>
    </Card>
  );
}

// RUN_SCAN_LIMIT mirrors node-card.tsx's own scan bound -- see
// usePairLastRun's doc comment for why this is a client-side scan rather
// than a server-side filter.
const RUN_SCAN_LIMIT = 20;

/** findLastRunForPair returns the newest run (details assumed newest-first,
 * matching GET /api/v1/runs' own order) whose results include this exact
 * directed pair, plus that one result row. */
export function findLastRunForPair(
  details: RunDetail[],
  source: string,
  destination: string,
): { run: RunDetail; result: RunResult } | undefined {
  for (const d of details) {
    const result = d.results.find((r) => r.sourceNode === source && r.destinationNode === destination);
    if (result) return { run: d, result };
  }
  return undefined;
}

/**
 * usePairLastRun scans the most recent RUN_SCAN_LIMIT runs' full details for
 * one touching this exact directed pair -- GET /api/v1/runs (RunQuery) has no
 * source/destination filter, and a run's per-pair results only come back
 * from GET /api/v1/runs/{id}, so this is client-side over the first page,
 * same limitation node-card.tsx's Diagnostics tab has, noted the same way.
 */
function usePairLastRun(source: string, destination: string) {
  const runsQuery = useQuery({ queryKey: ["runs", "recent-scan"], queryFn: () => getRuns({ limit: RUN_SCAN_LIMIT }) });
  const ids = useMemo(() => runsQuery.data?.runs.map((r) => r.id) ?? [], [runsQuery.data]);
  const detailsQuery = useQuery({
    queryKey: ["runs", "recent-scan", "details", ids.join(",")],
    queryFn: () => Promise.all(ids.map((id) => getRun(id))),
    enabled: ids.length > 0,
  });
  const last = useMemo(
    () => (detailsQuery.data ? findLastRunForPair(detailsQuery.data, source, destination) : undefined),
    [detailsQuery.data, source, destination],
  );
  return {
    last,
    isLoading: runsQuery.isLoading || (ids.length > 0 && detailsQuery.isLoading),
    error: runsQuery.error ?? detailsQuery.error,
  };
}

function PairDiagnosticsTab({
  source,
  destination,
  canCreate,
}: {
  source: string;
  destination: string;
  canCreate: boolean;
}) {
  const { last, isLoading, error } = usePairLastRun(source, destination);
  const writesDisabled = useWritesDisabled();
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string>();

  async function runCheck() {
    setSubmitError(undefined);
    setSubmitting(true);
    try {
      const res = await createRun({ type: "tcp", plane: "pod", sources: [source], destinations: [destination] });
      goTo(`/diagnostics/runs/${res.id}`);
    } catch (err) {
      setSubmitError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : "Failed to start run");
      setSubmitting(false);
    }
  }

  return (
    <Card asChild className="p-5">
      <section>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <h3 className="text-sm font-semibold">Last run for this pair</h3>
          {/* Permission decides whether this button EXISTS; time decides
              whether it is usable (lib/timemachine.tsx's useWritesDisabled
              documents the split). Starting a probe from a view of the past
              would run it now, against the present fleet — the one thing the
              mode must not let happen by accident. */}
          {canCreate ? (
            <Button size="sm" loading={submitting} disabled={writesDisabled} onClick={() => void runCheck()}>
              Run check
            </Button>
          ) : null}
        </div>

        {submitError ? (
          <p role="alert" className="mt-2 text-sm text-health-bad">
            {submitError}
          </p>
        ) : null}

        {error ? (
          <p role="alert" className="mt-3 text-sm text-health-bad">
            Run history is unavailable.
          </p>
        ) : null}

        {isLoading && !last && !error ? <Skeleton className="mt-3 h-8 w-full" /> : null}

        {!isLoading && !last && !error ? (
          <p className="mt-3 text-xs leading-relaxed text-muted-foreground">
            No matching run in the most recent {RUN_SCAN_LIMIT} runs — GET /api/v1/runs has no source/destination
            filter yet, so an older run for this pair may exist but is not shown here.
          </p>
        ) : null}

        {last ? (
          <dl className="nums mt-3 grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
            <div>
              <dt className="text-xs text-muted-foreground">Run</dt>
              <dd className="mt-0.5">
                <a href={`/diagnostics/runs/${last.run.id}`} className="font-medium text-primary hover:underline">
                  {last.run.id}
                </a>
              </dd>
            </div>
            <div>
              <dt className="text-xs text-muted-foreground">Result</dt>
              <dd className="mt-0.5">{last.result.success ? "ok" : "failed"}</dd>
            </div>
            <div>
              <dt className="text-xs text-muted-foreground">Duration</dt>
              <dd className="mt-0.5">{fmtDuration(last.result.durationNs)}</dd>
            </div>
            <div>
              <dt className="text-xs text-muted-foreground">Recorded</dt>
              <dd className="mt-0.5">{fmtTime(last.result.recordedAt)}</dd>
            </div>
          </dl>
        ) : null}
      </section>
    </Card>
  );
}

type PairTab = "overview" | "diagnostics";

const TABS: { value: PairTab; label: string }[] = [
  { value: "overview", label: "Overview" },
  { value: "diagnostics", label: "Diagnostics" },
];

function NotFound() {
  return (
    <PageShell title="Pair" description="No pair in the URL.">
      <Card role="status" className="px-6 py-10 text-center text-sm text-muted-foreground">
        This link is missing a source and destination.
      </Card>
    </PageShell>
  );
}

export function PairCardPage() {
  const { source, destination } = pairFromPath(window.location.pathname);
  const { can } = useAuth();
  const matrix = useMatrix("tcp");
  const [tab, setTab] = useState<PairTab>("overview");

  if (source === "" || destination === "") return <NotFound />;

  const cells = matrix.data?.cells ?? [];
  const forward = cells.find((c) => c.source === source && c.destination === destination);
  const reverse = cells.find((c) => c.source === destination && c.destination === source);
  const scope = pairScope(source, destination);

  return (
    <PageShell
      title={`${source} → ${destination}`}
      description="Pair connectivity (TCP matrix)"
      actions={
        <>
          <DirectionStat label={`${source} → ${destination}`} cell={forward} />
          <DirectionStat label={`${destination} → ${source}`} cell={reverse} />
        </>
      }
    >
      {matrix.error ? (
        <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
          <p className="text-sm font-medium">Matrix is unavailable</p>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{matrix.error.message}</p>
        </Card>
      ) : null}

      <div className="grid grid-cols-1 gap-5 lg:grid-cols-[minmax(0,1fr)_20rem]">
        <div className="flex flex-col gap-5">
          <Segmented aria-label="Tab" options={TABS} value={tab} onChange={setTab} />
          {tab === "overview" ? (
            <PairOverviewTab source={source} destination={destination} />
          ) : (
            <PairDiagnosticsTab source={source} destination={destination} canCreate={can("runs:create")} />
          )}
        </div>
        <RecentChanges scope={scope} />
      </div>
    </PageShell>
  );
}
