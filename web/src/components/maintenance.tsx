import { useCallback, useMemo, useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { DateTimePicker } from "@/components/ui/datetime-picker";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { useAuth } from "@/hooks/use-auth";
import { ApiError, createMaintenance, deleteMaintenance, getMaintenance } from "@/lib/api";
import { GLOBAL_SCOPE, MAINTENANCE_REASON_MAX, mergeMaintenanceWindows } from "@/lib/annotations";
import { useTimeContext, useWritesDisabled } from "@/lib/timemachine";
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

/** fmtStamp is the row's time column: date + minute, in the reader's own
 *  locale. */
function fmtStamp(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
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
function CreateMaintenanceForm({ scope, onDone, onCancel }: { scope: string; onDone: () => void; onCancel: () => void }) {
  const [start, setStart] = useState(() => floorToMinute(new Date()));
  const [end, setEnd] = useState(() => new Date(floorToMinute(new Date()).getTime() + DEFAULT_WINDOW_SECONDS * 1000));
  const [reason, setReason] = useState("");
  const [submitting, setSubmitting] = useState(false);
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
    setSubmitting(true);
    try {
      await createMaintenance({
        scope,
        startAt: start.toISOString(),
        endAt: end.toISOString(),
        reason: why,
      });
      onDone();
    } catch (err) {
      setError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : "Failed to declare the window");
      setSubmitting(false);
    }
  }

  return (
    <Card asChild className="p-4">
      <form role="dialog" aria-label="New maintenance window" onSubmit={handleSubmit} className="flex flex-col gap-3">
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

function MaintenanceRow({
  window: w,
  canWrite,
  onChanged,
}: {
  window: MaintenanceWindow;
  canWrite: boolean;
  onChanged: () => void;
}) {
  const writesDisabled = useWritesDisabled();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  async function handleDelete() {
    setBusy(true);
    setError(undefined);
    try {
      await deleteMaintenance(w.id);
      onChanged();
    } catch (err) {
      setError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : "Failed to delete");
      setBusy(false);
    }
  }

  return (
    <li data-testid="maintenance-item" className="flex flex-wrap items-center gap-2 py-1.5 text-xs">
      {/* Both edges, always. A window rendered by its start alone reads as an
          instant, which is the one thing it never is. */}
      <span className="nums shrink-0 text-muted-foreground">
        {fmtStamp(w.startAt)} → {fmtStamp(w.endAt)}
      </span>
      <span className="min-w-0 flex-1 truncate" title={w.reason}>
        {w.reason}
      </span>
      <span className="shrink-0 text-[11px] text-muted-foreground">{scopeLabel(w.scope)}</span>
      {error ? (
        <span role="alert" className="text-[11px] text-health-bad">
          {error}
        </span>
      ) : null}
      {/* Permission decides whether this EXISTS; time decides whether it is
          usable — lib/timemachine.tsx's useWritesDisabled documents the split. */}
      {canWrite ? (
        <Button
          type="button"
          size="sm"
          variant="ghost"
          loading={busy}
          disabled={writesDisabled}
          aria-label={`Delete maintenance window: ${w.reason}`}
          onClick={() => void handleDelete()}
        >
          Delete
        </Button>
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
  windows,
  error,
  onChanged,
  createLabel = "＋ maintenance",
  className,
}: {
  scope: string;
  windows: MaintenanceWindow[];
  error?: Error | null;
  onChanged: () => void;
  /** The create button's text. Investigate's actions rail names it "Create
   *  maintenance" because that rail speaks in verbs; everywhere else the
   *  compact twin of "＋ annotate" is what sits under a chart. */
  createLabel?: string;
  className?: string;
}) {
  const { can } = useAuth();
  const writesDisabled = useWritesDisabled();
  const canRead = can("maintenance:read");
  const canWrite = can("maintenance:write");
  const [open, setOpen] = useState(false);

  if (!canRead) return null;

  return (
    <div data-testid="maintenance-bar" className={cn("mt-3 flex flex-col gap-2", className)}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs text-muted-foreground">
          {error
            ? "Maintenance windows are unavailable."
            : `${windows.length} maintenance window${windows.length === 1 ? "" : "s"} in this window · scope ${scopeLabel(scope)}`}
        </span>
        {/* HIDE on permission, DISABLE on time — never the other way round. */}
        {canWrite ? (
          <Button
            type="button"
            size="sm"
            variant="outline"
            className="ml-auto"
            disabled={writesDisabled}
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
          onDone={() => {
            setOpen(false);
            onChanged();
          }}
          onCancel={() => setOpen(false)}
        />
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
