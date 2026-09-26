import indexCss from "@/index.css?raw";
import { matchesWidthQuery } from "./viewport-stub";

/**
 * Test support for the page-level phone (375px) and light-theme checks, the way lib/fake-websocket.ts
 * is test support for the socket. jsdom lays nothing out and resolves no CSS, so both checks read the
 * rendered DOM's classes and inline styles, the same structural pins the per-page 375px tests make
 * (pages/overview.test.tsx, pages/targets.test.tsx), applied to every page.
 */

const PHONE_WIDTH = 375;
const REM_PX = 16;

/** A 375px phone less the shell's px-4 gutter on each side. */
export const PHONE_CONTENT_PX = PHONE_WIDTH - 2 * REM_PX;

// Tailwind v4's container scale, which w-*, min-w-* and basis-* also accept.
const CONTAINER_REM: Record<string, number> = {
  "3xs": 16, "2xs": 18, xs: 20, sm: 24, md: 28, lg: 32, xl: 36,
  "2xl": 42, "3xl": 48, "4xl": 56, "5xl": 64, "6xl": 72, "7xl": 80,
};
const BREAKPOINT_VARIANT = /^(sm|md|lg|xl|2xl|min-\[[^\]]*\]|@[a-z0-9]+)$/;
const SCROLLER = new Set(["overflow-auto", "overflow-scroll", "overflow-x-auto", "overflow-x-scroll"]);

function classesOf(el: Element): string[] {
  return (el.getAttribute("class") ?? "").split(/\s+/).filter(Boolean);
}

/** Splits `sm:hover:w-[20rem]` into its variants and the utility, leaving brackets intact. */
function splitVariants(token: string): { variants: string[]; utility: string } {
  const parts: string[] = [];
  let depth = 0;
  let cur = "";
  for (const ch of token) {
    if (ch === "[" || ch === "(") depth++;
    if (ch === "]" || ch === ")") depth--;
    if (ch === ":" && depth === 0) {
      parts.push(cur);
      cur = "";
    } else {
      cur += ch;
    }
  }
  return { variants: parts, utility: cur.replace(/^!/, "") };
}

/** The utilities that apply at 375px: anything behind a breakpoint variant does not. */
function phoneUtilities(el: Element): string[] {
  return classesOf(el)
    .map(splitVariants)
    .filter(({ variants }) => !variants.some((v) => BREAKPOINT_VARIANT.test(v)))
    .map(({ variants, utility }) => (variants.length === 0 ? utility : `${variants.join(":")}:${utility}`));
}

function lengthPx(value: string): number | undefined {
  const container = CONTAINER_REM[value];
  if (container !== undefined) return container * REM_PX;
  if (/^\d+(\.\d+)?$/.test(value)) return Number(value) * 4;
  const arbitrary = /^\[(\d+(?:\.\d+)?)(px|rem)\]$/.exec(value);
  if (arbitrary) return Number(arbitrary[1]) * (arbitrary[2] === "rem" ? REM_PX : 1);
  return undefined;
}

function styleLengthPx(value: string): number | undefined {
  const m = /^(\d+(?:\.\d+)?)(px|rem)$/.exec(value.trim());
  return m ? Number(m[1]) * (m[2] === "rem" ? REM_PX : 1) : undefined;
}

function describeEl(el: Element): string {
  const id = el.getAttribute("data-testid") ?? el.getAttribute("aria-label") ?? el.id;
  const text = (el.textContent ?? "").trim().slice(0, 40);
  return `<${el.tagName.toLowerCase()}${id ? ` "${id}"` : ""}> ${text ? `"${text}"` : ""}`.trim();
}

/**
 * phoneOverflowHazards lists what would push a 375px page sideways: a fixed width, min-width or basis
 * wider than the phone's content box, and a <table>, which cannot wrap, unless it sits inside a
 * horizontal scroller of its own. Subtrees hidden below sm (`hidden sm:flex`) are skipped.
 */
export function phoneOverflowHazards(root: Element): string[] {
  const hazards: string[] = [];
  const walk = (el: Element, scrolled: boolean) => {
    const utilities = phoneUtilities(el);
    if (utilities.includes("hidden") || utilities.includes("sr-only")) return;
    if (!scrolled) {
      if (el.tagName === "TABLE") hazards.push(`table outside a horizontal scroller: ${describeEl(el)}`);
      for (const u of utilities) {
        const m = /^(w|min-w|basis)-(.+)$/.exec(u);
        const px = m ? lengthPx(m[2]) : undefined;
        if (px !== undefined && px > PHONE_CONTENT_PX) hazards.push(`${u} (${px}px): ${describeEl(el)}`);
      }
      // An aria-hidden decoration (the segmented control's thumb) copies a width it MEASURED from a
      // sibling; that sibling is the constraint, and it is checked in its own right.
      const style = el.getAttribute("aria-hidden") === "true" ? undefined : (el as HTMLElement).style;
      for (const prop of ["width", "minWidth", "flexBasis"] as const) {
        const px = style ? styleLengthPx(style[prop] ?? "") : undefined;
        if (px !== undefined && px > PHONE_CONTENT_PX) hazards.push(`style ${prop}: ${px}px: ${describeEl(el)}`);
      }
    }
    const scrollsHere = utilities.some((u) => SCROLLER.has(u));
    for (const child of el.children) walk(child, scrolled || scrollsHere);
  };
  walk(root, false);
  return hazards;
}

/* ── the light theme ─────────────────────────────────────────────────────── */

