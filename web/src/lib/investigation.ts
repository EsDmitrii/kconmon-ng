/**
 * investigation.ts — timeline assembly and correlation for Investigation Mode.
 *
 * Pure TypeScript: no fetch, no React, no dates read off the wall clock. Every
 * source the Investigate page merges (events, audit rows, annotations, MTR path
 * changes, runs, K8s events, maintenance windows, derived threshold crossings)
 * is fetched elsewhere and arrives here already shaped as TimelineEntry[].
 * Assembly being client-side is plan Decision 1; correlation being documented
 * heuristics rather than a model is Decision 2.
 *
 * The exported constants here are the AUTHORITY for the ranking rules.
 * docs/console/product/INVESTIGATION.md restates them for the operator and
 * cites these names; if the two ever disagree, the doc is the one that is wrong.
 */

/** The closed set of timeline sources. Closed on purpose: CAUSE_WEIGHTS has to
 *  score every one of them, and an open union would let a new source arrive
 *  silently unweighted (a test asserts the two stay in sync). */
export type TimelineKind =
  | "event"
  | "audit"
  | "annotation"
  | "path-change"
  | "run"
  | "k8s"
  | "maintenance"
  | "threshold"
  | "alert";

export interface TimelineEntry {
  at: Date;
  kind: TimelineKind;
  severity: "info" | "warn" | "error";
  /** One human line. The row's headline — never a JSON blob. */
  title: string;
  /** Optional second line: the numbers behind the headline. */
  detail?: string;
  /** Deep-link target AND the dedupe identity. Two entries carrying the same
   *  (kind, id) are the same fact seen twice, whatever else differs. */
  ref?: { kind: TimelineKind; id: string };
}

/**
 * compareEntries is the total order the timeline is presented in: time first,
 * then kind, then ref id. It is total rather than "good enough" so that the
 * page renders identically no matter which source's fetch resolved first —
 * an operator comparing two permalinks of the same window must see one answer.
 *
 * Entries without a ref sort as if their id were "", i.e. ahead of referenced
 * siblings of the same kind and instant. Remaining full ties are left to
 * Array#sort's stability, which preserves the caller's source order.
 */
function compareEntries(a: TimelineEntry, b: TimelineEntry): number {
  const byTime = a.at.getTime() - b.at.getTime();
  if (byTime !== 0) return byTime;
  if (a.kind !== b.kind) return a.kind < b.kind ? -1 : 1;
  const aId = a.ref?.id ?? "";
  const bId = b.ref?.id ?? "";
  if (aId !== bId) return aId < bId ? -1 : 1;
  return 0;
}

/**
 * mergeTimeline folds every source into one ascending timeline.
 *
 * Dedupe is by ref only, and the seen-set is nested (kind → ids) rather than a
 * joined string key — no separator means no id can impersonate another ref by
 * containing one. The same K8s event can reach the page twice (a watch re-list
 * after expiry re-emits it) and the same audit row can be both an audit entry
 * and the thing an annotation points at; keeping the EARLIEST copy keeps the
 * timeline honest about when the fact first existed rather than when this
 * particular fetch happened to observe it. Refless entries are never deduped —
 * without an identity there is no evidence two rows are the same fact.
 *
 * Inputs are not mutated (flat() already returns a fresh array).
 */
export function mergeTimeline(...sources: TimelineEntry[][]): TimelineEntry[] {
  const sorted = sources.flat().sort(compareEntries);
  const seen = new Map<TimelineKind, Set<string>>();
  const out: TimelineEntry[] = [];
  for (const entry of sorted) {
    if (entry.ref) {
      let ids = seen.get(entry.ref.kind);
      if (ids === undefined) {
        ids = new Set<string>();
        seen.set(entry.ref.kind, ids);
      }
      if (ids.has(entry.ref.id)) continue;
      ids.add(entry.ref.id);
    }
    out.push(entry);
  }
  return out;
}

export interface ThresholdRules {
  /** Loss percentage, NOT a ratio: 1 means "1%", compared against a sample's
   *  `loss` ratio as lossPct / 100. */
  lossPct: number;
  /** Multiple of the series median RTT that counts as degraded. */
  rttFactor: number;
}

/**
 * DEFAULT_THRESHOLDS — plan Decision 2(b), and the numbers INVESTIGATION.md
 * quotes. Loss above 1% and RTT above 2× the range median are the two signals
 * an operator already reads the matrix for; the derived timeline just says out
 * loud when they crossed.
 */
export const DEFAULT_THRESHOLDS: ThresholdRules = { lossPct: 1, rttFactor: 2 };

/** One sample of the scope's primary signals. Both fields are optional because
 *  loss and RTT come from different PromQL queries and can be sampled at
 *  different resolutions — an absent field means "not measured here", never 0. */
