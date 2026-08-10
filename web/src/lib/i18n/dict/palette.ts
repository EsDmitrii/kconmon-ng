import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * palette — lib/commands.ts's registry and components/command-palette.tsx's
 * chrome: the group headers, the action and view titles, the empty state, and
 * the KEYWORD CORPUS the scorer ranks a query against.
 *
 * ── the gap this dictionary closes ────────────────────────────────────────
 * The palette shipped English because its titles are not only labels: they are
 * the search corpus, and lib/commands.test.ts pins English titles as ranking
 * fixtures. Translating the display alone would have made a Russian operator's
 * query miss the entry whose Russian label they had just read.
 *
 * So the palette does BOTH, and the two halves are deliberately different
 * questions:
 *
 *   DISPLAY follows the locale — commandTitle(cmd, locale) picks `titleRu`
 *   when there is one, and falls back to the English `title`, which stays the
 *   SOURCE field every existing fixture reads.
 *
 *   SEARCH matches BOTH languages, always, in either locale. Each entry
 *   carries its English title AND its Russian one in the scorer's title set,
 *   and its keyword list holds one English blob and one Russian blob. An
 *   operator who reads «Матрица» and types "matrix" finds it; one who reads
 *   "Matrix" and types «матрица» finds it too. Nothing about that depends on
 *   which language the console happens to be in — a bilingual team shares one
 *   muscle memory, and a palette that only answers to the language of the
 *   moment would be the console withholding what it knows.
 *
 * ── keywords are BLOBS, not word lists ────────────────────────────────────
 * One space-joined string per language rather than an array of terms. The
 * scorer only ever asks "does this keyword contain the query word, and at a
 * word boundary?", and a blob answers that identically to the same words held
 * apart — while making "the corpus has an English half and a Russian half" a
 * fact the table shows rather than a convention the reader has to infer.
 *
 * ── what is NOT here ──────────────────────────────────────────────────────
 * Navigation TITLES. They come from lib/i18n/dict/chrome.ts through NAV_KEYS,
 * the very table components/app-sidebar.tsx renders its links from, so the
 * palette and the sidebar cannot call one surface two things. Only the nav
 * DESCRIPTIONS live here — see NAV_DESC_KEYS below.
 */

const en = {
  /* ── the palette's own chrome ──────────────────────────────────────────── */
  "dialog": "Command palette",
  "list": "Commands",
  "input": "Type a command or search",
  "placeholder": "Type a command or search…",
  "empty": "Nothing matches",
  /* The tag a time-disabled row wears. The palette is an overlay with a scrim
     over the Time Machine's own banner, so this row says it for itself —
     visible text, not a title attribute, so it is part of the option's
     accessible name (components/command-palette.tsx says so at length). */
  "liveOnly": "Live only",

  /* ── group headers. `CommandGroup` stays an English TYPE; these are what it
        renders as, and what role="group" announces. ────────────────────── */
  "group.Navigation": "Navigation",
  "group.Actions": "Actions",
  "group.View": "View",

  /* ── actions ───────────────────────────────────────────────────────────── */
  "action.runCheck": "Run a diagnostic check…",
  "action.runCheck.kw": "diagnostics tcp udp icmp dns http run",
  "action.investigate": "Start an investigation…",
  "action.investigate.kw": "incident timeline root cause correlate",
  "action.alertRule": "Create an alert rule…",
  "action.alertRule.kw": "alerting prometheus rule severity threshold",
  "action.maintenance": "Declare a maintenance window…",
  "action.maintenance.kw": "downtime change window planned explore",
  "action.annotation": "Add an annotation…",
  "action.annotation.kw": "note marker comment explore",

  /* ── view ──────────────────────────────────────────────────────────────── */
  "view.timemachinePick": "Toggle Time Machine — pick a time…",
  "view.timemachinePick.kw": "history past at replay rewind",
  "view.timemachineLive": "Return to Live",
  "view.timemachineLive.kw": "time machine now present toggle",
  /* Two titles, one entry: the label names the theme it switches TO, the same
     convention components/theme-toggle.tsx's aria-label uses. */
  "view.themeLight": "Switch to light theme",
  "view.themeDark": "Switch to dark theme",
  "view.theme.kw": "theme dark light appearance contrast",

  /* ── nav descriptions ──────────────────────────────────────────────────────
     The `en` half is nav.ts's own `description`, BYTE FOR BYTE: it is the
     sidebar link's tooltip, it is the palette's English keyword text, and
     lib/commands.test.ts asserts the registry carries it verbatim. The ru half
     is the other half of the search corpus and the Russian tooltip.

     Both move together — a tooltip an operator can read in Russian and then
     not find by typing what they read would be worse than either alone. */
  "navDesc.overview": "Health summary, worst pairs, firing alerts, recent events.",
  "navDesc.live": "Real-time event feed.",
  "navDesc.investigate": "Investigation Mode entry and saved incidents.",
  "navDesc.matrix": "Live/historical N×N heatmap.",
  "navDesc.topology": "Interactive connectivity map.",
  "navDesc.mtr": "MTR Explorer.",
  "navDesc.diagnostics": "Run checks and run history.",
  "navDesc.targets": "External targets, definitions, schedules.",
  "navDesc.explore": "Curated metrics and A/B compare.",
  "navDesc.alerting": "Rule list and builder.",
  "navDesc.console": "PromQL dev-tools.",
  "navDesc.settings": "Auth, RBAC, retention, maintenance, webhooks, export/import.",
} as const;

