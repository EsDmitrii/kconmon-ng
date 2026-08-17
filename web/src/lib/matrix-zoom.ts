/**
 * matrix-zoom.ts — the arithmetic behind /matrix's zoom.
 *
 * The owner's report: at ten nodes the grid ran off the right of the screen,
 * the first column and the row labels clipped, and there was no way to shrink
 * it or to pan. Two things caused that and both are fixed here.
 *
 *   1. The COLUMN WIDTH was whatever a node name measured. A `<th>` holding
 *      "kconmon-prod-worker-03" made its whole column ~160px, so ten nodes were
 *      1.7 metres of table on a 1050px page. The column is a fixed width now
 *      (COLUMN_WIDTH), the name truncates inside it, and the full name lives in
 *      the header's tooltip and aria-label where it always did. Geometry that
 *      does not depend on the data is also geometry a pure function can predict,
 *      which is what makes `fitScale` exact rather than a guess.
 *
 *   2. There was no scale at all. There is now, in fixed steps, and the default
 *      is the largest step at which the whole grid is inside its box.
 *
 * The floor is deliberate. Below MIN_ZOOM a cell is smaller than the smallest
 * legible figure, so shrinking further would trade a grid you cannot fit for a
 * grid you cannot read; at the floor the container takes over and pans.
 */

/* ── natural geometry, in CSS pixels at scale 1 ─────────────────────────── */

/** One destination column. Fixed, not measured — see the header note above. */
export const COLUMN_WIDTH = 96;

/** A cell's height (the `h-12` the grid has always drawn). */
export const CELL_HEIGHT = 48;

/** The `p-0.5` padding either side of a cell, i.e. what a column costs on top of its width. */
export const CELL_GAP = 4;

/** The sticky source-name column. */
export const LABEL_WIDTH = 144;

/** How narrow that column may get before a row name stops naming anything.
 *  Scaling stops here while the cells keep shrinking. */
export const LABEL_MIN_WIDTH = 88;

/* ── the type scale, and the floor under it ─────────────────────────────── */

const FONT_HERO = 13;
const FONT_SUB = 10.5;
const FONT_LABEL = 11;

/** No text in the grid is ever drawn smaller than this. */
const MIN_FONT = 9;

/* ── the steps ──────────────────────────────────────────────────────────── */

/**
 * The stops the two buttons walk. Discrete rather than continuous because the
 * grid is DOM: a fixed set of scales means a fixed set of rendered sizes, and
 * the reader can get back to a scale they liked.
 */
export const ZOOM_STEPS = [0.4, 0.5, 0.6, 0.75, 0.9, 1, 1.25, 1.5] as const;

export type ZoomStep = (typeof ZOOM_STEPS)[number];

export const MIN_ZOOM: ZoomStep = ZOOM_STEPS[0];
export const MAX_ZOOM: ZoomStep = ZOOM_STEPS[ZOOM_STEPS.length - 1];

/**
 * usable is the last gate before arithmetic, and it exists because everything
 * this module computes is written into a CSS custom property: one NaN reaching
 * `--m-col-w` is a `width: NaNpx` the browser drops, i.e. a grid with no
 * columns, and it would arrive silently. The two callers below can only be
 * handed a step today — but they are exported, and a bound that costs a
 * comparison is cheaper than the class of bug it forecloses. A fleet count is
 * held to the same rule: a negative or fractional node count is not a fleet.
 */
function usableScale(scale: number): number {
  if (!Number.isFinite(scale)) return 1;
  return Math.min(Math.max(scale, MIN_ZOOM), MAX_ZOOM);
}

function usableCount(nodes: number): number {
  return Number.isFinite(nodes) && nodes > 0 ? Math.floor(nodes) : 0;
}

/** gridWidth is exactly how wide the table draws at `scale`. */
export function gridWidth(nodes: number, scale: number): number {
  const s = usableScale(scale);
  const label = Math.max(Math.round(LABEL_WIDTH * s), LABEL_MIN_WIDTH);
  return label + usableCount(nodes) * (Math.round(COLUMN_WIDTH * s) + CELL_GAP);
}

/** gridHeight is exactly how tall the table draws at `scale`: the header row plus one row per node. */
export function gridHeight(nodes: number, scale: number): number {
  const s = usableScale(scale);
  const row = Math.round(CELL_HEIGHT * s) + CELL_GAP;
  // The column-header row is the same height as a cell row in the rendered table.
  return row * (usableCount(nodes) + 1);
}

