import type { Translate } from "@/lib/i18n";
import { enT, type MatrixCellsKey } from "@/lib/i18n/dict/matrix-cells";
import type { MatrixCell } from "@/lib/types";

/** The fail-ratio series is LAZY — a pair that has never failed emits no `result="fail"` sample. */

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
 * severityRatio is the worst ratio the cell ACTUALLY carries, or null when it carries neither;
 * worst-of rather than fail-first: a pair losing 30% of its UDP packets is failing whatever the
 * failure-ratio series has to say.
 */
export function severityRatio(cell: MatrixCell | undefined): number | null {
  if (!cell) return null;
  const ratios: number[] = [];
  if (cell.failRatio !== null && cell.failRatio !== undefined) ratios.push(cell.failRatio);
  if (cell.lossRatio !== undefined) ratios.push(cell.lossRatio);
  return ratios.length === 0 ? null : Math.max(...ratios);
}

/** cellTier is the colour/badge every surface paints from; "unknown" is reserved for silence — and only silence. */
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

/** cellSummary is the sentence a tooltip and an aria-label say about one cell — one wording. */
export function cellSummary(cell: MatrixCell | undefined, t: Translate<MatrixCellsKey> = enT): string {
  if (!isMeasured(cell)) return t("noData");
  const parts: string[] = [];
  parts.push(
    cell?.failRatio !== null && cell?.failRatio !== undefined
      ? t("fail", { ratio: fmtRatio(cell.failRatio) })
      : t("noFailSignal"),
  );
  if (cell?.rttP95 !== undefined) parts.push(t("rttP95", { rtt: fmtRtt(cell.rttP95) }));
  if (cell?.lossRatio !== undefined) parts.push(t("packetLoss", { ratio: fmtRatio(cell.lossRatio) }));
  return parts.join(", ");
}
