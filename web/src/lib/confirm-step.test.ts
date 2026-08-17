import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * confirm-step.test.ts — a SOURCE-level invariant, for the same reason timemachine-links.test.ts is
 * one: the failure is invisible to a component test that does not think to look for it.
 *
 * Every "Delete → Confirm delete" row in this console swaps one button for a pair of them. React
 * sees a different element type at that slot, so the focused button's DOM node is destroyed and
 * focus falls back to `<body>`: the next Tab restarts at the skip link, and a keyboard user has to
 * walk the sidebar, the header and every row above to reach the confirm button they just summoned.
 * A screen reader hears nothing at all, so pressing Delete appears to do nothing.
 *
 * hooks/use-confirm-step ships the handoff. It was applied to five rows and missed on four others,
 * which is exactly the kind of miss that looks identical to correct code at the call site — the
 * hand-rolled `useState(false)` reads perfectly well. One test over the tree is what keeps the next
 * row from being written the old way.
 */

const ROOTS = ["src/pages", "src/components"];

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

/** A `useState` whose name says it holds a confirm step — the hand-rolled shape. */
const HAND_ROLLED = /(?:const|let)\s*\[\s*(confirm\w*)\s*,\s*set\w+\s*\]\s*=\s*useState/gi;

describe("every two-press destructive control uses the shared confirm step", () => {
  const files = ROOTS.flatMap((root) => walk(root));

  it("finds the confirm steps at all — a matcher that matches nothing proves nothing", () => {
    const users = files.filter((f) => /useC\w*onfirmStep/.test(readFileSync(f, "utf8")));
    expect(users.length).toBeGreaterThan(4);
  });

  it("leaves no hand-rolled confirm state behind", () => {
    const offenders: string[] = [];
    for (const file of files) {
      for (const match of readFileSync(file, "utf8").matchAll(HAND_ROLLED)) {
        offenders.push(`${file}: useState for \`${match[1]}\` — use useConfirmStep instead`);
      }
    }
    expect(offenders).toEqual([]);
  });

  it("puts the ref on the confirm control wherever the hook is used", () => {
    const offenders: string[] = [];
    for (const file of files) {
      const source = readFileSync(file, "utf8");
      if (!/useC\w*onfirmStep\(/.test(source)) continue;
      /* Both halves, or the handoff is one-way: confirmRef is what catches focus when the pair
         mounts, triggerRef is what gives it back when the reader cancels. */
      for (const ref of ["confirmRef", "triggerRef"]) {
        if (!new RegExp(`ref=\\{${ref}\\}`).test(source)) offenders.push(`${file}: ${ref} is never placed`);
      }
    }
    expect(offenders).toEqual([]);
  });
});
