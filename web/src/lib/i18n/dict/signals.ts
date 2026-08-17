import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * signals — components/investigation-signals.tsx, the Investigate page's
 * right-hand column: the matrix-delta chip, the two signal charts, the cursor
 * readout and the two lines the column shows instead of charts.
 *
 * Its own file rather than a wing of dict/investigate.ts because the component
 * is a component: it is exported, unit-tested on its own (the option builders
 * are asserted where they are BUILT — EChart is mocked everywhere), and the
 * page hands it only data.
 *
 * The vocabulary is dict/matrix.ts's and dict/investigate.ts's: «Доля сбоев»,
 * «Потери пакетов», «область». The chip and the matrix cell it was computed
 * from must not call one number two things.
 *
 * NOT HERE, on purpose:
 *   - the scope headline. The page builds it (scopeHeadline) from
 *     dict/investigate.ts and passes it in already worded.
 *   - every problem+json detail. `problemDetail` prefers the SERVER's sentence
 *     and only its ABSENCE — a transport failure with no body — is ours.
 *   - "RTT p95", "Prometheus", `promql:query`, `console.prometheus.address`,
 *     and "pp" (percentage points, a unit).
 *   - the ECharts SERIES names (lib/annotations.ts's two). A series name is
 *     what the legend toggles a series BY — identity, not a label — and both
 *     are exported constants tests read as such.
 */

const en = {
  "title": "Signals",

  /* ── the matrix delta chip ─────────────────────────────────────────────── */
  "delta.failRatio": "Fail ratio",
  "delta.caption": "window start vs window end",
  /* Percentage POINTS, the unit on the delta chip. A word, so it is translated —
     it used to be a hard-coded "pp" in the Russian interface too. */
  "delta.unit": "pp",

  /* ── the cursor readout ────────────────────────────────────────────────── */
  /* DOM, not a chart tooltip: a canvas marker cannot be focused or read aloud.
     {at} is either the formatted instant (the VIEWER's locale) or the phrase
     below — one key, so the Russian can put the verb where it belongs. The
     cursor is the PAGE's now (lib/chart-cursor.tsx), moved by a timeline row or
     by hovering any chart, which is why the empty phrase no longer says "row". */
  "cursor": "Cursor {at}",
  "cursor.none": "— nothing hovered",

  /* ── the two lines that replace the charts ─────────────────────────────── */
  /* "Nothing was requested" is the load-bearing half: the panes are not broken
     and not empty, they were never asked. dict/investigate.ts's source list
     draws the same distinction in the same strength. */
  "gated":
    "The loss and RTT panes read Prometheus through the guarded proxy, which needs promql:query. Nothing was " +
    "requested.",
  "promUnset":
    "Prometheus is not configured for this console — set console.prometheus.address to draw the scope's signals. " +
    "The timeline above does not depend on it.",

  /* ── the two charts ────────────────────────────────────────────────────── */
  "chart.loss": "Packet loss",
  "chart.rtt": "RTT p95",
  "chart.loss.empty":
    "No loss series for this scope in the window — nothing is probing it, or the samples have not been scraped yet.",
  "chart.rtt.empty": "No RTT series for this scope in the window.",

  /* The window is wider than one query_range may be, so the two charts are not
     fetched at all and say why HERE instead of letting the proxy's own
     "range 29h30m0s > max 24h0m0s: range exceeds maximum" stand in a chart's
     place (QA scope 4, finding #5). {hours} is PROMQL_MAX_RANGE_MS, so the
     number cannot drift away from the constant that decides it. */
  "chart.tooWide":
    "This range is wider than {hours}h, and one Prometheus query covers at most that much " +
    "(console.prometheus.maxRange) — narrow the range to draw this chart. The timeline is not affected.",

  /* Prometheus's own error envelope RESOLVES rather than throws, so a
     query-level failure lands in the body. Both of these are the stand-in for
     a failure that carried no sentence of its own. */
  "error.queryFailed": "query failed",
  "error.noBody": "The query could not be run.",
} as const;

export type SignalsKey = keyof typeof en;

export const signalsDict: Dictionary<SignalsKey> = defineDict(en, {
  "title": "Сигналы",

  "delta.failRatio": "Доля сбоев",
  "delta.unit": "п.п.",
  "delta.caption": "начало интервала против конца",

  "cursor": "Курсор {at}",
  "cursor.none": "ни на что не наведён",

  "gated":
    "Панели потерь и RTT читают Prometheus через защищённый прокси, а ему нужно право promql:query. Ничего не " +
    "запрашивалось.",
  "promUnset":
    "Prometheus для этой консоли не настроен, задайте console.prometheus.address, и сигналы этой области " +
    "нарисуются. Лента выше от него не зависит.",

  "chart.loss": "Потери пакетов",
  "chart.rtt": "RTT p95",
  "chart.loss.empty":
    "За этот интервал по области нет серии потерь: либо её никто не зондирует, либо выборки ещё не собраны.",
  "chart.rtt.empty": "За этот интервал по области нет серии RTT.",

  "chart.tooWide":
    "Этот интервал шире {hours} ч, а один запрос к Prometheus столько и охватывает " +
    "(console.prometheus.maxRange). Сузьте интервал, и график нарисуется. На ленту это не влияет.",

  "error.queryFailed": "запрос не выполнен",
  "error.noBody": "Запрос выполнить не удалось.",
});
