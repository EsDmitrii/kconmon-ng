import { describe, expect, it } from "vitest";
import { translate, type Translate } from "@/lib/i18n";
import {
  investigationSourcesDict,
  type InvestigationSourcesKey,
} from "@/lib/i18n/dict/investigation-sources";
import {
  anomalyOnset,
  CAUSE_WEIGHTS,
  DEFAULT_CAUSE_WINDOW_SECONDS,
  DEFAULT_THRESHOLDS,
  mergeTimeline,
  pageOfIndex,
  rankCauses,
  thresholdCrossings,
  TIMELINE_DEFAULT_PAGE_SIZE,
  TIMELINE_PAGE_SIZES,
  timelineSlice,
  type TimelineEntry,
  type TimelineKind,
} from "./investigation";

// A fixed epoch keeps every expectation readable as "T + n seconds" instead of
// as an ISO string nobody can diff by eye.
const T0 = Date.parse("2026-08-08T12:00:00.000Z");
const at = (offsetSeconds: number) => new Date(T0 + offsetSeconds * 1000);

function entry(
  offsetSeconds: number,
  kind: TimelineKind,
  id?: string,
  over: Partial<TimelineEntry> = {},
): TimelineEntry {
  return {
    at: at(offsetSeconds),
    kind,
    severity: "info",
    title: `${kind}@${offsetSeconds}`,
    ...(id === undefined ? {} : { ref: { kind, id } }),
    ...over,
  };
}

/** A deterministic shuffle — a seeded rotation, so "shuffled input" in a test
 *  name means the same permutation on every run and a failure is reproducible. */
function shuffled<T>(items: T[], seed: number): T[] {
  const out = [...items];
  for (let i = out.length - 1; i > 0; i--) {
    seed = (seed * 1103515245 + 12345) % 2147483648;
    const j = seed % (i + 1);
    [out[i], out[j]] = [out[j], out[i]];
  }
  return out;
}

const titles = (entries: TimelineEntry[]) => entries.map((e) => e.title);
const times = (entries: TimelineEntry[]) => entries.map((e) => e.at.getTime());

describe("mergeTimeline", () => {
  it("returns [] for no sources and for empty sources", () => {
    expect(mergeTimeline()).toEqual([]);
    expect(mergeTimeline([], [], [])).toEqual([]);
  });

  it("orders every source's entries by time ascending", () => {
    const a = [entry(30, "event"), entry(10, "event")];
    const b = [entry(20, "audit"), entry(0, "audit")];
    expect(times(mergeTimeline(a, b))).toEqual([T0, T0 + 10_000, T0 + 20_000, T0 + 30_000]);
  });

  it("does not mutate its input arrays", () => {
    const a = [entry(30, "event"), entry(10, "event")];
    const snapshot = [...a];
    mergeTimeline(a);
    expect(a).toEqual(snapshot);
  });

  it("breaks same-instant ties by kind ascending", () => {
    const input = [entry(0, "run", "r"), entry(0, "audit", "a"), entry(0, "k8s", "k")];
    expect(titles(mergeTimeline(input))).toEqual(["audit@0", "k8s@0", "run@0"]);
  });

  it("breaks same-instant same-kind ties by ref id ascending", () => {
    const merged = mergeTimeline([entry(0, "event", "c"), entry(0, "event", "a"), entry(0, "event", "b")]);
    expect(merged.map((e) => e.ref?.id)).toEqual(["a", "b", "c"]);
  });

  it("sorts refless entries before referenced ones at the same instant and kind", () => {
    const merged = mergeTimeline([entry(0, "event", "a"), entry(0, "event")]);
    expect(merged.map((e) => e.ref?.id)).toEqual([undefined, "a"]);
  });

  it("is stable for full ties — argument order then source order wins", () => {
    const first: TimelineEntry = { ...entry(0, "event", "x"), title: "first" };
    const second: TimelineEntry = { ...entry(0, "event", "x"), title: "second" };
    // Different sources, identical (at, kind, id): source-1-then-source-2.
    expect(titles(mergeTimeline([first], [second]))).toEqual(["first"]);
  });

  it("dedupes identical (kind,id) refs keeping the earliest", () => {
    const late: TimelineEntry = { ...entry(60, "k8s", "uid-1"), title: "late copy" };
    const early: TimelineEntry = { ...entry(10, "k8s", "uid-1"), title: "early copy" };
    expect(titles(mergeTimeline([late], [early]))).toEqual(["early copy"]);
  });

  it("dedupes across three sources, not just two", () => {
    const dupes = [entry(90, "audit", "same"), entry(5, "audit", "same"), entry(45, "audit", "same")];
    const merged = mergeTimeline([dupes[0]], [dupes[1]], [dupes[2]]);
    expect(times(merged)).toEqual([T0 + 5_000]);
  });

  it("treats the same id under different kinds as different entries", () => {
    const merged = mergeTimeline([entry(0, "event", "7"), entry(0, "k8s", "7")]);
    expect(merged.map((e) => e.kind)).toEqual(["event", "k8s"]);
  });

  it("never dedupes refless entries, however identical they look", () => {
    const twin: TimelineEntry = entry(0, "annotation");
    expect(mergeTimeline([twin], [{ ...twin }])).toHaveLength(2);
  });

  it("produces the same output for shuffled input", () => {
    const source = [
      entry(0, "event", "a"),
      entry(0, "audit", "b"),
      entry(30, "k8s", "c"),
      entry(30, "k8s", "d"),
      entry(15, "annotation", "e"),
      entry(90, "run", "f"),
    ];
    const baseline = mergeTimeline(source);
    for (const seed of [1, 7, 4242]) {
      expect(titles(mergeTimeline(shuffled(source, seed)))).toEqual(titles(baseline));
    }
  });
});

