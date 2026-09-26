import { Fragment, useEffect, useId, useMemo, useRef, useState, type FormEvent, type ReactNode } from "react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { ExternalLink } from "lucide-react";
import { docsConsoleUrl } from "@/components/page-help";
import { PageShell } from "@/components/page-shell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { TextLink } from "@/components/ui/text-link";
import { EmptyState } from "@/components/ui/empty-state";
import { Input, Textarea } from "@/components/ui/input";
import { Pager, usePager } from "@/components/ui/pager";
import { Select } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { useAuth } from "@/hooks/use-auth";
import { useConfirmStep } from "@/hooks/use-confirm-step";
import { useFocusOnRefusal } from "@/hooks/use-focus-on-refusal";
import { useSubmitGuard } from "@/hooks/use-submit-guard";
import { MaintenanceRow } from "@/components/maintenance";
import { ROW_ACTION, RowActionLabel } from "@/components/settings-section";
import {
  ApiError,
  createAlertRule,
  deleteAlertRule,
  getMaintenance,
  importForeignAlertRules,
  listAlertRules,
  listForeignAlertRules,
  listAllTargets,
  previewAlertRule,
  queryErrorMessage,
  syncAlertRules,
  updateAlertRule,
} from "@/lib/api";
// Read in each mutating component rather than threaded down as a prop, the same
// way pages/settings.tsx and pages/targets.tsx do it: a permission decides
// whether a control EXISTS, this decides whether it is usable right now.
import { stampFull, translate, useLocale, useT, type Locale, type Translate } from "@/lib/i18n";
import { alertingDict, pluralKey, type AlertingKey } from "@/lib/i18n/dict/alerting";
import { useWriteGuard, withAtParam } from "@/lib/timemachine";
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
import { CHECKBOX_CLASS, cn } from "@/lib/utils";

/** Prometheus evaluates, the console MANAGES; with alerting off this section is the only one that stops working. */

/* ── shared bits ────────────────────────────────────────────────────────── */

function problemStatus(error: unknown): number | undefined {
  return error instanceof ApiError ? error.problem.status : undefined;
}

function SectionCard({
  title,
  blurb,
  action,
  children,
}: {
  title: string;
  /** The one-paragraph explanation under the heading. */
  blurb?: ReactNode;
  /** The section's own create button, on the heading line at the right. */
  action?: ReactNode;
  children: ReactNode;
}) {
  return (
    <Card asChild className="p-4 sm:p-6">
      <section>
        <div className="flex items-start justify-between gap-4">
          <h2 className="type-section">{title}</h2>
          {action ? <div className="shrink-0">{action}</div> : null}
        </div>
        {blurb ? <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{blurb}</p> : null}
        {children}
      </section>
    </Card>
  );
}

function ErrorLine({
  testId,
  onRetry,
  className,
  children,
}: {
  testId?: string;
  /** Re-runs the read that failed; a small ghost button beside the sentence. */
  onRetry?: () => void;
  className?: string;
  children: ReactNode;
}) {
  const t = useT(alertingDict);
  /* The sentence and the role stay on ONE element — tests and assistive tech
     both find the alert by its text — and the button rides inside it. */
  return (
    <p role="alert" data-testid={testId} className={cn("mt-3 text-sm leading-relaxed text-health-bad", className)}>
      {children}
      {onRetry ? (
        <Button type="button" size="sm" variant="ghost" className="ml-3 h-7 px-2 align-middle" onClick={onRetry}>
          {t("error.retry")}
        </Button>
      ) : null}
    </p>
  );
}

/** withNodes renders a translated sentence that contains LINKS — the maintenance
 *  blurb says "…on Investigate or Explore…" with each name an anchor. The same
 *  helper pages/settings.tsx keeps. */
function withNodes(template: string, nodes: Record<string, ReactNode>): ReactNode[] {
  return template.split(/(\{\w+\})/).map((chunk, i) => {
    const name = /^\{(\w+)\}$/.exec(chunk)?.[1];
    const node = name === undefined ? undefined : nodes[name];
    return node === undefined ? chunk : <Fragment key={i}>{node}</Fragment>;
  });
}

/** SurfaceLink is a plain in-app link that applies withAtParam ITSELF so its
 *  callers cannot forget — a bare href would silently drop a pinned ?at=. */
function SurfaceLink({ to, children }: { to: string; children: ReactNode }) {
  return (
    <TextLink href={withAtParam(to)}>
      {children}
    </TextLink>
  );
}

/**
 * SyncDisabledNotice is the ONE rendering of "rule sync is not running here" —
 * the 409 whose paragraph names console.alerting.enabled. It exists as a
 * component rather than as a class repeated in three places because it used to
 * BE three places: amber and role=status in the managed section, red and
 * role=alert in the foreign list, red again in the click banner, all printing
 * the same server sentence and each implying a different severity for it.
 * Amber, always: a console that does not sync is configured that way, no rule is
 * at risk, and nothing here is a failure to act on.
 */
function SyncDisabledNotice({ testId, children }: { testId: string; children: ReactNode }) {
  return (
    <p role="status" data-testid={testId} className="mt-3 max-w-prose text-xs leading-relaxed text-health-warn">
      {children}
    </p>
  );
}

function PermissionCard({ permission, children }: { permission: string; children: ReactNode }) {
  const t = useT(alertingDict);
  /* The permission string is interpolated, never translated — "alerts:manage"
     is what an operator asks for and what authz/roles.go spells. */
  return (
    <Card role="status" className="p-4 sm:p-6">
      <p className="text-sm font-medium">{t("permission.requires", { permission })}</p>
      <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{children}</p>
    </Card>
  );
}

function ListSkeleton() {
  const t = useT(alertingDict);
  return (
    <div role="status" aria-live="polite" className="mt-4 flex flex-col gap-2">
      <span className="sr-only">{t("loading")}</span>
      {Array.from({ length: 3 }, (_, i) => (
        <Skeleton key={i} className="h-10 w-full" />
      ))}
    </div>
  );
}

/** enT is the English translator this file's PURE helpers default to. */
const enT: Translate<AlertingKey> = (key, vars) => translate(alertingDict, "en", key, vars);

/* ── durations ──────────────────────────────────────────────────────────── */

/**
 * DURATION_UNITS is Prometheus's duration grammar in the DESCENDING order the grammar itself
 * requires; the order is load-bearing twice: it is the legality check for a composite string (a
 * unit may only be followed by a smaller one).
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

/**
 * A refusal carries `message` — the sentence, already in the caller's language
 * — and nothing else the caller has to reassemble. The translator is threaded
 * IN rather than the key threaded out, so the type stays the two-case union it
 * has always been and every existing reader of `.message` is unaffected.
 */
export type DurationParse = { ok: true; ns: number } | { ok: false; message: string };

/**
 * MAX_DURATION_NS is what a Prometheus `for` is stored in on the other side: a
 * Go time.Duration, i.e. int64 nanoseconds, which tops out a shade under 292
 * years.
 *
 * The bound is a REFUSAL rather than a clamp because of what the old parser did
 * without one (QA hostile pass): `ns += Number(digits) * unit` on a long enough
 * digit run is Infinity, the parse answered ok, JSON.stringify wrote that field
 * as `null`, and the server read `null` back as ZERO. The rule saved with a
 * `for` of 0s — firing instantly, forever — and not one thing on screen ever
 * said the number had been thrown away.
 */
export const MAX_DURATION_NS = 9_223_372_036_854_775_807;

/**
 * parsePromDuration reads what an operator typed into the `for` box; a value+unit pair would also
 * have to decide what "90" plus "seconds" renders as when the stored value is 90s (a unit select
 * cannot show "1m30s").
 */
export function parsePromDuration(text: string, t: Translate<AlertingKey> = enT): DurationParse {
  const s = text.trim();
  if (s === "") return { ok: true, ns: 0 };

  let i = 0;
  let ns = 0;
  let lastUnit = -1;
  while (i < s.length) {
    const digitsAt = i;
    while (i < s.length && s[i] >= "0" && s[i] <= "9") i++;
    if (i === digitsAt) {
      return { ok: false, message: t("duration.notADuration", { text: s }) };
    }
    const digits = s.slice(digitsAt, i);

    const lettersAt = i;
    while (i < s.length && s[i] >= "a" && s[i] <= "z") i++;
    const unit = s.slice(lettersAt, i);
    if (unit === "") {
      return { ok: false, message: t("duration.noUnit", { text: s, digits }) };
    }
    const at = DURATION_UNITS.findIndex(([u]) => u === unit);
    if (at === -1) {
      /* The unit LIST inside the sentence stays as it is: those seven letters
         are Prometheus's grammar, not vocabulary. */
      return { ok: false, message: t("duration.badUnit", { unit }) };
    }
    if (at <= lastUnit) {
      return { ok: false, message: t("duration.order", { text: s }) };
    }
    lastUnit = at;
    ns += Number(digits) * DURATION_UNITS[at][1];
  }
  /* Checked AFTER the loop, not per term: "100y100y" is refused by the ordering
     rule long before it could overflow, and a composite that only overflows on
     its last term is still one value the box cannot hold. */
  if (!Number.isFinite(ns) || ns > MAX_DURATION_NS) {
    return { ok: false, message: t("duration.tooLong", { text: s }) };
  }
  return { ok: true, ns };
}

