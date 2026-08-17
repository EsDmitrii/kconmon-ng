import { defineDict, type Dictionary } from "@/lib/i18n";
import type { LiveEventSeverity, LiveEventType } from "@/lib/types";

/**
 * live — pages/live.tsx: the event feed's chrome. Its filters, its column
 * headers, the transport and pause states, the loss notice, the blank slates
 * and the two cards that explain a degraded feed.
 *
 * NOT HERE, on purpose:
 *   - every event's own `summary` and `scope`, and an annotation's `text` and
 *     `createdBy`. The feed's rows are the record; translating them would be
 *     this console rewriting what the controller and the operators wrote.
 *   - the WIRE values behind the type and severity labels below
 *     (`check_observed`, `warn`): TYPE_KEYS/SEVERITY_KEYS in live.tsx map the
 *     value onto a key exactly the way chrome.ts's NAV_KEYS maps a path, and an
 *     unknown value still prints raw — Go's fields are open strings.
 *   - `problem.detail` behind "Event history is unavailable" and the server's
 *     own text behind "The live topic was rejected". Only the two FALLBACKS
 *     below are ours, for the paths where the server said nothing usable.
 *   - console.database.retentionDays, MTR, WebSocket, Prometheus.
 *
 * "Live" is the page, so its title is «Онлайн» — chrome.ts's word, and NOT the
 * Time Machine's «реальное время», which is the moment you return to.
 */

const en = {
  "title": "Live",
  /* {cap} is LIVE_RING_CAP, {at} the engaged instant (toLocaleString, the
     VIEWER's locale — see lib/i18n's module doc). The engaged sentence names
     the "Load older" button, so both move together if either is reworded. */
  "description.live":
    "Controller events pushed over the WebSocket, newest first. The browser holds the most recent {cap}; anything older is Prometheus' job.",
  "description.engaged":
    "Scrollback ending {at}, newest first. The live tail is off while the Time Machine is engaged — \"Load older\" walks back from here.",

  /* ── toolbar ────────────────────────────────────────────────────────────── */
  "filters.severity": "Severity",
  "filters.severity.all": "All",
  "filters.type": "Type",
  "filters.type.all": "All types",
  "filters.scope.label": "Scope contains",
  /* node-a->node-b is an EXAMPLE scope, not a word: it stays as typed in both
     languages. The hyphen arrow rather than the pretty one on PURPOSE — a
     placeholder shows what a keyboard can produce, and U+2192 cannot be typed on
     any of them; the box normalises "->", "-->", "=>" and ">" into the arrow the
     rows are drawn with (lib/utils.ts's normalizePairInput), so the example is
     the shortest thing that actually works. Both halves are measured against the
     box (w-64 minus pl-8/pr-2 = 216px at 14px): the longer wordings clipped
     mid-example, en at 221px and ru at 314px. */
  "filters.scope.placeholder": "Scope — node-a->node-b",
  "filters.clear": "Clear filters",

  "pause": "Pause",
  "resume": "Resume",
  "resume.buffered": "Resume ({count} buffered)",
  "paused.badge": "Paused · {count} buffered",
  /* Paused hides the transport badge, which is the one thing that would say
     whether the feed an operator is about to resume is still there at all. */
  "paused.socket.live": "Paused · socket live",
  "paused.socket.down": "Paused · socket down",
  "connecting": "Connecting…",

  "loadOlder": "Load older",
  "loadOlder.loading": "Loading older…",
  "loadOlder.exhausted": "Nothing older matches the current filters.",
  /* The ring is FULL: anything older that arrived would be dropped on the way
     in, so the button would do nothing but spend a round trip. */
  "loadOlder.atCap":
    "The buffer is full at {cap} events. Older ones cannot be added without dropping newer ones; narrow the filters or reload to start a fresh buffer.",

  "counts": "Showing {shown} of {held} events · capped at {cap}",
  "missed.one": "{count} event may have been missed",
  "missed.many": "{count} events may have been missed",
  "missed.title.gaps":
    "Holes in the controller's event numbering — something went missing between the controller and this tab.",
  "missed.title.both":
    "{gaps} from holes in the controller's numbering, {discarded} dropped by this tab because arrivals outran it (a hidden tab gets no frames to render in).",
  /* The same two sentences, but REACHABLE: a title attribute is invisible to
     touch and to anyone not hovering, and this one carries the only
     explanation of a warning triangle. */
  "missed.why": "Why?",
  "missed.whyAria": "Why events may have been missed",
  "missed.hide": "Hide",

  /* ── the feed itself ────────────────────────────────────────────────────── */
  "col.time": "Time",
  "col.severity": "Severity",
  "col.summary": "Summary",
  "col.type": "Type",
  "col.scope": "Scope",
  "feed.aria": "Event feed, newest first",
  "skeleton.loading": "Connecting to the event stream…",

  "severity.info": "Info",
  "severity.warn": "Warn",
  "severity.error": "Error",

  "type.topologyChanged": "Topology changed",
  "type.checkObserved": "Check observed",
  "type.mtrTriggered": "MTR triggered",
  "type.mtrCompleted": "MTR completed",
  "type.diagnosticProgress": "Diagnostic progress",

  /* An operator's note wearing the feed's columns. */
  "note.badge": "Note",
  "note.moment": "Annotation",
  "note.span": "Annotation (span)",

  /* ── the cards above the feed ───────────────────────────────────────────── */
  "topicError.title": "The live topic was rejected",
  "topicError.fallback": "the server rejected the live topic",
  "noRealtime.title": "This replica is not receiving the controller event stream",
  "noRealtime.body":
    "No events will arrive here while that is the case — the feed is not broken, it is unfed. Matrix and Topology fall back to 15s polling, and the feed resumes on its own within 15s of the stream coming back.",
  "history.title": "Event history is unavailable",
  "history.fallback": "failed to load event history",

  /* ── blank slates ───────────────────────────────────────────────────────── */
  "empty.waiting.title": "Waiting for events",
  "empty.waiting.body":
    "Nothing has been pushed since this page opened. Topology changes, observed checks and MTR runs land here the moment the controller emits them.",
  "empty.engaged.title": "No events at or before this time",
  "empty.engaged.body":
    "Event history goes back as far as console.database.retentionDays and no further — an instant older than that has nothing to show, and so does a quiet cluster.",
  "empty.filtered.title": "No events match these filters",
  "empty.filtered.body": "{count} held events, none of them matching. Widen the filters to see them again.",
} as const;

