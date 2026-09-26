import { describe, expect, it } from "vitest";
import {
  aggregateSamples,
  effectivePlannedSamplesPerPair,
  effectiveSampleIntervalNs,
  fmtMicrosNs,
  fmtNsCompact,
  formatCadenceNs,
  formatDurationNs,
  groupSamplesByPair,
  isIntervalRun,
  MAX_SAMPLES_PER_PAIR,
  MICROSECOND_FLOOR_NS,
  MTR_PER_PAIR_BUDGET_NS,
  pairProgress,
  percentileNs,
  perPairBudgetNs,
  plannedCadenceFromSpec,
  plannedSamplesPerPair,
  runCadence,
  coveringSnapshots,
  runDurationNs,
  sampleIntervalNs,
} from "./run-samples";
import type { RunResult } from "./types";

function sample(over: Partial<RunResult> = {}): RunResult {
  return {
    sourceNode: "n1",
    destinationNode: "n2",
    success: true,
    durationNs: 1_000_000,
    recordedAt: "2026-08-09T10:00:00Z",
    sampleSeq: 0,
    ...over,
  };
}

describe("runDurationNs / isIntervalRun", () => {
  it("reads Duration out of the spec snapshot", () => {
    expect(runDurationNs({ spec: { Duration: 60_000_000_000 } })).toBe(60_000_000_000);
    expect(isIntervalRun({ spec: { Duration: 60_000_000_000 } })).toBe(true);
  });

  // Every run created before Spec.Duration existed has no such key, and its
  // spec is `omitempty`-free of one even now. Those are instant runs, not
  // malformed ones.
  it("treats a spec with no Duration as an instant run", () => {
    expect(runDurationNs({ spec: { Type: "tcp" } })).toBe(0);
    expect(isIntervalRun({ spec: { Type: "tcp" } })).toBe(false);
  });

  // Total over garbage: a spec this parser cannot read must degrade to
  // "instant", never throw and blank the permalink.
  it("is total over malformed specs", () => {
    expect(runDurationNs(undefined)).toBe(0);
    expect(runDurationNs({ spec: null })).toBe(0);
    expect(runDurationNs({ spec: "nope" })).toBe(0);
    expect(runDurationNs({ spec: { Duration: "60s" } })).toBe(0);
    expect(runDurationNs({ spec: { Duration: Number.NaN } })).toBe(0);
    expect(runDurationNs({ spec: { Duration: -5 } })).toBe(0);
  });
});

describe("groupSamplesByPair", () => {
  it("buckets per pair and preserves completion order within each", () => {
    const groups = groupSamplesByPair([
      sample({ destinationNode: "n2", sampleSeq: 0 }),
      sample({ destinationNode: "n3", sampleSeq: 0 }),
      sample({ destinationNode: "n2", sampleSeq: 1 }),
      sample({ destinationNode: "n3", sampleSeq: 1 }),
    ]);
    expect(groups).toHaveLength(2);
    expect(groups[0].destination).toBe("n2");
    expect(groups[0].samples.map((s) => s.sampleSeq)).toEqual([0, 1]);
    expect(groups[1].destination).toBe("n3");
    expect(groups[1].samples.map((s) => s.sampleSeq)).toEqual([0, 1]);
  });

  it("keeps pairs in first-seen order so the timeline does not reshuffle", () => {
    const groups = groupSamplesByPair([
      sample({ sourceNode: "z", destinationNode: "y" }),
      sample({ sourceNode: "a", destinationNode: "b" }),
    ]);
    expect(groups.map((g) => g.source)).toEqual(["z", "a"]);
  });

  it("returns nothing for a run with no results yet", () => {
    expect(groupSamplesByPair([])).toEqual([]);
  });
});

describe("percentileNs", () => {
  // Nearest-rank: every answer is a value that was actually measured.
  it("returns a measured value, never an interpolation", () => {
    const sorted = [10, 20, 30, 40];
    expect(percentileNs(sorted, 95)).toBe(40);
    expect(percentileNs(sorted, 50)).toBe(20);
    expect(percentileNs(sorted, 0)).toBe(10);
    expect(percentileNs([7], 95)).toBe(7);
  });

  it("is undefined with nothing to rank", () => {
    expect(percentileNs([], 95)).toBeUndefined();
  });
});

