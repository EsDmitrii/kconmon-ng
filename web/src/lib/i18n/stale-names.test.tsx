import { describe, expect, it } from "vitest";
import { investigateDict } from "@/lib/i18n/dict/investigate";
import { runDetailDict } from "@/lib/i18n/dict/run-detail";
import { stubPageDict } from "@/lib/i18n/dict/stub-page";

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

  it("states the stub page's roadmap fact without the information-architecture argument", () => {
    expect(stubPageDict.en["body"]).not.toMatch(/information architecture/i);
    expect(stubPageDict.ru["body"]).not.toContain("структура разделов");
    expect(stubPageDict.en["body"]).toContain("later milestone");
    expect(stubPageDict.ru["body"]).toContain("вех");
  });
});
