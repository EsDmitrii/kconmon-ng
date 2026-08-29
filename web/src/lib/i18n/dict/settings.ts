import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * settings — the Settings page: the language switcher, webhook endpoints, the
 * unbounded maintenance-window list, configuration export/import, and About.
 *
 * The switcher's own section came first, with the i18n foundation, for the
 * obvious reason: the switch has to be translated by the thing it switches, or
 * the one control an operator reaches for after picking Russian is the one
 * control still in English. The rest was added here, key by key, when the
 * manage pages were translated.
 *
 * The two language NAMES are not in this table — see LANGUAGE_OPTIONS in
 * pages/settings.tsx for why.
 *
 * ── what stays English ────────────────────────────────────────────────────
 *   - Webhook EVENT ids (`incident.created`, `alert.fired`). They are the
 *     wire values the checkbox group writes and the payload carries.
 *   - A token's name, its owner subject id and the minted secret itself.
 *     Operator bytes and server bytes; neither is prose.
 *   - `lastStatus` on an endpoint row. The delivery ladder wrote that string
 *     ("ok", "failed: 502"); this page picks its colour and prints it.
 *   - Config keys and route paths inside a sentence: console.database.mode,
 *     console.retention.*, console.webhooks.encryptionKey, GET /api/v1/config.
 *   - Every problem+json detail, and every per-item `reason` in an import
 *     result. The server named the item and said why in one sentence.
 *   - The auth MODE and the role name in About. Both are config values.
 */

