import { useEffect, useId, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { PageShell } from "@/components/page-shell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { useAuth } from "@/hooks/use-auth";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import {
  ApiError,
  checksProjection,
  createCheck,
  createSchedule,
  createTarget,
  deleteCheck,
  deleteSchedule,
  deleteTarget,
  listChecks,
  listSchedules,
  listTargets,
  updateCheck,
  updateSchedule,
  updateTarget,
} from "@/lib/api";
// Read directly in each mutating component rather than threaded down from the
// page as a prop: it is a context read, it costs nothing, and every affordance
// that needs it then states its own dependency instead of inheriting one four
// levels up. See lib/timemachine.tsx for the hide-vs-disable rule these all
// follow — `canWrite` decides whether a control EXISTS, this decides whether it
// is usable right now.
import { useWritesDisabled } from "@/lib/timemachine";
import {
  CHECK_TYPES,
  type CheckDefinition,
  type CheckDefinitionRequest,
  type CheckType,
  type DestinationKind,
  type Projection,
  type Schedule,
  type ScheduleKind,
  type ScheduleRequest,
  type SourceSelection,
  type Target,
  type TargetKind,
  type TargetRequest,
} from "@/lib/types";
import { cn } from "@/lib/utils";

const TABS = [
  { value: "targets", label: "Targets" },
  { value: "definitions", label: "Definitions" },
  { value: "schedules", label: "Schedules" },
] as const;
type Tab = (typeof TABS)[number]["value"];

const TARGET_KINDS: TargetKind[] = ["host", "url"];
const SOURCE_SELECTIONS: SourceSelection[] = ["all", "per-zone", "one-per-zone"];
const DESTINATION_KINDS: DestinationKind[] = ["node", "target", "adhoc"];

/**
 * PROJECTION_DEBOUNCE_MS keeps "project on change" from meaning "one POST per
 * keystroke". Deliberately well under vitest's 1s waitFor budget so the tests
 * exercise the real timer rather than a fake-clock double.
 */
export const PROJECTION_DEBOUNCE_MS = 250;

/* ── 422 → form field ───────────────────────────────────────────────────────
   A rejected write comes back as RFC 7807 problem+json with exactly four
   members (docs/console-api.yaml's Problem schema) — there is NO field
   pointer. So the field a `detail` belongs to is recovered from the field
   NOUN the server's own message leads with: store.TargetInput.Validate and
   store.DefinitionInput.Validate build every message as "<resource>: <field>
   ..." (internal/console/store/targets.go), and the duplicate-name /
   unknown-target 422s httpapi writes by hand keep the same shape.

   This is a PRESENTATION heuristic, not a contract. A detail that matches no
   phrase is still shown, verbatim, as a form-level error — the server's words
   are never dropped on the floor just because this table did not recognise
   them. Adding a real `errors[]` member to Problem would let this go away;
   until then, the failure mode is "the message renders one level up", not
   "the message disappears". */

export type TargetField = "name" | "kind" | "address" | "labels";
export type DefinitionField =
  | "name"
  | "sourceSelection"
  | "destinationKind"
  | "destinationTargetId"
  | "destinationAddress"
  | "checkType"
  | "plane"
  | "params";

/** Most specific phrase FIRST: "destination address" must win over the bare
 *  "address" that a target detail uses, and "destination target id" must win
 *  over "destination kind"'s shared prefix. */
export const TARGET_FIELD_PHRASES: readonly (readonly [TargetField, string])[] = [
  ["labels", "labels"],
  ["address", "address"],
  ["kind", "kind"],
  ["name", "name"],
];

export type ScheduleField = "definitionId" | "kind" | "intervalNs" | "runAt";

/** Same most-specific-first rule as the two tables below/above. The one
 *  ordering subtlety: store's enum message ("kind %q must be one of once,
 *  interval, continuous") CONTAINS the word "interval", so it would land on
 *  intervalNs rather than kind — harmless in practice, because the kind
 *  control is a three-option select that cannot produce an invalid kind at
 *  all, and a message that lands one field over still renders in full. */
export const SCHEDULE_FIELD_PHRASES: readonly (readonly [ScheduleField, string])[] = [
  ["definitionId", "definition id"],
  ["runAt", "run at"],
  ["intervalNs", "interval"],
  ["kind", "kind"],
];

export const DEFINITION_FIELD_PHRASES: readonly (readonly [DefinitionField, string])[] = [
  ["destinationTargetId", "destination target id"],
  ["destinationAddress", "destination address"],
  ["destinationKind", "destination kind"],
  ["sourceSelection", "source selection"],
  ["checkType", "check type"],
  ["params", "params"],
  ["plane", "plane"],
  ["name", "name"],
];

/** fieldForDetail returns the first field whose phrase appears in `detail`,
 *  or null when the message names no field this form has. */
export function fieldForDetail<F extends string>(
  detail: string,
  phrases: readonly (readonly [F, string])[],
): F | null {
  const haystack = detail.toLowerCase();
  for (const [field, phrase] of phrases) {
    if (haystack.includes(phrase)) return field;
  }
  return null;
}

/** "form" is the catch-all slot: a problem this table cannot place still
 *  renders, above the submit button, exactly where diagnostics.tsx's RunForm
 *  already puts a submit error. */
type FieldErrors<F extends string> = Partial<Record<F | "form", string>>;

function errorsFromProblem<F extends string>(
  err: unknown,
  phrases: readonly (readonly [F, string])[],
  fallback: string,
): FieldErrors<F> {
  // The casts are unavoidable with a generic key: TS cannot prove a computed
  // key of type F indexes Partial<Record<F | "form", string>>.
  if (!(err instanceof ApiError)) return { form: fallback } as FieldErrors<F>;
  const detail = err.problem.detail ?? err.problem.title;
  const field = fieldForDetail(detail, phrases);
  return (field ? { [field]: detail } : { form: detail }) as FieldErrors<F>;
}

/* ── labels ─────────────────────────────────────────────────────────────── */

/**
 * parseLabels reads the "k=v, k=v" text field into the object the API takes.
 * It THROWS on a malformed pair rather than dropping it: a label silently
 * discarded on the way to a FULL-replace PUT is a label deleted from the
 * stored target, which is the one outcome an operator would never guess from
 * the UI.
 */
