import { describe, expect, it } from "vitest";
import { LANDSCAPE_PANE_ASPECT, PORTRAIT_PANE_ASPECT, packZones } from "./zone-layout";

/* Sizes here mirror what pages/topology.tsx feeds in: a 1-node zone is 300×128,
 * a 2-node zone 580×128, a 4-node zone 580×192, a 16-node zone 1140×320. */
const z = (width: number, height: number) => ({ width, height });

describe("packZones — one row while there is nothing to wrap", () => {
  it("lays 2 zones in the exact one-row shape the map always had", () => {
    // x advances by width + 80, y stays 0 — byte-for-byte the legacy layout.
    expect(packZones([z(300, 128), z(580, 128)])).toEqual([
      { x: 0, y: 0 },
      { x: 380, y: 0 },
    ]);
  });

  it("puts a single zone at the origin", () => {
    expect(packZones([z(300, 128)])).toEqual([{ x: 0, y: 0 }]);
  });

  it("answers an empty fleet with an empty layout", () => {
    expect(packZones([])).toEqual([]);
  });
});

describe("packZones — rows once a strip would overflow", () => {
  it("packs 6 small zones as two rows of three", () => {
    /* One row of six was 2200×128 — a strip fitView could only fit by shrinking
       everything; 3 per row is nearly the pane's own 2:1. */
    expect(packZones(Array.from({ length: 6 }, () => z(300, 128)))).toEqual([
      { x: 0, y: 0 },
      { x: 380, y: 0 },
      { x: 760, y: 0 },
      { x: 0, y: 208 },
      { x: 380, y: 208 },
      { x: 760, y: 208 },
    ]);
  });

  it("drops to two per row when the zones themselves are wide", () => {
    /* Six 16-node zones at 3 per row are a 3580×720 drawing (≈5:1); at 2 per
       row it is 2360×1120 (≈2:1) — the column count follows the widths, not
       just the count. */
    expect(packZones(Array.from({ length: 6 }, () => z(1140, 320)))).toEqual([
      { x: 0, y: 0 },
      { x: 1220, y: 0 },
      { x: 0, y: 400 },
      { x: 1220, y: 400 },
      { x: 0, y: 800 },
      { x: 1220, y: 800 },
    ]);
  });

  it("top-aligns a tiny zone beside a tall one and starts the next row under the TALLEST", () => {
    /* The external-agent case: a 1-node zone shares a row with a 4-node zone.
       The tiny box keeps its own height (heights are the caller's; this
       function never stretches one to the row), and the next row clears the
       tall neighbour — 192 + 80, not 128 + 80. */
    const points = packZones([z(580, 192), z(300, 128), z(300, 128), z(300, 128), z(300, 128)]);
    expect(points).toEqual([
      { x: 0, y: 0 },
      { x: 660, y: 0 },
      { x: 0, y: 272 },
      { x: 380, y: 272 },
      { x: 0, y: 480 },
    ]);
  });
});

describe("packZones — a portrait pane stacks", () => {
  /* On a 390×844 phone the pane is about 366×620. Packed two across for a 2:1 desktop pane, three
     zones fitted at scale 0.26 with node names 5px tall and most of the canvas empty. */
  it("stacks three zones in one column for a phone's pane", () => {
    expect(packZones([z(580, 192), z(580, 192), z(300, 128)], PORTRAIT_PANE_ASPECT)).toEqual([
      { x: 0, y: 0 },
      { x: 0, y: 272 },
      { x: 0, y: 544 },
    ]);
  });

  it("stacks two zones rather than laying them side by side", () => {
    expect(packZones([z(300, 128), z(580, 128)], PORTRAIT_PANE_ASPECT)).toEqual([
      { x: 0, y: 0 },
      { x: 0, y: 208 },
    ]);
  });

  it("keeps the desktop packing when no aspect is given", () => {
    const sizes = Array.from({ length: 6 }, () => z(300, 128));
    expect(packZones(sizes)).toEqual(packZones(sizes, LANDSCAPE_PANE_ASPECT));
    expect(packZones([z(300, 128), z(580, 128)], LANDSCAPE_PANE_ASPECT)).toEqual([
      { x: 0, y: 0 },
      { x: 380, y: 0 },
    ]);
  });
});
