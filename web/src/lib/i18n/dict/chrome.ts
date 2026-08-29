import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * chrome — the frame every route renders inside: the sidebar (its links, its
 * group headers, its footer), the skip link, the anonymous-mode banner and the
 * Time Machine bar.
 *
 * This is the REFERENCE dictionary. A page surface looks exactly like it, only
 * smaller — see lib/i18n/README.md.
 *
 * NOT HERE, on purpose:
 *   - nav.ts's per-item `description` (the link's hover tooltip). It is also
 *     lib/commands.ts's SEARCH CORPUS for the palette, so the two had to move
 *     together — and they did, into dict/palette.ts (NAV_DESC_KEYS). The
 *     sidebar reads its tooltip from there for exactly that reason: a tooltip
 *     an operator can read in Russian and then not find by typing what they
 *     read would be worse than either alone.
 *   - "kconmon-ng" and every protocol or tool name (MTR, PromQL, TCP, DNS).
 *     Product and protocol names are identifiers, not prose.
 *
 * READ BY the palette as well as by the sidebar: lib/commands.ts takes each
 * nav entry's Russian title from NAV_KEYS + this table rather than keeping a
 * second copy, so one surface cannot end up with two names.
 */

const en = {
  /* ── sidebar navigation, keyed by SURFACE rather than by path so the key
        reads at the call site. NAV_KEYS below is the path→key map. ────────── */
  "nav.overview": "Overview",
  "nav.live": "Live",
  "nav.investigate": "Investigate",
  "nav.matrix": "Matrix",
  "nav.topology": "Topology",
  "nav.mtr": "MTR",
  "nav.diagnostics": "Diagnostics",
  "nav.targets": "Targets & Schedules",
  "nav.explore": "Explore",
  "nav.alerting": "Alerting",
  "nav.console": "Console",
  "nav.settings": "Settings",

  /* Sidebar group headers. Rendered uppercase by CSS (tracking-[0.1em]), so
     these stay in sentence case and Russian keeps its own capitalisation. */
  "nav.group.monitor": "Monitor",
  "nav.group.investigate": "Investigate",
  "nav.group.manage": "Manage",

  "sidebar.footer": "Network connectivity console",
  /* The footer's <kbd> tooltip. {keys} is the palette hotkey as this OS spells
     it (⌘K or Ctrl+K) — an identifier, interpolated rather than translated. */
  "sidebar.palette.hint": "{keys} — search and commands",

  "shell.skipToContent": "Skip to main content",
  /* The <nav>'s accessible NAME. A screen reader announces it before the first
     link, and an unnamed landmark in a page with several is one an operator has
     to enter to identify. */
  "shell.nav.aria": "Main",

  /* ── the narrow-viewport drawer ────────────────────────────────────────── */
  /* Below 768px the sidebar is a drawer rather than a column. The trigger says
     what it OPENS, since a hamburger glyph says nothing on its own. */
  "shell.menu.open": "Open navigation",
  "shell.menu.close": "Close navigation",
  "shell.menu.aria": "Navigation",

  /* ── anonymous-mode banner ─────────────────────────────────────────────── */
  "banner.anonymous.title": "Anonymous mode.",
  /* The role NAME comes from GET /api/v1/config and goes in verbatim, the same
     way Settings' about.anonymous already interpolates it. The role-less form
     is the fallback for a config that did not carry one. */
  "banner.anonymous.body.role":
    "Authentication is disabled — everyone has the {role} role (console.auth.anonymous.role). Do not use in production.",
  "banner.anonymous.body":
    "Authentication is disabled — everyone has the fixed role. Do not use in production.",

  /* ── Time Machine bar ──────────────────────────────────────────────────── */
  "timemachine.label": "Time Machine",
  "timemachine.trigger": "Now — Time Machine, view the console at a past time",
  /* The idle chip names the feature AND the anchor: sitting beside the range
     presets, a bare "Now" read as one of them and nobody found the Time
     Machine behind it (M3-11, live pass). */
  "timemachine.now": "Time Machine: Now",
  "timemachine.hint": "Time Machine: the presets pick how far back, this picks the moment you are looking from.",
  /* {at} is the bar's ONE stamp, formatted in the INTERFACE language (lib/i18n's
     localeTag) because it lands inside these sentences — see
     components/timemachine-bar.tsx. */
  "timemachine.viewing": "You are viewing {at}",
  "timemachine.viewingHint": "— return to Live to act.",
  "timemachine.change": "Change the viewing time — currently {at}",
  "timemachine.returnToLive": "Return to Live",
} as const;

