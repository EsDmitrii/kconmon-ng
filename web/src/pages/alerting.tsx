import { useEffect, useId, useMemo, useRef, useState, type FormEvent, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { PageShell } from "@/components/page-shell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { useAuth } from "@/hooks/use-auth";
import {
  ApiError,
  createAlertRule,
  deleteAlertRule,
  importForeignAlertRules,
  listAlertRules,
  listForeignAlertRules,
  previewAlertRule,
  syncAlertRules,
  updateAlertRule,
} from "@/lib/api";
// Read in each mutating component rather than threaded down as a prop, the same
// way pages/settings.tsx and pages/targets.tsx do it: a permission decides
// whether a control EXISTS, this decides whether it is usable right now.
import { useWritesDisabled } from "@/lib/timemachine";
import type {
  AlertRule,
  AlertRuleImportReport,
  AlertRuleKind,
  AlertRulePreview,
  AlertRuleRequest,
  AlertSeverity,
  AlertSyncStatus,
  ForeignRule,
} from "@/lib/types";
import { cn } from "@/lib/utils";

/**
 * The Alerting page (M7 Task 7, plan Decisions 2/4/5/6).
 *
 * Prometheus evaluates, the console MANAGES. Nothing on this page evaluates
 * anything: a rule is builder fields in PostgreSQL, the server renders them
 * into PromQL, and a reconciler server-side-applies one PrometheusRule bundle
 * holding every enabled rule. What the page shows is that pipeline's own
 * account of itself.
 *
 * THREE SECTIONS, and the split between them is a dependency split, not a
 * layout choice:
 *
 *   1. Rules — needs the DATABASE. Lists, creates, edits and deletes work on a
 *      console with alerting switched off; the rules simply sit in PostgreSQL
 *      with nothing applying them.
 *   2. Builder — the same database, plus (for the live preview only) whatever
 *      Prometheus the console proxies.
 *   3. Foreign rules — needs the CLUSTER, i.e. the reconciler. With alerting
 *      off this section is the only one that stops working.
 *
 * That is why the two failures render as two different section states rather
 * than one page-level banner: the API answers 503 for "no database" and 409 for
 * "sync is disabled", and it is the ONE place in this API where those two come
 * apart (httpapi/alertrules.go's alertingDisabledDetail says so at length). A
 * page that collapsed them would send an operator looking at their database for
 * a reconciler nobody asked to start.
 *
 * WHAT THE PAGE IS NOT ALLOWED TO CLAIM. Three facts belong to the server and
 * are rendered as the server states them, never improved on:
 *
 *   - A sync kick answers 202. It means the work was REQUESTED. The outcome
 *     lands on the rules as syncStatus/syncMessage/lastSyncedAt and is read
 *     back on the next list, so the ack says "requested" and stops there.
 *   - A preview's two halves fail independently. `series` is an ANSWER only
 *     when `error` is absent; with an error, 0 series means nobody counted.
 *   - An import report's created/skipped/notes are three different statements
 *     and all three are rendered. Collapsing them into a toast would make an
 *     operator re-check rules that adopted perfectly.
 *
 * DEGRADED MODE. Neither section pre-checks `database.configured` the way
 * pages/targets.tsx does — pages/settings.tsx's newer line: the 503 the routes
 * answer with names console.database.mode in its own detail, in better words
 * than a second copy here would, and rendering the server's sentence verbatim
 * keeps one authority on what is missing instead of two that can drift.
 */

/* ── shared bits ────────────────────────────────────────────────────────── */

function queryErrorMessage(error: unknown, fallback: string): string {
  return error instanceof ApiError ? (error.problem.detail ?? error.problem.title) : fallback;
}

function problemStatus(error: unknown): number | undefined {
  return error instanceof ApiError ? error.problem.status : undefined;
}

function fieldClasses(invalid: boolean): string {
  return cn(
    "h-9 rounded-md border bg-transparent px-3 text-[13px]",
    invalid ? "border-health-bad" : "border-border-strong",
  );
}

function SectionCard({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Card asChild className="p-6">
      <section>
        <h2 className="text-sm font-semibold">{title}</h2>
        {children}
      </section>
    </Card>
  );
}

function ErrorLine({ testId, children }: { testId?: string; children: ReactNode }) {
  return (
    <p role="alert" data-testid={testId} className="mt-3 text-sm leading-relaxed text-health-bad">
      {children}
    </p>
  );
}

function PermissionCard({ permission, children }: { permission: string; children: ReactNode }) {
  return (
    <Card role="status" className="p-6">
      <p className="text-sm font-medium">Requires the {permission} permission</p>
      <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{children}</p>
    </Card>
  );
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

/* ── durations ──────────────────────────────────────────────────────────── */

/**
 * DURATION_UNITS is Prometheus's duration grammar in the DESCENDING order the
 * grammar itself requires, mirroring internal/console/alerting/render.go's
 * promDurationUnit table. The order is load-bearing twice: it is the legality
 * check for a composite string (a unit may only be followed by a smaller one),
 * and it is why "ms" can sit next to "m" without ambiguity — the scanner reads
 * a whole letter run, so "500ms" yields the unit "ms" and never "m" plus a
 * stray "s".
 *
 * y and w are 365d and 7d, Prometheus's own definitions. They are accepted on
 * the way IN and never emitted on the way out (see formatPromDuration).
 */
const DURATION_UNITS: readonly (readonly [unit: string, ns: number])[] = [
  ["y", 365 * 24 * 3600 * 1e9],
  ["w", 7 * 24 * 3600 * 1e9],
  ["d", 24 * 3600 * 1e9],
  ["h", 3600 * 1e9],
  ["m", 60 * 1e9],
  ["s", 1e9],
  ["ms", 1e6],
];

export type DurationParse = { ok: true; ns: number } | { ok: false; message: string };

/**
 * parsePromDuration reads what an operator typed into the `for` box.
 *
 * ONE input, not a value box plus a unit select. A `for` is a Prometheus
 * duration and operators already write it as one — "30s", "5m", "2h" is the
 * vocabulary of every alerting rule anybody has ever read — so a text box that
 * accepts exactly that string is the control that needs no translation in
 * either direction. A value+unit pair would also have to decide what "90" plus
 * "seconds" renders as when the stored value is 90s (a unit select cannot show
 * "1m30s"), and that is a rounding decision this page has no business making.
 *
 * An EMPTY box is 0, not an error: the API's forNs is optional and omitting it
 * means "fire as soon as the expression holds", which is a real and common
 * choice. A bare number IS an error — "30" could mean seconds or minutes, and
 * guessing which is exactly the bug this box exists to prevent.
 */
export function parsePromDuration(text: string): DurationParse {
  const s = text.trim();
  if (s === "") return { ok: true, ns: 0 };

  let i = 0;
  let ns = 0;
  let lastUnit = -1;
  while (i < s.length) {
    const digitsAt = i;
    while (i < s.length && s[i] >= "0" && s[i] <= "9") i++;
    if (i === digitsAt) {
      return { ok: false, message: `"${s}" is not a duration: write a number and a unit, like 30s, 5m or 2h` };
    }
    const digits = s.slice(digitsAt, i);

    const lettersAt = i;
    while (i < s.length && s[i] >= "a" && s[i] <= "z") i++;
    const unit = s.slice(lettersAt, i);
    if (unit === "") {
      return { ok: false, message: `"${s}" has no unit: write ${digits}s, ${digits}m or ${digits}h` };
    }
    const at = DURATION_UNITS.findIndex(([u]) => u === unit);
    if (at === -1) {
      return { ok: false, message: `"${unit}" is not a Prometheus duration unit (ms, s, m, h, d, w, y)` };
    }
    if (at <= lastUnit) {
      return { ok: false, message: `"${s}" must run from the largest unit to the smallest, like 1h30m` };
    }
    lastUnit = at;
    ns += Number(digits) * DURATION_UNITS[at][1];
  }
  return { ok: true, ns };
}

/**
 * formatPromDuration is the inverse, and it mirrors render.go's own
 * FormatPromDuration byte for byte: the largest unit that divides EVENLY, never
 * a compound. 300s renders "5m" and 90s renders "90s" rather than "1m30s",
 * because a single-unit string is what somebody typed into this box in the
 * first place and a compound would make two equal durations render differently
 * depending on which unit the UI happened to pick.
 *
 * Units stop at days on the way out: "1w" reads as a different setting than the
 * 7d that was entered. A value that is not a whole number of milliseconds can
 * only reach the browser from a row this page did not write, so it is printed
 * in nanoseconds rather than rounded into a lie.
 */
export function formatPromDuration(ns: number): string {
  if (ns === 0) return "0s";
  if (ns < 0 || !Number.isFinite(ns)) return `${ns}ns`;
  for (const unit of ["d", "h", "m", "s", "ms"]) {
    const size = DURATION_UNITS.find(([u]) => u === unit)?.[1] ?? 0;
    if (size > 0 && ns % size === 0) return `${ns / size}${unit}`;
  }
  return `${ns}ns`;
}

/** relativeTime renders an instant as an age. "—" for an absent one: a rule
 *  that has never been applied has no lastSyncedAt, and inventing "never
 *  synced, probably" out of an absent field is the page speaking for the
 *  reconciler. The absolute instant stays available as a title. */
export function relativeTime(iso: string | undefined, now: Date): string {
  if (!iso) return "—";
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return iso;
  const seconds = Math.floor((now.getTime() - t) / 1000);
  if (seconds < 0) return "just now";
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

function absoluteTime(iso?: string): string | undefined {
  if (!iso) return undefined;
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}

/* ── the closed schemas, mirrored ───────────────────────────────────────── */

/**
 * RESERVED_LABEL_NAMES are the two label names the renderer stamps on every
 * rule entry itself (alerting/render.go's SeverityLabel and RuleIDLabel), so a
 * user label of either name is an ERROR and never a silent override.
 *
 * This is the ONE server rule mirrored client-side on this page, and it earns
 * that place the way settings.tsx's blank-secret check does: everything else —
 * the name charset, the param ranges, the label-name grammar — is left to the
 * server's 422, whose wording is better than a second copy would be. This one
 * is mirrored because it is the only case where a form the operator can see
 * being filled in is guaranteed to be refused, and the refusal is about a field
 * they can fix in place.
 */
export const RESERVED_LABEL_NAMES: readonly string[] = ["severity", "kconmon_ng_rule_id"];

/** reservedLabelMessage returns the SERVER'S own sentence for a reserved label
 *  name (render.go: `label %q is reserved by the console`), minus the rule-name
 *  prefix the server adds and the browser does not have yet. */
export function reservedLabelMessage(key: string): string | undefined {
  return RESERVED_LABEL_NAMES.includes(key) ? `label "${key}" is reserved by the console` : undefined;
}

export interface ParamField {
  /** The WIRE key. A dot means the param is nested (pair-loss's `scope`). */
  key: string;
  label: string;
  type: "number" | "text" | "expr" | "enum";
  required: boolean;
  /** Set for `enum`; values are the wire values. */
  options?: readonly string[];
  /** Set on an enum whose wire value is a NUMBER (zone-latency's quantile). */
  numeric?: boolean;
  hint?: string;
}

/**
 * KIND_PARAMS mirrors internal/console/alerting/render.go's `kindSchemas`
 * table — the CLOSED per-kind param schema — field for field, including which
 * ones are required.
 *
 * Mirrored rather than derived because there is nothing to derive from:
 * openapi-typescript emits AlertRule.params as an open `{[key: string]:
 * unknown}` (the OpenAPI schema cannot express "closed, but differently per
 * kind"), so the shape of the form is knowledge this file has to carry. Every
 * entry below cites the Go it came from:
 *
 *   pair-loss             protocol (tcp|udp|icmp, required), thresholdPercent
 *                         (required, 0-100 — percentParam), scope.sourceNode,
 *                         scope.destNode (scopeSchema, both optional strings)
 *   zone-latency          protocol (required), quantile (required, one of
 *                         0.5/0.95/0.99 — validQuantiles), thresholdMs
 *                         (required, > 0 — positiveParam), sourceZone, destZone
 *   dns-failures          thresholdPercent (required, 0-100)
 *   http-ttfb             thresholdMs (required, > 0), url. The quantile is
 *                         FIXED at 0.95 (TTFBQuantile) and is not a param.
 *   agent-missing         none. `for` lives on the rule, so a forMinutes param
 *                         here would be a second place meaning the same thing.
 *   external-target-down  targetName
 *   raw                   expr (required)
 *
 * cert-expiry is absent on purpose and is absent from the API enum too: the
 * alert_rules.kind CHECK constraint accepts it, no template renders it (no
 * certificate-expiry metric family exists in this codebase), and a write is
 * 422ed. A form field for it would offer an alert that can never fire.
 *
 * The units are the OPERATOR's, not the metric's: loss thresholds are PERCENT
 * against ratio gauges and latency thresholds are MILLISECONDS against second
 * histograms, and render.go does both conversions at render time so the stored
 * params stay in the units that were typed.
 */
export const KIND_PARAMS: Record<AlertRuleKind, readonly ParamField[]> = {
  "pair-loss": [
    { key: "protocol", label: "Protocol", type: "enum", required: true, options: ["tcp", "udp", "icmp"] },
    {
      key: "thresholdPercent",
      label: "Loss threshold (%)",
      type: "number",
      required: true,
      hint: "0–100. The loss metrics are ratios; the renderer multiplies by 100, so this is the percentage a chart shows.",
    },
    { key: "scope.sourceNode", label: "Source node", type: "text", required: false, hint: "Optional. Blank means every source." },
    { key: "scope.destNode", label: "Destination node", type: "text", required: false, hint: "Optional. Blank means every destination." },
  ],
  "zone-latency": [
    { key: "protocol", label: "Protocol", type: "enum", required: true, options: ["tcp", "udp", "icmp"] },
    {
      key: "quantile",
      label: "Quantile",
      type: "enum",
      required: true,
      numeric: true,
      options: ["0.5", "0.95", "0.99"],
      hint: "The three the renderer accepts.",
    },
    {
      key: "thresholdMs",
      label: "Latency threshold (ms)",
      type: "number",
      required: true,
      hint: "Greater than 0. The histograms are in seconds; the renderer converts.",
    },
    { key: "sourceZone", label: "Source zone", type: "text", required: false, hint: "Optional." },
    { key: "destZone", label: "Destination zone", type: "text", required: false, hint: "Optional." },
  ],
  "dns-failures": [
    {
      key: "thresholdPercent",
      label: "Failure threshold (%)",
      type: "number",
      required: true,
      hint: "0–100, as a share of the DNS results counter.",
    },
  ],
  "http-ttfb": [
    {
      key: "thresholdMs",
      label: "TTFB threshold (ms)",
      type: "number",
      required: true,
      hint: "Greater than 0. The quantile is fixed at 0.95.",
    },
    { key: "url", label: "URL", type: "text", required: false, hint: "Optional. Blank means every probed URL." },
  ],
  "agent-missing": [],
  "external-target-down": [
    { key: "targetName", label: "Target name", type: "text", required: false, hint: "Optional. Blank means every external target." },
  ],
  raw: [
    {
      key: "expr",
      label: "PromQL expression",
      type: "expr",
      required: true,
      hint: "Stored verbatim. Validity is what the preview below reports — this console ships no Prometheus parser.",
    },
  ],
};

/** ALERT_RULE_KINDS is the select's order and the one-line blurb next to each
 *  option. Templates first, `raw` last: raw is the escape hatch, and a builder
 *  that offers it first teaches operators to skip the templates that carry the
 *  metric names for them. */
export const ALERT_RULE_KINDS: readonly (readonly [AlertRuleKind, string])[] = [
  ["pair-loss", "packet loss between nodes"],
  ["zone-latency", "cross-zone latency quantile"],
  ["dns-failures", "DNS failure share"],
  ["http-ttfb", "HTTP time-to-first-byte"],
  ["agent-missing", "registered agents below expected"],
  ["external-target-down", "external target failing"],
  ["raw", "hand-written PromQL"],
];

const SEVERITIES: readonly AlertSeverity[] = ["info", "warning", "critical"];

/**
 * problemField routes a 422's detail to the form field it is about, so the
 * message lands where the value is instead of at the bottom of a form.
 *
 * The renderer's errors already NAME the param that caused them
 * (`pair-loss: param "thresholdPercent" is required`), which is the whole
 * reason they are surfaced verbatim — the caller typed that param a second
 * ago — so matching the quoted name is enough and no message is rewritten.
 * Anything this cannot place returns undefined and the caller banners it,
 * which is the honest failure direction: a message shown in the wrong place is
 * worse than one shown at the top.
 */
export function problemField(detail: string): string | undefined {
  const param = /param "([^"]+)"/.exec(detail);
  if (param) return param[1];
  if (/^alert rule: name\b/.test(detail)) return "name";
  if (/^alert rule: severity\b/.test(detail)) return "severity";
  if (/^alert rule: for\b/.test(detail)) return "for";
  return undefined;
}

/* ── rule ⇄ request ─────────────────────────────────────────────────────── */

/**
 * alertRuleRequestFrom turns a STORED rule back into a write body.
 *
 * It exists because PUT /api/v1/alert-rules/{id} is a FULL REPLACE with no
 * PATCH counterpart, so the row toggle — which changes exactly one boolean —
 * still has to send everything. Sending everything by hand at each call site is
 * how a field goes missing, and one field going missing here is not cosmetic:
 * `enabled` OMITTED means TRUE on the wire, so a partial body would silently
 * re-enable a rule somebody deliberately turned off. This function writes it
 * explicitly, always.
 *
 * renderedExpr and the three sync fields are deliberately absent: the first is
 * the server's (it re-renders from these params on every write) and the others
 * are the reconciler's.
 */
export function alertRuleRequestFrom(rule: AlertRule, over: Partial<AlertRuleRequest> = {}): AlertRuleRequest {
  return {
    name: rule.name,
    kind: rule.kind,
    params: rule.params,
    severity: rule.severity,
    forNs: rule.forNs,
    labels: rule.labels,
    annotations: rule.annotations,
    enabled: rule.enabled,
    ...over,
  };
}

interface Pair {
  key: string;
  value: string;
}

export interface RuleDraft {
  name: string;
  kind: AlertRuleKind;
  /** FLAT and string-valued, keyed by ParamField.key — a form holds strings.
   *  paramsFromDraft is the one place they become typed and nested. */
  params: Record<string, string>;
  severity: AlertSeverity;
  forText: string;
  labels: Pair[];
  annotations: Pair[];
  enabled: boolean;
}

function readParam(params: Record<string, unknown>, key: string): unknown {
  let cursor: unknown = params;
  for (const part of key.split(".")) {
    if (cursor === null || typeof cursor !== "object") return undefined;
    cursor = (cursor as Record<string, unknown>)[part];
  }
  return cursor;
}

function pairsFrom(map: Record<string, string>): Pair[] {
  return Object.entries(map).map(([key, value]) => ({ key, value }));
}

function mapFrom(pairs: Pair[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const pair of pairs) {
    const key = pair.key.trim();
    if (key !== "") out[key] = pair.value;
  }
  return out;
}

/**
 * paramFieldsFor is the ONE way this file reads KIND_PARAMS, because the lookup
 * can miss. AlertRuleKind is the set the API ACCEPTS, while the alert_rules
 * kind CHECK constraint additionally allows `cert-expiry` — a row that predates
 * this build, or one written by a console that had a template for it, arrives
 * as a kind with no entry here. Listing such a row is fine (the list never
 * looks at the schema); opening it in the builder must not take the page down
 * with a TypeError, so it opens with no param fields and the server's 422 is
 * what explains the refusal.
 */
function paramFieldsFor(kind: AlertRuleKind): readonly ParamField[] {
  return KIND_PARAMS[kind] ?? [];
}

function draftFrom(rule?: AlertRule): RuleDraft {
  const kind: AlertRuleKind = rule?.kind ?? "pair-loss";
  const params: Record<string, string> = {};
  for (const field of paramFieldsFor(kind)) {
    const value = rule ? readParam(rule.params, field.key) : undefined;
    params[field.key] = value === undefined || value === null ? "" : String(value);
  }
  return {
    name: rule?.name ?? "",
    kind,
    params,
    severity: rule?.severity ?? "warning",
    forText: rule ? formatPromDuration(rule.forNs) : "",
    labels: pairsFrom(rule?.labels ?? {}),
    annotations: pairsFrom(rule?.annotations ?? {}),
    enabled: rule?.enabled ?? true,
  };
}

/**
 * paramsFromDraft types and nests the form's strings into the params object.
 *
 * An empty optional field is OMITTED rather than sent as "": params are CLOSED
 * per kind and the renderer rejects an empty string param by name
 * (`must not be empty`), so a blank box has to mean "not set" — which is what
 * it looks like to the person who left it blank.
 *
 * A number that will not parse is sent as the STRING the operator typed, on
 * purpose: the server then answers `param "x" must be a number` naming the
 * field, which is a better message than anything this function could invent,
 * and NaN would go on the wire as null and be reported as a missing param
 * instead of a malformed one.
 */
export function paramsFromDraft(kind: AlertRuleKind, params: Record<string, string>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const field of paramFieldsFor(kind)) {
    const raw = (params[field.key] ?? "").trim();
    if (raw === "") continue;
    const numeric = field.type === "number" || field.numeric === true;
    const parsed = Number(raw);
    const value: unknown = numeric && Number.isFinite(parsed) ? parsed : raw;

    const parts = field.key.split(".");
    if (parts.length === 1) {
      out[field.key] = value;
      continue;
    }
    const [head, tail] = parts;
    const nested = (out[head] as Record<string, unknown> | undefined) ?? {};
    nested[tail] = value;
    out[head] = nested;
  }
  return out;
}

