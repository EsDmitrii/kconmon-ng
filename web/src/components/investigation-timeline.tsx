import { ChevronLeft, ChevronRight, Pin, PinOff } from "lucide-react";
import { useState } from "react";
import { Badge, type BadgeProps } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { stampClock, useLocale, useT, type Locale } from "@/lib/i18n";
import { countForm, investigateDict, type InvestigateKey } from "@/lib/i18n/dict/investigate";
import {
  TIMELINE_DEFAULT_PAGE_SIZE,
  TIMELINE_PAGE_SIZES,
  pageOfIndex,
  timelineSlice,
  type TimelineEntry,
  type TimelineKind,
  type TimelinePageSize,
} from "@/lib/investigation";
import { pinKey, pinnedRefFor } from "@/lib/investigation-sources";
import { cn } from "@/lib/utils";

/** All three are computed from the whole merged array and rendered in the header on every page. */

/** The selector's options. Module-level: the labels are the numbers themselves
 *  and rebuilding the array per render would only churn Segmented's measure. */
const PAGE_SIZE_OPTIONS = TIMELINE_PAGE_SIZES.map((n) => ({ value: String(n), label: String(n) }));

/** Below this there is nothing for a page-size control to do — every option
 *  shows the whole list — so no chrome is drawn at all. */
const PAGER_FLOOR = TIMELINE_PAGE_SIZES[0];

/** Lifted verbatim from ui/datetime-picker.tsx's month pager, which is this
 *  console's only other prev/next control. One idiom, not two. */
const PAGER_BUTTON_CLASS =
  "grid size-7 place-items-center rounded-md text-muted-foreground transition-colors duration-(--dur-fast) ease-(--ease) hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-30";

/**
 * KIND_KEY maps a source onto its badge text in lib/i18n/dict/investigate.ts; the words there are
 * deliberately operator vocabulary rather than the API's.
 */
export const KIND_KEY: Record<TimelineKind, InvestigateKey> = {
  event: "kind.event",
  audit: "kind.audit",
  annotation: "kind.annotation",
  "path-change": "kind.pathChange",
  run: "kind.run",
  k8s: "kind.k8s",
  maintenance: "kind.maintenance",
  threshold: "kind.threshold",
  alert: "kind.alert",
};

const SEVERITY_VARIANT: Record<TimelineEntry["severity"], NonNullable<BadgeProps["variant"]>> = {
  info: "neutral",
  warn: "warn",
  error: "bad",
};

/** The severity tint is a left border rather than a filled row: a wall of
 *  coloured backgrounds makes the ONE red row impossible to find, which is the
 *  opposite of what a severity tint is for. */
const SEVERITY_BORDER: Record<TimelineEntry["severity"], string> = {
  info: "border-l-border",
  warn: "border-l-health-warn",
  error: "border-l-health-bad",
};

export interface SourceNote {
  id: string;
  text: string;
  /** True when this line describes a source that FAILED, as opposed to one that was never asked. */
  failed?: boolean;
}

/**
 * TimelineRow is hoverable AND focusable, and both gestures move the shared cursor; keyboard parity
 * is the point: the cursor sync is the pane's whole reason for existing.
 */
function TimelineRow({
  entry,
  active,
  locale,
  onCursor,
  pinning,
}: {
  entry: TimelineEntry;
  active: boolean;
  /** Passed rather than read per row: this list renders up to a hundred of
   *  them, and the clock is one of the four stamp shapes lib/i18n now owns. */
  locale: Locale;
  onCursor: (at: Date | null) => void;
  pinning?: PinControl;
}) {
  const t = useT(investigateDict);
  /* The toggle appears only when there is an incident to pin INTO, the subject may write it. */
  const ref = pinning ? pinnedRefFor(entry) : null;
  const pinned = ref !== null && pinning !== undefined && pinning.pinnedKeys.has(pinKey(ref));

  return (
    <li
      data-testid="timeline-row"
      tabIndex={0}
      onMouseEnter={() => onCursor(entry.at)}
      onMouseLeave={() => onCursor(null)}
      onFocus={() => onCursor(entry.at)}
      onBlur={() => onCursor(null)}
      className={cn(
        "flex flex-wrap items-baseline gap-x-3 gap-y-1 border-l-2 px-3 py-2 text-sm outline-none",
        SEVERITY_BORDER[entry.severity],
        active ? "bg-surface-2" : "hover:bg-surface-2/60",
        "focus-visible:ring-2 focus-visible:ring-ring",
      )}
    >
      <span className="nums w-20 shrink-0 text-xs text-muted-foreground">{stampClock(entry.at, locale)}</span>
      <Badge variant={SEVERITY_VARIANT[entry.severity]}>{t(KIND_KEY[entry.kind])}</Badge>
      <span className="min-w-0 flex-1 break-words">{entry.title}</span>
      {ref && pinning ? (
        <button
          type="button"
          aria-pressed={pinned}
          /* entry.title is the row's own text off the wire — interpolated,
             never translated. */
          aria-label={t(pinned ? "timeline.unpin" : "timeline.pin", { title: entry.title })}
          disabled={pinning.busy}
          onClick={() => pinning.onToggle(ref)}
          className={cn(
            "shrink-0 rounded-md p-1 text-muted-foreground outline-none",
            "hover:bg-accent hover:text-accent-foreground focus-visible:ring-2 focus-visible:ring-ring",
            "disabled:opacity-50",
            pinned && "text-primary",
          )}
        >
          {pinned ? (
            <PinOff aria-hidden="true" className="size-3.5" />
          ) : (
            <Pin aria-hidden="true" className="size-3.5" />
          )}
        </button>
      ) : null}
      {entry.detail ? <span className="w-full text-xs text-muted-foreground">{entry.detail}</span> : null}
    </li>
  );
}

