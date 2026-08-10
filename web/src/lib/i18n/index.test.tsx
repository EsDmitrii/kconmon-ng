import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  DEFAULT_LOCALE,
  LOCALE_STORAGE_KEY,
  LocaleProvider,
  defineDict,
  interpolate,
  isLocale,
  stampClock,
  stampFull,
  stampInstant,
  stampShort,
  translate,
  useLocale,
  useT,
  type Dictionary,
} from "@/lib/i18n";

/**
 * The i18n CONTRACT, pinned. Every one of these is a property some other file
 * is allowed to depend on:
 *
 *   - "en" is what you get unless someone explicitly chose otherwise, which is
 *     what keeps the ~1600 English-pinned assertions in this suite honest;
 *   - a missing Russian string renders the English one, never a key name;
 *   - a choice survives a reload, and nothing else writes to storage;
 *   - {placeholders} substitute, and an unknown one stays visible.
 *
 * lib/i18n/chrome.test.tsx is the other half: the same contract observed
 * through the real sidebar and top bars.
 */

/* ── fixtures ───────────────────────────────────────────────────────────── */

const probeDict = defineDict(
  {
    greeting: "Hello, {name}",
    plain: "Plain English",
    counted: "{count} pairs",
  } as const,
  {
    greeting: "Привет, {name}",
    plain: "Просто по-русски",
    counted: "{count} пар",
  },
);

/**
 * A dictionary whose Russian half has a HOLE. defineDict cannot build this —
 * that is the whole point of defineDict — so it is asserted into shape here,
 * which is exactly the shape a dictionary reaching this module from untyped JS
 * would have. It exists to prove the runtime fallback, not to model anything
 * this repo is allowed to write.
 */
const holedDict = {
  en: { kept: "Kept", dropped: "Dropped" },
  ru: { kept: "Сохранено" },
} as unknown as Dictionary<"kept" | "dropped">;

function Probe() {
  const { locale, setLocale } = useLocale();
  const t = useT(probeDict);
  return (
    <div>
      <span data-testid="locale">{locale}</span>
      <span data-testid="greeting">{t("greeting", { name: "Ада" })}</span>
      <span data-testid="plain">{t("plain")}</span>
      <button onClick={() => setLocale("ru")}>to ru</button>
      <button onClick={() => setLocale("en")}>to en</button>
    </div>
  );
}

const locale = () => screen.getByTestId("locale").textContent;
const plain = () => screen.getByTestId("plain").textContent;
const greeting = () => screen.getByTestId("greeting").textContent;

afterEach(() => {
  cleanup();
  // vitest.setup.ts backs localStorage with ONE Map per test file, so a locale
  // left behind here would leak into every case below it.
  localStorage.removeItem(LOCALE_STORAGE_KEY);
  vi.restoreAllMocks();
});

/* ── pure helpers ───────────────────────────────────────────────────────── */

describe("isLocale", () => {
  it("accepts exactly the two shipped locales", () => {
    expect(isLocale("en")).toBe(true);
    expect(isLocale("ru")).toBe(true);
    expect(isLocale("de")).toBe(false);
    expect(isLocale(null)).toBe(false);
    expect(isLocale("")).toBe(false);
  });
});

describe("interpolate", () => {
  it("substitutes named placeholders", () => {
    expect(interpolate("Hello, {name}", { name: "Ада" })).toBe("Hello, Ада");
  });

  it("substitutes the same placeholder everywhere it appears", () => {
    expect(interpolate("{n} of {n}", { n: 3 })).toBe("3 of 3");
  });

  it("stringifies numbers without formatting them", () => {
    // No thousands separator, no locale digits: a count inside a sentence is
    // the same digits in both languages, and formatting is not i18n's job.
    expect(interpolate("{count} pairs", { count: 1234 })).toBe("1234 pairs");
  });

  it("leaves an unknown placeholder VERBATIM rather than blanking it", () => {
    // A typo has to be visible on the page, not a hole in a sentence that
    // reads like it was written that way.
    expect(interpolate("Hello, {nmae}", { name: "Ада" })).toBe("Hello, {nmae}");
  });

  it("returns the template untouched when there are no vars at all", () => {
    expect(interpolate("Nothing to do {here}")).toBe("Nothing to do {here}");
  });
});

