import { defineDict, translate, type Dictionary, type Translate } from "@/lib/i18n";

/**
 * matrix-cells — the five phrases lib/matrix-cells.ts's `cellSummary` builds a
 * cell's sentence out of.
 *
 * ONE READING OF A CELL, SHARED. The same sentence is the matrix grid's
 * aria-label (after the "src → dst: " part), the pair card's badge tooltip, and
 * the wording the Overview and the topology edges inherit through the same
 * helper. dict/matrix.ts names it in its own NOT-HERE list for exactly that
 * reason: the grid must not fork a phrase four other surfaces read.
 *
 * The vocabulary is dict/matrix.ts's and dict/cards.ts's — «сбой», «потери
 * пакетов», «нет данных» — so the chip on a card and the spoken name of the
 * cell it came from cannot disagree about what was measured.
 *
 * That sentence was a WISH until the «отказ»/«сбой» sweep: dict/cards.ts said
 * «Отказ» on the tier badge, «Доля отказов» on the breakdown column and
 * «отказ» on a run row, so a pair card's badge and the aria-label of the very
 * cell it was read from used two different nouns for one English word. The
 * cards moved to «сбой» (the matrix legend is the half that ships in the
 * screenshots), and it is a claim with a test behind it now:
 * lib/i18n/cards.test.tsx pins the equalities cell-by-cell and
 * lib/i18n/index.test.tsx sweeps every dictionary so no future key can
 * translate a fail-of-a-probe source as «отказ» again.
 *
 * NOT HERE: the numbers. fmtRatio and fmtRtt are digits plus a unit, identical
 * in both languages, and "RTT p95" is a metric name — the console does not
 * translate what an operator will next type into a PromQL box.
 */

const en = {
  /* The ONLY phrase that may claim absence — all three vectors silent. A cell
     with a p95 and no failure series says the second key instead. */
  "noData": "no data",
  "fail": "fail {ratio}",
  "noFailSignal": "no failure signal recorded",
  "rttP95": "RTT p95 {rtt}",
  "packetLoss": "packet loss {ratio}",
} as const;

export type MatrixCellsKey = keyof typeof en;

export const matrixCellsDict: Dictionary<MatrixCellsKey> = defineDict(en, {
  "noData": "нет данных",
  "fail": "сбой {ratio}",
  /* «не записано», not «нет»: the series is LAZY, so silence means nobody
     emitted a failure sample, not that the pair was never probed. The English
     draws the same distinction and the Russian must keep it. */
  "noFailSignal": "данных о сбоях не записано",
  "rttP95": "RTT p95 {rtt}",
  "packetLoss": "потери пакетов {ratio}",
});

/** enT is the ENGLISH translator cellSummary defaults to, so a caller with one
 *  argument — and lib/matrix-cells.test.ts's four assertions — reads exactly
 *  the bytes it always did. */
export const enT: Translate<MatrixCellsKey> = (key, vars) => translate(matrixCellsDict, "en", key, vars);