function requestFromDraft(draft: RuleDraft, forNs: number): AlertRuleRequest {
  return {
    name: draft.name,
    kind: draft.kind,
    params: paramsFromDraft(draft.kind, draft.params),
    severity: draft.severity,
    forNs,
    labels: mapFrom(draft.labels),
    annotations: mapFrom(draft.annotations),
    enabled: draft.enabled,
  };
}

/* ── the rule list ──────────────────────────────────────────────────────── */

const SYNC_TONE: Record<AlertSyncStatus, "ok" | "warn" | "bad" | "unknown" | "neutral"> = {
  // synced is the only green. drift is amber and PAST TENSE: a reconcile always
  // re-asserts the console's bytes, so drift means "the cluster had diverged as
  // of lastSyncedAt and we corrected it", never "it is diverged right now".
  synced: "ok",
  drift: "warn",
  error: "bad",
  // unsynced is the state of every freshly created or freshly edited rule and
  // is NOT an error, so it gets the neutral pill rather than the unknown one.
  unsynced: "neutral",
};

const SEVERITY_TONE: Record<AlertSeverity, "neutral" | "warn" | "bad"> = {
  info: "neutral",
  warning: "warn",
  critical: "bad",
};

function RuleRow({
  rule,
  canManage,
  onEdit,
  onSyncConflict,
}: {
  rule: AlertRule;
  canManage: boolean;
  onEdit: () => void;
  onSyncConflict: (detail: string) => void;
}) {
  const qc = useQueryClient();
  const writesDisabled = useWritesDisabled();
  const detailsId = useId();
  const [expanded, setExpanded] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [kicked, setKicked] = useState(false);
  const [error, setError] = useState<string>();

  async function handleToggle() {
    setBusy(true);
    setError(undefined);
    try {
      await updateAlertRule(rule.id, alertRuleRequestFrom(rule, { enabled: !rule.enabled }));
      await qc.invalidateQueries({ queryKey: ["alert-rules"] });
    } catch (err) {
      setError(queryErrorMessage(err, "Failed to save the rule"));
    }
    setBusy(false);
  }

  async function handleDelete() {
    setBusy(true);
    setError(undefined);
    try {
      await deleteAlertRule(rule.id);
      await qc.invalidateQueries({ queryKey: ["alert-rules"] });
    } catch (err) {
      setError(queryErrorMessage(err, "Failed to delete the rule"));
      setBusy(false);
      setConfirming(false);
    }
  }

  async function handleSync() {
    setBusy(true);
    setError(undefined);
    setKicked(false);
    try {
      await syncAlertRules(rule.id);
    } catch (err) {
      // 409 is not this row's failure — it is the whole console's alerting
      // switch, and it says so in a paragraph. It goes to the section banner so
      // it is stated ONCE, however many rows the operator clicks.
      if (problemStatus(err) === 409) onSyncConflict(queryErrorMessage(err, "Prometheus rule sync is disabled"));
      else setError(queryErrorMessage(err, "Failed to request a reconcile"));
      setBusy(false);
      return;
    }
    setBusy(false);
    setKicked(true);
  }

  return (
    <li className="flex flex-wrap items-center gap-3 py-3 text-sm">
      <span className="font-medium">{rule.name}</span>
      <Badge variant="neutral">{rule.kind}</Badge>
      <Badge variant={SEVERITY_TONE[rule.severity]}>{rule.severity}</Badge>
      {/* The reconciler's one-liner rides the chip as a title so it is reachable
          without expanding, and is repeated in full in the details panel: a
          title alone is invisible to touch and to anyone not hovering. */}
      <span data-testid="sync-status" title={rule.syncMessage === "" ? undefined : rule.syncMessage}>
        <Badge variant={SYNC_TONE[rule.syncStatus]}>{rule.syncStatus}</Badge>
      </span>
      <span data-testid="last-synced" title={absoluteTime(rule.lastSyncedAt)} className="text-xs text-muted-foreground">
        {relativeTime(rule.lastSyncedAt, new Date())}
      </span>
      {canManage ? (
        <input
          type="checkbox"
          aria-label={`Enabled ${rule.name}`}
          checked={rule.enabled}
          disabled={writesDisabled || busy}
          onChange={() => void handleToggle()}
        />
      ) : (
        <Badge variant={rule.enabled ? "ok" : "unknown"}>{rule.enabled ? "enabled" : "disabled"}</Badge>
      )}

      <span className="ml-auto flex flex-wrap items-center gap-2">
        {/* aria-controls, not aria-expanded alone: components/mtr-hop-table.tsx
            set this shape's bar (a row that expands into a detail block names
            the block it expands), and "expanded" with nothing named leaves a
            screen-reader user hunting the page for what just appeared. */}
        <Button
          size="sm"
          variant="ghost"
          aria-expanded={expanded}
          aria-controls={detailsId}
          onClick={() => setExpanded((v) => !v)}
        >
          Details for {rule.name}
        </Button>
        {canManage ? (
          confirming ? (
            <>
              <Button size="sm" variant="outline" loading={busy} disabled={writesDisabled} onClick={() => void handleDelete()}>
                Confirm delete {rule.name}
              </Button>
              <Button size="sm" variant="ghost" onClick={() => setConfirming(false)}>
                Cancel
              </Button>
            </>
          ) : (
            <>
              <Button size="sm" variant="ghost" disabled={writesDisabled || busy} onClick={() => void handleSync()}>
                Sync {rule.name} now
              </Button>
              <Button size="sm" variant="ghost" disabled={writesDisabled} onClick={onEdit}>
                Edit {rule.name}
              </Button>
              <Button size="sm" variant="ghost" disabled={writesDisabled} onClick={() => setConfirming(true)}>
                Delete {rule.name}
              </Button>
            </>
          )
        ) : null}
      </span>

      {expanded ? (
        <div id={detailsId} className="w-full border-l-2 border-border pl-3 text-xs">
          <p className="text-muted-foreground">Rendered expression</p>
          {/* The SERVER's bytes, not a re-render: renderedExpr is on the row so
              the expression an operator reads is the one the bundle carries. */}
          <code className="mt-1 block break-all whitespace-pre-wrap font-mono text-[12px]">{rule.renderedExpr}</code>
          {rule.syncMessage === "" ? null : (
            <p className="mt-2 leading-relaxed text-muted-foreground">{rule.syncMessage}</p>
          )}
          <p className="mt-2 text-muted-foreground">
            for {formatPromDuration(rule.forNs)} · last applied {absoluteTime(rule.lastSyncedAt) ?? "never"}
          </p>
        </div>
      ) : null}

      {kicked ? (
        <span role="status" data-testid="sync-ack" className="w-full text-xs text-muted-foreground">
          Reconcile requested. The outcome lands on this row as its sync status — it is not known yet.
        </span>
      ) : null}
      {error ? (
        <span role="alert" className="w-full text-xs leading-relaxed text-health-bad">
          {error}
        </span>
      ) : null}
    </li>
  );
}