/** PinControl is the incident-mode half of this pane. */
export interface PinControl {
  /** pinKey() of every ref currently on the incident. */
  pinnedKeys: ReadonlySet<string>;
  /** Toggles this ref and PATCHes the WHOLE list (the API replaces `pinned`
   *  wholesale — there is no add/remove). */
  onToggle: (ref: { kind: string; id: string }) => void;
  busy: boolean;
}

export function InvestigationTimeline({
  entries,
  notes,
  loading,
  allFailed,
  cursorAt,
  onCursor,
  pinning,
  windowKey,
}: {
  entries: TimelineEntry[];
  notes: SourceNote[];
  loading: boolean;
  /**
   * Every source this investigation actually ASKED came back an error (QA scope
   * 3, finding #1). Distinct from `notes.some(n => n.failed)`, which says "some
   * of the picture is missing": with all of it missing there is no picture, and
   * the pane owes the reader an error state rather than a quieter caveat over an
   * empty list. The page computes it — only the page knows which of the eleven
   * queries were enabled.
   */
  allFailed: boolean;
  cursorAt: Date | null;
  onCursor: (at: Date | null) => void;
  pinning?: PinControl;
  /**
   * Identity of the scope+window these entries describe; the page passes the same string it keys
   * its source queries.
   */
  windowKey: string;
}) {
  const t = useT(investigateDict);
  const { locale } = useLocale();
  const cursorMs = cursorAt?.getTime() ?? null;
  const failed = notes.filter((n) => n.failed === true).length;

  const [pageSize, setPageSize] = useState<TimelinePageSize>(TIMELINE_DEFAULT_PAGE_SIZE);
  const [page, setPage] = useState(1);

  /*
   * Adjusting state during render rather than in an effect (React's own "resetting state when a
   * prop changes" pattern).
   */
  const [seenWindow, setSeenWindow] = useState(windowKey);
  if (seenWindow !== windowKey) {
    setSeenWindow(windowKey);
    setPage(1);
  }

  /*
   * The slice is DERIVED, never stored: `page` is a wish and timelineSlice grants as much of it as
   * the current list can honour.
   */
  const slice = timelineSlice(entries.length, page, pageSize);
  const visible = entries.slice(slice.start, slice.end);
  const showPager = entries.length > PAGER_FLOOR;

  /* The anchor: keep whatever was at the top of the page at the top of the new
     one. See lib/investigation.ts's pageOfIndex. */
  const changePageSize = (next: TimelinePageSize) => {
    setPage(pageOfIndex(slice.start, next));
    setPageSize(next);
  };

  return (
    <Card asChild className="overflow-hidden p-0">
      <section aria-label={t("timeline.aria")}>
        <div className="border-b border-border px-4 py-3">
          <div className="flex items-baseline justify-between gap-3">
            <h3 className="text-sm font-semibold">{t("timeline.title")}</h3>
            {/* The WINDOW's count, never the page's (see the file header), and
                it is rendered at ZERO too (QA scope 3, finding #15). A count
                that disappears when it reaches nought leaves the reader to work
                out whether the pane counted and found nothing or never counted
                at all — «0 записей в этом интервале» answers that in four
                words. Suppressed only while the first fetch is still out, where
                the number would be a claim rather than a count. */}
            {loading && entries.length === 0 ? null : (
              <span data-testid="timeline-count" className="nums text-[11px] text-muted-foreground">
                {t(`timeline.entries.${countForm(locale, entries.length)}` as InvestigateKey, {
                  count: entries.length,
                })}
              </span>
            )}
          </div>
          <ul aria-label={t("timeline.sources.aria")} className="mt-2 flex flex-col gap-1">
            {notes.map((n) => (
              <li
                key={n.id}
                {...(n.failed ? { role: "alert" as const, "data-failed": "true" } : {})}
                className={cn(
                  "text-[11px] leading-relaxed",
                  n.failed ? "text-health-bad" : "text-muted-foreground",
                )}
              >
                {n.text}
              </li>
            ))}
          </ul>
          {/* One line for the WHOLE picture, above the rows (finding #1). The
              per-source lines say which and why; this one says what that costs
              the reader, which is the thing they have to carry while reading
              everything below it. */}
          {failed > 0 ? (
            <p data-testid="timeline-partial" className="mt-2 text-[11px] font-medium leading-relaxed text-health-bad">
              {t(`timeline.partial.${countForm(locale, failed)}` as InvestigateKey, { count: failed })}
            </p>
          ) : null}

          {showPager ? (
            <div className="mt-3 flex flex-wrap items-center justify-between gap-x-4 gap-y-2 border-t border-border/60 pt-2.5">
              <div className="flex items-center gap-2">
                <span className="text-[11px] text-muted-foreground">{t("pager.size")}</span>
                <Segmented
                  aria-label={t("pager.size")}
                  options={PAGE_SIZE_OPTIONS}
                  value={String(pageSize)}
                  onChange={(v) => changePageSize(Number(v) as TimelinePageSize)}
                />
              </div>
              <div className="flex items-center gap-1">
                <button
                  type="button"
                  aria-label={t("pager.prev")}
                  disabled={slice.page <= 1}
                  onClick={() => setPage(slice.page - 1)}
                  className={PAGER_BUTTON_CLASS}
                >
                  <ChevronLeft aria-hidden="true" className="size-4" />
                </button>
                {/* aria-live so the page number is ANNOUNCED after a click: the
                    button that was pressed keeps focus and its own label never
                    changes, so nothing else would say where the reader landed. */}
                <span
                  data-testid="timeline-page-label"
                  aria-live="polite"
                  className="nums px-1 text-[11px] text-muted-foreground"
                >
                  {t("pager.page", { page: slice.page, count: slice.pageCount })}
                </span>
                <button
                  type="button"
                  aria-label={t("pager.next")}
                  disabled={slice.page >= slice.pageCount}
                  onClick={() => setPage(slice.page + 1)}
                  className={PAGER_BUTTON_CLASS}
                >
                  <ChevronRight aria-hidden="true" className="size-4" />
                </button>
              </div>
            </div>
          ) : null}
        </div>

        {loading && entries.length === 0 ? (
          <div role="status" aria-live="polite" className="flex flex-col gap-2 p-4">
            <span className="sr-only">{t("timeline.loading")}</span>
            {Array.from({ length: 4 }, (_, i) => (
              <Skeleton key={i} className="h-8 w-full" />
            ))}
          </div>
        ) : null}

        {/* EVERYTHING failed: an error state, not a caveat (QA scope 3,
            finding #1). The per-source lines above name which requests were
            refused and what the server said about each; this says the one thing
            they cannot say individually — that there is no timeline here at all,
            and that the emptiness below is a property of the console rather than
            of the fleet. role="alert" because it is the first thing a reader
            needs and the last thing they should have to infer. */}
        {allFailed ? (
          <div
            data-testid="timeline-all-failed"
            role="alert"
            className="border-t border-border/60 px-4 py-10 text-center"
          >
            <p className="text-sm font-medium text-health-bad">{t("timeline.allFailed.title")}</p>
            <p className="mx-auto mt-1 max-w-md text-xs leading-relaxed text-muted-foreground">
              {t("timeline.allFailed.body")}
            </p>
          </div>
        ) : null}

        {/* The nothing-happened claim requires EVERY enabled source to have
            settled successfully (finding #1). With one of them failed, an empty
            list is not evidence of a quiet fleet — it is evidence of a fetch
            that did not come back, and the partial line above has already said
            so rather than this sentence contradicting it. */}
        {!loading && entries.length === 0 && failed === 0 && !allFailed ? (
          <p className="px-4 py-12 text-center text-xs leading-relaxed text-muted-foreground">
            {t("timeline.empty")}
          </p>
        ) : null}

        {entries.length > 0 ? (
          <ul aria-label={t("timeline.entries.aria")} className="divide-y divide-border/60">
            {visible.map((e, i) => (
              <TimelineRow
                /* The index in the WHOLE list, not in the page: a refless row's
                   key would otherwise change identity purely by being paged to,
                   remounting it and dropping focus mid-keyboard-walk. */
                key={`${e.kind}:${e.ref?.id ?? slice.start + i}:${e.at.getTime()}`}
                entry={e}
                active={cursorMs !== null && e.at.getTime() === cursorMs}
                locale={locale}
                onCursor={onCursor}
                pinning={pinning}
              />
            ))}
          </ul>
        ) : null}
      </section>
    </Card>
  );
}
