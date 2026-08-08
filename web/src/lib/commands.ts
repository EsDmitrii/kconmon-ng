import { NAV_ITEMS } from "@/nav";

/**
 * commands.ts — the command palette's REGISTRY and its SCORING, both pure.
 *
 * Split from components/command-palette.tsx on purpose: everything here is a
 * function of a plain context object, so the whole of "what can be invoked,
 * who may see it, and how a query ranks it" is unit-testable without a DOM, a
 * router or a query client. The component owns keys, focus and paint; this
 * file owns meaning.
 *
 * HONEST SCOPE (plan Decision 8, and the PAGES.md rewrite that rides with
 * it): M7 ships a registry the PALETTE owns. The sidebar links, the page
 * buttons and the bars were NOT migrated onto it — an entry here is a second
 * way to reach a surface, not the single definition of an affordance. The one
 * place that is genuinely shared is navigation: the entries below are built
 * from NAV_ITEMS itself (imported, never copied), so a nav label or
 * description can never drift away from what the palette says.
 *
 * PERMISSIONS. `can` takes a bare string because that is what use-auth
 * exposes (`can: (p: string) => boolean`, over `Me.permissions: string[]`);
 * there is no narrower union anywhere in the TS surface, and inventing one
 * here would be a second source of truth for the Go role tables. `Permission`
 * is an alias, present so the intent of the field reads at the call site.
 */
export type Permission = string;

/**
 * CommandContext is everything a command may touch. Flat and boring by
 * design: a command must never reach for a hook, a store or the DOM itself,
 * because then it stops being testable and starts being a component.
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
  /**
   * Opens the Time Machine BAR's own picker. Engaging needs an instant, and
   * the palette has no honest one to offer — see the TIME MACHINE note under
   * buildRegistry.
   */
  openTimeMachinePicker: () => void;
}

export type CommandGroup = "Navigation" | "Actions" | "View";

/** GROUP_ORDER is the order the palette renders its section headers in. */
export const GROUP_ORDER: CommandGroup[] = ["Navigation", "Actions", "View"];

export interface Command {
  id: string;
  title: string;
  group: CommandGroup;
  /** Extra text the query may match, ranked below the title. */
  keywords?: string[];
  perform: (ctx: CommandContext) => void;
  /** HIDE=permission: absent permission ⇒ the entry is not in the registry. */
  permission?: Permission;
  /** Any other reason to leave the entry out entirely (Time Machine state). */
  visibleWhen?: (ctx: CommandContext) => boolean;
  /**
   * DISABLE=time: this entry leads to CREATING something, so it stays VISIBLE
   * and goes disabled while the Time Machine is engaged. Additive to the
   * shape the brief sketched — a boolean rather than a second predicate,
   * because there is exactly one rule and it must not be restated per entry.
   */
  write?: boolean;
}

/* ---------------------------------------------------------------- scoring */

/* The ladder, in one place so the ordering the tests pin is readable as data
   rather than inferred from branches. Title beats keyword at every rung; a
   word boundary beats a mid-word substring; the very start of the title beats
   a boundary further in. */
const TITLE_START = 120;
const TITLE_BOUNDARY = 100;
const TITLE_SUBSTRING = 60;
const KEYWORD_BOUNDARY = 40;
const KEYWORD_SUBSTRING = 25;

/** boundaryIndex finds `needle` at a word boundary — start of string, or
 *  preceded by anything that is not a letter or a digit. */
function boundaryIndex(hay: string, needle: string): number {
  let from = 0;
  for (;;) {
    const i = hay.indexOf(needle, from);
    if (i < 0) return -1;
    if (i === 0 || !/[a-z0-9]/.test(hay[i - 1])) return i;
    from = i + 1;
  }
}

