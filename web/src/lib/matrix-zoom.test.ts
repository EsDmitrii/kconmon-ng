import { describe, expect, it } from "vitest";
import {
  CELL_GAP,
  elideForHeaders,
  COLUMN_WIDTH,
  heightBudget,
  LABEL_MIN_WIDTH,
  LABEL_WIDTH,
  MAX_ZOOM,
  MIN_ZOOM,
  ZOOM_STEPS,
  cellDensity,
  fitScale,
  gridHeight,
  gridMetrics,
  gridWidth,
  zoomStep,
  type ZoomStep,
} from "./matrix-zoom";

/**
 * The owner's report: at ten nodes the grid runs off the screen and there is no
 * way to shrink or pan it. The arithmetic here is what "fit" means — the largest
 * step at which the WHOLE grid is inside the box — and what a step costs in
 * legibility once it gets small.
 *
 * Geometry is deterministic on purpose: the column is a fixed width rather than
 * whatever a node name measures, which is what let one long name push the whole
 * table off screen in the first place.
 */

describe("gridWidth is the exact width the table will occupy", () => {
  it("counts the sticky label column plus one column per node", () => {
    expect(gridWidth(10, 1)).toBe(LABEL_WIDTH + 10 * (COLUMN_WIDTH + CELL_GAP));
  });

  it("shrinks with the scale", () => {
    expect(gridWidth(10, 0.5)).toBeLessThan(gridWidth(10, 1));
  });

  it("stops shrinking the LABEL column at its floor, so row names stay readable", () => {
    // At 0.4 the label column would be 58px, which names nothing.
    expect(gridWidth(1, MIN_ZOOM)).toBe(LABEL_MIN_WIDTH + Math.round(COLUMN_WIDTH * MIN_ZOOM) + CELL_GAP);
  });

  it("grows with the fleet, which is the whole problem being solved", () => {
    expect(gridWidth(50, 1)).toBeGreaterThan(gridWidth(10, 1));
  });
});

describe("fitScale is the default when the grid overflows", () => {
  it("picks a step at which ten nodes fit a 1050px column", () => {
    const scale = fitScale(10, 1050);
    expect(ZOOM_STEPS).toContain(scale);
    expect(gridWidth(10, scale)).toBeLessThanOrEqual(1050);
  });

  it("never zooms IN to fill the box — a three-node grid stays its own size", () => {
    expect(fitScale(3, 1600)).toBe(1);
  });

  it("stops at the floor and hands over to panning rather than shrinking to nothing", () => {
    // Fifty nodes in a laptop-width column cannot fit at any legible scale.
    const scale = fitScale(50, 1050);
    expect(scale).toBe(MIN_ZOOM);
    expect(gridWidth(50, scale)).toBeGreaterThan(1050);
  });

  it("answers 1 while the container has not been measured, instead of guessing", () => {
    // First render, jsdom, a hidden tab: clientWidth is 0 and any scale derived
    // from it would be a fabrication.
    expect(fitScale(20, 0)).toBe(1);
    expect(fitScale(20, Number.NaN)).toBe(1);
  });

  it("always returns one of the offered steps, at every width", () => {
    for (const nodes of [1, 5, 10, 25, 50, 120]) {
      for (const width of [0, 320, 700, 1050, 1440, 2560, 6000]) {
        expect(ZOOM_STEPS).toContain(fitScale(nodes, width));
      }
    }
  });

  it("is monotonic: a wider box never fits at a smaller scale", () => {
    let previous = 0;
    for (const width of [320, 700, 1050, 1440, 2560, 6000]) {
      const scale = fitScale(20, width);
      expect(scale).toBeGreaterThanOrEqual(previous);
      previous = scale;
    }
  });
});

describe("zoomStep walks the offered steps and stops at both ends", () => {
  it("steps in and out one stop at a time", () => {
    expect(zoomStep(1, 1)).toBe(1.25);
    expect(zoomStep(1, -1)).toBe(0.9);
  });

  it("clamps at the ceiling and at the floor rather than wrapping", () => {
    expect(zoomStep(MAX_ZOOM, 1)).toBe(MAX_ZOOM);
    expect(zoomStep(MIN_ZOOM, -1)).toBe(MIN_ZOOM);
  });

  it("snaps a scale that is between two stops onto the grid of stops", () => {
    // 0.83 is what an arbitrary fit could produce; stepping out from it must
    // land on a real stop below it, never on 0.83 - something.
    expect(ZOOM_STEPS).toContain(zoomStep(0.83, -1));
    expect(zoomStep(0.83, -1)).toBeLessThan(0.83);
    expect(zoomStep(0.83, 1)).toBeGreaterThan(0.83);
  });
});

