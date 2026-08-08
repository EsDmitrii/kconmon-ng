import { useMemo, useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { SearchX } from "lucide-react";
import { AnnotationBar, useAnnotations } from "@/components/annotations";
import { EChart } from "@/components/echart";
import { InvestigateLink, RelatedIncidents } from "@/components/investigate-entry";
import { MaintenanceBar, useMaintenance } from "@/components/maintenance";
import { PageShell } from "@/components/page-shell";
import { RecentChanges } from "@/components/recent-changes";
import { useTheme } from "@/components/theme-provider";
import { Badge, type BadgeProps } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { useAuth } from "@/hooks/use-auth";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import { ApiError, getConfig, getRun, getRuns, getTarget, listChecks, listSchedules, promqlQuery, promqlQueryRange } from "@/lib/api";
import { toSeriesOption, type CuratedChart } from "@/lib/curated-metrics";
import type { InvestigationScope } from "@/lib/investigation-sources";
import { useTimeContext } from "@/lib/timemachine";
import type { CheckDefinition, PromResult, RunDetail, Schedule, Target } from "@/lib/types";
// fmtIntervalNs is imported rather than re-derived so a schedule's cadence
// reads identically on this card and on the Targets page's own Schedules tab —
// the same reason recent-changes.tsx imports pushEvents from pages/live.
import { escapeLabelValue } from "@/lib/utils";
import { fmtIntervalNs } from "@/pages/targets";

const TARGET_PATH_PREFIX = "/targets/";

/**
 * targetIdFromPath mirrors run-detail.tsx's runIdFromPath and node-card.tsx's
 * nodeNameFromPath: the object id is read straight off
 * window.location.pathname rather than through TanStack Router's own param
 * matching. That keeps this page testable with a plain render and — the point
 * of a permalink — correct on a cold load of a bookmarked /targets/{id} with
 * no in-memory state to inherit from the list page.
 *
 * "/targets" and "/targets/" are the LIST route and answer "", so the card
 * never fires a GET /api/v1/targets/ with an empty id. A malformed
 * percent-escape falls back to the raw remainder rather than throwing: the
 * server will answer 404 for it, which is the honest outcome for a hand-typed
 * URL, and a page that crashes is not.
 */
export function targetIdFromPath(pathname: string): string {
  if (!pathname.startsWith(TARGET_PATH_PREFIX)) return "";
  const rest = pathname.slice(TARGET_PATH_PREFIX.length);
  if (rest === "") return "";
  try {
    return decodeURIComponent(rest);
  } catch {
    return rest;
  }
}

/**
 * targetDurationQuery is the History tab's series: probe latency p95 per SOURCE
 * node for this one target. Metric name and label set are the agent's own
 * external family (internal/metrics/prometheus.go: `_external_duration_seconds`
 * over {source_node, source_zone, target, target_kind}) — `target` carries the
 * target ROW's name, which is why the card queries by name and not by id.
 *
 * Duration is the one external metric every check type populates; rtt and
 * packet-loss exist only for icmp and http_status_code only for http, so a
 * chart built on those would be empty for a plain tcp target and read as an
 * outage rather than as "not measured".
 */
export function targetDurationQuery(name: string): string {
  const sel = `target="${escapeLabelValue(name)}"`;
  return `histogram_quantile(0.95, sum by (source_node, le) (rate(kconmon_ng_external_duration_seconds_bucket{${sel}}[5m])))`;
}

/**
 * targetHealthQuery is the header's health%: the share of external probe
 * results for this target that succeeded over the last 5 minutes.
 * `_external_results_total` counts only probes that REACHED the network — an
 * allowlist denial increments `_external_denied_total` instead — so a
 * misconfigured CIDR list shows up as "no data", never as a target that looks
 * down.
 */
export function targetHealthQuery(name: string): string {
  const sel = `target="${escapeLabelValue(name)}"`;
  return (
    `sum(rate(kconmon_ng_external_results_total{${sel},result="success"}[5m])) / ` +
    `sum(rate(kconmon_ng_external_results_total{${sel}}[5m]))`
  );
}

type Tier = "ok" | "warn" | "bad" | "unknown";

const TIER_VARIANT: Record<Tier, NonNullable<BadgeProps["variant"]>> = {
  ok: "ok",
  warn: "warn",
  bad: "bad",
  unknown: "unknown",
};
const TIER_LABEL: Record<Tier, string> = {
  ok: "Healthy",
  warn: "Degraded",
  bad: "Failing",
  unknown: "No data",
};

// Prometheus instant-vector entry (Prometheus's own envelope shape, narrowed
// from PromResult.data.result's `unknown[]`).
interface VectorEntry {
  metric: Record<string, string>;
  value: [number, string];
}

/**
 * healthFromVector turns targetHealthQuery's instant vector into the header's
 * percentage and tier. The three no-answer shapes — an empty vector (nothing
 * has probed this target), Prometheus's error envelope (which
 * promqlQuery RESOLVES rather than throws, see lib/api.ts's `handle`), and a
 * non-numeric sample — all read "no data", never 0%: a target nobody probes is
 * not a target that fails every probe.
 *
 * Tier thresholds mirror node-card.tsx's fail-ratio bands exactly (1% warn,
 * 10% bad), so a badge means the same thing on every object card.
 */
export function healthFromVector(res: PromResult | undefined): { percent: number | null; tier: Tier } {
  const none = { percent: null, tier: "unknown" as const };
  if (!res || res.status !== "success" || res.data?.resultType !== "vector") return none;
  const entries = (res.data.result ?? []) as VectorEntry[];
  const raw = entries[0]?.value?.[1];
  if (raw === undefined) return none;
  const ratio = Number(raw);
  if (!Number.isFinite(ratio)) return none;
  const percent = Math.min(100, Math.max(0, 100 * ratio));
  const failRatio = 1 - ratio;
  const tier: Tier = failRatio >= 0.1 ? "bad" : failRatio >= 0.01 ? "warn" : "ok";
  return { percent, tier };
}

/**
 * runsTouchingTarget filters already-fetched run details down to the ones whose
 * SPEC names this target.
 *
 * The spec is the snapshot checks.Spec marshals into check_runs.spec, so its
 * keys are Go's exported field names ("TypedDestinations") while a Destination
 * carries its own lowercase json tags (kind/name/address). Only kind "target"
 * counts: an ad-hoc destination is an operator-typed address that never went
 * through the targets table, and matching one because its label happens to
 * equal this target's name would attribute someone else's probe to this row.
 *
 * Matching is by NAME rather than by target id because the id is not in the
 * snapshot at all — the spec resolves a target to {name, address} at start
 * time. A target renamed after a run therefore stops matching that run; the
 * snapshot is a record of what was probed, and rewriting history to follow a
 * rename would be the bigger lie.
 */
export function runsTouchingTarget(details: RunDetail[], targetName: string): RunDetail[] {
  return details.filter((d) => specNamesTarget(d.spec, targetName));
}

function specNamesTarget(spec: unknown, targetName: string): boolean {
  if (typeof spec !== "object" || spec === null) return false;
  const typed = (spec as { TypedDestinations?: unknown }).TypedDestinations;
  if (!Array.isArray(typed)) return false;
  return typed.some((d) => {
    if (typeof d !== "object" || d === null) return false;
    const dest = d as { kind?: unknown; name?: unknown };
    return dest.kind === "target" && dest.name === targetName;
  });
}

function fmtTime(timestamp?: string | null): string {
  if (!timestamp) return "—";
  const d = new Date(timestamp);
  return Number.isNaN(d.getTime()) ? timestamp : d.toLocaleString();
}

function cadence(s: Schedule): string {
  switch (s.kind) {
    case "interval":
      return `every ${fmtIntervalNs(s.intervalNs)}`;
    case "once":
      return `once at ${fmtTime(s.runAt)}`;
    case "continuous":
      return "continuous";
    default:
      return s.kind;
  }
}

function queryErrorMessage(error: unknown, fallback: string): string {
  return error instanceof ApiError ? (error.problem.detail ?? error.problem.title) : fallback;
}

/** PermissionCard is PAGES.md:126-129's pattern, the same one targets.tsx uses:
 *  name the permission, say what the reader CAN still do, and never render a
 *  disabled control in place of one they simply do not have. */
function PermissionCard({ permission, children }: { permission: string; children: ReactNode }) {
  return (
    <Card role="status" className="p-6">
      <p className="text-sm font-medium">Requires the {permission} permission</p>
      <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{children}</p>
    </Card>
  );
}

function ListSkeleton() {
  return (
    <div role="status" aria-live="polite" className="mt-4 flex flex-col gap-2">
      <span className="sr-only">Loading…</span>
      {Array.from({ length: 3 }, (_, i) => (
        <Skeleton key={i} className="h-10 w-full" />
      ))}
    </div>
  );
}

/* ── Checks & Schedules tab ─────────────────────────────────────────────── */

/**
 * useTargetChecks reads the definitions pointing at this target (GET
 * /api/v1/checks?targetId=, a real server-side filter — see
 * docs/console-api.yaml) and then each one's schedules (GET
 * /api/v1/schedules?definitionId=, likewise real). Both live behind
 * checks:read; there is no schedules:read at all
 * (internal/console/httpapi/middleware_auth.go), so one permission gates both
 * halves of this tab.
 */
function useTargetChecks(targetId: string, enabled: boolean) {
  const definitionsQuery = useQuery({
    queryKey: ["checks", "target", targetId],
    queryFn: () => listChecks({ targetId }),
    enabled,
  });
  const definitions = useMemo(() => definitionsQuery.data?.definitions ?? [], [definitionsQuery.data]);
  const ids = useMemo(() => definitions.map((d) => d.id), [definitions]);
  const schedulesQuery = useQuery({
    queryKey: ["schedules", "target", targetId, ids.join(",")],
    queryFn: async () => {
      const pages = await Promise.all(ids.map((id) => listSchedules({ definitionId: id })));
      const byDefinition: Record<string, Schedule[]> = {};
      pages.forEach((page, i) => {
        byDefinition[ids[i]] = page.schedules;
      });
      return byDefinition;
    },
    enabled: enabled && ids.length > 0,
  });
  return {
    definitions,
    schedules: schedulesQuery.data ?? {},
    isLoading: definitionsQuery.isLoading || (ids.length > 0 && schedulesQuery.isLoading),
    error: definitionsQuery.error ?? schedulesQuery.error,
  };
}

function DefinitionRow({ definition, schedules }: { definition: CheckDefinition; schedules: Schedule[] }) {
  return (
    <li className="flex flex-col gap-2 py-3 text-sm">
      <div className="flex flex-wrap items-center gap-3">
        <span className="font-medium">{definition.name}</span>
        <Badge variant="neutral">{definition.checkType}</Badge>
        <span className="text-xs text-muted-foreground">{definition.sourceSelection}</span>
        <Badge variant={definition.enabled ? "ok" : "unknown"} dot>
          {definition.enabled ? "enabled" : "disabled"}
        </Badge>
      </div>
      {schedules.length === 0 ? (
        <p className="text-xs text-muted-foreground">
          No schedule — this definition only runs when someone starts it by hand.
        </p>
      ) : (
        <ul className="flex flex-col gap-1">
          {schedules.map((s) => (
            <li key={s.id} className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
              <span className="nums">{cadence(s)}</span>
              <Badge variant={s.enabled ? "ok" : "unknown"} dot>
                {s.enabled ? "enabled" : "disabled"}
              </Badge>
              <span className="nums">next {fmtTime(s.nextFireAt)}</span>
              <span className="nums">last {fmtTime(s.lastFiredAt)}</span>
            </li>
          ))}
        </ul>
      )}
    </li>
  );
}

function ChecksTab({ targetId, canRead }: { targetId: string; canRead: boolean }) {
  const { definitions, schedules, isLoading, error } = useTargetChecks(targetId, canRead);

  if (!canRead) {
    return (
      <PermissionCard permission="checks:read">
        The header above is everything targets:read alone can show. The definitions probing this target, and their
        cadence, are read with checks:read — schedules ride on the same permission, since a cadence tells you nothing
        the definition it belongs to does not.
      </PermissionCard>
    );
  }

  return (
    <Card asChild className="p-6">
      <section>
        <h3 className="text-sm font-semibold">Definitions probing this target</h3>
        {error ? (
          <p role="alert" className="mt-3 text-sm text-health-bad">
            {queryErrorMessage(error, "Check definitions are unavailable")}
          </p>
        ) : null}
        {isLoading && !error ? <ListSkeleton /> : null}
        {!isLoading && !error && definitions.length === 0 ? (
          <p className="px-1 py-10 text-center text-xs text-muted-foreground">
            No check definition points at this target yet. Until one does, nothing probes it on a schedule.
          </p>
        ) : null}
        {definitions.length > 0 ? (
          <ul aria-label="Definitions" className="mt-4 divide-y divide-border">
            {definitions.map((d) => (
              <DefinitionRow key={d.id} definition={d} schedules={schedules[d.id] ?? []} />
            ))}
          </ul>
        ) : null}
      </section>
    </Card>
  );
}

/* ── History tab ────────────────────────────────────────────────────────── */

const HISTORY_RANGE_SECONDS = 60 * 60;
const HISTORY_TARGET_POINTS = 120;
const HISTORY_MIN_STEP_SECONDS = 15;

/**
 * HistoryTab renders the target's kconmon_ng_external_* series through the
 * EXISTING guarded PromQL proxy (POST /api/v1/promql/query_range), the same
 * path pair-card.tsx already uses — no new endpoint, and the proxy's own
 * allowlist/limits still apply.
 *
 * Two honest degradations instead of an empty-axis chart:
 *
 *  - Prometheus not configured on this replica (GET /api/v1/config's
 *    `prometheus.configured`, the same signal the proxy's own 503 gate reads):
 *    say so and make NO request, since every one of them would fail.
 *  - An empty series. GET /api/v1/config does NOT expose an external-checks
 *    capability today (httpapi's handleConfig answers auth/anonymousBanner/
 *    controller/prometheus/database and nothing else), so "the external feature
 *    is off fleet-wide" and "nothing has probed this target yet" are the SAME
 *    observation from here — the note is worded to cover both rather than
 *    asserting a cause the console cannot actually distinguish.
 */
function HistoryTab({ targetName, promConfigured, promResolved }: { targetName: string; promConfigured: boolean; promResolved: boolean }) {
  const { theme } = useTheme();
  /* The scope is the target's NAME, not its id — the same string the events
     rail and every other scope in this console is keyed by, and the one an
     operator can actually recognise in a listing. It is also stable across the
     id: a target deleted and recreated under the same name keeps its notes,
     which is the honest outcome for a mark about a place in the network.
     The annotations do not depend on Prometheus, so unlike the chart above they
     are fetched even when this replica has no Prometheus at all. */
  const { annotations, error: annotationsError, refresh } = useAnnotations(targetName, HISTORY_RANGE_SECONDS);
  /* The declared change windows over the same hour and the same scope (M6 Task
     9), and for the same reason the annotations are fetched here: a provider's
     maintenance on this target does not depend on Prometheus, so the bands are
     read even where the chart above cannot be. */
  const {
    windows,
    error: maintenanceError,
    refresh: refreshMaintenance,
  } = useMaintenance(targetName, HISTORY_RANGE_SECONDS);
  const chart = useMemo<CuratedChart>(
    () => ({
      id: "target-duration",
      title: "External probe duration p95 by source node",
      unit: "seconds",
      query: targetDurationQuery(targetName),
    }),
    [targetName],
  );
  // Engaged, the window ends at `t`: the hour before the instant being read,
  // not the hour before this render.
  const { at } = useTimeContext();
  const { data, isLoading, error } = useQuery({
    queryKey: at ? ["target-series", targetName, "at", at.toISOString()] : ["target-series", targetName],
    queryFn: () => {
      const end = at ?? new Date();
      const start = new Date(end.getTime() - HISTORY_RANGE_SECONDS * 1000);
      const stepSeconds =
        Math.ceil(HISTORY_RANGE_SECONDS / HISTORY_TARGET_POINTS / HISTORY_MIN_STEP_SECONDS) * HISTORY_MIN_STEP_SECONDS;
      return promqlQueryRange(chart.query, start, end, stepSeconds * 1e9);
    },
    enabled: promResolved && promConfigured,
  });
  const option = useMemo(() => (data ? toSeriesOption(chart, data, theme === "dark") : undefined), [chart, data, theme]);
  // promqlQueryRange RESOLVES (rather than throws) for Prometheus's own error
  // envelope -- lib/api.ts's `handle` -- so a query-level failure surfaces via
  // data.status, not react-query's `error`.
  const queryError = data?.status === "error" ? (data.error ?? "query failed") : undefined;
  const empty =
    data?.status === "success" && (data.data?.resultType !== "matrix" || (data.data?.result ?? []).length === 0);

  return (
    <Card asChild className="p-5">
      <section>
        <h3 className="text-sm font-semibold">
          {chart.title} {at ? `(hour ending ${at.toLocaleString()})` : "(last hour)"}
        </h3>

        {promResolved && !promConfigured ? (
          <p role="status" className="mt-3 text-xs leading-relaxed text-muted-foreground">
            Prometheus is not configured for this console — set console.prometheus.address to read probe history. The
            other tabs do not depend on it.
          </p>
        ) : null}

        {error ? (
          <p role="alert" className="mt-3 text-sm text-health-bad">
            {queryErrorMessage(error, "Probe history is unavailable")}
          </p>
        ) : null}
        {queryError ? (
          <p role="alert" className="mt-3 text-sm text-health-bad">
            {queryError}
          </p>
        ) : null}

        {promConfigured && isLoading && !data ? <Skeleton className="mt-3 h-64 w-full" /> : null}

        {empty ? (
          <p className="mt-3 text-xs leading-relaxed text-muted-foreground">
            No external probe series for this target in the last hour. Either nothing is probing it — external checks
            are off fleet-wide (checkers.external.enabled), or no enabled definition and schedule point here yet — or
            probing started too recently for Prometheus to have scraped it.
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
          scope={targetName}
          annotations={annotations}
          error={annotationsError}
          onChanged={() => void refresh()}
        />
        <MaintenanceBar
          scope={targetName}
          windows={windows}
          error={maintenanceError}
          onChanged={() => void refreshMaintenance()}
        />
      </section>
    </Card>
  );
}

/* ── Runs tab ───────────────────────────────────────────────────────────── */

// RUN_SCAN_LIMIT bounds the client-side scan below, mirroring node-card.tsx's
// and pair-card.tsx's own bound -- kept small because each candidate costs one
// extra GET /api/v1/runs/{id}.
const RUN_SCAN_LIMIT = 20;

const STATUS_VARIANT: Record<string, NonNullable<BadgeProps["variant"]>> = {
  pending: "neutral",
  running: "neutral",
  succeeded: "ok",
  failed: "bad",
  partial: "warn",
  cancelled: "neutral",
};

/**
 * useTargetRuns is the Runs tab's data source. GET /api/v1/runs takes only
 * ?type=&status=&limit=&cursor= (docs/console-api.yaml's listRuns — verified,
 * not assumed), and a run's destination lives in its SPEC, which only GET
 * /api/v1/runs/{id} returns. So "runs against this target" is a client-side
 * filter over the most recent RUN_SCAN_LIMIT runs' full details, exactly as
 * the Node and Pair cards already do for their own object — and the tab says
 * so rather than implying complete history.
 */
function useTargetRuns(targetName: string, enabled: boolean) {
  const runsQuery = useQuery({
    queryKey: ["runs", "recent-scan"],
    queryFn: () => getRuns({ limit: RUN_SCAN_LIMIT }),
    enabled,
  });
  const ids = useMemo(() => runsQuery.data?.runs.map((r) => r.id) ?? [], [runsQuery.data]);
  const detailsQuery = useQuery({
    queryKey: ["runs", "recent-scan", "details", ids.join(",")],
    queryFn: () => Promise.all(ids.map((id) => getRun(id))),
    enabled: enabled && ids.length > 0,
  });
  const runs = useMemo(
    () => (detailsQuery.data ? runsTouchingTarget(detailsQuery.data, targetName) : []),
    [detailsQuery.data, targetName],
  );
  return {
    runs,
    isLoading: runsQuery.isLoading || (ids.length > 0 && detailsQuery.isLoading),
    error: runsQuery.error ?? detailsQuery.error,
  };
}

function RunsTab({ targetName }: { targetName: string }) {
  const { runs, isLoading, error } = useTargetRuns(targetName, true);

  return (
    <Card asChild className="overflow-hidden p-0">
      <section>
        <div className="border-b border-border px-4 py-3">
          <h3 className="text-sm font-semibold">Runs against this target</h3>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
            No server-side target filter on GET /api/v1/runs yet — this scans the most recent {RUN_SCAN_LIMIT} runs'
            specs client-side. An older run against this target may exist but will not show up here.
          </p>
        </div>

        {error ? (
          <p role="alert" className="px-4 py-4 text-sm text-health-bad">
            {queryErrorMessage(error, "Run history is unavailable")}
          </p>
        ) : null}

        {isLoading && runs.length === 0 && !error ? (
          <div role="status" aria-live="polite" className="flex flex-col gap-2 p-4">
            <span className="sr-only">Scanning recent runs…</span>
            {Array.from({ length: 3 }, (_, i) => (
              <Skeleton key={i} className="h-8 w-full" />
            ))}
          </div>
        ) : null}

        {!isLoading && !error && runs.length === 0 ? (
          <p className="px-4 py-10 text-center text-xs text-muted-foreground">
            No run against this target in the most recent {RUN_SCAN_LIMIT} runs.
          </p>
        ) : null}

        {runs.length > 0 ? (
          <ul className="divide-y divide-border">
            {runs.map((r) => (
              <li key={r.id} className="flex flex-wrap items-center gap-3 px-4 py-3 text-sm">
                <a href={`/diagnostics/runs/${r.id}`} className="font-medium text-primary hover:underline">
                  {r.id}
                </a>
                <Badge variant={STATUS_VARIANT[r.status] ?? "unknown"} dot>
                  {r.status}
                </Badge>
                <span className="text-xs uppercase tracking-wide text-muted-foreground">{r.type}</span>
                <span className="nums ml-auto text-xs text-muted-foreground">
                  {r.pairOk}/{r.pairTotal} ok
                </span>
                <span className="text-xs text-muted-foreground">{fmtTime(r.createdAt)}</span>
              </li>
            ))}
          </ul>
        ) : null}
      </section>
    </Card>
  );
}

/* ── The card ───────────────────────────────────────────────────────────── */

type TargetTab = "checks" | "history" | "runs";

/** Three real tabs and no placeholders (M4 Plan Decision 17). Alerts,
 *  Incidents, Maintenance and Audit-per-target are ABSENT rather than empty:
 *  their tables land in M5-M7, and an absent tab is honest where an empty one
 *  promises something that does not exist. */
const TABS: { value: TargetTab; label: string }[] = [
  { value: "checks", label: "Checks & Schedules" },
  { value: "history", label: "History" },
  { value: "runs", label: "Runs" },
];

function NotFound({ id, known }: { id: string; known: boolean }) {
  return (
    <PageShell title="Target not found" description={id ? `No target matches “${id}”.` : "No target id in the URL."}>
      <Card role="status" className="flex flex-col items-center gap-3 px-8 py-16 text-center">
        <span
          aria-hidden="true"
          className="flex size-12 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
        >
          <SearchX className="size-5" />
        </span>
        <p className="text-sm font-medium">{known ? "This target does not exist" : "This link is missing a target id."}</p>
        <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">
          {known
            ? "An unknown id and a malformed one look the same from here — both answer 404. It may have been deleted, or the id may be a typo."
            : "A target card needs an id: /targets/{id}."}
        </p>
        <a href="/targets" className="text-xs font-medium text-primary hover:underline">
          Back to Targets
        </a>
      </Card>
    </PageShell>
  );
}

/**
 * TargetCardPage is PAGES.md §6.4's Target card: header (name, kind, address,
 * health), a three-tab strip, and the shared RecentChanges rail.
 *
 * It is READ-ONLY by design. targets:write, checks:write and schedules:write
 * change nothing here — every mutation lives on the Targets page's forms, so
 * "read without write" is not a degraded state for this page, it is the only
 * state it has.
 *
 * Order of the guards below is deliberate and matches targets.tsx's: nothing
 * is fetched until both auth and the database capability have RESOLVED, so a
 * cold load neither fires a request that is certain to 403/503 nor flashes a
 * permission card at a subject that turns out to hold the permission.
 */
export function TargetCardPage() {
  const id = targetIdFromPath(window.location.pathname);
  const { at } = useTimeContext();
  const { me, can } = useAuth();
  const { available: dbAvailable, resolved: dbResolved } = useDatabaseAvailable();
  // Same ["config"] cache entry useDatabaseAvailable and AppShell already read
  // (staleTime: Infinity) — one shared fetch, read here for a second field.
  const { data: config } = useQuery({ queryKey: ["config"], queryFn: getConfig, staleTime: Infinity });
  const [tab, setTab] = useState<TargetTab>("checks");

  const authResolved = me !== undefined;
  const canRead = can("targets:read");
  const targetEnabled = id !== "" && authResolved && canRead && dbResolved && dbAvailable;

  const targetQuery = useQuery({
    queryKey: ["target", id],
    queryFn: () => getTarget(id),
    enabled: targetEnabled,
    retry: false,
  });
  const target: Target | undefined = targetQuery.data;
  const notFound = targetQuery.error instanceof ApiError && targetQuery.error.problem.status === 404;

  const promResolved = config !== undefined;
  const promConfigured = config?.prometheus.configured ?? false;
  // The header's health figure is an INSTANT query, so the Time Machine moves
  // the evaluation instant itself (the proxy's own `time` parameter) rather
  // than a window — this is the "state as of t" the header claims to show.
  const healthQuery = useQuery({
    queryKey: at
      ? ["target-health", target?.name ?? "", "at", at.toISOString()]
      : ["target-health", target?.name ?? ""],
    queryFn: () => promqlQuery(targetHealthQuery(target?.name ?? ""), at ?? undefined),
    enabled: target !== undefined && promConfigured,
  });
  const health = healthFromVector(healthQuery.data);

  if (id === "") return <NotFound id={id} known={false} />;

  if (!authResolved || !dbResolved) {
    return (
      <PageShell title="Target" description="Loading…">
        <Card role="status" aria-live="polite" className="p-6">
          <span className="sr-only">Loading target…</span>
          <Skeleton className="h-4 w-48" />
          <div className="mt-4 flex flex-col gap-2">
            {Array.from({ length: 4 }, (_, i) => (
              <Skeleton key={i} className="h-8 w-full" />
            ))}
          </div>
        </Card>
      </PageShell>
    );
  }

  if (!canRead) {
    return (
      <PageShell title="Target" description={id}>
        <PermissionCard permission="targets:read">
          External targets, their check definitions and their schedules are configuration, not telemetry: reading them
          is granted to the operator and admin roles, and deliberately not to viewer — which is the role an anonymous
          session gets. Sign in with an account that holds it.
        </PermissionCard>
      </PageShell>
    );
  }

  // Database disabled: targets are configuration and get no in-memory fallback
  // (M4 Plan Decision 13), so every route this card reads answers 503. One
  // line, and not a single request — including the RecentChanges rail, which
  // cannot even be mounted here: its scope is the target's NAME, and the name
  // lives in the row this replica cannot read.
  if (!dbAvailable) {
    return (
      <PageShell title="Target" description={id}>
        <Card role="status" className="p-6">
          <p className="text-sm">Targets, definitions and schedules are stored in the database — set console.database.mode</p>
        </Card>
      </PageShell>
    );
  }

  if (notFound) return <NotFound id={id} known />;

  if (!target) {
    if (targetQuery.error) {
      return (
        <PageShell title="Target" description={id}>
          <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
            <p className="text-sm font-medium">This target is unavailable</p>
            <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
              {queryErrorMessage(targetQuery.error, "Failed to load this target.")}
            </p>
          </Card>
        </PageShell>
      );
    }
    return (
      <PageShell title="Target" description={id}>
        <Card role="status" aria-live="polite" className="p-6">
          <span className="sr-only">Loading target…</span>
          <Skeleton className="h-4 w-48" />
          <div className="mt-4 flex flex-col gap-2">
            {Array.from({ length: 4 }, (_, i) => (
              <Skeleton key={i} className="h-8 w-full" />
            ))}
          </div>
        </Card>
      </PageShell>
    );
  }

  const investigationScope: InvestigationScope = { kind: "target", a: target.name, b: "" };

  return (
    <PageShell
      title={target.name}
      description={at ? `External probe target — state as of ${at.toLocaleString()}` : "External probe target"}
      actions={
        <>
          <Badge variant="neutral">{target.kind}</Badge>
          <span className="nums max-w-[18rem] truncate text-sm text-muted-foreground" title={target.address}>
            {target.address}
          </span>
          <span className="nums text-sm text-muted-foreground">
            {health.percent === null ? "—" : `${health.percent.toFixed(1)}%`} healthy
          </span>
          <Badge variant={TIER_VARIANT[health.tier]} dot>
            {TIER_LABEL[health.tier]}
          </Badge>
          {/* The entry point into Investigation Mode (plan Decision 11). The
              scope carries the target's NAME, not its id: that is the label
              value the external metric family is selected by, and the string
              an incident's scope is matched against. */}
          <InvestigateLink scope={investigationScope} />
        </>
      }
    >
      <div className="grid grid-cols-1 gap-5 lg:grid-cols-[minmax(0,1fr)_20rem]">
        <div className="flex flex-col gap-5">
          <Segmented aria-label="Tab" options={TABS} value={tab} onChange={setTab} />
          {tab === "checks" ? <ChecksTab targetId={target.id} canRead={can("checks:read")} /> : null}
          {tab === "history" ? (
            <HistoryTab targetName={target.name} promConfigured={promConfigured} promResolved={promResolved} />
          ) : null}
          {tab === "runs" ? <RunsTab targetName={target.name} /> : null}
        </div>
        <div className="flex flex-col gap-2">
          <RelatedIncidents scope={investigationScope} />
          <RecentChanges scope={target.name} />
          {/* Honest about what this rail can and cannot show today: every event
              a probe of this target produces is scoped per SOURCE node
              ("node-a→edge-gw", internal/console/events/live_event.go's
              pairScope), and GET /api/v1/events matches `scope` by exact
              equality. So this rail carries events scoped to the target itself
              — which is where target-level events will land — and not the
              per-source probe results. Saying so beats a rail that is silently
              empty and looks broken. */}
          <p className="px-1 text-[11px] leading-relaxed text-muted-foreground">
            Probe results are recorded per source node (e.g. node-a→{target.name}) and appear on those pair cards; this
            rail shows changes scoped to the target itself.
          </p>
        </div>
      </div>
    </PageShell>
  );
}
