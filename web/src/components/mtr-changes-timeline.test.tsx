import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { PathChangesTimeline, lossFamilyFor, pairLossQuery } from "./mtr-changes-timeline";
import type { PathSnapshot } from "@/lib/types";

/* The option is captured, not ignored: the marker track has to be inset by the
   SAME grid the chart is drawn with, and comparing the two is the only way to
   pin that they cannot drift apart. */
const captured = vi.hoisted(() => ({ options: [] as Record<string, unknown>[] }));
vi.mock("@/components/echart", () => ({
  EChart: ({ option, className }: { option: Record<string, unknown>; className?: string }) => {
    captured.options.push(option);
    return <div data-testid="echart" className={className} />;
  },
}));

interface Grid { left: number; right: number; top: number; bottom: number }
const lastGrid = (): Grid => (captured.options.at(-1) as { grid: Grid }).grid;

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function snapshot(over: Partial<PathSnapshot> = {}): PathSnapshot {
  return {
    id: "s1",
    sourceNode: "node-a",
    destination: "node-b",
    pathHash: "aaaaaaaaaaaa0000",
    hopCount: 1,
    hops: [{ number: 1, ip: "10.0.0.1", rttNs: 1_000_000, lossRatio: 0 }],
    firstSeen: "2026-08-01T10:00:00Z",
    lastSeen: "2026-08-02T10:00:00Z",
    traceCount: 3,
    ...over,
  };
}

interface Call {
  method: string;
  url: string;
  body: unknown;
}

function renderTimeline(
  opts: {
    source?: string;
    destination?: string;
    snapshots?: PathSnapshot[];
    nodes?: string[];
    prometheusConfigured?: boolean;
    matrix?: unknown[];
    selectedId?: string | null;
    onSelect?: (s: PathSnapshot) => void;
  } = {},
) {
  const {
    source = "node-a",
    destination = "node-b",
    snapshots = [snapshot()],
    nodes = ["node-a", "node-b"],
    prometheusConfigured = true,
    matrix = [{ metric: { protocol: "icmp" }, values: [[1_754_000_000, "0.02"]] }],
    selectedId,
    onSelect,
  } = opts;
  const calls: Call[] = [];

  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const href = String(url);
    calls.push({
      method: (init?.method ?? "GET").toUpperCase(),
      url: href,
      body: init?.body ? JSON.parse(String(init.body)) : undefined,
    });
    if (href.includes("/api/v1/config")) {
      return Promise.resolve(
        json({
          auth: { mode: "local", role: "", loginPath: "/api/v1/auth/login" },
          anonymousBanner: false,
          controller: { configured: true },
          prometheus: { configured: prometheusConfigured },
          database: { configured: true },
        }),
      );
    }
    if (href.includes("/api/v1/topology")) {
      return Promise.resolve(json({ nodes: nodes.map((n) => ({ name: n, zone: "z1", ready: true })), agents: [], timestamp: "t" }));
    }
    if (href.includes("/api/v1/promql/query_range")) {
      return Promise.resolve(json({ status: "success", data: { resultType: "matrix", result: matrix } }));
    }
    return Promise.resolve(json({}));
  });
  vi.stubGlobal("fetch", fetchMock);

  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <PathChangesTimeline
          source={source}
          destination={destination}
          snapshots={snapshots}
          selectedId={selectedId}
          onSelect={onSelect}
        />
      </ThemeProvider>
    </QueryClientProvider>,
  );
  const promCalls = () => calls.filter((c) => c.url.includes("/api/v1/promql"));
  return { ...utils, calls, promCalls };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  captured.options.length = 0;
});

describe("lossFamilyFor", () => {
  it("uses the PEER family when the destination is a node the controller reports", () => {
    expect(lossFamilyFor("node-b", ["node-a", "node-b"])).toBe("peer");
  });

  it("uses the EXTERNAL family for a destination that is not a known node — i.e. a target name", () => {
    expect(lossFamilyFor("api-gw", ["node-a", "node-b"])).toBe("external");
  });

  it("answers external for an empty node list rather than guessing", () => {
    expect(lossFamilyFor("node-b", [])).toBe("external");
  });
});

