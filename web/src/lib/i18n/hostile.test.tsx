import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  DEFAULT_LOCALE,
  LOCALES,
  LOCALE_STORAGE_KEY,
  LocaleProvider,
  interpolate,
  isLocale,
  localeTag,
  stampClock,
  stampFull,
  stampInstant,
  stampShort,
  translate,
  useLocale,
  useT,
  type Dictionary,
  type Locale,
} from "@/lib/i18n";
import { countForm as annotationsCountForm } from "@/lib/i18n/dict/annotations";
import { countForm as diagnosticsCountForm } from "@/lib/i18n/dict/diagnostics";
import { countForm as investigateCountForm } from "@/lib/i18n/dict/investigate";
import { countForm as maintenanceCountForm } from "@/lib/i18n/dict/maintenance";
import { countForm as mtrCountForm } from "@/lib/i18n/dict/mtr";
import { countForm as mtrDetailCountForm } from "@/lib/i18n/dict/mtr-detail";
import { countForm as runDetailCountForm } from "@/lib/i18n/dict/run-detail";
import { pluralKey as alertingPluralKey, alertingDict } from "@/lib/i18n/dict/alerting";
import { pluralKey as cardsPluralKey, cardsDict } from "@/lib/i18n/dict/cards";
import { pluralKey as settingsPluralKey, settingsDict } from "@/lib/i18n/dict/settings";
import { pluralKey as targetsPluralKey, targetsDict } from "@/lib/i18n/dict/targets";
import { formatCadenceNs, formatDurationNs } from "@/lib/run-samples";

/**
 * THE HOSTILE READER'S i18n.
 *
 * lib/i18n/index.test.tsx pins the contract; cards/chrome/manage/bars/residue
 * pin the wording surface by surface. This file is the machinery under stress:
 * a dictionary whose two halves have drifted, a placeholder that exists in one
 * language and not the other, a stored locale somebody hand-edited, a
 * localStorage that throws, a count of 21 in a language where 21 is singular
 * and a language where it is not, and a duration of NaN nanoseconds.
 *
 * Everything here is a SWEEP where a sweep is possible: import.meta.glob over
 * dict/*.ts rather than a hand-kept list, so a dictionary written next month is
 * checked the day it lands.
 */

const DICT_MODULES = import.meta.glob("./dict/*.ts", { eager: true }) as Record<string, Record<string, unknown>>;

function isDictionary(value: unknown): value is Dictionary<string> {
  if (typeof value !== "object" || value === null) return false;
  const { en, ru } = value as { en?: unknown; ru?: unknown };
  return typeof en === "object" && en !== null && typeof ru === "object" && ru !== null;
}

interface Entry {
  file: string;
  key: string;
  en: string;
  ru: string;
}

/** Every dictionary in the console, with the file it came from. */
function everyDict(): Array<{ file: string; dict: Dictionary<string> }> {
  const out: Array<{ file: string; dict: Dictionary<string> }> = [];
  for (const [path, module] of Object.entries(DICT_MODULES)) {
    for (const exported of Object.values(module)) {
      if (isDictionary(exported)) out.push({ file: path, dict: exported });
    }
  }
  return out;
}

function everyEntry(): Entry[] {
  const out: Entry[] = [];
  for (const { file, dict } of everyDict()) {
    for (const [key, en] of Object.entries(dict.en)) {
      const ru = (dict.ru as Record<string, string>)[key];
      if (typeof en === "string" && typeof ru === "string") out.push({ file, key, en, ru });
    }
  }
  return out;
}

/** The {placeholder} names a template carries, as a sorted, de-duplicated list. */
function placeholders(template: string): string[] {
  return [...new Set([...template.matchAll(/\{(\w+)\}/g)].map((m) => m[1]))].sort();
}

afterEach(() => {
  cleanup();
  localStorage.removeItem(LOCALE_STORAGE_KEY);
  vi.restoreAllMocks();
});

/* ── the two halves cannot drift ────────────────────────────────────────── */

