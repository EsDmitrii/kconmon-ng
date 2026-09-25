import { promqlQuery } from "./api";
import type { Matrix, MatrixCell, PromResult, Protocol } from "./types";

/** GET /api/v1/matrix stays live-only. */

/** METRICS_PREFIX mirrors internal/config/defaults.go's MetricsPrefix. */
export const METRICS_PREFIX = "kconmon_ng";

/** RATE_WINDOW is matrix.go's own `[5m]`, in one place so the three query
 *  builders below cannot drift apart from each other. */
const RATE_WINDOW = "5m";

function failRatioQuery(proto: Protocol): string {
  const m = `${METRICS_PREFIX}_${proto}_results_total`;
  return (
    `sum by (source_node, destination_node) (rate(${m}{result="fail"}[${RATE_WINDOW}])) / ` +
    `sum by (source_node, destination_node) (rate(${m}[${RATE_WINDOW}]))`
  );
}

function p95Query(bucketMetric: string): string {
  return `histogram_quantile(0.95, sum by (source_node, destination_node, le) (rate(${bucketMetric}[${RATE_WINDOW}])))`;
}

function lossQuery(proto: Protocol): string {
  return `avg by (source_node, destination_node) (${METRICS_PREFIX}_${proto}_packet_loss_ratio)`;
}

function mtuQuery(): string {
  return `min by (source_node, destination_node) (${METRICS_PREFIX}_pmtu_bytes)`;
}

function probeQuery(): string {
  return `max by (source_node) (${METRICS_PREFIX}_agent_pmtu_probe_bytes)`;
}

/**
 * matrixQueries is the per-protocol query set, mirroring Compute's switch. TCP
 * has no packet-loss series at all (it is a connect/duration probe, not a
 * datagram one), so `loss` is absent rather than an empty string — an absent
 * query is one fewer request, not a request for nothing. pmtu has no RTT: it
 * measures sizes, and reads its source's probe size to tell a reduced path.
 */
export function matrixQueries(protocol: Protocol): {
  fail: string;
  rtt?: string;
  loss?: string;
  mtu?: string;
  probe?: string;
} {
  switch (protocol) {
    case "tcp":
      return { fail: failRatioQuery("tcp"), rtt: p95Query(`${METRICS_PREFIX}_tcp_total_duration_seconds_bucket`) };
    case "udp":
      return {
        fail: failRatioQuery("udp"),
        rtt: p95Query(`${METRICS_PREFIX}_udp_rtt_seconds_bucket`),
        loss: lossQuery("udp"),
      };
    case "icmp":
      return {
        fail: failRatioQuery("icmp"),
        rtt: p95Query(`${METRICS_PREFIX}_icmp_rtt_seconds_bucket`),
        loss: lossQuery("icmp"),
      };
    case "pmtu":
      return { fail: failRatioQuery("pmtu"), mtu: mtuQuery(), probe: probeQuery() };
  }
}

/** PairKey is "<source>\0<destination>" — NUL keeps the composite key
 *  unambiguous, the same reason pages/matrix.tsx uses it for its own lookup. */
type PairKey = string;

const key = (src: string, dst: string): PairKey => `${src}\0${dst}`;

/**
 * vectorByPair folds ONE instant-vector response into pair→value; samples missing either label, or
 * carrying a value Prometheus rendered as a string this cannot parse (`NaN`, `+Inf` — a
 * histogram_quantile over an empty bucket set produces exactly those).
 */
export function vectorByPair(res: PromResult): Map<PairKey, number> {
  const out = new Map<PairKey, number>();
  if (res.status !== "success" || res.data?.resultType !== "vector") return out;
  for (const raw of res.data.result) {
    const sample = raw as { metric?: Record<string, string>; value?: [number, string] };
    const src = sample.metric?.source_node;
    const dst = sample.metric?.destination_node;
    if (!src || !dst) continue;
    const v = Number(sample.value?.[1]);
    if (!Number.isFinite(v)) continue;
    out.set(key(src, dst), v);
  }
  return out;
}

/** vectorBySource folds a source_node-keyed vector: the agent-level probe size. */
export function vectorBySource(res: PromResult): Map<string, number> {
  const out = new Map<string, number>();
  if (res.status !== "success" || res.data?.resultType !== "vector") return out;
  for (const raw of res.data.result) {
    const sample = raw as { metric?: Record<string, string>; value?: [number, string] };
    const src = sample.metric?.source_node;
    const v = Number(sample.value?.[1]);
    if (!src || !Number.isFinite(v)) continue;
    out.set(src, v);
  }
  return out;
}

/** foldMatrix is matrix.go's Compute minus the fetching: union the pairs. */
export function foldMatrix(
  protocol: Protocol,
  fail: Map<PairKey, number>,
  rtt: Map<PairKey, number>,
  loss: Map<PairKey, number>,
  at: Date,
  mtu: Map<PairKey, number> = new Map(),
  probe: Map<string, number> = new Map(),
): Matrix {
  const nodes = new Set<string>();
  const pairs = new Set<PairKey>();
  for (const m of [fail, rtt, loss, mtu]) {
    for (const k of m.keys()) {
      const [src, dst] = k.split("\0");
      nodes.add(src);
      nodes.add(dst);
      pairs.add(k);
    }
  }

  const cells: MatrixCell[] = [...pairs].map((k) => {
    const [source, destination] = k.split("\0");
    const cell: MatrixCell = { source, destination, failRatio: fail.has(k) ? (fail.get(k) as number) : null };
    if (rtt.has(k)) cell.rttP95 = Math.round((rtt.get(k) as number) * 1e9);
    if (loss.has(k)) cell.lossRatio = loss.get(k) as number;
    if (mtu.has(k)) {
      cell.mtuBytes = mtu.get(k) as number;
      if (probe.has(source)) cell.probeMtuBytes = probe.get(source) as number;
    }
    return cell;
  });
  cells.sort((a, b) => (a.source === b.source ? a.destination.localeCompare(b.destination) : a.source.localeCompare(b.source)));

  return {
    protocol,
    plane: "pod",
    nodes: [...nodes].sort(),
    cells,
    timestamp: at.toISOString(),
  };
}

/** promqlError surfaces Prometheus's OWN error envelope as a thrown Error. */
function promqlError(res: PromResult): string | undefined {
  return res.status === "error" ? (res.error ?? "PromQL query failed") : undefined;
}

/**
 * getMatrixAt is getMatrix's Time Machine counterpart: the same matrix; the protocol's queries go
 * out in parallel, and an absent one is no request at all.
 */
export async function getMatrixAt(protocol: Protocol, at: Date): Promise<Matrix> {
  const q = matrixQueries(protocol);
  const optional = (query: string | undefined) =>
    query ? promqlQuery(query, at) : Promise.resolve<PromResult>({ status: "success" });
  const [failRes, rttRes, lossRes, mtuRes, probeRes] = await Promise.all([
    promqlQuery(q.fail, at),
    optional(q.rtt),
    optional(q.loss),
    optional(q.mtu),
    optional(q.probe),
  ]);
  const err =
    promqlError(failRes) ?? promqlError(rttRes) ?? promqlError(lossRes) ?? promqlError(mtuRes) ?? promqlError(probeRes);
  if (err) throw new Error(err);
  return foldMatrix(
    protocol,
    vectorByPair(failRes),
    vectorByPair(rttRes),
    vectorByPair(lossRes),
    at,
    vectorByPair(mtuRes),
    vectorBySource(probeRes),
  );
}
