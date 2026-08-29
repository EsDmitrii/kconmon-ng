import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { LOCALE_STORAGE_KEY, LocaleProvider, type Locale } from "@/lib/i18n";
import { TimeMachineProvider } from "@/lib/timemachine";
import {
  MTRPage,
  deepLinkDestination,
  deepLinkSource,
  destinationPage,
  groupDestinations,
  pathChangeFlags,
  shortHash,
  toggleCompare,
} from "./mtr";

// Same reason as target-card.test.tsx: echarts.init() needs a canvas context
// jsdom does not have. The trend's DATA is asserted in
// components/mtr-hop-table.test.tsx; here the chart only has to appear.
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

/** Decision 11: mtr:read is telemetry, so EVERY built-in role holds it —
 *  viewer included, which is the role an anonymous session gets. The "no
 *  permission" case below is therefore a hand-rolled role, not a default. */
const VIEWER = ["mtr:read"];

function meBody(permissions: string[]) {
  return {
    subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: ["viewer"] },
    permissions,
  };
}

function configBody(databaseConfigured: boolean, prometheusConfigured = true) {
  return {
    auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
    anonymousBanner: false,
    controller: { configured: true },
    prometheus: { configured: prometheusConfigured },
    database: { configured: databaseConfigured },
  };
}

function topologyBody(names: string[], agents: string[] = []) {
  return {
    nodes: names.map((n) => ({ name: n, zone: "z1", ready: true })),
    agents: agents.map((n, i) => ({ agentId: `a${i}`, nodeName: n, zone: "z1", ready: true })),
    timestamp: "t",
  };
}

function targetRow(over: Record<string, unknown> = {}) {
  return {
    id: "t-1",
    name: "api-gw",
    kind: "host",
    address: "10.0.0.1",
    labels: {},
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
    ...over,
  };
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

function renderPage(
  opts: {
    permissions?: string[];
    databaseConfigured?: boolean;
    prometheusConfigured?: boolean;
    destinations?: unknown[];
    nodes?: string[];
    /** Agents the controller does NOT list as nodes. */
    agents?: string[];
    targets?: unknown[];
    onDestinations?: () => Response;
    /** Answers GET /api/v1/mtr/snapshots from the parsed query string, so a
     *  test can make page two depend on the cursor it was handed. */
    onSnapshots?: (qs: URLSearchParams) => Response;
    onSnapshot?: (id: string, qs: URLSearchParams) => Response;
    /** GET /api/v1/mtr/snapshots/{id}/traces — the individual traces behind one route. */
    onTraces?: (id: string, qs: URLSearchParams) => Response;
    onRun?: (body: unknown) => Response;
    /** Mounts a <LocaleProvider> above the page. Absent — every case but the ru
     *  smoke pin at the bottom of this file — there is no provider at all,
     *  which lib/i18n defines as English. */
    locale?: Locale;
    /** RFC 3339 instant to engage the Time Machine at, through the URL — the
     *  same seam pages/diagnostics.test.tsx uses. */
    at?: string;
    /** The ?destination= a shared link carries, through the same URL. */
    destination?: string;
  } = {},
) {
  const {
    permissions = VIEWER,
    databaseConfigured = true,
    prometheusConfigured = true,
    destinations = [destinationRow()],
    nodes = ["node-a", "node-b", "node-c"],
    agents = [],
    targets = [targetRow()],
    onDestinations,
    onSnapshots,
    onSnapshot,
    onTraces,
    onRun,
    locale,
    at,
    destination: linked,
  } = opts;
  if (locale !== undefined) localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  const qs = new URLSearchParams();
  if (at) qs.set("at", at);
  if (linked) qs.set("destination", linked);
  window.history.pushState({}, "", qs.size > 0 ? `/mtr?${qs}` : "/mtr");
  const calls: Call[] = [];

  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    const method = (init?.method ?? "GET").toUpperCase();
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    calls.push({ method, url: href, body });

    if (href.includes("/api/v1/auth/me")) return Promise.resolve(json(meBody(permissions)));
    if (href.includes("/api/v1/config")) {
      return Promise.resolve(json(configBody(databaseConfigured, prometheusConfigured)));
    }
    if (href.startsWith("/api/v1/topology")) return Promise.resolve(json(topologyBody(nodes, agents)));
    if (href.startsWith("/api/v1/targets")) return Promise.resolve(json({ targets, nextCursor: "" }));
    if (href.startsWith("/api/v1/promql/")) {
      return Promise.resolve(
        json({
          status: "success",
          data: { resultType: "matrix", result: [{ metric: { protocol: "icmp" }, values: [[1_754_000_000, "0.02"]] }] },
        }),
      );
    }
    if (href.startsWith("/api/v1/runs")) {
      if (onRun) return Promise.resolve(onRun(body));
      return Promise.resolve(json({ id: "run-1" }, { status: 202 }));
    }
    // Traces before detail before list: each is a longer path under the same prefix.
    if (href.startsWith("/api/v1/mtr/snapshots/") && href.split("?")[0].endsWith("/traces")) {
      const [path, query = ""] = href.slice("/api/v1/mtr/snapshots/".length).split("?");
      const id = decodeURIComponent(path.slice(0, -"/traces".length));
      const qs = new URLSearchParams(query);
      if (onTraces) return Promise.resolve(onTraces(id, qs));
      return Promise.resolve(json({ traces: [], nextCursor: "", scanned: 0 }));
    }
    if (href.startsWith("/api/v1/mtr/snapshots/")) {
      const [path, query = ""] = href.slice("/api/v1/mtr/snapshots/".length).split("?");
      const qs = new URLSearchParams(query);
      if (onSnapshot) return Promise.resolve(onSnapshot(decodeURIComponent(path), qs));
      return Promise.resolve(json(snapshotRow({ id: decodeURIComponent(path) })));
    }
    if (href.startsWith("/api/v1/mtr/snapshots")) {
      const qs = new URLSearchParams(href.split("?")[1] ?? "");
      if (onSnapshots) return Promise.resolve(onSnapshots(qs));
      return Promise.resolve(json({ snapshots: [snapshotRow()], nextCursor: "" }));
    }
    if (href.startsWith("/api/v1/mtr/destinations")) {
      if (onDestinations) return Promise.resolve(onDestinations());
      return Promise.resolve(json({ destinations }));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);

  /* staleTime mirrors main.tsx's own 10s. It is load-bearing for the warm-cache case: with the
     default 0 every remount refetches, which would hide a component that renders from state instead
     of from the cache. */
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 10_000 } } });
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

  /** Every request the PAGE itself makes, i.e. excluding the /auth/me and
   *  /config chrome every route fetches regardless of what it renders. */
  const resourceCalls = () => calls.filter((c) => c.url.startsWith("/api/v1/mtr"));
  const snapshotListCalls = () =>
    calls.filter((c) => /^\/api\/v1\/mtr\/snapshots(\?|$)/.test(c.url));
  const promCalls = () => calls.filter((c) => c.url.startsWith("/api/v1/promql/"));
  const runCalls = () => calls.filter((c) => c.method === "POST" && c.url.startsWith("/api/v1/runs"));
  return { ...utils, fetchMock, calls, resourceCalls, snapshotListCalls, promCalls, runCalls, qc };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  /* vitest.setup.ts backs localStorage with one Map per test FILE — a locale
     left behind would flip every later case in this one. */
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

/** Opens a destination card. Cards are collapsed on arrival — a fleet's worth of
 *  source rows expanded inline was the wall this pane used to be — so every
 *  interaction with a row goes through its card first. */
async function expandDestination(destination: string) {
  const header = await screen.findByRole("button", { name: new RegExp(`^${destination}[,:]`) });
  if (header.getAttribute("aria-expanded") !== "true") fireEvent.click(header);
  return header;
}

/** Clicks the pane-1 row for a pair; the accessible name carries BOTH halves
 *  because a source name alone is ambiguous across destination groups. */
async function selectPair(source: string, destination: string) {
  await expandDestination(destination);
  fireEvent.click(await screen.findByRole("button", { name: new RegExp(`${source}.*→.*${destination}`) }));
}


/* Ticking two boxes is a SELECTION; the comparison opens on the reader's word.
   The old three-pane layout swapped pane 3 the moment the second box was ticked
   — a dialog thrown at you mid-choice — so every case that wants the diff now
   presses the button the way a reader does. */
async function openCompare() {
  // By the COUNT, not the word: the button is translated and this helper runs in both locales.
  fireEvent.click(await screen.findByRole("button", { name: /\(2\/2\)$/ }));
  // One dialog is open at a time, so the role alone addresses it in any language.
  return screen.findByRole("dialog");
}

