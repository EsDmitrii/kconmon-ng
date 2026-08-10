import { describe, expect, it } from "vitest";
import {
  aggregateSamples,
  groupSamplesByPair,
  isIntervalRun,
  MAX_SAMPLES_PER_PAIR,
  pairProgress,
  percentileNs,
  plannedSamplesPerPair,
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
    expect(p).toEqual({ expected: 12, arrived: 7, remaining: 5, remainingNs: 25 * s, framed: true });
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

  // A single-slot track is not worth framing -- see the flag's use in pages/run-detail.tsx.
  it("declines to frame a one-slot track", () => {
    expect(pairProgress(0, 1).framed).toBe(false);
    expect(pairProgress(5 * s, 0).framed).toBe(false);
    expect(pairProgress(10 * s, 0).framed).toBe(true);
  });
});
