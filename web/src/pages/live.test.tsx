import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { resetWsClient } from "@/hooks/use-ws-topic";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { liveDict } from "@/lib/i18n/dict/live";
import { FakeSocket } from "@/lib/fake-websocket";
import type { LiveEvent } from "@/lib/types";
import { fmtEventStamp } from "@/lib/utils";
import { TOPIC_LIVE } from "@/lib/ws";
import { NAV_ITEMS } from "@/nav";
import {
  LIVE_RING_CAP,
  LivePage,
  ROW_HEIGHT,
  countMissedEvents,
  filterEvents,
  pushEvents,
} from "./live";

// Give the layout a real height for this file only (jsdom defines these as configurable accessors)
// and restore it afterwards.
const offsetHeightDescriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetHeight");
const offsetWidthDescriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetWidth");

beforeAll(() => {
  Object.defineProperty(HTMLElement.prototype, "offsetHeight", { configurable: true, get: () => 600 });
  Object.defineProperty(HTMLElement.prototype, "offsetWidth", { configurable: true, get: () => 900 });
});

afterAll(() => {
  if (offsetHeightDescriptor) {
    Object.defineProperty(HTMLElement.prototype, "offsetHeight", offsetHeightDescriptor);
  }
  if (offsetWidthDescriptor) {
    Object.defineProperty(HTMLElement.prototype, "offsetWidth", offsetWidthDescriptor);
  }
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

/** `capabilities: null` leaves the version cache unseeded, i.e. the cold load. */
function renderPage(capabilities: string[] | null = ["events"], databaseConfigured = false) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  if (capabilities !== null) {
    qc.setQueryData(["version"], { version: "1.6.0", commit: "abc123", capabilities });
  }
  qc.setQueryData(["config"], {
    auth: { mode: "anonymous", role: "admin" },
    anonymousBanner: true,
    controller: { configured: true },
    prometheus: { configured: true },
    database: { configured: databaseConfigured },
  });
  return render(
    <QueryClientProvider client={qc}>
      <LivePage />
    </QueryClientProvider>,
  );
}

/** URL-aware fetch double for the scrollback tests: routes /api/v1/events to
 * `onEvents` (given the query string) and answers every other URL (the
 * background /api/v1/version refetch) with a plain version payload. */
function stubEventsFetch(onEvents: (qs: URLSearchParams) => Response) {
  const fn = vi.fn((url: string) => {
    if (typeof url === "string" && url.startsWith("/api/v1/events")) {
      return Promise.resolve(onEvents(new URLSearchParams(url.split("?")[1] ?? "")));
    }
    return Promise.resolve(json({ version: "1.6.0", commit: "abc123", capabilities: ["events"] }));
  });
  vi.stubGlobal("fetch", fn);
  return fn;
}

/** The page merges arrivals once per animation frame rather than once per event. */
async function emit(events: LiveEvent[]) {
  emitHidden(events);
  await nextFrame();
}

/** Delivers without letting a frame run — a hidden tab gets no frames at all. */
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

function open() {
  act(() => {
    FakeSocket.last().emitOpen();
  });
}

beforeEach(() => {
  FakeSocket.reset();
  vi.stubGlobal("WebSocket", FakeSocket);
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(json({ version: "1.6.0", commit: "abc123", capabilities: ["events"] })),
  );
});

// cleanup() BEFORE resetWsClient(): React Testing Library must unmount the tree
// (which unsubscribes) while the client is still alive, otherwise the close
// races the effect teardown and React logs an act() warning.
afterEach(() => {
  cleanup();
  resetWsClient();
  vi.unstubAllGlobals();
});

