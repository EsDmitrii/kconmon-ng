import { useCallback, useEffect, useId, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { PageShell } from "@/components/page-shell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { DateTimePicker } from "@/components/ui/datetime-picker";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { useAuth } from "@/hooks/use-auth";
import { useDatabaseAvailable } from "@/hooks/use-capabilities";
import { useSubmitGuard } from "@/hooks/use-submit-guard";
import { subscribeToLocation } from "@/lib/location";
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
// Read directly in each mutating component rather than threaded down from the page as a prop: it is
// a context read.
import { stampFull, useLocale, useT, type Locale, type Translate, type Vars } from "@/lib/i18n";
import { pluralKey, targetsDict, type TargetsKey } from "@/lib/i18n/dict/targets";
/* The ad-hoc address refusal is lib/utils.ts's, shared with the run form on
   /diagnostics — one rule, one sentence, one table. */
import { validationDict } from "@/lib/i18n/dict/validation";
import { useWriteGuard } from "@/lib/timemachine";
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
import { CHECKBOX_CLASS, cn, isValidAdhocAddress } from "@/lib/utils";

/* The tab STRIP is a value plus the key that names it, not a value plus a
   label: `Tab` is derived from these values and is a type, while what the
   segment reads is a translation the page resolves at render. */
const TABS = [
  { value: "targets", labelKey: "tab.targets" },
  { value: "definitions", labelKey: "tab.definitions" },
  { value: "schedules", labelKey: "tab.schedules" },
] as const satisfies readonly { value: string; labelKey: TargetsKey }[];
type Tab = (typeof TABS)[number]["value"];

/** VIEW_PARAM puts the sub-view in the URL, so a reload, a Back and a pasted
 *  link all land on the tab the reader was actually looking at. */
export const VIEW_PARAM = "view";

function tabFromSearch(search: string): Tab {
  const raw = new URLSearchParams(search).get(VIEW_PARAM);
  return TABS.some((tb) => tb.value === raw) ? (raw as Tab) : "targets";
}

/**
 * useTabParam keeps ?view= and the rendered tab as one value.
 *
 * Selecting a tab PUSHES, so Back returns to the previous one rather than
 * leaving the page entirely; arriving via Back or a reload re-reads the param,
 * which is why this subscribes instead of reading once on mount the way
 * alerting's ?rule= does — that one is a landing hint, this one is the
 * control's own state.
 */
function useTabParam(): [Tab, (next: Tab) => void] {
  const [tab, setTab] = useState<Tab>(() => tabFromSearch(window.location.search));

  useEffect(
    () =>
      subscribeToLocation(() => {
        setTab(tabFromSearch(window.location.search));
      }),
    [],
  );

  const select = useCallback((next: Tab) => {
    const url = new URL(window.location.href);
    if (url.searchParams.get(VIEW_PARAM) === next) return;
    // The default view stays a CLEAN url: /targets, not /targets?view=targets.
    if (next === "targets") url.searchParams.delete(VIEW_PARAM);
    else url.searchParams.set(VIEW_PARAM, next);
    window.history.pushState({}, "", url);
    setTab(next);
  }, []);

  return [tab, select];
}

const TARGET_KINDS: TargetKind[] = ["host", "url"];

/* One field serves both kinds, and the server validates them by two different
   rules: kind=url wants an http(s) URL with a host, kind=host wants an IP or a
   resolvable name with an optional port. A single placeholder that showed a URL
   to someone filling in a HOST was suggesting the exact value that comes back a
   422 — the same reasoning, and the same shape, as diagnostics.tsx's
   ADHOC_PLACEHOLDER. Addresses are syntax, so the examples live here rather
   than in the dictionary. */
export const TARGET_ADDRESS_PLACEHOLDER: Record<TargetKind, string> = {
  host: "10.0.0.1 · edge-gateway.internal · 10.0.0.1:8443",
  url: "https://example.test/health",
};
const SOURCE_SELECTIONS: SourceSelection[] = ["all", "per-zone", "one-per-zone"];
const DESTINATION_KINDS: DestinationKind[] = ["node", "target", "adhoc"];

/**
 * PROJECTION_DEBOUNCE_MS keeps "project on change" from meaning "one POST per
 * keystroke". Deliberately well under vitest's 1s waitFor budget so the tests
 * exercise the real timer rather than a fake-clock double.
 */
export const PROJECTION_DEBOUNCE_MS = 250;

/* A detail that matches no phrase is still shown, verbatim, as a form-level error. */

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

/**
 * Same most-specific-first rule as the two tables below/above; the one ordering subtlety: store's
 * enum message ("kind %q must be one of once, interval, continuous") CONTAINS the word "interval".
 */
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
 * parseLabels reads the "k=v, k=v" text field into the object the API takes; it THROWS on a
 * malformed pair rather than dropping.
 */
export function parseLabels(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const raw of text.split(",")) {
    const part = raw.trim();
    if (part === "") continue;
    const eq = part.indexOf("=");
    if (eq <= 0) throw new LabelSyntaxError(part);
    out[part.slice(0, eq).trim()] = part.slice(eq + 1).trim();
  }
  return out;
}

/**
 * LabelSyntaxError is a plain Error with the OFFENDING FRAGMENT attached.
 *
 * The message is unchanged, byte for byte — parseLabels is a pure exported
 * parser and its sentence is pinned as data. What the subclass adds is the one
 * thing the form needed and could not recover from a formatted string: WHICH
 * fragment was wrong, so the field can render the same sentence in the reader's
 * own language with the same quoted bytes in it. A caller that only catches
 * `Error` and reads `.message` is unaffected.
 */
