import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * cards — the three OBJECT cards: pages/node-card.tsx, pages/pair-card.tsx and
 * pages/target-card.tsx.
 *
 * ONE dictionary for three files, and that is deliberate rather than lazy:
 * they are three views of the same idea (an object, its health badge, its
 * tabs, its run scan) and they already share their vocabulary in code — the
 * identical TIER_LABEL table, the identical RUN_SCAN_LIMIT sentence, the
 * cadence line target-card imports from targets.tsx. Splitting them into three
 * tables would have been three places for «Здоров» to drift into «Исправен»,
 * which is precisely what the README's one-word-per-concept rule forbids. They
 * are one SURFACE in the sense that matters: an operator moving from a node to
 * one of its pairs must not feel the language change under them.
 *
 * ── the severity vocabulary, fixed here ───────────────────────────────────
 * The tier badge is PRESENTATION — this console's verdict on a ratio, not a
 * field any API returns — so it translates, and it translates the same way on
 * all three cards:
 *
 *     Healthy → Здоров      Degraded → Деградация
 *     Failing → Сбой        No data  → Нет данных
 *
 * ONE WORD FOR A FAILED PROBE: «сбой». This file used to say «Отказ» while
 * dict/matrix.ts, dict/matrix-cells.ts, dict/topology.ts and dict/overview.ts
 * all said «сбой» for the same English concept — the matrix legend is the half
 * an operator sees in every screenshot, so the cards moved. Every fail/failed/
 * failure of a probe or a pair reads «сбой» here now; «отказ» is left to mean
 * REFUSAL and nothing else (dict/investigate.ts's `incident.copy.refused`).
 * lib/i18n/cards.test.tsx asserts the equalities, lib/i18n/index.test.tsx
 * sweeps every dictionary for a relapse.
 *
 * ── and one phrase for a SILENT failure counter: «сбои: н/д» ──────────────
 * `cell.noFailData` is the second half of that same rule. dict/matrix.ts moved
 * off «нет данных о сбоях» because it OPENS with «нет данных» — the phrase this
 * console reserves for a pair nothing probed — and documented the rejection at
 * length; these cards kept the rejected phrasing anyway, on the pair card's leg
 * badges, where it sits beside a p95 saying the opposite (QA scope 2, #11). The
 * two keys are now `cell.noData` / `cell.noFailData`, one pair of readings
 * shared by the leg badges and the node card's per-destination table, and
 * cards.test.tsx pins both against dict/matrix.ts so they cannot drift apart
 * again.
 *
 * ── what stays ───────────────────────────────────────────────────────────
 *   - Node, pair, zone and target NAMES, agent ids, pod IPs, run ids.
 *   - `r.status` and `r.type` on a run row: those are the run's own stored
 *     enum values, shown in a technical list beside the run's id.
 *   - `checkType` and `sourceSelection` on a definition row, for the same
 *     reason they stay on pages/targets.tsx.
 *   - Metric names, PromQL, `checkers.external.enabled`,
 *     `console.database.mode`, `console.prometheus.address`, and every
 *     `GET /api/v1/...` in a limitation sentence.
 *   - Every server message: topology/matrix errors, problem+json details, a
 *     schedule's own lastError.
 */

const en = {
  /* ── the tier badge, shared by all three cards ─────────────────────────── */
  "tier.ok": "Healthy",
  "tier.warn": "Degraded",
  "tier.bad": "Failing",
  "tier.unknown": "No data",
  "health.percent": "{percent}% healthy",
  /* The same figure, with the coverage it actually rests on. Overview's
     worstPairs.scoredGap says this for the fleet; the node header says it for
     one node, and shorter, because it sits in a row of chips rather than under
     a table. */
  "health.percent.scoped": "{percent}% healthy · {scored} of {total} pairs scored",

  /* ── what a matrix cell says when a figure is absent ───────────────────── */
  /* ONE concept, ONE key, used by the pair card's leg badges and by the node
     card's per-destination table. They used to be `pair.leg.*` and the node
     table printed a bare em-dash instead of either — which put three readings
     of two facts on three surfaces (QA scope 2, findings #4 and #11). */
  "cell.noData": "no data",
  "cell.noFailData": "no fail data",

  /* ── tabs ──────────────────────────────────────────────────────────────── */
  "tab.aria": "Tab",
  "tab.overview": "Overview",
  "tab.diagnostics": "Diagnostics",
  "tab.checks": "Checks & Schedules",
  "tab.history": "History",
  "tab.runs": "Runs",
  "protocol.aria": "Protocol",
  "loading": "Loading…",
  "permission.requires": "Requires the {permission} permission",

  /* ── node card ─────────────────────────────────────────────────────────── */
  "node.title": "Node",
  "node.loading": "Loading node…",
  "node.notFound.withName": "No node name in the URL for “{name}”.",
  "node.notFound.bare": "No node name in the URL.",
  "node.notFound.body": "This link is missing a node name.",
  "node.zone": "Zone {zone}",
  "node.stateAsOf": "{zone}state as of {at}",
  "node.topologyUnavailable": "Topology is unavailable",
  "node.matrixUnavailable": "Matrix is unavailable",
  "node.identity": "Agent identity",
  "node.identity.zone": "Zone",
  "node.identity.agentId": "Agent ID",
  "node.identity.podIP": "Pod IP",
  "node.identity.ready": "Ready",
  "node.identity.yes": "yes",
  "node.identity.no": "no",
  /* Why `Ready` can be an em dash when Zone falls back to the agent: readiness
     is a KUBERNETES node condition, and a registered agent is not evidence of
     it. */
  "node.identity.readyNote": "node readiness comes from the Kubernetes node informer",
  "node.breakdown": "Per-destination breakdown",
  "node.breakdown.caption": "Per-destination breakdown for {name}",
  "node.breakdown.destination": "Destination",
  "node.breakdown.failRatio": "Fail ratio",
  /* Present only while the cells carry loss — the same rule the matrix tooltip
     follows, and the vector that decides the header's tier on UDP/ICMP. */
  "node.breakdown.loss": "Packet loss",
  "node.breakdown.rtt": "RTT p95",
  "node.breakdown.empty": "No probe data for this node yet.",
  "node.runs.heading": "Runs touching this node",
  "node.runs.scanNote":
    "No server-side node filter on GET /api/v1/runs yet — this scans the most recent {limit} runs' results " +
    "client-side. An older run touching this node may exist but will not show up here{engaged}.",
  "node.runs.scanNote.engaged": ", and only runs started at or before the viewed instant are listed",
  "node.runs.unavailable": "Run history is unavailable",
  "node.runs.scanning": "Scanning recent runs…",
  "node.runs.empty": "No runs touching this node in the most recent {limit} runs.",
  "node.runs.pairs": "{count} {word}",
  "count.pairs.one": "pair",
  "count.pairs.few": "pairs",
  "count.pairs.many": "pairs",
  "node.annotations": "Annotations",
  "node.annotations.blurb": "Notes pinned to this node over the last 24 hours, plus the fleet-wide ones.",

  /* ── pair card ─────────────────────────────────────────────────────────── */
  "pair.title": "Pair",
  "pair.description": "Pair connectivity (TCP matrix)",
  "pair.notFound.bare": "No pair in the URL.",
  "pair.notFound.body": "This link is missing a source and destination.",
  "pair.matrixUnavailable": "Matrix is unavailable",
  "pair.notFound.unknownEndpoints": "No such pair",
  /* Named endpoints, so an operator can see WHICH half of the URL is the typo. */
  "pair.notFound.oneUnknown": "This fleet has no node called “{name}”.",
  "pair.notFound.bothUnknown": "This fleet has no node called “{a}” and none called “{b}”.",
  "pair.notFound.unknownBody":
    "A pair card is a view of two nodes the fleet actually reports. The names may be a typo, or the nodes may have left the fleet since this link was made — the topology and the matrix agree that neither answers to them now.",
  "pair.notFound.back": "Back to Matrix",
  "pair.chart.title": "RTT p95 by protocol",
  "pair.chart.hourEnding": "(hour ending {at})",
  "pair.chart.lastHour": "(last hour)",
  "pair.chart.emptyAt": "No series returned for this pair in the hour before that instant.",
  "pair.chart.empty": "No series returned for this pair in the last hour.",
  "pair.lastRun": "Last run for this pair",
  "pair.runCheck": "Run check",
  "pair.runFailed": "Failed to start run",
  "pair.runs.unavailable": "Run history is unavailable",
  "pair.runs.scanNote":
    "No matching run in the most recent {limit} runs — GET /api/v1/runs has no source/destination filter yet, so " +
    "an older run for this pair may exist but is not shown here{engaged}.",
  "pair.runs.scanNote.engaged": ", and only runs started at or before the viewed instant are considered",
  "pair.run": "Run",
  "pair.result": "Result",
  "pair.duration": "Duration",
  "pair.recorded": "Recorded",
  /* A boolean this card is describing, not an enum the API returned. */
  "pair.result.ok": "ok",
  "pair.result.failed": "failed",

  /* ── target card ───────────────────────────────────────────────────────── */
  "target.title": "Target",
  "target.loading": "Loading target…",
  "target.notFound.title": "Target not found",
  "target.notFound.withId": "No target matches “{id}”.",
  "target.notFound.bare": "No target id in the URL.",
  "target.notFound.known": "This target does not exist",
  "target.notFound.unknown": "This link is missing a target id.",
  "target.notFound.knownBody":
    "An unknown id and a malformed one look the same from here — both answer 404. It may have been deleted, or " +
    "the id may be a typo.",
  "target.notFound.unknownBody": "A target card needs an id: /targets/{id}.",
  "target.notFound.back": "Back to Targets",
  "target.description": "External probe target",
  "target.descriptionAt": "External probe target — state as of {at}",
  "target.unavailable": "This target is unavailable",
  "target.loadFailed": "Failed to load this target.",
  "target.gate.read":
    "External targets, their check definitions and their schedules are configuration, not telemetry: reading them " +
    "is granted to the operator and admin roles, and deliberately not to viewer — which is the role an anonymous " +
    "session gets. Sign in with an account that holds it.",
  "target.gate.noDatabase":
    "Targets, definitions and schedules are stored in the database — set console.database.mode",
  "target.checks.gate":
    "The header above is everything targets:read alone can show. The definitions probing this target, and their " +
    "cadence, are read with checks:read — schedules ride on the same permission, since a cadence tells you nothing " +
    "the definition it belongs to does not.",
  "target.checks.heading": "Definitions probing this target",
  "target.checks.listAria": "Definitions",
  "target.checks.tmNotice": "Target configuration is shown as of now — only the probe series time-travel.",
  "target.checks.unavailable": "Check definitions are unavailable",
  "target.checks.empty": "No check definition points at this target yet. Until one does, nothing probes it on a schedule.",
  "target.checks.noSchedule": "No schedule — this definition only runs when someone starts it by hand.",
  "target.history.title": "External probe duration p95 by source node",
  "target.history.hourEnding": "(hour ending {at})",
  "target.history.lastHour": "(last hour)",
  "target.history.noPrometheus":
    "Prometheus is not configured for this console — set console.prometheus.address to read probe history. The " +
    "other tabs do not depend on it.",
  "target.history.unavailable": "Probe history is unavailable",
  /* Four candidates, and the fourth is the one the stand actually hit: the
     series is a DURATION histogram, so a probe that never completes contributes
     no sample. A target being hammered and failing every time looks, in this
     chart, exactly like a target nobody probes (QA scope 2, finding #15). */
  "target.history.empty":
    "No external probe series for this target in the last hour. Either nothing is probing it — external checks " +
    "are off fleet-wide (checkers.external.enabled), or no enabled definition and schedule point here yet — or " +
    "probing started too recently for Prometheus to have scraped it, or every probe failed: this chart is built " +
    "from probe durations, and a probe that never completes records none.",
  "target.runs.heading": "Runs against this target",
  "target.runs.scanNote":
    "No server-side target filter on GET /api/v1/runs yet — this scans the most recent {limit} runs' specs " +
    "client-side. An older run against this target may exist but will not show up here.",
  "target.runs.unavailable": "Run history is unavailable",
  "target.runs.scanning": "Scanning recent runs…",
  "target.runs.empty": "No run against this target in the most recent {limit} runs.",
  "target.runs.pairsOk": "{ok}/{total} ok",
  "target.changesNote":
    "Probe results are recorded per source node (e.g. node-a→{name}); this rail matches the target on either side " +
    "of a pair, alongside changes scoped to the target itself.",

  /* ── schedule rows on the target card. The same words pages/targets.tsx
        uses for the same things — duplicated across dictionaries, per the
        README, rather than shared through a file both would edit. ──────── */
  "schedule.enabled": "enabled",
  "schedule.disabled": "disabled",
  "schedule.paused": "paused: definition disabled",
  "schedule.paused.title": "The schedule is on, but {name} is switched off, so nothing fires. The cadence keeps its place and resumes when the definition is switched back on.",
  "schedule.next": "next {at}",
  "schedule.last": "last {at}",
  "schedule.failing": "failing: {message}",
  "schedule.recorded": "Recorded {at}",
  "schedule.cadence.interval": "every {interval}",
  "schedule.cadence.every.second": "every second",
  "schedule.cadence.every.minute": "every minute",
  "schedule.cadence.every.hour": "every hour",
  "schedule.cadence.unit.second": "s",
  "schedule.cadence.unit.minute": "m",
  "schedule.cadence.unit.hour": "h",
  "schedule.cadence.once": "once at {at}",
  "schedule.cadence.continuous": "continuous",
} as const;

export type CardsKey = keyof typeof en;

export const cardsDict: Dictionary<CardsKey> = defineDict(en, {
  "tier.ok": "Здоров",
  "tier.warn": "Деградация",
  /* «Сбой», the word dict/matrix.ts's legend.bad opens with. The badge and the
     legend rank the same ratio through the same lib/matrix-cells.ts threshold;
     two words for it would read as two verdicts. */
  "tier.bad": "Сбой",
  "tier.unknown": "Нет данных",
  "health.percent": "{percent}% здоровья",
  "health.percent.scoped": "{percent}% здоровья · оценку имеют {scored} из {total} пар",

  "cell.noData": "нет данных",
  /* «сбои: н/д», word for word dict/matrix.ts's own cell.noFailData, and for
     the reason that file spells out at length: «нет данных о сбоях» OPENS with
     «нет данных», the phrase reserved for a pair nothing probed, so the two
     facts read as one. The scope goes first here too. lib/i18n/cards.test.tsx
     pins the equality against the matrix. */
  "cell.noFailData": "сбои: н/д",

  "tab.aria": "Вкладка",
  "tab.overview": "Обзор",
  "tab.diagnostics": "Диагностика",
  "tab.checks": "Проверки и расписания",
  "tab.history": "История",
  "tab.runs": "Запуски",
  "protocol.aria": "Протокол",
  "loading": "Загрузка…",
  "permission.requires": "Нужно право {permission}",

  "node.title": "Узел",
  "node.loading": "Загружаем узел…",
  "node.notFound.withName": "В URL нет имени узла для «{name}».",
  "node.notFound.bare": "В URL нет имени узла.",
  "node.notFound.body": "В этой ссылке не хватает имени узла.",
  "node.zone": "Зона {zone}",
  "node.stateAsOf": "{zone}состояние на {at}",
  "node.topologyUnavailable": "Топология недоступна",
  "node.matrixUnavailable": "Матрица недоступна",
  "node.identity": "Идентификация агента",
  "node.identity.zone": "Зона",
  "node.identity.agentId": "ID агента",
  "node.identity.podIP": "IP пода",
  "node.identity.ready": "Готов",
  "node.identity.yes": "да",
  "node.identity.no": "нет",
  "node.identity.readyNote": "готовность узла приходит от информера узлов Kubernetes",
  "node.breakdown": "Разбивка по назначениям",
  "node.breakdown.caption": "Разбивка по назначениям для {name}",
  "node.breakdown.destination": "Назначение",
  /* Word-for-word dict/matrix.ts's "tooltip.failRatio" — the column and the
     matrix tooltip name the same series, so they name it the same. */
  "node.breakdown.failRatio": "Доля сбоев",
  /* dict/matrix.ts's "tooltip.loss", word for word: one series, one name. */
  "node.breakdown.loss": "Потери пакетов",
  "node.breakdown.rtt": "RTT p95",
  "node.breakdown.empty": "Данных зондирования по этому узлу пока нет.",
  "node.runs.heading": "Запуски, затрагивающие этот узел",
  "node.runs.scanNote":
    "Серверного фильтра по узлу у GET /api/v1/runs пока нет, поэтому результаты последних {limit} запусков " +
    "перебираются на клиенте. Запуск постарше, задевающий этот узел, вполне может существовать, но сюда не " +
    "попадёт{engaged}.",
  "node.runs.scanNote.engaged": ", и учитываются только запуски, начатые не позже просматриваемого момента",
  "node.runs.unavailable": "История запусков недоступна",
  "node.runs.scanning": "Просматриваем недавние запуски…",
  "node.runs.empty": "Среди последних {limit} запусков нет ни одного, затрагивающего этот узел.",
  "node.runs.pairs": "{count} {word}",
  "count.pairs.one": "пара",
  "count.pairs.few": "пары",
  "count.pairs.many": "пар",
  "node.annotations": "Заметки",
  "node.annotations.blurb": "Заметки к этому узлу за последние сутки и общефлотовые заодно.",

  "pair.title": "Пара",
  "pair.description": "Связность пары (матрица TCP)",
  "pair.notFound.bare": "В URL нет пары.",
  "pair.notFound.body": "В этой ссылке не хватает источника и назначения.",
  "pair.matrixUnavailable": "Матрица недоступна",
  "pair.notFound.unknownEndpoints": "Такой пары нет",
  "pair.notFound.oneUnknown": "Узла «{name}» во флоте нет.",
  "pair.notFound.bothUnknown": "Ни узла «{a}», ни узла «{b}» во флоте нет.",
  "pair.notFound.unknownBody":
    "Карточка пары показывает два узла, о которых флот действительно сообщает. Возможно, в именах опечатка, а возможно, узлы успели уйти из флота с тех пор, как сделали эту ссылку: сейчас ни топология, ни матрица их не знают.",
  "pair.notFound.back": "Назад к матрице",
  "pair.chart.title": "RTT p95 по протоколам",
  "pair.chart.hourEnding": "(час до {at})",
  "pair.chart.lastHour": "(последний час)",
  "pair.chart.emptyAt": "За час до этого момента серий по этой паре нет.",
  "pair.chart.empty": "За последний час серий по этой паре нет.",
  "pair.lastRun": "Последний запуск по этой паре",
  "pair.runCheck": "Запустить проверку",
  "pair.runFailed": "Не удалось запустить проверку",
  "pair.runs.unavailable": "История запусков недоступна",
  "pair.runs.scanNote":
    "Среди последних {limit} запусков подходящих нет. У GET /api/v1/runs пока нет фильтра по источнику и " +
    "назначению, так что запуск постарше по этой паре вполне может существовать, но здесь не показан{engaged}.",
  "pair.runs.scanNote.engaged": ", и учитываются только запуски, начатые не позже просматриваемого момента",
  "pair.run": "Запуск",
  "pair.result": "Результат",
  "pair.duration": "Длительность",
  "pair.recorded": "Записано",
  "pair.result.ok": "успех",
  "pair.result.failed": "сбой",

  "target.title": "Цель",
  "target.loading": "Загружаем цель…",
  "target.notFound.title": "Цель не найдена",
  "target.notFound.withId": "Ни одна цель не совпадает с «{id}».",
  "target.notFound.bare": "В URL нет идентификатора цели.",
  "target.notFound.known": "Такой цели не существует",
  "target.notFound.unknown": "В этой ссылке не хватает идентификатора цели.",
  "target.notFound.knownBody":
    "Неизвестный идентификатор и битый отсюда выглядят одинаково: на оба приходит 404. Может, цель удалили, а " +
    "может, в идентификаторе опечатка.",
  "target.notFound.unknownBody": "Карточке цели нужен идентификатор: /targets/{id}.",
  "target.notFound.back": "Назад к целям",
  "target.description": "Внешняя цель зондирования",
  "target.descriptionAt": "Внешняя цель зондирования, состояние на {at}",
  "target.unavailable": "Эта цель недоступна",
  "target.loadFailed": "Не удалось загрузить эту цель.",
  "target.gate.read":
    "Внешние цели, их определения проверок и расписания относятся к конфигурации, а не к телеметрии. Читать их " +
    "могут operator и admin, а viewer намеренно не может, и именно viewer достаётся анонимной сессии. Войдите " +
    "под учётной записью с нужной ролью.",
  "target.gate.noDatabase": "Цели, определения и расписания хранятся в базе, задайте console.database.mode",
  "target.checks.gate":
    "Одно только targets:read даёт ровно то, что в заголовке выше. Определения, которые зондируют эту цель, и их " +
    "периодичность читаются по checks:read. Расписания идут тем же правом: периодичность не рассказывает ничего " +
    "сверх определения, которому принадлежит.",
  "target.checks.heading": "Определения, зондирующие эту цель",
  "target.checks.listAria": "Определения",
  "target.checks.tmNotice":
    "Конфигурация цели показана на текущий момент: во времени путешествуют только серии зондирования.",
  "target.checks.unavailable": "Определения проверок недоступны",
  "target.checks.empty":
    "На эту цель пока не указывает ни одно определение проверки. Пока не укажет, по расписанию её никто не зондирует.",
  "target.checks.noSchedule": "Расписания нет, это определение запускается только руками.",
  "target.history.title": "Длительность внешнего зондирования p95 по узлам-источникам",
  "target.history.hourEnding": "(час до {at})",
  "target.history.lastHour": "(последний час)",
  "target.history.noPrometheus":
    "Prometheus для этой консоли не настроен, задайте console.prometheus.address, и история зондирования появится. " +
    "Остальные вкладки от него не зависят.",
  "target.history.unavailable": "История зондирования недоступна",
  "target.history.empty":
    "За последний час внешних серий зондирования по этой цели нет. Либо её никто не зондирует: внешние проверки " +
    "выключены на всём флоте (checkers.external.enabled) или сюда пока не указывает ни одно включённое " +
    "определение с расписанием. Либо зондирование началось совсем недавно, и Prometheus просто не успел собрать " +
    "данные. Либо все зонды падают: график строится по длительностям, а зонд, который не дошёл до конца, " +
    "длительность не записывает.",
  "target.runs.heading": "Запуски по этой цели",
  "target.runs.scanNote":
    "Серверного фильтра по цели у GET /api/v1/runs пока нет, поэтому спецификации последних {limit} запусков " +
    "перебираются на клиенте. Запуск постарше по этой цели вполне может существовать, но сюда не попадёт.",
  "target.runs.unavailable": "История запусков недоступна",
  "target.runs.scanning": "Просматриваем недавние запуски…",
  "target.runs.empty": "Среди последних {limit} запусков нет ни одного по этой цели.",
  "target.runs.pairsOk": "{ok}/{total} успешно",
  "target.changesNote":
    "Результаты зондирования пишутся по узлу-источнику (например, node-a→{name}). Эта лента ловит цель с любой " +
    "стороны пары и заодно изменения в области самой цели.",

  "schedule.enabled": "включено",
  "schedule.disabled": "выключено",
  "schedule.paused": "пауза: определение выключено",
  "schedule.paused.title": "Расписание включено, но {name} выключено, поэтому ничего не запускается. Отсчёт продолжается и возобновится, когда определение снова включат.",
  "schedule.next": "следующий {at}",
  "schedule.last": "последний {at}",
  "schedule.failing": "сбой: {message}",
  "schedule.recorded": "Записано {at}",
  "schedule.cadence.interval": "каждые {interval}",
  "schedule.cadence.every.second": "каждую секунду",
  "schedule.cadence.every.minute": "каждую минуту",
  "schedule.cadence.every.hour": "каждый час",
  "schedule.cadence.unit.second": "с",
  "schedule.cadence.unit.minute": "мин",
  "schedule.cadence.unit.hour": "ч",
  "schedule.cadence.once": "однократно в {at}",
  "schedule.cadence.continuous": "непрерывно",
});

/** pluralKey picks the Russian form for `count`. Duplicated per dictionary on
 *  purpose — see lib/i18n/README.md's one-file-per-surface rule. */
export function pluralKey(count: number, one: CardsKey, few: CardsKey, many: CardsKey): CardsKey {
  const hundred = Math.abs(count) % 100;
  const ten = Math.abs(count) % 10;
  if (hundred >= 11 && hundred <= 14) return many;
  if (ten === 1) return one;
  if (ten >= 2 && ten <= 4) return few;
  return many;
}