describe("pushEvents", () => {
  it("prepends newest-first and drops the oldest past the cap", () => {
    const seeded = pushEvents(
      [],
      Array.from({ length: LIVE_RING_CAP }, (_, i) => ev(i + 1)),
    );
    expect(seeded).toHaveLength(LIVE_RING_CAP);
    expect(seeded[0].seq).toBe(LIVE_RING_CAP);
    expect(seeded[LIVE_RING_CAP - 1].seq).toBe(1);

    const overflowed = pushEvents(seeded, [ev(LIVE_RING_CAP + 1)]);
    expect(overflowed).toHaveLength(LIVE_RING_CAP);
    expect(overflowed[0].seq).toBe(LIVE_RING_CAP + 1);
    expect(overflowed.some((e) => e.seq === 1)).toBe(false);
    expect(overflowed[LIVE_RING_CAP - 1].seq).toBe(2);
  });

  it("returns the previous array untouched when nothing arrived", () => {
    const prev = [ev(1)];
    expect(pushEvents(prev, [])).toBe(prev);
  });

  // The hub is exactly-once per connection, but a reconnect replays from the
  // resume cursor and a Broadcast can race that replay — the same event arrives
  // twice, with the same controller-assigned id.
  it("dedupes by id, within a batch and against what is already held", () => {
    const held = pushEvents([], [ev(1), ev(2)]);
    expect(pushEvents(held, [ev(2)])).toBe(held);
    expect(pushEvents(held, [ev(2), ev(3), ev(3)]).map((e) => e.seq)).toEqual([3, 2, 1]);
  });

  // A replayed straggler must never be discarded: the transport delivers seq 6
  // before the replayed 1..5 and dropping the low ones loses rows for good.
  it("files a late lower-seq event into place instead of dropping it", () => {
    const held = pushEvents([], [ev(1), ev(2), ev(5)]);
    const filled = pushEvents(held, [ev(3), ev(4)]);
    expect(filled.map((e) => e.seq)).toEqual([5, 4, 3, 2, 1]);
  });

  // Seq is controller-assigned, so a controller restart takes it back to 1.
  // Ordering on seq alone would bury the newest event at the bottom of the
  // feed; the timestamp is what says which numbering era an event belongs to.
  it("keeps a restarted, lower-seq but newer event at the head", () => {
    const held = pushEvents([], [ev(900, { timestamp: "2026-07-28T10:00:00Z" })]);
    const afterRestart = pushEvents(held, [ev(1, { timestamp: "2026-07-28T10:05:00Z", id: "1-2" })]);
    expect(afterRestart.map((e) => e.seq)).toEqual([1, 900]);
  });

  // An unparseable timestamp must not poison the comparator — NaN compares
  // false in both directions, which is not an ordering at all. It sorts to the
  // bottom and stays there.
  it("keeps a total order when a timestamp is junk", () => {
    const merged = pushEvents([], [ev(1), ev(2, { timestamp: "not a date" }), ev(3)]);
    expect(merged.map((e) => e.seq)).toEqual([3, 1, 2]);
  });
});

describe("filterEvents", () => {
  const events = [
    ev(1, { type: "check_observed", severity: "error", scope: "node-a→node-b" }),
    ev(2, { type: "topology_changed", severity: "info", scope: "node-c" }),
    ev(3, { type: "mtr_completed", severity: "error", scope: "node-a→node-c" }),
  ];

  it("filters by type, severity and case-insensitive scope substring", () => {
    expect(filterEvents(events, { type: "all", severity: "all", scope: "" })).toHaveLength(3);
    expect(filterEvents(events, { type: "mtr_completed", severity: "all", scope: "" }).map((e) => e.seq)).toEqual([3]);
    expect(filterEvents(events, { type: "all", severity: "error", scope: "" }).map((e) => e.seq)).toEqual([1, 3]);
    expect(filterEvents(events, { type: "all", severity: "all", scope: "NODE-C" }).map((e) => e.seq)).toEqual([2, 3]);
    expect(filterEvents(events, { type: "check_observed", severity: "info", scope: "" })).toHaveLength(0);
  });

  /* U+2192 is not on any keyboard: before this the owner had to copy the arrow
     out of a row to filter by a pair. Every typeable separator now reads as one. */
  it.each([
    ["a hyphen arrow", "node-a->node-b"],
    ["a long hyphen arrow", "node-a-->node-b"],
    ["a fat arrow", "node-a=>node-b"],
    ["a bare greater-than", "node-a>node-b"],
    ["spaces around the arrow", "node-a -> node-b"],
    ["the pretty arrow, still", "node-a→node-b"],
  ])("matches the pair scope typed with %s", (_name, typed) => {
    expect(filterEvents(events, { type: "all", severity: "all", scope: typed }).map((e) => e.seq)).toEqual([1]);
  });

  it("stays case-insensitive through the normalisation", () => {
    expect(filterEvents(events, { type: "all", severity: "all", scope: "NODE-A -> NODE-B" }).map((e) => e.seq)).toEqual([1]);
  });

  it("keeps a single node name a plain substring, matching either side of a pair", () => {
    expect(filterEvents(events, { type: "all", severity: "all", scope: "node-a" }).map((e) => e.seq)).toEqual([1, 3]);
    expect(filterEvents(events, { type: "all", severity: "all", scope: "node-c" }).map((e) => e.seq)).toEqual([2, 3]);
  });

  it("still matches NOTHING for a pair that is not on the feed", () => {
    // Normalisation must not become "any two names match any pair".
    expect(filterEvents(events, { type: "all", severity: "all", scope: "node-b->node-a" })).toHaveLength(0);
    expect(filterEvents(events, { type: "all", severity: "all", scope: "node-x->node-y" })).toHaveLength(0);
    // A bare space is not an arrow: this stays a substring nothing carries.
    expect(filterEvents(events, { type: "all", severity: "all", scope: "node-a node-b" })).toHaveLength(0);
  });
});

