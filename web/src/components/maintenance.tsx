import { useCallback, useMemo, useRef, useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { DateTimePicker } from "@/components/ui/datetime-picker";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { useAuth } from "@/hooks/use-auth";
import { useConfirmStep } from "@/hooks/use-confirm-step";
import { useSubmitGuard } from "@/hooks/use-submit-guard";
import { ApiError, createMaintenance, deleteMaintenance, getMaintenance } from "@/lib/api";
import {
  GLOBAL_SCOPE,
  MAINTENANCE_REASON_MAX,
  defaultStartIn,
  mergeMaintenanceWindows,
  outsideWindowNote,
  type FrozenWindow,
} from "@/lib/annotations";
import {
  stampFull,
  stampShort,
  useLocale,
  useT,
  translate,
  type Locale,
  type Translate,
} from "@/lib/i18n";
import { countForm, maintenanceDict, type MaintenanceKey } from "@/lib/i18n/dict/maintenance";
import { useTimeContext, useWriteGuard } from "@/lib/timemachine";
import type { MaintenanceWindow } from "@/lib/types";
import { cn } from "@/lib/utils";

/** The pure half — the markArea builder and the list folding. */

const MAINTENANCE_POLL_MS = 60_000;

/** enT is the ENGLISH translator this file's pure helper defaults to — the
 *  pattern dict/topology.ts and pages/alerting.tsx established. */
const enT: Translate<MaintenanceKey> = (key, vars) => translate(maintenanceDict, "en", key, vars);

/** scopeLabel names a scope for a human — "" is GLOBAL. */
export function scopeLabel(scope: string, t: Translate<MaintenanceKey> = enT): string {
  return scope === GLOBAL_SCOPE ? t("scope.global") : scope;
}

export interface MaintenanceResult {
  windows: MaintenanceWindow[];
  isLoading: boolean;
  error: Error | null;
  /** Re-reads every maintenance query — what create and delete call. */
  refresh: () => Promise<void>;
}

/** The half-open range a surface is reading — its chart's X axis and its
 *  maintenance bar's count, which have to be the same range or the count is
 *  about something the reader cannot see. */
export interface TimeWindow {
  from: Date;
  to: Date;
}

/**
 * useWindowAnchor is the ONE `now` a surface takes, for its chart AND for the
 * bar beneath it.
 *
 * Both used to call `new Date()` in their own queryFn, milliseconds to seconds
 * apart, and a window declared in that gap was counted by the bar while sitting
 * outside the range the chart had already resolved (QA scope 2, finding #20).
 * Taken ONCE per mount rather than on a ticker: the chart it must agree with
 * fetches once too, and a bar whose range crept forward under a static chart
 * would be the same bug wearing a clock.
 */
export function useWindowAnchor(rangeSeconds: number): TimeWindow {
  const { at } = useTimeContext();
  const [mountedAt] = useState(() => Date.now());
  return useMemo(() => {
    const to = at ?? new Date(mountedAt);
    return { from: new Date(to.getTime() - rangeSeconds * 1000), to };
  }, [at, mountedAt, rangeSeconds]);
}

/**
 * useMaintenance fetches the declared windows a surface should draw; everything useAnnotations
 * documents about the window and the scopes holds here verbatim.
 *
 * `range` is the shared anchor above, passed by a surface that also draws a
 * chart. Without it the hook keeps computing its own, which is right for a bar
 * that stands alone. (Named `range`, not `window`: this file already spends the
 * word `window` on a declared MaintenanceWindow.)
 */
export function useMaintenance(scope: string, rangeSeconds: number, range?: TimeWindow): MaintenanceResult {
  const { at } = useTimeContext();
  const { can } = useAuth();
  const qc = useQueryClient();
  const canRead = can("maintenance:read");
  /* The shared anchor keys the cache too — two surfaces on the same scope and
     the same span but different anchors are two different questions. */
  const anchor = range ? range.to.toISOString() : at ? at.toISOString() : "live";

  const windowFor = useCallback(() => {
    if (range) return range;
    const to = at ?? new Date();
    return { from: new Date(to.getTime() - rangeSeconds * 1000), to };
  }, [at, rangeSeconds, range]);

  const global = useQuery({
    queryKey: ["maintenance", GLOBAL_SCOPE, anchor, rangeSeconds],
    queryFn: () => getMaintenance({ ...windowFor(), scope: GLOBAL_SCOPE }),
    enabled: canRead,
    refetchInterval: at ? false : MAINTENANCE_POLL_MS,
  });

  // Always declared (hook order is not negotiable), disabled for a global
  // surface — there is no second scope to ask about.
  const scoped = useQuery({
    queryKey: ["maintenance", scope, anchor, rangeSeconds],
    queryFn: () => getMaintenance({ ...windowFor(), scope }),
    enabled: canRead && scope !== GLOBAL_SCOPE,
    refetchInterval: at ? false : MAINTENANCE_POLL_MS,
  });

  const windows = useMemo(
    () => mergeMaintenanceWindows(global.data?.windows ?? [], scoped.data?.windows ?? []),
    [global.data, scoped.data],
  );

  const refresh = useCallback(async () => {
    await qc.invalidateQueries({ queryKey: ["maintenance"] });
  }, [qc]);

  return {
    windows,
    isLoading: canRead && (global.isLoading || (scope !== GLOBAL_SCOPE && scoped.isLoading)),
    error: (global.error ?? (scope !== GLOBAL_SCOPE ? scoped.error : null)) as Error | null,
    refresh,
  };
}

/** DEFAULT_WINDOW_SECONDS is the length the create form opens on — an hour, the
 *  same span the repo's charts default to, and a value an operator corrects
 *  rather than composes from nothing. */
const DEFAULT_WINDOW_SECONDS = 3600;

/** floorToMinute drops the seconds the DateTimePicker cannot express. Without
 *  it the form would post an instant the operator never saw and could not
 *  reproduce by re-opening the picker. */
function floorToMinute(d: Date): Date {
  const out = new Date(d);
  out.setSeconds(0, 0);
  return out;
}

/* Both stamps go through lib/i18n's shared helpers — the reasoning is spelled
   out over components/annotations.tsx's pair, and the point of the change is
   that these two and that pair and the timeline's rows now agree. */

/** fmtStamp is the FULL local stamp — the row's `title`. */
function fmtStamp(iso: string, locale: Locale): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : stampFull(d, locale);
}

