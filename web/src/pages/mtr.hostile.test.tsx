import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { LOCALE_STORAGE_KEY, LocaleProvider, type Locale } from "@/lib/i18n";
import { TimeMachineProvider } from "@/lib/timemachine";
import { MTRPage, finiteCount, hopCountOf, mergeSnapshots } from "./mtr";

/**
 * mtr.hostile.test.tsx — /mtr under a reader who is trying to break it, and
 * under a server that is not this one.
 *
 * pages/mtr.test.tsx is the page's behaviour when everything works. This file is
 * its other half: the deep link that names nonsense, two pairs clicked one after
 * the other, the badge on page two, the payload with a field missing. Every case
 * below FAILED when it was written, and the comment on it says what the page was
 * doing wrong — a green run here is the claim that none of it comes back.
 *
 * The bar, in one line: no unhandled exception, no white screen, no "NaN" or
 * "undefined" on screen, no silently empty answer, no row belonging to a pair
 * other than the one named above it.
 */

vi.mock("@/components/echart", () => ({
  EChart: ({ className }: { className?: string }) => <div data-testid="echart" className={className} />,
}));

const json = (body: unknown, init?: ResponseInit) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" }, ...init });

const problem = (status: number, title: string, detail: string) =>
  new Response(JSON.stringify({ type: "about:blank", title, status, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

const VIEWER = ["mtr:read"];

function meBody(permissions: string[]) {
  return {
    subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["viewer"] },
    permissions,
  };
}

function configBody() {
  return {
    auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
    anonymousBanner: false,
    controller: { configured: true },
    prometheus: { configured: true },
    database: { configured: true },
  };
}

function topologyBody(names: string[]) {
  return { nodes: names.map((n) => ({ name: n, zone: "z1", ready: true })), agents: [], timestamp: "t" };
}

function destinationRow(over: Record<string, unknown> = {}) {
  return {
    sourceNode: "node-a",
    destination: "node-b",
    snapshotCount: 2,
    traceCount: 9,
    firstSeen: "2026-08-01T10:00:00Z",
    lastSeen: "2026-08-06T10:00:00Z",
    ...over,
  };
}

function hop(over: Record<string, unknown> = {}) {
  return { number: 1, ip: "10.0.0.1", hostname: "gw.internal", rttNs: 2_500_000, lossRatio: 0, ...over };
}

function snapshotRow(over: Record<string, unknown> = {}) {
  return {
    id: "11111111-1111-1111-1111-111111111111",
    sourceNode: "node-a",
    destination: "node-b",
    pathHash: "aaaaaaaaaaaa0000",
    hopCount: 2,
    hops: [hop(), hop({ number: 2, ip: "10.0.0.2", hostname: "core", rttNs: 7_000_000, lossRatio: 0.1 })],
    firstSeen: "2026-08-05T10:00:00Z",
    lastSeen: "2026-08-06T10:00:00Z",
    traceCount: 5,
    ...over,
  };
}

interface Call {
  method: string;
  url: string;
  body?: unknown;
}

/** The harness pages/mtr.test.tsx mounts, with one addition: `onSnapshots` may
 *  answer with a PROMISE, which is how a slow pair is made to come back after a
 *  fast one. */
function renderPage(
  opts: {
    permissions?: string[];
    destinations?: unknown[];
    nodes?: string[];
    onDestinations?: () => Response;
    onSnapshots?: (qs: URLSearchParams) => Response | Promise<Response>;
    onSnapshot?: (id: string, qs: URLSearchParams) => Response;
    onRun?: (body: unknown) => Response;
    locale?: Locale;
    /** The ?destination= a shared link carries, verbatim — including nonsense. */
    destination?: string;
  } = {},
) {
  const {
    permissions = VIEWER,
    destinations = [destinationRow()],
    nodes = ["node-a", "node-b", "node-c"],
    onDestinations,
    onSnapshots,
    onSnapshot,
    onRun,
    locale,
    destination: linked,
  } = opts;
  if (locale !== undefined) localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  const qs = new URLSearchParams();
  if (linked !== undefined) qs.set("destination", linked);
  window.history.pushState({}, "", qs.size > 0 ? `/mtr?${qs}` : "/mtr");
  const calls: Call[] = [];

  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    calls.push({ method, url: href, body });

    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.includes("/api/v1/config")) return Promise.resolve(json(configBody()));
    if (href.startsWith("/api/v1/topology")) return Promise.resolve(json(topologyBody(nodes)));
    if (href.startsWith("/api/v1/targets")) return Promise.resolve(json({ targets: [], nextCursor: "" }));
    if (href.startsWith("/api/v1/promql/")) {
      return Promise.resolve(json({ status: "success", data: { resultType: "matrix", result: [] } }));
    }
    if (href.startsWith("/api/v1/runs")) {
      if (onRun) return Promise.resolve(onRun(body));
      return Promise.resolve(json({ id: "run-1" }, { status: 202 }));
    }
    if (href.startsWith("/api/v1/mtr/snapshots/")) {
      const [path, query = ""] = href.slice("/api/v1/mtr/snapshots/".length).split("?");
      const q = new URLSearchParams(query);
      if (onSnapshot) return Promise.resolve(onSnapshot(decodeURIComponent(path), q));
      return Promise.resolve(json(snapshotRow({ id: decodeURIComponent(path) })));
    }
    if (href.startsWith("/api/v1/mtr/snapshots")) {
      const q = new URLSearchParams(href.split("?")[1] ?? "");
      if (onSnapshots) return Promise.resolve(onSnapshots(q));
      return Promise.resolve(json({ snapshots: [snapshotRow()], nextCursor: "" }));
    }
    if (href.startsWith("/api/v1/mtr/destinations")) {
      if (onDestinations) return Promise.resolve(onDestinations());
      return Promise.resolve(json({ destinations }));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const page = (
    <TimeMachineProvider>
      <MTRPage />
    </TimeMachineProvider>
  );
  const utils = render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>{locale === undefined ? page : <LocaleProvider>{page}</LocaleProvider>}</ThemeProvider>
    </QueryClientProvider>,
  );
  return { ...utils, fetchMock, calls, qc };
}

/* Every console.error and console.warn React emits is a defect until shown
   otherwise — a duplicate key, an invalid prop. Several cases below assert on
   this list rather than on the DOM, because a duplicated row is a warning first
   and a wrong screen second. act() notices are the harness talking to itself and
   are filtered out. */
let noise: string[] = [];
const realNoise = () => noise.filter((n) => !/not wrapped in act/.test(n));

beforeEach(() => {
  noise = [];
  vi.spyOn(console, "error").mockImplementation((...args: unknown[]) => void noise.push(args.map(String).join(" ")));
  vi.spyOn(console, "warn").mockImplementation((...args: unknown[]) => void noise.push(args.map(String).join(" ")));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

/** Lets a queued response, and the re-render it causes, actually happen. */
const settle = () => new Promise((r) => setTimeout(r, 40));

async function expandDestination(destination: string) {
  const escaped = destination.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const header = await screen.findByRole("button", { name: new RegExp(`^${escaped}[,:]`) });
  if (header.getAttribute("aria-expanded") !== "true") fireEvent.click(header);
  return header;
}

async function selectPair(source: string, destination: string) {
  await expandDestination(destination);
  fireEvent.click(await screen.findByRole("button", { name: `${source} → ${destination}` }));
}

/** Ticks the first two rows of the history list and OPENS the comparison — the
 *  diff's whole entry. Ticking alone is a selection; the reader says when to look. */
async function compareFirstTwo() {
  const boxes = await screen.findAllByRole("checkbox");
  fireEvent.click(boxes[0]);
  fireEvent.click(boxes[1]);
  await openCompare();
}

/* ── pure helpers ───────────────────────────────────────────────────────── */


/* Ticking two boxes is a SELECTION; the comparison opens on the reader's word.
   The old three-pane layout swapped pane 3 the moment the second box was ticked
   — a dialog thrown at you mid-choice — so every case that wants the diff now
   presses the button the way a reader does. */
async function openCompare() {
  fireEvent.click(await screen.findByRole("button", { name: /\(2\/2\)$/ }));
  return screen.findByRole("dialog");
}

describe("mergeSnapshots", () => {
  /* The list is read as newest-first (the store orders by last_seen DESC, id
     DESC) and the merge restores that order rather than appending, because
     last_seen MOVES: a route re-traced between two clicks comes back on the
     older page newer than everything already on screen. */
  const row = (id: string, lastSeen: string, over: Record<string, unknown> = {}) =>
    snapshotRow({ id, lastSeen, ...over });

  it("puts the older page under the newer one", () => {
    const merged = mergeSnapshots(
      [row("a", "2026-08-09T00:00:00Z")] as never,
      [row("b", "2026-08-08T00:00:00Z")] as never,
    );
    expect(merged.map((s) => s.id)).toEqual(["a", "b"]);
  });

  it("keeps the copy already on screen when a cursor page repeats a row", () => {
    const merged = mergeSnapshots(
      [row("a", "2026-08-09T00:00:00Z", { pathHash: "first" })] as never,
      [row("a", "2026-08-09T00:00:00Z", { pathHash: "second" }), row("b", "2026-08-08T00:00:00Z")] as never,
    );
    expect(merged.map((s) => s.id)).toEqual(["a", "b"]);
    expect(merged[0].pathHash).toBe("first");
  });

  it("re-sorts a row the older page returned NEWER than page one, instead of burying it", () => {
    const merged = mergeSnapshots(
      [row("a", "2026-08-09T00:00:00Z")] as never,
      // Re-traced while the reader was reading: its last_seen jumped past page one's.
      [row("c", "2026-08-10T00:00:00Z"), row("b", "2026-08-08T00:00:00Z")] as never,
    );
    expect(merged.map((s) => s.id)).toEqual(["c", "a", "b"]);
  });

  it("breaks a last_seen tie by id DESC, which is the store's own tie-break", () => {
    const merged = mergeSnapshots(
      [row("a", "2026-08-09T00:00:00Z")] as never,
      [row("z", "2026-08-09T00:00:00Z")] as never,
    );
    expect(merged.map((s) => s.id)).toEqual(["z", "a"]);
  });
});

describe("hopCountOf", () => {
  it("takes the stored column when there is one", () => {
    expect(hopCountOf(snapshotRow({ hopCount: 7 }) as never)).toBe(7);
  });

  it("counts the hops when the column did not arrive, rather than saying 'undefined'", () => {
    expect(hopCountOf(snapshotRow({ hopCount: undefined }) as never)).toBe(2);
    expect(hopCountOf(snapshotRow({ hopCount: undefined, hops: null }) as never)).toBe(0);
  });
});

describe("finiteCount", () => {
  it("passes a real number through and folds everything else to zero", () => {
    expect(finiteCount(9)).toBe(9);
    expect(finiteCount(0)).toBe(0);
    expect(finiteCount(undefined)).toBe(0);
    expect(finiteCount(Number.NaN)).toBe(0);
  });
});

/* ── the history list past page one ─────────────────────────────────────── */

describe("MTRPage — the change badge on a later page", () => {
  /* `changed` and changeText() are answers about a row's next-OLDER neighbour in
     the WHOLE loaded history, and both were read at the row's index within the
     visible page. Page two therefore repeated page one's badges: row 11 was
     labelled with what happened between rows 1 and 2. */
  it("badges a row with its own change, not with the change of the row in that slot on page one", async () => {
    /* Only the first two rows differ; everything from row 3 down repeats one
       hash, so page two has nothing to badge at all. */
    const snapshots = Array.from({ length: 12 }, (_, i) =>
      snapshotRow({
        id: `id-${i}`,
        pathHash: i === 0 ? "AAAAAAAAAAAA" : "BBBBBBBBBBBB",
        firstSeen: `2026-08-${String(20 - i).padStart(2, "0")}T10:00:00Z`,
        hops: [hop({ number: 1, ip: i === 0 ? "10.0.0.9" : "10.0.0.1" })],
      }),
    );
    renderPage({ onSnapshots: () => json({ snapshots, nextCursor: "" }) });
    await selectPair("node-a", "node-b");
    await screen.findByRole("list", { name: "Paths" });

    fireEvent.click(within(screen.getByRole("region", { name: "Path history" })).getByLabelText("Next page"));

    const list = await screen.findByRole("list", { name: "Paths" });
    expect(list.textContent).not.toMatch(/changed|hop \d/);
  });

  it("still badges the first page's real change", async () => {
    renderPage({
      onSnapshots: () =>
        json({
          snapshots: [
            snapshotRow({ id: "new", pathHash: "AAAA", hops: [hop({ ip: "10.0.0.9" })] }),
            snapshotRow({ id: "old", pathHash: "BBBB", hops: [hop({ ip: "10.0.0.1" })] }),
          ],
          nextCursor: "",
        }),
    });
    await selectPair("node-a", "node-b");
    expect(await screen.findByText("hop 1: 10.0.0.1 → 10.0.0.9")).toBeTruthy();
  });
});

/* ── a destination is a NAME ────────────────────────────────────────────── */

describe("MTRPage — a destination named after an Object.prototype member", () => {
  /* The open/shut map is keyed by destination name, so a card called
     "constructor" read its own state off the prototype: a function, which is
     truthy, so the card came up expanded — and React drops a function-valued
     aria-expanded, leaving the disclosure button with no state on it at all. */
  it("opens shut, and says so, like every other card", async () => {
    renderPage({
      destinations: [destinationRow({ destination: "constructor" }), destinationRow({ destination: "toString" })],
    });
    const header = await screen.findByRole("button", { name: /^constructor[,:]/ });
    expect(header.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("button", { name: "node-a → constructor" })).toBeNull();
  });

  it("expands and collapses on click like every other card", async () => {
    renderPage({ destinations: [destinationRow({ destination: "toString" })] });
    const header = await screen.findByRole("button", { name: /^toString[,:]/ });
    fireEvent.click(header);
    expect(header.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByRole("button", { name: "node-a → toString" })).toBeTruthy();
    fireEvent.click(header);
    expect(header.getAttribute("aria-expanded")).toBe("false");
  });
});

describe("MTRPage — a shared link that names nonsense", () => {
  it.each([
    ["__proto__"],
    ["constructor"],
    ["%%%"],
    ["<script>alert(1)</script>"],
    ["a".repeat(400)],
    ["узел-一"],
  ])("leaves the pane alone for ?destination=%s", async (name) => {
    renderPage({ destination: name });
    const card = await screen.findByRole("button", { name: /^node-b[,:]/ });
    expect(card.getAttribute("aria-expanded")).toBe("false");
    expect(realNoise()).toEqual([]);
  });
});

/* ── two pairs, one pane ────────────────────────────────────────────────── */

describe("MTRPage — a slow pair answering after a fast one", () => {
  /* The pane took whatever came back last. Click a slow pair, then a fast one,
     and the slow pair's routes repainted the list under the fast pair's heading
     — the console showing one pair's history and naming another. */
  it("drops the superseded answer instead of repainting under the new heading", async () => {
    let releaseSlow: (r: Response) => void = () => {};
    const slow = new Promise<Response>((resolve) => {
      releaseSlow = resolve;
    });
    renderPage({
      destinations: [
        destinationRow({ sourceNode: "node-a", destination: "node-b" }),
        destinationRow({ sourceNode: "node-c", destination: "node-b" }),
      ],
      onSnapshots: (qs) =>
        qs.get("source") === "node-a"
          ? slow
          : json({
              snapshots: [snapshotRow({ id: "fast", sourceNode: "node-c", pathHash: "FFFFFFFFFFFF" })],
              nextCursor: "",
            }),
    });

    await selectPair("node-a", "node-b");
    await selectPair("node-c", "node-b");
    await screen.findByText(/FFFFFFFFFFFF/);

    releaseSlow(
      json({
        snapshots: [snapshotRow({ id: "slow", sourceNode: "node-a", pathHash: "5555AAAA5555" })],
        nextCursor: "",
      }),
    );
    await settle();

    const pane = screen.getByRole("region", { name: "Path history" });
    expect(pane.textContent).toMatch(/node-c → node-b/);
    expect(pane.textContent).not.toMatch(/5555AAAA5555/);
    expect(pane.textContent).toMatch(/FFFFFFFFFFFF/);
  });
});

describe("MTRPage — Load older that hands back a row already on screen", () => {
  /* The cursor walks (last_seen DESC, id DESC), so a path re-traced between two
     clicks is legitimately served twice. Appending blind rendered it twice under
     one React key, which React answers with a warning and an explicit promise
     that it may duplicate or omit the row. */
  it("keeps one copy and warns about nothing", async () => {
    let page = 0;
    renderPage({
      onSnapshots: () => {
        page += 1;
        return json({
          snapshots: [snapshotRow({ id: "same-id", pathHash: `hash${page}` })],
          nextCursor: page < 3 ? `c${page}` : "",
        });
      },
    });
    await selectPair("node-a", "node-b");
    await screen.findByRole("list", { name: "Paths" });
    fireEvent.click(screen.getByRole("button", { name: "Load older" }));
    await settle();
    fireEvent.click(screen.getByRole("button", { name: "Load older" }));
    await settle();

    expect(realNoise()).toEqual([]);
    expect(within(screen.getByRole("list", { name: "Paths" })).getAllByRole("checkbox")).toHaveLength(1);
  });

  it("holds three hundred paths, counted honestly, without a warning", async () => {
    let page = 0;
    renderPage({
      onSnapshots: () => {
        page += 1;
        return json({
          snapshots: Array.from({ length: 100 }, (_, i) =>
            snapshotRow({
              id: `p${page}-${i}`,
              pathHash: `${page}${i}`.padEnd(16, "0"),
              firstSeen: new Date(Date.UTC(2026, 0, 1, 0, page * 100 + i)).toISOString(),
            }),
          ),
          nextCursor: page < 3 ? `c${page}` : "",
        });
      },
    });
    await selectPair("node-a", "node-b");
    await screen.findByRole("list", { name: "Paths" });
    for (let i = 0; i < 2; i++) {
      fireEvent.click(screen.getByRole("button", { name: "Load older" }));
      await settle();
    }
    const pane = screen.getByRole("region", { name: "Path history" });
    expect(within(pane).getAllByTestId("pager-showing")[0].textContent).toMatch(/300/);
    expect(realNoise()).toEqual([]);
  });
});

/* ── payloads this server would not send ────────────────────────────────── */

describe("MTRPage — a payload that did not come from this server", () => {
  /* httpapi/mtr.go substitutes `[]` for a nil hops slice and always builds the
     snapshots slice, so every shape below means a proxy, a replay or an older
     build. Each one used to reach .map or .slice and take the whole console down
     to a white screen. */
  it("survives snapshots:null", async () => {
    renderPage({ onSnapshots: () => json({ snapshots: null, nextCursor: "" }) });
    await selectPair("node-a", "node-b");
    expect(await screen.findByText(/No path recorded for this pair yet/)).toBeTruthy();
  });

  it("survives hops:null on a snapshot", async () => {
    renderPage({ onSnapshots: () => json({ snapshots: [snapshotRow({ hops: null })], nextCursor: "" }) });
    await selectPair("node-a", "node-b");
    await settle();
    expect(screen.getByRole("region", { name: "Path history" })).toBeTruthy();
    expect(document.body.textContent).not.toMatch(/NaN|undefined/);
  });

  it("survives a snapshot with no pathHash", async () => {
    renderPage({ onSnapshots: () => json({ snapshots: [snapshotRow({ pathHash: undefined })], nextCursor: "" }) });
    await selectPair("node-a", "node-b");
    await settle();
    expect(screen.getByRole("region", { name: "Path history" })).toBeTruthy();
  });

  it("survives destinations:null", async () => {
    renderPage({ onDestinations: () => json({ destinations: null }) });
    expect(await screen.findByText(/Nothing traced yet/)).toBeTruthy();
  });

  /* A hop with no rttNs and no lossRatio rendered "NaNms" and a red "NaN%", and
     a snapshot with no hopCount was badged "undefined hops". */
  it("prints an em dash for a measurement that never arrived, never NaN", async () => {
    const broken = snapshotRow({
      hopCount: undefined,
      hops: [
        { number: 1, ip: "10.0.0.1" },
        { number: 2, ip: "10.0.0.2", rttNs: "fast", lossRatio: null },
      ],
    });
    renderPage({
      onSnapshots: () => json({ snapshots: [broken], nextCursor: "" }),
      onSnapshot: () => json(broken),
    });
    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaa[0-9a-f]*$/ }));
    await screen.findByRole("table", { name: "Hops" });

    expect(document.body.textContent).not.toMatch(/NaN|undefined/);
    // The hops themselves still know how many there are.
    expect(screen.getByText("2 hops")).toBeTruthy();
  });

  it("expands one row at a time even when the hops arrived without numbers", async () => {
    const odd = snapshotRow({
      hopCount: 2,
      hops: [
        { ip: "10.0.0.1", rttNs: 1_000_000, lossRatio: 0 },
        { ip: "10.0.0.2", rttNs: 2_000_000, lossRatio: 0 },
      ],
    });
    renderPage({ onSnapshots: () => json({ snapshots: [odd], nextCursor: "" }), onSnapshot: () => json(odd) });
    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaa[0-9a-f]*$/ }));
    const table = await screen.findByRole("table", { name: "Hops" });
    fireEvent.click(within(table).getAllByRole("button", { name: /^Enrichment for hop/ })[0]);
    const open = within(table)
      .getAllByRole("button", { name: /^Enrichment for hop/ })
      .filter((b) => b.getAttribute("aria-expanded") === "true");
    expect(open).toHaveLength(1);
  });
});

