import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
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
import { cn, fmtEventTime } from "@/lib/utils";
import { TOPIC_LIVE, type WsEnvelope } from "@/lib/ws";

/**
 * The browser keeps a bounded ring of the most recent events and drops the
 * oldest. Deep history is a REST concern (GET /api/v1/events, a later
 * milestone), not an unbounded array that grows until the tab dies — a busy
 * cluster emits an event per check observation.
 */
export const LIVE_RING_CAP = 2000;

/* Fixed-height rows, so the virtualizer never needs measureElement: a feed line
   is one line tall, and dynamic measurement would cost a layout pass per row
   for nothing. The virtualizer writes this height onto every row inline, so
   this constant IS the row height; the only place it also appears as a class is
   the skeleton's h-11, which has to match it by hand. Exported because the
   scroll-anchoring test has to reason in the same units. */
export const ROW_HEIGHT = 44;

export interface LiveFilters {
  type: LiveEventType | "all";
  severity: LiveEventSeverity | "all";
  scope: string;
}

/* ---------------------------------------------------------------------------
   The store: three pure functions, unit-tested without React. Everything the
   feed knows about ordering, duplication and loss lives here.
   --------------------------------------------------------------------------- */

/**
 * eventTime is the parsed timestamp, memoised per event object. Events are
 * immutable payloads straight off the socket, so one parse each is enough — and
 * without the cache a single insertion into a full ring could run 2000
 * Date.parse calls. A WeakMap keeps this invisible to the ring's memory bound:
 * an event dropped off the tail takes its entry with it.
 *
 * An unparseable timestamp becomes -Infinity rather than NaN. That keeps the
 * comparator TOTAL: NaN makes every comparison against it false, which is not
 * an ordering at all and can produce cycles once seq is mixed in as a secondary
 * key. Sorting junk to the bottom is a defined, boring outcome.
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
 * compareDesc orders two events for the newest-first feed — lexicographic on
 * (timestamp, seq), a total order.
 *
 * The timestamp leads and the controller-assigned seq only breaks ties. Seq
 * alone would be wrong across a controller restart, which takes the counter
 * back to 1: seq-ordering would file the newest event at the very bottom of the
 * feed, where the ring cap then eats it. Sub-millisecond ties (Date.parse
 * truncates to ms) fall back to seq, which is the right order inside one era.
 *
 * Written as explicit comparisons rather than subtraction because -Infinity
 * (an unparseable timestamp) minus itself is NaN, and a comparator that returns
 * NaN has undefined sort behaviour.
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
    if (seen.has(e.id)) continue;
    seen.add(e.id);
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

/** filterEvents applies the three UI filters; scope is a case-insensitive substring. */
export function filterEvents(events: LiveEvent[], filters: LiveFilters): LiveEvent[] {
  const scope = filters.scope.trim().toLowerCase();
  return events.filter(
    (e) =>
      (filters.type === "all" || e.type === filters.type) &&
      (filters.severity === "all" || e.severity === filters.severity) &&
      (scope === "" || e.scope.toLowerCase().includes(scope)),
  );
}

/**
 * countMissedEvents is the loss signal. The WebSocket envelope's own seq is
 * gapless by construction — the hub numbers what it sends, so anything dropped
 * on the Valkey bus or evicted from a replay ring never shows up there. The
 * controller-assigned seq INSIDE the payload is the one that can have holes,
 * and only a consumer that decodes the payload can see them.
 *
 * It walks a copy sorted BY SEQ, never the display order. Display order is
 * timestamp-primary, and observations from different agents legitimately arrive
 * time-shuffled relative to the controller's numbering — walking the displayed
 * array would read that shuffle as holes and pin a false "events may have been
 * missed" over a perfectly healthy stream.
 *
 * A hole and a restarted controller counter are told apart by DIRECTION, never
 * by size: at an era boundary the lower-seq side carries the NEWER timestamp
 * (the counter went back to 1 while time kept going forward), whereas inside one
 * era seq and time climb together. So there is no magnitude threshold — a
 * genuine hole is reported however large it is, which is the whole point of the
 * signal. Guessing "too big to be real" is exactly how a detector goes quiet
 * during the outages it exists for.
 *
 * Counted over what is currently held, so a replay that later fills a hole makes
 * the notice disappear by itself.
 */
