import { DEFAULT_LOCALE, type Locale } from "@/lib/i18n";
import { chromeDict, NAV_KEYS } from "@/lib/i18n/dict/chrome";
import { NAV_DESC_KEYS, paletteDict, type PaletteKey } from "@/lib/i18n/dict/palette";
import { NAV_ITEMS } from "@/nav";

/** commands.ts — the command palette's REGISTRY and its SCORING. */
export type Permission = string;

/* ── opening the palette from somewhere that owns the keyboard ───────────── */

/**
 * PALETTE_OPEN_EVENT is the one-line bus that lets a surface with its OWN keymap open the palette;
 * the palette's hotkey is a document keydown listener.
 */
export const PALETTE_OPEN_EVENT = "kconmon:open-palette";

/** openCommandPalette asks whatever palette is mounted to open. A no-op when
 *  none is (the event simply has no listener) — deliberately, because a
 *  keybinding must never throw at whoever pressed it. */
export function openCommandPalette(): void {
  window.dispatchEvent(new CustomEvent(PALETTE_OPEN_EVENT));
}

/**
 * CommandContext is everything a command may touch; flat and boring by design: a command must never
 * reach for a hook, a store or the DOM itself.
 */
export interface CommandContext {
  /** use-auth's predicate, verbatim. */
  can: (p: Permission) => boolean;
  /** useWritesDisabled() — true while the Time Machine is engaged. */
  writesDisabled: boolean;
  /** Router navigation (the component supplies TanStack's navigate). */
  navigate: (path: string) => void;
  theme: "dark" | "light";
  toggleTheme: () => void;
  isLive: boolean;
  returnToLive: () => void;
  /** Opens the Time Machine BAR's own picker. */
  openTimeMachinePicker: () => void;
}

/** CommandGroup stays an ENGLISH UNION, because it is a type before it is a word: entries are declared with it. */
export type CommandGroup = "Navigation" | "Actions" | "View";

/** GROUP_ORDER is the order the palette renders its section headers in. */
export const GROUP_ORDER: CommandGroup[] = ["Navigation", "Actions", "View"];

/** GROUP_KEYS turns a group into the dictionary key that names it on screen. */
export const GROUP_KEYS: Readonly<Record<CommandGroup, PaletteKey>> = {
  Navigation: "group.Navigation",
  Actions: "group.Actions",
  View: "group.View",
};

export interface Command {
  id: string;
  /** ENGLISH, and the SOURCE title: the display fallback, half the search
   *  corpus, and what every ranking fixture in commands.test.ts reads. */
  title: string;
  /** The Russian title. Optional — an entry without one displays `title` in
   *  both locales, which is the right answer for anything named after a
   *  machine thing. Indexed for search in BOTH locales when present. */
  titleRu?: string;
  group: CommandGroup;
  /** Extra text the query may match, ranked below the title. One blob per
   *  language: see the module doc for why the corpus is bilingual. */
  keywords?: string[];
  perform: (ctx: CommandContext) => void;
  /** HIDE=permission: absent permission ⇒ the entry is not in the registry. */
  permission?: Permission;
  /** Any other reason to leave the entry out entirely (Time Machine state). */
  visibleWhen?: (ctx: CommandContext) => boolean;
  /** Additive to the shape sketched — a boolean rather than a second predicate. */
  write?: boolean;
}

/* ---------------------------------------------------------------- scoring */

/* The ladder, in one place so the ordering the tests pin is readable as data rather than inferred from branches. */
const TITLE_START = 120;
const TITLE_BOUNDARY = 100;
const TITLE_SUBSTRING = 60;
const KEYWORD_BOUNDARY = 40;
const KEYWORD_SUBSTRING = 25;

/**
 * WORD_CHAR is what counts as "inside a word", and Cyrillic is in it; the original class was
 * /[a-z0-9]/, which was correct for an English-only corpus and quietly wrong the moment Russian
 * titles joined.
 */
const WORD_CHAR = /[a-z0-9Ѐ-ӿ]/;

/** boundaryIndex finds `needle` at a word boundary — start of string, or
 *  preceded by anything that is not a letter or a digit. */
