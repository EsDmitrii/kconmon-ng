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
import { useCapabilities } from "@/hooks/use-capabilities";
import { getWsClient } from "@/hooks/use-ws-topic";
import {
  LIVE_EVENT_SEVERITIES,
  LIVE_EVENT_TYPES,
  type LiveEvent,
  type LiveEventSeverity,
  type LiveEventType,
} from "@/lib/types";
import { cn } from "@/lib/utils";
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

function fmtTime(timestamp: string): string {
  const d = new Date(timestamp);
  return Number.isNaN(d.getTime()) ? timestamp : d.toISOString().slice(11, 23);
}

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
      <span className="nums w-24 shrink-0 text-xs text-muted-foreground">{fmtTime(event.timestamp)}</span>
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

export function LivePage() {
  const { realtime, resolved } = useCapabilities();
  const [events, setEvents] = useState<LiveEvent[]>([]);
  const [connected, setConnected] = useState(false);
  const [topicError, setTopicError] = useState<string | null>(null);
  const [paused, setPaused] = useState(false);
  const [buffered, setBuffered] = useState(0);
  const [filters, setFilters] = useState<LiveFilters>(EMPTY_FILTERS);

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

  // Subscribed unconditionally, and deliberately NOT through useWsTopic: that
  // hook keeps only the latest envelope, which is right for whole-state
  // snapshot topics and wrong for an append-only feed — two events in one tick
  // would collapse into a single state update. The topic is also valid on a
  // replica whose own ingester is down, because another replica's events still
  // arrive over the Valkey bus; `realtime` therefore drives the badge and the
  // copy, never the subscription. StrictMode double-invokes this in dev; the
  // cleanup makes that a no-op.
  useEffect(() => {
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
  }, [flush]);

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
  // Two ways to lose an event, one number: a hole in the controller's numbering
  // (something went missing between the controller and this tab) and a slab
  // trim (this tab could not keep up and dropped its own backlog).
  const gaps = useMemo(() => countMissedEvents(events), [events]);
  const missed = gaps + discarded;

  const virtualizer = useVirtualizer({
    count: visible.length,
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
  const feedRef = useRef({ visible, filterKey });
  feedRef.current = { visible, filterKey };

  const recordAnchor = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    const { visible: rows, filterKey: key } = feedRef.current;
    const index = Math.max(0, Math.round(el.scrollTop / ROW_HEIGHT));
    anchorRef.current = { key, index, id: rows[index]?.id ?? null };
  }, []);

  useLayoutEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const anchor = anchorRef.current;
    if (anchor.key !== filterKey) {
      // A new filter is a new list; there is nothing to hold in place.
      el.scrollTop = 0;
    } else if (el.scrollTop > 0 && anchor.id !== null) {
      const now = visible.findIndex((e) => e.id === anchor.id);
      // Gone (evicted off the tail) reads as -1: nothing left to anchor to, so
      // leave the offset alone and re-record against whatever is there now.
      if (now >= 0 && now !== anchor.index) el.scrollTop += (now - anchor.index) * ROW_HEIGHT;
    }
    recordAnchor();
  }, [filterKey, visible, recordAnchor]);

  const clearFilters = useCallback(() => setFilters(EMPTY_FILTERS), []);
  // Only a filter can empty a non-empty ring, so `events.length > 0 &&
  // visible.length === 0` below is the "filtered to nothing" state by
  // construction — no separate flag to keep in sync.
  //
  // Unresolved capabilities read as "connecting", never as "no realtime": until
  // /api/v1/version answers, false means unknown, and treating unknown as an
  // answer is what flashes a warning card on every cold load.
  const connecting = events.length === 0 && (!resolved || (realtime && !connected));

  return (
    <PageShell
      title="Live"
      description={`Controller events pushed over the WebSocket, newest first. The browser holds the most recent ${LIVE_RING_CAP}; anything older is Prometheus' job.`}
      actions={
        <>
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
          <Button variant="outline" size="sm" onClick={togglePause}>
            {paused ? <Play aria-hidden="true" className="size-3.5" /> : <Pause aria-hidden="true" className="size-3.5" />}
            {paused ? (buffered > 0 ? `Resume (${buffered} buffered)` : "Resume") : "Pause"}
          </Button>
          {/* Pushed or not — the badge states the transport, never colour alone.
              While the capability is still unknown it says so rather than
              guessing "delayed". */}
          {resolved ? (
            <RealtimeBadge realtime={realtime && connected} />
          ) : (
            <Badge variant="neutral" dot>
              Connecting…
            </Badge>
          )}
        </>
      }
    >
      {topicError ? (
        <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
          <p className="text-sm font-medium">The live topic was rejected</p>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{topicError}</p>
        </Card>
      ) : null}

      {resolved && !realtime ? (
        <Card role="status" className="border-l-4 border-l-health-warn bg-health-warn-soft/40 p-5">
          <p className="text-sm font-medium">This replica is not receiving the controller event stream</p>
          <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">
            No events will arrive here while that is the case — the feed is not broken, it is unfed. Matrix and
            Topology fall back to 15s polling, and the feed resumes on its own within 15s of the stream coming back.
          </p>
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

        {!connecting && events.length === 0 ? (
          <BlankSlate
            title="Waiting for events"
            body="Nothing has been pushed since this page opened. Topology changes, observed checks and MTR runs land here the moment the controller emits them."
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

        {visible.length > 0 ? (
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
                const event = visible[item.index];
                return (
                  <li
                    key={event.id}
                    // key is the controller-assigned "<seq>-<unixNano>", the
                    // same string the hub dedupes on — identical on every
                    // console replica, which keeps React's reconciliation
                    // stable as the ring shifts. An index key would not.
                    style={{ height: `${item.size}px`, transform: `translateY(${item.start}px)` }}
                    className={cn(
                      "absolute left-0 top-0 flex w-full items-center gap-4 px-4",
                      "border-b border-border/60",
                    )}
                  >
                    <EventRow event={event} />
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