export function countMissedEvents(events: LiveEvent[]): number {
  if (events.length < 2) return 0;
  const bySeq = events.slice().sort((a, b) => a.seq - b.seq);
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
 * LIVE_ANNOTATION_RANGE_SECONDS bounds the annotation fetch this page makes. A
 * day, because the scrollback can walk back a long way and a note the operator
 * cannot see is a note that may as well not exist — but bounded all the same,
 * because "every annotation ever" is not a request this page has any business
 * making.
 */
export const LIVE_ANNOTATION_RANGE_SECONDS = 24 * 60 * 60;

/**
 * FeedRow is what the virtualizer actually renders: an event, or a GLOBAL
 * annotation filed at its own timestamp among them.
 *
 * `key` is namespaced (`annotation:<id>`) rather than the bare id, because an
 * event id ("<seq>-<unixNano>") and an annotation id (a UUID) come from
 * different issuers and nothing guarantees they never collide — and a duplicate
 * React key in a virtualized list is a rendering bug that only shows up under
 * scroll.
 */
export type FeedRow =
  | { kind: "event"; key: string; at: number; event: LiveEvent }
  | { kind: "annotation"; key: string; at: number; annotation: Annotation };

/**
 * mergeFeedRows interleaves annotations into the (already filtered, already
 * newest-first) event list at their own timestamp position.
 *
 * Annotations are NOT filtered by the type/severity/scope controls: those
 * filters describe the controller's event stream, and an operator note is not
 * one of its rows — hiding a human's note because they picked "topology
 * changed" would be the feed editing the record.
 *
 * The sort is descending on time only, and Array#sort is stable, so events keep
 * the (timestamp, seq) order pushEvents already gave them and an annotation
 * sharing a millisecond with an event lands just after it. Deterministic, which
 * is what a virtualized list with sticky scroll anchoring needs.
 */
export function mergeFeedRows(events: LiveEvent[], annotations: Annotation[]): FeedRow[] {
  const rows: FeedRow[] = events.map((event) => ({ kind: "event", key: event.id, at: timeOf(event), event }));
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

/* ---------------------------------------------------------------------------
   Presentation. Go's event type and severity are open strings; the TypeScript
   unions are a convenience for us, not a promise from the wire. Every lookup
   below therefore degrades to the raw string instead of rendering `undefined`.
   --------------------------------------------------------------------------- */

const TYPE_LABELS: Record<LiveEventType, string> = {
  topology_changed: "Topology changed",
  check_observed: "Check observed",
  mtr_triggered: "MTR triggered",
  mtr_completed: "MTR completed",
  diagnostic_progress: "Diagnostic progress",
};

const SEVERITY_LABELS: Record<LiveEventSeverity, string> = {
  info: "Info",
  warn: "Warn",
  error: "Error",
};

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

function typeLabel(type: string): string {
  return isKnownType(type) ? TYPE_LABELS[type] : type;
}

/* The feed's clock is lib/utils.fmtEventTime, shared with the Overview's
   recent-events card. It used to be a private UTC ISO slice here, which put
   the same event at two different times on two pages with nothing on either
   saying which zone it was in (QA round 1, finding #10). Local wall clock wins
   the tie-break: it is the one an operator correlates against. The millisecond
   field went with the ISO slice — seconds are what the density argument
   actually needed. */

function SeverityBadge({ severity }: { severity: string }) {
  const known = isKnownSeverity(severity);
  return (
    <Badge variant={known ? SEVERITY_VARIANT[severity] : "unknown"} dot>
      {known ? SEVERITY_LABELS[severity] : severity}
    </Badge>
  );
}

function EventRow({ event }: { event: LiveEvent }) {
  return (
    <>
      <span className="nums w-24 shrink-0 text-xs text-muted-foreground">{fmtEventTime(event.timestamp)}</span>
      <span className="w-[5.25rem] shrink-0">
        <SeverityBadge severity={event.severity} />
      </span>
      <span className="min-w-0 flex-1 truncate text-sm">{event.summary}</span>
      <span className="hidden w-40 shrink-0 truncate text-xs text-muted-foreground lg:block">
        {typeLabel(event.type)}
      </span>
      <span className="hidden w-52 shrink-0 truncate text-xs text-muted-foreground md:block">{event.scope}</span>
    </>
  );
}

/**
 * AnnotationFeedRow is an operator's note wearing the feed's own columns, so it
 * lines up with the events around it — and nothing else about it is the same.
 * The badge says "Note" rather than a severity (a note has none), the type
 * column says whether it is a moment or a span, and the scope column carries
 * the AUTHOR instead: on this page every annotation is global by construction,
 * so printing "global" five times would spend the column on a constant, while
 * "who wrote this" is the thing an operator reading back actually wants.
 */
function AnnotationFeedRow({ annotation }: { annotation: Annotation }) {
  return (
    <>
      <span className="nums w-24 shrink-0 text-xs text-muted-foreground">{fmtEventTime(annotation.startAt)}</span>
      <span className="w-[5.25rem] shrink-0">
        <Badge variant="neutral" dot>
          Note
        </Badge>
      </span>
      <span className="min-w-0 flex-1 truncate text-sm italic" title={annotation.text}>
        {annotation.text}
      </span>
      <span className="hidden w-40 shrink-0 truncate text-xs text-muted-foreground lg:block">
        {annotation.endAt ? "Annotation (span)" : "Annotation"}
      </span>
      <span className="hidden w-52 shrink-0 truncate text-xs text-muted-foreground md:block">
        {annotation.createdBy}
      </span>
    </>
  );
}

/* The skeleton mirrors the loaded shape — same columns, same row rhythm — so
   the page does not reflow when the first event lands. h-11 is ROW_HEIGHT. */
function FeedSkeleton() {
  return (
    <div role="status" aria-live="polite" className="flex flex-col">
      <span className="sr-only">Connecting to the event stream…</span>
      {Array.from({ length: 8 }, (_, i) => (
        <div key={i} className="flex h-11 items-center gap-4 border-b border-border/60 px-4 last:border-b-0">
          <Skeleton className="h-3 w-20" />
          <Skeleton className="h-4 w-16 rounded-full" />
          <Skeleton className="h-3 flex-1" />
          <Skeleton className="hidden h-3 w-32 lg:block" />
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

/** Why "Load older" is greyed out. The filters are named because they are the
 *  half the operator can change: the scrollback is server-filtered by type and
 *  scope, so an exhausted cursor is a statement about THIS query, not about
 *  the retention window. */
const NOTHING_OLDER = "Nothing older matches the current filters.";

export function LivePage() {
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

  /* Scrollback (GET /api/v1/events, Task 5). `history.nextCursor` doubles as
     both "there is nothing more to load" (exhausted, "" after a real page)
     and "nothing has loaded yet" (the initial value, also "") — both read the
     same on the Load older button, and that is the right default: a button
     that has not yet heard back from its first page should not be clickable
     either. A failed load (the 503 case) also lands on "", not on the retry
     button, because console.database.mode being unset is a deploy-time state
     that a retry click cannot fix. */
  const [history, setHistory] = useState<{ nextCursor: string; loading: boolean; notice: string | null }>({
    nextCursor: "",
    loading: false,
    notice: null,
  });

  /* Server-side filtering keeps a page relevant to what the operator is
     actually looking at; severity has no place here because GET
     /api/v1/events has no severity param — that filter stays client-side only,
     same as it always has. */
  const loadHistory = useCallback(
    async (cursor?: string) => {
      setHistory((h) => ({ ...h, loading: true }));
      try {
        const types = filters.type === "all" ? undefined : [filters.type];
        const scope = filters.scope.trim() || undefined;
        // Engaged: `to=t` turns the feed into a scrollback ENDING at t. The
        // cursor pagination underneath is unchanged — "Load older" still walks
        // backwards from wherever the last page stopped — so the mode only
        // moves where the walk begins, which is exactly what "scrollback around
        // t" means. The bound is exclusive server-side (store.EventFilter.To).
        const page = await getEvents({ types, scope, cursor, ...(at ? { to: at } : {}) });
        // Merged through the exact same dedupe/sort pushEvents uses for the
        // socket: a historical row and one already delivered live share an id
        // ("<seq>-<unixNano>" on both paths), so pushEvents' Set-based dedupe
        // folds them into one row with no special-casing here.
        setEvents((prev) => pushEvents(prev, page.events));
        setHistory({ nextCursor: page.nextCursor, loading: false, notice: null });
      } catch (err) {
        const notice =
          err instanceof ApiError ? (err.problem.detail ?? err.problem.title) : "failed to load event history";
        setHistory({ nextCursor: "", loading: false, notice });
      }
    },
    [filters.type, filters.scope, at],
  );

  // Fetches page one on mount, and again whenever the type or scope filter
  // changes — "one consistent stream" per the operator's current filter,
  // rather than a filtered live half sitting over an unfiltered old half.
  // Gated on historyResolved so a cold /api/v1/config never gets read as "no
  // history": that would skip the very fetch this effect exists to make.
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
  /* Paused holds the rows still; the subscription stays up and arrivals queue
     here instead. The queue is trimmed to the ring cap, so `bufferedRef` counts
     ARRIVALS rather than queue length — otherwise the button would claim
     "2000 buffered" after five thousand events had gone past. */
  const pendingRef = useRef<LiveEvent[]>([]);
  const bufferedRef = useRef(0);
  const pausedRef = useRef(false);
  const scrollRef = useRef<HTMLDivElement>(null);
  /* Events this tab threw away itself, at the slab trim below, before they ever
     reached the ring. countMissedEvents cannot see them — they leave no hole,
     they truncate the bottom of the range — so they are counted here and added
     to the same notice. Without this a tab that spent an hour hidden comes back
     to a feed that is quietly missing thousands of events and says nothing. */
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

  /* Moving through time EMPTIES the ring, unlike a filter change (which merges
     the new page into what is already held). The difference is not stylistic:
     a filter narrows one stream, while `at` redefines WHICH stream this is, and
     rows from after t are precisely what a view of t must not contain. Both
     directions clear — arriving at t drops the live tail, and returning to Live
     drops the historical page rather than leaving it stranded above rows the
     socket is about to append. The queues and their counters go with it: a
     "buffered" number carried over from the other mode counts nothing that is
     still on screen. */
  useEffect(() => {
    setEvents([]);
    inboxRef.current = [];
    pendingRef.current = [];
    bufferedRef.current = 0;
    discardedRef.current = 0;
    setBuffered(0);
    setDiscarded(0);
  }, [atKey]);

  // Subscribed unconditionally, and deliberately NOT through useWsTopic: that
  // hook keeps only the latest envelope, which is right for whole-state
  // snapshot topics and wrong for an append-only feed — two events in one tick
  // would collapse into a single state update. The topic is also valid on a
  // replica whose own ingester is down, because another replica's events still
  // arrive over the Valkey bus; `realtime` therefore drives the badge and the
  // copy, never the subscription. StrictMode double-invokes this in dev; the
  // cleanup makes that a no-op.
  //
  // The ONE thing that stops it is the Time Machine: engaged, this page is a
  // scrollback ending at t, and a live tail appending events from now on top of
  // it would be the mode lying about what it shows. Not paused — pause holds
  // arrivals and replays them on resume, which is the opposite of what is
  // wanted here — simply not subscribed, so returning to Live re-runs this
  // effect and the feed picks up from a clean ring.
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
      // A rejection belongs to the connection that produced it. Reaching open
      // again means the subscribe was accepted, so the alert has to go —
      // otherwise one transient error pins a red banner over a working feed for
      // the life of the page.
      if (s === "open") setTopicError(null);
    });
    const off = ws.subscribe<LiveEvent>(TOPIC_LIVE, (env: WsEnvelope<LiveEvent>) => {
      if (env.type === "error") {
        setTopicError(typeof env.data === "string" ? env.data : "the server rejected the live topic");
        return;
      }
      if (env.type !== "event") return;
      if (pausedRef.current) {
        bufferedRef.current += 1;
        pendingRef.current.push(env.data);
        // Trimmed in slabs rather than per event: a hidden tab gets no
        // animation frames at all, so without a bound these queues would grow
        // for as long as it stays hidden. Slicing at twice the cap keeps the
        // copy amortised while holding memory to a small multiple of the ring.
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
      // Set the ref here rather than from an effect: the socket callback can
      // fire between this click and the effect that would have synced it, and
      // those events would slip into the feed after the operator asked it to
      // hold still.
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
  /* GLOBAL annotations only, and that is the whole story on this page: the feed
     is fleet-wide, so a note scoped to one node or one pair belongs on that
     object's card, not interleaved with everybody else's events.
     Read-only here — creating a mark needs a time to pin it to, and this page's
     one useful default (the row you are looking at) is a canvas-free list with
     no such affordance in M5. The cards and Explore own create/delete. */
  const { annotations } = useAnnotations(GLOBAL_SCOPE, LIVE_ANNOTATION_RANGE_SECONDS);
  const rows = useMemo(() => mergeFeedRows(visible, annotations), [visible, annotations]);
  // Two ways to lose an event, one number: a hole in the controller's numbering
  // (something went missing between the controller and this tab) and a slab
  // trim (this tab could not keep up and dropped its own backlog).
  const gaps = useMemo(() => countMissedEvents(events), [events]);
  const missed = gaps + discarded;

  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => ROW_HEIGHT,
    overscan: 12,
  });

  /* Rows are PREPENDED (newest first). At the top that is exactly what a live
     feed should do; further down it would shove whatever the operator is
     reading out from under them.

     The anchor is the IDENTITY of the row at the top of the viewport, never a
     row count: once the ring is full every arrival also evicts one from the
     tail, so the length stops changing and a length-based anchor silently stops
     compensating at exactly the moment there is most to scroll through. It is
     re-recorded on every scroll and after every merge, so it also survives a
     late event filed into the middle of the list rather than at the head. */
  const filterKey = `${filters.type} ${filters.severity} ${filters.scope}`;
  const anchorRef = useRef<{ key: string; id: string | null; index: number }>({
    key: filterKey,
    id: null,
    index: 0,
  });
  // The anchor tracks ROWS, not events: an annotation filed into the middle of
  // the scrollback shifts everything below it exactly the way a late event
  // does, and an anchor that could not see it would compensate by the wrong
  // number.
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

  /* Exhausted: the walk has nowhere left to go under the CURRENT filters —
     either the server answered an empty cursor or no page has landed yet. Both
     read the same on the button, and both are the state the note below
     explains. `loading` is a third thing and keeps its own label. */
  const exhausted = history.nextCursor === "" && !history.loading;

  const clearFilters = useCallback(() => setFilters(EMPTY_FILTERS), []);
  // Only a filter can empty a non-empty ring, so `events.length > 0 &&
  // visible.length === 0` below is the "filtered to nothing" state by
  // construction — no separate flag to keep in sync.
  //
  // Unresolved capabilities read as "connecting", never as "no realtime": until
  // /api/v1/version answers, false means unknown, and treating unknown as an
  // answer is what flashes a warning card on every cold load.
  // Engaged there is nothing to connect TO — the rows come from GET
  // /api/v1/events alone — so the socket's state must not be allowed to hold
  // this page on a skeleton that would never resolve.
  const connecting = events.length === 0 && !engaged && (!resolved || (realtime && !connected));

  return (
    <PageShell
      title="Live"
      description={
        at
          ? `Scrollback ending ${at.toLocaleString()}, newest first. The live tail is off while the Time Machine is engaged — "Load older" walks back from here.`
          : `Controller events pushed over the WebSocket, newest first. The browser holds the most recent ${LIVE_RING_CAP}; anything older is Prometheus' job.`
      }
      actions={
        /* ONE slot, both modes (QA round 1, finding #13). The toolbar used to
           be a bare fragment whose width changed with the transport badge, and
           in a wrapping header that moved the filters and Pause to a different
           line the moment the Time Machine engaged — controls relocating under
           the cursor as a side effect of a mode change. The container is now a
           real element in a fixed position and the badge lives in a slot that
           reserves its width, so what changes is the badge and nothing else. */
        <div data-testid="live-toolbar" className="flex flex-wrap items-center gap-2">
          <Segmented
            aria-label="Severity"
            options={[
              { value: "all", label: "All" },
              ...LIVE_EVENT_SEVERITIES.map((s) => ({ value: s, label: SEVERITY_LABELS[s] })),
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
              aria-label="Type"
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
              <option value="all">All types</option>
              {LIVE_EVENT_TYPES.map((t) => (
                <option key={t} value={t}>
                  {TYPE_LABELS[t]}
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
            {paused ? (buffered > 0 ? `Resume (${buffered} buffered)` : "Resume") : "Pause"}
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
              The slot keeps its width in every case, so nothing to its left
              moves when the badge goes. */}
          <span data-testid="live-transport-slot" className="inline-flex min-w-[7.5rem] justify-end">
            {engaged || paused ? null : resolved ? (
              <RealtimeBadge realtime={realtime && connected} />
            ) : (
              <Badge variant="neutral" dot>
                Connecting…
              </Badge>
            )}
          </span>
        </div>
      }
    >
      {topicError ? (
        <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
          <p className="text-sm font-medium">The live topic was rejected</p>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{topicError}</p>
        </Card>
      ) : null}

      {/* Not while engaged: "no events will arrive here" is true by design in
          that mode and reads as a fault. */}
      {!engaged && resolved && !realtime ? (
        <Card role="status" className="border-l-4 border-l-health-warn bg-health-warn-soft/40 p-5">
          <p className="text-sm font-medium">This replica is not receiving the controller event stream</p>
          <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">
            No events will arrive here while that is the case — the feed is not broken, it is unfed. Matrix and
            Topology fall back to 15s polling, and the feed resumes on its own within 15s of the stream coming back.
          </p>
        </Card>
      ) : null}

      {/* Non-fatal: the scrollback endpoint failing (503, history disabled)
          says nothing about the live feed, which keeps working off the socket
          regardless. */}
      {history.notice ? (
        <Card role="status" className="border-l-4 border-l-health-warn bg-health-warn-soft/40 p-5">
          <p className="text-sm font-medium">Event history is unavailable</p>
          <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{history.notice}</p>
        </Card>
      ) : null}

      <Card className="overflow-hidden p-0">
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-border px-4 py-3">
          <label className="relative flex items-center">
            <span className="sr-only">Scope contains</span>
            <Search aria-hidden="true" className="pointer-events-none absolute left-2.5 size-3.5 text-muted-foreground" />
            <input
              value={filters.scope}
              onChange={(e) => setFilters((f) => ({ ...f, scope: e.target.value }))}
              placeholder="Filter by scope — node-a→node-b"
              className="h-8 w-64 rounded-md bg-surface-2 pl-8 pr-2 text-sm placeholder:text-muted-foreground/70 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            />
          </label>

          {historyAvailable ? (
            /* A disabled control that will not say why is a dead end: the
               cursor is exhausted for the CURRENT filters, which is a fact the
               operator can act on (widen them) and could not previously see
               (QA round 1, finding #16). The sentence is visually hidden and
               therefore part of the accessible name, with a title for the
               pointer — the loading state needs neither, its label already
               says what it is doing. */
            <Button
              variant="outline"
              size="sm"
              disabled={exhausted || history.loading}
              title={exhausted ? NOTHING_OLDER : undefined}
              onClick={() => loadHistory(history.nextCursor)}
            >
              {history.loading ? "Loading older…" : "Load older"}
              {exhausted ? <span className="sr-only">{NOTHING_OLDER}</span> : null}
            </Button>
          ) : null}

          {paused ? <Badge variant="warn" dot>{`Paused · ${buffered} buffered`}</Badge> : null}

          {missed > 0 ? (
            <span
              role="status"
              title={
                discarded > 0
                  ? `${gaps} from holes in the controller's numbering, ${discarded} dropped by this tab because arrivals outran it (a hidden tab gets no frames to render in).`
                  : "Holes in the controller's event numbering — something went missing between the controller and this tab."
              }
              className="flex items-center gap-1.5 text-xs text-muted-foreground"
            >
              <TriangleAlert aria-hidden="true" className="size-3.5 text-health-warn" />
              {`${missed} event${missed === 1 ? "" : "s"} may have been missed`}
            </span>
          ) : null}

          <p className="nums ml-auto text-xs text-muted-foreground">
            {`Showing ${visible.length} of ${events.length} events · capped at ${LIVE_RING_CAP}`}
          </p>
        </div>

        <div
          aria-hidden="true"
          className="flex items-center gap-4 border-b border-border px-4 py-2 text-[11px] font-medium text-muted-foreground"
        >
          <span className="w-24 shrink-0">Time</span>
          <span className="w-[5.25rem] shrink-0">Severity</span>
          <span className="min-w-0 flex-1">Summary</span>
          <span className="hidden w-40 shrink-0 lg:block">Type</span>
          <span className="hidden w-52 shrink-0 md:block">Scope</span>
        </div>

        {connecting ? <FeedSkeleton /> : null}

        {/* rows.length rather than events.length: a window with no events but
            an operator note in it is not an empty feed. */}
        {!connecting && events.length === 0 && rows.length === 0 ? (
          <BlankSlate
            title={engaged ? "No events at or before this time" : "Waiting for events"}
            body={
              engaged
                ? "Event history goes back as far as console.database.retentionDays and no further — an instant older than that has nothing to show, and so does a quiet cluster."
                : "Nothing has been pushed since this page opened. Topology changes, observed checks and MTR runs land here the moment the controller emits them."
            }
          />
        ) : null}

        {!connecting && events.length > 0 && visible.length === 0 ? (
          <BlankSlate
            title="No events match these filters"
            body={`${events.length} held events, none of them matching. Widen the filters to see them again.`}
            action={
              <Button variant="outline" size="sm" onClick={clearFilters}>
                Clear filters
              </Button>
            }
          />
        ) : null}

        {rows.length > 0 ? (
          <div ref={scrollRef} onScroll={recordAnchor} className="h-[min(60vh,40rem)] overflow-auto">
            <ul
              role="log"
              // role="log" names the region for what it is: an append-only feed
              // that a screen-reader user navigates deliberately. aria-live is
              // pinned OFF rather than left to log's implicit polite — a stream
              // that announces every arriving row talks over the operator
              // without pause. The counts line and the missed-events notice are
              // the summaries worth hearing, and they carry their own
              // role="status".
              aria-live="off"
              aria-label="Event feed, newest first"
              className="relative m-0 list-none p-0"
              style={{ height: `${virtualizer.getTotalSize()}px` }}
            >
              {virtualizer.getVirtualItems().map((item) => {
                const row = rows[item.index];
                return (
                  <li
                    key={row.key}
                    // For an event the key is the controller-assigned
                    // "<seq>-<unixNano>", the same string the hub dedupes on —
                    // identical on every console replica, which keeps React's
                    // reconciliation stable as the ring shifts. An index key
                    // would not. Annotations carry a namespaced id for the same
                    // stability (mergeFeedRows).
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
