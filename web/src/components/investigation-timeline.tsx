import { Pin, PinOff } from "lucide-react";
import { useEffect, useMemo, useState } from "react";
import { Badge, type BadgeProps } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Pager, usePager } from "@/components/ui/pager";
import { Skeleton } from "@/components/ui/skeleton";
import { TIMELINE_CURSOR_SOURCE, useChartCursor } from "@/lib/chart-cursor";
import { stampShort, useLocale, useT, type Locale } from "@/lib/i18n";
import { countForm, investigateDict, type InvestigateKey } from "@/lib/i18n/dict/investigate";
import type { TimelineEntry, TimelineKind } from "@/lib/investigation";
import { pinKey, pinnedRefFor } from "@/lib/investigation-sources";
import { cn } from "@/lib/utils";

/** All three are computed from the whole merged array and rendered in the header on every page. */

/* The pager itself now comes from ui/pager.tsx: this pane wrote the idiom, and
   the owner's product rule made it every list's. */

/** Who this pane is in the page's cursor group — charts skip their own echo by
 *  source, and the timeline is never a chart, so it just needs a name. The
 *  constant lives in lib/chart-cursor.tsx because the signal column's readout
 *  reads it too. */
const CURSOR_SOURCE = TIMELINE_CURSOR_SOURCE;

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

const ROW_CLASS = "flex flex-wrap items-baseline gap-x-3 gap-y-1 border-l-2 px-3 py-2 text-sm outline-none";

/* ONE row, two shapes, the same DOM. From sm up the stamp and the badge are
   direct flex items (the wrapper is display:contents) and the title is the
   flexible column between them and the action. Below sm the wrapper is a real
   line of its own — stamp and badge — the title drops to a full-width line
   under it (basis-full, ordered past the action so the action stays on the
   first line), and the detail is the third. The phone layout is the desktop
   row reflowed, never a second copy of it. */
const STAMP_GROUP_CLASS = "flex min-w-0 flex-1 basis-0 items-baseline gap-2 sm:contents";
const STAMP_CLASS = "mono-data w-28 shrink-0 text-muted-foreground";
/* [overflow-wrap:anywhere] rather than break-all: a URL or a src→dst pair
   breaks at its separators first and mid-token only when a single token is
   wider than the column. */
const TITLE_CLASS = "order-1 min-w-0 basis-full break-words [overflow-wrap:anywhere] sm:order-none sm:flex-1";
const DETAIL_CLASS = "order-2 w-full text-xs sm:order-none";

/**
 * rowKey is a row's IDENTITY — its React key, and what the pane highlights by.
 *
 * `index` is the position in the WHOLE list, not in the page: a refless row's key
 * would otherwise change identity purely by being paged to, remounting it and
 * dropping focus mid-keyboard-walk.
 */
function rowKey(entry: TimelineEntry, index: number): string {
  return `${entry.kind}:${entry.ref?.id ?? index}:${entry.at.getTime()}`;
}

/* ── the fold: a run of read-only audit rows drawn as ONE row ─────────────── */

/**
 * The console audits its own reads, and an investigation page polls PromQL
 * through the audited proxy: on a busy hour the audit source alone contributed
 * eight of the ten rows on page one, every one of them "POST
 * /api/v1/promql/query_range · allowed". They are history and they stay in the
 * list (lib/investigation-sources.ts's auditEntries keeps them, the export
 * carries them, the count above counts them) — what changes is how a RUN of
 * them is drawn: one summary row per run, opened on request.
 *
 * The fold is a rendering decision, made here and nowhere earlier: the merge,
 * the dedupe, the onset detection and the cause ranking all still see every
 * row. FOLD_MIN is two because a single read-only call is a row like any
 * other, and a summary of one thing is longer than the thing.
 */
export const FOLD_MIN = 2;

export interface TimelineFold {
  /** Identity for React and for the open/closed set — the first row's key. */
  key: string;
  /** NEWEST FIRST, the order the pane draws in. */
  entries: TimelineEntry[];
  /** The first entry's position in the whole drawn list; each entry's own
   *  index is `index + i`, which keeps rowKey stable across a fold opening. */
  index: number;
}

export type TimelineItem =
  | { kind: "row"; entry: TimelineEntry; index: number }
  | { kind: "fold"; fold: TimelineFold };

/** A read the audit log recorded — see lib/investigation-sources.ts's isReadOnlyAudit. */
function isReadOnlyAudit(entry: TimelineEntry): boolean {
  return entry.kind === "audit" && entry.readOnly === true;
}

