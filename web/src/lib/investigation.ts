/** investigation.ts — timeline assembly and correlation for Investigation Mode. */

import type { Translate } from "./i18n";
import { enT, type InvestigationSourcesKey } from "./i18n/dict/investigation-sources";

/** T is this module's translator, the OPTIONAL TRAILING parameter defaulting to
 *  `enT` that lib/investigation-sources.ts's every renderer already takes — so
 *  every existing call, fixture and English assertion answers the same bytes. */
type T = Translate<InvestigationSourcesKey>;

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
  /** Tooltip (title attribute) for the detail line — the machine identity a
   *  human-readable detail replaced, e.g. the raw audit subjectKind:subjectId. */
  detailTitle?: string;
  /** Deep-link target AND the dedupe identity. Two entries carrying the same
   *  (kind, id) are the same fact seen twice, whatever else differs. */
  ref?: { kind: TimelineKind; id: string };
  /**
   * True when this row records a READ rather than a change. Set by
   * lib/investigation-sources.ts's auditEntries, honoured by rankCauses below,
   * and ignored by the timeline itself: a read belongs in the history, it just
   * never belongs in a list of things that could have caused an outage
   * (QA scope 3, finding #8).
   */
  readOnly?: boolean;
}

/**
 * compareEntries is the total order the timeline is presented in: time first; it is total rather
 * than "good enough" so that the page renders identically no matter which source's fetch resolved.
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
 * mergeTimeline folds every source into one ascending timeline; dedupe is by ref only, and the
 * seen-set is nested (kind → ids) rather than a joined string key.
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

/** Loss above 1% and RTT above 2× the range median are the two signals an operator already reads the matrix for. */
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
 * median over the defined samples of one signal; even-length series take the mean of the two middle
 * samples (the textbook definition, so the number in a tooltip is the number an operator would
 * compute by hand).
 */
function median(values: number[]): number | null {
  if (values.length === 0) return null;
  const sorted = [...values].sort((a, b) => a - b);
  const mid = sorted.length >> 1;
  return sorted.length % 2 === 1 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2;
}

/**
 * crossings walks one signal in time order and emits EDGES; a level-triggered timeline would put a
 * row on every sample of a ten-minute outage and bury the causes among them.
 */
function crossings(
  samples: { at: Date; value: number }[],
  threshold: number,
  signal: "loss" | "rtt",
  describe: (value: number) => string,
  t: T,
): TimelineEntry[] {
  const out: TimelineEntry[] = [];
  let above = false;
  for (const sample of samples) {
    /* An UNREADABLE instant never becomes an edge. `sample.at.toISOString()`
       below is the ref id, and it THROWS on an Invalid Date — so one malformed
       row in a proxied PromQL response used to take the whole page down rather
       than one sample out of a series. lib/investigation-sources.ts's
       samplesFromMatrix is the place that stops building such a sample; this is
       the line that stops it mattering, because thresholdCrossings is public and
       a caller may assemble its own series. */
    if (!Number.isFinite(sample.at.getTime())) continue;
    const isAbove = sample.value > threshold;
    if (isAbove === above) continue;
    above = isAbove;
    const iso = sample.at.toISOString();
    out.push({
      at: sample.at,
      kind: "threshold",
      severity: isAbove ? "warn" : "info",
      /* The KEY is assembled from the two closed enumerations this function
         already switches on, so the four headlines stay four keys and the type
         checker still sees a member of the union. */
      title: t(`entry.threshold.${signal}.${isAbove ? "above" : "recovered"}` as InvestigationSourcesKey),
      detail: describe(sample.value),
      /* The id is built from the SIGNAL and the instant, never from the title:
         mergeTimeline dedupes on it and the export permalinks by it, so it must
         read the same bytes in both languages. */
      ref: { kind: "threshold", id: `${signal}:${isAbove ? "above" : "recovered"}:${iso}` },
    });
  }
  return out;
}

const formatPct = (ratio: number) => `${(ratio * 100).toFixed(2)}%`;
const formatMs = (ns: number) => `${(ns / 1e6).toFixed(2)}ms`;

/** thresholdCrossings derives timeline entries from the scope's own signals. */
export function thresholdCrossings(
  series: SignalSample[],
  rules: ThresholdRules = DEFAULT_THRESHOLDS,
  t: T = enT,
): TimelineEntry[] {
  const ordered = [...series].sort((a, b) => a.at.getTime() - b.at.getTime());

  const lossSamples = ordered
    .filter((s) => s.loss !== undefined)
    .map((s) => ({ at: s.at, value: s.loss as number }));
  const lossEntries = crossings(
    lossSamples,
    rules.lossPct / 100,
    "loss",
    (v) => t("entry.threshold.loss.detail", { value: formatPct(v), threshold: `${rules.lossPct}%` }),
    t,
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
            t("entry.threshold.rtt.detail", {
              value: formatMs(v),
              threshold: formatMs(baseline * rules.rttFactor),
              factor: rules.rttFactor,
              median: formatMs(baseline),
            }),
          t,
        );

  return mergeTimeline(lossEntries, rttEntries);
}

/** anomalyOnset — the documented onset definition: the earliest threshold CROSSING in the timeline. */
export function anomalyOnset(entries: TimelineEntry[]): Date | null {
  let onset: Date | null = null;
  for (const entry of entries) {
    if (entry.kind !== "threshold" || entry.severity === "info") continue;
    if (onset === null || entry.at.getTime() < onset.getTime()) onset = entry.at;
  }
  return onset;
}

/** Plausible, one step removed. 1 — maintenance: a window EXPLAINS a degradation rather than implicating anything. */
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
 * rankCauses ranks candidate causes by temporal proximity BEFORE the onset; linear rather than
 * exponential because it is the shape an operator can verify by eye against the "N seconds before"
 * label.
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
    /* A READ is never a cause (QA scope 3, finding #8). The audit log records
       every authorization DECISION, so the console's own GETs — and the two
       PromQL POSTs the Investigate page itself fires to draw its charts —
       arrived here as weight-2 "config changes" and out-ranked the real ones.
       The rows keep their place in the timeline; they just stop being suspects. */
    if (entry.readOnly === true) continue;
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

/* The client-side pagination that used to live here now serves every list in
   the console — see lib/pagination.ts and components/ui/pager.tsx. */
