import type { LineSeriesOption } from "echarts";
import { describe, expect, it } from "vitest";
import {
  ANNOTATION_SERIES_NAME,
  ANNOTATION_TEXT_MAX,
  GLOBAL_SCOPE,
  annotationOverlaySeries,
  isInstant,
  mergeAnnotations,
  withAnnotations,
} from "./annotations";
import { CHART_FALLBACK } from "./chart-theme";
import type { Annotation } from "./types";

/**
 * The option-builder is a pure function on purpose: EChart is mocked in every
 * page test (echarts.init reaches for a 2d canvas context jsdom does not
 * implement), so this file is the ONLY place the markLine/markArea shape is
 * actually asserted. Nothing here renders.
 */

function ann(over: Partial<Annotation> = {}): Annotation {
  return {
    id: "a-1",
    startAt: "2026-08-01T11:30:00Z",
    scope: "",
    text: "rolled the gateway",
    createdBy: "user:ada",
    createdAt: "2026-08-01T11:30:01Z",
    ...over,
  };
}

const START_MS = Date.parse("2026-08-01T11:30:00Z");
const END_MS = Date.parse("2026-08-01T11:45:00Z");

describe("isInstant", () => {
  it("is true when endAt is absent", () => {
    expect(isInstant(ann())).toBe(true);
  });

  it("is false when endAt is present", () => {
    expect(isInstant(ann({ endAt: "2026-08-01T11:45:00Z" }))).toBe(false);
  });
});

describe("annotationOverlaySeries", () => {
  it("returns null for an empty list — nothing to overlay", () => {
    expect(annotationOverlaySeries([], true)).toBeNull();
  });

  it("turns an INSTANT annotation into a markLine at its startAt", () => {
    const series = annotationOverlaySeries([ann()], true) as LineSeriesOption;
    const data = series.markLine?.data as { name: string; xAxis: number }[];
    expect(data).toHaveLength(1);
    expect(data[0].xAxis).toBe(START_MS);
    expect(data[0].name).toBe("rolled the gateway");
    expect(series.markArea).toBeUndefined();
  });

  it("turns a RANGED annotation into a markArea spanning [startAt, endAt]", () => {
    const series = annotationOverlaySeries([ann({ endAt: "2026-08-01T11:45:00Z" })], true) as LineSeriesOption;
    const data = series.markArea?.data as [{ name: string; xAxis: number }, { xAxis: number }][];
    expect(data).toHaveLength(1);
    expect(data[0][0].xAxis).toBe(START_MS);
    expect(data[0][1].xAxis).toBe(END_MS);
    expect(data[0][0].name).toBe("rolled the gateway");
    expect(series.markLine).toBeUndefined();
  });

  it("splits a mixed list into both marks on the SAME series", () => {
    const series = annotationOverlaySeries(
      [ann({ id: "i" }), ann({ id: "r", endAt: "2026-08-01T11:45:00Z", text: "drain" })],
      true,
    ) as LineSeriesOption;
    expect((series.markLine?.data as unknown[])).toHaveLength(1);
    expect((series.markArea?.data as unknown[])).toHaveLength(1);
  });

  it("carries no data of its own — it is a marker host, not a line", () => {
    const series = annotationOverlaySeries([ann()], true) as LineSeriesOption;
    expect(series.type).toBe("line");
    expect(series.data).toEqual([]);
    expect(series.name).toBe(ANNOTATION_SERIES_NAME);
  });

  it("shows the text on hover rather than at rest", () => {
    const series = annotationOverlaySeries([ann({ endAt: "2026-08-01T11:45:00Z" })], true) as LineSeriesOption;
    expect(series.markArea?.label?.show).toBe(false);
    expect(series.markArea?.emphasis?.label?.show).toBe(true);
    expect(series.markArea?.emphasis?.label?.formatter).toBe("{b}");
    const line = annotationOverlaySeries([ann()], true) as LineSeriesOption;
    expect(line.markLine?.label?.show).toBe(false);
    expect(line.markLine?.emphasis?.label?.show).toBe(true);
  });

  it("draws in the muted house colour, per theme", () => {
    const dark = annotationOverlaySeries([ann()], true) as LineSeriesOption;
    const light = annotationOverlaySeries([ann()], false) as LineSeriesOption;
    expect(dark.markLine?.lineStyle?.color).toBe(CHART_FALLBACK.dark.other);
    expect(light.markLine?.lineStyle?.color).toBe(CHART_FALLBACK.light.other);
  });

  it("drops an annotation whose startAt does not parse", () => {
    expect(annotationOverlaySeries([ann({ startAt: "not-a-time" })], true)).toBeNull();
  });

  it("treats an unparseable endAt as an instant rather than an infinite band", () => {
    const series = annotationOverlaySeries([ann({ endAt: "later" })], true) as LineSeriesOption;
    expect((series.markLine?.data as unknown[])).toHaveLength(1);
    expect(series.markArea).toBeUndefined();
  });
});

describe("withAnnotations", () => {
  const base = { series: [{ type: "line" as const, name: "tcp", data: [] }] };

  it("appends the overlay after the real series", () => {
    const out = withAnnotations(base, [ann()], true);
    const series = out.series as LineSeriesOption[];
    expect(series).toHaveLength(2);
    expect(series[0].name).toBe("tcp");
    expect(series[1].name).toBe(ANNOTATION_SERIES_NAME);
  });

  it("returns the option UNCHANGED when there is nothing to draw", () => {
    expect(withAnnotations(base, [], true)).toBe(base);
  });

  it("does not mutate the option it was given", () => {
    withAnnotations(base, [ann()], true);
    expect(base.series).toHaveLength(1);
  });

  it("copes with an option carrying a single series object rather than an array", () => {
    const out = withAnnotations({ series: { type: "line", name: "solo", data: [] } }, [ann()], true);
    expect((out.series as LineSeriesOption[]).map((s) => s.name)).toEqual(["solo", ANNOTATION_SERIES_NAME]);
  });

  it("copes with an option carrying no series at all", () => {
    const out = withAnnotations({ xAxis: { type: "time" } }, [ann()], true);
    expect((out.series as LineSeriesOption[])).toHaveLength(1);
  });
});

describe("mergeAnnotations", () => {
  it("folds several lists into one, oldest first", () => {
    const a = ann({ id: "a", startAt: "2026-08-01T12:00:00Z" });
    const b = ann({ id: "b", startAt: "2026-08-01T11:00:00Z" });
    expect(mergeAnnotations([a], [b]).map((x) => x.id)).toEqual(["b", "a"]);
  });

  it("de-duplicates by id — a global mark fetched twice renders once", () => {
    const a = ann({ id: "a" });
    expect(mergeAnnotations([a], [a, ann({ id: "b" })])).toHaveLength(2);
  });

  it("breaks a startAt tie on id so the order is total and stable", () => {
    const x = ann({ id: "x" });
    const y = ann({ id: "y" });
    expect(mergeAnnotations([y], [x]).map((a) => a.id)).toEqual(["x", "y"]);
  });

  it("is empty for no input", () => {
    expect(mergeAnnotations()).toEqual([]);
  });
});

describe("constants", () => {
  it("pins the global scope to the empty string the API defines", () => {
    expect(GLOBAL_SCOPE).toBe("");
  });

  it("pins the text bound the server enforces", () => {
    expect(ANNOTATION_TEXT_MAX).toBe(1024);
  });
});
