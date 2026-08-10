import type { RankedCause, SignalSample, TimelineEntry, TimelineKind } from "./investigation";
import type {
  Alert,
  Annotation,
  AuditEntry,
  Incident,
  K8sEvent,
  LiveEvent,
  MaintenanceWindow,
  PathSnapshot,
  PinnedRef,
  PromResult,
  RunDetail,
} from "./types";
import { stampFull, type Locale, type Translate } from "./i18n";
import { enT, investigationSourcesDict, type InvestigationSourcesKey } from "./i18n/dict/investigation-sources";
import { escapeLabelValue } from "./utils";

/** T is this module's translator, spelled once. Every function that renders a
 *  sentence takes one as an OPTIONAL TRAILING parameter defaulting to `enT`,
 *  which is what keeps every existing call, fixture and English assertion
 *  answering the same bytes — the shape pages/alerting.tsx's parsePromDuration
 *  and pages/settings.tsx's parseBundle already use. */
type T = Translate<InvestigationSourcesKey>;

/**
 * investigation-sources.ts — the PURE half of Investigation Mode's page: the scope vocabulary and
 * its URL encoding.
 */

/* ── the scope vocabulary ───────────────────────────────────────────────── */

export type ScopeKind = "pair" | "node" | "target" | "zone-pair" | "cluster";

export const SCOPE_KINDS: ScopeKind[] = ["pair", "node", "target", "zone-pair", "cluster"];

/** PAIR_SEPARATOR is U+2192, the SAME arrow internal/console/events/
 *  live_event.go's pairScope and pages/pair-card.tsx's pairScope use. It is not
 *  cosmetic: a pair investigation filters GET /api/v1/events by exact scope
 *  equality, so a hyphen-arrow here would silently match nothing. */
export const PAIR_SEPARATOR = "→";

export interface InvestigationScope {
  kind: ScopeKind;
  /** node name | target name | source node | source zone. "" for cluster. */
  a: string;
  /** destination node | destination zone. "" for the single-object kinds. */
  b: string;
}

export interface InvestigationParams extends InvestigationScope {
  from: Date;
  to: Date;
}

export const DEFAULT_RANGE_SECONDS = 60 * 60;

export const RANGE_PRESETS = [
  { value: "15m", label: "15m", seconds: 15 * 60 },
  { value: "1h", label: "1h", seconds: 60 * 60 },
  { value: "6h", label: "6h", seconds: 6 * 60 * 60 },
  { value: "custom", label: "Custom", seconds: 0 },
] as const;

export type RangePreset = (typeof RANGE_PRESETS)[number]["value"];

function parseInstant(raw: string | null): Date | null {
  if (!raw) return null;
  const d = new Date(raw);
  return Number.isNaN(d.getTime()) ? null : d;
}

/**
 * parseInvestigationParams resolves a query string into the investigation it names; it is TOTAL:
 * every malformed input degrades to something the page can actually fetch rather than to an error
 * state.
 */
export function parseInvestigationParams(search: string, now: Date): InvestigationParams {
  const qs = new URLSearchParams(search);
  const rawKind = qs.get("kind") ?? "";
  const kind = (SCOPE_KINDS as string[]).includes(rawKind) ? (rawKind as ScopeKind) : "cluster";

  const rawScope = qs.get("scope") ?? "";
  let a = "";
  let b = "";
  if (kind === "pair" || kind === "zone-pair") {
    const i = rawScope.indexOf(PAIR_SEPARATOR);
    if (i === -1) a = rawScope;
    else {
      a = rawScope.slice(0, i);
      b = rawScope.slice(i + PAIR_SEPARATOR.length);
    }
  } else if (kind !== "cluster") {
    a = rawScope;
  }

  const parsedFrom = parseInstant(qs.get("from"));
  const parsedTo = parseInstant(qs.get("to"));
  const span = DEFAULT_RANGE_SECONDS * 1000;
  const to = parsedTo ?? (parsedFrom ? new Date(parsedFrom.getTime() + span) : now);
  const from = parsedFrom ?? new Date(to.getTime() - span);

  return { kind, a, b, from, to };
}

/**
 * ignoredInvestigationParams names every parameter the URL carried that
 * parseInvestigationParams then THREW AWAY. The parser is total on purpose — a
 * `?kind=galaxy` renders the cluster rather than an error page — but total and
 * SILENT are two different things: an operator who mistyped a parameter got a
 * plausible default and no way to know their link was not being honoured
 * (QA scope 3, finding #14). Same contract lib/timemachine.tsx keeps for `?at=`,
 * which warns and then cleans the URL.
 *
 * Returned in URL order and empty for every well-formed link, including the bare
 * one: a default that was never overridden is not an ignored parameter.
 */