describe("thresholdCrossings — loss edge semantics", () => {
  const loss = (offsetSeconds: number, value: number) => ({ at: at(offsetSeconds), loss: value });

  it("returns [] for an empty series", () => {
    expect(thresholdCrossings([])).toEqual([]);
  });

  it("returns [] when every sample stays at or below the threshold", () => {
    // 1% is DEFAULT_THRESHOLDS.lossPct, so 0.01 is AT the line, not above it.
    expect(thresholdCrossings([loss(0, 0), loss(30, 0.005), loss(60, 0.01)])).toEqual([]);
  });

  it("emits ONE entry for a level that stays above across three samples", () => {
    const out = thresholdCrossings([loss(0, 0.05), loss(30, 0.06), loss(60, 0.07)]);
    expect(out).toHaveLength(1);
    expect(out[0].at.getTime()).toBe(T0);
    expect(out[0].kind).toBe("threshold");
    expect(out[0].severity).toBe("warn");
  });

  it("emits crossing → recovered → crossing for a dip and a second breach", () => {
    const out = thresholdCrossings([
      loss(0, 0),
      loss(30, 0.05), // crossing 1
      loss(60, 0), // recovered
      loss(90, 0.05), // crossing 2
    ]);
    expect(out.map((e) => [e.at.getTime() - T0, e.severity])).toEqual([
      [30_000, "warn"],
      [60_000, "info"],
      [90_000, "warn"],
    ]);
  });

  it("does not emit a recovered entry when the series ends while still above", () => {
    const out = thresholdCrossings([loss(0, 0), loss(30, 0.05)]);
    expect(out.map((e) => e.severity)).toEqual(["warn"]);
  });

  it("does not emit a recovered entry for a series that starts above and never dips", () => {
    expect(thresholdCrossings([loss(0, 0.5)]).map((e) => e.severity)).toEqual(["warn"]);
  });

  it("honours a custom lossPct", () => {
    const series = [loss(0, 0.02), loss(30, 0.06)];
    expect(thresholdCrossings(series, { lossPct: 5, rttFactor: 2 })).toHaveLength(1);
    expect(thresholdCrossings(series, { lossPct: 10, rttFactor: 2 })).toHaveLength(0);
  });

  it("ignores samples with no loss field rather than reading them as zero", () => {
    const out = thresholdCrossings([loss(0, 0.05), { at: at(30) }, loss(60, 0.05)]);
    expect(out).toHaveLength(1);
  });

  it("sorts the series by time before detecting edges", () => {
    const out = thresholdCrossings([loss(60, 0), loss(30, 0.05), loss(0, 0)]);
    expect(out.map((e) => [e.at.getTime() - T0, e.severity])).toEqual([
      [30_000, "warn"],
      [60_000, "info"],
    ]);
  });

  it("gives every crossing a distinct ref so merges never collapse them", () => {
    const out = thresholdCrossings([loss(0, 0.05), loss(30, 0), loss(60, 0.05)]);
    const ids = out.map((e) => e.ref?.id);
    expect(new Set(ids).size).toBe(ids.length);
    expect(out.every((e) => e.ref?.kind === "threshold")).toBe(true);
  });
});