function boundaryIndex(hay: string, needle: string): number {
  let from = 0;
  for (;;) {
    const i = hay.indexOf(needle, from);
    if (i < 0) return -1;
    if (i === 0 || !WORD_CHAR.test(hay[i - 1])) return i;
    from = i + 1;
  }
}

/**
 * titleScore ranks one query word against ALL of an entry's titles; the BEST rung across the
 * languages wins rather than the first language that matches.
 */
function titleScore(word: string, titles: readonly string[]): number {
  let best = 0;
  for (const title of titles) {
    const b = boundaryIndex(title, word);
    if (b === 0) return TITLE_START;
    if (b > 0) best = Math.max(best, TITLE_BOUNDARY);
    else if (title.includes(word)) best = Math.max(best, TITLE_SUBSTRING);
  }
  return best;
}

function wordScore(word: string, titles: readonly string[], keywords: string[]): number {
  const t = titleScore(word, titles);
  if (t > 0) return t;
  let best = 0;
  for (const k of keywords) {
    if (boundaryIndex(k, word) >= 0) best = Math.max(best, KEYWORD_BOUNDARY);
    else if (k.includes(word)) best = Math.max(best, KEYWORD_SUBSTRING);
  }
  return best;
}

/** commandTitle is what an entry READS AS; the one place the display language is decided, so the rendered row. */
export function commandTitle(cmd: Command, locale: Locale): string {
  return locale === "ru" ? (cmd.titleRu ?? cmd.title) : cmd.title;
}

/** searchTitles is the SEARCH set — every language this entry has a name in,
 *  regardless of which one is on screen. */
function searchTitles(cmd: Command): string[] {
  const titles = [cmd.title.toLowerCase()];
  if (cmd.titleRu !== undefined && cmd.titleRu !== cmd.title) titles.push(cmd.titleRu.toLowerCase());
  return titles;
}

/** scoreCommand ranks one command against one query; an empty (or whitespace-only) query scores 1 for everything. */
export function scoreCommand(query: string, cmd: Command): number {
  const words = query.toLowerCase().trim().split(/\s+/).filter(Boolean);
  if (words.length === 0) return 1;
  const titles = searchTitles(cmd);
  const keywords = (cmd.keywords ?? []).map((k) => k.toLowerCase());
  let total = 0;
  for (const w of words) {
    const s = wordScore(w, titles, keywords);
    if (s === 0) return 0;
    total += s;
  }
  return total;
}

/**
 * searchCommands is the ranked, filtered list the palette renders; the tie-break is the TITLE (then
 * the id), never the input order.
 */
export function searchCommands(query: string, cmds: Command[], locale: Locale = DEFAULT_LOCALE): Command[] {
  return cmds
    .map((cmd) => ({ cmd, score: scoreCommand(query, cmd) }))
    .filter((r) => r.score > 0)
    .sort(
      (a, b) =>
        b.score - a.score ||
        cmpStr(commandTitle(a.cmd, locale), commandTitle(b.cmd, locale)) ||
        cmpStr(a.cmd.id, b.cmd.id),
    )
    .map((r) => r.cmd);
}

/* Plain code-unit comparison rather than localeCompare: this is a tie-break
   that must be identical in every browser and in jsdom, not a human-facing
   collation. */
