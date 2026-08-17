import { describe, expect, it } from "vitest";
import type { RunDetail, RunResult } from "./types";
import {
  aggregateSamples,
  effectivePlannedSamplesPerPair,
  effectiveSampleIntervalNs,
  formatCadenceNs,
  formatDurationNs,
  groupSamplesByPair,
  isIntervalRun,
  pairProgress,
  percentileNs,
  plannedCadenceFromSpec,
  runCadence,
  runDurationNs,
  snapshotForSample,
} from "./run-samples";

/**
 * run-samples under a HOSTILE wire.
 *
 * lib/run-samples.test.ts pins the arithmetic against the numbers the server
 * actually sends. This file pins it against the ones it must never send and
 * one day will: a duration that arrived as a string, a pairTotal that came
 * back "abc", a successful sample with no durationNs on it at all.
 *
 * The bar is a single sentence, and it is the owner's: nothing on the run
 * permalink may render the word NaN, Infinity or undefined. Every function
 * here feeds a tile, a caption or an aria-label, so a non-number that survives
 * one of them is a number an operator reads off the screen.
 */

function sample(over: Partial<RunResult> = {}): RunResult {
  return {
    sourceNode: "a",
    destinationNode: "b",
    success: true,
    durationNs: 1_000_000,
    recordedAt: "2026-07-28T10:00:00Z",
    sampleSeq: 0,
    ...over,
  };
}

/** The one assertion this whole file exists for. */
function expectRenderable(s: string) {
  expect(s).not.toMatch(/NaN|Infinity|undefined/);
}

/* ── a sample whose latency is not a latency ──────────────────────────────── */

describe("aggregateSamples refuses to average a non-number", () => {
  it("counts a successful sample with no duration as sent, and leaves it out of the latency", () => {
    // The shape the store would produce if durationNs were ever omitted from a
    // row: JSON.parse hands back `undefined`, and `undefined` in a sum is NaN
    // all the way onto the Avg tile.
    const agg = aggregateSamples([
      sample({ durationNs: undefined as unknown as number }),
      sample({ durationNs: 4_000_000 }),
    ]);
    expect(agg.sent).toBe(2);
    expect(agg.failed).toBe(0);
    expect(agg.minNs).toBe(4_000_000);
    expect(agg.maxNs).toBe(4_000_000);
    expect(agg.avgNs).toBe(4_000_000);
    expect(agg.p95Ns).toBe(4_000_000);
  });

  it("ignores a duration that arrived as a string rather than concatenating it", () => {
    const agg = aggregateSamples([
      sample({ durationNs: "2000000" as unknown as number }),
      sample({ durationNs: 6_000_000 }),
    ]);
    expect(agg.sent).toBe(2);
    expect(agg.avgNs).toBe(6_000_000);
    expect(Number.isFinite(agg.avgNs)).toBe(true);
  });

  it("ignores NaN and Infinity, which are numbers and still not latencies", () => {
    const agg = aggregateSamples([
      sample({ durationNs: Number.NaN }),
      sample({ durationNs: Number.POSITIVE_INFINITY }),
      sample({ durationNs: 3_000_000 }),
    ]);
    expect(agg.minNs).toBe(3_000_000);
    expect(agg.maxNs).toBe(3_000_000);
    expect(agg.avgNs).toBe(3_000_000);
  });

  it("reports no latency at all when every successful sample carried a bad one", () => {
    // Not zero: zero is a measurement. The card's own contract is that a run
    // with nothing measurable shows an em dash.
    const agg = aggregateSamples([sample({ durationNs: Number.NaN }), sample({ durationNs: undefined as unknown as number })]);
    expect(agg.sent).toBe(2);
    expect(agg.minNs).toBeUndefined();
    expect(agg.avgNs).toBeUndefined();
    expect(agg.p95Ns).toBeUndefined();
  });

  it("keeps a negative duration out of the distribution", () => {
    // A clock that went backwards is not a round trip.
    const agg = aggregateSamples([sample({ durationNs: -5 }), sample({ durationNs: 2_000_000 })]);
    expect(agg.minNs).toBe(2_000_000);
  });

  it("still counts every sample in `sent`, whatever its duration says", () => {
    const agg = aggregateSamples([
      sample({ durationNs: Number.NaN }),
      sample({ success: false, durationNs: Number.NaN }),
    ]);
    expect(agg.sent).toBe(2);
    expect(agg.failed).toBe(1);
    expect(agg.failRatio).toBe(0.5);
  });
});