const en = {
  /* ── language ──────────────────────────────────────────────────────────── */
  "language.title": "Language",
  "language.description":
    "The console's interface language. It applies immediately and is remembered in this browser. " +
    "Text that comes from the server — node and target names, metric names, API messages — is never translated.",
  /* The radiogroup's accessible name. "Interface language", not "Language":
     the visible heading already says Language, and a screen-reader user
     arrowing onto the control needs to hear WHICH language is being set. */
  "language.aria": "Interface language",

  /* ── page chrome ───────────────────────────────────────────────────────── */
  "title": "Settings",
  "description": "API tokens, webhook endpoints, configuration export/import, and what this console is running as.",
  "loading": "Loading…",
  "cancel": "Cancel",
  "nothing.title": "Your role can view none of the console's settings.",
  "nothing.body":
    "API tokens need tokens:manage, webhook endpoints need webhooks:manage, configuration export/import needs " +
    "settings:write, and the maintenance-window list needs maintenance:write. The first three are admin-only in " +
    "the built-in roles. What is below is everything this role can read here.",

  /* ── webhooks ──────────────────────────────────────────────────────────── */
  "webhooks.heading": "Webhooks",
  "webhooks.listAria": "Webhook endpoints",
  /* ui/pager.tsx's noun for this list. */
  "webhooks.subject": "endpoints",
  "webhooks.blurb":
    "Outbound endpoints the console signs and POSTs incident events to. Delivery is asynchronous with a retry " +
    "ladder, so the last outcome below is what actually happened, not what was attempted.",
  "webhooks.empty": "No endpoints yet. Nothing is being notified.",
  "webhooks.unavailable": "Webhooks are unavailable",
  "webhooks.new": "New endpoint",
  "webhooks.form.edit": "Edit {name}",
  "webhooks.form.create": "New endpoint",
  "webhooks.form.name": "Name",
  "webhooks.form.url": "URL",
  "webhooks.form.events": "Events",
  "webhooks.form.enabled": "Enabled",
  "webhooks.form.secret": "Secret",
  "webhooks.form.secretKeep": "Leave blank to keep the current secret. Typing here replaces it.",
  "webhooks.form.secretNew": "Required — every delivery is signed with it. It is never shown again.",
  "webhooks.form.secretRequired":
    "A secret is required: every delivery is signed, so an endpoint without one cannot exist.",
  /* The console's own words for the basics, so a draft missing three of them
     hears about all three at once instead of one per round trip. The SERVER's
     texts are still what render for anything it refuses — these only cover the
     cases a browser can be certain about. */
  "webhooks.form.nameRequired": "A name is required.",
  "webhooks.form.nameCharset": "The name may only use lowercase letters, digits and hyphens.",
  "webhooks.form.urlRequired": "A URL is required.",
  "webhooks.form.urlScheme": "The URL must start with http:// or https://.",
  "webhooks.form.eventsRequired": "Pick at least one event; an endpoint that listens for nothing is never called.",
  "webhooks.form.save": "Save endpoint",
  "webhooks.form.createButton": "Create endpoint",
  "webhooks.form.discard": "Discard changes",
  "webhooks.form.discardConfirm": "Discard the changes?",
  "webhooks.form.keepEditing": "Keep editing",
  "webhooks.form.failed": "Failed to save the endpoint",
  "webhooks.row.enabled": "enabled",
  "webhooks.row.disabled": "disabled",
  "webhooks.row.signed": "signed",
  "webhooks.row.noSecret": "no secret",
  "webhooks.row.test": "Send test to {name}",
  "webhooks.row.edit": "Edit {name}",
  "webhooks.row.delete": "Delete {name}",
  "webhooks.row.confirmDelete": "Confirm delete {name}",
  "webhooks.row.queued": "Test queued; the outcome lands on this row.",
  "webhooks.row.deleteFailed": "Failed to delete the endpoint",
  "webhooks.row.testFailed": "Failed to enqueue the test delivery",
  "webhooks.row.failures": "{count} consecutive {word}",
  /* The `.one` slot said "failures" too, so a single bad delivery announced
     itself as "1 consecutive failures". English needs the singular here. */
  "count.failures.one": "failure",
  "count.failures.few": "failures",
  "count.failures.many": "failures",

  /* ── API tokens ────────────────────────────────────────────────────────── */
  "tokens.heading": "API tokens",
  "tokens.listAria": "API tokens",
  "tokens.subject": "tokens",
  "tokens.blurb":
    "Bearer tokens for calling this API without a session. The console stores a hash, never the token, so the " +
    "secret below is shown once at creation and cannot be recovered afterwards — a lost one is replaced, not read.",
  "tokens.empty": "No tokens. Nothing is calling this API with one.",
  "tokens.unavailable": "API tokens are unavailable",
  "tokens.new": "New token",
  "tokens.form.create": "New token",
  "tokens.form.name": "Name",
  "tokens.form.nameHelp": "What this token is for. It is what the list and the audit log show instead of the secret.",
  "tokens.form.expires": "Expires",
  /* datetime-local, so the operator types a LOCAL wall clock; the request
     carries the instant it names. */
  "tokens.form.expiresHelp": "Optional. Left empty, the token is valid until it is revoked.",
  "tokens.form.expiresNotSet": "No expiry",
  "tokens.form.expiresClear": "Clear",
  "tokens.form.expiresClearAria": "Clear the expiry",
  "tokens.form.pastExpiry": "The expiry must be in the future; a token that has already expired can never authenticate.",
  "tokens.form.nameRequired": "A token needs a name — it is the only thing the list can show.",
  "tokens.form.badExpiry": "That expiry is not a time this page can read.",
  "tokens.form.createButton": "Create token",
  "tokens.form.failed": "Failed to create the token",
  /* The one-time secret. It is the server's own bytes and is printed verbatim. */
  "tokens.secret.title": "Copy {name} now — this is the only time it is shown.",
  "tokens.secret.aria": "The new token's secret",
  "tokens.secret.copy": "Copy token",
  "tokens.secret.copied": "Token copied.",
  "tokens.secret.noClipboard": "This browser gave the page no clipboard — select the token above and copy it.",
  "tokens.secret.refused": "The browser refused the copy — select the token above and copy it.",
  "tokens.secret.dismiss": "I have saved it",
  "tokens.col.owner": "owner",
  "tokens.col.created": "created",
  "tokens.col.lastUsed": "last used",
  "tokens.col.expires": "expires",
  "tokens.lastUsed.never": "never used",
  "tokens.revoked": "revoked",
  "tokens.expired": "expired",
  "tokens.row.delete": "Revoke {name}",
  "tokens.row.confirmDelete": "Confirm revoke {name}",
  "tokens.row.deleteFailed": "Failed to revoke the token",
  /* A spent token is DELETED, not revoked — two different acts, so two
     different words. Revoking ends a live credential; deleting removes a row
     that already authenticates nothing. */
  "tokens.row.purge": "Delete {name}",
  "tokens.row.confirmPurge": "Confirm delete {name}",
  "tokens.row.purgeFailed": "Failed to delete the token",
  "tokens.row.purgeHint": "This token can no longer authenticate anything. Deleting removes the row for good.",

  /* ── maintenance windows ───────────────────────────────────────────────── */
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

  /* The two surfaces named in the sentences above and in About. The SAME
     words dict/chrome.ts's sidebar uses — one surface, one name. */
  "link.investigate": "Investigate",
  "link.explore": "Explore",

  /* ── export / import ───────────────────────────────────────────────────── */
  "bundle.heading": "Configuration export / import",
  "bundle.blurb":
    "Targets, check definitions, schedules, alert rules, webhook endpoints and maintenance windows — what was " +
    "declared, never what was observed. With rbac:manage the bundle also carries custom ROLES (bindings are " +
    "exported for the record and never imported: a binding names a person in the source console's identity " +
    "namespace). Every section applies only if you hold the permission its own page requires. Webhook endpoints " +
    "are NOT created by an import: a bundle never carries a secret and an endpoint cannot exist without one. " +
    "Create the endpoint here first, then import to apply the bundle's url, events and enabled flag.",
  "bundle.export": "Export configuration",
  "bundle.exportFailed": "Failed to export the configuration",
  "bundle.field": "Configuration bundle",
  "bundle.choose": "Choose bundle…",
  "bundle.noFile": "No file chosen",
  "bundle.dryRunNote":
    "Choosing a file runs a dry run immediately: it writes nothing and predicts, per collection, exactly what " +
    "Apply would do.",
  "bundle.apply": "Apply import",
  "bundle.importRefused": "The import was refused",
  "bundle.notJson": "That file is not valid JSON. A configuration bundle is the JSON this page exports.",
  "bundle.notObject": "That file is valid JSON but not a bundle: a bundle is a JSON object.",
  "bundle.versionMismatch":
    "This console reads bundle version {expected}; that file declares {found}. Importing the part this build " +
    "recognises would be a partial restore presented as a complete one, so it is refused.",
  "bundle.dryRun": "Dry run — nothing was written.",
  "bundle.applied": "Applied — these writes happened.",
  "bundle.col.collection": "Collection",
  "bundle.col.created": "Created",
  "bundle.col.updated": "Updated",
  "bundle.col.skipped": "Skipped",
  "bundle.errors": "Errors",
  "bundle.warnings": "Warnings",
  "collection.targets": "Targets",
  "collection.checkDefinitions": "Check definitions",
  "collection.checkSchedules": "Check schedules",
  "collection.alertRules": "Alert rules",
  "collection.webhooks": "Webhooks",
  "collection.maintenanceWindows": "Maintenance windows",
  "collection.rbacRoles": "Custom roles",
  "collection.rbacBindings": "Role bindings",

  /* ── About ─────────────────────────────────────────────────────────────── */
  "about.heading": "About this console",
  "about.authMode": "Auth mode",
  "about.roles": "Your roles",
  "about.subject": "Your subject",
  "about.version": "Console build",
  "about.commit": "Commit",
  "about.controller": "Controller",
  "about.prometheus": "Prometheus",
  "about.database": "Database",
  "about.configured": "configured",
  "about.notConfigured": "not configured",
  /* Russian agrees the participle with the SUBJECT's gender, so one word
     cannot serve all three facts: «Контроллер настроен» but «База данных
     настроена». English has no such agreement, which is exactly why the single
     key survived this long. */
  "about.configured.f": "configured",
  "about.notConfigured.f": "not configured",
  "about.anonymous":
    "Anonymous mode: every unauthenticated request is the {role} role (console.auth.anonymous.role). There is no " +
    "sign-in.",
  /* {days} is console.database.retentionDays, straight from GET /api/v1/config. */
  "about.retention":
    "Retention: the database keeps {days} days of history (console.database.retentionDays), and the pruner logs " +
    "what it swept.",
  "about.retention.off":
    "Retention: pruning is disabled (console.database.retentionDays: 0) — history is kept until removed by hand.",
  "about.maintenance":
    "Maintenance windows are declared where they explain something — on {investigate} and {explore}, next to the " +
    "chart they cover — rather than a second time here. The section above lists every declared window with no " +
    "range, which is the only place a future one can be found and removed. Roles and role bindings are not " +
    "administered from this console at all; API tokens are, in the section above.",
} as const;