function block(selector: RegExp): string {
  const m = selector.exec(indexCss);
  if (!m) throw new Error(`index.css has no ${selector} block`);
  let depth = 0;
  const start = m.index + m[0].length;
  for (let i = start; i < indexCss.length; i++) {
    if (indexCss[i] === "{") depth++;
    if (indexCss[i] === "}") {
      if (depth === 0) return indexCss.slice(start, i);
      depth--;
    }
  }
  throw new Error(`index.css ${selector} block is not closed`);
}

function declaredVars(css: string): Set<string> {
  return new Set([...css.matchAll(/(--[a-z0-9-]+)\s*:/g)].map((m) => m[1]));
}

const LIGHT_VARS = declaredVars(block(/\n\.light\s*\{/));
const ROOT_VARS = declaredVars(block(/\n:root\s*\{/));
/** Every colour utility name (`health-bad-soft`) → the variable it reads (`--health-bad-soft`). */
const COLOR_TOKENS = new Map(
  [...block(/@theme inline\s*\{/).matchAll(/--color-([a-z0-9-]+)\s*:\s*hsl\(var\((--[a-z0-9-]+)\)\)/g)].map((m) => [
    m[1],
    m[2],
  ]),
);

const COLOR_UTILITY =
  /^(bg|text|border(?:-[trblxyse])?|ring-offset|ring|outline|divide|fill|stroke|from|via|to|decoration|placeholder|caret|accent|shadow)-(.+)$/;
const PALETTE =
  /^(white|black|(slate|gray|zinc|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose)-\d{2,3})$/;
const LITERAL_COLOR = /#[0-9a-f]{3,8}\b|\b(rgba?|hsla?|oklch|oklab|lab|lch)\((?!\s*var\()/i;

/** A variable the dark `:root` defines and `.light` does not: it would keep its dark value. */
function darkOnly(name: string): boolean {
  return ROOT_VARS.has(name) && !LIGHT_VARS.has(name);
}

function varHazards(value: string): string[] {
  return [...value.matchAll(/var\((--[a-z0-9-]+)/g)].map((m) => m[1]).filter(darkOnly);
}

/**
 * lightThemeHazards lists every colour on the rendered page that the light theme cannot restyle: a
 * literal palette class or colour value, which looks the same on white as on near-black, and a theme
 * token or variable with no `.light` value in index.css.
 */
export function lightThemeHazards(root: Element): string[] {
  const hazards: string[] = [];
  for (const el of [root, ...root.querySelectorAll("*")]) {
    for (const token of classesOf(el)) {
      const { utility } = splitVariants(token);
      const m = COLOR_UTILITY.exec(utility);
      if (!m) continue;
      const value = m[2].replace(/\/(\d+|\[[^\]]+\])$/, "");
      const tokenVar = COLOR_TOKENS.get(value);
      if (tokenVar !== undefined) {
        if (!LIGHT_VARS.has(tokenVar)) hazards.push(`${token}: ${tokenVar} has no light value: ${describeEl(el)}`);
      } else if (PALETTE.test(value)) {
        hazards.push(`${token}: a literal palette colour: ${describeEl(el)}`);
      } else if (value.startsWith("[")) {
        if (LITERAL_COLOR.test(value)) hazards.push(`${token}: a literal colour: ${describeEl(el)}`);
        for (const v of varHazards(value)) hazards.push(`${token}: ${v} has no light value: ${describeEl(el)}`);
      }
    }
    const style = el.getAttribute("style");
    if (style) {
      if (LITERAL_COLOR.test(style)) hazards.push(`style "${style}": a literal colour: ${describeEl(el)}`);
      for (const v of varHazards(style)) hazards.push(`style "${style}": ${v} has no light value: ${describeEl(el)}`);
    }
  }
  return hazards;
}

/** THEME_STORAGE_KEY is components/theme-provider.tsx's key; seeding it is how a test starts in light. */
export const THEME_STORAGE_KEY = "kconmon-console-theme";

/** startInLight makes the next ThemeProvider mount resolve to the light theme. */
export function startInLight(): void {
  localStorage.setItem(THEME_STORAGE_KEY, "light");
}

/** resetTheme undoes startInLight: vitest.setup.ts backs localStorage with one Map per test FILE. */
export function resetTheme(): void {
  localStorage.removeItem(THEME_STORAGE_KEY);
  document.documentElement.classList.remove("light", "dark");
}

/** phoneMatchMedia answers width queries the way a 375px viewport does; anything else does not match. */
export function phoneMatchMedia(query: string): MediaQueryList {
  return {
    matches: matchesWidthQuery(query, PHONE_WIDTH, REM_PX),
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  } as MediaQueryList;
}

let desktop: { matchMedia: typeof window.matchMedia; innerWidth: number } | null = null;

/** emulatePhone makes window.matchMedia and innerWidth answer as a 375px viewport until restoreViewport. */
export function emulatePhone(): void {
  desktop ??= { matchMedia: window.matchMedia, innerWidth: window.innerWidth };
  Object.defineProperty(window, "matchMedia", { configurable: true, writable: true, value: phoneMatchMedia });
  Object.defineProperty(window, "innerWidth", { configurable: true, writable: true, value: PHONE_WIDTH });
}

export function restoreViewport(): void {
  if (desktop === null) return;
  Object.defineProperty(window, "matchMedia", { configurable: true, writable: true, value: desktop.matchMedia });
  Object.defineProperty(window, "innerWidth", { configurable: true, writable: true, value: desktop.innerWidth });
  desktop = null;
}