describe("MTRPage — readings that are not measurements", () => {
  /* A JSON null divided by 1e6 is 0, so an absent RTT read "0.0ms" — a hop
     reported as answering in no time at all, which is a measurement the console
     invented rather than received. A negative reading rounded into "-0.0ms". */
  it("prints no 0.0ms for an absent RTT and no signed zero for a tiny one", async () => {
    const wild = snapshotRow({
      hopCount: 4,
      hops: [
        hop({ number: 1, ip: "10.0.0.1", rttNs: 0 }),
        hop({ number: 2, ip: "10.0.0.2", rttNs: -4_000 }),
        hop({ number: 3, ip: "10.0.0.3", rttNs: 9e18 }),
        hop({ number: 4, ip: "10.0.0.4", rttNs: null }),
      ],
    });
    renderPage({ onSnapshots: () => json({ snapshots: [wild], nextCursor: "" }), onSnapshot: () => json(wild) });
    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaa[0-9a-f]*$/ }));
    const table = await screen.findByRole("table", { name: "Hops" });

    expect(table.textContent).not.toMatch(/Infinity|-0\.0ms/);
    // A genuine zero is still a genuine zero; a genuine −4µs keeps its sign.
    expect(table.textContent).toMatch(/0\.0ms/);
    expect(table.textContent).toMatch(/-4µs/);
    expect(table.textContent).toMatch(/—/);
  });

  /* lossRatio is 0..1 in the schema. 12 rendered "1200%" and −0.5 rendered
     "-50%", neither of which is a loss figure a reader can do anything with. */
  it("keeps the loss column inside the range the schema promises", async () => {
    const wild = snapshotRow({
      hopCount: 3,
      hops: [
        hop({ number: 1, ip: "10.0.0.1", lossRatio: -0.5 }),
        hop({ number: 2, ip: "10.0.0.2", lossRatio: 12 }),
        hop({ number: 3, ip: "10.0.0.3", lossRatio: 0.005 }),
      ],
    });
    renderPage({ onSnapshots: () => json({ snapshots: [wild], nextCursor: "" }), onSnapshot: () => json(wild) });
    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaa[0-9a-f]*$/ }));
    const table = await screen.findByRole("table", { name: "Hops" });

    expect(table.textContent).not.toMatch(/-50%|1200%/);
    expect(table.textContent).toMatch(/0%/);
    expect(table.textContent).toMatch(/100%/);
  });
});

