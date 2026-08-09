import { useCallback, useMemo, useRef, useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { DateTimePicker } from "@/components/ui/datetime-picker";
import { useAuth } from "@/hooks/use-auth";
import { useSubmitGuard } from "@/hooks/use-submit-guard";
import { ApiError, createAnnotation, deleteAnnotation, listAnnotations } from "@/lib/api";
import {
  ANNOTATION_TEXT_MAX,
  GLOBAL_SCOPE,
  mergeAnnotations,
  outsideWindowNote,
  type FrozenWindow,
} from "@/lib/annotations";
import { useTimeContext, useWriteGuard } from "@/lib/timemachine";
import type { Annotation } from "@/lib/types";
import { cn } from "@/lib/utils";

/**
 * annotations.tsx — the REACT half of M5's annotations (the pure half, the
 * ECharts option builder and the list folding, is lib/annotations.ts).
 *
 * One hook and one bar, shared by every surface, so "what window do we ask
 * for", "which scopes does this surface see" and "who may write here" are
 * answered once rather than five times.
 */

const ANNOTATIONS_POLL_MS = 60_000;

/** scopeLabel names a scope for a human. "" is not "none" — it is GLOBAL, and
 *  saying so is the difference between a note an operator expects everywhere
 *  and one they think went missing. */
export function scopeLabel(scope: string): string {
  return scope === GLOBAL_SCOPE ? "global" : scope;
}

export interface AnnotationsResult {
  annotations: Annotation[];
  isLoading: boolean;
  error: Error | null;
  /** Re-reads every annotation query — what create and delete call. */
  refresh: () => Promise<void>;
}

/**
 * useAnnotations fetches the marks a surface should draw, WINDOW-BOUNDED to
 * exactly the range its chart shows.
 *
 * The window is expressed as (anchor, rangeSeconds) rather than as two Dates
 * because a `new Date()` computed during render changes on every render and
 * would make the query key — and therefore the request — churn forever. So the
 * key carries `at` (stable, seconds precision) or the literal "live", and the
 * Date is built inside queryFn. That is exactly how explore.tsx's
 * useExploreQuery already anchors its own range, and the two must agree: a
 * marker outside the plotted window is a marker in the wrong place.
 *
 * SCOPES. The endpoint matches a scope EXACTLY, and "" is a real value rather
 * than a wildcard, so a surface that wants "its own scope plus the global
 * notes" genuinely needs two requests:
 *
 *   scope === ""  → ONE request, `?scope=` (present-but-empty = global only)
 *   scope !== ""  → TWO requests, `?scope=<name>` and `?scope=`, merged
 *
 * The alternative — one request with the parameter ABSENT (every scope) and a
 * client-side filter — was rejected: it drags every other object's private
 * notes across the wire and spends the page limit on rows this surface will
 * throw away, which on a busy cluster is how the global marks fall off the end
 * of the page and quietly stop rendering.
 *
 * Both legs stop polling while the Time Machine is engaged, for the same reason
 * every other historical query does: a window that ends at a fixed past instant
 * answers the same rows forever.
 */
export function useAnnotations(scope: string, rangeSeconds: number): AnnotationsResult {
  const { at } = useTimeContext();
  const qc = useQueryClient();
  const anchor = at ? at.toISOString() : "live";

  const windowFor = useCallback(() => {
    const to = at ?? new Date();
    return { from: new Date(to.getTime() - rangeSeconds * 1000), to };
  }, [at, rangeSeconds]);

  const global = useQuery({
    queryKey: ["annotations", GLOBAL_SCOPE, anchor, rangeSeconds],
    queryFn: () => listAnnotations({ ...windowFor(), scope: GLOBAL_SCOPE }),
    refetchInterval: at ? false : ANNOTATIONS_POLL_MS,
  });

  // Always declared (hook order is not negotiable), disabled for a global
  // surface — there is no second scope to ask about.
  const scoped = useQuery({
    queryKey: ["annotations", scope, anchor, rangeSeconds],
    queryFn: () => listAnnotations({ ...windowFor(), scope }),
    enabled: scope !== GLOBAL_SCOPE,
    refetchInterval: at ? false : ANNOTATIONS_POLL_MS,
  });

  const annotations = useMemo(
    () => mergeAnnotations(global.data?.annotations ?? [], scoped.data?.annotations ?? []),
    [global.data, scoped.data],
  );

  const refresh = useCallback(async () => {
    await qc.invalidateQueries({ queryKey: ["annotations"] });
  }, [qc]);

  return {
    annotations,
    isLoading: global.isLoading || (scope !== GLOBAL_SCOPE && scoped.isLoading),
    error: (global.error ?? (scope !== GLOBAL_SCOPE ? scoped.error : null)) as Error | null,
    refresh,
  };
}

/* toLocalInputValue lived here to feed `<input type="datetime-local">`. Both
   annotate forms now use the M5 DateTimePicker (QA round 2, finding #13), so
   the string round-trip — and the timezone trap it existed to avoid — is gone
   with it: the picker speaks Dates. */

/** floorToMinute drops the seconds the DateTimePicker cannot express, so the
 *  form never posts an instant the operator could not reproduce by re-opening
 *  it. Same rule maintenance.tsx's create form applies. */
function floorToMinute(d: Date): Date {
  const out = new Date(d);
  out.setSeconds(0, 0);
  return out;
}

/** fmtStamp is the full local stamp — the row's `title`, and its fallback for
 *  a timestamp that will not parse. */
function fmtStamp(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}

/**
 * fmtStampCompact is the row's VISIBLE time column (QA round 2, finding #11).
 *
 * A full toLocaleString needs about 11rem; the column was 10rem and never
 * shrank, so on the node card's 20rem rail it ate the note. Month-day-time is
 * the shortest form that stays unambiguous over the 24 hours these lists span
 * — the seconds and the year are what the note needed the room for — and the
 * whole stamp stays one hover away on `title`.
 */
function fmtStampCompact(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

function Field({
  label,
  children,
  hint,
}: {
  label: string;
  children: React.ReactNode;
  hint?: string;
}) {
  return (
    <label className="flex flex-col gap-1 text-xs">
      <span className="font-medium text-muted-foreground">{label}</span>
      {children}
      {hint ? <span className="text-[11px] text-muted-foreground">{hint}</span> : null}
    </label>
  );
}

/**
 * CreateAnnotationForm is the popover the "＋ annotate" button opens.
 *
 * The scope is FIXED to the surface, shown and not editable. An editable scope
 * would let an operator file a note against an object they are not looking at,
 * from a form that names a different one — the mark would then be invisible
 * here and appear somewhere they never visit. A note belongs to the thing you
 * were reading when you wrote it.
 *
 * BOTH EDGES GO THROUGH THE DateTimePicker (QA round 2, finding #13). The raw
 * `<input type="datetime-local">` this form used was the control the M5 picker
 * was built to replace, and keeping it here meant the console asked for an
 * instant two different ways depending on which form you opened — and the raw
 * one clipped to unusability inside the node card's 20rem rail (finding #12).
 *
 * NEITHER edge allows a future instant. An annotation is a record of something
 * that HAPPENED — "rolled the gateway", not "will roll the gateway" — and a
 * mark drawn on a chart at a time no data exists for yet is a mark nobody can
 * check. The API itself does not care (store.AnnotationInput.Validate enforces
 * only a non-empty ≤1024-byte text and end-not-before-start, no future bound),
 * so this is a product rule and it is stated here rather than assumed:
 * maintenance windows, which ARE declared in advance, keep allowFuture.
 *
 * The start defaults to NOW rather than to a clicked point on the chart:
 * ECharts click plumbing is not wired in this milestone (see the task report),
 * and a default an operator can see and correct beats one derived from a pixel.
 */
function CreateAnnotationForm({
  scope,
  onDone,
  onCancel,
}: {
  scope: string;
  /** Called with the instants that were STORED, so the bar can say whether they
   *  land in the window it is showing (QA round 3, finding #8). */
  onDone: (created: { start: Date; end: Date | null }) => void;
  onCancel: () => void;
}) {
  const [start, setStart] = useState(() => floorToMinute(new Date()));
  /* null, not a Date: absence is the whole meaning of an INSTANT mark, and a
     picker seeded with "now" would make every note a span by default. */
  const [end, setEnd] = useState<Date | null>(null);
  const [text, setText] = useState("");
  /* The in-flight guard, not just a disabled look (QA round 5, finding #17):
     begin() is a REF write, so three clicks in one task produce one request.
     hooks/use-submit-guard.ts says why a useState flag cannot do this. */
  const { submitting, begin, end: endSubmit } = useSubmitGuard();
  const [error, setError] = useState<string>();
  const formRef = useRef<HTMLFormElement>(null);

  /* Focus goes to the field that is wrong (QA round 2, finding #20). Located
     by aria-label inside this form: the note is a plain textarea and the two
     edges are DateTimePicker triggers, which forward no ref — one lookup that
     works for all three beats three different plumbings. */
  function focusField(label: string) {
    formRef.current?.querySelector<HTMLElement>(`[aria-label="${label}"]`)?.focus();
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const note = text.trim();
    if (note === "") {
      setError("A note is required.");
      focusField("Note");
      return;
    }
    /* The end-before-start test MIRRORS the store's own rule
       (store.AnnotationInput.Validate: `EndAt.Before(StartAt)` — equal is
       legal). It is not a substitute for it: the server still answers 422 and
       that answer still lands verbatim below. It exists so the common mistake
       costs a sentence in the reader's own timezone instead of a round trip
       that comes back naming UTC instants and a field path (finding #17). */
    if (end !== null && end.getTime() < start.getTime()) {
      setError("End is before start.");
      focusField("End");
      return;
    }
    setError(undefined);
    if (!begin()) return;
    try {
      // endAt is OMITTED, never sent empty: its absence is what makes this an
      // instant mark rather than a zero-length span (lib/annotations.ts's
      // isInstant reads exactly this).
      await createAnnotation({
        startAt: start.toISOString(),
        ...(end ? { endAt: end.toISOString() } : {}),
        scope,
        text: note,
      });
      onDone({ start, end });
    } catch (err) {
      setError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : "Failed to create the annotation");
      endSubmit();
    }
  }

  return (
    <Card asChild className="p-4">
      {/* role="form", not role="dialog" (QA round 3, finding #15). This is a
          DISCLOSURE: the page behind it stays live and interactive, focus is
          not trapped, and Escape does not dismiss it — none of which is what a
          dialog role promises a screen-reader user. Escape-to-discard is
          deliberately absent rather than missing: the form holds a typed draft
          somebody is mid-way through, and a single stray keypress that threw it
          away with no undo is exactly the mis-aimed-gesture problem the
          confirm-delete idiom exists to prevent. Cancel is the way out, and it
          is one Tab away. */}
      <form ref={formRef} role="form" aria-label="New annotation" onSubmit={handleSubmit} className="flex flex-col gap-3">
        <p className="text-xs text-muted-foreground">
          Scope <span className="font-medium text-foreground">{scopeLabel(scope)}</span> — fixed to this view.
        </p>
        {/* flex-wrap, not `sm:grid-cols-2`: `sm:` is a VIEWPORT breakpoint, so
            on a desktop the two columns were forced even inside the node
            card's 20rem rail, where each got ~150px and the control clipped
            (finding #12). Wrapping is driven by the actual width available.
            Same shape maintenance.tsx's form already uses. */}
        <div className="flex flex-wrap items-start gap-3">
          <Field label="Start">
            <DateTimePicker aria-label="Start" value={start} onApply={(d) => setStart(floorToMinute(d))} />
          </Field>
          <Field label="End (optional)" hint="Leave unset for a mark at a single moment.">
            <div className="flex items-center gap-1">
              <DateTimePicker
                aria-label="End"
                value={end}
                label={end === null ? "Not set" : undefined}
                onApply={(d) => setEnd(floorToMinute(d))}
              />
              {/* The picker can set an instant but not unset one, and an
                  optional field that cannot be emptied is not optional. */}
              {end !== null ? (
                <Button type="button" size="sm" variant="ghost" aria-label="Clear end" onClick={() => setEnd(null)}>
                  Clear
                </Button>
              ) : null}
            </div>
          </Field>
        </div>
        <Field label="Note" hint={`${text.length}/${ANNOTATION_TEXT_MAX}`}>
          <textarea
            aria-label="Note"
            value={text}
            maxLength={ANNOTATION_TEXT_MAX}
            rows={2}
            onChange={(e) => setText(e.target.value)}
            placeholder="Rolled the gateway"
            className="rounded-md bg-surface-2 px-2 py-1.5 text-sm text-foreground placeholder:text-muted-foreground/70 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          />
        </Field>
        {error ? (
          <p role="alert" className="text-xs text-health-bad">
            {error}
          </p>
        ) : null}
        <div className="flex gap-2">
          <Button type="submit" size="sm" loading={submitting}>
            Create annotation
          </Button>
          {/* Cancel closes a form and touches nothing, so it stays live —
              targets.tsx's own create form makes the same distinction. */}
          <Button type="button" size="sm" variant="outline" onClick={onCancel}>
            Cancel
          </Button>
        </div>
      </form>
    </Card>
  );
}

function AnnotationRow({ annotation, canWrite, onChanged }: { annotation: Annotation; canWrite: boolean; onChanged: () => void }) {
  const guard = useWriteGuard();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  /* Second-click confirm, the exact idiom pages/alerting.tsx's rule rows use
     (QA round 2, finding #14). A single click used to destroy a note with no
     undo anywhere in this console — and the button sits at the end of a dense
     row, right where a mis-aimed click lands. Not a modal: the row already
     owns the space for a second control, and a dialog for one line of text is
     more ceremony than the act deserves. */
  const [confirming, setConfirming] = useState(false);

  async function handleDelete() {
    setBusy(true);
    setError(undefined);
    try {
      await deleteAnnotation(annotation.id);
      onChanged();
    } catch (err) {
      setError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : "Failed to delete");
      setBusy(false);
      setConfirming(false);
    }
  }

  return (
    <li data-testid="annotation-item" className="flex flex-wrap items-center gap-2 py-1.5 text-xs">
      {/* Narrow, truncating and allowed to shrink — the note is what this row
          is for, and the stamp was taking a quarter of a 20rem rail for a year
          and a seconds field nobody reads here (finding #11). */}
      <span className="nums w-28 shrink-0 truncate text-muted-foreground" title={fmtStamp(annotation.startAt)}>
        {fmtStampCompact(annotation.startAt)}
      </span>
      <span data-testid="annotation-text" className="min-w-0 flex-1 truncate" title={annotation.text}>
        {annotation.text}
      </span>
      {/* The scope column now truncates too (QA round 3, finding #11). Round 2
          gave the stamp a w-28 cap, but the scope kept its natural width, and a
          pair scope ("node-a→node-b") is wider than the stamp: inside the
          Investigate page's 24rem right column the note — the one thing this
          row exists to show — was squeezed to about 38px. A scope the reader
          already chose is the cheapest thing on the row to shorten, and the
          whole value stays one hover away. */}
      <span
        className="max-w-[7rem] shrink-0 truncate text-[11px] text-muted-foreground"
        title={scopeLabel(annotation.scope)}
      >
        {scopeLabel(annotation.scope)}
      </span>
      {error ? (
        <span role="alert" className="text-[11px] text-health-bad">
          {error}
        </span>
      ) : null}
      {/* Permission decides whether this EXISTS; time decides whether it is
          usable — lib/timemachine.tsx's useWriteGuard documents the split, and
          this is the composition it prescribes. */}
      {canWrite ? (
        confirming ? (
          <>
            <Button
              type="button"
              size="sm"
              variant="outline"
              loading={busy}
              {...guard}
              aria-label={`Confirm delete annotation: ${annotation.text}`}
              onClick={() => void handleDelete()}
            >
              Confirm delete
            </Button>
            <Button type="button" size="sm" variant="ghost" onClick={() => setConfirming(false)}>
              Cancel
            </Button>
          </>
        ) : (
          <Button
            type="button"
            size="sm"
            variant="ghost"
            {...guard}
            aria-label={`Delete annotation: ${annotation.text}`}
            onClick={() => setConfirming(true)}
          >
            Delete
          </Button>
        )
      ) : null}
    </li>
  );
}

