import { describe, expect, it } from "vitest";

import {
  MAX_SAMPLES_PER_PAIR,
  MIN_SAMPLE_INTERVAL_NS,
  MTR_PER_PAIR_BUDGET_NS,
  baseSampleIntervalNs,
  cadenceParts,
  effectivePlannedSamplesPerPair,
  effectiveSampleIntervalNs,
  formatCadenceProse,
  observedCadence,
  planCadenceFor,
  sampleIntervalNs,
} from "./run-samples";
import type { PairSamples } from "./run-samples";
import type { RunResult } from "./types";

/*
run-samples.interval.test.ts is the client mirror of internal/console/checks's cadence planner: the
operator-requested interval, the two ways the plan may move away from it, and the OBSERVED cadence
the run permalink measures off the samples on screen.

The bug behind all of it: one quantity — a duration run's sample cadence — was reported as three
different numbers (the base cadence in the MTR Runner's caption, the worst-case round floor on the
permalink's tile, and neither of them by the run itself). These pin that each of the three is now
named for what it is.
*/

const SECOND = 1e9;
const MINUTE = 60 * SECOND;
const HOUR = 60 * MINUTE;

function sample(recordedAt: string): RunResult {
  return {
    sourceNode: "n1",
    destinationNode: "n2",
    success: true,
    durationNs: 2 * SECOND,
    recordedAt,
    sampleSeq: 0,
  } as RunResult;
}

function pair(source: string, stamps: string[]): PairSamples {
  return { source, destination: "dst", samples: stamps.map(sample) };
}

describe("baseSampleIntervalNs", () => {
  it("derives duration/500 floored at 5s when nothing is requested", () => {
    expect(baseSampleIntervalNs(5 * MINUTE)).toBe(MIN_SAMPLE_INTERVAL_NS);
    expect(baseSampleIntervalNs(5 * MINUTE)).toBe(sampleIntervalNs(5 * MINUTE));
    expect(baseSampleIntervalNs(24 * HOUR)).toBe((24 * HOUR) / MAX_SAMPLES_PER_PAIR);
  });

  it("honours a request below the derived 5s floor", () => {
    // That floor stops an arithmetic accident nobody chose; an operator who picked 1s has chosen.
    expect(baseSampleIntervalNs(5 * MINUTE, SECOND)).toBe(SECOND);
    expect(SECOND).toBeLessThan(MIN_SAMPLE_INTERVAL_NS);
  });

  it("lets the 500-sample cap bind a request it cannot afford", () => {
    expect(baseSampleIntervalNs(24 * HOUR, SECOND)).toBe((24 * HOUR) / MAX_SAMPLES_PER_PAIR);
  });

  it("reads a NaN or negative request as no request at all", () => {
    expect(baseSampleIntervalNs(5 * MINUTE, Number.NaN)).toBe(MIN_SAMPLE_INTERVAL_NS);
    expect(baseSampleIntervalNs(5 * MINUTE, -1)).toBe(MIN_SAMPLE_INTERVAL_NS);
    expect(baseSampleIntervalNs(0, SECOND)).toBe(0);
  });
});

describe("planCadenceFor", () => {
  it("honours a feasible request and reports no adjustment", () => {
    const plan = planCadenceFor(5 * MINUTE, "tcp", 4, 2, SECOND);
    expect(plan).toEqual({
      requestedNs: SECOND,
      baseNs: SECOND,
      intervalNs: SECOND,
      samplesPerPair: 300,
      adjusted: "",
    });
  });

  it("names the cap when it binds rather than truncating in silence", () => {
    const plan = planCadenceFor(24 * HOUR, "tcp", 4, 2, SECOND);
    expect(plan.adjusted).toBe("cap");
    expect(plan.requestedNs).toBe(SECOND);
    expect(plan.intervalNs).toBe((24 * HOUR) / MAX_SAMPLES_PER_PAIR);
    expect(plan.samplesPerPair).toBe(MAX_SAMPLES_PER_PAIR);
  });

  it("stretches a below-floor MTR request instead of refusing it, and says which", () => {
    // The owner's run: 10 pairs of traces over 5 sources, asked for every second.
    const plan = planCadenceFor(5 * MINUTE, "mtr", 10, 5, SECOND);
    expect(plan.adjusted).toBe("round");
    // Requested is still reported: the two numbers travel apart, never collapsed into one.
    expect(plan.requestedNs).toBe(SECOND);
    expect(plan.intervalNs).toBeGreaterThan(SECOND);
    expect(plan.intervalNs).toBe(2 * MTR_PER_PAIR_BUDGET_NS);
    expect(plan.samplesPerPair).toBeGreaterThanOrEqual(1);
  });

  it("leaves the requestless path byte-identical to what it always did", () => {
    for (const type of ["tcp", "udp", "icmp", "dns", "http", "mtr"]) {
      for (const d of [0, 10 * SECOND, MINUTE, 15 * MINUTE, 24 * HOUR]) {
        const plan = planCadenceFor(d, type, 10, 5);
        expect(plan.requestedNs).toBe(0);
        expect(plan.baseNs).toBe(sampleIntervalNs(d));
        expect(plan.intervalNs).toBe(effectiveSampleIntervalNs(d, type, 10, 5));
        expect(plan.samplesPerPair).toBe(effectivePlannedSamplesPerPair(d, type, 10, 5));
      }
    }
  });

  it("keeps the old four-argument callers on the old answers", () => {
    // 4 pairs over 2 sources is one batch on both gates, so one round is one trace budget.
    expect(effectiveSampleIntervalNs(15 * MINUTE, "mtr", 4, 2)).toBe(MTR_PER_PAIR_BUDGET_NS);
    expect(effectivePlannedSamplesPerPair(15 * MINUTE, "mtr", 4, 2)).toBe(10);
    expect(effectiveSampleIntervalNs(15 * MINUTE, "tcp", 4, 2)).toBe(MIN_SAMPLE_INTERVAL_NS);
    expect(effectivePlannedSamplesPerPair(15 * MINUTE, "tcp", 4, 2)).toBe(180);
  });

  it("answers one honest pass for an instant run", () => {
    expect(planCadenceFor(0, "mtr", 10, 5, SECOND)).toEqual({
      requestedNs: SECOND,
      baseNs: 0,
      intervalNs: 0,
      samplesPerPair: 1,
      adjusted: "",
    });
  });
});