describe("pairLossQuery", () => {
  it("selects the peer loss gauges by BOTH node labels", () => {
    const q = pairLossQuery("node-a", "node-b", "peer");

    expect(q).toContain('kconmon_ng_icmp_packet_loss_ratio{source_node="node-a",destination_node="node-b"}');
    expect(q).toContain('kconmon_ng_udp_packet_loss_ratio{source_node="node-a",destination_node="node-b"}');
    // A ratio is not additive: the worst of the pair's series is the honest
    // summary, a sum of ratios is not a ratio at all.
    expect(q.startsWith("max by (protocol)")).toBe(true);
  });

  it("selects the external loss gauge by source node and TARGET NAME", () => {
    const q = pairLossQuery("node-a", "api-gw", "external");

    expect(q).toContain('kconmon_ng_external_packet_loss_ratio{source_node="node-a",target="api-gw"}');
    expect(q).not.toContain("destination_node");
  });

  it("escapes quotes and backslashes in either name", () => {
    const q = pairLossQuery('no"de', "ta\\rget", "external");

    expect(q).toContain('source_node="no\\"de"');
    expect(q).toContain('target="ta\\\\rget"');
  });
});

describe("PathChangesTimeline", () => {
  it("puts one marker on the axis per snapshot, labelled with its path and first-seen", async () => {
    renderTimeline({
      snapshots: [
        snapshot({ id: "s2", pathHash: "bbbbbbbbbbbb1111", firstSeen: "2026-08-05T10:00:00Z" }),
        snapshot({ id: "s1", pathHash: "aaaaaaaaaaaa0000", firstSeen: "2026-08-01T10:00:00Z" }),
      ],
    });

    const axis = await screen.findByRole("list", { name: /path changes/i });
    expect(within(axis).getAllByRole("listitem")).toHaveLength(2);
    expect(within(axis).getByLabelText(/aaaaaaaaaaaa/)).toBeInTheDocument();
    expect(within(axis).getByLabelText(/bbbbbbbbbbbb/)).toBeInTheDocument();
  });

  it("asks the PromQL proxy for the peer loss series over the window the snapshots span", async () => {
    const { promCalls } = renderTimeline({ destination: "node-b", nodes: ["node-a", "node-b"] });

    await waitFor(() => expect(promCalls()).toHaveLength(1));
    const call = promCalls()[0];
    expect(call.method).toBe("POST");
    expect(call.url).toContain("/api/v1/promql/query_range");
    const body = call.body as { query: string; start: string; end: string; step: number };
    expect(body.query).toContain("kconmon_ng_icmp_packet_loss_ratio");
    /* The REQUEST is clamped to the proxy's own maximum range: the fixture's
       route is older than that, and asking for the whole span earns a 422 that
       leaves the strip with no chart at all. The AXIS still spans the full
       history — that is the case below. */
    const span = new Date(body.end).getTime() - new Date(body.start).getTime();
    expect(span).toBeLessThanOrEqual(24 * 60 * 60 * 1000);
    expect(span).toBeGreaterThan(0);
    expect(body.step).toBeGreaterThan(0);
  });

  it("switches to the external family when the destination is not a known node", async () => {
    const { promCalls } = renderTimeline({ destination: "api-gw", nodes: ["node-a", "node-b"] });

    await waitFor(() => expect(promCalls()).toHaveLength(1));
    const body = promCalls()[0].body as { query: string };
    expect(body.query).toContain("kconmon_ng_external_packet_loss_ratio");
    expect(body.query).not.toContain("kconmon_ng_icmp_packet_loss_ratio");
  });

  it("draws the loss chart once the proxy answers", async () => {
    renderTimeline();

    expect(await screen.findByTestId("echart")).toBeInTheDocument();
  });

  it("with Prometheus unconfigured keeps the markers, says so once, and makes ZERO promql requests", async () => {
    const { promCalls } = renderTimeline({ prometheusConfigured: false });

    expect(await screen.findByText(/console\.prometheus\.address/)).toBeInTheDocument();
    expect(within(screen.getByRole("list", { name: /path changes/i })).getAllByRole("listitem")).toHaveLength(1);
    expect(screen.queryByTestId("echart")).not.toBeInTheDocument();
    // Not "fire it and render the 503".
    await waitFor(() => expect(promCalls()).toEqual([]));
  });

  it("admits an empty loss series instead of drawing a flat line at zero", async () => {
    renderTimeline({ matrix: [] });

    expect(await screen.findByText(/no loss series/i)).toBeInTheDocument();
    expect(screen.queryByTestId("echart")).not.toBeInTheDocument();
  });

  /* QA round 4, finding #8. The 112px box was unconditional, so an empty
     series drew an empty grey rectangle above the note explaining that there
     was nothing to draw — a chart frame containing nothing reads as a chart
     that failed to load. */
  it("drops the 112px chart frame entirely when the loss series is empty", async () => {
    renderTimeline({ matrix: [] });

    await screen.findByText(/no loss series/i);
    const track = screen.getByRole("list", { name: /path changes/i }).parentElement;
    expect(track?.className).not.toContain("h-28");
    expect(track?.className).toContain("h-6");
  });

  it("drops it with Prometheus unconfigured too — same empty box, same reason", async () => {
    renderTimeline({ prometheusConfigured: false });

    await screen.findByText(/console\.prometheus\.address/);
    expect(screen.getByRole("list", { name: /path changes/i }).parentElement?.className).not.toContain("h-28");
  });

  it("keeps the frame once there IS a series to draw in it", async () => {
    renderTimeline();

    await screen.findByTestId("echart");
    // h-36, not h-28: the strip was too short for the loss chart's own axis labels.
    expect(screen.getByRole("list", { name: /path changes/i }).parentElement?.className).toContain("h-36");
  });

  /* The other half of #8: the markers carry the only copy of the full path
     hash and the first-seen stamp, in a title — on an element the track's own
     pointer-events-none made unhoverable. */
  it("makes each marker a real hit target, so its title is reachable", async () => {
    renderTimeline();

    const axis = await screen.findByRole("list", { name: /path changes/i });
    const marker = within(axis).getByLabelText(/aaaaaaaaaaaa/);
    expect(marker.className).toContain("pointer-events-auto");
    expect(marker).toHaveAttribute("title", expect.stringContaining("first seen"));
    /* Keyboard-reachable too: a hover-only secret is not a fact. A real
       <button> now, so it is focusable natively rather than by tabindex — and
       the reader who tried to click one gets something (owner report). */
    expect(marker.tagName).toBe("BUTTON");
  });

  it("renders nothing at all without snapshots — there is no window to draw", () => {
    renderTimeline({ snapshots: [] });

    expect(screen.queryByRole("list", { name: /path changes/i })).not.toBeInTheDocument();
  });
});

