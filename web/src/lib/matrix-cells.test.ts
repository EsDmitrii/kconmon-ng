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
import { translate, type Translate } from "@/lib/i18n";
import { matrixCellsDict, type MatrixCellsKey } from "@/lib/i18n/dict/matrix-cells";

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

  it("flags a recovering path as a problem pair even under the 1% line, as the grid paints it amber", () => {
    const recovering = {
      source: "a", destination: "b", failRatio: 0.005, mtuBytes: 1500, probeMtuBytes: 1500, recentFailRatio: 0,
    };
    expect(cellTier(recovering)).toBe("warn");
    expect(isProblemCell(recovering)).toBe(true);
  });

  it("flags a reduced path as a problem pair, and a full one not", () => {
    expect(isProblemCell(reduced)).toBe(true);
    expect(isProblemCell(ok)).toBe(false);
    expect(isReducedPath(reduced)).toBe(true);
    expect(isReducedPath({ ...reduced, probeMtuBytes: undefined })).toBe(false);
  });

  /* The grid's aria-label is this sentence and hides the visible sub-line, so the verdict word
     has to be in it. */
  it("names a black hole and a recovering path in the summary, in both languages", () => {
    expect(cellSummary(blackhole)).toBe("fail 100.0%, path MTU 1400 of 1500 bytes, black hole");
    const recovering = {
      source: "a", destination: "b", failRatio: 0.4, mtuBytes: 1500, probeMtuBytes: 1500, recentFailRatio: 0,
    };
    expect(cellSummary(recovering)).toBe("fail 40.0%, path MTU 1500 bytes, recovering");
    const ruT: Translate<MatrixCellsKey> = (k, v) => translate(matrixCellsDict, "ru", k, v);
    expect(cellSummary(blackhole, ruT)).toContain("чёрная дыра");
    expect(cellSummary(recovering, ruT)).toContain("после сбоя");
  });

  it("says the path MTU in the summary", () => {
    expect(cellSummary(reduced)).toContain("path MTU 1450 of 1500 bytes");
    expect(cellSummary(ok)).toContain("path MTU 1500 bytes");
  });
});

describe("pmtuReading", () => {
  const cell = (failRatio: number | null, mtuBytes: number, probeMtuBytes = 1500, recentFailRatio?: number) => ({
    source: "a", destination: "b", failRatio, mtuBytes, probeMtuBytes, recentFailRatio,
  });
  it("reads a full path, a reduced one and a black hole", () => {
    expect(pmtuReading(cell(0, 1500))).toBe("full");
    expect(pmtuReading(cell(0, 1450))).toBe("reduced");
    expect(pmtuReading(cell(null, 1450))).toBe("reduced");
    expect(pmtuReading(cell(1, 1400))).toBe("blackhole");
  });
  it("calls a fresh black hole one before its ratio crosses the failing line", () => {
    expect(pmtuReading(cell(0.03, 1400))).toBe("blackhole");
  });
  /* Recovering is a pair whose recent probes are clean after failures: the window still holds
     them, the last minutes do not. */
  it("reads a full-size path with failures only earlier in the window as recovering, amber", () => {
    expect(pmtuReading(cell(0.03, 1500, 1500, 0))).toBe("recovering");
    expect(pmtuReading(cell(0.4, 1500, 1500, 0))).toBe("recovering");
    expect(cellTier(cell(0.4, 1500, 1500, 0))).toBe("warn");
  });
  /* Each probe dials a fresh UDP socket, so an ECMP-split black hole fails some of the probes while
     the gauge flips between reduced and full size. A full-size reading is only the last probe. */
  it("keeps an ECMP-split black hole red when its last probe crossed at full size", () => {
    expect(pmtuReading(cell(0.2, 1500, 1500, 0.5))).toBe("blackhole");
    expect(cellTier(cell(0.2, 1500, 1500, 0.5))).toBe("bad");
    expect(pmtuReading(cell(0.4, 1500, 1500, 0.34))).toBe("blackhole");
    expect(cellTier(cell(0.03, 1500, 1500, 0.1))).toBe("bad");
  });
  it("does not call a pair recovering without the recent window: the failing line decides", () => {
    expect(pmtuReading(cell(0.25, 1500))).toBe("blackhole");
    expect(cellTier(cell(0.25, 1500))).toBe("bad");
    expect(pmtuReading(cell(0.5, 1500))).toBe("blackhole");
    expect(pmtuReading(cell(1, 1460, 1450))).toBe("blackhole");
    expect(cellTier(cell(1, 1460, 1450))).toBe("bad");
    expect(pmtuReading(cell(0.05, 1500))).toBe("full");
    expect(cellTier(cell(0.05, 1500))).toBe("warn");
  });
  it("keeps a reduced reading with failures in the window a black hole even when recent probes are clean", () => {
    expect(pmtuReading(cell(0.2, 1400, 1500, 0))).toBe("blackhole");
  });
  it("keeps the failing line as the only rule when the probe size is unknown", () => {
    const noProbe = { source: "a", destination: "b", failRatio: 0.4, mtuBytes: 1500 };
    expect(pmtuReading(noProbe)).toBe("blackhole");
  });
  it("has nothing to say about a cell with no path MTU", () => {
    expect(pmtuReading({ source: "a", destination: "b", failRatio: 0 })).toBeNull();
  });
});
