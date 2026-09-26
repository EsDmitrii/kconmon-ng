import { useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { Pause, Play, Radio, Search, TriangleAlert } from "lucide-react";
import { PageShell } from "@/components/page-shell";
import { RealtimeBadge } from "@/components/realtime-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { scrollRegionClass } from "@/components/ui/scroll-region";
import { EmptyState } from "@/components/ui/empty-state";
import { Segmented } from "@/components/ui/segmented";
import { Select } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { useCapabilities, useDatabaseAvailable } from "@/hooks/use-capabilities";
import { useFillHeight } from "@/hooks/use-fill-height";
import { getWsClient } from "@/hooks/use-ws-topic";
import { useAnnotations } from "@/components/annotations";
import { localeTag, stampFull, useLocale, useT } from "@/lib/i18n";
import { SEVERITY_KEYS, TYPE_KEYS, liveDict } from "@/lib/i18n/dict/live";
import { getEvents, queryErrorMessage } from "@/lib/api";
import { GLOBAL_SCOPE } from "@/lib/annotations";
import { useTimeContext } from "@/lib/timemachine";
import {
  LIVE_EVENT_SEVERITIES,
  LIVE_EVENT_TYPES,
  type Annotation,
  type LiveEvent,
  type LiveEventSeverity,
  type LiveEventType,
} from "@/lib/types";
import { cn, fmtEventStamp, normalizePairInput } from "@/lib/utils";
import { TOPIC_LIVE, type WsEnvelope } from "@/lib/ws";
import { LIVE_RING_CAP, pushEvents, timeOf } from "@/lib/live-events";


/* Fixed-height rows, so the virtualizer never needs measureElement: from md up a feed line is one
   line tall. Below md the row stacks (clock and badge, then a two-line summary) and takes
   STACKED_ROW_HEIGHT; useRowHeight picks between the two on the same query the row's classes use. */
export const ROW_HEIGHT = 44;
export const STACKED_ROW_HEIGHT = 64;

/* Tailwind's md is 48rem; this is its complement, so the query is true exactly where the row
   markup stacks. Written with `not` rather than a max-width so the boundary is the same pixel. */
const STACKED_QUERY = "not all and (min-width: 48rem)";

function matchesStacked(): boolean {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") return false;
  return window.matchMedia(STACKED_QUERY).matches;
}

/**
 * useRowHeight is the row height the virtualizer and the scroll anchor both read, so the two never
 * disagree about where a row sits. Without matchMedia (jsdom) it answers the desktop height.
 */
export function useRowHeight(): number {
  const [stacked, setStacked] = useState(matchesStacked);
  useEffect(() => {
    if (typeof window === "undefined" || typeof window.matchMedia !== "function") return;
    const mq = window.matchMedia(STACKED_QUERY);
    const update = () => setStacked(mq.matches);
    update();
    if (typeof mq.addEventListener === "function") {
      mq.addEventListener("change", update);
      return () => mq.removeEventListener("change", update);
    }
    mq.addListener(update);
    return () => mq.removeListener(update);
  }, []);
  return stacked ? STACKED_ROW_HEIGHT : ROW_HEIGHT;
}

export interface LiveFilters {
  type: LiveEventType | "all";
  severity: LiveEventSeverity | "all";
  scope: string;
}

const EMPTY_FILTERS: LiveFilters = { type: "all", severity: "all", scope: "" };

/* The store: three pure functions, unit-tested without React. */

/**
 * filterEvents applies the three UI filters; scope is a case-insensitive
 * substring of the NORMALISED box.
 *
 * The event's own scope is canonical by construction — the server writes the
 * arrow — so only the typed side needs normalising: "node-a->node-b",
 * "node-a => node-b" and "node-a node-b" all become the pair this feed draws.
 */
export function filterEvents(events: LiveEvent[], filters: LiveFilters): LiveEvent[] {
  const scope = normalizePairInput(filters.scope).toLowerCase();
  return events.filter(
    (e) =>
      (filters.type === "all" || e.type === filters.type) &&
      (filters.severity === "all" || e.severity === filters.severity) &&
      (scope === "" || e.scope.toLowerCase().includes(scope)),
  );
}

/** The WebSocket envelope's own seq is gapless by construction — the hub numbers what it sends.
 *
 *  An event whose `seq` is not a number is SKIPPED rather than compared: the
 *  arithmetic below is subtraction, one NaN poisons the accumulator, and a NaN
 *  total reads as `missed > 0 === false` — so a single malformed row would
 *  switch the loss warning off for the whole session, which is the one failure
 *  a loss detector must not have. The holes between the events that ARE
 *  numbered still get counted.
 */
export function countMissedEvents(events: LiveEvent[]): number {
  /* Rows the server was ASKED to filter are not evidence of loss.
     History is fetched with ?type= / ?scope= applied server-side and merged into the same ring the
     socket feeds unfiltered, so those rows are necessarily sparse in the controller's global
     sequence — and every hole between them was counted as a missing event. Picking a type in the
     dropdown made this page report ~100 events "may have been missed", i.e. it reported the
     operator's own filter back to them as data loss. */
  const numbered = events.filter((e) => Number.isFinite(e.seq) && !e.filteredHistory);
  if (numbered.length < 2) return 0;
  const bySeq = numbered.slice().sort((a, b) => a.seq - b.seq);
  let missed = 0;
  for (let i = 0; i + 1 < bySeq.length; i++) {
    const lower = bySeq[i];
    const higher = bySeq[i + 1];
    const delta = higher.seq - lower.seq;
    if (delta <= 1) continue;
    if (timeOf(lower) > timeOf(higher)) continue; // era boundary, not a hole
    missed += delta - 1;
  }
  return missed;
}

/**
 * LIVE_ANNOTATION_RANGE_SECONDS bounds the annotation fetch this page makes; a day, because the
 * scrollback can walk back a long way and a note the operator cannot see is a note that may as well
 * not exist.
 */
export const LIVE_ANNOTATION_RANGE_SECONDS = 24 * 60 * 60;

/**
 * FeedRow is what the virtualizer actually renders: an event; `key` is namespaced
 * (`annotation:<id>`) rather than the bare id.
 */
export type FeedRow =
  | { kind: "event"; key: string; at: number; event: LiveEvent }
  | { kind: "annotation"; key: string; at: number; annotation: Annotation };

/**
 * annotationMatchesFilters is the note's side of the filter bar. A note has no type and no
 * severity, so narrowing on either excludes it; a scope query keeps a note whose scope contains
 * it, and a global note only while the query is empty. Without this, a filter that matched no
 * event still drew a lone note under "Showing 0 of N", which is not "no events match" and not a
 * feed either.
 */
export function annotationMatchesFilters(annotation: Annotation, filters: LiveFilters): boolean {
  if (filters.type !== "all" || filters.severity !== "all") return false;
  const scope = normalizePairInput(filters.scope).toLowerCase();
  if (scope === "") return true;
  return annotation.scope !== "" && annotation.scope.toLowerCase().includes(scope);
}

/**
 * mergeFeedRows interleaves annotations into the (already filtered, already newest-first) event
 * list at their own timestamp position; annotations answer to the same filters through
 * annotationMatchesFilters.
 */
export function mergeFeedRows(
  events: LiveEvent[],
  annotations: Annotation[],
  filters: LiveFilters = EMPTY_FILTERS,
): FeedRow[] {
  const rows: FeedRow[] = events.map((event, i) => ({
    kind: "event",
    // The controller-assigned id, and a positional fallback for a row that
    // arrived without one — React needs a key that is UNIQUE far more than it
    // needs one that is stable, and two rows keyed `undefined` render as one.
    key: typeof event.id === "string" && event.id !== "" ? event.id : `row:${i}`,
    at: timeOf(event),
    event,
  }));
  for (const annotation of annotations) {
    if (!annotationMatchesFilters(annotation, filters)) continue;
    const parsed = Date.parse(annotation.startAt);
    rows.push({
      kind: "annotation",
      key: `annotation:${annotation.id}`,
      // Same treatment junk timestamps get on the event side (timeOf): sort to
      // the bottom rather than poison the comparator with NaN.
      at: Number.isNaN(parsed) ? Number.NEGATIVE_INFINITY : parsed,
      annotation,
    });
  }
  return rows.sort((a, b) => b.at - a.at);
}

/*
 * Go's event type and severity are open strings; the TypeScript unions are a convenience for us,
 * not a promise from the wire.
 */

/* The type and severity WORDS live in dict/live.ts, reached through TYPE_KEYS
   and SEVERITY_KEYS — the wire value is the lookup, exactly as chrome.ts's
   NAV_KEYS turns a route path into a label key. */

/* Worded badge + saturated dot: the state is never colour alone. Info stays
   neutral so a quiet feed reads quiet and colour is spent only on trouble. */
const SEVERITY_VARIANT: Record<LiveEventSeverity, "neutral" | "warn" | "bad"> = {
  info: "neutral",
  warn: "warn",
  error: "bad",
};

function isKnownType(value: string): value is LiveEventType {
  return (LIVE_EVENT_TYPES as readonly string[]).includes(value);
}

function isKnownSeverity(value: string): value is LiveEventSeverity {
  return (LIVE_EVENT_SEVERITIES as readonly string[]).includes(value);
}

/* The feed's clock is lib/utils.fmtEventStamp — fmtEventTime plus the DAY for a row that is not
   from today. A 2000-event ring plus "Load older" reaches back past midnight routinely, and a bare
   15:12 on yesterday's row reads as this afternoon's, which is the one reading a change feed must
   not invite. */

/* The word shows at every width: below md the badge shares the first line with the clock alone,
   and a dot by itself would leave warn and error to colour. */
function SeverityBadge({ severity }: { severity: string }) {
  const t = useT(liveDict);
  const known = isKnownSeverity(severity);
  return (
    <Badge variant={known ? SEVERITY_VARIANT[severity] : "unknown"} dot>
      <span>{known ? t(SEVERITY_KEYS[severity]) : severity}</span>
    </Badge>
  );
}

/* The row's columns. One line from md up; below md the li wraps and the summary takes a full
   line of its own (basis-full), clamped to two lines, under the clock and the badge.

   From md the summary and the scope share the slack with the summary ahead: the summary starts
   at 28rem and takes three quarters of any room left over, the scope starts at 20rem, takes the
   last quarter up to 26rem and gives way four times faster when the row is short of room. A
   basis of 0 on the summary handed the scope its full width first and left the summary whatever
   remained, which at 768px was nothing at all. Both are fixed bases, so the columns still line
   up row to row. */
const STAMP_CLASSES = "mono-data shrink-0 leading-5 text-muted-foreground md:w-32";
const BADGE_CELL_CLASSES = "shrink-0 md:w-[5.25rem]";
const PAYLOAD_CLASSES = "min-w-0 basis-full text-sm md:flex-[3_1_28rem]";
const SCOPE_CLASSES =
  "mono-data hidden min-w-0 truncate text-muted-foreground md:block md:max-w-[26rem] md:flex-[1_4_20rem]";

function EventRow({ event }: { event: LiveEvent }) {
  const { locale } = useLocale();
  return (
    <>
      <span className={STAMP_CLASSES}>{fmtEventStamp(event.timestamp, localeTag(locale))}</span>
      <span className={BADGE_CELL_CLASSES}>
        <SeverityBadge severity={event.severity} />
      </span>
      {/* The whole summary in the title, because the tail is where the node name lives: the row
          has no expander, no click handler and no detail view — the truncated half was simply
          gone. The annotation row beside it has carried a title all along. */}
      {/* No Type column: the summary opens with the same words, so the column repeated
          every row's first breath while eating 160px the summary needed. */}
      <span title={event.summary} className={cn(PAYLOAD_CLASSES, "line-clamp-2 md:line-clamp-1")}>
        {event.summary}
      </span>
      {/* Titled like the summary: a long pair still truncates past the cap, and the tail is the
          destination. */}
      <span className={SCOPE_CLASSES} title={event.scope}>
        {event.scope}
      </span>
    </>
  );
}

/**
 * AnnotationFeedRow is an operator's note wearing the feed's own columns; the badge says "Note"
 * rather than a severity (a note has none). The text wraps to two lines rather than truncating —
 * a note is prose an operator wrote, and its second half is the half with the reason in it.
 */
function AnnotationFeedRow({ annotation }: { annotation: Annotation }) {
  const t = useT(liveDict);
  const { locale } = useLocale();
  return (
    <>
      <span className={STAMP_CLASSES}>{fmtEventStamp(annotation.startAt, localeTag(locale))}</span>
      <span className={BADGE_CELL_CLASSES}>
        <Badge variant="neutral" dot>
          <span>{t("note.badge")}</span>
        </Badge>
      </span>
      {/* order-last below md: the text is the second line, so the span marker (when there is one)
          stays on the first line beside the badge rather than opening a third. */}
      <span className={cn(PAYLOAD_CLASSES, "order-last line-clamp-2 italic md:order-none")} title={annotation.text}>
        {annotation.text}
      </span>
      {/* Only a RANGED note earns a marker (the Type column that carried it is gone):
          the badge already says "Note", but not that it covers a span. */}
      {annotation.endAt ? (
        <span className="shrink-0 text-xs text-muted-foreground">{t("note.span")}</span>
      ) : null}
      <span className={SCOPE_CLASSES} title={annotation.createdBy}>
        {annotation.createdBy}
      </span>
    </>
  );
}

/* The skeleton mirrors the loaded shape — same columns, same row rhythm — so
   the page does not reflow when the first event lands. h-11 is ROW_HEIGHT,
   h-16 the stacked height below md. */
function FeedSkeleton() {
  const t = useT(liveDict);
  return (
    <div role="status" aria-live="polite" className="flex flex-col">
      <span className="sr-only">{t("skeleton.loading")}</span>
      {Array.from({ length: 8 }, (_, i) => (
        <div
          key={i}
          className="flex h-16 items-center gap-4 border-b border-border/60 px-3 last:border-b-0 sm:px-4 md:h-11"
        >
          <Skeleton className="h-3 w-20" />
          <Skeleton className="h-4 w-16 rounded-full" />
          <Skeleton className="h-3 flex-1" />
          <Skeleton className="hidden h-3 w-44 md:block" />
        </div>
      ))}
    </div>
  );
}

/* The feed's blank slates draw the shared EmptyState with the feed's own glyph. */
function FeedGlyph() {
  return <Radio className="size-5" />;
}

/**
 * SCOPE_MAX is what the scope box will hold: two Kubernetes node names (253
 * each) and an arrow, rounded up. Past that the box is not being used as a
 * filter — it is a paste accident — and the text still travels into a query
 * string, where a long enough one comes back as a status nobody can act on.
 */
export const SCOPE_MAX = 512;

/**
 * SCOPE_DEBOUNCE_MS is how long the box waits before asking the SERVER. The
 * client-side filter is applied on every keystroke as it always was — this only
 * governs the round trip, which used to be one keyset scan per letter typed.
 */
export const SCOPE_DEBOUNCE_MS = 250;

/**
 * sanitizeScope drops the characters a scope can never contain and a query
 * string should never carry. A NUL byte is the one that matters: text
 * parameters reach Postgres, which refuses it outright, and the request
 * came back 502 — an "unavailable" for a byte the console itself sent. None of
 * these is visible in a node name, so nothing an operator typed on purpose is
 * lost.
 */
export function sanitizeScope(raw: string): string {
  return raw.replace(/[\u0000-\u001F\u007F]/g, "").slice(0, SCOPE_MAX);
}

/* Why "Load older" is greyed out is dict/live.ts's "loadOlder.exhausted". */

export function LivePage() {
  const t = useT(liveDict);
  const { locale } = useLocale();
  /* Two strings below are produced inside callbacks that must NOT re-create themselves when the language changes. */
  const tRef = useRef(t);
  tRef.current = t;
  const { realtime, resolved } = useCapabilities();
  const { available: historyAvailable, resolved: historyResolved, error: historyConfigError } = useDatabaseAvailable();
  const { at } = useTimeContext();
  const engaged = at !== null;
  const atKey = at ? at.toISOString() : "";
  const [events, setEvents] = useState<LiveEvent[]>([]);
  const [connected, setConnected] = useState(false);
  const [topicError, setTopicError] = useState<string | null>(null);
  const [paused, setPaused] = useState(false);
  const [buffered, setBuffered] = useState(0);
  const [filters, setFilters] = useState<LiveFilters>(EMPTY_FILTERS);

  /* nextCursor is "" at the end of the feed, and undefined while page one is still owed because it
     failed: "Load older" then asks for page one again rather than calling the feed exhausted. */
  const [history, setHistory] = useState<{ nextCursor: string | undefined; loading: boolean; notice: string | null }>({
    nextCursor: "",
    loading: false,
    notice: null,
  });

  /* The scope the SERVER has been asked about, which trails the box by
     SCOPE_DEBOUNCE_MS. The client-side filter still reacts to every keystroke —
     this only governs the round trip, and typing a node name used to spend one
     keyset scan per letter. */
  const [queriedScope, setQueriedScope] = useState("");
  useEffect(() => {
    if (queriedScope === filters.scope) return;
    const timer = setTimeout(() => setQueriedScope(filters.scope), SCOPE_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [filters.scope, queriedScope]);

  /* Which load is the CURRENT one. Two of these can be in flight at once (a
     second filter change while the first is still on the wire), and the first
     to answer is not necessarily the one the operator is now looking at: a
     stale answer used to install ITS cursor, so "Load older" then walked a
     filter that had already been left, and its rows were merged into a feed
     that no longer asked for them. */
  const loadGeneration = useRef(0);

  /* Server-side filtering keeps a page relevant to what the operator is actually looking at. */
  const loadHistory = useCallback(
    async (cursor?: string) => {
      const generation = ++loadGeneration.current;
      const current = () => generation === loadGeneration.current;
      setHistory((h) => ({ ...h, loading: true }));
      try {
        const types = filters.type === "all" ? undefined : [filters.type];
        /* Normalised here too, and it matters more here: GET /api/v1/events
           compares the scope for EQUALITY, so a typed hyphen-arrow asked the
           server for a pair no row has ever been written under. */
        const scope = normalizePairInput(queriedScope) || undefined;
        // Engaged: `to=t` turns the feed into a scrollback ENDING at t; the cursor pagination
        // underneath is unchanged.
        const page = await getEvents({ types, scope, cursor, ...(at ? { to: at } : {}) });
        if (!current()) return;
        /* Tagged when the SERVER filtered this page: those rows carry holes by construction, and
           the gap detector must not read them as loss (countMissedEvents). An unfiltered page is
           left alone, so a plain scrollback still contributes to the check. */
        const filtered = Boolean(types || scope);
        const rows = filtered ? page.events.map((e) => ({ ...e, filteredHistory: true })) : page.events;
        // Merged through the exact same dedupe/sort pushEvents uses for the socket.
        setEvents((prev) => pushEvents(prev, rows));
        setHistory({ nextCursor: page.nextCursor, loading: false, notice: null });
      } catch (err) {
        if (!current()) return;
        const notice = queryErrorMessage(err, tRef.current("history.fallback"));
        /* A failed page keeps its cursor, page one's too, so the button retries it rather than
           claiming the feed is exhausted. */
        setHistory({ nextCursor: cursor, loading: false, notice });
      }
    },
    [filters.type, queriedScope, at],
  );

  // Fetches page one on mount, and again whenever the type or scope filter changes.
  useEffect(() => {
    if (!historyResolved) return;
    if (!historyAvailable) {
      setHistory({
        nextCursor: "",
        loading: false,
        notice: historyConfigError
          ? tRef.current("config.failed", {
              error: queryErrorMessage(historyConfigError, tRef.current("config.failed.generic")),
            })
          : null,
      });
      return;
    }
    void loadHistory(undefined);
  }, [historyAvailable, historyResolved, historyConfigError, loadHistory]);

  /* Arrivals land in a ref and are merged once per animation frame. A busy
     cluster emits an event per check observation, and a setState per event
     would pay for the ring merge, the filter pass, the gap scan and a React
     render at the ARRIVAL rate rather than at the display rate. */
  const inboxRef = useRef<LiveEvent[]>([]);
  const frameRef = useRef<number | null>(null);
  /* The queue is trimmed to the ring cap, so `bufferedRef` counts ARRIVALS rather than queue length. */
  const pendingRef = useRef<LiveEvent[]>([]);
  const bufferedRef = useRef(0);
  const pausedRef = useRef(false);
  const scrollRef = useRef<HTMLDivElement>(null);
  /* Events this tab threw away itself, at the slab trim below, before they ever reached the ring. */
  const discardedRef = useRef(0);
  const [discarded, setDiscarded] = useState(0);

  const flush = useCallback(() => {
    if (frameRef.current !== null) {
      cancelAnimationFrame(frameRef.current);
      frameRef.current = null;
    }
    setBuffered(bufferedRef.current);
    setDiscarded(discardedRef.current);
    const batch = inboxRef.current;
    if (batch.length === 0) return;
    inboxRef.current = [];
    setEvents((prev) => pushEvents(prev, batch));
  }, []);

  /* The difference is not stylistic: a filter narrows one stream, while `at` redefines WHICH stream. */
  useEffect(() => {
    setEvents([]);
    inboxRef.current = [];
    pendingRef.current = [];
    bufferedRef.current = 0;
    discardedRef.current = 0;
    setBuffered(0);
    setDiscarded(0);
    /* The pause goes with the rest of the store, and it has to: engaged there
       is no tail to hold, and the button that would release it is disabled —
       so a pause latched before the switch was a state the operator could
       neither see the point of nor undo, and returning to Live handed them a
       frozen feed they never asked to freeze. */
    pausedRef.current = false;
    setPaused(false);
  }, [atKey]);

  // Subscribed unconditionally, and deliberately NOT through useWsTopic: that hook keeps only the
  // latest envelope.
  useEffect(() => {
    if (engaged) {
      setConnected(false);
      setTopicError(null);
      return;
    }
    const ws = getWsClient();
    setConnected(ws.state === "open");
    const offState = ws.onStateChange((s) => {
      setConnected(s === "open");
      // A rejection belongs to the connection that produced it; reaching open again means the
      // subscribe was accepted, so the alert has to go.
      if (s === "open") setTopicError(null);
    });
    const off = ws.subscribe<LiveEvent>(TOPIC_LIVE, (env: WsEnvelope<LiveEvent>) => {
      if (env.type === "error") {
        /* The hub sends `{"error": "..."}`, never a bare string, so the string branch was dead and
           every rejection — an unknown topic, a missing permission — printed the same generic
           sentence instead of the reason the server gave. */
        const payload = env.data as unknown;
        const reason =
          typeof payload === "object" && payload !== null && typeof (payload as { error?: unknown }).error === "string"
            ? (payload as { error: string }).error
            : tRef.current("topicError.fallback");
        setTopicError(reason);
        return;
      }
      if (env.type !== "event") return;
      if (pausedRef.current) {
        bufferedRef.current += 1;
        pendingRef.current.push(env.data);
        // Trimmed in slabs rather than per event: a hidden tab gets no animation frames at all.
        if (pendingRef.current.length > LIVE_RING_CAP * 2) {
          discardedRef.current += pendingRef.current.length - LIVE_RING_CAP;
          pendingRef.current = pendingRef.current.slice(-LIVE_RING_CAP);
        }
      } else {
        inboxRef.current.push(env.data);
        if (inboxRef.current.length > LIVE_RING_CAP * 2) {
          discardedRef.current += inboxRef.current.length - LIVE_RING_CAP;
          inboxRef.current = inboxRef.current.slice(-LIVE_RING_CAP);
        }
      }
      if (frameRef.current === null) {
        frameRef.current = requestAnimationFrame(() => {
          frameRef.current = null;
          flush();
        });
      }
    });
    return () => {
      off();
      offState();
      // Cancelled, deliberately NOT flushed: the store is page-level and dies
      // with the page, so a queued batch has nowhere to land. Leaving the feed
      // is a full reset — the ring, the queues and the notices all go.
      if (frameRef.current !== null) {
        cancelAnimationFrame(frameRef.current);
        frameRef.current = null;
      }
    };
  }, [flush, engaged]);

  const togglePause = () => {
    // Whatever arrived in the current frame arrived BEFORE the click, so drain
    // it into the feed first: it is neither stranded in the inbox nor merged
    // later as though it had come in during the pause.
    flush();
    if (!paused) {
      // Set the ref here rather than from an effect.
      pausedRef.current = true;
      setPaused(true);
      return;
    }
    const drained = pendingRef.current;
    pendingRef.current = [];
    bufferedRef.current = 0;
    pausedRef.current = false;
    setBuffered(0);
    setPaused(false);
    if (drained.length > 0) setEvents((prev) => pushEvents(prev, drained));
  };

  const visible = useMemo(() => filterEvents(events, filters), [events, filters]);
  /* GLOBAL annotations only, and that is the whole story on this page: the feed is fleet-wide. */
  const { annotations } = useAnnotations(GLOBAL_SCOPE, LIVE_ANNOTATION_RANGE_SECONDS);
  const rows = useMemo(() => mergeFeedRows(visible, annotations, filters), [visible, annotations, filters]);
  // Two ways to lose an event, one number: a hole in the controller's numbering
  // (something went missing between the controller and this tab) and a slab
  // trim (this tab could not keep up and dropped its own backlog).
  const gaps = useMemo(() => countMissedEvents(events), [events]);
  /* The gap note is collapsed by default — the count is the headline, the
     explanation is what a reader asks for next. */
  const [missedOpen, setMissedOpen] = useState(false);
  const missedNoteId = useId();
  const missed = gaps + discarded;

  const rowHeight = useRowHeight();
  const rowHeightRef = useRef(rowHeight);
  rowHeightRef.current = rowHeight;

  const feedHeight = useFillHeight(scrollRef, rows.length > 0);

  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => rowHeightRef.current,
    overscan: 12,
  });

  /* The virtualizer caches the estimate per item; crossing md changes every row's height at
     once, so the cache is dropped and the rows are laid out again at the new size. */
  useEffect(() => {
    virtualizer.measure();
  }, [rowHeight, virtualizer]);

  /* Rows are PREPENDED (newest first); at the top that is exactly what a live feed should do. */
  const filterKey = `${filters.type} ${filters.severity} ${filters.scope}`;
  const anchorRef = useRef<{ key: string; id: string | null; index: number }>({
    key: filterKey,
    id: null,
    index: 0,
  });
  // The anchor tracks ROWS, not events: an annotation filed into the middle of the scrollback
  // shifts everything below it exactly the way a late event does.
  const feedRef = useRef({ rows, filterKey });
  feedRef.current = { rows, filterKey };

  const recordAnchor = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    const { rows: current, filterKey: key } = feedRef.current;
    const index = Math.max(0, Math.round(el.scrollTop / rowHeightRef.current));
    anchorRef.current = { key, index, id: current[index]?.key ?? null };
  }, []);

  useLayoutEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const anchor = anchorRef.current;
    if (anchor.key !== filterKey) {
      // A new filter is a new list; there is nothing to hold in place.
      el.scrollTop = 0;
    } else if (el.scrollTop > 0 && anchor.id !== null) {
      const now = rows.findIndex((r) => r.key === anchor.id);
      // Gone (evicted off the tail) reads as -1: nothing left to anchor to, so
      // leave the offset alone and re-record against whatever is there now.
      if (now >= 0 && now !== anchor.index) el.scrollTop += (now - anchor.index) * rowHeight;
    }
    recordAnchor();
  }, [filterKey, rows, recordAnchor, rowHeight]);

  /* Exhausted: the walk has nowhere left to go under the CURRENT filters. */
  const exhausted = history.nextCursor === "" && !history.loading;
  /* At the cap the ring is full, and pushEvents drops everything past
     LIVE_RING_CAP on the way in — so "Load older" would spend a round trip to
     change nothing. A control that does nothing must say so rather than look
     available. */
  const atCap = events.length >= LIVE_RING_CAP;

  const clearFilters = useCallback(() => setFilters(EMPTY_FILTERS), []);
  // Only a filter can empty a non-empty ring.
  const connecting = events.length === 0 && !engaged && (!resolved || (realtime && !connected));

  return (
    <PageShell
      variant="tool"
      timeMachine
      title={t("title")}
      help={{ body: t("help.body"), slug: "events" }}
      description={
        at
          ? /* Inside a translated sentence, so the stamp takes that sentence's
               language and the house clock — lib/i18n's stampFull. */
            t("description.engaged", { at: stampFull(at, locale) })
          : t("description.live", { cap: LIVE_RING_CAP })
      }
      actions={
        <div data-testid="live-toolbar" className="flex flex-wrap items-center gap-2">
          <Segmented
            aria-label={t("filters.severity")}
            options={[
              { value: "all", label: t("filters.severity.all") },
              ...LIVE_EVENT_SEVERITIES.map((s) => ({ value: s, label: t(SEVERITY_KEYS[s]) })),
            ]}
            value={filters.severity}
            onChange={(severity) => setFilters((f) => ({ ...f, severity }))}
          />
          {/* The shared filter Select: a native <select> keeps the platform
              picker (keyboard, mobile, screen readers), dressed like the
              Segmented track beside it. */}
          <Select
            variant="filter"
            aria-label={t("filters.type")}
            value={filters.type}
            onChange={(e) => {
              const next = e.target.value;
              // Runtime membership, not a cast: the union is ours, the wire is
              // Go's. An unknown value falls back to "all" rather than
              // filtering the feed down to nothing forever.
              setFilters((f) => ({ ...f, type: isKnownType(next) ? next : "all" }));
            }}
          >
            <option value="all">{t("filters.type.all")}</option>
            {LIVE_EVENT_TYPES.map((value) => (
              <option key={value} value={value}>
                {t(TYPE_KEYS[value])}
              </option>
            ))}
          </Select>
          {/* Pause holds a live tail still. Engaged there is no tail, so the
              button has nothing to act on — disabled rather than removed, the
              same rule the mutation affordances follow
              (lib/timemachine.tsx's useWritesDisabled). */}
          <Button variant="outline" size="sm" disabled={engaged} onClick={togglePause}>
            {paused ? <Play aria-hidden="true" className="size-3.5" /> : <Pause aria-hidden="true" className="size-3.5" />}
            {paused
              ? buffered > 0
                ? t("resume.buffered", { count: buffered })
                : t("resume")
              : t("pause")}
          </Button>
          {/* Pushed or not — the badge states the transport, never colour alone.
              While the capability is still unknown it says so rather than
              guessing "delayed". */}
          {/* Engaged the transport question does not arise — the badge would
              be answering "is the push live?" about a feed that is deliberately
              not live. The mode itself is the answer, and the top-bar banner
              says it once for the whole console.
              Paused is the same shape of lie in the other direction: arrivals
              are being held, so a green "Live" over a frozen list claims
              exactly what the operator just switched off. The badge then says
              paused, and what the transport is doing under it: a socket that
              dropped during a long pause is what an operator needs to know
              before pressing Resume. The Resume button carries the buffered
              count, so this is the only Paused chip on the page. */}
          <span data-testid="live-transport-slot" className="inline-flex">
            {engaged ? null : paused ? (
              <Badge variant={realtime && connected ? "neutral" : "warn"} dot>
                {t(realtime && connected ? "paused.socket.live" : "paused.socket.down")}
              </Badge>
            ) : resolved ? (
              <RealtimeBadge realtime={realtime && connected} />
            ) : (
              <Badge variant="neutral" dot>
                {t("connecting")}
              </Badge>
            )}
          </span>
        </div>
      }
    >
      {topicError ? (
        <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
          <p className="text-sm font-medium">{t("topicError.title")}</p>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{topicError}</p>
        </Card>
      ) : null}

      {/* Not while engaged: "no events will arrive here" is true by design in
          that mode and reads as a fault. */}
      {!engaged && resolved && !realtime ? (
        <Card role="status" className="border-l-4 border-l-health-warn bg-health-warn-soft/40 p-5">
          <p className="text-sm font-medium">{t("noRealtime.title")}</p>
          <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{t("noRealtime.body")}</p>
        </Card>
      ) : null}

      {/* Non-fatal: the scrollback endpoint failing (503, history disabled)
          says nothing about the live feed, which keeps working off the socket
          regardless. */}
      {history.notice ? (
        <Card role="status" className="border-l-4 border-l-health-warn bg-health-warn-soft/40 p-5">
          <p className="text-sm font-medium">{t("history.title")}</p>
          <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{history.notice}</p>
          {history.nextCursor === undefined ? (
            <Button
              type="button"
              size="sm"
              variant="outline"
              className="mt-3"
              disabled={history.loading}
              onClick={() => void loadHistory(undefined)}
            >
              {t("history.retry")}
            </Button>
          ) : null}
        </Card>
      ) : null}

      {/* The working surface, unboxed: the shell's "tool" variant runs the feed
          to the edges, so the card that used to frame it is gone and the filter
          bar reads as the slim toolbar it is. The block bleeds out by
          the shell's own padding (-mx-3 sm:-mx-4) and every bar and row puts
          it back (px-3 sm:px-4), so the rules and the hover run edge to edge
          while the text lines up with the toolbar above. */}
      <div className="-mx-3 sm:-mx-4">
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-border px-3 py-3 sm:px-4">
          <label className="relative flex items-center">
            <span className="sr-only">{t("filters.scope.label")}</span>
            <Search aria-hidden="true" className="pointer-events-none absolute left-2.5 size-3.5 text-muted-foreground" />
            <input
              value={filters.scope}
              /* Sanitised on the way IN rather than on the way out, so the
                 box, the client-side filter and the query string are all
                 looking at the same string — a control character the operator
                 pasted and cannot see must not be the difference between what
                 the feed matches and what the server was asked. */
              onChange={(e) => setFilters((f) => ({ ...f, scope: sanitizeScope(e.target.value) }))}
              maxLength={SCOPE_MAX}
              placeholder={t("filters.scope.placeholder")}
              className="h-8 w-64 rounded-md bg-surface-2 pl-8 pr-2 text-sm placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            />
          </label>

          {historyAvailable ? (
            /*
             * A disabled control that will not say why is a dead end: the cursor is exhausted for
             * the CURRENT filters.
             */
            <Button
              variant="outline"
              size="sm"
              disabled={exhausted || atCap || history.loading}
              title={
                atCap
                  ? t("loadOlder.atCap", { cap: LIVE_RING_CAP })
                  : exhausted
                    ? t("loadOlder.exhausted")
                    : undefined
              }
              onClick={() => loadHistory(history.nextCursor)}
            >
              {t(history.loading ? "loadOlder.loading" : "loadOlder")}
              {/* The reason is READABLE, not just hoverable: a title alone is
                  invisible to touch and to a screen reader on a disabled
                  control. */}
              {atCap ? (
                <span className="sr-only">{t("loadOlder.atCap", { cap: LIVE_RING_CAP })}</span>
              ) : exhausted ? (
                <span className="sr-only">{t("loadOlder.exhausted")}</span>
              ) : null}
            </Button>
          ) : null}

          {missed > 0 ? (
            /* Not a title attribute, which is invisible to touch and to anyone who does not
               hover a warning triangle: this is the only account of why the feed has holes
               in it, so it gets a control that opens it in the page. */
            <span role="status" className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <TriangleAlert aria-hidden="true" className="size-3.5 text-health-warn" />
              {t(missed === 1 ? "missed.one" : "missed.many", { count: missed })}
              <button
                type="button"
                aria-expanded={missedOpen}
                aria-controls={missedNoteId}
                aria-label={t("missed.whyAria")}
                onClick={() => setMissedOpen((v) => !v)}
                className="underline underline-offset-2 hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              >
                {t(missedOpen ? "missed.hide" : "missed.why")}
              </button>
            </span>
          ) : null}

          {missed > 0 && missedOpen ? (
            <p
              id={missedNoteId}
              data-testid="missed-note"
              className="basis-full max-w-prose text-xs leading-relaxed text-muted-foreground"
            >
              {discarded > 0 ? t("missed.title.both", { gaps, discarded }) : t("missed.title.gaps")}
            </p>
          ) : null}

          <p className="nums ml-auto text-xs text-muted-foreground">
            {t("counts", { shown: visible.length, held: events.length, cap: LIVE_RING_CAP })}
          </p>
        </div>

        {/* Column headers only where there are columns: below md the row is
            two stacked lines and a header over them would name nothing. */}
        <div
          aria-hidden="true"
          className="hidden items-center gap-4 border-b border-border px-3 py-2 text-[11px] font-medium text-muted-foreground sm:px-4 md:flex"
        >
          <span className="w-32 shrink-0">{t("col.time")}</span>
          <span className="w-[5.25rem] shrink-0">{t("col.severity")}</span>
          {/* The same flex as the row's own columns, so the headers sit over them. */}
          <span className="min-w-0 md:flex-[3_1_28rem]">{t("col.summary")}</span>
          <span className="hidden min-w-0 md:block md:max-w-[26rem] md:flex-[1_4_20rem]">{t("col.scope")}</span>
        </div>

        {connecting ? <FeedSkeleton /> : null}

        {/* rows.length rather than events.length: a window with no events but
            an operator note in it is not an empty feed. */}
        {!connecting && events.length === 0 && rows.length === 0 ? (
          <EmptyState
            icon={<FeedGlyph />}
            title={t(engaged ? "empty.engaged.title" : "empty.waiting.title")}
            body={t(engaged ? "empty.engaged.body" : "empty.waiting.body")}
          />
        ) : null}

        {/* rows, not `visible`: the list below renders annotation rows too, so gating on the event
            count alone put "no events match these filters" directly above a populated list. */}
        {!connecting && events.length > 0 && rows.length === 0 ? (
          <EmptyState
            icon={<FeedGlyph />}
            title={t("empty.filtered.title")}
            body={t("empty.filtered.body", { count: events.length })}
            action={
              <Button variant="outline" size="sm" onClick={clearFilters}>
                {t("filters.clear")}
              </Button>
            }
          />
        ) : null}

        {/* The feed is the page, so it runs to the bottom of the window
            (useFillHeight). The class is the first paint's estimate: 17rem for
            the chrome above it, with a floor so a short window still has a feed. */}
        {rows.length > 0 ? (
          <div
            ref={scrollRef}
            onScroll={recordAnchor}
            // role="log" names the region for what it is: an append-only feed that a screen-reader
            // user navigates deliberately. It sits on the scroller, not the <ul>, so the list keeps
            // its own semantics and the keyboard can focus and scroll the feed.
            role="log"
            aria-live="off"
            aria-label={t("feed.aria")}
            tabIndex={0}
            style={feedHeight === undefined ? undefined : { height: feedHeight }}
            className={cn("h-[calc(100dvh-17rem)] min-h-[20rem] overflow-auto", scrollRegionClass)}
          >
            <ul
              className="relative m-0 list-none p-0"
              style={{ height: `${virtualizer.getTotalSize()}px` }}
            >
              {virtualizer.getVirtualItems().map((item) => {
                const row = rows[item.index];
                return (
                  <li
                    key={row.key}
                    // For an event the key is the controller-assigned "<seq>-<unixNano>", the same
                    // string the hub dedupes on.
                    data-testid={row.kind === "annotation" ? "annotation-feed-row" : undefined}
                    style={{ height: `${item.size}px`, transform: `translateY(${item.start}px)` }}
                    className={cn(
                      /* Wraps below md (the summary takes its own line), one line from md up;
                         content-center keeps the two lines together in the fixed-height row. */
                      "absolute left-0 top-0 flex w-full flex-wrap content-center items-center gap-x-4 gap-y-0.5 px-3 sm:px-4 md:flex-nowrap",
                      "border-b border-border/60 transition-colors duration-(--dur-fast) ease-(--ease) hover:bg-accent/40",
                      // A note is not an event, and the row says so before it is
                      // read: recessed, with a left rule in the accent colour.
                      row.kind === "annotation" && "border-l-2 border-l-primary bg-surface-2/50",
                    )}
                  >
                    {row.kind === "event" ? (
                      <EventRow event={row.event} />
                    ) : (
                      <AnnotationFeedRow annotation={row.annotation} />
                    )}
                  </li>
                );
              })}
            </ul>
          </div>
        ) : null}
      </div>
    </PageShell>
  );
}