describe("aggregateSamples", () => {
  it("counts sent/failed and the fail ratio over every sample", () => {
    const agg = aggregateSamples([
      sample({ success: true, durationNs: 1_000_000 }),
      sample({ success: false, durationNs: 2_000_000_000, error: "timeout" }),
      sample({ success: true, durationNs: 3_000_000 }),
      sample({ success: true, durationNs: 2_000_000 }),
    ]);
    expect(agg.sent).toBe(4);
    expect(agg.failed).toBe(1);
    expect(agg.failRatio).toBeCloseTo(0.25);
  });

  // The load-bearing one: a 2s timeout must not be averaged in as a 2s
  // "latency". Latency describes probes that answered.
  it("computes latency over successful samples only", () => {
    const agg = aggregateSamples([
      sample({ success: true, durationNs: 1_000_000 }),
      sample({ success: false, durationNs: 120_000_000_000, error: "timeout" }),
      sample({ success: true, durationNs: 3_000_000 }),
    ]);
    expect(agg.minNs).toBe(1_000_000);
    expect(agg.maxNs).toBe(3_000_000);
    expect(agg.avgNs).toBe(2_000_000);
    expect(agg.p95Ns).toBe(3_000_000);
  });

  it("reports no latency at all when every sample failed", () => {
    const agg = aggregateSamples([
      sample({ success: false, durationNs: 5_000_000_000 }),
      sample({ success: false, durationNs: 5_000_000_000 }),
    ]);
    expect(agg.sent).toBe(2);
    expect(agg.failed).toBe(2);
    expect(agg.failRatio).toBe(1);
    expect(agg.minNs).toBeUndefined();
    expect(agg.avgNs).toBeUndefined();
    expect(agg.p95Ns).toBeUndefined();
    expect(agg.maxNs).toBeUndefined();
  });

  it("is safe on an empty run", () => {
    const agg = aggregateSamples([]);
    expect(agg).toEqual({ sent: 0, failed: 0, failRatio: 0 });
  });
});

describe("sampleIntervalNs / plannedSamplesPerPair", () => {
  // These MUST agree with checks.SampleInterval / plannedRounds in
  // internal/console/checks/checks.go -- the console shows the operator what
  // the server is going to do, and a drift here is a lie in the UI.
  it("mirrors the server's cadence derivation", () => {
    const s = 1_000_000_000;
    expect(sampleIntervalNs(0)).toBe(0);
    expect(sampleIntervalNs(10 * s)).toBe(5 * s); // floored
    expect(sampleIntervalNs(60 * s)).toBe(5 * s); // floored
    expect(sampleIntervalNs(3600 * s)).toBe(7.2 * s); // widened
    expect(sampleIntervalNs(86400 * s)).toBe(172.8 * s); // widened
  });

  it("never plans more than the documented cap", () => {
    const s = 1_000_000_000;
    expect(plannedSamplesPerPair(0)).toBe(1);
    expect(plannedSamplesPerPair(10 * s)).toBe(2);
    expect(plannedSamplesPerPair(60 * s)).toBe(12);
    expect(plannedSamplesPerPair(900 * s)).toBe(180);
    expect(plannedSamplesPerPair(3600 * s)).toBe(MAX_SAMPLES_PER_PAIR);
    expect(plannedSamplesPerPair(86400 * s)).toBe(MAX_SAMPLES_PER_PAIR);
  });
});