export function ignoredInvestigationParams(search: string): string[] {
  const qs = new URLSearchParams(search);
  const out: string[] = [];
  const rawKind = qs.get("kind");
  const kindOk = rawKind === null || (SCOPE_KINDS as string[]).includes(rawKind);
  if (!kindOk) out.push("kind");
  const rawScope = qs.get("scope");
  /* A scope is only ignored when the kind that survived has nowhere to put it:
     the cluster names no object. A scope alongside an unparseable kind is
     dropped for exactly that reason, so it is reported too. */
  if (rawScope !== null && rawScope !== "" && (kindOk ? rawKind === null || rawKind === "cluster" : true)) {
    out.push("scope");
  }
  for (const key of ["from", "to"] as const) {
    const raw = qs.get(key);
    if (raw !== null && parseInstant(raw) === null) out.push(key);
  }
  return out;
}

/**
 * exportFileName is the download's name. The instant is ISO with its COLONS
 * REPLACED (QA scope 3, finding #20): a colon is a path separator on classic
 * macOS, illegal in a Windows filename, and browsers silently mangle or refuse
 * such a download rather than saying so.
 */
export function exportFileName(from: Date): string {
  const iso = Number.isNaN(from.getTime()) ? "unknown" : from.toISOString().replace(/[:.]/g, "-");
  return `investigation-${iso}.json`;
}

/** scopeParamValue is the `?scope=` half of the permalink: the two-object kinds
 *  join with the arrow, the single-object kinds are the name itself, and the
 *  cluster has none. */
export function scopeParamValue(scope: InvestigationScope): string {
  if (scope.kind === "cluster") return "";
  if (scope.kind === "pair" || scope.kind === "zone-pair") {
    return scope.b === "" ? scope.a : `${scope.a}${PAIR_SEPARATOR}${scope.b}`;
  }
  return scope.a;
}

/** investigationParamsToSearch is parseInvestigationParams' exact inverse for
 *  every value the form can produce — the property a test pins, because a
 *  permalink that does not survive a reload is not a permalink. */
export function investigationParamsToSearch(p: InvestigationParams): string {
  const qs = new URLSearchParams();
  qs.set("kind", p.kind);
  const scope = scopeParamValue(p);
  if (scope !== "") qs.set("scope", scope);
  qs.set("from", p.from.toISOString());
  qs.set("to", p.to.toISOString());
  return `?${qs.toString()}`;
}

/* ── what the entry form is allowed to commit (QA round 3) ─────────────── */

/**
 * scopeIncompleteReason answers "may the Investigate button fire yet?" for a DRAFT scope; a pair of
 * one node with itself is refused too: the peer metric family carries no self-pair.
 */
export function scopeIncompleteReason(scope: InvestigationScope, t: T = enT): string | null {
  switch (scope.kind) {
    case "pair":
      if (scope.a === "") return t("scope.incomplete.sourceNode");
      if (scope.b === "") return t("scope.incomplete.destinationNode");
      if (scope.a === scope.b) return t("scope.incomplete.samePair");
      return null;
    case "zone-pair":
      if (scope.a === "") return t("scope.incomplete.sourceZone");
      if (scope.b === "") return t("scope.incomplete.destinationZone");
      return null;
    case "node":
      return scope.a === "" ? t("scope.incomplete.node") : null;
    case "target":
      return scope.a === "" ? t("scope.incomplete.target") : null;
    default:
      return null;
  }
}

/** WindowCommit is what commitWindow answers: the range to investigate, or the
 *  one sentence saying why there isn't one. `clamped` is true only when the
 *  Time Machine actually moved an edge — the banner is drawn from it, and a
 *  banner that appears when nothing changed is noise. */
export type WindowCommit =
  | { ok: true; from: Date; to: Date; clamped: boolean }
  | { ok: false; reason: string };

/**
 * PROMQL_MAX_RANGE_MS mirrors console.prometheus.maxRange, whose default is 24h
 * (internal/console/config/config.go) and whose refusal is
 * internal/console/promql/client.go's `end.Sub(start) > MaxRange`. It is a
 * CONSTANT rather than a value read from the API because /api/v1/config does not
 * publish the guards — it answers `prometheus: {configured}` and nothing more.
 *
 * It is deliberately NOT a commitWindow refusal. Only two of this page's nine
 * sources are range queries against Prometheus; the timeline's events,
 * annotations, audit rows, runs, path changes and maintenance windows are all
 * store-backed and have no such bound, so refusing a two-day window at the form
 * would take away an investigation the console can perfectly well answer in
 * order to spare two charts. The two charts state the bound themselves instead.
 */
export const PROMQL_MAX_RANGE_MS = 24 * 60 * 60 * 1000;

