// ws.ts — the console's single multiplexed WebSocket client (ADR-003): ONE
// socket per WsClient instance, N topic subscriptions multiplexed over it,
// reconnect on the repo's standard 1s→15s doubling backoff, and per-topic
// `lastSeq` resume so a reconnect asks the hub to replay what this tab missed.
//
// The wire types mirror internal/console/ws.Envelope and ws.ClientMessage
// field-for-field; there is no codegen (see FRONTEND.md "Data layer").

/** Envelope is what the browser receives: internal/console/ws.Envelope. */
export interface WsEnvelope<T = unknown> {
  topic: string;
  type: "snapshot" | "delta" | "event" | "error";
  seq: number;
  data: T;
}

export type WsState = "connecting" | "open" | "closed";

export interface WsClientOptions {
  /** Defaults to wsUrl(). */
  url?: string;
  /**
   * Constructor to dial with. Exists so tests inject a hand-written double
   * (lib/fake-websocket.ts) instead of jsdom's real WebSocket.
   */
  WebSocketImpl?: typeof WebSocket;
}

/** Topic names are the wire contract with internal/console/ws (ws.TopicLive, ws.TopicTopology). */
export const TOPIC_LIVE = "live";
export const TOPIC_TOPOLOGY = "topology";

const RECONNECT_MIN_MS = 1_000;
const RECONNECT_MAX_MS = 15_000;

type Handler = (env: WsEnvelope) => void;

/** ClientMessage is what the browser sends: internal/console/ws.ClientMessage. */
interface ClientMessage {
  action: "subscribe" | "unsubscribe";
  topic: string;
  lastSeq?: number;
}

/**
 * isSnapshotTopic classifies a topic for the seq rule below. Snapshot topics
 * carry the whole state in every frame ("topology", "matrix:{proto}:pod");
 * "live" carries an append-only set of events.
 */
function isSnapshotTopic(topic: string): boolean {
  return topic === TOPIC_TOPOLOGY || topic.startsWith("matrix:");
}

/**
 * wsUrl derives ws(s)://<host>/ws from the page location. `/ws` is top level,
 * not under /api/v1 (docs/console/architecture/API.md, ADR-003). The argument
 * is only for tests — jsdom's window.location cannot be stubbed.
 */
export function wsUrl(loc: { protocol: string; host: string } = window.location): string {
  const scheme = loc.protocol === "https:" ? "wss:" : "ws:";
  return `${scheme}//${loc.host}/ws`;
}

export class WsClient {
  private readonly url: string;
  private readonly impl: typeof WebSocket;
  private readonly handlers = new Map<string, Set<Handler>>();
  /**
   * Highest seq ever seen per topic — the replay cursor sent as `lastSeq` on
   * (re)subscribe. It is a MAX, not a last-write: a frame that arrives out of
   * order must not rewind the cursor and make the hub replay what this tab
   * already has.
   */
  private readonly resumeSeq = new Map<string, number>();
  /**
   * Highest seq DELIVERED per topic on the current connection, used only to
   * drop stale snapshot frames. Deliberately reset per connection: seq counters
   * are hub-local, so a reconnect that lands on another console replica starts
   * over at a low seq, and a persistent watermark would silently swallow every
   * snapshot from that replica. The inversion this guards against
   * (replay vs. concurrent Broadcast) happens inside one connection anyway.
   */
  private deliveredSeq = new Map<string, number>();
  /**
   * Last delivered envelope per SNAPSHOT topic, so a handler that subscribes to
   * an already-subscribed topic gets the current state immediately instead of
   * rendering empty until the next push (≤15s for snapshots). Kept across
   * reconnects: a stale whole state beats no state during an outage.
   *
   * Deliberately NOT kept for "live": that topic is an append-only feed, not a
   * current state, so a one-frame cache would show a late subscriber a single
   * arbitrary event. Owning live history is Task 17's store, not the transport.
   */
  private readonly snapshotCache = new Map<string, WsEnvelope>();
  private readonly stateListeners = new Set<(s: WsState) => void>();

  private socket: WebSocket | null = null;
  private currentState: WsState = "closed";
  private backoffMs = RECONNECT_MIN_MS;
  private retryTimer: ReturnType<typeof setTimeout> | null = null;
  private disposed = false;

