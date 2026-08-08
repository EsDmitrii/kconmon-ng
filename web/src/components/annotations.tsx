import { useCallback, useMemo, useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { useAuth } from "@/hooks/use-auth";
import { ApiError, createAnnotation, deleteAnnotation, listAnnotations } from "@/lib/api";
import { ANNOTATION_TEXT_MAX, GLOBAL_SCOPE, mergeAnnotations } from "@/lib/annotations";
import { useTimeContext, useWritesDisabled } from "@/lib/timemachine";
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

/** toLocalInputValue renders an instant for <input type="datetime-local">,
 *  which speaks LOCAL wall-clock time with no zone at all. Building it by hand
 *  rather than slicing toISOString(), which would silently show UTC and file
 *  every mark at the wrong hour outside UTC. */
export function toLocalInputValue(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** fmtStamp is the marker list's time column: date + minute, local. */
function fmtStamp(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
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

const INPUT_CLASS =
  "h-8 rounded-md bg-surface-2 px-2 text-sm text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring";

/**
 * CreateAnnotationForm is the popover the "＋ annotate" button opens.
 *
 * The scope is FIXED to the surface, shown and not editable. An editable scope
 * would let an operator file a note against an object they are not looking at,
 * from a form that names a different one — the mark would then be invisible
 * here and appear somewhere they never visit. A note belongs to the thing you
 * were reading when you wrote it.
 *
 * The start defaults to NOW rather than to a clicked point on the chart:
 * ECharts click plumbing is not wired in this milestone (see the task report),
 * and a default an operator can see and correct in a plain datetime field beats
 * one derived from a pixel.
 */
function CreateAnnotationForm({ scope, onDone, onCancel }: { scope: string; onDone: () => void; onCancel: () => void }) {
  const [start, setStart] = useState(() => toLocalInputValue(new Date()));
  const [end, setEnd] = useState("");
  const [text, setText] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string>();

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    const note = text.trim();
    if (note === "") {
      setError("A note is required.");
      return;
    }
    const startAt = new Date(start);
    if (Number.isNaN(startAt.getTime())) {
      setError("Start is not a valid time.");
      return;
    }
    const endAt = end === "" ? undefined : new Date(end);
    if (endAt && Number.isNaN(endAt.getTime())) {
      setError("End is not a valid time.");
      return;
    }
    setError(undefined);
    setSubmitting(true);
    try {
      // endAt is OMITTED, never sent empty: its absence is what makes this an
      // instant mark rather than a zero-length span (lib/annotations.ts's
      // isInstant reads exactly this).
      await createAnnotation({
        startAt: startAt.toISOString(),
        ...(endAt ? { endAt: endAt.toISOString() } : {}),
        scope,
        text: note,
      });
      onDone();
    } catch (err) {
      setError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : "Failed to create the annotation");
      setSubmitting(false);
    }
  }

  return (
    <Card asChild className="p-4">
      <form role="dialog" aria-label="New annotation" onSubmit={handleSubmit} className="flex flex-col gap-3">
        <p className="text-xs text-muted-foreground">
          Scope <span className="font-medium text-foreground">{scopeLabel(scope)}</span> — fixed to this view.
        </p>
        <div className="grid gap-3 sm:grid-cols-2">
          <Field label="Start">
            <input
              type="datetime-local"
              aria-label="Start"
              value={start}
              onChange={(e) => setStart(e.target.value)}
              className={INPUT_CLASS}
            />
          </Field>
          <Field label="End (optional)" hint="Leave empty for a mark at a single moment.">
            <input
              type="datetime-local"
              aria-label="End"
              value={end}
              onChange={(e) => setEnd(e.target.value)}
              className={INPUT_CLASS}
            />
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
  const writesDisabled = useWritesDisabled();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  async function handleDelete() {
    setBusy(true);
    setError(undefined);
    try {
      await deleteAnnotation(annotation.id);
      onChanged();
    } catch (err) {
      setError(err instanceof ApiError ? (err.problem.detail || err.problem.title) : "Failed to delete");
      setBusy(false);
    }
  }

  return (
    <li data-testid="annotation-item" className="flex flex-wrap items-center gap-2 py-1.5 text-xs">
      <span className="nums w-40 shrink-0 text-muted-foreground">{fmtStamp(annotation.startAt)}</span>
      <span className="min-w-0 flex-1 truncate" title={annotation.text}>
        {annotation.text}
      </span>
      <span className="shrink-0 text-[11px] text-muted-foreground">{scopeLabel(annotation.scope)}</span>
      {error ? (
        <span role="alert" className="text-[11px] text-health-bad">
          {error}
        </span>
      ) : null}
      {/* Permission decides whether this EXISTS; time decides whether it is
          usable — lib/timemachine.tsx's useWritesDisabled documents the split,
          and this is the composition it prescribes. */}
      {canWrite ? (
        <Button
          type="button"
          size="sm"
          variant="ghost"
          loading={busy}
          disabled={writesDisabled}
          aria-label={`Delete annotation: ${annotation.text}`}
          onClick={() => void handleDelete()}
        >
          Delete
        </Button>
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
  annotations,
  error,
  onChanged,
  className,
}: {
  scope: string;
  annotations: Annotation[];
  error?: Error | null;
  onChanged: () => void;
  className?: string;
}) {
  const { can } = useAuth();
  const writesDisabled = useWritesDisabled();
  const canWrite = can("annotations:write");
  const [open, setOpen] = useState(false);

  return (
    <div data-testid="annotation-bar" className={cn("mt-3 flex flex-col gap-2", className)}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs text-muted-foreground">
          {error
            ? "Annotations are unavailable."
            : `${annotations.length} annotation${annotations.length === 1 ? "" : "s"} in this window · scope ${scopeLabel(scope)}`}
        </span>
        {/* HIDE on permission, DISABLE on time. Never the other way round:
            hiding it while engaged would read as "you lost the permission". */}
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
            ＋ annotate
          </Button>
        ) : null}
      </div>

      {open ? (
        <CreateAnnotationForm
          scope={scope}
          onDone={() => {
            setOpen(false);
            onChanged();
          }}
          onCancel={() => setOpen(false)}
        />
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
