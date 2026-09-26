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

/** A wire number counts only when it is actually a finite number; null, NaN and Infinity are not measurements. */
function finite(v: unknown): v is number {
  return typeof v === "number" && Number.isFinite(v);
}

/** isMeasured answers "did anything probe this pair?" — and a latency sample
 *  is an answer. */
export function isMeasured(cell: MatrixCell | undefined): boolean {
  if (!cell) return false;
  return finite(cell.failRatio) || finite(cell.rttP95) || finite(cell.lossRatio) || finite(cell.mtuBytes);
}

/** isReducedPath: the path carries less than this pair is probed at, and says so (PMTUD works). */
export function isReducedPath(cell: MatrixCell | undefined): boolean {
  return !!cell && finite(cell.mtuBytes) && finite(cell.probeMtuBytes) && cell.mtuBytes < cell.probeMtuBytes;
}

/**
 * severityRatio is the worst ratio the cell ACTUALLY carries, or null when it carries neither;
 * worst-of rather than fail-first: a pair losing 30% of its UDP packets is failing whatever the
 * failure-ratio series has to say.
 */
export function severityRatio(cell: MatrixCell | undefined): number | null {
  if (!cell) return null;
  const ratios: number[] = [];
  if (finite(cell.failRatio)) ratios.push(cell.failRatio);
  if (finite(cell.lossRatio)) ratios.push(cell.lossRatio);
  return ratios.length === 0 ? null : Math.max(...ratios);
}

/**
 * pmtuReading names what a path MTU cell shows. A reduced path loses nothing (the path says how big
 * it may be), so ANY failure next to a smaller path MTU is a black hole, even while its ratio over
 * the window is still under the failing line. A full-size reading is only the last probe: each probe
 * dials a fresh socket, so an ECMP-split black hole flips the gauge between full and reduced. So a
 * full-size path with failures is recovering only when the recent window is clean, and a black hole
 * while it still fails; without the recent window only the failing line makes a black hole.
 */
export function pmtuReading(cell: MatrixCell | undefined): "blackhole" | "reduced" | "recovering" | "full" | null {
  if (!cell || !finite(cell.mtuBytes)) return null;
  const fail = finite(cell.failRatio) ? cell.failRatio : 0;
  if (isReducedPath(cell)) return fail > 0 ? "blackhole" : "reduced";
  if (fail > 0 && finite(cell.recentFailRatio)) return cell.recentFailRatio > 0 ? "blackhole" : "recovering";
  return fail >= FAILING_AT ? "blackhole" : "full";
}

/** cellTier is the colour/badge every surface paints from; "unknown" is reserved for silence — and only silence. */
export function cellTier(cell: MatrixCell | undefined): CellTier {
  if (!isMeasured(cell)) return "unknown";
  // A black hole is red from its first failed probe: the 5m fail ratio lags, the legend does not.
  if (pmtuReading(cell) === "blackhole") return "bad";
  if (pmtuReading(cell) === "recovering") return "warn";
  const ratio = severityRatio(cell);
  if (ratio !== null && ratio >= FAILING_AT) return "bad";
  if (ratio !== null && ratio >= DEGRADED_AT) return "warn";
  // A reduced path fails nothing and still breaks UDP applications without their own PMTUD.
  if (isReducedPath(cell)) return "warn";
  return "ok";
}

/** isProblemCell is the topology edge filter: a cell the matrix paints amber or red, so a reduced
 *  or recovering path MTU and loss-only pairs qualify. */
export function isProblemCell(cell: MatrixCell | undefined): boolean {
  const tier = cellTier(cell);
  return tier === "warn" || tier === "bad";
}

/** fmtRatio renders a 0–1 ratio as the percentage every surface prints; a non-finite ratio is no measurement, not "NaN%". */
export function fmtRatio(ratio?: number | null): string {
  return finite(ratio) ? `${(100 * ratio).toFixed(1)}%` : "—";
}

/** fmtRtt renders nanoseconds as milliseconds, or an em-dash when absent; null/NaN is absent, never "0.0ms". */
export function fmtRtt(ns?: number | null): string {
  return finite(ns) ? `${(ns / 1e6).toFixed(1)}ms` : "—";
}

/** cellSummary is the sentence a tooltip and an aria-label say about one cell — one wording. */
export function cellSummary(cell: MatrixCell | undefined, t: Translate<MatrixCellsKey> = enT): string {
  if (!isMeasured(cell)) return t("noData");
  const parts: string[] = [];
  parts.push(
    finite(cell?.failRatio)
      ? t("fail", { ratio: fmtRatio(cell.failRatio) })
      : t("noFailSignal"),
  );
  if (finite(cell?.rttP95)) parts.push(t("rttP95", { rtt: fmtRtt(cell.rttP95) }));
  if (finite(cell?.lossRatio)) parts.push(t("packetLoss", { ratio: fmtRatio(cell.lossRatio) }));
  if (finite(cell?.mtuBytes)) {
    parts.push(
      isReducedPath(cell)
        ? t("pathMtuReduced", { mtu: String(cell.mtuBytes), probe: String(cell.probeMtuBytes) })
        : t("pathMtu", { mtu: String(cell.mtuBytes) }),
    );
    const reading = pmtuReading(cell);
    if (reading === "blackhole") parts.push(t("mtuBlackhole"));
    if (reading === "recovering") parts.push(t("mtuRecovering"));
  }
  return parts.join(", ");
}
