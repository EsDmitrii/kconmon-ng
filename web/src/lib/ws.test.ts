import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FakeSocket, fakeWebSocketImpl } from "./fake-websocket";
import { WsClient, wsUrl, type WsEnvelope, type WsState } from "./ws";

function newClient(): WsClient {
  return new WsClient({ url: "ws://console.test/ws", WebSocketImpl: fakeWebSocketImpl });
}

beforeEach(() => {
  FakeSocket.reset();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("wsUrl", () => {
  it("maps the page protocol to ws/wss and always targets /ws", () => {
    expect(wsUrl({ protocol: "http:", host: "console.local:8080" })).toBe("ws://console.local:8080/ws");
    expect(wsUrl({ protocol: "https:", host: "console.example.com" })).toBe("wss://console.example.com/ws");
  });

  it("defaults to the current page location", () => {
    // jsdom serves the test page over http:, so the ws: branch is the one
    // exercised here; the wss: branch is covered by the explicit-argument test
    // above because jsdom's window.location is non-configurable.
    expect(wsUrl()).toBe(`ws://${window.location.host}/ws`);
  });
});

describe("WsClient", () => {
  it("shares a single socket across topics (ADR-003 multiplexing)", () => {
    const client = newClient();
    const offLive = client.subscribe("live", () => {});
    const offTopology = client.subscribe("topology", () => {});

    expect(FakeSocket.instances).toHaveLength(1);
    expect(FakeSocket.last().url).toBe("ws://console.test/ws");

    FakeSocket.last().emitOpen();
    expect(FakeSocket.last().sent).toEqual([
      '{"action":"subscribe","topic":"live"}',
      '{"action":"subscribe","topic":"topology"}',
    ]);

    offLive();
    offTopology();
    client.close();
  });

  it("sends the subscribe frame on open, and immediately for topics added later", () => {
    const client = newClient();
    client.subscribe("live", () => {});
    expect(FakeSocket.last().sent).toEqual([]); // nothing is written before the socket opens

    FakeSocket.last().emitOpen();
    expect(client.state).toBe("open");
    expect(FakeSocket.last().sent).toEqual(['{"action":"subscribe","topic":"live"}']);

    client.subscribe("matrix:tcp:pod", () => {});
    expect(FakeSocket.last().sent).toEqual([
      '{"action":"subscribe","topic":"live"}',
      '{"action":"subscribe","topic":"matrix:tcp:pod"}',
    ]);

    client.close();
  });

  it("routes an envelope only to the callbacks of its own topic", () => {
    const client = newClient();
    const live: WsEnvelope[] = [];
    const topology: WsEnvelope[] = [];
    client.subscribe("live", (env: WsEnvelope) => {
      live.push(env);
    });
    client.subscribe("topology", (env: WsEnvelope) => {
      topology.push(env);
    });
    FakeSocket.last().emitOpen();

    FakeSocket.last().emitEnvelope({ topic: "live", type: "event", seq: 4, data: { id: "4-17" } });

    expect(live).toHaveLength(1);
    expect(live[0].seq).toBe(4);
    expect((live[0].data as { id: string }).id).toBe("4-17");
    expect(topology).toHaveLength(0);

    client.close();
  });

  it("stops delivery and tells the server after unsubscribe", () => {
    const client = newClient();
    const seen: WsEnvelope[] = [];
    const off = client.subscribe("live", (env: WsEnvelope) => {
      seen.push(env);
    });
    FakeSocket.last().emitOpen();

    off();
    expect(FakeSocket.last().sent).toEqual([
      '{"action":"subscribe","topic":"live"}',
      '{"action":"unsubscribe","topic":"live"}',
    ]);

    FakeSocket.last().emitEnvelope({ topic: "live", type: "event", seq: 5, data: {} });
    expect(seen).toHaveLength(0);

    client.close();
  });

  it("re-subscribes every topic after a reconnect, resuming the live feed only", () => {
    vi.useFakeTimers();
    const client = newClient();
    client.subscribe("live", () => {});
    client.subscribe("topology", () => {});

    const first = FakeSocket.last();
    first.emitOpen();
    first.emitEnvelope({ topic: "live", type: "event", seq: 7, data: {} });
    first.emitEnvelope({ topic: "topology", type: "snapshot", seq: 2, data: {} });
    first.emitClose();
    expect(client.state).toBe("closed");

    vi.advanceTimersByTime(1_000);
    expect(FakeSocket.instances).toHaveLength(2);

    const second = FakeSocket.last();
    second.emitOpen();
    // The live feed resumes from its cursor. "topology" deliberately does NOT:
    // its ring holds one whole-state entry a reconnecting tab always wants, and
    // a sticky cursor would let the hub's `seq > lastSeq` filter suppress it.
    expect(second.sent).toEqual([
      '{"action":"subscribe","topic":"live","lastSeq":7}',
      '{"action":"subscribe","topic":"topology"}',
    ]);

    client.close();
  });

  it("never sends a resume cursor for a matrix topic either", () => {
    vi.useFakeTimers();
    const client = newClient();
    client.subscribe("matrix:tcp:pod", () => {});

    const first = FakeSocket.last();
    first.emitOpen();
    first.emitEnvelope({ topic: "matrix:tcp:pod", type: "snapshot", seq: 11, data: {} });
    first.emitClose();

    vi.advanceTimersByTime(1_000);
    const second = FakeSocket.last();
    second.emitOpen();
    expect(second.sent).toEqual(['{"action":"subscribe","topic":"matrix:tcp:pod"}']);

    client.close();
  });

  it("doubles the reconnect delay from 1s and caps it at 15s", () => {
    vi.useFakeTimers();
    const client = newClient();
    client.subscribe("live", () => {});
    FakeSocket.last().emitOpen(); // a successful open resets the backoff to the floor
    FakeSocket.last().emitClose();

    // 8s would double to 16s; the cap pins it at 15s from then on.
    const delays = [1_000, 2_000, 4_000, 8_000, 15_000, 15_000];
    delays.forEach((delay, i) => {
      vi.advanceTimersByTime(delay - 1);
      expect(FakeSocket.instances).toHaveLength(i + 1);
      vi.advanceTimersByTime(1);
      expect(FakeSocket.instances).toHaveLength(i + 2);
      FakeSocket.last().emitClose(); // never opens, so the backoff keeps growing
    });

    client.close();
  });

  it("does not reconnect after close()", () => {
    vi.useFakeTimers();
    const client = newClient();
    client.subscribe("live", () => {});
    FakeSocket.last().emitOpen();

    client.close();
    expect(client.state).toBe("closed");

    vi.advanceTimersByTime(60_000);
    expect(FakeSocket.instances).toHaveLength(1);
  });

  it("reports connecting → open to state listeners and stops after unsubscribing them", () => {
    const client = newClient();
    const seen: WsState[] = [];
    const offState = client.onStateChange((s) => {
      seen.push(s);
    });

    client.subscribe("live", () => {});
    FakeSocket.last().emitOpen();
    offState();
    client.close();

    expect(seen).toEqual(["connecting", "open"]);
    expect(client.state).toBe("closed");
  });
});

// Delivery is exactly-once but NOT ordered: the hub hands frames to a client outside its lock.
describe("WsClient seq discipline", () => {
  it("keeps only the highest seq on snapshot topics (topology, matrix:*)", () => {
    const client = newClient();
    const topology: number[] = [];
    const matrix: number[] = [];
    client.subscribe("topology", (env: WsEnvelope) => {
      topology.push(env.seq);
    });
    client.subscribe("matrix:tcp:pod", (env: WsEnvelope) => {
      matrix.push(env.seq);
    });
    const socket = FakeSocket.last();
    socket.emitOpen();

    socket.emitEnvelope({ topic: "topology", type: "snapshot", seq: 5, data: {} });
    socket.emitEnvelope({ topic: "topology", type: "snapshot", seq: 3, data: {} }); // stale
    socket.emitEnvelope({ topic: "topology", type: "snapshot", seq: 6, data: {} });
    socket.emitEnvelope({ topic: "matrix:tcp:pod", type: "snapshot", seq: 8, data: {} });
    socket.emitEnvelope({ topic: "matrix:tcp:pod", type: "delta", seq: 7, data: {} }); // stale

    // An inverted pair must never latch the OLDER whole state into the UI.
    expect(topology).toEqual([5, 6]);
    expect(matrix).toEqual([8]);

    client.close();
  });

  it("never discards an unseen lower seq on the live topic", () => {
    const client = newClient();
    const seen: number[] = [];
    client.subscribe("live", (env: WsEnvelope) => {
      seen.push(env.seq);
    });
    const socket = FakeSocket.last();
    socket.emitOpen();

    // A racing broadcast delivers 6 before the replayed 1..5.
    for (const seq of [6, 1, 2, 3, 4, 5]) {
      socket.emitEnvelope({ topic: "live", type: "event", seq, data: {} });
    }

    expect(seen).toEqual([6, 1, 2, 3, 4, 5]);
    expect(client.lastSeqFor("live")).toBe(6);

    client.close();
  });

  it("resumes from the highest seq seen, not the most recent one", () => {
    vi.useFakeTimers();
    const client = newClient();
    client.subscribe("live", () => {});

    const first = FakeSocket.last();
    first.emitOpen();
    first.emitEnvelope({ topic: "live", type: "event", seq: 6, data: {} });
    first.emitEnvelope({ topic: "live", type: "event", seq: 4, data: {} }); // late replay frame
    first.emitClose();

    vi.advanceTimersByTime(1_000);
    const second = FakeSocket.last();
    second.emitOpen();
    expect(second.sent).toEqual(['{"action":"subscribe","topic":"live","lastSeq":6}']);

    client.close();
  });

  it("delivers error envelopes without letting their seq 0 disturb tracking", () => {
    vi.useFakeTimers();
    const client = newClient();
    const seen: WsEnvelope[] = [];
    client.subscribe("live", (env: WsEnvelope) => {
      seen.push(env);
    });

    const first = FakeSocket.last();
    first.emitOpen();
    first.emitEnvelope({ topic: "live", type: "event", seq: 3, data: {} });
    first.emitEnvelope({ topic: "live", type: "error", seq: 0, data: { error: "unknown topic" } });

    expect(seen.map((env) => env.type)).toEqual(["event", "error"]);
    expect(client.lastSeqFor("live")).toBe(3);

    first.emitClose();
    vi.advanceTimersByTime(1_000);
    FakeSocket.last().emitOpen();
    expect(FakeSocket.last().sent).toEqual(['{"action":"subscribe","topic":"live","lastSeq":3}']);

    client.close();
  });

  it("does not treat a fresh socket's lower seqs as stale", () => {
    vi.useFakeTimers();
    const client = newClient();
    const seen: number[] = [];
    client.subscribe("topology", (env: WsEnvelope) => {
      seen.push(env.seq);
    });

    const first = FakeSocket.last();
    first.emitOpen();
    first.emitEnvelope({ topic: "topology", type: "snapshot", seq: 9, data: {} });
    first.emitClose();

    vi.advanceTimersByTime(1_000);
    const second = FakeSocket.last();
    second.emitOpen();
    // Seq counters are per hub, so a reconnect that lands on another console
    // replica starts low. The staleness watermark only guards inversions inside
    // one connection; the resume cursor is what persists across them.
    second.emitEnvelope({ topic: "topology", type: "snapshot", seq: 2, data: {} });

    expect(seen).toEqual([9, 2]);

    client.close();
  });
});

describe("WsClient robustness", () => {
  it("hands a late subscriber the cached snapshot instead of starving it", () => {
    const client = newClient();
    const first: WsEnvelope[] = [];
    const late: WsEnvelope[] = [];
    client.subscribe("topology", (env: WsEnvelope) => {
      first.push(env);
    });
    const socket = FakeSocket.last();
    socket.emitOpen();
    socket.emitEnvelope({ topic: "topology", type: "snapshot", seq: 4, data: { nodes: [] } });

    client.subscribe("topology", (env: WsEnvelope) => {
      late.push(env);
    });

    // Synchronously, and only to the new handler; the server would not resend
    // a snapshot, so the alternative is an empty render until the next push.
    expect(late.map((env) => env.seq)).toEqual([4]);
    expect(first).toHaveLength(1);
    // No redundant wire subscribe for a topic already subscribed.
    expect(socket.sent).toEqual(['{"action":"subscribe","topic":"topology"}']);

    client.close();
  });

  it("caches nothing for the live topic", () => {
    const client = newClient();
    const late: WsEnvelope[] = [];
    client.subscribe("live", () => {});
    const socket = FakeSocket.last();
    socket.emitOpen();
    socket.emitEnvelope({ topic: "live", type: "event", seq: 2, data: {} });

    client.subscribe("live", (env: WsEnvelope) => {
      late.push(env);
    });

    // The live feed is append-only, not a current state: replaying one
    // arbitrary event to a late subscriber would be worse than nothing.
    // History belongs to the store, not the transport.
    expect(late).toHaveLength(0);
    expect(socket.sent).toEqual(['{"action":"subscribe","topic":"live"}']);

    client.close();
  });

  it("survives malformed frames with its cursors untouched", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const client = newClient();
    const seen: WsEnvelope[] = [];
    client.subscribe("live", (env: WsEnvelope) => {
      seen.push(env);
    });
    const socket = FakeSocket.last();
    socket.emitOpen();
    socket.emitEnvelope({ topic: "live", type: "event", seq: 2, data: {} });

    socket.onmessage?.({ data: "{not json" });
    socket.onmessage?.({ data: 42 as unknown as string });
    socket.onmessage?.({ data: JSON.stringify({ type: "event", seq: 99 }) }); // no topic

    expect(seen).toHaveLength(1);
    expect(client.lastSeqFor("live")).toBe(2);
    expect(client.state).toBe("open");
    // The warning must not echo the frame body — it can carry topology data.
    expect(warn.mock.calls[0][1]).toEqual({ bytes: "{not json".length });

    client.close();
    warn.mockRestore();
  });

  it("reconnects after the WebSocket constructor throws", () => {
    vi.useFakeTimers();
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    let reject = true;
    // A CSP connect-src denial throws out of `new WebSocket(...)` itself.
    const throwingImpl = function (url: string) {
      if (reject) {
        reject = false;
        throw new Error("refused to connect: connect-src");
      }
      return new FakeSocket(url);
    } as unknown as typeof WebSocket;

    const client = new WsClient({ url: "ws://console.test/ws", WebSocketImpl: throwingImpl });
    client.subscribe("live", () => {});
    expect(FakeSocket.instances).toHaveLength(0);
    expect(client.state).toBe("closed");

    vi.advanceTimersByTime(1_000);
    expect(FakeSocket.instances).toHaveLength(1);
    FakeSocket.last().emitOpen();
    expect(client.state).toBe("open");

    client.close();
    warn.mockRestore();
  });
});