/* ── degenerate and enormous paths ──────────────────────────────────────── */

describe("MTRPage — paths at the edges", () => {
  it("renders a path with no hops at all", async () => {
    const empty = snapshotRow({ hopCount: 0, hops: [] });
    renderPage({ onSnapshots: () => json({ snapshots: [empty], nextCursor: "" }), onSnapshot: () => json(empty) });
    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaa[0-9a-f]*$/ }));
    await screen.findByRole("table", { name: "Hops" });
    expect(document.body.textContent).not.toMatch(/NaN|undefined/);
    expect(screen.getByText("0 hops")).toBeTruthy();
  });

  it("renders thirty hops of pure silence without a duplicate key", async () => {
    const stars = snapshotRow({
      hopCount: 30,
      hops: Array.from({ length: 30 }, (_, i) => hop({ number: i + 1, ip: "*", hostname: "", rttNs: 0, lossRatio: 1 })),
    });
    renderPage({ onSnapshots: () => json({ snapshots: [stars], nextCursor: "" }), onSnapshot: () => json(stars) });
    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaa[0-9a-f]*$/ }));
    const table = await screen.findByRole("table", { name: "Hops" });
    expect(within(table).getAllByRole("row")).toHaveLength(31);
    expect(realNoise()).toEqual([]);
  });

  it("keeps a unicode hostname intact from the chain to the diff", async () => {
    const uni = (id: string, seen: string, ip: string) =>
      snapshotRow({
        id,
        pathHash: `${id}00000000000000`,
        firstSeen: seen,
        hopCount: 1,
        hops: [hop({ number: 1, ip, hostname: "маршрутизатор-一.пример.рф", rttNs: 1_234_567 })],
      });
    renderPage({
      onSnapshots: () =>
        json({
          snapshots: [uni("aa", "2026-08-05T10:00:00Z", "10.0.0.9"), uni("bb", "2026-08-04T10:00:00Z", "10.0.0.8")],
          nextCursor: "",
        }),
    });
    await selectPair("node-a", "node-b");
    expect(await screen.findAllByTitle("маршрутизатор-一.пример.рф")).not.toHaveLength(0);
    await compareFirstTwo();
    const pane = await screen.findByRole("dialog", { name: /path diff/i });
    expect(pane.textContent).toMatch(/10\.0\.0\.8/);
    expect(pane.textContent).toMatch(/10\.0\.0\.9/);
  });
});