/** rangeExceedsPromBound answers whether the committed window is wider than a single query_range may be. */
export function rangeExceedsPromBound(from: Date, to: Date): boolean {
  return to.getTime() - from.getTime() > PROMQL_MAX_RANGE_MS;
}

/**
 * commitWindow is the ONE gate between the range fields and `params`; a window that lies ENTIRELY
 * after `t` cannot be clamped into anything meaningful (it would collapse to a point or invert).
 */
export function commitWindow(from: Date, to: Date, at: Date | null, t: T = enT): WindowCommit {
  if (from.getTime() >= to.getTime()) {
    return { ok: false, reason: t("window.inverted") };
  }
  if (at === null) return { ok: true, from, to, clamped: false };
  if (from.getTime() >= at.getTime()) {
    return { ok: false, reason: t("window.afterInstant") };
  }
  if (to.getTime() > at.getTime()) return { ok: true, from, to: at, clamped: true };
  return { ok: true, from, to, clamped: false };
}

/** The banner line the page shows under its header when commitWindow moved an edge. */
export const CLAMPED_BANNER = investigationSourcesDict.en["banner.clamped"];

/** scopeCaptionValue is what the annotation / maintenance bars should CALL the scope they are showing. */
export function scopeCaptionValue(scope: InvestigationScope, t: T = enT): string {
  return scopesToQuery(scope)[0] === undefined ? t("scope.allScopes") : scopeFilterValue(scope);
}

/* ── the scope selects' options (QA round 3, finding #5) ────────────────── */

/** The shape scopeNodeOptions/scopeZoneOptions read. Structural rather than
 *  lib/types.ts's Topology so a caller can pass a partial response (and a test
 *  a two-line fixture) without inventing the fields neither function reads. */
export interface ScopeOptionSource {
  nodes?: readonly { name: string; zone: string }[];
  agents?: readonly { nodeName: string; zone: string }[];
}

/** dedupeSorted drops "" (an absent name is not an option) and any repeat, and
 *  returns the rest in one stable order — the selects must not reshuffle
 *  between two renders of the same topology. */
function dedupeSorted(values: (string | undefined)[]): string[] {
  return [...new Set(values.filter((v): v is string => v !== undefined && v !== ""))].sort();
}

/** scopeNodeOptions is the node/pair selects' option list. */
export function scopeNodeOptions(topo: ScopeOptionSource | undefined): string[] {
  return dedupeSorted([...(topo?.nodes ?? []).map((n) => n.name), ...(topo?.agents ?? []).map((a) => a.nodeName)]);
}

/** scopeZoneOptions is the zone-pair selects' list, from the same two sources:
 *  agents carry their own `zone`, so a controller-less console can still name
 *  its failure domains. */
export function scopeZoneOptions(topo: ScopeOptionSource | undefined): string[] {
  return dedupeSorted([...(topo?.nodes ?? []).map((n) => n.zone), ...(topo?.agents ?? []).map((a) => a.zone)]);
}

/* ── the entry-point URL (plan Decision 11) ─────────────────────────────── */

export const INVESTIGATE_PATH = "/investigate";

/**
 * buildInvestigateURL is the SINGLE writer of an "Investigate this" link; it exists so the entry
 * contract has one spelling rather than four.
 */
export function buildInvestigateURL(
  scope: InvestigationScope,
  now: Date,
  rangeSeconds: number = DEFAULT_RANGE_SECONDS,
): string {
  const to = now;
  const from = new Date(to.getTime() - rangeSeconds * 1000);
  return `${INVESTIGATE_PATH}${investigationParamsToSearch({ ...scope, from, to })}`;
}

/* ── incident mode: the row is the authority (plan Decision 7) ──────────── */

/** incidentPermalink is /investigate?incident={id} — the ONLY parameter an incident link carries. */
export function incidentPermalink(id: string): string {
  return `${INVESTIGATE_PATH}?incident=${encodeURIComponent(id)}`;
}

/* The API's own maxLengths (docs/console-api.yaml's Incident/PinnedRef), named
   here so the form's maxLength attribute and the counter beside it cite ONE
   number rather than three literals that can drift apart from the schema. */
export const INCIDENT_TITLE_MAX = 255;
export const INCIDENT_NOTES_MAX = 16384;
export const PIN_NOTE_MAX = 512;

/** PinKind is the store's CLOSED pin vocabulary (internal/console/store/
 *  incidents.go's pinnedKinds), narrowed straight off the generated schema so
 *  widening it server-side is a compile error here rather than a silent 422. */
export type PinKind = PinnedRef["kind"];

/**
 * PIN_KIND_BY_TIMELINE_KIND maps a timeline row's class onto that closed set; three of the nine are
 * deliberately UNPINNABLE.
 */