describe("translate", () => {
  it("reads the requested locale", () => {
    expect(translate(probeDict, "ru", "plain")).toBe("Просто по-русски");
    expect(translate(probeDict, "en", "plain")).toBe("Plain English");
  });

  it("falls back to the ENGLISH string when the Russian one is missing", () => {
    expect(translate(holedDict, "ru", "kept")).toBe("Сохранено");
    expect(translate(holedDict, "ru", "dropped")).toBe("Dropped");
  });

  it("never renders a key name in place of a string", () => {
    expect(translate(holedDict, "ru", "dropped")).not.toContain("dropped");
  });

  it("interpolates in whichever locale answered", () => {
    expect(translate(probeDict, "ru", "greeting", { name: "Ада" })).toBe("Привет, Ада");
    expect(translate(probeDict, "en", "greeting", { name: "Ada" })).toBe("Hello, Ada");
  });
});

describe("defineDict", () => {
  it("keeps both halves keyed identically", () => {
    // The type checker owns this rule; the test states it so the invariant is
    // readable as an invariant rather than inferred from a mapped type.
    expect(Object.keys(probeDict.ru).sort()).toEqual(Object.keys(probeDict.en).sort());
  });
});

/* ── the provider ───────────────────────────────────────────────────────── */

describe("LocaleProvider", () => {
  it("defaults to English with nothing stored — the load-bearing default", () => {
    render(
      <LocaleProvider>
        <Probe />
      </LocaleProvider>,
    );
    expect(locale()).toBe(DEFAULT_LOCALE);
    expect(locale()).toBe("en");
    expect(plain()).toBe("Plain English");
  });

  it("does not write to storage for an operator who never chose", () => {
    render(
      <LocaleProvider>
        <Probe />
      </LocaleProvider>,
    );
    expect(localStorage.getItem(LOCALE_STORAGE_KEY)).toBeNull();
  });

  it("restores a stored choice on mount", () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    render(
      <LocaleProvider>
        <Probe />
      </LocaleProvider>,
    );
    expect(locale()).toBe("ru");
    expect(plain()).toBe("Просто по-русски");
  });

  it("reads a junk stored value as 'never chose' rather than failing", () => {
    localStorage.setItem(LOCALE_STORAGE_KEY, "klingon");
    render(
      <LocaleProvider>
        <Probe />
      </LocaleProvider>,
    );
    expect(locale()).toBe("en");
  });

  it("re-renders every consumer on the spot, with no reload", () => {
    render(
      <LocaleProvider>
        <Probe />
      </LocaleProvider>,
    );
    expect(greeting()).toBe("Hello, Ада");
    fireEvent.click(screen.getByRole("button", { name: "to ru" }));
    expect(locale()).toBe("ru");
    expect(greeting()).toBe("Привет, Ада");
    fireEvent.click(screen.getByRole("button", { name: "to en" }));
    expect(greeting()).toBe("Hello, Ада");
  });

  it("persists the choice so the next load opens in it", () => {
    render(
      <LocaleProvider>
        <Probe />
      </LocaleProvider>,
    );
    fireEvent.click(screen.getByRole("button", { name: "to ru" }));
    expect(localStorage.getItem(LOCALE_STORAGE_KEY)).toBe("ru");

    // The next load, literally: a fresh provider reading the same storage.
    cleanup();
    render(
      <LocaleProvider>
        <Probe />
      </LocaleProvider>,
    );
    expect(locale()).toBe("ru");
  });

  it("survives a storage that refuses to be written", () => {
    // Private mode / quota: the choice must still apply for this session, so
    // the switcher does not go down with the storage.
    vi.spyOn(localStorage, "setItem").mockImplementation(() => {
      throw new Error("QuotaExceededError");
    });
    render(
      <LocaleProvider>
        <Probe />
      </LocaleProvider>,
    );
    fireEvent.click(screen.getByRole("button", { name: "to ru" }));
    expect(locale()).toBe("ru");
  });

  it("sets <html lang> so a screen reader picks the right pronunciation", () => {
    render(
      <LocaleProvider>
        <Probe />
      </LocaleProvider>,
    );
    expect(document.documentElement.lang).toBe("en");
    fireEvent.click(screen.getByRole("button", { name: "to ru" }));
    expect(document.documentElement.lang).toBe("ru");
  });
});