/* ── the builder ────────────────────────────────────────────────────────── */

/**
 * PREVIEW_DEBOUNCE_MS is how long the builder waits after the last edit before
 * asking the server to render and evaluate the draft.
 *
 * The preview is a POST that runs an instant query against Prometheus, so
 * firing one per keystroke would put a real query on the cluster for every
 * digit of a threshold. Kept well under vitest's waitFor budget so the tests
 * exercise the real timer instead of a fake clock — settings.tsx's
 * TEST_REFETCH_DELAY_MS reasoning.
 */
export const PREVIEW_DEBOUNCE_MS = 300;

function Field({
  label,
  hint,
  error,
  testId,
  children,
}: {
  label: string;
  hint?: string;
  error?: string;
  testId: string;
  children: (id: string, invalid: boolean) => ReactNode;
}) {
  const id = useId();
  return (
    <div className="flex flex-col gap-1 text-[13px]">
      <label htmlFor={id} className="text-muted-foreground">
        {label}
      </label>
      {children(id, error !== undefined)}
      {hint ? <span className="text-xs leading-relaxed text-muted-foreground">{hint}</span> : null}
      {error ? (
        <p role="alert" data-testid={`field-error-${testId}`} className="text-xs leading-relaxed text-health-bad">
          {error}
        </p>
      ) : null}
    </div>
  );
}