export function parseLabels(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const raw of text.split(",")) {
    const part = raw.trim();
    if (part === "") continue;
    const eq = part.indexOf("=");
    if (eq <= 0) throw new Error(`labels must be "key=value" pairs separated by commas; got ${JSON.stringify(part)}`);
    out[part.slice(0, eq).trim()] = part.slice(eq + 1).trim();
  }
  return out;
}

export function formatLabels(labels: Record<string, string> | undefined): string {
  if (!labels) return "";
  return Object.entries(labels)
    .map(([k, v]) => `${k}=${v}`)
    .join(", ");
}

/* ── small form primitives ──────────────────────────────────────────────── */

function fieldClasses(invalid: boolean): string {
  return cn(
    "h-9 rounded-md border bg-transparent px-3 text-[13px]",
    invalid ? "border-health-bad" : "border-border-strong",
  );
}

function TextField({
  label,
  value,
  onChange,
  error,
  placeholder,
  textarea,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  error?: string;
  placeholder?: string;
  textarea?: boolean;
}) {
  const id = useId();
  const errorId = `${id}-error`;
  const shared = {
    id,
    value,
    placeholder,
    "aria-invalid": error ? (true as const) : undefined,
    "aria-describedby": error ? errorId : undefined,
  };
  return (
    <div className="flex flex-col gap-1 text-[13px]">
      <label htmlFor={id} className="text-muted-foreground">
        {label}
      </label>
      {textarea ? (
        <textarea
          {...shared}
          rows={3}
          onChange={(e) => onChange(e.target.value)}
          className={cn(fieldClasses(!!error), "h-auto py-2 font-mono")}
        />
      ) : (
        <input {...shared} onChange={(e) => onChange(e.target.value)} className={fieldClasses(!!error)} />
      )}
      {error ? (
        <span id={errorId} role="alert" className="text-xs leading-relaxed text-health-bad">
          {error}
        </span>
      ) : null}
    </div>
  );
}

function SelectField<T extends string>({
  label,
  value,
  onChange,
  options,
  error,
  disabled,
}: {
  label: string;
  value: T;
  onChange: (v: T) => void;
  options: readonly { value: T; label: string }[];
  error?: string;
  disabled?: boolean;
}) {
  const id = useId();
  const errorId = `${id}-error`;
  return (
    <div className="flex flex-col gap-1 text-[13px]">
      <label htmlFor={id} className="text-muted-foreground">
        {label}
      </label>
      <select
        id={id}
        value={value}
        disabled={disabled}
        aria-invalid={error ? true : undefined}
        aria-describedby={error ? errorId : undefined}
        onChange={(e) => onChange(e.target.value as T)}
        className={cn(fieldClasses(!!error), "disabled:opacity-70")}
      >
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
      {error ? (
        <span id={errorId} role="alert" className="text-xs leading-relaxed text-health-bad">
          {error}
        </span>
      ) : null}
    </div>
  );
}

function plainOptions<T extends string>(values: readonly T[]): { value: T; label: string }[] {
  return values.map((v) => ({ value: v, label: v }));
}

/** PermissionCard is PAGES.md:126-129's pattern: name the permission, say what
 *  the reader CAN still do, and never render a disabled control in place of
 *  one they simply do not have. */
function PermissionCard({ permission, children }: { permission: string; children: ReactNode }) {
  return (
    <Card role="status" className="p-6">
      <p className="text-sm font-medium">Requires the {permission} permission</p>
      <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{children}</p>
    </Card>
  );
}

function EmptyRow({ children }: { children: ReactNode }) {
  return <p className="px-1 py-10 text-center text-xs text-muted-foreground">{children}</p>;
}

function ListSkeleton() {
  return (
    <div role="status" aria-live="polite" className="mt-4 flex flex-col gap-2">
      <span className="sr-only">Loading…</span>
      {Array.from({ length: 3 }, (_, i) => (
        <Skeleton key={i} className="h-10 w-full" />
      ))}
    </div>
  );
}

function queryErrorMessage(error: unknown, fallback: string): string {
  return error instanceof ApiError ? (error.problem.detail ?? error.problem.title) : fallback;
}

function fmtTime(timestamp?: string | null): string {
  if (!timestamp) return "—";
  const d = new Date(timestamp);
  return Number.isNaN(d.getTime()) ? timestamp : d.toLocaleString();
}

/* ── Targets tab ────────────────────────────────────────────────────────── */

function TargetForm({ initial, onDone }: { initial?: Target; onDone: () => void }) {
  const qc = useQueryClient();
  const writesDisabled = useWritesDisabled();
  const [name, setName] = useState(initial?.name ?? "");
  const [kind, setKind] = useState<TargetKind>(initial?.kind ?? "host");
  const [address, setAddress] = useState(initial?.address ?? "");
  const [labels, setLabels] = useState(formatLabels(initial?.labels));
  const [submitting, setSubmitting] = useState(false);
  const [errors, setErrors] = useState<FieldErrors<TargetField>>({});

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setErrors({});
    let parsedLabels: Record<string, string>;
    try {
      parsedLabels = parseLabels(labels);
    } catch (err) {
      setErrors({ labels: err instanceof Error ? err.message : "labels are malformed" });
      return;
    }
    // A full replace, so every field goes on the wire — an omitted one means
    // EMPTY server-side (PUT /api/v1/targets/{id}), never "leave as-is".
    const req: TargetRequest = { name, kind, address, labels: parsedLabels };
    setSubmitting(true);
    try {
      if (initial) await updateTarget(initial.id, req);
      else await createTarget(req);
      await qc.invalidateQueries({ queryKey: ["targets"] });
      onDone();
    } catch (err) {
      setErrors(errorsFromProblem(err, TARGET_FIELD_PHRASES, "Failed to save the target"));
      setSubmitting(false);
    }
  }

  return (
    <Card asChild className="p-6">
      <form onSubmit={handleSubmit} className="flex flex-col gap-4">
        <h3 className="text-sm font-semibold">{initial ? `Edit ${initial.name}` : "New target"}</h3>
        <div className="grid gap-4 sm:grid-cols-2">
          <TextField label="Name" value={name} onChange={setName} error={errors.name} placeholder="edge-gateway" />
          <SelectField
            label="Kind"
            value={kind}
            onChange={setKind}
            options={plainOptions(TARGET_KINDS)}
            error={errors.kind}
          />
          <TextField
            label="Address"
            value={address}
            onChange={setAddress}
            error={errors.address}
            placeholder="10.0.0.1 or https://example.test/health"
          />
          <TextField
            label="Labels"
            value={labels}
            onChange={setLabels}
            error={errors.labels}
            placeholder="env=prod, tier=edge"
          />
        </div>
        {errors.form ? (
          <p role="alert" className="text-sm text-health-bad">
            {errors.form}
          </p>
        ) : null}
        <div className="flex gap-2">
          {/* Only the WRITE is disabled. Cancel closes a form and touches
              nothing, so it stays live — a modal an operator cannot dismiss
              would be the mode holding the page hostage. */}
          <Button type="submit" loading={submitting} disabled={writesDisabled}>
            {initial ? "Save target" : "Create target"}
          </Button>
          <Button type="button" variant="outline" onClick={onDone}>
            Cancel
          </Button>
        </div>
      </form>
    </Card>
  );
}

