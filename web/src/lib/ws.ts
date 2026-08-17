// ws.ts — the console's single multiplexed WebSocket client (ADR-003): ONE socket per WsClient
// instance.

/**
 * Envelope is what the browser receives: internal/console/ws.Envelope; "closed" mirrors
 * ws.TypeClosed (hub.go) -- a topic's terminal control frame.
 */
export interface WsEnvelope<T = unknown> {
  topic: string;
  type: "snapshot" | "delta" | "event" | "error" | "closed";
  seq: number;
  /** Which hub numbered this seq; see ClientMessage.epoch. Absent on frames from an older server. */
  epoch?: string;
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
  /* Whose numbering `lastSeq` belongs to. Sequence numbers are per-hub and start at 1, so a cursor
     from one console replica means nothing on another: after a rollout a tab used to ask its new
     replica to replay "everything after 412", that replica's counter was at 7, and it replayed
     NOTHING — the whole gap lost in silence, while the feed looked alive because fresh broadcasts
     arrive regardless of seq. The server ignores a cursor whose epoch is not its own. */
  epoch?: string;
}

/**
 * isSnapshotTopic classifies a topic for the seq rule below; snapshot topics carry the whole state
 * in every frame ("topology", "matrix:{proto}:pod").
 */
function isSnapshotTopic(topic: string): boolean {
  return topic === TOPIC_TOPOLOGY || topic.startsWith("matrix:");
}

/**
 * wsUrl derives ws(s)://<host>/ws from the page location; the argument is only for tests — jsdom's
 * window.location cannot be stubbed.
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
   * It is a MAX, not a last-write: a frame that arrives out of order must not rewind the cursor and
   * make the hub replay what this tab already has.
   */
  private readonly resumeSeq = new Map<string, number>();
  /** The hub epoch every cursor in resumeSeq belongs to; see ClientMessage.epoch. */
  private epoch = "";
  /** Highest seq DELIVERED per topic on the current connection, used only to drop stale snapshot frames. */
  private deliveredSeq = new Map<string, number>();
  /** Last delivered envelope per SNAPSHOT topic. */
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
   * lastSeqFor exposes the replay cursor so a consumer can reason about the stream position; note
   * the envelope seq is gapless by construction — a bus-side drop is invisible.
   */
  lastSeqFor(topic: string): number {
    return this.resumeSeq.get(topic) ?? 0;
  }

  /**
   * The returned unsubscribe is idempotent; it only sends an `unsubscribe` frame once the last
   * local listener for that topic is gone.
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

    // The constructor itself can throw — a CSP connect-src denial is the common one.
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

  /** dispatch applies the hub's consumer contract (internal/console/ws/hub.go, `func (h *Hub) subscribe`). */
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

    // An error envelope carries no position in the topic stream (the hub sends it with seq 0), so
    // it moves neither cursor.
    if (env.type !== "error" && typeof env.seq === "number") {
      const snapshot = isSnapshotTopic(env.topic);
      /* A new epoch is a NEW numbering: every cursor held for the old one is meaningless, and
         keeping it would make the next resume ask for frames that will never come. */
      if (env.epoch && env.epoch !== this.epoch) {
        this.epoch = env.epoch;
        this.resumeSeq.clear();
        this.deliveredSeq.clear();
      }
      if (snapshot && env.seq <= (this.deliveredSeq.get(env.topic) ?? 0)) return;
      this.deliveredSeq.set(env.topic, Math.max(this.deliveredSeq.get(env.topic) ?? 0, env.seq));
      this.resumeSeq.set(env.topic, Math.max(this.resumeSeq.get(env.topic) ?? 0, env.seq));
      if (snapshot) this.snapshotCache.set(env.topic, env);
    }

    for (const handler of [...listeners]) handler(env);
  }

  /** sendSubscribe frames a subscribe, with a resume cursor for the live topic only. */
  private sendSubscribe(topic: string): void {
    const lastSeq = isSnapshotTopic(topic) ? 0 : this.lastSeqFor(topic);
    // The cursor travels with the epoch it was issued under; see ClientMessage.epoch. A server that
    // does not stamp one leaves this.epoch empty, and the frame is exactly what it always was.
    if (lastSeq <= 0) {
      this.send({ action: "subscribe", topic });
      return;
    }
    this.send(this.epoch ? { action: "subscribe", topic, lastSeq, epoch: this.epoch } : { action: "subscribe", topic, lastSeq });
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
