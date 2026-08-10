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