function TargetRowActions({ target, onEdit }: { target: Target; onEdit: () => void }) {
  const qc = useQueryClient();
  const writesDisabled = useWritesDisabled();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  async function handleDelete() {
    setBusy(true);
    setError(undefined);
    try {
      await deleteTarget(target.id);
      await qc.invalidateQueries({ queryKey: ["targets"] });
    } catch (err) {
      // A 409 (the target is still referenced by a definition) lands here and
      // is worth every word the server wrote: it names the query that lists
      // the offending definitions.
      setError(queryErrorMessage(err, "Failed to delete the target"));
      setBusy(false);
      setConfirming(false);
    }
  }

  if (confirming) {
    return (
      <span className="flex items-center gap-2">
        <Button size="sm" variant="outline" loading={busy} disabled={writesDisabled} onClick={handleDelete}>
          Confirm delete {target.name}
        </Button>
        <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>
          Cancel
        </Button>
      </span>
    );
  }
  return (
    <span className="flex items-center gap-2">
      {/* Edit opens a form whose only purpose is to submit a PUT, so it is
          disabled with the write it leads to rather than left to dead-end at a
          greyed Save. */}
      <Button size="sm" variant="ghost" disabled={writesDisabled} onClick={onEdit}>
        Edit {target.name}
      </Button>
      <Button size="sm" variant="ghost" disabled={writesDisabled} onClick={() => setConfirming(true)}>
        Delete {target.name}
      </Button>
      {error ? (
        <span role="alert" className="text-xs text-health-bad">
          {error}
        </span>
      ) : null}
    </span>
  );
}

function TargetsTab({ canWrite }: { canWrite: boolean }) {
  const writesDisabled = useWritesDisabled();
  const [editing, setEditing] = useState<{ mode: "none" } | { mode: "create" } | { mode: "edit"; target: Target }>({
    mode: "none",
  });
  const query = useQuery({ queryKey: ["targets"], queryFn: () => listTargets() });
  const targets = query.data?.targets ?? [];

  return (
    <div className="flex flex-col gap-4">
      {canWrite ? null : (
        <PermissionCard permission="targets:write">
          The list below is complete and current — creating, editing and deleting targets is what needs the extra
          permission. Ask an operator to change the fleet's probe configuration.
        </PermissionCard>
      )}

      {canWrite && editing.mode === "none" ? (
        <div>
          <Button size="sm" disabled={writesDisabled} onClick={() => setEditing({ mode: "create" })}>
            New target
          </Button>
        </div>
      ) : null}
      {canWrite && editing.mode !== "none" ? (
        <TargetForm
          key={editing.mode === "edit" ? editing.target.id : "create"}
          initial={editing.mode === "edit" ? editing.target : undefined}
          onDone={() => setEditing({ mode: "none" })}
        />
      ) : null}

      <Card asChild className="p-6">
        <section>
          <h2 className="text-sm font-semibold">Targets</h2>
          {query.isError ? (
            <p role="alert" className="mt-3 text-sm text-health-bad">
              {queryErrorMessage(query.error, "Targets are unavailable")}
            </p>
          ) : null}
          {query.isLoading ? <ListSkeleton /> : null}
          {!query.isLoading && targets.length === 0 && !query.isError ? (
            <EmptyRow>No targets yet. External checks probe what is listed here.</EmptyRow>
          ) : null}
          {targets.length > 0 ? (
            <ul aria-label="Targets" className="mt-4 divide-y divide-border">
              {targets.map((t) => (
                <li key={t.id} className="flex flex-wrap items-center gap-3 py-3 text-sm">
                  {/* The row's name is the way into the Target card
                      (pages/target-card.tsx). A plain <a href>, not a router
                      <Link>: the card reads its id off
                      window.location.pathname, so a full navigation and a
                      bookmarked cold load take exactly the same code path —
                      the same choice pages/diagnostics.tsx already makes for a
                      run permalink. */}
                  <a
                    href={`/targets/${encodeURIComponent(t.id)}`}
                    className="font-medium text-primary hover:underline"
                  >
                    {t.name}
                  </a>
                  <Badge variant="neutral">{t.kind}</Badge>
                  <span className="truncate text-xs text-muted-foreground">{t.address}</span>
                  {Object.keys(t.labels).length > 0 ? (
                    <span className="truncate text-xs text-muted-foreground">{formatLabels(t.labels)}</span>
                  ) : null}
                  {canWrite ? (
                    <span className="ml-auto">
                      <TargetRowActions target={t} onEdit={() => setEditing({ mode: "edit", target: t })} />
                    </span>
                  ) : null}
                </li>
              ))}
            </ul>
          ) : null}
        </section>
      </Card>
    </div>
  );
}

/* ── Definitions tab ────────────────────────────────────────────────────── */

/** projectionWarning words the over-limit case out of the server's OWN
 *  numbers — the limit included. Nothing here recomputes `overLimit`: the
 *  boolean that gates submit is the one the response carried, so the warning
 *  and the enforcement cannot disagree. See DefinitionForm's projection
 *  comment for why that matters. */