describe("pairProgress", () => {
  const s = 1_000_000_000;

  // Twelve slots for a 1m run, seven filled, and a tail worth five more cadences.
  it("frames the track and counts the tail down in TIME, not in ticks", () => {
    const p = pairProgress(60 * s, 7);
    expect(p).toEqual({ planned: 12, expected: 12, arrived: 7, remaining: 5, remainingNs: 25 * s, framed: true });
  });

  // Flooring, never rounding up: 72s/5s is 14.4 and the fifteenth probe has nowhere to land.
  it("floors a duration that is not a whole number of cadences", () => {
    expect(pairProgress(72 * s, 0).expected).toBe(14);
    expect(pairProgress(72 * s, 0).remainingNs).toBe(70 * s);
  });

  // The case the caption's "~" exists for: past an hour the server widens the cadence to hold the cap.
  it("follows the server's widened cadence at the 500-sample cap", () => {
    const p = pairProgress(3600 * s, 10);
    expect(p.expected).toBe(MAX_SAMPLES_PER_PAIR);
    expect(p.remaining).toBe(490);
    expect(p.remainingNs).toBe(3528 * s); // 490 slots at the widened 7.2s cadence
  });

  // The frame must never call an ARRIVED sample impossible, so it grows instead of reading "-1 left".
  it("widens rather than lies when more samples arrive than were planned", () => {
    const p = pairProgress(60 * s, 13);
    expect(p.expected).toBe(13);
    expect(p.remaining).toBe(0);
    expect(p.remainingNs).toBe(0);
  });

  /* `planned` is the PLAN's floor and `expected` is the drawn WIDTH, and the two
     part company in exactly one direction. The strip must widen to hold a
     thirteenth sample the floor did not predict, but the caption still says what
     was planned: "13 of ≥12" is the truth, and "13 of ≥13" invents a plan that
     was never made -- the same ≥ semantics the Go planner ships. */
  it("keeps the PLANNED floor while the drawn frame widens around it", () => {
    const over = pairProgress(60 * s, 13);
    expect(over.planned).toBe(12);
    expect(over.expected).toBe(13);

    // And with a server-snapshotted cadence, the snapshot's own floor is the one kept.
    const stretched = pairProgress(900 * s, 11, { intervalNs: 90 * s, samplesPerPair: 10 });
    expect(stretched.planned).toBe(10);
    expect(stretched.expected).toBe(11);
  });

  // The other direction: nothing arrived yet, and the plan is still the whole frame.
  it("never collapses the frame onto the arrived count", () => {
    const p = pairProgress(60 * s, 0);
    expect(p.planned).toBe(12);
    expect(p.expected).toBe(12);
    expect(p.remaining).toBe(12);
  });

  // A single-slot track is not worth framing -- see the flag's use in pages/run-detail.tsx.
  it("declines to frame a one-slot track", () => {
    expect(pairProgress(0, 1).framed).toBe(false);
    expect(pairProgress(5 * s, 0).framed).toBe(false);
    expect(pairProgress(10 * s, 0).framed).toBe(true);
  });
});

describe("effective cadence for slow check types", () => {
  const s = 1_000_000_000;
  const m = 60 * s;

  it("leaves every fast type on the base cadence", () => {
    // A tcp probe's timeout bounds a failure, not an expectation: planning around it would slow
    // every healthy run down.
    for (const type of ["tcp", "udp", "icmp", "dns", "http"]) {
      expect(effectiveSampleIntervalNs(15 * m, type, 90, 10)).toBe(sampleIntervalNs(15 * m));
      expect(effectivePlannedSamplesPerPair(15 * m, type, 90, 10)).toBe(plannedSamplesPerPair(15 * m));
    }
  });

  it("stretches mtr to its trace budget", () => {
    // The owner's run: 15m, 4 pairs across 4 sources. One batch of 90s.
    expect(effectiveSampleIntervalNs(15 * m, "mtr", 4, 4)).toBe(MTR_PER_PAIR_BUDGET_NS);
    expect(effectivePlannedSamplesPerPair(15 * m, "mtr", 4, 4)).toBe(10);
  });

  it("counts the whole round, not one pair", () => {
    // 90 pairs over 10 sources: 12 batches of 90s, so one round outlasts a 15m run.
    expect(effectiveSampleIntervalNs(15 * m, "mtr", 90, 10)).toBe(12 * MTR_PER_PAIR_BUDGET_NS);
    expect(effectivePlannedSamplesPerPair(15 * m, "mtr", 90, 10)).toBe(1);
  });

  it("stretches pmtu to its per-pair timeout, as checks.perPairBudget does", () => {
    // Over a black hole a pmtu probe waits out a deadline per lost datagram, so the server plans
    // it around checks.pmtuMinPerPairTimeout (15s) like mtr around its trace budget.
    expect(perPairBudgetNs("pmtu")).toBe(15 * s);
    expect(effectiveSampleIntervalNs(m, "pmtu", 4, 4)).toBe(15 * s);
    expect(effectivePlannedSamplesPerPair(m, "pmtu", 4, 4)).toBe(4);
    // 90 pairs over 10 sources: 12 batches of 15s.
    expect(effectiveSampleIntervalNs(15 * m, "pmtu", 90, 10)).toBe(12 * 15 * s);
  });

  it("never plans less than one sample", () => {
    for (const duration of [10 * s, m, 15 * m, 24 * 60 * m]) {
      for (const pairs of [1, 4, 90, 400]) {
        expect(effectivePlannedSamplesPerPair(duration, "mtr", pairs, 10)).toBeGreaterThanOrEqual(1);
      }
    }
  });

  it("treats an instant run as one sample at no cadence", () => {
    expect(effectiveSampleIntervalNs(0, "mtr", 4, 4)).toBe(0);
    expect(effectivePlannedSamplesPerPair(0, "mtr", 4, 4)).toBe(1);
  });

  it("reads the server's snapshot back when the run carries one", () => {
    expect(plannedCadenceFromSpec({ spec: { PlannedSampleIntervalNs: 90 * s, PlannedSamplesPerPair: 10 } })).toEqual({
      intervalNs: 90 * s,
      samplesPerPair: 10,
    });
    // A run created before the fields existed falls back to the caller's own derivation.
    expect(plannedCadenceFromSpec({ spec: { Duration: 15 * m } })).toBeUndefined();
    expect(plannedCadenceFromSpec(undefined)).toBeUndefined();
  });
});

