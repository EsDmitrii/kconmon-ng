import { useCallback, useMemo, useRef, useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { DateTimePicker } from "@/components/ui/datetime-picker";
import { useAuth } from "@/hooks/use-auth";
import { useConfirmStep } from "@/hooks/use-confirm-step";
import { useSubmitGuard } from "@/hooks/use-submit-guard";
import { ApiError, createAnnotation, deleteAnnotation, listAnnotations } from "@/lib/api";
import {
  ANNOTATION_TEXT_MAX,
  GLOBAL_SCOPE,
  defaultStartIn,
  mergeAnnotations,
  outsideWindowNote,
  type FrozenWindow,
} from "@/lib/annotations";
import { stampFull, stampShort, useLocale, useT, type Locale, type Translate } from "@/lib/i18n";
import { annotationsDict, countForm, enT, type AnnotationsKey } from "@/lib/i18n/dict/annotations";
import type { TimeWindow } from "@/components/maintenance";
import { useTimeContext, useWriteGuard } from "@/lib/timemachine";
import type { Annotation } from "@/lib/types";
import { cn } from "@/lib/utils";

/** One hook and one bar, shared by every surface, so "what window do we ask for". */

const ANNOTATIONS_POLL_MS = 60_000;

/**
 * scopeLabel names a scope for a human; `t` is optional and defaults to ENGLISH, the pattern every
 * pure function in this wave takes (pages/alerting.tsx's relativeTime, pages/settings.tsx's
 * parseBundle).
 */
export function scopeLabel(scope: string, t: Translate<AnnotationsKey> = enT): string {
  return scope === GLOBAL_SCOPE ? t("scope.global") : scope;
}

export interface AnnotationsResult {
  annotations: Annotation[];
  isLoading: boolean;
  error: Error | null;
  /** Re-reads every annotation query — what create and delete call. */
  refresh: () => Promise<void>;
}

/**
 * useAnnotations fetches the marks a surface should draw; the window is expressed as (anchor,
 * rangeSeconds) rather than as two Dates because a `new Date` computed during render changes on
 * every render and would make the query key.
 *
 * `range` is components/maintenance.tsx's useWindowAnchor, passed by a surface that also draws a
 * chart — and it is not optional decoration. Without it this hook recomputed `new Date()` on every
 * 60s poll while the chart beside it stayed frozen at its mount anchor: after twenty minutes the bar
 * described [now-1h, now] against a chart drawing [T-1h, T], so a note made since the page opened
 * was drawn as a markLine past the end of the data and notes from the chart's own first twenty
 * minutes disappeared from the bar. useMaintenance was given this parameter for exactly that
 * failure; the annotations half was left behind.
 */
export function useAnnotations(scope: string, rangeSeconds: number, range?: TimeWindow): AnnotationsResult {
  const { at } = useTimeContext();
  const qc = useQueryClient();
  /* The shared anchor keys the cache too — two surfaces on the same scope and span but different
     anchors are two different questions. */
  const anchor = range ? range.to.toISOString() : at ? at.toISOString() : "live";

  const windowFor = useCallback(() => {
    if (range) return range;
    const to = at ?? new Date();
    return { from: new Date(to.getTime() - rangeSeconds * 1000), to };
  }, [at, rangeSeconds, range]);

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

/* toLocalInputValue lived here to feed `<input type="datetime-local">`. */

/** floorToMinute drops the seconds the DateTimePicker cannot express, so the
 *  form never posts an instant the operator could not reproduce by re-opening
 *  it. Same rule maintenance.tsx's create form applies. */
function floorToMinute(d: Date): Date {
  const out = new Date(d);
  out.setSeconds(0, 0);
  return out;
}

/* Both stamps go through lib/i18n's shared helpers (QA scope 3, findings #7,
   #17 and #18): the interface language decides the WORDS in a date, an options
   bag renders words, and this row sits inches from a timeline that draws the
   same instant. A timestamp that will not parse falls back to its own bytes. */

/** fmtStamp is the full local stamp — the row's `title`. */
function fmtStamp(iso: string, locale: Locale): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : stampFull(d, locale);
}

/** fmtStampCompact is the row's VISIBLE time column; the full stamp needs about 11rem. */
function fmtStampCompact(iso: string, locale: Locale): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : stampShort(d, locale);
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
 * CreateAnnotationForm is the popover the "＋ annotate" button opens; an editable scope would let an
 * operator file a note against an object they are not looking.
 */
function CreateAnnotationForm({
  scope,
  frozenWindow,
  onDone,
  onCancel,
}: {
  scope: string;
  /** The window the list behind this form is frozen to, when it has one. It
   *  decides where the Start field OPENS — see lib/annotations' defaultStartIn. */
  frozenWindow?: FrozenWindow;
  /** Called with the instants that were STORED, so the bar can say whether they land in the window it is showing. */
  onDone: (created: { start: Date; end: Date | null }) => void;
  onCancel: () => void;
}) {
  const t = useT(annotationsDict);
  /* NOW, unless now sits outside the frozen window this bar is listing — in
     which case the window's own end, so the note lands where it will be read
     (QA scope 3, finding #5). An instant mark has no span to reserve. */
  const [start, setStart] = useState(() => floorToMinute(defaultStartIn(new Date(), frozenWindow)));
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

  /*
   * Focus goes to the field that is wrong; the lookup is fed the TRANSLATED label, from the same
   * key the control was rendered.
   */
  function focusField(label: string) {
    formRef.current?.querySelector<HTMLElement>(`[aria-label="${label}"]`)?.focus();
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const note = text.trim();
    if (note === "") {
      setError(t("form.error.noteRequired"));
      focusField(t("form.note"));
      return;
    }
    /*
     * The end-before-start test MIRRORS the store's own rule (store.AnnotationInput.Validate:
     * `EndAt.Before(StartAt)` — equal is legal).
     */
    if (end !== null && end.getTime() < start.getTime()) {
      setError(t("form.error.endBeforeStart"));
      focusField(t("form.end"));
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
      setError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : t("form.error.createFailed"));
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
      <form ref={formRef} role="form" aria-label={t("form.aria")} onSubmit={handleSubmit} className="flex flex-col gap-3">
        <p className="text-xs text-muted-foreground">
          {t("form.scope.before")}{" "}
          <span className="font-medium text-foreground">{scopeLabel(scope, t)}</span> {t("form.scope.after")}
        </p>
        {/* flex-wrap, not `sm:grid-cols-2`: `sm:` is a VIEWPORT breakpoint, so
            on a desktop the two columns were forced even inside the node
            card's 20rem rail, where each got ~150px and the control clipped
            (finding #12). Wrapping is driven by the actual width available.
            Same shape maintenance.tsx's form already uses. */}
        <div className="flex flex-wrap items-start gap-3">
          <Field label={t("form.start")}>
            <DateTimePicker aria-label={t("form.start")} value={start} onApply={(d) => setStart(floorToMinute(d))} />
          </Field>
          <Field label={t("form.end.label")} hint={t("form.end.hint")}>
            <div className="flex items-center gap-1">
              <DateTimePicker
                aria-label={t("form.end")}
                value={end}
                label={end === null ? t("form.end.unset") : undefined}
                onApply={(d) => setEnd(floorToMinute(d))}
              />
              {/* The picker can set an instant but not unset one, and an
                  optional field that cannot be emptied is not optional. */}
              {end !== null ? (
                <Button
                  type="button"
                  size="sm"
                  variant="ghost"
                  aria-label={t("form.end.clear.aria")}
                  onClick={() => setEnd(null)}
                >
                  {t("form.end.clear")}
                </Button>
              ) : null}
            </div>
          </Field>
        </div>
        <Field label={t("form.note")} hint={`${text.length}/${ANNOTATION_TEXT_MAX}`}>
          <textarea
            aria-label={t("form.note")}
            value={text}
            maxLength={ANNOTATION_TEXT_MAX}
            rows={2}
            onChange={(e) => setText(e.target.value)}
            placeholder={t("form.note.placeholder")}
            className="rounded-md bg-surface-2 px-2 py-1.5 text-sm text-foreground placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          />
        </Field>
        {error ? (
          <p role="alert" className="text-xs text-health-bad">
            {error}
          </p>
        ) : null}
        <div className="flex gap-2">
          <Button type="submit" size="sm" loading={submitting}>
            {t("form.submit")}
          </Button>
          {/* Cancel closes a form and touches nothing, so it stays live —
              targets.tsx's own create form makes the same distinction. */}
          <Button type="button" size="sm" variant="outline" onClick={onCancel}>
            {t("form.cancel")}
          </Button>
        </div>
      </form>
    </Card>
  );
}

function AnnotationRow({ annotation, canWrite, onChanged }: { annotation: Annotation; canWrite: boolean; onChanged: () => void }) {
  const t = useT(annotationsDict);
  const { locale } = useLocale();
  const guard = useWriteGuard();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  /* Second-click confirm, the exact idiom pages/alerting.tsx's rule rows use. */
  const { confirming, confirmRef, triggerRef, ask, reset } = useConfirmStep();

  async function handleDelete() {
    setBusy(true);
    setError(undefined);
    try {
      await deleteAnnotation(annotation.id);
      onChanged();
    } catch (err) {
      setError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : t("row.deleteFailed"));
      setBusy(false);
      reset();
    }
  }

  return (
    <li data-testid="annotation-item" className="flex flex-wrap items-center gap-2 py-1.5 text-xs">
      {/* Narrow, truncating and allowed to shrink — the note is what this row
          is for, and the stamp was taking a quarter of a 20rem rail for a year
          and a seconds field nobody reads here (finding #11). */}
      <span className="nums w-28 shrink-0 truncate text-muted-foreground" title={fmtStamp(annotation.startAt, locale)}>
        {fmtStampCompact(annotation.startAt, locale)}
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
        title={scopeLabel(annotation.scope, t)}
      >
        {scopeLabel(annotation.scope, t)}
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
            {/* The confirm step is SPOKEN as well as shown: the row swaps one control for two, and
                a screen reader hearing nothing reads that as "Delete did nothing". */}
            <span role="status" className="sr-only">
              {t("row.confirmDelete.aria", { text: annotation.text })}
            </span>
            <Button
              ref={confirmRef}
              type="button"
              size="sm"
              variant="outline"
              loading={busy}
              {...guard}
              aria-label={t("row.confirmDelete.aria", { text: annotation.text })}
              onClick={() => void handleDelete()}
            >
              {t("row.confirmDelete")}
            </Button>
            <Button type="button" size="sm" variant="ghost" onClick={reset}>
              {t("row.cancel")}
            </Button>
          </>
        ) : (
          <Button
            ref={triggerRef}
            type="button"
            size="sm"
            variant="ghost"
            {...guard}
            aria-label={t("row.delete.aria", { text: annotation.text })}
            onClick={ask}
          >
            {t("row.delete")}
          </Button>
        )
      ) : null}
    </li>
  );
}