function projectionWarning(p: Projection): string {
  return (
    `~${p.series} series (${p.agents} agents × ${p.protocols} protocol${p.protocols === 1 ? "" : "s"}) — ` +
    `above the ${p.limit}-series limit; narrow sourceSelection to one-per-zone, or save this definition disabled`
  );
}

function tryParseParams(text: string): { ok: true; value: Record<string, unknown> | undefined } | { ok: false; message: string } {
  const trimmed = text.trim();
  if (trimmed === "") return { ok: true, value: undefined };
  try {
    const parsed: unknown = JSON.parse(trimmed);
    if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
      return { ok: false, message: "params must be a JSON object" };
    }
    return { ok: true, value: parsed as Record<string, unknown> };
  } catch {
    return { ok: false, message: "params must be valid JSON" };
  }
}

function DefinitionForm({
  initial,
  targets,
  onDone,
}: {
  initial?: CheckDefinition;
  targets: Target[];
  onDone: () => void;
}) {
  const qc = useQueryClient();
  const writesDisabled = useWritesDisabled();
  const [name, setName] = useState(initial?.name ?? "");
  const [checkType, setCheckType] = useState<CheckType>(initial?.checkType ?? "tcp");
  const [sourceSelection, setSourceSelection] = useState<SourceSelection>(initial?.sourceSelection ?? "one-per-zone");
  const [destinationKind, setDestinationKind] = useState<DestinationKind>(initial?.destinationKind ?? "node");
  const [destinationTargetId, setDestinationTargetId] = useState(initial?.destinationTargetId ?? "");
  const [destinationAddress, setDestinationAddress] = useState(initial?.destinationAddress ?? "");
  // Defaults to enabled: a definition saved disabled is the deliberate escape
  // hatch from the projection guard, not the ordinary case. The field is
  // always sent explicitly, because an OMITTED "enabled" decodes to false
  // server-side (httpapi's definitionRequest is a plain Go bool).
  const [enabled, setEnabled] = useState(initial?.enabled ?? true);
  const [paramsText, setParamsText] = useState(
    initial && Object.keys(initial.params).length > 0 ? JSON.stringify(initial.params, null, 2) : "",
  );
  const [submitting, setSubmitting] = useState(false);
  const [errors, setErrors] = useState<FieldErrors<DefinitionField>>({});
  const [projection, setProjection] = useState<Projection>();

  const params = useMemo(() => tryParseParams(paramsText), [paramsText]);

  // The exact body the submit is about to send, so the projection can never
  // be computed from a different draft than the one being saved.
  const draft = useMemo<CheckDefinitionRequest>(
    () => ({
      name,
      sourceSelection,
      destinationKind,
      destinationTargetId: destinationKind === "target" ? destinationTargetId : undefined,
      destinationAddress: destinationKind === "adhoc" ? destinationAddress : undefined,
      checkType,
      // The only plane that exists (PAGES.md:108); the field is required by
      // the API, so it is sent rather than omitted.
      plane: "pod",
      params: params.ok ? params.value : undefined,
      enabled,
    }),
    [name, sourceSelection, destinationKind, destinationTargetId, destinationAddress, checkType, params, enabled],
  );

  /* The client mirror of Decision 12. POST /api/v1/checks/projection runs the
     very same projectDefinition against the very same limit that create and
     update run through enforceProjection (internal/console/httpapi/
     definitions.go), so unlike diagnostics.tsx's hand-copied MAX_PAIRS echo
     this preview cannot arithmetically disagree with enforcement.

     It is still only a preview, and THE SERVER IS THE ARBITER: the number is
     computed against the topology as of the moment it is asked, so a cluster
     that scales up between this answer and the submit turns an "under the
     limit" preview into a real 422 — which is exactly why the submit path
     below renders 422 details instead of trusting `overLimit` to have already
     caught everything.

     No checks:write guard is needed HERE only because this whole form is
     behind one: DefinitionsTab renders it exclusively for a writer, and the
     projection endpoint is gated on checks:write, not checks:read
     (middleware_auth.go), so a reader must never reach this code at all.

     Skipped while the draft has no name: the endpoint validates the body
     before projecting, so a nameless draft is a guaranteed 422 with nothing
     to show. */
  useEffect(() => {
    if (draft.name === "") {
      setProjection(undefined);
      return;
    }
    let cancelled = false;
    const timer = setTimeout(() => {
      checksProjection(draft)
        .then((p) => {
          if (!cancelled) setProjection(p);
        })
        .catch(() => {
          // Advisory only: a draft the server will not even validate has no
          // meaningful number to show, and the write is re-checked anyway.
          if (!cancelled) setProjection(undefined);
        });
    }, PROJECTION_DEBOUNCE_MS);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [draft]);

  // Mirrors enforceProjection exactly: the guard runs ONLY for a definition
  // arriving enabled, so a draft saved disabled must stay submittable no
  // matter how large it would project.
  const blocked = enabled && projection?.overLimit === true;

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setErrors({});
    if (!params.ok) {
      setErrors({ params: params.message });
      return;
    }
    setSubmitting(true);
    try {
      if (initial) await updateCheck(initial.id, draft);
      else await createCheck(draft);
      await qc.invalidateQueries({ queryKey: ["definitions"] });
      onDone();
    } catch (err) {
      setErrors(errorsFromProblem(err, DEFINITION_FIELD_PHRASES, "Failed to save the definition"));
      setSubmitting(false);
    }
  }

  return (
    <Card asChild className="p-6">
      <form onSubmit={handleSubmit} className="flex flex-col gap-4">
        <h3 className="text-sm font-semibold">{initial ? `Edit ${initial.name}` : "New definition"}</h3>
        <div className="grid gap-4 sm:grid-cols-2">
          <TextField label="Name" value={name} onChange={setName} error={errors.name} placeholder="edge-gateway-tcp" />
          <SelectField
            label="Check type"
            value={checkType}
            onChange={setCheckType}
            options={plainOptions(CHECK_TYPES)}
            error={errors.checkType}
          />
          <SelectField
            label="Source selection"
            value={sourceSelection}
            onChange={setSourceSelection}
            options={plainOptions(SOURCE_SELECTIONS)}
            error={errors.sourceSelection}
          />
          <SelectField
            label="Destination kind"
            value={destinationKind}
            onChange={setDestinationKind}
            options={plainOptions(DESTINATION_KINDS)}
            error={errors.destinationKind}
          />
          {destinationKind === "target" ? (
            <SelectField
              label="Destination target"
              value={destinationTargetId}
              onChange={setDestinationTargetId}
              options={[{ value: "", label: "— pick a target —" }, ...targets.map((t) => ({ value: t.id, label: t.name }))]}
              error={errors.destinationTargetId}
            />
          ) : null}
          {destinationKind === "adhoc" ? (
            <TextField
              label="Destination address"
              value={destinationAddress}
              onChange={setDestinationAddress}
              error={errors.destinationAddress}
              placeholder="10.0.0.1"
            />
          ) : null}
          <SelectField
            label="Plane"
            value="pod"
            onChange={() => {}}
            disabled
            options={[{ value: "pod", label: "pod" }]}
            error={errors.plane}
          />
        </div>

        <TextField
          label="Params (JSON)"
          value={paramsText}
          onChange={setParamsText}
          error={errors.params}
          placeholder={'{"port": 443}'}
          textarea
        />

        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
            className="size-4 rounded border-border-strong"
          />
          Enabled
        </label>

        {projection ? (
          <p
            role="status"
            className={cn("nums text-sm", projection.overLimit ? "text-health-bad" : "text-muted-foreground")}
          >
            {projection.overLimit
              ? projectionWarning(projection)
              : `~${projection.series} series (${projection.agents} agents × ${projection.protocols} protocol${
                  projection.protocols === 1 ? "" : "s"
                }), limit ${projection.limit}`}
          </p>
        ) : null}

        {errors.form ? (
          <p role="alert" className="text-sm text-health-bad">
            {errors.form}
          </p>
        ) : null}

        <div className="flex gap-2">
          <Button type="submit" loading={submitting} disabled={blocked || writesDisabled}>
            {initial ? "Save definition" : "Create definition"}
          </Button>
          <Button type="button" variant="outline" onClick={onDone}>
            Cancel
          </Button>
        </div>
      </form>
    </Card>
  );
}