describe("without a provider", () => {
  it("renders English instead of throwing — the property page tests rely on", () => {
    // Deliberately unlike useTheme/useTimeMachine, which DO throw. lib/i18n's
    // module doc says why: this default is already correct, and throwing would
    // make translating one page a forty-file test migration.
    localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
    render(<Probe />);
    expect(locale()).toBe("en");
    expect(plain()).toBe("Plain English");
  });

  it("drops a setLocale call rather than pretending it worked", () => {
    vi.spyOn(console, "warn").mockImplementation(() => {});
    render(<Probe />);
    fireEvent.click(screen.getByRole("button", { name: "to ru" }));
    expect(locale()).toBe("en");
    expect(localStorage.getItem(LOCALE_STORAGE_KEY)).toBeNull();
  });
});

/* ── one word per concept, swept over every dictionary ───────────────────── */

/**
 * The console renders a probe or a pair that FAILED as «сбой» — dict/matrix.ts's
 * legend is the half that ships in every screenshot, and dict/matrix-cells.ts,
 * dict/topology.ts and dict/overview.ts had already followed it. dict/cards.ts
 * ("Отказ" on the tier badge, "Доля отказов" on the breakdown column),
 * dict/alerting.ts and dict/settings.ts had not, so one English word arrived in
 * Russian as two different nouns depending on which page an operator was on.
 *
 * lib/i18n/cards.test.tsx pins the resulting equalities key by key. This is the
 * OPEN end of that pin: it walks every dict/*.ts through import.meta.glob rather
 * than a hand-kept list, so a dictionary written next month is swept the day it
 * lands and cannot quietly reopen the split.
 *
 * «отказ» is NOT banned. It is the right Russian for a REFUSAL, and
 * dict/investigate.ts's `incident.copy.refused` — "The browser refused the
 * copy" — keeps it. The rule is about the English SOURCE, which is why both
 * halves below read `en` and judge `ru` by it.
 */

const DICT_MODULES = import.meta.glob("./dict/*.ts", { eager: true }) as Record<string, Record<string, unknown>>;

function isDictionary(value: unknown): value is Dictionary<string> {
  if (typeof value !== "object" || value === null) return false;
  const { en, ru } = value as { en?: unknown; ru?: unknown };
  return typeof en === "object" && en !== null && typeof ru === "object" && ru !== null;
}

/** Every (file, key, en, ru) quadruple in every dictionary, flattened once. */
function everyEntry(): Array<{ where: string; en: string; ru: string }> {
  const out: Array<{ where: string; en: string; ru: string }> = [];
  for (const [path, module] of Object.entries(DICT_MODULES)) {
    for (const exported of Object.values(module)) {
      if (!isDictionary(exported)) continue;
      for (const [key, en] of Object.entries(exported.en)) {
        const ru = exported.ru[key];
        if (typeof en === "string" && typeof ru === "string") out.push({ where: `${path} → ${key}`, en, ru });
      }
    }
  }
  return out;
}