describe("thresholdCrossings — RTT median baseline", () => {
  const rtt = (offsetSeconds: number, ns: number) => ({ at: at(offsetSeconds), rttNs: ns });

  it.each([
    {
      name: "odd length → the middle value (median 20ms, threshold 40ms)",
      series: [rtt(0, 10e6), rtt(30, 20e6), rtt(60, 30e6)],
      wantCrossings: 0,
    },
    {
      name: "even length → the mean of the two middles (median 25ms, threshold 50ms)",
      series: [rtt(0, 10e6), rtt(30, 20e6), rtt(60, 30e6), rtt(90, 40e6)],
      wantCrossings: 0,
    },
    {
      name: "even length with a spike above 2× the median (median 25ms) ",
      series: [rtt(0, 10e6), rtt(30, 20e6), rtt(60, 30e6), rtt(90, 200e6)],
      wantCrossings: 1,
    },
  ])("median: $name", ({ series, wantCrossings }) => {
    expect(thresholdCrossings(series).filter((e) => e.severity !== "info")).toHaveLength(wantCrossings);
  });

  it("computes the median from unsorted samples", () => {
    // Same four samples as the even-length case, in scrambled arrival order.
    const out = thresholdCrossings([rtt(90, 200e6), rtt(0, 10e6), rtt(60, 30e6), rtt(30, 20e6)]);
    expect(out.map((e) => e.at.getTime() - T0)).toEqual([90_000]);
  });

  it("is not skewed by the spike it is detecting (a mean baseline would miss it)", () => {
    // mean = 100ms → 2× = 200ms, and the 300ms spike alone would not clear a
    // mean-based bar built the same way once more spikes arrive. Median = 10ms.
    const series = [rtt(0, 10e6), rtt(30, 10e6), rtt(60, 10e6), rtt(90, 30e6), rtt(120, 450e6)];
    const mean = series.reduce((s, p) => s + p.rttNs, 0) / series.length;
    expect(thresholdCrossings(series).filter((e) => e.severity !== "info")).toHaveLength(1);
    expect(series[3].rttNs).toBeLessThan(mean * DEFAULT_THRESHOLDS.rttFactor);
    expect(series[4].rttNs).toBeGreaterThan(10e6 * DEFAULT_THRESHOLDS.rttFactor);
  });

  it("applies edge semantics to RTT too — one entry per level, plus a recovery", () => {
    const out = thresholdCrossings([
      rtt(0, 10e6),
      rtt(30, 100e6),
      rtt(60, 100e6),
      rtt(90, 10e6),
      rtt(120, 10e6),
    ]);
    expect(out.map((e) => [e.at.getTime() - T0, e.severity])).toEqual([
      [30_000, "warn"],
      [90_000, "info"],
    ]);
  });

  it("emits nothing when there are no RTT samples at all", () => {
    expect(thresholdCrossings([{ at: at(0) }, { at: at(30) }])).toEqual([]);
  });

  it("emits nothing when the median is zero — a zero baseline is not a baseline", () => {
    expect(thresholdCrossings([rtt(0, 0), rtt(30, 0), rtt(60, 5e6)])).toEqual([]);
  });

  it("honours a custom rttFactor", () => {
    const series = [rtt(0, 10e6), rtt(30, 10e6), rtt(60, 25e6)];
    expect(thresholdCrossings(series, { lossPct: 1, rttFactor: 2 })).toHaveLength(1);
    expect(thresholdCrossings(series, { lossPct: 1, rttFactor: 5 })).toHaveLength(0);
  });

  it("interleaves loss and RTT entries in time order", () => {
    const out = thresholdCrossings([
      { at: at(0), loss: 0, rttNs: 10e6 },
      { at: at(30), loss: 0, rttNs: 100e6 }, // rtt crossing
      { at: at(60), loss: 0.5, rttNs: 10e6 }, // loss crossing + rtt recovery
    ]);
    expect(out.map((e) => e.at.getTime() - T0)).toEqual([30_000, 60_000, 60_000]);
  });
});