function cmpStr(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/**
 * isCommandDisabled is the DISABLE=time half of the two gates lib/timemachine.tsx documents;
 * permissions HIDE (they never reach here).
 */
export function isCommandDisabled(cmd: Command, ctx: CommandContext): boolean {
  return cmd.write === true && ctx.writesDisabled;
}

/* --------------------------------------------------------------- registry */

/* en / ru read the palette's table as DATA — this module is hookless on
   purpose (see the module doc), and a Dictionary is a plain object. */
const en = (key: PaletteKey): string => paletteDict.en[key];
const ru = (key: PaletteKey): string => paletteDict.ru[key];

/** kw is one entry's whole keyword corpus: the English blob and the Russian
 *  one, side by side, always both. */
function kw(key: PaletteKey): string[] {
  return [en(key), ru(key)];
}

function navCommands(): Command[] {
  return NAV_ITEMS.map((item) => {
    /*
     * The label comes from the CHROME dictionary, through the same NAV_KEYS map the sidebar renders
     * its links from.
     */
    const labelKey = NAV_KEYS[item.path];
    const descKey = NAV_DESC_KEYS[item.path];
    return {
      id: `nav:${item.path}`,
      title: item.label,
      titleRu: labelKey ? chromeDict.ru[labelKey] : undefined,
      group: "Navigation" as const,
      /* nav.ts's own description is the keyword text. */
      keywords: descKey ? [item.description, ru(descKey), item.path] : [item.description, item.path],
      perform: (ctx: CommandContext) => ctx.navigate(item.path),
    };
  });
}

/**
 * ACTIONS are deep links, not inline forms; every one of them lands on the page that already owns
 * the affordance (with its own permission checks, its own Time-Machine disabling and its own
 * validation) rather than reproducing a create form inside a 480px overlay.
 */
function actionCommands(): Command[] {
  return [
    {
      id: "action:run-check",
      title: en("action.runCheck"),
      titleRu: ru("action.runCheck"),
      group: "Actions",
      keywords: kw("action.runCheck.kw"),
      permission: "runs:create",
      write: true,
      perform: (ctx) => ctx.navigate("/diagnostics"),
    },
    {
      id: "action:investigate",
      title: en("action.investigate"),
      titleRu: ru("action.investigate"),
      group: "Actions",
      keywords: kw("action.investigate.kw"),
      // No permission and no write flag: opening Investigation Mode reads,
      // and the page hides/disables its own save-as-incident affordance.
      perform: (ctx) => ctx.navigate("/investigate"),
    },
    {
      id: "action:alert-rule",
      title: en("action.alertRule"),
      titleRu: ru("action.alertRule"),
      group: "Actions",
      keywords: kw("action.alertRule.kw"),
      permission: "alerts:manage",
      write: true,
      perform: (ctx) => ctx.navigate("/alerting"),
    },
    {
      id: "action:maintenance",
      title: en("action.maintenance"),
      titleRu: ru("action.maintenance"),
      group: "Actions",
      keywords: kw("action.maintenance.kw"),
      permission: "maintenance:write",
      write: true,
      perform: (ctx) => ctx.navigate("/explore"),
    },
    {
      id: "action:annotation",
      title: en("action.annotation"),
      titleRu: ru("action.annotation"),
      group: "Actions",
      keywords: kw("action.annotation.kw"),
      permission: "annotations:write",
      write: true,
      perform: (ctx) => ctx.navigate("/explore"),
    },
  ];
}

/** VIEW entries change how the console is being looked at rather than what is in it. */
function viewCommands(ctx: CommandContext): Command[] {
  return [
    {
      id: "view:timemachine-pick",
      title: en("view.timemachinePick"),
      titleRu: ru("view.timemachinePick"),
      group: "View",
      keywords: kw("view.timemachinePick.kw"),
      visibleWhen: (c) => c.isLive,
      perform: (c) => c.openTimeMachinePicker(),
    },
    {
      id: "view:timemachine-live",
      /* The same two words chrome.ts's Time Machine bar uses for the same
         click — «Вернуться в реальное время», never a second wording. */
      title: en("view.timemachineLive"),
      titleRu: ru("view.timemachineLive"),
      group: "View",
      keywords: kw("view.timemachineLive.kw"),
      visibleWhen: (c) => !c.isLive,
      perform: (c) => c.returnToLive(),
    },
    {
      // The label names the theme it switches TO.
      id: "view:theme",
      title: ctx.theme === "dark" ? en("view.themeLight") : en("view.themeDark"),
      titleRu: ctx.theme === "dark" ? ru("view.themeLight") : ru("view.themeDark"),
      group: "View",
      keywords: kw("view.theme.kw"),
      perform: (c) => c.toggleTheme(),
    },
  ];
}

/**
 * buildRegistry assembles the palette's whole vocabulary for one subject in one console state;
 * disabling (DISABLE=time) is deliberately left to isCommandDisabled.
 */
export function buildRegistry(ctx: CommandContext): Command[] {
  return [...navCommands(), ...actionCommands(), ...viewCommands(ctx)].filter(
    (c) => (c.permission === undefined || ctx.can(c.permission)) && (c.visibleWhen?.(ctx) ?? true),
  );
}
