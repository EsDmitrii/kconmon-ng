import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

/*
The Time Machine's control is opt-in per page (components/page-shell.tsx). Opt-in is only honest
while the two halves agree, and they are edited in different files months apart: a page that
RESOLVES its reads through ?at= must offer the control, and a page that ignores ?at= must not —
Targets, Alerting and Settings each carried one and none of them honours it (owner report).

Source text rather than a render: the claim is about every page at once, including the ones whose
render needs a router, a query client and a controller.
*/

const PAGES = join(__dirname);

function pageSources(): { name: string; src: string }[] {
  return readdirSync(PAGES)
    .filter((f) => f.endsWith(".tsx") && !f.includes(".test."))
    .map((name) => ({ name, src: readFileSync(join(PAGES, name), "utf8") }))
    .filter(({ src }) => src.includes("<PageShell"));
}

const readsTime = (src: string) => src.includes("useTimeContext");
const offersControl = (src: string) => /<PageShell\s[^>]*timeMachine|<PageShell\n\s*timeMachine/.test(src);

describe("Time Machine surfaces", () => {
  it("offers the control on every page that resolves its reads through ?at=", () => {
    const missing = pageSources()
      .filter(({ src }) => readsTime(src) && !offersControl(src))
      .map(({ name }) => name);
    expect(missing).toEqual([]);
  });

  it("offers it nowhere else, so no page invites a past it will ignore", () => {
    const spurious = pageSources()
      .filter(({ src }) => !readsTime(src) && offersControl(src))
      .map(({ name }) => name);
    expect(spurious).toEqual([]);
  });

  /* The guard is only a guard while both sides are non-empty — a rename that
     made `readsTime` match nothing would leave both lists empty and green. */
  it("is checking real pages on both sides of the split", () => {
    const pages = pageSources();
    expect(pages.filter(({ src }) => readsTime(src)).length).toBeGreaterThan(5);
    expect(pages.filter(({ src }) => !readsTime(src)).length).toBeGreaterThan(0);
  });
});
