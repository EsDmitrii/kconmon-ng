import type { ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { PageShell } from "@/components/page-shell";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { useAuth } from "@/hooks/use-auth";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import { useMatrix } from "@/hooks/use-matrix";
import { useTopology } from "@/hooks/use-topology";
import { getEvents, getIncidents } from "@/lib/api";
import { incidentPermalink } from "@/lib/investigation-sources";
import type { LiveEvent, LiveEventSeverity, Matrix, MatrixCell, Topology } from "@/lib/types";
import { cn } from "@/lib/utils";

export interface OverviewSummary {
  totalNodes: number;
  readyNodes: number;
  pairsTotal: number;
  pairsFailing: number;
  pairsDegraded: number;
  worstPairs: MatrixCell[]; // top 5 by failRatio desc, failing/degraded only
}

// Health tiers mirror the matrix/topology thresholds: fail ≥ 10% is "failing",
// 1%–10% is "degraded". Cells with a null failRatio have no probe data and are
// excluded from every count so an unmeasured pair never reads as healthy.
export function summarize(matrix: Matrix, topo?: Topology): OverviewSummary {
  const scored = matrix.cells.filter((c) => c.failRatio !== null);
  const failing = scored.filter((c) => (c.failRatio ?? 0) >= 0.1);
  const degraded = scored.filter((c) => (c.failRatio ?? 0) >= 0.01 && (c.failRatio ?? 0) < 0.1);
  return {
    totalNodes: topo?.nodes.length ?? matrix.nodes.length,
    readyNodes: topo ? topo.nodes.filter((n) => n.ready).length : matrix.nodes.length,
    pairsTotal: scored.length,
    pairsFailing: failing.length,
    pairsDegraded: degraded.length,
    worstPairs: [...failing, ...degraded]
      .sort((a, b) => (b.failRatio ?? 0) - (a.failRatio ?? 0))
      .slice(0, 5),
  };
}

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

// A section whose data plane lands in a later milestone (firing alerts →
// alerting, M7). Rendered as an honest placeholder rather than fabricated rows.
// "Recent events" and "Open incidents" left this shape in M6 — both now read
// real APIs below.
function LaterMilestone({ title, note }: { title: string; note: string }) {
  return (
    <Card asChild className="p-6">
      <section>
        <h2 className="text-sm font-semibold">{title}</h2>
        <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{note}</p>
      </section>
    </Card>
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
  const canRead = can("incidents:read");
  const enabled = me !== undefined && canRead && resolved && available;

  const query = useQuery({
    queryKey: ["overview", "incidents"],
    queryFn: () => getIncidents({ status: "open", limit: OPEN_INCIDENTS_LIMIT }),
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

function fmtEventTime(timestamp: string): string {
  const d = new Date(timestamp);
  return Number.isNaN(d.getTime()) ? timestamp : d.toLocaleTimeString();
}

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
        {event.severity}
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
  const canRead = can("events:read");
  const enabled = me !== undefined && canRead && resolved && available;

  const query = useQuery({
    queryKey: ["overview", "events"],
    queryFn: () => getEvents({ limit: RECENT_EVENTS_LIMIT }),
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

export function OverviewPage() {
  const topo = useTopology();
  const matrix = useMatrix("tcp");

  const error = matrix.error ?? topo.error;
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
        {error ? (
          <Card
            role="alert"
            className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5"
          >
            <p className="text-sm font-medium">Overview data is unavailable</p>
            <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{error.message}</p>
            <p className="mt-2 text-xs leading-relaxed text-muted-foreground">
              The page keeps retrying every 15s. If it persists, check that the console can reach
              Prometheus.
            </p>
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
              <StatTile
                label="Failing pairs"
                value={summary.pairsFailing}
                tone={summary.pairsFailing > 0 ? "bad" : undefined}
                toneLabel="Fail ≥ 10%"
              />
              <StatTile
                label="Degraded pairs"
                value={summary.pairsDegraded}
                tone={summary.pairsDegraded > 0 ? "warn" : undefined}
                toneLabel="Fail 1–10%"
              />
            </div>

            <Card asChild className="p-6">
              <section>
                <div className="flex flex-wrap items-baseline justify-between gap-2">
                  <h2 className="text-sm font-semibold">Worst pairs</h2>
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
                  ) : (
                    <BlankSlate
                      title="No failing or degraded pairs"
                      body="Every measured pair is under a 1% failure ratio. Anything that crosses that line shows up here, worst first."
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
              {/* Still a placeholder, and deliberately so: nothing evaluates
                  alert rules until M7, and a fabricated "0 firing" would read
                  as a quiet fleet rather than as a missing engine. */}
              <LaterMilestone
                title="Firing alerts"
                note="Arrives with a later milestone (Alertmanager wiring)."
              />
              <OpenIncidents />
            </div>

            <RecentEvents />
          </>
        ) : null}
      </div>
    </PageShell>
  );
}