describe("groupDestinations", () => {
  /* The endpoint answers newest-traced first, so the pane arrived in an order
     nobody could scan and reshuffled itself whenever a trace landed. */
  it("groups the flat API list by destination and sorts BOTH levels by name", () => {
    const groups = groupDestinations([
      destinationRow({ sourceNode: "node-c", destination: "node-b" }),
      destinationRow({ sourceNode: "node-a", destination: "node-b" }),
      destinationRow({ sourceNode: "node-a", destination: "api-gw" }),
    ]);

    expect(groups.map((g) => g.destination)).toEqual(["api-gw", "node-b"]);
    expect(groups[1].sources.map((s) => s.sourceNode)).toEqual(["node-a", "node-c"]);
  });

  /* An operator reads m9 before m10; a codepoint sort does not. */
  it("reads the numbers in a node name rather than its codepoints", () => {
    const groups = groupDestinations(
      ["m10", "m2", "m9"].flatMap((n) => [
        destinationRow({ sourceNode: `src-${n}`, destination: `dst-${n}` }),
        destinationRow({ sourceNode: `src-${n}`, destination: "one" }),
      ]),
    );

    expect(groups.map((g) => g.destination)).toEqual(["dst-m2", "dst-m9", "dst-m10", "one"]);
    expect(groups[3].sources.map((s) => s.sourceNode)).toEqual(["src-m2", "src-m9", "src-m10"]);
  });

  it("sums each group's snapshot counts and keeps the newest lastSeen of its members", () => {
    const groups = groupDestinations([
      destinationRow({ sourceNode: "node-a", snapshotCount: 2, lastSeen: "2026-08-01T00:00:00Z" }),
      destinationRow({ sourceNode: "node-c", snapshotCount: 3, lastSeen: "2026-08-07T00:00:00Z" }),
    ]);

    expect(groups).toHaveLength(1);
    expect(groups[0].snapshotCount).toBe(5);
    expect(groups[0].lastSeen).toBe("2026-08-07T00:00:00Z");
  });

  /* The collapsed card claims both figures, so the group has to carry both. */
  it("sums the group's TRACES too, which is the other half of a shut card's summary", () => {
    const groups = groupDestinations([
      destinationRow({ sourceNode: "node-a", traceCount: 9 }),
      destinationRow({ sourceNode: "node-c", traceCount: 4 }),
    ]);

    expect(groups[0].traceCount).toBe(13);
  });

  it("is empty for an empty list rather than inventing a group", () => {
    expect(groupDestinations([])).toEqual([]);
  });
});

describe("deepLinkDestination", () => {
  it("reads the destination a shared link names", () => {
    expect(deepLinkDestination("?destination=api-gw")).toBe("api-gw");
  });

  it("decodes a name that had to be escaped to travel in a URL", () => {
    expect(deepLinkDestination("?destination=10.0.0.1%2F32")).toBe("10.0.0.1/32");
  });

  it("is null for a plain link, and for an empty parameter that names nothing", () => {
    expect(deepLinkDestination("")).toBeNull();
    expect(deepLinkDestination("?at=2026-08-08T00:00:00Z")).toBeNull();
    expect(deepLinkDestination("?destination=")).toBeNull();
  });
});

describe("deepLinkSource", () => {
  it("reads the source half of a pair link", () => {
    expect(deepLinkSource("?source=node-a&destination=node-b")).toBe("node-a");
  });

  it("decodes an escaped name", () => {
    expect(deepLinkSource("?source=10.0.0.1%2F32&destination=x")).toBe("10.0.0.1/32");
  });

  it("is null for a destination-only link and an empty parameter", () => {
    expect(deepLinkSource("?destination=node-b")).toBeNull();
    expect(deepLinkSource("?source=")).toBeNull();
    expect(deepLinkSource("")).toBeNull();
  });
});

describe("destinationPage", () => {
  const groups = (n: number) =>
    groupDestinations(
      Array.from({ length: n }, (_, i) => destinationRow({ destination: `dst-${String(i).padStart(2, "0")}` })),
    );

  it("is page 1 for a destination in the first page-worth", () => {
    expect(destinationPage(groups(25), "dst-03", 10)).toBe(1);
  });

  it("finds the page a card further down the list actually sits on", () => {
    expect(destinationPage(groups(25), "dst-17", 10)).toBe(2);
    expect(destinationPage(groups(25), "dst-24", 10)).toBe(3);
  });

  it("is null when the list does not hold it, so nothing is turned to on a guess", () => {
    expect(destinationPage(groups(25), "nowhere", 10)).toBeNull();
    expect(destinationPage(groups(25), null, 10)).toBeNull();
    expect(destinationPage([], "dst-00", 10)).toBeNull();
  });
});

describe("pathChangeFlags", () => {
  it("flags a row whose hash differs from the NEXT-OLDER row's", () => {
    const flags = pathChangeFlags([
      snapshotRow({ id: "s3", pathHash: "ccc" }),
      snapshotRow({ id: "s2", pathHash: "bbb" }),
      snapshotRow({ id: "s1", pathHash: "aaa" }),
    ]);

    // The OLDEST row has nothing older to differ from: it is the route this
    // pair started on, not a change.
    expect(flags).toEqual([true, true, false]);
  });

  it("does not flag a row that repeats the next-older hash", () => {
    const flags = pathChangeFlags([
      snapshotRow({ id: "s3", pathHash: "bbb" }),
      snapshotRow({ id: "s2", pathHash: "bbb" }),
      snapshotRow({ id: "s1", pathHash: "aaa" }),
    ]);

    expect(flags).toEqual([false, true, false]);
  });

  it("flags nothing for a single row or an empty page", () => {
    expect(pathChangeFlags([snapshotRow()])).toEqual([false]);
    expect(pathChangeFlags([])).toEqual([]);
  });
});

describe("shortHash", () => {
  it("keeps a stable 12-character prefix so a row is identifiable without being a wall of hex", () => {
    expect(shortHash("aaaaaaaaaaaa0000")).toBe("aaaaaaaaaaaa");
    expect(shortHash("abc")).toBe("abc");
  });
});

describe("MTRPage — no mtr:read", () => {
  it("is a single permission-explained card and issues zero mtr requests", async () => {
    const { resourceCalls } = renderPage({ permissions: [] });

    expect(await screen.findByText(/Requires the mtr:read permission/)).toBeInTheDocument();
    // Not "fire the destinations request and render the 403".
    expect(resourceCalls()).toEqual([]);
    expect(screen.queryByRole("region", { name: /destinations/i })).not.toBeInTheDocument();
  });

  it("says the permission is held by every built-in role, so the reader knows this is a custom role", async () => {
    renderPage({ permissions: [] });

    expect(await screen.findByText(/built-in role/i)).toBeInTheDocument();
  });
});

describe("MTRPage — database.mode=disabled", () => {
  it("names console.database.mode and issues zero mtr requests", async () => {
    const { resourceCalls } = renderPage({ databaseConfigured: false });

    expect(await screen.findByText(/console\.database\.mode/)).toBeInTheDocument();
    expect(resourceCalls()).toEqual([]);
  });
});

describe("MTRPage — no history yet", () => {
  it("points at Diagnostics instead of pretending the feature is broken, and asks for no snapshots", async () => {
    const { snapshotListCalls } = renderPage({ destinations: [] });

    expect(await screen.findByRole("link", { name: /run an MTR from Diagnostics/i })).toHaveAttribute(
      "href",
      "/diagnostics",
    );
    // Nothing is selected, so there is no pair to ask about.
    expect(snapshotListCalls()).toEqual([]);
  });
});

describe("MTRPage — destinations pane", () => {
  it("renders one group per destination with its sources and counts", async () => {
    renderPage({
      destinations: [
        destinationRow({ sourceNode: "node-a", destination: "node-b", snapshotCount: 2 }),
        destinationRow({ sourceNode: "node-c", destination: "node-b", snapshotCount: 3 }),
        destinationRow({ sourceNode: "node-a", destination: "api-gw", snapshotCount: 1 }),
      ],
    });

    const pane = await screen.findByRole("region", { name: /destinations/i });
    expect(await within(pane).findByRole("heading", { name: /^node-b/ })).toBeInTheDocument();
    await expandDestination("node-b");
    await expandDestination("api-gw");
    // One list per destination, each labelled with the destination's name.
    expect(within(pane).getAllByRole("list")).toHaveLength(2);
    expect(within(within(pane).getByRole("list", { name: "node-b" })).getAllByRole("button")).toHaveLength(2);
    expect(within(within(pane).getByRole("list", { name: "api-gw" })).getAllByRole("button")).toHaveLength(1);
    // The group header states the GROUP's total, not its first member's.
    expect(within(pane).getByRole("button", { name: /^node-b/ })).toHaveTextContent("5 paths");
  });

  /* QA scope 4, finding #5: the count column was shrink-0, so a Russian row —
     "трассировок" against "traces" — squeezed the source name down to
     «от qa-nod…», which identifies nothing. */
  it("gives the source name the width and lets the count column be the one that truncates", async () => {
    renderPage({
      destinations: [destinationRow({ sourceNode: "qa-node-worker-07", destination: "node-b" })],
    });

    await expandDestination("node-b");
    const row = await screen.findByRole("button", { name: /qa-node-worker-07.*node-b/ });
    const [name, , count] = Array.from(row.children) as HTMLElement[];
    // The name grows into the leftover and cannot be shrunk away...
    expect(name.className).toMatch(/flex-1/);
    // ...and the count is capped and shrinkable, which is what makes it yield
    // first. `shrink-0` here is the bug.
    expect(count.className).not.toMatch(/shrink-0/);
    expect(count.className).toMatch(/max-w-\[45%\]/);
    // Whichever side does clip, the full text stays reachable.
    expect(name).toHaveAttribute("title", expect.stringContaining("qa-node-worker-07"));
    expect(count).toHaveAttribute("title", expect.stringContaining("traces"));
  });

  it("makes no snapshots request until a pair is picked", async () => {
    const { snapshotListCalls } = renderPage();

    await expandDestination("node-b");
    await screen.findByRole("button", { name: /node-a.*node-b/ });
    expect(snapshotListCalls()).toEqual([]);
  });
});

