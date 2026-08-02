import { useEffect, useState } from "react";
import { WsClient, type WsEnvelope } from "@/lib/ws";

let client: WsClient | null = null;

/**
 * getWsClient returns the page-wide WsClient, created on first use, so every
 * hook and the Live page multiplex over ONE socket (ADR-003) instead of one
 * socket per subscription.
 *
 * Creation is LAZY on purpose, and nothing but resetWsClient() ever calls
 * close(). Both matter:
 *   - lazy: the WsClient constructor captures the WebSocket impl, so building
 *     it at module scope would capture jsdom's real WebSocket before a test's
 *     vi.stubGlobal("WebSocket", FakeSocket) ever ran.
 *   - no close on unsubscribe: WsClient.close() is irreversible (it sets
 *     `disposed` and no reconnect is ever scheduled again). A React 19
 *     StrictMode double-mount tears every effect down once; if that teardown
 *     closed the singleton, realtime would be dead page-wide for the rest of
 *     the session. useWsTopic's cleanup therefore only unsubscribes.
 */
export function getWsClient(): WsClient {
  client ??= new WsClient();
  return client;
}

/**
 * resetWsClient closes and drops the singleton. Test-only seam: a vitest file
 * that installs its own WebSocket double must not inherit a client built from
 * the previous file's double.
 */
export function resetWsClient(): void {
  client?.close();
  client = null;
}

export interface WsTopicResult<T> {
  data: T | undefined;
  connected: boolean;
  lastSeq: number;
}

/**
 * The value is stored WITH the topic it arrived on. A topic change re-runs the
 * effect, but the state update that clears the old value can only land after
 * the render that already asked for the new topic — so a plain `data` state
 * would hand the caller the PREVIOUS topic's payload for at least one render
 * (e.g. the TCP matrix while the page has already switched to UDP). Tagging the
 * value makes that structurally impossible: a mismatched tag reads as "nothing
 * yet", which is the truth.
 */
interface TopicValue<T> {
  topic: string;
  data?: T;
  seq: number;
}

/**
 * useWsTopic subscribes to a WebSocket topic and keeps the LATEST envelope's
 * data. That is exactly right for the snapshot topics (topology, matrix:*),
 * where every message is a full snapshot (Decision 6) and older ones are
 * worthless — a snapshot is consumed as whole-state replacement, never merged,
 * and seq is never compared across connections. It is deliberately not used
 * for `live`: two events arriving in one tick would collapse into a single
 * state update, so pages/live.tsx subscribes to getWsClient() directly and
 * accumulates instead.
 *
 * opts.enabled=false is a hard off switch — no socket is opened at all, which
 * is what keeps the M1-only path (no realtime capability) socket-free.
 */
export function useWsTopic<T>(topic: string, opts?: { enabled?: boolean }): WsTopicResult<T> {
  const enabled = opts?.enabled ?? true;
  const [value, setValue] = useState<TopicValue<T>>({ topic, seq: 0 });
  const [connected, setConnected] = useState(false);

  useEffect(() => {
    if (!enabled) {
      setConnected(false);
      // Drop the retained payload too: state must not outlive the subscription
      // that produced it on ANY axis. Without this, a realtime outage-and-
      // recovery re-runs consumers' seeding effects with the pre-outage
      // snapshot (same topic, tag matches!) and overwrites fresher polled
      // data with it.
      setValue({ topic, seq: 0 });
      return;
    }
    const ws = getWsClient();
    setConnected(ws.state === "open");
    const offState = ws.onStateChange((s) => setConnected(s === "open"));
    const off = ws.subscribe<T>(topic, (env: WsEnvelope<T>) => {
      if (env.type === "error") {
        console.warn("console websocket: server rejected topic", topic, env.data);
        return;
      }
      setValue({ topic, data: env.data, seq: env.seq });
    });
    // Both teardowns are idempotent (WsClient's unsubscribe latches on
    // `released`, the state listener is removed from a Set), so a StrictMode
    // remount re-subscribes cleanly and a topic change leaks nothing.
    return () => {
      off();
      offState();
    };
  }, [topic, enabled]);

  // Read through the tag, not around it: anything held for a different topic is
  // not this topic's state, however recently it arrived.
  const current = value.topic === topic;
  return { data: current ? value.data : undefined, connected, lastSeq: current ? value.seq : 0 };
}
