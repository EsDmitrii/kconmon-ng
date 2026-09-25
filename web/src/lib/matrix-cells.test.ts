import { describe, expect, it } from "vitest";
import {
  cellSummary,
  cellTier,
  isMeasured,
  isProblemCell,
  isReducedPath,
  pmtuReading,
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
  it("paints a fresh PMTU black hole red, the colour the legend gives a black hole", () => {
    const fresh = { source: "a", destination: "b", failRatio: 0.05, mtuBytes: 1400, probeMtuBytes: 1500 };
    expect(pmtuReading(fresh)).toBe("blackhole");
    expect(cellTier(fresh)).toBe("bad");
  });

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

describe("path MTU cells", () => {
  const ok = { source: "a", destination: "b", failRatio: 0, mtuBytes: 1500, probeMtuBytes: 1500 };
  const reduced = { source: "a", destination: "b", failRatio: 0, mtuBytes: 1450, probeMtuBytes: 1500 };
  const blackhole = { source: "a", destination: "b", failRatio: 1, mtuBytes: 1400, probeMtuBytes: 1500 };

  it("reads a full-size path as ok, a reduced one as degraded, a black hole as failing", () => {
    expect(cellTier(ok)).toBe("ok");
    expect(cellTier(reduced)).toBe("warn");
    expect(cellTier(blackhole)).toBe("bad");
  });

  it("counts an MTU reading as a measurement even without a failure series", () => {
    expect(isMeasured({ source: "a", destination: "b", failRatio: null, mtuBytes: 1500 })).toBe(true);
  });

  it("flags a reduced path as a problem pair, and a full one not", () => {
    expect(isProblemCell(reduced)).toBe(true);
    expect(isProblemCell(ok)).toBe(false);
    expect(isReducedPath(reduced)).toBe(true);
    expect(isReducedPath({ ...reduced, probeMtuBytes: undefined })).toBe(false);
  });

  it("says the path MTU in the summary", () => {
    expect(cellSummary(reduced)).toContain("path MTU 1450 of 1500 bytes");
    expect(cellSummary(ok)).toContain("path MTU 1500 bytes");
  });
});

describe("pmtuReading", () => {
  const cell = (failRatio: number | null, mtuBytes: number, probeMtuBytes = 1500) => ({
    source: "a", destination: "b", failRatio, mtuBytes, probeMtuBytes,
  });
  it("reads a full path, a reduced one and a black hole", () => {
    expect(pmtuReading(cell(0, 1500))).toBe("full");
    expect(pmtuReading(cell(0, 1450))).toBe("reduced");
    expect(pmtuReading(cell(null, 1450))).toBe("reduced");
    expect(pmtuReading(cell(1, 1400))).toBe("blackhole");
  });
  it("calls a fresh black hole one before its ratio crosses the failing line", () => {
    expect(pmtuReading(cell(0.03, 1400))).toBe("blackhole");
    expect(pmtuReading(cell(0.03, 1500))).toBe("full");
  });
  it("has nothing to say about a cell with no path MTU", () => {
    expect(pmtuReading({ source: "a", destination: "b", failRatio: 0 })).toBeNull();
  });
});