/* ── the cadence mirror, fed a pair count that is not a count ─────────────── */

describe("the effective cadence stays a number", () => {
  const hostile: unknown[] = [Number.NaN, Number.POSITIVE_INFINITY, -1, "abc", undefined, null, "4"];

  it("never returns NaN for a hostile pair count", () => {
    for (const pairCount of hostile) {
      const ns = effectiveSampleIntervalNs(900e9, "mtr", pairCount as number, 2);
      expect(Number.isFinite(ns), `pairCount=${String(pairCount)} gave ${ns}`).toBe(true);
      expect(ns).toBeGreaterThan(0);
    }
  });

  it("never returns NaN for a hostile source count", () => {
    for (const sourceCount of hostile) {
      const ns = effectiveSampleIntervalNs(900e9, "mtr", 4, sourceCount as number);
      expect(Number.isFinite(ns), `sourceCount=${String(sourceCount)} gave ${ns}`).toBe(true);
      expect(ns).toBeGreaterThan(0);
    }
  });

  it("never plans a non-integer number of samples per pair", () => {
    for (const pairCount of hostile) {
      const n = effectivePlannedSamplesPerPair(900e9, "mtr", pairCount as number, 2);
      expect(Number.isInteger(n), `pairCount=${String(pairCount)} gave ${n}`).toBe(true);
      expect(n).toBeGreaterThanOrEqual(1);
    }
  });

  it("keeps the honest four-pair MTR answer while doing it", () => {
    // The rev13 acceptance number, unchanged by the guards above: 15m over four
    // pairs from two agents is one round every 90s, not every 5s.
    expect(effectiveSampleIntervalNs(900e9, "mtr", 4, 2)).toBe(90e9);
    expect(effectivePlannedSamplesPerPair(900e9, "mtr", 4, 2)).toBe(10);
  });
});

describe("runCadence survives a run whose own fields are wrong", () => {
  function run(over: Record<string, unknown>): Pick<RunDetail, "spec" | "type" | "pairTotal"> {
    return { spec: { Duration: 900e9 }, type: "mtr", pairTotal: 4, ...over } as Pick<
      RunDetail,
      "spec" | "type" | "pairTotal"
    >;
  }

  it("does not hand the cadence tile a NaN when pairTotal came back as a word", () => {
    const c = runCadence(run({ pairTotal: "abc" }), 2);
    expect(c).toBeDefined();
    expect(Number.isFinite(c!.intervalNs)).toBe(true);
    expectRenderable(formatCadenceNs(c!.intervalNs, "en"));
  });

  it("does not hand it a NaN when pairTotal is missing entirely", () => {
    const c = runCadence(run({ pairTotal: undefined }), 2);
    expect(Number.isFinite(c!.intervalNs)).toBe(true);
  });

  it("does not hand it a NaN when the source count is not a count", () => {
    const c = runCadence(run({}), Number.NaN);
    expect(Number.isFinite(c!.intervalNs)).toBe(true);
  });

  it("prefers the server's snapshot and ignores a snapshot written in strings", () => {
    const c = runCadence(
      run({ spec: { Duration: 900e9, PlannedSampleIntervalNs: "90000000000", PlannedSamplesPerPair: "10" } }),
      2,
    );
    // A string is not the server's number, so the derivation stands rather than
    // the tile claiming an authority it does not have.
    expect(c!.fromSpec).toBe(false);
    expect(Number.isFinite(c!.intervalNs)).toBe(true);
  });
});

/* ── the duration off the spec ────────────────────────────────────────────── */

