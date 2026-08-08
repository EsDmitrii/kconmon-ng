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
import { escapeLabelValue } from "./utils";

/**
 * investigation-sources.ts — the PURE half of Investigation Mode's page: the
 * scope vocabulary and its URL encoding, the PromQL each scope produces, and
 * the mappers that turn each source's API rows into lib/investigation.ts's
 * TimelineEntry.
 *
 * It sits beside lib/investigation.ts (merge + correlation) rather than inside
 * pages/investigate.tsx for the same reason lib/annotations.ts sits beside
 * components/annotations.tsx: this is where the decisions an operator can
 * DISAGREE with live — which label family a target belongs to, what counts as a
 * run touching a pair, which sources need a client-side window filter — and
 * every one of them is unit-testable without mounting a page or stubbing fetch.
 *
 * No React, no fetch, no wall clock: `now` is a parameter wherever a default
 * range is needed, so a permalink resolves identically in a test and in a
 * browser.
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
 * parseInvestigationParams resolves a query string into the investigation it
 * names. It is TOTAL: every malformed input degrades to something the page can
 * actually fetch rather than to an error state, because this function's inputs
 * are hand-typed URLs, stale bookmarks and links pasted into chat.
 *
 * An unknown kind becomes "cluster" — the one scope that needs no object and
 * can therefore always be rendered. An unparseable instant falls back to the
 * default hour, anchored to the OTHER end when that one parsed, so a link
 * carrying a good `to` and a typo'd `from` still frames the moment its author
 * meant.
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

/* ── the entry-point URL (plan Decision 11) ─────────────────────────────── */

export const INVESTIGATE_PATH = "/investigate";

/**
 * buildInvestigateURL is the SINGLE writer of an "Investigate this" link — the
 * node/pair/target cards' header action and every matrix cell.
 *
 * It exists so the entry contract has one spelling rather than four. The URL is
 * the whole contract (Decision 11), which means a card that hand-assembled
 * `?scope=node-a-node-b` with a hyphen instead of PAIR_SEPARATOR would open an
 * investigation of a node that does not exist — silently, with an empty
 * timeline that reads as a quiet fleet. Composing investigationParamsToSearch
 * (parseInvestigationParams' own inverse) makes the round trip a property a
 * test can pin in both directions instead of a convention four call sites are
 * trusted to remember.
 *
 * `now` is a parameter, not `new Date()`: the same reason nothing else in this
 * file reads the wall clock — a link must resolve identically in a test and in
 * a browser. The default span is DEFAULT_RANGE_SECONDS, the hour the page
 * itself opens on, so arriving from a card and arriving from the sidebar frame
 * the same window.
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

/** incidentPermalink is /investigate?incident={id} — the ONLY parameter an
 *  incident link carries. Scope and range come from the row, so a link that
 *  also spelled them could disagree with the incident it names after a single
 *  edit. */
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
 * PIN_KIND_BY_TIMELINE_KIND maps a timeline row's class onto that closed set.
 *
 * Three of the nine are deliberately UNPINNABLE, and `null` is the honest
 * answer rather than a fallback kind:
 *   - `maintenance` — a declared window lives in maintenance_windows, a table
 *     the pin vocabulary has no member for. Pinning it as, say, "annotation"
 *     would store an id that resolves to nothing.
 *   - `threshold` — a derived row. Its "id" is a synthetic signal:direction:
 *     instant string computed from a PromQL response; there is no row anywhere
 *     to point at, and re-deriving it needs the query, not the id.
 *   - `alert` (M7 Task 8) — the same problem one step further out: a firing
 *     alert lives in PROMETHEUS, not in any table this console owns, and its
 *     id here is a fingerprint of the label set. It also stops existing the
 *     moment the alert resolves, so a pin would dangle by design.
 * The UI hides the pin toggle on all three rather than offering a control the
 * server would reject.
 *
 * `path-change` is the one RENAME: the timeline calls the class what an
 * operator calls it, and the store names the TABLE the id came from
 * (mtr_path_snapshots → "snapshot"). Same fact, two vocabularies, one map.
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
 * pinnedRefFor turns a timeline row into the PinnedRef the API stores, or null
 * when this class of row cannot be pinned at all. A row with no `ref` is also
 * null: without an identity there is nothing to point at.
 */