  constructor(opts: WsClientOptions = {}) {
    this.url = opts.url ?? wsUrl();
    this.impl = opts.WebSocketImpl ?? WebSocket;
  }

  get state(): WsState {
    return this.currentState;
  }

  /**
   * lastSeqFor exposes the replay cursor so a consumer can reason about the
   * stream position. Note the envelope seq is gapless by construction — a
   * bus-side drop is invisible in it — so the loss signal for the live feed is
   * a gap in the controller-assigned `LiveEvent.seq` INSIDE the payload, which
   * only a consumer that decodes the payload can detect.
   */
  lastSeqFor(topic: string): number {
    return this.resumeSeq.get(topic) ?? 0;
  }

  /**
   * subscribe registers onMessage for topic, dialling the shared socket on
   * first use. The returned unsubscribe is idempotent; it only sends an
   * `unsubscribe` frame once the last local listener for that topic is gone.
   *
   * A handler added to a topic somebody else already subscribed to gets the
   * cached snapshot synchronously (snapshot topics only) — the server would not
   * resend one, so without this the new consumer renders empty until the next
   * push. Only the new handler is called, and no second wire subscribe is sent.
   */
  subscribe<T>(topic: string, onMessage: (env: WsEnvelope<T>) => void): () => void {
    const handler = onMessage as Handler;
    let listeners = this.handlers.get(topic);
    if (!listeners) {
      listeners = new Set<Handler>();
      this.handlers.set(topic, listeners);
    }
    listeners.add(handler);
    const isFirstForTopic = listeners.size === 1;

    this.connect();
    if (isFirstForTopic) {
      if (this.currentState === "open") this.sendSubscribe(topic);
    } else {
      const cached = this.snapshotCache.get(topic);
      if (cached) handler(cached);
    }

    let released = false;
    return () => {
      if (released) return;
      released = true;
      const current = this.handlers.get(topic);
      if (!current) return;
      current.delete(handler);
      if (current.size > 0) return;
      this.handlers.delete(topic);
      this.resumeSeq.delete(topic);
      this.deliveredSeq.delete(topic);
      this.snapshotCache.delete(topic);
      if (this.currentState === "open") this.send({ action: "unsubscribe", topic });
    };
  }

  onStateChange(fn: (s: WsState) => void): () => void {
    this.stateListeners.add(fn);
    return () => {
      this.stateListeners.delete(fn);
    };
  }

  /** close disposes the client permanently: no further reconnect is scheduled. */
  close(): void {
    this.disposed = true;
    if (this.retryTimer !== null) {
      clearTimeout(this.retryTimer);
      this.retryTimer = null;
    }
    const socket = this.socket;
    this.socket = null;
    if (socket) {
      // Drop the listeners before closing: the double (and a real WebSocket
      // that closes synchronously) would otherwise still fire onclose into a
      // disposed client.
      socket.onopen = null;
      socket.onmessage = null;
      socket.onerror = null;
      socket.onclose = null;
      socket.close();
    }
    this.setState("closed");
  }

  private connect(): void {
    // A pending retry timer counts as "already connecting": a component that
    // mounts mid-outage must wait out the backoff rather than dial immediately,
    // otherwise every mount during an outage is an extra dial.
    if (this.disposed || this.socket !== null || this.retryTimer !== null) return;

    // The constructor itself can throw — a CSP connect-src denial is the common
    // one. Left uncaught inside the retry callback (which has already nulled
    // retryTimer) the exception would escape with no reconnect armed, wedging
    // the client for the life of the page.
    let socket: WebSocket;
    try {
      socket = new this.impl(this.url);
    } catch (err) {
      console.warn("console websocket: dial rejected", err);
      this.setState("closed");
      this.scheduleReconnect();
      return;
    }
    this.socket = socket;
    // Snapshot staleness is judged within one connection only (see deliveredSeq).
    this.deliveredSeq = new Map<string, number>();
    this.setState("connecting");

    socket.onopen = () => {
      if (this.socket !== socket) return;
      this.backoffMs = RECONNECT_MIN_MS;
      this.setState("open");
      // Re-subscribe every live topic. On a fresh socket lastSeq is 0 and the
      // frame carries no lastSeq at all; after a drop it carries the highest
      // seq this tab saw, which the hub uses as its replay hint.
      for (const topic of this.handlers.keys()) this.sendSubscribe(topic);
    };

    socket.onmessage = (ev: MessageEvent) => {
      if (this.socket !== socket) return;
      this.dispatch(ev.data);
    };

    socket.onerror = () => {
      if (this.socket !== socket) return;
      // onclose always follows, and it owns the reconnect decision; this is
      // diagnostics only.
      console.warn("console websocket error", this.url);
    };

    socket.onclose = () => {
      if (this.socket !== socket) return;
      this.socket = null;
      this.setState("closed");
      this.scheduleReconnect();
    };
  }

