import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * diagnostics — pages/diagnostics.tsx: the run form, "Save as definition", and
 * the run history list underneath them.
 *
 * NodeSelector, FieldLabel and the run form's destination vocabulary live in
 * that file and are imported by pages/mtr.tsx's Runner, so THIS dictionary is
 * what a node picker says on BOTH pages. pages/mtr.tsx keeps its own copy of
 * the labels it renders itself (Duration, Destination, Sources, …) rather than
 * reaching in here — two surfaces, two files, per lib/i18n/README.md.
 *
 * NOT HERE, on purpose:
 *   - the check types (TCP, UDP, ICMP, DNS, HTTP, MTR) and the plane (`pod`):
 *     protocol names and a wire enum.
 *   - every run STATUS badge and `r.type`: the store's own enum.
 *   - run ids, node names, target names, the ad-hoc address, and the address
 *     placeholder ("10.0.0.1 or https://example.test/health") — a literal an
 *     operator copies, not prose.
 *   - `console.database.mode`, `runs:create`, `GET /api/v1/runs`: config keys,
 *     permission strings, endpoints.
 *   - every problem+json detail from a rejected run or definition, including
 *     ADHOC_ADDRESS_ERROR (lib/utils.ts) and the phrase table in
 *     pages/targets.tsx that decides which field a 422 lands on.
 *   - the duration tokens 1m … 24h. Only "Instant" is a word.
 */

