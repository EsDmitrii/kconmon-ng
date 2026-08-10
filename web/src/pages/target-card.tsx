import { useMemo, useState, type ReactNode } from "react";
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
import { Card } from "@/components/ui/card";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { useAuth } from "@/hooks/use-auth";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import { ApiError, getConfig, getRun, getRuns, getTarget, listChecks, listSchedules, promqlQuery, promqlQueryRange } from "@/lib/api";
import { toSeriesOption, type CuratedChart } from "@/lib/curated-metrics";
import { localeTag, stampFull, useLocale, useT, type Locale, type Translate } from "@/lib/i18n";
import { cardsDict, type CardsKey } from "@/lib/i18n/dict/cards";
import type { InvestigationScope } from "@/lib/investigation-sources";
import { useTimeContext } from "@/lib/timemachine";
import type { CheckDefinition, PromResult, RunDetail, Schedule, Target } from "@/lib/types";
// fmtIntervalNs is imported rather than re-derived so a schedule's cadence
// reads identically on this card and on the Targets page's own Schedules tab —
// the same reason recent-changes.tsx imports pushEvents from pages/live.
import { escapeLabelValue } from "@/lib/utils";
import { fmtCadence } from "@/pages/targets";

const TARGET_PATH_PREFIX = "/targets/";

/** targetIdFromPath mirrors run-detail.tsx's runIdFromPath and node-card.tsx's nodeNameFromPath. */
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
 * targetHealthQuery is the header's health%; `_external_results_total` counts only probes that
 * REACHED the network.
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
/* The same four words the node and pair cards use — lib/i18n/dict/cards.ts
   holds the table so a badge means one thing everywhere. */
const TIER_KEYS: Record<Tier, CardsKey> = {
  ok: "tier.ok",
  warn: "tier.warn",
  bad: "tier.bad",
  unknown: "tier.unknown",
};

// Prometheus instant-vector entry (Prometheus's own envelope shape, narrowed
// from PromResult.data.result's `unknown[]`).
interface VectorEntry {
  metric: Record<string, string>;
  value: [number, string];
}

/**
 * healthFromVector turns targetHealthQuery's instant vector into the header's percentage and tier;
 * the three no-answer shapes — an empty vector (nothing has probed this target).
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
 * runsTouchingTarget filters already-fetched run details down to the ones whose SPEC names this
 * target; only kind "target" counts: an ad-hoc destination is an operator-typed address that never
 * went through the targets table.
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

/** The locale is required: a bare toLocaleString() reorders the date and swaps in AM/PM from
 *  whatever the browser was installed in — "8/10/2026 3:47 AM" on a Russian page. */
function fmtTime(timestamp: string | null | undefined, locale: Locale): string {
  if (!timestamp) return "—";
  const d = new Date(timestamp);
  return Number.isNaN(d.getTime()) ? timestamp : stampFull(d, locale);
}

