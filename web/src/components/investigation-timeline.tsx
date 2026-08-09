import { Pin, PinOff } from "lucide-react";
import { Badge, type BadgeProps } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import type { TimelineEntry, TimelineKind } from "@/lib/investigation";
import { pinKey, pinnedRefFor } from "@/lib/investigation-sources";
import { cn } from "@/lib/utils";

/**
 * investigation-timeline.tsx — the Investigate page's centre pane: the merged,
 * time-ordered entries, and above them the honest per-source status list.
 *
 * The SOURCE LIST is not decoration. Every source is gated on its own read
 * permission and every one of them can be legitimately absent, so a timeline
 * with no audit rows is ambiguous — "nothing was configured in this window" and
 * "you may not read the audit log" look identical from a list of rows. The
 * notes say which, one line each, and the page issues zero requests for the
 * sources they describe (M6 Global Constraints).
 */

/**
 * KIND_LABEL is the badge text per source. Deliberately operator vocabulary
 * rather than the API's: "k8s" is what a cluster event is called out loud.
 *
 * `audit` says "audit", not "config change" (QA round 5, finding #19). The old
 * label was a guess about what the row means, and the guess is wrong for most
 * rows: the audit log records every authorization DECISION the API makes —
 * `GET /api/v1/targets`, `allowed`; a denied read; a login — and lib/
 * investigation-sources.ts's auditEntries filters none of them out. Labelling
 * a read "config change" tells an operator hunting a cause that somebody
 * changed something during the incident window when nobody did, which is the
 * most expensive kind of wrong a timeline can be. "audit" names the SOURCE,
 * which is what every other label here does, and the row's own title carries
 * the action verbatim.
 */
export const KIND_LABEL: Record<TimelineKind, string> = {
  event: "event",
  audit: "audit",
  annotation: "annotation",
  "path-change": "path change",
  run: "run",
  k8s: "k8s",
  maintenance: "maintenance",
  threshold: "threshold",
  alert: "alert",
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
  /**
   * True when this line describes a source that FAILED, as opposed to one that
   * was never asked (QA round 3, finding #1).
   *
   * The distinction is the whole point of the list. A source the subject cannot
   * read contributed nothing and was never requested — the timeline is complete
   * for what this session can see. A source whose request came back 500
   * contributed nothing EITHER, but the timeline is now missing rows that exist,
   * and the page must not go on to claim nothing happened. Failed lines render
   * as alerts and suppress the nothing-happened empty state below.
   */
  failed?: boolean;
}

function fmtClock(d: Date): string {
  return d.toLocaleTimeString();
}

/**
 * TimelineRow is hoverable AND focusable, and both gestures move the shared
 * cursor. Keyboard parity is the point: the cursor sync is the pane's whole
 * reason for existing, and a mouse-only affordance would put it out of reach of
 * exactly the operator who navigates a long timeline by keyboard.
 */
function TimelineRow({
  entry,
  active,
  onCursor,
  pinning,
}: {
  entry: TimelineEntry;
  active: boolean;
  onCursor: (at: Date | null) => void;
  pinning?: PinControl;
}) {
  /* The toggle appears only when there is an incident to pin INTO, the subject
     may write it, and this class of row has a store kind at all — a maintenance
     window and a derived threshold crossing have none (PIN_KIND_BY_TIMELINE_
     KIND documents why), so they get no control rather than one the server
     would reject. */
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
      <span className="nums w-20 shrink-0 text-xs text-muted-foreground">{fmtClock(entry.at)}</span>
      <Badge variant={SEVERITY_VARIANT[entry.severity]}>{KIND_LABEL[entry.kind]}</Badge>
      <span className="min-w-0 flex-1 break-words">{entry.title}</span>
      {ref && pinning ? (
        <button
          type="button"
          aria-pressed={pinned}
          aria-label={`${pinned ? "Unpin" : "Pin"}: ${entry.title}`}
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

/**
 * PinControl is the incident-mode half of this pane, passed as ONE optional
 * object rather than three loose props so the "no incident loaded" case is a
 * single `undefined` — there is no state where a pinned set is meaningful but
 * the toggle is not, and three independent booleans would allow four.
 */
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
  cursorAt,
  onCursor,
  pinning,
}: {
  entries: TimelineEntry[];
  notes: SourceNote[];
  loading: boolean;
  cursorAt: Date | null;
  onCursor: (at: Date | null) => void;
  pinning?: PinControl;
}) {
  const cursorMs = cursorAt?.getTime() ?? null;
  const failed = notes.filter((n) => n.failed === true).length;

  return (
    <Card asChild className="overflow-hidden p-0">
      <section aria-label="Timeline">
        <div className="border-b border-border px-4 py-3">
          <h3 className="text-sm font-semibold">Timeline</h3>
          <ul aria-label="Timeline sources" className="mt-2 flex flex-col gap-1">
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
              {failed} source{failed === 1 ? "" : "s"} failed; the timeline below is partial.
            </p>
          ) : null}
        </div>

        {loading && entries.length === 0 ? (
          <div role="status" aria-live="polite" className="flex flex-col gap-2 p-4">
            <span className="sr-only">Assembling the timeline…</span>
            {Array.from({ length: 4 }, (_, i) => (
              <Skeleton key={i} className="h-8 w-full" />
            ))}
          </div>
        ) : null}

        {/* The nothing-happened claim requires EVERY enabled source to have
            settled successfully (finding #1). With one of them failed, an empty
            list is not evidence of a quiet fleet — it is evidence of a fetch
            that did not come back, and the partial line above has already said
            so rather than this sentence contradicting it. */}
        {!loading && entries.length === 0 && failed === 0 ? (
          <p className="px-4 py-12 text-center text-xs leading-relaxed text-muted-foreground">
            Nothing happened in this window — no event, no configuration change and no threshold crossing from any
            source this session can read.
          </p>
        ) : null}

        {entries.length > 0 ? (
          <ul aria-label="Timeline entries" className="divide-y divide-border/60">
            {entries.map((e, i) => (
              <TimelineRow
                key={`${e.kind}:${e.ref?.id ?? i}:${e.at.getTime()}`}
                entry={e}
                active={cursorMs !== null && e.at.getTime() === cursorMs}
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