describe("formatCadenceProse", () => {
  it("spells the unit out in Russian, where a bare «с» reads as a preposition", () => {
    expect(formatCadenceProse(5 * SECOND, "ru")).toBe("5 секунд");
    expect(formatCadenceProse(2 * MINUTE, "ru")).toBe("2 минуты");
    expect(formatCadenceProse(90 * SECOND, "ru")).toBe("90 секунд");
    expect(formatCadenceProse(3 * HOUR, "ru")).toBe("3 часа");
    expect(formatCadenceProse(21 * SECOND, "ru")).toBe("21 секунду");
    expect(formatCadenceProse(11 * SECOND, "ru")).toBe("11 секунд");
    expect(formatCadenceProse(15 * MINUTE, "ru")).toBe("15 минут");
  });

  it("drops the numeral at one — «раз в минуту», never «раз в 1 минуту»", () => {
    expect(formatCadenceProse(MINUTE, "ru")).toBe("минуту");
    expect(formatCadenceProse(SECOND, "ru")).toBe("секунду");
    expect(formatCadenceProse(HOUR, "ru")).toBe("час");
  });

  it("leaves English alone: 'every 5s' already reads as a sentence", () => {
    expect(formatCadenceProse(5 * SECOND, "en")).toBe("5s");
    expect(formatCadenceProse(90 * SECOND, "en")).toBe("90s");
    expect(formatCadenceProse(2 * MINUTE, "en")).toBe("2m");
  });

  it("draws a span that is not a number as no span at all", () => {
    expect(formatCadenceProse(Number.NaN, "ru")).toBe("0 секунд");
    expect(formatCadenceProse(Number.POSITIVE_INFINITY, "ru")).toBe("0 секунд");
    expect(formatCadenceProse(Number.NaN, "en")).toBe("0s");
  });

  it("keeps 90s in seconds rather than rounding it up to two minutes", () => {
    expect(cadenceParts(90 * SECOND)).toEqual({ value: 90, unit: "second" });
    expect(cadenceParts(2 * MINUTE)).toEqual({ value: 2, unit: "minute" });
    expect(cadenceParts(2 * HOUR)).toEqual({ value: 2, unit: "hour" });
  });
});

describe("observedCadence", () => {
  it("is undefined until some pair has two samples to space apart", () => {
    expect(observedCadence([])).toBeUndefined();
    expect(observedCadence([pair("a", ["2026-08-11T13:33:00Z"])])).toBeUndefined();
  });

  it("measures the spacing the samples actually have", () => {
    // The owner's run: started 13:33, three probes by 13:36 — about one a minute.
    const observed = observedCadence([
      pair("a", ["2026-08-11T13:33:00Z", "2026-08-11T13:34:00Z", "2026-08-11T13:35:00Z"]),
      pair("b", ["2026-08-11T13:33:00Z", "2026-08-11T13:34:00Z", "2026-08-11T13:35:00Z"]),
    ]);
    expect(observed).toEqual({ intervalNs: MINUTE, samplesPerPair: 3, pairs: 2 });
  });

  it("takes the median so one stalled pair cannot rewrite the number", () => {
    const observed = observedCadence([
      pair("a", ["2026-08-11T13:33:00Z", "2026-08-11T13:34:00Z"]),
      pair("b", ["2026-08-11T13:33:00Z", "2026-08-11T13:34:00Z"]),
      // An agent that went away and came back an hour later.
      pair("c", ["2026-08-11T13:33:00Z", "2026-08-11T14:33:00Z"]),
    ]);
    expect(observed?.intervalNs).toBe(MINUTE);
  });

  it("counts the SMALLEST per-pair total, so «≥ N per pair» is never an overstatement", () => {
    const observed = observedCadence([
      pair("a", ["2026-08-11T13:33:00Z", "2026-08-11T13:34:00Z", "2026-08-11T13:35:00Z"]),
      pair("b", ["2026-08-11T13:33:00Z", "2026-08-11T13:34:00Z"]),
    ]);
    expect(observed?.samplesPerPair).toBe(2);
  });

  it("drops rows whose timestamp is not a timestamp rather than measuring NaN", () => {
    expect(observedCadence([pair("a", ["yesterday", "also yesterday"])])).toBeUndefined();
    const observed = observedCadence([
      pair("a", ["2026-08-11T13:33:00Z", "not a date", "2026-08-11T13:35:00Z"]),
    ]);
    // Two usable stamps, one gap of two minutes.
    expect(observed?.intervalNs).toBe(2 * MINUTE);
  });

  it("never returns a negative cadence when a clock stepped backwards", () => {
    const observed = observedCadence([
      pair("a", ["2026-08-11T13:35:00Z", "2026-08-11T13:33:00Z"]),
    ]);
    expect(observed?.intervalNs).toBe(2 * MINUTE);
  });
});