export type PaletteKey = keyof typeof en;

export const paletteDict: Dictionary<PaletteKey> = defineDict(en, {
  "dialog": "Палитра команд",
  "list": "Команды",
  "input": "Команда или поиск",
  "placeholder": "Команда или поиск…",
  "empty": "Ничего не найдено",
  "liveOnly": "только в реальном времени",

  "group.Navigation": "Навигация",
  "group.Actions": "Действия",
  "group.View": "Вид",

  "action.runCheck": "Запустить проверку…",
  /* The Russian blobs carry the words an operator actually types, including a
     couple the title does not use — «прогон» for a run, and both spellings of
     «тёмная»/«темная», because nobody reaches for ё at 3am. */
  "action.runCheck.kw": "диагностика проверка запустить прогон tcp udp icmp dns http",
  "action.investigate": "Начать расследование…",
  "action.investigate.kw": "инцидент расследование таймлайн первопричина корреляция",
  "action.alertRule": "Создать правило оповещения…",
  "action.alertRule.kw": "оповещение правило порог важность алерт prometheus",
  "action.maintenance": "Объявить окно работ…",
  "action.maintenance.kw": "работы простой окно регламент план метрики",
  "action.annotation": "Добавить заметку…",
  "action.annotation.kw": "заметка метка примечание комментарий метрики",

  "view.timemachinePick": "Машина времени: выбрать момент…",
  "view.timemachinePick.kw": "история прошлое машина времени перемотка повтор момент",
  "view.timemachineLive": "Вернуться в реальное время",
  "view.timemachineLive.kw": "машина времени сейчас настоящее реальное время переключить",
  "view.themeLight": "Переключить на светлую тему",
  "view.themeDark": "Переключить на тёмную тему",
  "view.theme.kw": "тема тёмная темная светлая оформление контраст",

  "navDesc.overview": "Сводка здоровья, худшие пары, активные оповещения, свежие события.",
  "navDesc.live": "Лента событий в реальном времени.",
  "navDesc.investigate": "Вход в режим расследования и сохранённые инциденты.",
  "navDesc.matrix": "Тепловая карта N×N: сейчас и в прошлом.",
  "navDesc.topology": "Интерактивная карта связности.",
  "navDesc.mtr": "Обозреватель MTR.",
  "navDesc.diagnostics": "Запуск проверок и история запусков.",
  "navDesc.targets": "Внешние цели, определения проверок, расписания.",
  "navDesc.explore": "Подобранные метрики и сравнение A/B.",
  "navDesc.alerting": "Список правил и конструктор.",
  "navDesc.console": "Инструменты PromQL.",
  "navDesc.settings": "Аутентификация, RBAC, хранение, окна работ, вебхуки, экспорт/импорт.",
});

/**
 * NAV_DESC_KEYS maps a nav.ts PATH onto its description key, the same shape
 * (and the same fallback rule) chrome.ts's NAV_KEYS uses for the labels: a path
 * this build's table has not been taught renders nav.ts's own English sentence
 * rather than nothing.
 */
export const NAV_DESC_KEYS: Readonly<Record<string, PaletteKey>> = {
  "/": "navDesc.overview",
  "/live": "navDesc.live",
  "/investigate": "navDesc.investigate",
  "/matrix": "navDesc.matrix",
  "/topology": "navDesc.topology",
  "/mtr": "navDesc.mtr",
  "/diagnostics": "navDesc.diagnostics",
  "/targets": "navDesc.targets",
  "/explore": "navDesc.explore",
  "/alerting": "navDesc.alerting",
  "/console": "navDesc.console",
  "/settings": "navDesc.settings",
};
