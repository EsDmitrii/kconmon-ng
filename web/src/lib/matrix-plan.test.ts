import { describe, expect, it } from "vitest";
import { isPlanExcluded, readProbePlan } from "./matrix-plan";

describe("readProbePlan", () => {
  it("answers null — the full-mesh reading — for an absent or non-object field", () => {
    expect(readProbePlan(undefined)).toBeNull();
    expect(readProbePlan(null)).toBeNull();
    expect(readProbePlan("plan")).toBeNull();
    expect(readProbePlan(7)).toBeNull();
    expect(readProbePlan(["a"])).toBeNull();
  });

  it("reads a well-formed plan, empty lists included", () => {
    const plan = readProbePlan({ a: ["b", "c"], b: [] });
    expect(plan).not.toBeNull();
    expect(plan?.get("a")).toEqual(new Set(["b", "c"]));
    expect(plan?.get("b")).toEqual(new Set());
  });

  it("drops an unreadable entry rather than reading it as 'probes nobody'", () => {
    // A row misread as empty would paint that node's whole line 'not probed' —
    // the calming state — so a non-array value must vanish, not become [].
    const plan = readProbePlan({ a: "b", b: ["a"] });
    expect(plan?.has("a")).toBe(false);
    expect(plan?.get("b")).toEqual(new Set(["a"]));
  });

  it("skips non-string and empty members inside a list", () => {
    const plan = readProbePlan({ a: ["b", 7, null, "", "c"] });
    expect(plan?.get("a")).toEqual(new Set(["b", "c"]));
  });
});

describe("isPlanExcluded", () => {
  const plan = readProbePlan({ a: ["b"], b: [], c: ["a"] });

  it("is always false with no plan in force — full mode renders zero 'not probed' cells", () => {
    expect(isPlanExcluded(null, "a", "b")).toBe(false);
    expect(isPlanExcluded(null, "a", "c")).toBe(false);
  });

  it("marks exactly the complement of the plan's assignments", () => {
    expect(isPlanExcluded(plan, "a", "b")).toBe(false); // assigned
    expect(isPlanExcluded(plan, "a", "c")).toBe(true); // not assigned
    expect(isPlanExcluded(plan, "b", "a")).toBe(true); // fail-closed empty list: probes nobody
    expect(isPlanExcluded(plan, "b", "c")).toBe(true);
    expect(isPlanExcluded(plan, "c", "a")).toBe(false);
    expect(isPlanExcluded(plan, "c", "b")).toBe(true);
  });

  it("never marks a source the plan does not mention — a missing agent is an incident, not an exclusion", () => {
    expect(isPlanExcluded(plan, "ghost", "a")).toBe(false);
  });

  it("never marks the diagonal", () => {
    expect(isPlanExcluded(plan, "b", "b")).toBe(false);
  });
});
