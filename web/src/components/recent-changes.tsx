import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import { getWsClient } from "@/hooks/use-ws-topic";
import { ApiError, getEvents } from "@/lib/api";
import type { LiveEvent, LiveEventSeverity } from "@/lib/types";
import { TOPIC_LIVE, type WsEnvelope } from "@/lib/ws";
// Reuses the Live page's own merge/dedupe store rather than re-implementing
// it: pushEvents is exported from pages/live.tsx precisely so a second
// consumer of the same LiveEvent stream (this rail) shares its ordering
// (timestamp, seq) and its id-based dedupe across a reconnect replay,
// instead of drifting from it.
import { pushEvents } from "@/pages/live";
import { Badge } from "./ui/badge";
import { Card } from "./ui/card";
import { Skeleton } from "./ui/skeleton";

/** GET /api/v1/events page size for the rail — task-25-brief.md's own number. */
export const RECENT_CHANGES_LIMIT = 50;

// A cap on the merged (history + live) ring. Generous enough that a card left
// open through a busy incident does not grow without bound, but far below the
// Live page's 2000-row budget (PAGES.md §7.8) — this is a narrow rail on an
// object card, not the primary feed.
export const RECENT_CHANGES_CAP = 200;

const SEVERITY_VARIANT: Record<LiveEventSeverity, "neutral" | "warn" | "bad"> = {
  info: "neutral",
  warn: "warn",
  error: "bad",
};

function isKnownSeverity(v: string): v is LiveEventSeverity {
  return v === "info" || v === "warn" || v === "error";
}

function fmtTime(timestamp: string): string {
  const d = new Date(timestamp);
  return Number.isNaN(d.getTime()) ? timestamp : d.toLocaleTimeString();
}

/** mergeCapped is pushEvents plus RECENT_CHANGES_CAP, preserving pushEvents'
 * "return prev unchanged when nothing new arrived" identity so an unrelated
 * live event never triggers a re-render of this rail. */
function mergeCapped(prev: LiveEvent[], incoming: LiveEvent[]): LiveEvent[] {
  const merged = pushEvents(prev, incoming);
  if (merged === prev) return prev;
  return merged.length > RECENT_CHANGES_CAP ? merged.slice(0, RECENT_CHANGES_CAP) : merged;
}

/**
 * RecentChanges is the shared right rail every M3 object card (Node, Pair)
 * mounts, per PAGES.md §6.4. It is parameterised only by `scope` — the exact
 * string events.LiveEvent.Scope carries for this object
 * (internal/console/events/live_event.go): a bare node name for a node card,
 * or "<source>→<destination>" (U+2192 — pairScope's own separator, NOT a
 * hyphen-arrow) for a pair card. Getting that string wrong yields a silently
 * empty rail — there is no error state for "scope matched nothing" — so
 * callers must build it exactly the way the controller does.
 *
 * Two sources feed the same ring: GET /api/v1/events?scope=...&limit=50 for
 * history, and the `live` WebSocket topic (filtered to an EXACT scope match,
 * unlike the Live page's own case-insensitive substring filter — a card is
 * pinned to one precise object, not a search) for real-time updates while the
 * card stays open. Both merge through pushEvents' id-based dedupe, so an
 * event the history page already returned and one this tab later sees live
 * collapse into a single row rather than appearing twice.
 */