/* The same cadence sentence pages/targets.tsx builds, with the same words. */
function cadence(s: Schedule, locale: Locale, t: Translate<CardsKey>): string {
  switch (s.kind) {
    case "interval":
      return fmtCadence(s.intervalNs, locale, {
        interval: (interval) => t("schedule.cadence.interval", { interval }),
        every: {
          second: t("schedule.cadence.every.second"),
          minute: t("schedule.cadence.every.minute"),
          hour: t("schedule.cadence.every.hour"),
        },
        unit: {
          second: t("schedule.cadence.unit.second"),
          minute: t("schedule.cadence.unit.minute"),
          hour: t("schedule.cadence.unit.hour"),
        },
      });
    case "once":
      return t("schedule.cadence.once", { at: fmtTime(s.runAt, locale) });
    case "continuous":
      return t("schedule.cadence.continuous");
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
  const t = useT(cardsDict);
  return (
    <Card role="status" className="p-6">
      <p className="text-sm font-medium">{t("permission.requires", { permission })}</p>
      <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{children}</p>
    </Card>
  );
}

function ListSkeleton() {
  const t = useT(cardsDict);
  return (
    <div role="status" aria-live="polite" className="mt-4 flex flex-col gap-2">
      <span className="sr-only">{t("loading")}</span>
      {Array.from({ length: 3 }, (_, i) => (
        <Skeleton key={i} className="h-10 w-full" />
      ))}
    </div>
  );
}

/* ── Checks & Schedules tab ─────────────────────────────────────────────── */

/**
 * useTargetChecks reads the definitions pointing at this target (GET /api/v1/checks?targetId=, a
 * real server-side filter — see docs/console-api.yaml) and then each one's schedules (GET
 * /api/v1/schedules?definitionId=, likewise real).
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
  const t = useT(cardsDict);
  const { locale } = useLocale();
  return (
    <li className="flex flex-col gap-2 py-3 text-sm">
      <div className="flex flex-wrap items-center gap-3">
        <span className="font-medium">{definition.name}</span>
        {/* checkType and sourceSelection are stored values, exactly as on
            pages/targets.tsx; only the pill describing `enabled` translates. */}
        <Badge variant="neutral">{definition.checkType}</Badge>
        <span className="text-xs text-muted-foreground">{definition.sourceSelection}</span>
        <Badge variant={definition.enabled ? "ok" : "unknown"} dot>
          {definition.enabled ? t("schedule.enabled") : t("schedule.disabled")}
        </Badge>
      </div>
      {schedules.length === 0 ? (
        <p className="text-xs text-muted-foreground">{t("target.checks.noSchedule")}</p>
      ) : (
        <ul className="flex flex-col gap-1">
          {schedules.map((s) => {
            // Same treatment as the Schedules tab's own rows: the cadence advances whether the fire
            // produced a run or not.
            // An enabled schedule under a DISABLED definition fires nothing at all — that is a
            // paused check, and it is its own state rather than a shade of "enabled" (finding 25).
            const paused = s.enabled && !definition.enabled;
            // Shown whether or not the schedule is switched on: a run that failed, failed, and
            // switching the cadence off afterwards does not unmake it.
            const failing = s.lastError !== "";
            return (
              <li key={s.id} className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
                <span className="nums">{cadence(s, locale, t)}</span>
                <Badge
                  variant={paused ? "unknown" : !s.enabled ? "unknown" : failing ? "warn" : "ok"}
                  dot
                  title={paused ? t("schedule.paused.title", { name: definition.name }) : undefined}
                >
                  {paused ? t("schedule.paused") : s.enabled ? t("schedule.enabled") : t("schedule.disabled")}
                </Badge>
                <span className="nums">{t("schedule.next", { at: fmtTime(s.nextFireAt, locale) })}</span>
                <span className="nums">{t("schedule.last", { at: fmtTime(s.lastFiredAt, locale) })}</span>
                {failing ? (
                  /* The scheduler's own message follows the colon, verbatim. */
                  <p
                    data-testid="schedule-failure"
                    className="basis-full text-xs leading-relaxed text-health-bad"
                    title={s.lastErrorAt ? t("schedule.recorded", { at: fmtTime(s.lastErrorAt, locale) }) : undefined}
                  >
                    {t("schedule.failing", { message: s.lastError })}
                  </p>
                ) : null}
              </li>
            );
          })}
        </ul>
      )}
    </li>
  );
}