function DefinitionRowActions({ definition, onEdit }: { definition: CheckDefinition; onEdit: () => void }) {
  const qc = useQueryClient();
  const writesDisabled = useWritesDisabled();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  async function handleDelete() {
    setBusy(true);
    setError(undefined);
    try {
      await deleteCheck(definition.id);
      await qc.invalidateQueries({ queryKey: ["definitions"] });
    } catch (err) {
      setError(queryErrorMessage(err, "Failed to delete the definition"));
      setBusy(false);
      setConfirming(false);
    }
  }

  if (confirming) {
    return (
      <span className="flex items-center gap-2">
        <Button size="sm" variant="outline" loading={busy} disabled={writesDisabled} onClick={handleDelete}>
          Confirm delete {definition.name}
        </Button>
        <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>
          Cancel
        </Button>
      </span>
    );
  }
  return (
    <span className="flex items-center gap-2">
      <Button size="sm" variant="ghost" disabled={writesDisabled} onClick={onEdit}>
        Edit {definition.name}
      </Button>
      <Button size="sm" variant="ghost" disabled={writesDisabled} onClick={() => setConfirming(true)}>
        Delete {definition.name}
      </Button>
      {error ? (
        <span role="alert" className="text-xs text-health-bad">
          {error}
        </span>
      ) : null}
    </span>
  );
}

function destinationLabel(d: CheckDefinition, targets: Target[]): string {
  switch (d.destinationKind) {
    case "node":
      return "every node";
    case "target":
      return targets.find((t) => t.id === d.destinationTargetId)?.name ?? d.destinationTargetId;
    case "adhoc":
      return d.destinationAddress;
    default:
      return d.destinationKind;
  }
}

function DefinitionsTab({ canRead, canWrite }: { canRead: boolean; canWrite: boolean }) {
  const writesDisabled = useWritesDisabled();
  const [editing, setEditing] = useState<
    { mode: "none" } | { mode: "create" } | { mode: "edit"; definition: CheckDefinition }
  >({ mode: "none" });
  const query = useQuery({ queryKey: ["definitions"], queryFn: () => listChecks(), enabled: canRead });
  // The definition form's target picker needs the target list; same query key
  // the Targets tab uses, so this shares one cache entry rather than
  // introducing a second notion of "the targets".
  const targetsQuery = useQuery({ queryKey: ["targets"], queryFn: () => listTargets(), enabled: canRead });
  const definitions = query.data?.definitions ?? [];
  const targets = targetsQuery.data?.targets ?? [];

  if (!canRead) {
    return (
      <PermissionCard permission="checks:read">
        Check definitions say what the fleet probes and how often. Reading them is granted to the operator and admin
        roles.
      </PermissionCard>
    );
  }

  return (
    <div className="flex flex-col gap-4">
      {canWrite ? null : (
        <PermissionCard permission="checks:write">
          The list below is complete and current. Creating, editing and deleting definitions — and asking for the
          projected series count before enabling one — all need the write permission.
        </PermissionCard>
      )}

      {canWrite && editing.mode === "none" ? (
        <div>
          <Button size="sm" disabled={writesDisabled} onClick={() => setEditing({ mode: "create" })}>
            New definition
          </Button>
        </div>
      ) : null}
      {canWrite && editing.mode !== "none" ? (
        <DefinitionForm
          key={editing.mode === "edit" ? editing.definition.id : "create"}
          initial={editing.mode === "edit" ? editing.definition : undefined}
          targets={targets}
          onDone={() => setEditing({ mode: "none" })}
        />
      ) : null}

      <Card asChild className="p-6">
        <section>
          <h2 className="text-sm font-semibold">Check definitions</h2>
          {query.isError ? (
            <p role="alert" className="mt-3 text-sm text-health-bad">
              {queryErrorMessage(query.error, "Check definitions are unavailable")}
            </p>
          ) : null}
          {query.isLoading ? <ListSkeleton /> : null}
          {!query.isLoading && definitions.length === 0 && !query.isError ? (
            <EmptyRow>No check definitions yet.</EmptyRow>
          ) : null}
          {definitions.length > 0 ? (
            <ul aria-label="Check definitions" className="mt-4 divide-y divide-border">
              {definitions.map((d) => (
                <li key={d.id} className="flex flex-wrap items-center gap-3 py-3 text-sm">
                  <span className="font-medium">{d.name}</span>
                  <span className="text-xs uppercase tracking-wide text-muted-foreground">{d.checkType}</span>
                  <span className="text-xs text-muted-foreground">
                    {d.sourceSelection} → {destinationLabel(d, targets)}
                  </span>
                  <Badge variant={d.enabled ? "ok" : "neutral"} dot>
                    {d.enabled ? "enabled" : "disabled"}
                  </Badge>
                  {canWrite ? (
                    <span className="ml-auto">
                      <DefinitionRowActions
                        definition={d}
                        onEdit={() => setEditing({ mode: "edit", definition: d })}
                      />
                    </span>
                  ) : null}
                </li>
              ))}
            </ul>
          ) : null}
        </section>
      </Card>
    </div>
  );
}

