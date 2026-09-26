import { describe, expect, it } from "vitest";
import type { Dictionary } from "@/lib/i18n";
import { cardsDict } from "@/lib/i18n/dict/cards";
import { chromeDict, NAV_KEYS } from "@/lib/i18n/dict/chrome";
import { diagnosticsDict } from "@/lib/i18n/dict/diagnostics";
import { investigateDict } from "@/lib/i18n/dict/investigate";
import { investigateEntryDict } from "@/lib/i18n/dict/investigate-entry";
import { liveDict } from "@/lib/i18n/dict/live";
import { mtrDict } from "@/lib/i18n/dict/mtr";
import { overviewDict } from "@/lib/i18n/dict/overview";
import { paletteDict } from "@/lib/i18n/dict/palette";
import { promqlConsoleDict } from "@/lib/i18n/dict/promql-console";
import { runDetailDict } from "@/lib/i18n/dict/run-detail";
import { stubPageDict } from "@/lib/i18n/dict/stub-page";
import { targetsDict } from "@/lib/i18n/dict/targets";

/**
 * Two classes of copy the screenshots must not show, swept together because
 * they leak the same way — a string written before a rename, or for a reader
 * who was never the operator:
 *
 *   1. PRE-RENAME PAGE NAMES. M3-8 regrouped the nav (Explore→Metrics,
 *      Diagnostics→Run checks, Console→PromQL, Live→Events), and a dict string
 *      that still points an operator at "Explore" points at a page that no
 *      longer answers to the name. "Live" the Time Machine term and "MTR
 *      Explorer" the sub-view are NOT renames and are not swept here.
 *
 *   2. RATIONALE ADDRESSED TO DEVELOPERS. A sentence that argues for its own
 *      existence ("saying so beats a link that…") is a changelog entry, not UI
 *      copy — the argument belongs in the source comment above the key, and
 *      the string keeps only the half the operator can act on.
 */

describe("stale page names (pre-M3-8) are out of the dict strings", () => {
  it("sends the compare action to Metrics, in both languages", () => {
    expect(investigateDict.en["actions.compare"]).toBe("Compare in Metrics");
    expect(investigateDict.ru["actions.compare"]).toBe("Сравнить в Метриках");
  });

  it("names the Metrics page, not Explore, in the compare caveat", () => {
    expect(investigateDict.en["actions.compareNote"]).not.toMatch(/\bExplore\b/);
    expect(investigateDict.en["actions.compareNote"]).toContain("Metrics");
    expect(investigateDict.ru["actions.compareNote"]).toContain("Метрик");
  });

  it("titles a run permalink with the generic term, not the retired page name", () => {
    expect(runDetailDict.en["title"]).toBe("Diagnostic run");
    expect(runDetailDict.ru["title"]).toBe("Диагностический запуск");
  });

  it("labels the not-found back link with the page's current name", () => {
    expect(runDetailDict.en["notFound.back"]).toBe("Back to Run checks");
    expect(runDetailDict.ru["notFound.back"]).toBe("Назад на страницу «Проверки вручную»");
  });

  /* The Overview's "open X" links name the page the sidebar shows for the same path. Russian
     declines the name after «открыть» (Матрица, Матрицу), so it is compared by stem. */
  it("labels every Overview 'open X' link with the sidebar name of the page it opens", () => {
    const links = {
      "worstPairs.open": "/matrix",
      "alerts.open": "/alerting",
      "incidents.open": "/investigate",
      "events.open": "/live",
    } as const;
    for (const [key, path] of Object.entries(links) as [keyof typeof links, string][]) {
      const nav = NAV_KEYS[path];
      expect(overviewDict.en[key], key).toBe(`open ${chromeDict.en[nav]}`);
      expect(overviewDict.ru[key], key).toMatch(/^открыть /);
      expect(overviewDict.ru[key], key).toContain(chromeDict.ru[nav].slice(0, -1));
    }
  });

  it("names pages, not URL paths, in the Overview's empty states", () => {
    for (const lang of ["en", "ru"] as const) {
      for (const key of ["alerts.empty", "incidents.empty"] as const) {
        expect(overviewDict[lang][key], `${lang} ${key}`).not.toMatch(/\/[a-z]/);
      }
    }
    expect(overviewDict.en["alerts.empty"]).toContain(chromeDict.en["nav.alerting"]);
    expect(overviewDict.en["incidents.empty"]).toContain(chromeDict.en["nav.investigate"]);
    expect(overviewDict.ru["alerts.empty"]).toContain(`«${chromeDict.ru["nav.alerting"]}»`);
    expect(overviewDict.ru["incidents.empty"]).toContain(`«${chromeDict.ru["nav.investigate"]}»`);
  });

  /* Russian «Обзор» is the Overview page; the MTR sub-view is the Explorer, «Обозреватель», in the
     tab, the prose around it, the run permalink's link and the command palette alike. */
  it("gives the MTR Explorer one Russian word, and not the Overview's", () => {
    expect(mtrDict.ru["view.explorer"]).toBe("Обозреватель");
    expect(mtrDict.ru["view.explorer"]).not.toBe(chromeDict.ru["nav.overview"]);
    expect(runDetailDict.ru["trace.openInExplorer"]).toContain("Обозревател");
    expect(paletteDict.ru["navDesc.mtr"]).toContain("Обозреватель");
    const mtrStrings = Object.values(mtrDict.ru).join("\n");
    expect(mtrStrings).not.toMatch(/[Оо]бзор(?!н)/);
  });

  it("keeps internal milestone names out of the Plane tooltip", () => {
    for (const lang of ["en", "ru"] as const) {
      expect(targetsDict[lang]["definitions.form.planeNote"], lang).not.toMatch(/\bM\d+\b/);
    }
  });
});

