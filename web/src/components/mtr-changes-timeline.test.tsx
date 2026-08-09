import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ThemeProvider } from "@/components/theme-provider";
import { PathChangesTimeline, lossFamilyFor, pairLossQuery } from "./mtr-changes-timeline";
import type { PathSnapshot } from "@/lib/types";

vi.mock("@/components/echart", () => ({
  EChart: ({ className }: { className?: string }) => <div data-testid="echart" className={className} />,
}));

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
  } = {},
) {
  const {
    source = "node-a",
    destination = "node-b",
    snapshots = [snapshot()],
    nodes = ["node-a", "node-b"],
    prometheusConfigured = true,
    matrix = [{ metric: { protocol: "icmp" }, values: [[1_754_000_000, "0.02"]] }],
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
        <PathChangesTimeline source={source} destination={destination} snapshots={snapshots} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
  const promCalls = () => calls.filter((c) => c.url.includes("/api/v1/promql"));
  return { ...utils, calls, promCalls };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
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
    // The window opens at the OLDEST snapshot's first_seen and runs to now.
    expect(body.start).toBe(new Date("2026-08-01T10:00:00Z").toISOString());
    expect(new Date(body.end).getTime()).toBeGreaterThan(new Date(body.start).getTime());
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
    expect(screen.getByRole("list", { name: /path changes/i }).parentElement?.className).toContain("h-28");
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
    // Keyboard-reachable too: a hover-only secret is not a fact.
    expect(marker).toHaveAttribute("tabindex", "0");
  });

  it("renders nothing at all without snapshots — there is no window to draw", () => {
    renderTimeline({ snapshots: [] });

    expect(screen.queryByRole("list", { name: /path changes/i })).not.toBeInTheDocument();
  });
});