/* ── the cadence the run ACTUALLY ran at ─────────────────────────────────── */

/**
 * Live acceptance on rev13: a 15m MTR over four pairs executed at the effective
 * 90s cadence (12 samples over 3 rounds, verified on the stand) while the run
 * permalink's Cadence tile read "5s" — the BASE cadence off the spec's duration,
 * which is a number that run never used.
 *
 * runCadence is the one answer that tile and the progress frames now share. The
 * server's own snapshot wins where it exists; the client's mirror of
 * checks.EffectiveSampleInterval covers the runs created before those fields did.
 */
const s = 1_000_000_000;

const run = (spec: unknown, over: { type?: string; pairTotal?: number } = {}) =>
  ({ spec, type: over.type ?? "mtr", pairTotal: over.pairTotal ?? 4 }) as Parameters<typeof runCadence>[0];

describe("runCadence", () => {
  it("prefers the cadence the SERVER snapshotted onto the run", () => {
    const c = runCadence(run({ Duration: 900 * s, Type: "mtr", PlannedSampleIntervalNs: 90 * s, PlannedSamplesPerPair: 10 }), 2);
    expect(c).toEqual({ intervalNs: 90 * s, samplesPerPair: 10, fromSpec: true });
  });

  it("derives the EFFECTIVE cadence for a run created before the server recorded one", () => {
    // 15m/500 floors to 5s, but four mtr pairs over two sources is one round of
    // 90s, and a cadence cannot be shorter than the round it has to fit.
    const c = runCadence(run({ Duration: 900 * s, Type: "mtr" }), 2);
    expect(c?.intervalNs).toBe(90 * s);
    expect(c?.samplesPerPair).toBe(10);
    expect(c?.fromSpec).toBe(false);
  });

  it("leaves a FAST check type on its base cadence — a tcp probe answers in milliseconds", () => {
    const c = runCadence(run({ Duration: 900 * s, Type: "tcp" }, { type: "tcp" }), 2);
    expect(c?.intervalNs).toBe(sampleIntervalNs(900 * s));
  });

  it("is undefined for an instant run, which has no cadence to name", () => {
    expect(runCadence(run({}), 2)).toBeUndefined();
    expect(runCadence(undefined, 2)).toBeUndefined();
  });

  it("survives a run whose sources are not known yet, rather than dividing by none", () => {
    const c = runCadence(run({ Duration: 900 * s, Type: "mtr" }), 0);
    expect(c?.intervalNs).toBeGreaterThanOrEqual(90 * s);
    expect(Number.isFinite(c?.intervalNs)).toBe(true);
  });
});

describe("pairProgress at a stretched cadence", () => {
  it("counts the tail down in the EFFECTIVE interval, not the base one", () => {
    // Four samples in, six to go, at 90s each — not at 5s each, which would have
    // promised the run was thirty seconds from finishing.
    const p = pairProgress(900 * s, 4, { intervalNs: 90 * s, samplesPerPair: 10 });
    expect(p.expected).toBe(10);
    expect(p.remaining).toBe(6);
    expect(p.remainingNs).toBe(540 * s);
  });

  it("still widens rather than lies when a late sample lands past the plan", () => {
    const p = pairProgress(900 * s, 11, { intervalNs: 90 * s, samplesPerPair: 10 });
    expect(p.expected).toBe(11);
    expect(p.remaining).toBe(0);
  });

  it("falls back to the base cadence when no cadence is handed to it", () => {
    expect(pairProgress(60 * s, 7)).toEqual({
      planned: 12,
      expected: 12,
      arrived: 7,
      remaining: 5,
      remainingNs: 25 * s,
      framed: true,
    });
  });
});