describe("countMissedEvents", () => {
  it("counts the holes in the controller seq", () => {
    expect(countMissedEvents([])).toBe(0);
    expect(countMissedEvents([ev(3), ev(2), ev(1)])).toBe(0);
    // 9→3 hides 4..8, 3→1 hides 2.
    expect(countMissedEvents([ev(9), ev(3), ev(1)])).toBe(5 + 1);
  });

  // A restart is told from a hole by DIRECTION, not size: at an era boundary the
  // lower seq carries the newer timestamp. Same two numbers, opposite verdicts.
  it("separates a restarted counter from a large hole by direction, not size", () => {
    const restarted = [ev(1, { timestamp: "2026-07-28T10:05:00Z" }), ev(900)];
    expect(countMissedEvents(restarted)).toBe(0);

    // Same 899-wide jump, but time climbs with seq: this really is loss, and a
    // size threshold here is exactly how a detector goes quiet during an outage.
    const lost = [ev(900, { timestamp: "2026-07-28T10:05:00Z" }), ev(1)];
    expect(countMissedEvents(lost)).toBe(898);
  });

  // Display order is timestamp-primary, and agents' observations are genuinely time-shuffled
  // against the controller's numbering.
  it("reports nothing for a gapless run that is shuffled in display order", () => {
    const shuffled = [
      ev(2, { timestamp: "2026-07-28T10:00:09Z" }),
      ev(4, { timestamp: "2026-07-28T10:00:07Z" }),
      ev(1, { timestamp: "2026-07-28T10:00:05Z" }),
      ev(5, { timestamp: "2026-07-28T10:00:03Z" }),
      ev(3, { timestamp: "2026-07-28T10:00:01Z" }),
    ];
    expect(countMissedEvents(shuffled)).toBe(0);
  });
});