export const PIN_KIND_BY_TIMELINE_KIND: Record<TimelineKind, PinKind | null> = {
  event: "event",
  audit: "audit",
  annotation: "annotation",
  "path-change": "snapshot",
  run: "run",
  k8s: "k8s",
  maintenance: null,
  threshold: null,
  alert: null,
};

/** pinKey is the identity a pinned list is deduped and toggled by: the store
 *  kind and the id, joined for a Set. Two refs of different kinds may share an
 *  id (a bigint 7 is a plausible event AND k8s row), so the kind is part of the
 *  key, never the id alone. */
export function pinKey(ref: { kind: string; id: string }): string {
  return `${ref.kind} ${ref.id}`;
}

/**
 * pinnedRefFor turns a timeline row into the PinnedRef the API stores, or null when this class of
 * row cannot be pinned at all.
 */
export function pinnedRefFor(entry: TimelineEntry): PinnedRef | null {
  const kind = PIN_KIND_BY_TIMELINE_KIND[entry.kind];
  if (kind === null || entry.ref === undefined || entry.ref.id === "") return null;
  return { kind, id: entry.ref.id };
}

/**
 * scopeFromIncidentScope reads an incident's stored `scope` back into the page's scope vocabulary;
 * without that permission the list is empty and a target incident reopens as a NODE scope.
 */
export function scopeFromIncidentScope(scope: string, targetNames: readonly string[]): InvestigationScope {
  if (scope === "") return { kind: "cluster", a: "", b: "" };
  const i = scope.indexOf(PAIR_SEPARATOR);
  if (i !== -1) {
    return { kind: "pair", a: scope.slice(0, i), b: scope.slice(i + PAIR_SEPARATOR.length) };
  }
  if (targetNames.includes(scope)) return { kind: "target", a: scope, b: "" };
  return { kind: "node", a: scope, b: "" };
}

/**
 * incidentParams is what `?incident={id}` hydrates the page with; an ABSENT toAt is an OPEN-ENDED
 * incident ("from then until further notice", the opposite of an annotation's absent endAt) and
 * frames to `now`.
 */
export function incidentParams(incident: Incident, targetNames: readonly string[], now: Date): InvestigationParams {
  const scope = scopeFromIncidentScope(incident.scope, targetNames);
  const parsedTo = incident.toAt === undefined ? null : parseInstant(incident.toAt);
  const to = parsedTo ?? now;
  const from = parseInstant(incident.fromAt) ?? new Date(to.getTime() - DEFAULT_RANGE_SECONDS * 1000);
  return { ...scope, from, to };
}

/**
 * scopeFilterValue is the ANNOTATIONS/EVENTS scope vocabulary — "" global; a zone pair answers ""
 * because that vocabulary simply has no zone member: no annotation.
 */
export function scopeFilterValue(scope: InvestigationScope): string {
  switch (scope.kind) {
    case "pair":
      return `${scope.a}${PAIR_SEPARATOR}${scope.b}`;
    case "node":
    case "target":
      return scope.a;
    default:
      return "";
  }
}

/**
 * scopesToQuery turns a scope into the list of `scope` parameter values a three-state endpoint
 * (annotations, maintenance, incidents) must be asked for.
 */
export function scopesToQuery(scope: InvestigationScope): (string | undefined)[] {
  const value = scopeFilterValue(scope);
  return value === "" ? [undefined] : [value, ""];
}

/* ── the scope's own PromQL ─────────────────────────────────────────────── */

const METRICS_PREFIX = "kconmon_ng";
const RATE_WINDOW = "5m";

/**
 * peerSelector is the label selector for a scope inside the PEER metric family
 * ({source_node, destination_node, source_zone, destination_zone} — docs/
 * metrics.md). "" for the cluster: an empty selector is the whole fleet, and
 * emitting `{}` instead would be the same query with more punctuation.
 */
function peerSelector(scope: InvestigationScope): string {
  switch (scope.kind) {
    case "pair":
      return `source_node="${escapeLabelValue(scope.a)}",destination_node="${escapeLabelValue(scope.b)}"`;
    case "node":
      return `source_node="${escapeLabelValue(scope.a)}"`;
    case "zone-pair":
      return `source_zone="${escapeLabelValue(scope.a)}",destination_zone="${escapeLabelValue(scope.b)}"`;
    default:
      return "";
  }
}

/**
 * externalSelector is the EXTERNAL family's ({source_node, source_zone, target, target_kind}) — a
 * target scope's only home.
 */
function externalSelector(scope: InvestigationScope): string {
  return `target="${escapeLabelValue(scope.a)}"`;
}

const braces = (sel: string) => (sel === "" ? "" : `{${sel}}`);

const withResult = (sel: string, result: string) => (sel === "" ? `{result="${result}"}` : `{${sel},result="${result}"}`);