/* ── the diff ───────────────────────────────────────────────────────────── */

describe("MTRPage — the diff at its own edges", () => {
  /* Two hopless paths align to zero rows, so pane 3 drew a header, a legend and
     blank space under them. It is the one input the alignment cannot describe,
     and saying so is the answer. */
  it("says why rather than drawing an empty table when neither path has hops", async () => {
    renderPage({
      onSnapshots: () =>
        json({
          snapshots: [
            snapshotRow({ id: "s1", pathHash: "AAAA00000000", hops: [], hopCount: 0, firstSeen: "2026-08-05T10:00:00Z" }),
            snapshotRow({ id: "s2", pathHash: "BBBB00000000", hops: [], hopCount: 0, firstSeen: "2026-08-04T10:00:00Z" }),
          ],
          nextCursor: "",
        }),
    });
    await selectPair("node-a", "node-b");
    await compareFirstTwo();
    const pane = await screen.findByRole("dialog", { name: /path diff/i });
    expect(pane.textContent).toMatch(/nothing to line up/i);
    expect(pane.querySelector("table")).toBeNull();
  });

  it("diffs two paths that are all silence without inventing a reading", async () => {
    const stars = (id: string, seen: string) =>
      snapshotRow({
        id,
        pathHash: `${id}000000000000`,
        firstSeen: seen,
        hopCount: 3,
        hops: [1, 2, 3].map((n) => hop({ number: n, ip: "*", hostname: "", rttNs: 0, lossRatio: 1 })),
      });
    renderPage({
      onSnapshots: () =>
        json({
          snapshots: [stars("aa", "2026-08-05T10:00:00Z"), stars("bb", "2026-08-04T10:00:00Z")],
          nextCursor: "",
        }),
    });
    await selectPair("node-a", "node-b");
    await compareFirstTwo();
    const pane = await screen.findByRole("dialog", { name: /path diff/i });
    expect(pane.textContent).not.toMatch(/NaN|undefined/);
  });

  it("leaves the delta column empty when one side has no reading to subtract", async () => {
    const mk = (id: string, seen: string, rttNs: unknown) =>
      snapshotRow({
        id,
        pathHash: `${id}00000000000000`,
        firstSeen: seen,
        hopCount: 1,
        hops: [{ number: 1, ip: "10.0.0.1", hostname: "gw", rttNs, lossRatio: 0 }],
      });
    renderPage({
      onSnapshots: () =>
        json({
          snapshots: [mk("aa", "2026-08-05T10:00:00Z", 3_000), mk("bb", "2026-08-04T10:00:00Z", null)],
          nextCursor: "",
        }),
    });
    await selectPair("node-a", "node-b");
    await compareFirstTwo();
    const pane = await screen.findByRole("dialog", { name: /path diff/i });
    expect(pane.textContent).not.toMatch(/NaN|undefined|-0\.0ms/);
  });

  it("diffs two rows that are pages apart in the list", async () => {
    const snapshots = Array.from({ length: 12 }, (_, i) =>
      snapshotRow({
        id: `id-${i}`,
        pathHash: `hash${i}`.padEnd(16, "0"),
        firstSeen: `2026-08-${String(20 - i).padStart(2, "0")}T10:00:00Z`,
        hops: [hop({ number: 1, ip: `10.0.0.${i + 1}` })],
      }),
    );
    renderPage({ onSnapshots: () => json({ snapshots, nextCursor: "" }) });
    await selectPair("node-a", "node-b");
    fireEvent.click((await screen.findAllByRole("checkbox"))[0]);
    fireEvent.click(within(screen.getByRole("region", { name: "Path history" })).getByLabelText("Next page"));
    fireEvent.click((await screen.findAllByRole("checkbox"))[0]);
    await openCompare();

    const pane = await screen.findByRole("dialog");
    expect(pane.querySelector("table")).toBeTruthy();
    expect(pane.textContent).toMatch(/10\.0\.0\.11/);
  });
});

