// viewport-stub.ts — test support: a window.matchMedia double that answers width queries for a
// resizable viewport, the way lib/fake-websocket.ts is test support for the socket.
import { act } from "@testing-library/react";
import { vi } from "vitest";

/** undefined: not a width query, which a double cannot answer either way. */
function evaluateWidth(query: string, widthPx: number, remPx: number): boolean | undefined {
  const negated = /^not all and (\(.+\))$/.exec(query);
  if (negated) {
    const inner = evaluateWidth(negated[1], widthPx, remPx);
    return inner === undefined ? undefined : !inner;
  }
  const px = (n: string, unit: string) => Number(n) * (unit === "px" ? 1 : remPx);
  const feature = /^\(\s*(min|max)-width\s*:\s*([\d.]+)(px|rem|em)\s*\)$/.exec(query);
  if (feature) {
    const limit = px(feature[2], feature[3]);
    return feature[1] === "min" ? widthPx >= limit : widthPx <= limit;
  }
  const range = /^\(\s*width\s*(>=|<=|>|<)\s*([\d.]+)(px|rem|em)\s*\)$/.exec(query);
  if (!range) return undefined;
  const limit = px(range[2], range[3]);
  switch (range[1]) {
    case ">=":
      return widthPx >= limit;
    case "<=":
      return widthPx <= limit;
    case ">":
      return widthPx > limit;
    default:
      return widthPx < limit;
  }
}

/**
 * matchesWidthQuery answers a media query the way a viewport `widthPx` CSS px wide does: min-width,
 * max-width, the range syntax and `not all and (...)`, with rem resolved against the browser's
 * default font size (what a media query uses, not the root's CSS font-size). Any other query does
 * not match.
 */
export function matchesWidthQuery(query: string, widthPx: number, remPx = 16): boolean {
  return evaluateWidth(query.trim(), widthPx, remPx) ?? false;
}

/**
 * stubViewport answers width queries through matchesWidthQuery for a resizable viewport, and fires
 * `change` on every list whose answer flips on resize.
 */
export function stubViewport(width: number, remPx = 16) {
  const lists: { query: string; matches: boolean; listeners: Set<(e: { matches: boolean }) => void> }[] = [];
  const evaluate = (query: string): boolean => matchesWidthQuery(query, width, remPx);
  vi.stubGlobal("matchMedia", (query: string) => {
    const entry = { query, matches: evaluate(query), listeners: new Set<(e: { matches: boolean }) => void>() };
    lists.push(entry);
    return {
      get matches() {
        return entry.matches;
      },
      media: query,
      onchange: null,
      addEventListener: (_: string, fn: (e: { matches: boolean }) => void) => entry.listeners.add(fn),
      removeEventListener: (_: string, fn: (e: { matches: boolean }) => void) => entry.listeners.delete(fn),
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    };
  });
  return {
    resize(next: number) {
      width = next;
      act(() => {
        for (const entry of lists) {
          const matches = evaluate(entry.query);
          if (matches === entry.matches) continue;
          entry.matches = matches;
          for (const fn of [...entry.listeners]) fn({ matches });
        }
      });
    },
  };
}