const en = {
  "title": "Run checks",
  /* Joins the two address examples in the ad-hoc destination placeholder. */
  "adhoc.or": "or",
  "description": "Run on-demand checks against the mesh, and browse run history.",
  "description.at":
    "Run on-demand checks against the mesh. History is cut to {at} — runs started later are not listed.",

  /* ── the node pickers (shared with /mtr's Runner) ──────────────────────── */
  "nodes.all": "All nodes ({count})",
  "nodes.empty": "No nodes reported by the controller yet.",

  /* ── the run form ──────────────────────────────────────────────────────── */
  "form.checkType": "Check type",
  "form.checkType.aria": "Check type",
  "form.duration": "Duration",
  "form.duration.aria": "Duration",
  "duration.instant": "Instant",
  /* The two duration captions. The instant one is the default; the other
     spells out the fan-out BEFORE Run is pressed rather than leaving it to be
     discovered afterwards. {interval}, {label} and {samples} are computed by
     the page from the server's own cadence rule. */
  "duration.caption.instant": "One probe per pair, right now.",
  "duration.caption.interval":
    "Each pair is probed every {interval} for {label} — about {samples} samples per pair. " +
    "The run stays running, and cancellable, until it finishes.",
  /* mtr plans its own cadence: a trace walks up to 30 hops in sequence, so the base 5s cadence
     cannot hold one and the server stretches the interval instead of refusing the run. */
  "duration.caption.interval.mtr":
    "An MTR trace takes up to {budget} per pair, so a {label} run traces every {interval} — " +
    "about {samples} traces per pair. The run stays running, and cancellable, until it finishes.",

  /* ── the cadence control ───────────────────────────────────────────────────
     "Auto" posts nothing and is exactly the behaviour that existed before this
     control did; the presets above it are bounded by the duration already
     picked, because the server refuses a cadence longer than the run. */
  "form.sampleInterval": "Sample interval",
  "form.sampleInterval.aria": "Sample interval",
  "sampleInterval.auto": "Auto",
  /* The two sentences a picked cadence may earn. Neither replaces the caption
     above — they LEAD it, because an operator who has just clicked 1s needs to
     learn it will not be 1s before being told what it will be. {requested} is
     what they picked; {interval} is what the run will keep. */
  "duration.caption.adjusted.cap":
    "Every {requested} would be more than 500 samples for one pair, which is the ceiling, so this run cannot go faster than every {interval}.",
  "duration.caption.adjusted.round":
    "Every {requested} is faster than one round over this many pairs can finish, so this run cannot go faster than every {interval}.",

  "form.plane": "Plane",
  "form.destination": "Destination",
  "form.destination.aria": "Destination",
  "destination.kind.node": "Nodes",
  "destination.kind.target": "Target",
  "destination.kind.adhoc": "Ad-hoc",

  "form.sources": "Sources",
  "form.destinations": "Destinations",
  "form.destinationTarget": "Destination target",
  "form.destinationTarget.placeholder": "— pick a target —",
  "form.destinationAddress": "Destination address",
  /* ── the external destination, per check type (QA scope 4, finding #10) ───
     One label and one placeholder used to serve all six types. These follow
     what internal/agent/tasks.go actually does with the string: tcp defaults a
     missing port to 80, udp has no default (0 is dialled), icmp and mtr have
     no ports at all, and dns and http are not in externalCapableChecks. The
     port numbers and the type names are syntax and stand in both languages. */
  "adhoc.label.hostPort": "Destination host (port optional)",
  "adhoc.label.hostPortRequired": "Destination host:port",
  "adhoc.label.hostOnly": "Destination host",
  "adhoc.label.unsupported": "Destination address",
  "adhoc.hint.hostPort": "A host, an IP, or host:port. Without a port the agent dials 80.",
  "adhoc.hint.hostPortRequired": "A host or IP with a port — udp has no default, so an address without one dials nothing.",
  "adhoc.hint.hostOnly": "A host or an IP. There are no ports here; one written in is ignored.",
  "adhoc.hint.unsupported":
    "A one-off RUN cannot probe an external destination with this check type. Saving it as a definition below can — " +
    "that is the continuous external checker, which does speak dns and http.",
  "adhoc.mismatch.unsupported":
    "Start run would be refused: an agent answers a one-off external probe for tcp, udp, icmp and mtr only. " +
    "Pick one of those, send the run at nodes, or save this as a definition instead.",
  "adhoc.mismatch.url":
    "This check type resolves and dials the string itself, so a scheme and a path are never read — drop them and " +
    "leave the host, with a port if you need one.",
  "adhoc.mismatch.port": "udp has no default port, so this address needs one written in: host:port.",

  /* A RESET, and it says so (QA round 4, #15): the glyph alone read as "swap
     the two columns" or "run every pair". */
  "form.resetPickers": "Reset both pickers to every node",
  "form.resetPickers.label": "All ↔ All",

  /* "~" flags an estimate — the server is the only real arbiter. Three forms
     for the Russian count; English's `.few` carries its plural. */
  "pairs.one": "~{count} pair",
  "pairs.few": "~{count} pairs",
  "pairs.many": "~{count} pairs",
  /* Appended to the count when the RAW S×D product is over the server's limit.
     It names both numbers because the self-excluded estimate can read as under
     the limit while the product the server gates on is over it. */
  "pairs.overLimit":
    " — above the {max}-pair limit (server enforces the raw {sources}×{destinations} limit of {max}), " +
    "narrow the selection",
  /* Appended when the estimate is ZERO. "~0 pairs" on its own is a dead button
     with no explanation; /mtr's Runner has always named the reason, and these
     are the same sentences for the four ways this form can reach zero. Each
     one is something the operator can act on. */
  "pairs.noSources": " — no sources to check from, so there is nothing to run",
  "pairs.noDestinations": " — no destinations picked, so there is nothing to check against",
  /* The one-node cluster, and the reason this key exists: with both pickers on
     All and the one node there is sitting in both of them, "no destinations
     picked" was the single thing the operator could SEE was untrue. A run's
     pairs are the cross product minus the self-pairs, and on a cluster of one
     that leaves nothing — a fact about the fleet, not about the form. */
  "pairs.selfOnly":
    " — the same nodes are on both sides and a node never probes itself, so no pair is left; " +
    "add a node, or send the run at a target or an ad-hoc address",
  "pairs.noTarget": " — no target picked yet, so there is no destination to check against",
  "pairs.noAddress": " — no address typed yet, so there is no destination to check against",

  "form.submit": "Start run",
  "form.submitFailed": "Failed to start run",

  /* ── save as definition ────────────────────────────────────────────────── */
  "definition.name": "Definition name",
  "definition.save": "Save as definition",
  "definition.hint":
    "Saved enabled and probing from all agents — a definition has no per-node source list. Edit it on Scheduled " +
    "checks.",
  "definition.nameRequired": "a definition needs a name",
  "definition.saveFailed": "Failed to save the definition",
  "definition.saved": "Saved definition “{name}”",

  /* ── run history ───────────────────────────────────────────────────────── */
  "history.title": "Run history",
  "history.notPersisted": "History is not persisted — set console.database.mode",
  /* The bound on the Time Machine's cut, stated rather than implied: GET
     /api/v1/runs has no `to` parameter, so the cut happens in the browser over
     the pages already loaded. */
  "history.atNote":
    "GET /api/v1/runs has no time filter, so this cut to the viewed instant happens in the browser over the " +
    "pages loaded here — a run older than them is not reached by paging backwards from this list.",
  "history.unavailable": "Run history is unavailable",
  /* The two history filters (QA scope 4, finding #11). They ride the server's
     own ?type=&status=, so the options are the store's enum and stand
     untranslated — only the "everything" row and the control names are words. */
  "history.filter.type.aria": "Filter runs by check type",
  "history.filter.type.all": "All types",
  "history.filter.status.aria": "Filter runs by status",
  "history.filter.status.all": "All statuses",
  "history.empty.title": "No runs yet",
  "history.empty.body": "Runs started from the form above (or by another operator) show up here.",
  /* A filter that matched nothing is not an empty history, and the form above
     is not the remedy — the server was asked for this slice and answered with
     none. */
  "history.emptyFiltered.title": "No runs match these filters",
  "history.emptyFiltered.body": "The server was asked for this type and status and has none. Widen the filters above.",
  /* Engaged with everything filtered out is a DIFFERENT fact from "nobody has
     ever run one", and the form above is disabled anyway. */
  "history.emptyAt.title": "No runs at or before the viewed instant",
  "history.emptyAt.body": "Every run on the loaded page started later than this. Return to Live, or load older pages.",
  "history.run.okOfTotal": "{ok}/{total} ok",
  /* ui/pager.tsx's noun for this list — "Showing 50 of 214 runs". */
  "history.subject": "runs",
  "history.loadOlder": "Load older",
  "history.loadingOlder": "Loading older…",

  /* ── no runs:create ────────────────────────────────────────────────────── */
  "gate.title": "Starting a run requires the runs:create permission",
  "gate.body": "You can still see run history below. Ask an operator to start a new run.",
} as const;

