import { useMemo, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { PageShell } from "@/components/page-shell";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { EmptyState } from "@/components/ui/empty-state";
import { Skeleton } from "@/components/ui/skeleton";
import { Table, TBody, Td, Th, THead, Tr } from "@/components/ui/table";
import { useAuth } from "@/hooks/use-auth";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import { useMatrix } from "@/hooks/use-matrix";
import { useTopology } from "@/hooks/use-topology";
import { localeTag, useLocale, useT, type Translate } from "@/lib/i18n";
import { overviewDict, type OverviewKey } from "@/lib/i18n/dict/overview";
import { getEvents, getIncidents, isServerSentence, listAlerts } from "@/lib/api";
import { buildInvestigateURL, incidentPermalink, scopeFromAlertLabels } from "@/lib/investigation-sources";
import { isMeasured } from "@/lib/matrix-cells";
import { withAtParam, useTimeContext } from "@/lib/timemachine";
import type { Alert, LiveEvent, LiveEventSeverity, Matrix, MatrixCell, Topology } from "@/lib/types";
import { cn, fmtEventStamp } from "@/lib/utils";

export interface OverviewSummary {
  totalNodes: number;
  readyNodes: number;
  /** Pairs with ANY measurement — see isMeasured. */
  pairsTotal: number;
  /** Pairs carrying a failure ratio, i.e. the ones the tiers below can rank.
   *  Always ≤ pairsTotal, and the gap is what the third blank slate is for. */
  pairsScored: number;
  pairsFailing: number;
  pairsDegraded: number;
  worstPairs: MatrixCell[]; // top 5, failing/degraded only, worst first
}

/* isMeasured used to live here. */

/**
 * compareWorst orders the problem table: failure ratio first, RTT as the tiebreak; two pairs
 * failing at the same ratio are not equally bad.
 */
function compareWorst(a: MatrixCell, b: MatrixCell): number {
  const fa = a.failRatio ?? 0;
  const fb = b.failRatio ?? 0;
  if (fa !== fb) return fb - fa;
  return (b.rttP95 ?? 0) - (a.rttP95 ?? 0);
}

/**
 * isScored is what "this pair can be RANKED" means, and it is deliberately
 * stricter than `!== null`. An absent key is `undefined`, which is not null and
 * used to be counted among the ranked — so a pair the console cannot place in a
 * tier padded the scored count, closed the scored/measured gap line and left
 * the page choosing "no failing or degraded pairs", which is a health claim
 * about pairs nobody scored. NaN and Infinity are the same case: they compare
 * false against every threshold, so they can only ever be counted as healthy.
 */
function isScored(cell: MatrixCell): boolean {
  return typeof cell.failRatio === "number" && Number.isFinite(cell.failRatio);
}

// A tier still needs a failure ratio — a cell with only an RTT is measured but unranked.
export function summarize(matrix: Matrix, topo?: Topology): OverviewSummary {
  const measured = matrix.cells.filter(isMeasured);
  const scored = matrix.cells.filter(isScored);
  const failing = scored.filter((c) => (c.failRatio ?? 0) >= 0.1);
  const degraded = scored.filter((c) => (c.failRatio ?? 0) >= 0.01 && (c.failRatio ?? 0) < 0.1);
  return {
    totalNodes: topo?.nodes.length ?? matrix.nodes.length,
    readyNodes: topo ? topo.nodes.filter((n) => n.ready).length : matrix.nodes.length,
    pairsTotal: measured.length,
    pairsScored: scored.length,
    pairsFailing: failing.length,
    pairsDegraded: degraded.length,
    worstPairs: [...failing, ...degraded].sort(compareWorst).slice(0, 5),
  };
}

/* It moved into the dictionary rather than staying a module constant because a constant cannot see the locale. */

/** T is this page's translator, threaded into the pure helpers below. */
type T = Translate<OverviewKey>;

/**
 * NodesTile is what the "Nodes ready" tile can honestly say. `noInventory` is the case that used to
 * read "0/0": the topology ANSWERED and carried no nodes, while agents (or the matrix) plainly knew
 * of some.
 */
export type NodesTile =
  | { kind: "loading" }
  | { kind: "unavailable" }
  | { kind: "counts"; ready: number; total: number }
  | { kind: "noInventory"; nodes: number; source: "agents" | "matrix" };

export function nodesTile(topo: Topology | undefined, loading: boolean, matrix?: Matrix): NodesTile {
  if (!topo) return loading ? { kind: "loading" } : { kind: "unavailable" };
  if (topo.nodes.length > 0) {
    return { kind: "counts", ready: topo.nodes.filter((n) => n.ready).length, total: topo.nodes.length };
  }
  // One agent per node (DaemonSet), so distinct nodeNames IS a node count.
  const fromAgents = new Set(topo.agents.map((a) => a.nodeName)).size;
  if (fromAgents > 0) return { kind: "noInventory", nodes: fromAgents, source: "agents" };
  const fromMatrix = matrix?.nodes.length ?? 0;
  if (fromMatrix > 0) return { kind: "noInventory", nodes: fromMatrix, source: "matrix" };
  // Nothing anywhere knows of a node: 0/0 is the answer, not a cover story.
  return { kind: "counts", ready: 0, total: 0 };
}

/**
 * foldBounds is the historical fold's own admission of incompleteness, or undefined while the
 * counters say nothing was lost.
 */
export function foldBounds(topo: Topology | undefined, t: T): string | undefined {
  if (!topo?.historical) return undefined;
  const lines: string[] = [];
  if (topo.truncated === true) lines.push(t("tiles.nodesReady.bounded.truncated"));
  const unfoldable = topo.unfoldableEvents ?? 0;
  if (unfoldable > 0) lines.push(t("tiles.nodesReady.bounded.unfoldable", { count: unfoldable }));
  return lines.length > 0 ? lines.join(" ") : undefined;
}

/**
 * healthStatement is the page's lead: the summarize() verdict in one sentence.
 * It says nothing without a scored pair (a health claim needs evidence), and
 * the healthy claim shrinks to the scored set when the scored/measured gap
 * exists — "all 90 healthy" off 9 ratios would be the scored-gap lie in words.
 */
export function healthStatement(s: OverviewSummary, t: T): { text: string; tone?: Tone } | null {
  if (s.pairsScored === 0) return null;
  if (s.pairsFailing > 0) {
    return {
      text: t(s.pairsFailing === 1 ? "health.failing.one" : "health.failing.many", { count: s.pairsFailing }),
      tone: "bad",
    };
  }
  if (s.pairsDegraded > 0) {
    return {
      text: t(s.pairsDegraded === 1 ? "health.degraded.one" : "health.degraded.many", { count: s.pairsDegraded }),
      tone: "warn",
    };
  }
  if (s.pairsScored < s.pairsTotal) return { text: t("health.healthy.scoped", { count: s.pairsScored }) };
  return { text: t(s.pairsTotal === 1 ? "health.healthy.one" : "health.healthy.many", { count: s.pairsTotal }) };
}

/**
 * fmtRtt renders nanoseconds as milliseconds, or an em-dash for anything that
 * is not a real measurement.
 *
 * `=== undefined` was too narrow by exactly the shapes the wire actually sends:
 * Go marshals a nil *float64 as null, and null/1e6 is 0 — so a pair with NO
 * latency sample reported 0.0ms, the fastest link in the fleet. JSON.parse
 * turns an out-of-range literal into Infinity, which toFixed renders as the
 * word. Neither is a number an operator may read as a latency.
 */
function fmtRtt(ns?: number | null): string {
  if (typeof ns !== "number" || !Number.isFinite(ns)) return "—";
  return `${(ns / 1e6).toFixed(1)}ms`;
}

type Tone = "warn" | "bad";

/* A tone only appears when the value itself means trouble, and it arrives on three channels at once — a left rail.
   No Card since M4-6: the tiles sit straight on the page background, so the
   figures read as the page's own numbers rather than three boxed widgets. */
function StatTile({
  label,
  value,
  tone,
  hint,
  note,
  toneLabel,
}: {
  label: string;
  value: ReactNode;
  tone?: Tone;
  hint?: string;
  /** A second line, for what BOUNDS the value rather than what qualifies it. */
  note?: string;
  toneLabel?: string;
}) {
  return (
    <div className="relative pl-4">
      {tone ? (
        <span
          aria-hidden="true"
          className={cn(
            "absolute inset-y-0 left-0 w-1",
            tone === "bad" ? "bg-health-bad" : "bg-health-warn",
          )}
        />
      ) : null}
      <div className="text-[11px] font-semibold uppercase tracking-[0.09em] text-muted-foreground">
        {label}
      </div>
      <div className="mt-3 flex flex-wrap items-baseline gap-x-3 gap-y-2">
        <span
          data-testid="stat-value"
          className={cn(
            "nums text-4xl font-semibold leading-none tracking-tight",
            tone === "bad" && "text-health-bad",
            tone === "warn" && "text-health-warn",
          )}
        >
          {value}
        </span>
        {tone && toneLabel ? (
          <Badge variant={tone === "bad" ? "bad" : "warn"}>{toneLabel}</Badge>
        ) : null}
      </div>
      {hint ? <p className="mt-2 text-xs text-muted-foreground">{hint}</p> : null}
      {note ? (
        <p data-testid="tile-note" className="mt-1 text-xs leading-relaxed text-muted-foreground">
          {note}
        </p>
      ) : null}
    </div>
  );
}

/* The blank slates render through ui/empty-state — the BlankSlate pattern that
   used to live here, lifted so every page draws the same slate. */

/** One install step: met or not, its value, and — unmet — the next thing to check. */
function SetupStep({ met, label, value, fix }: { met: boolean; label: string; value: ReactNode; fix: string }) {
  return (
    <li data-testid="setup-step" className="flex flex-col gap-1">
      <div className="flex items-center gap-3">
        <span
          aria-hidden="true"
          className={cn(
            "flex size-5 shrink-0 items-center justify-center rounded-full",
            met ? "bg-health-ok-soft text-health-ok" : "bg-surface-2 text-muted-foreground",
          )}
        >
          {met ? (
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" className="size-3">
              <path d="M5 13l4 4 10-10" strokeLinecap="round" strokeLinejoin="round" />
            </svg>
          ) : (
            <span className="size-1.5 rounded-full bg-current" />
          )}
        </span>
        <span className="text-sm font-medium">{label}</span>
        <span className={cn("text-sm", met ? "text-foreground" : "text-muted-foreground")}>{value}</span>
      </div>
      {!met ? <p className="pl-8 text-xs leading-relaxed text-muted-foreground">{fix}</p> : null}
    </li>
  );
}

/**
 * SetupProgress is the first-run card (M4-6): Live with zero measured pairs is
 * an install in progress, and ONE card walking agents → scrape → first round
 * says more than four separate empty panels. The signals are the ones the page
 * already holds: useTopology's agent list, useMatrix's series.
 */
function SetupProgress({ agents, promScraped }: { agents: number; promScraped: boolean }) {
  const t = useT(overviewDict);
  return (
    <Card asChild className="p-6" data-testid="setup-progress">
      <section aria-label={t("setup.title")}>
        <h2 className="type-section">{t("setup.title")}</h2>
        <ol className="mt-4 flex flex-col gap-3">
          <SetupStep
            met={agents > 0}
            label={t("setup.agents")}
            value={<span className="mono-data">{agents}</span>}
            fix={t("setup.agents.fix")}
          />
          <SetupStep
            met={promScraped}
            label={t("setup.prometheus")}
            value={t(promScraped ? "setup.yes" : "setup.no")}
            fix={t("setup.prometheus.fix")}
          />
          {/* Inside this card the round has by definition not landed yet. */}
          <SetupStep
            met={false}
            label={t("setup.probes")}
            value={t("setup.waiting")}
            fix={t("worstPairs.empty.noData.body")}
          />
        </ol>
      </section>
    </Card>
  );
}

function SkeletonBar({ className }: { className?: string }) {
  return <span aria-hidden="true" className={cn("block animate-pulse rounded-sm bg-muted", className)} />;
}

/* Loading state mirrors the shape of the loaded page — three tiles and a table —
   so nothing jumps when the data lands. */
function OverviewSkeleton() {
  const t = useT(overviewDict);
  return (
    <div role="status" aria-live="polite" className="flex flex-col gap-6">
      <span className="sr-only">{t("loading")}</span>
      <div className="grid gap-4 sm:grid-cols-3">
        {/* Bare like the loaded tiles — a boxed skeleton would jump on load. */}
        {[0, 1, 2].map((i) => (
          <div key={i} className="pl-4">
            <SkeletonBar className="h-2.5 w-24" />
            <SkeletonBar className="mt-4 h-8 w-20" />
          </div>
        ))}
      </div>
      <Card className="p-6">
        <SkeletonBar className="h-3 w-28" />
        <div className="mt-6 flex flex-col gap-4">
          {[0, 1, 2, 3].map((i) => (
            <SkeletonBar key={i} className="h-4 w-full" />
          ))}
        </div>
      </Card>
    </div>
  );
}

/*
 * the two panels that used to be placeholders Both follow the house degradation pattern the object
 * cards already use.
 */

const OPEN_INCIDENTS_LIMIT = 5;
const RECENT_EVENTS_LIMIT = 10;

/*
 * PANEL_POLL_MS is the cadence the three lower Overview panels (firing alerts, open incidents,
 * recent events) refetch at. Same 15s as MATRIX_POLL_MS / CAPABILITIES_POLL_MS, deliberately: the
 * page header tells the operator the view recomputes every 15 seconds.
 *
 * They had no refetchInterval at all, so they were fetched once on mount and never again. Overview
 * is the page an operator leaves open, and beside these three the tiles and the worst-pairs table
 * update from the WebSocket matrix push while the incident "age" column keeps counting — so the
 * panels looked live while a new incident, a rule that started firing, or an event that landed after
 * mount never appeared, and a resolved alert never went away.
 *
 * Engaged (Time Machine) they must NOT poll: `enabled` already goes false there, because at a pinned
 * instant "what is open now" is the wrong question — the same rule useMatrix follows.
 */
const PANEL_POLL_MS = 15_000;

/* The one-line database note is dict/overview.ts's "db.note", worded once so
   both panels say the same thing the Investigate page and the object cards
   already say. */

function PanelNote({ children }: { children: ReactNode }) {
  return <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{children}</p>;
}

function PanelSkeleton({ rows }: { rows: number }) {
  const t = useT(overviewDict);
  return (
    <div role="status" aria-live="polite" className="mt-3 flex flex-col gap-2">
      <span className="sr-only">{t("panel.loading")}</span>
      {Array.from({ length: rows }, (_, i) => (
        <Skeleton key={i} className="h-6 w-full" />
      ))}
    </div>
  );
}

/** fmtAge is the "how long has this been going" column — coarse on purpose:
 *  an incident open for three days does not become more legible at minute
 *  precision, and a bare timestamp makes the reader do the subtraction. The
 *  unit letter comes from the dictionary; s/m/h/d is English, not arithmetic. */
export function fmtAge(iso: string, now: Date, t: T): string {
  const then = new Date(iso);
  if (Number.isNaN(then.getTime())) return "—";
  const seconds = Math.max(0, Math.round((now.getTime() - then.getTime()) / 1000));
  if (seconds < 60) return t("age.seconds", { count: seconds });
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return t("age.minutes", { count: minutes });
  const hours = Math.round(minutes / 60);
  if (hours < 48) return t("age.hours", { count: hours });
  return t("age.days", { count: Math.round(hours / 24) });
}

/** OpenIncidents is the Overview's link into Investigation Mode: the newest five incidents still open. */
function OpenIncidents() {
  const t = useT(overviewDict);
  const { me, can } = useAuth();
  const { available, resolved } = useDatabaseAvailable();
  const { at } = useTimeContext();
  const canRead = can("incidents:read");
  const enabled = me !== undefined && canRead && resolved && available;

  /*
   * Engaged, "open" is the wrong question to ask this endpoint; `status` is a NOW fact (it is
   * resolved_at's witness).
   */
  const query = useQuery({
    queryKey: at ? ["overview", "incidents", "at", at.toISOString()] : ["overview", "incidents"],
    queryFn: () =>
      getIncidents(
        at
          ? { from: at, to: new Date(at.getTime() + 1000), limit: OPEN_INCIDENTS_LIMIT }
          : { status: "open", limit: OPEN_INCIDENTS_LIMIT },
      ),
    enabled,
    refetchInterval: enabled ? PANEL_POLL_MS : false,
  });
  const incidents = query.data?.incidents ?? [];
  /* Engaged, "how long has this been open" is measured from the instant on
     screen: against the wall clock a row asked for at t reported an age that
     included every hour since t. */
  const now = at ?? new Date();

  return (
    /* min-w-0: this Card is a GRID CHILD (the two-panel row at the bottom of the
       page), and a grid item's default min-width:auto refuses to shrink below
       its content's min-content width — at 375px that pushed the implicit
       column to 479px and the whole main to a 495px horizontal scroll. */
    <Card asChild className="min-w-0 p-6" data-testid="open-incidents-panel">
      <section aria-label={t("incidents.title")}>
        <h2 className="type-section">{t("incidents.title")}</h2>

        {me !== undefined && !canRead ? (
          <PanelNote>{t("incidents.denied")}</PanelNote>
        ) : resolved && !available ? (
          <PanelNote>{t("db.note")}</PanelNote>
        ) : query.isError ? (
          <PanelNote>{t("incidents.error")}</PanelNote>
        ) : !enabled || query.isLoading ? (
          <PanelSkeleton rows={3} />
        ) : incidents.length === 0 ? (
          <PanelNote>{t("incidents.empty")}</PanelNote>
        ) : (
          <ul className="mt-3 flex flex-col divide-y divide-border">
            {incidents.map((i) => (
              <li key={i.id} data-testid="open-incident" className="flex items-center gap-3 py-2">
                <a href={incidentPermalink(i.id)} className="min-w-0 flex-1 truncate text-sm text-primary hover:underline">
                  {i.title}
                </a>
                {/* TRUNCATED and shrinkable: a scope is a node or pair name off the wire, and an
                    unbounded nowrap badge pushed the title and the age clean off the card. */}
                <Badge variant="neutral" className="max-w-[12rem] shrink truncate" title={i.scope}>
                  {i.scope === "" ? t("incidents.scope.global") : i.scope}
                </Badge>
                <span className="nums w-10 shrink-0 text-right text-xs text-muted-foreground">
                  {fmtAge(i.fromAt, now, t)}
                </span>
              </li>
            ))}
          </ul>
        )}
      </section>
    </Card>
  );
}

const SEVERITY_VARIANT: Record<LiveEventSeverity, "neutral" | "warn" | "bad"> = {
  info: "neutral",
  warn: "warn",
  error: "bad",
};

function isKnownSeverity(v: string): v is LiveEventSeverity {
  return v === "info" || v === "warn" || v === "error";
}

/*
 * The Live feed's own wording, kept identical here on purpose: the same event was "warn" on this
 * card and "Warn" on /live.
 */
const SEVERITY_KEYS: Record<LiveEventSeverity, OverviewKey> = {
  info: "severity.info",
  warn: "severity.warn",
  error: "severity.error",
};

/**
 * OverviewEventRow is a deliberately MINIMAL copy of pages/live.tsx's EventRow (the seam: live.tsx
 * does not export it, and exporting a row built for a full-width virtualised feed into a half-width
 * summary card would drag its five fixed-width columns along).
 */
function OverviewEventRow({ event }: { event: LiveEvent }) {
  const t = useT(overviewDict);
  const { locale } = useLocale();
  return (
    <Tr data-testid="overview-event">
      {/* The DAY, not a bare clock. This card is fed with `to = t` when the Time Machine is engaged
          and has no lower bound at all, so its ten newest rows can be days old — and under a heading
          that says "Recent events", beside a banner naming another date, a bare "14:03" reads as
          this afternoon. The two sibling feeds (/live, recent-changes) already print it this way. */}
      <Td className="mono-data whitespace-nowrap pr-3 text-muted-foreground">
        {fmtEventStamp(event.timestamp, localeTag(locale))}
      </Td>
      <Td className="pr-3">
        <Badge variant={isKnownSeverity(event.severity) ? SEVERITY_VARIANT[event.severity] : "unknown"} dot>
          {isKnownSeverity(event.severity) ? t(SEVERITY_KEYS[event.severity]) : event.severity}
        </Badge>
      </Td>
      {/* w-full + max-w-0 is the table-cell spelling of min-w-0 flex-1: take
          the slack, and truncate rather than push the card open. */}
      <Td className="w-full max-w-0">
        <span className="block truncate" title={event.summary}>
          {event.summary}
        </span>
      </Td>
      <Td className="hidden pl-3 sm:table-cell">
        <span className="mono-data block w-36 truncate text-muted-foreground" title={event.scope}>
          {event.scope}
        </span>
      </Td>
    </Tr>
  );
}

/** RecentEvents finally wires the events API. */
function RecentEvents() {
  const t = useT(overviewDict);
  const { me, can } = useAuth();
  const { available, resolved } = useDatabaseAvailable();
  const { at } = useTimeContext();
  const canRead = can("events:read");
  const enabled = me !== undefined && canRead && resolved && available;

  /* Engaged, `to=t` makes this the newest ten AT OR BEFORE t — the same bound
     /live's scrollback takes, and for the same reason: "recent" under a banner
     that says "you are viewing 12:00" cannot mean "since 12:00". Exclusive
     server-side (store.EventFilter.To). */
  const query = useQuery({
    queryKey: at ? ["overview", "events", "at", at.toISOString()] : ["overview", "events"],
    queryFn: () => getEvents({ limit: RECENT_EVENTS_LIMIT, ...(at ? { to: at } : {}) }),
    enabled,
    refetchInterval: enabled ? PANEL_POLL_MS : false,
  });
  const events = query.data?.events ?? [];

  return (
    <Card asChild className="p-6">
      <section aria-label={t("events.title")}>
        <div className="flex flex-wrap items-baseline justify-between gap-2">
          <h2 className="type-section">{t("events.title")}</h2>
          <a href={withAtParam("/live")} className="text-xs text-primary hover:underline">
            {t("events.open")}
          </a>
        </div>

        {me !== undefined && !canRead ? (
          <PanelNote>{t("events.denied")}</PanelNote>
        ) : resolved && !available ? (
          <PanelNote>{t("db.note")}</PanelNote>
        ) : query.isError ? (
          <PanelNote>{t("events.error")}</PanelNote>
        ) : !enabled || query.isLoading ? (
          <PanelSkeleton rows={4} />
        ) : events.length === 0 ? (
          /* "Nothing has happened yet" is a LIVE sentence; bounded by to=t it
             would be claiming the fleet had never done anything by then. */
          <PanelNote>{t(at ? "events.empty.engaged" : "events.empty")}</PanelNote>
        ) : (
          <Table variant="dense" containerClassName="mt-3">
            <TBody>
              {events.map((e) => (
                <OverviewEventRow key={e.id} event={e} />
              ))}
            </TBody>
          </Table>
        )}
      </section>
    </Card>
  );
}

/*
 * It is the operator's morning view, and it asks for the rules this console MANAGES and no others:
 * kconmon-ng is not an aggregator of a cluster's whole firing state. See lib/api.ts's listAlerts.
 */

const ALERT_SEVERITY_RANK: Record<string, number> = { critical: 0, warning: 1, info: 2 };
const ALERT_SEVERITY_TONE: Record<string, "neutral" | "warn" | "bad"> = {
  critical: "bad",
  warning: "warn",
  info: "neutral",
};

/** How many rows the card shows before it stops. The firing set is unbounded
 *  (a bad afternoon in a big cluster is hundreds of series) and this is a
 *  summary card, so it truncates and SAYS the number it left out rather than
 *  ending in a silent cliff. */
const FIRING_ALERTS_LIMIT = 8;

/**
 * sortFiringAlerts is the card's order: severity first; an unrecognised severity sorts LAST rather
 * than being dropped or guessed.
 */
export function sortFiringAlerts(alerts: Alert[]): Alert[] {
  const rank = (a: Alert) => ALERT_SEVERITY_RANK[a.severity] ?? Object.keys(ALERT_SEVERITY_RANK).length;
  const started = (a: Alert) => (a.activeAt === undefined ? Number.POSITIVE_INFINITY : Date.parse(a.activeAt));
  return [...alerts].sort((x, y) => {
    if (rank(x) !== rank(y)) return rank(x) - rank(y);
    const sx = started(x);
    const sy = started(y);
    if (sx !== sy) return sx < sy ? -1 : 1;
    return x.name < y.name ? -1 : x.name > y.name ? 1 : 0;
  });
}

/**
 * alertLabels is the one place a row's label map is read, and it tolerates the
 * map not being there. Go marshals a nil map as null: Object.keys(null) throws,
 * and with no error boundary over this route ONE such row took the whole
 * Overview to a blank page.
 */
function alertLabels(alert: Alert): Record<string, string> {
  const labels: unknown = alert.labels;
  return labels !== null && typeof labels === "object" ? (labels as Record<string, string>) : {};
}

function alertLabelLine(labels: Record<string, string>): string {
  return Object.keys(labels)
    .sort()
    .map((k) => `${k}=${labels[k]}`)
    .join(" ");
}

/**
 * alertSeverity is the badge's text. "" is the documented empty case and an
 * ABSENT key is the same case — rendering `undefined` there produced a coloured
 * chip with nothing in it, which asserts a severity the row does not carry.
 */
function alertSeverity(alert: Alert): string {
  return typeof alert.severity === "string" ? alert.severity.trim() : "";
}

/** One firing row. The label set travels in a `title` attribute on a truncated
 *  line — the worst-pairs table's own idiom for detail that will not fit. */
function FiringAlertRow({ alert, now }: { alert: Alert; now: Date }) {
  const t = useT(overviewDict);
  const labelMap = alertLabels(alert);
  const scope = scopeFromAlertLabels(labelMap);
  const labels = alertLabelLine(labelMap);
  const severity = alertSeverity(alert);

  return (
    <li data-testid="firing-alert" className="flex flex-wrap items-baseline gap-x-3 gap-y-1 py-2">
      {/* Every row here is a rule this console manages — lib/api.ts asks for no
          others — so every name links to its rule. The "unmanaged" badge that
          used to sit beside a foreign row went with the rows it explained. */}
      {/* ?rule= rather than a bare /alerting: the list can be long and the row an
          operator is chasing is one of many. */}
      <a
        href={withAtParam(`/alerting?rule=${encodeURIComponent(alert.ruleId ?? "")}`)}
        data-testid="firing-alert-name"
        className="min-w-0 flex-1 truncate text-sm text-primary hover:underline"
      >
        {alert.name}
      </a>
      <Badge variant={ALERT_SEVERITY_TONE[severity] ?? "unknown"} dot>
        {severity === "" ? t("alerts.noSeverity") : severity}
      </Badge>
      <span className="nums w-10 shrink-0 text-right text-xs text-muted-foreground">
        {alert.activeAt === undefined ? "—" : fmtAge(alert.activeAt, now, t)}
      </span>
      <span
        data-testid="firing-alert-labels"
        title={labels}
        className="w-full truncate text-xs text-muted-foreground"
      >
        {labels}
      </span>
      {scope ? (
        <a href={buildInvestigateURL(scope, now)} className="text-xs text-primary hover:underline">
          {t("alerts.investigate")}
        </a>
      ) : null}
    </li>
  );
}

/** FiringAlerts reads GET /api/v1/alerts, whose degraded shape is the reason this card can be honest at all. */
function FiringAlerts() {
  const t = useT(overviewDict);
  const { me, can } = useAuth();
  const { at } = useTimeContext();
  const engaged = at !== null;
  const canRead = can("alerts:read");
  /*
   * Engaged this card asks for NOTHING; it says so, and the request is not made at all rather than
   * made and discarded.
   */
  const enabled = me !== undefined && canRead && !engaged;

  const query = useQuery({
    queryKey: ["overview", "alerts"],
    queryFn: listAlerts,
    enabled,
    refetchInterval: enabled ? PANEL_POLL_MS : false,
  });
  const now = new Date();
  /*
   * The route serves both states (a rule's `for` window is exactly the gap between them) and a card
   * called "Firing alerts" that listed pending rows would be claiming a page that has not happened.
   */
  const alerts = useMemo(
    () => sortFiringAlerts((query.data?.alerts ?? []).filter((a) => a.state === "firing")),
    [query.data],
  );
  const shown = alerts.slice(0, FIRING_ALERTS_LIMIT);
  const hidden = alerts.length - shown.length;

  return (
    /* min-w-0 for the same reason as OpenIncidents above: grid child, min-width:auto, 375 → 495. */
    <Card asChild className="min-w-0 p-6" data-testid="firing-alerts-panel">
      <section aria-label={t("alerts.title")}>
        <div className="flex flex-wrap items-baseline justify-between gap-2">
          <h2 className="type-section">{t("alerts.title")}</h2>
          {canRead ? (
            <a href={withAtParam("/alerting")} className="text-xs text-primary hover:underline">
              {t("alerts.open")}
            </a>
          ) : null}
        </div>

        {engaged ? (
          <PanelNote>{t("alerts.engaged")}</PanelNote>
        ) : me !== undefined && !canRead ? (
          <PanelNote>{t("alerts.denied")}</PanelNote>
        ) : query.isError ? (
          /* The server's own sentence wins, and ONLY the server's: ApiError is
             what carries problem+json through verbatim. Any other rejection —
             a cut connection, a proxy's HTML page under a JSON content type —
             would otherwise make the fetch layer's or the JSON parser's own
             wording the operator's error line, naming a mechanism nobody can
             act on. */
          <PanelNote>{isServerSentence(query.error) ? query.error.message : t("alerts.error")}</PanelNote>
        ) : !enabled || query.isLoading ? (
          <PanelSkeleton rows={3} />
        ) : query.data?.promConfigured === false ? (
          <PanelNote>{t("alerts.noPrometheus")}</PanelNote>
        ) : alerts.length === 0 ? (
          <PanelNote>{t("alerts.empty")}</PanelNote>
        ) : (
          <>
            <ul className="mt-3 flex flex-col divide-y divide-border">
              {shown.map((a) => (
                <FiringAlertRow key={`${a.name}{${alertLabelLine(alertLabels(a))}}`} alert={a} now={now} />
              ))}
            </ul>
            {hidden > 0 ? (
              <PanelNote>
                {t(hidden === 1 ? "alerts.hidden.one" : "alerts.hidden.many", { count: hidden })}
              </PanelNote>
            ) : null}
            {/* WHOSE alerts these are. Without it a card headed "Firing alerts"
                over a filtered list reads as the cluster's whole firing state,
                which is the one thing it is not. */}
            <PanelNote>{t("alerts.managedOnly")}</PanelNote>
          </>
        )}
      </section>
    </Card>
  );
}

function WorstPairsTable({ pairs }: { pairs: MatrixCell[] }) {
  const t = useT(overviewDict);
  /* The instant the table is drawn at, so a drill-down opens the same moment (null = Live) —
     the matrix grid's own rule for its cell links. */
  const { at } = useTimeContext();
  return (
    <Table variant="dense">
      <caption className="sr-only">{t("table.caption")}</caption>
      <THead>
        <Tr>
          {/* "#" is a symbol, not a word — the rank column reads the same in
              every language, so it stays out of the dictionary. */}
          <Th className="w-10 pr-4">#</Th>
          <Th className="pr-6">{t("table.pair")}</Th>
          <Th numeric className="pr-6">
            {t("table.fail")}
          </Th>
          <Th numeric className="pr-6">
            {t("table.rtt")}
          </Th>
          <Th>{t("table.status")}</Th>
          {/* The investigate column carries links, not data — named for screen readers only. */}
          <Th className="pl-4">
            <span className="sr-only">{t("table.investigate")}</span>
          </Th>
        </Tr>
      </THead>
      <TBody>
        {pairs.map((c, i) => {
            const fail = c.failRatio ?? 0;
            const failing = fail >= 0.1;
            /* Same two links a matrix cell carries: the pair card AT the viewed instant, and an
               investigation window ending there rather than at the wall clock. */
            const pairHref = withAtParam(
              `/pairs/${encodeURIComponent(c.source)}/${encodeURIComponent(c.destination)}`,
            );
            const investigateHref = buildInvestigateURL(
              { kind: "pair", a: c.source, b: c.destination },
              at ?? new Date(),
            );
            return (
              <Tr
                /* The rank leads the key: two cells naming the same pair is
                   nonsense the wire can still carry, and two rows under one
                   React key render as one. */
                key={`${i} ${c.source} ${c.destination}`}
                className="transition-colors duration-(--dur) ease-(--ease) hover:bg-accent/40"
              >
                <Td className="nums pr-4 text-xs text-muted-foreground">{i + 1}</Td>
                <Td className="max-w-[22rem] pr-6">
                  <a
                    href={pairHref}
                    data-testid="worst-pair-link"
                    className="mono-data flex items-center gap-2 rounded text-primary hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  >
                    <span className="truncate" title={c.source}>
                      {c.source}
                    </span>
                    <span aria-hidden="true" className="shrink-0 text-muted-foreground">
                      →
                    </span>
                    <span className="truncate" title={c.destination}>
                      {c.destination}
                    </span>
                  </a>
                </Td>
                <Td
                  numeric
                  className={cn("pr-6 font-semibold tracking-tight", failing ? "text-health-bad" : "text-health-warn")}
                >
                  {(100 * fail).toFixed(1)}%
                </Td>
                {/* The RTT is a value, not a caption — it reads in the foreground. */}
                <Td numeric className="pr-6">
                  {fmtRtt(c.rttP95)}
                </Td>
                <Td>
                  <Badge variant={failing ? "bad" : "warn"} dot>
                    {t(failing ? "table.status.failing" : "table.status.degraded")}
                  </Badge>
                </Td>
                <Td className="pl-4 text-right">
                  <a
                    href={investigateHref}
                    data-testid="worst-pair-investigate"
                    className="text-xs text-primary hover:underline"
                  >
                    {t("table.investigate")}
                  </a>
                </Td>
              </Tr>
            );
          })}
      </TBody>
    </Table>
  );
}

/**
 * problemDetail is the second line of a failed-dependency card: the SERVER's
 * own sentence when it sent one, and this console's own when it did not.
 *
 * `error.message` alone was right only for problem+json. A cut connection or a
 * proxy answering an HTML page under a JSON content type produced a fetch or
 * parser message — "Failed to fetch", "Unexpected end of JSON input" — which
 * names a mechanism no operator can act on and is in one language whatever the
 * chrome is set to.
 */
function problemDetail(error: Error, t: T): string {
  return isServerSentence(error) ? error.message : t("problem.unreadable");
}

/** PageProblem is one failed dependency, said in its own sentence. */
function PageProblem({ what, detail }: { what: string; detail: string }) {
  return (
    <div data-testid="overview-problem">
      <p className="text-sm font-medium">{what}</p>
      <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{detail}</p>
    </div>
  );
}

export function OverviewPage() {
  const t = useT(overviewDict);
  const topo = useTopology();
  const matrix = useMatrix("tcp");
  const { isLive } = useTimeContext();

  const summary = matrix.data ? summarize(matrix.data, topo.data) : undefined;
  const nodes = nodesTile(topo.data, topo.isLoading, matrix.data);
  const nodesValue =
    nodes.kind === "counts"
      ? `${nodes.ready}/${nodes.total}`
      : nodes.kind === "noInventory"
        ? String(nodes.nodes)
        : nodes.kind === "loading"
          ? "…"
          : "—";
  const nodesHint =
    nodes.kind === "unavailable"
      ? t("tiles.nodesReady.noTopology")
      : nodes.kind === "noInventory"
        ? t(nodes.source === "agents" ? "tiles.nodesReady.fromAgents" : "tiles.nodesReady.fromMatrix")
        : undefined;
  /* Zero measured pairs means zero over zero, and a bare 0 there reads as a
     clean fleet — the tile takes the nodes tile's em-dash instead. */
  const noPairs = summary !== undefined && summary.pairsTotal === 0;
  const pairsValue = (n: number) => (noPairs ? "—" : n);
  const pairsNote = noPairs ? t("tiles.pairs.noData") : undefined;
  const statement = summary ? healthStatement(summary, t) : null;
  /* Live at zero measured pairs is an install in progress; engaged it is a
     past instant with no samples, which the engaged slate already explains. */
  const firstRun = isLive && noPairs;

  return (
    <PageShell timeMachine title={t("title")} help={{ body: t("help.body"), slug: "overview" }} description={t(isLive ? "description" : "description.engaged")}>
      <div className="flex flex-col gap-6">
        {matrix.error || topo.error ? (
          <Card role="alert" className="flex flex-col gap-3 border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
            {matrix.error ? <PageProblem what={t("problem.matrix")} detail={problemDetail(matrix.error, t)} /> : null}
            {topo.error ? <PageProblem what={t("problem.topology")} detail={problemDetail(topo.error, t)} /> : null}
            {/* Only true while Live: engaged, both queries have their poll off
                on purpose (a past instant's answer cannot change), so promising
                a retry that is never going to happen would be a second lie
                stacked on the first. */}
            {isLive ? (
              <p className="text-xs leading-relaxed text-muted-foreground">{t("problem.retry")}</p>
            ) : null}
          </Card>
        ) : null}

        {!summary && matrix.isLoading ? <OverviewSkeleton /> : null}

        {summary ? (
          <>
            {/* The page LEADS with the verdict in words (M4-6); the tiles below
                carry the arithmetic. Nothing scored — nothing claimed. */}
            {statement ? (
              <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
                <p
                  data-testid="health-statement"
                  className={cn(
                    "nums text-3xl font-semibold tracking-tight",
                    statement.tone === "bad" && "text-health-bad",
                    statement.tone === "warn" && "text-health-warn",
                  )}
                >
                  {statement.text}
                </p>
                <Badge variant="neutral">{t("qualifier")}</Badge>
              </div>
            ) : null}

            {firstRun ? (
              <SetupProgress
                agents={topo.data?.agents.length ?? 0}
                promScraped={
                  (matrix.data?.cells.length ?? 0) > 0 || (matrix.data?.nodes.length ?? 0) > 0
                }
              />
            ) : null}

            <div className="grid gap-4 sm:grid-cols-3">
              <StatTile
                label={t("tiles.nodesReady")}
                value={nodesValue}
                hint={nodesHint}
                /* A historical fold is only as complete as the events it had. */
                note={foldBounds(topo.data, t)}
              />
              {/* Both pair tiles carry the qualifier: they count ONE protocol
                  on ONE plane, and the bare label claimed the whole fleet. */}
              <StatTile
                label={t("tiles.failing")}
                value={pairsValue(summary.pairsFailing)}
                tone={summary.pairsFailing > 0 ? "bad" : undefined}
                toneLabel={t("tiles.failing.tone")}
                hint={t("qualifier")}
                note={pairsNote}
              />
              <StatTile
                label={t("tiles.degraded")}
                value={pairsValue(summary.pairsDegraded)}
                tone={summary.pairsDegraded > 0 ? "warn" : undefined}
                toneLabel={t("tiles.degraded.tone")}
                hint={t("qualifier")}
                note={pairsNote}
              />
            </div>

            {/* On first run the setup card above IS this panel's answer. */}
            {firstRun ? null : (
            <Card asChild className="p-6">
              <section>
                <div className="flex flex-wrap items-baseline justify-between gap-2">
                  <span className="flex flex-wrap items-baseline gap-2">
                    <h2 className="type-section">{t("worstPairs.title")}</h2>
                    <Badge variant="neutral">{t("qualifier")}</Badge>
                  </span>
                  <p className="nums text-xs text-muted-foreground">
                    {t(summary.pairsTotal === 1 ? "worstPairs.measured.one" : "worstPairs.measured.many", {
                      count: summary.pairsTotal,
                    })}
                  </p>
                </div>
                {/* Stated wherever the gap exists, not only at scored=0: 9
                    ranked out of 90 measured is not a healthy fleet. */}
                {summary.pairsScored > 0 && summary.pairsScored < summary.pairsTotal ? (
                  <p data-testid="scored-gap" className="mt-1 text-xs leading-relaxed text-muted-foreground">
                    {t("worstPairs.scoredGap", { scored: summary.pairsScored, total: summary.pairsTotal })}
                  </p>
                ) : null}
                {summary.worstPairs.length === 0 ? (
                  summary.pairsTotal === 0 ? (
                    <EmptyState
                      title={t(isLive ? "worstPairs.empty.noData.title" : "worstPairs.empty.noData.title.engaged")}
                      body={t(isLive ? "worstPairs.empty.noData.body" : "worstPairs.empty.noData.body.engaged")}
                    />
                  ) : summary.pairsScored === 0 ? (
                    // Measured, but not RANKABLE: latency arrived and the failure-ratio series did
                    // not.
                    <EmptyState
                      title={t("worstPairs.empty.unscored.title")}
                      body={t(
                        summary.pairsTotal === 1
                          ? "worstPairs.empty.unscored.body.one"
                          : "worstPairs.empty.unscored.body.many",
                        { count: summary.pairsTotal },
                      )}
                    />
                  ) : (
                    <EmptyState
                      title={t("worstPairs.empty.healthy.title")}
                      body={t("worstPairs.empty.healthy.body")}
                    />
                  )
                ) : (
                  <div className="mt-4">
                    <WorstPairsTable pairs={summary.worstPairs} />
                  </div>
                )}
              </section>
            </Card>
            )}

            <div className="grid gap-4 sm:grid-cols-2">
              <FiringAlerts />
              <OpenIncidents />
            </div>

            <RecentEvents />
          </>
        ) : null}
      </div>
    </PageShell>
  );
}