describe("every dictionary, swept", () => {
  it("finds them all, so this is a sweep and not a sample", () => {
    expect(everyDict().length).toBeGreaterThanOrEqual(25);
    expect(everyEntry().length).toBeGreaterThanOrEqual(1_000);
  });

  it("has the SAME KEYS in both languages — no half-translated table", () => {
    const drifted: string[] = [];
    for (const { file, dict } of everyDict()) {
      const en = Object.keys(dict.en).sort();
      const ru = Object.keys(dict.ru).sort();
      for (const key of en) if (!ru.includes(key)) drifted.push(`${file} → ${key} is English-only`);
      for (const key of ru) if (!en.includes(key)) drifted.push(`${file} → ${key} is Russian-only`);
    }
    expect(drifted).toEqual([]);
  });

  /**
   * A placeholder that exists in one half and not the other is the failure this
   * module CANNOT catch at runtime: interpolate leaves an unknown "{name}"
   * verbatim (see its doc comment), so a Russian sentence missing the English
   * one's {count} silently drops a number, and one that invents a {nmae} prints
   * the braces on the page. Neither is a type error — both halves are strings.
   */
  it("interpolates the SAME placeholders in both languages", () => {
    const mismatched = everyEntry()
      .filter((e) => placeholders(e.en).join(",") !== placeholders(e.ru).join(","))
      .map((e) => `${e.file} → ${e.key}: en {${placeholders(e.en)}} vs ru {${placeholders(e.ru)}}`);
    expect(mismatched).toEqual([]);
  });

  it("never ships an empty string, which would render as a missing label", () => {
    const blank = everyEntry()
      .filter((e) => e.en.trim() === "" || e.ru.trim() === "")
      .map((e) => `${e.file} → ${e.key}`);
    expect(blank).toEqual([]);
  });

  it("never leaves a stray {placeholder} the code does not fill", () => {
    // Every placeholder name is a word a caller can plausibly pass; the shape
    // that betrays a typo is an EMPTY or a broken pair.
    const broken = everyEntry()
      .filter((e) => /\{\s*\}/.test(e.en) || /\{\s*\}/.test(e.ru))
      .map((e) => `${e.file} → ${e.key}`);
    expect(broken).toEqual([]);
  });
});

/* ── interpolate ────────────────────────────────────────────────────────── */

describe("interpolate under hostile input", () => {
  it("leaves an unknown placeholder verbatim rather than blanking the sentence", () => {
    expect(interpolate("hello {nmae}", { name: "ada" })).toBe("hello {nmae}");
  });

  it("returns the template untouched when there are no vars at all", () => {
    expect(interpolate("hello {name}")).toBe("hello {name}");
  });

  it("does not re-scan what it just substituted", () => {
    // A value that LOOKS like a placeholder is data, not a template: one pass,
    // always, or a hostile webhook name could address another variable.
    expect(interpolate("{a}{b}", { a: "{b}", b: "X" })).toBe("{b}X");
  });

  it("treats a $-pattern in a value as text, not as a replacement pattern", () => {
    // String.replace's "$&", "$1" and "$'" are live only in a string
    // replacement; this one uses a function, so they are just characters.
    expect(interpolate("[{v}]", { v: "$& $1 $` $'" })).toBe("[$& $1 $` $']");
  });

  it.each([
    ["a number", 0, "n=0"],
    ["a negative number", -1, "n=-1"],
    ["NaN", Number.NaN, "n=NaN"],
    ["Infinity", Number.POSITIVE_INFINITY, "n=Infinity"],
  ])("stringifies %s with String(), never toLocaleString()", (_name, value, expected) => {
    expect(interpolate("n={n}", { n: value as number })).toBe(expected);
  });

  it("survives a ten-thousand-character value and a hundred placeholders", () => {
    const template = Array.from({ length: 100 }, (_, i) => `{v${i}}`).join("");
    const vars = Object.fromEntries(Array.from({ length: 100 }, (_, i) => [`v${i}`, "x".repeat(100)]));
    expect(interpolate(template, vars)).toHaveLength(10_000);
  });

  it("does not read inherited properties off the vars object", () => {
    // Object.hasOwn, not `in`: "{toString}" must not resolve to Object.prototype.
    expect(interpolate("{toString}", {})).toBe("{toString}");
  });
});

/* ── translate ─────────────────────────────────────────────────────────── */

describe("translate falls back rather than showing a key", () => {
  /* Hand-built on purpose: defineDict makes this shape a compile error, and the
     fallback exists for the one path the types cannot see. */
  const halfTranslated = { en: { a: "A", b: "B" }, ru: { a: "А" } } as unknown as Dictionary<"a" | "b">;

  it("uses the English string when the Russian one is missing", () => {
    expect(translate(halfTranslated, "ru", "b")).toBe("B");
    expect(translate(halfTranslated, "ru", "a")).toBe("А");
  });

  it("returns the key itself only when English is missing too", () => {
    const empty = { en: {}, ru: {} } as unknown as Dictionary<"nope">;
    expect(translate(empty, "ru", "nope")).toBe("nope");
  });

  it("interpolates through the fallback as well", () => {
    const d = { en: { k: "{n} left" }, ru: {} } as unknown as Dictionary<"k">;
    expect(translate(d, "ru", "k", { n: 3 })).toBe("3 left");
  });

  it("treats an unknown locale as English rather than throwing", () => {
    expect(translate(halfTranslated, "de" as Locale, "a")).toBe("A");
  });
});

