import { DEFAULT_LOCALE, type Locale, defineDict, type Dictionary } from "@/lib/i18n";

/**
 * alerting — pages/alerting.tsx: the console-managed Prometheus rules, the
 * builder that writes them, and the foreign PrometheusRule objects beside them.
 *
 * ── the data line on this page ────────────────────────────────────────────
 * This page manages objects Prometheus evaluates, so more of it is DATA than
 * on any other surface in this zone. What stays untranslated, and why:
 *
 *   - `severity` (info / warning / critical). It is a Prometheus LABEL VALUE:
 *     the builder's select writes it, the renderer stamps it on the rule, and
 *     Alertmanager routes on it. An operator reads it here and types it into a
 *     routing tree next. A badge saying «критическая» over a label that says
 *     `critical` would be this console renaming somebody's alert.
 *   - `kind` (pair-loss, raw, …). The template's identifier, likewise written
 *     by the select and stored. Its BLURB — the sentence after the em dash in
 *     the option — is ours and does translate.
 *   - `renderedExpr`, `syncMessage`, every problem+json detail, the import
 *     report's per-item reasons, and reservedLabelMessage. The last one is the
 *     odd-looking case and it is deliberate: that string is the SERVER's own
 *     sentence, reproduced so the client and the server refuse a reserved
 *     label in identical words. Translating it would make the two disagree.
 *   - Prometheus's duration grammar (30s, 5m, 1h30m) and the unit list. Those
 *     are what goes in the box.
 *
 * `syncStatus` DOES translate, and it is the one enum here that should: no
 * form writes it, nothing routes on it, and it is the reconciler's verdict
 * rendered as a pill — presentation, not payload. An unknown status still
 * renders verbatim rather than vanishing.
 */

