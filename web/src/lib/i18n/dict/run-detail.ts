import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * run-detail — pages/run-detail.tsx, the /diagnostics/runs/{id} permalink: one
 * run's header, its per-pair table, and (for an interval run) the aggregate
 * card plus the per-probe tick strip.
 *
 * NOT HERE, on purpose:
 *   - the run id, the node names on both sides of every pair, `run.plane`, and
 *     `run.type.toUpperCase()` ("TCP", "MTR"): identifiers and protocol names.
 *   - the run STATUS and every pair STATE badge (pending / running / succeeded
 *     / failed / partial / cancelled / dispatched / timeout). They are the
 *     store's own enum, they are what the API answers and what an operator
 *     greps a log for, and the Run checks history list beside them prints the
 *     same words.
 *   - a pair's `error` string and a cancel refusal's problem.detail: the
 *     server wrote them.
 *   - every measured VALUE: fmtNsCompact's latencies and fmtPercent's ratios,
 *     because a millisecond is a millisecond in both languages.
 *
 * DURATIONS are no longer on that list. A rendered span ("5s", "12m") is a word
 * rather than a measurement, so formatDurationNs now takes the locale and ru
 * reads «5 с / 12 мин»; the duration selector's own range tokens ("1m", "24h")
 * stay Latin, being the control's vocabulary.
 *
 * STAMPS are not in that list, and the reason the old note gave for putting
 * them there ("digits are digits") was false: date ORDER and the AM/PM marker
 * are locale-dependent, which is how a Russian page came to print
 * "8/10/2026 3:47 AM". Every stamp on this page goes through lib/i18n's
 * stampFull, i.e. through localeTag — the values stay the runtime's, the
 * SHAPE follows the interface language.
 */

