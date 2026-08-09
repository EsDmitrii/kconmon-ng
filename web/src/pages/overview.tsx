import { useMemo, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { PageShell } from "@/components/page-shell";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { useAuth } from "@/hooks/use-auth";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import { useMatrix } from "@/hooks/use-matrix";
import { useTopology } from "@/hooks/use-topology";
import { getEvents, getIncidents, listAlerts } from "@/lib/api";
import { buildInvestigateURL, incidentPermalink, scopeFromAlertLabels } from "@/lib/investigation-sources";
import { isMeasured } from "@/lib/matrix-cells";
import { useTimeContext } from "@/lib/timemachine";
import type { Alert, LiveEvent, LiveEventSeverity, Matrix, MatrixCell, Topology } from "@/lib/types";
import { cn, fmtEventTime } from "@/lib/utils";

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

/* isMeasured used to live here (QA round 1, finding #3). It is now
   lib/matrix-cells.ts's, imported above and shared with the grid, the object
   cards and the topology edges — round 2's finding #1 was the same misreading
   surviving in four OTHER files, which is what a private copy buys. The one
   change on the way out: packet loss counts as a measurement too. */

/**
 * compareWorst orders the problem table: failure ratio first, RTT as the
 * tiebreak. Two pairs failing at the same ratio are not equally bad — the
 * slower one is where an operator should look — and a comparator that left
 * them in map order would reshuffle them between renders.
 */
function compareWorst(a: MatrixCell, b: MatrixCell): number {
  const fa = a.failRatio ?? 0;
  const fb = b.failRatio ?? 0;
  if (fa !== fb) return fb - fa;
  return (b.rttP95 ?? 0) - (a.rttP95 ?? 0);
}

// Health tiers mirror the matrix/topology thresholds: fail ≥ 10% is "failing",
// 1%–10% is "degraded". A tier still needs a failure ratio — a cell with only
// an RTT is measured but unranked, which is why pairsScored exists next to
// pairsTotal rather than one number standing in for both.
export function summarize(matrix: Matrix, topo?: Topology): OverviewSummary {
  const measured = matrix.cells.filter(isMeasured);
  const scored = matrix.cells.filter((c) => c.failRatio !== null);
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

/** The qualifier every pair number on this page needs: the tiles and the worst
 *  list read ONE protocol on ONE plane (useMatrix("tcp") below), and an
 *  unlabelled "Failing pairs" claims the whole fleet's UDP and ICMP too. */
const MATRIX_QUALIFIER = "TCP · pod plane";

function fmtRtt(ns?: number): string {
  if (ns === undefined) return "—";
  return `${(ns / 1e6).toFixed(1)}ms`;
}

type Tone = "warn" | "bad";

/* Stat tile: the number is the hero, the label is a quiet caption above it. A
   tone only appears when the value itself means trouble, and it arrives on three
   channels at once — a left rail, the figure's colour and a worded badge — so
   the state survives greyscale and colour-blind readers. */
function StatTile({
  label,
  value,
  tone,
  hint,
  toneLabel,
}: {
  label: string;
  value: ReactNode;
  tone?: Tone;
  hint?: string;
  toneLabel?: string;
}) {
  return (
    <Card className="relative overflow-hidden p-5">
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
    </Card>
  );
}

/* Blank Slate: a short sentence saying why the panel is empty plus the next
   action, never a bare "No data." */
function BlankSlate({ title, body }: { title: string; body: string }) {
  return (
    <div className="flex flex-col items-center gap-2 px-6 py-10 text-center">
      <span
        aria-hidden="true"
        className="mb-1 flex size-10 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
      >
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" className="size-5">
          <circle cx="12" cy="12" r="9" />
          <path d="M9 12h6" strokeLinecap="round" />
        </svg>
      </span>
      <p className="text-sm font-medium">{title}</p>
      <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">{body}</p>
    </div>
  );
}

function SkeletonBar({ className }: { className?: string }) {
  return <span aria-hidden="true" className={cn("block animate-pulse rounded-sm bg-muted", className)} />;
}

/* Loading state mirrors the shape of the loaded page — three tiles and a table —
   so nothing jumps when the data lands. */
function OverviewSkeleton() {
  return (
    <div role="status" aria-live="polite" className="flex flex-col gap-6">
      <span className="sr-only">Loading overview…</span>
      <div className="grid gap-4 sm:grid-cols-3">
        {[0, 1, 2].map((i) => (
          <Card key={i} className="p-5">
            <SkeletonBar className="h-2.5 w-24" />
            <SkeletonBar className="mt-4 h-8 w-20" />
          </Card>
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

/* ── M6: the two panels that used to be placeholders (plan Decision 9) ─────
   Both follow the house degradation pattern the object cards already use: a
   missing READ permission and a console with no database are DIFFERENT facts
   and get different sentences, and neither issues a request. An empty list
   after a successful read is a third fact again — "nothing is open" is an
   answer, not a failure. */

const OPEN_INCIDENTS_LIMIT = 5;
const RECENT_EVENTS_LIMIT = 10;

/** The one-line database note, worded once so both panels say the same thing
 *  the Investigate page and the object cards already say. */
const DB_NOTE = "History needs a database — set console.database.mode. Nothing was requested.";

function PanelNote({ children }: { children: ReactNode }) {
  return <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{children}</p>;
}

function PanelSkeleton({ rows }: { rows: number }) {
  return (
    <div role="status" aria-live="polite" className="mt-3 flex flex-col gap-2">
      <span className="sr-only">Loading…</span>
      {Array.from({ length: rows }, (_, i) => (
        <Skeleton key={i} className="h-6 w-full" />
      ))}
    </div>
  );
}

/** fmtAge is the "how long has this been going" column — coarse on purpose:
 *  an incident open for three days does not become more legible at minute
 *  precision, and a bare timestamp makes the reader do the subtraction. */
export function fmtAge(iso: string, now: Date): string {
  const then = new Date(iso);
  if (Number.isNaN(then.getTime())) return "—";
  const seconds = Math.max(0, Math.round((now.getTime() - then.getTime()) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.round(minutes / 60);
  if (hours < 48) return `${hours}h`;
  return `${Math.round(hours / 24)}d`;
}

/**
 * OpenIncidents is the Overview's link into Investigation Mode: the newest five
 * incidents still open, each one a permalink that hydrates the whole page from
 * the saved row (/investigate?incident={id} — there is no incident page).
 */
function OpenIncidents() {
  const { me, can } = useAuth();
  const { available, resolved } = useDatabaseAvailable();
  const { at } = useTimeContext();
  const canRead = can("incidents:read");
  const enabled = me !== undefined && canRead && resolved && available;

  /* Engaged, "open" is the wrong question to ask this endpoint. `status` is a
     NOW fact (it is resolved_at's witness), so an incident that was ongoing at
     t and has since been resolved would be filtered out of a view of t.
     ListIncidents' from/to bound the window an incident's OWN RANGE must
     overlap — from_at < to AND coalesce(to_at,'infinity') >= from — so the
     one-second window [t, t+1s) selects exactly the incidents whose range
     covers t, which is what "open as of t" means. The Time Machine's own
     precision is the second, so that window is the finest honest one. */
  const query = useQuery({
    queryKey: at ? ["overview", "incidents", "at", at.toISOString()] : ["overview", "incidents"],
    queryFn: () =>
      getIncidents(
        at
          ? { from: at, to: new Date(at.getTime() + 1000), limit: OPEN_INCIDENTS_LIMIT }
          : { status: "open", limit: OPEN_INCIDENTS_LIMIT },
      ),
    enabled,
  });
  const incidents = query.data?.incidents ?? [];
  const now = new Date();

  return (
    <Card asChild className="p-6">
      <section aria-label="Open incidents">
        <h2 className="text-sm font-semibold">Open incidents</h2>

        {me !== undefined && !canRead ? (
          <PanelNote>Open incidents need incidents:read — none was requested.</PanelNote>
        ) : resolved && !available ? (
          <PanelNote>{DB_NOTE}</PanelNote>
        ) : query.isError ? (
          <PanelNote>The incident list is unavailable right now.</PanelNote>
        ) : !enabled || query.isLoading ? (
          <PanelSkeleton rows={3} />
        ) : incidents.length === 0 ? (
          <PanelNote>No open incidents. Saving an investigation on /investigate opens one.</PanelNote>
        ) : (
          <ul className="mt-3 flex flex-col divide-y divide-border">
            {incidents.map((i) => (
              <li key={i.id} data-testid="open-incident" className="flex items-center gap-3 py-2">
                <a href={incidentPermalink(i.id)} className="min-w-0 flex-1 truncate text-sm text-primary hover:underline">
                  {i.title}
                </a>
                <Badge variant="neutral">{i.scope === "" ? "global" : i.scope}</Badge>
                <span className="nums w-10 shrink-0 text-right text-xs text-muted-foreground">
                  {fmtAge(i.fromAt, now)}
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

/* The Live feed's own wording, kept identical here on purpose (QA round 1,
   finding #14): the same event was "warn" on this card and "Warn" on /live,
   which reads as two vocabularies for one fact. Live's capitalized form wins
   because it is the one an operator spends the most time in front of. An
   unknown severity is still printed raw — Go's field is an open string. */
const SEVERITY_LABELS: Record<LiveEventSeverity, string> = {
  info: "Info",
  warn: "Warn",
  error: "Error",
};

/**
 * OverviewEventRow is a deliberately MINIMAL copy of pages/live.tsx's EventRow
 * (the seam: live.tsx does not export it, and exporting a row built for a
 * full-width virtualised feed into a half-width summary card would drag its
 * five fixed-width columns along).
 *
 * Same vocabulary — clock, severity badge, summary, scope — in the space this
 * card actually has. If the Live row ever becomes exported and fluid, this is
 * the thing to delete.
 */
function OverviewEventRow({ event }: { event: LiveEvent }) {
  return (
    <li data-testid="overview-event" className="flex items-center gap-3 py-2">
      <span className="nums w-16 shrink-0 text-xs text-muted-foreground">{fmtEventTime(event.timestamp)}</span>
      <Badge variant={isKnownSeverity(event.severity) ? SEVERITY_VARIANT[event.severity] : "unknown"} dot>
        {isKnownSeverity(event.severity) ? SEVERITY_LABELS[event.severity] : event.severity}
      </Badge>
      <span className="min-w-0 flex-1 truncate text-sm" title={event.summary}>
        {event.summary}
      </span>
      <span className="hidden w-36 shrink-0 truncate text-xs text-muted-foreground sm:block" title={event.scope}>
        {event.scope}
      </span>
    </li>
  );
}

/** RecentEvents finally wires the events API M3 shipped: the newest ten, and a
 *  link to the page that streams them live rather than a poll on this one. */
function RecentEvents() {
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
  });
  const events = query.data?.events ?? [];

  return (
    <Card asChild className="p-6">
      <section aria-label="Recent events">
        <div className="flex flex-wrap items-baseline justify-between gap-2">
          <h2 className="text-sm font-semibold">Recent events</h2>
          <a href="/live" className="text-xs text-primary hover:underline">
            open Live
          </a>
        </div>

        {me !== undefined && !canRead ? (
          <PanelNote>Fleet events need events:read — none was requested.</PanelNote>
        ) : resolved && !available ? (
          <PanelNote>{DB_NOTE}</PanelNote>
        ) : query.isError ? (
          <PanelNote>The event feed is unavailable right now.</PanelNote>
        ) : !enabled || query.isLoading ? (
          <PanelSkeleton rows={4} />
        ) : events.length === 0 ? (
          <PanelNote>Nothing has happened yet. Agent restarts, node readiness changes and MTR triggers land here.</PanelNote>
        ) : (
          <ul className="mt-3 flex flex-col divide-y divide-border">
            {events.map((e) => (
              <OverviewEventRow key={e.id} event={e} />
            ))}
          </ul>
        )}
      </section>
    </Card>
  );
}

/* ── M7 Task 8: Firing alerts (plan Decision 6) ──────────────────────────────
   The card that replaced the "arrives with a later milestone" placeholder. It
   is the operator's morning view, which is why it asks for the WHOLE fleet's
   firing state rather than only the rules this console manages: an alert
   somebody else wrote is still firing in this cluster, and a console that hid
   it would make its own silence mean less than it does. Foreign rows are shown
   and TAGGED — displayed, never claimed. */

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
 * sortFiringAlerts is the card's order: severity first, then the OLDEST
 * firing first inside a severity band.
 *
 * Oldest-first is the deliberate half. "Newest first" is the feed instinct, but
 * this is not a feed: the alert that has been critical for three hours is the
 * one nobody has dealt with, and it is exactly the one a newest-first list
 * pushes off the bottom of a truncated card.
 *
 * An unrecognised severity sorts LAST rather than being dropped or guessed at:
 * a foreign rule may label anything at all, and the console does not know what
 * somebody else's word ranks as. The order is total (severity, then start, then
 * name) so two renders of the same firing set never disagree, and the input is
 * not mutated.
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

function alertLabelLine(labels: Record<string, string>): string {
  return Object.keys(labels)
    .sort()
    .map((k) => `${k}=${labels[k]}`)
    .join(" ");
}

/** One firing row. The label set travels in a `title` attribute on a truncated
 *  line — the worst-pairs table's own idiom for detail that will not fit. */
function FiringAlertRow({ alert, now }: { alert: Alert; now: Date }) {
  const managed = alert.ruleId !== undefined && alert.ruleId !== "";
  const scope = scopeFromAlertLabels(alert.labels);
  const labels = alertLabelLine(alert.labels);

  return (
    <li data-testid="firing-alert" className="flex flex-wrap items-baseline gap-x-3 gap-y-1 py-2">
      {managed ? (
        // ?rule= rather than a bare /alerting: the list can be long and the row
        // an operator is chasing is one of many, so the link names it and the
        // page opens it (pages/alerting.tsx reads the param). Same idiom as
        // /investigate?incident=.
        <a
          href={`/alerting?rule=${encodeURIComponent(alert.ruleId as string)}`}
          data-testid="firing-alert-name"
          className="min-w-0 flex-1 truncate text-sm text-primary hover:underline"
        >
          {alert.name}
        </a>
      ) : (
        <span data-testid="firing-alert-name" className="min-w-0 flex-1 truncate text-sm" title={alert.name}>
          {alert.name}
        </span>
      )}
      <Badge variant={ALERT_SEVERITY_TONE[alert.severity] ?? "unknown"} dot>
        {alert.severity === "" ? "no severity" : alert.severity}
      </Badge>
      {managed ? null : (
        // Not a warning, a FACT: this console does not own the rule, so it
        // offers no edit path to it and says why the link is missing.
        <Badge variant="neutral" title="This console does not manage the rule behind this alert.">
          unmanaged
        </Badge>
      )}
      <span className="nums w-10 shrink-0 text-right text-xs text-muted-foreground">
        {alert.activeAt === undefined ? "—" : fmtAge(alert.activeAt, now)}
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
          investigate
        </a>
      ) : null}
    </li>
  );
}

/**
 * FiringAlerts reads GET /api/v1/alerts, whose degraded shape is the reason
 * this card can be honest at all: `promConfigured` rides IN the 200 body, so
 * "nothing is firing" and "nobody is watching" are two different sentences
 * here rather than one empty list. A 502 is neither — Prometheus is wired and
 * did not answer — and it is surfaced with the server's own detail, because
 * rendering a failing evaluator as a quiet fleet is the worst lie this page
 * could tell.
 *
 * No database gating: the firing set is Prometheus's, not the store's.
 */
function FiringAlerts() {
  const { me, can } = useAuth();
  const { at } = useTimeContext();
  const engaged = at !== null;
  const canRead = can("alerts:read");
  /* Engaged this card asks for NOTHING. /api/v1/alerts is Prometheus's ACTIVE
     alert set — a now-only signal by design, with no history behind it — so
     the only two things this card could do at t are lie (render now's firing
     set under a past instant) or say so. It says so, and the request is not
     made at all rather than made and discarded. */
  const enabled = me !== undefined && canRead && !engaged;

  const query = useQuery({ queryKey: ["overview", "alerts"], queryFn: listAlerts, enabled });
  const now = new Date();
  /* PENDING is not firing. The route serves both states (a rule's `for` window
     is exactly the gap between them) and a card called "Firing alerts" that
     listed pending rows would be claiming a page that has not happened — the
     same line the webhook contract draws, and the same one the /investigate
     source draws. A response carrying only pending alerts therefore renders
     "Nothing is firing", which is true. */
  const alerts = useMemo(
    () => sortFiringAlerts((query.data?.alerts ?? []).filter((a) => a.state === "firing")),
    [query.data],
  );
  const shown = alerts.slice(0, FIRING_ALERTS_LIMIT);
  const hidden = alerts.length - shown.length;

  return (
    <Card asChild className="p-6">
      <section aria-label="Firing alerts">
        <div className="flex flex-wrap items-baseline justify-between gap-2">
          <h2 className="text-sm font-semibold">Firing alerts</h2>
          {canRead ? (
            <a href="/alerting" className="text-xs text-primary hover:underline">
              open Alerting
            </a>
          ) : null}
        </div>

        {engaged ? (
          <PanelNote>Alert state is a live-only signal — Prometheus keeps no firing history here.</PanelNote>
        ) : me !== undefined && !canRead ? (
          <PanelNote>Firing alerts need alerts:read — none was requested.</PanelNote>
        ) : query.isError ? (
          <PanelNote>
            {query.error instanceof Error ? query.error.message : "The firing set is unavailable right now."}
          </PanelNote>
        ) : !enabled || query.isLoading ? (
          <PanelSkeleton rows={3} />
        ) : query.data?.promConfigured === false ? (
          <PanelNote>
            Prometheus is not configured for this console — set console.prometheus.address. There is no firing state to
            show.
          </PanelNote>
        ) : alerts.length === 0 ? (
          <PanelNote>Nothing is firing. Rules live on /alerting; Prometheus evaluates them.</PanelNote>
        ) : (
          <>
            <ul className="mt-3 flex flex-col divide-y divide-border">
              {shown.map((a) => (
                <FiringAlertRow key={`${a.name}{${alertLabelLine(a.labels)}}`} alert={a} now={now} />
              ))}
            </ul>
            {hidden > 0 ? (
              <PanelNote>
                {hidden} more firing alert{hidden === 1 ? " is" : "s are"} not shown here.
              </PanelNote>
            ) : null}
          </>
        )}
      </section>
    </Card>
  );
}

function WorstPairsTable({ pairs }: { pairs: MatrixCell[] }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <caption className="sr-only">Worst pairs by failure ratio</caption>
        <thead>
          <tr className="border-b border-border text-left text-[11px] uppercase tracking-[0.07em] text-muted-foreground">
            <th scope="col" className="w-10 py-3 pr-4 font-semibold">
              #
            </th>
            <th scope="col" className="py-3 pr-6 font-semibold">
              Pair
            </th>
            <th scope="col" className="py-3 pr-6 text-right font-semibold">
              Fail %
            </th>
            <th scope="col" className="py-3 pr-6 text-right font-semibold">
              p95 RTT
            </th>
            <th scope="col" className="py-3 font-semibold">
              Status
            </th>
          </tr>
        </thead>
        <tbody className="divide-y divide-border">
          {pairs.map((c, i) => {
            const fail = c.failRatio ?? 0;
            const failing = fail >= 0.1;
            return (
              <tr
                key={`${c.source} ${c.destination}`}
                className="transition-colors duration-(--dur) ease-(--ease) hover:bg-accent/40"
              >
                <td className="nums py-4 pr-4 text-xs text-muted-foreground">{i + 1}</td>
                <td className="max-w-[22rem] py-4 pr-6">
                  <span className="flex items-center gap-2">
                    <span className="truncate" title={c.source}>
                      {c.source}
                    </span>
                    <span aria-hidden="true" className="shrink-0 text-muted-foreground">
                      →
                    </span>
                    <span className="truncate" title={c.destination}>
                      {c.destination}
                    </span>
                  </span>
                </td>
                <td
                  className={cn(
                    "nums py-4 pr-6 text-right text-base font-semibold tracking-tight",
                    failing ? "text-health-bad" : "text-health-warn",
                  )}
                >
                  {(100 * fail).toFixed(1)}%
                </td>
                <td className="nums py-4 pr-6 text-right text-muted-foreground">{fmtRtt(c.rttP95)}</td>
                <td className="py-4">
                  <Badge variant={failing ? "bad" : "warn"} dot>
                    {failing ? "Failing" : "Degraded"}
                  </Badge>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

/**
 * PageProblem is one failed dependency, said in its own sentence.
 *
 * The page used to surface `matrix.error ?? topo.error` — one slot for two
 * independent queries — so with both down the topology detail was silently
 * dropped, and the historical fold's 422 (the one that names
 * console.database.retentionDays, i.e. the only actionable one) was exactly
 * the message a reader lost while the NODES READY tile quietly showed an
 * em-dash (QA round 1, finding #5). Two failures are two facts and get two
 * lines, each carrying the server's own detail: ApiError's message IS
 * problem.detail, so the retention sentence arrives verbatim.
 */
function PageProblem({ what, error }: { what: string; error: Error }) {
  return (
    <div data-testid="overview-problem">
      <p className="text-sm font-medium">{what}</p>
      <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{error.message}</p>
    </div>
  );
}

export function OverviewPage() {
  const topo = useTopology();
  const matrix = useMatrix("tcp");
  const { isLive } = useTimeContext();

  const summary = matrix.data ? summarize(matrix.data, topo.data) : undefined;
  // summarize()'s topology-absent fallback (readyNodes = totalNodes) is meant
  // for "no data at all" — it must not be read as "all nodes ready" while the
  // topology query is still in flight or has genuinely errored (e.g. the
  // controller isn't configured, per Task 11). The tile shows an explicit
  // loading/unknown state in those cases instead of a silently optimistic count.
  const nodesReadyDisplay = topo.data
    ? `${summary?.readyNodes ?? 0}/${summary?.totalNodes ?? 0}`
    : topo.isLoading
      ? "…"
      : "—";

  return (
    <PageShell
      title="Overview"
      description="Cluster health at a glance, recomputed from Prometheus every 15s."
    >
      <div className="flex flex-col gap-6">
        {matrix.error || topo.error ? (
          <Card role="alert" className="flex flex-col gap-3 border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
            {matrix.error ? <PageProblem what="The pair matrix is unavailable" error={matrix.error} /> : null}
            {topo.error ? <PageProblem what="The node list is unavailable" error={topo.error} /> : null}
            {/* Only true while Live: engaged, both queries have their poll off
                on purpose (a past instant's answer cannot change), so promising
                a retry that is never going to happen would be a second lie
                stacked on the first. */}
            {isLive ? (
              <p className="text-xs leading-relaxed text-muted-foreground">
                The page keeps retrying every 15s. If it persists, check that the console can reach Prometheus.
              </p>
            ) : null}
          </Card>
        ) : null}

        {!summary && matrix.isLoading ? <OverviewSkeleton /> : null}

        {summary ? (
          <>
            <div className="grid gap-4 sm:grid-cols-3">
              <StatTile
                label="Nodes ready"
                value={nodesReadyDisplay}
                hint={topo.data ? undefined : "Topology unavailable"}
              />
              {/* Both pair tiles carry the qualifier: they count ONE protocol
                  on ONE plane, and the bare label claimed the whole fleet. */}
              <StatTile
                label="Failing pairs"
                value={summary.pairsFailing}
                tone={summary.pairsFailing > 0 ? "bad" : undefined}
                toneLabel="Fail ≥ 10%"
                hint={MATRIX_QUALIFIER}
              />
              <StatTile
                label="Degraded pairs"
                value={summary.pairsDegraded}
                tone={summary.pairsDegraded > 0 ? "warn" : undefined}
                toneLabel="Fail 1–10%"
                hint={MATRIX_QUALIFIER}
              />
            </div>

            <Card asChild className="p-6">
              <section>
                <div className="flex flex-wrap items-baseline justify-between gap-2">
                  <span className="flex flex-wrap items-baseline gap-2">
                    <h2 className="text-sm font-semibold">Worst pairs</h2>
                    <Badge variant="neutral">{MATRIX_QUALIFIER}</Badge>
                  </span>
                  <p className="nums text-xs text-muted-foreground">
                    {summary.pairsTotal} measured pair{summary.pairsTotal === 1 ? "" : "s"}
                  </p>
                </div>
                {summary.worstPairs.length === 0 ? (
                  summary.pairsTotal === 0 ? (
                    <BlankSlate
                      title="No probe data in Prometheus yet"
                      body="Pairs appear here once the agents have completed a probe round and Prometheus has scraped them — usually within a minute of the DaemonSet becoming ready."
                    />
                  ) : summary.pairsScored === 0 ? (
                    // Measured, but not RANKABLE: latency arrived and the
                    // failure-ratio series did not, so "nothing is failing" is
                    // a claim this page has no samples for. Naming the missing
                    // half is the honest half of finding #3.
                    <BlankSlate
                      title="No failure ratio for these pairs"
                      body={`${summary.pairsTotal} pair${summary.pairsTotal === 1 ? " is" : "s are"} reporting latency, but the failure-ratio series has no samples here — worst-first ranking needs it, so this list stays empty rather than reading as healthy.`}
                    />
                  ) : (
                    <BlankSlate
                      title="No failing or degraded pairs"
                      body="Every scored pair is under a 1% failure ratio. Anything that crosses that line shows up here, worst first."
                    />
                  )
                ) : (
                  <div className="mt-4">
                    <WorstPairsTable pairs={summary.worstPairs} />
                  </div>
                )}
              </section>
            </Card>

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