/** fmtStampCompact is the row's VISIBLE time column, the same treatment components/annotations.tsx got. */
function fmtStampCompact(iso: string, locale: Locale): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : stampShort(d, locale);
}

function Field({ label, children, hint }: { label: string; children: React.ReactNode; hint?: string }) {
  return (
    <div className="flex flex-col gap-1 text-xs">
      <span className="font-medium text-muted-foreground">{label}</span>
      {children}
      {hint ? <span className="text-[11px] text-muted-foreground">{hint}</span> : null}
    </div>
  );
}

/**
 * CreateMaintenanceForm is the popover the create affordance opens; the scope is FIXED to the
 * surface, shown and not editable.
 */
function CreateMaintenanceForm({
  scope,
  frozenWindow,
  onDone,
  onCancel,
}: {
  scope: string;
  /** The window the list behind this form is frozen to, when it has one. It
   *  decides where the Start field OPENS — see lib/annotations' defaultStartIn. */
  frozenWindow?: FrozenWindow;
  /** The stored edges, so the bar can say whether they land in the window it is showing. */
  onDone: (created: { start: Date; end: Date }) => void;
  onCancel: () => void;
}) {
  const t = useT(maintenanceDict);
  /* NOW, unless now sits outside the frozen window this bar lists — in which
     case the hour that ENDS at that window's end, so both edges of the declared
     window land inside the one on screen (QA scope 3, finding #5). */
  const [start, setStart] = useState(() =>
    floorToMinute(defaultStartIn(new Date(), frozenWindow, DEFAULT_WINDOW_SECONDS)),
  );
  const [end, setEnd] = useState(() => new Date(start.getTime() + DEFAULT_WINDOW_SECONDS * 1000));
  const [reason, setReason] = useState("");
  /* The in-flight guard, not just a disabled look (QA round 5, finding #17):
     begin() is a REF write, so three clicks in one task produce one request.
     hooks/use-submit-guard.ts says why a useState flag cannot do this. */
  const { submitting, begin, end: endSubmit } = useSubmitGuard();
  const [error, setError] = useState<string>();

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const why = reason.trim();
    if (why === "") {
      setError(t("form.error.reasonRequired"));
      return;
    }
    if (end.getTime() <= start.getTime()) {
      setError(t("form.error.endNotAfterStart"));
      return;
    }
    setError(undefined);
    if (!begin()) return;
    try {
      await createMaintenance({
        scope,
        startAt: start.toISOString(),
        endAt: end.toISOString(),
        reason: why,
      });
      onDone({ start, end });
    } catch (err) {
      setError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : t("form.error.createFailed"));
      endSubmit();
    }
  }

  return (
    <Card asChild className="p-4">
      {/* role="form", not role="dialog" — the twin of the annotation form's own
          change (QA round 3, finding #15), and for the same reason: this is a
          disclosure with no focus trap and no Escape-to-dismiss, and claiming
          the dialog role promises a screen-reader user three behaviours it does
          not have. Escape-to-discard stays deliberately absent: the reason box
          holds typed text, and losing it to a stray keypress has no undo. */}
      <form role="form" aria-label={t("form.aria")} onSubmit={handleSubmit} className="flex flex-col gap-3">
        <p className="text-xs text-muted-foreground">
          {t("form.scope.before")}{" "}
          <span className="font-medium text-foreground">{scopeLabel(scope, t)}</span> {t("form.scope.after")}
        </p>
        <div className="flex flex-wrap items-start gap-3">
          <Field label={t("form.start")}>
            <DateTimePicker aria-label={t("form.start")} value={start} onApply={setStart} allowFuture />
          </Field>
          <Field label={t("form.end")} hint={t("form.end.hint")}>
            <DateTimePicker aria-label={t("form.end")} value={end} onApply={setEnd} allowFuture />
          </Field>
        </div>
        <label className="flex flex-col gap-1 text-xs">
          <span className="font-medium text-muted-foreground">{t("form.reason")}</span>
          <textarea
            aria-label={t("form.reason")}
            value={reason}
            maxLength={MAINTENANCE_REASON_MAX}
            rows={2}
            onChange={(e) => setReason(e.target.value)}
            placeholder={t("form.reason.placeholder")}
            className="rounded-md bg-surface-2 px-2 py-1.5 text-sm text-foreground placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          />
          <span className="text-[11px] text-muted-foreground">
            {reason.length}/{MAINTENANCE_REASON_MAX}
          </span>
        </label>
        {error ? (
          <p role="alert" className="text-xs text-health-bad">
            {error}
          </p>
        ) : null}
        <div className="flex gap-2">
          <Button type="submit" size="sm" loading={submitting}>
            {t("form.submit")}
          </Button>
          <Button type="button" size="sm" variant="outline" onClick={onCancel}>
            {t("form.cancel")}
          </Button>
        </div>
      </form>
    </Card>
  );
}

