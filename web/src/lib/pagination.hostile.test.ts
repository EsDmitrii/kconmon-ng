import { describe, expect, it } from "vitest";
import { DEFAULT_PAGE_SIZE, PAGE_SIZES, pageOfIndex, pageSlice } from "./pagination";

/**
 * pagination.ts under nonsense.
 *
 * lib/pagination.test.ts pins the arithmetic against the inputs a list actually
 * produces. This file pins the INVARIANTS — the four properties every caller of
 * pageSlice depends on without ever asserting them — against inputs no caller
 * should produce and one eventually will: a page held in state after the list
 * under it was replaced, a total that came back as a string, a size a stale
 * localStorage value put there.
 *
 * A page control that returns a slice outside the array is not a rendering bug,
 * it is an empty list under a caption claiming ninety rows.
 */

/** The four properties. Asserted for every (total, page, size) below. */
function expectSane(total: number, page: number, size: number) {
  const s = pageSlice(total, page, size);
  const label = `pageSlice(${total}, ${page}, ${size}) = ${JSON.stringify(s)}`;
  const rows = Number.isFinite(total) && total > 0 ? Math.floor(total) : 0;
  // 1. every field is a number a caption can print and Array#slice can take.
  for (const [name, v] of Object.entries(s)) {
    expect(Number.isInteger(v), `${label}: ${name} is not an integer`).toBe(true);
  }
  // 2. the page is inside the page count, and the page count is at least one.
  expect(s.pageCount, label).toBeGreaterThanOrEqual(1);
  expect(s.page, label).toBeGreaterThanOrEqual(1);
  expect(s.page, label).toBeLessThanOrEqual(s.pageCount);
  // 3. the cut is inside the array, forwards.
  expect(s.start, label).toBeGreaterThanOrEqual(0);
  expect(s.end, label).toBeGreaterThanOrEqual(s.start);
  expect(s.end, label).toBeLessThanOrEqual(rows);
  // 4. a non-empty list never hands back an empty cut. This is the one that
  //    matters: an empty page under "Showing 0 of 90" is the silent cliff.
  if (rows > 0) expect(s.end - s.start, `${label}: empty page over ${rows} rows`).toBeGreaterThan(0);
}

describe("pageSlice holds its invariants for every page a caller can be holding", () => {
  it("holds across the whole grid of realistic totals, pages and sizes", () => {
    const totals = [0, 1, 9, 10, 11, 19, 20, 21, 49, 50, 51, 99, 100, 101, 499, 500, 45_000];
    const pages = [1, 2, 3, 5, 10, 50, 5000];
    for (const total of totals) for (const page of pages) for (const size of PAGE_SIZES) expectSane(total, page, size);
  });

  it("holds for a page number no list ever had", () => {
    for (const page of [0, -1, -1e9, 1.5, 1e15, Number.NaN, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY]) {
      for (const size of PAGE_SIZES) expectSane(90, page, size);
    }
  });

  it("holds for a total that is not a count", () => {
    for (const total of [-1, 1.7, Number.NaN, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY]) {
      expectSane(total, 3, 10);
    }
  });

  it("holds for a size a stale preference could put there", () => {
    for (const size of [0, -10, 0.5, 1, 7, 1e9, Number.NaN, Number.POSITIVE_INFINITY]) expectSane(90, 3, size);
  });

  it("falls back to the product default for a size that is not a size", () => {
    // Not to "one row a page", which would draw ninety pages, and not to the
    // whole list either.
    expect(pageSlice(90, 1, Number.NaN).pageCount).toBe(90 / DEFAULT_PAGE_SIZE);
    expect(pageSlice(90, 1, 0).pageCount).toBe(90 / DEFAULT_PAGE_SIZE);
  });

  it("reads an empty list as page one of one, never page one of zero", () => {
    const s = pageSlice(0, 1, 10);
    expect(s).toEqual({ page: 1, pageCount: 1, start: 0, end: 0 });
  });

  it("puts a list of exactly one page-size on a single page", () => {
    for (const size of PAGE_SIZES) {
      const s = pageSlice(size, 1, size);
      expect(s.pageCount, `size=${size}`).toBe(1);
      expect(s.end - s.start).toBe(size);
    }
  });

  it("clamps a page held over from a longer list onto the last one that has rows", () => {
    // The exact shape of a filter change: the reader was on page 9 of 90 and
    // the list became 12 rows long under them.
    const s = pageSlice(12, 9, 10);
    expect(s.page).toBe(2);
    expect(s.start).toBe(10);
    expect(s.end).toBe(12);
  });
});

describe("pageOfIndex anchors rather than resets", () => {
  it("keeps the anchored row on the page it lands on, at every size pairing", () => {
    const total = 457;
    for (const from of PAGE_SIZES) {
      for (const to of PAGE_SIZES) {
        for (const page of [1, 2, 5, 12]) {
          const before = pageSlice(total, page, from);
          const after = pageSlice(total, pageOfIndex(before.start, to), to);
          expect(
            before.start >= after.start && before.start < Math.max(after.end, after.start + 1),
            `${from}→${to} page ${page}: row ${before.start} fell outside [${after.start}, ${after.end})`,
          ).toBe(true);
        }
      }
    }
  });

  it("answers page one for an index that is not an index", () => {
    for (const i of [-1, Number.NaN, Number.NEGATIVE_INFINITY]) expect(pageOfIndex(i, 20)).toBe(1);
  });

  it("answers a whole page number for a size that is not a size", () => {
    for (const size of [0, -1, Number.NaN]) {
      const p = pageOfIndex(50, size);
      expect(Number.isInteger(p), `size=${size} gave ${p}`).toBe(true);
      expect(p).toBeGreaterThanOrEqual(1);
    }
  });
});