describe("DEFAULT_THRESHOLDS", () => {
  it("is the documented pair (loss 1%, RTT 2× median)", () => {
    expect(DEFAULT_THRESHOLDS).toEqual({ lossPct: 1, rttFactor: 2 });
  });
});

describe("anomalyOnset", () => {
  const crossing = (offsetSeconds: number): TimelineEntry => ({
    at: at(offsetSeconds),
    kind: "threshold",
    severity: "warn",
    title: `crossing@${offsetSeconds}`,
  });

  it("returns null for an empty timeline", () => {
    expect(anomalyOnset([])).toBeNull();
  });

  it("returns null when nothing crossed a threshold", () => {
    expect(anomalyOnset([entry(0, "event", "a"), entry(30, "k8s", "b")])).toBeNull();
  });

  it("returns the earliest threshold entry's at", () => {
    expect(anomalyOnset([crossing(90), crossing(10), crossing(45)])?.getTime()).toBe(T0 + 10_000);
  });

  it("ignores non-threshold entries that are earlier", () => {
    expect(anomalyOnset([entry(0, "k8s", "a"), crossing(60)])?.getTime()).toBe(T0 + 60_000);
  });

  it("ignores recovered entries — a recovery is not an onset", () => {
    const recovered: TimelineEntry = { ...crossing(10), severity: "info", title: "recovered" };
    expect(anomalyOnset([recovered, crossing(60)])?.getTime()).toBe(T0 + 60_000);
  });

  it("returns null when the only threshold entries are recoveries", () => {
    expect(anomalyOnset([{ ...crossing(10), severity: "info" }])).toBeNull();
  });

  it("reads the onset straight out of thresholdCrossings", () => {
    const out = thresholdCrossings([
      { at: at(0), loss: 0 },
      { at: at(30), loss: 0.05 },
      { at: at(60), loss: 0 },
    ]);
    expect(anomalyOnset(out)?.getTime()).toBe(T0 + 30_000);
  });
});

describe("CAUSE_WEIGHTS", () => {
  it("is the documented table, verbatim", () => {
    expect(CAUSE_WEIGHTS).toEqual({
      "path-change": 3,
      k8s: 3,
      event: 2,
      audit: 2,
      maintenance: 1,
      annotation: 0,
      run: 0,
      threshold: 0,
      alert: 0,
    });
  });

  it("weights alerts 0, the same as the threshold rows they restate", () => {
    expect(CAUSE_WEIGHTS.alert).toBe(0);
    expect(CAUSE_WEIGHTS.alert).toBe(CAUSE_WEIGHTS.threshold);
  });

  it("scores every TimelineKind — a new kind cannot slip in unweighted", () => {
    const kinds: TimelineKind[] = [
      "event",
      "audit",
      "annotation",
      "path-change",
      "run",
      "k8s",
      "maintenance",
      "threshold",
      "alert",
    ];
    expect(Object.keys(CAUSE_WEIGHTS).sort()).toEqual([...kinds].sort());
  });

  it("defaults the candidate window to 300 seconds", () => {
    expect(DEFAULT_CAUSE_WINDOW_SECONDS).toBe(300);
  });
});

