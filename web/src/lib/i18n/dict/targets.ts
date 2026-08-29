import { DEFAULT_LOCALE, type Locale, defineDict, type Dictionary } from "@/lib/i18n";

/**
 * targets — pages/targets.tsx: the external probe targets, the check
 * definitions that point at them, and the schedules that fire those
 * definitions.
 *
 * ── what stayed English, and why ──────────────────────────────────────────
 * This page is mostly a set of forms over CONFIGURATION, so the line between
 * a label and a value runs right through the middle of it:
 *
 *   - Every SELECT's options are wire values. `host`/`url`, `node`/`target`/
 *     `adhoc`, `all`/`per-zone`/`one-per-zone`, `once`/`interval`/`continuous`
 *     and the check types render through plainOptions(), which uses the value
 *     AS the label. Translating those would mean the box shows «узел» and the
 *     API stores "node" — a control lying about what it is about to write.
 *   - Field names quoted inside a sentence stay too: `sourceSelection`,
 *     `one-per-zone`, `console.database.mode`, `GET /api/v1/runs`. An operator
 *     types those into a config file next, not into a conversation.
 *   - Sample values in placeholders — "edge-gateway", "10.0.0.1",
 *     "env=prod, tier=edge", '{"port": 443}' — are examples of DATA. Only the
 *     English connective in the address placeholder ("or") is translated.
 *   - Badge words are PRESENTATION and do translate: a definition or schedule
 *     reads «включено»/«выключено», never `enabled`, because that pill is this
 *     page describing a boolean rather than echoing a field.
 *
 * ── plurals ───────────────────────────────────────────────────────────────
 * The projection line is the one place a count lands inside a sentence, and
 * Russian needs three forms where English needs one and a half. There is no
 * plural machinery in lib/i18n on purpose, so the forms are separate keys and
 * the page picks between them (see `pluralKey` in the page). English fills all
 * three slots with the word it would have used anyway, which is why the
 * rendered English is byte-for-byte what it was.
 */

