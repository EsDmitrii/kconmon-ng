import type { LineSeriesOption } from "echarts";
import { describe, expect, it } from "vitest";
import {
  ANNOTATION_SERIES_NAME,
  ANNOTATION_TEXT_MAX,
  GLOBAL_SCOPE,
  MAINTENANCE_REASON_MAX,
  MAINTENANCE_SERIES_NAME,
  annotationOverlaySeries,
  isInstant,
  maintenanceOverlaySeries,
  mergeAnnotations,
  outsideWindowNote,
  mergeMaintenanceWindows,
  withAnnotations,
  withMaintenance,
} from "./annotations";
import { CHART_FALLBACK } from "./chart-theme";
import type { Annotation, MaintenanceWindow } from "./types";

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

  /* QA round 4, finding #7. Without an explicit series colour the legend key
     takes the next entry from ECharts' palette, so it was drawn in a blue
     that appears nowhere on the chart. */
  it("gives the legend swatch the marker's OWN colour, not the next palette entry", () => {
    const dark = annotationOverlaySeries([ann()], true) as LineSeriesOption;
    const light = annotationOverlaySeries([ann()], false) as LineSeriesOption;
    expect((dark.itemStyle as { color?: string }).color).toBe(CHART_FALLBACK.dark.other);
    expect((light.itemStyle as { color?: string }).color).toBe(CHART_FALLBACK.light.other);
    // It is never a series ramp colour — that is the whole mismatch.
    expect(CHART_FALLBACK.dark.series).not.toContain((dark.itemStyle as { color?: string }).color);
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

  it("pins the reason bound the maintenance endpoint enforces", () => {
    expect(MAINTENANCE_REASON_MAX).toBe(512);
  });
});

/* ── M6 Task 9: the maintenance overlay, annotations' sibling ─────────────── */

function win(over: Partial<MaintenanceWindow> = {}): MaintenanceWindow {
  return {
    id: "m-1",
    scope: "",
    startAt: "2026-08-01T11:30:00Z",
    endAt: "2026-08-01T11:45:00Z",
    reason: "switch upgrade",
    createdBy: "user:ada",
    createdAt: "2026-08-01T10:00:00Z",
    ...over,
  };
}

describe("maintenanceOverlaySeries", () => {
  it("returns null for an empty list — nothing to overlay", () => {
    expect(maintenanceOverlaySeries([], true)).toBeNull();
  });

  it("turns each window into a markArea spanning [startAt, endAt]", () => {
    const series = maintenanceOverlaySeries([win()], true) as LineSeriesOption;
    const data = series.markArea?.data as [{ name: string; xAxis: number }, { xAxis: number }][];
    expect(data).toHaveLength(1);
    expect(data[0][0].xAxis).toBe(START_MS);
    expect(data[0][1].xAxis).toBe(END_MS);
  });

  it("has NO markLine at all — a window is always a span, never an instant", () => {
    const series = maintenanceOverlaySeries([win()], true) as LineSeriesOption;
    expect(series.markLine).toBeUndefined();
    expect(series.type).toBe("line");
    expect(series.data).toEqual([]);
    expect(series.name).toBe(MAINTENANCE_SERIES_NAME);
  });

  it("carries the reason as the hover label rather than showing it at rest", () => {
    const series = maintenanceOverlaySeries([win({ reason: "core switch reboot" })], true) as LineSeriesOption;
    const data = series.markArea?.data as [{ name: string }, unknown][];
    expect(data[0][0].name).toBe("core switch reboot");
    expect(series.markArea?.label?.show).toBe(false);
    expect(series.markArea?.emphasis?.label?.show).toBe(true);
    expect(series.markArea?.emphasis?.label?.formatter).toBe("{b}");
  });

  /* THE differentiating property. Both overlays are muted bands drawn from the
     same axis colour, so the thing that tells them apart on a busy chart is the
     DASHED OUTLINE and the fainter fill a declared window carries — an
     annotation's band has neither. Asserted on both sides so a future edit that
     makes them identical fails here rather than in an operator's eyes. */
  it("is visually DISTINCT from an annotation band: dashed outline, fainter fill", () => {
    const maintenance = maintenanceOverlaySeries([win()], true) as LineSeriesOption;
    const annotation = annotationOverlaySeries([ann({ endAt: "2026-08-01T11:45:00Z" })], true) as LineSeriesOption;
    const mStyle = maintenance.markArea?.itemStyle as { borderType?: string; borderWidth?: number; opacity?: number };
    const aStyle = annotation.markArea?.itemStyle as { borderType?: string; opacity?: number };
    expect(mStyle.borderType).toBe("dashed");
    expect(mStyle.borderWidth).toBeGreaterThan(0);
    expect(aStyle.borderType).toBeUndefined();
    expect(mStyle.opacity).toBeLessThan(aStyle.opacity as number);
    expect(maintenance.name).not.toBe(annotation.name);
  });

  it("draws in the muted house colour, per theme", () => {
    const dark = maintenanceOverlaySeries([win()], true) as LineSeriesOption;
    const light = maintenanceOverlaySeries([win()], false) as LineSeriesOption;
    expect((dark.markArea?.itemStyle as { color?: string }).color).toBe(CHART_FALLBACK.dark.axis);
    expect((light.markArea?.itemStyle as { color?: string }).color).toBe(CHART_FALLBACK.light.axis);
  });

  /* QA round 4, finding #7 — the one the report caught: the "Maintenance"
     legend key was BLUE while the bands it switches are the axis grey. */
  it("gives the legend swatch the BAND's colour, so the key and the band agree", () => {
    const dark = maintenanceOverlaySeries([win()], true) as LineSeriesOption;
    const light = maintenanceOverlaySeries([win()], false) as LineSeriesOption;
    const swatch = (s: LineSeriesOption) => (s.itemStyle as { color?: string }).color;
    expect(swatch(dark)).toBe((dark.markArea?.itemStyle as { color?: string }).color);
    expect(swatch(light)).toBe((light.markArea?.itemStyle as { color?: string }).color);
    expect(CHART_FALLBACK.dark.series).not.toContain(swatch(dark));
  });

  it("SKIPS a window with an unparseable edge rather than drawing a band with a NaN bound", () => {
    expect(maintenanceOverlaySeries([win({ endAt: "later" })], true)).toBeNull();
    expect(maintenanceOverlaySeries([win({ startAt: "not-a-time" })], true)).toBeNull();
  });
});

