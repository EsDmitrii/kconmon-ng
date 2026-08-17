import type { PathSnapshot, RunDetail, RunResult } from "./types";

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
    /* LENGTH-PREFIXED, not a bare NUL join. use-run.ts's pairKey separates the
       two names with a NUL on the grounds that no node name may carry one, and
       for node names that holds. This side also groups EXTERNAL destinations,
       which are whatever address the operator typed, and a separator argument
       is only as strong as its weakest input: ("a\0b" → "c") and ("a" → "b\0c")
       join to the same bytes and became ONE row of a strip that was supposed to
       be two. The source's own length disambiguates them for good. */
    const source = String(r.sourceNode ?? "");
    const destination = String(r.destinationNode ?? "");
    const key = `${source.length}\u0000${source}${destination}`;
    const existing = byPair.get(key);
    if (existing) {
      existing.samples.push(r);
      continue;
    }
    byPair.set(key, { source, destination, samples: [r] });
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
/**
 * isLatency is what may enter min/avg/p95/max: a finite, non-negative NUMBER.
 *
 * Everything else is a value that arrived where a latency was promised —
 * `undefined` from a row missing the field, the string "2000000" from a wire
 * that stringified it, a NaN, a clock that went backwards — and every one of
 * them poisoned the whole distribution rather than one sample: `0 + "2000000"`
 * is string concatenation, so one stringy row made the Avg tile read
 * "10000003000000ms", and one NaN made it read "NaNms". A sample whose latency
 * is not a latency is still SENT (it is counted above), it just has nothing to
 * contribute to a percentile.
 */
function isLatency(ns: unknown): ns is number {
  return typeof ns === "number" && Number.isFinite(ns) && ns >= 0;
}

