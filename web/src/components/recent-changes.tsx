import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import { getWsClient } from "@/hooks/use-ws-topic";
import { getEvents, queryErrorMessage } from "@/lib/api";
import { localeTag, stampFull, useLocale, useT } from "@/lib/i18n";
import { recentChangesDict } from "@/lib/i18n/dict/recent-changes";
import { useTimeContext } from "@/lib/timemachine";
import type { LiveEvent, LiveEventSeverity } from "@/lib/types";
import { cn, fmtEventStamp } from "@/lib/utils";
import { TOPIC_LIVE, type WsEnvelope } from "@/lib/ws";
// Reuses the Live page's own merge/dedupe store rather than re-implementing it.
import { pushEvents } from "@/lib/live-events";
import { Badge } from "./ui/badge";
import { Card } from "./ui/card";
import { EmptyState } from "./ui/empty-state";
import { scrollRegionClass } from "./ui/scroll-region";
import { Skeleton } from "./ui/skeleton";

/** GET /api/v1/events page size for the rail. */
export const RECENT_CHANGES_LIMIT = 50;

// A cap on the merged (history + live) ring.
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

/* Stamps go through lib/utils' fmtEventStamp, the Live feed's own idiom (hour12:false, plus the day
   for a row that is not from today), so one event reads the same in both places. */

/** mergeCapped is pushEvents plus RECENT_CHANGES_CAP, preserving pushEvents'
 * "return prev unchanged when nothing new arrived" identity so an unrelated
 * live event never triggers a re-render of this rail. */
function mergeCapped(prev: LiveEvent[], incoming: LiveEvent[]): LiveEvent[] {
  const merged = pushEvents(prev, incoming);
  if (merged === prev) return prev;
  return merged.length > RECENT_CHANGES_CAP ? merged.slice(0, RECENT_CHANGES_CAP) : merged;
}

/** RecentChanges is the shared right rail; getting the string wrong yields a silently empty rail. */
export type RecentChangesProps =
  | { scope: string; scopeNode?: undefined }
  | { scope?: undefined; scopeNode: string };

export function RecentChanges({ scope = "", scopeNode = "" }: RecentChangesProps) {
  const { available: dbAvailable, resolved: dbResolved, error: configError } = useDatabaseAvailable();
  const { at } = useTimeContext();
  const t = useT(recentChangesDict);
  const { locale } = useLocale();
  const atKey = at ? at.toISOString() : "";
  const [events, setEvents] = useState<LiveEvent[]>([]);

  // The identity of "which object is this rail pinned to" — the two props are exclusive, so one of
  // them is it.
  const filterName = scopeNode !== "" ? "scopeNode" : "scope";
  const filterValue = scopeNode !== "" ? scopeNode : scope;

  // Gated on dbResolved too, not just dbAvailable: a cold /api/v1/config must
  // not be read as "no database" (which would skip the fetch this exists to
  // make) the same way useDatabaseAvailable's own doc comment warns about.
  const historyEnabled = dbResolved && dbAvailable && filterValue !== "";
  const historyQuery = useQuery({
    // `to` bounds the rail to the Time Machine's instant.
    queryKey: at
      ? ["events", filterName, filterValue, "to", at.toISOString()]
      : ["events", filterName, filterValue],
    queryFn: () =>
      getEvents({ [filterName]: filterValue, limit: RECENT_CHANGES_LIMIT, ...(at ? { to: at } : {}) }),
    enabled: historyEnabled,
  });

  // A scope change (a different node/pair card mounted in place of this one) must not carry over
  // the previous object's history for even one render.
  useEffect(() => {
    setEvents([]);
  }, [filterName, filterValue, atKey]);

  useEffect(() => {
    if (historyQuery.data) setEvents((prev) => mergeCapped(prev, historyQuery.data.events));
  }, [historyQuery.data]);

  // Subscribed unconditionally, same as the Live page.
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

  const historyError = historyQuery.isError ? queryErrorMessage(historyQuery.error, t("error.fallback")) : null;

  // "Loading" spans two waits, not one: whether this replica even has a database (dbResolved) comes
  // back before the events page itself does.
  const waitingOnHistory = !dbResolved || (historyEnabled && historyQuery.isLoading);
  const loadingFirstPage = waitingOnHistory && events.length === 0;

  return (
    <Card asChild className="flex max-h-[calc(100vh-13rem)] flex-col gap-0 overflow-hidden p-0">
      <aside aria-label={t("aria")} className="flex flex-col">
        <div className="border-b border-border px-4 py-3">
          <h2 className="text-sm font-semibold">{t("title")}</h2>
          {/* The rail says what it is bounded BY, right where the rows are —
              the top-bar banner explains the mode, this states the cut. The
              stamp lands INSIDE that translated sentence, so it takes the
              sentence's own language and the house clock: lib/i18n's stampFull,
              the one formatter every page description shares. */}
          {at ? (
            <p className="mt-0.5 text-[11px] text-muted-foreground">
              {t("upTo", { at: stampFull(at, locale) })}
            </p>
          ) : null}
        </div>

        {/* Degraded, not broken: no database means no scrollback, but the
            live half of this rail keeps working off the socket regardless. */}
        {configError !== null ? (
          <p role="status" className="border-b border-border px-4 py-2 text-xs leading-relaxed text-muted-foreground">
            {t("config.failed", { error: queryErrorMessage(configError, t("config.failed.generic")) })}
          </p>
        ) : dbResolved && !dbAvailable ? (
          <p role="status" className="border-b border-border px-4 py-2 text-xs leading-relaxed text-muted-foreground">
            {t("db.note")}
          </p>
        ) : null}

        {historyError ? (
          <p role="status" className="border-b border-border px-4 py-2 text-xs leading-relaxed text-muted-foreground">
            {historyError}
          </p>
        ) : null}

        {loadingFirstPage ? (
          <div role="status" aria-live="polite" className="flex flex-col gap-2 p-4">
            <span className="sr-only">{t("loading")}</span>
            {Array.from({ length: 5 }, (_, i) => (
              <Skeleton key={i} className="h-9 w-full" />
            ))}
          </div>
        ) : null}

        {/* No Investigate CTA on purpose: the card header above carries the
            one Investigate link the page gets (see investigate-entry.tsx). */}
        {!loadingFirstPage && events.length === 0 ? (
          <EmptyState compact title={t("empty")} body={t("empty.body")} />
        ) : null}

        {events.length > 0 ? (
          <div
            tabIndex={0}
            role="region"
            aria-label={t("list.aria")}
            className={cn("min-h-0 overflow-y-auto", scrollRegionClass)}
          >
            <ul className="flex flex-col divide-y divide-border">
              {events.map((e) => (
                <li key={e.id} className="flex flex-col gap-1 px-4 py-2.5">
                  <div className="flex items-center justify-between gap-2">
                    <span className="nums text-[11px] text-muted-foreground" title={e.timestamp}>
                      {fmtEventStamp(e.timestamp, localeTag(locale))}
                    </span>
                    <Badge variant={isKnownSeverity(e.severity) ? SEVERITY_VARIANT[e.severity] : "unknown"} dot>
                      {e.severity}
                    </Badge>
                  </div>
                  <p className="text-xs leading-snug text-foreground">{e.summary}</p>
                </li>
              ))}
            </ul>
          </div>
        ) : null}
      </aside>
    </Card>
  );
}
