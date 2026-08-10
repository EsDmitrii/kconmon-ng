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
  /* The disabled Chart tab's tooltip — a range query has an x axis, an instant
     one has a single point per series and nothing to draw it against. */
  "tab.chart.disabled": "Chart is only available for range queries",

  /* ── the three placeholders ────────────────────────────────────────────── */
  /* "empty" only ever renders when NOTHING failed (QA round 4, finding #2):
     a failed query has no result to be empty, and the page used to show the
     red error card and this line at the same time. */
  "table.empty": "No data — the query returned an empty result.",
  "table.idle": "Run a query to see results.",
  "chart.empty": "No series to chart.",
  "chart.idle": "Run a range query to see a chart.",
  "json.idle": "Run a query to see the raw response.",

  /* The stand-in for a Prometheus error envelope that carries no message. Its
     `error` field, when there is one, is server text and renders verbatim. */
  "queryFailed": "query failed",
} as const;

export type PromQLConsoleKey = keyof typeof en;

export const promqlConsoleDict: Dictionary<PromQLConsoleKey> = defineDict(en, {
  "title": "Консоль",
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
  "tab.chart.disabled": "График рисуется только для запросов по диапазону",

  "table.empty": "Данных нет: запрос вернул пустой результат.",
  "table.idle": "Выполните запрос, чтобы увидеть результат.",
  "chart.empty": "Рисовать нечего, серий нет.",
  "chart.idle": "Выполните запрос по диапазону, чтобы увидеть график.",
  "json.idle": "Выполните запрос, чтобы увидеть сырой ответ.",

  "queryFailed": "запрос не выполнен",
});
