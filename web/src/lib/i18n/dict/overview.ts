import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * overview — pages/overview.tsx: the three stat tiles, the worst-pairs table
 * and the three summary panels (firing alerts, open incidents, recent events).
 *
 * NOT HERE, on purpose:
 *   - `problem.detail` behind every "unavailable" note. The card prints
 *     `error.message`, which IS the server's own sentence (ApiError carries
 *     problem+json through verbatim), and the only strings below are the
 *     headline WE put above it.
 *   - node names, pair names, incident titles, alert names, alert label sets,
 *     event summaries and event scopes. All data.
 *   - a raw severity this console does not know ("no severity" is our word for
 *     an EMPTY one; anything else Prometheus labelled is printed as it came).
 *   - permission strings (incidents:read, events:read, alerts:read) and config
 *     keys (console.database.mode, console.prometheus.address) — an operator
 *     types those into a role binding or a values file.
 *   - "#", "→" and "—". Symbols, not prose; identical in both languages.
 *
 * The Live feed's severity words are duplicated here rather than shared (QA
 * round 1, finding #14 keeps the two surfaces saying ONE word per severity —
 * that is a wording rule, not a reason to make two pages share a file).
 */

const en = {
  "title": "Overview",
  "description": "Cluster health at a glance, recomputed from Prometheus every 15s.",
  /* Engaged, every query on this page has its poll off — promising a 15s
     refresh would describe a page that is deliberately frozen. */
  "description.engaged": "Cluster health at the instant you are viewing. Nothing here refreshes.",

  "loading": "Loading overview…",
  /* The three summary panels share one skeleton and one sr-only line. */
  "panel.loading": "Loading…",

  /* The qualifier every pair number on this page needs: one protocol, one
     plane. TCP and pod are identifiers — only "plane" is ours. */
  "qualifier": "TCP · pod plane",

  /* ── the failed-dependency card ─────────────────────────────────────────── */
  "problem.matrix": "The pair matrix is unavailable",
  "problem.topology": "The node list is unavailable",
  "problem.retry":
    "The page keeps retrying every 15s. If it persists, check that the console can reach Prometheus.",
  /* The one case where the server's own sentence is NOT what gets printed:
     nothing readable arrived, so the fetch layer's or the JSON parser's own
     wording would be standing in for it — a mechanism nobody can act on, in one
     language whatever the chrome is set to. */
  "problem.unreadable":
    "The console could not read the answer. Something between the browser and the console — an ingress, a proxy, a dropped connection — replied instead of it.",

  /* ── stat tiles ─────────────────────────────────────────────────────────── */
  "tiles.nodesReady": "Nodes ready",
  "tiles.nodesReady.noTopology": "Topology unavailable",
  /* The topology ANSWERED and carried no nodes at all, while agents (or the
     matrix) plainly know of some. "0/0" would be this page inventing an empty
     cluster, so the tile counts what it actually has and names the source —
     readiness is the half nobody told it. */
  "tiles.nodesReady.fromAgents": "Counted from agents — no k8s node inventory, so readiness is unknown.",
  "tiles.nodesReady.fromMatrix": "Counted from the pair matrix — no k8s node inventory, so readiness is unknown.",
  /* A historical fold is bounded by what the event log kept, and the body says
     so in its own counters. Shown only when a bound actually bit. */
  "tiles.nodesReady.bounded.truncated": "The event window was truncated, so this reconstruction is partial.",
  "tiles.nodesReady.bounded.unfoldable": "{count} events carried no node detail and could not be folded in.",
  "tiles.failing": "Failing pairs",
  "tiles.failing.tone": "Fail ≥ 10%",
  "tiles.degraded": "Degraded pairs",
  "tiles.degraded.tone": "Fail 1–10%",
  /* Zero over zero. A bare "0" under "Failing pairs" reads as a clean fleet,
     and the nodes tile beside it already answers "nothing measured" with an
     em-dash — the pair tiles now say it the same way. */
  "tiles.pairs.noData": "No pair was measured here, so there is nothing to count.",

  /* ── worst pairs ────────────────────────────────────────────────────────── */
  "worstPairs.title": "Worst pairs",
  /* English needs two forms, Russian sidesteps the count entirely ("Измерено
     пар: 5" agrees with nothing) — so both keys carry the same ru string. */
  "worstPairs.measured.one": "{count} measured pair",
  "worstPairs.measured.many": "{count} measured pairs",

  /* The scored/measured gap, stated wherever it exists rather than only at
     scored=0: 9 ranked out of 90 measured is not a healthy fleet, it is a
     mostly unranked one. */
  "worstPairs.scoredGap": "{scored} of {total} pairs have a failure ratio; the rest have no failure samples.",

  "worstPairs.empty.noData.title": "No probe data in Prometheus yet",
  "worstPairs.empty.noData.body":
    "Pairs appear here once the agents have completed a probe round and Prometheus has scraped them — usually within a minute of the DaemonSet becoming ready.",
  /* Engaged, "not yet" is the wrong tense: the DaemonSet's first round is not
     something a past instant is still waiting for. */
  "worstPairs.empty.noData.title.engaged": "No probe data at this instant",
  "worstPairs.empty.noData.body.engaged":
    "Prometheus has no samples for these pairs at the instant you are viewing. Return to Live, or pick another instant.",
  "worstPairs.empty.unscored.title": "No failure ratio for these pairs",
  "worstPairs.empty.unscored.body.one":
    "{count} pair is reporting latency, but the failure-ratio series has no samples here — worst-first ranking needs it, so this list stays empty rather than reading as healthy.",
  "worstPairs.empty.unscored.body.many":
    "{count} pairs are reporting latency, but the failure-ratio series has no samples here — worst-first ranking needs it, so this list stays empty rather than reading as healthy.",
  "worstPairs.empty.healthy.title": "No failing or degraded pairs",
  "worstPairs.empty.healthy.body":
    "Every scored pair is under a 1% failure ratio. Anything that crosses that line shows up here, worst first.",

  "table.caption": "Worst pairs by failure ratio",
  "table.pair": "Pair",
  "table.fail": "Fail %",
  "table.rtt": "p95 RTT",
  "table.status": "Status",
  "table.status.failing": "Failing",
  "table.status.degraded": "Degraded",
  /* The row's investigate affordance — deliberately the same word the firing-alert rows use. */
  "table.investigate": "investigate",

  /* ── firing alerts ──────────────────────────────────────────────────────── */
  "alerts.title": "Firing alerts",
  "alerts.open": "open Alerting",
  "alerts.engaged": "Alert state is a live-only signal — Prometheus keeps no firing history here.",
  "alerts.denied": "Firing alerts need alerts:read — none was requested.",
  "alerts.error": "The firing set is unavailable right now.",
  "alerts.noPrometheus":
    "Prometheus is not configured for this console — set console.prometheus.address. There is no firing state to show.",
  /* NOT "nothing is firing": this card reads the rules this console manages and
     no others, so a quiet list is a fact about OUR rules, never about the
     cluster. Somebody else's rule may well be firing this second. */
  "alerts.empty": "None of this console's rules is firing. Rules live on /alerting; Prometheus evaluates them.",
  "alerts.hidden.one": "{count} more firing alert is not shown here.",
  "alerts.hidden.many": "{count} more firing alerts are not shown here.",
  /* `severity` is the Prometheus LABEL name, so it stays in both languages —
     this is the badge for a row that carries the label empty. */
  "alerts.noSeverity": "no severity",
  /* The card's own bound. kconmon-ng is not an aggregator of everybody's alerts:
     a rule this console does not manage belongs to whoever wrote it, and its
     firing state is read in Alertmanager and Grafana, not here. */
  "alerts.managedOnly":
    "This card lists only the rules this console manages. Anything else firing in this cluster is read in " +
    "Alertmanager or Grafana, not here.",
  "alerts.investigate": "investigate",

  /* ── open incidents ─────────────────────────────────────────────────────── */
  "incidents.title": "Open incidents",
  "incidents.denied": "Open incidents need incidents:read — none was requested.",
  "incidents.error": "The incident list is unavailable right now.",
  "incidents.empty": "No open incidents. Saving an investigation on /investigate opens one.",
  /* Our word for an incident whose scope is EMPTY; any other scope is data. */
  "incidents.scope.global": "global",

  /* ── recent events ──────────────────────────────────────────────────────── */
  "events.title": "Recent events",
  "events.open": "open Live",
  "events.denied": "Fleet events need events:read — none was requested.",
  "events.error": "The event feed is unavailable right now.",
  "events.empty":
    "Nothing has happened yet. Agent restarts, node readiness changes and MTR triggers land here.",
  /* Engaged the panel asks for the newest ten AT OR BEFORE t, so an empty
     answer means "nothing by then", not "nothing ever". */
  "events.empty.engaged":
    "No events at or before the instant you are viewing. Return to Live, or pick another instant.",

  "severity.info": "Info",
  "severity.warn": "Warn",
  "severity.error": "Error",

  /* The age column, coarse on purpose (see fmtAge). Units only — the letters
     are the same ones dict/alerting.ts's "{count} с назад" family uses, so one
     age reads the same on both surfaces. */
  "age.seconds": "{count}s",
  "age.minutes": "{count}m",
  "age.hours": "{count}h",
  "age.days": "{count}d",

  /* Said once, by both the incidents and the events panel. */
  "db.note": "History needs a database — set console.database.mode. Nothing was requested.",
} as const;

