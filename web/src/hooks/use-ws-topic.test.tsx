import { act, cleanup, renderHook } from "@testing-library/react";
import { StrictMode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FakeSocket } from "@/lib/fake-websocket";
import { TOPIC_TOPOLOGY } from "@/lib/ws";
import { getWsClient, resetWsClient, useWsTopic } from "./use-ws-topic";

beforeEach(() => {
  FakeSocket.reset();
  vi.stubGlobal("WebSocket", FakeSocket);
});

afterEach(() => {
  // Unmount BEFORE disposing the shared client: resetWsClient() calls close(),
  // which notifies state listeners, and a still-mounted hook would take that
  // setConnected(false) outside act().
  cleanup();
  resetWsClient();
  vi.unstubAllGlobals();
});

describe("useWsTopic", () => {
  it("keeps the latest snapshot for its topic and tracks the connection state", () => {
    const { result } = renderHook(() => useWsTopic<{ nodes: string[] }>(TOPIC_TOPOLOGY));

    expect(FakeSocket.instances).toHaveLength(1);
    expect(result.current.data).toBeUndefined();
    expect(result.current.connected).toBe(false);

    act(() => {
      FakeSocket.last().emitOpen();
    });
    expect(result.current.connected).toBe(true);

    act(() => {
      FakeSocket.last().emitEnvelope({
        topic: TOPIC_TOPOLOGY,
        type: "snapshot",
        seq: 3,
        data: { nodes: ["a"] },
      });
    });
    expect(result.current.data).toEqual({ nodes: ["a"] });
    expect(result.current.lastSeq).toBe(3);

    act(() => {
      FakeSocket.last().emitEnvelope({
        topic: TOPIC_TOPOLOGY,
        type: "snapshot",
        seq: 4,
        data: { nodes: ["a", "b"] },
      });
    });
    expect(result.current.data).toEqual({ nodes: ["a", "b"] });
    expect(result.current.lastSeq).toBe(4);
  });

  it("multiplexes every hook over one shared socket", () => {
    renderHook(() => {
      useWsTopic(TOPIC_TOPOLOGY);
      useWsTopic("matrix:tcp:pod");
    });

    expect(FakeSocket.instances).toHaveLength(1);
    act(() => {
      FakeSocket.last().emitOpen();
    });
    expect(FakeSocket.last().sent).toEqual([
      '{"action":"subscribe","topic":"topology"}',
      '{"action":"subscribe","topic":"matrix:tcp:pod"}',
    ]);
    expect(getWsClient().state).toBe("open");
  });

  it("ignores error envelopes rather than surfacing them as data", () => {
    const { result } = renderHook(() => useWsTopic<{ nodes: string[] }>(TOPIC_TOPOLOGY));
    act(() => {
      FakeSocket.last().emitOpen();
      FakeSocket.last().emitEnvelope({
        topic: TOPIC_TOPOLOGY,
        type: "error",
        seq: 0,
        data: { title: "unknown topic" },
      });
    });
    expect(result.current.data).toBeUndefined();
    expect(result.current.lastSeq).toBe(0);
  });

  it("opens no socket at all when disabled", () => {
    const { result } = renderHook(() => useWsTopic(TOPIC_TOPOLOGY, { enabled: false }));
    expect(FakeSocket.instances).toHaveLength(0);
    expect(result.current.connected).toBe(false);
  });

  // State must not outlive its subscription on the `enabled` axis either: a
  // realtime outage-and-recovery re-runs consumers' seeding effects, and a
  // payload retained across the disabled window would overwrite fresher polled
  // data with the pre-outage snapshot.
  it("drops the retained payload when the subscription is disabled", () => {
    const { result, rerender } = renderHook(
      ({ enabled }: { enabled: boolean }) => useWsTopic<{ n: number }>(TOPIC_TOPOLOGY, { enabled }),
      { initialProps: { enabled: true } },
    );
    const sock = FakeSocket.instances[0];
    act(() => sock.emitOpen());
    act(() =>
      sock.emitEnvelope({ topic: TOPIC_TOPOLOGY, type: "snapshot", seq: 4, data: { n: 1 } }),
    );
    expect(result.current.data).toEqual({ n: 1 });

    rerender({ enabled: false });
    expect(result.current.data).toBeUndefined();
    expect(result.current.lastSeq).toBe(0);

    // Re-enabling starts from "nothing yet" until the topic delivers again.
    rerender({ enabled: true });
    expect(result.current.data).toBeUndefined();
  });

  // A topic change must never expose the previous topic's payload, not even for
  // the single render between "caller asked for the new topic" and "the effect
  // resubscribed" — that window is what put one protocol's matrix under
  // another protocol's label.
  it("reports nothing for a new topic until that topic delivers", () => {
    const { result, rerender } = renderHook(({ topic }: { topic: string }) => useWsTopic<{ n: string }>(topic), {
      initialProps: { topic: "matrix:tcp:pod" },
    });
    act(() => {
      FakeSocket.last().emitOpen();
      FakeSocket.last().emitEnvelope({
        topic: "matrix:tcp:pod",
        type: "snapshot",
        seq: 7,
        data: { n: "tcp" },
      });
    });
    expect(result.current.data).toEqual({ n: "tcp" });
    expect(result.current.lastSeq).toBe(7);

    rerender({ topic: "matrix:udp:pod" });
    expect(result.current.data).toBeUndefined();
    expect(result.current.lastSeq).toBe(0);

    act(() => {
      FakeSocket.last().emitEnvelope({
        topic: "matrix:udp:pod",
        type: "snapshot",
        seq: 1,
        data: { n: "udp" },
      });
    });
    expect(result.current.data).toEqual({ n: "udp" });
    expect(result.current.lastSeq).toBe(1);
  });

  // The singleton is page-wide, so a StrictMode double-mount must not be able
  // to wedge realtime for the rest of the session: WsClient.close() is
  // irreversible, and the hook's teardown therefore only unsubscribes.
  //
  // The socket is driven open BEFORE the StrictMode hook mounts, on purpose: a
  // subscribe/unsubscribe is only written to the wire while the connection is
  // open, so on a still-connecting socket the double-mount is invisible and any
  // assertion about it would pass just as well without StrictMode.
  it("survives a StrictMode double-mount without killing the shared client", () => {
    const keepalive = renderHook(() => useWsTopic("matrix:tcp:pod"));
    act(() => {
      FakeSocket.last().emitOpen();
    });
    const socket = FakeSocket.last();
    const before = socket.sent.length;

    const { result } = renderHook(() => useWsTopic<{ nodes: string[] }>(TOPIC_TOPOLOGY), {
      wrapper: StrictMode,
    });

    // Mount, immediate teardown, remount — the double-mount is observable, so
    // this test fails if React ever stops doing it (or if the hook stops
    // cleaning up).
    const frames = socket.sent.slice(before);
    expect(frames).toEqual([
      '{"action":"subscribe","topic":"topology"}',
      '{"action":"unsubscribe","topic":"topology"}',
      '{"action":"subscribe","topic":"topology"}',
    ]);
    expect(frames.filter((f) => f.includes("unsubscribe"))).toHaveLength(1);

    // No second socket, and the teardown did not dispose the shared client.
    expect(FakeSocket.instances).toHaveLength(1);
    expect(getWsClient().state).toBe("open");
    expect(result.current.connected).toBe(true);

    // The surviving subscription is live: the remount left exactly one working
    // handler, not zero (over-unsubscribed) and not two (leaked).
    act(() => {
      socket.emitEnvelope({
        topic: TOPIC_TOPOLOGY,
        type: "snapshot",
        seq: 1,
        data: { nodes: ["a"] },
      });
    });
    expect(result.current.data).toEqual({ nodes: ["a"] });

    keepalive.unmount();
  });

  it("unsubscribes on unmount", () => {
    const { unmount } = renderHook(() => useWsTopic(TOPIC_TOPOLOGY));
    act(() => {
      FakeSocket.last().emitOpen();
    });
    unmount();
    expect(FakeSocket.last().sent).toEqual([
      '{"action":"subscribe","topic":"topology"}',
      '{"action":"unsubscribe","topic":"topology"}',
    ]);
  });
});