describe("the ONE word for a probe that failed", () => {
  it("sweeps every dictionary, so this is a sweep and not a sample", () => {
    // 31 dict files at the time of writing; the floor is deliberately loose so
    // adding a surface does not fail the suite, but a glob that silently
    // resolved to nothing would.
    const files = new Set(everyEntry().map((entry) => entry.where.split(" → ")[0]));
    expect(files.size).toBeGreaterThanOrEqual(25);
  });

  it("never renders a fail/failed/failure English source as «отказ»", () => {
    const offenders = everyEntry()
      .filter((entry) => /\bfail/i.test(entry.en) && /отказ/i.test(entry.ru))
      .map((entry) => `${entry.where}: «${entry.ru}»`);
    expect(offenders).toEqual([]);
  });

  it("keeps «отказ» for the one concept it is right for — an English REFUSAL", () => {
    const kept = everyEntry().filter((entry) => /отказ/i.test(entry.ru));
    // Not empty: the word is scoped, not banned. If this ever hits zero the
    // test above has stopped proving anything.
    expect(kept.length).toBeGreaterThan(0);
    expect(kept.filter((entry) => !/refus|deni|reject/i.test(entry.en))).toEqual([]);
  });
});

/**
 * The localeTag policy, swept over the SOURCE rather than over one page at a
 * time.
 *
 * This module's own doc comment states the rule: a stamp standing on its own
 * keeps the viewer's locale, and a stamp INTERPOLATED INTO A TRANSLATED
 * SENTENCE goes through localeTag, or a Russian line ends up carrying an
 * American date. Round 6 fixed the Time Machine bar and left twelve other call
 * sites doing the bare thing; QA scope 2's finding #8 found them all again.
 *
 * A grep is the only pin that scales here — twelve sites across nine files, and
 * the thirteenth would be written by somebody who never read either doc
 * comment. The pattern is narrow on purpose: a bare `toLocale*()` on the SAME
 * LINE as a `t("…")` call is, by construction, a stamp being interpolated into
 * a translated string.
 */
const SOURCE_MODULES = import.meta.glob("../../{pages,components,lib}/**/*.{ts,tsx}", {
  eager: true,
  query: "?raw",
  import: "default",
}) as Record<string, string>;

