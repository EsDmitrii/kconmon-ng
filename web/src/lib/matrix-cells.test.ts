import { describe, expect, it } from "vitest";
import {
  cellSummary,
  cellTier,
  isMeasured,
  isProblemCell,
  severityRatio,
} from "./matrix-cells";
import type { MatrixCell } from "./types";

/** The shared cell reading. */

function cell(over: Partial<MatrixCell> = {}): MatrixCell {
  return { source: "a", destination: "b", failRatio: null, ...over };
}

describe("isMeasured", () => {
  it("is false only when all three vectors are silent", () => {
    expect(isMeasured(cell())).toBe(false);
    expect(isMeasured(undefined)).toBe(false);
  });

  it("counts a latency sample as a measurement", () => {
    expect(isMeasured(cell({ rttP95: 2_200_000 }))).toBe(true);
  });

  it("counts a packet-loss sample as a measurement", () => {
    expect(isMeasured(cell({ lossRatio: 0 }))).toBe(true);
  });

  it("counts a failure ratio of zero as a measurement, not as absence", () => {
    expect(isMeasured(cell({ failRatio: 0 }))).toBe(true);
  });
});

describe("severityRatio", () => {
  it("is null when neither ratio was reported, however much latency there is", () => {
    expect(severityRatio(cell({ rttP95: 9_000_000 }))).toBeNull();
  });

  it("takes the worst of the two ratios that are present", () => {
    expect(severityRatio(cell({ failRatio: 0.02, lossRatio: 0.4 }))).toBe(0.4);
    expect(severityRatio(cell({ failRatio: 0.5, lossRatio: 0.01 }))).toBe(0.5);
  });
});

describe("cellTier", () => {
  it("is unknown only for a cell nothing measured", () => {
    expect(cellTier(cell())).toBe("unknown");
  });

  it("is ok for a measured cell whose failure series simply has no samples", () => {
    expect(cellTier(cell({ rttP95: 2_200_000 }))).toBe("ok");
  });

  it("reads the tier from packet loss alone when there is no failure ratio", () => {
    expect(cellTier(cell({ rttP95: 1e6, lossRatio: 0.05 }))).toBe("warn");
    expect(cellTier(cell({ rttP95: 1e6, lossRatio: 0.2 }))).toBe("bad");
    expect(cellTier(cell({ rttP95: 1e6, lossRatio: 0.001 }))).toBe("ok");
  });

  it("still reads the failure ratio when it is the louder of the two", () => {
    expect(cellTier(cell({ failRatio: 0.3, lossRatio: 0 }))).toBe("bad");
  });
});

describe("isProblemCell", () => {
  it("includes a loss-only cell that crossed the degraded line", () => {
    expect(isProblemCell(cell({ lossRatio: 0.05 }))).toBe(true);
  });

  it("excludes a measured cell with no ratio at all — silence is not a problem", () => {
    expect(isProblemCell(cell({ rttP95: 5e6 }))).toBe(false);
  });
});

describe("cellSummary", () => {
  it("says no data only when nothing was measured", () => {
    expect(cellSummary(cell())).toBe("no data");
  });

  it("names the missing half rather than claiming the whole cell is empty", () => {
    expect(cellSummary(cell({ rttP95: 2_200_000 }))).toBe("no failure signal recorded, RTT p95 2.2ms");
  });

  it("carries packet loss when the protocol reports it", () => {
    expect(cellSummary(cell({ rttP95: 2_200_000, lossRatio: 0.05 }))).toBe(
      "no failure signal recorded, RTT p95 2.2ms, packet loss 5.0%",
    );
  });

  it("keeps the wording the grid's aria-labels already used for a full cell", () => {
    expect(cellSummary(cell({ failRatio: 0.5, rttP95: 2_000_000 }))).toBe("fail 50.0%, RTT p95 2.0ms");
  });
});