/**
 * fitScale is the largest offered step at which the whole grid is inside
 * `available` — the default whenever the grid overflows.
 *
 * BOTH axes. It used to solve for width only, so "Fit to view" on a ten-node grid in a short
 * viewport left the last row four-fifths below the fold — a control whose whole promise is that
 * everything is on screen. `availableHeight` is optional: a caller that cannot measure it (or does
 * not care) gets the old width-only answer rather than a wrong one.
 *
 * It never returns a step above 1: fitting is about getting a big grid IN, not
 * about inflating a small one to fill the page. When nothing fits it returns the
 * floor and the container pans from there, which is the honest answer for a
 * fifty-node fleet on a laptop.
 */
export function fitScale(nodes: number, available: number, availableHeight?: number): number {
  if (!Number.isFinite(available) || available <= 0) return 1;
  const boundHeight = Number.isFinite(availableHeight) && (availableHeight ?? 0) > 0;
  for (const step of [...ZOOM_STEPS].reverse()) {
    if (step > 1) continue;
    if (gridWidth(nodes, step) > available) continue;
    if (boundHeight && gridHeight(nodes, step) > (availableHeight ?? 0)) continue;
    return step;
  }
  return MIN_ZOOM;
}

/** zoomStep moves one stop in `direction`, snapping a between-stops scale onto the grid of stops. */
export function zoomStep(scale: number, direction: 1 | -1): number {
  /* Not `scale` directly: every comparison against a NaN is false, so a NaN
     walked straight past both ends of the scale to whichever clamp the
     direction happened to name. */
  const s = usableScale(scale);
  if (direction === 1) return ZOOM_STEPS.find((step) => step > s + 1e-6) ?? MAX_ZOOM;
  return [...ZOOM_STEPS].reverse().find((step) => step < s - 1e-6) ?? MIN_ZOOM;
}

/**
 * cellDensity is what a cell can honestly SHOW at a given scale.
 *
 * Zoomed out far enough the grid stops being a table of figures and becomes what
 * it is better at anyway — a heat map of tiers, with every number still one hover
 * away in the tooltip. That is also what keeps a fifty-node grid affordable: a
 * `tile` cell renders a quarter of the elements a `full` one does.
 */
export type CellDensity = "full" | "compact" | "tile";

export function cellDensity(scale: number): CellDensity {
  const s = usableScale(scale);
  if (s >= 0.75) return "full";
  if (s >= 0.5) return "compact";
  return "tile";
}

export interface GridMetrics {
  columnWidth: number;
  cellHeight: number;
  labelWidth: number;
  fontHero: number;
  fontSub: number;
  fontLabel: number;
  density: CellDensity;
  /** The custom properties the grid's own class names read. ONE object for the
   *  whole table rather than a style object per cell — at fifty nodes that is
   *  the difference between one allocation and two and a half thousand. */
  vars: Record<string, string>;
}

/** Fonts shrink with the boxes but stop at MIN_FONT; the boxes keep going. */
function fontAt(base: number, scale: number): number {
  return Math.max(Math.round(base * scale * 10) / 10, MIN_FONT);
}

export function gridMetrics(scale: number): GridMetrics {
  /* Every number below ends up in a CSS custom property, so this is the last
     place a NaN can be stopped before it becomes `width: NaNpx` — which a
     browser drops, silently, leaving a grid with no columns. */
  const s = usableScale(scale);
  const columnWidth = Math.round(COLUMN_WIDTH * s);
  const cellHeight = Math.round(CELL_HEIGHT * s);
  const labelWidth = Math.max(Math.round(LABEL_WIDTH * s), LABEL_MIN_WIDTH);
  const fontHero = fontAt(FONT_HERO, s);
  const fontSub = fontAt(FONT_SUB, s);
  const fontLabel = fontAt(FONT_LABEL, s);
  return {
    columnWidth,
    cellHeight,
    labelWidth,
    fontHero,
    fontSub,
    fontLabel,
    density: cellDensity(s),
    vars: {
      "--m-col-w": `${columnWidth}px`,
      "--m-cell-h": `${cellHeight}px`,
      "--m-label-w": `${labelWidth}px`,
      "--m-font-hero": `${fontHero}px`,
      "--m-font-sub": `${fontSub}px`,
      "--m-font-label": `${fontLabel}px`,
    },
  };
}