/* ── the owner's report: every card arrived expanded, which at fleet scale is a
      wall of rows nobody can read past ───────────────────────────────────── */

describe("MTRPage — destination cards collapse", () => {
  /** n sources, all tracing the same destination. */
  const fanIn = (n: number, destination = "node-b") =>
    Array.from({ length: n }, (_, i) =>
      destinationRow({
        sourceNode: `node-${String(i).padStart(2, "0")}`,
        destination,
        snapshotCount: 2,
        traceCount: 3,
      }),
    );

  it("opens collapsed: the header names the destination and counts it, and no source row is drawn", async () => {
    renderPage({ destinations: fanIn(12) });

    const header = await screen.findByRole("button", { name: /^node-b/ });
    expect(header).toHaveAttribute("aria-expanded", "false");
    // The summary is the card's whole claim while it is shut: 24 paths, 36 traces.
    expect(header).toHaveTextContent("24 paths");
    expect(header).toHaveTextContent("36 traces");
    expect(screen.queryByRole("list", { name: "node-b" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /node-00.*→.*node-b/ })).not.toBeInTheDocument();
  });

  it("expands on a click and shuts again on the next one", async () => {
    renderPage({ destinations: fanIn(3) });

    const header = await screen.findByRole("button", { name: /^node-b/ });
    fireEvent.click(header);
    expect(header).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByRole("list", { name: "node-b" })).toBeInTheDocument();
    // The control names what it opened, so a reader who cannot see it is told.
    expect(header.getAttribute("aria-controls")).toBe(screen.getByRole("list", { name: "node-b" }).id);

    fireEvent.click(header);
    expect(header).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("list", { name: "node-b" })).not.toBeInTheDocument();
  });

  it("holds one open state PER card — opening one leaves its neighbour shut", async () => {
    renderPage({ destinations: [...fanIn(2, "node-b"), ...fanIn(2, "api-gw")] });

    await expandDestination("node-b");
    expect(screen.getByRole("list", { name: "node-b" })).toBeInTheDocument();
    expect(screen.queryByRole("list", { name: "api-gw" })).not.toBeInTheDocument();

    await expandDestination("api-gw");
    // Opening the second does not shut the first: this is not an accordion.
    expect(screen.getByRole("list", { name: "node-b" })).toBeInTheDocument();
    expect(screen.getByRole("list", { name: "api-gw" })).toBeInTheDocument();
  });

  it("pages an expanded card's own sources past ten, counting them against the whole card", async () => {
    renderPage({ destinations: fanIn(23) });

    await expandDestination("node-b");
    const list = screen.getByRole("list", { name: "node-b" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(10);
    expect(screen.getByTestId("pager-showing")).toHaveTextContent("Showing 10 of 23 sources");

    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    expect(within(screen.getByRole("list", { name: "node-b" })).getAllByRole("listitem")).toHaveLength(10);
    expect(screen.getByRole("button", { name: /node-10.*→.*node-b/ })).toBeInTheDocument();
  });

  /* Live acceptance on rev13: the collapsed header read "kco…" and the rows
     "from kconmon…" while the pane beside them stood half empty. Two causes and
     both are fixed — the header's COUNTS were shrink-0, so they took what they
     wanted and left the name the remainder, and the pane itself was the
     narrowest of the three columns while the widest one had nothing in it. */
  it("lets the destination name take the width and makes the COUNTS yield first", async () => {
    renderPage({ destinations: fanIn(2) });

    const header = await screen.findByRole("button", { name: /^node-b[,:]/ });
    const [, name, , counts] = Array.from(header.children) as HTMLElement[];
    // The name grows into every spare pixel and can never be shrunk away...
    expect(name.className).toMatch(/flex-1/);
    expect(name.className).toMatch(/min-w-0/);
    // ...and the counts are the shrinkable half. `shrink-0` here is the bug.
    expect(counts.className).not.toMatch(/shrink-0/);
    expect(counts.className).toMatch(/truncate/);
    // Whichever side does clip, the whole string stays reachable.
    expect(name).toHaveAttribute("title", "node-b");
    expect(counts).toHaveAttribute("title", expect.stringContaining("traces"));
  });

  it("never abbreviates a twenty-character node name in the markup itself", async () => {
    // Truncation is the browser's, on overflow, and reversible by widening the
    // window. A name cut in JS would be gone for good.
    renderPage({
      destinations: [
        destinationRow({ sourceNode: "kconmon-prod-m10", destination: "kconmon-prod-worker-07" }),
      ],
    });

    const header = await expandDestination("kconmon-prod-worker-07");
    expect(header).toHaveTextContent("kconmon-prod-worker-07");
    const row = screen.getByRole("button", { name: /kconmon-prod-m10.*→/ });
    expect(row).toHaveTextContent("from kconmon-prod-m10");
    expect(row.children[0]).toHaveAttribute("title", "from kconmon-prod-m10");
  });

  /* TWO panes now, not three: the trace opens over the page instead of taking a
     column, and the width it freed goes to the route history — the pane that
     carries a whole route per row. The destinations pane holds names, which are
     shorter than routes, so here it is deliberately the narrower of the two. */
  it("gives the route history the wider half of the row", async () => {
    renderPage({ destinations: fanIn(2) });
    await screen.findByRole("button", { name: /^node-b[,:]/ });

    const panes = screen.getByTestId("mtr-panes");
    const widths = (panes.className.match(/minmax\(0,([\d.]+)fr\)/g) ?? []).map((m) =>
      Number(m.replace(/[^\d.]/g, "")),
    );
    expect(widths).toHaveLength(2);
    expect(widths[1]).toBeGreaterThan(widths[0]);
  });

  it("draws no inner pager for a card that fits on one page", async () => {
    renderPage({ destinations: fanIn(4) });

    await expandDestination("node-b");
    expect(screen.queryByTestId("pager-showing")).not.toBeInTheDocument();
  });
});

describe("MTRPage — a shared link opens the card it names", () => {
  it("expands the deep-linked destination and leaves the others shut", async () => {
    renderPage({
      destination: "api-gw",
      destinations: [
        destinationRow({ sourceNode: "node-a", destination: "node-b" }),
        destinationRow({ sourceNode: "node-a", destination: "api-gw" }),
      ],
    });

    expect(await screen.findByRole("list", { name: "api-gw" })).toBeInTheDocument();
    expect(screen.queryByRole("list", { name: "node-b" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^api-gw/ })).toHaveAttribute("aria-expanded", "true");
  });

  it("turns to the PAGE the deep-linked destination is on, not just to its card", async () => {
    renderPage({
      destination: "dst-17",
      destinations: Array.from({ length: 25 }, (_, i) =>
        destinationRow({ sourceNode: "node-a", destination: `dst-${String(i).padStart(2, "0")}` }),
      ),
    });

    // dst-17 is the eighteenth group, i.e. page 2 at ten cards a page. A link
    // that opened a card nobody could see would be no link at all.
    expect(await screen.findByRole("list", { name: "dst-17" })).toBeInTheDocument();
    expect(screen.getByTestId("pager-page")).toHaveTextContent("Page 2 of 3");
  });

  it("ignores a destination the fleet has never traced rather than emptying the pane", async () => {
    renderPage({
      destination: "nowhere",
      destinations: [destinationRow({ sourceNode: "node-a", destination: "node-b" })],
    });

    expect(await screen.findByRole("button", { name: /^node-b/ })).toHaveAttribute("aria-expanded", "false");
  });
});

/* ── the owner's second report on this pane ─────────────────────────────── */

describe("MTRPage — the source row never runs its name into a count", () => {
  /**
   * The row read "from kconmon-prod-m10" followed straight by "2 · 3 traces",
   * so the node name and the leading snapshot count arrived as one string:
   * «from kconmon-prod-m102 · 3 traces». Two things were wrong and both are
   * fixed — the count led with a BARE DIGIT that named no unit, and nothing
   * separated the two spans but a CSS gap, which is not a character.
   */
  it("gives the leading count its noun and puts a separator between the two halves", async () => {
    renderPage({
      destinations: [
        destinationRow({ sourceNode: "kconmon-prod-m10", destination: "node-b", snapshotCount: 2, traceCount: 3 }),
      ],
    });

    await expandDestination("node-b");
    const row = await screen.findByRole("button", { name: /kconmon-prod-m10.*→.*node-b/ });
    expect(row.textContent).toBe("from kconmon-prod-m10 · 2 paths · 3 traces");
    expect(row.textContent).not.toMatch(/m102/);
  });
});