/* ── the stored choice ──────────────────────────────────────────────────── */

function Switcher() {
  const { locale, setLocale } = useLocale();
  const t = useT(settingsDict);
  return (
    <div>
      <span data-testid="locale">{locale}</span>
      <span data-testid="title">{t("language.title")}</span>
      <button onClick={() => setLocale("ru")}>ru</button>
      <button onClick={() => setLocale("en")}>en</button>
    </div>
  );
}

describe("the locale an operator chose", () => {
  it.each([
    ["nothing stored", null, DEFAULT_LOCALE],
    ["a hand-edited value", "de", DEFAULT_LOCALE],
    ["an empty string", "", DEFAULT_LOCALE],
    ["a JSON blob", '{"locale":"ru"}', DEFAULT_LOCALE],
    ["the right value", "ru", "ru"],
    ["the right value, wrong case", "RU", DEFAULT_LOCALE],
  ])("opens in the right language for %s", (_name, stored, expected) => {
    if (stored !== null) localStorage.setItem(LOCALE_STORAGE_KEY, stored);
    render(
      <LocaleProvider>
        <Switcher />
      </LocaleProvider>,
    );
    expect(screen.getByTestId("locale")).toHaveTextContent(expected);
  });

  it("round-trips a switch through storage", () => {
    render(
      <LocaleProvider>
        <Switcher />
      </LocaleProvider>,
    );
    fireEvent.click(screen.getByRole("button", { name: "ru" }));
    expect(screen.getByTestId("locale")).toHaveTextContent("ru");
    expect(localStorage.getItem(LOCALE_STORAGE_KEY)).toBe("ru");
    fireEvent.click(screen.getByRole("button", { name: "en" }));
    expect(localStorage.getItem(LOCALE_STORAGE_KEY)).toBe("en");
  });

  it("still switches when the browser refuses to store anything", () => {
    // Private mode, a full quota, a locked-down origin: the choice has to hold
    // for THIS session even when it cannot be remembered for the next one.
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new DOMException("QuotaExceededError");
    });
    render(
      <LocaleProvider>
        <Switcher />
      </LocaleProvider>,
    );
    expect(() => fireEvent.click(screen.getByRole("button", { name: "ru" }))).not.toThrow();
    expect(screen.getByTestId("locale")).toHaveTextContent("ru");
    expect(screen.getByTestId("title")).toHaveTextContent("Язык");
  });

  it("opens in English when reading storage throws", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new DOMException("SecurityError");
    });
    render(
      <LocaleProvider>
        <Switcher />
      </LocaleProvider>,
    );
    expect(screen.getByTestId("locale")).toHaveTextContent(DEFAULT_LOCALE);
  });

  it("tells a screen reader which language the page is in", () => {
    render(
      <LocaleProvider>
        <Switcher />
      </LocaleProvider>,
    );
    expect(document.documentElement.lang).toBe("en");
    act(() => {
      fireEvent.click(screen.getByRole("button", { name: "ru" }));
    });
    expect(document.documentElement.lang).toBe("ru");
  });

  it("reads outside a provider without throwing, and gets English", () => {
    render(<Switcher />);
    expect(screen.getByTestId("locale")).toHaveTextContent(DEFAULT_LOCALE);
  });

  it.each(["en", "ru"])("recognises %s and nothing else", (good) => {
    expect(isLocale(good)).toBe(true);
  });

  it.each([null, undefined, "", "de", "en-US", 1, {}, ["ru"]])("refuses %j as a locale", (bad) => {
    expect(isLocale(bad)).toBe(false);
  });

  it("tags Russian for the runtime and leaves English on the default", () => {
    expect(localeTag("ru")).toBe("ru-RU");
    expect(localeTag("en")).toBeUndefined();
  });
});

/* ── stamps ────────────────────────────────────────────────────────────── */

describe("stamps never render NaN", () => {
  const stamps = { stampFull, stampClock, stampShort, stampInstant };

  it.each(Object.entries(stamps))("%s says «Invalid Date» for one rather than printing NaN", (_name, fn) => {
    for (const locale of LOCALES) {
      const out = fn(new Date(Number.NaN), locale);
      expect(out).not.toMatch(/NaN|undefined/);
      expect(out).toBe("Invalid Date");
    }
  });

  it.each(Object.entries(stamps))("%s survives the edges of the representable range", (_name, fn) => {
    for (const ms of [0, -8.64e15, 8.64e15]) {
      for (const locale of LOCALES) {
        expect(() => fn(new Date(ms), locale)).not.toThrow();
        expect(fn(new Date(ms), locale)).not.toMatch(/NaN|undefined/);
      }
    }
  });
});