describe("cellDensity is what a cell can honestly show at a given size", () => {
  it("shows the value and its second line at full size", () => {
    expect(cellDensity(1)).toBe("full");
  });

  it("drops the second line once the cell is too short for two", () => {
    expect(cellDensity(0.6)).toBe("compact");
  });

  it("becomes a coloured tile below the size any figure would fit in", () => {
    // 26px wide: "50.0%" does not go in there at any font size worth reading.
    expect(cellDensity(MIN_ZOOM)).toBe("tile");
  });

  it("only ever loosens as the scale grows", () => {
    const rank = { tile: 0, compact: 1, full: 2 };
    let previous = -1;
    for (const step of ZOOM_STEPS) {
      const value = rank[cellDensity(step)];
      expect(value).toBeGreaterThanOrEqual(previous);
      previous = value;
    }
  });
});

describe("gridMetrics keeps text legible while the boxes shrink", () => {
  it("scales the boxes with the zoom", () => {
    expect(gridMetrics(0.5).cellHeight).toBe(24);
    expect(gridMetrics(1).cellHeight).toBe(48);
  });

  it("floors every font rather than scaling it to nothing", () => {
    const small = gridMetrics(MIN_ZOOM);
    expect(small.fontLabel).toBeGreaterThanOrEqual(9);
    expect(small.fontHero).toBeGreaterThanOrEqual(9);
  });

  it("does not INFLATE the fonts past their design size when zoomed in", () => {
    // Zooming in is for hit area and for the reader's eyes, but the type scale
    // is the design system's — 1.5× of an 13px figure is not a heading.
    expect(gridMetrics(MAX_ZOOM).fontHero).toBeLessThanOrEqual(gridMetrics(1).fontHero * MAX_ZOOM);
  });

  it("hands the CSS custom properties the grid actually reads", () => {
    const vars = gridMetrics(1).vars;
    expect(vars["--m-col-w"]).toBe("96px");
    expect(vars["--m-cell-h"]).toBe("48px");
    expect(vars["--m-label-w"]).toBe("144px");
  });
});

/* ── inputs that are not geometry ───────────────────────────────────────────
 *
 * QA scope 4. Every number in this module is written into a CSS custom property
 * the grid's class names read (`w-[var(--m-col-w)]`), and a browser DROPS
 * `width: NaNpx` — so one NaN through here is a grid with no columns and no
 * error anywhere. The page cannot produce one today (a manual scale is always a
 * step, and fitScale refuses an unmeasured box), which is exactly why the bound
 * belongs in the pure module rather than in the caller that happens to be safe.
 */
describe("the geometry refuses an input that is not geometry", () => {
  const badScales = [NaN, Infinity, -Infinity, -1, 0];
  const badCounts = [NaN, Infinity, -5, -0.5];

  it.each(badScales)("gives gridMetrics(%s) a real, drawable size", (scale) => {
    const m = gridMetrics(scale);
    for (const [name, value] of Object.entries(m.vars)) {
      expect(value, name).toMatch(/^\d+(\.\d+)?px$/);
    }
    expect(m.columnWidth).toBeGreaterThan(0);
    expect(m.cellHeight).toBeGreaterThan(0);
    expect(m.labelWidth).toBeGreaterThanOrEqual(LABEL_MIN_WIDTH);
  });

  it.each(badScales)("keeps gridWidth(10, %s) a finite width", (scale) => {
    expect(Number.isFinite(gridWidth(10, scale))).toBe(true);
    expect(gridWidth(10, scale)).toBeGreaterThan(0);
  });

  it.each(badCounts)("treats a fleet of %s as no fleet at all, not as a negative table", (nodes) => {
    expect(gridWidth(nodes, 1)).toBe(gridWidth(0, 1));
  });

  /* Every comparison against a NaN is false, so an unguarded NaN walked past
     both ends of the scale to whichever clamp the direction happened to name —
     one wheel notch and the grid was at 40% or 150%. */
  it.each(badScales)("steps from %s onto a real stop rather than to a clamp", (scale) => {
    expect(ZOOM_STEPS).toContain(zoomStep(scale, 1) as ZoomStep);
    expect(ZOOM_STEPS).toContain(zoomStep(scale, -1) as ZoomStep);
    expect(zoomStep(scale, -1)).toBeLessThan(zoomStep(scale, 1));
  });

  it.each(badScales)("still answers cellDensity(%s) with a density the cell can render", (scale) => {
    expect(["full", "compact", "tile"]).toContain(cellDensity(scale));
  });

  it("holds a scale above the ceiling to the ceiling instead of drawing past it", () => {
    expect(gridMetrics(99).columnWidth).toBe(gridMetrics(MAX_ZOOM).columnWidth);
    expect(cellDensity(99)).toBe("full");
  });

  /* 100 nodes at the floor is 10 000 cells — the shape the cap and the tile
     density exist for. What is pinned here is only that the arithmetic stays
     arithmetic at that size. */
  it("still predicts an exact width for a fleet far larger than any offered step fits", () => {
    expect(fitScale(100, 1200)).toBe(MIN_ZOOM);
    expect(gridWidth(100, MIN_ZOOM)).toBe(88 + 100 * (38 + 4));
  });
});

