import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

/**
 * i18n — the console's en/ru switch. Hand-rolled, zero new dependencies
 * (the same line plan Decision 8 took for the command palette).
 *
 * THE WHOLE CONTRACT IN ONE PLACE. A surface declares its own strings in
 * lib/i18n/dict/<surface>.ts, a component asks for a translator with
 * useT(<surface>Dict), and calls t("some.key"). That is all there is:
 *
 *     import { useT } from "@/lib/i18n";
 *     import { overviewDict } from "@/lib/i18n/dict/overview";
 *     const t = useT(overviewDict);
 *     <h2>{t("worstPairs.title")}</h2>
 *
 * See lib/i18n/README.md for the page-agent how-to. What follows is the why.
 *
 * ── the dictionary is PASSED, not named ──────────────────────────────────
 * useT takes the dictionary OBJECT rather than a surface NAME ("overview")
 * resolved through a central registry. A registry would be one file that every
 * page touches, i.e. exactly the merge conflict a parallel translation effort
 * cannot afford, and it would buy nothing: an import is already a name, the
 * bundler already resolves it, and TypeScript already narrows t's key union to
 * that one dictionary — t("nope") is a compile error, not a runtime miss.
 *
 * ── "en" is the default, ALWAYS ───────────────────────────────────────────
 * No navigator.language sniffing, no Accept-Language, no URL parameter. A
 * console that came up in Russian because the laptop happened to be Russian
 * would flip the language of ~1600 English-pinned assertions the moment the CI
 * image changed its locale, and every one of those failures would be a lie
 * about the code. The ONLY thing that produces a non-English console is an
 * explicit choice in Settings, stored under LOCALE_STORAGE_KEY. This property
 * is load-bearing: do not add sniffing "as a nicety".
 *
 * ── no provider ⇒ English, quietly ────────────────────────────────────────
 * useLocale/useT do NOT throw when there is no <LocaleProvider> above them —
 * unlike useTheme or useTimeMachine, deliberately. Those two guard a WIRING
 * bug (a control outside the provider it drives is broken). This one guards a
 * default that is already correct: hundreds of existing tests render a page or
 * a chrome component with their own minimal provider stack, and every one of
 * them wants English. Throwing would turn "translate one page" into "add a
 * wrapper to forty test files", and the strings would be identical afterwards.
 * setLocale outside a provider is the one case that IS a wiring bug, so it
 * warns in dev instead of failing silently.
 *
 * ── what is NEVER translated ──────────────────────────────────────────────
 * Server-originated text is DATA, and data is not localised: problem+json
 * `title`/`detail`, API error strings, metric names, PromQL, node and target
 * names, event kinds, permission strings, role names, webhook event ids, the
 * product name kconmon-ng, and protocol/tool names (MTR, TCP, DNS, HTTP,
 * PromQL, Prometheus). Rendering a server sentence in Russian would mean this
 * console inventing what the backend said. If the server should speak Russian,
 * that is a server change.
 *
 * ── dates and numbers ─────────────────────────────────────────────────────
 * A stamp INTERPOLATED INTO A TRANSLATED SENTENCE goes through localeTag below:
 * «Вы смотрите состояние на 8/8/2026, 12:00 PM» is a Russian sentence with an
 * American date wedged into it, which is not the viewer's answer to anything
 * (QA round 6, finding #13).
 *
 * A stamp STANDING ON ITS OWN — a table column, a row's time cell — used to be
 * exempt, and that exemption is what put four different date shapes on the
 * Investigate page at once (QA scope 3, finding #7): a row whose clock says
 * "1:23 PM" beside a bar whose column says «8 авг., 13:23» is one page speaking
 * two languages about one instant. The stamp helpers below are the whole answer
 * — every surface formats through one of the four, each takes the locale, and
 * `localeTag` is applied inside rather than at each call site.
 *
 * ── plurals ───────────────────────────────────────────────────────────────
 * There is no plural machinery here, on purpose: Russian has three plural forms
 * and a correct implementation is Intl.PluralRules plus a per-key form table,
 * which is a real feature and not the point of a language switch. A surface
 * that needs "1 узел / 2 узла / 5 узлов" declares SEPARATE KEYS and picks
 * between them itself. README.md has the worked pattern.
 */

/* ── locale ─────────────────────────────────────────────────────────────── */

export type Locale = "en" | "ru";

/** Every locale this console ships, in the order the switcher renders them. */
export const LOCALES = ["en", "ru"] as const;