export interface SignalSample {
  at: Date;
  /** Packet loss as a RATIO (0…1), matching kconmon_ng_*_packet_loss_ratio. */
  loss?: number;
  /** RTT in nanoseconds, matching the store's ns duration convention. */
  rttNs?: number;
}

/**
 * median over the defined samples of one signal.
 *
 * The median — not the mean — is the documented baseline choice, and the reason
 * is the thing being detected: a spike drags a mean up toward itself, so a
 * mean-based bar rises exactly when the anomaly arrives and can shrug off the
 * degradation that produced it. The median of a mostly-healthy range stays at
 * the healthy level however violent the excursion. Even-length series take the
 * mean of the two middle samples (the textbook definition, so the number in a
 * tooltip is the number an operator would compute by hand).
 */
function median(values: number[]): number | null {
  if (values.length === 0) return null;
  const sorted = [...values].sort((a, b) => a - b);
  const mid = sorted.length >> 1;
  return sorted.length % 2 === 1 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2;
}

/**
 * crossings walks one signal in time order and emits EDGES, not levels.
 *
 * A level-triggered timeline would put a row on every sample of a ten-minute
 * outage and bury the causes among them. Edge semantics give exactly one entry
 * when the signal goes above the bar and one info entry when it comes back, so
 * the row count is the number of things that happened.
 *
 * "Above" is strict: a sample sitting exactly on the threshold has not crossed
 * it. No recovery entry is emitted for a series that ends while still above —
 * it has not recovered, and claiming otherwise at the right edge of the window
 * would be a lie the operator cannot see.
 */
function crossings(
  samples: { at: Date; value: number }[],
  threshold: number,
  signal: "loss" | "rtt",
  describe: (value: number) => string,
): TimelineEntry[] {
  const label = signal === "loss" ? "Packet loss" : "RTT";
  const out: TimelineEntry[] = [];
  let above = false;
  for (const sample of samples) {
    const isAbove = sample.value > threshold;
    if (isAbove === above) continue;
    above = isAbove;
    const iso = sample.at.toISOString();
    out.push({
      at: sample.at,
      kind: "threshold",
      severity: isAbove ? "warn" : "info",
      title: isAbove ? `${label} crossed the threshold` : `${label} recovered`,
      detail: describe(sample.value),
      ref: { kind: "threshold", id: `${signal}:${isAbove ? "above" : "recovered"}:${iso}` },
    });
  }
  return out;
}

const formatPct = (ratio: number) => `${(ratio * 100).toFixed(2)}%`;
const formatMs = (ns: number) => `${(ns / 1e6).toFixed(2)}ms`;

/**
 * thresholdCrossings derives timeline entries from the scope's own signals.
 *
 * The series is sorted before detection — edge state is order-dependent, and
 * range queries from two different Prometheus panels do not arrive interleaved.
 * The RTT baseline is the median of the WHOLE series (see median()), which
 * means the entries depend on the window the operator chose; that is intended
 * and is what "2× the range median" says.
 *
 * A median of zero yields no RTT entries at all: with a zero baseline every
 * measurable RTT is infinitely above it, so the honest read is "no baseline",
 * not "everything is an anomaly".
 */
export function thresholdCrossings(
  series: SignalSample[],
  rules: ThresholdRules = DEFAULT_THRESHOLDS,
): TimelineEntry[] {
  const ordered = [...series].sort((a, b) => a.at.getTime() - b.at.getTime());

  const lossSamples = ordered
    .filter((s) => s.loss !== undefined)
    .map((s) => ({ at: s.at, value: s.loss as number }));
  const lossEntries = crossings(
    lossSamples,
    rules.lossPct / 100,
    "loss",
    (v) => `${formatPct(v)} (threshold ${rules.lossPct}%)`,
  );

  const rttSamples = ordered
    .filter((s) => s.rttNs !== undefined)
    .map((s) => ({ at: s.at, value: s.rttNs as number }));
  const baseline = median(rttSamples.map((s) => s.value));
  const rttEntries =
    baseline === null || baseline <= 0
      ? []
      : crossings(
          rttSamples,
          baseline * rules.rttFactor,
          "rtt",
          (v) =>
            `${formatMs(v)} (threshold ${formatMs(baseline * rules.rttFactor)} = ` +
            `${rules.rttFactor}× median ${formatMs(baseline)})`,
        );

  return mergeTimeline(lossEntries, rttEntries);
}

/**
 * anomalyOnset — the documented onset definition: the earliest threshold
 * CROSSING in the timeline.
 *
 * Recoveries are threshold entries too and carry severity "info"; they are
 * excluded, because "when did it get better" is not "when did it start". Null
 * means nothing crossed, and the correlation panel has nothing to rank against
 * rather than a fabricated anchor.
 */