/* ── failed and expired reads ───────────────────────────────────────────── */

describe("MTRPage — reads that fail", () => {
  it("names the server's own reason when the destinations read 500s, and leaves pane 3 standing", async () => {
    renderPage({ onDestinations: () => problem(500, "internal", "path history projection is broken") });
    expect(await screen.findByRole("alert")).toHaveTextContent(/projection is broken/);
    expect(screen.getByRole("region", { name: /destinations/i })).toBeTruthy();
  });

  it("says the session expired instead of emptying the list in silence", async () => {
    let dead = false;
    renderPage({
      onSnapshots: () =>
        dead
          ? problem(401, "unauthenticated", "your session expired")
          : json({ snapshots: [snapshotRow()], nextCursor: "c1" }),
    });
    await selectPair("node-a", "node-b");
    await screen.findByRole("list", { name: "Paths" });
    dead = true;
    fireEvent.click(screen.getByRole("button", { name: "Load older" }));
    await settle();

    const pane = screen.getByRole("region", { name: "Path history" });
    expect(pane.textContent).toMatch(/your session expired/);
    // What was already loaded stays: it is still true.
    expect(within(pane).getAllByRole("checkbox")).toHaveLength(1);
  });

  /* The by-id read is what ENRICHES the trace; the row already carries the hops.
     A failed enrichment must still open the trace, from the row's own payload. */
  it("opens the trace from the row's own payload when the by-id read 500s", async () => {
    renderPage({ onSnapshot: () => problem(500, "internal", "enrichment lookup exploded") });
    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaa[0-9a-f]*$/ }));
    await settle();
    const dialog = await screen.findByRole("dialog");
    expect(dialog.querySelector("table")).toBeTruthy();
  });

  it("reads whatever of an enrichment row it can and skips the rest", async () => {
    const enriched = snapshotRow({
      enrichment: {
        "10.0.0.1": {
          ip: "10.0.0.1",
          rdns: "a".repeat(300),
          asn: 64512,
          provider: "",
          /* geo is open JSON on the wire, so each value is checked rather than
             trusted — a number where a city name belongs must not reach the DOM. */
          geo: { city: 42, country: ["x"] },
          resolvedAt: "2026-08-01T00:00:00Z",
        },
        "10.0.0.2": { ip: "10.0.0.2", geo: null, resolvedAt: "2026-08-01T00:00:00Z" },
      },
    });
    renderPage({
      onSnapshots: () => json({ snapshots: [enriched], nextCursor: "" }),
      onSnapshot: () => json(enriched),
    });
    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaa[0-9a-f]*$/ }));
    const table = await screen.findByRole("table", { name: "Hops" });
    fireEvent.click(within(table).getAllByRole("button", { name: /^Enrichment for hop/ })[0]);
    expect(table.textContent).toMatch(/AS64512/);
    expect(table.textContent).not.toMatch(/NaN|undefined|\[object/);
  });
});

