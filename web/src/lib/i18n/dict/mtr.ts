import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * mtr — pages/mtr.tsx: the three-pane path explorer (destinations, the pair's
 * distinct routes, one route's hops or a diff of two) and the Runner tab.
 *
 * The Runner reuses pages/diagnostics.tsx's NodeSelector and FieldLabel, so
 * the node pickers' own two strings ("All nodes (n)", "No nodes reported…")
 * come from dict/diagnostics.ts on this page too. Everything the Runner
 * renders ITSELF is here, including its own copy of the three destination
 * kinds — two surfaces, two files, per lib/i18n/README.md.
 *
 * NOT HERE, on purpose:
 *   - "MTR" itself, and every protocol name. The nav calls the page MTR in
 *     both languages (dict/chrome.ts) and so does this one.
 *   - node names, destination names, target names, path hashes (`shortHash`),
 *     hop addresses, and the ad-hoc address placeholder.
 *   - permission strings (`mtr:read`, `runs:create`, `targets:read`), config
 *     keys (`console.database.mode`) and endpoints (`POST /api/v1/runs`).
 *   - every problem+json detail a failed read or a refused run carries.
 *   - the duration tokens 1m … 24h; only "Instant" is a word, and it lives in
 *     dict/diagnostics.ts because RUN_DURATIONS does.
 *   - the trace-detail table and the per-hop trend: they are
 *     components/mtr-hop-table.tsx, a shared component with its own owner.
 */

const en = {
  "title": "MTR Explorer",
  "description": "Every distinct route the fleet's traces have taken, and when each one changed.",
  /* Two descriptions, same shape as /diagnostics and /explore: engaged, the
     subtitle must not keep promising a view of the viewed instant. {at} is the
     page's own localeTag stamp. */
  "description.at":
    "Every distinct route the fleet's traces have taken. The Explorer below is NOT cut to {at} — it is live.",
  /* The verbatim disclosure, /diagnostics's history.atNote treatment: name the
     endpoints and say what the reader is actually looking at, rather than
     letting the banner above imply a cut that never happened. */
  "explorer.atNote":
    "GET /api/v1/mtr/destinations and the path-history reads behind these three panes take no time parameter, so the " +
    "routes below are the ones recorded NOW — including any traced after the viewed instant.",

  "loading": "Loading…",

  /* ── the two degraded states ───────────────────────────────────────────── */
  "permission.title": "Requires the {permission} permission",
  /* mtr:read is held by every BUILT-IN role, viewer included — which is what
     an anonymous session gets. Reaching this card means a hand-rolled role,
     and the copy says exactly that so the reader looks in the right place. */
  "permission.body":
    "Path history is telemetry, and every built-in role holds this permission — viewer included, which is the role an " +
    "anonymous session gets. Seeing this card means the role in use was defined by hand without it; ask an admin to " +
    "add mtr:read to it.",
  "database.gate": "Path history is projected into the database — set console.database.mode",

  "view.aria": "View",
  "view.explorer": "Explorer",
  "view.runner": "Runner",

  /* ── pane 1: destinations ──────────────────────────────────────────────── */
  "destinations.title": "Destinations",
  "destinations.error": "Path history is unavailable",
  /* Three keys because a link sits in the middle of the sentence — the one
     case interpolation cannot cover. Path history is a PROJECTION of MTR
     results the console ingested, so "nothing here" means "nobody has run
     one", and the place to run one is Diagnostics. */
  "destinations.empty.before": "Nothing traced yet.",
  "destinations.empty.link": "Run an MTR from Diagnostics",
  /* ui/pager.tsx's nouns for the paged lists on this page. The sources one is
     the list INSIDE a card, which pages on its own once a destination has been
     traced from more than a screenful of nodes. */
  "destinations.subject": "destinations",
  "destinations.sources.subject": "sources",
  "destinations.empty.after": "— its path lands here.",
  "destinations.from": "from {node}",
  /* Both figures, in one string, so the separator between them is TEXT and not
     a CSS gap. A shut card and a source row make the same claim in the same
     shape: how many distinct routes, over how many traces. */
  "destinations.counts": "{paths} · {traces}",
  /* The card's SPOKEN name. Punctuated, because a name computed from the card's
     own two spans arrives as one welded token and a decorative separator cannot
     mend it — aria-hidden text is excluded from the name. */
  "destinations.card.aria": "{destination}: {paths}, {traces}",
  "paths.one": "{count} path",
  "paths.few": "{count} paths",
  "paths.many": "{count} paths",
  "traces.one": "{count} trace",
  "traces.few": "{count} traces",
  "traces.many": "{count} traces",

  /* ── pane 2: the pair's routes ─────────────────────────────────────────── */
  "history.title": "Path history",
  /* Deliberately says no "on the left" (QA round 4, #20): under ~700px the
     three panes stack and the destinations pane is ABOVE this one. */
  "history.noPair": "Pick a source to see its path history.",
  "history.empty": "No path recorded for this pair yet.",
  /* A shared "Open in MTR Explorer" link for a pair path history has never seen.
     The generic list said nothing about the pair that was asked for. */
  "history.linkEmpty": "No path recorded yet for {source} → {destination}.",
  /* The button that OPENS the comparison; {count} is how many of the two are ticked. */
  "history.compareOpen": "Compare ({count}/2)",
  "history.compareHint": "Tick two paths to diff them — a third pick replaces the earlier of the two.",
  "history.list.aria": "Paths",
  "history.subject": "paths",
  "history.compare.aria": "Compare path {hash}",
  "history.path.aria": "Path {hash}",
  /* The badge used to say only this, beside two hashes — that something moved,
     and nothing about what. It survives as the fallback for a first path with
     nothing to compare against; the four below are the answer. */
  "history.changed": "path changed",
  "history.changed.moved": "hop {hop}: {from} → {to}",
  "history.changed.added": "hop {hop} added: {to}",
  "history.changed.removed": "hop {hop} gone: {from}",
  "history.changed.several": "{count} hops changed",
  "history.span": "{from} → {to} · {traces}",
  "history.loadOlder": "Load older",
  "history.loadingOlder": "Loading older…",
  /* The END of the list, said out loud. A permanently disabled "Load older" reads as a broken
     button, not as "there is nothing older" — and the count is what answers the question the
     reader actually has: the sidebar counts TRACES, this list shows distinct ROUTES, and a pair
     with hundreds of traces can honestly have six of them. */
  "history.allShown": "{paths} · {traces} · nothing older is retained",
  "hops.one": "{count} hop",
  "hops.few": "{count} hops",
  "hops.many": "{count} hops",

  /* ── pane 3: the trace, or the diff ────────────────────────────────────── */
  "detail.title": "Trace detail",
  "detail.empty": "Pick a path in the history to see its hops.",
  "detail.error": "This path is unavailable",
  "diff.title": "Path diff",
  "diff.empty": "Both paths must still be loaded to diff them.",
  /* The one input the alignment has nothing to say about. Drawing an empty
     table under a legend answers the reader with blank space. */
  "diff.noHops": "Neither path recorded a single hop, so there is nothing to line up.",

  /* ── the Runner ────────────────────────────────────────────────────────── */
  "runner.aria": "Run a trace",
  "runner.title": "Run an MTR",
  "runner.body":
    "The same POST /api/v1/runs the Diagnostics page uses, with the check type fixed to mtr. Every path it " +
    "produces lands in this page's history.",

  "runner.duration": "Duration",
  "runner.duration.aria": "Duration",
  /* The Instant OPTION. Its siblings (1m … 24h) are durations and stay; this
     surface keeps its own copy of the one word rather than reading
     dict/diagnostics.ts, which is where the same option lives for that form. */
  "runner.duration.instantLabel": "Instant",
  "runner.duration.instant": "One trace per pair, right now.",
  /* ── the duration caption ──────────────────────────────────────────────────
     These four keys sit under one prefix because pages/diagnostics.tsx's
     cadenceCaption builds all of them, from ONE mirror of the server's planner.
     They replaced a single "runner.duration.interval" that quoted the BASE
     cadence — duration/500, floored at 5s — for the one check type that cannot
     keep it: a 5m run over ten pairs advertised «раз в 5 с» here while the run
     permalink said 3m and the fleet did neither.

     `.interval` can only be reached by a non-mtr type, which this pane has
     none of; it exists so the shared builder has the branch it expects, and so
     that nailing the pane to a different type later cannot silently produce a
     missing key. */
  "runner.duration.caption.interval":
    "Each pair is re-probed every {interval} for {label}, about {samples} samples per pair — cancellable throughout.",
  /* An interval MTR is the most direct way to catch a route that flaps — the
     one thing a single instant trace cannot see. A trace walks up to 30 hops in
     sequence, so the cadence is one ROUND, not the base 5s. */
  "runner.duration.caption.interval.mtr":
    "An MTR trace takes up to {budget} per pair, so a {label} run re-traces every pair every {interval} — " +
    "about {samples} traces per pair, cancellable throughout.",
  "runner.duration.caption.adjusted.cap":
    "Every {requested} would be more than 500 traces for one pair, which is the ceiling, so this run cannot go faster than every {interval}.",
  "runner.duration.caption.adjusted.round":
    "Every {requested} is faster than one round of traces over this many pairs can finish, so this run cannot go faster than every {interval}.",

  "runner.sampleInterval": "Trace interval",
  "runner.sampleInterval.aria": "Trace interval",
  "runner.sampleInterval.auto": "Auto",

  "runner.destination": "Destination",
  "runner.destination.aria": "Destination",
  "runner.kind.node": "Nodes",
  "runner.kind.target": "Target",
  "runner.kind.adhoc": "Ad-hoc",

  "runner.sources": "Sources",
  "runner.destinations": "Destinations",
  "runner.destinationTarget": "Destination target",
  "runner.destinationTarget.placeholder": "— pick a target —",
  "runner.destinationAddress": "Destination address",

  "runner.pairs.one": "~{count} pair",
  "runner.pairs.few": "~{count} pairs",
  "runner.pairs.many": "~{count} pairs",
  /* A dead button owes an explanation, and each of these four is one the
     operator can act on: wire a controller, wait for an agent to register,
     pick a target, type an address. */
  "runner.noPairs": " — no sources to trace from, so there is nothing to run",
  "runner.noDestinations": " — no destinations picked, so there is nothing to trace to",
  "runner.noTarget": " — no target picked yet, so there is no destination to trace to",
  "runner.noAddress": " — no address typed yet, so there is no destination to trace to",

  "runner.submit": "Start MTR",
  "runner.submitFailed": "Failed to start the trace",
  /* Three keys, link in the middle again. The Runner does NOT navigate on 202:
     an operator who launched a trace from here is in the middle of reading a
     pair's history, so the run is offered as a link and the explorer stays. */
  "runner.started.before": "Run started —",
  "runner.started.link": "watch it here",
  "runner.started.after": ". Its path lands in the history on the Explorer tab once the run finishes.",
} as const;

export type MTRKey = keyof typeof en;

export const mtrDict: Dictionary<MTRKey> = defineDict(en, {
  "title": "MTR Explorer",
  "description": "Все различные маршруты, по которым ходили трассировки флота, и когда каждый из них менялся.",
  "description.at":
    "Все различные маршруты, по которым ходили трассировки флота. Обзор ниже не обрезан по {at} — он живой.",
  "explorer.atNote":
    "У GET /api/v1/mtr/destinations и у чтений истории путей за этими тремя панелями нет параметра времени, поэтому " +
    "ниже показаны маршруты, записанные на сейчас, включая те, что трассировали позже выбранного момента.",

  "loading": "Загрузка…",

  "permission.title": "Нужно право {permission}",
  "permission.body":
    "История путей относится к телеметрии, и право на неё есть у любой встроенной роли, включая viewer, который " +
    "достаётся анонимной сессии. Раз вы видите эту карточку, роль собрали руками и права в ней не оказалось. " +
    "Попросите админа добавить mtr:read.",
  "database.gate": "История путей проецируется в базу, задайте console.database.mode",

  "view.aria": "Вид",
  "view.explorer": "Обзор",
  "view.runner": "Запуск",

  "destinations.title": "Назначения",
  "destinations.error": "История путей недоступна",
  "destinations.empty.before": "Пока ничего не трассировали.",
  "destinations.empty.link": "Запустите MTR из Диагностики",
  "destinations.subject": "Назначения",
  "destinations.sources.subject": "Источники",
  "destinations.empty.after": "(путь появится здесь).",
  "destinations.from": "от {node}",
  "destinations.counts": "{paths} · {traces}",
  "destinations.card.aria": "{destination}: {paths}, {traces}",
  "paths.one": "{count} путь",
  "paths.few": "{count} пути",
  "paths.many": "{count} путей",
  "traces.one": "{count} трассировка",
  "traces.few": "{count} трассировки",
  "traces.many": "{count} трассировок",

  "history.title": "История путей",
  "history.noPair": "Выберите источник, чтобы увидеть историю его путей.",
  "history.empty": "Для этой пары путей пока не записано.",
  "history.linkEmpty": "Для пары {source} → {destination} путей пока не записано.",
  "history.compareHint": "Отметьте два пути, чтобы сравнить. Третья отметка вытеснит более раннюю из двух.",
  "history.compareOpen": "Сравнить ({count}/2)",
  "history.list.aria": "Пути",
  "history.subject": "Пути",
  "history.compare.aria": "Сравнить путь {hash}",
  "history.path.aria": "Путь {hash}",
  "history.changed": "путь изменился",
  "history.changed.moved": "хоп {hop}: {from} → {to}",
  "history.changed.added": "добавлен хоп {hop}: {to}",
  "history.changed.removed": "пропал хоп {hop}: {from}",
  "history.changed.several": "изменилось хопов: {count}",
  "history.span": "{from} → {to} · {traces}",
  "history.loadOlder": "Загрузить старые",
  "history.loadingOlder": "Загружаем старые…",
  "history.allShown": "{paths} · {traces} · старее ничего не хранится",
  "hops.one": "{count} хоп",
  "hops.few": "{count} хопа",
  "hops.many": "{count} хопов",

  "detail.title": "Детали трассировки",
  "detail.empty": "Выберите путь в истории, чтобы увидеть его хопы.",
  "detail.error": "Этот путь недоступен",
  "diff.title": "Разница путей",
  "diff.empty": "Чтобы сравнить, оба пути должны быть загружены.",
  "diff.noHops": "Ни в одном из путей не записано ни одного хопа, сопоставлять нечего.",

  "runner.aria": "Запустить трассировку",
  "runner.title": "Запустить MTR",
  "runner.body":
    "Тот же POST /api/v1/runs, что и на странице диагностики, только тип проверки жёстко задан как mtr. Каждый " +
    "полученный путь попадает в историю этой страницы.",

  "runner.duration": "Длительность",
  "runner.duration.aria": "Длительность",
  "runner.duration.instantLabel": "Мгновенно",
  "runner.duration.instant": "По одной трассировке на пару, прямо сейчас.",
  /* «раз в {interval}» — период словом, а не сокращением: «раз в 5 с» читается как предлог «с»
     и ломает фразу ровно на том слове, ради которого она написана. */
  "runner.duration.caption.interval":
    "Каждая пара зондируется заново раз в {interval} на протяжении {label}, это примерно {samples} проб " +
    "на пару. Отменить запуск можно в любой момент.",
  "runner.duration.caption.interval.mtr":
    "Трассировка MTR занимает до {budget} на пару, поэтому за {label} каждая пара трассируется заново " +
    "раз в {interval}, это примерно {samples} трассировок на пару. Отменить запуск можно в любой момент.",
  "runner.duration.caption.adjusted.cap":
    "Раз в {requested} — это больше 500 трассировок на пару, а это потолок, так что чаще чем раз в {interval} этот запуск не пойдёт.",
  "runner.duration.caption.adjusted.round":
    "Раз в {requested} — быстрее, чем успевает пройти круг трассировок по такому числу пар, так что чаще чем раз в {interval} этот запуск не пойдёт.",

  "runner.sampleInterval": "Период трассировки",
  "runner.sampleInterval.aria": "Период трассировки",
  "runner.sampleInterval.auto": "Авто",

  "runner.destination": "Назначение",
  "runner.destination.aria": "Назначение",
  "runner.kind.node": "Узлы",
  "runner.kind.target": "Цель",
  "runner.kind.adhoc": "Произвольный",

  "runner.sources": "Источники",
  "runner.destinations": "Назначения",
  "runner.destinationTarget": "Целевой объект",
  "runner.destinationTarget.placeholder": "выберите цель…",
  "runner.destinationAddress": "Адрес назначения",

  "runner.pairs.one": "~{count} пара",
  "runner.pairs.few": "~{count} пары",
  "runner.pairs.many": "~{count} пар",
  "runner.noPairs": ", и трассировать не с чего: источников нет",
  "runner.noDestinations": ", и трассировать некуда: назначения не выбраны",
  "runner.noTarget": ", и трассировать некуда: цель ещё не выбрана",
  "runner.noAddress": ", и трассировать некуда: адрес ещё не введён",

  "runner.submit": "Запустить MTR",
  "runner.submitFailed": "Не удалось запустить трассировку",
  "runner.started.before": "Запуск начат,",
  "runner.started.link": "посмотреть можно здесь",
  "runner.started.after": ". Его путь попадёт в историю на вкладке «Обзор», когда запуск закончится.",
});

/**
 * countForm picks between the `.one` / `.few` / `.many` keys above (paths,
 * traces, hops, pairs).
 *
 * No plural MACHINERY — lib/i18n ships none on purpose. This is the README's
 * "declare the forms as keys and pick between them" pattern, with the rule per
 * language because the rules genuinely differ: English needs two forms and 21
 * must read "21 paths", Russian needs three and 21 must read «21 путь».
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