/**
 * foldReadOnlyAudit turns the drawn list into rows and folds. Pure, and over
 * the WHOLE list rather than the page: folding inside a page would leave the
 * page mostly empty when the run is what filled it, and the point is that page
 * one shows the things that happened.
 */
export function foldReadOnlyAudit(list: readonly TimelineEntry[]): TimelineItem[] {
  const items: TimelineItem[] = [];
  let i = 0;
  while (i < list.length) {
    if (!isReadOnlyAudit(list[i])) {
      items.push({ kind: "row", entry: list[i], index: i });
      i++;
      continue;
    }
    let end = i;
    while (end < list.length && isReadOnlyAudit(list[end])) end++;
    if (end - i < FOLD_MIN) {
      items.push({ kind: "row", entry: list[i], index: i });
      i++;
      continue;
    }
    items.push({
      kind: "fold",
      fold: { key: `fold:${rowKey(list[i], i)}`, entries: list.slice(i, end), index: i },
    });
    i = end;
  }
  return items;
}

/**
 * FoldRow is the summary: the newest call's stamp, the audit badge, how many
 * calls, and the verb that opens them. The verb is the row's only new text
 * treatment — the whole sentence is in aria-label and title. Nothing here moves
 * the shared cursor: a run spans a stretch of time, and a cursor is an instant.
 */
function FoldRow({
  fold,
  open,
  locale,
  onToggle,
}: {
  fold: TimelineFold;
  open: boolean;
  locale: Locale;
  onToggle: () => void;
}) {
  const t = useT(investigateDict);
  const count = fold.entries.length;
  const newest = fold.entries[0];
  const oldest = fold.entries[count - 1];
  const sentence = t(open ? "timeline.fold.hide.aria" : "timeline.fold.show.aria", { count });
  const from = stampShort(oldest.at, locale);
  const to = stampShort(newest.at, locale);
  return (
    <li data-testid="timeline-fold" className={cn(ROW_CLASS, "border-l-border")}>
      <div className={STAMP_GROUP_CLASS}>
        <span className={STAMP_CLASS}>{to}</span>
        <Badge variant="neutral">{t("kind.audit")}</Badge>
      </div>
      <span className={cn(TITLE_CLASS, "text-muted-foreground")}>
        {t(`timeline.fold.${countForm(locale, count)}` as InvestigateKey, { count })}
      </span>
      <Button
        type="button"
        size="sm"
        variant="ghost"
        aria-expanded={open}
        aria-label={sentence}
        title={sentence}
        onClick={onToggle}
        /* -my-1.5: the 32px control sits on the text's own line height rather
           than making the summary row taller than the rows it stands for. */
        className="-my-1.5 shrink-0"
      >
        {t(open ? "timeline.fold.hide" : "timeline.fold.show")}
      </Button>
      {/* The stretch the run covers, only when it is wider than one minute —
          the newest stamp alone would let thirty minutes of polling read as one. */}
      {from !== to ? (
        <span className={cn(DETAIL_CLASS, "mono-data text-muted-foreground")}>
          {from} – {to}
        </span>
      ) : null}
    </li>
  );
}

/**
 * TimelineRow is hoverable AND focusable, and both gestures move the shared cursor; keyboard parity
 * is the point: the cursor sync is the pane's whole reason for existing.
 *
 * The HIGHLIGHT, though, is this row's own identity and not the cursor. It used
 * to be `entry.at === cursorAt`, and an instant is shared — every alert already
 * firing when the window opens sits at the window's own start, and two audit rows
 * can land in one second — so hovering one row lit the whole batch as a single
 * block. The cursor still travels by instant, because that is what the signal
 * charts read; the highlight travels by row.
 */