const en = {
  /* ── the page frame ────────────────────────────────────────────────────── */
  "title": "Diagnostic run",
  "loading": "Loading…",
  "loading.run": "Loading run…",
  /* Engaged, the permalink is still shown IN FULL — refusing to render the run
     somebody linked to would be a worse answer — so the header says which
     instant the rest of the console is on. {id} is the decoded run id. */
  "description.at": "{id} — this permalink is shown in full; the console is otherwise viewing {at}",

  "unavailable.title": "This run is unavailable",
  "unavailable.body": "Failed to load this run.",

  /* ── not found ─────────────────────────────────────────────────────────── */
  "notFound.title": "Run not found",
  "notFound.description": "No run matches “{id}”.",
  "notFound.noId": "No run id in the URL.",
  "notFound.heading": "This run does not exist",
  "notFound.body":
    "It may have been an id typo, or the run history behind it is not persisted (in-memory only) and the " +
    "console has since restarted.",
  "notFound.back": "Back to Run checks",

  /* ── the header card ───────────────────────────────────────────────────── */
  "field.type": "Type",
  "field.plane": "Plane",
  "field.pairs": "Pairs",
  "field.started": "Started",
  /* ok/total, worded like the history row on /diagnostics (QA round 4, #14):
     the bare "2/2" was arrived/total and read as passed/total, so a run whose
     every pair FAILED announced itself as a complete success. */
  "pairs.okOfTotal": "{ok}/{total} ok",

  /* ── the route behind a pair row ──────────────────────────────────────────
     The owner on this page: «вся суть MTR — это путь», and «ничего не
     кликабельно». A run's results carry an outcome and a duration and no hops,
     so the route is read back out of the MTR projection when a row is opened.

     NOT HERE: hop addresses, hostnames and every measured figure — those belong
     to components/mtr-hop-table.tsx, whose dictionary already owns them. */
  "pairs.col.expand": "Show the route",
  "pairs.expand.aria": "Show the route from {source} to {destination}",
  "trace.loading": "Loading the recorded route…",
  "trace.error": "The recorded route for this pair is unavailable",
  "trace.none": "No route recorded for this pair yet.",
  /* Deliberately different from the line above: nothing is missing in general,
     this ONE probe falls outside every window the store kept. */
  "trace.noneForProbe": "No recorded route covers this probe.",
  /* A probe that failed walked no route AT ALL — it timed out or never left the
     dispatcher — so it gets this instead of a stored hop table captioned as the
     route it took. */
  "trace.probeFailed": "This probe recorded no route: it never completed a trace.",
  /* A long interval run holds more results than one response may carry, so the page is looking at
     the newest slice of it. Every figure above is computed from what is on screen, which makes this
     sentence the difference between a summary and a wrong one. */
  "results.truncated":
    "Showing the {count} most recent results — this run recorded more than one page can carry, so the figures above describe that slice, not the whole run.",
  /* The probe the reader actually clicked. It leads the panel because it is the only part of it
     that changes from tick to tick. */
  "trace.probe": "Probe #{seq}",
  /* And the reason the hops below often do not change: they belong to the ROUTE, which is folded
     over every trace that walked it. Without this line a strip of identical-looking panels reads
     as a broken control rather than as a stable path. */
  "trace.sharedRoute":
    "The hops below are the route this probe walked, folded over every trace that walked it — while the route holds, every probe reads the same.",
  "trace.openInExplorer": "Open in MTR Explorer",
  "timeline.tick.open": "Show the route this probe took",

  /* ── a non-MTR pair row opens onto the sample's own facts ───────────────── */
  "detail.source": "From",
  "detail.destination": "To",
  "detail.duration": "Duration",
  "detail.state": "State",
  /* The cell above truncates it; this is the agent's sentence in full, which is
     where a timeout usually gets interesting. */
  "detail.error": "Error",

  "cancel": "Cancel run",
  "cancel.failed": "Failed to cancel this run",

  /* ── the interval aggregate ────────────────────────────────────────────── */
  "summary.duration": "Duration",
  "summary.cadence": "Cadence",
  /* The tile's second line, and the reason it exists: it used to read "5s × 4"
     — the BASE cadence and a bare multiplier that named nothing. A stretched
     run (an MTR is thirty hops walked in sequence, not a probe with a timeout)
     keeps a far slower cadence than duration/500, and "× 4" turned out to be
     pairs. Both are said out loud now. "~" for the same reason every other
     total on this page carries one: the sample count is a floor of
     duration/cadence and a late arrival can widen it. */
  "summary.cadence.plan": "{pairs} · ≥ {samples} per pair",
  /* ── measured vs planned ───────────────────────────────────────────────────
     The tile's number was still a PLAN wearing no label: it read "3m" on a run
     producing a probe a minute, because the planner's interval is a WORST CASE
     — a round that finishes early starts the next one immediately, so the plan
     bounds the spacing from above and the truth sits below it by however much
     the fleet beats its own budget.

     Every number on this tile now carries its kind in words. "measured" is
     arithmetic over the timestamps already on screen; "planned" is the run's own
     snapshot. The plan keeps the "no slower than" wording for the same reason
     the sample count keeps its "≥": both are floors on the RATE, and stating
     either as an equality is the exact mistake being fixed. */
  "summary.cadence.value.measured": "{interval} measured",
  "summary.cadence.value.planned": "{interval} planned",
  "summary.cadence.observed": "{pairs} · ≥ {samples} per pair so far",
  "summary.cadence.planNote": "planned no slower than once every {interval}, ≥ {samples} per pair",
  "summary.pairs.one": "{count} pair",
  "summary.pairs.few": "{count} pairs",
  "summary.pairs.many": "{count} pairs",
  "summary.sent": "Sent",
  "summary.failed": "Failed",
  "summary.min": "Min",
  "summary.avg": "Avg",
  /* p95 and max are statistic names and stay Latin in both languages. */
  "summary.p95max": "p95 / max",
  "summary.note":
    "Latency covers probes that answered; a failed or timed-out probe is counted in “failed” and left out of " +
    "min/avg/p95, so a timeout never masquerades as a round trip.",

  /* ── the pair table ────────────────────────────────────────────────────── */
  "pairs.title": "Pairs",
  "pairs.intervalNote": "One row per pair, showing its most recent probe. Every probe is below.",
  "pairs.empty": "No pairs dispatched yet.",
  "pairs.caption": "Per-pair run results",
  "pairs.col.pair": "Pair",
  "pairs.col.state": "State",
  "pairs.col.duration": "Duration",
  "pairs.col.error": "Error",
  /* The pager's noun, for ui/pager.tsx's "Showing 50 of 90 pairs". Lower case
     in English because it ends the sentence; the Russian opens with it. */
  "pairs.subject": "pairs",

  /* ── the probe timeline ────────────────────────────────────────────────── */
  "timeline.title": "Probe timeline",
  "timeline.note":
    "One tick per probe, in the order results arrived. Hover a tick for its latency — a failed probe " +
    "shows what went wrong instead, because it never completed a round trip to time.",
  "timeline.empty": "No probes recorded yet.",
  "timeline.rowStats": "{sent} sent · {failed} failed · p95 {p95}",
  /* TWO tick titles, not one with an {outcome} hole (QA scope 4, finding #12).
     The single sentence appended a duration to every tick, so a probe that
     died on a malformed response advertised "… unexpected end of JSON input
     8.0ms" — a latency for a round trip that never happened, right under the
     card's promise that failures never masquerade as one. A failed tick's
     elapsed time is dispatch overhead, not a measurement, and the honest thing
     to print is the error alone. */
  "timeline.tick": "#{seq} ok {duration}",
  /* {outcome} is the probe's OWN error string from the server when it has one,
     which is why it is interpolated rather than branched inside the sentence. */
  "timeline.tickFailed": "#{seq} {outcome}",
  "timeline.tick.failed": "failed",
  /* The screen-reader summary for an UNFRAMED strip only; a framed track uses the labels below. */
  "timeline.rowLabel": "{source} to {destination}: {sent} probes, {failed} failed",

  /* ── the progress frame ────────────────────────────────────────────────────
     Every total is "~" because plannedSamplesPerPair floors duration/cadence and widens at the
     500-sample cap, so an exact "12" that turned into 13 would be a lie. */
  "timeline.progress.running": "{arrived} of ≥{expected} · ~{remaining} left",
  /* No tail once the run has stopped: "0s left" would suggest something still is coming. */
  "timeline.progress.settled": "{arrived} of ≥{expected}",

  /* {count} is the remaining slot count and picks the form (countForm below). */
  "timeline.trackLabel.pending.one":
    "{source} to {destination}: {arrived} of at least {expected} probes recorded, {count} more probe still to come",
  "timeline.trackLabel.pending.few":
    "{source} to {destination}: {arrived} of at least {expected} probes recorded, {count} more probes still to come",
  "timeline.trackLabel.pending.many":
    "{source} to {destination}: {arrived} of at least {expected} probes recorded, {count} more probes still to come",
  "timeline.trackLabel.settled":
    "{source} to {destination}: {arrived} of at least {expected} probes recorded, {failed} failed",

  /* The "N more pairs are not drawn here" line went with the fixed limit that
     made it necessary: the strips are PAGED now, so every pair is reachable and
     the pager's own count says which of them are on screen. */
} as const;