function ChecksTab({ targetId, canRead }: { targetId: string; canRead: boolean }) {
  const t = useT(cardsDict);
  const { definitions, schedules, isLoading, error } = useTargetChecks(targetId, canRead);
  /* The Time Machine's honest line for this panel; saying so is the only option that is both true and cheap. */
  const { at } = useTimeContext();

  if (!canRead) {
    return <PermissionCard permission="checks:read">{t("target.checks.gate")}</PermissionCard>;
  }

  return (
    <Card asChild className="p-6">
      <section>
        <h3 className="text-sm font-semibold">{t("target.checks.heading")}</h3>
        {at ? (
          <p data-testid="checks-tm-notice" className="mt-1 text-xs leading-relaxed text-muted-foreground">
            {t("target.checks.tmNotice")}
          </p>
        ) : null}
        {error ? (
          <p role="alert" className="mt-3 text-sm text-health-bad">
            {queryErrorMessage(error, t("target.checks.unavailable"))}
          </p>
        ) : null}
        {isLoading && !error ? <ListSkeleton /> : null}
        {!isLoading && !error && definitions.length === 0 ? (
          <p className="px-1 py-10 text-center text-xs text-muted-foreground">{t("target.checks.empty")}</p>
        ) : null}
        {definitions.length > 0 ? (
          <ul aria-label={t("target.checks.listAria")} className="mt-4 divide-y divide-border">
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

/** Two honest degradations instead of an empty-axis chart. */
function HistoryTab({ targetName, promConfigured, promResolved }: { targetName: string; promConfigured: boolean; promResolved: boolean }) {
  const t = useT(cardsDict);
  const { locale } = useLocale();
  const { theme } = useTheme();
  /* ONE hour, resolved once, for the chart below AND for the bar under it —
     see useWindowAnchor (QA scope 2, finding #20). */
  const range = useWindowAnchor(HISTORY_RANGE_SECONDS);
  /* The scope is the target's NAME. */
  const { annotations, error: annotationsError, refresh } = useAnnotations(targetName, HISTORY_RANGE_SECONDS);
  /* The declared change windows over the same hour and the same scope (M6 Task
     9), and for the same reason the annotations are fetched here: a provider's
     maintenance on this target does not depend on Prometheus, so the bands are
     read even where the chart above cannot be. */
  const {
    windows,
    error: maintenanceError,
    refresh: refreshMaintenance,
  } = useMaintenance(targetName, HISTORY_RANGE_SECONDS, range);
  const chart = useMemo<CuratedChart>(
    () => ({
      id: "target-duration",
      title: t("target.history.title"),
      unit: "seconds",
      query: targetDurationQuery(targetName),
    }),
    [t, targetName],
  );
  // Engaged, the window ends at `t`: the hour before the instant being read,
  // not the hour before this render.
  const { at } = useTimeContext();
  const { data, isLoading, error } = useQuery({
    queryKey: ["target-series", targetName, range.to.toISOString()],
    queryFn: () => {
      const stepSeconds =
        Math.ceil(HISTORY_RANGE_SECONDS / HISTORY_TARGET_POINTS / HISTORY_MIN_STEP_SECONDS) * HISTORY_MIN_STEP_SECONDS;
      return promqlQueryRange(chart.query, range.from, range.to, stepSeconds * 1e9);
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
          {chart.title}{" "}
          {/* Inside a translated sentence, so the stamp takes that sentence's
              language — lib/i18n's localeTag. */}
          {at
            ? t("target.history.hourEnding", { at: at.toLocaleString(localeTag(locale)) })
            : t("target.history.lastHour")}
        </h3>

        {promResolved && !promConfigured ? (
          <p role="status" className="mt-3 text-xs leading-relaxed text-muted-foreground">
            {t("target.history.noPrometheus")}
          </p>
        ) : null}

        {error ? (
          <p role="alert" className="mt-3 text-sm text-health-bad">
            {queryErrorMessage(error, t("target.history.unavailable"))}
          </p>
        ) : null}
        {queryError ? (
          <p role="alert" className="mt-3 text-sm text-health-bad">
            {queryError}
          </p>
        ) : null}

        {promConfigured && isLoading && !data ? <Skeleton className="mt-3 h-64 w-full" /> : null}

        {empty ? (
          <p className="mt-3 text-xs leading-relaxed text-muted-foreground">{t("target.history.empty")}</p>
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
 * useTargetRuns is the Runs tab's data source; GET /api/v1/runs takes only
 * ?type=&status=&limit=&cursor= (docs/console-api.yaml's listRuns — verified, not assumed).
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
  const t = useT(cardsDict);
  const { locale } = useLocale();
  const { runs, isLoading, error } = useTargetRuns(targetName, true);

  return (
    <Card asChild className="overflow-hidden p-0">
      <section>
        <div className="border-b border-border px-4 py-3">
          <h3 className="text-sm font-semibold">{t("target.runs.heading")}</h3>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
            {t("target.runs.scanNote", { limit: RUN_SCAN_LIMIT })}
          </p>
        </div>

        {error ? (
          <p role="alert" className="px-4 py-4 text-sm text-health-bad">
            {queryErrorMessage(error, t("target.runs.unavailable"))}
          </p>
        ) : null}

        {isLoading && runs.length === 0 && !error ? (
          <div role="status" aria-live="polite" className="flex flex-col gap-2 p-4">
            <span className="sr-only">{t("target.runs.scanning")}</span>
            {Array.from({ length: 3 }, (_, i) => (
              <Skeleton key={i} className="h-8 w-full" />
            ))}
          </div>
        ) : null}

        {!isLoading && !error && runs.length === 0 ? (
          <p className="px-4 py-10 text-center text-xs text-muted-foreground">
            {t("target.runs.empty", { limit: RUN_SCAN_LIMIT })}
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
                  {t("target.runs.pairsOk", { ok: r.pairOk, total: r.pairTotal })}
                </span>
                <span className="text-xs text-muted-foreground">{fmtTime(r.createdAt, locale)}</span>
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
const TABS: { value: TargetTab; labelKey: CardsKey }[] = [
  { value: "checks", labelKey: "tab.checks" },
  { value: "history", labelKey: "tab.history" },
  { value: "runs", labelKey: "tab.runs" },
];

function NotFound({ id, known }: { id: string; known: boolean }) {
  const t = useT(cardsDict);
  return (
    <PageShell
      title={t("target.notFound.title")}
      description={id ? t("target.notFound.withId", { id }) : t("target.notFound.bare")}
    >
      <Card role="status" className="flex flex-col items-center gap-3 px-8 py-16 text-center">
        <span
          aria-hidden="true"
          className="flex size-12 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
        >
          <SearchX className="size-5" />
        </span>
        <p className="text-sm font-medium">
          {known ? t("target.notFound.known") : t("target.notFound.unknown")}
        </p>
        <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">
          {known ? t("target.notFound.knownBody") : t("target.notFound.unknownBody")}
        </p>
        <a href="/targets" className="text-xs font-medium text-primary hover:underline">
          {t("target.notFound.back")}
        </a>
      </Card>
    </PageShell>
  );
}

/**
 * TargetCardPage is PAGES.md §6.4's Target card: header (name, kind, address, health); it is
 * READ-ONLY by design. targets:write.
 */
export function TargetCardPage() {
  const t = useT(cardsDict);
  const { locale } = useLocale();
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
      <PageShell title={t("target.title")} description={t("loading")}>
        <Card role="status" aria-live="polite" className="p-6">
          <span className="sr-only">{t("target.loading")}</span>
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
      /* The DESCRIPTION is the id from the URL — data. */
      <PageShell title={t("target.title")} description={id}>
        <PermissionCard permission="targets:read">{t("target.gate.read")}</PermissionCard>
      </PageShell>
    );
  }

  // Database disabled: targets are configuration and get no in-memory fallback.
  if (!dbAvailable) {
    return (
      <PageShell title={t("target.title")} description={id}>
        <Card role="status" className="p-6">
          <p className="text-sm">{t("target.gate.noDatabase")}</p>
        </Card>
      </PageShell>
    );
  }

  if (notFound) return <NotFound id={id} known />;

  if (!target) {
    if (targetQuery.error) {
      return (
        <PageShell title={t("target.title")} description={id}>
          <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
            <p className="text-sm font-medium">{t("target.unavailable")}</p>
            <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
              {queryErrorMessage(targetQuery.error, t("target.loadFailed"))}
            </p>
          </Card>
        </PageShell>
      );
    }
    return (
      <PageShell title={t("target.title")} description={id}>
        <Card role="status" aria-live="polite" className="p-6">
          <span className="sr-only">{t("target.loading")}</span>
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
      description={
        at ? t("target.descriptionAt", { at: at.toLocaleString(localeTag(locale)) }) : t("target.description")
      }
      actions={
        <>
          {/* kind and address are the target ROW's own fields. */}
          <Badge variant="neutral">{target.kind}</Badge>
          <span className="nums max-w-[18rem] truncate text-sm text-muted-foreground" title={target.address}>
            {target.address}
          </span>
          {/* No percentage, no sentence. "— healthy" read as a claim with a
              missing number in front of it; the badge beside it already says
              the state in words, and it is the honest one. Round 2's finding
              #16 fixed exactly this on the node card and it was never carried
              across to this one (QA round 5, finding #7). */}
          {health.percent === null ? null : (
            <span className="nums text-sm text-muted-foreground">
              {t("health.percent", { percent: health.percent.toFixed(1) })}
            </span>
          )}
          <Badge variant={TIER_VARIANT[health.tier]} dot>
            {t(TIER_KEYS[health.tier])}
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
          <Segmented
            aria-label={t("tab.aria")}
            options={TABS.map((tb) => ({ value: tb.value, label: t(tb.labelKey) }))}
            value={tab}
            onChange={setTab}
          />
          {tab === "checks" ? <ChecksTab targetId={target.id} canRead={can("checks:read")} /> : null}
          {tab === "history" ? (
            <HistoryTab targetName={target.name} promConfigured={promConfigured} promResolved={promResolved} />
          ) : null}
          {tab === "runs" ? <RunsTab targetName={target.name} /> : null}
        </div>
        <div className="flex flex-col gap-2">
          <RelatedIncidents scope={investigationScope} />
          {/* scopeNode, not scope (QA round 4, finding #22). Every event a
              probe of this target produces is scoped per SOURCE node
              ("node-a→edge-gw", internal/console/events/live_event.go's
              pairScope), and `?scope=` is exact equality — so this rail was
              matching only the target-scoped events and silently dropping
              every pair row, which is nearly all of them. `?scopeNode=`
              (store.EventFilter.ScopeNode) is the filter that was built for
              exactly this: it admits the bare scope AND either side of a pair
              scope, the same mechanism pages/node-card.tsx already uses. */}
          <RecentChanges scopeNode={target.name} />
          <p className="px-1 text-[11px] leading-relaxed text-muted-foreground">
            {t("target.changesNote", { name: target.name })}
          </p>
        </div>
      </div>
    </PageShell>
  );
}
