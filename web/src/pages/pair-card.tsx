import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { SearchX } from "lucide-react";
import { AnnotationBar, useAnnotations } from "@/components/annotations";
import { EChart } from "@/components/echart";
import { InvestigateLink, RelatedIncidents } from "@/components/investigate-entry";
import { MaintenanceBar, useMaintenance, useWindowAnchor } from "@/components/maintenance";
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
import { useTopology } from "@/hooks/use-topology";
import { ApiError, createRun, getRun, getRuns, goTo, promqlQueryRange } from "@/lib/api";
import { toSeriesOption, type CuratedChart } from "@/lib/curated-metrics";
import type { InvestigationScope } from "@/lib/investigation-sources";
import { localeTag, stampFull, useLocale, useT, type Locale } from "@/lib/i18n";
import { cardsDict, type CardsKey } from "@/lib/i18n/dict/cards";
/* The badge's TOOLTIP is cellSummary's shared sentence, which has its own
   table — one reading of a cell for every surface that draws one. */
import { matrixCellsDict } from "@/lib/i18n/dict/matrix-cells";
import { cellSummary, cellTier, fmtRatio, isMeasured } from "@/lib/matrix-cells";
import { useTimeContext, useWriteGuard } from "@/lib/timemachine";
import type { Matrix, MatrixCell, RunDetail, RunResult, Topology } from "@/lib/types";
import { escapeLabelValue, runsAtOrBefore } from "@/lib/utils";

const PAIR_PATH_PREFIX = "/pairs/";

/**
 * pairFromPath reads {source, destination} straight off window.location.pathname; the two segments
 * are separated by a literal "/".
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

function fmtDuration(ns?: number): string {
  return ns === undefined ? "—" : `${(ns / 1e6).toFixed(0)}ms`;
}

/** The locale is required: a bare toLocaleString() reorders the date and swaps in AM/PM from
 *  whatever the browser was installed in — "8/10/2026 3:47 AM" on a Russian page. */
function fmtTime(ts: string | undefined, locale: Locale): string {
  if (!ts) return "—";
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? ts : stampFull(d, locale);
}

