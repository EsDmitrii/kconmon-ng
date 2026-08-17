import { describe, expect, it } from "vitest";
import { cellTier, fmtRatio, fmtRtt, isMeasured, isProblemCell, severityRatio } from "@/lib/matrix-cells";
import type { MatrixCell } from "@/lib/types";

/* The shared cell helpers must survive the wire shapes a nil-slice / nil-pointer
   backend and a hostile fuzzer produce — null is not zero, and a corrupt number
   never gets to look healthy. */

const cell = (over: Partial<MatrixCell>): MatrixCell =>
  ({ source: "a", destination: "b", failRatio: null, ...over }) as MatrixCell;

describe("fmtRtt", () => {
  it("renders a finite ns as ms", () => expect(fmtRtt(2_000_000)).toBe("2.0ms"));
  it("dashes null rather than fabricating 0.0ms", () => expect(fmtRtt(null)).toBe("—"));
  it("dashes undefined", () => expect(fmtRtt(undefined)).toBe("—"));
  it("dashes NaN", () => expect(fmtRtt(Number.NaN)).toBe("—"));
  it("dashes Infinity", () => expect(fmtRtt(Number.POSITIVE_INFINITY)).toBe("—"));
});

describe("fmtRatio", () => {
  it("renders a finite ratio as a percentage", () => expect(fmtRatio(0.5)).toBe("50.0%"));
  it("dashes NaN rather than printing NaN%", () => expect(fmtRatio(Number.NaN)).toBe("—"));
  it("dashes Infinity rather than printing Infinity%", () => expect(fmtRatio(Number.POSITIVE_INFINITY)).toBe("—"));
  it("dashes null", () => expect(fmtRatio(null)).toBe("—"));
});

describe("isMeasured", () => {
  it("is false for a cell whose only fields are null/NaN/Infinity", () => {
    expect(isMeasured(cell({ failRatio: null, rttP95: Number.NaN, lossRatio: Number.POSITIVE_INFINITY }))).toBe(false);
  });
  it("is true for a real latency sample", () => expect(isMeasured(cell({ rttP95: 1_000_000 }))).toBe(true));
});

describe("cellTier never paints a corrupt cell green", () => {
  it("tiers unknown, not ok, when every number is non-finite", () => {
    expect(cellTier(cell({ failRatio: Number.NaN, lossRatio: Number.POSITIVE_INFINITY }))).toBe("unknown");
  });
  it("ignores a NaN ratio and reads the finite loss", () => {
    expect(cellTier(cell({ failRatio: Number.NaN, lossRatio: 0.3 }))).toBe("bad");
  });
  it("stays ok on a genuinely clean cell", () => {
    expect(cellTier(cell({ failRatio: 0, rttP95: 900_000 }))).toBe("ok");
  });
});

describe("severityRatio / isProblemCell drop non-finite inputs", () => {
  it("returns null when nothing finite is carried", () => {
    expect(severityRatio(cell({ failRatio: Number.NaN }))).toBeNull();
  });
  it("does not flag a problem on a corrupt-only cell", () => {
    expect(isProblemCell(cell({ failRatio: Number.POSITIVE_INFINITY }))).toBe(false);
  });
});
