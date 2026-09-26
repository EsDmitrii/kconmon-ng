import { afterEach, describe, expect, it, vi } from "vitest";
import { phoneMatchMedia } from "./phone-and-light";
import { matchesWidthQuery, stubViewport } from "./viewport-stub";

/* The app asks for its breakpoint two ways: nav-drawer.tsx with `(width >= 48rem)` and live.tsx
   with `not all and (min-width: 48rem)`. Both doubles have to evaluate both, not answer one of
   them by the default for a query they cannot parse. */
describe("matchesWidthQuery", () => {
  it.each([
    ["(min-width: 48rem)", 768, true],
    ["(min-width: 48rem)", 767, false],
    ["(max-width: 600px)", 600, true],
    ["(max-width: 600px)", 601, false],
    ["not all and (min-width: 48rem)", 390, true],
    ["not all and (min-width: 48rem)", 1024, false],
    ["(width >= 48rem)", 768, true],
    ["(width >= 48rem)", 375, false],
    ["(width < 48rem)", 375, true],
    ["(width < 48rem)", 768, false],
    ["(width <= 30em)", 480, true],
    ["(width > 480px)", 480, false],
  ] as const)("%s at %ipx is %s", (query, width, expected) => {
    expect(matchesWidthQuery(query, width)).toBe(expected);
  });

  it("does not match a query about anything but width", () => {
    expect(matchesWidthQuery("(prefers-color-scheme: light)", 375)).toBe(false);
    expect(matchesWidthQuery("not all and (prefers-color-scheme: light)", 375)).toBe(false);
  });
});

describe("the two matchMedia doubles agree on the app's breakpoint queries", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("stubViewport stacks a 390px phone for live.tsx's negated query", () => {
    stubViewport(390);
    expect(window.matchMedia("not all and (min-width: 48rem)").matches).toBe(true);
    expect(window.matchMedia("(width >= 48rem)").matches).toBe(false);
  });

  it("phoneMatchMedia evaluates the range syntax instead of defaulting to false", () => {
    expect(phoneMatchMedia("(width < 48rem)").matches).toBe(true);
    expect(phoneMatchMedia("(width >= 48rem)").matches).toBe(false);
    expect(phoneMatchMedia("not all and (min-width: 48rem)").matches).toBe(true);
  });
});