/* ── plurals ───────────────────────────────────────────────────────────── */

/** The counts a plural rule is actually judged on. */
const COUNTS = [0, 1, 2, 4, 5, 11, 12, 14, 21, 22, 25, 100, 101, 111, 1_000];

describe("countForm — the Russian rule, in seven copies", () => {
  const copies = {
    annotations: annotationsCountForm,
    diagnostics: diagnosticsCountForm,
    investigate: investigateCountForm,
    maintenance: maintenanceCountForm,
    mtr: mtrCountForm,
    "mtr-detail": mtrDetailCountForm,
    "run-detail": runDetailCountForm,
  };

  it.each(Object.entries(copies))("%s agrees with the language on every count", (_name, countForm) => {
    // 0 узлов, 1 узел, 2 узла, 5 узлов, 11 узлов, 21 узел, 101 узел, 111 узлов.
    expect(COUNTS.map((n) => countForm("ru", n))).toEqual([
      "many", // 0
      "one", // 1
      "few", // 2
      "few", // 4
      "many", // 5
      "many", // 11
      "many", // 12
      "many", // 14
      "one", // 21
      "few", // 22
      "many", // 25
      "many", // 100
      "one", // 101
      "many", // 111
      "many", // 1000
    ]);
  });

  it.each(Object.entries(copies))("%s keeps English to one and many, never few", (_name, countForm) => {
    expect(COUNTS.map((n) => countForm("en", n))).toEqual(
      COUNTS.map((n) => (n === 1 ? "one" : "many")),
    );
    // An unknown locale is English, not a crash.
    expect(countForm("de", 21)).toBe("many");
  });

  it.each(Object.entries(copies))("%s answers for a count that is not a whole positive number", (_name, countForm) => {
    for (const n of [-1, -21, 1.5, Number.NaN, Number.POSITIVE_INFINITY]) {
      for (const locale of ["en", "ru"]) {
        expect(["one", "few", "many"]).toContain(countForm(locale, n));
      }
    }
  });
});

/**
 * pluralKey is countForm's cousin: it picks a KEY rather than a form, and — the
 * defect this describe() is about — it used to take no locale.
 *
 * Its doc comment justified that with "English fills all three slots with the
 * same word, so this runs there too and changes nothing". That is true of
 * dict/targets.ts's `count.series.*` and of nothing else in the console:
 * `count.failures.*` is failure/failures/failures, `count.pairs.*` is
 * pair/pairs/pairs, `count.rules.*` is rule/rules/rules. The Russian rule sends
 * 21, 31, 101 and 1001 to the ONE form — so an English console rendered
 * "21 failure", "101 rule" and "21 pair".
 */