export function RecentChanges({ scope }: { scope: string }) {
  const { available: dbAvailable, resolved: dbResolved } = useDatabaseAvailable();
  const [events, setEvents] = useState<LiveEvent[]>([]);

  // Gated on dbResolved too, not just dbAvailable: a cold /api/v1/config must
  // not be read as "no database" (which would skip the fetch this exists to
  // make) the same way useDatabaseAvailable's own doc comment warns about.
  const historyEnabled = dbResolved && dbAvailable && scope !== "";
  const historyQuery = useQuery({
    queryKey: ["events", scope],
    queryFn: () => getEvents({ scope, limit: RECENT_CHANGES_LIMIT }),
    enabled: historyEnabled,
  });

  // A scope change (a different node/pair card mounted in place of this one)
  // must not carry over the previous object's history for even one render.
  useEffect(() => {
    setEvents([]);
  }, [scope]);

  useEffect(() => {
    if (historyQuery.data) setEvents((prev) => mergeCapped(prev, historyQuery.data.events));
  }, [historyQuery.data]);

  // Subscribed unconditionally, same as the Live page: another console
  // replica's events still arrive over the Valkey bus even while this
  // replica's own ingester is down, and holding the socket open costs
  // nothing extra (it is already page-wide, ADR-003).
  useEffect(() => {
    if (scope === "") return;
    const ws = getWsClient();
    const off = ws.subscribe<LiveEvent>(TOPIC_LIVE, (env: WsEnvelope<LiveEvent>) => {
      if (env.type !== "event") return;
      if (env.data.scope !== scope) return;
      setEvents((prev) => mergeCapped(prev, [env.data]));
    });
    return () => off();
  }, [scope]);

  const historyError = historyQuery.isError
    ? historyQuery.error instanceof ApiError
      ? (historyQuery.error.problem.detail ?? historyQuery.error.problem.title)
      : "Event history is unavailable"
    : null;

  // "Loading" spans two waits, not one: whether this replica even has a
  // database (dbResolved) comes back before the events page itself does, and
  // showing "No recent changes" in between would flash an empty state that a
  // moment later flips to a loading skeleton once the history fetch actually
  // starts — see recent-changes.test.tsx's different-scope live-event case,
  // which caught exactly that flicker.
  const waitingOnHistory = !dbResolved || (historyEnabled && historyQuery.isLoading);
  const loadingFirstPage = waitingOnHistory && events.length === 0;

  return (
    <Card asChild className="flex max-h-[calc(100vh-13rem)] flex-col gap-0 overflow-hidden p-0">
      <aside aria-label="Recent changes" className="flex flex-col">
        <div className="border-b border-border px-4 py-3">
          <h2 className="text-sm font-semibold">Recent changes</h2>
        </div>

        {/* Degraded, not broken: no database means no scrollback, but the
            live half of this rail keeps working off the socket regardless. */}
        {dbResolved && !dbAvailable ? (
          <p role="status" className="border-b border-border px-4 py-2 text-xs leading-relaxed text-muted-foreground">
            History requires a database — showing live events only.
          </p>
        ) : null}

        {historyError ? (
          <p role="status" className="border-b border-border px-4 py-2 text-xs leading-relaxed text-muted-foreground">
            {historyError}
          </p>
        ) : null}

        {loadingFirstPage ? (
          <div role="status" aria-live="polite" className="flex flex-col gap-2 p-4">
            <span className="sr-only">Loading recent changes…</span>
            {Array.from({ length: 5 }, (_, i) => (
              <Skeleton key={i} className="h-9 w-full" />
            ))}
          </div>
        ) : null}

        {!loadingFirstPage && events.length === 0 ? (
          <p className="px-4 py-10 text-center text-xs text-muted-foreground">No recent changes.</p>
        ) : null}

        {events.length > 0 ? (
          <ul className="flex flex-col divide-y divide-border overflow-y-auto">
            {events.map((e) => (
              <li key={e.id} className="flex flex-col gap-1 px-4 py-2.5">
                <div className="flex items-center justify-between gap-2">
                  <span className="nums text-[11px] text-muted-foreground">{fmtTime(e.timestamp)}</span>
                  <Badge variant={isKnownSeverity(e.severity) ? SEVERITY_VARIANT[e.severity] : "unknown"} dot>
                    {e.severity}
                  </Badge>
                </div>
                <p className="text-xs leading-snug text-foreground">{e.summary}</p>
              </li>
            ))}
          </ul>
        ) : null}
      </aside>
    </Card>
  );
}