function PairEditor({
  legend,
  noun,
  pairs,
  disabled,
  onChange,
}: {
  legend: string;
  /** "Label" or "Annotation" — the singular that names each row's two boxes. */
  noun: string;
  pairs: Pair[];
  disabled: boolean;
  onChange: (pairs: Pair[]) => void;
}) {
  return (
    <fieldset className="flex flex-col gap-2 text-[13px]">
      <legend className="text-muted-foreground">{legend}</legend>
      {pairs.map((pair, i) => (
        <div key={i} className="flex flex-wrap items-center gap-2">
          <input
            aria-label={`${noun} name ${i + 1}`}
            value={pair.key}
            onChange={(e) => onChange(pairs.map((p, j) => (i === j ? { ...p, key: e.target.value } : p)))}
            className={fieldClasses(reservedLabelMessage(pair.key.trim()) !== undefined)}
          />
          <input
            aria-label={`${noun} value ${i + 1}`}
            value={pair.value}
            onChange={(e) => onChange(pairs.map((p, j) => (i === j ? { ...p, value: e.target.value } : p)))}
            className={fieldClasses(false)}
          />
          <Button
            type="button"
            size="sm"
            variant="ghost"
            onClick={() => onChange(pairs.filter((_, j) => j !== i))}
          >
            Remove {noun.toLowerCase()} {i + 1}
          </Button>
        </div>
      ))}
      <div>
        <Button type="button" size="sm" variant="outline" disabled={disabled} onClick={() => onChange([...pairs, { key: "", value: "" }])}>
          Add {noun.toLowerCase()}
        </Button>
      </div>
    </fieldset>
  );
}