describe("MTRPage — history pane", () => {
  it("fetches the selected pair's snapshots with BOTH filters", async () => {
    const { snapshotListCalls } = renderPage({
      destinations: [destinationRow({ sourceNode: "node-a", destination: "api-gw" })],
    });

    await selectPair("node-a", "api-gw");

    await waitFor(() => expect(snapshotListCalls()).toHaveLength(1));
    const qs = new URLSearchParams(snapshotListCalls()[0].url.split("?")[1]);
    expect(qs.get("source")).toBe("node-a");
    expect(qs.get("destination")).toBe("api-gw");
  });

  it("badges every row whose path differs from the next-older one, and leaves the oldest unbadged", async () => {
    renderPage({
      onSnapshots: () =>
        json({
          snapshots: [
            snapshotRow({ id: "s3", pathHash: "ccccccccccccffff", hopCount: 4 }),
            snapshotRow({ id: "s2", pathHash: "bbbbbbbbbbbbffff", hopCount: 3 }),
            snapshotRow({ id: "s1", pathHash: "aaaaaaaaaaaaffff", hopCount: 2 }),
          ],
          nextCursor: "",
        }),
    });

    await selectPair("node-a", "node-b");

    const rows = within(await screen.findByRole("list", { name: "Paths" })).getAllByRole("listitem");
    expect(rows).toHaveLength(3);
    expect(within(rows[0]).getByText(/path changed/i)).toBeInTheDocument();
    expect(within(rows[1]).getByText(/path changed/i)).toBeInTheDocument();
    expect(within(rows[2]).queryByText(/path changed/i)).not.toBeInTheDocument();
  });

  it("'Load older' appends the next page via cursor and gives way to an end-of-list line", async () => {
    renderPage({
      onSnapshots: (qs) =>
        json(
          qs.get("cursor") === "cursor-1"
            ? { snapshots: [snapshotRow({ id: "s1", pathHash: "aaaaaaaaaaaa1111" })], nextCursor: "" }
            : { snapshots: [snapshotRow({ id: "s2", pathHash: "bbbbbbbbbbbb2222" })], nextCursor: "cursor-1" },
        ),
    });

    await selectPair("node-a", "node-b");

    const older = await screen.findByRole("button", { name: /load older/i });
    await waitFor(() => expect(older).toBeEnabled());
    fireEvent.click(older);

    expect(await screen.findByText(/aaaaaaaaaaaa/)).toBeInTheDocument();
    // Appended, not replaced.
    expect(screen.getByText(/bbbbbbbbbbbb/)).toBeInTheDocument();
    /* And once there is nothing older, the button GOES rather than sitting there greyed out. A
       permanently disabled control reads as broken — the owner reported exactly that against a pair
       whose whole route history was already on screen. */
    await waitFor(() => expect(screen.queryByRole("button", { name: /load older/i })).toBeNull());
    expect(screen.getByText(/nothing older is retained/i)).toBeInTheDocument();
  });

  it("counts what it is showing, so 'six routes' cannot be misread as 'six of 232 traces'", async () => {
    /* The sidebar counts TRACES and this list shows distinct ROUTES; a pair with hundreds of traces
       honestly has a handful of routes, and the footer is where the two numbers are reconciled. */
    renderPage({
      onSnapshots: () =>
        json({
          snapshots: [
            snapshotRow({ id: "s2", pathHash: "bbbbbbbbbbbb2222", traceCount: 147 }),
            snapshotRow({ id: "s1", pathHash: "aaaaaaaaaaaa1111", traceCount: 85 }),
          ],
          nextCursor: "",
        }),
    });

    await selectPair("node-a", "node-b");

    const footer = await screen.findByText(/nothing older is retained/i);
    expect(footer).toHaveTextContent("2 paths");
    expect(footer).toHaveTextContent("232 traces");
  });

  it("re-asks from page one when the selection changes, rather than appending another pair's paths", async () => {
    const { snapshotListCalls } = renderPage({
      destinations: [
        destinationRow({ sourceNode: "node-a", destination: "node-b" }),
        destinationRow({ sourceNode: "node-c", destination: "node-b" }),
      ],
      onSnapshots: (qs) =>
        json({
          snapshots: [snapshotRow({ id: `s-${qs.get("source")}`, pathHash: `${qs.get("source")}-hash` })],
          nextCursor: "",
        }),
    });

    await selectPair("node-a", "node-b");
    await screen.findByText(/node-a-hash/);
    await selectPair("node-c", "node-b");

    expect(await screen.findByText(/node-c-hash/)).toBeInTheDocument();
    expect(screen.queryByText(/node-a-hash/)).not.toBeInTheDocument();
    await waitFor(() => expect(snapshotListCalls()).toHaveLength(2));
    expect(snapshotListCalls().every((c) => !c.url.includes("cursor="))).toBe(true);
  });

  it("surfaces the server's own problem detail rather than a generic failure", async () => {
    renderPage({ onSnapshots: () => problem(422, "Unprocessable Entity", "source and destination are both required") });

    await selectPair("node-a", "node-b");

    expect(await screen.findByRole("alert")).toHaveTextContent(/both required/i);
  });
});

describe("MTRPage — detail pane", () => {
  it("fetches the clicked snapshot by id and renders its hop table", async () => {
    const { calls } = renderPage({
      onSnapshot: (id) =>
        json(
          snapshotRow({
            id,
            hops: [
              hop({ number: 1, ip: "10.0.0.1", hostname: "gw.internal", rttNs: 2_500_000 }),
              hop({ number: 2, ip: "203.0.113.9", hostname: undefined, rttNs: 41_000_000 }),
            ],
          }),
        ),
    });

    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaaaaaaaaaa$/ }));

    const hops = await screen.findByRole("table", { name: /hops/i });
    await waitFor(() =>
      expect(
        calls.some((c) => c.url.startsWith("/api/v1/mtr/snapshots/11111111-1111-1111-1111-111111111111")),
      ).toBe(true),
    );
    expect(within(hops).getByText("10.0.0.1")).toBeInTheDocument();
    expect(within(hops).getByText("gw.internal")).toBeInTheDocument();
    expect(within(hops).getByText("203.0.113.9")).toBeInTheDocument();
    // 2_500_000ns rendered in the repo's ms convention.
    expect(within(hops).getByText("2.5ms")).toBeInTheDocument();
  });

  /* The detail is no longer a column that sits there empty asking to be filled:
     it opens when a route is picked and is absent until then. */
  it("shows no trace detail at all until a route is picked", async () => {
    renderPage();

    await screen.findByRole("heading", { name: /destinations/i });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.queryByRole("table", { name: /hops/i })).not.toBeInTheDocument();
  });

  it("asks for enrichment on the by-id read — the hop rows are what turns it on", async () => {
    const seen: URLSearchParams[] = [];
    renderPage({
      onSnapshot: (id, qs) => {
        seen.push(qs);
        return json(snapshotRow({ id }));
      },
    });

    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaaaaaaaaaa$/ }));

    await waitFor(() => expect(seen).toHaveLength(1));
    expect(seen[0].get("enrich")).toBe("true");
  });

  it("expands a hop into the enrichment the detail response carried", async () => {
    renderPage({
      onSnapshot: (id) =>
        json(
          snapshotRow({
            id,
            enrichment: {
              "10.0.0.2": {
                ip: "10.0.0.2",
                rdns: "core1.example.net",
                asn: 64500,
                provider: "Example Transit",
                geo: { country: "GB", city: "London" },
                resolvedAt: "2026-08-06T10:00:00Z",
              },
            },
          }),
        ),
    });

    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaaaaaaaaaa$/ }));
    fireEvent.click(await screen.findByRole("button", { name: /hop 2/i }));

    expect(await screen.findByText("core1.example.net")).toBeInTheDocument();
    expect(screen.getByText(/AS64500/)).toBeInTheDocument();
  });

  it("charts a hop's trend from the paths the HISTORY pane loaded, and admits when they are partial", async () => {
    renderPage({
      onSnapshots: () =>
        json({
          snapshots: [
            snapshotRow({ id: "s2", pathHash: "aaaaaaaaaaaa2222", firstSeen: "2026-08-05T10:00:00Z" }),
            snapshotRow({ id: "s1", pathHash: "bbbbbbbbbbbb1111", firstSeen: "2026-08-01T10:00:00Z" }),
          ],
          nextCursor: "cursor-1",
        }),
    });

    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaaaaaaaaaa\d*$/ }));
    fireEvent.click(await screen.findByRole("button", { name: /trend for 10\.0\.0\.1/i }));

    expect(screen.getByTestId("echart")).toBeInTheDocument();
    // Two of the pair's paths are loaded and the cursor says there are more.
    expect(screen.getByText(/2 paths loaded/i)).toBeInTheDocument();
  });
});

describe("toggleCompare", () => {
  it("adds a first and a second pick, in click order", () => {
    expect(toggleCompare([], "a")).toEqual(["a"]);
    expect(toggleCompare(["a"], "b")).toEqual(["a", "b"]);
  });

  it("un-picks a snapshot that is already selected", () => {
    expect(toggleCompare(["a", "b"], "a")).toEqual(["b"]);
    expect(toggleCompare(["a"], "a")).toEqual([]);
  });

  it("swaps the OLDEST pick on a third click, so the pair is always the two most recent picks", () => {
    expect(toggleCompare(["a", "b"], "c")).toEqual(["b", "c"]);
    expect(toggleCompare(["b", "c"], "d")).toEqual(["c", "d"]);
  });
});

