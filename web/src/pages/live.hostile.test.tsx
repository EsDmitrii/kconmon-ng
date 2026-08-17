import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { FakeSocket } from "@/lib/fake-websocket";
import { TimeMachineProvider } from "@/lib/timemachine";
import type { LiveEvent } from "@/lib/types";
import { TOPIC_LIVE } from "@/lib/ws";
import { LIVE_RING_CAP, LivePage, countMissedEvents } from "./live";

/*
 * /live with an operator trying to break it: nonsense in the scope box, a
 * filter changed faster than the network answers, a socket that drops under a
 * pause, a payload with fields the wire is not supposed to omit. The bar is the
 * page still standing, no NaN or undefined on screen, and every empty state
 * saying WHY it is empty.
 */

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

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function ev(seq: number, over: Partial<LiveEvent> = {}): LiveEvent {
  return {
    id: `${seq}-1785276000000000000`,
    seq,
    type: "check_observed",
    severity: "info",
    scope: "node-a→node-b",
    timestamp: "2026-07-28T10:00:00Z",
    summary: `event ${seq}`,
    details: {},
    ...over,
  };
}

function renderPage(databaseConfigured = true) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["version"], { version: "1.6.0", commit: "abc123", capabilities: ["events"] });
  qc.setQueryData(["config"], {
    auth: { mode: "anonymous", role: "admin" },
    anonymousBanner: true,
    controller: { configured: true },
    prometheus: { configured: true },
    database: { configured: databaseConfigured },
  });
  return render(
    <QueryClientProvider client={qc}>
      <TimeMachineProvider>
        <LivePage />
      </TimeMachineProvider>
    </QueryClientProvider>,
  );
}

/** Every /api/v1/events request this page made, in order, as URLSearchParams. */
function eventsCalls(fn: ReturnType<typeof vi.fn>): URLSearchParams[] {
  return fn.mock.calls
    .map(([url]) => String(url))
    .filter((u) => u.startsWith("/api/v1/events"))
    .map((u) => new URLSearchParams(u.split("?")[1] ?? ""));
}