/**
 * tag adds a synthetic label so `or` unions the operands instead of letting the first non-empty one
 * shadow the rest.
 */
const tag = (expr: string, value: string) => `label_replace(${expr}, "signal", "${value}", "", "")`;

/**
 * investigationLossQuery is the scope's packet loss as ONE series; the empty-sum trap needs a
 * BINARY operator to bite, and there is none here.
 */
export function investigationLossQuery(scope: InvestigationScope): string {
  if (scope.kind === "target") {
    return `max(${METRICS_PREFIX}_external_packet_loss_ratio${braces(externalSelector(scope))})`;
  }
  const sel = braces(peerSelector(scope));
  return (
    "max(" +
    `${tag(`${METRICS_PREFIX}_icmp_packet_loss_ratio${sel}`, "icmp")} or ` +
    `${tag(`${METRICS_PREFIX}_udp_packet_loss_ratio${sel}`, "udp")}` +
    ")"
  );
}

/**
 * investigationRttQuery is the scope's RTT p95 as one series: the WORST of the
 * three peer protocols' p95s (a mesh runs whichever check types it was
 * configured for, and an ICMP-only query would read as "no RTT" on a TCP-only
 * fleet), or the external RTT histogram for a target.
 */
export function investigationRttQuery(scope: InvestigationScope): string {
  const p95 = (metric: string, sel: string) =>
    `histogram_quantile(0.95, sum by (le) (rate(${metric}${sel}[${RATE_WINDOW}])))`;
  if (scope.kind === "target") {
    return `max(${p95(`${METRICS_PREFIX}_external_rtt_seconds_bucket`, braces(externalSelector(scope)))})`;
  }
  const sel = braces(peerSelector(scope));
  return (
    "max(" +
    `${tag(p95(`${METRICS_PREFIX}_tcp_total_duration_seconds_bucket`, sel), "tcp")} or ` +
    `${tag(p95(`${METRICS_PREFIX}_udp_rtt_seconds_bucket`, sel), "udp")} or ` +
    `${tag(p95(`${METRICS_PREFIX}_icmp_rtt_seconds_bucket`, sel), "icmp")}` +
    ")"
  );
}

/**
 * orZero guards ONE aggregate against being absent; a bare `sum(rate(...))` over a metric family
 * that has no series at all evaluates to the EMPTY vector.
 */
const orZero = (expr: string) => `(${expr} or vector(0))`;

/** investigationFailRatioQuery is the matrix delta chip's number; every per-protocol sum wears orZero above. */
export function investigationFailRatioQuery(scope: InvestigationScope): string {
  if (scope.kind === "target") {
    const sel = externalSelector(scope);
    const m = `${METRICS_PREFIX}_external_results_total`;
    return (
      `${orZero(`sum(rate(${m}${withResult(sel, "fail")}[${RATE_WINDOW}]))`)} / ` +
      `${orZero(`sum(rate(${m}${braces(sel)}[${RATE_WINDOW}]))`)}`
    );
  }
  const sel = peerSelector(scope);
  const protocols = ["tcp", "udp", "icmp"];
  const fails = protocols
    .map((p) => orZero(`sum(rate(${METRICS_PREFIX}_${p}_results_total${withResult(sel, "fail")}[${RATE_WINDOW}]))`))
    .join(" + ");
  const totals = protocols
    .map((p) => orZero(`sum(rate(${METRICS_PREFIX}_${p}_results_total${braces(sel)}[${RATE_WINDOW}]))`))
    .join(" + ");
  return `(${fails}) / (${totals})`;
}

/* ── PromQL responses → the lib's SignalSample[] ────────────────────────── */

interface MatrixSeriesEntry {
  metric: Record<string, string>;
  values: [number, string][];
}

function firstSeriesValues(res: PromResult | undefined): [number, string][] {
  if (!res || res.status !== "success" || res.data?.resultType !== "matrix") return [];
  const series = (res.data.result ?? []) as MatrixSeriesEntry[];
  return series[0]?.values ?? [];
}

/**
 * samplesFromMatrix folds the loss and RTT range responses onto ONE ascending series of samples for
 * lib/investigation.ts's thresholdCrossings; the two responses are merged BY TIMESTAMP rather than
 * zipped by index.
 */
export function samplesFromMatrix(loss: PromResult | undefined, rtt: PromResult | undefined): SignalSample[] {
  const byMs = new Map<number, SignalSample>();
  const at = (seconds: number) => {
    const ms = Math.round(seconds * 1000);
    let sample = byMs.get(ms);
    if (!sample) {
      sample = { at: new Date(ms) };
      byMs.set(ms, sample);
    }
    return sample;
  };
  for (const [ts, raw] of firstSeriesValues(loss)) {
    const v = Number(raw);
    if (Number.isFinite(v)) at(ts).loss = v;
  }
  for (const [ts, raw] of firstSeriesValues(rtt)) {
    const v = Number(raw);
    if (Number.isFinite(v)) at(ts).rttNs = v * 1e9;
  }
  return [...byMs.values()].sort((x, y) => x.at.getTime() - y.at.getTime());
}