export function pinnedRefFor(entry: TimelineEntry): PinnedRef | null {
  const kind = PIN_KIND_BY_TIMELINE_KIND[entry.kind];
  if (kind === null || entry.ref === undefined || entry.ref.id === "") return null;
  return { kind, id: entry.ref.id };
}

/**
 * scopeFromIncidentScope reads an incident's stored `scope` back into the
 * page's scope vocabulary — the INVERSE of scopeFilterValue, and lossy in one
 * documented place.
 *
 * The stored string is the annotations/events vocabulary ("" global, a node
 * name, `src→dst`, a target name), which carries no KIND: a bare name is a node
 * and a target spelled identically. So the target list decides, and the caller
 * passes whatever names it holds (targets:read gated). Without that permission
 * the list is empty and a target incident reopens as a NODE scope — the page
 * says so next to the header strip rather than quietly querying the wrong
 * metric family.
 *
 * A wide scope ("" — a zone pair or a cluster investigation, both of which
 * store "") comes back as `cluster`: the one kind that needs no object and can
 * therefore always be rendered, the same degradation parseInvestigationParams
 * already makes for an unknown kind.
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
 * incidentParams is what `?incident={id}` hydrates the page with: the saved
 * scope and the saved range.
 *
 * An ABSENT toAt is an OPEN-ENDED incident ("from then until further notice",
 * the opposite of an annotation's absent endAt) and frames to `now` — which is
 * the honest read of "still going" and the only one that keeps the signal
 * charts covering the present. An unparseable fromAt degrades to the default
 * hour before `to` rather than to a NaN range that would render an empty page.
 */
export function incidentParams(incident: Incident, targetNames: readonly string[], now: Date): InvestigationParams {
  const scope = scopeFromIncidentScope(incident.scope, targetNames);
  const parsedTo = incident.toAt === undefined ? null : parseInstant(incident.toAt);
  const to = parsedTo ?? now;
  const from = parseInstant(incident.fromAt) ?? new Date(to.getTime() - DEFAULT_RANGE_SECONDS * 1000);
  return { ...scope, from, to };
}

/**
 * scopeFilterValue is the ANNOTATIONS/EVENTS scope vocabulary (plan Decision 6)
 * — "" global, a node name, a pair `src→dst`, a target name.
 *
 * A zone pair answers "" because that vocabulary simply has no zone member: no
 * annotation, event or maintenance window is ever filed against a zone pair, so
 * pretending one could be filtered by it would produce a permanently empty
 * result that looks like a quiet fleet. The wide scopes ask for EVERY scope
 * instead (see scopesToQuery below), which is the honest read of "investigate
 * two zones": all the context, none of it filtered by a key nothing writes.
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
 * scopesToQuery turns a scope into the list of `scope` parameter values a
 * three-state endpoint (annotations, maintenance, incidents) must be asked for.
 *
 *   wide scope (zone pair, cluster) → [undefined], i.e. ONE request with the
 *     parameter ABSENT: every scope, which is what "the whole cluster" means.
 *   narrow scope                    → [<name>, ""], i.e. TWO requests: this
 *     object's own marks and the GLOBAL ones, exactly as
 *     components/annotations.tsx's useAnnotations already does. The endpoint
 *     matches a scope EXACTLY and "" is a real value, not a wildcard, so there
 *     is no single request that returns both.
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

/** externalSelector is the EXTERNAL family's ({source_node, source_zone,
 *  target, target_kind}) — a target scope's only home. There is no
 *  destination_node there, which is why a target can never be queried through
 *  peerSelector and vice versa: the wrong family yields an EMPTY series, not a
 *  wrong number. */
function externalSelector(scope: InvestigationScope): string {
  return `target="${escapeLabelValue(scope.a)}"`;
}

const braces = (sel: string) => (sel === "" ? "" : `{${sel}}`);

const withResult = (sel: string, result: string) => (sel === "" ? `{result="${result}"}` : `{${sel},result="${result}"}`);