/** formatPromDuration is the inverse, and it mirrors render.go's own
 *  FormatPromDuration byte for byte for every value a rule can really carry.
 *
 *  A value it CANNOT carry is where the mirror stops (QA hostile pass): NaN,
 *  Infinity and an absent field all fell through to the template literal and
 *  put the words "NaNns" and "undefinedns" on screen, in a row and in the `for`
 *  box of the builder that opened it. There is no duration to state, so the
 *  console states none — the same em dash it already uses for an instant it
 *  does not have. A NEGATIVE ns keeps render.go's own rendering: that is a real
 *  int64 the server could have written, and showing it is how anyone finds out. */
export function formatPromDuration(ns: number): string {
  if (!Number.isFinite(ns)) return "—";
  if (ns === 0) return "0s";
  if (ns < 0) return `${ns}ns`;
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
export function relativeTime(iso: string | undefined, now: Date, t: Translate<AlertingKey> = enT): string {
  if (!iso) return "—";
  const at = new Date(iso).getTime();
  /* An unparseable stamp renders VERBATIM — it is the server's bytes, and a
     translated guess about them would be this console inventing a time. */
  if (Number.isNaN(at)) return iso;
  const seconds = Math.floor((now.getTime() - at) / 1000);
  if (seconds < 0) return t("age.justNow");
  if (seconds < 60) return t("age.seconds", { count: seconds });
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return t("age.minutes", { count: minutes });
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return t("age.hours", { count: hours });
  return t("age.days", { count: Math.floor(hours / 24) });
}

/** The locale is required: a bare toLocaleString() reorders the date and swaps in AM/PM from
 *  whatever the browser was installed in — "8/10/2026 3:47 AM" on a Russian page. */
function absoluteTime(iso: string | undefined, locale: Locale): string | undefined {
  if (!iso) return undefined;
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : stampFull(d, locale);
}

/* ── the closed schemas, mirrored ───────────────────────────────────────── */

/**
 * RESERVED_LABEL_NAMES are the two label names the renderer stamps on every rule entry itself
 * (alerting/render.go's SeverityLabel and RuleIDLabel).
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
  /** What the FORM calls this param, as a dictionary key — the wire key above
   *  is the identity, this is only the label over the box. */
  labelKey: AlertingKey;
  /** "target" is "text" that KNOWS what it names. */
  type: "number" | "text" | "expr" | "enum" | "target";
  required: boolean;
  /** Set for `enum`; values are the wire values. */
  options?: readonly string[];
  /** Set on an enum whose wire value is a NUMBER (zone-latency's quantile). */
  numeric?: boolean;
  hintKey?: AlertingKey;
}

/**
 * KIND_PARAMS mirrors internal/console/alerting/render.go's `kindSchemas` table; mirrored rather
 * than derived because there is nothing to derive.
 */
export const KIND_PARAMS: Record<AlertRuleKind, readonly ParamField[]> = {
  "pair-loss": [
    { key: "protocol", labelKey: "param.protocol", type: "enum", required: true, options: ["tcp", "udp", "icmp"] },
    {
      key: "thresholdPercent",
      labelKey: "param.thresholdPercent.loss",
      type: "number",
      required: true,
      hintKey: "param.thresholdPercent.loss.hint",
    },
    {
      key: "scope.sourceNode",
      labelKey: "param.scope.sourceNode",
      type: "text",
      required: false,
      hintKey: "param.scope.sourceNode.hint",
    },
    {
      key: "scope.destNode",
      labelKey: "param.scope.destNode",
      type: "text",
      required: false,
      hintKey: "param.scope.destNode.hint",
    },
  ],
  "zone-latency": [
    { key: "protocol", labelKey: "param.protocol", type: "enum", required: true, options: ["tcp", "udp", "icmp"] },
    {
      key: "quantile",
      labelKey: "param.quantile",
      type: "enum",
      required: true,
      numeric: true,
      options: ["0.5", "0.95", "0.99"],
      hintKey: "param.quantile.hint",
    },
    {
      key: "thresholdMs",
      labelKey: "param.thresholdMs.latency",
      type: "number",
      required: true,
      hintKey: "param.thresholdMs.latency.hint",
    },
    { key: "sourceZone", labelKey: "param.sourceZone", type: "text", required: false, hintKey: "param.optional" },
    { key: "destZone", labelKey: "param.destZone", type: "text", required: false, hintKey: "param.optional" },
  ],
  "dns-failures": [
    {
      key: "thresholdPercent",
      labelKey: "param.thresholdPercent.dns",
      type: "number",
      required: true,
      hintKey: "param.thresholdPercent.dns.hint",
    },
  ],
  "http-ttfb": [
    {
      key: "thresholdMs",
      labelKey: "param.thresholdMs.ttfb",
      type: "number",
      required: true,
      hintKey: "param.thresholdMs.ttfb.hint",
    },
    { key: "url", labelKey: "param.url", type: "text", required: false, hintKey: "param.url.hint" },
  ],
  "agent-missing": [],
  "external-target-down": [
    /* type "target", not "text": the value has to match a target's name
       EXACTLY or the rendered expression selects nothing, and a free box gave
       an operator no way to know what the names are. */
    {
      key: "targetName",
      labelKey: "param.targetName",
      type: "target",
      required: false,
      hintKey: "param.targetName.hint",
    },
  ],
  raw: [
    { key: "expr", labelKey: "param.expr", type: "expr", required: true, hintKey: "param.expr.hint" },
  ],
};

/**
 * ALERT_RULE_KINDS is the select's order and the key of the one-line blurb next to each option; the
 * KIND is the template's identifier and renders as itself.
 */
export const ALERT_RULE_KINDS: readonly (readonly [AlertRuleKind, AlertingKey])[] = [
  ["pair-loss", "kind.pair-loss"],
  ["zone-latency", "kind.zone-latency"],
  ["dns-failures", "kind.dns-failures"],
  ["http-ttfb", "kind.http-ttfb"],
  ["agent-missing", "kind.agent-missing"],
  ["external-target-down", "kind.external-target-down"],
  ["raw", "kind.raw"],
];

const SEVERITIES: readonly AlertSeverity[] = ["info", "warning", "critical"];

/** problemField routes a 422's detail to the form field it is about. */
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
 * alertRuleRequestFrom turns a STORED rule back into a write body; sending everything by hand at
 * each call site is how a field goes missing.
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
 * duplicateKey returns the first key two rows share, or undefined.
 *
 * mapFrom is a fold into an object, so a repeated key does not error — it
 * OVERWRITES, and the survivor is whichever row happens to come last. On screen
 * both rows stay, both look saved, and one of the two values is simply gone
 * (QA hostile pass). The form refuses instead, by name, next to the boxes.
 */
export function duplicateKey(pairs: Pair[]): string | undefined {
  const seen = new Set<string>();
  for (const pair of pairs) {
    const key = pair.key.trim();
    if (key === "") continue;
    if (seen.has(key)) return key;
    seen.add(key);
  }
  return undefined;
}

/** repeatIndex is the row of the SECOND occurrence of `key` — the one the
 *  refusal renders under, since the first is the one that would have won. */