describe("formatCadenceNs", () => {
  it("says a stretched cadence in seconds rather than rounding it to a wrong minute", () => {
    // "2m" for a 90s round is a third longer than the fleet actually keeps.
    expect(formatCadenceNs(90 * s, "en")).toBe("90s");
    expect(formatCadenceNs(173 * s, "en")).toBe("173s");
  });

  it("leaves a whole number of minutes — and an hour — reading as one", () => {
    expect(formatCadenceNs(120 * s, "en")).toBe("2m");
    expect(formatCadenceNs(3600 * s, "en")).toBe("1h");
  });

  it("keeps short cadences exactly as they always read", () => {
    expect(formatCadenceNs(5 * s, "en")).toBe("5s");
    expect(formatCadenceNs(7 * s, "en")).toBe("7s");
  });

  it("takes the interface language's own unit word", () => {
    expect(formatCadenceNs(90 * s, "ru")).toBe("90 с");
    expect(formatCadenceNs(120 * s, "ru")).toBe("2 мин");
  });

  it("says a whole-minute cadence past an hour in minutes, not a rounded hour", () => {
    // 44 MTR batches × 90 s: "every 1h" would hide six minutes of every round.
    expect(formatCadenceNs(3960 * s, "en")).toBe("66m");
    expect(formatCadenceNs(7200 * s, "en")).toBe("2h");
  });
});

describe("formatDurationNs past an hour", () => {
  it("keeps the minutes instead of rounding to a whole hour", () => {
    expect(formatDurationNs(90 * 60 * s, "en")).toBe("1h 30m");
    expect(formatDurationNs(89 * 60 * s, "en")).toBe("1h 29m");
    expect(formatDurationNs(150 * 60 * s, "ru")).toBe("2 ч 30 мин");
    expect(formatDurationNs(6 * 3600 * s, "en")).toBe("6h");
  });

  it("says 1h, not 60m, once the minutes round up to an hour", () => {
    expect(formatDurationNs(3569 * s, "en")).toBe("59m");
    expect(formatDurationNs(3570 * s, "en")).toBe("1h");
    expect(formatDurationNs(3599 * s, "ru")).toBe("1 ч");
    expect(formatDurationNs(3600 * s, "en")).toBe("1h");
  });
});

/* ── which recorded ROUTE a probe belongs to ─────────────────────────────── */

describe("coveringSnapshots", () => {
  const snap = (id: string, firstSeen: string, lastSeen: string) =>
    ({ id, firstSeen, lastSeen }) as Parameters<typeof coveringSnapshots>[0][number];
  const ids = (list: { id: string }[]) => list.map((x) => x.id);

  // Newest first, which is the order the store returns.
  const snapshots = [
    snap("new", "2026-08-09T12:00:00Z", "2026-08-09T12:30:00Z"),
    snap("old", "2026-08-09T11:00:00Z", "2026-08-09T11:59:00Z"),
  ];

  it("picks the path whose window contains the probe", () => {
    expect(ids(coveringSnapshots(snapshots, "2026-08-09T11:30:00Z"))).toEqual(["old"]);
    expect(ids(coveringSnapshots(snapshots, "2026-08-09T12:10:00Z"))).toEqual(["new"]);
  });

  it("counts both ends of the window as inside it", () => {
    expect(ids(coveringSnapshots(snapshots, "2026-08-09T12:00:00Z"))).toEqual(["new"]);
    expect(ids(coveringSnapshots(snapshots, "2026-08-09T11:59:00Z"))).toEqual(["old"]);
  });

  it("answers nothing for a probe no stored path covers, rather than the nearest one", () => {
    expect(coveringSnapshots(snapshots, "2026-08-09T10:00:00Z")).toEqual([]);
    expect(coveringSnapshots(snapshots, "2026-08-09T13:00:00Z")).toEqual([]);
  });

  it("answers nothing for a missing or unparsable stamp rather than guessing", () => {
    for (const ts of [undefined, "", "not-a-time", "0000", "%%%"]) {
      expect(coveringSnapshots(snapshots, ts), String(ts)).toEqual([]);
    }
    expect(coveringSnapshots([], "2026-08-09T12:10:00Z")).toEqual([]);
  });

  it("skips a stored window whose own bounds are unparsable", () => {
    const broken = [snap("bad", "nope", "nope"), ...snapshots];
    expect(ids(coveringSnapshots(broken, "2026-08-09T12:10:00Z"))).toEqual(["new"]);
  });

  it("returns every route whose window covers the probe, so alternating paths are visible as such", () => {
    const ecmp = [snap("b", "2026-08-09T10:05:00Z", "2026-08-09T10:30:00Z"), snap("a", "2026-08-09T10:00:00Z", "2026-08-09T10:29:30Z")];
    expect(ids(coveringSnapshots(ecmp, "2026-08-09T10:29:30Z"))).toEqual(["b", "a"]);
    expect(ids(coveringSnapshots(ecmp, "2026-08-09T10:02:00Z"))).toEqual(["a"]);
  });

  /* Routes stored before the projection used the result row's recorded_at were stamped a few
     hundred microseconds after the trace that created them. Five seconds of grace covers that skew
     and nothing more: ten seconds early is still nothing. */
  it("still covers the probe that created the route, stamped just before it", () => {
    const one = [snap("s1", "2026-07-28T10:00:00Z", "2026-07-28T10:05:00Z")];
    expect(ids(coveringSnapshots(one, "2026-07-28T09:59:59.9Z"))).toEqual(["s1"]);
    expect(ids(coveringSnapshots(one, "2026-07-28T09:59:56Z"))).toEqual(["s1"]);
    expect(coveringSnapshots(one, "2026-07-28T09:59:50Z")).toEqual([]);
  });

  it("uses the leading-edge grace only when no window covers the probe outright", () => {
    // A route change: the last probe on A falls inside B's grace, and is still A's.
    const change = [snap("b", "2026-08-09T10:05:03Z", "2026-08-09T10:30:00Z"), snap("a", "2026-08-09T10:00:00Z", "2026-08-09T10:05:00Z")];
    expect(ids(coveringSnapshots(change, "2026-08-09T10:05:00Z"))).toEqual(["a"]);
    expect(ids(coveringSnapshots(change, "2026-08-09T10:05:02.999Z"))).toEqual(["b"]);
  });
});

