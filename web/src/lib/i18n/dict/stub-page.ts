import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * stub-page — components/stub-page.tsx, the Blank Slate a nav entry gets when
 * the view behind it ships in a later milestone.
 *
 * Two strings, and they are the component's ENTIRE contribution: the page's
 * `title` and `description` are props, handed down by routes.tsx from nav.ts's
 * own entry, and they are translated where the nav is (dict/chrome.ts's
 * NAV_KEYS for the label, dict/palette.ts's NAV_DESC_KEYS for the sentence) —
 * not a second time here.
 *
 * The Russian keeps the register: this is an honest statement about the
 * roadmap, not an apology and not an error. «Пока не сделано» rather than
 * «Недоступно» — nothing is broken, the view does not exist yet.
 */

const en = {
  "title": "Not built yet — on the roadmap",
  "body":
    "This view is delivered in a later milestone. The navigation shows the full product so " +
    "the information architecture stays honest about what is coming.",
} as const;

export type StubPageKey = keyof typeof en;

export const stubPageDict: Dictionary<StubPageKey> = defineDict(en, {
  "title": "Пока не сделано, но в плане есть",
  "body":
    "Этот экран появится в одной из следующих вех. Навигация показывает продукт целиком, " +
    "чтобы структура разделов честно говорила о том, что ещё впереди.",
});