/** Two snapshots of one pair that differ by one substituted hop and one added
 *  one — enough for every marker the diff table can draw. */
function twoPaths() {
  return json({
    snapshots: [
      snapshotRow({
        id: "s2",
        pathHash: "bbbbbbbbbbbb2222",
        firstSeen: "2026-08-06T10:00:00Z",
        hopCount: 3,
        hops: [
          hop({ number: 1, ip: "10.0.0.1", rttNs: 3_000_000 }),
          hop({ number: 2, ip: "10.0.0.7", rttNs: 6_000_000 }),
          hop({ number: 3, ip: "10.0.0.9", rttNs: 9_000_000 }),
        ],
      }),
      snapshotRow({
        id: "s1",
        pathHash: "aaaaaaaaaaaa1111",
        firstSeen: "2026-08-01T10:00:00Z",
        hopCount: 2,
        hops: [
          hop({ number: 1, ip: "10.0.0.1", rttNs: 1_000_000 }),
          hop({ number: 2, ip: "10.0.0.9", rttNs: 9_000_000 }),
        ],
      }),
    ],
    nextCursor: "",
  });
}

describe("MTRPage — comparing two paths", () => {
  it("offers a compare checkbox per row and says what a third pick will do", async () => {
    renderPage({ onSnapshots: twoPaths });

    await selectPair("node-a", "node-b");

    const boxes = await screen.findAllByRole("checkbox", { name: /^Compare path/ });
    expect(boxes).toHaveLength(2);
    // The swap rule is stated in the UI, not left to be discovered.
    expect(screen.getByText(/replaces the earlier/i)).toBeInTheDocument();
  });

  /* QA scope 4, finding #13: an instruction the reader cannot follow. */
  it("withholds the two-path hint when there is only one path to tick", async () => {
    renderPage({ onSnapshots: () => json({ snapshots: [snapshotRow()], nextCursor: "" }) });

    await selectPair("node-a", "node-b");

    expect(await screen.findByRole("checkbox", { name: /^Compare path/ })).toBeInTheDocument();
    expect(screen.queryByText(/replaces the earlier/i)).not.toBeInTheDocument();
  });

  it("shows the diff — older on the left, newer on the right — once two are picked", async () => {
    renderPage({ onSnapshots: twoPaths });

    await selectPair("node-a", "node-b");
    for (const box of await screen.findAllByRole("checkbox", { name: /^Compare path/ })) fireEvent.click(box);
    await openCompare();

    const diff = await screen.findByRole("region", { name: /path diff/i });
    const table = within(diff).getByRole("table", { name: /path diff/i });
    // The OLDER snapshot heads the left column whichever order they were
    // picked in: the diff reads forwards in time or it reads nothing.
    const headers = within(table).getAllByRole("columnheader");
    expect(headers[1]).toHaveTextContent("aaaaaaaaaaaa");
    expect(headers[2]).toHaveTextContent("bbbbbbbbbbbb");
  });

  it("marks the substituted hop and the added one, and signs the RTT delta of the shared hop", async () => {
    renderPage({ onSnapshots: twoPaths });

    await selectPair("node-a", "node-b");
    for (const box of await screen.findAllByRole("checkbox", { name: /^Compare path/ })) fireEvent.click(box);
    await openCompare();

    const diff = await screen.findByRole("region", { name: /path diff/i });
    expect(within(diff).getAllByLabelText("same")).toHaveLength(2);
    expect(within(diff).getAllByLabelText("added")).toHaveLength(1);
    // hop 1 went 1.0ms -> 3.0ms.
    expect(within(diff).getByText("+2.0ms")).toBeInTheDocument();
  });

  /* Un-ticking closes the comparison rather than leaving half a diff up, and the
     button that opens it goes back to saying it cannot yet. */
  it("closes the comparison when one of the two is un-picked", async () => {
    renderPage({ onSnapshots: twoPaths });

    await selectPair("node-a", "node-b");
    const boxes = await screen.findAllByRole("checkbox", { name: /^Compare path/ });
    fireEvent.click(boxes[0]);
    fireEvent.click(boxes[1]);
    await openCompare();

    fireEvent.click(boxes[1]);

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(screen.getByRole("button", { name: /^Compare \(1\/2\)$/ })).toBeDisabled();
  });

  it("drops the comparison when the pair changes — two pairs' routes are not comparable", async () => {
    renderPage({
      destinations: [
        destinationRow({ sourceNode: "node-a", destination: "node-b" }),
        destinationRow({ sourceNode: "node-c", destination: "node-b" }),
      ],
      onSnapshots: twoPaths,
    });

    await selectPair("node-a", "node-b");
    for (const box of await screen.findAllByRole("checkbox", { name: /^Compare path/ })) fireEvent.click(box);
    await openCompare();
    await screen.findByRole("region", { name: /path diff/i });

    await selectPair("node-c", "node-b");

    await waitFor(() => expect(screen.queryByRole("region", { name: /path diff/i })).not.toBeInTheDocument());
  });
});

describe("MTRPage — path changes timeline", () => {
  it("marks every loaded snapshot on the axis above the list", async () => {
    renderPage({ onSnapshots: twoPaths });

    await selectPair("node-a", "node-b");

    const axis = await screen.findByRole("list", { name: /path changes/i });
    expect(within(axis).getAllByRole("listitem")).toHaveLength(2);
  });

  it("overlays the PEER loss family when the destination is a node the controller knows", async () => {
    const { promCalls } = renderPage({ nodes: ["node-a", "node-b"], onSnapshots: twoPaths });

    await selectPair("node-a", "node-b");

    await waitFor(() => expect(promCalls()).toHaveLength(1));
    const body = promCalls()[0].body as { query: string };
    expect(body.query).toContain('kconmon_ng_icmp_packet_loss_ratio{source_node="node-a",destination_node="node-b"}');
  });

  it("overlays the EXTERNAL family for a destination that is a target name, not a node", async () => {
    const { promCalls } = renderPage({
      nodes: ["node-a"],
      destinations: [destinationRow({ sourceNode: "node-a", destination: "api-gw" })],
      onSnapshots: twoPaths,
    });

    await selectPair("node-a", "api-gw");

    await waitFor(() => expect(promCalls()).toHaveLength(1));
    const body = promCalls()[0].body as { query: string };
    expect(body.query).toContain('kconmon_ng_external_packet_loss_ratio{source_node="node-a",target="api-gw"}');
  });

  it("keeps the markers, names the setting and asks Prometheus for NOTHING when it is unconfigured", async () => {
    const { promCalls } = renderPage({ prometheusConfigured: false, onSnapshots: twoPaths });

    await selectPair("node-a", "node-b");

    const axis = await screen.findByRole("list", { name: /path changes/i });
    expect(within(axis).getAllByRole("listitem")).toHaveLength(2);
    expect(screen.getByText(/console\.prometheus\.address/)).toBeInTheDocument();
    await waitFor(() => expect(promCalls()).toEqual([]));
  });
});