const en = {
  /* ── page chrome ───────────────────────────────────────────────────────── */
  "title": "Scheduled checks",
  "description": "External probe targets, the check definitions that point at them, and their schedules.",
  "tabs.aria": "Section",
  "tab.targets": "Targets",
  "tab.definitions": "Definitions",
  "tab.schedules": "Schedules",
  "loading": "Loading…",
  "permission.requires": "Requires the {permission} permission",
  "cancel": "Cancel",

  /* ── page-level degraded states ────────────────────────────────────────── */
  "gate.read":
    "External targets, their check definitions and their schedules are configuration, not telemetry: reading them is " +
    "granted to the operator and admin roles, and deliberately not to viewer — which is the role an anonymous " +
    "session gets. Sign in with an account that holds it.",
  "gate.noDatabase": "Targets, definitions and schedules are stored in the database — set console.database.mode",

  /* ── Targets tab ───────────────────────────────────────────────────────── */
  "targets.heading": "Targets",
  "targets.listAria": "Targets",
  /* ui/pager.tsx's noun for this list. */
  "targets.subject": "targets",
  /* Each *.empty teaches in dict/mtr.ts destinations.empty.*'s three-part
     shape: what the object is, what appears once one exists, and a CTA naming
     the create button — the .cta key renders only beside that button. */
  "targets.empty":
    "No targets yet. A target names a host or URL outside the fleet, and external checks probe what is listed " +
    "here. Point a definition at one, and its results land in run history and metrics.",
  "targets.empty.cta": "Create the first one with the New target button above.",
  "targets.unavailable": "Targets are unavailable",
  "targets.gate.write":
    "The list below is complete and current — creating, editing and deleting targets is what needs the extra " +
    "permission. Ask an operator to change the fleet's probe configuration.",
  "targets.new": "New target",
  "targets.form.edit": "Edit {name}",
  "targets.form.create": "New target",
  "targets.form.name": "Name",
  "targets.form.kind": "Kind",
  "targets.form.address": "Address",
  "targets.form.labels": "Labels",
  /* Only the connective is ours; both sides of it are sample addresses. */
  "targets.form.save": "Save target",
  "targets.form.createButton": "Create target",
  /* The label parser's own sentence, moved here verbatim. {part} arrives
     already JSON-quoted from the parser, so the sentence reads the same. */
  "targets.form.labelsSyntax": "labels must be \"key=value\" pairs separated by commas; got {part}",
  "targets.form.labelsMalformed": "labels are malformed",
  "targets.form.failed": "Failed to save the target",
  "targets.row.edit": "Edit {name}",
  "targets.row.delete": "Delete {name}",
  "targets.row.confirmDelete": "Confirm delete {name}",
  "targets.row.deleteFailed": "Failed to delete the target",

  /* ── Definitions tab ───────────────────────────────────────────────────── */
  "definitions.heading": "Check definitions",
  "definitions.listAria": "Check definitions",
  "definitions.subject": "definitions",
  "definitions.empty":
    "No check definitions yet. A definition says what the fleet probes: a check type, which agents send, and " +
    "where the probes go — a target or the nodes themselves. Give one a schedule, and its results land in run " +
    "history and metrics.",
  "definitions.empty.cta": "Create one with the New definition button above.",
  "definitions.unavailable": "Check definitions are unavailable",
  "definitions.gate.read":
    "Check definitions say what the fleet probes and how often. Reading them is granted to the operator and admin " +
    "roles.",
  "definitions.gate.write":
    "The list below is complete and current. Creating, editing and deleting definitions — and asking for the " +
    "projected series count before enabling one — all need the write permission.",
  "definitions.new": "New definition",
  "definitions.form.edit": "Edit {name}",
  "definitions.form.create": "New definition",
  "definitions.form.name": "Name",
  "definitions.form.checkType": "Check type",
  "definitions.form.sourceSelection": "Source selection",
  "definitions.form.destinationKind": "Destination kind",
  "definitions.form.destinationTarget": "Destination target",
  "definitions.form.destinationAddress": "Destination address",
  "definitions.form.pickTarget": "— pick a target —",
  "definitions.form.plane": "Plane",
  "definitions.form.planeNote":
    "Definitions probe from the pod network. M4 ships no second plane, so this is fixed rather than chosen.",
  "definitions.form.params": "Params (JSON)",
  "definitions.form.enabled": "Enabled",
  "definitions.form.save": "Save definition",
  "definitions.form.createButton": "Create definition",
  "definitions.form.failed": "Failed to save the definition",
  "definitions.form.paramsNotObject": "params must be a JSON object",
  "definitions.form.paramsNotJson": "params must be valid JSON",
  "definitions.row.edit": "Edit {name}",
  "definitions.row.delete": "Delete {name}",
  "definitions.row.confirmDelete": "Confirm delete {name}",
  "definitions.row.deleteFailed": "Failed to delete the definition",
  /* The destination COLUMN's wording for destinationKind "node" — the only
     one of the three that is a phrase rather than a name. */
  "definitions.destination.everyNode": "every node",
  "definitions.enabled": "enabled",
  "definitions.disabled": "disabled",

  /* The projection preview. `sourceSelection` and `one-per-zone` stay put:
     they name a form field and a wire value the operator acts on next. */
  "projection.over":
    "~{series} {seriesWord} ({agents} {agentsWord} × {protocols} {protocolsWord}) — above the {limit}-series limit; " +
    "narrow sourceSelection to one-per-zone, or save this definition disabled",
  "projection.ok": "~{series} {seriesWord} ({agents} {agentsWord} × {protocols} {protocolsWord}), limit {limit}",
  "count.series.one": "series",
  "count.series.few": "series",
  "count.series.many": "series",
  /* Was "agents", so a one-agent projection read "(1 agents × …)". */
  "count.agents.one": "agent",
  "count.agents.few": "agents",
  "count.agents.many": "agents",
  "count.protocols.one": "protocol",
  "count.protocols.few": "protocols",
  "count.protocols.many": "protocols",

  /* ── Schedules tab ─────────────────────────────────────────────────────── */
  "schedules.heading": "Schedules",
  "schedules.listAria": "Schedules",
  "schedules.subject": "schedules",
  /* "never fires on its own" is the scheduler's contract, not a guess: the
     loop fires only schedule rows, and agents get only continuous ones. */
  "schedules.empty":
    "No schedules yet. A schedule is the cadence that fires a check definition: once, at an interval, or " +
    "continuously on the agents. A definition without a schedule never fires on its own.",
  "schedules.empty.cta": "Create one with the New schedule button above.",
  "schedules.unavailable": "Schedules are unavailable",
  "schedules.schedulerOff":
    "These schedules will not fire: the scheduler loop is disabled on this install " +
    "(console.scheduler.enabled). Continuous schedules are unaffected — they run on the agents.",
  "schedules.gate.read":
    "Schedules have no read permission of their own — listing them rides on the definitions they belong to.",
  "schedules.gate.write":
    "The list below is complete and current. Creating a cadence, enabling or disabling one, and deleting one all " +
    "need the write permission — reading them only needs checks:read.",
  "schedules.new": "New schedule",
  "schedules.form.create": "New schedule",
  "schedules.form.edit": "Edit the {cadence} schedule of {name}",
  "schedules.form.save": "Save schedule",
  "schedules.form.definitionFixed": "A schedule belongs to its definition; create a new one to point elsewhere.",
  "schedules.form.error.runAtPast": "The moment must be in the future.",
  "schedules.form.definition": "Definition",
  "schedules.form.pickDefinition": "— pick a definition —",
  "schedules.form.kind": "Kind",
  "schedules.form.interval": "Interval (seconds)",
  "schedules.form.runAt": "Run at",
  "schedules.form.runAtNotSet": "Not set",
  "schedules.form.runAtClear": "Clear",
  "schedules.form.runAtClearAria": "Clear run at",
  "schedules.form.enabled": "Enabled",
  "schedules.form.createButton": "Create schedule",
  "schedules.form.failed": "Failed to save the schedule",
  "schedules.form.hint.interval": "Intervals below {seconds}s are raised to {seconds}s.",
  "schedules.form.hint.once": "A one-off fire, and it must be in the future.",
  "schedules.form.hint.continuous":
    "Continuous schedules are pushed to the agents and never fire on the scheduler's clock — they carry no interval " +
    "and no run-at.",
  /* Says what the box WANTS rather than what was wrong with what was typed:
     one sentence covers "5 seconds", "abc", "0x10", "-1" and an empty box, and
     an operator reading it knows immediately what to write instead. */
  "schedules.form.error.interval": "interval must be a positive number of seconds, like 60 or 2.5",
  /* The other refusal is a RANGE, so it names the bound. */
  "schedules.form.error.intervalRange": "interval must be at most {max} seconds",
  "schedules.form.error.runAt": "kind once requires a run at time",
  "schedules.row.edit": "Edit {name}",
  "schedules.row.enable": "Enable {name}",
  "schedules.row.disable": "Disable {name}",
  "schedules.row.delete": "Delete {name}",
  "schedules.row.confirmDelete": "Confirm delete {name}",
  "schedules.row.deleteFailed": "Failed to delete the schedule",
  "schedules.row.updateFailed": "Failed to update the schedule",
  "schedules.row.next": "next {at}",
  "schedules.row.last": "last {at}",
  "schedules.row.recorded": "Recorded {at}",
  /* The scheduler's own message follows the colon, verbatim — this console
     does not paraphrase what the scheduler recorded. */
  "schedules.row.failing": "failing: {message}",
  "schedules.enabled": "enabled",
  "schedules.disabled": "disabled",
  "schedules.paused": "paused: definition disabled",
  "schedules.paused.title": "The schedule is on, but {name} is switched off, so nothing fires. The cadence keeps its place and resumes when the definition is switched back on.",
  "schedules.rowAria": "{name}, {cadence}",
  "schedules.cadence.interval": "every {interval}",
  "schedules.cadence.every.second": "every second",
  "schedules.cadence.every.minute": "every minute",
  "schedules.cadence.every.hour": "every hour",
  "schedules.cadence.unit.second": "s",
  "schedules.cadence.unit.minute": "m",
  "schedules.cadence.unit.hour": "h",
  "schedules.cadence.once": "once at {at}",
  "schedules.cadence.continuous": "continuous",
} as const;