/**
 * The owner, on the yellow hairlines: «очень странно расположены, как я понимаю,
 * они уходят за график, просто видимого поля».
 *
 * They were positioned against the BOX — `left: 0%` is the box's left edge —
 * while the chart draws its plot inside a grid that reserves room for the y-axis
 * labels. The instant a marker claims to mark was therefore ~44px to the right
 * of where the marker stood, and the two ends of the window fell outside the
 * plot altogether. The track has to live on the chart's own plot rectangle.
 */
describe("PathChangesTimeline — the markers stand on the plot, not on the box", () => {
  /** The left edge of one marker's <li>, as the percentage it is positioned by. */
  const markerPct = (hash: string): number => {
    const li = screen.getByLabelText(new RegExp(hash)).closest("li") as HTMLElement;
    return Number.parseFloat(li.style.left);
  };

  it("insets the track by EXACTLY the grid the chart is drawn with", async () => {
    renderTimeline();

    await screen.findByTestId("echart");
    const grid = lastGrid();
    // The reserved axis gutter is the whole problem; a zero here would make the
    // rest of this case vacuously true.
    expect(grid.left).toBeGreaterThan(0);
    expect(screen.getByRole("list", { name: /path changes/i })).toHaveStyle({
      left: `${grid.left}px`,
      right: `${grid.right}px`,
      top: `${grid.top}px`,
      bottom: `${grid.bottom}px`,
    });
  });

  it("puts the window's first instant on the plot's left edge and the last on its right", async () => {
    const start = new Date(Date.now() - 3_600_000).toISOString();
    renderTimeline({
      snapshots: [
        snapshot({ id: "s2", pathHash: "bbbbbbbbbbbb1111", firstSeen: new Date().toISOString() }),
        snapshot({ id: "s1", pathHash: "aaaaaaaaaaaa0000", firstSeen: start }),
      ],
    });

    await screen.findByTestId("echart");
    // 0% and 100% OF THE INSET TRACK — i.e. the plot's own two edges, which is
    // where x=window.start and x=now are drawn. The percentages alone were
    // always right; what was wrong is the box they were percentages OF.
    const track = screen.getByRole("list", { name: /path changes/i });
    expect(track).toHaveStyle({ left: `${lastGrid().left}px`, right: `${lastGrid().right}px` });
    expect(markerPct("aaaaaaaaaaaa")).toBe(0);
    expect(markerPct("bbbbbbbbbbbb")).toBeCloseTo(100, 1);
  });

  it("centres a snapshot that arrived halfway through the window", async () => {
    renderTimeline({
      snapshots: [
        snapshot({ id: "s3", pathHash: "cccccccccccc2222", firstSeen: new Date().toISOString() }),
        snapshot({ id: "s2", pathHash: "bbbbbbbbbbbb1111", firstSeen: new Date(Date.now() - 1_800_000).toISOString() }),
        snapshot({ id: "s1", pathHash: "aaaaaaaaaaaa0000", firstSeen: new Date(Date.now() - 3_600_000).toISOString() }),
      ],
    });

    await screen.findByTestId("echart");
    expect(markerPct("bbbbbbbbbbbb")).toBeCloseTo(50, 1);
  });

  it("falls back to the whole box when there is no chart to stand on", async () => {
    // Prometheus off: no plot, no grid, and the markers are the only thing this
    // strip still has to say — so they take the full width, as they always did.
    renderTimeline({ prometheusConfigured: false });

    await screen.findByText(/console\.prometheus\.address/);
    expect(screen.getByRole("list", { name: /path changes/i })).toHaveStyle({
      left: "0px",
      right: "0px",
      top: "0px",
      bottom: "0px",
    });
  });
});