export type DiagnosticsKey = keyof typeof en;

export const diagnosticsDict: Dictionary<DiagnosticsKey> = defineDict(en, {
  "adhoc.or": "или",
  "title": "Проверки вручную",
  "description": "Запуск проверок по требованию и история запусков.",
  "description.at":
    "Запуск проверок по требованию. История обрезана по {at}, запуски, начатые позже, сюда не попадут.",

  "nodes.all": "Все узлы ({count})",
  "nodes.empty": "Контроллер пока не сообщил ни одного узла.",

  "form.checkType": "Тип проверки",
  "form.checkType.aria": "Тип проверки",
  "form.duration": "Длительность",
  "form.duration.aria": "Длительность",
  "duration.instant": "Мгновенно",
  "duration.caption.instant": "По одному зонду на пару, прямо сейчас.",
  /* «раз в {interval}» получает слово целиком, а не сокращение: «раз в 5 с» глаз читает как
     предлог «с» и спотыкается ровно там, где надо назвать период. Формы даёт
     formatCadenceProse (lib/run-samples.ts). */
  "duration.caption.interval":
    "Каждая пара зондируется раз в {interval} на протяжении {label}, это примерно {samples} проб на пару. " +
    "Запуск идёт и остаётся отменяемым, пока не закончится.",
  "duration.caption.interval.mtr":
    "Трассировка MTR занимает до {budget} на пару, поэтому за {label} каждая пара трассируется " +
    "раз в {interval}, это примерно {samples} трассировок на пару. " +
    "Запуск идёт и остаётся отменяемым, пока не закончится.",

  "form.sampleInterval": "Период опроса",
  "form.sampleInterval.aria": "Период опроса",
  "sampleInterval.auto": "Авто",
  "duration.caption.adjusted.cap":
    "Раз в {requested} — это больше 500 проб на пару, а это потолок, так что чаще чем раз в {interval} этот запуск не пойдёт.",
  "duration.caption.adjusted.round":
    "Раз в {requested} — быстрее, чем успевает пройти один круг по такому числу пар, так что чаще чем раз в {interval} этот запуск не пойдёт.",

  "form.plane": "Плоскость",
  "form.destination": "Назначение",
  "form.destination.aria": "Назначение",
  "destination.kind.node": "Узлы",
  "destination.kind.target": "Цель",
  "destination.kind.adhoc": "Произвольный",

  "form.sources": "Источники",
  "form.destinations": "Назначения",
  "form.destinationTarget": "Целевой объект",
  "form.destinationTarget.placeholder": "выберите цель…",
  "form.destinationAddress": "Адрес назначения",
  "adhoc.label.hostPort": "Хост назначения (порт необязателен)",
  "adhoc.label.hostPortRequired": "Хост назначения и порт",
  "adhoc.label.hostOnly": "Хост назначения",
  "adhoc.label.unsupported": "Адрес назначения",
  "adhoc.hint.hostPort": "Хост, IP или host:port. Без порта агент пойдёт на 80.",
  "adhoc.hint.hostPortRequired": "Хост или IP с портом: у udp дефолтного порта нет, без него стучаться некуда.",
  "adhoc.hint.hostOnly": "Хост или IP. Портов тут нет, написанный порт просто проигнорируют.",
  "adhoc.hint.unsupported":
    "Разовый запуск по внешнему адресу с таким типом проверки не пойдёт. А вот определением ниже сохранить можно: " +
    "постоянные внешние проверки dns и http умеют.",
  "adhoc.mismatch.unsupported":
    "«Запустить» откажет: разовую внешнюю проверку агент делает только для tcp, udp, icmp и mtr. " +
    "Возьмите один из них, отправьте запуск по узлам или сохраните определение.",
  "adhoc.mismatch.url":
    "Этот тип проверки резолвит и подключается ровно по строке, схему и путь никто не читает: уберите их, оставьте " +
    "хост и при необходимости порт.",
  "adhoc.mismatch.port": "У udp дефолтного порта нет, поэтому его надо дописать: host:port.",

  "form.resetPickers": "Сбросить оба списка на все узлы",
  "form.resetPickers.label": "Все ↔ все",

  "pairs.one": "~{count} пара",
  "pairs.few": "~{count} пары",
  "pairs.many": "~{count} пар",
  "pairs.overLimit":
    ", это больше лимита в {max} пар (сервер сверяет с лимитом {max} сырое произведение " +
    "{sources}×{destinations}), сузьте выбор",
  "pairs.noSources": ", и проверять не с чего: источников нет",
  "pairs.noDestinations": ", и проверять нечего: назначения не выбраны",
  "pairs.selfOnly":
    ", и пар не остаётся: с обеих сторон одни и те же узлы, а сам себя узел не проверяет. " +
    "Добавьте узел или отправьте запуск в цель либо на произвольный адрес",
  "pairs.noTarget": ", и проверять некуда: цель ещё не выбрана",
  "pairs.noAddress": ", и проверять некуда: адрес ещё не введён",

  "form.submit": "Запустить",
  "form.submitFailed": "Не удалось запустить",

  "definition.name": "Имя определения",
  "definition.save": "Сохранить как определение",
  "definition.hint":
    "Сохраняется включённым и зондирует со всех агентов: списка узлов-источников у определения нет. " +
    "Править его на странице «Плановые проверки».",
  "definition.nameRequired": "определению нужно имя",
  "definition.saveFailed": "Не удалось сохранить определение",
  "definition.saved": "Определение «{name}» сохранено",

  "history.title": "История запусков",
  "history.notPersisted": "История не сохраняется, задайте console.database.mode",
  "history.atNote":
    "У GET /api/v1/runs нет фильтра по времени, поэтому обрезка по выбранному моменту делается в браузере, по " +
    "уже загруженным страницам. Запуск старше них листанием назад отсюда не достать.",
  "history.unavailable": "История запусков недоступна",
  "history.filter.type.aria": "Фильтр запусков по типу проверки",
  "history.filter.type.all": "Все типы",
  "history.filter.status.aria": "Фильтр запусков по статусу",
  "history.filter.status.all": "Все статусы",
  "history.empty.title": "Запусков пока нет",
  "history.empty.body": "Запуски из формы выше (или запущенные другим оператором) появятся здесь.",
  "history.emptyFiltered.title": "Под эти фильтры ничего не подходит",
  "history.emptyFiltered.body": "Сервер спросили про этот тип и статус, у него таких нет. Ослабьте фильтры выше.",
  "history.emptyAt.title": "Запусков на выбранный момент и раньше нет",
  "history.emptyAt.body":
    "Все запуски на загруженной странице начались позже. Вернитесь в реальное время или подгрузите старые страницы.",
  "history.run.okOfTotal": "{ok}/{total} успешно",
  "history.subject": "Запуски",
  "history.loadOlder": "Загрузить старые",
  "history.loadingOlder": "Загружаем старые…",

  "gate.title": "Для запуска нужно право runs:create",
  "gate.body": "История запусков ниже вам всё равно доступна. Попросите оператора запустить проверку.",
});

/**
 * countForm picks between the `.one` / `.few` / `.many` pair-count keys.
 *
 * No plural MACHINERY (lib/i18n's module doc says why the module ships none) —
 * this is the README's "declare the forms as keys and pick between them"
 * pattern, with the rule per language because the rules genuinely differ:
 * English has two forms and 21 must read "~21 pairs", Russian has three and 21
 * must read «~21 пара».
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
