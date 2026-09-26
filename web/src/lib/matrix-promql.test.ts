import { afterEach, describe, expect, it, vi } from "vitest";
import { METRICS_PREFIX, foldMatrix, getMatrixAt, matrixQueries, vectorByPair } from "./matrix-promql";
import type { PromResult } from "./types";

/** These pin matrix-promql against the Go implementation it ports (internal/console/matrix/matrix.go). */

const AT = new Date("2026-08-01T12:00:00Z");

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

function vector(samples: [string, string, string][]): PromResult {
  return {
    status: "success",
    data: {
      resultType: "vector",
      result: samples.map(([source_node, destination_node, v]) => ({
        metric: { source_node, destination_node },
        value: [1785276000, v],
      })),
    },
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("matrixQueries", () => {
  it("builds TCP's pair exactly as matrix.go's failRatioQuery/p95Query do", () => {
    expect(matrixQueries("tcp")).toEqual({
      fail:
        'sum by (source_node, destination_node) (rate(kconmon_ng_tcp_results_total{result="fail"}[5m])) / ' +
        "sum by (source_node, destination_node) (rate(kconmon_ng_tcp_results_total[5m]))",
      rtt: "histogram_quantile(0.95, sum by (source_node, destination_node, le) (rate(kconmon_ng_tcp_total_duration_seconds_bucket[5m])))",
    });
  });

  it("gives TCP no loss query at all — there is no such series for it", () => {
    expect(matrixQueries("tcp").loss).toBeUndefined();
  });

  it("uses each datagram protocol's own rtt bucket and packet-loss gauge", () => {
    expect(matrixQueries("udp").rtt).toContain("kconmon_ng_udp_rtt_seconds_bucket");
    expect(matrixQueries("udp").loss).toBe(
      "avg by (source_node, destination_node) (kconmon_ng_udp_packet_loss_ratio)",
    );
    expect(matrixQueries("icmp").rtt).toContain("kconmon_ng_icmp_rtt_seconds_bucket");
    expect(matrixQueries("icmp").loss).toBe(
      "avg by (source_node, destination_node) (kconmon_ng_icmp_packet_loss_ratio)",
    );
  });

  it("keeps the prefix assumption in one named place", () => {
    expect(METRICS_PREFIX).toBe("kconmon_ng");
  });
});

describe("vectorByPair", () => {
  it("keys samples by the pair labels", () => {
    const m = vectorByPair(vector([["a", "b", "0.25"]]));
    expect(m.get("a\0b")).toBe(0.25);
  });

  it("skips a sample missing either pair label rather than inventing one", () => {
    const res: PromResult = {
      status: "success",
      data: { resultType: "vector", result: [{ metric: { source_node: "a" }, value: [1, "0.5"] }] },
    };
    expect(vectorByPair(res).size).toBe(0);
  });

  it("skips NaN — an empty histogram must read as no data, never as zero", () => {
    expect(vectorByPair(vector([["a", "b", "NaN"]])).size).toBe(0);
  });

  it("ignores a matrix (range) reply — this path only ever folds instant vectors", () => {
    const res: PromResult = { status: "success", data: { resultType: "matrix", result: [] } };
    expect(vectorByPair(res).size).toBe(0);
  });
});

describe("foldMatrix", () => {
  it("unions pairs across all three vectors and sorts nodes and cells", () => {
    const m = foldMatrix(
      "udp",
      vectorByPair(vector([["b", "a", "0.5"]])),
      vectorByPair(vector([["a", "b", "0.002"]])),
      vectorByPair(vector([["a", "b", "0.1"]])),
      AT,
    );
    expect(m.nodes).toEqual(["a", "b"]);
    expect(m.cells.map((c) => `${c.source}->${c.destination}`)).toEqual(["a->b", "b->a"]);
    expect(m.protocol).toBe("udp");
    expect(m.plane).toBe("pod");
  });

  it("converts the RTT quantile from seconds to nanoseconds", () => {
    const m = foldMatrix("tcp", new Map(), vectorByPair(vector([["a", "b", "0.002"]])), new Map(), AT);
    expect(m.cells[0].rttP95).toBe(2_000_000);
  });

  it("leaves failRatio null and the optional fields absent when a vector had nothing", () => {
    const m = foldMatrix("tcp", new Map(), vectorByPair(vector([["a", "b", "0.002"]])), new Map(), AT);
    expect(m.cells[0].failRatio).toBeNull();
    expect(m.cells[0].lossRatio).toBeUndefined();
  });

  it("stamps the instant it was EVALUATED at, not the moment it was computed", () => {
    const m = foldMatrix("tcp", vectorByPair(vector([["a", "b", "0"]])), new Map(), new Map(), AT);
    expect(m.timestamp).toBe(AT.toISOString());
  });

  it("keeps a genuine zero failure ratio distinct from no data", () => {
    const m = foldMatrix("tcp", vectorByPair(vector([["a", "b", "0"]])), new Map(), new Map(), AT);
    expect(m.cells[0].failRatio).toBe(0);
  });
});

describe("getMatrixAt", () => {
  it("evaluates every query at t through the promql proxy, in one round of parallel POSTs", async () => {
    const bodies: { query: string; time?: string }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string, init?: RequestInit) => {
        expect(String(url)).toBe("/api/v1/promql/query");
        bodies.push(JSON.parse(String(init?.body)) as { query: string; time?: string });
        return Promise.resolve(json(vector([["a", "b", "0.25"]])));
      }),
    );

    const m = await getMatrixAt("udp", AT);

    expect(bodies).toHaveLength(3);
    for (const b of bodies) expect(b.time).toBe(AT.toISOString());
    expect(bodies.map((b) => b.query)).toEqual([
      matrixQueries("udp").fail,
      matrixQueries("udp").rtt,
      matrixQueries("udp").loss,
    ]);
    expect(m.cells[0]).toMatchObject({ source: "a", destination: "b", failRatio: 0.25 });
  });

  it("issues only the two TCP queries — no request for a series that does not exist", async () => {
    const queries: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((_url: string, init?: RequestInit) => {
        queries.push((JSON.parse(String(init?.body)) as { query: string }).query);
        return Promise.resolve(json(vector([])));
      }),
    );
    await getMatrixAt("tcp", AT);
    expect(queries).toHaveLength(2);
  });

  it("throws Prometheus' own error rather than folding it into an empty grid", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.resolve(json({ status: "error", errorType: "bad_data", error: "parse error at char 7" }))),
    );
    await expect(getMatrixAt("tcp", AT)).rejects.toThrow("parse error at char 7");
  });
});