/** DirectionStat renders one directed leg's severity as a labelled badge. */
function DirectionStat({ label, cell }: { label: string; cell?: MatrixCell }) {
  const t = useT(cardsDict);
  const tc = useT(matrixCellsDict);
  const tier = cellTier(cell);
  const measured = isMeasured(cell);
  /* Both chips describe what this console DID or DID NOT measure, so both
     translate; the ratio between them is a number and does not. `label` is the
     two node names with an arrow and is passed in already built. */
  return (
    <span className="flex items-center gap-1.5 text-xs">
      <span className="text-muted-foreground">{label}</span>
      <Badge variant={TIER_VARIANT[tier]} title={measured ? cellSummary(cell, tc) : undefined} dot>
        {!measured ? t("cell.noData") : cell?.failRatio == null ? t("cell.noFailData") : fmtRatio(cell.failRatio)}
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
  const t = useT(cardsDict);
  const { locale } = useLocale();
  const { theme } = useTheme();
  /*
   * The pair's own scope is the SAME string the RecentChanges rail and the controller's live events
   * use — pairScope's U+2192.
   */
  const scope = pairScope(source, destination);
  const { annotations, error: annotationsError, refresh } = useAnnotations(scope, PAIR_RANGE_SECONDS);
  /*
   * The declared change windows over the same hour and the same scope; a separate hook and a
   * separate bar rather than one merged list.
   */
  /* ONE hour, resolved once, for the chart and for the bar under it — the same
     shared anchor the target card takes (QA scope 2, finding #20). */
  const range = useWindowAnchor(PAIR_RANGE_SECONDS);
  const {
    windows,
    error: maintenanceError,
    refresh: refreshMaintenance,
  } = useMaintenance(scope, PAIR_RANGE_SECONDS, range);
  const chart = useMemo<CuratedChart>(
    () => ({
      id: "pair-rtt",
      title: t("pair.chart.title"),
      unit: "seconds",
      query: pairSeriesQuery(source, destination),
    }),
    [t, source, destination],
  );
  // Engaged, the window ends at `t` rather than now — "state as of t" for a
  // chart means the hour BEFORE t, not the hour before this render.
  const { at } = useTimeContext();
  const { data, isLoading, error } = useQuery({
    queryKey: ["pair-series", source, destination, range.to.toISOString()],
    queryFn: () => {
      const stepSeconds =
        Math.ceil(PAIR_RANGE_SECONDS / PAIR_TARGET_POINTS / PAIR_MIN_STEP_SECONDS) * PAIR_MIN_STEP_SECONDS;
      return promqlQueryRange(chart.query, range.from, range.to, stepSeconds * 1e9);
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
          {t("pair.chart.title")}{" "}
          {/* Interpolated into a translated sentence, so it takes that
              sentence's language — lib/i18n's localeTag. */}
          {at ? t("pair.chart.hourEnding", { at: at.toLocaleString(localeTag(locale)) }) : t("pair.chart.lastHour")}
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
            {at ? t("pair.chart.emptyAt") : t("pair.chart.empty")}
          </p>
        ) : null}
        {option && !empty && !queryError ? (
          <EChart
            option={option}
            annotations={annotations}
            maintenance={windows}
            dark={theme === "dark"}
            className="mt-3 h-64 w-full"
          />
        ) : null}
        <AnnotationBar
          scope={scope}
          annotations={annotations}
          error={annotationsError}
          onChanged={() => void refresh()}
        />
        <MaintenanceBar
          scope={scope}
          windows={windows}
          error={maintenanceError}
          onChanged={() => void refreshMaintenance()}
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
 * usePairLastRun scans the most recent RUN_SCAN_LIMIT runs' full details for one touching this
 * exact directed pair.
 */
function usePairLastRun(source: string, destination: string) {
  const { at } = useTimeContext();
  const runsQuery = useQuery({ queryKey: ["runs", "recent-scan"], queryFn: () => getRuns({ limit: RUN_SCAN_LIMIT }) });
  /* Cut to the viewed instant before fetching details, exactly as the node card's own scan does. */
  const ids = useMemo(
    () => runsAtOrBefore(runsQuery.data?.runs ?? [], at).map((r) => r.id),
    [runsQuery.data, at],
  );
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
  const t = useT(cardsDict);
  const { locale } = useLocale();
  const { last, isLoading, error } = usePairLastRun(source, destination);
  const { at } = useTimeContext();
  const guard = useWriteGuard();
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string>();

  async function runCheck() {
    setSubmitError(undefined);
    setSubmitting(true);
    try {
      const res = await createRun({ type: "tcp", plane: "pod", sources: [source], destinations: [destination] });
      goTo(`/diagnostics/runs/${res.id}`);
    } catch (err) {
      setSubmitError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : t("pair.runFailed"));
      setSubmitting(false);
    }
  }

  return (
    <Card asChild className="p-5">
      <section>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <h3 className="text-sm font-semibold">{t("pair.lastRun")}</h3>
          {/* Permission decides whether this button EXISTS; time decides
              whether it is usable (lib/timemachine.tsx's useWriteGuard
              documents the split, and now carries the REASON with it).
              Starting a probe from a view of the past would run it now,
              against the present fleet — the one thing the mode must not let
              happen by accident. */}
          {canCreate ? (
            <Button size="sm" loading={submitting} {...guard} onClick={() => void runCheck()}>
              {t("pair.runCheck")}
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
            {t("pair.runs.unavailable")}.
          </p>
        ) : null}

        {isLoading && !last && !error ? <Skeleton className="mt-3 h-8 w-full" /> : null}

        {/* And no time filter either, so the cut to `t` is client-side over that
            same page — both bounds, stated together, as ONE key so the
            translation decides where the second clause goes. */}
        {!isLoading && !last && !error ? (
          <p className="mt-3 text-xs leading-relaxed text-muted-foreground">
            {t("pair.runs.scanNote", {
              limit: RUN_SCAN_LIMIT,
              engaged: at ? t("pair.runs.scanNote.engaged") : "",
            })}
          </p>
        ) : null}

        {last ? (
          <dl className="nums mt-3 grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
            <div>
              <dt className="text-xs text-muted-foreground">{t("pair.run")}</dt>
              <dd className="mt-0.5">
                <a href={`/diagnostics/runs/${last.run.id}`} className="font-medium text-primary hover:underline">
                  {last.run.id}
                </a>
              </dd>
            </div>
            <div>
              {/* `success` is a BOOLEAN and this is the word this card puts on
                  it, so it translates — unlike a run's own `status` enum. */}
              <dt className="text-xs text-muted-foreground">{t("pair.result")}</dt>
              <dd className="mt-0.5">{last.result.success ? t("pair.result.ok") : t("pair.result.failed")}</dd>
            </div>
            <div>
              <dt className="text-xs text-muted-foreground">{t("pair.duration")}</dt>
              <dd className="mt-0.5">{fmtDuration(last.result.durationNs)}</dd>
            </div>
            <div>
              <dt className="text-xs text-muted-foreground">{t("pair.recorded")}</dt>
              <dd className="mt-0.5">{fmtTime(last.result.recordedAt, locale)}</dd>
            </div>
          </dl>
        ) : null}
      </section>
    </Card>
  );
}

type PairTab = "overview" | "diagnostics";

const TABS: { value: PairTab; labelKey: CardsKey }[] = [
  { value: "overview", labelKey: "tab.overview" },
  { value: "diagnostics", labelKey: "tab.diagnostics" },
];

function NotFound() {
  const t = useT(cardsDict);
  return (
    <PageShell title={t("pair.title")} description={t("pair.notFound.bare")}>
      <Card role="status" className="px-6 py-10 text-center text-sm text-muted-foreground">
        {t("pair.notFound.body")}
      </Card>
    </PageShell>
  );
}

/**
 * knownNodes is the fleet's own answer to "is there a node by this name": the
 * topology's Kubernetes nodes, the node each registered AGENT runs on (the only
 * inventory off-cluster, and the one the topology map itself draws from), the
 * matrix's node list, and both ends of every matrix CELL.
 *
 * The cells are in there deliberately, and generously: a name Prometheus has a
 * measurement for is a real node whatever the inventory lists, and a false
 * not-found on a node that exists is a far worse failure than a missed one on a
 * node that does not.
 *
 * `null` means NOBODY answered yet — neither query has data — and that is not
 * the same as "no such node". A card that 404s while its inventory is still in
 * flight would flash a not-found at every reader.
 */
export function knownNodes(topo?: Topology, matrix?: Matrix): Set<string> | null {
  if (!topo && !matrix) return null;
  const known = new Set<string>();
  for (const n of topo?.nodes ?? []) known.add(n.name);
  for (const a of topo?.agents ?? []) if (a.nodeName !== "") known.add(a.nodeName);
  for (const n of matrix?.nodes ?? []) known.add(n);
  for (const c of matrix?.cells ?? []) {
    known.add(c.source);
    known.add(c.destination);
  }
  return known;
}

/**
 * unknownPairEndpoints names the halves of the URL the fleet does not report.
 * Empty while the inventory is unknown, so validation only ever fires on
 * evidence — /pairs/node-a/there-is-no-such-node used to render a WORKING card,
 * annotate and maintenance writes included (QA scope 2, finding #7).
 */
export function unknownPairEndpoints(
  known: Set<string> | null,
  source: string,
  destination: string,
): string[] {
  if (known === null || known.size === 0) return [];
  return [source, destination].filter((n) => !known.has(n));
}

/** The targets card's own not-found treatment, for the same kind of fact. */
function UnknownEndpoints({ unknown }: { unknown: string[] }) {
  const t = useT(cardsDict);
  return (
    <PageShell
      title={t("pair.notFound.unknownEndpoints")}
      description={
        unknown.length === 1
          ? t("pair.notFound.oneUnknown", { name: unknown[0] })
          : t("pair.notFound.bothUnknown", { a: unknown[0], b: unknown[1] })
      }
    >
      <Card role="status" className="flex flex-col items-center gap-3 px-8 py-16 text-center">
        <span
          aria-hidden="true"
          className="flex size-12 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
        >
          <SearchX className="size-5" />
        </span>
        <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">{t("pair.notFound.unknownBody")}</p>
        <a href="/matrix" className="text-xs font-medium text-primary hover:underline">
          {t("pair.notFound.back")}
        </a>
      </Card>
    </PageShell>
  );
}

export function PairCardPage() {
  const t = useT(cardsDict);
  const { source, destination } = pairFromPath(window.location.pathname);
  const { can } = useAuth();
  const { at } = useTimeContext();
  /* The topology is read for its INVENTORY, not for the chart: an off-cluster
     fleet answers nodes:null and carries its node names on the agents. */
  const topo = useTopology();
  const matrix = useMatrix("tcp");
  const [tab, setTab] = useState<PairTab>("overview");

  if (source === "" || destination === "") return <NotFound />;

  /* Judged only while LIVE. A historical view's inventory is a RECONSTRUCTION
     and says so itself — the topology fold can be truncated, and the matrix is
     evaluated at an instant Prometheus may have nothing for — so "the fleet has
     no such node" is a claim only the present can support. Engaged, `null` says
     there is no basis, and the card renders as it always did. */
  const known = at === null ? knownNodes(topo.data, matrix.data) : null;
  const unknown = unknownPairEndpoints(known, source, destination);
  if (unknown.length > 0) return <UnknownEndpoints unknown={unknown} />;

  const cells = matrix.data?.cells ?? [];
  const forward = cells.find((c) => c.source === source && c.destination === destination);
  const reverse = cells.find((c) => c.source === destination && c.destination === source);
  const scope = pairScope(source, destination);
  const investigationScope: InvestigationScope = { kind: "pair", a: source, b: destination };

  return (
    <PageShell
      /* The TITLE is the two node names and the arrow — data, in both. */
      title={`${source} → ${destination}`}
      description={t("pair.description")}
      actions={
        <>
          <DirectionStat label={`${source} → ${destination}`} cell={forward} />
          <DirectionStat label={`${destination} → ${source}`} cell={reverse} />
          {/* The entry point into Investigation Mode (plan Decision 11) —
              buildInvestigateURL joins the two node names with the very
              separator pairScope above uses, so the two never drift. */}
          <InvestigateLink scope={investigationScope} />
        </>
      }
    >
      {matrix.error ? (
        <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
          <p className="text-sm font-medium">{t("pair.matrixUnavailable")}</p>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{matrix.error.message}</p>
        </Card>
      ) : null}

      <div className="grid grid-cols-1 gap-5 lg:grid-cols-[minmax(0,1fr)_20rem]">
        <div className="flex flex-col gap-5">
          <Segmented
            aria-label={t("tab.aria")}
            options={TABS.map((tb) => ({ value: tb.value, label: t(tb.labelKey) }))}
            value={tab}
            onChange={setTab}
          />
          {tab === "overview" ? (
            <PairOverviewTab source={source} destination={destination} />
          ) : (
            <PairDiagnosticsTab source={source} destination={destination} canCreate={can("runs:create")} />
          )}
        </div>
        <div className="flex flex-col gap-5">
          <RelatedIncidents scope={investigationScope} />
          <RecentChanges scope={scope} />
        </div>
      </div>
    </PageShell>
  );
}