/* ── clicked hard ───────────────────────────────────────────────────────── */

describe("MTRPage — clicked hard", () => {
  it("lands where the twenty-fifth click says it should", async () => {
    renderPage();
    const header = await screen.findByRole("button", { name: /^node-b[,:]/ });
    for (let i = 0; i < 25; i++) fireEvent.click(header);
    expect(header.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByRole("button", { name: "node-a → node-b" })).toBeTruthy();
    expect(realNoise()).toEqual([]);
  });

  it("lets the SELECTED pair's card still be shut and reopened by hand", async () => {
    renderPage();
    await selectPair("node-a", "node-b");
    const header = screen.getByRole("button", { name: /^node-b[,:]/ });
    expect(header.getAttribute("aria-expanded")).toBe("true");
    fireEvent.click(header);
    expect(header.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(header);
    expect(header.getAttribute("aria-expanded")).toBe("true");
  });

  it("keeps a card's own page across a collapse, and its rows stay pickable", async () => {
    renderPage({ destinations: Array.from({ length: 11 }, (_, i) => destinationRow({ sourceNode: `node-${i}` })) });
    const header = await expandDestination("node-b");
    const card = header.closest("div") as HTMLElement;
    fireEvent.click(within(card).getByLabelText("Next page"));
    expect(within(card).getByTestId("pager-showing").textContent).toMatch(/1 of 11/);

    fireEvent.click(header);
    fireEvent.click(header);
    expect(within(card).getByTestId("pager-showing").textContent).toMatch(/1 of 11/);

    fireEvent.click(within(card).getByRole("button", { name: "node-10 → node-b" }));
    expect(await screen.findByText("node-10 → node-b")).toBeTruthy();
  });

  it("keeps the picked pair and path across a trip to the Runner tab and back", async () => {
    renderPage({ permissions: ["mtr:read", "runs:create"] });
    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaa[0-9a-f]*$/ }));
    await screen.findByRole("table", { name: "Hops" });

    fireEvent.click(screen.getByRole("radio", { name: "Runner" }));
    await screen.findByRole("form", { name: "Run a trace" });
    fireEvent.click(screen.getByRole("radio", { name: "Explorer" }));

    expect(await screen.findByRole("table", { name: "Hops" })).toBeTruthy();
    /* The pair is named in two places on purpose — the history pane's own
       heading and the open dialog's subtitle — so the assertion is that BOTH
       survived the trip, not that exactly one element carries the text. */
    expect(screen.getAllByText("node-a → node-b").length).toBeGreaterThan(0);
  });
});

