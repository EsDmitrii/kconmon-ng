import type { RunDetail, RunResult } from "./types";

/**
 * run-samples.ts is the run permalink's INTERVAL-RUN arithmetic: turning a
 * run's flat result list into the per-pair timeline and the aggregate the
 * detail page draws.
 *
 * It sits in lib/ rather than inside pages/run-detail.tsx for the reason
 * lib/investigation-sources.ts sits beside pages/investigate.tsx: these are
 * the numbers an operator will argue with -- which samples count towards a
 * latency percentile, what "fail%" is a percentage OF -- and every one of them
 * is unit-testable without mounting a page or stubbing fetch.
 *
 * No React, no fetch, no wall clock.
 */

/** One pair's samples, in completion order. */
export interface PairSamples {
  source: string;
  destination: string;
  samples: RunResult[];
}

/**
 * SampleAggregate is the interval run's headline: how many probes were sent,
 * how many failed, and the latency distribution of the ones that answered.
 *
 * `sent` counts every sample, failures included -- it is what was ATTEMPTED,
 * which is the denominator failRatio needs. The latency fields describe
 * SUCCESSFUL samples only and are undefined when there are none: a timed-out
 * probe's durationNs is how long the console waited before giving up, and
 * averaging that into a latency would report the timeout setting as though it
 * were the network's round trip -- the exact number an operator would then act
 * on. A run where everything failed therefore shows "0 ok" and no latency at
 * all, which is the honest answer.
 */
export interface SampleAggregate {
  sent: number;
  failed: number;
  /** failed / sent, 0..1. Zero when nothing was sent. */
  failRatio: number;
  minNs?: number;
  avgNs?: number;
  maxNs?: number;
  p95Ns?: number;
}

/**
 * runDurationNs reads the run's requested duration back out of its spec
 * snapshot, which is the ONLY place it is recorded -- check_runs has no
 * duration column, because the spec column already snapshots the whole
 * request (see checks.Spec.Duration).
 *
 * Total, like every other parser in this codebase's lib/: a spec that is not
 * an object, or carries no Duration, or carries a non-number, is an instant
 * run. That is also the truthful reading of every run created before the field
 * existed.
 */
export function runDurationNs(run: Pick<RunDetail, "spec"> | undefined): number {
  if (!run || typeof run.spec !== "object" || run.spec === null) return 0;
  const d = (run.spec as Record<string, unknown>).Duration;
  return typeof d === "number" && Number.isFinite(d) && d > 0 ? d : 0;
}

/** isIntervalRun is "did this run repeat its probes", the switch the detail
 *  page uses to decide whether the timeline and aggregate are worth showing at
 *  all. An instant run's single sample per pair is already fully described by
 *  the pair table. */
export function isIntervalRun(run: Pick<RunDetail, "spec"> | undefined): boolean {
  return runDurationNs(run) > 0;
}

/**
 * groupSamplesByPair buckets a run's results per (source, destination),
 * preserving the order the server returned them -- which is completion order
 * (GetRunResults is `ORDER BY id`), and therefore the timeline.
 *
 * Pairs come back in first-seen order rather than sorted, so the timeline's
 * rows keep the same vertical order as the pair table above them instead of
 * re-shuffling under the reader between two renders of the same run.
 */
export function groupSamplesByPair(results: RunResult[]): PairSamples[] {
  const byPair = new Map<string, PairSamples>();
  for (const r of results) {
    // NUL separator, the same convention use-run.ts's pairKey uses: no node
    // name can contain it, so the composite key is unambiguous.
    const key = `${r.sourceNode}\0${r.destinationNode}`;
    const existing = byPair.get(key);
    if (existing) {
      existing.samples.push(r);
      continue;
    }
    byPair.set(key, { source: r.sourceNode, destination: r.destinationNode, samples: [r] });
  }
  return [...byPair.values()];
}

/**
 * percentileNs is the NEAREST-RANK percentile over an already-sorted ascending
 * array: the smallest value at or above the p-th position. Nearest-rank rather
 * than interpolated because it always returns a latency that was actually
 * MEASURED -- an interpolated p95 of two samples is a number no probe ever
 * recorded, and on the small sample counts a short run produces (a 1m run is
 * twelve samples per pair) that is most of the time.
 */
export function percentileNs(sortedAsc: number[], p: number): number | undefined {
  if (sortedAsc.length === 0) return undefined;
  const rank = Math.ceil((p / 100) * sortedAsc.length);
  const idx = Math.min(Math.max(rank - 1, 0), sortedAsc.length - 1);
  return sortedAsc[idx];
}