export type TargetsKey = keyof typeof en;

export const targetsDict: Dictionary<TargetsKey> = defineDict(en, {
  "title": "Плановые проверки",
  "description": "Внешние цели зондирования, определения проверок, которые на них указывают, и их расписания.",
  "tabs.aria": "Раздел",
  "tab.targets": "Цели",
  "tab.definitions": "Определения",
  "tab.schedules": "Расписания",
  "loading": "Загрузка…",
  "permission.requires": "Нужно право {permission}",
  "cancel": "Отмена",

  "gate.read":
    "Внешние цели, их определения проверок и расписания относятся к конфигурации, а не к телеметрии. Читать их " +
    "могут operator и admin, а viewer намеренно не может, и именно viewer достаётся анонимной сессии. Войдите " +
    "под учётной записью с нужной ролью.",
  "gate.noDatabase": "Цели, определения и расписания хранятся в базе, задайте console.database.mode",

  "targets.heading": "Цели",
  "targets.listAria": "Цели",
  "targets.subject": "Цели",
  "targets.empty":
    "Целей пока нет. Цель — это хост или URL за пределами флота; внешние проверки зондируют то, что перечислено " +
    "здесь. Наведите на цель определение — и результаты пойдут в историю запусков и в метрики.",
  "targets.empty.cta": "Создайте первую кнопкой «Новая цель» выше.",
  "targets.unavailable": "Цели недоступны",
  "targets.gate.write":
    "Список ниже полон и актуален. Дополнительное право нужно только на то, чтобы создавать, изменять и удалять " +
    "цели. Попросите оператора поправить конфигурацию зондирования.",
  "targets.new": "Новая цель",
  "targets.form.edit": "Изменить {name}",
  "targets.form.create": "Новая цель",
  "targets.form.name": "Имя",
  "targets.form.kind": "Вид",
  "targets.form.address": "Адрес",
  "targets.form.labels": "Метки",
  "targets.form.save": "Сохранить цель",
  "targets.form.createButton": "Создать цель",
  "targets.form.labelsSyntax": "метки задаются парами \"ключ=значение\" через запятую; получено {part}",
  "targets.form.labelsMalformed": "метки заданы неверно",
  "targets.form.failed": "Не удалось сохранить цель",
  "targets.row.edit": "Изменить {name}",
  "targets.row.delete": "Удалить {name}",
  "targets.row.confirmDelete": "Подтвердить удаление {name}",
  "targets.row.deleteFailed": "Не удалось удалить цель",

  "definitions.heading": "Определения проверок",
  "definitions.listAria": "Определения проверок",
  "definitions.subject": "Проверки",
  "definitions.empty":
    "Определений проверок пока нет. Определение описывает проверку: её тип, какие агенты зондируют и куда — " +
    "на цель или на сами узлы. Дайте ему расписание — и результаты пойдут в историю запусков и в метрики.",
  "definitions.empty.cta": "Создайте первое кнопкой «Новое определение» выше.",
  "definitions.unavailable": "Определения проверок недоступны",
  "definitions.gate.read":
    "Определения проверок описывают, что и как часто зондирует флот. Читать их могут роли operator и admin.",
  "definitions.gate.write":
    "Список ниже полон и актуален. Создать, изменить или удалить определение, да и спросить прогноз числа серий " +
    "перед включением, можно только с правом на запись.",
  "definitions.new": "Новое определение",
  "definitions.form.edit": "Изменить {name}",
  "definitions.form.create": "Новое определение",
  "definitions.form.name": "Имя",
  "definitions.form.checkType": "Тип проверки",
  "definitions.form.sourceSelection": "Выбор источников",
  "definitions.form.destinationKind": "Вид назначения",
  "definitions.form.destinationTarget": "Цель назначения",
  "definitions.form.destinationAddress": "Адрес назначения",
  "definitions.form.pickTarget": "выберите цель…",
  "definitions.form.plane": "Плоскость",
  "definitions.form.planeNote":
    "Определения зондируют из сети подов. Второй плоскости в M4 нет, поэтому значение фиксировано, а не выбирается.",
  "definitions.form.params": "Параметры (JSON)",
  "definitions.form.enabled": "Включено",
  "definitions.form.save": "Сохранить определение",
  "definitions.form.createButton": "Создать определение",
  "definitions.form.failed": "Не удалось сохранить определение",
  "definitions.form.paramsNotObject": "params должен быть объектом JSON",
  "definitions.form.paramsNotJson": "params должен быть корректным JSON",
  "definitions.row.edit": "Изменить {name}",
  "definitions.row.delete": "Удалить {name}",
  "definitions.row.confirmDelete": "Подтвердить удаление {name}",
  "definitions.row.deleteFailed": "Не удалось удалить определение",
  "definitions.destination.everyNode": "каждый узел",
  "definitions.enabled": "включено",
  "definitions.disabled": "выключено",

  "projection.over":
    "~{series} {seriesWord} ({agents} {agentsWord} × {protocols} {protocolsWord}), это выше лимита в {limit} " +
    "серий: сузьте sourceSelection до one-per-zone или сохраните определение выключенным",
  "projection.ok": "~{series} {seriesWord} ({agents} {agentsWord} × {protocols} {protocolsWord}), лимит {limit}",
  "count.series.one": "серия",
  "count.series.few": "серии",
  "count.series.many": "серий",
  "count.agents.one": "агент",
  "count.agents.few": "агента",
  "count.agents.many": "агентов",
  "count.protocols.one": "протокол",
  "count.protocols.few": "протокола",
  "count.protocols.many": "протоколов",

  "schedules.heading": "Расписания",
  "schedules.listAria": "Расписания",
  "schedules.subject": "Расписания",
  "schedules.empty":
    "Расписаний пока нет. Расписание — это ритм, по которому срабатывает определение: однократно, с интервалом " +
    "или непрерывно на агентах. Определение без расписания само не срабатывает никогда.",
  "schedules.empty.cta": "Создайте первое кнопкой «Новое расписание» выше.",
  "schedules.unavailable": "Расписания недоступны",
  "schedules.schedulerOff":
    "Эти расписания не сработают: цикл планировщика на этой инсталляции выключен " +
    "(console.scheduler.enabled). Расписаний continuous это не касается — они выполняются на агентах.",
  "schedules.gate.read":
    "Своего права на чтение у расписаний нет: список открывается вместе с определениями, которым они " +
    "принадлежат.",
  "schedules.gate.write":
    "Список ниже полон и актуален. Создать расписание, включить, выключить, удалить: на всё это нужно право на " +
    "запись. Для чтения хватает checks:read.",
  "schedules.new": "Новое расписание",
  "schedules.form.create": "Новое расписание",
  "schedules.form.edit": "Изменить расписание «{cadence}» для {name}",
  "schedules.form.save": "Сохранить расписание",
  "schedules.form.definitionFixed": "Расписание принадлежит своему определению; чтобы указать другое, создайте новое.",
  "schedules.form.error.runAtPast": "Момент должен быть в будущем.",
  "schedules.form.definition": "Определение",
  "schedules.form.pickDefinition": "выберите определение…",
  "schedules.form.kind": "Вид",
  "schedules.form.interval": "Интервал (секунды)",
  "schedules.form.runAt": "Момент запуска",
  "schedules.form.runAtNotSet": "Не задан",
  "schedules.form.runAtClear": "Очистить",
  "schedules.form.runAtClearAria": "Очистить момент запуска",
  "schedules.form.enabled": "Включено",
  "schedules.form.createButton": "Создать расписание",
  "schedules.form.failed": "Не удалось сохранить расписание",
  "schedules.form.hint.interval": "Интервалы меньше {seconds}с поднимаются до {seconds}с.",
  "schedules.form.hint.once": "Однократный запуск, и он должен быть в будущем.",
  "schedules.form.hint.continuous":
    "Непрерывные расписания раздаются агентам и по часам планировщика не срабатывают никогда: ни интервала, " +
    "ни момента запуска у них нет.",
  "schedules.form.error.interval": "интервал — положительное число секунд, например 60 или 2.5",
  "schedules.form.error.intervalRange": "интервал не может быть больше {max} секунд",
  "schedules.form.error.runAt": "для вида once нужен момент запуска",
  "schedules.row.edit": "Изменить {name}",
  "schedules.row.enable": "Включить {name}",
  "schedules.row.disable": "Выключить {name}",
  "schedules.row.delete": "Удалить {name}",
  "schedules.row.confirmDelete": "Подтвердить удаление {name}",
  "schedules.row.deleteFailed": "Не удалось удалить расписание",
  "schedules.row.updateFailed": "Не удалось изменить расписание",
  "schedules.row.next": "следующий {at}",
  "schedules.row.last": "последний {at}",
  "schedules.row.recorded": "Записано {at}",
  "schedules.row.failing": "сбой: {message}",
  "schedules.enabled": "включено",
  "schedules.disabled": "выключено",
  "schedules.paused": "пауза: определение выключено",
  "schedules.paused.title": "Расписание включено, но {name} выключено, поэтому ничего не запускается. Отсчёт продолжается и возобновится, когда определение снова включат.",
  "schedules.rowAria": "{name}, {cadence}",
  "schedules.cadence.interval": "каждые {interval}",
  "schedules.cadence.every.second": "каждую секунду",
  "schedules.cadence.every.minute": "каждую минуту",
  "schedules.cadence.every.hour": "каждый час",
  "schedules.cadence.unit.second": "с",
  "schedules.cadence.unit.minute": "мин",
  "schedules.cadence.unit.hour": "ч",
  "schedules.cadence.once": "однократно в {at}",
  "schedules.cadence.continuous": "непрерывно",
});