function wordScore(word: string, title: string, keywords: string[]): number {
  const b = boundaryIndex(title, word);
  if (b === 0) return TITLE_START;
  if (b > 0) return TITLE_BOUNDARY;
  if (title.includes(word)) return TITLE_SUBSTRING;
  let best = 0;
  for (const k of keywords) {
    if (boundaryIndex(k, word) >= 0) best = Math.max(best, KEYWORD_BOUNDARY);
    else if (k.includes(word)) best = Math.max(best, KEYWORD_SUBSTRING);
  }
  return best;
}

/**
 * scoreCommand ranks one command against one query. 0 means "filtered out",
 * never "matched badly" — the palette shows nothing that scores 0.
 *
 * Multi-word queries are an AND: every word must land somewhere (title or a
 * keyword), and the total is the sum, so "alert rule" ranks a command that
 * carries both above one that carries either. That is the whole of the "fuzzy"
 * promise — substring and word-boundary matching, no edit distance, no
 * subsequence matching, no library (plan Decision 8: no cmdk, no kbar).
 *
 * An empty (or whitespace-only) query scores 1 for everything: the palette
 * opens showing the entire registry rather than an empty box.
 */
export function scoreCommand(query: string, cmd: Command): number {
  const words = query.toLowerCase().trim().split(/\s+/).filter(Boolean);
  if (words.length === 0) return 1;
  const title = cmd.title.toLowerCase();
  const keywords = (cmd.keywords ?? []).map((k) => k.toLowerCase());
  let total = 0;
  for (const w of words) {
    const s = wordScore(w, title, keywords);
    if (s === 0) return 0;
    total += s;
  }
  return total;
}

/**
 * searchCommands is the ranked, filtered list the palette renders. The
 * tie-break is the TITLE (then the id), never the input order: two entries
 * that score the same must land in the same place whichever way the registry
 * was assembled, or the highlighted row moves for reasons the user cannot see.
 */
export function searchCommands(query: string, cmds: Command[]): Command[] {
  return cmds
    .map((cmd) => ({ cmd, score: scoreCommand(query, cmd) }))
    .filter((r) => r.score > 0)
    .sort((a, b) => b.score - a.score || cmpStr(a.cmd.title, b.cmd.title) || cmpStr(a.cmd.id, b.cmd.id))
    .map((r) => r.cmd);
}

/* Plain code-unit comparison rather than localeCompare: this is a tie-break
   that must be identical in every browser and in jsdom, not a human-facing
   collation. */
