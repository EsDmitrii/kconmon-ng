import { useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { ChevronDown, Pause, Play, Radio, Search, TriangleAlert } from "lucide-react";
import { PageShell } from "@/components/page-shell";
import { RealtimeBadge } from "@/components/realtime-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { useCapabilities, useDatabaseAvailable } from "@/hooks/use-capabilities";
import { getWsClient } from "@/hooks/use-ws-topic";
import { useAnnotations } from "@/components/annotations";
import { localeTag, useLocale, useT } from "@/lib/i18n";
import { SEVERITY_KEYS, TYPE_KEYS, liveDict } from "@/lib/i18n/dict/live";
import { ApiError, getEvents } from "@/lib/api";
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

/** The browser keeps a bounded ring of the most recent events and drops the oldest. */
export const LIVE_RING_CAP = 2000;

/* Fixed-height rows, so the virtualizer never needs measureElement: a feed line is one line tall. */
export const ROW_HEIGHT = 44;

export interface LiveFilters {
  type: LiveEventType | "all";
  severity: LiveEventSeverity | "all";
  scope: string;
}

/* The store: three pure functions, unit-tested without React. */

/**
 * eventTime is the parsed timestamp, memoised per event object; an unparseable timestamp becomes
 * -Infinity rather than NaN.
 */
const eventTime = new WeakMap<LiveEvent, number>();

function timeOf(e: LiveEvent): number {
  const cached = eventTime.get(e);
  if (cached !== undefined) return cached;
  const parsed = Date.parse(e.timestamp);
  const value = Number.isNaN(parsed) ? Number.NEGATIVE_INFINITY : parsed;
  eventTime.set(e, value);
  return value;
}

/**
 * compareDesc orders two events for the newest-first feed — lexicographic on (timestamp, seq); the
 * timestamp leads and the controller-assigned seq only breaks ties.
 */
function compareDesc(a: LiveEvent, b: LiveEvent): number {
  const ta = timeOf(a);
  const tb = timeOf(b);
  if (ta !== tb) return ta > tb ? -1 : 1;
  if (a.seq !== b.seq) return a.seq > b.seq ? -1 : 1;
  return 0;
}

/** isAbove is compareDesc as a predicate; the merge below reads better with it. */
function isAbove(a: LiveEvent, b: LiveEvent): boolean {
  return compareDesc(a, b) < 0;
}

/**
 * pushEvents merges arrivals into a newest-first ring capped at LIVE_RING_CAP,
 * returning a new array — or `prev` itself when nothing new arrived, so React
 * skips the re-render.
 *
 * Two transport facts shape this. Delivery is exactly-once per connection but
 * NOT ordered (a Broadcast can beat the replay that a reconnect asked for), so
 * a lower seq than one already held is a late arrival to be filed in place,
 * never a straggler to discard. And a reconnect replays from the resume cursor,
 * so the same event can genuinely arrive twice — deduped here on the
 * controller-assigned id, the very key the hub itself dedupes on.
 *
 * Both sides are sorted, so this is a straight two-list merge: O(n + m) with the
 * cap applied as it builds, rather than a splice per arrival. That matters on
 * exactly one path — a tab returning from hidden flushes a whole queue at once,
 * and a per-item splice into a full ring would be tens of millions of element
 * moves inside a single frame.
 */
export function pushEvents(prev: LiveEvent[], incoming: LiveEvent[]): LiveEvent[] {
  if (incoming.length === 0) return prev;

  const seen = new Set(prev.map((e) => e.id));
  const fresh: LiveEvent[] = [];
  for (const e of incoming) {
    // Dedupe on a REAL id only. An id is controller-assigned and never absent
    // on the wire, but a row that arrives without one is not "the same event"
    // as every other row that arrives without one — keying them all on
    // `undefined` collapsed the whole batch into its first member, which is
    // the one thing a feed built not to lose events must not do.
    const identified = typeof e.id === "string" && e.id !== "";
    if (identified) {
      if (seen.has(e.id)) continue;
      seen.add(e.id);
    }
    fresh.push(e);
  }
  if (fresh.length === 0) return prev;

  fresh.sort(compareDesc);

  const next: LiveEvent[] = [];
  let i = 0;
  let j = 0;
  while (i < prev.length && j < fresh.length && next.length < LIVE_RING_CAP) {
    next.push(isAbove(fresh[j], prev[i]) ? fresh[j++] : prev[i++]);
  }
  while (i < prev.length && next.length < LIVE_RING_CAP) next.push(prev[i++]);
  while (j < fresh.length && next.length < LIVE_RING_CAP) next.push(fresh[j++]);
  return next;
}

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
 * mergeFeedRows interleaves annotations into the (already filtered, already newest-first) event
 * list at their own timestamp position; annotations are NOT filtered by the type/severity/scope
 * controls.
 */
export function mergeFeedRows(events: LiveEvent[], annotations: Annotation[]): FeedRow[] {
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

function SeverityBadge({ severity }: { severity: string }) {
  const t = useT(liveDict);
  const known = isKnownSeverity(severity);
  return (
    <Badge variant={known ? SEVERITY_VARIANT[severity] : "unknown"} dot>
      {known ? t(SEVERITY_KEYS[severity]) : severity}
    </Badge>
  );
}

function EventRow({ event }: { event: LiveEvent }) {
  const { locale } = useLocale();
  return (
    <>
      <span className="nums w-24 shrink-0 text-xs text-muted-foreground">{fmtEventStamp(event.timestamp, localeTag(locale))}</span>
      <span className="w-[5.25rem] shrink-0">
        <SeverityBadge severity={event.severity} />
      </span>
      {/* The whole summary in the title, because the tail is where the node name lives: the fixed
          columns to the right leave this one about 300px, and the row has no expander, no click
          handler and no detail view — the truncated half was simply gone. The annotation row beside
          it has carried a title all along. */}
      {/* No Type column: the summary opens with the same words, so the column repeated
          every row's first breath while eating 160px the summary needed. */}
      <span title={event.summary} className="min-w-0 flex-1 truncate text-sm">
        {event.summary}
      </span>
      <span className="hidden w-52 shrink-0 truncate text-xs text-muted-foreground md:block">{event.scope}</span>
    </>
  );
}

/**
 * AnnotationFeedRow is an operator's note wearing the feed's own columns; the badge says "Note"
 * rather than a severity (a note has none).
 */
function AnnotationFeedRow({ annotation }: { annotation: Annotation }) {
  const t = useT(liveDict);
  const { locale } = useLocale();
  return (
    <>
      <span className="nums w-24 shrink-0 text-xs text-muted-foreground">{fmtEventStamp(annotation.startAt, localeTag(locale))}</span>
      <span className="w-[5.25rem] shrink-0">
        <Badge variant="neutral" dot>
          {t("note.badge")}
        </Badge>
      </span>
      <span className="min-w-0 flex-1 truncate text-sm italic" title={annotation.text}>
        {annotation.text}
      </span>
      {/* Only a RANGED note earns a marker (the Type column that carried it is gone):
          the badge already says "Note", but not that it covers a span. */}
      {annotation.endAt ? (
        <span className="shrink-0 text-xs text-muted-foreground">{t("note.span")}</span>
      ) : null}
      <span className="hidden w-52 shrink-0 truncate text-xs text-muted-foreground md:block">
        {annotation.createdBy}
      </span>
    </>
  );
}

/* The skeleton mirrors the loaded shape — same columns, same row rhythm — so
   the page does not reflow when the first event lands. h-11 is ROW_HEIGHT. */
function FeedSkeleton() {
  const t = useT(liveDict);
  return (
    <div role="status" aria-live="polite" className="flex flex-col">
      <span className="sr-only">{t("skeleton.loading")}</span>
      {Array.from({ length: 8 }, (_, i) => (
        <div key={i} className="flex h-11 items-center gap-4 border-b border-border/60 px-4 last:border-b-0">
          <Skeleton className="h-3 w-20" />
          <Skeleton className="h-4 w-16 rounded-full" />
          <Skeleton className="h-3 flex-1" />
          <Skeleton className="hidden h-3 w-44 md:block" />
        </div>
      ))}
    </div>
  );
}

function BlankSlate({ title, body, action }: { title: string; body: string; action?: ReactNode }) {
  return (
    <div className="flex flex-col items-center gap-3 px-6 py-14 text-center">
      <span
        aria-hidden="true"
        className="flex size-12 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
      >
        <Radio className="size-5" />
      </span>
      <p className="text-sm font-medium">{title}</p>
      <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">{body}</p>
      {action}
    </div>
  );
}

const EMPTY_FILTERS: LiveFilters = { type: "all", severity: "all", scope: "" };

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
  const { available: historyAvailable, resolved: historyResolved } = useDatabaseAvailable();
  const { at } = useTimeContext();
  const engaged = at !== null;
  const atKey = at ? at.toISOString() : "";
  const [events, setEvents] = useState<LiveEvent[]>([]);
  const [connected, setConnected] = useState(false);
  const [topicError, setTopicError] = useState<string | null>(null);
  const [paused, setPaused] = useState(false);
  const [buffered, setBuffered] = useState(0);
  const [filters, setFilters] = useState<LiveFilters>(EMPTY_FILTERS);

  /* A failed load (the 503 case) also lands on "", not on the retry button. */
  const [history, setHistory] = useState<{ nextCursor: string; loading: boolean; notice: string | null }>({
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
        const notice =
          err instanceof ApiError ? (err.problem.detail ?? err.problem.title) : tRef.current("history.fallback");
        setHistory({ nextCursor: "", loading: false, notice });
      }
    },
    [filters.type, queriedScope, at],
  );

  // Fetches page one on mount, and again whenever the type or scope filter changes.
  useEffect(() => {
    if (!historyResolved) return;
    if (!historyAvailable) {
      setHistory({ nextCursor: "", loading: false, notice: null });
      return;
    }
    void loadHistory(undefined);
  }, [historyAvailable, historyResolved, loadHistory]);

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
  const rows = useMemo(() => mergeFeedRows(visible, annotations), [visible, annotations]);
  // Two ways to lose an event, one number: a hole in the controller's numbering
  // (something went missing between the controller and this tab) and a slab
  // trim (this tab could not keep up and dropped its own backlog).
  const gaps = useMemo(() => countMissedEvents(events), [events]);
  /* The gap note is collapsed by default — the count is the headline, the
     explanation is what a reader asks for next. */
  const [missedOpen, setMissedOpen] = useState(false);
  const missedNoteId = useId();
  const missed = gaps + discarded;

  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => ROW_HEIGHT,
    overscan: 12,
  });

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
    const index = Math.max(0, Math.round(el.scrollTop / ROW_HEIGHT));
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
      if (now >= 0 && now !== anchor.index) el.scrollTop += (now - anchor.index) * ROW_HEIGHT;
    }
    recordAnchor();
  }, [filterKey, rows, recordAnchor]);

  /* Exhausted: the walk has nowhere left to go under the CURRENT filters. */
  const exhausted = history.nextCursor === "" && !history.loading;
  /* At the cap the ring is full, and pushEvents drops everything past
     LIVE_RING_CAP on the way in — so "Load older" would spend a round trip to
     change nothing. A control that does nothing must say so rather than look
     available (QA scope 5, finding #22). */
  const atCap = events.length >= LIVE_RING_CAP;

  const clearFilters = useCallback(() => setFilters(EMPTY_FILTERS), []);
  // Only a filter can empty a non-empty ring.
  const connecting = events.length === 0 && !engaged && (!resolved || (realtime && !connected));

  return (
    <PageShell
      timeMachine
      title={t("title")}
      description={
        at
          ? /* Inside a translated sentence, so the stamp takes that sentence's
               language — lib/i18n's localeTag (QA scope 2, finding #8). */
            t("description.engaged", { at: at.toLocaleString(localeTag(locale)) })
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
          {/* A native <select> keeps the platform picker (keyboard, mobile,
              screen readers) — only its closed face is dressed in kit tokens:
              recessed surface-2 like the Segmented track, no border, a real
              chevron instead of the UA arrow. */}
          <span className="relative inline-flex">
            <select
              aria-label={t("filters.type")}
              value={filters.type}
              onChange={(e) => {
                const next = e.target.value;
                // Runtime membership, not a cast: the union is ours, the wire is
                // Go's. An unknown value falls back to "all" rather than
                // filtering the feed down to nothing forever.
                setFilters((f) => ({ ...f, type: isKnownType(next) ? next : "all" }));
              }}
              className="h-10 appearance-none rounded-md bg-surface-2 py-1 pl-3.5 pr-8 text-sm text-foreground transition-colors duration-(--dur-fast) ease-(--ease) hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            >
              <option value="all">{t("filters.type.all")}</option>
              {LIVE_EVENT_TYPES.map((value) => (
                <option key={value} value={value}>
                  {t(TYPE_KEYS[value])}
                </option>
              ))}
            </select>
            <ChevronDown
              aria-hidden="true"
              className="pointer-events-none absolute right-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground"
            />
          </span>
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
              exactly what the operator just switched off. The Paused chip in
              the filter bar is the state (QA round 1, finding #12).
              But EMPTYING the slot while paused threw away the other half of
              the answer: whether the feed being resumed into is still there.
              A socket that dropped during a long pause is exactly what an
              operator needs to know BEFORE pressing Resume, so the badge stays
              — saying paused, and saying what the transport is doing under it.
              The slot keeps its width in every case, so nothing to its left
              moves when the badge changes. */}
          <span data-testid="live-transport-slot" className="inline-flex min-w-[7.5rem] justify-end">
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
        </Card>
      ) : null}

      <Card className="overflow-hidden p-0">
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-border px-4 py-3">
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

          {paused ? <Badge variant="warn" dot>{t("paused.badge", { count: buffered })}</Badge> : null}

          {missed > 0 ? (
            /* The explanation used to live ONLY in a title attribute — invisible
               to touch, and to anyone who does not think to hover a warning
               triangle. It is the only account of why the feed has holes in it,
               so it gets a control that opens it in the page (finding 19). */
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

        <div
          aria-hidden="true"
          className="flex items-center gap-4 border-b border-border px-4 py-2 text-[11px] font-medium text-muted-foreground"
        >
          <span className="w-24 shrink-0">{t("col.time")}</span>
          <span className="w-[5.25rem] shrink-0">{t("col.severity")}</span>
          <span className="min-w-0 flex-1">{t("col.summary")}</span>
          <span className="hidden w-52 shrink-0 md:block">{t("col.scope")}</span>
        </div>

        {connecting ? <FeedSkeleton /> : null}

        {/* rows.length rather than events.length: a window with no events but
            an operator note in it is not an empty feed. */}
        {!connecting && events.length === 0 && rows.length === 0 ? (
          <BlankSlate
            title={t(engaged ? "empty.engaged.title" : "empty.waiting.title")}
            body={t(engaged ? "empty.engaged.body" : "empty.waiting.body")}
          />
        ) : null}

        {/* rows, not `visible`: the list below renders annotation rows too, so gating on the event
            count alone put "no events match these filters" directly above a populated list. */}
        {!connecting && events.length > 0 && rows.length === 0 ? (
          <BlankSlate
            title={t("empty.filtered.title")}
            body={t("empty.filtered.body", { count: events.length })}
            action={
              <Button variant="outline" size="sm" onClick={clearFilters}>
                {t("filters.clear")}
              </Button>
            }
          />
        ) : null}

        {rows.length > 0 ? (
          <div ref={scrollRef} onScroll={recordAnchor} className="h-[min(60vh,40rem)] overflow-auto">
            <ul
              role="log"
              // role="log" names the region for what it is: an append-only feed that a
              // screen-reader user navigates deliberately.
              aria-live="off"
              aria-label={t("feed.aria")}
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
                      "absolute left-0 top-0 flex w-full items-center gap-4 px-4",
                      "border-b border-border/60",
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
      </Card>
    </PageShell>
  );
}
