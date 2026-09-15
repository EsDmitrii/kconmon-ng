import { readdirSync, readFileSync } from "node:fs";
import { join, relative, sep } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * house-clock.test.ts — the console has ONE clock.
 *
 * Every instant a page prints goes through lib/i18n's stamp helpers
 * (stampFull, stampClock, stampShort, stampInstant) or lib/utils' event stamp,
 * all of which pin hour12: false. A bare `toLocaleString(localeTag(locale))`
 * anywhere else reads the runtime's default, which is 12-hour in en-US: the
 * Matrix description said "12:30:00 PM" over a timeline stamped "11:48:27",
 * and at night the two notations collide on the same hour. Ten page files had
 * exactly that call before the sweep that added this guard.
 *
 * This is a source-level test: it walks web/src and fails on any
 * `.toLocaleString(`, `.toLocaleTimeString(` or `.toLocaleDateString(` call
 * outside the two files allowed to make one. Test files are skipped — they
 * compute EXPECTATIONS with the same calls, which is the point of them — and so
 * are comments, where the old idiom is described by name.
 */

/* Relative to the vitest root (web/), the way confirm-step.test.ts and
   timemachine-links.test.ts walk the tree — import.meta.url is not a file:
   URL under the jsdom environment. */
const SRC = "src";

/** The two places a locale call is the implementation, not a leak. */
const ALLOWLIST = new Set(["lib/i18n/index.tsx", "lib/utils.ts"]);

const CALL = /\.toLocale(?:Date|Time)?String\(/g;

function* walk(dir: string): Generator<string> {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) {
      if (entry.name === "node_modules") continue;
      yield* walk(path);
    } else if (/\.tsx?$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name) && !entry.name.endsWith(".d.ts")) {
      yield path;
    }
  }
}

/** Block and line comments go: the guard is about calls, and the old call is
 *  named in prose next to several of the fixes. `://` is spared so a URL in a
 *  string does not eat the rest of its line. */
function stripComments(source: string): string {
  return source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/(^|[^:\\])\/\/[^\n]*/g, "$1");
}

describe("the house clock", () => {
  it("is the only clock: no bare toLocale*String( call outside lib/i18n and lib/utils", () => {
    const offenders: string[] = [];
    let scanned = 0;
    for (const file of walk(SRC)) {
      const rel = relative(SRC, file).split(sep).join("/");
      if (ALLOWLIST.has(rel)) continue;
      scanned += 1;
      const body = stripComments(readFileSync(file, "utf8"));
      const lines = body.split("\n");
      lines.forEach((line, i) => {
        if (CALL.test(line)) offenders.push(`${rel}:${i + 1}: ${line.trim()}`);
        CALL.lastIndex = 0;
      });
    }
    // A guard that scanned nothing would pass vacuously; the tree has hundreds of files.
    expect(scanned).toBeGreaterThan(100);
    expect(offenders).toEqual([]);
  });

  it("still sees the two allowed implementations, so the allowlist is not stale", () => {
    for (const rel of ALLOWLIST) {
      const body = stripComments(readFileSync(join(SRC, rel), "utf8"));
      expect(body, rel).toMatch(CALL);
      CALL.lastIndex = 0;
    }
  });
});
