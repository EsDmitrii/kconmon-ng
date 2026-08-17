import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * explore — pages/explore.tsx: the curated metric grid and the A/B compare
 * panel above it.
 *
 * NOT HERE, and not translatable from this surface:
 *   - the five CURATED_CHARTS titles ("TCP RTT p95, worst 5 pairs" and its
 *     four siblings). They live in lib/curated-metrics.ts next to the PromQL
 *     that produces them, they name metric families, and the compare panel
 *     puts them in an ECharts legend where they sit beside label values the
 *     server wrote. One vocabulary, one language.
 *   - the range and shift tokens (15m / 1h / 6h / 24h / 7d). Durations, not
 *     words.
 *   - `chart.unit` ("seconds" / "ratio"), printed verbatim in the mixed-units
 *     caption: it is the curated chart's own declared unit, i.e. a value from
 *     the same module as the titles.
 */

const en = {
  "title": "Explore",
  /* Two descriptions, not one with an optional tail: the Time Machine one
     states a different fact (where the window ENDS), and {at} is the viewer's
     own toLocaleString stamp computed by the page. */
  "description": "Curated metric charts across TCP/UDP/ICMP/DNS, recomputed from Prometheus every 30s.",
  "description.at":
    "Curated metric charts across TCP/UDP/ICMP/DNS, ending {at} — the range below is measured back from there.",
  "range.aria": "Time range",

  /* ── a curated card ────────────────────────────────────────────────────── */
  "chart.loading": "Loading chart…",
  "chart.empty": "No series returned for this range — try a longer window above.",
  /* The stand-in when Prometheus's error envelope carries no message of its
     own. The envelope's own `error` string is server text and renders verbatim
     — this is the console admitting it was handed nothing to show. */
  "chart.queryFailed": "query failed",

  /* ── the compare panel ─────────────────────────────────────────────────── */
  "compare.title": "Compare",
  "compare.metricA": "Metric A",
  "compare.metricB": "Compare with metric",
  "compare.metricB.none": "No second metric",
  /* The mode is a CONTROL now, not a consequence of touching the shift picker.
     Choosing a shift used to retire the B metric and its lines without saying
     so, with one line of fine print under the chart as the only notice. */
  "compare.mode.aria": "Compare A with",
  "compare.mode.metric": "another metric",
  "compare.mode.self": "itself, earlier",
  "compare.shift": "Compare with earlier",
  "compare.shift.none": "No shift",
  "compare.shift.earlier": "{label} earlier",
  /* Legend prefixes. "A" and "B" are the panel's two slots and stay Latin in
     both languages — they are what the copy above and below the chart calls
     the legs, and a legend entry reading «А:» beside a Russian «Б:» is two
     glyphs away from the Cyrillic А/В confusion. */
  "compare.legA": "A: {title}",
  "compare.legB": "B: {title}",
  /* Self-shift mode: both legs are the same metric, so its title says nothing
     about which is which. The clock does, and so does the stroke — the two legs
     share a colour, and naming the stroke is what keeps them apart in
     greyscale, for a colour-blind reader and in a printed screenshot. */
  "compare.legA.now": "A · now (solid)",
  "compare.legB.earlier": "A · {label} earlier (dashed)",

  /* The mixed-units caption names both units, and the two halves are NOT the
     same word list: A's is its declared `unit` ("seconds" | "ratio") because
     that is what the axis is, while B's says what KIND of quantity it is
     ("duration" for a seconds chart). Four keys rather than two, so the
     Russian can decline each side on its own. */
  "compare.unitsDiffer": "B is a {unitB} on A's {unitA} axis — read its shape, not its height.",
  "compare.unitB.ratio": "ratio",
  "compare.unitB.duration": "duration",
  "compare.unitA.ratio": "ratio",
  "compare.unitA.seconds": "seconds",
  "compare.idle":
    "Pick a second metric or an earlier window to overlay a reference leg on A's axes. Nothing is queried " +
    "until you do.",

  /**
   * The note under a compare chart whose EARLIER leg came back with nothing
   * (QA round 4, finding #6).
   *
   * The silent case is the whole finding. Picking "7d earlier" on a Prometheus
   * whose retention is 24h drew leg A alone — one line, no legend entry for B,
   * no error — and the honest reading of that picture is "the fleet behaved
   * identically a week ago", which is the opposite of the truth. So the note
   * names retention AND the distance, which is the number an operator has to
   * compare against their own `--storage.tsdb.retention.time`.
   */
  "compare.shiftedEmpty": "No data {shift} ago — Prometheus's retention does not reach that far back.",
} as const;

export type ExploreKey = keyof typeof en;

/**
 * «Метрики», not «Исследование» — chrome.ts's nav already made that call:
 * «Расследование» belongs to Investigate, and this page is curated metrics
 * plus an A/B overlay, so the honest noun beats the calque.
 */
export const exploreDict: Dictionary<ExploreKey> = defineDict(en, {
  "title": "Метрики",
  "description": "Готовые графики по TCP/UDP/ICMP/DNS, пересчитываются из Prometheus каждые 30 с.",
  "description.at":
    "Готовые графики по TCP/UDP/ICMP/DNS до момента {at}, диапазон ниже отсчитывается назад от него.",
  "range.aria": "Диапазон времени",

  "chart.loading": "Загрузка графика…",
  "chart.empty": "На этом диапазоне серий нет, возьмите интервал подлиннее.",
  "chart.queryFailed": "запрос не выполнен",

  "compare.title": "Сравнение",
  "compare.metricA": "Метрика A",
  "compare.metricB": "Сравнить с метрикой",
  "compare.metricB.none": "Без второй метрики",
  "compare.mode.aria": "Сравнить A",
  "compare.mode.metric": "с другой метрикой",
  "compare.mode.self": "с собой раньше",
  "compare.shift": "Сравнить с прошлым",
  "compare.shift.none": "Без сдвига",
  "compare.shift.earlier": "{label} назад",

  "compare.legA": "A: {title}",
  "compare.legB": "B: {title}",
  "compare.legA.now": "A · сейчас (сплошная)",
  "compare.legB.earlier": "A · {label} назад (пунктир)",

  "compare.unitsDiffer": "B здесь {unitB} на оси A ({unitA}), поэтому смотрите на форму, а не на высоту.",
  "compare.unitB.ratio": "доля",
  "compare.unitB.duration": "длительность",
  "compare.unitA.ratio": "доли",
  "compare.unitA.seconds": "секунды",
  "compare.idle":
    "Выберите вторую метрику или прошлое окно, и на оси A ляжет опорная линия. Пока этого нет, " +
    "ни один запрос не уходит.",

  "compare.shiftedEmpty": "Данных за {shift} назад нет: так далеко ретенция Prometheus не достаёт.",
});