export class LabelSyntaxError extends Error {
  readonly part: string;
  constructor(part: string) {
    super(`labels must be "key=value" pairs separated by commas; got ${JSON.stringify(part)}`);
    this.name = "LabelSyntaxError";
    this.part = part;
  }
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
  hint,
}: {
  label: string;
  value: T;
  onChange: (v: T) => void;
  options: readonly { value: T; label: string }[];
  error?: string;
  disabled?: boolean;
  /** Why the field reads the way it does — a locked picker owes the reader a reason. */
  hint?: string;
}) {
  const id = useId();
  const errorId = `${id}-error`;
  const hintId = `${id}-hint`;
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
        aria-describedby={cn(error ? errorId : undefined, hint ? hintId : undefined) || undefined}
        onChange={(e) => onChange(e.target.value as T)}
        className={cn(fieldClasses(!!error), "disabled:opacity-70")}
      >
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
      {hint ? (
        <span id={hintId} className="text-xs leading-relaxed text-muted-foreground">
          {hint}
        </span>
      ) : null}
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
  const t = useT(targetsDict);
  /* The permission STRING is interpolated, never translated: "targets:write"
     is what an operator asks their admin for and what authz.go's role tables
     spell. Only the sentence around it is ours. */
  return (
    <Card role="status" className="p-6">
      <p className="text-sm font-medium">{t("permission.requires", { permission })}</p>
      <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{children}</p>
    </Card>
  );
}

function EmptyRow({ children }: { children: ReactNode }) {
  return <p className="px-1 py-10 text-center text-xs text-muted-foreground">{children}</p>;
}

function ListSkeleton() {
  const t = useT(targetsDict);
  return (
    <div role="status" aria-live="polite" className="mt-4 flex flex-col gap-2">
      <span className="sr-only">{t("loading")}</span>
      {Array.from({ length: 3 }, (_, i) => (
        <Skeleton key={i} className="h-10 w-full" />
      ))}
    </div>
  );
}

function queryErrorMessage(error: unknown, fallback: string): string {
  return error instanceof ApiError ? (error.problem.detail ?? error.problem.title) : fallback;
}

/** The locale is required: a bare toLocaleString() reorders the date and swaps in AM/PM from
 *  whatever the browser was installed in — "8/10/2026 3:47 AM" on a Russian page. */
function fmtTime(timestamp: string | null | undefined, locale: Locale): string {
  if (!timestamp) return "—";
  const d = new Date(timestamp);
  return Number.isNaN(d.getTime()) ? timestamp : stampFull(d, locale);
}

/* ── Targets tab ────────────────────────────────────────────────────────── */

function TargetForm({ initial, onDone }: { initial?: Target; onDone: () => void }) {
  const t = useT(targetsDict);
  const qc = useQueryClient();
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's useWriteGuard. */
  const guard = useWriteGuard();
  const [name, setName] = useState(initial?.name ?? "");
  const [kind, setKind] = useState<TargetKind>(initial?.kind ?? "host");
  const [address, setAddress] = useState(initial?.address ?? "");
  const [labels, setLabels] = useState(formatLabels(initial?.labels));
  /* The in-flight guard, not just a disabled look (QA round 5, finding #17):
     begin() is a REF write, so three clicks in one task produce one request.
     hooks/use-submit-guard.ts says why a useState flag cannot do this. */
  const { submitting, begin, end } = useSubmitGuard();
  const [errors, setErrors] = useState<FieldErrors<TargetField>>({});

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setErrors({});
    let parsedLabels: Record<string, string>;
    try {
      parsedLabels = parseLabels(labels);
    } catch (err) {
      setErrors({
        labels:
          err instanceof LabelSyntaxError
            ? t("targets.form.labelsSyntax", { part: JSON.stringify(err.part) })
            : err instanceof Error
              ? err.message
              : t("targets.form.labelsMalformed"),
      });
      return;
    }
    // A full replace, so every field goes on the wire — an omitted one means
    // EMPTY server-side (PUT /api/v1/targets/{id}), never "leave as-is".
    const req: TargetRequest = { name, kind, address, labels: parsedLabels };
    if (!begin()) return;
    try {
      if (initial) await updateTarget(initial.id, req);
      else await createTarget(req);
      await qc.invalidateQueries({ queryKey: ["targets"] });
      onDone();
    } catch (err) {
      setErrors(errorsFromProblem(err, TARGET_FIELD_PHRASES, t("targets.form.failed")));
      end();
    }
  }

  return (
    <Card asChild className="p-6">
      <form onSubmit={handleSubmit} className="flex flex-col gap-4">
        <h3 className="text-sm font-semibold">
          {initial ? t("targets.form.edit", { name: initial.name }) : t("targets.form.create")}
        </h3>
        <div className="grid gap-4 sm:grid-cols-2">
          {/* The placeholders stay: "edge-gateway" and "env=prod, tier=edge"
              are example VALUES, and a Russian sample label key would be an
              example nobody can paste. Only the connective in the address
              placeholder is prose, so only that one is translated. */}
          <TextField
            label={t("targets.form.name")}
            value={name}
            onChange={setName}
            error={errors.name}
            placeholder="edge-gateway"
          />
          <SelectField
            label={t("targets.form.kind")}
            value={kind}
            onChange={setKind}
            options={plainOptions(TARGET_KINDS)}
            error={errors.kind}
          />
          <TextField
            label={t("targets.form.address")}
            value={address}
            onChange={setAddress}
            error={errors.address}
            placeholder={TARGET_ADDRESS_PLACEHOLDER[kind]}
          />
          <TextField
            label={t("targets.form.labels")}
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
          <Button type="submit" loading={submitting} {...guard}>
            {initial ? t("targets.form.save") : t("targets.form.createButton")}
          </Button>
          <Button type="button" variant="outline" onClick={onDone}>
            {t("cancel")}
          </Button>
        </div>
      </form>
    </Card>
  );
}

/**
 * RowActionLabel is the VISIBLE half of a row button whose label carries the
 * object's own name.
 *
 * "Delete {name}" is the right thing for a screen reader — three "Delete"
 * buttons in a list are three identical announcements — but a 60-character
 * target name rendered in full blew the row's width apart (QA scope 2, #22).
 * The name stays whole in the aria-label and in `title`; only the pixels are
 * bounded, and the truncation is CSS, so nothing has to guess a character
 * count for a language it has not seen.
 */
function RowActionLabel({ text }: { text: string }) {
  return (
    <span aria-hidden="true" className="block max-w-[14rem] truncate" title={text}>
      {text}
    </span>
  );
}