describe("rankCauses", () => {
  const onset = at(600);

  it("returns [] for an empty timeline", () => {
    expect(rankCauses([], onset)).toEqual([]);
  });

  it.each([
    { name: "exactly at the onset", offsetSeconds: 600, included: true },
    { name: "one second before the onset", offsetSeconds: 599, included: true },
    { name: "exactly at the window edge (onset - 300s)", offsetSeconds: 300, included: true },
    { name: "one second past the window edge (onset - 301s)", offsetSeconds: 299, included: false },
    { name: "one second after the onset", offsetSeconds: 601, included: false },
    { name: "long after the onset", offsetSeconds: 3600, included: false },
  ])("window boundary: $name → included=$included", ({ offsetSeconds, included }) => {
    const ranked = rankCauses([entry(offsetSeconds, "k8s", "x")], onset);
    expect(ranked).toHaveLength(included ? 1 : 0);
  });

  it.each([
    { name: "at the onset: full weight", offsetSeconds: 600, kind: "k8s" as const, want: 3 },
    { name: "60s before: 3 × (1 - 60/300)", offsetSeconds: 540, kind: "k8s" as const, want: 2.4 },
    { name: "150s before: half decay on weight 2", offsetSeconds: 450, kind: "audit" as const, want: 1 },
    { name: "270s before: weight 1 nearly spent", offsetSeconds: 330, kind: "maintenance" as const, want: 0.1 },
    { name: "at the window edge: decayed to zero", offsetSeconds: 300, kind: "path-change" as const, want: 0 },
  ])("score: $name", ({ offsetSeconds, kind, want }) => {
    const [ranked] = rankCauses([entry(offsetSeconds, kind, "x")], onset);
    expect(ranked.score).toBeCloseTo(want, 6);
  });

  it("scores sub-second proximity without rounding it away", () => {
    const halfSecondBefore = new Date(onset.getTime() - 500);
    const [ranked] = rankCauses([{ ...entry(0, "k8s", "x"), at: halfSecondBefore }], onset);
    expect(ranked.score).toBeCloseTo(3 * (1 - 0.5 / 300), 6);
  });

  it("honours a custom windowSeconds", () => {
    const entries = [entry(540, "k8s", "in-60s")]; // 60s before onset
    expect(rankCauses(entries, onset, { windowSeconds: 60 })[0].score).toBeCloseTo(0, 6);
    expect(rankCauses(entries, onset, { windowSeconds: 30 })).toEqual([]);
    expect(rankCauses(entries, onset, { windowSeconds: 120 })[0].score).toBeCloseTo(1.5, 6);
  });

  it.each([0, -1, Number.NaN])("returns [] for a non-positive window (%s)", (windowSeconds) => {
    expect(rankCauses([entry(600, "k8s", "x")], onset, { windowSeconds })).toEqual([]);
  });

  it.each(["annotation", "run", "threshold", "alert"] as const)("never ranks zero-weight kind %s", (kind) => {
    expect(rankCauses([entry(600, kind, "x")], onset)).toEqual([]);
  });

  it("keeps zero-weight kinds out even when they are the closest thing to the onset", () => {
    const ranked = rankCauses([entry(600, "annotation", "note"), entry(400, "audit", "cfg")], onset);
    expect(ranked.map((r) => r.entry.kind)).toEqual(["audit"]);
  });

  it("sorts by score descending", () => {
    const ranked = rankCauses(
      [
        entry(310, "k8s", "far-heavy"), // 290s before, weight 3 → 0.1
        entry(590, "maintenance", "near-light"), // 10s before, weight 1 → 0.966…
        entry(450, "audit", "mid"), // 150s before, weight 2 → 1
      ],
      onset,
    );
    expect(ranked.map((r) => r.entry.ref?.id)).toEqual(["mid", "near-light", "far-heavy"]);
    expect(ranked.map((r) => r.score)).toEqual([...ranked.map((r) => r.score)].sort((a, b) => b - a));
  });

  it("breaks equal scores newest first", () => {
    // audit@450 is 150s before onset: 2 × (1 - 0.5) = 1. maintenance@600 sits
    // ON the onset: 1 × 1 = 1. Both halves land on exactly-representable
    // doubles, so this is a real tie and not a float near-miss. Newest wins.
    const ranked = rankCauses([entry(450, "audit", "older"), entry(600, "maintenance", "newer")], onset);
    expect(ranked.map((r) => r.score)).toEqual([1, 1]);
    expect(ranked.map((r) => r.entry.ref?.id)).toEqual(["newer", "older"]);
  });

  it("breaks same-score same-instant ties deterministically by kind then id", () => {
    const same = [entry(600, "k8s", "b"), entry(600, "k8s", "a"), entry(600, "path-change", "c")];
    const ranked = rankCauses(same, onset);
    expect(ranked.map((r) => `${r.entry.kind}:${r.entry.ref?.id}`)).toEqual([
      "k8s:a",
      "k8s:b",
      "path-change:c",
    ]);
  });

  it("is deterministic across shuffled input", () => {
    const source = [
      entry(600, "k8s", "a"),
      entry(590, "audit", "b"),
      entry(480, "k8s", "c"),
      entry(570, "audit", "d"),
      entry(300, "path-change", "e"),
      entry(200, "k8s", "too-old"),
      entry(650, "k8s", "after-onset"),
      entry(595, "annotation", "note"),
    ];
    const baseline = rankCauses(source, onset);
    for (const seed of [1, 7, 4242, 99991]) {
      const again = rankCauses(shuffled(source, seed), onset);
      expect(again.map((r) => [r.entry.ref?.id, r.score])).toEqual(
        baseline.map((r) => [r.entry.ref?.id, r.score]),
      );
    }
  });

  it("does not mutate its input array", () => {
    const source = [entry(600, "k8s", "a"), entry(590, "audit", "b")];
    const snapshot = [...source];
    rankCauses(source, onset);
    expect(source).toEqual(snapshot);
  });

  it("hands back the original entry objects, not copies", () => {
    const original = entry(600, "k8s", "a");
    expect(rankCauses([original], onset)[0].entry).toBe(original);
  });

  it("ranks a realistic merged timeline end to end", () => {
    const crossings = thresholdCrossings([
      { at: at(0), loss: 0 },
      { at: at(600), loss: 0.08 },
    ]);
    const context = [
      entry(480, "path-change", "hop-7"), // 120s before onset → 3 × 0.6 = 1.8
      entry(560, "k8s", "node-cordoned"), // 40s before onset → 3 × 0.8666… = 2.6
      entry(100, "audit", "old-config"), // 500s before onset → out of window
      entry(605, "annotation", "operator-note"), // after onset AND weightless
    ];
    const timeline = mergeTimeline(context, crossings);
    const found = anomalyOnset(timeline);
    expect(found?.getTime()).toBe(T0 + 600_000);
    const ranked = rankCauses(timeline, found as Date);
    expect(ranked.map((r) => r.entry.ref?.id)).toEqual(["node-cordoned", "hop-7"]);
    expect(ranked[0].score).toBeCloseTo(2.6, 6);
    expect(ranked[1].score).toBeCloseTo(1.8, 6);
  });
});