export type ChromeKey = keyof typeof en;

/**
 * "Live" carries two different meanings in this chrome and they are two
 * different Russian words on purpose: the NAV item is a real-time event feed
 * («Онлайн»), while the Time Machine's "Live" is the PRESENT MOMENT you left
 * and return to («реальное время»). One word for both would make "Return to
 * Live" read as a link to the events page.
 *
 * "Explore" is «Метрики», not a literal «Исследование»: «Расследование» is
 * already Investigate, and the page is curated metrics plus A/B compare, so
 * the honest noun beats the calque.
 */
export const chromeDict: Dictionary<ChromeKey> = defineDict(en, {
  "nav.overview": "Обзор",
  "nav.live": "Онлайн",
  "nav.investigate": "Расследование",
  "nav.matrix": "Матрица",
  "nav.topology": "Топология",
  "nav.mtr": "MTR",
  "nav.diagnostics": "Диагностика",
  "nav.targets": "Цели и расписания",
  "nav.explore": "Метрики",
  "nav.alerting": "Оповещения",
  "nav.console": "Консоль",
  "nav.settings": "Настройки",

  "nav.group.monitor": "Мониторинг",
  "nav.group.investigate": "Расследование",
  "nav.group.manage": "Управление",

  "sidebar.footer": "Консоль сетевой связности",
  "sidebar.palette.hint": "{keys} — поиск и команды",

  "shell.skipToContent": "Перейти к основному содержимому",
  "shell.nav.aria": "Основная навигация",

  "shell.menu.open": "Открыть навигацию",
  "shell.menu.close": "Закрыть навигацию",
  "shell.menu.aria": "Навигация",

  "banner.anonymous.title": "Анонимный режим.",
  "banner.anonymous.body.role":
    "Аутентификация выключена, у всех роль {role} (console.auth.anonymous.role). Не используйте в продакшене.",
  "banner.anonymous.body":
    "Аутентификация выключена, у всех одна фиксированная роль. Не используйте в продакшене.",

  "timemachine.label": "Машина времени",
  "timemachine.trigger": "Сейчас. Машина времени: посмотреть консоль на момент в прошлом",
  "timemachine.now": "Машина времени: сейчас",
  "timemachine.hint": "Машина времени: пресеты задают глубину окна, а это — момент, из которого вы смотрите.",
  "timemachine.viewing": "Вы смотрите состояние на {at}.",
  "timemachine.viewingHint": "Чтобы что-то менять, вернитесь в реальное время.",
  "timemachine.change": "Изменить момент просмотра, сейчас {at}",
  "timemachine.returnToLive": "Вернуться в реальное время",
});

/**
 * NAV_KEYS maps a nav.ts PATH onto this dictionary's key. It lives here rather
 * than in nav.ts because nav.ts is the route table's source of truth and knows
 * nothing about languages: `label` there stays English and remains the
 * FALLBACK for any path this map has not been taught, which is the same rule
 * components/app-sidebar.tsx already applies to its icons and groups — a new
 * nav entry renders, in English, instead of vanishing.
 */
export const NAV_KEYS: Readonly<Record<string, ChromeKey>> = {
  "/": "nav.overview",
  "/live": "nav.live",
  "/investigate": "nav.investigate",
  "/matrix": "nav.matrix",
  "/topology": "nav.topology",
  "/mtr": "nav.mtr",
  "/diagnostics": "nav.diagnostics",
  "/targets": "nav.targets",
  "/explore": "nav.explore",
  "/alerting": "nav.alerting",
  "/console": "nav.console",
  "/settings": "nav.settings",
};