/* ── sources → TimelineEntry[] ──────────────────────────────────────────── */

export const inRange = (d: Date, from: Date, to: Date) => d.getTime() >= from.getTime() && d.getTime() <= to.getTime();

export function validAt(raw: string): Date | null {
  const d = new Date(raw);
  return Number.isNaN(d.getTime()) ? null : d;
}

/** eventEntries: the fleet's own live events (agent restarts, node NotReady,
 *  MTR triggers). The window and the scope are the SERVER's filter here, so
 *  nothing is re-filtered client-side. */
export function eventEntries(events: LiveEvent[]): TimelineEntry[] {
  const out: TimelineEntry[] = [];
  for (const e of events) {
    const at = validAt(e.timestamp);
    if (!at) continue;
    out.push({
      at,
      kind: "event",
      severity: e.severity,
      title: e.summary,
      detail: e.scope === "" ? e.type : `${e.type} · ${e.scope}`,
      ref: { kind: "event", id: e.id },
    });
  }
  return out;
}

/**
 * auditDetailLine renders the row's allow-listed `detail` map next to who did it; the map is
 * whatever the per-route allow-list let through.
 */
export function auditDetailLine(row: AuditEntry): string {
  const kv = Object.entries(row.detail ?? {})
    .map(([k, v]) => `${k}=${typeof v === "string" ? v : JSON.stringify(v)}`)
    .join(" ");
  return [`${row.subjectKind}:${row.subjectId}`, row.resource, row.outcome, kv].filter((s) => s !== "").join(" · ");
}

/**
 * READ_ONLY_AUDIT_POSTS is the CLOSED list of POST routes that only read. A POST
 * is a change by default and this list is the exception the console makes for
 * itself: PromQL is expressed as a request body, so evaluating a query has to be
 * a POST even though it stores nothing.
 *
 * THE CONSTRAINT, and it is the whole reason the list is spelled out rather than
 * pattern-matched: a route belongs here ONLY if it cannot change any state a
 * later request could observe. Adding a route that does write would hide a real
 * configuration change from the cause ranking, which is the most expensive kind
 * of wrong an investigation surface can be.
 */
const READ_ONLY_AUDIT_POSTS = ["/api/v1/promql/query", "/api/v1/promql/query_range"];

/**
 * isReadOnlyAudit answers "did this audited request only READ?" from the row's
 * `action`, which the API defines as a method plus a route pattern ("POST
 * /api/v1/runs"). GET, plus the two PromQL POSTs above — nothing else.
 */
export function isReadOnlyAudit(action: string): boolean {
  const space = action.indexOf(" ");
  const method = (space === -1 ? action : action.slice(0, space)).toUpperCase();
  if (method === "GET") return true;
  const route = space === -1 ? "" : action.slice(space + 1).trim();
  return method === "POST" && READ_ONLY_AUDIT_POSTS.includes(route);
}

/** auditEntries: configuration changes; CLIENT-SIDE window filtering, and the only source here that needs. */
export function auditEntries(rows: AuditEntry[], from: Date, to: Date): TimelineEntry[] {
  const out: TimelineEntry[] = [];
  for (const row of rows) {
    const at = validAt(row.at);
    if (!at || !inRange(at, from, to)) continue;
    out.push({
      at,
      kind: "audit",
      severity: row.outcome === "denied" ? "warn" : row.outcome === "error" ? "error" : "info",
      title: row.action,
      detail: auditDetailLine(row),
      /* The row STAYS in the timeline — the badge already says "audit" out loud
         — and is only kept out of the cause candidates (finding #8). */
      readOnly: isReadOnlyAudit(row.action),
      ref: { kind: "audit", id: String(row.id) },
    });
  }
  return out;
}

/** annotationEntries: the operator notes already on this scope's charts, as
 *  rows. CAUSE_WEIGHTS scores them 0 — a note ABOUT a problem is never its
 *  cause — so they are context, never a suspect. */
export function annotationEntries(annotations: Annotation[], t: T = enT): TimelineEntry[] {
  const out: TimelineEntry[] = [];
  for (const a of annotations) {
    const at = validAt(a.startAt);
    if (!at) continue;
    out.push({
      at,
      kind: "annotation",
      severity: "info",
      /* The note's own text is the title, verbatim — an operator wrote it and
         this console does not paraphrase it. Only "" → «глобальная» moves. */
      title: a.text,
      detail: `${a.scope === "" ? t("scope.global") : a.scope} · ${a.createdBy}`,
      ref: { kind: "annotation", id: a.id },
    });
  }
  return out;
}