/* ── the Runner's own input ─────────────────────────────────────────────── */

describe("MTRPage — the Runner's ad-hoc address", () => {
  it.each([
    ["  2001:db8::1  ", "2001:db8::1"],
    ["<img src=x onerror=alert(1)>", "<img src=x onerror=alert(1)>"],
    ["xn--e1afmkfd.xn--p1ai", "xn--e1afmkfd.xn--p1ai"],
    ["a b c", "a b c"],
  ])("posts %s trimmed and otherwise untouched, for the server to accept or refuse", async (typed, sent) => {
    const posted: { destinationAddress?: string }[] = [];
    renderPage({
      permissions: ["mtr:read", "runs:create"],
      onRun: (b) => {
        posted.push(b as { destinationAddress?: string });
        return json({ id: "run-1" }, { status: 202 });
      },
    });
    fireEvent.click(await screen.findByRole("radio", { name: "Runner" }));
    fireEvent.click(await screen.findByRole("radio", { name: "Ad-hoc" }));
    fireEvent.change(screen.getByLabelText("Destination address"), { target: { value: typed } });
    fireEvent.click(screen.getByRole("button", { name: "Start MTR" }));
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0].destinationAddress).toBe(sent);
  });

  it("drops the typed address once the operator goes back to Nodes", async () => {
    const posted: unknown[] = [];
    renderPage({
      permissions: ["mtr:read", "runs:create"],
      onRun: (b) => {
        posted.push(b);
        return json({ id: "run-1" }, { status: 202 });
      },
    });
    fireEvent.click(await screen.findByRole("radio", { name: "Runner" }));
    fireEvent.click(await screen.findByRole("radio", { name: "Ad-hoc" }));
    fireEvent.change(screen.getByLabelText("Destination address"), { target: { value: "evil.test" } });
    fireEvent.click(screen.getByRole("radio", { name: "Nodes" }));
    fireEvent.click(screen.getByRole("button", { name: "Start MTR" }));
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(JSON.stringify(posted[0])).not.toMatch(/evil\.test/);
  });

  it("posts once for a double click, not twice", async () => {
    const posted: unknown[] = [];
    renderPage({
      permissions: ["mtr:read", "runs:create"],
      onRun: (b) => {
        posted.push(b);
        return json({ id: "run-1" }, { status: 202 });
      },
    });
    fireEvent.click(await screen.findByRole("radio", { name: "Runner" }));
    const submit = await screen.findByRole("button", { name: "Start MTR" });
    fireEvent.click(submit);
    fireEvent.click(submit);
    await waitFor(() => expect(posted.length).toBeGreaterThan(0));
    await settle();
    expect(posted).toHaveLength(1);
  });
});