describe("LivePage", () => {
  it("renders events arriving over the socket as rows, newest first", async () => {
    renderPage();
    open();
    await emit([ev(1, { summary: "first thing" }), ev(2, { summary: "second thing" })]);

    const rows = screen.getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent("second thing");
    expect(rows[1]).toHaveTextContent("first thing");
  });

  /* M4's iron rule: identifiers and stamps in the feed wear the data face. */
  it("sets the stamp and the scope in mono-data", async () => {
    renderPage();
    open();
    await emit([ev(1)]);

    const row = screen.getAllByRole("listitem")[0];
    const [stamp] = row.querySelectorAll("span.mono-data");
    expect(stamp).toBeDefined();
    expect(screen.getByText("node-a→node-b").className).toMatch(/\bmono-data\b/);
  });

  it("narrows the feed with the type filter", async () => {
    renderPage();
    open();
    await emit([
      ev(1, { type: "check_observed", summary: "probe failed" }),
      ev(2, { type: "topology_changed", scope: "node-a", summary: "node joined" }),
    ]);
    expect(screen.getAllByRole("listitem")).toHaveLength(2);

    fireEvent.change(screen.getByLabelText("Type"), { target: { value: "topology_changed" } });

    const rows = screen.getAllByRole("listitem");
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveTextContent("node joined");
    expect(screen.getByText(/Showing 1 of 2 events/)).toBeInTheDocument();
  });

  it("narrows the feed with the severity control", async () => {
    renderPage();
    open();
    await emit([
      ev(1, { severity: "info", summary: "all good" }),
      ev(2, { severity: "error", summary: "it broke" }),
    ]);

    fireEvent.click(screen.getByRole("radio", { name: "Error" }));

    const rows = screen.getAllByRole("listitem");
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveTextContent("it broke");
  });

  it("buffers while paused, shows the buffered count, and flushes on resume", async () => {
    renderPage();
    open();
    await emit([ev(1, { summary: "before pause" })]);

    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    await emit([ev(2, { summary: "while paused" }), ev(3, { summary: "also while paused" })]);

    expect(screen.getAllByRole("listitem")).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: "Resume (2 buffered)" }));

    const rows = screen.getAllByRole("listitem");
    expect(rows).toHaveLength(3);
    expect(rows[0]).toHaveTextContent("also while paused");
    expect(rows[1]).toHaveTextContent("while paused");
    expect(rows[2]).toHaveTextContent("before pause");
    expect(screen.getByRole("button", { name: "Pause" })).toBeInTheDocument();
  });

  it("keeps only the newest LIVE_RING_CAP events and says so", async () => {
    renderPage();
    open();
    await emit(Array.from({ length: LIVE_RING_CAP + 1 }, (_, i) => ev(i + 1)));

    expect(screen.getByText(new RegExp(`Showing ${LIVE_RING_CAP} of ${LIVE_RING_CAP} events`))).toBeInTheDocument();
    expect(screen.getByText(new RegExp(`capped at ${LIVE_RING_CAP}`))).toBeInTheDocument();
  });

  // Once the ring is full, one event in means one event out and the row COUNT
  // stops changing — an anchor that watches the length quietly stops
  // compensating at exactly the moment there is most to scroll through.
  it("holds the operator's row still when arrivals prepend into a full ring", async () => {
    renderPage();
    open();
    await emit(Array.from({ length: LIVE_RING_CAP }, (_, i) => ev(i + 1)));

    const viewport = screen.getByRole("log").parentElement as HTMLElement;
    // jsdom does no layout, so scrollTop is inert; give this one element a real
    // one — the same boundary stub as offsetHeight above.
    let scrollTop = 40 * ROW_HEIGHT;
    Object.defineProperty(viewport, "scrollTop", {
      configurable: true,
      get: () => scrollTop,
      set: (next: number) => {
        scrollTop = next;
      },
    });
    fireEvent.scroll(viewport);

    await emit([ev(LIVE_RING_CAP + 1, { summary: "newest" })]);

    expect(scrollTop).toBe(41 * ROW_HEIGHT);
  });

  it("says events may have been missed when the controller seq has a hole", async () => {
    renderPage();
    open();
    await emit([ev(1), ev(2)]);
    expect(screen.queryByText(/may have been missed/)).not.toBeInTheDocument();

    await emit([ev(4)]);
    expect(screen.getByText(/1 event may have been missed/)).toBeInTheDocument();
  });

  // The loss a gap scan structurally cannot see: a tab that spends long enough hidden gets no
  // frames.
  it("says so when a hidden tab's backlog outgrew the queue and was trimmed", async () => {
    renderPage();
    open();
    emitHidden(Array.from({ length: LIVE_RING_CAP * 2 + 1 }, (_, i) => ev(i + 1)));
    await nextFrame();

    expect(screen.getByText(/events may have been missed/)).toBeInTheDocument();
    expect(
      screen.getByText(new RegExp(`Showing ${LIVE_RING_CAP} of ${LIVE_RING_CAP} events`)),
    ).toBeInTheDocument();
  });

  // Go's event type is an open string; TypeScript's union is a convenience, not
  // a guarantee. A sixth type must render, not blow the page up.
  it("renders an event type the frontend has never heard of", async () => {
    renderPage();
    open();
    await emit([ev(1, { type: "quantum_flux" as LiveEvent["type"], summary: "from the future" })]);

    const rows = screen.getAllByRole("listitem");
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveTextContent("from the future");
    // The Type column is gone (M3-4): the summary is the row's account of what happened.
    expect(rows[0]).not.toHaveTextContent("quantum_flux");
  });

  /* M3-4: the Type column duplicated the summary's own prefix word for word,
     so it went; Scope stays — it is the scannable right edge of the feed. */
  it("has no Type column, keeps the Scope column", async () => {
    renderPage();
    open();
    await emit([ev(1, { type: "check_observed", summary: "probe failed", scope: "node-a→node-b" })]);

    expect(screen.getByText("Scope")).toBeInTheDocument();
    expect(screen.queryByText("Type")).toBeNull();
    const row = screen.getAllByRole("listitem")[0];
    expect(row).toHaveTextContent("node-a→node-b");
    expect(row).not.toHaveTextContent("Check observed");
  });

  it("waits for events once the socket is open and the feed is empty", () => {
    renderPage();
    open();
    expect(screen.getByText(/Waiting for events/)).toBeInTheDocument();
    expect(screen.queryAllByRole("listitem")).toHaveLength(0);
  });

  it("explains itself when this replica has no realtime capability", () => {
    renderPage([]);
    open();
    expect(screen.getByText(/Delayed data/)).toBeInTheDocument();
    expect(screen.getByText(/not receiving the controller event stream/)).toBeInTheDocument();
  });

  // A cold load has no answer yet, and "no answer" is not "no realtime" —
  // reading one as the other flashes a yellow warning on every page load.
  it("shows the connecting skeleton while the capability probe is still in flight", () => {
    vi.stubGlobal("fetch", vi.fn(() => new Promise(() => {})));
    renderPage(null);

    expect(screen.getByText("Connecting to the event stream…")).toBeInTheDocument();
    expect(screen.queryByText(/not receiving the controller event stream/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Waiting for events/)).not.toBeInTheDocument();
  });

  // A rejection belongs to the connection that produced it; leaving it up would
  // pin a red alert over a feed that has since recovered.
  it("clears the topic rejection once the socket is open again", async () => {
    renderPage();
    open();
    act(() => {
      FakeSocket.last().emitEnvelope({ topic: TOPIC_LIVE, type: "error", seq: 0, data: "unknown topic" });
    });
    expect(screen.getByRole("alert")).toBeInTheDocument();

    act(() => {
      FakeSocket.last().emitClose();
    });
    // The client's reconnect backoff starts at 1s; wait it out rather than
    // reaching into its private timer.
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 1_100));
    });
    open();

    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("keeps the /live nav path that the route wiring keys off", () => {
    expect(NAV_ITEMS.map((item) => item.path)).toContain("/live");
  });
});