/** pathChangeEntries: one row per DISTINCT route the pair has taken, at the
 *  instant that route was first seen. Filtered to the window client-side — GET
 *  /api/v1/mtr/snapshots pages by last_seen and takes no time filter. */
export function pathChangeEntries(snapshots: PathSnapshot[], from: Date, to: Date, t: T = enT): TimelineEntry[] {
  const out: TimelineEntry[] = [];
  for (const s of snapshots) {
    const at = validAt(s.firstSeen);
    if (!at || !inRange(at, from, to)) continue;
    out.push({
      at,
      kind: "path-change",
      severity: "warn",
      title: t("entry.pathChange.title", { src: s.sourceNode, sep: PAIR_SEPARATOR, dst: s.destination }),
      detail: t("entry.pathChange.detail", {
        hash: s.pathHash.slice(0, 12),
        hops: s.hopCount,
        traces: s.traceCount,
      }),
      /* The id is the snapshot's, never the title's: mergeTimeline dedupes on
         it and a pin permalinks by it, so it must read the same in both
         languages. */
      ref: { kind: "path-change", id: s.id },
    });
  }
  return out;
}

const RUN_SEVERITY: Record<string, TimelineEntry["severity"]> = {
  failed: "error",
  partial: "warn",
  cancelled: "warn",
};

/** runEntries: the diagnostic runs that touched this scope. Weight 0 in
 *  CAUSE_WEIGHTS — a probe fired AT the problem is not what started it. */
export function runEntries(runs: RunDetail[], t: T = enT): TimelineEntry[] {
  const out: TimelineEntry[] = [];
  for (const r of runs) {
    const at = validAt(r.createdAt);
    if (!at) continue;
    out.push({
      at,
      /* `r.type` and `r.status` go through untranslated: they are the runs
         enum, the same tokens the Diagnostics page's badges and the API
         itself use. */
      kind: "run",
      severity: RUN_SEVERITY[r.status] ?? "info",
      title: t("entry.run.title", { type: r.type, status: r.status }),
      detail: t("entry.run.detail", {
        ok: r.pairOk,
        total: r.pairTotal,
        by: `${r.initiatorKind}:${r.initiatorId}`,
      }),
      ref: { kind: "run", id: r.id },
    });
  }
  return out;
}

/** k8sEntries: the cluster moving the infrastructure under the probe. Weight 3
 *  — nothing explains a network symptom more directly except the route itself
 *  moving. */
export function k8sEntries(events: K8sEvent[]): TimelineEntry[] {
  const out: TimelineEntry[] = [];
  for (const e of events) {
    const at = validAt(e.eventTime);
    if (!at) continue;
    out.push({
      at,
      kind: "k8s",
      severity: e.type === "Warning" ? "warn" : "info",
      title: `${e.kind} ${e.name}: ${e.reason}`,
      detail: e.count > 1 ? `${e.message} (×${e.count})` : e.message,
      ref: { kind: "k8s", id: e.id },
    });
  }
  return out;
}

/** maintenanceEntries: one row at each declared window's START. */
export function maintenanceEntries(
  windows: MaintenanceWindow[],
  t: T = enT,
  locale: Locale = "en",
): TimelineEntry[] {
  const out: TimelineEntry[] = [];
  for (const w of windows) {
    const at = validAt(w.startAt);
    if (!at) continue;
    out.push({
      at,
      kind: "maintenance",
      severity: "info",
      title: t("entry.maintenance.title", { reason: w.reason }),
      /* The end stamp goes through lib/i18n's stampFull rather than a bare
         toLocaleString: it sits INSIDE a translated sentence, and the same
         window is drawn by the maintenance bar three inches away — one shape or
         the page is speaking two languages about one instant (finding #18). */
      detail: t("entry.maintenance.detail", {
        scope: w.scope === "" ? t("scope.global") : w.scope,
        until: stampFull(new Date(w.endAt), locale),
        by: w.createdBy,
      }),
      ref: { kind: "maintenance", id: w.id },
    });
  }
  return out;
}

/* ── source 9: firing alerts (M7 Task 8, plan Decision 6) ────────────────── */

/**
 * ALERT_SEVERITY maps Prometheus's severity LABEL onto the timeline's three levels; the label is a
 * free string (a foreign rule may carry anything, or nothing).
 */
const ALERT_SEVERITY: Record<string, TimelineEntry["severity"]> = {
  critical: "error",
  warning: "warn",
  info: "info",
};

/** alertLabelLine renders the label set deterministically (sorted by key) so
 *  the same alert produces the same line — and the same ref id — on every
 *  render and in every permalink. */