function cmpStr(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/**
 * isCommandDisabled is the DISABLE=time half of the two gates
 * lib/timemachine.tsx documents. Permissions HIDE (they never reach here);
 * time DISABLES, and only for entries that lead to creating something.
 */
export function isCommandDisabled(cmd: Command, ctx: CommandContext): boolean {
  return cmd.write === true && ctx.writesDisabled;
}

/* --------------------------------------------------------------- registry */

function navCommands(): Command[] {
  return NAV_ITEMS.map((item) => ({
    id: `nav:${item.path}`,
    title: item.label,
    group: "Navigation" as const,
    // nav.ts's own description is the keyword text, so "worst pairs" finds
    // Overview without this file holding a second copy of that sentence.
    keywords: [item.description, item.path],
    perform: (ctx: CommandContext) => ctx.navigate(item.path),
  }));
}

/**
 * ACTIONS are deep links, not inline forms. Every one of them lands on the
 * page that already owns the affordance (with its own permission checks, its
 * own Time-Machine disabling and its own validation) rather than reproducing
 * a create form inside a 480px overlay. The ellipsis in each title says so.
 *
 * DESTINATIONS, chosen from where the affordance actually lives today:
 *   maintenance  → /explore   MaintenanceBar at global scope sits at the top
 *                             of the page (pages/explore.tsx). /settings is
 *                             the other candidate and is still a stub whose
 *                             M7 scope (plan Decision 10) is webhooks +
 *                             export/import, NOT maintenance.
 *   annotation   → /explore   AnnotationBar at global scope, same place. The
 *                             other mounts (node / pair / target cards,
 *                             /investigate) are all scoped to an object the
 *                             palette has not been told about.
 */
function actionCommands(): Command[] {
  return [
    {
      id: "action:run-check",
      title: "Run a diagnostic check…",
      group: "Actions",
      keywords: ["diagnostics", "tcp", "udp", "icmp", "dns", "http", "run"],
      permission: "runs:create",
      write: true,
      perform: (ctx) => ctx.navigate("/diagnostics"),
    },
    {
      id: "action:investigate",
      title: "Start an investigation…",
      group: "Actions",
      keywords: ["incident", "timeline", "root cause", "correlate"],
      // No permission and no write flag: opening Investigation Mode reads,
      // and the page hides/disables its own save-as-incident affordance.
      perform: (ctx) => ctx.navigate("/investigate"),
    },
    {
      id: "action:alert-rule",
      title: "Create an alert rule…",
      group: "Actions",
      keywords: ["alerting", "prometheus", "rule", "severity", "threshold"],
      permission: "alerts:manage",
      write: true,
      perform: (ctx) => ctx.navigate("/alerting"),
    },
    {
      id: "action:maintenance",
      title: "Declare a maintenance window…",
      group: "Actions",
      keywords: ["downtime", "change window", "planned", "explore"],
      permission: "maintenance:write",
      write: true,
      perform: (ctx) => ctx.navigate("/explore"),
    },
    {
      id: "action:annotation",
      title: "Add an annotation…",
      group: "Actions",
      keywords: ["note", "marker", "comment", "explore"],
      permission: "annotations:write",
      write: true,
      perform: (ctx) => ctx.navigate("/explore"),
    },
  ];
}

/**
 * VIEW entries change how the console is being looked at rather than what is
 * in it.
 *
 * TIME MACHINE — what "toggle" honestly means here. Returning to Live is a
 * complete action: it takes no argument and lib/timemachine.tsx exposes it
 * directly, so the palette calls it. ENGAGING is not: it needs an instant,
 * and a palette that picked one for you ("an hour ago"?) would be inventing
 * the answer to the only question that matters. So the ON direction opens the
 * TimeMachineBar's existing picker and hands the choice back to the user —
 * one entry per direction, mutually exclusive on `isLive`, and neither
 * required a line of lib/timemachine.tsx to change.
 */
function viewCommands(ctx: CommandContext): Command[] {
  return [
    {
      id: "view:timemachine-pick",
      title: "Toggle Time Machine — pick a time…",
      group: "View",
      keywords: ["history", "past", "at", "replay", "rewind"],
      visibleWhen: (c) => c.isLive,
      perform: (c) => c.openTimeMachinePicker(),
    },
    {
      id: "view:timemachine-live",
      title: "Return to Live",
      group: "View",
      keywords: ["time machine", "now", "present", "toggle"],
      visibleWhen: (c) => !c.isLive,
      perform: (c) => c.returnToLive(),
    },
    {
      // The label names the theme it switches TO — the same convention (and
      // the same wording) components/theme-toggle.tsx's aria-label uses, so
      // the two controls cannot describe one click two ways.
      id: "view:theme",
      title: `Switch to ${ctx.theme === "dark" ? "light" : "dark"} theme`,
      group: "View",
      keywords: ["theme", "dark", "light", "appearance", "contrast"],
      perform: (c) => c.toggleTheme(),
    },
  ];
}

/**
 * buildRegistry assembles the palette's whole vocabulary for one subject in
 * one console state, with the HIDE gates already applied: what comes back is
 * what may be SEEN. Disabling (DISABLE=time) is deliberately left to
 * isCommandDisabled, because a disabled entry must still be rendered.
 */
export function buildRegistry(ctx: CommandContext): Command[] {
  return [...navCommands(), ...actionCommands(), ...viewCommands(ctx)].filter(
    (c) => (c.permission === undefined || ctx.can(c.permission)) && (c.visibleWhen?.(ctx) ?? true),
  );
}