/** The localStorage key. Namespaced "kconmon." so it reads as ours in a shared
 *  origin; the theme's older, unprefixed "kconmon-console-theme" predates the
 *  convention and is left alone rather than migrated for tidiness. */
export const LOCALE_STORAGE_KEY = "kconmon.locale";

/** English, always — see the module doc. Exported so a test can state the
 *  property it depends on rather than hard-coding "en" in an assertion. */
export const DEFAULT_LOCALE: Locale = "en";

export function isLocale(value: unknown): value is Locale {
  return value === "en" || value === "ru";
}

/**
 * localeTag is the tag a stamp gets when it is interpolated into a TRANSLATED sentence; `undefined`
 * for English keeps the runtime default, which is the viewer's own locale and already reads
 * correctly inside an English sentence.
 */
export function localeTag(locale: Locale): string | undefined {
  return locale === "ru" ? "ru-RU" : undefined;
}

/* ── stamps ─────────────────────────────────────────────────────────────── */

/**
 * The four stamp shapes this console draws, spelled once each so a window shown
 * in a bar, in a row's detail line and in a form reads the same way in all
 * three (QA scope 3, findings #7 and #18).
 *
 * `hour: "numeric"` rather than "2-digit" throughout: a 12-hour locale pads to
 * "08:00 PM", which is not a form English writes anywhere else (finding #17),
 * and a 24-hour locale pads to "20:00" on its own regardless.
 */

/** stampFull is the whole instant, date and clock — a row `title`, a tooltip,
 *  anything that has room for it. */
export function stampFull(d: Date, locale: Locale): string {
  return d.toLocaleString(localeTag(locale));
}

/** stampClock is the clock alone, for a column that is already inside one
 *  known day (the timeline's rows, the signal cursor). */
export function stampClock(d: Date, locale: Locale): string {
  return d.toLocaleTimeString(localeTag(locale));
}

/** stampShort is the compact column: a day and a clock, no year. About 7rem,
 *  which is what a 20rem rail can actually spare. */
export function stampShort(d: Date, locale: Locale): string {
  return d.toLocaleString(localeTag(locale), {
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });
}

/** stampInstant is stampShort plus the year — the date picker's trigger, which
 *  may name a day months away and must not read as "this year, probably". */
export function stampInstant(d: Date, locale: Locale): string {
  return d.toLocaleString(localeTag(locale), {
    month: "short",
    day: "numeric",
    year: "numeric",
    hour: "numeric",
    minute: "2-digit",
  });
}

/* ── dictionaries ───────────────────────────────────────────────────────── */

/** Vars for {placeholder} substitution. Numbers are accepted and stringified
 *  with String(), NOT toLocaleString(): a count inside a sentence is the same
 *  digits in both languages, and formatting is not this module's business. */
export type Vars = Record<string, string | number>;

export type Translate<K extends string> = (key: K, vars?: Vars) => string;

/**
 * A Dictionary is one surface's strings in both languages, keyed identically.
 * Build one with defineDict — never by hand, or the key parity below is a
 * promise the type system has not checked.
 */
export interface Dictionary<K extends string> {
  readonly en: Readonly<Record<K, string>>;
  readonly ru: Readonly<Record<K, string>>;
}

/**
 * defineDict pairs an English table with its Russian one and makes DIVERGENCE
 * A COMPILE ERROR, which is the whole reason this helper exists rather than a
 * bare object literal:
 *
 *   - a key in `en` with no `ru` counterpart      → error (missing property)
 *   - a key in `ru` that `en` never declared      → error (excess property)
 *   - a value that is not a string                → error
 *
 * So "the Russian half fell behind" is a state this repo cannot reach through
 * the type checker; the runtime fallback below exists for the one path types
 * cannot see (a dictionary reaching here from untyped JS), not as a licence to
 * ship half a table.
 *
 * `en` is the SOURCE table: it is the fallback, it is what every existing
 * English-pinned test reads, and its keys define the union t() accepts.
 */
export function defineDict<E extends Record<string, string>>(
  en: E,
  ru: { [K in keyof E]: string },
): Dictionary<Extract<keyof E, string>> {
  return { en, ru };
}

/**
 * interpolate substitutes {placeholders}. The whole of it: no plurals, no
 * dates, no nesting, no expressions.
 *
 * An unknown placeholder is left VERBATIM rather than replaced with "" — a
 * typo should show up as "{nmae}" on the page, where it gets fixed, instead of
 * as a hole in a sentence that reads like it was written that way.
 */