/* the timeline's client-side pagination Arithmetic only. */

describe("TIMELINE_PAGE_SIZES / TIMELINE_DEFAULT_PAGE_SIZE", () => {
  it("offers exactly the three sizes the owner asked for, ascending", () => {
    expect([...TIMELINE_PAGE_SIZES]).toEqual([10, 50, 100]);
  });

  it("defaults to the middle one", () => {
    expect(TIMELINE_DEFAULT_PAGE_SIZE).toBe(50);
    expect(TIMELINE_PAGE_SIZES).toContain(TIMELINE_DEFAULT_PAGE_SIZE);
  });
});

describe("timelineSlice", () => {
  it("cuts the first page at [0, size)", () => {
    expect(timelineSlice(137, 1, 50)).toEqual({ page: 1, pageCount: 3, start: 0, end: 50 });
  });

  it("cuts a middle page at exactly one size-worth", () => {
    expect(timelineSlice(137, 2, 50)).toEqual({ page: 2, pageCount: 3, start: 50, end: 100 });
  });

  it("stops the LAST page at the total rather than past it", () => {
    expect(timelineSlice(137, 3, 50)).toEqual({ page: 3, pageCount: 3, start: 100, end: 137 });
  });

  it("counts an exact multiple as whole pages — never a trailing empty one", () => {
    expect(timelineSlice(100, 1, 50).pageCount).toBe(2);
    expect(timelineSlice(100, 2, 50)).toEqual({ page: 2, pageCount: 2, start: 50, end: 100 });
  });

  it("is ONE page for an empty list — 'Page 1 of 0' is not a thing the reader can be on", () => {
    expect(timelineSlice(0, 1, 50)).toEqual({ page: 1, pageCount: 1, start: 0, end: 0 });
  });

  it("clamps a page below the first one", () => {
    expect(timelineSlice(137, 0, 50).page).toBe(1);
    expect(timelineSlice(137, -4, 50).page).toBe(1);
  });

  /* The live-shifting case: the reader sits on page 7, the window's sources
     re-fetch and the list is suddenly 30 rows long. Clamping to the LAST page
     shows rows; honouring the stale number shows an empty box under a header
     that just claimed 30 entries. */
  it("clamps a page that ran off the end of a list that shrank underneath it", () => {
    expect(timelineSlice(30, 7, 10)).toEqual({ page: 3, pageCount: 3, start: 20, end: 30 });
  });

  it("survives a garbage page number instead of slicing with NaN", () => {
    expect(timelineSlice(137, Number.NaN, 50).page).toBe(1);
    expect(timelineSlice(137, Number.POSITIVE_INFINITY, 50)).toEqual({ page: 3, pageCount: 3, start: 100, end: 137 });
  });

  it("falls back to the default size rather than dividing by zero", () => {
    expect(timelineSlice(137, 1, 0)).toEqual({ page: 1, pageCount: 3, start: 0, end: 50 });
    expect(timelineSlice(137, 1, Number.NaN).end).toBe(50);
  });

  it("never returns a slice wider than the size, at any page of any total", () => {
    for (const total of [0, 1, 9, 10, 11, 49, 50, 51, 99, 100, 101, 137, 1000]) {
      for (const size of TIMELINE_PAGE_SIZES) {
        for (let p = 1; p <= Math.max(1, Math.ceil(total / size)); p++) {
          const s = timelineSlice(total, p, size);
          expect(s.end - s.start).toBeLessThanOrEqual(size);
          expect(s.start).toBeGreaterThanOrEqual(0);
          expect(s.end).toBeLessThanOrEqual(total);
          expect(s.page).toBe(p);
        }
      }
    }
  });

  it("walking every page covers the list exactly once, with no gap and no repeat", () => {
    const total = 137;
    const seen: number[] = [];
    const { pageCount } = timelineSlice(total, 1, 50);
    for (let p = 1; p <= pageCount; p++) {
      const s = timelineSlice(total, p, 50);
      for (let i = s.start; i < s.end; i++) seen.push(i);
    }
    expect(seen).toEqual(Array.from({ length: total }, (_, i) => i));
  });
});