/*
Two defects the strip shipped with, both reported by the owner as "the hairline sits outside the
plot and clicking it does nothing".

The window opened at the LAST row's first_seen, but the store orders by last_seen — a route the
pair keeps reverting to has the oldest first_seen AND the newest last_seen, so it sorts first and
the window opened days too late. Every marker then clamped onto the left edge.

And the marker was a decorative <span>: a help cursor, a tab stop, and nothing behind either.
*/
describe("PathChangesTimeline — the window and the markers", () => {
  /* Ordered the way GET /api/v1/mtr/snapshots orders them: last_seen DESC. */
  const byLastSeen = [
    snapshot({ id: "steady", pathHash: "aaaaaaaaaaaa0000", firstSeen: "2026-08-01T00:00:00Z", lastSeen: "2026-08-09T00:00:00Z" }),
    snapshot({ id: "detour", pathHash: "bbbbbbbbbbbb1111", firstSeen: "2026-08-05T00:00:00Z", lastSeen: "2026-08-05T01:00:00Z" }),
  ];

  it("opens the AXIS at the EARLIEST first_seen, not at the last row's", async () => {
    renderTimeline({ snapshots: byLastSeen });

    await waitFor(() => expect(captured.options.length).toBeGreaterThan(0));
    const xAxis = (captured.options.at(-1) as { xAxis?: { min?: number } }).xAxis;
    // 08-01 is the steady route's, and it is the FIRST row.
    expect(xAxis?.min).toBe(new Date("2026-08-01T00:00:00Z").getTime());
  });

  /* The axis and the request answer different questions: the axis is the history,
     the request is what the proxy will actually serve. */
  it("says so when the request had to be clamped, instead of drawing a shorter window silently", async () => {
    renderTimeline({ snapshots: byLastSeen });
    expect(await screen.findByText(/last 24h/i)).toBeInTheDocument();
  });

  it("spreads the markers across the track instead of stacking them at the left edge", async () => {
    renderTimeline({ snapshots: byLastSeen });

    const axis = await screen.findByRole("list", { name: /path changes/i });
    const lefts = within(axis)
      .getAllByRole("listitem")
      .map((li) => Number.parseFloat((li as HTMLElement).style.left));

    expect(lefts).toHaveLength(2);
    expect(lefts[0]).toBeCloseTo(0, 1); // the window opens on the earliest
    expect(lefts[1]).toBeGreaterThan(5); // the detour sits well inside it
    expect(new Set(lefts).size).toBe(2);
  });

  it("draws the chart on the SAME window the markers are placed in", async () => {
    renderTimeline({ snapshots: byLastSeen });

    await waitFor(() => expect(captured.options.length).toBeGreaterThan(0));
    const xAxis = (captured.options.at(-1) as { xAxis?: { min?: number; max?: number } }).xAxis;
    expect(xAxis?.min).toBe(new Date("2026-08-01T00:00:00Z").getTime());
    expect(xAxis?.max).toBeGreaterThan(xAxis!.min!);
  });

  /* Loss is a ratio ≤ 1, but ECharts's nice rounding pushed the auto axis to 1.2
     on a fully-lossy pair and the labels read "120%" (M3-3, live pass). The clamp
     is a callback over the DATA extent so a 2% wiggle keeps its auto scale. */
  it("never lets the loss axis pass 100%, while small losses keep their auto scale", async () => {
    renderTimeline({ snapshots: byLastSeen });

    await waitFor(() => expect(captured.options.length).toBeGreaterThan(0));
    const yAxis = (captured.options.at(-1) as { yAxis?: { max?: unknown } }).yAxis;
    expect(typeof yAxis?.max).toBe("function");
    const clamp = yAxis!.max as (extent: { min: number; max: number }) => number | null;
    expect(clamp({ min: 0, max: 1 })).toBe(1); // total loss: the axis tops out at 100%, not 120%
    expect(clamp({ min: 0, max: 0.7 })).toBe(1); // high enough that nice rounding could overshoot
    expect(clamp({ min: 0, max: 0.02 })).toBeNull(); // low loss stays auto-scaled and readable
  });

  it("selects that route when a marker is clicked", async () => {
    const onSelect = vi.fn();
    renderTimeline({ snapshots: byLastSeen, onSelect });

    const axis = await screen.findByRole("list", { name: /path changes/i });
    const marker = within(axis).getByRole("button", { name: /bbbbbbbbbbbb/ });
    marker.click();

    expect(onSelect).toHaveBeenCalledTimes(1);
    expect(onSelect.mock.calls[0][0].id).toBe("detour");
  });

  it("marks the selected route's own hairline, so the strip agrees with the panes below", async () => {
    renderTimeline({ snapshots: byLastSeen, selectedId: "detour", onSelect: () => {} });

    const axis = await screen.findByRole("list", { name: /path changes/i });
    expect(within(axis).getByRole("button", { name: /bbbbbbbbbbbb/ })).toHaveAttribute("aria-pressed", "true");
    expect(within(axis).getByRole("button", { name: /aaaaaaaaaaaa/ })).toHaveAttribute("aria-pressed", "false");
  });
});

