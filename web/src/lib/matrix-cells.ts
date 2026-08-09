import type { MatrixCell } from "@/lib/types";

/**
 * matrix-cells.ts — the ONE reading of a matrix cell, shared by every surface
 * that draws one (the grid, the Overview summary, a node's header health, a
 * pair's two direction badges, the topology problem edges).
 *
 * It exists because the same misreading had been re-implemented five times:
 * `failRatio !== null` as the test for "was this pair measured at all?" (QA
 * round 2, finding #1). That test is wrong in the most common state this
 * console has. The fail-ratio series is LAZY — a pair that has never failed
 * emits no `result="fail"` sample at all — so a healthy fleet answers
 * `failRatio: null` for nearly every cell while `rttP95` and `lossRatio` are
 * full of numbers. Every `failRatio !== null` filter therefore threw away real
 * measurements and rendered "no data" over the top of them.
 *
 * The three vectors are independent on purpose. Engaged, the matrix is folded
 * from separate PromQL instant queries (lib/matrix-promql.ts) and one can
 * return samples while another returns none; live, a recording rule can be
 * missing for one and present for the others. So:
 *
 *   MEASURED   = ANY of failRatio / rttP95 / lossRatio carries a value.
 *   TIER       = read from the ratios that ARE present — packet loss can carry
 *                the tier on its own, latency never can (a slow pair is not a
 *                failing one, and this console does not invent a latency SLO).
 *   UNMEASURED = all three silent. Only then may a surface say "no data".
 */

export type CellTier = "ok" | "warn" | "bad" | "unknown";

/** The two thresholds the whole console ranks on, stated once. Matrix legend,
 *  Overview tiles, node colouring and the topology edge filter all read them
 *  from here so a fourth copy cannot drift a percentage point away. */
export const DEGRADED_AT = 0.01;
export const FAILING_AT = 0.1;

/** isMeasured answers "did anything probe this pair?" — and a latency sample
 *  is an answer. */
export function isMeasured(cell: MatrixCell | undefined): boolean {
  if (!cell) return false;
  return cell.failRatio !== null || cell.rttP95 !== undefined || cell.lossRatio !== undefined;
}

/**
 * severityRatio is the worst ratio the cell ACTUALLY carries, or null when it
 * carries neither. Worst-of rather than fail-first: a pair losing 30% of its
 * UDP packets is failing whatever the failure-ratio series has to say, and a
 * pair with both should be ranked by the louder of the two.
 */
export function severityRatio(cell: MatrixCell | undefined): number | null {
  if (!cell) return null;
  const ratios: number[] = [];
  if (cell.failRatio !== null && cell.failRatio !== undefined) ratios.push(cell.failRatio);
  if (cell.lossRatio !== undefined) ratios.push(cell.lossRatio);
  return ratios.length === 0 ? null : Math.max(...ratios);
}

/**
 * cellTier is the colour/badge every surface paints from.
 *
 * "ok" for a measured cell with no ratio at all is the deliberate half: the
 * pair was probed and nothing reported a failure or a loss, which is the
 * healthy fleet's normal state. "unknown" is reserved for silence — and only
 * silence — because that is the one word an operator reads as "go look".
 */
export function cellTier(cell: MatrixCell | undefined): CellTier {
  if (!isMeasured(cell)) return "unknown";
  const ratio = severityRatio(cell);
  if (ratio === null) return "ok";
  if (ratio >= FAILING_AT) return "bad";
  if (ratio >= DEGRADED_AT) return "warn";
  return "ok";
}

/** isProblemCell is the topology edge filter and the worst-pairs cut: a cell
 *  whose severity has crossed the degraded line. Loss-only qualifies. */
export function isProblemCell(cell: MatrixCell | undefined): boolean {
  const ratio = severityRatio(cell);
  return ratio !== null && ratio >= DEGRADED_AT;
}

/** fmtRatio renders a 0–1 ratio as the percentage every surface prints. */
export function fmtRatio(ratio: number): string {
  return `${(100 * ratio).toFixed(1)}%`;
}

/** fmtRtt renders nanoseconds as milliseconds, or an em-dash when absent. */
export function fmtRtt(ns?: number): string {
  return ns === undefined ? "—" : `${(ns / 1e6).toFixed(1)}ms`;
}

/**
 * cellSummary is the sentence a tooltip and an aria-label say about one cell —
 * one wording, so the two can never disagree about what is known.
 *
 * It states what IS known and never rounds a partial measurement down to "no
 * data": a cell with a p95 and no failure series says so in those words, and
 * an operator reading the screen reader output hears the same latency the
 * sighted reader sees in the cell.
 */
export function cellSummary(cell: MatrixCell | undefined): string {
  if (!isMeasured(cell)) return "no data";
  const parts: string[] = [];
  parts.push(
    cell?.failRatio !== null && cell?.failRatio !== undefined
      ? `fail ${fmtRatio(cell.failRatio)}`
      : "no failure signal recorded",
  );
  if (cell?.rttP95 !== undefined) parts.push(`RTT p95 ${fmtRtt(cell.rttP95)}`);
  if (cell?.lossRatio !== undefined) parts.push(`packet loss ${fmtRatio(cell.lossRatio)}`);
  return parts.join(", ");
}