function stubEventsFetch(onEvents: (qs: URLSearchParams) => Response | Promise<Response>) {
  const fn = vi.fn((url: string) => {
    if (typeof url === "string" && url.startsWith("/api/v1/events")) {
      return Promise.resolve(onEvents(new URLSearchParams(url.split("?")[1] ?? "")));
    }
    return Promise.resolve(json({ version: "1.6.0", commit: "abc123", capabilities: ["events"] }));
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

function open() {
  act(() => {
    FakeSocket.last().emitOpen();
  });
}

function emitHidden(events: LiveEvent[]) {
  act(() => {
    for (const e of events) {
      FakeSocket.last().emitEnvelope({ topic: TOPIC_LIVE, type: "event", seq: e.seq, data: e });
    }
  });
}

async function nextFrame() {
  await act(async () => {
    await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
  });
}

async function emit(events: LiveEvent[]) {
  emitHidden(events);
  await nextFrame();
}

beforeEach(() => {
  FakeSocket.reset();
  vi.stubGlobal("WebSocket", FakeSocket);
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(json({ version: "1.6.0", commit: "abc123", capabilities: ["events"] })),
  );
  window.history.replaceState({}, "", "/live");
});

afterEach(() => {
  cleanup();
  resetWsClient();
  vi.unstubAllGlobals();
  window.history.replaceState({}, "", "/");
});

/* ── the loss counter ───────────────────────────────────────────────────── */

describe("countMissedEvents against a seq the wire did not send", () => {
  /* `higher.seq - lower.seq` on a missing seq is NaN, and NaN accumulates: the
     whole counter became NaN, `missed > 0` went false, and the loss warning
     disappeared for the rest of the session — the failure mode a loss detector
     must never have. */
  it("returns a real number when an event carries no seq at all", () => {
    const noSeq = ev(2, { seq: undefined as unknown as number });
    expect(countMissedEvents([ev(9), noSeq, ev(1)])).toBeTypeOf("number");
    expect(Number.isFinite(countMissedEvents([ev(9), noSeq, ev(1)]))).toBe(true);
  });

  /* And the holes between the events that ARE numbered still get counted:
     one unnumbered row must not blind the scan. */
  it("still counts the holes between the events that are numbered", () => {
    const noSeq = ev(50, { seq: null as unknown as number, id: "junk" });
    expect(countMissedEvents([ev(4), noSeq, ev(1)])).toBe(2); // 4→1 hides 2 and 3
  });

  it("counts nothing when only one event is numbered", () => {
    const noSeq = ev(50, { seq: Number.NaN, id: "junk" });
    expect(countMissedEvents([ev(1), noSeq])).toBe(0);
  });
});

describe("the loss warning on screen", () => {
  it("never prints NaN, and still reports a hole beside an unnumbered event", async () => {
    renderPage(false);
    open();
    await emit([ev(1), ev(4), ev(7, { seq: undefined as unknown as number, id: "no-seq" })]);

    expect(document.body.textContent).not.toContain("NaN");
    expect(screen.getByText(/events may have been missed/)).toBeInTheDocument();
  });
});

/* ── the scope box ──────────────────────────────────────────────────────── */

describe("the scope filter under hostile input", () => {
  const box = () => screen.getByLabelText("Scope contains");

  it.each([
    ["whitespace only", "     "],
    ["an emoji", "💥🙈"],
    ["Cyrillic", "узел-а"],
    ["right-to-left text", "‮نص عربي‬"],
    ["a script payload", '<img src=x onerror="window.__pwned=1">'],
    ["percent junk", "%%%%%%%%%%"],
    ["arrows only", ">>>>"],
    ["a lone arrow", "->"],
    ["a number", "-12345.678"],
  ])("survives %s in the box", async (_name, typed) => {
    stubEventsFetch(() => json({ events: [], nextCursor: "" }));
    renderPage();
    open();
    await emit([ev(1, { summary: "held" })]);

    fireEvent.change(box(), { target: { value: typed } });
    await nextFrame();

    expect(screen.getByTestId("live-toolbar")).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("NaN");
    expect(document.querySelector("img")).toBeNull();
    expect((window as unknown as Record<string, unknown>).__pwned).toBeUndefined();
  });

  /* A whitespace-only box is not a filter, and emptying the feed for it would
     be the page hiding everything over a stray space. */
  it("keeps every row for a box holding nothing but spaces", async () => {
    stubEventsFetch(() => json({ events: [], nextCursor: "" }));
    renderPage();
    open();
    await emit([ev(1, { summary: "still here" })]);

    fireEvent.change(box(), { target: { value: "   " } });
    await nextFrame();

    expect(screen.getByText("still here")).toBeInTheDocument();
    expect(screen.queryByText("No events match these filters")).toBeNull();
  });

  /* A NUL byte in a text parameter reaches Postgres and comes back a 502 — the
     console must not hand the server a byte no text column can hold. Nothing an
     operator can SEE is lost by dropping it: control characters do not occur in
     a node name. */
  it("never sends a control character to the server", async () => {
    const fetchMock = stubEventsFetch(() => json({ events: [], nextCursor: "" }));
    renderPage();
    open();

    const CONTROLS = /[\u0000-\u001F\u007F]/;
    fireEvent.change(box(), { target: { value: "node\u0000-a\u0007\u001b" } });

    await waitFor(() => expect(eventsCalls(fetchMock).length).toBeGreaterThan(1));
    for (const qs of eventsCalls(fetchMock)) {
      expect(qs.get("scope") ?? "").not.toMatch(CONTROLS);
    }
    // And the box itself holds the name, not an emptied one.
    expect((box() as HTMLInputElement).value).toBe("node-a");
  });

  /* A pair scope is at most two k8s node names and an arrow; a 10k-character
     box is a paste accident, and sending it builds a URL an ingress answers
     with a status nobody can act on. */
  it("caps what the box will hold", async () => {
    stubEventsFetch(() => json({ events: [], nextCursor: "" }));
    renderPage();
    open();

    const input = box() as HTMLInputElement;
    expect(input.maxLength).toBeGreaterThan(0);
    expect(input.maxLength).toBeLessThanOrEqual(1024);
  });

  /* One request per keystroke turned a typed node name into six round trips,
     each of them a keyset scan. */
  it("asks the server once for a name that was typed, not once per letter", async () => {
    const fetchMock = stubEventsFetch(() => json({ events: [], nextCursor: "" }));
    renderPage();
    open();
    await waitFor(() => expect(eventsCalls(fetchMock).length).toBe(1)); // page one, on mount

    for (const value of ["n", "no", "nod", "node", "node-", "node-a"]) {
      fireEvent.change(box(), { target: { value } });
    }

    await waitFor(() => expect(eventsCalls(fetchMock).length).toBe(2));
    // Give any straggler timer room to fire before counting.
    await new Promise((resolve) => setTimeout(resolve, 400));
    const calls = eventsCalls(fetchMock);
    expect(calls).toHaveLength(2);
    expect(calls[1].get("scope")).toBe("node-a");
  });
});

/* ── a filter changed faster than the network ───────────────────────────── */

describe("a superseded history load", () => {
  /* The scope box drives a server-side query, and two of them can be in flight
     at once. The FIRST one answering LAST used to win: its cursor became the
     page's cursor, so "Load older" then walked a filter the operator had
     already left, and its rows were merged into a feed that no longer asked
     for them. */
  it("keeps the cursor of the LAST request, not of the last answer", async () => {
    const gate: Array<() => void> = [];
    const fetchMock = stubEventsFetch((qs) => {
      const scope = qs.get("scope") ?? "";
      if (scope === "node-a") {
        // The stale one: held open until after the newer request has answered.
        return new Promise<Response>((resolve) => {
          gate.push(() => resolve(json({ events: [ev(1, { summary: "stale page" })], nextCursor: "STALE" })));
        });
      }
      return json({ events: [ev(2, { summary: "fresh page" })], nextCursor: "" });
    });

    renderPage();
    open();
    await waitFor(() => expect(eventsCalls(fetchMock).length).toBe(1));

    fireEvent.change(screen.getByLabelText("Scope contains"), { target: { value: "node-a" } });
    await waitFor(() => expect(eventsCalls(fetchMock).some((q) => q.get("scope") === "node-a")).toBe(true));

    fireEvent.change(screen.getByLabelText("Scope contains"), { target: { value: "node-b" } });
    await waitFor(() => expect(eventsCalls(fetchMock).some((q) => q.get("scope") === "node-b")).toBe(true));

    // Only now does the FIRST request answer.
    await act(async () => {
      for (const release of gate) release();
      await Promise.resolve();
    });
    await nextFrame();

    // "node-b" answered with an exhausted cursor, so the control must be dead.
    const button = screen.getByRole("button", { name: /Load older/ });
    expect(button).toBeDisabled();
    expect(button).toHaveAccessibleName(/Nothing older matches the current filters\./);
  });
});

/* ── the type select ────────────────────────────────────────────────────── */

describe("the type select refuses a value it does not know", () => {
  it("falls back to every type rather than filtering the feed down to nothing", async () => {
    renderPage(false);
    open();
    await emit([ev(1, { type: "check_observed", summary: "still visible" })]);

    fireEvent.change(screen.getByLabelText("Type"), { target: { value: "quantum_flux" } });
    await nextFrame();

    expect(screen.getByText("still visible")).toBeInTheDocument();
    expect(screen.getByText(/Showing 1 of 1 events/)).toBeInTheDocument();
  });
});

/* ── every filter at once ───────────────────────────────────────────────── */

describe("filters stacked until nothing is left", () => {
  it("says WHY the feed is empty and offers the way back", async () => {
    renderPage(false);
    open();
    await emit([ev(1, { severity: "info", type: "check_observed", scope: "node-a→node-b" })]);

    fireEvent.click(screen.getByRole("radio", { name: "Error" }));
    fireEvent.change(screen.getByLabelText("Type"), { target: { value: "mtr_completed" } });
    fireEvent.change(screen.getByLabelText("Scope contains"), { target: { value: "node-z" } });
    await nextFrame();

    expect(screen.getByText("No events match these filters")).toBeInTheDocument();
    expect(screen.getByText(/1 held events, none of them matching/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    await nextFrame();
    expect(screen.getByText("event 1")).toBeInTheDocument();
  });
});

/* ── pause under everything else ────────────────────────────────────────── */

describe("pause and the Time Machine", () => {
  /* Engaged, the Pause button is disabled — so a pause latched before the
     switch could not be released, and the feed came back frozen with a chip
     the operator never chose. Engaging is a full reset of this page's store,
     and the pause is part of it. */
  it("releases a pause when the Time Machine is engaged, rather than latching one nobody can reach", async () => {
    renderPage(false);
    open();
    await emit([ev(1)]);
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    expect(screen.getAllByText(/^Paused ·/).length).toBeGreaterThan(0);

    act(() => {
      window.history.pushState({}, "", "/live?at=2026-07-28T09:00:00Z");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });

    expect(screen.queryAllByText(/^Paused ·/)).toHaveLength(0);
    expect(screen.getByRole("button", { name: "Pause" })).toBeDisabled();
  });

  it("comes back live rather than silently held when the operator returns", async () => {
    renderPage(false);
    open();
    await emit([ev(1)]);
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));

    act(() => {
      window.history.pushState({}, "", "/live?at=2026-07-28T09:00:00Z");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });
    act(() => {
      window.history.pushState({}, "", "/live");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });
    await emit([ev(9, { summary: "arrived after the return" })]);

    expect(screen.getByText("arrived after the return")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Pause" })).toBeEnabled();
  });
});

describe("filters changed while the stream is held", () => {
  /* Pause, narrow, resume: the buffered arrivals have to land into the filter
     that is on screen NOW, and the counts have to agree with the rows. */
  it("drains the buffer into the filter the operator is looking at", async () => {
    renderPage(false);
    open();
    await emit([ev(1, { severity: "info", summary: "quiet one" })]);

    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    await emit([
      ev(2, { severity: "error", summary: "loud one" }),
      ev(3, { severity: "info", summary: "another quiet one" }),
    ]);
    fireEvent.click(screen.getByRole("radio", { name: "Error" }));

    // Nothing has been drained yet, so the only held row does not match.
    expect(screen.getByText("No events match these filters")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Resume (2 buffered)" }));
    await nextFrame();

    const rows = screen.getAllByRole("listitem");
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveTextContent("loud one");
    expect(screen.getByText(/Showing 1 of 3 events/)).toBeInTheDocument();
    expect(screen.queryByText(/^Paused ·/)).toBeNull();
  });

  it("counts what it holds, not what it shows, while a filter is on", async () => {
    renderPage(false);
    open();
    await emit([ev(1, { severity: "info" }), ev(2, { severity: "error" }), ev(3, { severity: "warn" })]);

    fireEvent.click(screen.getByRole("radio", { name: "Warn" }));
    expect(screen.getByText(/Showing 1 of 3 events/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("radio", { name: "All" }));
    expect(screen.getByText(/Showing 3 of 3 events/)).toBeInTheDocument();
  });
});

/* ── the transport under a drop ─────────────────────────────────────────── */

describe("the socket dropping under a running feed", () => {
  it("stops claiming the feed is live the moment the socket closes", async () => {
    renderPage(false);
    open();
    await emit([ev(1)]);
    expect(within(screen.getByTestId("live-transport-slot")).getByText("Live")).toBeInTheDocument();

    act(() => {
      FakeSocket.last().emitClose();
    });

    const slot = screen.getByTestId("live-transport-slot");
    expect(within(slot).queryByText("Live")).toBeNull();
    // The rows it already holds are still there — a dead socket is not a lost feed.
    expect(screen.getByText("event 1")).toBeInTheDocument();
  });
});

/* ── payloads with holes in them ────────────────────────────────────────── */

describe("events the wire mangled", () => {
  it("renders a row whose summary, scope and timestamp are all missing", async () => {
    renderPage(false);
    open();
    await emit([
      ev(1, {
        summary: undefined as unknown as string,
        scope: undefined as unknown as string,
        timestamp: undefined as unknown as string,
      }),
    ]);

    expect(screen.getAllByRole("listitem")).toHaveLength(1);
    expect(document.body.textContent).not.toContain("undefined");
    expect(document.body.textContent).not.toContain("NaN");
    expect(document.body.textContent).not.toContain("[object Object]");
  });

  /* Every row keyed on an absent id is the SAME key, and the ring's dedupe
     read them as one event: a batch of five arrived as one row. */
  it("keeps every row when the ids did not arrive, rather than collapsing them into one", async () => {
    renderPage(false);
    open();
    await emit([
      ev(1, { id: undefined as unknown as string, summary: "first" }),
      ev(2, { id: "", summary: "second" }),
      ev(3, { id: undefined as unknown as string, summary: "third" }),
    ]);

    expect(screen.getAllByRole("listitem")).toHaveLength(3);
    expect(screen.getByText("first")).toBeInTheDocument();
    expect(screen.getByText("third")).toBeInTheDocument();
  });

  it("holds a 10k-character summary in one row without breaking the virtualizer", async () => {
    renderPage(false);
    open();
    await emit([ev(1, { summary: "Ж".repeat(10_000) }), ev(2)]);

    expect(screen.getAllByRole("listitem")).toHaveLength(2);
  });

  /* Go marshals a nil slice as null, and the page feeds this straight into the
     ring merge. A null page must read as an empty one, not as a failure. */
  it("treats a null events array from the history endpoint as an empty page", async () => {
    stubEventsFetch(() => json({ events: null, nextCursor: null }));
    renderPage();
    open();
    await emit([ev(1, { summary: "live still works" })]);

    expect(screen.queryByText("Event history is unavailable")).toBeNull();
    expect(screen.getByText("live still works")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Load older/ })).toBeDisabled();
  });
});

/* ── the ring's own boundary ────────────────────────────────────────────── */

describe("Load older at the boundary", () => {
  it("is honest about a full ring rather than spending a round trip on nothing", async () => {
    const fetchMock = stubEventsFetch(() => json({ events: [ev(1)], nextCursor: "cursor-1" }));
    renderPage();
    open();
    await screen.findByRole("button", { name: /Load older/ });

    emitHidden(Array.from({ length: LIVE_RING_CAP }, (_, i) => ev(i + 1)));
    await nextFrame();

    const before = eventsCalls(fetchMock).length;
    const button = screen.getByRole("button", { name: /Load older/ });
    expect(button).toBeDisabled();
    fireEvent.click(button);
    fireEvent.click(button);
    await nextFrame();
    expect(eventsCalls(fetchMock)).toHaveLength(before);
  });
});