export function anomalyOnset(entries: TimelineEntry[]): Date | null {
  let onset: Date | null = null;
  for (const entry of entries) {
    if (entry.kind !== "threshold" || entry.severity === "info") continue;
    if (onset === null || entry.at.getTime() < onset.getTime()) onset = entry.at;
  }
  return onset;
}

/**
 * CAUSE_WEIGHTS — plan Decision 2(c), the class weights, and the table
 * INVESTIGATION.md restates.
 *
 * 3 — the infrastructure under the probe moved: a route changed (path-change)
 *     or the cluster changed a node/pod (k8s). Nothing explains a network
 *     symptom more directly.
 * 2 — the fleet or its configuration changed: a topology/agent event (event) or
 *     a console config write (audit). Plausible, one step removed.
 * 1 — maintenance: a window EXPLAINS a degradation rather than implicating
 *     anything; it belongs in the ranking so the operator stops looking, and
 *     below the real suspects so it never outranks one.
 * 0 — never a cause: annotations and runs are things a human did ABOUT the
 *     problem, and threshold entries are the symptom itself. Ranking a symptom
 *     as its own cause is the classic way these panels start lying. `alert`
 *     (M7 Task 8) joins them for exactly the threshold row's reason: a firing
 *     alert is a RESTATEMENT of the symptom — usually of the very series the
 *     threshold row already derived — so weighting it above zero would let the
 *     page rank a page about the outage as the outage's cause, and rank it
 *     twice.
 *
 * TODO(M7 Task 13): this table gained `alert: 0`. INVESTIGATION.md restates
 * CAUSE_WEIGHTS verbatim and names this file as the authority, so the doc's
 * table must gain the same row.
 */
export const CAUSE_WEIGHTS: Record<TimelineKind, number> = {
  "path-change": 3,
  k8s: 3,
  event: 2,
  audit: 2,
  maintenance: 1,
  annotation: 0,
  run: 0,
  threshold: 0,
  alert: 0,
};

/** The candidate window, in seconds, before the onset. Five minutes is long
 *  enough to catch a rollout that started before the probes noticed and short
 *  enough that an unrelated change an hour earlier cannot claim credit. */
export const DEFAULT_CAUSE_WINDOW_SECONDS = 300;

export interface RankedCause {
  entry: TimelineEntry;
  score: number;
}

/**
 * rankCauses ranks candidate causes by temporal proximity BEFORE the onset.
 *
 * Candidate = weight > 0 AND at <= onset AND (onset - at) <= window. Entries
 * after the onset are excluded outright: something that happened after the loss
 * started did not start it, and letting "close in time" mean "close in either
 * direction" is how correlation panels end up blaming the pager.
 *
 * score = weight * (1 - delta / window) — linear decay, so the class weight is
 * a ceiling reached only at the onset itself and an entry at the far edge of
 * the window scores 0 (present, listed, claiming nothing). Linear rather than
 * exponential because it is the shape an operator can verify by eye against the
 * "N seconds before" label next to it.
 *
 * Ties break newest-first, then by kind and ref id so the order is total and
 * reproducible across permalinks. A non-positive or non-finite window is not a
 * window: no candidates.
 *
 * Entries are returned by reference (not copied) — the panel deep-links off the
 * same objects the timeline rendered. The input array is not mutated.
 */
export function rankCauses(
  entries: TimelineEntry[],
  onset: Date,
  opts?: { windowSeconds?: number },
): RankedCause[] {
  const windowSeconds = opts?.windowSeconds ?? DEFAULT_CAUSE_WINDOW_SECONDS;
  if (!Number.isFinite(windowSeconds) || windowSeconds <= 0) return [];

  const onsetMs = onset.getTime();
  const ranked: RankedCause[] = [];
  for (const entry of entries) {
    const weight = CAUSE_WEIGHTS[entry.kind] ?? 0;
    if (weight <= 0) continue;
    const deltaSeconds = (onsetMs - entry.at.getTime()) / 1000;
    if (deltaSeconds < 0 || deltaSeconds > windowSeconds) continue;
    ranked.push({ entry, score: weight * (1 - deltaSeconds / windowSeconds) });
  }

  return ranked.sort((a, b) => {
    if (a.score !== b.score) return b.score - a.score;
    const byTime = b.entry.at.getTime() - a.entry.at.getTime();
    if (byTime !== 0) return byTime;
    if (a.entry.kind !== b.entry.kind) return a.entry.kind < b.entry.kind ? -1 : 1;
    const aId = a.entry.ref?.id ?? "";
    const bId = b.entry.ref?.id ?? "";
    if (aId !== bId) return aId < bId ? -1 : 1;
    return 0;
  });
}