describe("stamps interpolated into translated sentences", () => {
  it("reads the whole console's source, so this is a sweep and not a sample", () => {
    const files = Object.keys(SOURCE_MODULES).filter((p) => !p.includes(".test."));
    expect(files.length).toBeGreaterThanOrEqual(60);
  });

  it("never formats one with a bare toLocale* call", () => {
    const offenders: string[] = [];
    for (const [path, source] of Object.entries(SOURCE_MODULES)) {
      if (path.includes(".test.")) continue;
      source.split("\n").forEach((line, i) => {
        if (!/\bt\(\s*["'`]/.test(line)) return;
        if (!/\.toLocale(String|TimeString|DateString)\(\s*\)/.test(line)) return;
        offenders.push(`${path}:${i + 1}: ${line.trim()}`);
      });
    }
    expect(offenders).toEqual([]);
  });

  /**
   * The line-based rule above is necessary and NOT sufficient (QA scope 3,
   * finding #7). It only fires when a `t("…")` call happens to sit on the same
   * physical line as the stamp, so four call sites with no t() near them —
   * components/investigation-timeline.tsx's row clock, ui/datetime-picker.tsx's
   * trigger, and the compact columns in components/annotations.tsx and
   * components/maintenance.tsx — sailed through it and put four different date
   * shapes on the Investigate page at once.
   *
   * This rule is file-scope and does not care what else is on the line. Its
   * subject is the OPTIONS BAG: `toLocale*(undefined, { month: "short", … })`
   * asks the runtime for WORDS — a month abbreviation, an AM/PM marker — and a
   * word belongs to the interface language, not to whatever the browser was
   * installed in. It used to add that a stamp with no options "renders digits
   * and separators only" and could therefore stand alone; that was wrong, and
   * the strict rule below now covers the no-argument form too.
   *
   * The fix at every site is the same: one of lib/i18n's four stamp helpers,
   * each of which takes the locale.
   */
  it("never hands an OPTIONS BAG a bare locale — options render words, and words follow the interface", () => {
    const offenders: string[] = [];
    for (const [path, source] of Object.entries(SOURCE_MODULES)) {
      if (path.includes(".test.")) continue;
      // The i18n module itself is where the four helpers live; it is the one
      // file that legitimately spells the options out.
      if (path.endsWith("/i18n/index.tsx")) continue;
      const re = /\.toLocale(?:String|TimeString|DateString)\(\s*(undefined)?\s*,?\s*\{/g;
      let match: RegExpExecArray | null;
      while ((match = re.exec(source)) !== null) {
        const line = source.slice(0, match.index).split("\n").length;
        offenders.push(`${path}:${line}`);
      }
    }
    expect(offenders).toEqual([]);
  });

  /**
   * The two rules above are still not enough (QA scope 4, finding #7). Between
   * them they cover a stamp on the same line as a t() call and a stamp handed
   * an options bag — and the options-bag rule's own reasoning BLESSED the bare
   * no-argument form on the grounds that "a stamp with no options renders
   * digits and separators only".
   *
   * That reasoning is wrong. `toLocaleString()` with no locale picks the date
   * ORDER (8/10/2026 vs 10.08.2026) and the AM/PM marker from whatever the
   * browser was installed in, and both are part of the interface's language,
   * not of its arithmetic. A Russian /diagnostics run row was printing
   * "8/10/2026 3:47 AM".
   *
   * So the rule here is the strict one: in pages/ and components/ — the two
   * directories that render to a reader — a `toLocale*()` call takes a locale,
   * full stop. lib/ is excluded because that is where the four stamp helpers
   * and the shared formatters live; they receive the locale as an argument.
   *
   * REMAINING is a ratchet, not an allowlist: each entry is a page outside QA
   * scope 4's zone that still formats its stamps bare, and each one is a
   * one-line fix (thread `useLocale()` into that file's own fmtTime). A NEW
   * bare site fails this test immediately; FIXING one fails it too, which is
   * the point — the list only ever shrinks, and it shrinks by editing this
   * array.
   */
  // EMPTY, and it stays empty: every page and component now threads the
  // reader's locale into the stamp it prints. A new entry here is a
  // regression, not a TODO.
  const REMAINING_BARE_STAMP_SITES: string[] = [];

  it("never formats a user-facing stamp with a locale-less toLocale* call", () => {
    const offenders: string[] = [];
    for (const [path, source] of Object.entries(SOURCE_MODULES)) {
      if (path.includes(".test.")) continue;
      if (!path.includes("/pages/") && !path.includes("/components/")) continue;
      const re = /\.toLocale(?:String|TimeString|DateString)\(\s*\)/g;
      let match: RegExpExecArray | null;
      while ((match = re.exec(source)) !== null) {
        const line = source.slice(0, match.index).split("\n").length;
        offenders.push(`${path}:${line}`);
      }
    }
    const stillBare = [...new Set(offenders.map((o) => o.split(":")[0]))].sort();
    expect(stillBare).toEqual(REMAINING_BARE_STAMP_SITES);
  });

  it("exports one stamp helper per shape, and every one of them takes the locale", () => {
    const d = new Date("2026-08-08T13:05:00Z");
    // The helpers exist and differ — four shapes, not four spellings of one.
    const shapes = new Set([stampFull(d, "en"), stampClock(d, "en"), stampShort(d, "en"), stampInstant(d, "en")]);
    expect(shapes.size).toBe(4);
    // ...and the locale is honoured rather than ignored.
    expect(stampShort(d, "ru")).not.toBe(stampShort(d, "en"));
    expect(stampInstant(d, "ru")).not.toBe(stampInstant(d, "en"));
  });

  it("prints the hour in the locale's NATURAL form — no zero-padded 08:00 PM (finding #17)", () => {
    // 20:05 UTC is 8pm in any 12-hour locale; the padded "08:00 PM" shape was a
    // form English writes nowhere else in this console.
    const evening = new Date(2026, 7, 8, 20, 5);
    expect(stampInstant(evening, "en")).not.toMatch(/\b0\d:\d\d\s?(AM|PM)/i);
    expect(stampShort(evening, "en")).not.toMatch(/\b0\d:\d\d\s?(AM|PM)/i);
  });
});