/**
 * AnnotationBar is the DOM half of the overlay: the create affordance, and the
 * list that makes each marker deletable.
 *
 * Why a list rather than a delete button inside the marker's own tooltip:
 * ECharts draws to a CANVAS. A tooltip there is a transient, non-focusable
 * overlay that disappears on mouseout, has no place in the tab order, and is
 * invisible to every page test in this repo (EChart is mocked — echarts.init
 * needs a 2d context jsdom does not have). A real list of the marks in the
 * window is keyboard-reachable, screen-reader-readable and testable, and the
 * canvas markers keep doing what canvas is good at: showing WHERE, with the
 * text on hover.
 */
export function AnnotationBar({
  scope,
  scopeCaption,
  annotations,
  error,
  onChanged,
  frozenWindow,
  className,
}: {
  scope: string;
  /** What the count sentence CALLS the scope, when that differs from the value
   *  notes are filed under (QA round 3, finding #7): the Investigate page's
   *  wide scopes query every scope unfiltered while still filing new notes
   *  globally, so "scope global" was naming the one value it was not filtering
   *  by. Defaults to `scope`, which is the truth everywhere else. */
  scopeCaption?: string;
  annotations: Annotation[];
  error?: Error | null;
  onChanged: () => void;
  /** The FROZEN range this bar is listing, when it has one (Investigate).
   *  Enables the out-of-window note after a create — see outsideWindowNote. */
  frozenWindow?: FrozenWindow;
  className?: string;
}) {
  const { can } = useAuth();
  const guard = useWriteGuard();
  const canWrite = can("annotations:write");
  const [open, setOpen] = useState(false);
  const [createdNote, setCreatedNote] = useState<string>();
  const triggerRef = useRef<HTMLButtonElement>(null);

  /* Where focus goes when the form closes (QA round 2, finding #20). The form
     is unmounted by then, so without this the browser drops focus on <body>
     and a keyboard user restarts their tab walk at the top of the page. It
     returns to the control that opened the form — the standard disclosure
     contract, and the same one the DateTimePicker's own Cancel/Escape keeps. */
  const closeAndRefocus = () => {
    setOpen(false);
    triggerRef.current?.focus();
  };

  return (
    <div data-testid="annotation-bar" className={cn("mt-3 flex flex-col gap-2", className)}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs text-muted-foreground">
          {error
            ? "Annotations are unavailable."
            : `${annotations.length} annotation${annotations.length === 1 ? "" : "s"} in this window · scope ${scopeCaption ?? scopeLabel(scope)}`}
        </span>
        {/* HIDE on permission, DISABLE on time. Never the other way round:
            hiding it while engaged would read as "you lost the permission". */}
        {canWrite ? (
          <Button
            ref={triggerRef}
            type="button"
            size="sm"
            variant="outline"
            className="ml-auto"
            {...guard}
            aria-expanded={open}
            onClick={() => setOpen((o) => !o)}
          >
            ＋ annotate
          </Button>
        ) : null}
      </div>

      {open ? (
        <CreateAnnotationForm
          scope={scope}
          onDone={({ start, end }) => {
            closeAndRefocus();
            onChanged();
            /* Normal in-window creates stay SILENT — the row appearing in the
               list below is the feedback, and a note on every success would be
               noise (QA round 3, finding #8). */
            setCreatedNote(outsideWindowNote(start, end, frozenWindow) ?? undefined);
          }}
          onCancel={closeAndRefocus}
        />
      ) : null}

      {createdNote ? (
        <p role="status" className="text-[11px] leading-relaxed text-muted-foreground">
          {createdNote}
        </p>
      ) : null}

      {annotations.length > 0 ? (
        <ul aria-label="Annotations in this window" className="m-0 divide-y divide-border/60 p-0">
          {annotations.map((a) => (
            <AnnotationRow key={a.id} annotation={a} canWrite={canWrite} onChanged={onChanged} />
          ))}
        </ul>
      ) : null}
    </div>
  );
}