  private scheduleReconnect(): void {
    if (this.disposed || this.handlers.size === 0 || this.retryTimer !== null) return;
    const delay = this.backoffMs;
    this.backoffMs = Math.min(this.backoffMs * 2, RECONNECT_MAX_MS);
    this.retryTimer = setTimeout(() => {
      this.retryTimer = null;
      this.connect();
    }, delay);
  }

  /**
   * dispatch applies the hub's consumer contract (internal/console/ws/hub.go,
   * `func (h *Hub) subscribe`): delivery is exactly-once but NOT ordered, and
   * per-topic seq is the authoritative order.
   *
   *   - Snapshot topics: every frame is the whole state → keep only the highest
   *     seq and discard lower, or an inverted pair leaves the OLDER state
   *     rendered until the next push.
   *   - live: frames are an append-only SET → deliver every one of them. A
   *     Broadcast racing the replay can deliver seq 6 before replayed 1..5, and
   *     dropping those five would lose event rows permanently. Ordering and
   *     dedupe by LiveEvent.id belong to the consumer that decodes the payload.
   */
  private dispatch(raw: unknown): void {
    if (typeof raw !== "string") return;
    let env: WsEnvelope;
    try {
      env = JSON.parse(raw) as WsEnvelope;
    } catch {
      // Length only: the frame can carry topology/identity data, and this
      // warning is diagnostics, not an audit log.
      console.warn("console websocket: unparseable frame", { bytes: raw.length });
      return;
    }
    if (typeof env?.topic !== "string") return;

    const listeners = this.handlers.get(env.topic);
    // A straggler frame for an already-unsubscribed topic must not resurrect a
    // replay cursor that nobody will ever use.
    if (!listeners) return;

    // An error envelope carries no position in the topic stream (the hub sends
    // it with seq 0), so it moves neither cursor — but it is still delivered,
    // because "unknown topic"/"bad action" is exactly what a consumer wants to
    // surface.
    if (env.type !== "error" && typeof env.seq === "number") {
      const snapshot = isSnapshotTopic(env.topic);
      if (snapshot && env.seq <= (this.deliveredSeq.get(env.topic) ?? 0)) return;
      this.deliveredSeq.set(env.topic, Math.max(this.deliveredSeq.get(env.topic) ?? 0, env.seq));
      this.resumeSeq.set(env.topic, Math.max(this.resumeSeq.get(env.topic) ?? 0, env.seq));
      if (snapshot) this.snapshotCache.set(env.topic, env);
    }

    for (const handler of [...listeners]) handler(env);
  }

  /**
   * sendSubscribe frames a subscribe, with a resume cursor for the live topic
   * only.
   *
   * Snapshot topics never resume. Their ring holds a single entry — the current
   * whole state — which a reconnecting tab always wants. Sending the sticky
   * resume cursor would let ONE failover onto a lower-seq replica trip the
   * hub's `seq > lastSeq` replay filter, and because the cursor only ever grows
   * that suppression would repeat on every later reconnect, leaving the page up
   * to a full push interval stale each time. Re-receiving a whole state we
   * already have costs nothing: deliveredSeq drops it on the same replica.
   */
  private sendSubscribe(topic: string): void {
    const lastSeq = isSnapshotTopic(topic) ? 0 : this.lastSeqFor(topic);
    this.send(lastSeq > 0 ? { action: "subscribe", topic, lastSeq } : { action: "subscribe", topic });
  }

  private send(msg: ClientMessage): void {
    this.socket?.send(JSON.stringify(msg));
  }

  private setState(next: WsState): void {
    if (this.currentState === next) return;
    this.currentState = next;
    for (const fn of [...this.stateListeners]) fn(next);
  }
}