describe("withMaintenance", () => {
  const base = { series: [{ type: "line" as const, name: "tcp", data: [] }] };

  it("appends the overlay after the real series", () => {
    const out = withMaintenance(base, [win()], true);
    const series = out.series as LineSeriesOption[];
    expect(series.map((s) => s.name)).toEqual(["tcp", MAINTENANCE_SERIES_NAME]);
  });

  it("returns the option UNCHANGED when there is nothing to draw", () => {
    expect(withMaintenance(base, [], true)).toBe(base);
  });

  it("does not mutate the option it was given", () => {
    withMaintenance(base, [win()], true);
    expect(base.series).toHaveLength(1);
  });

  it("composes with withAnnotations — both overlays land on one option", () => {
    const out = withMaintenance(withAnnotations(base, [ann()], true), [win()], true);
    expect((out.series as LineSeriesOption[]).map((s) => s.name)).toEqual([
      "tcp",
      ANNOTATION_SERIES_NAME,
      MAINTENANCE_SERIES_NAME,
    ]);
  });
});

describe("mergeMaintenanceWindows", () => {
  it("folds the scoped and global legs into one, oldest first", () => {
    const a = win({ id: "a", startAt: "2026-08-01T12:00:00Z" });
    const b = win({ id: "b", startAt: "2026-08-01T11:00:00Z" });
    expect(mergeMaintenanceWindows([a], [b]).map((x) => x.id)).toEqual(["b", "a"]);
  });

  it("de-duplicates by id — a global window fetched twice renders once", () => {
    const a = win({ id: "a" });
    expect(mergeMaintenanceWindows([a], [a, win({ id: "b" })])).toHaveLength(2);
  });

  it("breaks a startAt tie on id so the order is total and stable", () => {
    expect(mergeMaintenanceWindows([win({ id: "y" })], [win({ id: "x" })]).map((w) => w.id)).toEqual(["x", "y"]);
  });

  it("is empty for no input", () => {
    expect(mergeMaintenanceWindows()).toEqual([]);
  });
});

/* ── QA round 3, finding #8: the silent out-of-window create ─────────────── */

describe("outsideWindowNote", () => {
  const frozen = { from: new Date("2026-08-08T00:00:00Z"), to: new Date("2026-08-08T01:00:00Z") };
  const at = (iso: string) => new Date(iso);

  it("says nothing at all without a frozen window — a live list re-fetches and the row simply appears", () => {
    expect(outsideWindowNote(at("2030-01-01T00:00:00Z"), null, undefined)).toBeNull();
  });

  it("says nothing for an instant inside the window, including both edges", () => {
    expect(outsideWindowNote(at("2026-08-08T00:30:00Z"), null, frozen)).toBeNull();
    expect(outsideWindowNote(frozen.from, null, frozen)).toBeNull();
    expect(outsideWindowNote(frozen.to, null, frozen)).toBeNull();
  });

  it("names the window's end for an instant outside it, in both directions", () => {
    const ends = frozen.to.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
    expect(outsideWindowNote(at("2026-08-08T02:00:00Z"), null, frozen)).toBe(
      `Created — outside this window (which ends ${ends}); press Investigate to reframe.`,
    );
    expect(outsideWindowNote(at("2026-08-07T23:00:00Z"), null, frozen)).toContain("outside this window");
  });

  it("only needs a SPAN to overlap — one that starts before and ends inside is visible", () => {
    expect(outsideWindowNote(at("2026-08-07T23:00:00Z"), at("2026-08-08T00:10:00Z"), frozen)).toBeNull();
    expect(outsideWindowNote(at("2026-08-08T00:50:00Z"), at("2026-08-08T03:00:00Z"), frozen)).toBeNull();
    // A span that straddles the whole window covers it, so it is visible too.
    expect(outsideWindowNote(at("2026-08-07T00:00:00Z"), at("2026-08-09T00:00:00Z"), frozen)).toBeNull();
  });

  it("notes a span entirely on either side", () => {
    expect(outsideWindowNote(at("2026-08-09T00:00:00Z"), at("2026-08-09T01:00:00Z"), frozen)).toContain("outside");
    expect(outsideWindowNote(at("2026-08-07T00:00:00Z"), at("2026-08-07T01:00:00Z"), frozen)).toContain("outside");
  });

  it("stays silent rather than guessing when an instant will not parse", () => {
    expect(outsideWindowNote(new Date("nope"), null, frozen)).toBeNull();
  });
});