describe("MTRPage — runner tab", () => {
  const RUNNER = ["mtr:read", "runs:create"];

  async function openRunner() {
    fireEvent.click(await screen.findByRole("radio", { name: /runner/i }));
  }

  it("is not offered at all without runs:create (no disabled tab, no empty form)", async () => {
    renderPage({ permissions: VIEWER });

    await screen.findByRole("region", { name: /destinations/i });
    expect(screen.queryByRole("radio", { name: /runner/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /start mtr/i })).not.toBeInTheDocument();
  });

  it("posts type=mtr to every node by default — the pre-M4 node body, with no destinationKind", async () => {
    const { runCalls } = renderPage({ permissions: RUNNER });

    await openRunner();
    fireEvent.click(await screen.findByRole("button", { name: /start mtr/i }));

    await waitFor(() => expect(runCalls()).toHaveLength(1));
    expect(runCalls()[0].body).toEqual({ type: "mtr", plane: "pod", sources: [], destinations: [] });
  });

  it("posts destinationKind=target with the picked target's id, and no node destinations", async () => {
    const { runCalls } = renderPage({ permissions: [...RUNNER, "targets:read"] });

    await openRunner();
    fireEvent.click(await screen.findByRole("radio", { name: /^target$/i }));
    // The picker's options arrive from GET /api/v1/targets, which is only
    // fetched once "Target" is the live kind — pick after they land.
    await screen.findByRole("option", { name: "api-gw" });
    fireEvent.change(screen.getByLabelText(/destination target/i), { target: { value: "t-1" } });
    fireEvent.click(screen.getByRole("button", { name: /start mtr/i }));

    await waitFor(() => expect(runCalls()).toHaveLength(1));
    expect(runCalls()[0].body).toEqual({
      type: "mtr",
      plane: "pod",
      sources: [],
      destinations: [],
      destinationKind: "target",
      destinationTargetId: "t-1",
    });
  });

  it("posts destinationKind=adhoc with the typed address, trimmed", async () => {
    const { runCalls } = renderPage({ permissions: RUNNER });

    await openRunner();
    fireEvent.click(await screen.findByRole("radio", { name: /ad-hoc/i }));
    fireEvent.change(await screen.findByLabelText(/destination address/i), { target: { value: "  10.9.9.9  " } });
    fireEvent.click(screen.getByRole("button", { name: /start mtr/i }));

    await waitFor(() => expect(runCalls()).toHaveLength(1));
    expect(runCalls()[0].body).toEqual({
      type: "mtr",
      plane: "pod",
      sources: [],
      destinations: [],
      destinationKind: "adhoc",
      destinationAddress: "10.9.9.9",
    });
  });

  it("sends only the operator's picked sources when 'all nodes' is turned off", async () => {
    const { runCalls } = renderPage({ permissions: RUNNER, nodes: ["node-a", "node-b"] });

    await openRunner();
    // Sources and Destinations are two fieldsets with the same controls, so
    // the pick is scoped to the one under test rather than to the first match.
    const sourcesBox = within(await screen.findByRole("group", { name: /sources/i }));
    fireEvent.click(sourcesBox.getByRole("checkbox", { name: /all nodes/i }));
    fireEvent.click(sourcesBox.getByRole("checkbox", { name: "node-b" }));
    fireEvent.click(screen.getByRole("button", { name: /start mtr/i }));

    await waitFor(() => expect(runCalls()).toHaveLength(1));
    expect((runCalls()[0].body as { sources: string[] }).sources).toEqual(["node-b"]);
  });

  it("links to the started run rather than navigating away from the explorer", async () => {
    renderPage({ permissions: RUNNER, onRun: () => json({ id: "run-42" }, { status: 202 }) });

    await openRunner();
    fireEvent.click(await screen.findByRole("button", { name: /start mtr/i }));

    expect(await screen.findByRole("link", { name: /watch it here/i })).toHaveAttribute(
      "href",
      "/diagnostics/runs/run-42",
    );
  });

  it("surfaces the server's own refusal instead of a generic failure", async () => {
    renderPage({
      permissions: RUNNER,
      onRun: () => problem(422, "Unprocessable Entity", "the selection expands to no pairs"),
    });

    await openRunner();
    fireEvent.click(await screen.findByRole("button", { name: /start mtr/i }));

    expect(await screen.findByRole("alert")).toHaveTextContent(/no pairs/i);
  });

  /* Start MTR was enabled with zero known sources: pressing it posted `sources: []`. */
  it("disables Start MTR with no sources at all, and says why", async () => {
    renderPage({ permissions: RUNNER, nodes: [] });

    await openRunner();
    // The reason appears once the topology has ANSWERED with an empty fleet —
    // which is the state this gate is about, as opposed to a request still in
    // flight.
    expect(await screen.findByText(/no sources to trace from/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /start mtr/i })).toBeDisabled();
  });

  it("disables it when the operator has unticked every source by hand", async () => {
    renderPage({ permissions: RUNNER, nodes: ["node-a", "node-b"] });

    await openRunner();
    await screen.findByText(/~2 pairs/);
    const sourcesBox = within(screen.getByRole("group", { name: /sources/i }));
    fireEvent.click(sourcesBox.getByRole("checkbox", { name: /all nodes/i }));
    expect(screen.getByRole("button", { name: /start mtr/i })).toBeDisabled();

    fireEvent.click(sourcesBox.getByRole("checkbox", { name: "node-a" }));
    expect(screen.getByRole("button", { name: /start mtr/i })).toBeEnabled();
  });

  it("previews the pair count the way the Diagnostics form does", async () => {
    renderPage({ permissions: RUNNER, nodes: ["node-a", "node-b"] });

    await openRunner();
    // Two nodes, node destinations: the self-excluded cross product.
    expect(await screen.findByText(/~2 pairs/)).toBeInTheDocument();
  });

  /* QA scope 4, finding #9 — the Runner carries the Diagnostics form's bug and
     therefore its fix: with no destination resolved there is no second factor
     in sources x destinations, so the estimate is zero and says which side is
     missing. */
  it("estimates ZERO, and names the missing side, while the ad-hoc address is empty", async () => {
    renderPage({ permissions: RUNNER, nodes: ["node-a", "node-b"] });

    await openRunner();
    fireEvent.click(await screen.findByRole("radio", { name: /ad-hoc/i }));
    await waitFor(() => expect(screen.getByText(/~0 pairs/)).toBeInTheDocument());
    expect(screen.getByText(/no address typed yet/i)).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText(/destination address/i), { target: { value: "10.0.0.9" } });
    await waitFor(() => expect(screen.getByText(/~2 pairs/)).toBeInTheDocument());
  });

  it("does not flicker dead while the topology request is still in flight", async () => {
    renderPage({ permissions: RUNNER, nodes: ["node-a", "node-b"] });

    await openRunner();
    // The very first paint, before GET /api/v1/topology has answered: "we do
    // not know yet" is not "there is nothing to run".
    expect(screen.getByRole("button", { name: /start mtr/i })).toBeEnabled();
  });

  it("lists the agents' own node names when the controller reports none", async () => {
    renderPage({ permissions: RUNNER, nodes: [], agents: ["node-a", "node-b"] });

    await openRunner();
    await screen.findByText(/~2 pairs/);
    const sourcesBox = within(screen.getByRole("group", { name: /sources/i }));
    fireEvent.click(sourcesBox.getByRole("checkbox", { name: /all nodes/i }));
    expect(sourcesBox.getByRole("checkbox", { name: "node-a" })).toBeInTheDocument();
    expect(sourcesBox.getByRole("checkbox", { name: "node-b" })).toBeInTheDocument();
  });

  /* QA round 4, finding #10: a refusal outlived the form state it was about. */
  it("clears a stale submit error when the destination mode changes", async () => {
    renderPage({
      permissions: [...RUNNER, "targets:read"],
      onRun: () => problem(422, "Unprocessable Entity", "the selection expands to no pairs"),
    });

    await openRunner();
    fireEvent.click(await screen.findByRole("button", { name: /start mtr/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/no pairs/i);

    fireEvent.click(screen.getByRole("radio", { name: /ad-hoc/i }));
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
  });

  it("drops the started-run link too — it names a run the form no longer describes", async () => {
    renderPage({ permissions: RUNNER, onRun: () => json({ id: "run-42" }, { status: 202 }) });

    await openRunner();
    fireEvent.click(await screen.findByRole("button", { name: /start mtr/i }));
    await screen.findByRole("link", { name: /watch it here/i });

    fireEvent.click(screen.getByRole("radio", { name: /ad-hoc/i }));
    await waitFor(() => expect(screen.queryByRole("link", { name: /watch it here/i })).not.toBeInTheDocument());
  });
});

/** QA round 4, findings #12 and #20 — the copy the explorer prints. */
describe("MTRPage — copy", () => {
  it("agrees the count with its noun rather than printing '1 hops'", async () => {
    renderPage({
      destinations: [destinationRow({ snapshotCount: 1, traceCount: 1 })],
      onSnapshots: () => json({ snapshots: [snapshotRow({ hopCount: 1, traceCount: 1 })], nextCursor: "" }),
    });

    await selectPair("node-a", "node-b");
    expect(await screen.findByText("1 hop")).toBeInTheDocument();
    // Both the destinations pane's group header and the snapshot row.
    expect(screen.getAllByText(/\b1 path\b/).length).toBeGreaterThan(0);
    expect(screen.queryByText(/1 hops/)).not.toBeInTheDocument();
    expect(screen.queryByText(/1 traces/)).not.toBeInTheDocument();
  });

  it("still pluralises a real plural", async () => {
    renderPage();

    await selectPair("node-a", "node-b");
    expect(await screen.findByText("2 hops")).toBeInTheDocument();
  });

  /* Two rules in one string. No "on the left" (under ~700px the panes stack); and
     no bare "pick a source" either — the pane it points at is titled Destinations,
     and the source rows only exist inside an expanded destination card (M3-2). */
  it("names the actual gesture — expand a destination, pick a source — and never says 'on the left'", async () => {
    renderPage();

    expect(
      await screen.findByText("Expand a destination and pick a source to see that pair's path history."),
    ).toBeInTheDocument();
    expect(screen.queryByText(/on the left/i)).not.toBeInTheDocument();
  });
});

/* QA scope 4, findings #4 and #15 — the Explorer is LIVE under the "viewing
   past" banner, because none of the reads behind it takes a time parameter.
   That has to be SAID, the way /diagnostics says it over its history list. */
describe("MTRPage — Time Machine disclosure", () => {
  const AT = "2026-08-08T02:14:00Z";

  it("names the endpoints and admits the routes are the ones recorded now", async () => {
    renderPage({ at: AT });

    const note = await screen.findByText(/take no time parameter/i);
    expect(note).toBeInTheDocument();
    expect(note).toHaveTextContent("GET /api/v1/mtr/destinations");
    expect(note).toHaveTextContent(/recorded NOW/);
  });

  it("amends the subtitle instead of leaving it promising a view of the viewed instant", async () => {
    renderPage({ at: AT });

    expect(await screen.findByText(/is NOT cut to .* — it is live/)).toBeInTheDocument();
  });

  it("says none of it while Live — there is nothing to disclose", async () => {
    renderPage();

    await expandDestination("node-b");
    expect(screen.queryByText(/take no time parameter/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/it is live/)).not.toBeInTheDocument();
  });
});

