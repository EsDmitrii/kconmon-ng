// fake-websocket.ts — hand-written WebSocket double for tests. It implements
// only the members WsClient actually touches (send/close/readyState/onopen/
// onmessage/onclose/onerror) plus the emit* drivers a test calls, and is
// injected either through WsClientOptions.WebSocketImpl (lib/ws.test.ts) or
// with vi.stubGlobal("WebSocket", FakeSocket) for code that goes through the
// module-level singleton (hooks + Live page). That is the same
// mock-at-the-boundary convention as vi.stubGlobal("fetch", …) in
// lib/api.test.ts; vitest.setup.ts intentionally has no WebSocket stub, so a
// test that forgets to inject fails loudly instead of dialling for real.
//
// This module is imported only from *.test.ts(x) files, so it never reaches the
// production bundle (rollup only walks what index.html/main.tsx reach).
import type { WsEnvelope } from "./ws";

export class FakeSocket {
  static instances: FakeSocket[] = [];

  static reset(): void {
    FakeSocket.instances = [];
  }

  /** The most recently constructed socket — the one WsClient is currently using. */
  static last(): FakeSocket {
    const socket = FakeSocket.instances.at(-1);
    if (!socket) throw new Error("no FakeSocket has been constructed yet");
    return socket;
  }

  /** Every frame the client wrote, in order, as raw JSON text. */
  readonly sent: string[] = [];
  /** 0 CONNECTING, 1 OPEN, 3 CLOSED — the same numbering as the DOM constants. */
  readyState = 0;
  onopen: (() => void) | null = null;
  onmessage: ((ev: { data: string }) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;

  constructor(public readonly url: string) {
    FakeSocket.instances.push(this);
  }

  send(data: string): void {
    this.sent.push(data);
  }

  close(): void {
    this.emitClose();
  }

  emitOpen(): void {
    this.readyState = 1;
    this.onopen?.();
  }

  emitClose(): void {
    if (this.readyState === 3) return;
    this.readyState = 3;
    this.onclose?.();
  }

  emitEnvelope(env: WsEnvelope): void {
    this.onmessage?.({ data: JSON.stringify(env) });
  }
}

// WsClientOptions.WebSocketImpl is typed as `typeof WebSocket`; FakeSocket
// implements only the used surface, so it is cast once here instead of
// stubbing the ~20 unused members of the DOM interface.
export const fakeWebSocketImpl = FakeSocket as unknown as typeof WebSocket;