export type OverviewKey = keyof typeof en;

/**
 * «Сбой» is this console's word for failing, «деградация» for degraded, and
 * they hold across the tiles, the table badge and the blank slates — the same
 * one-word-per-concept rule the matrix and topology legends follow.
 *
 * "Warn" is «Внимание» rather than «Предупреждение»: the badge sits in a fixed
 * 5.25rem column on /live and the long word does not fit it, so the short one
 * wins on both pages rather than the vocabulary splitting between them.
 */
export const overviewDict: Dictionary<OverviewKey> = defineDict(en, {
  "title": "Обзор",
  "description": "Здоровье кластера одним взглядом, пересчёт из Prometheus каждые 15 с.",
  "description.engaged": "Здоровье кластера на выбранный момент, без обновления.",

  "loading": "Загрузка обзора…",
  "panel.loading": "Загрузка…",

  "qualifier": "TCP · плоскость pod",

  "problem.matrix": "Матрица пар недоступна",
  "problem.topology": "Список узлов недоступен",
  "problem.retry":
    "Страница повторяет запрос каждые 15 с. Если не отпускает, проверьте, дотягивается ли консоль до Prometheus.",
  "problem.unreadable":
    "Консоль не смогла прочитать ответ. Похоже, ответила не она, а что-то по дороге: ingress, прокси или оборвавшееся соединение.",

  "tiles.nodesReady": "Готовых узлов",
  "tiles.nodesReady.noTopology": "Топология недоступна",
  "tiles.nodesReady.fromAgents": "Считано по агентам, без инвентаря узлов k8s: готовность неизвестна.",
  "tiles.nodesReady.fromMatrix": "Считано по матрице пар, без инвентаря узлов k8s: готовность неизвестна.",
  "tiles.nodesReady.bounded.truncated": "Окно событий обрезано, поэтому восстановление неполное.",
  "tiles.nodesReady.bounded.unfoldable": "Событий без деталей узла, которые не свернулись: {count}.",
  "tiles.failing": "Пары со сбоями",
  "tiles.failing.tone": "Сбой ≥ 10%",
  "tiles.degraded": "Пары с деградацией",
  "tiles.degraded.tone": "Сбой 1–10%",
  "tiles.pairs.noData": "Здесь не измерена ни одна пара, считать нечего.",

  "worstPairs.title": "Худшие пары",
  "worstPairs.measured.one": "Измерено пар: {count}",
  "worstPairs.measured.many": "Измерено пар: {count}",

  "worstPairs.scoredGap": "Оценку имеют {scored} из {total} пар, у остальных нет выборок сбоев.",

  "worstPairs.empty.noData.title": "В Prometheus ещё нет данных зондов",
  "worstPairs.empty.noData.body":
    "Пары появятся, как только агенты пройдут круг зондирования, а Prometheus их соберёт. Обычно это минута с того момента, как DaemonSet стал готов.",
  "worstPairs.empty.noData.title.engaged": "На этот момент данных зондов нет",
  "worstPairs.empty.noData.body.engaged":
    "У Prometheus нет выборок по этим парам на просматриваемый момент. Вернитесь в реальное время или выберите другой момент.",
  "worstPairs.empty.unscored.title": "Для этих пар нет доли сбоев",
  "worstPairs.empty.unscored.body.one":
    "Пар с измеренной задержкой: {count}, но у серии доли сбоев здесь нет ни одной выборки. Ранжирование от худших держится именно на ней, поэтому список остаётся пустым, а не выдаёт себя за норму.",
  "worstPairs.empty.unscored.body.many":
    "Пар с измеренной задержкой: {count}, но у серии доли сбоев здесь нет ни одной выборки. Ранжирование от худших держится именно на ней, поэтому список остаётся пустым, а не выдаёт себя за норму.",
  "worstPairs.empty.healthy.title": "Пар со сбоями и деградацией нет",
  "worstPairs.empty.healthy.body":
    "У всех оценённых пар доля сбоев ниже 1%. Что перевалит за эту черту, появится здесь, худшее первым.",

  "table.caption": "Худшие пары по доле сбоев",
  "table.pair": "Пара",
  "table.fail": "Сбой %",
  "table.rtt": "p95 RTT",
  "table.status": "Статус",
  "table.status.failing": "Сбой",
  "table.status.degraded": "Деградация",
  "table.investigate": "расследовать",

  "alerts.title": "Активные оповещения",
  "alerts.open": "открыть Оповещения",
  "alerts.engaged":
    "Состояние оповещений говорит только про сейчас: истории срабатываний Prometheus здесь не держит.",
  "alerts.denied": "Активным оповещениям нужно право alerts:read. Его нет, поэтому запрос не отправлялся.",
  "alerts.error": "Набор активных оповещений сейчас недоступен.",
  "alerts.noPrometheus":
    "Prometheus для этой консоли не настроен, задайте console.prometheus.address. Пока что состояние срабатываний брать неоткуда.",
  "alerts.empty": "Ни одно правило этой консоли не срабатывает. Правила живут на /alerting, вычисляет их Prometheus.",
  "alerts.hidden.one": "Ещё активных оповещений не показано: {count}.",
  "alerts.hidden.many": "Ещё активных оповещений не показано: {count}.",
  "alerts.noSeverity": "без severity",
  "alerts.managedOnly":
    "В карточке только правила, которыми управляет эта консоль. Всё остальное, что горит в кластере, смотрят в " +
    "Alertmanager или Grafana, а не здесь.",
  "alerts.investigate": "расследовать",

  "incidents.title": "Открытые инциденты",
  "incidents.denied": "Открытым инцидентам нужно право incidents:read, которого у роли нет, так что запрос не отправлялся.",
  "incidents.error": "Список инцидентов сейчас недоступен.",
  "incidents.empty": "Открытых инцидентов нет. Сохраните расследование на /investigate, и инцидент откроется.",
  "incidents.scope.global": "глобальный",

  "events.title": "Последние события",
  "events.open": "открыть Онлайн",
  "events.denied": "Событиям флота нужно право events:read. Его нет, и запрос не отправлялся.",
  "events.error": "Лента событий сейчас недоступна.",
  "events.empty":
    "Пока ничего не происходило. Рестарты агентов, смена готовности узлов и запуски MTR попадают сюда.",
  "events.empty.engaged":
    "На просматриваемый момент и раньше событий нет. Вернитесь в реальное время или выберите другой момент.",

  "severity.info": "Инфо",
  "severity.warn": "Внимание",
  "severity.error": "Ошибка",

  "age.seconds": "{count} с",
  "age.minutes": "{count} м",
  "age.hours": "{count} ч",
  "age.days": "{count} д",

  "db.note": "Истории нужна база, задайте console.database.mode. Запрос не отправлялся.",
});
