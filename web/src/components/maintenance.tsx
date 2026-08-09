import { useCallback, useMemo, useRef, useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { DateTimePicker } from "@/components/ui/datetime-picker";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { useAuth } from "@/hooks/use-auth";
import { useSubmitGuard } from "@/hooks/use-submit-guard";
import { ApiError, createMaintenance, deleteMaintenance, getMaintenance } from "@/lib/api";
import {
  GLOBAL_SCOPE,
  MAINTENANCE_REASON_MAX,
  mergeMaintenanceWindows,
  outsideWindowNote,
  type FrozenWindow,
} from "@/lib/annotations";
import { useTimeContext, useWriteGuard } from "@/lib/timemachine";
import type { MaintenanceWindow } from "@/lib/types";
import { cn } from "@/lib/utils";

/**
 * maintenance.tsx — the REACT half of M6 Task 9's maintenance windows, and a
 * deliberate TWIN of components/annotations.tsx: one hook and one bar, shared
 * by every surface, so "what window do we ask for", "which scopes does this
 * surface see" and "who may write here" are answered once rather than four
 * times. The pure half — the markArea builder and the list folding — lives in
 * lib/annotations.ts beside the annotation overlay it is modelled on.
 *
 * WHY A TWIN RATHER THAN ONE GENERIC COMPONENT. The two shapes look alike and
 * are not: an annotation may be an INSTANT (absent endAt) and a window is
 * always a SPAN with a strictly-later end (the store carries that as a CHECK);
 * an annotation carries `text` and a window carries `reason`; they ride
 * different permissions. A single parameterised component would spend its body
 * branching on those three facts and would still have to be read twice. Two
 * files that say the same thing about scope, and different things about time,
 * is the cheaper honesty.
 *
 * Maintenance windows are DATA AND RENDERING in M6, not suppression (plan
 * Decision 6): nothing evaluates alert rules until M7, so declaring a window
 * silences nothing. What it does is stop the console claiming a degradation was
 * a surprise when somebody had written down that they were upgrading a switch.
 */

const MAINTENANCE_POLL_MS = 60_000;

/** scopeLabel names a scope for a human — "" is GLOBAL, not "none". The same
 *  vocabulary annotations use, because it is the same vocabulary
 *  (plan Decision 6: the annotations scope convention, verbatim). */
export function scopeLabel(scope: string): string {
  return scope === GLOBAL_SCOPE ? "global" : scope;
}

export interface MaintenanceResult {
  windows: MaintenanceWindow[];
  isLoading: boolean;
  error: Error | null;
  /** Re-reads every maintenance query — what create and delete call. */
  refresh: () => Promise<void>;
}

/**
 * useMaintenance fetches the declared windows a surface should draw,
 * WINDOW-BOUNDED to exactly the range its chart shows.
 *
 * Everything useAnnotations documents about the window and the scopes holds
 * here verbatim, because it is the same endpoint contract:
 *
 *   scope === ""  → ONE request, `?scope=` (present-but-empty = global only)
 *   scope !== ""  → TWO requests, `?scope=<name>` and `?scope=`, merged
 *
 * and the key carries `at` (or the literal "live") rather than a Date computed
 * during render, so the query does not churn on every frame.
 *
 * The ONE difference from the twin: this hook is PERMISSION-GATED at the
 * request. M6's global constraint is "ZERO requests for sources the operator's
 * role cannot read" — a subject without maintenance:read must cost the API
 * nothing, not fetch a 403 and hide the result.
 *
 * The overlap semantics are the server's: GET /api/v1/maintenance returns
 * windows whose OWN SPAN overlaps [from,to), so a window that opened before the
 * chart's range and is still running is included. That is the one an operator
 * looking at this range most needs.
 */
export function useMaintenance(scope: string, rangeSeconds: number): MaintenanceResult {
  const { at } = useTimeContext();
  const { can } = useAuth();
  const qc = useQueryClient();
  const canRead = can("maintenance:read");
  const anchor = at ? at.toISOString() : "live";

  const windowFor = useCallback(() => {
    const to = at ?? new Date();
    return { from: new Date(to.getTime() - rangeSeconds * 1000), to };
  }, [at, rangeSeconds]);

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

/** fmtStamp is the FULL local stamp — the row's `title`, and its fallback for
 *  a timestamp that will not parse. */
function fmtStamp(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}

/**
 * fmtStampCompact is the row's VISIBLE time column, the same treatment
 * components/annotations.tsx got in QA round 2 and this row did not
 * (QA round 3, finding #11).
 *
 * A window renders BOTH edges, so it was carrying two full toLocaleStrings —
 * about 22rem of un-shrinkable text — in lists as narrow as the Investigate
 * page's 24rem column, and the reason (what the row is actually for) was left
 * with about 38px. Month-day-time is the shortest unambiguous form over the
 * span these lists cover, and the whole pair stays one hover away on `title`.
 */
function fmtStampCompact(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
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
 * CreateMaintenanceForm is the popover the create affordance opens.
 *
 * The scope is FIXED to the surface, shown and not editable — the same rule
 * CreateAnnotationForm documents: a window filed against an object you are not
 * looking at, from a form naming a different one, is invisible here and appears
 * somewhere you never visit.
 *
 * Both edges go through the M5 DateTimePicker rather than a raw
 * datetime-local, and the picker is opened with `allowFuture` because a
 * maintenance window is most often DECLARED IN ADVANCE — the Time Machine's own
 * past-clamp is exactly wrong for this one form.
 *
 * The end>start test below MIRRORS the store's CHECK. It is not a substitute
 * for it: the server still answers 422, and that answer still lands inline
 * under the fields. It exists so the common mistake costs a sentence instead of
 * a round trip.
 */
function CreateMaintenanceForm({
  scope,
  onDone,
  onCancel,
}: {
  scope: string;
  /** The stored edges, so the bar can say whether they land in the window it is
   *  showing (QA round 3, finding #8). */
  onDone: (created: { start: Date; end: Date }) => void;
  onCancel: () => void;
}) {
  const [start, setStart] = useState(() => floorToMinute(new Date()));
  const [end, setEnd] = useState(() => new Date(floorToMinute(new Date()).getTime() + DEFAULT_WINDOW_SECONDS * 1000));
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
      setError("A reason is required.");
      return;
    }
    if (end.getTime() <= start.getTime()) {
      setError("The end must be after the start.");
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
      setError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : "Failed to declare the window");
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
      <form role="form" aria-label="New maintenance window" onSubmit={handleSubmit} className="flex flex-col gap-3">
        <p className="text-xs text-muted-foreground">
          Scope <span className="font-medium text-foreground">{scopeLabel(scope)}</span> — fixed to this view.
        </p>
        <div className="flex flex-wrap items-start gap-3">
          <Field label="Start">
            <DateTimePicker aria-label="Start" value={start} onApply={setStart} allowFuture />
          </Field>
          <Field label="End" hint="Must be after the start — the server refuses anything else.">
            <DateTimePicker aria-label="End" value={end} onApply={setEnd} allowFuture />
          </Field>
        </div>
        <label className="flex flex-col gap-1 text-xs">
          <span className="font-medium text-muted-foreground">Reason</span>
          <textarea
            aria-label="Reason"
            value={reason}
            maxLength={MAINTENANCE_REASON_MAX}
            rows={2}
            onChange={(e) => setReason(e.target.value)}
            placeholder="Core switch firmware upgrade"
            className="rounded-md bg-surface-2 px-2 py-1.5 text-sm text-foreground placeholder:text-muted-foreground/70 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
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
            Create maintenance window
          </Button>
          <Button type="button" size="sm" variant="outline" onClick={onCancel}>
            Cancel
          </Button>
        </div>
      </form>
    </Card>
  );
}

/**
 * MaintenanceRow is one declared window and its delete.
 *
 * EXPORTED for pages/settings.tsx (QA round 3, finding #9), which lists the
 * windows this bar cannot reach — the bar is bounded to the chart's range, so
 * a window declared for next Tuesday existed with no surface anywhere. Sharing
 * the row rather than writing a second one keeps the confirm idiom, the
 * compact stamp and the write guard in ONE place: three things that must not
 * drift between two lists of the same rows.
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
  const guard = useWriteGuard();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  /* Second-click confirm, the same idiom the annotation rows and
     pages/alerting.tsx's rule rows use (QA round 2, finding #14). A declared
     window is somebody's record that a change was planned; deleting it on one
     mis-aimed click rewrites that record with no undo. */
  const [confirming, setConfirming] = useState(false);

  async function handleDelete() {
    setBusy(true);
    setError(undefined);
    try {
      await deleteMaintenance(w.id);
      onChanged();
    } catch (err) {
      setError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : "Failed to delete");
      setBusy(false);
      setConfirming(false);
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

          The fix is a column that FITS (w-44) and never truncates: whitespace-
          nowrap plus shrink-0, so flex takes the space out of the REASON
          instead, which already truncates and already carries its own title.
          Below 700px (max-lg here matches the app's own breakpoint use) the
          range takes a full row of its own above the reason rather than
          fighting it for one line — basis-full only at that width. */}
      <span
        data-testid="maintenance-stamp"
        className="nums w-full shrink-0 basis-full whitespace-nowrap text-muted-foreground lg:w-44 lg:basis-auto"
        title={`${fmtStamp(w.startAt)} → ${fmtStamp(w.endAt)}`}
      >
        {fmtStampCompact(w.startAt)} → {fmtStampCompact(w.endAt)}
      </span>
      <span data-testid="maintenance-reason" className="min-w-0 flex-1 truncate" title={w.reason}>
        {w.reason}
      </span>
      <span className="max-w-[7rem] shrink-0 truncate text-[11px] text-muted-foreground" title={scopeLabel(w.scope)}>
        {scopeLabel(w.scope)}
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
            <Button
              type="button"
              size="sm"
              variant="outline"
              loading={busy}
              {...guard}
              aria-label={`Confirm delete maintenance window: ${w.reason}`}
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
            aria-label={`Delete maintenance window: ${w.reason}`}
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
 * MaintenanceBar is the DOM half: the create affordance, and the list that
 * makes each declared window deletable.
 *
 * A list rather than a control inside the band's own tooltip, for the reason
 * AnnotationBar spells out: ECharts draws to a CANVAS, and a tooltip there is a
 * transient non-focusable overlay with no place in the tab order and no
 * presence in any test in this repo. The canvas keeps doing what canvas is good
 * at — showing WHERE, with the reason on hover.
 *
 * The whole bar is HIDDEN without maintenance:read. That is not cosmetics: the
 * hook makes no request either, so a subject who cannot read these sees no
 * empty shell claiming there are none.
 */
export function MaintenanceBar({
  scope,
  scopeCaption,
  windows,
  error,
  onChanged,
  frozenWindow,
  createLabel = "＋ maintenance",
  className,
}: {
  scope: string;
  /** What the count sentence CALLS the scope, when the surface queried every
   *  scope rather than this one (QA round 3, finding #7). Defaults to `scope`. */
  scopeCaption?: string;
  windows: MaintenanceWindow[];
  error?: Error | null;
  onChanged: () => void;
  /** The FROZEN range this bar is listing, when it has one (Investigate) —
   *  see lib/annotations.ts's outsideWindowNote. */
  frozenWindow?: FrozenWindow;
  /** The create button's text. Investigate's actions rail names it "Create
   *  maintenance" because that rail speaks in verbs; everywhere else the
   *  compact twin of "＋ annotate" is what sits under a chart. */
  createLabel?: string;
  className?: string;
}) {
  const { can } = useAuth();
  const guard = useWriteGuard();
  const canRead = can("maintenance:read");
  const canWrite = can("maintenance:write");
  const [open, setOpen] = useState(false);
  const [createdNote, setCreatedNote] = useState<string>();
  const triggerRef = useRef<HTMLButtonElement>(null);

  /* Focus comes back to the control that opened the form, the same contract
     AnnotationBar keeps (QA round 2, finding #20): the form is unmounted by
     then, and without this a keyboard user is dropped on <body>. */
  const closeAndRefocus = () => {
    setOpen(false);
    triggerRef.current?.focus();
  };

  if (!canRead) return null;

  return (
    <div data-testid="maintenance-bar" className={cn("mt-3 flex flex-col gap-2", className)}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs text-muted-foreground">
          {error
            ? "Maintenance windows are unavailable."
            : `${windows.length} maintenance window${windows.length === 1 ? "" : "s"} in this window · scope ${scopeCaption ?? scopeLabel(scope)}`}
        </span>
        {/* HIDE on permission, DISABLE on time — never the other way round. */}
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
            {createLabel}
          </Button>
        ) : null}
      </div>

      {open ? (
        <CreateMaintenanceForm
          scope={scope}
          onDone={({ start, end }) => {
            closeAndRefocus();
            onChanged();
            /* Silent for the ordinary in-window create — the row appearing IS
               the feedback (QA round 3, finding #8). */
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

      {windows.length > 0 ? (
        <ul aria-label="Maintenance windows in this window" className="m-0 divide-y divide-border/60 p-0">
          {windows.map((w) => (
            <MaintenanceRow key={w.id} window={w} canWrite={canWrite} onChanged={onChanged} />
          ))}
        </ul>
      ) : null}
    </div>
  );
}