function TimelineRow({
  entry,
  active,
  locale,
  onEnter,
  onLeave,
  pinning,
}: {
  entry: TimelineEntry;
  active: boolean;
  /** Passed rather than read per row: this list renders up to a hundred of
   *  them, and the clock is one of the four stamp shapes lib/i18n now owns. */
  locale: Locale;
  onEnter: () => void;
  onLeave: () => void;
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
      onMouseEnter={onEnter}
      onMouseLeave={onLeave}
      onFocus={onEnter}
      onBlur={onLeave}
      className={cn(
        ROW_CLASS,
        SEVERITY_BORDER[entry.severity],
        active ? "bg-surface-2" : "hover:bg-surface-2/60",
        "focus-visible:ring-2 focus-visible:ring-ring",
      )}
    >
      <div className={STAMP_GROUP_CLASS}>
        {/* stampShort, not stampClock: the window is arbitrary (?from/?to, an incident permalink, and
            the 6h preset any time the operator looks between 00:00 and 06:00), so a clock alone made
            newest-first rows across midnight read as out of order — 23:50 sitting under 00:10. The
            column carries the day when there is one to carry. */}
        <span className={STAMP_CLASS}>{stampShort(entry.at, locale)}</span>
        <Badge variant={SEVERITY_VARIANT[entry.severity]}>{t(KIND_KEY[entry.kind])}</Badge>
      </div>
      <span className={TITLE_CLASS}>{entry.title}</span>
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
      {/* detailTitle keeps the machine identity a readable detail replaced (raw audit subject) one hover away. */}
      {entry.detail ? <span className={DETAIL_CLASS} title={entry.detailTitle}>{entry.detail}</span> : null}
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
  asked,
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
  /**
   * At least one source was actually ASKED (QA scope 4).
   *
   * `allFailed`'s mirror image, and the other half of the same rule: an empty
   * list is only evidence of a quiet fleet when somebody put the question. A
   * subject holding none of the eight read permissions had every query disabled,
   * so nothing failed, nothing was pending — and the pane read that settled
   * nothing as "Nothing happened in this window", a verdict on a fleet it had
   * not looked at. The per-source notes above already say, line by line, which
   * permission is missing; what this suppresses is the sentence that contradicts
   * them.
   */
  asked: boolean;
  /**
   * Where the shared cursor goes: the pane WRITES it and never reads it back.
   * It used to take the instant back as `cursorAt` and light every row sitting
   * on it, which turned one hover into a highlighted block wherever rows share a
   * timestamp; the highlight is row identity now, held here (see TimelineRow).
   *
   * OPTIONAL since the cursor became the page's rather than this pane's: the
   * instant goes into lib/chart-cursor.tsx's group, which every chart on the
   * page reads. A caller that wants the instant for something else still can.
   */
  onCursor?: (at: Date | null) => void;
  pinning?: PinControl;
  /**
   * Identity of the scope+window these entries describe; the page passes the same string it keys
   * its source queries.
   */
  windowKey: string;
}) {
  const t = useT(investigateDict);
  const { locale } = useLocale();
  const failed = notes.filter((n) => n.failed === true).length;

  /* Which ROW is lit. See TimelineRow: the cursor is an instant and instants are
     shared, so the highlight cannot be derived from one. */
  const [activeKey, setActiveKey] = useState<string | null>(null);
  /* The page's shared time cursor: hovering a row here is the same gesture as
     hovering a chart, and both land on the same line. */
  const cursor = useChartCursor();
  const enter = (key: string, at: Date) => {
    setActiveKey(key);
    cursor?.set(at.getTime(), CURSOR_SOURCE);
    onCursor?.(at);
  };
  const leave = () => {
    setActiveKey(null);
    cursor?.set(null, CURSOR_SOURCE);
    onCursor?.(null);
  };

  /* A new window is a new list, and the instant the reader was on is not in it. */
  useEffect(() => {
    cursor?.set(null, CURSOR_SOURCE);
  }, [cursor, windowKey]);

  /*
   * Adjusting state during render rather than in an effect (React's own "resetting state when a
   * prop changes" pattern).
   */
  /* Which folds the reader has opened, by fold key. */
  const [openFolds, setOpenFolds] = useState<ReadonlySet<string>>(() => new Set());
  const toggleFold = (key: string) =>
    setOpenFolds((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });

  const [seenWindow, setSeenWindow] = useState(windowKey);
  if (seenWindow !== windowKey) {
    setSeenWindow(windowKey);
    /* A new window is a new list; the row that was lit is not in it, and
       neither is the fold that was open. The PAGE is reset by usePager, which
       takes the same key. */
    setActiveKey(null);
    setOpenFolds(new Set());
  }

  /* NEWEST FIRST. The entries arrive ascending — mergeTimeline builds them that way and the onset
     detection and the correlation ranking both read them in that order — but a reader opening a
     window wants the most recent thing at the top, not on page 5 (owner report). The reversal is
     therefore here, at the render, and not in the data: everything computed FROM the timeline still
     sees time running forwards. */
  const newestFirst = useMemo<TimelineEntry[]>(() => [...entries].reverse(), [entries]);

  /* Then the fold, over the whole drawn list — see foldReadOnlyAudit. The
     pager pages over ROWS AS DRAWN, so "Showing 10 of 190" counts what is on
     screen against what can be paged to, while the header above keeps counting
     entries: the two are different numbers on purpose and each says which. */
  const items = useMemo(() => foldReadOnlyAudit(newestFirst), [newestFirst]);

  /* The slice, the size and the anchor all come from the shared pager now. */
  const pager = usePager(items, { resetKey: windowKey });
  const { visible } = pager;

  return (
    <Card asChild className="overflow-hidden p-0">
      <section aria-label={t("timeline.aria")}>
        <div className="border-b border-border px-4 py-3">
          <div className="flex items-baseline justify-between gap-3">
            <h2 className="type-section">{t("timeline.title")}</h2>
            {/* The WINDOW's count, never the page's (see the file header), and
                it is rendered at ZERO too (QA scope 3, finding #15). A count
                that disappears when it reaches nought leaves the reader to work
                out whether the pane counted and found nothing or never counted
                at all — «0 записей в этом интервале» answers that in four
                words. Suppressed only while the first fetch is still out, where
                the number would be a claim rather than a count. */}
            {loading && entries.length === 0 ? null : (
              <span data-testid="timeline-count" className="nums type-meta">
                {t(`timeline.entries.${countForm(locale, entries.length)}` as InvestigateKey, {
                  count: entries.length,
                })}
              </span>
            )}
          </div>
          {/* FOLDED when every source is healthy.
              Four source notes is 1 121 characters and about 230px of prose between the heading and
              the first row — on a normal laptop the panel that IS this page showed nothing but
              disclaimers until the reader scrolled. A caveat about a source that answered is worth
              having available, not worth the fold. A source that FAILED opens the list by itself and
              keeps its role="alert"; nothing about the failure path changes. */}
          <details open={failed > 0} className="mt-2 group">
            <summary
              className={cn(
                "cursor-pointer list-none text-[11px] leading-relaxed marker:content-none",
                failed > 0 ? "text-health-bad" : "text-muted-foreground",
              )}
            >
              <span className="underline decoration-dotted underline-offset-2">
                {t(`timeline.sources.summary.${countForm(locale, notes.length)}` as InvestigateKey, {
                  count: notes.length,
                })}
              </span>
            </summary>
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
          </details>
          {/* One line for the WHOLE picture, above the rows (finding #1). The
              per-source lines say which and why; this one says what that costs
              the reader, which is the thing they have to carry while reading
              everything below it. */}
          {failed > 0 ? (
            <p data-testid="timeline-partial" className="mt-2 text-[11px] font-medium leading-relaxed text-health-bad">
              {t(`timeline.partial.${countForm(locale, failed)}` as InvestigateKey, { count: failed })}
            </p>
          ) : null}

          {/* It sits in the header BELOW the caveats, so turning a page leaves
              the reader past them and the new rows already in view. */}
          <Pager pager={pager} className="mt-3 px-0 pb-0 pt-2.5" />
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
        {!loading && asked && entries.length === 0 && failed === 0 && !allFailed ? (
          <p className="px-4 py-12 text-center text-xs leading-relaxed text-muted-foreground">
            {t("timeline.empty")}
          </p>
        ) : null}

        {entries.length > 0 ? (
          <ul aria-label={t("timeline.entries.aria")} className="divide-y divide-border/60">
            {visible.map((item) => {
              if (item.kind === "row") {
                const key = rowKey(item.entry, item.index);
                return (
                  <TimelineRow
                    key={key}
                    entry={item.entry}
                    active={activeKey === key}
                    locale={locale}
                    onEnter={() => enter(key, item.entry.at)}
                    onLeave={leave}
                    pinning={pinning}
                  />
                );
              }
              const { fold } = item;
              const open = openFolds.has(fold.key);
              /* An OPEN fold is its summary followed by its rows, each a row
                 like any other — hoverable, focusable, pinnable — under the
                 same list, so a keyboard walk and the cursor sync see nothing
                 special about them. */
              return [
                <FoldRow key={fold.key} fold={fold} open={open} locale={locale} onToggle={() => toggleFold(fold.key)} />,
                ...(open
                  ? fold.entries.map((e, j) => {
                      const key = rowKey(e, fold.index + j);
                      return (
                        <TimelineRow
                          key={key}
                          entry={e}
                          active={activeKey === key}
                          locale={locale}
                          onEnter={() => enter(key, e.at)}
                          onLeave={leave}
                          pinning={pinning}
                        />
                      );
                    })
                  : []),
              ];
            })}
          </ul>
        ) : null}
      </section>
    </Card>
  );
}