describe("pmtu in the Time Machine", () => {
  it("asks for failures, the path MTU, the per-pair probe size and the recent window, and no RTT", () => {
    const q = matrixQueries("pmtu");
    expect(q.fail).toContain("kconmon_ng_pmtu_results_total");
    expect(q.mtu).toContain("kconmon_ng_pmtu_bytes");
    expect(q.probe).toBe("max by (source_node, destination_node) (kconmon_ng_pmtu_probe_bytes)");
    expect(q.recent).toBe(
      'sum by (source_node, destination_node) (rate(kconmon_ng_pmtu_results_total{result="fail"}[3m])) / ' +
        "sum by (source_node, destination_node) (rate(kconmon_ng_pmtu_results_total[3m]))",
    );
    expect(q.rtt).toBeUndefined();
    expect(matrixQueries("tcp").recent).toBeUndefined();
  });

  it("folds the MTU and the pair's own probe size into the cells", () => {
    const fail = new Map([["a\0b", 1]]);
    const mtu = new Map([["a\0b", 1400]]);
    const probe = new Map([["a\0b", 1500]]);
    const m = foldMatrix("pmtu", fail, new Map(), new Map(), new Date(0), mtu, probe);
    expect(m.cells).toEqual([
      { source: "a", destination: "b", failRatio: 1, mtuBytes: 1400, probeMtuBytes: 1500 },
    ]);
  });

  /* The gauge keeps its last value while the pair's probes stop producing a verdict (0/0 is NaN,
     dropped), so an MTU without a result in the window is a stale size, not a measurement. */
  it("shows no MTU for a pair with no pmtu result in the window", () => {
    const fail = vectorByPair(vector([["a", "b", "NaN"], ["a", "c", "0"]]));
    const mtu = new Map([["a\0b", 1500], ["a\0c", 1500]]);
    const probe = new Map([["a\0b", 1500], ["a\0c", 1500]]);
    const m = foldMatrix("pmtu", fail, new Map(), new Map(), new Date(0), mtu, probe);
    expect(m.cells).toEqual([
      { source: "a", destination: "c", failRatio: 0, mtuBytes: 1500, probeMtuBytes: 1500 },
    ]);
    expect(m.nodes).toEqual(["a", "c"]);
  });

  /* One agent reaches a VPN peer over a 1420-byte route and a LAN peer over 1500: each pair is
     compared with its own probe size, so the VPN pair is full size and the LAN pair reduced. */
  it("compares each pair with its own probe size, not the source's largest", () => {
    const fail = new Map([["a\0vpn", 0], ["a\0lan", 0]]);
    const mtu = new Map([["a\0vpn", 1420], ["a\0lan", 1400]]);
    const probe = new Map([["a\0vpn", 1420], ["a\0lan", 1500]]);
    const m = foldMatrix("pmtu", fail, new Map(), new Map(), new Date(0), mtu, probe);
    expect(m.cells.map((c) => [c.destination, c.probeMtuBytes])).toEqual([["lan", 1500], ["vpn", 1420]]);
  });

  it("carries the recent-window fail ratio on pmtu cells without making cells of its own", () => {
    const fail = new Map([["a\0b", 0.4], ["a\0c", 0.4]]);
    const mtu = new Map([["a\0b", 1500], ["a\0c", 1500]]);
    const probe = new Map([["a\0b", 1500], ["a\0c", 1500]]);
    const recent = vectorByPair(vector([["a", "b", "0"], ["a", "c", "NaN"], ["x", "y", "0"]]));
    const m = foldMatrix("pmtu", fail, new Map(), new Map(), new Date(0), mtu, probe, recent);
    expect(m.cells).toEqual([
      { source: "a", destination: "b", failRatio: 0.4, mtuBytes: 1500, probeMtuBytes: 1500, recentFailRatio: 0 },
      { source: "a", destination: "c", failRatio: 0.4, mtuBytes: 1500, probeMtuBytes: 1500 },
    ]);
    expect(m.nodes).toEqual(["a", "b", "c"]);
  });

  it("sends the recent-window query in the Time Machine and folds it in", async () => {
    const seen: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((_url: string, init?: RequestInit) => {
        const q = (JSON.parse(String(init?.body)) as { query: string }).query;
        seen.push(q);
        const v = q.includes("[3m]") ? "0" : q.includes("[5m]") ? "0.4" : "1500";
        return Promise.resolve(json(vector([["a", "b", v]])));
      }),
    );
    const m = await getMatrixAt("pmtu", AT);
    expect(seen.some((q) => q.includes("[3m]"))).toBe(true);
    expect(m.cells).toEqual([
      { source: "a", destination: "b", failRatio: 0.4, mtuBytes: 1500, probeMtuBytes: 1500, recentFailRatio: 0 },
    ]);
  });
});
