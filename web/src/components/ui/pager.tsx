import { useLayoutEffect, useRef, useState } from "react";
import { ChevronLeft, ChevronRight } from "lucide-react";
import { Segmented } from "@/components/ui/segmented";
import { useT } from "@/lib/i18n";
import { sharedDict } from "@/lib/i18n/dict/shared";
import { DEFAULT_PAGE_SIZE, PAGE_SIZES, pageOfIndex, pageSlice, type PageSize, type PageSlice } from "@/lib/pagination";
import { cn } from "@/lib/utils";

/**
 * Pager — the one page control in the console, and usePager the state behind it.
 *
 * Lifted out of components/investigation-timeline.tsx, which had the only
 * pagination here until the owner made it the product default: every list gets
 * portions, 10/20/50/100, because nobody should have to scroll a tab forever
 * looking for row ninety. The timeline is now one caller of this among a dozen,
 * and the arithmetic all of them share is lib/pagination.ts.
 */

/** Module-level: the labels are the numbers themselves, and rebuilding the
 *  array per render would only churn Segmented's measure. */
const SIZE_OPTIONS = PAGE_SIZES.map((n) => ({ value: String(n), label: String(n) }));

/** Below this there is nothing for a page control to do — every size shows the
 *  whole list — so no chrome is drawn at all. */
const PAGER_FLOOR = PAGE_SIZES[0];

/** Lifted verbatim from ui/datetime-picker.tsx's month pager, which was this
 *  console's first prev/next control. One idiom, not three. */
const PAGER_BUTTON_CLASS =
  "grid size-7 place-items-center rounded-md text-muted-foreground transition-colors duration-(--dur-fast) ease-(--ease) hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-30";

export interface PagerState {
  /** The cut to render, already clamped — see lib/pagination.ts. */
  slice: PageSlice;
  size: PageSize;
  /** The WHOLE list's length, which is what the caption counts against. */
  total: number;
  setPage: (page: number) => void;
  /** Anchoring, not resetting: the row at the top of the page stays on screen. */
  setSize: (size: PageSize) => void;
}

/**
 * usePager holds one list's page and size and hands back the rows to draw.
 *
 * `resetKey` is the list's IDENTITY — pass whatever string says "these are
 * different rows now" (a scope, a filter, a window). Page 7 of the previous
 * list addresses nothing in the new one.
 */
export function usePager<T>(
  items: readonly T[],
  opts: { size?: PageSize; resetKey?: string } = {},
): PagerState & { visible: T[] } {
  const [size, setSizeState] = useState<PageSize>(opts.size ?? DEFAULT_PAGE_SIZE);
  const [page, setPage] = useState(1);

  /* Adjusting state during render rather than in an effect — React's own
     "resetting state when a prop changes" pattern, the same way the timeline
     reset its page on a new window. */
  const [seenKey, setSeenKey] = useState(opts.resetKey);
  if (seenKey !== opts.resetKey) {
    setSeenKey(opts.resetKey);
    setPage(1);
  }

  /* DERIVED, never stored: `page` is a wish, and pageSlice grants as much of it
     as the current list can honour. */
  const slice = pageSlice(items.length, page, size);

  return {
    slice,
    size,
    total: items.length,
    visible: items.slice(slice.start, slice.end) as T[],
    setPage,
    setSize: (next: PageSize) => {
      setPage(pageOfIndex(slice.start, next));
      setSizeState(next);
    },
  };
}

/**
 * Pager draws the size selector, the honest count and the two arrows. It
 * renders NOTHING for a list that fits on one smallest page.
 */
export function Pager({
  pager,
  subject,
  truncated,
  className,
}: {
  pager: PagerState;
  /** The row's noun, already translated by the calling surface ("pairs",
   *  «Пары»). Optional: under a heading that already names them, the bare
   *  count reads better. */
  subject?: string;
  /** The listing this pager is paging was CAPPED by the server or by the page walker, so `total` is
   *  a floor rather than a count. Without it the caption asserted completeness over a partial list —
   *  the exact failure the walk-to-exhaustion fetcher exists to prevent, moved one layer up. */
  truncated?: boolean;
  className?: string;
}) {
  /*
  The two arrows hold each other's focus. An arrow that disables under the finger is blurred by the
  browser and focus falls to <body>, so the next Tab restarts at the top of the document instead of
  continuing from the pager.

  The handover runs AFTER the page changed, not inside the click: on a two-page list the opposite
  arrow is still in its old disabled state at click time, and .focus() on a disabled button does
  nothing — which is exactly the shape where this happens most.
  */
  const prevRef = useRef<HTMLButtonElement>(null);
  const nextRef = useRef<HTMLButtonElement>(null);
  const handoff = useRef<"prev" | "next" | null>(null);
  useLayoutEffect(() => {
    const to = handoff.current;
    if (!to) return;
    handoff.current = null;
    (to === "prev" ? prevRef : nextRef).current?.focus();
  });
  const t = useT(sharedDict);
  const { slice, size, total } = pager;
  if (total <= PAGER_FLOOR) return null;
  const shown = slice.end - slice.start;

  return (
    <div
      data-testid="pager"
      className={cn(
        "flex flex-wrap items-center justify-between gap-x-4 gap-y-2 border-t border-border/60 px-4 py-2.5",
        className,
      )}
    >
      <div className="flex items-center gap-2">
        {/* nowrap + shrink-0: this label was the only shrinkable item in the row, so inside a narrow
            rail it collapsed to the width of its longest word and stacked onto three lines. The row
            already declares flex-wrap — let it wrap there instead. */}
        <span className="shrink-0 whitespace-nowrap text-[11px] text-muted-foreground">{t("pager.size")}</span>
        <Segmented
          aria-label={t("pager.size")}
          options={SIZE_OPTIONS}
          value={String(size)}
          onChange={(v) => pager.setSize(Number(v) as PageSize)}
        />
      </div>

      {/* What is on screen against what there is — the sentence that makes a
          page honest instead of a silent cliff. */}
      <p data-testid="pager-showing" className="nums text-[11px] text-muted-foreground">
        {subject
          ? t("pager.showing.of", { shown, total, subject })
          : t("pager.showing", { shown, total })}
        {truncated ? <span data-testid="pager-truncated"> · {t("pager.truncated")}</span> : null}
      </p>

      <div className="flex items-center gap-1">
        <button
          ref={prevRef}
          type="button"
          aria-label={t("pager.prev")}
          disabled={slice.page <= 1}
          onClick={() => {
            if (slice.page - 1 <= 1) handoff.current = "next";
            pager.setPage(slice.page - 1);
          }}
          className={PAGER_BUTTON_CLASS}
        >
          <ChevronLeft aria-hidden="true" className="size-4" />
        </button>
        {/* aria-live so the page number is ANNOUNCED after a click: the button
            that was pressed keeps focus and its own label never changes, so
            nothing else would say where the reader landed. */}
        <span data-testid="pager-page" aria-live="polite" className="nums px-1 text-[11px] text-muted-foreground">
          {t("pager.page", { page: slice.page, count: slice.pageCount })}
        </span>
        <button
          ref={nextRef}
          type="button"
          aria-label={t("pager.next")}
          disabled={slice.page >= slice.pageCount}
          onClick={() => {
            if (slice.page + 1 >= slice.pageCount) handoff.current = "prev";
            pager.setPage(slice.page + 1);
          }}
          className={PAGER_BUTTON_CLASS}
        >
          <ChevronRight aria-hidden="true" className="size-4" />
        </button>
      </div>
    </div>
  );
}
