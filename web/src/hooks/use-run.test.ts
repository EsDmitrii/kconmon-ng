import { describe, expect, it } from "vitest";
import { pmtuReadingOf } from "./use-run";

describe("pmtuReadingOf", () => {
  it("reads a pmtu result's details", () => {
    expect(
      pmtuReadingOf({ type: "pmtu", details: { probeMtu: 1500, pathMtu: 1400, verdict: "reduced", steps: 2 } }),
    ).toEqual({ verdict: "reduced", probeMtu: 1500, pathMtu: 1400, steps: 2, truncated: false });
  });

  it("ignores other types, a missing or unknown verdict, and junk", () => {
    expect(pmtuReadingOf({ type: "tcp", details: { verdict: "ok" } })).toBeUndefined();
    expect(pmtuReadingOf({ type: "pmtu", details: { verdict: "maybe" } })).toBeUndefined();
    expect(pmtuReadingOf({ type: "pmtu" })).toBeUndefined();
    expect(pmtuReadingOf("pmtu")).toBeUndefined();
    expect(pmtuReadingOf(null)).toBeUndefined();
  });

  it("does not trust the numbers either", () => {
    expect(pmtuReadingOf({ type: "pmtu", details: { verdict: "blackhole", pathMtu: "1400", steps: Number.NaN } })).toEqual({
      verdict: "blackhole", probeMtu: 0, pathMtu: 0, steps: 0, truncated: false,
    });
  });
});