/* ── one unit rule for a latency, shared with the MTR hop table ─────────────── */
describe("fmtNsCompact / fmtMicrosNs", () => {
  it("switches to microseconds below 0.1ms instead of rounding a real probe to 0.0ms", () => {
    expect(fmtNsCompact(95_000)).toBe("95µs");
    expect(fmtNsCompact(38_000)).toBe("38µs");
    expect(fmtNsCompact(1_500)).toBe("2µs");
    // The boundary stays in milliseconds: 0.1ms has a decimal that holds it.
    expect(fmtNsCompact(MICROSECOND_FLOOR_NS)).toBe("0.1ms");
    expect(fmtNsCompact(99_999)).toBe("100µs");
  });

  it("keeps one decimal under 10ms and whole milliseconds above", () => {
    expect(fmtNsCompact(400_000)).toBe("0.4ms");
    expect(fmtNsCompact(2_500_000)).toBe("2.5ms");
    expect(fmtNsCompact(9_960_000)).toBe("10.0ms");
    expect(fmtNsCompact(12_400_000)).toBe("12ms");
  });

  it("keeps a genuine zero in milliseconds, and dashes anything that is not a number", () => {
    expect(fmtNsCompact(0)).toBe("0.0ms");
    expect(fmtNsCompact(undefined)).toBe("—");
    expect(fmtNsCompact(Number.NaN)).toBe("—");
    expect(fmtNsCompact(Number.POSITIVE_INFINITY)).toBe("—");
    expect(fmtNsCompact("fast" as unknown as number)).toBe("—");
  });

  /* A Russian run permalink printed «1 мин» and «5 с» beside "73µs" and "0.3ms". */
  it("speaks the page's language when given one, and English without", () => {
    expect(fmtNsCompact(73_000, "ru")).toBe("73 мкс");
    expect(fmtNsCompact(300_000, "ru")).toBe("0,3 мс");
    expect(fmtNsCompact(12_400_000, "ru")).toBe("12 мс");
    expect(fmtNsCompact(Number.NaN, "ru")).toBe("—");
    expect(fmtMicrosNs(-4_000, "ru")).toBe("-4 мкс");
    expect(fmtNsCompact(300_000, "en")).toBe("0.3ms");
  });

  it("is the same floor the hop table's fmtRttNs reads", () => {
    expect(MICROSECOND_FLOOR_NS).toBe(100_000);
    expect(fmtMicrosNs(-4_000)).toBe("-4µs");
    expect(fmtMicrosNs(0)).toBeUndefined();
    expect(fmtMicrosNs(100_000)).toBeUndefined();
  });
});