/* ── Schedules tab ──────────────────────────────────────────────────────── */

export function fmtIntervalNs(ns: number): string {
  if (!ns) return "—";
  const seconds = ns / 1_000_000_000;
  if (seconds < 60) return `${Number(seconds.toFixed(3))}s`;
  const minutes = seconds / 60;
  if (minutes < 60) return `${Number(minutes.toFixed(2))}m`;
  return `${Number((minutes / 60).toFixed(2))}h`;
}

function cadence(s: Schedule): string {
  switch (s.kind) {
    case "interval":
      return `every ${fmtIntervalNs(s.intervalNs)}`;
    case "once":
      return `once at ${fmtTime(s.runAt)}`;
    case "continuous":
      return "continuous";
    default:
      return s.kind;
  }
}

/** SCHEDULE_KINDS is the picker, and "cron" is ABSENT from it on purpose.
 *  The server refuses kind "cron" with a 422 naming the milestone it lands in
 *  (httpapi's cronDeferredDetail), so offering a control whose every use is a
 *  guaranteed rejection would be worse than not offering it at all — the
 *  operator would learn "not yet" only after filling the form in. It comes
 *  back here when the server accepts it, not before. */
const SCHEDULE_KINDS: ScheduleKind[] = ["once", "interval", "continuous"];

/**
 * scheduleRequestFrom rebuilds a stored schedule's OWN cadence as a request
 * body, changing only `enabled`. PUT /api/v1/schedules/{id} is a full replace
 * — an omitted field means empty, never "leave as-is" — so a toggle that sent
 * only {enabled} would erase the interval or the run-at it was toggling.
 *
 * Per-kind, mirroring what the server accepts: intervalNs only for
 * "interval", runAt only for "once", neither for "continuous" (store's
 * Validate refuses the extras rather than ignoring them).
 */
export function scheduleRequestFrom(s: Schedule, enabled: boolean): ScheduleRequest {
  return {
    definitionId: s.definitionId,
    kind: s.kind,
    ...(s.kind === "interval" ? { intervalNs: s.intervalNs } : {}),
    ...(s.kind === "once" && s.runAt ? { runAt: s.runAt } : {}),
    enabled,
  };
}

/** MIN_INTERVAL_SECONDS mirrors httpapi's minScheduleInterval: a positive
 *  sub-floor interval is CLAMPED up server-side rather than rejected, so this
 *  is advisory text, not a client-side gate. */
const MIN_INTERVAL_SECONDS = 10;

/** toIsoOrNull turns a datetime-local value ("2026-08-08T10:00", local time)
 *  into the RFC 3339 instant the API takes; null for anything unparseable, so
 *  the form reports it instead of posting "Invalid Date". */
export function localDateTimeToIso(value: string): string | null {
  if (value === "") return null;
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? null : d.toISOString();
}

/**
 * ScheduleForm creates a cadence. Its fields follow the server's own
 * cross-field rules exactly (store.ScheduleInput.Validate) rather than
 * offering every field for every kind and letting the 422 explain:
 *
 *   - interval: intervalNs required and positive; no runAt.
 *   - once:     runAt required, and in the FUTURE (the clock rule httpapi
 *               adds on top of store's); no intervalNs.
 *   - continuous: NEITHER — these are agent-side by definition, the scheduler
 *               loop never fires them, and they have no next fire at all.
 *
 * Whatever still comes back 422 renders inline at the field the server's own
 * message names, through the same fieldForDetail heuristic the other two
 * forms use.
 */