/*
The marker is a 1px hairline with a padded hit area. A flat padding is wider than the gap between
two routes that changed minutes apart, so the top button covered its neighbour's hairline and a
click selected the wrong route.
*/
describe("PathChangesTimeline — marker hit areas", () => {
  const padOf = (el: HTMLElement) => Number.parseFloat((el as HTMLElement).style.paddingLeft);

  it("never lets a marker's hit area reach past the midpoint to its neighbour", async () => {
    renderTimeline({
      snapshots: [
        snapshot({ id: "a", pathHash: "aaaaaaaaaaaa", firstSeen: "2026-08-01T00:00:00Z" }),
        // Half an hour later on a window that spans days: the two are almost on
        // the same pixel.
        snapshot({ id: "b", pathHash: "bbbbbbbbbbbb", firstSeen: "2026-08-01T00:30:00Z" }),
        snapshot({ id: "c", pathHash: "cccccccccccc", firstSeen: "2026-08-09T00:00:00Z" }),
      ],
    });

    const axis = await screen.findByRole("list", { name: /path changes/i });
    const items = within(axis).getAllByRole("listitem");
    const lefts = items.map((li) => Number.parseFloat((li as HTMLElement).style.left));
    const pads = items.map((li) => padOf(li.firstElementChild as HTMLElement));

    // Ascending along the axis, which is also the tab order.
    expect(lefts).toEqual([...lefts].sort((x, y) => x - y));
    // Neither of the close pair reaches the other.
    const gap = lefts[1] - lefts[0];
    expect(pads[0]).toBeLessThanOrEqual(gap / 2 + 0.001);
    expect(pads[1]).toBeLessThanOrEqual(gap / 2 + 0.001);
  });

  it("gives a lone marker the full padding, so it stays clickable", async () => {
    renderTimeline({ snapshots: [snapshot({ id: "only" })] });
    const axis = await screen.findByRole("list", { name: /path changes/i });
    const only = within(axis).getAllByRole("listitem")[0];
    expect(padOf(only.firstElementChild as HTMLElement)).toBeGreaterThan(0);
  });
});