function PreviewPanel({
  ready,
  loading,
  preview,
  error,
}: {
  ready: boolean;
  loading: boolean;
  preview?: AlertRulePreview;
  error?: string;
}) {
  return (
    <div role="region" aria-label="Expression preview" aria-live="polite" className="rounded-md border border-border p-4">
      <p className="text-[13px] font-medium">Preview</p>
      {!ready ? (
        <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">
          Fill the required parameters to preview the expression. Nothing is asked of Prometheus until then.
        </p>
      ) : null}
      {ready && loading && !preview ? (
        <p className="mt-1 text-xs text-muted-foreground">Rendering…</p>
      ) : null}
      {error ? <ErrorLine>{error}</ErrorLine> : null}
      {preview ? (
        <>
          <code className="mt-2 block break-all whitespace-pre-wrap font-mono text-[12px]">{preview.expr}</code>
          {preview.error ? (
            <>
              {/* The two halves failed independently. The expression rendered —
                  that is a real result and it is shown — but nobody counted, so
                  `series` is not a number this may print. */}
              <p className="mt-2 text-xs leading-relaxed text-muted-foreground">
                The expression rendered. It could not be evaluated, so how many series it matches is unknown.
              </p>
              <p className="mt-1 text-xs leading-relaxed text-health-warn">{preview.error}</p>
            </>
          ) : (
            <p className="mt-2 text-xs text-muted-foreground">
              Matches {preview.series} series right now.
              {preview.series === 0 ? " That is the answer, not a failure: nothing matches at this instant." : ""}
            </p>
          )}
        </>
      ) : null}
    </div>
  );
}