function ScheduleForm({ definitions, onDone }: { definitions: CheckDefinition[]; onDone: () => void }) {
  const qc = useQueryClient();
  const writesDisabled = useWritesDisabled();
  const [definitionId, setDefinitionId] = useState(definitions[0]?.id ?? "");
  const [kind, setKind] = useState<ScheduleKind>("interval");
  const [intervalSeconds, setIntervalSeconds] = useState("60");
  const [runAtLocal, setRunAtLocal] = useState("");
  const [enabled, setEnabled] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const [errors, setErrors] = useState<FieldErrors<ScheduleField>>({});

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setErrors({});

    const req: ScheduleRequest = { definitionId, kind, enabled };
    if (kind === "interval") {
      const seconds = Number(intervalSeconds);
      if (!Number.isFinite(seconds) || seconds <= 0) {
        setErrors({ intervalNs: "interval must be a positive number of seconds" });
        return;
      }
      req.intervalNs = Math.round(seconds * 1_000_000_000);
    }
    if (kind === "once") {
      const iso = localDateTimeToIso(runAtLocal);
      if (iso === null) {
        setErrors({ runAt: "kind once requires a run at time" });
        return;
      }
      req.runAt = iso;
    }

    setSubmitting(true);
    try {
      await createSchedule(req);
      await qc.invalidateQueries({ queryKey: ["schedules"] });
      onDone();
    } catch (err) {
      setErrors(errorsFromProblem(err, SCHEDULE_FIELD_PHRASES, "Failed to save the schedule"));
      setSubmitting(false);
    }
  }

  return (
    <Card asChild className="p-6">
      <form onSubmit={handleSubmit} className="flex flex-col gap-4">
        <h3 className="text-sm font-semibold">New schedule</h3>
        <div className="grid gap-4 sm:grid-cols-2">
          <SelectField
            label="Definition"
            value={definitionId}
            onChange={setDefinitionId}
            options={[
              { value: "", label: "— pick a definition —" },
              ...definitions.map((d) => ({ value: d.id, label: d.name })),
            ]}
            error={errors.definitionId}
          />
          <SelectField
            label="Kind"
            value={kind}
            onChange={setKind}
            options={plainOptions(SCHEDULE_KINDS)}
            error={errors.kind}
          />
          {kind === "interval" ? (
            <TextField
              label="Interval (seconds)"
              value={intervalSeconds}
              onChange={setIntervalSeconds}
              error={errors.intervalNs}
              placeholder="60"
            />
          ) : null}
          {kind === "once" ? (
            <div className="flex flex-col gap-1 text-[13px]">
              <label htmlFor="schedule-run-at" className="text-muted-foreground">
                Run at
              </label>
              <input
                id="schedule-run-at"
                type="datetime-local"
                value={runAtLocal}
                aria-invalid={errors.runAt ? true : undefined}
                onChange={(e) => setRunAtLocal(e.target.value)}
                className={fieldClasses(!!errors.runAt)}
              />
              {errors.runAt ? (
                <span role="alert" className="text-xs leading-relaxed text-health-bad">
                  {errors.runAt}
                </span>
              ) : null}
            </div>
          ) : null}
        </div>

        <p className="max-w-prose text-xs leading-relaxed text-muted-foreground">
          {kind === "interval"
            ? `Intervals below ${MIN_INTERVAL_SECONDS}s are raised to ${MIN_INTERVAL_SECONDS}s.`
            : null}
          {kind === "once" ? "A one-off fire, and it must be in the future." : null}
          {kind === "continuous"
            ? "Continuous schedules are pushed to the agents and never fire on the scheduler's clock — they carry no interval and no run-at."
            : null}
        </p>

        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
            className="size-4 rounded border-border-strong"
          />
          Enabled
        </label>

        {errors.form ? (
          <p role="alert" className="text-sm text-health-bad">
            {errors.form}
          </p>
        ) : null}

        <div className="flex gap-2">
          <Button type="submit" loading={submitting} disabled={writesDisabled}>
            Create schedule
          </Button>
          <Button type="button" variant="outline" onClick={onDone}>
            Cancel
          </Button>
        </div>
      </form>
    </Card>
  );
}

/** ScheduleRowActions is the enable/disable toggle and the delete, both
 *  behind schedules:write. The toggle is a full-replace PUT of the row's own
 *  cadence (scheduleRequestFrom) — and re-enabling a "once" schedule whose
 *  moment has passed is a 422 from the server's future-run-at rule, which is
 *  the honest answer and renders right here rather than being pre-empted. */
function ScheduleRowActions({ schedule, label }: { schedule: Schedule; label: string }) {
  const qc = useQueryClient();
  const writesDisabled = useWritesDisabled();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  async function run(fn: () => Promise<unknown>, fallback: string) {
    setBusy(true);
    setError(undefined);
    try {
      await fn();
      await qc.invalidateQueries({ queryKey: ["schedules"] });
    } catch (err) {
      setError(queryErrorMessage(err, fallback));
    }
    setBusy(false);
    setConfirming(false);
  }

  if (confirming) {
    return (
      <span className="flex items-center gap-2">
        <Button
          size="sm"
          variant="outline"
          loading={busy}
          disabled={writesDisabled}
          onClick={() => run(() => deleteSchedule(schedule.id), "Failed to delete the schedule")}
        >
          Confirm delete {label}
        </Button>
        <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>
          Cancel
        </Button>
      </span>
    );
  }
  return (
    <span className="flex items-center gap-2">
      <Button
        size="sm"
        variant="ghost"
        loading={busy}
        disabled={writesDisabled}
        onClick={() =>
          run(
            () => updateSchedule(schedule.id, scheduleRequestFrom(schedule, !schedule.enabled)),
            "Failed to update the schedule",
          )
        }
      >
        {schedule.enabled ? "Disable" : "Enable"} {label}
      </Button>
      <Button size="sm" variant="ghost" disabled={writesDisabled} onClick={() => setConfirming(true)}>
        Delete {label}
      </Button>
      {error ? (
        <span role="alert" className="text-xs text-health-bad">
          {error}
        </span>
      ) : null}
    </span>
  );
}

/**
 * SchedulesTab lists the cadences and, with schedules:write, creates,
 * enables/disables and deletes them.
 *
 * The permission split is the API's own, and it is asymmetric: READING rides
 * on checks:read, because there is no schedules:read permission at all
 * (middleware_auth.go: "reading a cadence tells you nothing the definition it
 * belongs to does not already tell you"), while every mutation needs
 * schedules:write. So a reader without it sees the same complete list, with
 * the write affordances ABSENT rather than disabled (PAGES.md:126-129).
 */