/**
 * aggregateSamples reduces every sample in the run to one row of numbers.
 *
 * Called with the WHOLE run's results (not one pair's) it answers "how did
 * this run go"; called with one pair's, "how did this pair go". The caller
 * chooses the scope; the arithmetic is the same either way.
 */
export function aggregateSamples(results: RunResult[]): SampleAggregate {
  const sent = results.length;
  let failed = 0;
  const okLatencies: number[] = [];
  for (const r of results) {
    if (r.success) {
      okLatencies.push(r.durationNs);
    } else {
      failed += 1;
    }
  }
  const agg: SampleAggregate = {
    sent,
    failed,
    failRatio: sent === 0 ? 0 : failed / sent,
  };
  if (okLatencies.length === 0) return agg;

  okLatencies.sort((a, b) => a - b);
  agg.minNs = okLatencies[0];
  agg.maxNs = okLatencies[okLatencies.length - 1];
  agg.avgNs = okLatencies.reduce((sum, v) => sum + v, 0) / okLatencies.length;
  agg.p95Ns = percentileNs(okLatencies, 95);
  return agg;
}

/**
 * formatSampleCadence renders the cadence the server derived for a duration,
 * mirroring checks.SampleInterval EXACTLY (duration / 500, floored at 5s). It
 * is duplicated here rather than sent over the wire for the same reason
 * pages/diagnostics.tsx recomputes the pair count client-side: the operator is
 * being shown what a choice WILL do, before any request exists to answer it.
 * The server stays the enforcement point.
 */
export const MAX_SAMPLES_PER_PAIR = 500;
export const MIN_SAMPLE_INTERVAL_NS = 5_000_000_000;

export function sampleIntervalNs(durationNs: number): number {
  if (durationNs <= 0) return 0;
  return Math.max(Math.floor(durationNs / MAX_SAMPLES_PER_PAIR), MIN_SAMPLE_INTERVAL_NS);
}

/** plannedSamplesPerPair is how many probes one pair will contribute over
 *  durationNs -- what the duration selector shows so an operator picking "24h"
 *  can see it means 500 samples every ~173s and not 86 400 of them. */
export function plannedSamplesPerPair(durationNs: number): number {
  if (durationNs <= 0) return 1;
  const n = Math.floor(durationNs / sampleIntervalNs(durationNs));
  return Math.min(Math.max(n, 1), MAX_SAMPLES_PER_PAIR);
}

/** One pair's tick strip as a FRAME rather than a pile: slots, arrivals, and the tail. */
export interface PairProgress {
  expected: number;
  arrived: number;
  /** expected - arrived, never negative. */
  remaining: number;
  /** The tail as time: remaining × cadence. */
  remainingNs: number;
  /** False for a one-slot run, which is not worth framing. */
  framed: boolean;
}

/** The expected count is plannedSamplesPerPair, never a second derivation, so the strip cannot
 *  frame a different run than the create form promised. */
export function pairProgress(durationNs: number, arrived: number): PairProgress {
  // Widen rather than lie: cadence drift can land a thirteenth sample the floor did not predict.
  const expected = Math.max(plannedSamplesPerPair(durationNs), arrived);
  const remaining = Math.max(expected - arrived, 0);
  return {
    expected,
    arrived,
    // Counted in time, not in ticks, and off the cadence rather than a wall clock.
    remainingNs: remaining * sampleIntervalNs(durationNs),
    remaining,
    framed: expected > 1,
  };
}

/** A rendered span is a WORD, not a value: ru reads «5 с / 12 мин / 24 ч» while measured latencies
 *  and the selector's own range tokens ("1m", "24h") stay Latin. */
const DURATION_UNITS: Record<string, readonly [string, string, string]> = {
  en: ["s", "m", "h"],
  ru: [" с", " мин", " ч"],
};

/** Whole units only — "2m53s" would claim a precision a derived cadence does not have — and it
 *  lives here so the create form and the run permalink cannot drift on the same number. */
export function formatDurationNs(ns: number, locale: string): string {
  const [sec, min, hour] = DURATION_UNITS[locale] ?? DURATION_UNITS.en;
  const s = Math.round(ns / 1e9);
  if (s < 60) return `${s}${sec}`;
  if (s < 3600) return `${Math.round(s / 60)}${min}`;
  return `${Math.round(s / 3600)}${hour}`;
}