function RuleForm({ initial, onDone }: { initial?: AlertRule; onDone: () => void }) {
  const qc = useQueryClient();
  const writesDisabled = useWritesDisabled();
  const [draft, setDraft] = useState<RuleDraft>(() => draftFrom(initial));
  const [submitting, setSubmitting] = useState(false);
  const [formError, setFormError] = useState<{ message: string; field?: string }>();
  const [preview, setPreview] = useState<AlertRulePreview>();
  const [previewError, setPreviewError] = useState<string>();
  const [previewLoading, setPreviewLoading] = useState(false);

  const fields = paramFieldsFor(draft.kind);

  // Live, not on submit: a reserved label is refused before a request is built,
  // so the message sits next to the box the whole time it is wrong.
  const reservedMessage = useMemo(
    () => draft.labels.map((p) => reservedLabelMessage(p.key.trim())).find((m) => m !== undefined),
    [draft.labels],
  );

  const duration = parsePromDuration(draft.forText);
  const previewReady = fields.every((f) => !f.required || (draft.params[f.key] ?? "").trim() !== "");

  // The preview body is what a SAVE would send, minus the two fields that
  // cannot change the expression: the name (which only seeds the alertname) and
  // the severity (a label). Keying the debounce on the rest means typing a name
  // does not put an instant query on the cluster.
  const previewBody: AlertRuleRequest = {
    name: draft.name,
    kind: draft.kind,
    params: paramsFromDraft(draft.kind, draft.params),
    severity: draft.severity,
    forNs: duration.ok ? duration.ns : 0,
    labels: mapFrom(draft.labels),
    annotations: mapFrom(draft.annotations),
    enabled: draft.enabled,
  };
  const previewKey = JSON.stringify({ kind: previewBody.kind, params: previewBody.params, labels: previewBody.labels });
  const bodyRef = useRef(previewBody);
  bodyRef.current = previewBody;

  useEffect(() => {
    if (!previewReady) {
      setPreview(undefined);
      setPreviewError(undefined);
      setPreviewLoading(false);
      return;
    }
    let cancelled = false;
    setPreviewLoading(true);
    const timer = setTimeout(() => {
      previewAlertRule(bodyRef.current)
        .then((p) => {
          if (cancelled) return;
          setPreview(p);
          setPreviewError(undefined);
        })
        .catch((err: unknown) => {
          if (cancelled) return;
          setPreview(undefined);
          setPreviewError(queryErrorMessage(err, "The preview could not be rendered"));
        })
        .finally(() => {
          if (!cancelled) setPreviewLoading(false);
        });
    }, PREVIEW_DEBOUNCE_MS);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [previewKey, previewReady]);

  /** The keys that have a place to render an error. Anything else banners. */
  const placeableFields = ["name", "severity", "for", ...fields.map((f) => f.key.split(".").pop() ?? f.key)];
  const errorFor = (key: string): string | undefined => {
    if (!formError?.field) return undefined;
    const tail = key.split(".").pop() ?? key;
    return formError.field === key || formError.field === tail ? formError.message : undefined;
  };
  const bannerError =
    formError && !(formError.field !== undefined && placeableFields.includes(formError.field))
      ? formError.message
      : undefined;

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setFormError(undefined);
    if (reservedMessage) return;
    if (!duration.ok) {
      setFormError({ message: duration.message, field: "for" });
      return;
    }
    setSubmitting(true);
    try {
      const req = requestFromDraft(draft, duration.ns);
      if (initial) await updateAlertRule(initial.id, req);
      else await createAlertRule(req);
      await qc.invalidateQueries({ queryKey: ["alert-rules"] });
      onDone();
    } catch (err) {
      const message = queryErrorMessage(err, "Failed to save the rule");
      setFormError({ message, field: problemField(message) });
      setSubmitting(false);
    }
  }

  return (
    <Card asChild className="p-6">
      <form onSubmit={handleSubmit} aria-label={initial ? `Edit ${initial.name}` : "New alert rule"} className="flex flex-col gap-4">
        <h3 className="text-sm font-semibold">{initial ? `Edit ${initial.name}` : "New rule"}</h3>

        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            label="Name"
            testId="name"
            error={errorFor("name")}
            hint="Seeds the alert's own name, so it becomes a Prometheus label value. CamelCase is the convention."
          >
            {(id, invalid) => (
              <input
                id={id}
                value={draft.name}
                placeholder="PairLossHigh"
                onChange={(e) => setDraft((d) => ({ ...d, name: e.target.value }))}
                className={fieldClasses(invalid)}
              />
            )}
          </Field>

          <Field label="Kind" testId="kind">
            {(id) => (
              <select
                id={id}
                value={draft.kind}
                onChange={(e) => setDraft((d) => ({ ...d, kind: e.target.value as AlertRuleKind }))}
                className={fieldClasses(false)}
              >
                {ALERT_RULE_KINDS.map(([kind, blurb]) => (
                  <option key={kind} value={kind}>
                    {kind} — {blurb}
                  </option>
                ))}
              </select>
            )}
          </Field>
        </div>

        <div className="grid gap-4 sm:grid-cols-2">
          {fields.map((field) => (
            <Field
              key={field.key}
              label={field.label}
              hint={field.hint}
              testId={field.key.split(".").pop() ?? field.key}
              error={errorFor(field.key)}
            >
              {(id, invalid) =>
                field.type === "enum" ? (
                  <select
                    id={id}
                    value={draft.params[field.key] ?? ""}
                    onChange={(e) => setDraft((d) => ({ ...d, params: { ...d.params, [field.key]: e.target.value } }))}
                    className={fieldClasses(invalid)}
                  >
                    <option value="">—</option>
                    {(field.options ?? []).map((option) => (
                      <option key={option} value={option}>
                        {option}
                      </option>
                    ))}
                  </select>
                ) : field.type === "expr" ? (
                  <textarea
                    id={id}
                    rows={3}
                    value={draft.params[field.key] ?? ""}
                    onChange={(e) => setDraft((d) => ({ ...d, params: { ...d.params, [field.key]: e.target.value } }))}
                    className={cn(
                      "rounded-md border bg-transparent p-3 font-mono text-[12px]",
                      invalid ? "border-health-bad" : "border-border-strong",
                    )}
                  />
                ) : (
                  <input
                    id={id}
                    type={field.type === "number" ? "number" : "text"}
                    value={draft.params[field.key] ?? ""}
                    onChange={(e) => setDraft((d) => ({ ...d, params: { ...d.params, [field.key]: e.target.value } }))}
                    className={fieldClasses(invalid)}
                  />
                )
              }
            </Field>
          ))}
          {fields.length === 0 && KIND_PARAMS[draft.kind] !== undefined ? (
            <p data-testid="no-params" className="max-w-prose text-xs leading-relaxed text-muted-foreground">
              This template takes no parameters. How long the condition must hold is the rule's own “for”.
            </p>
          ) : null}
          {KIND_PARAMS[draft.kind] === undefined ? (
            <p data-testid="unknown-kind" className="max-w-prose text-xs leading-relaxed text-health-warn">
              This build has no template for “{draft.kind}”, so it cannot show you its parameters or render an
              expression from them. Pick a kind above; saving this one is refused by the server.
            </p>
          ) : null}
        </div>

        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Severity" testId="severity" error={errorFor("severity")} hint="The label Alertmanager routes on.">
            {(id, invalid) => (
              <select
                id={id}
                value={draft.severity}
                onChange={(e) => setDraft((d) => ({ ...d, severity: e.target.value as AlertSeverity }))}
                className={fieldClasses(invalid)}
              >
                {SEVERITIES.map((s) => (
                  <option key={s} value={s}>
                    {s}
                  </option>
                ))}
              </select>
            )}
          </Field>

          <Field
            label="For"
            testId="for"
            error={errorFor("for") ?? (draft.forText.trim() !== "" && !duration.ok ? duration.message : undefined)}
            hint="How long the expression must hold — 30s, 5m, 2h. Blank fires as soon as it holds."
          >
            {(id, invalid) => (
              <input
                id={id}
                value={draft.forText}
                placeholder="5m"
                onChange={(e) => setDraft((d) => ({ ...d, forText: e.target.value }))}
                className={fieldClasses(invalid)}
              />
            )}
          </Field>
        </div>

        <PairEditor
          legend="Labels"
          noun="Label"
          pairs={draft.labels}
          disabled={false}
          onChange={(labels) => setDraft((d) => ({ ...d, labels }))}
        />
        {reservedMessage ? <ErrorLine testId="builder-error">{reservedMessage}</ErrorLine> : null}

        <PairEditor
          legend="Annotations"
          noun="Annotation"
          pairs={draft.annotations}
          disabled={false}
          onChange={(annotations) => setDraft((d) => ({ ...d, annotations }))}
        />

        <label className="flex items-center gap-2 text-[13px]">
          <input
            type="checkbox"
            checked={draft.enabled}
            onChange={(e) => setDraft((d) => ({ ...d, enabled: e.target.checked }))}
          />
          <span>Enabled</span>
        </label>

        <PreviewPanel ready={previewReady} loading={previewLoading} preview={preview} error={previewError} />

        {bannerError ? <ErrorLine testId="builder-error">{bannerError}</ErrorLine> : null}

        <div className="flex gap-2">
          <Button type="submit" loading={submitting} disabled={writesDisabled}>
            {initial ? "Save rule" : "Create rule"}
          </Button>
          {/* Cancel closes a form and touches nothing, so it stays live even
              while the Time Machine is engaged — settings.tsx's line. */}
          <Button type="button" variant="outline" onClick={onDone}>
            Cancel
          </Button>
        </div>
      </form>
    </Card>
  );
}