describe("runDurationNs reads only a number", () => {
  const notDurations: unknown[] = [
    "900000000000",
    "15m",
    null,
    [],
    Number.NaN,
    Number.POSITIVE_INFINITY,
    -1,
    0,
    { Duration: 1 },
  ];

  it("treats every non-number Duration as an instant run rather than throwing", () => {
    for (const Duration of notDurations) {
      const run = { spec: { Duration } } as Pick<RunDetail, "spec">;
      expect(runDurationNs(run), `Duration=${String(Duration)}`).toBe(0);
      expect(isIntervalRun(run)).toBe(false);
    }
  });

  it("survives a spec that is not an object at all", () => {
    for (const spec of ["", 0, [], null, undefined, "spec"] as unknown[]) {
      expect(runDurationNs({ spec } as Pick<RunDetail, "spec">)).toBe(0);
    }
  });

  it("takes an absurd but finite duration without pretending it is instant", () => {
    expect(runDurationNs({ spec: { Duration: 1e18 } } as Pick<RunDetail, "spec">)).toBe(1e18);
    expectRenderable(formatDurationNs(1e18, "en"));
  });
});

describe("plannedCadenceFromSpec refuses everything but two positive numbers", () => {
  const bad: unknown[] = ["90000000000", null, Number.NaN, 0, -1, Number.POSITIVE_INFINITY, {}, []];

  it("falls back rather than trusting a non-number interval", () => {
    for (const PlannedSampleIntervalNs of bad) {
      expect(
        plannedCadenceFromSpec({ spec: { PlannedSampleIntervalNs, PlannedSamplesPerPair: 10 } } as Pick<RunDetail, "spec">),
        String(PlannedSampleIntervalNs),
      ).toBeUndefined();
    }
  });

  it("falls back rather than trusting a non-number sample count", () => {
    for (const PlannedSamplesPerPair of [...bad, 0.5]) {
      expect(
        plannedCadenceFromSpec({
          spec: { PlannedSampleIntervalNs: 90e9, PlannedSamplesPerPair },
        } as Pick<RunDetail, "spec">),
        String(PlannedSamplesPerPair),
      ).toBeUndefined();
    }
  });
});

/* ── the tick strip's frame ───────────────────────────────────────────────── */

describe("pairProgress frames a run that overshot its own plan", () => {
  it("widens to the arrivals rather than reporting a negative tail", () => {
    const p = pairProgress(60e9, 40, { intervalNs: 5e9, samplesPerPair: 12 });
    expect(p.expected).toBe(40);
    expect(p.remaining).toBe(0);
    expect(p.remainingNs).toBe(0);
  });

  it("never returns a non-number when the arrival count is nonsense", () => {
    for (const arrived of [Number.NaN, -3, Number.POSITIVE_INFINITY] as number[]) {
      const p = pairProgress(60e9, arrived, { intervalNs: 5e9, samplesPerPair: 12 });
      expect(Number.isFinite(p.expected), `arrived=${arrived}`).toBe(true);
      expect(Number.isFinite(p.remaining)).toBe(true);
      expect(Number.isFinite(p.remainingNs)).toBe(true);
      expect(p.remaining).toBeGreaterThanOrEqual(0);
    }
  });

  it("never returns a non-number when the cadence it is handed is nonsense", () => {
    const p = pairProgress(60e9, 3, { intervalNs: Number.NaN, samplesPerPair: Number.NaN });
    expect(Number.isFinite(p.expected)).toBe(true);
    expect(Number.isFinite(p.remainingNs)).toBe(true);
    expectRenderable(formatDurationNs(p.remainingNs, "en"));
  });
});

/* ── the rendered spans ───────────────────────────────────────────────────── */

describe("a rendered span is never the word NaN", () => {
  const nonsense = [Number.NaN, Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY, -1];

  it("holds for formatDurationNs in both languages", () => {
    for (const ns of nonsense) {
      for (const locale of ["en", "ru"]) expectRenderable(formatDurationNs(ns, locale));
    }
  });

  it("holds for formatCadenceNs in both languages", () => {
    for (const ns of nonsense) {
      for (const locale of ["en", "ru"]) expectRenderable(formatCadenceNs(ns, locale));
    }
  });

  it("still says 90s rather than 2m for the stretched MTR cadence", () => {
    expect(formatCadenceNs(90e9, "en")).toBe("90s");
    expect(formatCadenceNs(90e9, "ru")).toBe("90 с");
    expect(formatCadenceNs(120e9, "en")).toBe("2m");
  });

  it("falls back to English units for a locale nobody has written yet", () => {
    expect(formatDurationNs(5e9, "kl")).toBe("5s");
  });
});