describe("LivePage scrollback (Task 5's GET /api/v1/events)", () => {
  it("fetches the first page of history on mount and renders it below the live rows, deduped by id", async () => {
    const seenBothWays = ev(2, { timestamp: "2026-07-28T09:30:00Z", summary: "seen both ways" });
    const historyOnly = ev(1, { timestamp: "2026-07-28T09:00:00Z", summary: "history only" });
    stubEventsFetch(() => json({ events: [seenBothWays, historyOnly], nextCursor: "" }));

    renderPage(["events"], true);
    open();
    await emit([ev(3, { timestamp: "2026-07-28T10:00:00Z", summary: "live only" }), seenBothWays]);
    await screen.findByText("history only");

    const rows = screen.getAllByRole("listitem");
    expect(rows).toHaveLength(3);
    expect(rows[0]).toHaveTextContent("live only");
    expect(rows[1]).toHaveTextContent("seen both ways");
    expect(rows[2]).toHaveTextContent("history only");
  });

  it("'Load older' appends the next page and disables itself once nextCursor is empty", async () => {
    const page1 = { events: [ev(5, { timestamp: "2026-07-28T09:50:00Z" })], nextCursor: "cursor-1" };
    const page2 = { events: [ev(4, { timestamp: "2026-07-28T09:40:00Z" })], nextCursor: "" };
    stubEventsFetch((qs) => json(qs.get("cursor") === "cursor-1" ? page2 : page1));

    renderPage(["events"], true);
    open();
    await screen.findByText("event 5");

    const loadOlder = screen.getByRole("button", { name: "Load older" });
    expect(loadOlder).not.toBeDisabled();
    fireEvent.click(loadOlder);
    await screen.findByText("event 4");

    expect(loadOlder).toBeDisabled();
  });

  it("makes no history request and shows no scrollback control when database.configured is false", async () => {
    const fetchMock = stubEventsFetch(() => json({ events: [], nextCursor: "" }));

    renderPage(["events"], false);
    open();
    await emit([ev(1, { summary: "live still works" })]);

    expect(screen.queryByRole("button", { name: "Load older" })).not.toBeInTheDocument();
    expect(fetchMock.mock.calls.some(([url]) => typeof url === "string" && url.startsWith("/api/v1/events"))).toBe(
      false,
    );
  });

  it("shows an inline notice on a 503 from /api/v1/events and leaves the live feed working", async () => {
    stubEventsFetch(
      () =>
        new Response(
          JSON.stringify({
            type: "about:blank",
            title: "event history not available",
            status: 503,
            detail: "set console.database.mode in the console config to enable GET /api/v1/events",
          }),
          { status: 503, headers: { "Content-Type": "application/problem+json" } },
        ),
    );

    renderPage(["events"], true);
    open();
    await screen.findByText(/set console.database.mode/);

    await emit([ev(1, { summary: "still live" })]);
    expect(screen.getAllByRole("listitem")).toHaveLength(1);
    expect(screen.getByText("still live")).toBeInTheDocument();
  });

  it("refetches history with the new type filter when it changes", async () => {
    const allTypes = { events: [ev(1, { type: "check_observed", summary: "history check" })], nextCursor: "" };
    const topologyOnly = {
      events: [ev(2, { type: "topology_changed", summary: "history topology" })],
      nextCursor: "",
    };
    const fetchMock = stubEventsFetch((qs) => json(qs.get("type") === "topology_changed" ? topologyOnly : allTypes));

    renderPage(["events"], true);
    open();
    await screen.findByText("history check");

    fireEvent.change(screen.getByLabelText("Type"), { target: { value: "topology_changed" } });
    await screen.findByText("history topology");

    expect(
      fetchMock.mock.calls.some(([url]) => typeof url === "string" && url.includes("type=topology_changed")),
    ).toBe(true);
  });

  /* The scope box drives TWO matchers: the client-side substring over what the
     socket delivered, and this server-side one, where GET /api/v1/events
     compares the scope for EQUALITY. A typed "node-a->node-b" matched nothing on
     either side; it has to be canonical by the time it reaches the query string. */
  it("sends the CANONICAL arrow to the server for a pair typed with hyphens", async () => {
    const fetchMock = stubEventsFetch(() => json({ events: [], nextCursor: "" }));

    renderPage(["events"], true);
    open();

    fireEvent.change(screen.getByLabelText("Scope contains"), { target: { value: "node-a -> node-b" } });

    await vi.waitFor(() =>
      expect(
        fetchMock.mock.calls.some(
          ([url]) =>
            typeof url === "string" &&
            new URLSearchParams(url.split("?")[1] ?? "").get("scope") === "node-a→node-b",
        ),
      ).toBe(true),
    );
  });
});

