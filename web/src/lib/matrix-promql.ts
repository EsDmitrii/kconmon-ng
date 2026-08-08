import { promqlQuery } from "./api";
import type { Matrix, MatrixCell, PromResult, Protocol } from "./types";

/**
 * matrix-promql rebuilds the N×N matrix from PromQL evaluated AT AN INSTANT —
 * the Time Machine's half of the matrix (plan Decision 7: "the matrix's cell
 * values are already Prometheus series; the historical matrix is the same
 * PromQL evaluated with the proxy's EXISTING `time` parameter. GET
 * /api/v1/matrix stays live-only").
 *
 * Why this is a faithful port and not a second, competing implementation:
 * every query string below is character-for-character `internal/console/
 * matrix/matrix.go`'s own failRatioQuery / p95Query / lossQuery, and the fold
 * is its Compute (union the pairs seen across all three vectors, sort nodes and
 * cells, seconds→ns for the RTT). The live endpoint runs exactly these three
 * queries through the same Prometheus, with `ts` zero (= now); this runs them
 * with `ts = t`. Nothing else differs, which is why a historical cell and the
 * live cell for the same pair are the same number computed the same way.
 *
 * THE ONE ASSUMPTION, stated plainly: the metric PREFIX. Server-side it is
 * `console.metricsPrefix` (config default "kconmon_ng") and GET /api/v1/config
 * does not publish it. This module hardcodes the default, exactly as
 * lib/curated-metrics.ts and pages/pair-card.tsx's pairSeriesQuery already do
 * for the same metric families — the frontend has assumed this prefix since M1.
 * A console running a CUSTOM prefix therefore gets an EMPTY historical matrix
 * (every series selector misses), which renders as the page's "no probe data"
 * state. That is a wrong-looking answer but never a WRONG answer: no cell can
 * be attributed a value that is not that pair's. Publishing the prefix on
 * /api/v1/config would retire the assumption for all three call sites at once;
 * that is a one-field change, deliberately not smuggled into this task.
 */

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

/**
 * matrixQueries is the per-protocol query set, mirroring Compute's switch. TCP
 * has no packet-loss series at all (it is a connect/duration probe, not a
 * datagram one), so `loss` is absent rather than an empty string — an absent
 * query is one fewer request, not a request for nothing.
 */
export function matrixQueries(protocol: Protocol): { fail: string; rtt: string; loss?: string } {
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
  }
}

/** PairKey is "<source>\0<destination>" — NUL keeps the composite key
 *  unambiguous, the same reason pages/matrix.tsx uses it for its own lookup. */
type PairKey = string;

const key = (src: string, dst: string): PairKey => `${src}\0${dst}`;

/**
 * vectorByPair folds ONE instant-vector response into pair→value, mirroring
 * matrix.go's vectorByPair. Samples missing either label, or carrying a value
 * Prometheus rendered as a string this cannot parse (`NaN`, `+Inf` — a
 * histogram_quantile over an empty bucket set produces exactly those), are
 * SKIPPED rather than defaulted to zero: "no data" and "zero failures" are
 * opposite answers and the matrix's `failRatio: null` exists to keep them
 * apart.
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

/**
 * foldMatrix is matrix.go's Compute minus the fetching: union the pairs, sort,
 * seconds→nanoseconds for the RTT (the wire's duration unit, Global
 * Constraints), and leave a cell's `rttP95`/`lossRatio` ABSENT when that vector
 * had nothing for the pair.
 *
 * `timestamp` is the instant the matrix was EVALUATED at, not the instant it
 * was computed — the live endpoint stamps time.Now() because for it the two are
 * the same thing; here they are not, and the honest stamp is `t`.
 */
export function foldMatrix(
  protocol: Protocol,
  fail: Map<PairKey, number>,
  rtt: Map<PairKey, number>,
  loss: Map<PairKey, number>,
  at: Date,
): Matrix {
  const nodes = new Set<string>();
  const pairs = new Set<PairKey>();
  for (const m of [fail, rtt, loss]) {
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

/**
 * promqlError surfaces Prometheus's OWN error envelope as a thrown Error.
 * promqlQuery resolves rather than throws for it (lib/api.ts's `handle`), which
 * is right for a chart that renders the message inline — but the matrix has one
 * error slot per grid, and silently folding an errored vector into an empty map
 * would paint "no data" over a query that actually failed.
 */
function promqlError(res: PromResult): string | undefined {
  return res.status === "error" ? (res.error ?? "PromQL query failed") : undefined;
}

/**
 * getMatrixAt is getMatrix's Time Machine counterpart: the same matrix, at `t`.
 * The two or three queries go out in parallel — they are independent instant
 * evaluations and serialising them would triple the wait for no gain.
 */
export async function getMatrixAt(protocol: Protocol, at: Date): Promise<Matrix> {
  const q = matrixQueries(protocol);
  const [failRes, rttRes, lossRes] = await Promise.all([
    promqlQuery(q.fail, at),
    promqlQuery(q.rtt, at),
    q.loss ? promqlQuery(q.loss, at) : Promise.resolve<PromResult>({ status: "success" }),
  ]);
  const err = promqlError(failRes) ?? promqlError(rttRes) ?? promqlError(lossRes);
  if (err) throw new Error(err);
  return foldMatrix(protocol, vectorByPair(failRes), vectorByPair(rttRes), vectorByPair(lossRes), at);
}