/* ── MTR runner duration (Task 2) ───────────────────────────────────────── */

describe("MTR runner duration", () => {
  const RUNNER = ["mtr:read", "runs:create"];

  async function openRunner() {
    fireEvent.click(await screen.findByRole("radio", { name: /runner/i }));
  }

  // MTR reuses the run mechanism wholesale -- same endpoint, same runner, same sample_seq.
  it("defaults to Instant and sends no durationNs", async () => {
    const { runCalls } = renderPage({ permissions: RUNNER });

    await openRunner();
    expect(await screen.findByRole("radio", { name: "Instant" })).toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: /start mtr/i }));

    await waitFor(() => expect(runCalls()).toHaveLength(1));
    expect(runCalls()[0].body).not.toHaveProperty("durationNs");
  });

  /* The bug this pane carried, stated as a test: the caption quoted the BASE cadence — duration/500,
     floored at 5s — for the one check type that cannot keep it. A 5m MTR over the whole mesh
     advertised "every 5s" while the run permalink said 3m and the fleet did neither. It reads the
     same planner mirror the Diagnostics form does now, so all three can only say one thing. */
  it("names the cadence MTR can actually keep, never the unstretched base one", async () => {
    renderPage({ permissions: RUNNER, nodes: ["a", "b"] });

    await openRunner();
    await screen.findByRole("radio", { name: "Instant" });
    fireEvent.click(screen.getByRole("radio", { name: "5m" }));

    const caption = await screen.findByText(/An MTR trace takes up to 90s per pair/i);
    expect(caption.textContent).toMatch(/re-traces every pair every 90s/i);
    expect(caption.textContent).not.toMatch(/every 5s/i);
  });

  it("offers the same cadence control the Diagnostics form does, for a duration run only", async () => {
    renderPage({ permissions: RUNNER, nodes: ["a", "b"] });

    await openRunner();
    await screen.findByRole("radio", { name: "Instant" });
    expect(screen.queryByTestId("runner-sample-interval")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("radio", { name: "5m" }));
    const control = await screen.findByRole("radiogroup", { name: "Trace interval" });
    for (const label of ["Auto", "1s", "5s", "15s", "30s", "1m", "5m"]) {
      expect(within(control).getByRole("radio", { name: label })).toBeInTheDocument();
    }
    expect(within(control).queryByRole("radio", { name: "15m" })).not.toBeInTheDocument();
    expect(within(control).getByRole("radio", { name: "Auto" })).toBeChecked();
  });

  it("posts the picked trace interval, and nothing at all on Auto", async () => {
    const first = renderPage({ permissions: RUNNER, nodes: ["a", "b"] });
    await openRunner();
    await screen.findByRole("radio", { name: "Instant" });
    fireEvent.click(screen.getByRole("radio", { name: "5m" }));
    fireEvent.click(screen.getByRole("button", { name: /start mtr/i }));
    await waitFor(() => expect(first.runCalls()).toHaveLength(1));
    expect(first.runCalls()[0].body).not.toHaveProperty("sampleIntervalNs");

    cleanup();
    const second = renderPage({ permissions: RUNNER, nodes: ["a", "b"] });
    await openRunner();
    await screen.findByRole("radio", { name: "Instant" });
    fireEvent.click(screen.getByRole("radio", { name: "5m" }));
    const control = await screen.findByRole("radiogroup", { name: "Trace interval" });
    fireEvent.click(within(control).getByRole("radio", { name: "30s" }));
    fireEvent.click(screen.getByRole("button", { name: /start mtr/i }));
    await waitFor(() => expect(second.runCalls()).toHaveLength(1));
    expect(second.runCalls()[0].body).toMatchObject({
      durationNs: 300_000_000_000,
      sampleIntervalNs: 30_000_000_000,
    });
  });

  it("leads with the adjustment when the picked interval is faster than one round of traces", async () => {
    renderPage({ permissions: RUNNER, nodes: ["a", "b"] });

    await openRunner();
    await screen.findByRole("radio", { name: "Instant" });
    fireEvent.click(screen.getByRole("radio", { name: "5m" }));
    const control = await screen.findByRole("radiogroup", { name: "Trace interval" });
    fireEvent.click(within(control).getByRole("radio", { name: "1s" }));

    // Accepted, not refused — and said out loud, requested apart from effective.
    const caption = await screen.findByText(/Every 1s is faster than one round of traces/i);
    expect(caption.textContent).toMatch(/cannot go faster than every 90s/i);
    expect(caption.textContent).toMatch(/re-traces every pair every 90s/i);
  });

  it("names the trace-interval control and its adjustment in Russian", async () => {
    renderPage({ permissions: RUNNER, nodes: ["a", "b"], locale: "ru" });

    fireEvent.click(await screen.findByRole("radio", { name: "Запуск" }));
    fireEvent.click(await screen.findByRole("radio", { name: "5m" }));

    const control = await screen.findByRole("radiogroup", { name: "Период трассировки" });
    expect(within(control).getByRole("radio", { name: "Авто" })).toBeChecked();
    fireEvent.click(within(control).getByRole("radio", { name: "1s" }));

    const caption = await screen.findByText(/Раз в секунду — быстрее, чем успевает пройти круг трассировок/);
    expect(caption.textContent).toMatch(/чаще чем раз в 90 секунд этот запуск не пойдёт/);
    // The abbreviation stays off prose entirely.
    expect(caption.textContent).not.toMatch(/раз в 90 с[^ек]/);
  });

  it("sends durationNs for a picked duration", async () => {
    const { runCalls } = renderPage({ permissions: RUNNER });

    await openRunner();
    await screen.findByRole("radio", { name: "Instant" });
    fireEvent.click(screen.getByRole("radio", { name: "5m" }));
    fireEvent.click(screen.getByRole("button", { name: /start mtr/i }));

    await waitFor(() => expect(runCalls()).toHaveLength(1));
    const body = runCalls()[0].body as { type?: string; durationNs?: number };
    expect(body.type).toBe("mtr");
    expect(body.durationNs).toBe(300_000_000_000);
  });

  /* The cadence span follows the interface language, and in PROSE it is a WORD: «раз в 90 секунд»,
     never «раз в 90 с» (a bare «с» reads as the preposition) and never "90s".

     The number itself is the other half of the fix. This caption used to quote the BASE cadence —
     duration/500 floored at 5s — for the one check type that cannot keep it, so a 1m MTR run
     advertised «раз в 5 с» while the fleet was walking one round of traces every 90s. */
  it("renders the cadence span in the interface language, at the cadence MTR actually keeps", async () => {
    renderPage({ permissions: RUNNER, locale: "ru" });

    fireEvent.click(await screen.findByRole("radio", { name: "Запуск" }));
    fireEvent.click(await screen.findByRole("radio", { name: "1m" }));
    const caption = await screen.findByText(/каждая пара трассируется заново раз в/i);
    expect(caption.textContent).toMatch(/раз в 90 секунд/);
    expect(caption.textContent).not.toMatch(/раз в 5s/);
    expect(caption.textContent).not.toMatch(/раз в 90 с[^ек]/);
  });
});

/* the Russian is wired ONE smoke pin. */
describe("MTRPage — Russian", () => {
  it("names its panes and its honest empty state in Russian", async () => {
    renderPage({ locale: "ru", destinations: [] });

    expect(await screen.findByRole("region", { name: "Назначения" })).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "История путей" })).toBeInTheDocument();

    // The empty state keeps its caveat AND its remedy: nothing has been traced,
    // and the place to trace something is Diagnostics.
    expect(await screen.findByText(/Пока ничего не трассировали\./)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Запустите MTR из Диагностики" })).toBeInTheDocument();
  });

  it("counts paths and traces with the right Russian plural form", async () => {
    renderPage({
      locale: "ru",
      destinations: [destinationRow({ snapshotCount: 2, traceCount: 5 })],
    });

    // 2 → «пути» (few), 5 → «трассировок» (many). A two-form language would
    // render «2 путей» here. The shut card claims both figures.
    const header = await expandDestination("node-b");
    expect(header).toHaveTextContent("2 пути");
    expect(header).toHaveTextContent("5 трассировок");
    // The row makes the same claim in the same shape.
    const row = screen.getByRole("button", { name: /node-a.*→.*node-b/ });
    expect(row).toHaveTextContent("2 пути · 5 трассировок");
  });

  it("names the expand control and the card's rows in Russian", async () => {
    renderPage({ locale: "ru", destinations: [destinationRow({ sourceNode: "node-a", destination: "node-b" })] });

    const header = await screen.findByRole("button", { name: /^node-b/ });
    expect(header).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(header);
    expect(screen.getByRole("button", { name: /node-a.*→.*node-b/ }).textContent).toMatch(/^от node-a · /);
  });
});

/* ── the Explorer had to be readable, not just correct ───────────────────── */

/**
 * The owner on this page: «ничего не понятно». A path was identified by a
 * twelve-character hash and a row that changed said only "path changed". An MTR
 * exists to show a ROUTE, so the route is what a row leads with, and a change is
 * described as a change of route.
 */