/* ── QA round 1: findings #10, #12, #13, #16 ─────────────────────────────── */

describe("LivePage — the feed's clock", () => {
  // #10: /live rendered a UTC ISO slice and the Overview card a local wall clock.
  it("stamps a row with the shared event clock, not a private ISO slice", async () => {
    renderPage();
    open();
    await emit([ev(1, { timestamp: "2026-07-28T10:00:00Z" })]);

    // fmtEventStamp, not fmtEventTime: a 2000-event ring reaches past midnight,
    // so a row that is not from today carries its day (finding 21).
    expect(screen.getByText(fmtEventStamp("2026-07-28T10:00:00Z"))).toBeInTheDocument();
    expect(screen.queryByText("10:00:00.000")).toBeNull();
  });

  it("carries the DAY for a row that is not from today, and drops it for one that is", async () => {
    renderPage();
    open();
    const today = new Date();
    today.setHours(9, 30, 0, 0);
    await emit([
      ev(1, { timestamp: "2026-07-28T10:00:00Z" }),
      ev(2, { timestamp: today.toISOString() }),
    ]);

    // Today's row is a bare clock; the old one is not.
    expect(screen.getByText(fmtEventStamp(today.toISOString()))).toBeInTheDocument();
    expect(fmtEventStamp(today.toISOString())).toMatch(/^\d\d:\d\d:\d\d$/);
    expect(fmtEventStamp("2026-07-28T10:00:00Z")).not.toMatch(/^\d\d:\d\d:\d\d$/);
  });
});