export function repeatIndex(pairs: Pair[], key: string): number {
  let seen = false;
  for (let i = 0; i < pairs.length; i++) {
    if (pairs[i].key.trim() !== key) continue;
    if (seen) return i;
    seen = true;
  }
  return -1;
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
 * paramsFromDraft types and nests the form's strings into the params object; an empty optional
 * field is OMITTED rather than sent as "".
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

/** SYNC_KEYS names each reconciler state on screen. Beside SYNC_TONE rather
 *  than inside it: one decides the colour, the other the word, and a row that
 *  changed only one of them would be the pill lying with its own paint. */
const SYNC_KEYS: Record<AlertSyncStatus, AlertingKey> = {
  synced: "sync.synced",
  drift: "sync.drift",
  error: "sync.error",
  unsynced: "sync.unsynced",
};

function RuleRow({
  rule,
  canManage,
  focused,
  onEdit,
  onSyncConflict,
}: {
  rule: AlertRule;
  canManage: boolean;
  /** This row is the one ?rule= named: open on arrival, and scroll to. */
  focused: boolean;
  onEdit: () => void;
  onSyncConflict: (detail: string) => void;
}) {
  const t = useT(alertingDict);
  const { locale } = useLocale();
  const qc = useQueryClient();
  const ruleSync = useRuleSync();
  /* Spread it onto the control; the alias below is for the controls that compose it with a local condition. */
  const guard = useWriteGuard();
  const writesDisabled = guard.disabled;
  const detailsId = useId();
  const rowRef = useRef<HTMLLIElement>(null);
  const [expanded, setExpanded] = useState(focused);

  /*
   * The deep link's landing: a firing alert on the Overview used to drop the reader at the top of
   * an unsorted list to find the rule themselves.
   */
  useEffect(() => {
    if (!focused) return;
    rowRef.current?.scrollIntoView?.({ block: "center" });
  }, [focused]);
  const { confirming, confirmRef, triggerRef, ask, reset } = useConfirmStep();
  const [busy, setBusy] = useState(false);
  const [kicked, setKicked] = useState(false);
  const [error, setError] = useState<string>();
  /* The in-flight guard for this ROW's three writes (QA hostile pass). `busy`
     is state, so `disabled={busy}` only takes effect after React commits — and
     three clicks landing in ONE task all read the pre-commit value and all
     fire. The enable checkbox was the visible case: hammering it sent three
     PUTs, whose responses then raced to decide what "enabled" ended up as.
     `begin()` is a REF write and settles immediately, which is the whole
     reason hooks/use-submit-guard.ts exists; the two forms on this page
     already use it, and a row's writes need it for the same reason. */
  const { begin, end } = useSubmitGuard();

  async function handleToggle() {
    if (!begin()) return;
    setBusy(true);
    setError(undefined);
    try {
      await updateAlertRule(rule.id, alertRuleRequestFrom(rule, { enabled: !rule.enabled }));
      await qc.invalidateQueries({ queryKey: ["alert-rules"] });
    } catch (err) {
      setError(queryErrorMessage(err, t("row.saveFailed")));
    }
    setBusy(false);
    end();
  }

  async function handleDelete() {
    if (!begin()) return;
    setBusy(true);
    setError(undefined);
    try {
      await deleteAlertRule(rule.id);
      await qc.invalidateQueries({ queryKey: ["alert-rules"] });
    } catch (err) {
      setError(queryErrorMessage(err, t("row.deleteFailed")));
      reset();
    }
    /* busy released on BOTH paths, for the same reason `end()` is. The success path almost always
       unmounts this row a moment later, but "almost always" is not a guarantee: a 204 whose refetch
       then fails leaves the row on screen with every control on it — confirm, the enable checkbox,
       Sync — disabled forever and nothing saying why. Only a page reload recovered it. */
    setBusy(false);
    end();
  }

  async function handleSync() {
    if (!begin()) return;
    setBusy(true);
    setError(undefined);
    setKicked(false);
    try {
      await syncAlertRules(rule.id);
    } catch (err) {
      // 409 is not this row's failure — it is the whole console's alerting
      // switch, and it says so in a paragraph. It goes to the section banner so
      // it is stated ONCE, however many rows the operator clicks.
      if (problemStatus(err) === 409) onSyncConflict(queryErrorMessage(err, t("row.syncDisabled")));
      else setError(queryErrorMessage(err, t("row.syncFailed")));
      setBusy(false);
      end();
      return;
    }
    setBusy(false);
    setKicked(true);
    end();
  }

  return (
    <li ref={rowRef} data-testid="rule-row" className="flex flex-wrap items-center gap-x-3 gap-y-1.5 py-2 text-sm">
      {/* The name is the ONE flexible column — flex-1 with a zero basis — so
          a long one truncates rather than pushing the chips and the actions
          onto a second line at desktop widths. The whole name is the title.
          min-w-[8rem] is the phone rule: the name keeps at least that much,
          so when the chip group cannot fit beside it the GROUP wraps under
          the name rather than the name shrinking to three letters. Each
          fixed group below (chips, stamp, toggle, actions) wraps as a whole. */}
      <span className="min-w-[8rem] flex-1 truncate font-medium" title={rule.name}>
        {rule.name}
      </span>
      <span className="flex shrink-0 items-center gap-2">
        {/* kind and severity are WIRE VALUES: the builder writes them, the
            renderer stamps severity onto the rule as a label, and Alertmanager
            routes on it. They render as themselves in both languages. The kind
            is an identity chip (neutral, no dot); severity carries a hue. */}
        <Badge variant="neutral">{rule.kind}</Badge>
        <Badge dot variant={SEVERITY_TONE[rule.severity]}>
          {rule.severity}
        </Badge>
        {/* syncStatus is the opposite case and does translate: nothing writes it
            and nothing routes on it — it is the reconciler's verdict rendered as
            a pill. A status this build has never heard of still renders, as
            itself. The reconciler's one-liner rides the chip as a title so it is
            reachable without expanding, and is repeated in full in the details
            panel: a title alone is invisible to touch and to anyone not
            hovering. */}
        <span data-testid="sync-status" title={rule.syncMessage === "" ? undefined : rule.syncMessage}>
          <Badge dot variant={SYNC_TONE[rule.syncStatus]}>
            {SYNC_KEYS[rule.syncStatus] ? t(SYNC_KEYS[rule.syncStatus]) : rule.syncStatus}
          </Badge>
        </span>
      </span>
      <span
        data-testid="last-synced"
        title={absoluteTime(rule.lastSyncedAt, locale)}
        className="type-meta shrink-0 whitespace-nowrap"
      >
        {relativeTime(rule.lastSyncedAt, new Date(), t)}
      </span>
      {canManage ? (
        /* LABELLED, and labelled with the same word a read-only reader gets from the badge below. A
           bare checkbox in a row of pills says nothing about what it toggles: the only clue was an
           aria-label, so a sighted operator had to infer it from position, and the two audiences saw
           two different controls for one fact. */
        <label className="type-meta flex shrink-0 items-center gap-1.5">
          <input
            type="checkbox"
            aria-label={t("row.enabledAria", { name: rule.name })}
            checked={rule.enabled}
            {...guard} disabled={writesDisabled || busy}
            onChange={() => void handleToggle()}
            className={CHECKBOX_CLASS}
          />
          {rule.enabled ? t("row.enabled") : t("row.disabled")}
        </label>
      ) : (
        <Badge dot variant={rule.enabled ? "ok" : "unknown"}>
          {rule.enabled ? t("row.enabled") : t("row.disabled")}
        </Badge>
      )}

      {/* The actions are the right column: ml-auto pins them to the edge on
          whichever line they land, so they can never fall under the data. On a
          phone they wrap inside the row instead of pushing past its edge. */}
      <span className="ml-auto flex max-w-full flex-wrap items-center justify-end gap-1 sm:shrink-0">
          {/* aria-controls, not aria-expanded alone: components/mtr-hop-table.tsx
              set this shape's bar (a row that expands into a detail block names
              the block it expands), and "expanded" with nothing named leaves a
              screen-reader user hunting the page for what just appeared. */}
          <Button
            size="sm"
            variant="ghost"
            className={ROW_ACTION}
            aria-expanded={expanded}
            aria-controls={detailsId}
            aria-label={t("row.details", { name: rule.name })}
            onClick={() => {
              setExpanded((v) => {
                writeFocusedRule(rule.id, !v);
                return !v;
              });
            }}
          >
            <RowActionLabel text={t("row.details.verb")} title={t("row.details", { name: rule.name })} />
          </Button>
          {canManage ? (
            confirming ? (
              <>
                {/* Spoken as well as drawn — the row swaps its controls under the reader. */}
                <span role="status" className="sr-only">
                  {t("row.confirmDelete", { name: rule.name })}
                </span>
                {/* The destructive treatment is reserved for this second click:
                    it is the one that cannot be taken back. */}
                <Button
                  ref={confirmRef}
                  size="sm"
                  variant="destructive"
                  className={ROW_ACTION}
                  loading={busy}
                  {...guard}
                  aria-label={t("row.confirmDelete", { name: rule.name })}
                  onClick={() => void handleDelete()}
                >
                  <RowActionLabel text={t("row.confirmDelete.verb")} title={t("row.confirmDelete", { name: rule.name })} />
                </Button>
                <Button size="sm" variant="ghost" className={ROW_ACTION} onClick={reset}>
                  {t("cancel")}
                </Button>
              </>
            ) : (
              <>
                {/* A control must not offer what the thing behind it refuses: with
                    sync off this button can only ever produce the 409 the section
                    header is already showing. The reason rides on it, so the
                    disabled state is never a mystery. */}
                <Button
                  size="sm"
                  variant="ghost"
                  className={ROW_ACTION}
                  {...guard}
                  disabled={writesDisabled || busy || ruleSync.disabled}
                  title={ruleSync.disabled ? ruleSync.message : undefined}
                  aria-label={t("row.sync", { name: rule.name })}
                  onClick={() => void handleSync()}
                >
                  <RowActionLabel text={t("row.sync.verb")} title={t("row.sync", { name: rule.name })} />
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  className={ROW_ACTION}
                  {...guard}
                  aria-label={t("row.edit", { name: rule.name })}
                  onClick={onEdit}
                >
                  <RowActionLabel text={t("row.edit.verb")} title={t("row.edit", { name: rule.name })} />
                </Button>
                <Button
                  ref={triggerRef}
                  size="sm"
                  variant="ghost"
                  className={ROW_ACTION}
                  {...guard}
                  aria-label={t("row.delete", { name: rule.name })}
                  onClick={ask}
                >
                  <RowActionLabel text={t("row.delete.verb")} title={t("row.delete", { name: rule.name })} />
                </Button>
              </>
            )
          ) : null}
      </span>

      {expanded ? (
        <div id={detailsId} className="basis-full border-l-2 border-border pl-3 text-xs">
          <p className="text-muted-foreground">{t("row.renderedExpr")}</p>
          {/* The SERVER's bytes, not a re-render: renderedExpr is on the row so
              the expression an operator reads is the one the bundle carries. */}
          <code className="mono-data mt-1 block break-all whitespace-pre-wrap">{rule.renderedExpr}</code>
          {rule.syncMessage === "" ? null : (
            <p className="mt-2 leading-relaxed text-muted-foreground">{rule.syncMessage}</p>
          )}
          {/* "for" keeps its own name inside the line — it is the rule's field,
              the same three letters the form's box is labelled with. */}
          <p className="mt-2 text-muted-foreground">
            {t("row.forLine", {
              duration: formatPromDuration(rule.forNs),
              at: absoluteTime(rule.lastSyncedAt, locale) ?? t("row.never"),
            })}
          </p>
        </div>
      ) : null}

      {kicked ? (
        <span role="status" data-testid="sync-ack" className="type-meta basis-full">
          {t("row.syncAck")}
        </span>
      ) : null}
      {error ? (
        <span role="alert" className="basis-full text-xs leading-relaxed text-health-bad">
          {error}
        </span>
      ) : null}
    </li>
  );
}

/* ── the builder ────────────────────────────────────────────────────────── */

/**
 * PREVIEW_DEBOUNCE_MS is how long the builder waits after the last edit before asking the server to
 * render and evaluate the draft; the preview is a POST that runs an instant query against
 * Prometheus.
 */
export const PREVIEW_DEBOUNCE_MS = 300;

function Field({
  label,
  hint,
  error,
  testId,
  className,
  children,
}: {
  label: string;
  hint?: string;
  error?: string;
  testId: string;
  /** Grid placement (the PromQL box spans both columns). */
  className?: string;
  children: (id: string, invalid: boolean) => ReactNode;
}) {
  const id = useId();
  /* min-w-0: a grid item's default min-width is its content, and a <select>
     is as wide as its widest option — at 375px that pushed the form past the
     viewport edge. The cell may now shrink below the control's natural width,
     and the control (w-full) follows the cell. */
  return (
    <div className={cn("flex min-w-0 flex-col gap-1 text-[13px]", className)}>
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

/** PairEditor names its strings by KEY rather than deriving them from one noun. */
function PairEditor({
  legendKey,
  nounKey,
  addKey,
  removeKey,
  pairs,
  disabled,
  errorAt,
  onChange,
}: {
  legendKey: AlertingKey;
  /** The singular that names each row's two boxes, for the aria-labels. */
  nounKey: AlertingKey;
  addKey: AlertingKey;
  removeKey: AlertingKey;
  pairs: Pair[];
  disabled: boolean;
  /** The refusal that belongs under row `i`, if any: it renders right there,
   *  with that row's name box marked invalid, rather than under the whole
   *  list where the reader has to work out which row it means. */
  errorAt?: (index: number) => string | undefined;
  onChange: (pairs: Pair[]) => void;
}) {
  const t = useT(alertingDict);
  const noun = t(nounKey);
  /* min-w-0 on the fieldset: browsers give a fieldset min-inline-size:
     min-content, so a one-line row of two boxes and a button widened the
     whole group past a phone viewport instead of letting the boxes shrink. */
  return (
    <fieldset className="flex min-w-0 flex-col gap-2 text-[13px]">
      <legend className="text-muted-foreground">{t(legendKey)}</legend>
      {pairs.map((pair, i) => {
        const error = errorAt?.(i);
        return (
          <div key={i} className="flex flex-col gap-1">
            <div className="flex items-center gap-2">
              {/* Placeholders, because "Add label" produces TWO identical empty
                  boxes and nothing on screen says which is which. The aria-labels have always been right; a sighted
                  operator had only the order to go on, and the order is the one
                  thing a two-box row does not communicate. The two boxes share
                  the row's width (flex-1, min-w-0) so the row is one line at
                  every width instead of stacking on a phone. */}
              <Input
                aria-label={t("pairs.nameAria", { noun, index: i + 1 })}
                placeholder={t("pairs.namePlaceholder")}
                value={pair.key}
                onChange={(e) => onChange(pairs.map((p, j) => (i === j ? { ...p, key: e.target.value } : p)))}
                invalid={error !== undefined}
                className="min-w-0 flex-1"
              />
              <Input
                aria-label={t("pairs.valueAria", { noun, index: i + 1 })}
                placeholder={t("pairs.valuePlaceholder")}
                value={pair.value}
                onChange={(e) => onChange(pairs.map((p, j) => (i === j ? { ...p, value: e.target.value } : p)))}
                className="min-w-0 flex-1"
              />
              <Button
                type="button"
                size="sm"
                variant="ghost"
                className="shrink-0"
                onClick={() => onChange(pairs.filter((_, j) => j !== i))}
              >
                {t(removeKey, { index: i + 1 })}
              </Button>
            </div>
            {error !== undefined ? (
              <ErrorLine testId="builder-error" className="mt-0">
                {error}
              </ErrorLine>
            ) : null}
          </div>
        );
      })}
      <div>
        <Button type="button" size="sm" variant="outline" disabled={disabled} onClick={() => onChange([...pairs, { key: "", value: "" }])}>
          {t(addKey)}
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
  const t = useT(alertingDict);
  return (
    <div
      role="region"
      aria-label={t("preview.region")}
      aria-live="polite"
      className="rounded-md border border-border p-4"
    >
      <p className="text-[13px] font-medium">{t("preview.heading")}</p>
      {!ready ? (
        <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{t("preview.notReady")}</p>
      ) : null}
      {ready && loading && !preview ? (
        <p className="mt-1 text-xs text-muted-foreground">{t("preview.rendering")}</p>
      ) : null}
      {error ? <ErrorLine>{error}</ErrorLine> : null}
      {preview ? (
        <>
          <code className="mono-data mt-2 block break-all whitespace-pre-wrap">{preview.expr}</code>
          {preview.error ? (
            <>
              {/* The two halves failed independently. The expression rendered —
                  that is a real result and it is shown — but nobody counted, so
                  `series` is not a number this may print. */}
              <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{t("preview.notEvaluated")}</p>
              {/* Prometheus's own error, verbatim. */}
              <p className="mt-1 text-xs leading-relaxed text-health-warn">{preview.error}</p>
            </>
          ) : (
            <p className="mt-2 text-xs text-muted-foreground">
              {t("preview.matches", { series: preview.series })}
              {preview.series === 0 ? t("preview.matchesZero") : ""}
            </p>
          )}
        </>
      ) : null}
    </div>
  );
}

function RuleForm({ initial, onDone }: { initial?: AlertRule; onDone: () => void }) {
  const t = useT(alertingDict);
  const qc = useQueryClient();
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's useWriteGuard. */
  const guard = useWriteGuard();
  const [draft, setDraft] = useState<RuleDraft>(() => draftFrom(initial));
  /* The draft as it was handed over, for the discard prompt below. A ref, not
     state: it never changes after mount, and comparing serialisations is
     cheaper than threading a dirty flag through every field. */
  const pristine = useRef(JSON.stringify(draftFrom(initial)));
  const dirty = JSON.stringify(draft) !== pristine.current;
  const [discarding, setDiscarding] = useState(false);
  /* The in-flight guard, not just a disabled look:
     begin() is a REF write, so three clicks in one task produce one request.
     hooks/use-submit-guard.ts says why a useState flag cannot do this. */
  const { submitting, begin, end } = useSubmitGuard();
  const [formError, setFormError] = useState<{ message: string; field?: string }>();
  const formRef = useRef<HTMLFormElement>(null);
  useFocusOnRefusal(formRef, formError);
  const [preview, setPreview] = useState<AlertRulePreview>();
  /* The previewKey whose preview came back with rejected=true. Held as the KEY
     rather than a boolean so any edit to the expression clears the block on its
     own: a different draft has a different key, and the block simply stops
     matching. */
  const [rejectedKey, setRejectedKey] = useState<string>();
  const [previewError, setPreviewError] = useState<string>();
  const [previewLoading, setPreviewLoading] = useState(false);

  const fields = paramFieldsFor(draft.kind);

  /*
   * The target list behind a "target" param; falling back to the free box rather than hiding the
   * field is the honest direction.
   */
  const { can } = useAuth();
  const needsTargets = fields.some((f) => f.type === "target");
  const canReadTargets = can("targets:read");
  const targetsQuery = useQuery({
    queryKey: ["targets"],
    // Every page: a picker that stops at the server's first 100 hides targets that exist.
    queryFn: () => listAllTargets(),
    enabled: needsTargets && canReadTargets,
  });
  const targetNames = (targetsQuery.data?.items ?? []).map((t) => t.name);
  /* Only once the list has ARRIVED. Rendering the select while the query is in
     flight would show an operator editing an existing rule an empty dropdown
     that silently does not contain their own stored value. */
  const targetsReady = canReadTargets && targetsQuery.isSuccess;

  // Live, not on submit: a reserved label is refused before a request is built,
  // so the message sits next to the box the whole time it is wrong.
  const reservedMessage = useMemo(
    () => draft.labels.map((p) => reservedLabelMessage(p.key.trim())).find((m) => m !== undefined),
    [draft.labels],
  );
  /* Same "live, not on submit" rule, for the other way a pair list can be
     wrong. Labels and annotations are checked separately because they become
     two separate maps, and a key may legitimately appear in both. */
  const duplicateLabel = useMemo(() => duplicateKey(draft.labels), [draft.labels]);
  const duplicateAnnotation = useMemo(() => duplicateKey(draft.annotations), [draft.annotations]);
  /* Each refusal renders under the row it is about. A reserved name is
     refused on every row that carries it; a repeat is refused on the row that
     repeats — the second one, since the first is the one the map would have
     kept. */
  const labelErrorAt = (i: number): string | undefined =>
    reservedLabelMessage(draft.labels[i]?.key.trim() ?? "") ??
    (duplicateLabel !== undefined && repeatIndex(draft.labels, duplicateLabel) === i
      ? t("pairs.duplicate", { name: duplicateLabel })
      : undefined);
  const annotationErrorAt = (i: number): string | undefined =>
    duplicateAnnotation !== undefined && repeatIndex(draft.annotations, duplicateAnnotation) === i
      ? t("pairs.duplicate", { name: duplicateAnnotation })
      : undefined;
  /* The selected kind's one-line blurb, as the hint under the select. It used
     to ride inside every <option> after an em dash, which made the closed
     select as wide as its longest sentence and pushed a phone-width form past
     the viewport; the identifier is the option, the blurb explains the pick. */
  const kindBlurb = ALERT_RULE_KINDS.find(([kind]) => kind === draft.kind)?.[1];

  const duration = parsePromDuration(draft.forText, t);
  const previewReady = fields.every((f) => !f.required || (draft.params[f.key] ?? "").trim() !== "");

  // The preview body is what a SAVE would send, minus the two fields that cannot change the
  // expression.
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
  const keyRef = useRef(previewKey);
  keyRef.current = previewKey;

  /* The block, and its exact scope: the LAST preview of the expression
     currently in the form proved that Prometheus refuses it. Saving anyway
     would put an expression into the rule bundle that Prometheus rejects,
     which on a sync-enabled console poisons the whole bundle — every other
     rule in it stops being applied too, over one rule nobody could have
     saved by accident.

     A preview is never REQUIRED: there is no PromQL parser in the browser, so
     "no preview yet" means "not known to be bad" and stays permissive. Only a
     proven rejection blocks, and any edit lifts it. */
  const exprRejected = rejectedKey !== undefined && rejectedKey === previewKey;

  useEffect(() => {
    if (!previewReady) {
      setPreview(undefined);
      setPreviewError(undefined);
      setPreviewLoading(false);
      setRejectedKey(undefined);
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
          // rejected=true means PROMETHEUS parsed this expression and refused
          // it. Anything else — unreachable, unconfigured, too much data — left
          // the expression unjudged and must not block a save.
          setRejectedKey(p.rejected ? keyRef.current : undefined);
        })
        .catch((err: unknown) => {
          if (cancelled) return;
          setPreview(undefined);
          setPreviewError(queryErrorMessage(err, t("preview.failed")));
          // The round trip itself failed, so nothing was proven either way.
          setRejectedKey(undefined);
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
    if (exprRejected) return;
    if (reservedMessage) return;
    // A refusal already on screen, next to the offending row.
    if (duplicateLabel !== undefined || duplicateAnnotation !== undefined) return;
    if (draft.name.trim() === "") {
      setFormError({ message: t("form.nameRequired"), field: "name" });
      return;
    }
    if (!duration.ok) {
      setFormError({ message: duration.message, field: "for" });
      return;
    }
    if (!begin()) return;
    try {
      const req = requestFromDraft(draft, duration.ns);
      if (initial) await updateAlertRule(initial.id, req);
      else await createAlertRule(req);
      await qc.invalidateQueries({ queryKey: ["alert-rules"] });
      onDone();
    } catch (err) {
      const message = queryErrorMessage(err, t("form.failed"));
      setFormError({ message, field: problemField(message) });
      end();
    }
  }

  return (
    <Card asChild className="p-4 sm:p-6">
      <form
        ref={formRef}
        onSubmit={handleSubmit}
        aria-label={initial ? t("form.editAria", { name: initial.name }) : t("form.createAria")}
        className="flex flex-col gap-4 [&>*]:max-w-2xl"
      >
        <h2 className="type-section">
          {initial ? t("form.edit", { name: initial.name }) : t("form.create")}
        </h2>

        <div className="grid grid-cols-[minmax(0,1fr)] gap-4 sm:grid-cols-2">
          <Field label={t("form.name")} testId="name" error={errorFor("name")} hint={t("form.nameHint")}>
            {(id, invalid) => (
              <Input
                id={id}
                value={draft.name}
                placeholder="PairLossHigh"
                onChange={(e) => setDraft((d) => ({ ...d, name: e.target.value }))}
                invalid={invalid}
                className="w-full"
              />
            )}
          </Field>

          <Field label={t("form.kind")} testId="kind" hint={kindBlurb ? t(kindBlurb) : undefined}>
            {(id) => (
              <Select
                id={id}
                value={draft.kind}
                onChange={(e) => setDraft((d) => ({ ...d, kind: e.target.value as AlertRuleKind }))}
                className="w-full"
              >
                {/* The option is the stored identifier alone; the blurb that
                    explains it is the hint under the select, and translates. */}
                {ALERT_RULE_KINDS.map(([kind]) => (
                  <option key={kind} value={kind}>
                    {kind}
                  </option>
                ))}
              </Select>
            )}
          </Field>
        </div>

        <div className="grid grid-cols-[minmax(0,1fr)] gap-4 sm:grid-cols-2">
          {fields.map((field) => (
            <Field
              key={field.key}
              className={field.type === "expr" ? "sm:col-span-2" : undefined}
              label={t(field.labelKey)}
              hint={
                field.type === "target" && !targetsReady
                  ? `${field.hintKey ? t(field.hintKey) : ""} ${t("form.targetFallbackHint")}`.trim()
                  : field.hintKey
                    ? t(field.hintKey)
                    : undefined
              }
              testId={field.key.split(".").pop() ?? field.key}
              error={errorFor(field.key)}
            >
              {(id, invalid) =>
                field.type === "target" && targetsReady ? (
                  <Select
                    id={id}
                    value={draft.params[field.key] ?? ""}
                    onChange={(e) => setDraft((d) => ({ ...d, params: { ...d.params, [field.key]: e.target.value } }))}
                    invalid={invalid}
                    className="w-full"
                  >
                    {/* "" is a real, meaningful value here — every external
                        target — so it is named rather than left as an em dash
                        the enum select uses for "unset". */}
                    <option value="">{t("form.everyTarget")}</option>
                    {targetNames.map((name) => (
                      <option key={name} value={name}>
                        {name}
                      </option>
                    ))}
                    {/* A stored value the list does not contain — a target
                        deleted since the rule was written — stays selectable
                        rather than being silently rewritten to "" on the next
                        save, which would widen the rule to every target. */}
                    {(draft.params[field.key] ?? "") !== "" &&
                    !targetNames.includes(draft.params[field.key] ?? "") ? (
                      <option value={draft.params[field.key]}>
                        {t("form.noSuchTarget", { name: draft.params[field.key] ?? "" })}
                      </option>
                    ) : null}
                  </Select>
                ) : field.type === "enum" ? (
                  <Select
                    id={id}
                    value={draft.params[field.key] ?? ""}
                    onChange={(e) => setDraft((d) => ({ ...d, params: { ...d.params, [field.key]: e.target.value } }))}
                    invalid={invalid}
                    className="w-full"
                  >
                    <option value="">{t("form.enumUnset")}</option>
                    {/* The options are the WIRE values (tcp, 0.95). They are
                        what the form writes and stay as they are. */}
                    {(field.options ?? []).map((option) => (
                      <option key={option} value={option}>
                        {option}
                      </option>
                    ))}
                  </Select>
                ) : field.type === "expr" ? (
                  /* PromQL keeps its mono face; the field chrome (border, focus
                     ring, invalid) now comes from the shared Textarea. */
                  <Textarea
                    id={id}
                    rows={3}
                    value={draft.params[field.key] ?? ""}
                    onChange={(e) => setDraft((d) => ({ ...d, params: { ...d.params, [field.key]: e.target.value } }))}
                    invalid={invalid}
                    className="mono-data w-full p-3"
                  />
                ) : (
                  <Input
                    id={id}
                    type={field.type === "number" ? "number" : "text"}
                    value={draft.params[field.key] ?? ""}
                    onChange={(e) => setDraft((d) => ({ ...d, params: { ...d.params, [field.key]: e.target.value } }))}
                    invalid={invalid}
                    className="w-full"
                  />
                )
              }
            </Field>
          ))}
          {fields.length === 0 && KIND_PARAMS[draft.kind] !== undefined ? (
            <p data-testid="no-params" className="max-w-prose text-xs leading-relaxed text-muted-foreground">
              {t("form.noParams")}
            </p>
          ) : null}
          {KIND_PARAMS[draft.kind] === undefined ? (
            <p data-testid="unknown-kind" className="max-w-prose text-xs leading-relaxed text-health-warn">
              {t("form.unknownKind", { kind: draft.kind })}
            </p>
          ) : null}
        </div>

        <div className="grid grid-cols-[minmax(0,1fr)] gap-4 sm:grid-cols-2">
          <Field
            label={t("form.severity")}
            testId="severity"
            error={errorFor("severity")}
            hint={t("form.severityHint")}
          >
            {(id, invalid) => (
              <Select
                id={id}
                value={draft.severity}
                onChange={(e) => setDraft((d) => ({ ...d, severity: e.target.value as AlertSeverity }))}
                invalid={invalid}
                className="w-full"
              >
                {SEVERITIES.map((s) => (
                  <option key={s} value={s}>
                    {s}
                  </option>
                ))}
              </Select>
            )}
          </Field>

          <Field
            label={t("form.for")}
            testId="for"
            error={errorFor("for") ?? (draft.forText.trim() !== "" && !duration.ok ? duration.message : undefined)}
            hint={t("form.forHint")}
          >
            {(id, invalid) => (
              <Input
                id={id}
                value={draft.forText}
                placeholder="5m"
                onChange={(e) => setDraft((d) => ({ ...d, forText: e.target.value }))}
                invalid={invalid}
                className="w-full"
              />
            )}
          </Field>
        </div>

        {/* A reserved name is refused in the SERVER's own sentence, reproduced
            so client and server refuse it in identical words — data here, and
            rendered as written, under the row that carries it. */}
        <PairEditor
          legendKey="pairs.labels"
          nounKey="pairs.noun.label"
          addKey="pairs.add.label"
          removeKey="pairs.remove.label"
          pairs={draft.labels}
          disabled={false}
          errorAt={labelErrorAt}
          onChange={(labels) => setDraft((d) => ({ ...d, labels }))}
        />

        <PairEditor
          legendKey="pairs.annotations"
          nounKey="pairs.noun.annotation"
          addKey="pairs.add.annotation"
          removeKey="pairs.remove.annotation"
          pairs={draft.annotations}
          disabled={false}
          errorAt={annotationErrorAt}
          onChange={(annotations) => setDraft((d) => ({ ...d, annotations }))}
        />

        <label className="flex items-center gap-2 text-[13px]">
          <input
            type="checkbox"
            checked={draft.enabled}
            onChange={(e) => setDraft((d) => ({ ...d, enabled: e.target.checked }))}
            className={CHECKBOX_CLASS}
          />
          <span>{t("form.enabled")}</span>
        </label>

        <PreviewPanel ready={previewReady} loading={previewLoading} preview={preview} error={previewError} />

        {bannerError ? <ErrorLine testId="builder-error">{bannerError}</ErrorLine> : null}
        {exprRejected ? (
          <p role="alert" data-testid="rejected-expr-block" className="text-sm leading-relaxed text-health-bad">
            {t("form.rejectedBlock")}
          </p>
        ) : null}

        <div className="flex flex-wrap gap-2">
          <Button type="submit" loading={submitting} {...guard} disabled={guard.disabled || exprRejected}>
            {initial ? t("form.save") : t("form.createButton")}
          </Button>
          {/* Cancel closes a form and touches nothing, so it stays live even
              while the Time Machine is engaged — settings.tsx's line. It asks
              first only when there is something to lose: this is the longest
              form in the console, and closing it by mistake costs an operator
              every field they filled in. */}
          {discarding ? (
            <>
              <Button type="button" variant="outline" onClick={onDone}>
                {t("form.discard")}
              </Button>
              <Button type="button" variant="ghost" onClick={() => setDiscarding(false)}>
                {t("form.keepEditing")}
              </Button>
              <span role="status" className="self-center text-xs text-muted-foreground">
                {t("form.discardConfirm")}
              </span>
            </>
          ) : (
            <Button type="button" variant="outline" onClick={() => (dirty ? setDiscarding(true) : onDone())}>
              {t("cancel")}
            </Button>
          )}
        </div>
      </form>
    </Card>
  );
}

/* ── sections ───────────────────────────────────────────────────────────── */

/** RULE_PARAM is the deep link the Overview's firing-alert rows point at. */
export const RULE_PARAM = "rule";

function focusedRuleId(): string {
  return new URLSearchParams(window.location.search).get(RULE_PARAM) ?? "";
}

/**
 * writeFocusedRule keeps ?rule= and the expanded row in step, in BOTH
 * directions: the link already opened a row on arrival, but the row could not
 * produce the link — an operator who found the rule by scrolling had no way to
 * hand it to anyone. Expanding writes the param; collapsing clears it, but only
 * when it still names this row, so collapsing one row cannot silently unlink
 * another that is also open.
 *
 * replaceState, not pushState: expanding a row is not a navigation, and one
 * history entry per twirl would turn Back into an undo of nothing.
 */
function writeFocusedRule(id: string, expanded: boolean): void {
  const url = new URL(window.location.href);
  const current = url.searchParams.get(RULE_PARAM);
  if (expanded) {
    if (current === id) return;
    url.searchParams.set(RULE_PARAM, id);
  } else {
    if (current !== id) return;
    url.searchParams.delete(RULE_PARAM);
  }
  window.history.replaceState({}, "", url);
}

/**
 * useRuleSync reads whether prometheus rule sync is running on this console,
 * and the server's own sentence for why not.
 *
 * The evidence is the /foreign endpoint's 409 — the same one a Sync button used
 * to produce only AFTER being clicked. It is a read this page already makes
 * (ForeignSection), on the same query key, so asking here costs no extra round
 * trip; react-query serves both callers from one entry.
 *
 * Undisclosed until the answer is IN: `disabled` stays false while the query is
 * pending, so a cold load does not flash "sync is off" at a console where it is
 * on.
 */
function useRuleSync(): { disabled: boolean; message?: string } {
  const query = useQuery({ queryKey: ["foreign-alert-rules"], queryFn: listForeignAlertRules });
  if (query.isError && problemStatus(query.error) === 409) {
    // The server's paragraph, verbatim: it names console.alerting.enabled and
    // says the rules themselves are unaffected. Nothing here paraphrases it.
    return { disabled: true, message: queryErrorMessage(query.error, "") || undefined };
  }
  return { disabled: false };
}

function RulesSection({ canManage }: { canManage: boolean }) {
  const t = useT(alertingDict);
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's useWriteGuard. */
  const guard = useWriteGuard();
  /* Read ONCE, on mount: the param is where the reader arrived from, not a
     control. Re-reading it on every render would re-open a row the operator
     has since collapsed. */
  const [focusedRule] = useState(focusedRuleId);
  const [editing, setEditing] = useState<{ mode: "none" } | { mode: "create" } | { mode: "edit"; rule: AlertRule }>({
    mode: "none",
  });
  const [syncConflict, setSyncConflict] = useState<string>();
  const ruleSync = useRuleSync();
  const query = useQuery({ queryKey: ["alert-rules"], queryFn: listAlertRules });
  const rules = query.data?.rules ?? [];
  /* A cluster's managed rule set grows without a ceiling; the list is paged. */
  const pager = usePager(rules);

  /* And ?rule= has to land ON the rule, not on page 1.
     Overview links a firing alert straight here by id precisely so the operator arrives at that row;
     `focused` was only computed for rows in pager.visible, so with more than one page of rules the
     link silently did nothing — the reader landed on page 1, top of the list, with no indication
     that the rule they clicked was three pages down. The page is moved once per (id, list) pair, and
     never while the reader is paging by hand: focusedPage is derived from the rule's INDEX, so it
     only changes when the id or the list does. */
  const focusedIndex = focusedRule === "" ? -1 : rules.findIndex((r) => r.id === focusedRule);
  const focusedPage = focusedIndex < 0 ? 0 : Math.floor(focusedIndex / pager.size) + 1;
  const [sentToPage, setSentToPage] = useState(0);
  if (focusedPage > 0 && focusedPage !== sentToPage) {
    setSentToPage(focusedPage);
    pager.setPage(focusedPage);
  }
  if (focusedPage === 0 && sentToPage !== 0) {
    setSentToPage(0);
  }

  /*
   * A ?rule= that names nothing SAYS SO; only once the list has SETTLED: while the query is pending
   * there is no evidence either way.
   */
  const unknownRule = focusedRule !== "" && query.isSuccess && !rules.some((r) => r.id === focusedRule);

  const listEmpty = query.isSuccess && rules.length === 0;
  /* The create button sits on the section's heading line, or in the empty
     slate when there is nothing listed, so the one action is never drawn
     twice. It is not drawn at all until the list has settled: mounting it in
     the heading and then moving it into the slate is a REMOUNT, and a click
     that landed on the first node between the two renders opened nothing.
     While the builder is open there is no button either — the form is the
     action. */
  const createButton =
    canManage && editing.mode === "none" && !query.isPending ? (
      <Button size="sm" {...guard} onClick={() => setEditing({ mode: "create" })}>
        {t("rules.new")}
      </Button>
    ) : null;

  return (
    <div className="flex flex-col gap-5">
      {canManage && editing.mode !== "none" ? (
        <RuleForm
          key={editing.mode === "edit" ? editing.rule.id : "create"}
          initial={editing.mode === "edit" ? editing.rule : undefined}
          onDone={() => setEditing({ mode: "none" })}
        />
      ) : null}

      <SectionCard title={t("rules.heading")} blurb={t("rules.blurb")} action={listEmpty ? null : createButton}>
        {unknownRule ? (
          <p
            role="status"
            data-testid="unknown-rule-notice"
            className="mt-3 text-xs leading-relaxed text-muted-foreground"
          >
            {t("rules.unknownLink")}
          </p>
        ) : null}
        {/* Said ONCE, up front, when the console already knows: a reader should
            not have to click Sync and read a 409 to learn that nothing on this
            console applies these rules. */}
        {ruleSync.disabled && ruleSync.message ? (
          <SyncDisabledNotice testId="sync-disabled-notice">{ruleSync.message}</SyncDisabledNotice>
        ) : null}
        {/* The 409's own paragraph — it names console.alerting.enabled and
            explains that the rules above are unaffected. Rendered verbatim.
            Still here for the console that only learns this ON the click. */}
        {syncConflict ? <SyncDisabledNotice testId="rules-sync-banner">{syncConflict}</SyncDisabledNotice> : null}
        {query.isError ? (
          <ErrorLine onRetry={() => void query.refetch()}>
            {queryErrorMessage(query.error, t("rules.unavailable"))}
          </ErrorLine>
        ) : null}
        {/* isPending, not isLoading: a query whose retry is PAUSED (react-query
            pauses retries while the browser thinks it is offline) is pending
            but not fetching — isLoading is false there, and an empty-state
            guard of !isLoading && !isError would present "no rules" as a
            settled answer nobody actually got. Found live at the M7 final
            gate; the only honest empty is isSuccess && empty. */}
        {query.isPending ? <ListSkeleton /> : null}
        {listEmpty ? (
          <EmptyState
            compact
            className="mt-4"
            title={t("rules.empty")}
            body={t("rules.empty.body")}
            action={createButton}
          />
        ) : null}
        {rules.length > 0 ? (
          <>
          <ul aria-label={t("rules.listAria")} className="mt-4 divide-y divide-border">
            {pager.visible.map((rule) => (
              <RuleRow
                key={rule.id}
                rule={rule}
                canManage={canManage}
                focused={focusedRule !== "" && rule.id === focusedRule}
                onEdit={() => setEditing({ mode: "edit", rule })}
                onSyncConflict={setSyncConflict}
              />
            ))}
          </ul>
          <Pager pager={pager} subject={t("rules.subject")} className="px-0" />
          </>
        ) : null}
      </SectionCard>
    </div>
  );
}

/* role="status" for the same reason the sync ack above it has one: the report appears when the POST answers. */
function ImportReport({ report }: { report: AlertRuleImportReport }) {
  const t = useT(alertingDict);
  /* `?? []` on all three, though the schema says all three are required (QA
     hostile pass). The report is assembled in Go, where a nil slice marshals as
     JSON `null`, and the only thing standing between this panel and a
     render-time TypeError — a WHITE SCREEN, on the one panel whose whole job is
     to report — is that httpapi's newAlertRuleImportResponse remembers to seed
     each slice. That is a promise held on the other side of the wire; this is
     three characters. */
  const created = report.created ?? [];
  const skipped = report.skipped ?? [];
  const notes = report.notes ?? [];
  return (
    <div role="status" data-testid="import-report" className="mt-3 w-full border-l-2 border-border pl-3 text-xs">
      {/* The three arrays are three different statements and are rendered as
          three, always — including when one is empty. A skip means "this is NOT
          in your console"; a note means "this IS, and one field is the
          console's choice rather than your object's". Only the three HEADINGS
          are ours; every name and every reason below them is the server's. */}
      <div data-testid="import-created">
        <p className="font-medium">{t("import.created")}</p>
        {created.length === 0 ? (
          <p className="text-muted-foreground">{t("import.none")}</p>
        ) : (
          <ul className="mt-1 flex flex-col gap-0.5">
            {created.map((name) => (
              <li key={name} className="mono-data">
                {name}
              </li>
            ))}
          </ul>
        )}
      </div>

      <div data-testid="import-skipped" className="mt-3">
        <p className="font-medium">{t("import.skipped")}</p>
        {skipped.length === 0 ? (
          <p className="text-muted-foreground">{t("import.none")}</p>
        ) : (
          <dl className="mt-1 flex flex-col gap-1">
            {skipped.map((item, i) => (
              <div key={`${item.name}-${i}`} className="flex flex-wrap gap-x-2">
                <dt className="mono-data">{item.name === "" ? t("import.unnamed") : item.name}</dt>
                {/* Verbatim: the server names the entry as the FOREIGN object
                    spells it and says why in one sentence. */}
                <dd className="text-muted-foreground">{item.reason}</dd>
              </div>
            ))}
          </dl>
        )}
      </div>

      <div data-testid="import-notes" className="mt-3">
        <p className="font-medium">{t("import.notes")}</p>
        {notes.length === 0 ? (
          <p className="text-muted-foreground">{t("import.none")}</p>
        ) : (
          <dl className="mt-1 flex flex-col gap-1">
            {notes.map((item, i) => (
              <div key={`${item.name}-${i}`} className="flex flex-wrap gap-x-2">
                <dt className="mono-data">{item.name}</dt>
                <dd className="text-muted-foreground">{item.note}</dd>
              </div>
            ))}
          </dl>
        )}
      </div>

      <p className="mt-3 max-w-prose leading-relaxed text-muted-foreground">{t("import.adoption")}</p>
    </div>
  );
}

function ForeignRow({ rule, canManage }: { rule: ForeignRule; canManage: boolean }) {
  const t = useT(alertingDict);
  /* The plural forms below are chosen per LANGUAGE, not per count alone. */
  const { locale } = useLocale();
  const qc = useQueryClient();
  /* guard carries the DISABLED flag AND the reason for it — lib/timemachine's useWriteGuard. */
  const guard = useWriteGuard();
  const { confirming, confirmRef, triggerRef, ask, reset } = useConfirmStep();
  const [busy, setBusy] = useState(false);
  const [report, setReport] = useState<AlertRuleImportReport>();
  const [error, setError] = useState<string>();

  async function handleImport() {
    reset();
    setBusy(true);
    setError(undefined);
    setReport(undefined);
    try {
      const result = await importForeignAlertRules(rule.name);
      setReport(result);
      await qc.invalidateQueries({ queryKey: ["alert-rules"] });
    } catch (err) {
      setError(queryErrorMessage(err, t("foreign.importRefused")));
    }
    setBusy(false);
  }

  return (
    <li className="flex flex-wrap items-center gap-x-3 gap-y-1.5 py-2 text-sm">
      {/* The name is the flexible column and truncates; the counts, the owner
          and the action keep their width and wrap under it on a phone (the
          same min-w-[8rem] rule the managed rows follow). */}
      <span className="min-w-[8rem] flex-1 truncate font-medium" title={rule.name}>
        {rule.name}
      </span>
      {/* Three Russian forms where English has two, so the noun is chosen by
          count rather than by an `=== 1` suffix — see pluralKey. */}
      <span className="type-meta nums shrink-0 whitespace-nowrap">
        {rule.groups} {t(pluralKey(rule.groups, "count.groups.one", "count.groups.few", "count.groups.many", locale))}
      </span>
      <span className="type-meta nums shrink-0 whitespace-nowrap">
        {rule.rules} {t(pluralKey(rule.rules, "count.rules.one", "count.rules.few", "count.rules.many", locale))}
      </span>
      {/* An object carrying no managed-by label gets an em dash, not a blank:
          "nobody claims this" is a fact, and a blank cell reads as a bug. */}
      <span data-testid="managed-by" className="mono-data shrink-0 text-muted-foreground">
        {rule.managedBy === "" ? "—" : rule.managedBy}
      </span>
      {canManage ? (
        <span className="ml-auto flex shrink-0 flex-wrap items-center gap-1">
          {/* Same bound as the managed rows above: a PrometheusRule's
              metadata.name is up to 253 characters and this button's accessible
              name carries it; the verb is what shows. Two presses, as a delete
              takes: every entry lands as an ENABLED console rule, and undoing
              that is one delete per rule. */}
          {confirming ? (
            <>
              <span role="status" className="sr-only">
                {t("foreign.importConfirm.note", {
                  count: rule.alertRules,
                  rules: t(pluralKey(rule.alertRules, "count.rules.one", "count.rules.few", "count.rules.many", locale)),
                })}
              </span>
              <Button
                ref={confirmRef}
                size="sm"
                className={ROW_ACTION}
                loading={busy}
                {...guard}
                aria-label={t("foreign.importConfirm", { name: rule.name })}
                onClick={() => void handleImport()}
              >
                <RowActionLabel
                  text={t("foreign.importConfirm.verb")}
                  title={t("foreign.importConfirm", { name: rule.name })}
                />
              </Button>
              <Button size="sm" variant="ghost" className={ROW_ACTION} onClick={reset}>
                {t("cancel")}
              </Button>
            </>
          ) : (
            <Button
              ref={triggerRef}
              size="sm"
              variant="ghost"
              className={ROW_ACTION}
              loading={busy}
              {...guard}
              aria-label={t("foreign.import", { name: rule.name })}
              onClick={ask}
            >
              <RowActionLabel text={t("foreign.import.verb")} title={t("foreign.import", { name: rule.name })} />
            </Button>
          )}
        </span>
      ) : null}
      {error ? (
        <span role="alert" className="basis-full text-xs leading-relaxed text-health-bad">
          {error}
        </span>
      ) : null}
      {report ? <ImportReport report={report} /> : null}
    </li>
  );
}

function ForeignSection({ canManage }: { canManage: boolean }) {
  const t = useT(alertingDict);
  // A key of its own rather than ["alert-rules", "foreign"]: a rule write
  // invalidates ["alert-rules"] by prefix, and this list is a read of the
  // CLUSTER that a database write cannot have changed.
  const query = useQuery({ queryKey: ["foreign-alert-rules"], queryFn: listForeignAlertRules });
  const foreign = query.data?.foreign ?? [];
  const pager = usePager(foreign);

  return (
    <SectionCard title={t("foreign.heading")} blurb={t("foreign.blurb")}>
      {/* Whatever the server said — the 409 that names console.alerting.enabled
          and explains that the rules above are unaffected, or the 503 that names
          database.dsnFile — is rendered as it was written. Both are one
          sentence better than a paraphrase of them. The 409 takes the amber
          notice the managed section gives it, because it is the SAME standing
          condition read through a second endpoint; everything else is red. */}
      {query.isError ? (
        problemStatus(query.error) === 409 ? (
          <SyncDisabledNotice testId="foreign-sync-disabled-notice">
            {queryErrorMessage(query.error, t("foreign.unavailable"))}
          </SyncDisabledNotice>
        ) : (
          <ErrorLine onRetry={() => void query.refetch()}>
            {queryErrorMessage(query.error, t("foreign.unavailable"))}
          </ErrorLine>
        )
      ) : null}
      {/* isPending / isSuccess, not !isLoading && !isError — the paused-retry
          trap; see the rules list above. */}
      {query.isPending ? <ListSkeleton /> : null}
      {query.isSuccess && foreign.length === 0 ? (
        <EmptyState
          compact
          className="mt-4"
          title={t("foreign.empty")}
          body={t("foreign.empty.body")}
          /* Nothing on this page creates a foreign object, so the next step is
             the chapter that says where they come from. Outbound: a new tab
             with no opener, the same way About's links open. */
          action={
            <a
              href={`${docsConsoleUrl("alerting")}#foreign-rules`}
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex items-center gap-1.5 text-sm font-medium text-primary hover:underline"
            >
              {t("foreign.empty.action")}
              <ExternalLink aria-hidden="true" className="size-3.5" />
            </a>
          }
        />
      ) : null}
      {foreign.length > 0 ? (
        <>
        <ul aria-label={t("foreign.listAria")} className="mt-4 divide-y divide-border">
          {pager.visible.map((rule, i) => (
            /* The name is the object's identity in ONE namespace; the list can span
               several, so two rows may legitimately share it. The index disambiguates
               rather than letting React reconcile two different objects as one. */
            <ForeignRow key={`${rule.name}-${i}`} rule={rule} canManage={canManage} />
          ))}
        </ul>
        <Pager pager={pager} subject={t("foreign.subject")} className="px-0" />
        </>
      ) : null}
    </SectionCard>
  );
}

/* ── maintenance windows ──────────────────────────────────────────────────── */

/**
 * MaintenanceSection is the ONLY unbounded view of the declared windows in this console — and it
 * has to actually be unbounded: the API answers 100 rows plus a nextCursor, so the cursor is
 * followed rather than the past windows silently dropped.
 *
 * It lives on THIS page because a window suppresses and annotates exactly the signals the sections
 * above manage; declaring one still happens next to the chart it explains, and this list is for
 * finding and removing one. Moved verbatim from pages/settings.tsx.
 */
function MaintenanceSection() {
  const t = useT(alertingDict);
  const qc = useQueryClient();
  const query = useInfiniteQuery({
    queryKey: ["alerting", "maintenance"],
    queryFn: ({ pageParam }) => getMaintenance({ limit: 100, cursor: pageParam || undefined }),
    initialPageParam: "",
    getNextPageParam: (page) => page.nextCursor || undefined,
  });
  const windows = query.data?.pages.flatMap((page) => page.windows ?? []) ?? [];
  const pager = usePager(windows);

  const onChanged = () => {
    void qc.invalidateQueries({ queryKey: ["alerting", "maintenance"] });
    // The range-bounded lists elsewhere hold the same rows; a delete here must
    // not leave a card in another tab drawing a band for a window that is gone.
    void qc.invalidateQueries({ queryKey: ["maintenance"] });
    void qc.invalidateQueries({ queryKey: ["investigate", "maintenance"] });
  };

  return (
    <SectionCard
      title={t("maintenance.heading")}
      blurb={withNodes(t("maintenance.blurb"), {
        investigate: <SurfaceLink to="/investigate">{t("link.investigate")}</SurfaceLink>,
        explore: <SurfaceLink to="/explore">{t("link.explore")}</SurfaceLink>,
      })}
    >
      {query.isError ? (
        <ErrorLine onRetry={() => void query.refetch()}>
          {queryErrorMessage(query.error, t("maintenance.unavailable"))}
        </ErrorLine>
      ) : null}
      {/* isPending / isSuccess, the guard every list on this page uses: a paused
          retry is pending-but-not-fetching, and presenting that as "none
          declared" would be a settled answer nobody gave. */}
      {query.isPending ? <ListSkeleton /> : null}
      {query.isSuccess && windows.length === 0 ? (
        <EmptyState
          compact
          className="mt-4"
          title={t("maintenance.empty")}
          body={t("maintenance.empty.body")}
          /* A window is declared beside a chart, so the slate points at one. */
          action={<SurfaceLink to="/investigate">{t("maintenance.empty.action")}</SurfaceLink>}
        />
      ) : null}
      {windows.length > 0 ? (
        <>
        <ul aria-label={t("maintenance.listAria")} className="mt-4 divide-y divide-border">
          {pager.visible.map((w) => (
            /* The SHARED row (components/maintenance.tsx): same confirm-delete,
               same compact stamp, same write guard. canWrite is true by
               construction — this whole section is behind maintenance:write. */
            <MaintenanceRow key={w.id} window={w} canWrite onChanged={onChanged} />
          ))}
        </ul>
        <Pager pager={pager} subject={t("maintenance.subject")} className="px-0" />
        {query.hasNextPage ? (
          <div className="mt-3 flex items-center gap-3">
            <Button
              size="sm"
              variant="outline"
              loading={query.isFetchingNextPage}
              onClick={() => void query.fetchNextPage()}
            >
              {t("maintenance.loadMore")}
            </Button>
            {/* A failed page is a note BESIDE the button, never the loss of the
                pages that succeeded. */}
            {query.isError ? (
              <span role="alert" className="text-xs text-health-bad">
                {queryErrorMessage(query.error, t("maintenance.unavailable"))}
              </span>
            ) : null}
          </div>
        ) : null}
        </>
      ) : null}
    </SectionCard>
  );
}

/* ── page ───────────────────────────────────────────────────────────────── */

/**
 * AlertingPage's floor is alerts:read, and without it the page asks for NOTHING —
 * pages/targets.tsx's first degraded state.
 */
export function AlertingPage() {
  const t = useT(alertingDict);
  const { me, can } = useAuth();
  const canRead = can("alerts:read");
  const canManage = can("alerts:manage");
  /* maintenance:WRITE, not :read — see MaintenanceSection. Deliberately NOT
     behind the alerts:read floor: a window is not an alert rule, and the role
     that could manage windows on /settings must not lose them in the move. */
  const canMaintenance = can("maintenance:write");

  let body: ReactNode;
  if (me === undefined) {
    body = (
      <Card role="status" aria-live="polite" className="p-6">
        <span className="sr-only">{t("loading")}</span>
        <Skeleton className="h-10 w-full" />
      </Card>
    );
  } else {
    body = (
      <>
        {!canRead ? (
          <PermissionCard permission="alerts:read">{t("gate.read")}</PermissionCard>
        ) : (
          <>
            <RulesSection canManage={canManage} />
            <ForeignSection canManage={canManage} />
          </>
        )}
        {canMaintenance ? <MaintenanceSection /> : null}
      </>
    );
  }

  return (
    /* The title is the same word the sidebar's nav.alerting uses. */
    <PageShell title={t("title")} help={{ body: t("help.body"), slug: "alerting" }} description={t("description")}>
      {body}
    </PageShell>
  );
}