/**
 * MaintenanceRow is one declared window and its delete; EXPORTED for pages/settings.tsx, which
 * lists the windows this bar cannot reach.
 */
export function MaintenanceRow({
  window: w,
  canWrite,
  onChanged,
}: {
  window: MaintenanceWindow;
  canWrite: boolean;
  onChanged: () => void;
}) {
  const t = useT(maintenanceDict);
  const { locale } = useLocale();
  const guard = useWriteGuard();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  /* Second-click confirm, the same idiom the annotation rows and pages/alerting.tsx's rule rows use. */
  const { confirming, confirmRef, triggerRef, ask, reset } = useConfirmStep();

  async function handleDelete() {
    setBusy(true);
    setError(undefined);
    try {
      await deleteMaintenance(w.id);
      onChanged();
    } catch (err) {
      setError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : t("row.deleteFailed"));
      setBusy(false);
      reset();
    }
  }

  return (
    <li data-testid="maintenance-item" className="flex flex-wrap items-center gap-2 py-1.5 text-xs">
      {/* Both edges, always. A window rendered by its start alone reads as an
          instant, which is the one thing it never is.

          QA round 5, finding #6: w-28 was too tight for the pair even in the
          compact form, so the END was the part that got cut — and a window
          whose end is "Aug 12, 14:…" tells an operator nothing about when the
          change actually finishes, which is the single fact they came for.

          The fix is a column that FITS and never truncates: shrink-0, so flex
          takes the space out of the REASON instead, which already truncates and
          already carries its own title. Below 700px (max-lg here matches the
          app's own breakpoint use) the range takes a full row of its own above
          the reason rather than fighting it for one line — basis-full only at
          that width.

          QA overflow sweep: w-44 did not actually fit. "Aug 9, 11:31 AM →
          Aug 9, 01:31 PM" measures 198px against an 11rem (176px) column, and
          because whitespace-nowrap sat on the WHOLE span with no clipping
          ancestor, the 22px spilled out of the box instead of wrapping. Two
          changes, and both are needed:

            - w-52 (13rem, 208px) so the ordinary range still gets its one line;
            - nowrap moved OFF the container and onto each stamp, which leaves
              the arrow as the only break opportunity. A longer form than the
              one measured (a two-digit day, a locale that spells the month out)
              now folds into a second line at the arrow — never mid-timestamp,
              never past the edge. Widening alone would only move the cliff. */}
      <span
        data-testid="maintenance-stamp"
        className="nums w-full shrink-0 basis-full text-muted-foreground lg:w-52 lg:basis-auto"
        title={`${fmtStamp(w.startAt, locale)} → ${fmtStamp(w.endAt, locale)}`}
      >
        <span className="whitespace-nowrap">{fmtStampCompact(w.startAt, locale)}</span>
        {" → "}
        <span className="whitespace-nowrap">{fmtStampCompact(w.endAt, locale)}</span>
      </span>
      {/* min-w-[10rem]: the confirm state adds a second button to this row, and
          flex took the space out of the one column that identifies WHICH window
          is about to be deleted — the reason collapsed to a single character
          under the very click that asks you to confirm it (QA scope 2, #19).
          With a floor on the column the row wraps instead, which is the same
          give the stamp already takes below lg. */}
      <span
        data-testid="maintenance-reason"
        className="min-w-0 flex-1 basis-40 truncate lg:min-w-[10rem]"
        title={w.reason}
      >
        {w.reason}
      </span>
      <span className="max-w-[7rem] shrink-0 truncate text-[11px] text-muted-foreground" title={scopeLabel(w.scope, t)}>
        {scopeLabel(w.scope, t)}
      </span>
      {error ? (
        <span role="alert" className="text-[11px] text-health-bad">
          {error}
        </span>
      ) : null}
      {/* Permission decides whether this EXISTS; time decides whether it is
          usable — lib/timemachine.tsx's useWriteGuard documents the split. */}
      {canWrite ? (
        confirming ? (
          <>
            {/* Spoken as well as drawn: the row swaps one control for two, and a reader hearing
                nothing reads the press as "Delete did nothing". */}
            <span role="status" className="sr-only">
              {t("row.confirmDelete.aria", { reason: w.reason })}
            </span>
            <Button
              ref={confirmRef}
              type="button"
              size="sm"
              variant="outline"
              loading={busy}
              {...guard}
              aria-label={t("row.confirmDelete.aria", { reason: w.reason })}
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
            aria-label={t("row.delete.aria", { reason: w.reason })}
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
 * MaintenanceBar is the DOM half: the create affordance; a list rather than a control inside the
 * band's own tooltip.
 */
export function MaintenanceBar({
  scope,
  scopeCaption,
  windows,
  error,
  onChanged,
  frozenWindow,
  createLabel,
  inline,
  className,
}: {
  scope: string;
  /** What the count sentence CALLS the scope, when the surface queried every scope rather than this one. */
  scopeCaption?: string;
  windows: MaintenanceWindow[];
  error?: Error | null;
  onChanged: () => void;
  /** The FROZEN range this bar is listing, when it has one (Investigate) —
   *  see lib/annotations.ts's outsideWindowNote. */
  frozenWindow?: FrozenWindow;
  /** The create button's text; defaulted in the BODY rather than in the parameter list. */
  createLabel?: string;
  /** Melts the bar into the PARENT's flex row — components/annotations.tsx's
   *  AnnotationBar documents the mechanism; the two are composed into one
   *  shared header row on Explore. `className` is ignored while inline. */
  inline?: boolean;
  className?: string;
}) {
  const t = useT(maintenanceDict);
  const { locale } = useLocale();
  const { can } = useAuth();
  const guard = useWriteGuard();
  const canRead = can("maintenance:read");
  const canWrite = can("maintenance:write");
  const [open, setOpen] = useState(false);
  const [createdNote, setCreatedNote] = useState<string>();
  const triggerRef = useRef<HTMLButtonElement>(null);

  /* The twin of components/annotations.tsx's reset (QA scope 3, finding #4):
     the note describes one scope and one window and must not outlive either. */
  const noteScope = `${scope}|${frozenWindow ? `${frozenWindow.from.getTime()}-${frozenWindow.to.getTime()}` : "live"}`;
  const [seenScope, setSeenScope] = useState(noteScope);
  if (seenScope !== noteScope) {
    setSeenScope(noteScope);
    setCreatedNote(undefined);
  }

  const handleChanged = () => {
    setCreatedNote(undefined);
    onChanged();
  };

  /* Focus comes back to the control that opened the form, the same contract
     AnnotationBar keeps (QA round 2, finding #20): the form is unmounted by
     then, and without this a keyboard user is dropped on <body>. */
  const closeAndRefocus = () => {
    setOpen(false);
    triggerRef.current?.focus();
  };

  if (!canRead) return null;

  return (
    <div data-testid="maintenance-bar" className={inline ? "contents" : cn("mt-3 flex flex-col gap-2", className)}>
      <div className={inline ? "contents" : "flex flex-wrap items-center gap-2"}>
        <span className="text-xs text-muted-foreground">
          {error
            ? t("bar.unavailable")
            : t(`bar.count.${countForm(locale, windows.length)}` as MaintenanceKey, {
                count: windows.length,
                scope: scopeCaption ?? scopeLabel(scope, t),
              })}
        </span>
        {/* HIDE on permission, DISABLE on time — never the other way round. */}
        {canWrite ? (
          <Button
            ref={triggerRef}
            type="button"
            size="sm"
            variant="outline"
            /* Inline, the push to the right edge is the PARENT row's spacer;
               order-1 only lines this button up after every count. */
            className={inline ? "order-1" : "ml-auto"}
            {...guard}
            aria-expanded={open}
            onClick={() => setOpen((o) => !o)}
          >
            {createLabel ?? t("bar.create")}
          </Button>
        ) : null}
      </div>

      {open ? (
        <div className={inline ? "order-2 basis-full" : undefined}>
          <CreateMaintenanceForm
            scope={scope}
            frozenWindow={frozenWindow}
            onDone={({ start, end }) => {
              closeAndRefocus();
              handleChanged();
              /* Silent for the ordinary in-window create — the row appearing IS the feedback. */
              setCreatedNote(outsideWindowNote(start, end, frozenWindow, t, locale) ?? undefined);
            }}
            onCancel={closeAndRefocus}
          />
        </div>
      ) : null}

      {createdNote ? (
        <p
          role="status"
          className={cn("text-[11px] leading-relaxed text-muted-foreground", inline && "order-2 basis-full")}
        >
          {createdNote}
        </p>
      ) : null}

      {/* NEWEST FIRST, the same rule the annotation bar follows: the merge sorts ascending for the
          chart's markAreas, the LIST is read from the top. */}
      {windows.length > 0 ? (
        <ul
          aria-label={t("bar.list.aria")}
          className={cn("m-0 divide-y divide-border/60 p-0", inline && "order-2 basis-full")}
        >
          {[...windows].reverse().map((w) => (
            <MaintenanceRow key={w.id} window={w} canWrite={canWrite} onChanged={handleChanged} />
          ))}
        </ul>
      ) : null}
    </div>
  );
}
