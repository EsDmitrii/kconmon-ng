/**
 * zone-layout packs the topology map's zone boxes into rows.
 *
 * One horizontal strip was fine at 2-3 zones and hopeless at 5+: fitView could
 * only fit a 6-zone strip by shrinking every label while four fifths of the
 * canvas above and below stayed empty. Pure geometry, no React Flow — the map
 * feeds sizes in and pins the results in unit tests.
 */

export interface ZoneSize {
  readonly width: number;
  readonly height: number;
}

export interface ZonePoint {
  x: number;
  y: number;
}

/** Gap between zone boxes, columns and rows alike — the 80 the one-row layout
 *  always put between neighbours. */
export const ZONE_GAP = 80;

/** The pane's own shape (≈1440×680 at a 1440×900 window). fitView rescales the
 *  drawing to the pane, so legibility is decided by the drawing's ASPECT, not
 *  its absolute size — the closer to this, the larger everything renders. */
const TARGET_ASPECT = 2;

function layout(
  sizes: readonly ZoneSize[],
  cols: number,
): { points: ZonePoint[]; width: number; height: number } {
  const points: ZonePoint[] = [];
  let x = 0;
  let y = 0;
  let rowHeight = 0;
  let width = 0;
  sizes.forEach((s, i) => {
    if (i > 0 && i % cols === 0) {
      x = 0;
      y += rowHeight + ZONE_GAP;
      rowHeight = 0;
    }
    points.push({ x, y });
    width = Math.max(width, x + s.width);
    x += s.width + ZONE_GAP;
    // Rows are TOP-ALIGNED: a 1-node zone next to a tall one keeps its own
    // height (heights stay the caller's), and only the row advance uses the max.
    rowHeight = Math.max(rowHeight, s.height);
  });
  return { points, width, height: y + rowHeight };
}

/** How far a drawing's aspect sits from the pane's, symmetric in log space so
 *  "twice too wide" and "twice too tall" miss by the same amount. */
const misfit = (b: { width: number; height: number }) =>
  Math.abs(Math.log(b.width / b.height / TARGET_ASPECT));

/**
 * packZones positions one box per zone, in the caller's order.
 *
 * 1-2 zones keep the exact one-row layout the map always had. From 3 up the
 * column count is 2 or 3 — derived from the zones' real widths, not the count
 * alone: whichever packing's bounding box sits closer to the pane's aspect,
 * so wide zones (many nodes) wrap sooner than narrow ones.
 */
export function packZones(sizes: readonly ZoneSize[]): ZonePoint[] {
  if (sizes.length <= 2) return layout(sizes, Math.max(1, sizes.length)).points;
  const two = layout(sizes, 2);
  const three = layout(sizes, 3);
  return misfit(three) <= misfit(two) ? three.points : two.points;
}
