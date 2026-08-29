import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import { TimeMachineProvider } from "@/lib/timemachine";
import type { LiveEvent } from "@/lib/types";
import { TOPIC_LIVE } from "@/lib/ws";
import { LivePage } from "./live";

/** /live engaged: a scrollback ENDING at t, with the live tail off. */

// Same jsdom layout stub live.test.tsx installs, and for the same reason: the
// virtualizer renders nothing into a zero-height viewport.
const offsetHeightDescriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetHeight");
const offsetWidthDescriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetWidth");

beforeAll(() => {
  Object.defineProperty(HTMLElement.prototype, "offsetHeight", { configurable: true, get: () => 600 });
  Object.defineProperty(HTMLElement.prototype, "offsetWidth", { configurable: true, get: () => 900 });
});

afterAll(() => {
  if (offsetHeightDescriptor) Object.defineProperty(HTMLElement.prototype, "offsetHeight", offsetHeightDescriptor);
  if (offsetWidthDescriptor) Object.defineProperty(HTMLElement.prototype, "offsetWidth", offsetWidthDescriptor);
});

const AT = "2026-08-01T12:00:00Z";

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function ev(seq: number, over: Partial<LiveEvent> = {}): LiveEvent {
  return {
    id: `${seq}-1785276000000000000`,
    seq,
    type: "check_observed",
    severity: "info",
    scope: "node-a→node-b",
    timestamp: "2026-08-01T11:59:00Z",
    summary: `event ${seq}`,
    details: {},
    ...over,
  };
}

function renderPage(events: LiveEvent[] = [ev(1)]) {
  const queries: URLSearchParams[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      const href = String(url);
      if (href.startsWith("/api/v1/events")) {
        queries.push(new URLSearchParams(href.split("?")[1] ?? ""));
        return Promise.resolve(json({ events, nextCursor: "cursor-1" }));
      }
      return Promise.resolve(json({}));
    }),
  );
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["version"], { version: "1.7.0", commit: "abc", capabilities: ["events"] });
  qc.setQueryData(["config"], {
    auth: { mode: "anonymous", role: "admin", loginPath: "" },
    anonymousBanner: true,
    controller: { configured: true },
    prometheus: { configured: true },
    database: { configured: true },
  });
  const utils = render(
    <QueryClientProvider client={qc}>
      <TimeMachineProvider>
        <LivePage />
      </TimeMachineProvider>
    </QueryClientProvider>,
  );
  return { ...utils, queries };
}

beforeEach(() => {
  FakeSocket.reset();
  vi.stubGlobal("WebSocket", FakeSocket);
  window.history.pushState({}, "", `/live?at=${AT}`);
});

afterEach(() => {
  cleanup();
  resetWsClient();
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
});

describe("LivePage engaged at t", () => {
  it("bounds the scrollback with to=t", async () => {
    const { queries } = renderPage();
    await waitFor(() => expect(queries.length).toBeGreaterThan(0));
    expect(queries[0].get("to")).toBe(new Date(AT).toISOString());
  });

  it("keeps the bound on every older page, so pagination cannot walk past t", async () => {
    const { queries } = renderPage();
    await screen.findByText("event 1");
    queries.length = 0;
    fireEvent.click(screen.getByRole("button", { name: "Load older" }));
    await waitFor(() => expect(queries.length).toBeGreaterThan(0));
    expect(queries[0].get("cursor")).toBe("cursor-1");
    expect(queries[0].get("to")).toBe(new Date(AT).toISOString());
  });

  it("opens no socket — a live arrival is by definition after t", async () => {
    renderPage();
    await screen.findByText("event 1");
    expect(FakeSocket.instances).toHaveLength(0);
  });

  it("shows no realtime badge and no unfed-stream warning", async () => {
    renderPage();
    await screen.findByText("event 1");
    // The badge is looked for by its own element rather than by the word alone
    // (the page <h1> was also "Live" before the M3-8 rename to Events).
    expect(screen.queryByText("Live", { selector: "span" })).not.toBeInTheDocument();
    expect(screen.queryByText("Delayed data")).not.toBeInTheDocument();
    expect(screen.queryByText("Connecting…")).not.toBeInTheDocument();
    expect(
      screen.queryByText("This replica is not receiving the controller event stream"),
    ).not.toBeInTheDocument();
  });

  it("disables Pause — there is no tail for it to hold still", async () => {
    renderPage();
    await screen.findByText("event 1");
    expect(screen.getByRole("button", { name: "Pause" })).toBeDisabled();
  });

  it("blames retention, not a quiet socket, for an empty scrollback", async () => {
    renderPage([]);
    await screen.findByText("No events at or before this time");
    expect(screen.queryByText("Waiting for events")).not.toBeInTheDocument();
  });

  it("says in the header that the tail is off and where the walk starts", async () => {
    renderPage();
    await screen.findByText(/The live tail is off while the Time Machine is engaged/);
  });
});

describe("LivePage while live", () => {
  it("sends no to param at all", async () => {
    window.history.pushState({}, "", "/live");
    const { queries } = renderPage();
    await waitFor(() => expect(queries.length).toBeGreaterThan(0));
    expect(queries[0].get("to")).toBeNull();
  });

  it("still subscribes and still takes pushed events", async () => {
    window.history.pushState({}, "", "/live");
    renderPage([]);
    await waitFor(() => expect(FakeSocket.instances).toHaveLength(1));
    const socket = FakeSocket.instances[0];
    await act(async () => {
      socket.emitOpen();
      socket.emitEnvelope({ type: "event", topic: TOPIC_LIVE, seq: 9, data: ev(9) });
    });
    await screen.findByText("event 9");
  });
});