/* ── QA round 5b: "Fit to view" has to fit ────────────────────────────────── */

describe("fitScale bounds BOTH axes", () => {
  it("shrinks until the grid fits vertically too", () => {
    const nodes = 10;
    // Wide enough for anything, short enough that a full-scale grid overflows downwards.
    const wide = gridWidth(nodes, 1) + 500;
    const short = gridHeight(nodes, 1) - 200;

    const widthOnly = fitScale(nodes, wide);
    const both = fitScale(nodes, wide, short);
    expect(both).toBeLessThan(widthOnly);
    expect(gridHeight(nodes, both)).toBeLessThanOrEqual(short);
  });

  it("is unchanged when the height is not measured", () => {
    const nodes = 8;
    const wide = gridWidth(nodes, 1) + 100;
    expect(fitScale(nodes, wide, 0)).toBe(fitScale(nodes, wide));
    expect(fitScale(nodes, wide, Number.NaN)).toBe(fitScale(nodes, wide));
  });

  it("returns the floor when nothing fits either way", () => {
    expect(fitScale(60, 300, 200)).toBe(MIN_ZOOM);
  });
});

/*
 * The grid opened at half size on a screen with room to spare.
 *
 * The viewport is `max-h-[...] min-h-64`, so its clientHeight is the height its CONTENT made. A
 * fresh render measured the 256px min, fitScale decided a seven-node grid did not fit, dropped to
 * 50%, and the smaller grid then kept the box at 256px -- a loop with no way out, on every fleet.
 */
describe("heightBudget", () => {
  it("takes the max-height over a content-driven clientHeight", () => {
    // 256 = the min-h-64 a fresh viewport reports; 640 = the resolved max-h.
    expect(heightBudget(256, 640)).toBe(640);
  });

  it("keeps clientHeight when the box is already taller than its max", () => {
    expect(heightBudget(800, 640)).toBe(800);
  });

  it("falls back to clientHeight where there is no usable max-height", () => {
    // getComputedStyle().maxHeight is "" in jsdom and "none" in a browser without one; both parse
    // to NaN, and a NaN budget must not become a NaN scale.
    expect(heightBudget(256, Number.NaN)).toBe(256);
    expect(heightBudget(256, 0)).toBe(256);
  });

  it("is zero when nothing has been measured yet, which fitScale reads as do-not-guess", () => {
    expect(heightBudget(0, Number.NaN)).toBe(0);
  });
});

/*
 * A seven-node fleet on a normal screen belongs at 100%.
 *
 * This is the arithmetic the loop above got wrong: with the real budget the answer was always 1.
 */
describe("fitScale with a real viewport", () => {
  it("keeps a seven-node grid at full size when the box has room", () => {
    expect(fitScale(7, 1000, heightBudget(256, 640))).toBe(1);
  });

  it("still shrinks when the box genuinely cannot hold the grid", () => {
    expect(fitScale(7, 1000, heightBudget(256, 260))).toBeLessThan(1);
  });
});

/*
 * Zooming in must give the names back.
 *
 * The shared-prefix elision is right while a column is narrower than the names and wrong the moment
 * it is not: at 125% a 180px label column holds "adm-kuber-01" with room over, and dropping the
 * prefix there left the axis reading "…01" -- a number where a name belongs.
 */
describe("elideForHeaders", () => {
  const fleet = ["adm-kuber-01", "adm-kuber-02", "adm-kuber-07"];

  it("keeps the whole name when the box holds it", () => {
    expect(elideForHeaders(fleet, "adm-kuber-", 180, 11)).toBe("");
  });

  it("elides when the box does not", () => {
    // A column at 50%: gridMetrics gives 48px and the 9px type floor. Nothing of that name fits.
    expect(elideForHeaders(fleet, "adm-kuber-", 48, 9)).toBe("adm-kuber-");
  });

  it("gives the name back as the grid grows", () => {
    // The same axis at 125%: 120px column, 14px type -- and the whole name is drawn.
    expect(elideForHeaders(fleet, "adm-kuber-", 120, 14)).toBe("");
  });

  it("decides on the LONGEST name, so one axis reads one way", () => {
    const mixed = ["adm-kuber-01", "adm-kuber-worker-frankfurt-12"];
    expect(elideForHeaders(mixed, "adm-kuber-", 180, 11)).toBe("adm-kuber-");
  });

  it("has nothing to elide without a shared prefix", () => {
    expect(elideForHeaders(fleet, "", 40, 11)).toBe("");
  });
});
