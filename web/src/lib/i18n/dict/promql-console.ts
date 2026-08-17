import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * promql-console — pages/promql-console.tsx, the /console route: an ad-hoc
 * PromQL box and its three result views.
 *
 * This surface is mostly IDENTIFIERS, and almost none of them move:
 *   - the query itself, every metric and label name in the result table's
 *     headers and cells, and the raw JSON envelope. That is the whole point of
 *     the page — an operator types PromQL here and pastes it into a rule file
 *     next.
 *   - "PromQL", "Prometheus", "JSON": tool and format names.
 *   - the range and step tokens (15m … 24h, 15s … 15m). Durations.
 *   - Prometheus's own `errorType` and `error` strings, and any problem+json
 *     detail: server text, rendered verbatim in both languages.
 */

const en = {
  "title": "Console",
  /* The editor's accessible NAME. CodeMirror's editable surface is a bare contenteditable, so
     without this the page's primary input announced itself as "edit, multi-line" and nothing
     else. */
  "editor.aria": "PromQL query",
  "description": "Run ad-hoc PromQL against the same Prometheus the rest of the console reads from.",
  "description.at":
    "Ad-hoc PromQL as of {at} — instant queries are evaluated at that instant, and a range ends there.",

  /* ── the toolbar ───────────────────────────────────────────────────────── */
  "mode.aria": "Query mode",
  "mode.instant": "Instant",
  "mode.range": "Range",
  "range.label": "Range",
  "range.aria": "Range",
  "step.label": "Step",
  "step.aria": "Step",
  "run": "Run",

  /* ── the result switcher ───────────────────────────────────────────────── */
  "tabs.aria": "Result view",
  "tab.table": "Table",
  "tab.chart": "Chart",
  "tab.json": "JSON",
  /* The disabled Chart tab's tooltip. The block follows the RESULT, not the query
     mode: an instant query on a range vector answers with a matrix and draws. */
  "tab.chart.disabled": "This result has no series to chart",

  /* ── the three placeholders ────────────────────────────────────────────── */
  /* "empty" only ever renders when NOTHING failed (QA round 4, finding #2):
     a failed query has no result to be empty, and the page used to show the
     red error card and this line at the same time. */
  "table.empty": "No data — the query returned an empty result.",
  /* ui/pager.tsx's noun for the result table. */
  "table.subject": "rows",
  "table.idle": "Run a query to see results.",
  /* Every figure below was read at some instant, and the table used to say
     which one nowhere. Two sentences because they are two claims: a vector is
     one reading, a range table prints the LAST of each series. */
  /* The result table's own column headers. The series listing under the chart
     already translates the same two words (raw.col.*); the table did not. */
  "table.col.value": "value",
  "table.col.points": "points",
  "table.col.last": "last value",
  "table.col.time": "time",
  "table.at": "Read at {at}",
  "table.lastAt": "Last values, read at {at}",
  "chart.empty": "No series to chart.",
  "chart.idle": "Run a query that returns a series to see a chart.",
  "json.idle": "Run a query to see the raw response.",

  /* ── the listing under the chart ───────────────────────────────────────────
     Grafana Explore's shape, and the answer to the owner's rejection: the Chart
     view drew a picture with no account of what was in it, so a result of 86
     series was readable only through an in-chart legend that had become a
     one-name-at-a-time pager. This table is that account, and past
     LEGEND_MAX_SERIES it IS the legend.

     NOT HERE, on purpose: label names, label values and the metric name. Those
     are Prometheus's own strings and render verbatim in both languages. */
  "raw.title": "Series",
  "raw.caption": "Every series the query matched, with its point count and last value",
  "raw.col.series": "Series",
  "raw.col.points": "Points",
  "raw.col.last": "Last value",
  /* ui/pager.tsx's noun for this list. */
  "raw.subject": "series",
  /* The row expander. The row already shows the labels that DISTINGUISH the
     series; this is the way back to every label it carries. */
  "raw.showFull": "Show all labels",

  /* The stand-in for a Prometheus error envelope that carries no message. Its
     `error` field, when there is one, is server text and renders verbatim. */
  "queryFailed": "query failed",
} as const;

export type PromQLConsoleKey = keyof typeof en;

export const promqlConsoleDict: Dictionary<PromQLConsoleKey> = defineDict(en, {
  "title": "Консоль",
  "editor.aria": "Запрос PromQL",
  "description": "Произвольный PromQL к тому же Prometheus, из которого читает вся консоль.",
  "description.at":
    "Произвольный PromQL на момент {at}: мгновенные запросы считаются в него, а диапазон им заканчивается.",

  "mode.aria": "Режим запроса",
  "mode.instant": "Мгновенный",
  "mode.range": "Диапазон",
  "range.label": "Диапазон",
  "range.aria": "Диапазон",
  "step.label": "Шаг",
  "step.aria": "Шаг",
  "run": "Выполнить",

  "tabs.aria": "Вид результата",
  "tab.table": "Таблица",
  "tab.chart": "График",
  "tab.json": "JSON",
  "tab.chart.disabled": "В этом результате нет серий для графика",

  "table.empty": "Данных нет: запрос вернул пустой результат.",
  "table.subject": "Строки",
  "table.idle": "Выполните запрос, чтобы увидеть результат.",
  "table.col.value": "значение",
  "table.col.points": "точек",
  "table.col.last": "последнее",
  "table.col.time": "время",
  "table.at": "Снято на {at}",
  "table.lastAt": "Последние значения, снято на {at}",
  "chart.empty": "Рисовать нечего, серий нет.",
  "chart.idle": "Выполните запрос, который вернёт серию, чтобы увидеть график.",

  "raw.title": "Серии",
  "raw.caption": "Все серии, которые нашёл запрос, с числом точек и последним значением",
  "raw.col.series": "Серия",
  "raw.col.points": "Точек",
  "raw.col.last": "Последнее",
  "raw.subject": "Серии",
  "raw.showFull": "Показать все метки",
  "json.idle": "Выполните запрос, чтобы увидеть сырой ответ.",

  "queryFailed": "запрос не выполнен",
});