describe("pageOfIndex (the page-size anchor)", () => {
  it("puts the entry that was first on screen back on screen at the new size", () => {
    // 10-per-page, page 5 → the first visible row is entry #40. At 50 per page
    // that row lives on page 1; at 100 it also lives on page 1.
    expect(pageOfIndex(40, 50)).toBe(1);
    expect(pageOfIndex(40, 100)).toBe(1);
    // …and the other direction: row #40 at 10 per page is page 5 again.
    expect(pageOfIndex(40, 10)).toBe(5);
  });

  it("is page 1 for the top of the list and for a nonsense index", () => {
    expect(pageOfIndex(0, 10)).toBe(1);
    expect(pageOfIndex(-1, 10)).toBe(1);
    expect(pageOfIndex(Number.NaN, 10)).toBe(1);
  });

  it("round-trips with timelineSlice: anchoring on a page's own start keeps that page", () => {
    for (const size of TIMELINE_PAGE_SIZES) {
      for (let p = 1; p <= 4; p++) {
        expect(pageOfIndex(timelineSlice(1000, p, size).start, size)).toBe(p);
      }
    }
  });
});

/* ── QA scope 3 ─────────────────────────────────────────────────────────── */

describe("threshold rows speak the interface's language (finding #6)", () => {
  const ru: Translate<InvestigationSourcesKey> = (key, vars) =>
    translate(investigationSourcesDict, "ru", key, vars);

  const samples = [
    { at: new Date(T0), loss: 0.0001 },
    { at: new Date(T0 + 60_000), loss: 0.5 },
    { at: new Date(T0 + 120_000), loss: 0.0001 },
  ];

  it("still answers ENGLISH with no translator, so every existing caller is unmoved", () => {
    const [above, back] = thresholdCrossings(samples);
    expect(above.title).toBe("Packet loss crossed the threshold");
    expect(back.title).toBe("Packet loss recovered");
    expect(above.detail).toBe("50.00% (threshold 1%)");
  });

  it("renders the four headlines in Russian, on the «порог» the badge already says", () => {
    const [above, back] = thresholdCrossings(samples, DEFAULT_THRESHOLDS, ru);
    expect(above.title).toBe("Потери пакетов перешли порог");
    expect(back.title).toBe("Потери пакетов вернулись в норму");
    expect(above.detail).toBe("50.00% (порог 1%)");
    // The badge's own word, so one concept is one word on one row.
    expect(above.title).toContain("порог");
  });

  it("translates the RTT pair too, numbers and all", () => {
    const rtt = [
      { at: new Date(T0), rttNs: 1e6 },
      { at: new Date(T0 + 60_000), rttNs: 1e6 },
      { at: new Date(T0 + 120_000), rttNs: 9e6 },
    ];
    const [crossed] = thresholdCrossings(rtt, DEFAULT_THRESHOLDS, ru);
    expect(crossed.title).toBe("RTT перешёл порог");
    expect(crossed.detail).toBe("9.00ms (порог 2.00ms = 2× медианы 1.00ms)");
  });

  it("keeps the ref id IDENTICAL in both languages — identity is never translated", () => {
    const en = thresholdCrossings(samples);
    const rus = thresholdCrossings(samples, DEFAULT_THRESHOLDS, ru);
    expect(rus.map((e) => e.ref?.id)).toEqual(en.map((e) => e.ref?.id));
  });
});

describe("a READ is never a cause (finding #8)", () => {
  const onset = new Date(T0 + 300_000);
  const at = new Date(T0 + 240_000);
  const audit = (over: Partial<TimelineEntry> = {}): TimelineEntry => ({
    at,
    kind: "audit",
    severity: "info",
    title: "POST /api/v1/promql/query",
    readOnly: true,
    ref: { kind: "audit", id: "1757" },
    ...over,
  });

  it("drops a read-only audit row from the ranking entirely", () => {
    expect(rankCauses([audit()], onset)).toEqual([]);
  });

  it("keeps a WRITE at its weight — the exclusion is about the method, not the source", () => {
    const ranked = rankCauses([audit({ readOnly: false, title: "POST /api/v1/targets" })], onset);
    expect(ranked.length).toBe(1);
    expect(ranked[0].score).toBeGreaterThan(0);
  });

  it("leaves the row in the TIMELINE — the badge already says 'audit' honestly", () => {
    // mergeTimeline is the timeline; rankCauses is the suspect list. Only the
    // second one filters.
    expect(mergeTimeline([audit()]).length).toBe(1);
  });
});