export type LiveKey = keyof typeof en;

/**
 * «Внимание» for warn, not «Предупреждение»: the severity badge lives in a
 * fixed 5.25rem column and the long word overflows it — the Overview's copy of
 * this vocabulary says the same, so one event reads the same word on both.
 *
 * "Scope" is «Область» everywhere on this page — column header, filter label
 * and placeholder — because the filter and the column are the same field.
 */
export const liveDict: Dictionary<LiveKey> = defineDict(en, {
  "title": "Онлайн",
  "description.live":
    "События контроллера приходят по WebSocket, новые сверху. Браузер держит последние {cap}, а всё, что старше, уже забота Prometheus.",
  "description.engaged":
    "Прокрутка назад до {at}, новые сверху. Пока включена Машина времени, живой хвост выключен, а «Загрузить старые» уходит вглубь отсюда.",

  "filters.severity": "Важность",
  "filters.severity.all": "Все",
  "filters.type": "Тип",
  "filters.type.all": "Все типы",
  "filters.scope.label": "Область содержит",
  "filters.scope.placeholder": "Область — node-a->node-b",
  "filters.clear": "Сбросить фильтры",

  "pause": "Пауза",
  "resume": "Продолжить",
  "resume.buffered": "Продолжить (в буфере {count})",
  "paused.badge": "Пауза · в буфере {count}",
  "paused.socket.live": "Пауза · сокет жив",
  "paused.socket.down": "Пауза · сокет отключён",
  "connecting": "Подключение…",

  "loadOlder": "Загрузить старые",
  "loadOlder.loading": "Загружаем старые…",
  "loadOlder.exhausted": "Под текущие фильтры ничего старее не подходит.",
  "loadOlder.atCap":
    "Буфер заполнен: {cap} событий. Добавить старые, не выбросив новые, нельзя — сузьте фильтры или перезагрузите страницу, чтобы начать буфер заново.",

  "counts": "Показано {shown} из {held} событий · предел {cap}",
  "missed.one": "Возможно, пропущено событий: {count}",
  "missed.many": "Возможно, пропущено событий: {count}",
  "missed.title.gaps":
    "Дыры в нумерации событий контроллера: что-то потерялось между ним и этой вкладкой.",
  "missed.title.both":
    "{gaps} пришлось на дыры в нумерации контроллера, а {discarded} вкладка выбросила сама: события шли быстрее, чем она успевала (скрытой вкладке кадров на отрисовку не дают).",
  "missed.why": "Почему?",
  "missed.whyAria": "Почему события могли быть пропущены",
  "missed.hide": "Скрыть",

  "col.time": "Время",
  "col.severity": "Важность",
  "col.summary": "Сводка",
  "col.type": "Тип",
  "col.scope": "Область",
  "feed.aria": "Лента событий, новые сверху",
  "skeleton.loading": "Подключаемся к потоку событий…",

  "severity.info": "Инфо",
  "severity.warn": "Внимание",
  "severity.error": "Ошибка",

  "type.topologyChanged": "Топология изменилась",
  "type.checkObserved": "Проверка зафиксирована",
  "type.mtrTriggered": "MTR запущен",
  "type.mtrCompleted": "MTR завершён",
  "type.diagnosticProgress": "Ход диагностики",

  "note.badge": "Заметка",
  "note.moment": "Заметка",
  "note.span": "Заметка (интервал)",

  "topicError.title": "Топик живой ленты отклонён",
  "topicError.fallback": "сервер отклонил топик живой ленты",
  "noRealtime.title": "Эта реплика не получает поток событий от контроллера",
  "noRealtime.body":
    "Пока так, события сюда приходить не будут: лента не сломана, её просто не кормят. Матрица и Топология уходят на опрос раз в 15 с, а лента подхватится сама в пределах 15 с после того, как поток вернётся.",
  "history.title": "История событий недоступна",
  "history.fallback": "не удалось загрузить историю событий",

  "empty.waiting.title": "Ждём события",
  "empty.waiting.body":
    "С момента открытия страницы ничего не прилетело. Изменения топологии, зафиксированные проверки и запуски MTR попадут сюда сразу, как контроллер их выпустит.",
  "empty.engaged.title": "Событий на этот момент и раньше нет",
  "empty.engaged.body":
    "История событий уходит назад ровно на console.database.retentionDays и не дальше. У момента старше показывать нечего, впрочем, как и у тихого кластера.",
  "empty.filtered.title": "Под эти фильтры событий нет",
  "empty.filtered.body": "Удержано событий: {count}, и ни одно не подходит. Расширьте фильтры, чтобы снова их увидеть.",
});

/**
 * The wire value → key maps. Go's `type` and `severity` are OPEN strings, so
 * these are lookups with a raw-string fallback at the call site, never casts —
 * the same contract chrome.ts's NAV_KEYS has with nav.ts's paths.
 */
export const TYPE_KEYS: Readonly<Record<LiveEventType, LiveKey>> = {
  topology_changed: "type.topologyChanged",
  check_observed: "type.checkObserved",
  mtr_triggered: "type.mtrTriggered",
  mtr_completed: "type.mtrCompleted",
  diagnostic_progress: "type.diagnosticProgress",
};

export const SEVERITY_KEYS: Readonly<Record<LiveEventSeverity, LiveKey>> = {
  info: "severity.info",
  warn: "severity.warn",
  error: "severity.error",
};