function alertLabelLine(labels: Record<string, string>): string {
  return Object.keys(labels)
    .sort()
    .map((k) => `${k}=${labels[k]}`)
    .join(" ");
}

/**
 * GET /api/v1/alerts serves current state and nothing else — no alert history endpoint exists
 * anywhere in this system.
 */
export function alertEntries(alerts: Alert[], from: Date, to: Date, t: T = enT): TimelineEntry[] {
  const out: TimelineEntry[] = [];
  for (const a of alerts) {
    if (a.state !== "firing") continue;
    const started = a.activeAt === undefined ? null : validAt(a.activeAt);
    if (started === null || started.getTime() > to.getTime()) continue;

    const labels = alertLabelLine(a.labels);
    const severity = ALERT_SEVERITY[a.severity] ?? "info";
    const before = started.getTime() < from.getTime();
    /* `a.severity` is the rule's own LABEL — a free string a foreign rule may
       have written — and renders verbatim. Only its ABSENCE is this console's
       word to choose. */
    const severityText = a.severity === "" ? t("entry.alert.noSeverity") : a.severity;
    out.push({
      at: before ? from : started,
      kind: "alert",
      severity,
      title: before
        ? t("entry.alert.title.before", { name: a.name })
        : t("entry.alert.title", { name: a.name }),
      detail: before
        ? t("entry.alert.detail.before", { severity: severityText, since: started.toISOString(), labels })
        : t("entry.alert.detail", { severity: severityText, labels }),
      /* The identity is the SERIES, not the rule: one rule fires once per label
         set, and keying on ruleId (or on the name) would let mergeTimeline
         dedupe two genuinely different firing pairs into one row. */
      ref: { kind: "alert", id: `${a.name}{${labels}}` },
    });
  }
  return out;
}

/**
 * scopeFromAlertLabels reads an investigation scope out of an alert's labels; the label names are
 * the probe metrics' own (internal/metrics/prometheus.go, restated in
 * internal/console/alerting/render.go's groupBy lists).
 */
export function scopeFromAlertLabels(labels: Record<string, string>): InvestigationScope | null {
  const at = (key: string): string => labels[key] ?? "";
  const source = at("source_node");
  const destination = at("destination_node");
  const target = at("target");
  if (source !== "" && destination !== "") return { kind: "pair", a: source, b: destination };
  if (source !== "") return { kind: "node", a: source, b: "" };
  if (target !== "") return { kind: "target", a: target, b: "" };
  return null;
}

/** runTouchesScope filters already-fetched run details down to the ones whose SPEC names this scope. */
export function runTouchesScope(spec: unknown, scope: InvestigationScope): boolean {
  if (scope.kind === "cluster" || scope.kind === "zone-pair") return true;
  if (typeof spec !== "object" || spec === null) return false;
  const s = spec as { Sources?: unknown; Destinations?: unknown; TypedDestinations?: unknown };
  const names = (v: unknown): string[] => (Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : []);
  const sources = names(s.Sources);
  const destinations = names(s.Destinations);
  const covers = (list: string[], name: string) => list.length === 0 || list.includes(name);

  if (scope.kind === "target") {
    if (!Array.isArray(s.TypedDestinations)) return false;
    return s.TypedDestinations.some((d) => {
      if (typeof d !== "object" || d === null) return false;
      const dest = d as { kind?: unknown; name?: unknown };
      return dest.kind === "target" && dest.name === scope.a;
    });
  }
  if (scope.kind === "node") return covers(sources, scope.a) || covers(destinations, scope.a);
  return covers(sources, scope.a) && covers(destinations, scope.b);
}

/* ── export ─────────────────────────────────────────────────────────────── */

export interface ExportPayload {
  params: { kind: ScopeKind; scope: string; from: string; to: string };
  entries: {
    at: string;
    kind: string;
    severity: string;
    title: string;
    detail?: string;
    ref?: { kind: string; id: string };
  }[];
  causes: { at: string; kind: string; title: string; score: number }[];
}

/** buildExportPayload is what the Export button downloads: the parameters, the assembled timeline and the ranking. */
export function buildExportPayload(
  params: InvestigationParams,
  entries: TimelineEntry[],
  causes: RankedCause[],
): ExportPayload {
  return {
    params: {
      kind: params.kind,
      scope: scopeParamValue(params),
      from: params.from.toISOString(),
      to: params.to.toISOString(),
    },
    entries: entries.map((e) => ({
      at: e.at.toISOString(),
      kind: e.kind,
      severity: e.severity,
      title: e.title,
      detail: e.detail,
      ref: e.ref,
    })),
    causes: causes.map((c) => ({
      at: c.entry.at.toISOString(),
      kind: c.entry.kind,
      title: c.entry.title,
      score: c.score,
    })),
  };
}