/* ── sections ───────────────────────────────────────────────────────────── */

function RulesSection({ canManage }: { canManage: boolean }) {
  const writesDisabled = useWritesDisabled();
  const [editing, setEditing] = useState<{ mode: "none" } | { mode: "create" } | { mode: "edit"; rule: AlertRule }>({
    mode: "none",
  });
  const [syncConflict, setSyncConflict] = useState<string>();
  const query = useQuery({ queryKey: ["alert-rules"], queryFn: listAlertRules });
  const rules = query.data?.rules ?? [];

  return (
    <div className="flex flex-col gap-4">
      {canManage ? (
        editing.mode === "none" ? (
          <div>
            <Button size="sm" disabled={writesDisabled} onClick={() => setEditing({ mode: "create" })}>
              New rule
            </Button>
          </div>
        ) : (
          <RuleForm
            key={editing.mode === "edit" ? editing.rule.id : "create"}
            initial={editing.mode === "edit" ? editing.rule : undefined}
            onDone={() => setEditing({ mode: "none" })}
          />
        )
      ) : null}

      <SectionCard title="Alert rules">
        <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">
          Rules this console manages. They live in the database and are applied to the cluster as one PrometheusRule
          object; the status on each row is the reconciler's view of whether the cluster agrees, as of the instant next
          to it.
        </p>
        {syncConflict ? <ErrorLine testId="rules-sync-banner">{syncConflict}</ErrorLine> : null}
        {query.isError ? <ErrorLine>{queryErrorMessage(query.error, "Alert rules are unavailable")}</ErrorLine> : null}
        {/* isPending, not isLoading: a query whose retry is PAUSED (react-query
            pauses retries while the browser thinks it is offline) is pending
            but not fetching — isLoading is false there, and an empty-state
            guard of !isLoading && !isError would present "no rules" as a
            settled answer nobody actually got. Found live at the M7 final
            gate; the only honest empty is isSuccess && empty. */}
        {query.isPending ? <ListSkeleton /> : null}
        {query.isSuccess && rules.length === 0 ? (
          <p className="px-1 py-10 text-center text-xs text-muted-foreground">
            No rules yet. Prometheus is evaluating nothing on this console's behalf.
          </p>
        ) : null}
        {rules.length > 0 ? (
          <ul aria-label="Alert rules" className="mt-4 divide-y divide-border">
            {rules.map((rule) => (
              <RuleRow
                key={rule.id}
                rule={rule}
                canManage={canManage}
                onEdit={() => setEditing({ mode: "edit", rule })}
                onSyncConflict={setSyncConflict}
              />
            ))}
          </ul>
        ) : null}
      </SectionCard>
    </div>
  );
}

/* role="status" for the same reason the sync ack above it has one: the report
   appears when the POST answers, nowhere near the button that was pressed, and
   an operator who does not happen to be looking at that row never learns the
   import landed. Polite, not an alert — the import SUCCEEDED; its refusal is
   the role="alert" line next to it. */