/* ── grouping ─────────────────────────────────────────────────────────────── */

describe("groupSamplesByPair keeps pairs apart", () => {
  it("does not merge two pairs whose names collide across the separator", () => {
    const groups = groupSamplesByPair([
      sample({ sourceNode: "a\0b", destinationNode: "c" }),
      sample({ sourceNode: "a", destinationNode: "b\0c" }),
    ]);
    // The NUL join is only unambiguous because no node name may carry one. If
    // one ever does, two different pairs must not become one row.
    expect(groups).toHaveLength(2);
  });

  it("takes an empty result list without inventing a row", () => {
    expect(groupSamplesByPair([])).toEqual([]);
  });

  it("keeps first-seen order so the strip does not reshuffle between renders", () => {
    const groups = groupSamplesByPair([
      sample({ sourceNode: "z" }),
      sample({ sourceNode: "a" }),
      sample({ sourceNode: "z" }),
    ]);
    expect(groups.map((g) => g.source)).toEqual(["z", "a"]);
    expect(groups[0].samples).toHaveLength(2);
  });
});

/* ── percentiles and snapshots ────────────────────────────────────────────── */

describe("percentileNs at the edges", () => {
  it("answers a measured value at p0 and p100 rather than reaching past the array", () => {
    expect(percentileNs([1, 2, 3], 0)).toBe(1);
    expect(percentileNs([1, 2, 3], 100)).toBe(3);
    expect(percentileNs([1, 2, 3], 1000)).toBe(3);
    expect(percentileNs([1, 2, 3], -50)).toBe(1);
  });

  it("has nothing to answer for an empty distribution", () => {
    expect(percentileNs([], 95)).toBeUndefined();
  });
});

describe("snapshotForSample refuses to guess", () => {
  const snaps = [{ id: "s1", firstSeen: "2026-07-28T10:00:00Z", lastSeen: "2026-07-28T10:05:00Z" }];

  it("covers both ends of the window", () => {
    expect(snapshotForSample(snaps, "2026-07-28T10:00:00Z")?.id).toBe("s1");
    expect(snapshotForSample(snaps, "2026-07-28T10:05:00Z")?.id).toBe("s1");
  });

  it("answers nothing for an instant nothing covers, rather than the nearest route", () => {
    expect(snapshotForSample(snaps, "2026-07-28T09:50:00Z")).toBeUndefined();
  });

  /* The trace that CREATED a route is recorded a few hundred microseconds BEFORE the route row is
     stamped (the result write lands first), so on routes stored before that was fixed the creating
     probe sat just outside its own route's window — and the tick that made the route was the one
     tick told no route covered it. A five-second grace on the leading edge covers that skew and
     nothing else: ten seconds early is still nothing. */
  it("still covers the probe that created the route, stamped microseconds before it", () => {
    expect(snapshotForSample(snaps, "2026-07-28T09:59:59.9Z")?.id).toBe("s1");
    expect(snapshotForSample(snaps, "2026-07-28T09:59:56Z")?.id).toBe("s1");
    expect(snapshotForSample(snaps, "2026-07-28T09:59:50Z")).toBeUndefined();
  });

  it("answers nothing for a timestamp that is not one", () => {
    for (const ts of ["", "not-a-time", "0000", "%%%"]) {
      expect(snapshotForSample(snaps, ts), ts).toBeUndefined();
    }
    expect(snapshotForSample(snaps, undefined)).toBeUndefined();
  });

  it("skips a stored window whose own bounds are unparseable", () => {
    const broken = [{ id: "bad", firstSeen: "nope", lastSeen: "nope" }, ...snaps];
    expect(snapshotForSample(broken, "2026-07-28T10:01:00Z")?.id).toBe("s1");
  });
});