describe("LivePage — paused is not live (#12)", () => {
  it("drops the green Live badge while paused; the Paused chip is the state", async () => {
    renderPage();
    open();
    await emit([ev(1)]);
    expect(screen.getByText("Live", { selector: "span" })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Pause" }));

    expect(screen.queryByText("Live", { selector: "span" })).toBeNull();
    // Two chips now, and deliberately: the filter bar's "Paused · N buffered"
    // is the state, and the transport slot's "Paused · socket live" keeps the
    // socket answerable while the feed is held (finding 20).
    expect(screen.getAllByText(/^Paused ·/).length).toBeGreaterThanOrEqual(1);
    expect(within(screen.getByTestId("live-transport-slot")).getByText(/^Paused ·/)).toBeInTheDocument();
  });

  it("lights it again on resume", async () => {
    renderPage();
    open();
    await emit([ev(1)]);
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    fireEvent.click(screen.getByRole("button", { name: "Resume" }));

    expect(screen.getByText("Live", { selector: "span" })).toBeInTheDocument();
  });
});

describe("LivePage — one toolbar slot (#13)", () => {
  /** The toolbar's placement, expressed structurally: it is inside the block
   *  that sits next to the page heading. Same answer in both states, or the
   *  controls moved under the operator when the mode changed. */
  function toolbarSitsBesideTheHeading(): boolean {
    const toolbar = screen.getByTestId("live-toolbar");
    const headingBlock = screen.getByRole("heading", { name: "Events" }).parentElement;
    return headingBlock?.nextElementSibling?.contains(toolbar) ?? false;
  }

  it("keeps the toolbar in the same slot whether the transport badge is there or not", async () => {
    renderPage();
    open();
    await emit([ev(1)]);
    expect(toolbarSitsBesideTheHeading()).toBe(true);

    // Pausing removes the badge (#12) — the toolbar must not move with it.
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    expect(toolbarSitsBesideTheHeading()).toBe(true);
    // The badge's slot is still there, holding the width.
    expect(screen.getByTestId("live-transport-slot")).toBeInTheDocument();
  });
});

describe("LivePage — Load older says why it is disabled (#16)", () => {
  it("names the reason in the accessible name once there is nothing older", async () => {
    const page1 = { events: [ev(5)], nextCursor: "" };
    stubEventsFetch(() => json(page1));

    renderPage(["events"], true);
    open();
    await screen.findByText("event 5");

    const button = screen.getByRole("button", { name: /Load older/ });
    expect(button).toBeDisabled();
    // Both halves, in order. Matched loosely on the joiner: the name is the
    // concatenation of the button's text nodes, and dom-accessibility-api
    // trims each one where a real screen reader inserts a pause.
    expect(button).toHaveAccessibleName(/^Load older\s*Nothing older matches the current filters\.$/);
    expect(button).toHaveAttribute("title", "Nothing older matches the current filters.");
  });

  it("says nothing extra while it is loadable", async () => {
    stubEventsFetch(() => json({ events: [ev(5)], nextCursor: "cursor-1" }));

    renderPage(["events"], true);
    open();
    await screen.findByText("event 5");

    expect(screen.getByRole("button", { name: "Load older" })).not.toBeDisabled();
  });
});

/* ru The one case in this file that mounts LocaleProvider. */

describe("LivePage — ru", () => {
  afterEach(() => {
    /* vitest.setup.ts backs localStorage with ONE Map per test FILE, so a
       locale left here would leak into every later test in it. */
    localStorage.removeItem(LOCALE_STORAGE_KEY);
  });

  it("renders the feed chrome and the unfed-replica card in Russian", () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    // No "events" capability: resolved, and honestly not receiving the stream.
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    qc.setQueryData(["version"], { version: "1.6.0", commit: "abc123", capabilities: [] });
    qc.setQueryData(["config"], {
      auth: { mode: "anonymous", role: "admin" },
      anonymousBanner: true,
      controller: { configured: true },
      prometheus: { configured: true },
      database: { configured: false },
    });
    render(
      <QueryClientProvider client={qc}>
        <LocaleProvider>
          <LivePage />
        </LocaleProvider>
      </QueryClientProvider>,
    );

    expect(screen.getByRole("heading", { name: "События", level: 1 })).toBeInTheDocument();
    expect(screen.getByText("Ждём события")).toBeInTheDocument();
    expect(screen.getByText(/лента не сломана, её просто не кормят/)).toBeInTheDocument();
  });
});

/* ── the scope filter's placeholder has to FIT its box (QA round 6, #8) ──── */

describe("scope filter placeholder", () => {
  /* The input is w-64 with pl-8/pr-2, i.e. 216px of text room at text-sm.
     Measured against the console's own stack (14px ui-sans-serif) on the live
     stand: the old wordings were 221px (en) and 314px (ru) and clipped the
     example they existed to show; the current pair is 167px and 182px. The
     character budget below is that headroom expressed in a way jsdom, which
     lays nothing out, can still hold: ~7.9px per glyph in this stack. */
  const ROOM_PX = 216;
  const PX_PER_CHAR = 7.9;
  const BUDGET = Math.floor(ROOM_PX / PX_PER_CHAR);

  it.each(["en", "ru"] as const)("fits the box and keeps the example scope (%s)", (locale) => {
    const placeholder = liveDict[locale]["filters.scope.placeholder"];
    expect(placeholder.length).toBeLessThanOrEqual(BUDGET);
    expect(placeholder).toContain("node-a->node-b");
  });

  /* The example must be TYPEABLE. U+2192 in a placeholder is an instruction the
     reader cannot follow on any keyboard, which is how the owner ended up
     copying the arrow out of a row. */
  it.each(["en", "ru"] as const)("shows a form a keyboard can produce (%s)", (locale) => {
    expect(liveDict[locale]["filters.scope.placeholder"]).not.toContain("→");
  });

  it("is what the box actually renders", () => {
    renderPage();
    expect(screen.getByPlaceholderText("Scope — node-a->node-b")).toBeInTheDocument();
  });
});

/* ── QA scope 5 ─────────────────────────────────────────────────────────── */

/* #19. The gap warning's only explanation lived in a title attribute, which is
   invisible to touch and to anyone who does not hover a warning triangle. */