function ImportReport({ report }: { report: AlertRuleImportReport }) {
  return (
    <div role="status" data-testid="import-report" className="mt-3 w-full border-l-2 border-border pl-3 text-xs">
      {/* The three arrays are three different statements and are rendered as
          three, always — including when one is empty. A skip means "this is NOT
          in your console"; a note means "this IS, and one field is the
          console's choice rather than your object's". */}
      <div data-testid="import-created">
        <p className="font-medium">Created</p>
        {report.created.length === 0 ? (
          <p className="text-muted-foreground">none</p>
        ) : (
          <ul className="mt-1 flex flex-col gap-0.5">
            {report.created.map((name) => (
              <li key={name} className="font-mono">
                {name}
              </li>
            ))}
          </ul>
        )}
      </div>

      <div data-testid="import-skipped" className="mt-3">
        <p className="font-medium">Skipped</p>
        {report.skipped.length === 0 ? (
          <p className="text-muted-foreground">none</p>
        ) : (
          <dl className="mt-1 flex flex-col gap-1">
            {report.skipped.map((item, i) => (
              <div key={`${item.name}-${i}`} className="flex flex-wrap gap-x-2">
                <dt className="font-mono">{item.name === "" ? "(unnamed entry)" : item.name}</dt>
                {/* Verbatim: the server names the entry as the FOREIGN object
                    spells it and says why in one sentence. */}
                <dd className="text-muted-foreground">{item.reason}</dd>
              </div>
            ))}
          </dl>
        )}
      </div>

      <div data-testid="import-notes" className="mt-3">
        <p className="font-medium">Notes</p>
        {report.notes.length === 0 ? (
          <p className="text-muted-foreground">none</p>
        ) : (
          <dl className="mt-1 flex flex-col gap-1">
            {report.notes.map((item, i) => (
              <div key={`${item.name}-${i}`} className="flex flex-wrap gap-x-2">
                <dt className="font-mono">{item.name}</dt>
                <dd className="text-muted-foreground">{item.note}</dd>
              </div>
            ))}
          </dl>
        )}
      </div>

      <p className="mt-3 max-w-prose leading-relaxed text-muted-foreground">
        Adoption copies: the original object is untouched, and the same alerts now exist twice in the cluster until its
        owner removes it. That is their decision, and this console will not make it for them.
      </p>
    </div>
  );
}

function ForeignRow({ rule, canManage }: { rule: ForeignRule; canManage: boolean }) {
  const qc = useQueryClient();
  const writesDisabled = useWritesDisabled();
  const [busy, setBusy] = useState(false);
  const [report, setReport] = useState<AlertRuleImportReport>();
  const [error, setError] = useState<string>();

  async function handleImport() {
    setBusy(true);
    setError(undefined);
    setReport(undefined);
    try {
      const result = await importForeignAlertRules(rule.name);
      setReport(result);
      await qc.invalidateQueries({ queryKey: ["alert-rules"] });
    } catch (err) {
      setError(queryErrorMessage(err, "The import was refused"));
    }
    setBusy(false);
  }

  return (
    <li className="flex flex-wrap items-center gap-3 py-3 text-sm">
      <span className="font-medium">{rule.name}</span>
      <span className="text-xs text-muted-foreground">
        {rule.groups} {rule.groups === 1 ? "group" : "groups"}
      </span>
      <span className="text-xs text-muted-foreground">
        {rule.rules} {rule.rules === 1 ? "rule" : "rules"}
      </span>
      {/* An object carrying no managed-by label gets an em dash, not a blank:
          "nobody claims this" is a fact, and a blank cell reads as a bug. */}
      <span data-testid="managed-by" className="text-xs text-muted-foreground">
        {rule.managedBy === "" ? "—" : rule.managedBy}
      </span>
      {canManage ? (
        <span className="ml-auto">
          <Button size="sm" variant="ghost" loading={busy} disabled={writesDisabled} onClick={() => void handleImport()}>
            Import {rule.name}
          </Button>
        </span>
      ) : null}
      {error ? (
        <span role="alert" className="w-full text-xs leading-relaxed text-health-bad">
          {error}
        </span>
      ) : null}
      {report ? <ImportReport report={report} /> : null}
    </li>
  );
}

function ForeignSection({ canManage }: { canManage: boolean }) {
  // A key of its own rather than ["alert-rules", "foreign"]: a rule write
  // invalidates ["alert-rules"] by prefix, and this list is a read of the
  // CLUSTER that a database write cannot have changed.
  const query = useQuery({ queryKey: ["foreign-alert-rules"], queryFn: listForeignAlertRules });
  const foreign = query.data?.foreign ?? [];

  return (
    <SectionCard title="Foreign rules">
      <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">
        PrometheusRule objects in this console's namespace that it does not own. Read-only: this console never writes to
        somebody else's object. Importing COPIES a rule's alerting entries into console-managed rows.
      </p>
      {/* Whatever the server said — the 409 that names console.alerting.enabled
          and explains that the rules above are unaffected, or the 503 that names
          console.database.mode — is rendered as it was written. Both are one
          sentence better than a paraphrase of them. */}
      {query.isError ? <ErrorLine>{queryErrorMessage(query.error, "Foreign rules are unavailable")}</ErrorLine> : null}
      {/* isPending / isSuccess, not !isLoading && !isError — the paused-retry
          trap; see the rules list above. */}
      {query.isPending ? <ListSkeleton /> : null}
      {query.isSuccess && foreign.length === 0 ? (
        <p className="px-1 py-10 text-center text-xs text-muted-foreground">
          No foreign PrometheusRule objects in this namespace.
        </p>
      ) : null}
      {foreign.length > 0 ? (
        <ul aria-label="Foreign PrometheusRule objects" className="mt-4 divide-y divide-border">
          {foreign.map((rule) => (
            <ForeignRow key={rule.name} rule={rule} canManage={canManage} />
          ))}
        </ul>
      ) : null}
    </SectionCard>
  );
}

/* ── page ───────────────────────────────────────────────────────────────── */

/**
 * AlertingPage's floor is alerts:read, and without it the page asks for
 * NOTHING — pages/targets.tsx's first degraded state, for the same reason: a
 * page that fires requests it cannot read the answers to collects 403s to
 * render as errors, when the honest statement is one card about the subject.
 *
 * alerts:read is held by EVERY built-in role including viewer, so that card is
 * for a hand-rolled role rather than a common state. alerts:manage is held by
 * operator, admin AND alert-editor — the last is the deliberate exception to
 * "statement-class writes stop at operator", because editing alert rules is
 * that role's entire charter (authz/roles.go says so at length).
 *
 * The gate waits for `me` before deciding: `can()` fails closed while GET
 * /api/v1/auth/me is in flight, so rendering on the un-resolved value would
 * flash the permission card on every cold load.
 */
export function AlertingPage() {
  const { me, can } = useAuth();
  const canRead = can("alerts:read");
  const canManage = can("alerts:manage");

  let body: ReactNode;
  if (me === undefined) {
    body = (
      <Card role="status" aria-live="polite" className="p-6">
        <span className="sr-only">Loading…</span>
        <Skeleton className="h-10 w-full" />
      </Card>
    );
  } else if (!canRead) {
    body = (
      <PermissionCard permission="alerts:read">
        Alert rules are what this console asks Prometheus to evaluate, and the foreign rules beside them are objects in
        its namespace. Every built-in role holds alerts:read; a role that does not sees this instead of a page firing
        requests it cannot read the answers to.
      </PermissionCard>
    );
  } else {
    body = (
      <>
        <RulesSection canManage={canManage} />
        <ForeignSection canManage={canManage} />
      </>
    );
  }

  return (
    <PageShell
      title="Alerting"
      description="Console-managed Prometheus alert rules, what the cluster thinks of them, and the rules it holds that this console does not own."
    >
      {body}
    </PageShell>
  );
}