export function interpolate(template: string, vars?: Vars): string {
  if (vars === undefined) return template;
  return template.replace(/\{(\w+)\}/g, (whole, name: string) =>
    Object.hasOwn(vars, name) ? String(vars[name]) : whole,
  );
}

/**
 * translate is the resolution rule, pure and exported so the fallback is
 * testable without a render.
 *
 * FALLBACK: the requested locale's string, else the ENGLISH one. Never the key
 * name — an operator reading "timemachine.returnToLive" in place of a button
 * label has been handed a bug report they cannot act on, while the English
 * string is at worst the wrong language and at best exactly what they needed.
 * The key itself is returned only when English is missing too, which the types
 * above make unreachable from TypeScript; if you ever see one on screen, a
 * dictionary was assembled without defineDict.
 */
export function translate<K extends string>(
  dict: Dictionary<K>,
  locale: Locale,
  key: K,
  vars?: Vars,
): string {
  const table = dict[locale] as Readonly<Record<string, string | undefined>>;
  const fallback = dict.en as Readonly<Record<string, string | undefined>>;
  return interpolate(table[key] ?? fallback[key] ?? key, vars);
}

/* ── context ────────────────────────────────────────────────────────────── */

export interface LocaleContextValue {
  locale: Locale;
  setLocale: (locale: Locale) => void;
}

function setLocaleWithoutProvider(): void {
  // Dev-only and deliberately loud: reading outside the provider is fine (you
  // get English, which is the default anyway), but WRITING outside it silently
  // drops an operator's explicit choice, and that is a wiring bug.
  if (import.meta.env.DEV) {
    console.warn("[i18n] setLocale() was called outside <LocaleProvider>; the choice was dropped.");
  }
}

const LocaleContext = createContext<LocaleContextValue>({
  locale: DEFAULT_LOCALE,
  setLocale: setLocaleWithoutProvider,
});

/**
 * LocaleProvider owns the choice and its persistence. Mounted once, in
 * main.tsx, above the router — the language of the chrome must not depend on
 * which route is on screen.
 *
 * The stored value is written on CHANGE, not on mount (which is where
 * components/theme-provider.tsx writes its own). An absent key therefore means
 * "never chose", not "chose English", and stays absent for every operator who
 * never opens the switcher — the console does not write to an operator's
 * browser to record that they did nothing.
 */
export function LocaleProvider({ children }: { children: ReactNode }) {
  const [locale, setLocaleState] = useState<Locale>(readStoredLocale);

  // <html lang> is how a screen reader picks its pronunciation rules; Russian
  // read with English phonetics is unusable. This is the one piece of DOM this
  // module owns, and it mirrors what ThemeProvider does with the dark/light
  // class on the same element.
  useEffect(() => {
    document.documentElement.lang = locale;
  }, [locale]);

  const setLocale = useCallback((next: Locale) => {
    setLocaleState(next);
    try {
      localStorage.setItem(LOCALE_STORAGE_KEY, next);
    } catch {
      // A storage quota or a locked-down private mode must not take the
      // switcher down with it: the choice still applies for this session.
    }
  }, []);

  const value = useMemo<LocaleContextValue>(() => ({ locale, setLocale }), [locale, setLocale]);
  return <LocaleContext.Provider value={value}>{children}</LocaleContext.Provider>;
}

function readStoredLocale(): Locale {
  try {
    const stored = localStorage.getItem(LOCALE_STORAGE_KEY);
    // Anything else — a stale value, a hand-edited one, a locale a later build
    // dropped — reads as "no choice" rather than as an error.
    return isLocale(stored) ? stored : DEFAULT_LOCALE;
  } catch {
    return DEFAULT_LOCALE;
  }
}

/** useLocale is the SWITCHER's hook: the current locale and the way to change
 *  it. A component that only renders strings wants useT instead. */
export function useLocale(): LocaleContextValue {
  return useContext(LocaleContext);
}

/**
 * useT returns this surface's translator, re-created only when the locale (or
 * the dictionary identity) actually changes, so it is safe in a dependency
 * array and cheap to call in a list.
 */
export function useT<K extends string>(dict: Dictionary<K>): Translate<K> {
  const { locale } = useLocale();
  return useMemo<Translate<K>>(
    () => (key, vars) => translate(dict, locale, key, vars),
    [dict, locale],
  );
}