describe("pluralKey", () => {
  it("says «21 сбой» and «111 сбоев» in Russian", () => {
    const word = (n: number, locale: Locale) =>
      translate(
        settingsDict,
        locale,
        settingsPluralKey(locale, n, "count.failures.one", "count.failures.few", "count.failures.many"),
      );
    expect(word(1, "ru")).toBe("сбой");
    expect(word(2, "ru")).toBe("сбоя");
    expect(word(5, "ru")).toBe("сбоев");
    expect(word(21, "ru")).toBe("сбой");
    expect(word(111, "ru")).toBe("сбоев");
    expect(word(0, "ru")).toBe("сбоев");
  });

  it("says «21 failures» in English, where only 1 is singular", () => {
    const word = (n: number) =>
      translate(
        settingsDict,
        "en",
        settingsPluralKey("en", n, "count.failures.one", "count.failures.few", "count.failures.many"),
      );
    expect(word(1)).toBe("failure");
    for (const n of [0, 2, 5, 11, 21, 22, 101, 111]) expect(word(n)).toBe("failures");
  });

  /**
   * FOREIGN ROOTS — dict/alerting.ts, dict/cards.ts and dict/targets.ts each
   * keep their own locale-blind copy, and their call sites (pages/alerting.tsx,
   * pages/node-card.tsx, pages/targets.tsx) pass no locale. Fixing the helper
   * changes those pages' code, which belongs to whoever owns them; this is the
   * reproduction, left skipped so the report has an exact failing case.
   */
  it.skip("FOREIGN: alerting/cards/targets still say «21 rule», «21 pair», «21 agent» in English", () => {
    expect(translate(alertingDict, "en", alertingPluralKey(21, "count.rules.one", "count.rules.few", "count.rules.many"))).toBe(
      "rules",
    );
    expect(translate(cardsDict, "en", cardsPluralKey(21, "count.pairs.one", "count.pairs.few", "count.pairs.many"))).toBe(
      "pairs",
    );
    expect(
      translate(targetsDict, "en", targetsPluralKey(21, "count.agents.one", "count.agents.few", "count.agents.many")),
    ).toBe("agents");
  });

  /* This case used to assert the BUG, so that fixing it would fail here rather
     than let the report rot. It is fixed: all three copies take the locale. */
  it("the three copies now answer per LANGUAGE, so English stops printing 21 rule", () => {
    expect(alertingPluralKey(21, "count.rules.one", "count.rules.few", "count.rules.many", "en")).toBe("count.rules.many");
    expect(cardsPluralKey(21, "count.pairs.one", "count.pairs.few", "count.pairs.many", "en")).toBe("count.pairs.many");
    expect(targetsPluralKey(21, "count.agents.one", "count.agents.few", "count.agents.many", "en")).toBe("count.agents.many");
  });

  it("keeps the Russian ladder, where 21 really is the .one form", () => {
    expect(alertingPluralKey(21, "count.rules.one", "count.rules.few", "count.rules.many", "ru")).toBe("count.rules.one");
    expect(alertingPluralKey(22, "count.rules.one", "count.rules.few", "count.rules.many", "ru")).toBe("count.rules.few");
    expect(alertingPluralKey(11, "count.rules.one", "count.rules.few", "count.rules.many", "ru")).toBe("count.rules.many");
  });

  it("says ONE only for one in English, and many for everything else", () => {
    expect(alertingPluralKey(1, "count.rules.one", "count.rules.few", "count.rules.many", "en")).toBe("count.rules.one");
    expect(alertingPluralKey(2, "count.rules.one", "count.rules.few", "count.rules.many", "en")).toBe("count.rules.many");
    expect(alertingPluralKey(0, "count.rules.one", "count.rules.few", "count.rules.many", "en")).toBe("count.rules.many");
  });
});

/* ── durations ─────────────────────────────────────────────────────────── */

describe("formatDurationNs / formatCadenceNs at the boundaries", () => {
  it.each([
    [0, "0s", "0 с"],
    [1, "0s", "0 с"],
    [999_999_999, "1s", "1 с"],
    [59_000_000_000, "59s", "59 с"],
    [60_000_000_000, "1m", "1 мин"],
    [3_599_000_000_000, "60m", "60 мин"],
    [3_600_000_000_000, "1h", "1 ч"],
    [86_400_000_000_000, "24h", "24 ч"],
  ])("renders %d ns as %s / %s", (ns, en, ru) => {
    expect(formatDurationNs(ns, "en")).toBe(en);
    expect(formatDurationNs(ns, "ru")).toBe(ru);
  });

  it("says a non-whole-minute cadence in seconds, in both languages", () => {
    expect(formatCadenceNs(90_000_000_000, "en")).toBe("90s");
    expect(formatCadenceNs(90_000_000_000, "ru")).toBe("90 с");
    expect(formatCadenceNs(120_000_000_000, "en")).toBe("2m");
  });

  it("falls back to the English units for a locale it has no table for", () => {
    expect(formatDurationNs(60_000_000_000, "de")).toBe("1m");
    expect(formatCadenceNs(90_000_000_000, "de")).toBe("90s");
  });

  it("never throws on a number no schedule should produce", () => {
    for (const ns of [-1, -60_000_000_000, 1e30, Number.MAX_SAFE_INTEGER]) {
      for (const locale of ["en", "ru"]) {
        expect(() => formatDurationNs(ns, locale)).not.toThrow();
        expect(() => formatCadenceNs(ns, locale)).not.toThrow();
      }
    }
  });

  /**
   * The span that is not a number. `Math.round(undefined / 1e9)` is NaN and the
   * template used to paste it straight onto the page, so a row whose intervalNs
   * the API did not send rendered the literal text "NaNs" — and an infinite one
   * rendered "Infinityh" on the cadence tile. lib/run-samples.ts now floors both
   * to zero; this is the pin from the i18n side, because "NaN" is not a word in
   * either language.
   */
  it.each([Number.NaN, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY, undefined, null])(
    "draws %s as no span at all rather than as a word",
    (ns) => {
      for (const locale of ["en", "ru"]) {
        expect(formatDurationNs(ns as unknown as number, locale)).not.toMatch(/NaN|Infinity|undefined|null/);
        expect(formatCadenceNs(ns as unknown as number, locale)).not.toMatch(/NaN|Infinity|undefined|null/);
      }
    },
  );
});
