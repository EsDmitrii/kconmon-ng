import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { MTRPage, groupDestinations, pathChangeFlags, shortHash, toggleCompare } from "./mtr";

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

function topologyBody(names: string[]) {
  return { nodes: names.map((n) => ({ name: n, zone: "z1", ready: true })), agents: [], timestamp: "t" };
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
    targets?: unknown[];
    onDestinations?: () => Response;
    /** Answers GET /api/v1/mtr/snapshots from the parsed query string, so a
     *  test can make page two depend on the cursor it was handed. */
    onSnapshots?: (qs: URLSearchParams) => Response;
    onSnapshot?: (id: string, qs: URLSearchParams) => Response;
    onRun?: (body: unknown) => Response;
  } = {},
) {
  const {
    permissions = VIEWER,
    databaseConfigured = true,
    prometheusConfigured = true,
    destinations = [destinationRow()],
    nodes = ["node-a", "node-b", "node-c"],
    targets = [targetRow()],
    onDestinations,
    onSnapshots,
    onSnapshot,
    onRun,
  } = opts;
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
    if (href.startsWith("/api/v1/topology")) return Promise.resolve(json(topologyBody(nodes)));
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
    // Detail before list: the detail path is a longer path under the same prefix.
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

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <MTRPage />
      </ThemeProvider>
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
});

/** Clicks the pane-1 row for a pair; the accessible name carries BOTH halves
 *  because a source name alone is ambiguous across destination groups. */
async function selectPair(source: string, destination: string) {
  fireEvent.click(await screen.findByRole("button", { name: new RegExp(`${source}.*${destination}`) }));
}

describe("groupDestinations", () => {
  it("groups the flat API list by destination, sources within, in encounter order", () => {
    const groups = groupDestinations([
      destinationRow({ sourceNode: "node-a", destination: "node-b" }),
      destinationRow({ sourceNode: "node-c", destination: "node-b" }),
      destinationRow({ sourceNode: "node-a", destination: "api-gw" }),
    ]);

    expect(groups.map((g) => g.destination)).toEqual(["node-b", "api-gw"]);
    expect(groups[0].sources.map((s) => s.sourceNode)).toEqual(["node-a", "node-c"]);
    expect(groups[1].sources.map((s) => s.sourceNode)).toEqual(["node-a"]);
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

  it("is empty for an empty list rather than inventing a group", () => {
    expect(groupDestinations([])).toEqual([]);
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
    expect(await within(pane).findByRole("heading", { name: "node-b" })).toBeInTheDocument();
    // One list per destination, each labelled with the destination's name.
    expect(within(pane).getAllByRole("list")).toHaveLength(2);
    expect(within(within(pane).getByRole("list", { name: "node-b" })).getAllByRole("button")).toHaveLength(2);
    expect(within(within(pane).getByRole("list", { name: "api-gw" })).getAllByRole("button")).toHaveLength(1);
    // The group header states the GROUP's total, not its first member's.
    expect(within(pane).getByText("5 paths")).toBeInTheDocument();
  });

  it("makes no snapshots request until a pair is picked", async () => {
    const { snapshotListCalls } = renderPage();

    await screen.findByRole("button", { name: /node-a.*node-b/ });
    expect(snapshotListCalls()).toEqual([]);
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

  it("'Load older' appends the next page via cursor and disables itself once nextCursor is empty", async () => {
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
    await waitFor(() => expect(screen.getByRole("button", { name: /load older/i })).toBeDisabled());
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
    fireEvent.click(await screen.findByRole("button", { name: /aaaaaaaaaaaa/ }));

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

  it("prompts for a selection before anything is picked", async () => {
    renderPage();

    expect(await screen.findByText(/pick a path/i)).toBeInTheDocument();
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
    fireEvent.click(await screen.findByRole("button", { name: /aaaaaaaaaaaa/ }));

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
    fireEvent.click(await screen.findByRole("button", { name: /aaaaaaaaaaaa/ }));
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
    fireEvent.click(await screen.findByRole("button", { name: /Path aaaaaaaaaaaa/ }));
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

  it("shows the diff — older on the left, newer on the right — once two are picked", async () => {
    renderPage({ onSnapshots: twoPaths });

    await selectPair("node-a", "node-b");
    for (const box of await screen.findAllByRole("checkbox", { name: /^Compare path/ })) fireEvent.click(box);

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

    const diff = await screen.findByRole("region", { name: /path diff/i });
    expect(within(diff).getAllByLabelText("same")).toHaveLength(2);
    expect(within(diff).getAllByLabelText("added")).toHaveLength(1);
    // hop 1 went 1.0ms -> 3.0ms.
    expect(within(diff).getByText("+2.0ms")).toBeInTheDocument();
  });

  it("un-picking one drops back to the trace detail rather than half a diff", async () => {
    renderPage({ onSnapshots: twoPaths });

    await selectPair("node-a", "node-b");
    const boxes = await screen.findAllByRole("checkbox", { name: /^Compare path/ });
    fireEvent.click(boxes[0]);
    fireEvent.click(boxes[1]);
    await screen.findByRole("region", { name: /path diff/i });

    fireEvent.click(boxes[1]);

    await waitFor(() => expect(screen.queryByRole("region", { name: /path diff/i })).not.toBeInTheDocument());
    expect(screen.getByRole("region", { name: /trace detail/i })).toBeInTheDocument();
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
});