function SchedulesTab({ canRead, canWrite }: { canRead: boolean; canWrite: boolean }) {
  const writesDisabled = useWritesDisabled();
  const [creating, setCreating] = useState(false);
  const query = useQuery({ queryKey: ["schedules"], queryFn: () => listSchedules(), enabled: canRead });
  // Named, not numbered: a schedule row that shows only a definition UUID
  // tells an operator nothing. Same ["definitions"] cache entry the
  // Definitions tab fills.
  const definitionsQuery = useQuery({ queryKey: ["definitions"], queryFn: () => listChecks(), enabled: canRead });
  const schedules = query.data?.schedules ?? [];
  const definitions = definitionsQuery.data?.definitions ?? [];
  const names = useMemo(() => {
    const map = new Map<string, string>();
    for (const d of definitionsQuery.data?.definitions ?? []) map.set(d.id, d.name);
    return map;
  }, [definitionsQuery.data]);

  if (!canRead) {
    return (
      <PermissionCard permission="checks:read">
        Schedules have no read permission of their own — listing them rides on the definitions they belong to.
      </PermissionCard>
    );
  }

  return (
    <div className="flex flex-col gap-4">
      {canWrite ? null : (
        <PermissionCard permission="schedules:write">
          The list below is complete and current. Creating a cadence, enabling or disabling one, and deleting one all
          need the write permission — reading them only needs checks:read.
        </PermissionCard>
      )}

      {canWrite && !creating ? (
        <div>
          <Button size="sm" disabled={writesDisabled} onClick={() => setCreating(true)}>
            New schedule
          </Button>
        </div>
      ) : null}
      {canWrite && creating ? <ScheduleForm definitions={definitions} onDone={() => setCreating(false)} /> : null}

      <Card asChild className="p-6">
        <section>
          <h2 className="text-sm font-semibold">Schedules</h2>
          {query.isError ? (
            <p role="alert" className="mt-3 text-sm text-health-bad">
              {queryErrorMessage(query.error, "Schedules are unavailable")}
            </p>
          ) : null}
          {query.isLoading ? <ListSkeleton /> : null}
          {!query.isLoading && schedules.length === 0 && !query.isError ? (
            <EmptyRow>No schedules yet.</EmptyRow>
          ) : null}
          {schedules.length > 0 ? (
            <ul aria-label="Schedules" className="mt-4 divide-y divide-border">
              {schedules.map((s) => {
                const label = names.get(s.definitionId) ?? s.definitionId;
                return (
                  <li key={s.id} className="flex flex-wrap items-center gap-3 py-3 text-sm">
                    <span className="font-medium">{label}</span>
                    <Badge variant="neutral">{s.kind}</Badge>
                    <span className="text-xs text-muted-foreground">{cadence(s)}</span>
                    <Badge variant={s.enabled ? "ok" : "neutral"} dot>
                      {s.enabled ? "enabled" : "disabled"}
                    </Badge>
                    {/* nextFireAt is null for a continuous schedule (the loop
                        never fires one) and for a retired "once" — fmtTime
                        renders that as an em dash rather than inventing a
                        time. */}
                    <span className="ml-auto text-xs text-muted-foreground">next {fmtTime(s.nextFireAt)}</span>
                    <span className="text-xs text-muted-foreground">last {fmtTime(s.lastFiredAt)}</span>
                    {canWrite ? <ScheduleRowActions schedule={s} label={label} /> : null}
                  </li>
                );
              })}
            </ul>
          ) : null}
        </section>
      </Card>
    </div>
  );
}

/* ── the page ───────────────────────────────────────────────────────────── */

/**
 * TargetsPage is /targets: external probe targets, the check definitions that
 * point at them, and the schedules that fire those definitions.
 *
 * Three degraded states are designed here rather than left to fall out of
 * failing requests, because they are the states the "feature-off keeps the M3
 * surface" constraint lives in:
 *
 *  1. NO targets:read — one permission card, ZERO requests. This is the
 *     anonymous/viewer default: none of the five M4 permissions is granted to
 *     `viewer` (Decision 3, authz.go's builtinRoles), and viewer is what
 *     auth.anonymous.role defaults to. targets:read is the page-level gate
 *     because the built-in roles move all five together — a hand-rolled role
 *     with checks:read but not targets:read would see this card too, which is
 *     the honest failure direction (nothing shown) rather than a page that
 *     fires requests it cannot read the answers to.
 *
 *  2. database.mode=disabled — one honest line naming console.database.mode
 *     and NO /targets, /checks or /schedules request at all, rather than five
 *     requests to collect five 503s. Derived from GET /api/v1/config's
 *     `database.configured`, which is exactly the gate the handlers' own 503
 *     reads; verbatim the pattern components/recent-changes.tsx uses for the
 *     event rail (PAGES.md:65-70).
 *
 *  Order matters: the permission card comes first, because "you cannot see
 *  this" is about the subject and stays true regardless of how the console is
 *  deployed, while "there is no database" is only interesting to someone who
 *  could otherwise have used the page.
 *
 *  3. targets:read but no targets:write — the create button and the row
 *     actions are ABSENT, not disabled-with-a-tooltip, above a fully
 *     functional read-only list, with a card naming the missing permission
 *     (PAGES.md:126-129's run-form pattern). Same shape one level down for
 *     checks:read / checks:write inside the Definitions tab.
 *
 * Both gates wait for their answer before deciding. `can()` fails closed while
 * GET /api/v1/auth/me is in flight and `available` is false before
 * /api/v1/config lands, so rendering on the un-resolved value would flash the
 * permission card (or the database note) on every cold load — the same
 * "resolved vs false" split useDatabaseAvailable's own doc comment warns
 * about, applied to `me` as well.
 */
export function TargetsPage() {
  const { me, can } = useAuth();
  const { available: dbAvailable, resolved: dbResolved } = useDatabaseAvailable();
  const [tab, setTab] = useState<Tab>("targets");

  const authResolved = me !== undefined;

  let body: ReactNode;
  if (!authResolved || !dbResolved) {
    body = (
      <Card role="status" aria-live="polite" className="p-6">
        <span className="sr-only">Loading…</span>
        <Skeleton className="h-10 w-full" />
      </Card>
    );
  } else if (!can("targets:read")) {
    body = (
      <PermissionCard permission="targets:read">
        External targets, their check definitions and their schedules are configuration, not telemetry: reading them is
        granted to the operator and admin roles, and deliberately not to viewer — which is the role an anonymous
        session gets. Sign in with an account that holds it.
      </PermissionCard>
    );
  } else if (!dbAvailable) {
    body = (
      <Card role="status" className="p-6">
        <p className="text-sm">Targets, definitions and schedules are stored in the database — set console.database.mode</p>
      </Card>
    );
  } else {
    body = (
      <>
        <Segmented aria-label="Section" options={TABS} value={tab} onChange={setTab} />
        {tab === "targets" ? <TargetsTab canWrite={can("targets:write")} /> : null}
        {tab === "definitions" ? (
          <DefinitionsTab canRead={can("checks:read")} canWrite={can("checks:write")} />
        ) : null}
        {tab === "schedules" ? (
          <SchedulesTab canRead={can("checks:read")} canWrite={can("schedules:write")} />
        ) : null}
      </>
    );
  }

  return (
    <PageShell
      title="Targets & Schedules"
      description="External probe targets, the check definitions that point at them, and their schedules."
    >
      {body}
    </PageShell>
  );
}