/**
 * pluralKey picks the Russian form for `count` out of the three a noun has.
 *
 * The rule is the language's, not this page's: 11-14 take the "many" form
 * whatever they end in, then the last digit decides — 1 is "one", 2-4 are
 * "few", everything else "many". English fills all three slots with the same
 * word, so this runs there too and changes nothing.
 *
 * It lives here, beside the tables it indexes, rather than in lib/i18n:
 * lib/i18n ships no plural machinery on purpose (its module doc says why), and
 * a helper over THIS dictionary's own keys is not machinery, it is a lookup.
 */
export function pluralKey(
  count: number,
  one: TargetsKey,
  few: TargetsKey,
  many: TargetsKey,
  locale: Locale = DEFAULT_LOCALE,
): TargetsKey {
  /* The RUSSIAN rule was applied to both languages, so an English console
     printed "21 rule" and "101 pair": Russian sends every number ending in a
     lone 1 to the .one form, English sends only 1 itself. dict/settings.ts had
     the locale-aware version all along. */
  if (locale !== "ru") return Math.abs(count) === 1 ? one : many;
  const hundred = Math.abs(count) % 100;
  const ten = Math.abs(count) % 10;
  if (hundred >= 11 && hundred <= 14) return many;
  if (ten === 1) return one;
  if (ten >= 2 && ten <= 4) return few;
  return many;
}
