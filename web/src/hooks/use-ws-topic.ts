import { useEffect, useState } from "react";
import { WsClient, type WsEnvelope } from "@/lib/ws";

let client: WsClient | null = null;

/** getWsClient returns the page-wide WsClient, created on first use. */
export function getWsClient(): WsClient {
  client ??= new WsClient();
  return client;
}

/**
 * resetWsClient closes and drops the singleton; test-only seam: a vitest file that installs its own
 * WebSocket double must not inherit a client built from the previous file's double.
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
 * The value is stored WITH the topic it arrived on; a topic change re-runs the effect, but the
 * state update that clears the old value can only land after the render that already asked for the
 * new topic.
 */
interface TopicValue<T> {
  topic: string;
  data?: T;
  seq: number;
}

/**
 * useWsTopic subscribes to a WebSocket topic and keeps the LATEST envelope's data; that is exactly
 * right for the snapshot topics (topology, matrix:*).
 */
export function useWsTopic<T>(topic: string, opts?: { enabled?: boolean }): WsTopicResult<T> {
  const enabled = opts?.enabled ?? true;
  const [value, setValue] = useState<TopicValue<T>>({ topic, seq: 0 });
  const [connected, setConnected] = useState(false);

  useEffect(() => {
    if (!enabled) {
      setConnected(false);
      // Drop the retained payload too: state must not outlive the subscription that produced it on
      // ANY axis.
      setValue({ topic, seq: 0 });
      return;
    }
    const ws = getWsClient();
    setConnected(ws.state === "open");
    const offState = ws.onStateChange((s) => setConnected(s === "open"));
    const off = ws.subscribe<T>(topic, (env: WsEnvelope<T>) => {
      if (env.type === "error") {
        console.warn("console websocket: server rejected topic", topic, env.data);
        /* The socket is open, but nothing will ever arrive on THIS topic, so
           reporting it as connected handed the caller a green Live badge over a
           view whose polling it had already switched off. A reconnect
           re-subscribes and onStateChange flips this back on `open`, so a
           transient refusal still recovers. */
        setConnected(false);
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