describe("the gap warning explains itself in the open (#19)", () => {
  it("offers a control that reveals the reason, and hides it again", async () => {
    renderPage();
    open();
    await emit([ev(1), ev(2)]);
    await emit([ev(4)]);

    expect(screen.queryByTestId("missed-note")).toBeNull();
    const why = screen.getByRole("button", { name: "Why events may have been missed" });
    expect(why).toHaveAttribute("aria-expanded", "false");

    fireEvent.click(why);
    expect(screen.getByTestId("missed-note")).toHaveTextContent(/holes in the controller's event numbering/i);
    expect(why).toHaveAttribute("aria-expanded", "true");

    fireEvent.click(why);
    expect(screen.queryByTestId("missed-note")).toBeNull();
  });

  it("names the note it opens, so the revealed text is reachable from the control", async () => {
    renderPage();
    open();
    await emit([ev(1)]);
    await emit([ev(3)]);

    const why = screen.getByRole("button", { name: "Why events may have been missed" });
    fireEvent.click(why);
    const controls = why.getAttribute("aria-controls");
    expect(controls).toBeTruthy();
    expect(document.getElementById(controls!)).toHaveAttribute("data-testid", "missed-note");
  });

  it("offers nothing at all when no events were missed", async () => {
    renderPage();
    open();
    await emit([ev(1), ev(2)]);
    expect(screen.queryByRole("button", { name: "Why events may have been missed" })).toBeNull();
  });
});

/* #20. Pausing emptied the transport slot, so the one fact an operator needs
   BEFORE pressing Resume — is the socket still there — went with it. */
describe("a paused feed still reports its socket (#20)", () => {
  it("says the socket is live while paused", async () => {
    renderPage();
    open();
    await emit([ev(1)]);
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));

    const slot = screen.getByTestId("live-transport-slot");
    expect(within(slot).getByText("Paused · socket live")).toBeInTheDocument();
  });

  it("says the socket is DOWN when it dropped during the pause", async () => {
    renderPage();
    open();
    await emit([ev(1)]);
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));

    act(() => {
      FakeSocket.last().emitClose();
    });

    const slot = screen.getByTestId("live-transport-slot");
    expect(within(slot).getByText("Paused · socket down")).toBeInTheDocument();
  });
});

/* #22. At the ring's cap "Load older" stayed enabled and did nothing: anything
   it fetched would be dropped on the way into a full buffer. */
describe("Load older is disabled at the cap (#22)", () => {
  it("disables itself, with the cap as the reason, once the ring is full", async () => {
    stubEventsFetch(() => json({ events: [ev(1)], nextCursor: "cursor-1" }));
    renderPage(["events"], true);
    open();
    await screen.findByRole("button", { name: /Load older/ });

    emitHidden(Array.from({ length: LIVE_RING_CAP }, (_, i) => ev(i + 1)));
    await nextFrame();

    const button = screen.getByRole("button", { name: /Load older/ });
    expect(button).toBeDisabled();
    expect(button).toHaveAttribute("title", expect.stringContaining(String(LIVE_RING_CAP)));
    // Readable, not merely hoverable.
    expect(button).toHaveAccessibleName(expect.stringContaining("buffer is full"));
  });

  it("stays live while the ring has room", async () => {
    stubEventsFetch(() => json({ events: [ev(1)], nextCursor: "cursor-1" }));
    renderPage(["events"], true);
    open();
    await screen.findByText("event 1");
    expect(screen.getByRole("button", { name: /Load older/ })).toBeEnabled();
  });
});

/* ── QA round 5b: a filter is not data loss ───────────────────────────────── */

describe("countMissedEvents and server-filtered history", () => {
  const ev = (seq: number, over: Partial<LiveEvent> = {}): LiveEvent => ({
    id: `e${seq}`,
    seq,
    type: "check_observed",
    severity: "info",
    scope: "cluster",
    timestamp: new Date(1_700_000_000_000 + seq * 1000).toISOString(),
    summary: `event ${seq}`,
    details: null,
    ...over,
  });

  it("counts a real hole in the unfiltered stream", () => {
    expect(countMissedEvents([ev(1), ev(5)])).toBe(3);
  });

  it("ignores rows the SERVER filtered — those holes are the operator's own filter", () => {
    const filtered = [ev(1, { filteredHistory: true }), ev(100, { filteredHistory: true })];
    expect(countMissedEvents(filtered)).toBe(0);
  });

  it("still sees a hole between unfiltered rows when filtered rows are mixed in", () => {
    const mixed = [ev(1), ev(4), ev(900, { filteredHistory: true })];
    expect(countMissedEvents(mixed)).toBe(2);
  });
});