const en = {
  /* ── page chrome ───────────────────────────────────────────────────────── */
  "title": "Alerting",
  "description":
    "Console-managed Prometheus alert rules, what the cluster thinks of them, the rules it holds that this " +
    "console does not own, and the maintenance windows that mute these signals.",
  /* The "?" by the title (M7-5); the docs page is docs/console/alerting. */
  "help.body":
    "Console-managed Prometheus alert rules, in three sections: the rules this console owns, the foreign rules it does not, and the maintenance windows that mute these signals. " +
    "Rules live in the console database, and a reconciler applies them to the cluster as one PrometheusRule object — only when console.alerting.enabled is set in the Helm values. " +
    "Each rule row carries the reconciler's verdict: synced, drift or error. " +
    "Reading needs alerts:read; managing rules needs alerts:manage.",
  "loading": "Loading…",
  "permission.requires": "Requires the {permission} permission",
  "cancel": "Cancel",
  "gate.read":
    "Alert rules are what this console asks Prometheus to evaluate, and the foreign rules beside them are objects in " +
    "its namespace. Every built-in role holds alerts:read; a role that does not sees this instead of a page firing " +
    "requests it cannot read the answers to.",

  /* ── duration parsing. The grammar's own letters stay; the sentence around
        them is ours. ──────────────────────────────────────────────────── */
  "duration.notADuration": "\"{text}\" is not a duration: write a number and a unit, like 30s, 5m or 2h",
  "duration.noUnit": "\"{text}\" has no unit: write {digits}s, {digits}m or {digits}h",
  "duration.badUnit": "\"{unit}\" is not a Prometheus duration unit (ms, s, m, h, d, w, y)",
  "duration.order": "\"{text}\" must run from the largest unit to the smallest, like 1h30m",
  /* 292 years is not a round number and is not ours: it is where an int64 of
     nanoseconds ends, which is what Prometheus stores a `for` in. */
  "duration.tooLong": "\"{text}\" is longer than a Prometheus `for` can hold — the limit is about 292y",

  /* ── relative time ─────────────────────────────────────────────────────── */
  "age.justNow": "just now",
  "age.seconds": "{count}s ago",
  "age.minutes": "{count}m ago",
  "age.hours": "{count}h ago",
  "age.days": "{count}d ago",

  /* ── rule list ─────────────────────────────────────────────────────────── */
  "rules.heading": "Alert rules",
  "rules.listAria": "Alert rules",
  /* ui/pager.tsx's noun for this list. */
  "rules.subject": "rules",
  "rules.blurb":
    "Rules this console manages. They live in the database and are applied to the cluster as one PrometheusRule " +
    "object; the status on each row is the reconciler's view of whether the cluster agrees, as of the instant next " +
    "to it.",
  "rules.empty": "No rules yet. Prometheus is evaluating nothing on this console's behalf.",
  "rules.unavailable": "Alert rules are unavailable",
  "rules.unknownLink": "No rule matches this link — it may have been deleted.",
  "rules.new": "New rule",

  /* The reconciler's verdict, as a word rather than as its wire value. */
  "sync.synced": "synced",
  "sync.drift": "drift",
  "sync.error": "error",
  "sync.unsynced": "unsynced",

  "row.enabledAria": "Enabled {name}",
  "row.enabled": "enabled",
  "row.disabled": "disabled",
  "row.details": "Details for {name}",
  "row.sync": "Sync {name} now",
  "row.edit": "Edit {name}",
  "row.delete": "Delete {name}",
  "row.confirmDelete": "Confirm delete {name}",
  "row.renderedExpr": "Rendered expression",
  "row.forLine": "for {duration} · last applied {at}",
  "row.never": "never",
  "row.syncAck": "Reconcile requested. The outcome lands on this row as its sync status — it is not known yet.",
  "row.saveFailed": "Failed to save the rule",
  "row.deleteFailed": "Failed to delete the rule",
  "row.syncFailed": "Failed to request a reconcile",
  "row.syncDisabled": "Prometheus rule sync is disabled",

  /* ── the builder ───────────────────────────────────────────────────────── */
  "form.createAria": "New alert rule",
  "form.editAria": "Edit {name}",
  "form.edit": "Edit {name}",
  "form.create": "New rule",
  "form.name": "Name",
  "form.nameHint":
    "Seeds the alert's own name, so it becomes a Prometheus label value. CamelCase is the convention.",
  "form.kind": "Kind",
  "form.severity": "Severity",
  "form.severityHint": "The label Alertmanager routes on.",
  "form.for": "For",
  "form.forHint": "How long the expression must hold — 30s, 5m, 2h. Blank fires as soon as it holds.",
  "form.enabled": "Enabled",
  "form.save": "Save rule",
  "form.createButton": "Create rule",
  /* Cancel on a form with unsaved work asks once. Only the two BIG forms do
     this — the rule builder and the webhook form — because they are the two a
     reader can spend real time in; a two-field form is cheaper to retype than
     to be interrogated about. */
  "form.discard": "Discard changes",
  "form.discardConfirm": "Discard the changes?",
  "form.keepEditing": "Keep editing",
  "form.rejectedBlock": "Prometheus refused this expression when the preview asked it. Saving it would put an expression into the rule bundle that Prometheus cannot load, which stops the OTHER rules in that bundle from being applied too. Change the expression to continue.",
  "form.failed": "Failed to save the rule",
  "form.everyTarget": "every external target",
  "form.noSuchTarget": "{name} (no such target)",
  /* The enum select's "nothing chosen" row. An em dash, in both languages —
     it is a mark, not a word. */
  "form.enumUnset": "—",
  "form.targetFallbackHint": "Type the exact target name — this console cannot list them for you here.",
  "form.noParams": "This template takes no parameters. How long the condition must hold is the rule's own “for”.",
  "form.unknownKind":
    "This build has no template for “{kind}”, so it cannot show you its parameters or render an expression from " +
    "them. Pick a kind above; saving this one is refused by the server.",

  /* Labels and annotations. The NOUN is interpolated into four different
     strings, so it is a key of its own rather than four hard-coded pairs. */
  "pairs.labels": "Labels",
  "pairs.annotations": "Annotations",
  "pairs.noun.label": "Label",
  "pairs.noun.annotation": "Annotation",
  "pairs.nameAria": "{noun} name {index}",
  "pairs.valueAria": "{noun} value {index}",
  "pairs.namePlaceholder": "name",
  "pairs.valuePlaceholder": "value",
  /* The lowercased noun the button verbs take. Separate keys rather than
     toLowerCase() on the singular: Russian needs the accusative («метку»,
     «примечание»), which no case transform produces. */
  "pairs.add.label": "Add label",
  "pairs.add.annotation": "Add annotation",
  "pairs.remove.label": "Remove label {index}",
  "pairs.remove.annotation": "Remove annotation {index}",
  /* Two rows with one key are not a typo the form can resolve on the operator's
     behalf: the map they become keeps ONE of the two values, and which one is
     an accident of order. So it is refused, next to the boxes, by name. */
  "pairs.duplicate": "\"{name}\" is set twice — remove one of the two rows, or the other value is lost",

  /* ── preview ───────────────────────────────────────────────────────────── */
  "preview.region": "Expression preview",
  "preview.heading": "Preview",
  "preview.notReady":
    "Fill the required parameters to preview the expression. Nothing is asked of Prometheus until then.",
  "preview.rendering": "Rendering…",
  "preview.failed": "The preview could not be rendered",
  "preview.notEvaluated":
    "The expression rendered. It could not be evaluated, so how many series it matches is unknown.",
  "preview.matches": "Matches {series} series right now.",
  "preview.matchesZero": " That is the answer, not a failure: nothing matches at this instant.",

  /* ── kind blurbs. The kind itself is the identifier and stays. ─────────── */
  "kind.pair-loss": "packet loss between nodes",
  "kind.zone-latency": "cross-zone latency quantile",
  "kind.dns-failures": "DNS failure share",
  "kind.http-ttfb": "HTTP time-to-first-byte",
  "kind.agent-missing": "registered agents below expected",
  "kind.external-target-down": "external target failing",
  "kind.raw": "hand-written PromQL",

  /* ── param fields. The `key` of each param is the WIRE key and never
        appears here; these are what the form calls them. ───────────────── */
  "param.protocol": "Protocol",
  "param.thresholdPercent.loss": "Loss threshold (%)",
  "param.thresholdPercent.loss.hint":
    "0–100. The loss metrics are ratios; the renderer multiplies by 100, so this is the percentage a chart shows.",
  "param.scope.sourceNode": "Source node",
  "param.scope.sourceNode.hint": "Optional. Blank means every source.",
  "param.scope.destNode": "Destination node",
  "param.scope.destNode.hint": "Optional. Blank means every destination.",
  "param.quantile": "Quantile",
  "param.quantile.hint": "The three the renderer accepts.",
  "param.thresholdMs.latency": "Latency threshold (ms)",
  "param.thresholdMs.latency.hint": "Greater than 0. The histograms are in seconds; the renderer converts.",
  "param.sourceZone": "Source zone",
  "param.destZone": "Destination zone",
  "param.optional": "Optional.",
  "param.thresholdPercent.dns": "Failure threshold (%)",
  "param.thresholdPercent.dns.hint": "0–100, as a share of the DNS results counter.",
  "param.thresholdMs.ttfb": "TTFB threshold (ms)",
  "param.thresholdMs.ttfb.hint": "Greater than 0. The quantile is fixed at 0.95.",
  "param.url": "URL",
  "param.url.hint": "Optional. Blank means every probed URL.",
  "param.targetName": "Target name",
  "param.targetName.hint": "Optional. Blank means every external target.",
  "param.expr": "PromQL expression",
  "param.expr.hint":
    "Stored verbatim. Validity is what the preview below reports — this console ships no Prometheus parser.",

  /* ── foreign rules ─────────────────────────────────────────────────────── */
  "foreign.heading": "Foreign rules",
  "foreign.listAria": "Foreign PrometheusRule objects",
  "foreign.subject": "objects",
  "foreign.blurb":
    "PrometheusRule objects in this console's namespace that it does not own. Read-only: this console never writes " +
    "to somebody else's object. Importing COPIES a rule's alerting entries into console-managed rows.",
  "foreign.empty": "No foreign PrometheusRule objects in this namespace.",
  "foreign.unavailable": "Foreign rules are unavailable",
  "foreign.import": "Import {name}",
  "foreign.importRefused": "The import was refused",

  /* ── maintenance windows ───────────────────────────────────────────────── */
  /* Moved here from dict/settings.ts with the section (M3-14): a window
     suppresses and annotates the signals this page owns. The strings came over
     verbatim. */
  "maintenance.heading": "Maintenance windows",
  "maintenance.listAria": "All maintenance windows",
  "maintenance.subject": "windows",
  /* {investigate} and {explore} are LINKS the page drops in — one sentence,
     one key, and the translation decides where the two links sit in it. */
  "maintenance.blurb":
    "Every declared window, with no time range — including the ones entirely in the future, which the bars beside " +
    "the charts cannot show because those are bounded to what the chart plots. Declaring a window still happens " +
    "next to the chart it explains, on {investigate} or {explore}; this list is for finding and removing one.",
  "maintenance.empty": "No maintenance windows have been declared.",
  "maintenance.loadMore": "Load older windows",
  "maintenance.unavailable": "Maintenance windows are unavailable",

  /* The two surfaces named in the blurb above. The SAME words dict/chrome.ts's
     sidebar uses — one surface, one name. */
  "link.investigate": "Incidents",
  "link.explore": "Metrics",
  "count.groups.one": "group",
  "count.groups.few": "groups",
  "count.groups.many": "groups",
  "count.rules.one": "rule",
  "count.rules.few": "rules",
  "count.rules.many": "rules",

  /* ── import report ─────────────────────────────────────────────────────── */
  "import.created": "Created",
  "import.skipped": "Skipped",
  "import.notes": "Notes",
  "import.none": "none",
  "import.unnamed": "(unnamed entry)",
  "import.adoption":
    "Adoption copies: the original object is untouched, and the same alerts now exist twice in the cluster until " +
    "its owner removes it. That is their decision, and this console will not make it for them.",
} as const;