describe("MTRPage — a path reads as a route", () => {
  const hop = (n: number, ip: string) => ({ number: n, ip, hostname: "", rttNs: 2_000_000, lossRatio: 0 });

  it("leads with the hop chain and keeps the hash as secondary metadata", async () => {
    renderPage({
      onSnapshots: () =>
        json({
          snapshots: [
            snapshotRow({ hops: [hop(1, "10.244.9.17"), hop(2, "10.0.0.9")], hopCount: 2 }),
          ],
          nextCursor: "",
        }),
    });
    await selectPair("node-a", "node-b");

    const list = await screen.findByRole("list", { name: "Paths" });
    // The route, end to end, with the pair's own endpoints framing it.
    expect(within(list).getByTitle("10.244.9.17 → 10.0.0.9")).toBeInTheDocument();
    expect(list).toHaveTextContent("10.244.9.17");
    // The hash is still there — it is what you paste into a ticket — just not
    // the thing the row is about.
    expect(list).toHaveTextContent("aaaaaaaaaaaa");
  });

  it("marks a hop nothing answered for, rather than leaving a bare asterisk", async () => {
    renderPage({
      onSnapshots: () =>
        json({ snapshots: [snapshotRow({ hops: [hop(1, "*"), hop(2, "10.0.0.9")], hopCount: 2 })], nextCursor: "" }),
    });
    await selectPair("node-a", "node-b");

    const list = await screen.findByRole("list", { name: "Paths" });
    expect(within(list).getByTitle("no reply was seen from this hop")).toBeInTheDocument();
  });

  it("says WHICH hop moved instead of badging a bare 'path changed'", async () => {
    renderPage({
      onSnapshots: () =>
        json({
          snapshots: [
            // Newest first, which is the order the store returns.
            snapshotRow({ id: "s-new", pathHash: "bbbb", hops: [hop(1, "10.244.9.21"), hop(2, "10.0.0.9")] }),
            snapshotRow({ id: "s-old", pathHash: "aaaa", hops: [hop(1, "10.244.9.17"), hop(2, "10.0.0.9")] }),
          ],
          nextCursor: "",
        }),
    });
    await selectPair("node-a", "node-b");

    expect(await screen.findByText("hop 1: 10.244.9.17 → 10.244.9.21")).toBeInTheDocument();
    expect(screen.queryByText("path changed")).not.toBeInTheDocument();
  });

  it("counts rather than lists when several hops moved at once", async () => {
    renderPage({
      onSnapshots: () =>
        json({
          snapshots: [
            snapshotRow({ id: "s-new", pathHash: "bbbb", hops: [hop(1, "10.0.0.2"), hop(2, "10.0.0.3")] }),
            snapshotRow({ id: "s-old", pathHash: "aaaa", hops: [hop(1, "10.0.0.7"), hop(2, "10.0.0.8")] }),
          ],
          nextCursor: "",
        }),
    });
    await selectPair("node-a", "node-b");

    expect(await screen.findByText("2 hops changed")).toBeInTheDocument();
  });
});

describe("MTRPage — the diff says what its marks mean", () => {
  const hop = (n: number, ip: string) => ({ number: n, ip, hostname: "", rttNs: 2_000_000, lossRatio: 0 });

  it("draws a visible key, not just a title attribute", async () => {
    renderPage({
      onSnapshots: () =>
        json({
          snapshots: [
            snapshotRow({ id: "s-new", pathHash: "bbbb", hops: [hop(1, "10.0.0.2")] }),
            snapshotRow({ id: "s-old", pathHash: "aaaa", hops: [hop(1, "10.0.0.7")] }),
          ],
          nextCursor: "",
        }),
    });
    await selectPair("node-a", "node-b");

    const boxes = await screen.findAllByRole("checkbox");
    fireEvent.click(boxes[0]);
    fireEvent.click(boxes[1]);
    await openCompare();

    const legend = await screen.findByRole("list", { name: "What the marks mean" });
    expect(within(legend).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "~changed",
      "+added",
      "−removed",
    ]);
  });
});

/* ── the traces behind a route ───────────────────────────────────────────── */

/*
 * The owner at a route reading "147 traces": «а как их посмотреть???». The row folds every trace
 * that walked the path into one count and shows ONE hop table — the last reading. The traces were
 * in the database the whole time, each with its own clock and its own RTTs.
 */
describe("MTRPage — the traces behind a route", () => {
  const trace = (over: Record<string, unknown> = {}) => ({
    id: 1,
    recordedAt: "2026-08-11T14:20:00Z",
    success: true,
    durationNs: 2_000_000,
    hops: [hop({ number: 1, ip: "10.0.0.1", hostname: "gw.internal", rttNs: 1_200_000 })],
    ...over,
  });

  it("lists the individual traces under the route's own hop table", async () => {
    renderPage({
      onTraces: () =>
        json({
          traces: [
            trace({ id: 2, recordedAt: "2026-08-11T14:21:00Z", durationNs: 3_400_000 }),
            trace({ id: 1 }),
          ],
          nextCursor: "",
          scanned: 2,
        }),
    });

    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaaaaaaaaaa$/ }));

    const list = await screen.findByRole("list", { name: /traces of this route/i });
    expect(within(list).getAllByRole("listitem")).toHaveLength(2);
    // Each trace carries its OWN duration — that is the whole reason to list them.
    expect(within(list).getByText("3.4ms")).toBeInTheDocument();
    expect(within(list).getByText("2.0ms")).toBeInTheDocument();
  });

  it("expands one trace into ITS hop readings, not the route's", async () => {
    renderPage({
      onTraces: () =>
        json({
          traces: [trace({ hops: [hop({ number: 1, ip: "10.9.9.9", hostname: "seen-only-here", rttNs: 7_000_000 })] })],
          nextCursor: "",
          scanned: 1,
        }),
    });

    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaaaaaaaaaa$/ }));

    const list = await screen.findByRole("list", { name: /traces of this route/i });
    fireEvent.click(within(list).getAllByRole("button")[0]);

    const hops = await screen.findByRole("table", { name: /hops of this trace/i });
    expect(within(hops).getByText("10.9.9.9")).toBeInTheDocument();
    expect(within(hops).getByText("seen-only-here")).toBeInTheDocument();
    expect(within(hops).getByText("7.0ms")).toBeInTheDocument();
  });

  it("says a failed trace's error INSTEAD of a latency", async () => {
    renderPage({
      onTraces: () =>
        json({
          traces: [trace({ success: false, error: "no agent on node-a", durationNs: 90_000_000, hops: [] })],
          nextCursor: "",
          scanned: 1,
        }),
    });

    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaaaaaaaaaa$/ }));

    const list = await screen.findByRole("list", { name: /traces of this route/i });
    expect(within(list).getByText("no agent on node-a")).toBeInTheDocument();
    // The elapsed time of a probe that never came back is dispatch overhead, not a round trip.
    expect(within(list).queryByText("90ms")).not.toBeInTheDocument();
  });

  /* The list is DERIVED from the query cache, not accumulated in state from inside queryFn. That
     distinction is the whole bug: react-query does not re-run queryFn for a cache hit, so
     re-opening a route within the 10s staleTime left the accumulator empty — and an empty list here
     renders the "swept by retention" note over a route with hundreds of traces. */
  it("still lists the traces when the route is re-opened from a warm cache", async () => {
    let calls = 0;
    renderPage({
      onTraces: () => {
        calls += 1;
        return json({ traces: [trace(), trace({ id: 2 })], nextCursor: "", scanned: 2 });
      },
    });

    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaaaaaaaaaa$/ }));
    expect(within(await screen.findByRole("list", { name: /traces of this route/i })).getAllByRole("listitem")).toHaveLength(2);

    // Close and re-open the same route, inside the staleTime window.
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: /^Path aaaaaaaaaaaa$/ }));

    const list = await screen.findByRole("list", { name: /traces of this route/i });
    expect(within(list).getAllByRole("listitem")).toHaveLength(2);
    expect(screen.queryByText(/no stored traces remain/i)).not.toBeInTheDocument();
    // And it did NOT have to re-ask to say so.
    expect(calls).toBe(1);
  });

  it("explains an empty list rather than showing a blank panel", async () => {
    renderPage({ onTraces: () => json({ traces: [], nextCursor: "", scanned: 0 }) });

    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaaaaaaaaaa$/ }));

    // Traces age out with the RUN sweep, the route with the path-history one — a route CAN outlive
    // the traces behind it, and that is what this says.
    expect(await screen.findByText(/no stored traces remain for this route/i)).toBeInTheDocument();
  });

  it("counts what it is showing against what the route claims", async () => {
    renderPage({
      onTraces: () => json({ traces: [trace()], nextCursor: "", scanned: 1 }),
      onSnapshot: (id) => json(snapshotRow({ id, traceCount: 147 })),
    });

    await selectPair("node-a", "node-b");
    fireEvent.click(await screen.findByRole("button", { name: /^Path aaaaaaaaaaaa$/ }));

    expect(await screen.findByText("1 of 147")).toBeInTheDocument();
  });
});