/** tag adds a synthetic label so `or` unions the operands instead of letting
 *  the first non-empty one shadow the rest — three expressions that all reduce
 *  to an EMPTY label set collide, and `a or b` would answer `a` alone. Same
 *  trick pages/pair-card.tsx's pairSeriesQuery and components/
 *  mtr-changes-timeline.tsx's pairLossQuery already use. */
const tag = (expr: string, value: string) => `label_replace(${expr}, "signal", "${value}", "", "")`;

/**
 * investigationLossQuery is the scope's packet loss as ONE series.
 *
 * `max(...)` over the union, not `avg` or `sum`: a loss RATIO is not additive
 * (two zones summed happily exceed 1.0), and for a scope covering many pairs
 * the honest one-number summary of "was this losing packets?" is the worst
 * observation in it. For a single pair the max IS the pair.
 *
 * The result feeds BOTH the signal chart and lib/investigation.ts's
 * thresholdCrossings, which is why it is one query and not two.
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
 * investigationFailRatioQuery is the matrix delta chip's number: failed probes
 * over all probes for the scope, the same ratio internal/console/matrix's
 * failRatioQuery computes per cell, aggregated across the three peer protocols
 * (or the external family for a target).
 */
export function investigationFailRatioQuery(scope: InvestigationScope): string {
  if (scope.kind === "target") {
    const sel = externalSelector(scope);
    const m = `${METRICS_PREFIX}_external_results_total`;
    return (
      `sum(rate(${m}${withResult(sel, "fail")}[${RATE_WINDOW}])) / ` +
      `sum(rate(${m}${braces(sel)}[${RATE_WINDOW}]))`
    );
  }
  const sel = peerSelector(scope);
  const protocols = ["tcp", "udp", "icmp"];
  const fails = protocols
    .map((p) => `sum(rate(${METRICS_PREFIX}_${p}_results_total${withResult(sel, "fail")}[${RATE_WINDOW}]))`)
    .join(" + ");
  const totals = protocols
    .map((p) => `sum(rate(${METRICS_PREFIX}_${p}_results_total${braces(sel)}[${RATE_WINDOW}]))`)
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
 * samplesFromMatrix folds the loss and RTT range responses onto ONE ascending
 * series of samples for lib/investigation.ts's thresholdCrossings.
 *
 * The two responses are merged BY TIMESTAMP rather than zipped by index: they
 * are separate queries, can be scraped at different resolutions and can have
 * different lengths, and a sample carrying the wrong partner's value would move
 * a threshold crossing to a time nothing happened. A field with no partner is
 * left ABSENT, which SignalSample documents as "not measured here" — never 0,
 * which would read as a perfect probe.
 *
 * RTT arrives in SECONDS from Prometheus and leaves in NANOSECONDS, the store's
 * duration unit and what SignalSample.rttNs declares.
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

/** auditDetailLine renders the row's allow-listed `detail` map next to who did
 *  it. The map is whatever the per-route allow-list let through — {} for almost
 *  everything (SECURITY.md's documented lossiness) — so the subject and the
 *  resource carry the line when it is empty, rather than an empty "{}". */
function auditDetailLine(row: AuditEntry): string {
  const kv = Object.entries(row.detail ?? {})
    .map(([k, v]) => `${k}=${typeof v === "string" ? v : JSON.stringify(v)}`)
    .join(" ");
  const who = `${row.subjectKind}:${row.subjectId}`;
  return kv === "" ? `${who} · ${row.resource} · ${row.outcome}` : `${who} · ${row.resource} · ${row.outcome} · ${kv}`;
}

/**
 * auditEntries: configuration changes.
 *
 * CLIENT-SIDE window filtering, and the only source here that needs it: GET
 * /api/v1/audit has no from/to at all (docs/console-api.yaml). The page says so
 * in its source list, because the consequence is real — a console busy enough
 * to fill the fetched page with newer rows can hide older in-range ones.
 *
 * A DENIED row is a warning rather than context: somebody tried to change
 * something during the incident window and was refused, which is exactly the
 * kind of thing an operator wants to see coloured.
 */
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
      ref: { kind: "audit", id: String(row.id) },
    });
  }
  return out;
}

/** annotationEntries: the operator notes already on this scope's charts, as
 *  rows. CAUSE_WEIGHTS scores them 0 — a note ABOUT a problem is never its
 *  cause — so they are context, never a suspect. */