describe("developer rationale stays out of user copy", () => {
  it("keeps the compare caveat factual — the 'saying so beats…' argument lives in the comment", () => {
    for (const s of [investigateDict.en["actions.compareNote"], investigateDict.ru["actions.compareNote"]]) {
      expect(s).not.toMatch(/beats/);
      expect(s).not.toMatch(/Честнее/);
    }
    /* The facts the caveat exists for are still said: the slots are curated,
       and the window has to be chosen on the page. */
    expect(investigateDict.en["actions.compareNote"]).toContain("curated metrics");
    expect(investigateDict.en["actions.compareNote"]).toContain("range");
    expect(investigateDict.ru["actions.compareNote"]).toContain("диапазон");
  });

  it("says what an operator can do with the Investigate URL, not how the permalink is built", () => {
    for (const lang of ["en", "ru"] as const) {
      const note = investigateDict[lang]["form.urlNote"];
      expect(note, lang).not.toMatch(/\?kind=|permalink|бесплатно/);
    }
    expect(investigateDict.en["actions.compareNote"]).not.toMatch(/binds|reads no range from the URL/);
    expect(investigateDict.ru["actions.compareNote"]).not.toMatch(/привязаны|из адреса/);
  });

  /* An instant vector is many series with one point each; the table beside the tab lists them.
     The block follows the result (an instant query on up[5m] charts), so the tooltip names what a
     chart needs rather than blaming the mode. */
  it("tells the reader what a chart needs instead of claiming the result has no series", () => {
    expect(promqlConsoleDict.en["tab.chart.disabled"]).not.toMatch(/no series/);
    expect(promqlConsoleDict.ru["tab.chart.disabled"]).not.toMatch(/нет серий/);
    expect(promqlConsoleDict.en["tab.chart.disabled"]).toContain(promqlConsoleDict.en["mode.range"]);
    expect(promqlConsoleDict.ru["tab.chart.disabled"]).toContain(`«${promqlConsoleDict.ru["mode.range"]}»`);
  });

  it("states the stub page's roadmap fact without the information-architecture argument", () => {
    expect(stubPageDict.en["body"]).not.toMatch(/information architecture/i);
    expect(stubPageDict.ru["body"]).not.toContain("структура разделов");
    expect(stubPageDict.en["body"]).toContain("later milestone");
    expect(stubPageDict.ru["body"]).toContain("вех");
  });
});

/* The console config has no database mode: the database is on when database.dsnFile names a DSN, and the
   chart fails a render that still sets console.database. The 503 details already say so; the UI's own
   gate lines have to send an operator to the same two keys. */
describe("the database gates name the keys that exist", () => {
  const dicts = import.meta.glob<Record<string, unknown>>("./dict/*.ts", { eager: true });

  it("leaves console.database.mode out of every string, in both languages", () => {
    const hits: string[] = [];
    for (const [file, mod] of Object.entries(dicts)) {
      for (const dict of Object.values(mod)) {
        const d = dict as Partial<Dictionary<string>>;
        if (typeof d?.en !== "object" || typeof d?.ru !== "object") continue;
        for (const lang of ["en", "ru"] as const) {
          for (const [key, value] of Object.entries(d[lang] ?? {})) {
            if (value.includes("console.database.mode")) hits.push(`${file} ${lang} ${key}`);
          }
        }
      }
    }
    expect(Object.keys(dicts).length).toBeGreaterThan(20);
    expect(hits).toEqual([]);
  });

  it("sends every gate line to database.dsnFile and the chart's database.existingSecret", () => {
    const gates: [Dictionary<string>, string][] = [
      [investigateDict, "source.database"],
      [investigateEntryDict, "noDatabase"],
      [overviewDict, "db.note"],
      [cardsDict, "target.gate.noDatabase"],
      [targetsDict, "gate.noDatabase"],
      [mtrDict, "database.gate"],
      [diagnosticsDict, "history.notPersisted"],
    ];
    for (const [dict, key] of gates) {
      for (const lang of ["en", "ru"] as const) {
        expect(dict[lang][key], `${lang} ${key}`).toContain("database.dsnFile");
        expect(dict[lang][key], `${lang} ${key}`).toContain("Helm: database.existingSecret");
      }
    }
  });

  /* A failed GET /api/v1/config also reads as "no database" (useDatabaseAvailable sets `error`).
     Every surface with a gate line has a second line for that case, which names the failure and
     does not send the operator to set a key that may well be set. */
  it("gives every gated surface a config-failure line that names the error, not the database keys", () => {
    const lines: [Dictionary<string>, string][] = [
      [investigateDict, "config.failed"],
      [overviewDict, "config.failed"],
      [cardsDict, "target.config.failed"],
      [targetsDict, "config.failed"],
      [mtrDict, "config.failed"],
      [diagnosticsDict, "config.failed"],
      [liveDict, "config.failed"],
    ];
    for (const [dict, key] of lines) {
      for (const lang of ["en", "ru"] as const) {
        const line = dict[lang][key];
        expect(line, `${lang} ${key}`).toContain("{error}");
        expect(line, `${lang} ${key}`).not.toContain("database.dsnFile");
        expect(dict[lang][`${key}.generic`], `${lang} ${key}.generic`).toBeTruthy();
      }
    }
  });
});
