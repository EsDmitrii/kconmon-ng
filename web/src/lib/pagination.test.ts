import { describe, expect, it } from "vitest";
import { DEFAULT_PAGE_SIZE, PAGE_SIZES, pageOfIndex, pageSlice } from "./pagination";

/**
 * The console's ONE pagination arithmetic. It started life inside
 * lib/investigation.ts as the Investigate timeline's private helper; the owner's
 * rule ("every list gets pages, 10/20/50/100") made it everybody's, so it moved
 * here and the timeline became one caller among many.
 */

describe("PAGE_SIZES / DEFAULT_PAGE_SIZE", () => {
  it("offers the four sizes the product rule names, ascending", () => {
    expect([...PAGE_SIZES]).toEqual([10, 20, 50, 100]);
  });

  it("defaults to the SMALLEST of them — ten rows, everywhere", () => {
    // The owner's rule, stated once here so no surface has to restate it: a
    // list opens at a glance, and a reader who wants more says so.
    expect(DEFAULT_PAGE_SIZE).toBe(10);
    expect(DEFAULT_PAGE_SIZE).toBe(PAGE_SIZES[0]);
    expect(PAGE_SIZES).toContain(DEFAULT_PAGE_SIZE);
  });
});

describe("pageSlice", () => {
  it("cuts the first page at [0, size)", () => {
    expect(pageSlice(137, 1, 50)).toEqual({ page: 1, pageCount: 3, start: 0, end: 50 });
  });

  it("cuts a middle page at exactly one size-worth", () => {
    expect(pageSlice(137, 2, 50)).toEqual({ page: 2, pageCount: 3, start: 50, end: 100 });
  });

  it("stops the LAST page at the total rather than past it", () => {
    expect(pageSlice(137, 3, 50)).toEqual({ page: 3, pageCount: 3, start: 100, end: 137 });
  });

  it("counts an exact multiple as whole pages — never a trailing empty one", () => {
    expect(pageSlice(100, 1, 50).pageCount).toBe(2);
    expect(pageSlice(100, 2, 50)).toEqual({ page: 2, pageCount: 2, start: 50, end: 100 });
  });

  it("is ONE page for an empty list — 'Page 1 of 0' is not a thing the reader can be on", () => {
    expect(pageSlice(0, 1, 50)).toEqual({ page: 1, pageCount: 1, start: 0, end: 0 });
  });

  it("clamps a page below the first one", () => {
    expect(pageSlice(137, 0, 50).page).toBe(1);
    expect(pageSlice(137, -4, 50).page).toBe(1);
  });

  it("clamps a page that ran off the end of a list that shrank underneath it", () => {
    expect(pageSlice(30, 7, 10)).toEqual({ page: 3, pageCount: 3, start: 20, end: 30 });
  });

  it("survives a garbage page number instead of slicing with NaN", () => {
    expect(pageSlice(30, Number.NaN, 10).page).toBe(1);
  });

  it("falls back to the default size rather than slicing by zero", () => {
    expect(pageSlice(30, 1, 0)).toEqual({ page: 1, pageCount: 3, start: 0, end: 10 });
  });

  it("cuts the new 20 the same way it cuts every other size", () => {
    expect(pageSlice(90, 3, 20)).toEqual({ page: 3, pageCount: 5, start: 40, end: 60 });
  });

  it("never returns a slice wider than the size, at any page of any total", () => {
    for (const total of [0, 1, 9, 10, 11, 19, 20, 21, 49, 50, 51, 99, 100, 101, 137, 1000]) {
      for (const size of PAGE_SIZES) {
        for (let p = 1; p <= Math.max(1, Math.ceil(total / size)); p++) {
          const s = pageSlice(total, p, size);
          expect(s.end - s.start).toBeLessThanOrEqual(size);
          expect(s.start).toBeGreaterThanOrEqual(0);
          expect(s.end).toBeLessThanOrEqual(total);
          expect(s.page).toBe(p);
        }
      }
    }
  });

  it("walking every page covers the list exactly once, with no gap and no repeat", () => {
    const total = 137;
    const seen: number[] = [];
    const { pageCount } = pageSlice(total, 1, 20);
    for (let p = 1; p <= pageCount; p++) {
      const s = pageSlice(total, p, 20);
      for (let i = s.start; i < s.end; i++) seen.push(i);
    }
    expect(seen).toEqual(Array.from({ length: total }, (_, i) => i));
  });
});

describe("pageOfIndex", () => {
  it("keeps the first visible row visible when the size grows", () => {
    // Row 50 is the top of page 6 at ten a page and of page 2 at fifty.
    expect(pageOfIndex(50, 10)).toBe(6);
    expect(pageOfIndex(50, 50)).toBe(2);
  });

  it("anchors the very first row to page one at every size", () => {
    for (const size of PAGE_SIZES) expect(pageOfIndex(0, size)).toBe(1);
  });

  it("answers page one for a nonsense index rather than page zero", () => {
    expect(pageOfIndex(-3, 20)).toBe(1);
    expect(pageOfIndex(Number.NaN, 20)).toBe(1);
  });

  it("round-trips with pageSlice: anchoring on a page's own start keeps that page", () => {
    for (const size of PAGE_SIZES) {
      for (let p = 1; p <= 4; p++) {
        expect(pageOfIndex(pageSlice(1000, p, size).start, size)).toBe(p);
      }
    }
  });
});