export type AlertingKey = keyof typeof en;

export const alertingDict: Dictionary<AlertingKey> = defineDict(en, {
  "title": "Оповещения",
  "description":
    "Правила оповещений Prometheus под управлением консоли, что о них думает кластер, чужие правила рядом, " +
    "которыми консоль не владеет, и окна работ, которые эти сигналы глушат.",
  "help.body":
    "Правила оповещений Prometheus под управлением консоли, три раздела: правила самой консоли, чужие правила, которыми она не владеет, и окна работ, которые эти сигналы глушат. " +
    "Правила лежат в базе консоли, и реконсилер применяет их в кластер одним объектом PrometheusRule — только если в значениях Helm включён console.alerting.enabled. " +
    "У каждой строки правила — вердикт реконсилера: синхронизировано, расхождение или ошибка. " +
    "На чтение нужно право alerts:read, на управление правилами — alerts:manage.",
  "loading": "Загрузка…",
  "permission.requires": "Нужно право {permission}",
  "cancel": "Отмена",
  "gate.read":
    "Правила оповещений консоль отдаёт на вычисление Prometheus, а чужие правила рядом лежат объектами в её " +
    "пространстве имён. Право alerts:read есть у всех встроенных ролей. Роль без него видит эту карточку вместо " +
    "страницы, которая шлёт запросы и не может прочитать ответы.",

  "duration.notADuration": "«{text}» не похоже на длительность: напишите число и единицу, например 30s, 5m или 2h",
  "duration.noUnit": "у «{text}» нет единицы: напишите {digits}s, {digits}m или {digits}h",
  "duration.badUnit": "«{unit}» не входит в единицы длительности Prometheus (ms, s, m, h, d, w, y)",
  "duration.order": "«{text}» должно идти от большей единицы к меньшей, например 1h30m",
  "duration.tooLong": "«{text}» длиннее, чем `for` в Prometheus может хранить: предел — примерно 292y",

  "age.justNow": "только что",
  "age.seconds": "{count} с назад",
  "age.minutes": "{count} мин назад",
  "age.hours": "{count} ч назад",
  "age.days": "{count} д назад",

  "rules.heading": "Правила оповещений",
  "rules.listAria": "Правила оповещений",
  "rules.subject": "Правила",
  "rules.blurb":
    "Правила, которыми управляет консоль. Лежат они в базе, а к кластеру применяются одним объектом " +
    "PrometheusRule. Статус в строке показывает, как реконсилер видит согласие кластера, по состоянию на " +
    "указанный рядом момент.",
  "rules.empty": "Правил пока нет. Prometheus ничего не вычисляет по поручению этой консоли.",
  "rules.unavailable": "Правила оповещений недоступны",
  "rules.unknownLink": "По этой ссылке правило не найдено. Возможно, его удалили.",
  "rules.new": "Новое правило",

  "sync.synced": "синхронизировано",
  "sync.drift": "расхождение",
  "sync.error": "ошибка",
  "sync.unsynced": "не синхронизировано",

  "row.enabledAria": "Включено {name}",
  "row.enabled": "включено",
  "row.disabled": "выключено",
  "row.details": "Подробности: {name}",
  "row.sync": "Синхронизировать {name}",
  "row.edit": "Изменить {name}",
  "row.delete": "Удалить {name}",
  "row.confirmDelete": "Подтвердить удаление {name}",
  "row.renderedExpr": "Сформированное выражение",
  "row.forLine": "for {duration} · применено {at}",
  "row.never": "никогда",
  "row.syncAck":
    "Реконсиляция запрошена. Результат придёт в эту же строку статусом синхронизации, пока он неизвестен.",
  "row.saveFailed": "Не удалось сохранить правило",
  "row.deleteFailed": "Не удалось удалить правило",
  "row.syncFailed": "Не удалось запросить реконсиляцию",
  "row.syncDisabled": "Синхронизация правил Prometheus выключена",

  "form.createAria": "Новое правило оповещения",
  "form.editAria": "Изменить {name}",
  "form.edit": "Изменить {name}",
  "form.create": "Новое правило",
  "form.name": "Имя",
  "form.nameHint":
    "Ложится в основу имени алерта и становится значением метки Prometheus. По соглашению пишут CamelCase.",
  "form.kind": "Вид",
  "form.severity": "Важность",
  "form.severityHint": "Метка, по которой маршрутизирует Alertmanager.",
  "form.for": "Удержание (for)",
  "form.forHint": "Сколько выражение должно держаться: 30s, 5m, 2h. Пусто значит срабатывает сразу.",
  "form.enabled": "Включено",
  "form.save": "Сохранить правило",
  "form.createButton": "Создать правило",
  "form.discard": "Отменить изменения",
  "form.discardConfirm": "Отменить изменения?",
  "form.keepEditing": "Продолжить правку",
  "form.rejectedBlock": "Prometheus отклонил это выражение, когда предпросмотр его спросил. Сохранение положило бы в набор правил выражение, которое Prometheus не может загрузить, — и тогда перестанут применяться остальные правила из этого набора. Измените выражение, чтобы продолжить.",
  "form.failed": "Не удалось сохранить правило",
  "form.everyTarget": "любая внешняя цель",
  "form.noSuchTarget": "{name} (такой цели нет)",
  "form.enumUnset": "—",
  "form.targetFallbackHint": "Введите точное имя цели: список здесь консоль показать не умеет.",
  "form.noParams": "У этого шаблона нет параметров. Сколько держится условие, решает собственное «for» правила.",
  "form.unknownKind":
    "В этой сборке нет шаблона для «{kind}», поэтому ни параметров, ни выражения она не покажет. Выберите вид " +
    "выше: сохранить этот сервер откажется.",

  "pairs.labels": "Метки",
  "pairs.annotations": "Примечания",
  "pairs.noun.label": "Метка",
  "pairs.noun.annotation": "Примечание",
  "pairs.nameAria": "{noun}: имя {index}",
  "pairs.valueAria": "{noun}: значение {index}",
  "pairs.namePlaceholder": "имя",
  "pairs.valuePlaceholder": "значение",
  "pairs.add.label": "Добавить метку",
  "pairs.add.annotation": "Добавить примечание",
  "pairs.remove.label": "Удалить метку {index}",
  "pairs.remove.annotation": "Удалить примечание {index}",
  "pairs.duplicate": "«{name}» задано дважды — уберите одну из строк, иначе второе значение потеряется",

  "preview.region": "Предпросмотр выражения",
  "preview.heading": "Предпросмотр",
  "preview.notReady":
    "Заполните обязательные параметры, чтобы увидеть выражение. До этого к Prometheus ничего не уходит.",
  "preview.rendering": "Формируется…",
  "preview.failed": "Не удалось сформировать предпросмотр",
  "preview.notEvaluated":
    "Выражение сформировано. Вычислить его не удалось, поэтому сколько серий оно охватывает, неизвестно.",
  "preview.matches": "Сейчас охватывает серий: {series}.",
  "preview.matchesZero": " Это ответ, а не сбой: на этот момент не подходит ничего.",

  "kind.pair-loss": "потери пакетов между узлами",
  "kind.zone-latency": "квантиль межзонной задержки",
  /* «сбоев», not «отказов» — the console has ONE word for a probe that failed
     and dict/matrix.ts's legend is where an operator learned it. */
  "kind.dns-failures": "доля сбоев DNS",
  "kind.http-ttfb": "HTTP: время до первого байта",
  "kind.agent-missing": "зарегистрированных агентов меньше ожидаемого",
  "kind.external-target-down": "внешняя цель сбоит",
  "kind.raw": "PromQL вручную",

  "param.protocol": "Протокол",
  "param.thresholdPercent.loss": "Порог потерь (%)",
  "param.thresholdPercent.loss.hint":
    "0–100. Метрики потерь идут долями, формирователь умножает на 100, так что здесь тот же процент, который показывает график.",
  "param.scope.sourceNode": "Узел-источник",
  "param.scope.sourceNode.hint": "Необязательно. Пусто значит любой источник.",
  "param.scope.destNode": "Узел назначения",
  "param.scope.destNode.hint": "Необязательно. Пусто значит любое назначение.",
  "param.quantile": "Квантиль",
  "param.quantile.hint": "Три значения, которые принимает формирователь.",
  "param.thresholdMs.latency": "Порог задержки (мс)",
  "param.thresholdMs.latency.hint": "Больше 0. Гистограммы в секундах, формирователь пересчитает.",
  "param.sourceZone": "Зона-источник",
  "param.destZone": "Зона назначения",
  "param.optional": "Необязательно.",
  "param.thresholdPercent.dns": "Порог сбоев (%)",
  "param.thresholdPercent.dns.hint": "0–100, как доля от счётчика результатов DNS.",
  "param.thresholdMs.ttfb": "Порог TTFB (мс)",
  "param.thresholdMs.ttfb.hint": "Больше 0. Квантиль зафиксирован на 0.95.",
  "param.url": "URL",
  "param.url.hint": "Необязательно. Пусто значит любой зондируемый URL.",
  "param.targetName": "Имя цели",
  "param.targetName.hint": "Необязательно. Пусто значит любая внешняя цель.",
  "param.expr": "Выражение PromQL",
  "param.expr.hint":
    "Хранится дословно. О корректности судит предпросмотр ниже: своего парсера Prometheus в консоли нет.",

  "foreign.heading": "Чужие правила",
  "foreign.listAria": "Чужие объекты PrometheusRule",
  "foreign.subject": "Объекты",
  "foreign.blurb":
    "Объекты PrometheusRule в пространстве имён консоли, которыми она не владеет. Только чтение: в чужой объект " +
    "консоль не пишет никогда. Импорт КОПИРУЕТ записи оповещений правила в строки под управлением консоли.",
  "foreign.empty": "Чужих объектов PrometheusRule в этом пространстве имён нет.",
  "foreign.unavailable": "Чужие правила недоступны",
  "foreign.import": "Импортировать {name}",
  "foreign.importRefused": "Импорт отклонён",

  "maintenance.heading": "Окна работ",
  "maintenance.listAria": "Все окна работ",
  "maintenance.subject": "Окна работ",
  "maintenance.blurb":
    "Все объявленные окна работ, без отсечки по времени, включая те, что целиком впереди. Полосы под графиками " +
    "их не покажут: полоса ограничена тем отрезком, который рисует график. Объявляют окно по-прежнему там, где оно " +
    "что-то объясняет, на {investigate} или {explore}. Этот список нужен для другого: найти окно и убрать.",
  "maintenance.empty": "Окна работ ещё не объявлялись.",
  "maintenance.loadMore": "Показать более старые окна",
  "maintenance.unavailable": "Окна работ недоступны",

  "link.investigate": "Инциденты",
  "link.explore": "Метрики",
  "count.groups.one": "группа",
  "count.groups.few": "группы",
  "count.groups.many": "групп",
  "count.rules.one": "правило",
  "count.rules.few": "правила",
  "count.rules.many": "правил",

  "import.created": "Создано",
  "import.skipped": "Пропущено",
  "import.notes": "Замечания",
  "import.none": "нет",
  "import.unnamed": "(запись без имени)",
  "import.adoption":
    "Импорт копирует: исходный объект не меняется, и одни и те же алерты существуют в кластере дважды, пока их " +
    "владелец не уберёт свой. Это его решение, и консоль не примет его за него.",
});

/** pluralKey picks the Russian form for `count`. Duplicated per dictionary on
 *  purpose — lib/i18n/README.md's one-file-per-surface rule beats sharing six
 *  lines of arithmetic through a file every surface would then touch. */
export function pluralKey(
  count: number,
  one: AlertingKey,
  few: AlertingKey,
  many: AlertingKey,
  locale: Locale = DEFAULT_LOCALE,
): AlertingKey {
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
