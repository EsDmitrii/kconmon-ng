import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * timemachine-links.test.ts — a SOURCE-level invariant, because the failure it guards is invisible
 * to a component test.
 *
 * A handful of in-app links are raw `<a href>` rather than router `<Link>`s (they sit in panels that
 * render outside a router in their own tests). A raw anchor is a full document load: the provider
 * unmounts, the new one reads `?at=` from the URL, and if the link did not carry it the reader is
 * silently returned to Live — mid-investigation, with the banner gone and live data on screen.
 *
 * lib/timemachine.tsx ships `withAtParam` for exactly this, and it was applied to the BACK links
 * ("/matrix", "/live") while every drill-down (a matrix cell, a node name, a run permalink, a
 * target) still dropped the instant. One test over the tree is what keeps the next one from doing
 * the same, since the mistake looks identical to correct code at the call site.
 */

const ROOTS = ["src/pages", "src/components"];

/** Files that legitimately build a bare path: they are not links a reader follows. */
const EXEMPT = new Set(["src/lib/timemachine.tsx"]);

function walk(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry);
    if (statSync(path).isDirectory()) {
      out.push(...walk(path));
      continue;
    }
    if (!/\.tsx?$/.test(entry)) continue;
    if (/\.test\.tsx?$/.test(entry)) continue;
    out.push(path);
  }
  return out;
}

/**
 * Every `href=` whose value is an in-app path, with the surrounding expression, so the assertion can
 * say WHICH link it is unhappy about.
 */
function inAppHrefs(source: string): string[] {
  const out: string[] = [];
  /* BOTH forms. The first cut matched only href={…}, and six real anchors were written the other way
     — href="/explore" — so the guard passed over exactly the links it existed to catch. A matcher
     that cannot see the mistake is worse than no matcher, because it reads as coverage. */
  const expressions = /href=\{([^}]*(?:\{[^}]*\}[^}]*)*)\}/g;
  const literals = /href=("[^"]*"|'[^']*')/g;
  for (const match of source.matchAll(expressions)) {
    const expr = match[1];
    // Only paths this app serves. An external URL, a mailto, or a variable we cannot read is not
    // this test's business.
    if (!/["'`]\//.test(expr)) continue;
    if (/^https?:/.test(expr)) continue;
    out.push(expr.replace(/\s+/g, " ").trim());
  }
  for (const match of source.matchAll(literals)) {
    const literal = match[1];
    // A path this app serves: starts with a single slash, and is not a protocol-relative URL.
    if (!/^["']\/[^/]/.test(literal)) continue;
    out.push(literal);
  }
  return out;
}

describe("every in-app anchor carries the viewed instant", () => {
  const files = ROOTS.flatMap((root) => walk(root)).filter((f) => !EXEMPT.has(f));

  it("finds the anchors at all — a matcher that matches nothing proves nothing", () => {
    const total = files.reduce((n, f) => n + inAppHrefs(readFileSync(f, "utf8")).length, 0);
    expect(total).toBeGreaterThan(5);
  });

  it("wraps each of them in withAtParam", () => {
    const offenders: string[] = [];
    for (const file of files) {
      const source = readFileSync(file, "utf8");
      for (const expr of inAppHrefs(source)) {
        if (expr.includes("withAtParam")) continue;
        /* A variable is followed to its declaration: several pages build the href above the JSX
           (`const pairHref = withAtParam(...)`) and pass the name in. */
        const name = /^[A-Za-z_$][\w$]*$/.exec(expr)?.[0];
        if (name) {
          const decl = new RegExp(`(?:const|let)\\s+${name}\\s*=([^;]*);`).exec(source)?.[1] ?? "";
          if (decl.includes("withAtParam")) continue;
        }
        offenders.push(`${file}: href={${expr}}`);
      }
    }
    expect(offenders).toEqual([]);
  });
});