/**
 * AnnotationBar is the DOM half of the overlay: the create affordance, and the list that makes each
 * marker deletable.
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
  /** What the count sentence CALLS the scope, when that differs from the value notes are filed under. */
  scopeCaption?: string;
  annotations: Annotation[];
  error?: Error | null;
  onChanged: () => void;
  /** The FROZEN range this bar is listing, when it has one (Investigate).
   *  Enables the out-of-window note after a create — see outsideWindowNote. */
  frozenWindow?: FrozenWindow;
  className?: string;
}) {
  const t = useT(annotationsDict);
  const { locale } = useLocale();
  const { can } = useAuth();
  const guard = useWriteGuard();
  const canWrite = can("annotations:write");
  const [open, setOpen] = useState(false);
  const [createdNote, setCreatedNote] = useState<string>();
  const triggerRef = useRef<HTMLButtonElement>(null);

  /*
   * "Created — outside this window" is a statement about ONE scope and ONE
   * window, and it used to outlive both: reframing the investigation, deleting
   * the row it described or navigating to another incident all left it standing
   * until a hard reload (QA scope 3, finding #4). Reset during render on the
   * identity it is about — React's own "resetting state when a prop changes"
   * pattern, the same one components/investigation-timeline.tsx uses for its
   * page number.
   */
  const noteScope = `${scope}|${frozenWindow ? `${frozenWindow.from.getTime()}-${frozenWindow.to.getTime()}` : "live"}`;
  const [seenScope, setSeenScope] = useState(noteScope);
  if (seenScope !== noteScope) {
    setSeenScope(noteScope);
    setCreatedNote(undefined);
  }

  /* A DELETE anywhere in the list retires the note too: it named a row, and the
     row may well be the one that just went. onChanged is what every mutation
     below calls, so wrapping it once covers all of them; the create path sets
     the note again after calling this, which is the order onDone already has. */
  const handleChanged = () => {
    setCreatedNote(undefined);
    onChanged();
  };

  /* Where focus goes when the form closes. */
  const closeAndRefocus = () => {
    setOpen(false);
    triggerRef.current?.focus();
  };

  return (
    <div data-testid="annotation-bar" className={cn("mt-3 flex flex-col gap-2", className)}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs text-muted-foreground">
          {error
            ? t("bar.unavailable")
            : t(`bar.count.${countForm(locale, annotations.length)}` as AnnotationsKey, {
                count: annotations.length,
                scope: scopeCaption ?? scopeLabel(scope, t),
              })}
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
            {t("bar.create")}
          </Button>
        ) : null}
      </div>

      {open ? (
        <CreateAnnotationForm
          scope={scope}
          frozenWindow={frozenWindow}
          onDone={({ start, end }) => {
            closeAndRefocus();
            handleChanged();
            /* Normal in-window creates stay SILENT — the row appearing in the list below is the feedback. */
            setCreatedNote(outsideWindowNote(start, end, frozenWindow, t, locale) ?? undefined);
          }}
          onCancel={closeAndRefocus}
        />
      ) : null}

      {createdNote ? (
        <p role="status" className="text-[11px] leading-relaxed text-muted-foreground">
          {createdNote}
        </p>
      ) : null}

      {/* NEWEST FIRST for the reader. mergeAnnotations sorts ascending because the chart's markLines
          and the window arithmetic read it that way; a list of notes is read from the top, and the
          note somebody just wrote belongs there. */}
      {annotations.length > 0 ? (
        <ul aria-label={t("bar.list.aria")} className="m-0 divide-y divide-border/60 p-0">
          {[...annotations].reverse().map((a) => (
            <AnnotationRow key={a.id} annotation={a} canWrite={canWrite} onChanged={handleChanged} />
          ))}
        </ul>
      ) : null}
    </div>
  );
}