export function annotationEntries(annotations: Annotation[]): TimelineEntry[] {
  const out: TimelineEntry[] = [];
  for (const a of annotations) {
    const at = validAt(a.startAt);
    if (!at) continue;
    out.push({
      at,
      kind: "annotation",
      severity: "info",
      title: a.text,
      detail: `${a.scope === "" ? "global" : a.scope} · ${a.createdBy}`,
      ref: { kind: "annotation", id: a.id },
    });
  }
  return out;
}

/** pathChangeEntries: one row per DISTINCT route the pair has taken, at the
 *  instant that route was first seen. Filtered to the window client-side — GET
 *  /api/v1/mtr/snapshots pages by last_seen and takes no time filter. */
export function pathChangeEntries(snapshots: PathSnapshot[], from: Date, to: Date): TimelineEntry[] {
  const out: TimelineEntry[] = [];
  for (const s of snapshots) {
    const at = validAt(s.firstSeen);
    if (!at || !inRange(at, from, to)) continue;
    out.push({
      at,
      kind: "path-change",
      severity: "warn",
      title: `Route changed: ${s.sourceNode} ${PAIR_SEPARATOR} ${s.destination}`,
      detail: `path ${s.pathHash.slice(0, 12)} · ${s.hopCount} hops · ${s.traceCount} traces`,
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
export function runEntries(runs: RunDetail[]): TimelineEntry[] {
  const out: TimelineEntry[] = [];
  for (const r of runs) {
    const at = validAt(r.createdAt);
    if (!at) continue;
    out.push({
      at,
      kind: "run",
      severity: RUN_SEVERITY[r.status] ?? "info",
      title: `${r.type} run ${r.status}`,
      detail: `${r.pairOk}/${r.pairTotal} ok · started by ${r.initiatorKind}:${r.initiatorId}`,
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

/**
 * maintenanceEntries: one row at each declared window's START.
 *
 * Deliberately NOT re-filtered to the range: the endpoint already answers the
 * windows that OVERLAP it, and a window that began before `from` and is still
 * running is precisely the one that explains the degradation. Its row therefore
 * sorts ahead of the range — which is honest about when it started, and is why
 * the detail line names both edges.
 */
export function maintenanceEntries(windows: MaintenanceWindow[]): TimelineEntry[] {
  const out: TimelineEntry[] = [];
  for (const w of windows) {
    const at = validAt(w.startAt);
    if (!at) continue;
    out.push({
      at,
      kind: "maintenance",
      severity: "info",
      title: `Maintenance: ${w.reason}`,
      detail: `${w.scope === "" ? "global" : w.scope} · until ${new Date(w.endAt).toLocaleString()} · ${w.createdBy}`,
      ref: { kind: "maintenance", id: w.id },
    });
  }
  return out;
}

/* ── source 9: firing alerts (M7 Task 8, plan Decision 6) ────────────────── */

/** ALERT_SEVERITY maps Prometheus's severity LABEL onto the timeline's three
 *  levels. The label is a free string (a foreign rule may carry anything, or
 *  nothing), so an unrecognised value reads as info rather than as an error —
 *  the console does not know what somebody else's word means, and colouring it
 *  red on a guess would be a claim it cannot support. */
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
 * alertEntries: the alerts Prometheus is firing NOW, placed on the window.
 *
 * THE HONESTY THIS SOURCE IS BUILT AROUND. GET /api/v1/alerts serves current
 * state and nothing else — no alert history endpoint exists anywhere in this
 * system — so this mapper can only say what is firing at fetch time, and it
 * says exactly that in two shapes:
 *
 *   - `activeAt` INSIDE the window → one row at activeAt. This alert started
 *     during the investigation, which is the fact a timeline wants.
 *   - `activeAt` BEFORE the window → one row at `from`, titled "already firing
 *     when this window opens". The row is deliberately NOT at the instant it
 *     names, so the detail spells the true start out in ISO — an operator
 *     reading a local clock in the left column must not be able to mistake the
 *     displaced row for a start inside the window.
 *
 * There are NO resolved rows, and their absence is a decision rather than an
 * omission: nothing records when an alert stopped. An alert missing from this
 * response might have resolved a second ago or an hour ago, or have never
 * fired in this window at all, and synthesizing a "resolved" row from an
 * absence would date an event that was never observed. The page's source note
 * states this out loud.
 *
 * PENDING alerts are dropped: pending is not fired (the same line the webhook
 * contract draws), and `activeAt` on a pending alert is when it went ACTIVE,
 * not when it fired — a row from it would be an early lie about a state that
 * may never arrive. An alert with no `activeAt` is dropped too: there is no
 * instant to place it at, and `from` is a claim, not a default.
 *
 * `to` bounds the future edge: an alert that started after the window closed
 * belongs to a different investigation.
 */
export function alertEntries(alerts: Alert[], from: Date, to: Date): TimelineEntry[] {
  const out: TimelineEntry[] = [];
  for (const a of alerts) {
    if (a.state !== "firing") continue;
    const started = a.activeAt === undefined ? null : validAt(a.activeAt);
    if (started === null || started.getTime() > to.getTime()) continue;

    const labels = alertLabelLine(a.labels);
    const severity = ALERT_SEVERITY[a.severity] ?? "info";
    const before = started.getTime() < from.getTime();
    out.push({
      at: before ? from : started,
      kind: "alert",
      severity,
      title: before ? `Alert ${a.name} — already firing when this window opens` : `Alert firing: ${a.name}`,
      detail: before
        ? `${a.severity === "" ? "no severity" : a.severity} · firing since ${started.toISOString()} · ${labels}`
        : `${a.severity === "" ? "no severity" : a.severity} · ${labels}`,
      /* The identity is the SERIES, not the rule: one rule fires once per label
         set, and keying on ruleId (or on the name) would let mergeTimeline
         dedupe two genuinely different firing pairs into one row. */
      ref: { kind: "alert", id: `${a.name}{${labels}}` },
    });
  }
  return out;
}

/**
 * scopeFromAlertLabels reads an investigation scope out of an alert's labels,
 * or null when they name nothing this page can open.
 *
 * The label names are the probe metrics' own (internal/metrics/prometheus.go,
 * restated in internal/console/alerting/render.go's groupBy lists), which is
 * why a rule built from a template lands here with a usable scope for free and
 * a raw expression only does so if its author grouped by the same labels.
 *
 * NULL IS THE POINT. A link built from labels that do not name an object opens
 * an investigation of something that does not exist — an empty timeline that
 * reads as a quiet fleet. The three shapes below are the only ones the label
 * set can support, and everything else gets no link rather than a wrong one:
 *   - source_node + destination_node → the pair
 *   - source_node alone              → that node
 *   - target alone                   → that external target
 * A destination_node with no source is deliberately NOT a node scope: the node
 * kind asks the peer metric family as a SOURCE, so it would silently answer a
 * different question. An empty label VALUE is an absent label, never a scope
 * named "".
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

/**
 * runTouchesScope filters already-fetched run details down to the ones whose
 * SPEC names this scope — the same client-side scan pages/target-card.tsx's
 * runsTouchingTarget does, and for the same reason: GET /api/v1/runs has no
 * scope filter, and a run's destinations live in the spec only GET
 * /api/v1/runs/{id} returns.
 *
 * The spec is the snapshot checks.Spec marshals into check_runs.spec, so its
 * keys are Go's exported field names while a typed Destination carries its own
 * lowercase json tags. An EMPTY sources/destinations list means "every node in
 * the current topology" (checks.Spec's own doc comment), which genuinely does
 * touch every pair — reading it as "no nodes" would hide exactly the
 * fleet-wide runs an investigation most wants to see.
 *
 * A zone pair and the cluster take every run: neither is expressible in a run
 * spec, and a run over the fleet is context for both.
 */
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

/**
 * buildExportPayload is what the Export button downloads: the parameters, the
 * assembled timeline and the ranking — the three things somebody would have to
 * reproduce by hand to check the console's work.
 *
 * Dates become ISO strings HERE rather than at JSON.stringify time, so the
 * shape is pinned by a test instead of by the serialiser's behaviour, and the
 * file reads the same in every timezone that opens it.
 */
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