export type RunDetailKey = keyof typeof en;

export const runDetailDict: Dictionary<RunDetailKey> = defineDict(en, {
  "title": "Диагностический запуск",
  "loading": "Загрузка…",
  "loading.run": "Загрузка запуска…",
  "description.at": "{id} · постоянная ссылка показана целиком, в остальном консоль смотрит на {at}",

  "unavailable.title": "Запуск недоступен",
  "unavailable.body": "Не удалось загрузить этот запуск.",

  "notFound.title": "Запуск не найден",
  "notFound.description": "Под «{id}» ничего нет.",
  "notFound.noId": "В адресе нет идентификатора запуска.",
  "notFound.heading": "Такого запуска нет",
  "notFound.body":
    "Возможно, в идентификаторе опечатка. А может, история запусков не сохраняется (живёт только в памяти), " +
    "а консоль с тех пор перезапускалась.",
  "notFound.back": "Назад на страницу «Проверки вручную»",

  "field.type": "Тип",
  "field.plane": "Плоскость",
  "field.pairs": "Пары",
  "field.started": "Начат",
  "pairs.okOfTotal": "{ok}/{total} успешно",

  "pairs.col.expand": "Показать маршрут",
  "pairs.expand.aria": "Показать маршрут от {source} до {destination}",
  "trace.loading": "Загружаем записанный маршрут…",
  "trace.error": "Записанный маршрут для этой пары недоступен",
  "trace.none": "Для этой пары маршрут ещё не записан.",
  "trace.noneForProbe": "Ни один записанный маршрут не покрывает этот зонд.",
  "trace.probeFailed": "Этот зонд не записал маршрут: трассировка не дошла до конца.",
  "results.truncated":
    "Показаны {count} последних результатов: прогон записал больше, чем помещается в одну выдачу, поэтому цифры выше описывают этот срез, а не весь прогон.",
  "trace.probe": "Зонд №{seq}",
  "trace.sharedRoute":
    "Хопы ниже — это маршрут, по которому прошёл зонд, свёрнутый по всем трассировкам этого маршрута. Пока маршрут не менялся, все зонды показывают одно и то же.",
  "trace.openInExplorer": "Открыть в обзоре MTR",
  "timeline.tick.open": "Показать маршрут этого зонда",

  "detail.source": "Откуда",
  "detail.destination": "Куда",
  "detail.duration": "Длительность",
  "detail.state": "Состояние",
  "detail.error": "Ошибка",

  "cancel": "Отменить запуск",
  "cancel.failed": "Не удалось отменить запуск",

  "summary.duration": "Длительность",
  "summary.cadence": "Периодичность",
  /* «на пару», не «за пару»: счёт идёт по каждой паре отдельно. */
  "summary.cadence.plan": "{pairs} · ≥ {samples} на пару",
  "summary.cadence.value.measured": "{interval} по факту",
  "summary.cadence.value.planned": "{interval} по плану",
  "summary.cadence.observed": "{pairs} · пока ≥ {samples} на пару",
  /* «не реже» — это именно то, что обещает план: он считает круг по худшему случаю, а круг,
     закончившийся раньше, сразу начинает следующий, так что фактический период всегда не больше
     планового. Период здесь словом (formatCadenceProse): «не реже раза в 5 с» читается как предлог. */
  "summary.cadence.planNote": "по плану не реже раза в {interval}, ≥ {samples} на пару",
  "summary.pairs.one": "{count} пара",
  "summary.pairs.few": "{count} пары",
  "summary.pairs.many": "{count} пар",
  "summary.sent": "Отправлено",
  "summary.failed": "Ошибок",
  "summary.min": "Мин",
  "summary.avg": "Сред",
  "summary.p95max": "p95 / макс",
  "summary.note":
    "Задержка считается только по ответившим зондам. Неудачный или отвалившийся по таймауту зонд идёт в счётчик " +
    "«Ошибок» и не попадает в мин/сред/p95, поэтому таймаут никогда не выдаёт себя за измеренную задержку.",

  "pairs.title": "Пары",
  "pairs.intervalNote": "По строке на пару, показан последний зонд. Все зонды ниже.",
  "pairs.empty": "Пока ни одна пара не отправлена.",
  "pairs.caption": "Результаты запуска по парам",
  "pairs.col.pair": "Пара",
  "pairs.col.state": "Состояние",
  "pairs.col.duration": "Длительность",
  "pairs.col.error": "Ошибка",
  "pairs.subject": "Пары",

  "timeline.title": "Лента зондов",
  "timeline.note":
    "По штриху на зонд, в порядке прихода результатов. Наведите на штрих, и он покажет задержку, а у неудачного " +
    "зонда — причину: замерять там нечего, ответ не вернулся.",
  "timeline.empty": "Зондов пока не записано.",
  "timeline.rowStats": "отправлено {sent} · ошибок {failed} · p95 {p95}",
  "timeline.tick": "#{seq} ок {duration}",
  "timeline.tickFailed": "#{seq} {outcome}",
  "timeline.tick.failed": "ошибка",
  "timeline.rowLabel": "{source} → {destination}: зондов {sent}, ошибок {failed}",

  "timeline.progress.running": "{arrived} из ≥{expected} · осталось ~{remaining}",
  "timeline.progress.settled": "{arrived} из ≥{expected}",

  "timeline.trackLabel.pending.one":
    "{source} → {destination}: записано {arrived} зондов из не меньше чем {expected}, ждём ещё {count} зонд",
  "timeline.trackLabel.pending.few":
    "{source} → {destination}: записано {arrived} зондов из не меньше чем {expected}, ждём ещё {count} зонда",
  "timeline.trackLabel.pending.many":
    "{source} → {destination}: записано {arrived} зондов из не меньше чем {expected}, ждём ещё {count} зондов",
  "timeline.trackLabel.settled":
    "{source} → {destination}: записано {arrived} зондов из не меньше чем {expected}, ошибок {failed}",
});

/**
 * countForm picks between the `.one` / `.few` / `.many` keys above.
 *
 * There is no plural MACHINERY here (lib/i18n's module doc says why the module
 * ships none): this is the README's "declare the forms as keys and pick
 * between them in the component" pattern, with the one wrinkle that the RULE
 * differs per language. English has two forms, so its `.few` key carries the
 * same string as its `.many` one and 21 reads "21 more pairs"; Russian has
 * three and 21 reads «Ещё 21 пара». One shared rule could not do both.
 */
export function countForm(locale: string, n: number): "one" | "few" | "many" {
  if (locale !== "ru") return n === 1 ? "one" : "many";
  const teen = n % 100;
  if (teen >= 11 && teen <= 14) return "many";
  const last = n % 10;
  if (last === 1) return "one";
  if (last >= 2 && last <= 4) return "few";
  return "many";
}