function TargetRowActions({ target, onEdit }: { target: Target; onEdit: () => void }) {
  const t = useT(targetsDict);
  const qc = useQueryClient();
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's useWriteGuard. */
  const guard = useWriteGuard();
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
      setError(queryErrorMessage(err, t("targets.row.deleteFailed")));
      setBusy(false);
      setConfirming(false);
    }
  }

  if (confirming) {
    return (
      <span className="flex flex-wrap items-center gap-2">
        <Button
          size="sm"
          variant="outline"
          loading={busy}
          {...guard}
          aria-label={t("targets.row.confirmDelete", { name: target.name })}
          onClick={handleDelete}
        >
          <RowActionLabel text={t("targets.row.confirmDelete", { name: target.name })} />
        </Button>
        <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>
          {t("cancel")}
        </Button>
      </span>
    );
  }
  return (
    <span className="flex flex-wrap items-center gap-2">
      {/* Edit opens a form whose only purpose is to submit a PUT, so it is
          disabled with the write it leads to rather than left to dead-end at a
          greyed Save. */}
      <Button size="sm" variant="ghost" {...guard} aria-label={t("targets.row.edit", { name: target.name })} onClick={onEdit}>
        <RowActionLabel text={t("targets.row.edit", { name: target.name })} />
      </Button>
      <Button
        size="sm"
        variant="ghost"
        {...guard}
        aria-label={t("targets.row.delete", { name: target.name })}
        onClick={() => setConfirming(true)}
      >
        <RowActionLabel text={t("targets.row.delete", { name: target.name })} />
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
  const t = useT(targetsDict);
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's useWriteGuard. */
  const guard = useWriteGuard();
  const [editing, setEditing] = useState<{ mode: "none" } | { mode: "create" } | { mode: "edit"; target: Target }>({
    mode: "none",
  });
  const query = useQuery({ queryKey: ["targets"], queryFn: () => listTargets() });
  const targets = query.data?.targets ?? [];

  return (
    <div className="flex flex-col gap-4">
      {canWrite ? null : (
        <PermissionCard permission="targets:write">{t("targets.gate.write")}</PermissionCard>
      )}

      {canWrite && editing.mode === "none" ? (
        <div>
          <Button size="sm" {...guard} onClick={() => setEditing({ mode: "create" })}>
            {t("targets.new")}
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
          <h2 className="text-sm font-semibold">{t("targets.heading")}</h2>
          {query.isError ? (
            <p role="alert" className="mt-3 text-sm text-health-bad">
              {queryErrorMessage(query.error, t("targets.unavailable"))}
            </p>
          ) : null}
          {query.isLoading ? <ListSkeleton /> : null}
          {!query.isLoading && targets.length === 0 && !query.isError ? (
            <EmptyRow>{t("targets.empty")}</EmptyRow>
          ) : null}
          {targets.length > 0 ? (
            <ul aria-label={t("targets.listAria")} className="mt-4 divide-y divide-border">
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
                  <span className="min-w-0 truncate text-xs text-muted-foreground">{t.address}</span>
                  {Object.keys(t.labels).length > 0 ? (
                    <span className="min-w-0 truncate text-xs text-muted-foreground">{formatLabels(t.labels)}</span>
                  ) : null}
                  {canWrite ? (
                    <span className="ml-auto flex flex-wrap items-center gap-2">
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
function projectionVars(p: Projection, t: Translate<TargetsKey>): Vars {
  /* The three nouns are looked up per COUNT, not pluralised by a suffix: "5
     протоколов" and "2 протокола" are different words, and the `${n === 1 ?
     "" : "s"}` the English used cannot produce either. English resolves all
     three forms to the word it always used, so the rendered English is
     unchanged. */
  return {
    series: p.series,
    seriesWord: t(pluralKey(p.series, "count.series.one", "count.series.few", "count.series.many")),
    agents: p.agents,
    agentsWord: t(pluralKey(p.agents, "count.agents.one", "count.agents.few", "count.agents.many")),
    protocols: p.protocols,
    protocolsWord: t(
      pluralKey(p.protocols, "count.protocols.one", "count.protocols.few", "count.protocols.many"),
    ),
    limit: p.limit,
  };
}

/* The two refusals are returned as KEYS rather than sentences: this is a pure
   parser and must not hold a translator, while the field that renders the
   result already has one. `params` itself stays untranslated inside both
   sentences — it is the wire field's name. */
function tryParseParams(
  text: string,
): { ok: true; value: Record<string, unknown> | undefined } | { ok: false; messageKey: TargetsKey } {
  const trimmed = text.trim();
  if (trimmed === "") return { ok: true, value: undefined };
  try {
    const parsed: unknown = JSON.parse(trimmed);
    if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
      return { ok: false, messageKey: "definitions.form.paramsNotObject" };
    }
    return { ok: true, value: parsed as Record<string, unknown> };
  } catch {
    return { ok: false, messageKey: "definitions.form.paramsNotJson" };
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
  const t = useT(targetsDict);
  const tv = useT(validationDict);
  const qc = useQueryClient();
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's
     useWriteGuard (QA round 2, finding #18; extended here in round 3). Spread it
     onto the control, and compose any local condition AFTER the spread. */
  const guard = useWriteGuard();
  const writesDisabled = guard.disabled;
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
  /* The in-flight guard, not just a disabled look (QA round 5, finding #17):
     begin() is a REF write, so three clicks in one task produce one request.
     hooks/use-submit-guard.ts says why a useState flag cannot do this. */
  const { submitting, begin, end } = useSubmitGuard();
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
      setErrors({ params: t(params.messageKey) });
      return;
    }
    /* The client mirror of store.validateAdhocAddress (QA round 4, finding
       #13). This form is the one that PERSISTS an ad-hoc address, and until
       the store learned to check it, "sdfsdfsdf !!" was accepted, written, and
       then failed as a resolver refusal on every assigned agent, every
       interval, forever — with nothing on this page ever saying so. The rule
       is derived from what the agent's own checker accepts (lib/utils'
       isValidAdhocAddress documents the derivation); the server remains the
       arbiter, and this only puts the refusal at the field. */
    if (
      destinationKind === "adhoc" &&
      destinationAddress.trim() !== "" &&
      !isValidAdhocAddress(destinationAddress)
    ) {
      // An EMPTY address keeps its existing path: "adhoc requires a
      // destination address" is the server's own message for a missing
      // required field, and duplicating it here would give one condition two
      // wordings. This branch is only about a value that IS there and cannot
      // be dialled.
      setErrors({ destinationAddress: tv("adhoc.address") });
      return;
    }
    if (!begin()) return;
    try {
      if (initial) await updateCheck(initial.id, draft);
      else await createCheck(draft);
      await qc.invalidateQueries({ queryKey: ["definitions"] });
      onDone();
    } catch (err) {
      setErrors(errorsFromProblem(err, DEFINITION_FIELD_PHRASES, t("definitions.form.failed")));
      end();
    }
  }

  return (
    <Card asChild className="p-6">
      <form onSubmit={handleSubmit} className="flex flex-col gap-4">
        <h3 className="text-sm font-semibold">
          {initial ? t("definitions.form.edit", { name: initial.name }) : t("definitions.form.create")}
        </h3>
        <div className="grid gap-4 sm:grid-cols-2">
          {/* Every select below renders its WIRE VALUES as its labels
              (plainOptions). They stay English because they are not English —
              they are the strings the API stores and the operator greps for:
              tcp, one-per-zone, adhoc. Only the field NAMES are translated. */}
          <TextField
            label={t("definitions.form.name")}
            value={name}
            onChange={setName}
            error={errors.name}
            placeholder="edge-gateway-tcp"
          />
          <SelectField
            label={t("definitions.form.checkType")}
            value={checkType}
            onChange={setCheckType}
            options={plainOptions(CHECK_TYPES)}
            error={errors.checkType}
          />
          <SelectField
            label={t("definitions.form.sourceSelection")}
            value={sourceSelection}
            onChange={setSourceSelection}
            options={plainOptions(SOURCE_SELECTIONS)}
            error={errors.sourceSelection}
          />
          <SelectField
            label={t("definitions.form.destinationKind")}
            value={destinationKind}
            onChange={setDestinationKind}
            options={plainOptions(DESTINATION_KINDS)}
            error={errors.destinationKind}
          />
          {destinationKind === "target" ? (
            <SelectField
              label={t("definitions.form.destinationTarget")}
              value={destinationTargetId}
              onChange={setDestinationTargetId}
              options={[
                { value: "", label: t("definitions.form.pickTarget") },
                ...targets.map((target) => ({ value: target.id, label: target.name })),
              ]}
              error={errors.destinationTargetId}
            />
          ) : null}
          {destinationKind === "adhoc" ? (
            <TextField
              label={t("definitions.form.destinationAddress")}
              value={destinationAddress}
              onChange={setDestinationAddress}
              error={errors.destinationAddress}
              placeholder="10.0.0.1"
            />
          ) : null}
          {/* Plane is STATIC TEXT, not a disabled one-option select (QA round
              5, finding #16). A select is a promise of a choice, and a greyed
              one with a single option promises a choice that is coming — it is
              not. M4 fixed the plane to "pod": the agents run in the pod
              network and there is no second plane to probe from, so the value
              is a fact about this console, not a preference. A field that
              cannot vary should read as a value, and the title says WHY rather
              than leaving an operator to guess whether they lack a permission. */}
          <div className="flex flex-col gap-1 text-[13px]">
            <span className="text-muted-foreground">{t("definitions.form.plane")}</span>
            {/* "pod" is the VALUE, and the one value there is — it stays. */}
            <span
              data-testid="definition-plane"
              title={t("definitions.form.planeNote")}
              className="flex h-9 items-center text-[13px]"
            >
              pod
            </span>
            {errors.plane ? (
              <span role="alert" className="text-xs leading-relaxed text-health-bad">
                {errors.plane}
              </span>
            ) : null}
          </div>
        </div>

        <TextField
          label={t("definitions.form.params")}
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
            className={CHECKBOX_CLASS}
          />
          {t("definitions.form.enabled")}
        </label>

        {projection ? (
          <p
            role="status"
            className={cn("nums text-sm", projection.overLimit ? "text-health-bad" : "text-muted-foreground")}
          >
            {t(projection.overLimit ? "projection.over" : "projection.ok", projectionVars(projection, t))}
          </p>
        ) : null}

        {errors.form ? (
          <p role="alert" className="text-sm text-health-bad">
            {errors.form}
          </p>
        ) : null}

        <div className="flex gap-2">
          <Button type="submit" loading={submitting} {...guard} disabled={blocked || writesDisabled}>
            {initial ? t("definitions.form.save") : t("definitions.form.createButton")}
          </Button>
          <Button type="button" variant="outline" onClick={onDone}>
            {t("cancel")}
          </Button>
        </div>
      </form>
    </Card>
  );
}

function DefinitionRowActions({ definition, onEdit }: { definition: CheckDefinition; onEdit: () => void }) {
  const t = useT(targetsDict);
  const qc = useQueryClient();
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's
     useWriteGuard (QA round 2, finding #18; extended here in round 3). Spread it
     onto the control, and compose any local condition AFTER the spread. */
  const guard = useWriteGuard();
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
      setError(queryErrorMessage(err, t("definitions.row.deleteFailed")));
      setBusy(false);
      setConfirming(false);
    }
  }

  if (confirming) {
    return (
      <span className="flex flex-wrap items-center gap-2">
        <Button
          size="sm"
          variant="outline"
          loading={busy}
          {...guard}
          aria-label={t("definitions.row.confirmDelete", { name: definition.name })}
          onClick={handleDelete}
        >
          <RowActionLabel text={t("definitions.row.confirmDelete", { name: definition.name })} />
        </Button>
        <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>
          {t("cancel")}
        </Button>
      </span>
    );
  }
  return (
    <span className="flex flex-wrap items-center gap-2">
      <Button
        size="sm"
        variant="ghost"
        {...guard}
        aria-label={t("definitions.row.edit", { name: definition.name })}
        onClick={onEdit}
      >
        <RowActionLabel text={t("definitions.row.edit", { name: definition.name })} />
      </Button>
      <Button
        size="sm"
        variant="ghost"
        {...guard}
        aria-label={t("definitions.row.delete", { name: definition.name })}
        onClick={() => setConfirming(true)}
      >
        <RowActionLabel text={t("definitions.row.delete", { name: definition.name })} />
      </Button>
      {error ? (
        <span role="alert" className="text-xs text-health-bad">
          {error}
        </span>
      ) : null}
    </span>
  );
}

/* "every node" is the only one of the three destinations that is a PHRASE
   rather than a name — the other two return a target's name or an operator's
   own address, both of which are data. */
function destinationLabel(d: CheckDefinition, targets: Target[], t: Translate<TargetsKey>): string {
  switch (d.destinationKind) {
    case "node":
      return t("definitions.destination.everyNode");
    case "target":
      return targets.find((t) => t.id === d.destinationTargetId)?.name ?? d.destinationTargetId;
    case "adhoc":
      return d.destinationAddress;
    default:
      return d.destinationKind;
  }
}

function DefinitionsTab({ canRead, canWrite }: { canRead: boolean; canWrite: boolean }) {
  const t = useT(targetsDict);
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's
     useWriteGuard (QA round 2, finding #18; extended here in round 3). Spread it
     onto the control, and compose any local condition AFTER the spread. */
  const guard = useWriteGuard();
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
    return <PermissionCard permission="checks:read">{t("definitions.gate.read")}</PermissionCard>;
  }

  return (
    <div className="flex flex-col gap-4">
      {canWrite ? null : (
        <PermissionCard permission="checks:write">{t("definitions.gate.write")}</PermissionCard>
      )}

      {canWrite && editing.mode === "none" ? (
        <div>
          <Button size="sm" {...guard} onClick={() => setEditing({ mode: "create" })}>
            {t("definitions.new")}
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
          <h2 className="text-sm font-semibold">{t("definitions.heading")}</h2>
          {query.isError ? (
            <p role="alert" className="mt-3 text-sm text-health-bad">
              {queryErrorMessage(query.error, t("definitions.unavailable"))}
            </p>
          ) : null}
          {query.isLoading ? <ListSkeleton /> : null}
          {!query.isLoading && definitions.length === 0 && !query.isError ? (
            <EmptyRow>{t("definitions.empty")}</EmptyRow>
          ) : null}
          {definitions.length > 0 ? (
            <ul aria-label={t("definitions.listAria")} className="mt-4 divide-y divide-border">
              {definitions.map((d) => (
                <li key={d.id} className="flex flex-wrap items-center gap-3 py-3 text-sm">
                  <span className="font-medium">{d.name}</span>
                  {/* checkType and sourceSelection are the row's own stored
                      values, shown as they are stored. The PILL beside them is
                      this page describing a boolean, so it translates. */}
                  <span className="text-xs uppercase tracking-wide text-muted-foreground">{d.checkType}</span>
                  <span className="text-xs text-muted-foreground">
                    {d.sourceSelection} → {destinationLabel(d, targets, t)}
                  </span>
                  <Badge variant={d.enabled ? "ok" : "neutral"} dot>
                    {d.enabled ? t("definitions.enabled") : t("definitions.disabled")}
                  </Badge>
                  {canWrite ? (
                    <span className="ml-auto flex flex-wrap items-center gap-2">
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

/**
 * intervalParts splits a nanosecond cadence into the number and the UNIT it is
 * counted in — the same s/m/h ladder fmtIntervalNs walks, kept as data so a
 * language that inflects can pick a word per unit instead of gluing a letter
 * onto a digit.
 */
export type IntervalUnit = "second" | "minute" | "hour";

export function intervalParts(ns: number): { value: number; unit: IntervalUnit } | null {
  if (!ns) return null;
  const seconds = ns / 1_000_000_000;
  if (seconds < 60) return { value: Number(seconds.toFixed(3)), unit: "second" };
  const minutes = seconds / 60;
  if (minutes < 60) return { value: Number(minutes.toFixed(2)), unit: "minute" };
  return { value: Number((minutes / 60).toFixed(2)), unit: "hour" };
}

/**
 * fmtCadence renders "every <interval>" as a SENTENCE in the reader's own
 * grammar.
 *
 * English glues a unit letter onto a number and is done — "every 1m" — so the
 * English path is left exactly as it was. Russian cannot do that: «каждые 1m»
 * is not a thing anyone writes, and it was what this row said. So on the
 * Russian path a cadence of exactly ONE unit takes the singular phrase
 * («каждую минуту»), and every other count takes a plural phrase with a
 * NON-DECLINING abbreviation («каждые 5 мин») — the one form that stays
 * grammatical for every number without carrying a case table around.
 */
export interface CadencePhrases {
  /** "every {interval}" / «каждые {interval}» — the counted form. */
  interval: (interval: string) => string;
  /** The phrase for exactly one of each unit: "every minute" / «каждую минуту». */
  every: Record<IntervalUnit, string>;
  /** The non-declining unit abbreviation used inside the counted form. */
  unit: Record<IntervalUnit, string>;
}

/* The phrases come in rather than a Translate, because the two callers read
   from two different dictionaries (targets' "schedules.*" and cards'
   "schedule.*") and one shared key union would have forced them into one. */
export function fmtCadence(ns: number, locale: Locale, phrases: CadencePhrases): string {
  const parts = intervalParts(ns);
  if (locale === "en" || !parts) {
    return phrases.interval(fmtIntervalNs(ns));
  }
  if (parts.value === 1) {
    return phrases.every[parts.unit];
  }
  return phrases.interval(`${parts.value} ${phrases.unit[parts.unit]}`);
}

/* The cadence is a SENTENCE about a schedule, not the schedule's `kind` field,
   so it translates — while the fallback for a kind this build has never heard
   of renders that kind verbatim, because inventing prose for an unknown value
   would be worse than showing it. */
function cadence(s: Schedule, locale: Locale, t: Translate<TargetsKey>): string {
  switch (s.kind) {
    case "interval":
      return fmtCadence(s.intervalNs, locale, {
        interval: (interval) => t("schedules.cadence.interval", { interval }),
        every: {
          second: t("schedules.cadence.every.second"),
          minute: t("schedules.cadence.every.minute"),
          hour: t("schedules.cadence.every.hour"),
        },
        unit: {
          second: t("schedules.cadence.unit.second"),
          minute: t("schedules.cadence.unit.minute"),
          hour: t("schedules.cadence.unit.hour"),
        },
      });
    case "once":
      return t("schedules.cadence.once", { at: fmtTime(s.runAt, locale) });
    case "continuous":
      return t("schedules.cadence.continuous");
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

/** localDateTimeToIso turns a local wall-clock string ("2026-08-08T10:00") into
 *  the RFC 3339 instant the API takes; null for anything unparseable, so the
 *  form reports it instead of posting "Invalid Date".
 *
 *  The form no longer feeds it from an `<input type="datetime-local">` — the
 *  DateTimePicker below hands over a Date directly (QA round 3, finding #12) —
 *  but it stays exported and tested: it is the exact conversion a permalink or
 *  an imported bundle would need, and its rule (unparseable → null, never
 *  Invalid Date on the wire) is the one worth keeping pinned. */
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
function ScheduleForm({
  initial,
  definitions,
  onDone,
}: {
  /** Present in EDIT mode: PUT /api/v1/schedules/{id} is a full replace, so the
   *  form is seeded from the stored row and sends every field back. */
  initial?: Schedule;
  definitions: CheckDefinition[];
  onDone: () => void;
}) {
  const t = useT(targetsDict);
  const { locale } = useLocale();
  const qc = useQueryClient();
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's
     useWriteGuard (QA round 2, finding #18; extended here in round 3). Spread it
     onto the control, and compose any local condition AFTER the spread. */
  const guard = useWriteGuard();
  const [definitionId, setDefinitionId] = useState(initial?.definitionId ?? definitions[0]?.id ?? "");
  const [kind, setKind] = useState<ScheduleKind>(initial?.kind ?? "interval");
  const [intervalSeconds, setIntervalSeconds] = useState(
    initial && initial.intervalNs > 0 ? String(initial.intervalNs / 1_000_000_000) : "60",
  );
  /* null, not a Date: "no moment chosen yet" is a real state of this field and
     the server refuses a `once` schedule without one. A picker seeded with now
     would offer an instant that is already in the past by the time it is
     submitted. */
  const [runAt, setRunAt] = useState<Date | null>(initial?.runAt ? new Date(initial.runAt) : null);
  const [enabled, setEnabled] = useState(initial?.enabled ?? true);
  /* The in-flight guard, not just a disabled look (QA round 5, finding #17):
     begin() is a REF write, so three clicks in one task produce one request.
     hooks/use-submit-guard.ts says why a useState flag cannot do this. */
  const { submitting, begin, end } = useSubmitGuard();
  const [errors, setErrors] = useState<FieldErrors<ScheduleField>>({});

  /* A server 422 describes the value that was SENT. The moment the reader
     changes that value the message is about something that no longer exists on
     screen, so it goes — along with any form-level detail, which was a verdict
     on the same draft. Editing is how someone answers an error; the answer must
     not be shouted down by the error it answers. */
  function edit<T>(field: ScheduleField, set: (v: T) => void): (v: T) => void {
    return (v) => {
      setErrors((prev) => {
        if (prev[field] === undefined && prev.form === undefined) return prev;
        const next = { ...prev };
        delete next[field];
        delete next.form;
        return next;
      });
      set(v);
    };
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setErrors({});

    const req: ScheduleRequest = { definitionId, kind, enabled };
    if (kind === "interval") {
      const seconds = Number(intervalSeconds);
      if (!Number.isFinite(seconds) || seconds <= 0) {
        setErrors({ intervalNs: t("schedules.form.error.interval") });
        return;
      }
      req.intervalNs = Math.round(seconds * 1_000_000_000);
    }
    if (kind === "once") {
      if (runAt === null) {
        setErrors({ runAt: t("schedules.form.error.runAt") });
        return;
      }
      /* The picker's disablePast only blocks past DAYS; on today it still hands
         back a time that has already gone by, and the server answers that with
         a 422. Saying so here costs no round trip — the 422 remains the net. */
      if (runAt.getTime() <= Date.now()) {
        setErrors({ runAt: t("schedules.form.error.runAtPast") });
        return;
      }
      req.runAt = runAt.toISOString();
    }

    if (!begin()) return;
    try {
      if (initial) await updateSchedule(initial.id, req);
      else await createSchedule(req);
      await qc.invalidateQueries({ queryKey: ["schedules"] });
      onDone();
    } catch (err) {
      setErrors(errorsFromProblem(err, SCHEDULE_FIELD_PHRASES, t("schedules.form.failed")));
      end();
    }
  }

  return (
    <Card asChild className="p-6">
      <form onSubmit={handleSubmit} className="flex flex-col gap-4">
        <h3 className="text-sm font-semibold">
          {initial
            ? t("schedules.form.edit", {
                cadence: cadence(initial, locale, t),
                name: definitions.find((d) => d.id === initial.definitionId)?.name ?? initial.definitionId,
              })
            : t("schedules.form.create")}
        </h3>
        <div className="grid gap-4 sm:grid-cols-2">
          {/* Which definition a schedule fires is not a cadence edit; changing
              it would silently move the row to another check. Editing keeps the
              picker visible (the reader needs to see WHOSE cadence this is) and
              locked, with the reason spelled out. */}
          <SelectField
            label={t("schedules.form.definition")}
            value={definitionId}
            onChange={edit("definitionId", setDefinitionId)}
            disabled={!!initial}
            hint={initial ? t("schedules.form.definitionFixed") : undefined}
            options={[
              { value: "", label: t("schedules.form.pickDefinition") },
              ...definitions.map((d) => ({ value: d.id, label: d.name })),
            ]}
            error={errors.definitionId}
          />
          <SelectField
            label={t("schedules.form.kind")}
            value={kind}
            onChange={edit("kind", setKind)}
            options={plainOptions(SCHEDULE_KINDS)}
            error={errors.kind}
          />
          {kind === "interval" ? (
            <TextField
              label={t("schedules.form.interval")}
              value={intervalSeconds}
              onChange={edit("intervalNs", setIntervalSeconds)}
              error={errors.intervalNs}
              placeholder="60"
            />
          ) : null}
          {kind === "once" ? (
            <div className="flex flex-col gap-1 text-[13px]">
              <span className="text-muted-foreground">{t("schedules.form.runAt")}</span>
              {/* The M5 DateTimePicker, not a raw <input type="datetime-local">
                  (QA round 3, finding #12) — the LAST one in web/src, and the
                  reason the whole console now asks for an instant exactly one
                  way. The native spinner is miserable to aim at a date weeks
                  out, which is precisely what a one-off schedule is for, and it
                  clipped to unusability inside a narrow column.

                  allowFuture, and this is the one field where that is not a
                  preference: httpapi refuses a `once` schedule whose runAt is
                  not in the FUTURE, so the past-clamp the picker defaults to
                  would make every legal value unreachable. It is the same
                  exception maintenance windows take, for the same reason —
                  this is a declaration, not a record.

                  disablePast is the OTHER half of that same rule, and it was
                  missing (QA round 5, finding #12): allowFuture only lifts the
                  ceiling, so the picker still offered ten years of past days,
                  every one of which the server answers with "kind once
                  requires a run at time in the future". A control must not
                  offer what the thing behind it refuses. Maintenance windows
                  do NOT set it — a window is often recorded after the fact —
                  and the Time Machine picker sets neither flag. */}
              <div className="flex items-center gap-1">
                <DateTimePicker
                  aria-label={t("schedules.form.runAt")}
                  aria-invalid={!!errors.runAt}
                  value={runAt}
                  label={runAt === null ? t("schedules.form.runAtNotSet") : undefined}
                  allowFuture
                  disablePast
                  onApply={edit("runAt", setRunAt)}
                />
                {/* A field the server requires is still one an operator must be
                    able to un-set while they change their mind about the kind. */}
                {runAt !== null ? (
                  <Button
                    type="button"
                    size="sm"
                    variant="ghost"
                    aria-label={t("schedules.form.runAtClearAria")}
                    onClick={() => edit("runAt", setRunAt)(null)}
                  >
                    {t("schedules.form.runAtClear")}
                  </Button>
                ) : null}
              </div>
              {errors.runAt ? (
                <span role="alert" className="text-xs leading-relaxed text-health-bad">
                  {errors.runAt}
                </span>
              ) : null}
            </div>
          ) : null}
        </div>

        <p className="max-w-prose text-xs leading-relaxed text-muted-foreground">
          {kind === "interval" ? t("schedules.form.hint.interval", { seconds: MIN_INTERVAL_SECONDS }) : null}
          {kind === "once" ? t("schedules.form.hint.once") : null}
          {kind === "continuous" ? t("schedules.form.hint.continuous") : null}
        </p>

        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
            className={CHECKBOX_CLASS}
          />
          {t("schedules.form.enabled")}
        </label>

        {errors.form ? (
          <p role="alert" className="text-sm text-health-bad">
            {errors.form}
          </p>
        ) : null}

        <div className="flex gap-2">
          <Button type="submit" loading={submitting} {...guard}>
            {initial ? t("schedules.form.save") : t("schedules.form.createButton")}
          </Button>
          <Button type="button" variant="outline" onClick={onDone}>
            {t("cancel")}
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
function ScheduleRowActions({
  schedule,
  label,
  onEdit,
}: {
  schedule: Schedule;
  /** Already carries the cadence — see rowLabel in SchedulesTab, finding 3. */
  label: string;
  onEdit: () => void;
}) {
  const t = useT(targetsDict);
  const qc = useQueryClient();
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's
     useWriteGuard (QA round 2, finding #18; extended here in round 3). Spread it
     onto the control, and compose any local condition AFTER the spread. */
  const guard = useWriteGuard();
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
      <span className="flex flex-wrap items-center gap-2">
        <Button
          size="sm"
          variant="outline"
          loading={busy}
          {...guard}
          aria-label={t("schedules.row.confirmDelete", { name: label })}
          onClick={() => run(() => deleteSchedule(schedule.id), t("schedules.row.deleteFailed"))}
        >
          <RowActionLabel text={t("schedules.row.confirmDelete", { name: label })} />
        </Button>
        <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>
          {t("cancel")}
        </Button>
      </span>
    );
  }
  return (
    <span className="flex flex-wrap items-center gap-2">
      {/* One key per direction rather than a translated verb glued to a name:
          «Включить X» and «Выключить X» are two whole sentences, and the
          English `{verb} {label}` shape cannot produce either without the
          interpolation being in the wrong half. */}
      <Button
        size="sm"
        variant="ghost"
        loading={busy}
        {...guard}
        onClick={() =>
          run(
            () => updateSchedule(schedule.id, scheduleRequestFrom(schedule, !schedule.enabled)),
            t("schedules.row.updateFailed"),
          )
        }
        aria-label={
          schedule.enabled
            ? t("schedules.row.disable", { name: label })
            : t("schedules.row.enable", { name: label })
        }
      >
        <RowActionLabel
          text={
            schedule.enabled
              ? t("schedules.row.disable", { name: label })
              : t("schedules.row.enable", { name: label })
          }
        />
      </Button>
      <Button
        size="sm"
        variant="ghost"
        loading={busy}
        {...guard}
        aria-label={t("schedules.row.edit", { name: label })}
        onClick={onEdit}
      >
        <RowActionLabel text={t("schedules.row.edit", { name: label })} />
      </Button>
      {/* loading={busy} for the same reason the toggle carries it: while one of
          this row's writes is in flight, none of the others may start. */}
      <Button
        size="sm"
        variant="ghost"
        loading={busy}
        {...guard}
        aria-label={t("schedules.row.delete", { name: label })}
        onClick={() => setConfirming(true)}
      >
        <RowActionLabel text={t("schedules.row.delete", { name: label })} />
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
  const t = useT(targetsDict);
  const { locale } = useLocale();
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's
     useWriteGuard (QA round 2, finding #18; extended here in round 3). Spread it
     onto the control, and compose any local condition AFTER the spread. */
  const guard = useWriteGuard();
  const [editing, setEditing] = useState<
    { mode: "none" } | { mode: "create" } | { mode: "edit"; schedule: Schedule }
  >({ mode: "none" });
  const query = useQuery({ queryKey: ["schedules"], queryFn: () => listSchedules(), enabled: canRead });
  // Named, not numbered: a schedule row that shows only a definition UUID
  // tells an operator nothing. Same ["definitions"] cache entry the
  // Definitions tab fills.
  const definitionsQuery = useQuery({ queryKey: ["definitions"], queryFn: () => listChecks(), enabled: canRead });
  const schedules = query.data?.schedules ?? [];
  const definitions = definitionsQuery.data?.definitions ?? [];
  /* The whole definition, not just its name: a schedule's row needs the
     definition's `enabled` flag too, because an enabled schedule under a
     DISABLED definition fires nothing at all (finding 25). */
  const defs = useMemo(() => {
    const map = new Map<string, CheckDefinition>();
    for (const d of definitionsQuery.data?.definitions ?? []) map.set(d.id, d);
    return map;
  }, [definitionsQuery.data]);

  if (!canRead) {
    return <PermissionCard permission="checks:read">{t("schedules.gate.read")}</PermissionCard>;
  }

  return (
    <div className="flex flex-col gap-4">
      {canWrite ? null : (
        <PermissionCard permission="schedules:write">{t("schedules.gate.write")}</PermissionCard>
      )}

      {canWrite && editing.mode === "none" ? (
        <div>
          <Button size="sm" {...guard} onClick={() => setEditing({ mode: "create" })}>
            {t("schedules.new")}
          </Button>
        </div>
      ) : null}
      {canWrite && editing.mode !== "none" ? (
        <ScheduleForm
          key={editing.mode === "edit" ? editing.schedule.id : "create"}
          initial={editing.mode === "edit" ? editing.schedule : undefined}
          definitions={definitions}
          onDone={() => setEditing({ mode: "none" })}
        />
      ) : null}

      <Card asChild className="p-6">
        <section>
          <h2 className="text-sm font-semibold">{t("schedules.heading")}</h2>
          {query.isError ? (
            <p role="alert" className="mt-3 text-sm text-health-bad">
              {queryErrorMessage(query.error, t("schedules.unavailable"))}
            </p>
          ) : null}
          {query.isLoading ? <ListSkeleton /> : null}
          {!query.isLoading && schedules.length === 0 && !query.isError ? (
            <EmptyRow>{t("schedules.empty")}</EmptyRow>
          ) : null}
          {schedules.length > 0 ? (
            <ul aria-label={t("schedules.listAria")} className="mt-4 divide-y divide-border">
              {schedules.map((s) => {
                const def = defs.get(s.definitionId);
                const label = def?.name ?? s.definitionId;
                const cadenceText = cadence(s, locale, t);
                /* Two schedules of one definition used to produce two IDENTICAL
                   action names ("Delete gw-tcp"), which is unusable by voice or
                   by screen reader. The cadence is what actually tells them
                   apart, so it rides in the accessible name (finding 3). */
                const rowLabel = t("schedules.rowAria", { name: label, cadence: cadenceText });
                /* A schedule ALWAYS advances its cadence, fired or not — so
                   before finding #5 a schedule whose definition pointed at a
                   deleted target looked exactly like a healthy one: enabled, a
                   fresh "last", a "next" a minute out. The pill carries the
                   state and the line carries the reason. */
                const paused = s.enabled && def !== undefined && !def.enabled;
                /* The failure is shown whether or not the schedule is switched
                   on: a run that failed, failed. Switching the cadence off
                   afterwards does not unmake it, and hiding the reason is how
                   an operator loses the only record of why. */
                const failing = s.lastError !== "";
                return (
                  <li key={s.id} className="flex flex-wrap items-center gap-3 py-3 text-sm">
                    <span className="font-medium">{label}</span>
                    {/* s.kind is the stored value (once/interval/continuous)
                        and stays; the cadence next to it is the sentence this
                        page builds out of it, so that one translates. */}
                    <Badge variant="neutral">{s.kind}</Badge>
                    <span className="text-xs text-muted-foreground">{cadenceText}</span>
                    {/* Paused is its own state, not a shade of "enabled": the
                        row IS on, and it still fires nothing, because the
                        definition behind it is off. Saying "enabled" here was
                        the console contradicting what the scheduler does. */}
                    <Badge
                      variant={paused ? "unknown" : !s.enabled ? "neutral" : failing ? "warn" : "ok"}
                      dot
                      title={paused ? t("schedules.paused.title", { name: label }) : undefined}
                    >
                      {paused
                        ? t("schedules.paused")
                        : s.enabled
                          ? t("schedules.enabled")
                          : t("schedules.disabled")}
                    </Badge>
                    {/* nextFireAt is null for a continuous schedule (the loop
                        never fires one) and for a retired "once" — fmtTime
                        renders that as an em dash rather than inventing a
                        time. */}
                    <span className="ml-auto text-xs text-muted-foreground">
                      {t("schedules.row.next", { at: fmtTime(s.nextFireAt, locale) })}
                    </span>
                    <span className="text-xs text-muted-foreground">
                      {t("schedules.row.last", { at: fmtTime(s.lastFiredAt, locale) })}
                    </span>
                    {canWrite ? (
                      <ScheduleRowActions
                        schedule={s}
                        label={rowLabel}
                        onEdit={() => setEditing({ mode: "edit", schedule: s })}
                      />
                    ) : null}
                    {failing ? (
                      /* Full width (basis-full) under the row rather than
                         inline: the server's own message is a sentence, and
                         squeezing it between two pills would truncate the
                         actionable half. Verbatim — this console does not
                         paraphrase what the scheduler recorded. */
                      <p
                        data-testid="schedule-failure"
                        className="basis-full text-xs leading-relaxed text-health-bad"
                        title={s.lastErrorAt ? t("schedules.row.recorded", { at: fmtTime(s.lastErrorAt, locale) }) : undefined}
                      >
                        {t("schedules.row.failing", { message: s.lastError })}
                      </p>
                    ) : null}
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
  const t = useT(targetsDict);
  const { me, can } = useAuth();
  const { available: dbAvailable, resolved: dbResolved } = useDatabaseAvailable();
  const [tab, setTab] = useTabParam();

  const authResolved = me !== undefined;

  let body: ReactNode;
  if (!authResolved || !dbResolved) {
    body = (
      <Card role="status" aria-live="polite" className="p-6">
        <span className="sr-only">{t("loading")}</span>
        <Skeleton className="h-10 w-full" />
      </Card>
    );
  } else if (!can("targets:read")) {
    body = <PermissionCard permission="targets:read">{t("gate.read")}</PermissionCard>;
  } else if (!dbAvailable) {
    body = (
      <Card role="status" className="p-6">
        {/* console.database.mode is a config key and stays one. */}
        <p className="text-sm">{t("gate.noDatabase")}</p>
      </Card>
    );
  } else {
    body = (
      <>
        <Segmented
          aria-label={t("tabs.aria")}
          options={TABS.map((tb) => ({ value: tb.value, label: t(tb.labelKey) }))}
          value={tab}
          onChange={setTab}
        />
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
    /* The title is the SAME words the sidebar's nav.targets uses — one surface,
       one name, wherever the operator reads it. */
    <PageShell title={t("title")} description={t("description")}>
      {body}
    </PageShell>
  );
}