export type SettingsKey = keyof typeof en;

export const settingsDict: Dictionary<SettingsKey> = defineDict(en, {
  "language.title": "Язык",
  "language.description":
    "Язык интерфейса. Переключается сразу и запоминается в этом браузере. Всё, что приходит с сервера, " +
    "остаётся как есть: имена узлов и целей, названия метрик, сообщения API.",
  "language.aria": "Язык интерфейса",

  "title": "Настройки",
  "description":
    "Токены API, точки доставки вебхуков, экспорт и импорт конфигурации, а также то, на чём эта консоль работает.",
  "loading": "Загрузка…",
  "cancel": "Отмена",
  "nothing.title": "Эта роль не видит ни одного раздела настроек.",
  "nothing.body":
    "Токенам API нужно tokens:manage, точкам вебхуков webhooks:manage, экспорту и импорту конфигурации " +
    "settings:write, списку окон работ maintenance:write. Первые три во встроенных ролях достались только admin. " +
    "Ниже всё, что эта роль здесь прочитает.",

  "webhooks.heading": "Вебхуки",
  "webhooks.listAria": "Точки доставки вебхуков",
  "webhooks.subject": "Точки доставки",
  "webhooks.blurb":
    "Исходящие точки: консоль подписывает событие инцидента и отправляет его POST-ом. Доставка асинхронная, с " +
    "лестницей повторов, поэтому последний исход в строке говорит, чем всё кончилось, а не что было предпринято.",
  "webhooks.empty": "Точек пока нет. Никого не уведомляем.",
  "webhooks.unavailable": "Вебхуки недоступны",
  "webhooks.new": "Новая точка",
  "webhooks.form.edit": "Изменить {name}",
  "webhooks.form.create": "Новая точка",
  "webhooks.form.name": "Имя",
  "webhooks.form.url": "URL",
  "webhooks.form.events": "События",
  "webhooks.form.enabled": "Включено",
  "webhooks.form.secret": "Секрет",
  "webhooks.form.secretKeep": "Оставьте пустым, и текущий секрет останется на месте. Впишете сюда своё, он заменится.",
  "webhooks.form.secretNew": "Обязателен: им подписывается каждая доставка. Больше вы его не увидите.",
  "webhooks.form.secretRequired":
    "Нужен секрет: каждая доставка подписывается, поэтому точки без секрета не бывает.",
  "webhooks.form.nameRequired": "Имя обязательно.",
  "webhooks.form.nameCharset": "В имени допустимы только строчные латинские буквы, цифры и дефис.",
  "webhooks.form.urlRequired": "URL обязателен.",
  "webhooks.form.urlScheme": "URL должен начинаться с http:// или https://.",
  "webhooks.form.eventsRequired": "Выберите хотя бы одно событие: точка, которая ничего не слушает, никогда не вызывается.",
  "webhooks.form.save": "Сохранить точку",
  "webhooks.form.createButton": "Создать точку",
  "webhooks.form.discard": "Отменить изменения",
  "webhooks.form.discardConfirm": "Отменить изменения?",
  "webhooks.form.keepEditing": "Продолжить правку",
  "webhooks.form.failed": "Не удалось сохранить точку",
  "webhooks.row.enabled": "включено",
  "webhooks.row.disabled": "выключено",
  "webhooks.row.signed": "подписывается",
  "webhooks.row.noSecret": "нет секрета",
  "webhooks.row.test": "Тестовая доставка в {name}",
  "webhooks.row.edit": "Изменить {name}",
  "webhooks.row.delete": "Удалить {name}",
  "webhooks.row.confirmDelete": "Подтвердить удаление {name}",
  "webhooks.row.queued": "Тест в очереди, исход появится в этой строке.",
  "webhooks.row.deleteFailed": "Не удалось удалить точку",
  "webhooks.row.testFailed": "Не удалось поставить тестовую доставку в очередь",
  "webhooks.row.failures": "{count} {word} подряд",
  /* «сбой», not «отказ». The endpoint is not refusing anything — the delivery
     FAILED, which is the one English word the whole console renders as «сбой»
     (dict/matrix.ts's legend, dict/cards.ts's tier badge, dict/topology.ts's
     health). «отказ» is reserved for a refusal, and a webhook that 500s is not
     one. lib/i18n/index.test.tsx sweeps every dictionary for a relapse. */
  "count.failures.one": "сбой",
  "count.failures.few": "сбоя",
  "count.failures.many": "сбоев",

  "tokens.heading": "Токены API",
  "tokens.listAria": "Токены API",
  "tokens.subject": "Токены",
  "tokens.blurb":
    "Bearer-токены для обращений к этому API без сессии. Консоль хранит хеш, а не сам токен, поэтому секрет " +
    "показывается один раз при создании и потом его уже не достать: потерянный не читают, а выпускают заново.",
  "tokens.empty": "Токенов нет. С токеном к этому API никто не ходит.",
  "tokens.unavailable": "Токены API недоступны",
  "tokens.new": "Новый токен",
  "tokens.form.create": "Новый токен",
  "tokens.form.name": "Имя",
  "tokens.form.nameHelp": "Для чего этот токен. Именно имя видно в списке и в журнале аудита, а не секрет.",
  "tokens.form.expires": "Истекает",
  "tokens.form.expiresHelp": "Необязательно. Если оставить пустым, токен живёт до отзыва.",
  "tokens.form.expiresNotSet": "Без срока",
  "tokens.form.expiresClear": "Очистить",
  "tokens.form.expiresClearAria": "Очистить срок действия",
  "tokens.form.pastExpiry": "Срок должен быть в будущем: уже истёкший токен не сможет пройти аутентификацию.",
  "tokens.form.nameRequired": "Токену нужно имя: больше списку показать нечего.",
  "tokens.form.badExpiry": "Такую дату истечения страница прочитать не может.",
  "tokens.form.createButton": "Создать токен",
  "tokens.form.failed": "Не удалось создать токен",
  "tokens.secret.title": "Скопируйте {name} сейчас, второй раз секрет не покажут.",
  "tokens.secret.aria": "Секрет нового токена",
  "tokens.secret.copy": "Скопировать токен",
  "tokens.secret.copied": "Токен скопирован.",
  "tokens.secret.noClipboard": "Браузер не дал странице доступ к буферу обмена. Выделите токен выше и скопируйте.",
  "tokens.secret.refused": "Браузер отказал в копировании. Выделите токен выше и скопируйте.",
  "tokens.secret.dismiss": "Я сохранил",
  "tokens.col.owner": "владелец",
  "tokens.col.created": "создан",
  "tokens.col.lastUsed": "последнее использование",
  "tokens.col.expires": "истекает",
  "tokens.lastUsed.never": "не использовался",
  "tokens.revoked": "отозван",
  "tokens.expired": "истёк",
  "tokens.row.delete": "Отозвать {name}",
  "tokens.row.confirmDelete": "Подтвердить отзыв {name}",
  "tokens.row.deleteFailed": "Не удалось отозвать токен",
  "tokens.row.purge": "Удалить {name}",
  "tokens.row.confirmPurge": "Подтвердить удаление {name}",
  "tokens.row.purgeFailed": "Не удалось удалить токен",
  "tokens.row.purgeHint": "Этот токен больше ничего не аутентифицирует. Удаление убирает строку насовсем.",

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

  "link.investigate": "Расследование",
  "link.explore": "Метрики",

  "bundle.heading": "Экспорт и импорт конфигурации",
  "bundle.blurb":
    "Цели, определения проверок, расписания, правила оповещений, точки вебхуков и окна работ. Только объявленное, " +
    "ничего из наблюдённого. С правом rbac:manage пакет несёт ещё и пользовательские РОЛИ (привязки экспортируются " +
    "для истории и никогда не импортируются: привязка называет человека в пространстве имён исходной консоли). " +
    "Каждый раздел применяется, только если у вас есть право, которого требует его собственная страница. Точки " +
    "вебхуков импорт НЕ создаёт: секрет в пакет не кладётся, а без секрета точки не существует. Сначала создайте " +
    "точку здесь, потом импортируйте — импорт применит url, события и флаг включения.",
  "bundle.export": "Экспортировать конфигурацию",
  "bundle.exportFailed": "Не удалось экспортировать конфигурацию",
  "bundle.field": "Пакет конфигурации",
  "bundle.choose": "Выбрать пакет…",
  "bundle.noFile": "Файл не выбран",
  "bundle.dryRunNote":
    "Как только файл выбран, идёт пробный прогон. Он ничего не пишет и по каждой коллекции показывает, что сделает " +
    "импорт.",
  "bundle.apply": "Применить импорт",
  "bundle.importRefused": "Импорт отклонён",
  "bundle.notJson": "Файл не разобрался как JSON. Пакет конфигурации выглядит ровно так, как его выгружает эта страница.",
  "bundle.notObject": "JSON корректный, но это не пакет: пакет должен быть объектом JSON.",
  "bundle.versionMismatch":
    "Консоль читает пакеты версии {expected}, а файл объявляет {found}. Затащить только понятную сборке часть " +
    "значило бы выдать частичное восстановление за полное, поэтому импорт отклонён.",
  "bundle.dryRun": "Пробный прогон, ничего не записано.",
  "bundle.applied": "Применено. Записи ниже действительно прошли.",
  "bundle.col.collection": "Коллекция",
  "bundle.col.created": "Создано",
  "bundle.col.updated": "Обновлено",
  "bundle.col.skipped": "Пропущено",
  "bundle.errors": "Ошибки",
  "bundle.warnings": "Предупреждения",
  "collection.targets": "Цели",
  "collection.checkDefinitions": "Определения проверок",
  "collection.checkSchedules": "Расписания проверок",
  "collection.alertRules": "Правила оповещений",
  "collection.webhooks": "Вебхуки",
  "collection.maintenanceWindows": "Окна работ",
  "collection.rbacRoles": "Пользовательские роли",
  "collection.rbacBindings": "Привязки ролей",

  "about.heading": "Об этой консоли",
  "about.authMode": "Режим аутентификации",
  "about.roles": "Ваши роли",
  "about.subject": "Ваш субъект",
  "about.version": "Сборка консоли",
  "about.commit": "Коммит",
  "about.controller": "Контроллер",
  "about.prometheus": "Prometheus",
  "about.database": "База данных",
  "about.configured": "настроен",
  "about.notConfigured": "не настроен",
  "about.configured.f": "настроена",
  "about.notConfigured.f": "не настроена",
  "about.anonymous":
    "Анонимный режим: каждый неаутентифицированный запрос идёт с ролью {role} (console.auth.anonymous.role). " +
    "Входа нет.",
  "about.retention":
    "Хранение: база держит историю {days} дн. (console.database.retentionDays), а очиститель пишет в лог, что " +
    "именно вычистил.",
  "about.retention.off":
    "Хранение: очистка выключена (console.database.retentionDays: 0) — история копится, пока её не удалят руками.",
  "about.maintenance":
    "Окна работ объявляют там, где они что-то объясняют: на {investigate} и {explore}, рядом с графиком, который " +
    "они закрывают. Второй такой формы здесь нет. Раздел выше показывает все объявленные окна без отсечки по " +
    "времени, и это единственное место, где будущее окно можно найти и убрать. Роли и их привязки из этой консоли " +
    "не администрируются вообще, а токены API — да, в разделе выше.",
});

/**
 * pluralKey picks the form `count` takes IN THE LANGUAGE ON SCREEN. Duplicated
 * per dictionary on purpose — see lib/i18n/README.md's one-file-per-surface
 * rule — and it takes the locale for the same reason dict/annotations.ts's
 * countForm does.
 *
 * It did not, and the doc comment justified that with "English fills all three
 * slots with the same word, so the Russian rule runs there too and changes
 * nothing". That is not true of this table: `count.failures.*` is
 * failure/failures/failures, and the Russian rule sends 21, 31, 101 and 1001 to
 * the ONE form. An English console read «21 failure». English has exactly two
 * forms and only the number 1 takes the singular.
 */
export function pluralKey(
  locale: string,
  count: number,
  one: SettingsKey,
  few: SettingsKey,
  many: SettingsKey,
): SettingsKey {
  if (locale !== "ru") return count === 1 ? one : many;
  const hundred = Math.abs(count) % 100;
  const ten = Math.abs(count) % 10;
  if (hundred >= 11 && hundred <= 14) return many;
  if (ten === 1) return one;
  if (ten >= 2 && ten <= 4) return few;
  return many;
}