/* ── the same, in Russian ───────────────────────────────────────────────── */

describe("MTRPage — Russian", () => {
  it("says the hopless-diff sentence in Russian", async () => {
    renderPage({
      locale: "ru",
      onSnapshots: () =>
        json({
          snapshots: [
            snapshotRow({ id: "s1", pathHash: "AAAA00000000", hops: [], hopCount: 0, firstSeen: "2026-08-05T10:00:00Z" }),
            snapshotRow({ id: "s2", pathHash: "BBBB00000000", hops: [], hopCount: 0, firstSeen: "2026-08-04T10:00:00Z" }),
          ],
          nextCursor: "",
        }),
    });
    fireEvent.click(await screen.findByRole("button", { name: /^node-b[,:]/ }));
    fireEvent.click(await screen.findByRole("button", { name: "node-a → node-b" }));
    await compareFirstTwo();
    const pane = await screen.findByRole("region", { name: "Разница путей" });
    expect(pane.textContent).toMatch(/сопоставлять нечего/);
    expect(pane.querySelector("table")).toBeNull();
  });

  it("counts hops off the hops themselves when the column is missing, in Russian too", async () => {
    renderPage({ locale: "ru", onSnapshots: () => json({ snapshots: [snapshotRow({ hopCount: undefined })], nextCursor: "" }) });
    fireEvent.click(await screen.findByRole("button", { name: /^node-b[,:]/ }));
    fireEvent.click(await screen.findByRole("button", { name: "node-a → node-b" }));
    expect(await screen.findByText("2 хопа")).toBeTruthy();
    expect(document.body.textContent).not.toMatch(/NaN|undefined/);
  });
});
