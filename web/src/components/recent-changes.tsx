import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import { getWsClient } from "@/hooks/use-ws-topic";
import { ApiError, getEvents } from "@/lib/api";
import { useTimeContext } from "@/lib/timemachine";
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

/** The separator events.pairScope writes between the two halves of a pair
 * scope (internal/console/events/live_event.go) — U+2192 RIGHTWARDS ARROW, NOT
 * a hyphen-arrow. Named rather than inlined so the one place this glyph is
 * written client-side is the one place to check it against the Go writer. */
const PAIR_SEPARATOR = "→";

/** matchesScope is the client-side twin of the server's two events filters, so
 * the live half of this rail admits exactly the rows the history half returned:
 * `scope` is equality (`?scope=`), `scopeNode` is "this name on either side of
 * a pair scope" (`?scopeNode=`, store.EventFilter.ScopeNode). Splitting on the
 * FIRST separator and comparing whole halves keeps it an exact match — a
 * substring test would let "node-a" claim "node-ax→b". */
export function matchesScope(eventScope: string, exact: string, node: string): boolean {
  if (node !== "") {
    if (eventScope === node) return true;
    const i = eventScope.indexOf(PAIR_SEPARATOR);
    return i >= 0 && (eventScope.slice(0, i) === node || eventScope.slice(i + PAIR_SEPARATOR.length) === node);
  }
  return eventScope === exact;
}

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
 * RecentChanges is the shared right rail every M3 object card (Node, Pair,
 * Target) mounts, per PAGES.md §6.4. Exactly ONE of its two props says which
 * events belong to this object, mirroring the two mutually-exclusive server
 * filters (GET /api/v1/events answers 422 if both arrive):
 *
 *   - `scope` — equality on events.LiveEvent.Scope
 *     (internal/console/events/live_event.go). What a PAIR card wants:
 *     "<source>→<destination>" (U+2192 — pairScope's own separator, NOT a
 *     hyphen-arrow) names one edge and nothing else.
 *   - `scopeNode` — a node/target NAME matched on either side of the scope.
 *     What an OBJECT card wants: a node takes part in pair-scoped rows
 *     ("node-a→node-b" — every check run, every path change) that an equality
 *     filter on its own name structurally cannot see. Before this existed the
 *     node card's rail silently dropped all of them (QA scope 2 #21).
 *
 * Getting the string wrong yields a silently empty rail — there is no error
 * state for "nothing matched" — so callers must build it exactly the way the
 * controller does.
 *
 * Two sources feed the same ring: GET /api/v1/events?…&limit=50 for history,
 * and the `live` WebSocket topic for real-time updates while the card stays
 * open. The socket half is filtered through matchesScope with the SAME two
 * props, so a live arrival and a history row are admitted by one rule — a
 * narrower live filter would make an event appear only after a reload. (Still
 * exact, unlike the Live page's case-insensitive substring search: a card is
 * pinned to one precise object.) Both merge through pushEvents' id-based
 * dedupe, so an event the history page already returned and one this tab later
 * sees live collapse into a single row rather than appearing twice.
 */
export type RecentChangesProps =
  | { scope: string; scopeNode?: undefined }
  | { scope?: undefined; scopeNode: string };

export function RecentChanges({ scope = "", scopeNode = "" }: RecentChangesProps) {
  const { available: dbAvailable, resolved: dbResolved } = useDatabaseAvailable();
  const { at } = useTimeContext();
  const atKey = at ? at.toISOString() : "";
  const [events, setEvents] = useState<LiveEvent[]>([]);

  // The identity of "which object is this rail pinned to" — the two props are
  // exclusive, so one of them is it. The filter name rides in the query key
  // alongside the value: ?scope=node-a and ?scopeNode=node-a are different
  // questions with the same argument, and caching one answer under the other
  // would hand a pair card a node card's rows.
  const filterName = scopeNode !== "" ? "scopeNode" : "scope";
  const filterValue = scopeNode !== "" ? scopeNode : scope;

  // Gated on dbResolved too, not just dbAvailable: a cold /api/v1/config must
  // not be read as "no database" (which would skip the fetch this exists to
  // make) the same way useDatabaseAvailable's own doc comment warns about.
  const historyEnabled = dbResolved && dbAvailable && filterValue !== "";
  const historyQuery = useQuery({
    // `to` bounds the rail to the Time Machine's instant (Task 11): "Recent
    // changes" on a card showing state-as-of-t means the changes up to t, not
    // everything that has happened since. The bound is EXCLUSIVE server-side
    // (store.EventFilter.To), and `at` carries seconds precision, so an event
    // stamped exactly at t belongs to the next second's view — the same edge
    // the annotations store already documents.
    queryKey: at
      ? ["events", filterName, filterValue, "to", at.toISOString()]
      : ["events", filterName, filterValue],
    queryFn: () =>
      getEvents({ [filterName]: filterValue, limit: RECENT_CHANGES_LIMIT, ...(at ? { to: at } : {}) }),
    enabled: historyEnabled,
  });

  // A scope change (a different node/pair card mounted in place of this one)
  // must not carry over the previous object's history for even one render.
  // Moving through time is the same kind of change for the same reason: rows
  // from after t are exactly what this rail must not be showing.
  useEffect(() => {
    setEvents([]);
  }, [filterName, filterValue, atKey]);

  useEffect(() => {
    if (historyQuery.data) setEvents((prev) => mergeCapped(prev, historyQuery.data.events));
  }, [historyQuery.data]);

  // Subscribed unconditionally, same as the Live page: another console
  // replica's events still arrive over the Valkey bus even while this
  // replica's own ingester is down, and holding the socket open costs
  // nothing extra (it is already page-wide, ADR-003).
  // ... EXCEPT while the Time Machine is engaged. A live arrival is by
  // definition after t, so subscribing at all would let the present trickle
  // into a rail whose whole claim is "up to t". The socket itself is page-wide
  // and stays open; this rail simply stops listening until Live returns.
  useEffect(() => {
    if (filterValue === "" || at !== null) return;
    const ws = getWsClient();
    const off = ws.subscribe<LiveEvent>(TOPIC_LIVE, (env: WsEnvelope<LiveEvent>) => {
      if (env.type !== "event") return;
      if (!matchesScope(env.data.scope, scope, scopeNode)) return;
      setEvents((prev) => mergeCapped(prev, [env.data]));
    });
    return () => off();
  }, [scope, scopeNode, filterValue, at]);

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
          {/* The rail says what it is bounded BY, right where the rows are —
              the top-bar banner explains the mode, this states the cut. */}
          {at ? <p className="mt-0.5 text-[11px] text-muted-foreground">up to {at.toLocaleString()}</p> : null}
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
