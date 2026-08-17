/**
 * pagination.ts — the console's ONE page arithmetic, and the sizes every list
 * offers.
 *
 * It was the Investigate timeline's private helper (lib/investigation.ts) until
 * the owner made pages the product default for every list: "я не хочу открывать
 * какую-то вкладку и сидеть листать бесконечно страницу". Extracted rather than
 * copied, so a rule about page counts is decided in one place and the Investigate
 * timeline is now one caller of it among a dozen.
 *
 * Arithmetic only — no React, no DOM. The state and the chrome live in
 * components/ui/pager.tsx, which is what a surface actually mounts.
 */

/** The sizes the selector offers, ascending. 20 joined the original three when
 *  the rule went product-wide: ten is a glance, a hundred is a scroll, and the
 *  gap between ten and fifty was the one every short list fell into. */
export const PAGE_SIZES = [10, 20, 50, 100] as const;

export type PageSize = (typeof PAGE_SIZES)[number];

/** Ten, on every list in the console. The owner made this uniform after three
 *  surfaces had picked 20 and 50 for themselves: a page size that differs per
 *  tab is a number the reader cannot trust, and a list is meant to open at a
 *  glance. A reader who wants more says so with the selector. */
export const DEFAULT_PAGE_SIZE: PageSize = PAGE_SIZES[0];

export interface PageSlice {
  /** 1-based, CLAMPED into [1, pageCount]. Render this, and step prev/next
   *  from it — a stale number stored in state must never address a row. */
  page: number;
  /** At least 1. An empty list is page 1 of 1, never "page 1 of 0". */
  pageCount: number;
  /** Half-open [start, end) into the caller's array. */
  start: number;
  end: number;
}

/** pageSlice turns (total, page, size) into the exact cut to render. */
export function pageSlice(total: number, page: number, size: number): PageSlice {
  const rows = Number.isFinite(total) && total > 0 ? Math.floor(total) : 0;
  const per = Number.isFinite(size) && size >= 1 ? Math.floor(size) : (DEFAULT_PAGE_SIZE as number);
  const pageCount = Math.max(1, Math.ceil(rows / per));
  const wanted = Number.isNaN(page) ? 1 : page;
  const clamped = Math.min(Math.max(Math.floor(Math.min(wanted, pageCount)), 1), pageCount);
  const start = Math.min((clamped - 1) * per, rows);
  return { page: clamped, pageCount, start, end: Math.min(start + per, rows) };
}

/**
 * pageOfIndex is the page-size ANCHOR: which page holds entry `index` once the size becomes `size`;
 * changing the size keeps the first visible entry in view rather than resetting to page 1.
 */
export function pageOfIndex(index: number, size: number): number {
  if (!Number.isFinite(index) || index <= 0) return 1;
  const per = Number.isFinite(size) && size >= 1 ? Math.floor(size) : (DEFAULT_PAGE_SIZE as number);
  return Math.floor(index / per) + 1;
}