export function aggregateSamples(results: RunResult[]): SampleAggregate {
  const sent = results.length;
  let failed = 0;
  const okLatencies: number[] = [];
  for (const r of results) {
    if (r.success) {
      if (isLatency(r.durationNs)) okLatencies.push(r.durationNs);
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
  // `!(x > 0)` rather than `x <= 0`, so a NaN duration is no duration instead of
  // a NaN cadence: every comparison against NaN is false, and the old spelling
  // let one through to be multiplied into a tick strip's remaining time.
  if (!(durationNs > 0) || !Number.isFinite(durationNs)) return 0;
  return Math.max(Math.floor(durationNs / MAX_SAMPLES_PER_PAIR), MIN_SAMPLE_INTERVAL_NS);
}

/** plannedSamplesPerPair is how many probes one pair will contribute over
 *  durationNs -- what the duration selector shows so an operator picking "24h"
 *  can see it means 500 samples every ~173s and not 86 400 of them. */
export function plannedSamplesPerPair(durationNs: number): number {
  const interval = sampleIntervalNs(durationNs);
  if (interval <= 0) return 1;
  const n = Math.floor(durationNs / interval);
  return Math.min(Math.max(n, 1), MAX_SAMPLES_PER_PAIR);
}

/** One pair's tick strip as a FRAME rather than a pile: slots, arrivals, and the tail. */
export interface PairProgress {
  /**
   * The PLAN's floor -- plannedSamplesPerPair, or the run's own snapshot -- and
   * never widened by what turned up. This is the number a caption may put behind
   * a "≥": the Go planner ships it as "at least N per pair", so "13 of ≥12" is
   * the truth about a run that overshot, where "13 of ≥13" invents a plan nobody
   * made and quietly hides that the run beat its floor.
   */
  planned: number;
  /** The drawn WIDTH: the floor, or the arrived count when that is larger. */
  expected: number;
  arrived: number;
  /** expected - arrived, never negative. */
  remaining: number;
  /** The tail as time: remaining × cadence. */
  remainingNs: number;
  /** False for a one-slot run, which is not worth framing. */
  framed: boolean;
}

/**
 * The expected count is plannedSamplesPerPair, never a second derivation, so the strip cannot
 * frame a different run than the create form promised.
 *
 * `cadence` is the run's EFFECTIVE plan when the caller knows it (see runCadence
 * below). Without it the base cadence stands, which is what every fast check
 * type runs at anyway — but for a stretched one it made "~30s left" out of a run
 * with nine minutes to go (rev13 acceptance).
 */
export function pairProgress(
  durationNs: number,
  arrived: number,
  cadence?: { intervalNs: number; samplesPerPair: number },
): PairProgress {
  /* Each of the three inputs is taken only if it IS a number; otherwise this
     falls back to its own derivation rather than carrying a NaN into the
     strip's slot count (Array.from({length: NaN}) is empty — a pair with no
     ticks at all) and into "~{remaining} left". */
  const planned =
    cadence && Number.isFinite(cadence.samplesPerPair) && cadence.samplesPerPair >= 1
      ? Math.floor(cadence.samplesPerPair)
      : plannedSamplesPerPair(durationNs);
  const intervalNs =
    cadence && Number.isFinite(cadence.intervalNs) && cadence.intervalNs > 0
      ? cadence.intervalNs
      : sampleIntervalNs(durationNs);
  const count = Number.isFinite(arrived) && arrived > 0 ? Math.floor(arrived) : 0;
  // Widen rather than lie: cadence drift can land a thirteenth sample the floor did not predict.
  const expected = Math.max(planned, count);
  const remaining = Math.max(expected - count, 0);
  return {
    planned,
    expected,
    arrived: count,
    // Counted in time, not in ticks, and off the cadence rather than a wall clock.
    remainingNs: remaining * intervalNs,
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
  /* A span that is not a number is drawn as no span. The alternative is what
     this used to do: `${Math.round(NaN / 1e9)}h` renders the literal word
     "NaNh" into a caption, and "Infinityh" into the cadence tile — which is the
     one thing the owner's bar says may never reach the screen. */
  const s = Number.isFinite(ns) ? Math.round(ns / 1e9) : 0;
  if (s < 60) return `${s}${sec}`;
  if (s < 3600) return `${Math.round(s / 60)}${min}`;
  return `${Math.round(s / 3600)}${hour}`;
}

/**
 * formatCadenceNs is formatDurationNs with one extra rule: a cadence that is not
 * a whole number of minutes is said in SECONDS.
 *
 * formatDurationNs rounds to whole units on purpose — a derived duration has no
 * business claiming "2m53s". A cadence does: the stretched MTR interval is 90s,
 * and rounding it to "2m" overstated by a third on the very tile that exists to
 * stop the cadence being misreported (rev13 acceptance). Whole minutes still
 * read as minutes, and hours as hours.
 */
export function formatCadenceNs(ns: number, locale: string): string {
  const { value, unit } = cadenceParts(ns);
  const [sec, min, hour] = DURATION_UNITS[locale] ?? DURATION_UNITS.en;
  return `${value}${unit === "second" ? sec : unit === "minute" ? min : hour}`;
}

/* ── a cadence said in WORDS, for prose ──────────────────────────────────────
   The tile spellings above are SI abbreviations, which is right for a tile: a
   number and its unit, standing alone in a `nums` column. Dropped into a
   sentence the Russian one falls apart — «раз в 5 с» puts a bare «с» where the
   reader's eye expects the preposition «с», and «Каждая пара трассируется раз в
   5 с на протяжении…» genuinely parses wrong on first read. The dictionaries
   around it already spell cadences out (pages/targets.tsx's fmtCadence says
   «каждую минуту», never «каждую 1 мин»), so prose gets words.

   The forms live here rather than in a dictionary for the reason DURATION_UNITS
   already does: these are the two spellings of ONE derived quantity, and
   splitting them across files is how the create form and the permalink drift on
   the same number. English is unchanged — "every 5s" reads fine in a sentence,
   and inventing "every 5 seconds" only for prose would put two English
   spellings on one screen. */
export type DurationUnitName = "second" | "minute" | "hour";

/**
 * cadenceParts is formatCadenceNs's own arithmetic, exposed: which unit a
 * cadence is said in, and how many of them. Both spellings read it, so an
 * abbreviation and a word can never disagree about the number in front of them.
 */
export function cadenceParts(ns: number): { value: number; unit: DurationUnitName } {
  /* A span that is not a number is drawn as no span. The alternative is what
     this used to do: `${Math.round(NaN / 1e9)}h` renders the literal word
     "NaNh" into a caption, and "Infinityh" into the cadence tile — which is the
     one thing the owner's bar says may never reach the screen. */
  const s = Number.isFinite(ns) ? Math.round(ns / 1e9) : 0;
  // Whole minutes read as minutes; 90s does NOT round up to "2m" (rev13).
  if (s < 60 || s % 60 !== 0) return { value: s, unit: "second" };
  if (s < 3600) return { value: Math.round(s / 60), unit: "minute" };
  return { value: Math.round(s / 3600), unit: "hour" };
}

/** ACCUSATIVE, because every sentence that interpolates one of these reaches it
 *  through «раз в …» / «не реже раза в …». The singular is the accusative one
 *  («минуту», not «минута»); the other two coincide with the nominative plural. */
const CADENCE_WORDS_RU: Record<DurationUnitName, readonly [string, string, string]> = {
  second: ["секунду", "секунды", "секунд"],
  minute: ["минуту", "минуты", "минут"],
  hour: ["час", "часа", "часов"],
};

/** The Russian plural rule, restated locally exactly as every dict/*.ts restates
 *  its own countForm — one shared copy would be a common table, which
 *  lib/i18n/README.md forbids. */
function ruForm(n: number): 0 | 1 | 2 {
  const teen = Math.abs(n) % 100;
  if (teen >= 11 && teen <= 14) return 2;
  const last = Math.abs(n) % 10;
  if (last === 1) return 0;
  if (last >= 2 && last <= 4) return 1;
  return 2;
}

/**
 * formatCadenceProse is the spelling a cadence takes INSIDE a sentence.
 *
 * English is formatCadenceNs verbatim. Russian spells the unit out, and drops
 * the numeral entirely at one — «раз в минуту», not «раз в 1 минуту», the same
 * call pages/targets.tsx's fmtCadence makes for the schedule cadences.
 */
export function formatCadenceProse(ns: number, locale: string): string {
  if (locale !== "ru") return formatCadenceNs(ns, locale);
  const { value, unit } = cadenceParts(ns);
  const forms = CADENCE_WORDS_RU[unit];
  if (value === 1) return forms[0];
  return `${value} ${forms[ruForm(value)]}`;
}

/* ── effective cadence: what a SLOW check type actually plans ─────────────── */

/**
 * A traceroute is not a probe with a timeout, it is thirty hops walked in
 * sequence, so its per-pair budget is a real expectation rather than a ceiling
 * on a failure. Mirrors checks.mtrMinPerPairTimeout.
 */
export const MTR_PER_PAIR_BUDGET_NS = 90_000_000_000;

/** Mirrors checks.maxConcurrency / checks.maxPerSourceConcurrency. */
export const MAX_CONCURRENCY = 8;
export const MAX_PER_SOURCE_CONCURRENCY = 2;

/**
 * perPairBudgetNs is how long ONE probe is expected to take. It is zero for
 * every type but mtr: a tcp/udp/icmp/dns/http probe answers in milliseconds and
 * its timeout only bounds one that has already failed, so planning a cadence
 * around it would slow down every healthy run. Mirrors checks.perPairBudget.
 */
export function perPairBudgetNs(checkType: string): number {
  return checkType === "mtr" ? MTR_PER_PAIR_BUDGET_NS : 0;
}

function ceilDiv(a: number, b: number): number {
  if (b <= 0) return a;
  return Math.max(Math.ceil(a / b), 1);
}

/**
 * asCount reads a COUNT off whatever the caller was handed.
 *
 * pairCount arrives as `run.pairTotal` — a field of a JSON body — and
 * sourceCount is derived from the run's own samples. A body with
 * `"pairTotal": "abc"` (or the field missing) used to make ceilDiv return NaN,
 * which multiplied by the MTR budget gave a NaN interval and put the literal
 * word "NaNs" on the Cadence tile of the run permalink. Anything that is not a
 * positive finite number is read as NONE, which the arithmetic below already
 * has an honest answer for: the base cadence, unstretched.
 */
function asCount(n: number): number {
  const v = typeof n === "number" ? n : Number(n);
  return Number.isFinite(v) && v > 0 ? Math.floor(v) : 0;
}

/**
 * effectiveSampleIntervalNs is the cadence the server will actually plan: the
 * base cadence, stretched to one round's floor when the check type is slower
 * than it. Mirrors checks.EffectiveSampleInterval, and like sampleIntervalNs it
 * is duplicated rather than fetched because the operator is being shown what a
 * choice WILL do, before any request exists to answer it. The server stays the
 * enforcement point and returns the authoritative numbers on create.
 */
export function effectiveSampleIntervalNs(
  durationNs: number,
  checkType: string,
  pairCount: number,
  sourceCount: number,
  requestedNs = 0,
): number {
  return planCadenceFor(durationNs, checkType, pairCount, sourceCount, requestedNs).intervalNs;
}

/**
 * baseSampleIntervalNs is what PACES the run: the operator's own requested
 * cadence when there is one, else the derived duration/500 floored at 5s.
 * Mirrors checks.BaseSampleInterval.
 *
 * MAX_SAMPLES_PER_PAIR binds either way — it is the hard ceiling on what one
 * pair may contribute, and a request cannot buy past it. MIN_SAMPLE_INTERVAL
 * deliberately does not: that 5s floor exists to stop an arithmetic accident
 * nobody chose, and an operator who picked 1s has chosen.
 */
export function baseSampleIntervalNs(durationNs: number, requestedNs = 0): number {
  const derived = sampleIntervalNs(durationNs);
  if (derived <= 0) return 0;
  // `!(x > 0)` so a NaN request is no request, the same guard sampleIntervalNs makes.
  if (!(requestedNs > 0) || !Number.isFinite(requestedNs)) return derived;
  return Math.max(requestedNs, Math.floor(durationNs / MAX_SAMPLES_PER_PAIR));
}

/** Why a plan is not the cadence that was asked for. Mirrors checks.IntervalCapped /
 *  checks.IntervalStretched, and "" is the honest majority: nothing moved. */
export type CadenceAdjustment = "" | "cap" | "round";

/** The whole cadence decision, so no caller has to reassemble it from two
 *  functions and guess which of its numbers is the measured one. Mirrors
 *  checks.Cadence. */
export interface PlannedCadence {
  /** What was asked for; zero when nothing was. */
  requestedNs: number;
  /** What paces the run: the request or the derived cadence, capped. */
  baseNs: number;
  /** What the run is PLANNED to keep — a worst case, never an observation. */
  intervalNs: number;
  /** A floor, capped at MAX_SAMPLES_PER_PAIR: read it as "at least N per pair". */
  samplesPerPair: number;
  adjusted: CadenceAdjustment;
}

/**
 * planCadenceFor mirrors checks.PlanCadence: the cadence a run will keep, and
 * why that is not the cadence it was given.
 *
 * Duplicated rather than fetched for the reason sampleIntervalNs is: the
 * operator is being shown what a choice WILL do, before any request exists to
 * answer it. The server stays the enforcement point and returns the
 * authoritative numbers on create.
 */
export function planCadenceFor(
  durationNs: number,
  checkType: string,
  pairCount: number,
  sourceCount: number,
  requestedNs = 0,
): PlannedCadence {
  const requested = Number.isFinite(requestedNs) && requestedNs > 0 ? requestedNs : 0;
  const baseNs = baseSampleIntervalNs(durationNs, requested);
  const plan: PlannedCadence = {
    requestedNs: requested,
    baseNs,
    intervalNs: baseNs,
    samplesPerPair: 1,
    adjusted: "",
  };
  if (baseNs <= 0) return plan;

  const budget = perPairBudgetNs(checkType);
  const pairs = asCount(pairCount);
  const sources = asCount(sourceCount);
  if (budget > 0 && pairs > 0) {
    // The busiest agent carries pairCount/sourceCount of the plan; both gates drain before a round
    // ends and the slower one governs.
    const perSource = sources > 0 ? ceilDiv(pairs, sources) : pairs;
    const batches = Math.max(ceilDiv(pairs, MAX_CONCURRENCY), ceilDiv(perSource, MAX_PER_SOURCE_CONCURRENCY));
    const floor = batches * budget;
    if (floor > baseNs) {
      plan.intervalNs = floor;
      plan.adjusted = "round";
    }
  }
  // Nothing stretched it, so the only thing that could have moved it is the cap.
  if (plan.adjusted === "" && requested > 0 && baseNs > requested) plan.adjusted = "cap";
  plan.samplesPerPair = Math.min(Math.max(Math.floor(durationNs / plan.intervalNs), 1), MAX_SAMPLES_PER_PAIR);
  return plan;
}

/**
 * effectivePlannedSamplesPerPair is a FLOOR, not a target: how many probes one pair contributes if
 * every round takes its worst case. A duration run runs for the wall clock it asked for and starts
 * the next round as soon as the previous finishes, so a healthy run produces MORE than this — read
 * it as "at least N per pair". MAX_SAMPLES_PER_PAIR is the true upper bound. At least one, because
 * a duration shorter than a single round is one honest pass rather than an error.
 */
export function effectivePlannedSamplesPerPair(
  durationNs: number,
  checkType: string,
  pairCount: number,
  sourceCount: number,
  requestedNs = 0,
): number {
  // Covers a non-positive duration and a NaN one alike — planCadenceFor answers
  // one honest pass for both.
  return planCadenceFor(durationNs, checkType, pairCount, sourceCount, requestedNs).samplesPerPair;
}

/**
 * plannedCadenceFromSpec reads the cadence the server SNAPSHOTTED onto the run,
 * which is authoritative where the client's mirror is only a preview. Undefined
 * for an instant run and for runs created before the fields existed — callers
 * fall back to their own derivation, exactly as they did before.
 */
export function plannedCadenceFromSpec(
  run: Pick<RunDetail, "spec"> | undefined,
): { intervalNs: number; samplesPerPair: number } | undefined {
  if (!run || typeof run.spec !== "object" || run.spec === null) return undefined;
  const spec = run.spec as Record<string, unknown>;
  const intervalNs = spec.PlannedSampleIntervalNs;
  const samplesPerPair = spec.PlannedSamplesPerPair;
  if (typeof intervalNs !== "number" || !Number.isFinite(intervalNs) || intervalNs <= 0) return undefined;
  if (typeof samplesPerPair !== "number" || !Number.isFinite(samplesPerPair) || samplesPerPair < 1) return undefined;
  return { intervalNs, samplesPerPair };
}

/** What the run's cadence is, and whether the SERVER said so or this client derived it. */
export interface RunCadence {
  intervalNs: number;
  samplesPerPair: number;
  /** True when the numbers are the run's own snapshot rather than a mirror of the planner. */
  fromSpec: boolean;
}

/**
 * runCadence is the ONE cadence the permalink speaks with — the header tile and
 * every pair's progress frame.
 *
 * The bug it exists for: the tile read the BASE cadence straight off the spec's
 * duration (duration/500, floored at 5s), so a 15m MTR over four pairs claimed
 * "5s" while the run was actually executing one round every 90s. A traceroute is
 * thirty hops walked in sequence, not a probe with a timeout, so a cadence
 * shorter than one round is a cadence nothing can keep.
 *
 * Three answers in order of authority: the server's snapshot, this client's
 * mirror of checks.EffectiveSampleInterval for runs older than those fields, and
 * undefined for an instant run — which has no cadence to name at all.
 */
export function runCadence(
  run: Pick<RunDetail, "spec" | "type" | "pairTotal"> | undefined,
  sourceCount: number,
): RunCadence | undefined {
  const durationNs = runDurationNs(run);
  if (durationNs <= 0) return undefined;

  const snapshot = plannedCadenceFromSpec(run);
  if (snapshot) return { ...snapshot, fromSpec: true };

  const checkType = run?.type ?? "";
  const pairCount = run?.pairTotal ?? 0;
  return {
    intervalNs: effectiveSampleIntervalNs(durationNs, checkType, pairCount, sourceCount),
    samplesPerPair: effectivePlannedSamplesPerPair(durationNs, checkType, pairCount, sourceCount),
    fromSpec: false,
  };
}

/* ── observed cadence: what the run is ACTUALLY doing ─────────────────────── */

/**
 * ObservedCadence is measured, not planned: the spacing the samples already on
 * screen actually have.
 *
 * It exists because the permalink used to print the planner's worst-case floor
 * on a tile labelled only "Cadence", and an operator read it as a fact about
 * their run. It was «Периодичность 3 мин» on a run producing a probe about
 * every minute — the plan is an upper bound on the spacing (a round that
 * finishes early starts the next one immediately), so the real number is
 * routinely far below it and the two must never share a label.
 */
export interface ObservedCadence {
  /** The median per-pair spacing, in ns. */
  intervalNs: number;
  /** The SMALLEST per-pair sample count: "at least N per pair, so far". */
  samplesPerPair: number;
  /** How many pairs actually contributed a spacing — the sample size behind intervalNs. */
  pairs: number;
}

/**
 * observedCadence measures one pair's spacing as (last − first) / (gaps), then
 * takes the MEDIAN across pairs.
 *
 * Median rather than mean because a single pair whose agent went away mid-run
 * contributes one enormous gap, and a mean would let that one pair rewrite the
 * number for all the others. Span-over-gaps rather than a mean of consecutive
 * deltas because they are the same arithmetic and the first needs two
 * timestamps instead of n.
 *
 * Undefined until some pair has TWO samples: one sample is an instant, and an
 * instant has no spacing. That is the whole precondition — a tile with nothing
 * measured yet shows the plan and says so.
 */
export function observedCadence(groups: readonly PairSamples[]): ObservedCadence | undefined {
  const intervals: number[] = [];
  let minCount = Infinity;
  for (const g of groups) {
    minCount = Math.min(minCount, g.samples.length);
    /* Timestamps are strings off a JSON body like any other field: a row with no
       recordedAt, or one carrying "yesterday", must not turn into a NaN spacing
       that renders "NaNs" onto the tile. Unparseable rows are dropped from the
       measurement rather than poisoning it. */
    const stamps: number[] = [];
    for (const s of g.samples) {
      const at = new Date(String(s.recordedAt ?? "")).getTime();
      if (Number.isFinite(at)) stamps.push(at);
    }
    if (stamps.length < 2) continue;
    /* Results arrive in completion order, which is ascending time — but a clock
       that stepped backwards would make first > last and hand back a negative
       cadence, so the span is taken over the real extremes. */
    const span = Math.max(...stamps) - Math.min(...stamps);
    if (!(span > 0)) continue;
    intervals.push((span * 1e6) / (stamps.length - 1));
  }
  if (intervals.length === 0) return undefined;

  intervals.sort((a, b) => a - b);
  const mid = Math.floor(intervals.length / 2);
  const intervalNs =
    intervals.length % 2 === 1 ? intervals[mid] : (intervals[mid - 1] + intervals[mid]) / 2;
  return {
    intervalNs,
    // Never larger than the truth: the floor across pairs is what "≥ N per pair" may claim.
    samplesPerPair: Number.isFinite(minCount) ? minCount : 0,
    pairs: intervals.length,
  };
}

/**
 * snapshotForSample answers which stored ROUTE a single probe walked.
 *
 * A run's results carry an outcome and a duration but no hops — the path lives
 * in the MTR projection, keyed by pair — so the link between the two is the
 * clock: the stored path whose [firstSeen, lastSeen] window covers the probe's
 * own instant. Both ends count as inside; a probe recorded exactly as a path was
 * first seen is that path.
 *
 * Undefined when nothing covers it, deliberately. Falling back to the nearest
 * path would put a route under a tick that did not walk it, which is a more
 * confident lie than showing nothing.
 *
 * The grace on the leading edge is for routes ALREADY STORED. The projection now
 * stamps a route with its result row's own recorded_at, but rows written before
 * that used the clock at the projection call — a few hundred microseconds later
 * than the trace that created them. Without the grace the tick that CREATED a
 * route was the one tick told "no recorded route covers this probe", which is
 * the least believable place for that sentence to appear.
 */
/** Milliseconds of slack on a stored route's leading edge; see snapshotForSample. */
const SNAPSHOT_START_GRACE_MS = 5_000;

export function snapshotForSample(
  snapshots: readonly Pick<PathSnapshot, "id" | "firstSeen" | "lastSeen">[],
  recordedAt: string | undefined,
): Pick<PathSnapshot, "id" | "firstSeen" | "lastSeen"> | undefined {
  if (!recordedAt) return undefined;
  const at = new Date(recordedAt).getTime();
  if (Number.isNaN(at)) return undefined;
  /* The list arrives newest-first, so the first cover IS the newest cover. */
  return snapshots.find((s) => {
    const from = new Date(s.firstSeen).getTime();
    const to = new Date(s.lastSeen).getTime();
    return Number.isFinite(from) && Number.isFinite(to) && at >= from - SNAPSHOT_START_GRACE_MS && at <= to;
  });
}
