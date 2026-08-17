import { describe, expect, it } from "vitest";
import { compareNaturalName } from "./natural-name";

describe("compareNaturalName", () => {
  it("reads the digits in a name as a number, not as characters", () => {
    expect(["m10", "m2", "m9", "m1"].sort(compareNaturalName)).toEqual(["m1", "m2", "m9", "m10"]);
  });

  it("puts a bare prefix before the names built on it", () => {
    expect(["kconmon-prod-m02", "kconmon-prod", "kconmon-prod-m10"].sort(compareNaturalName)).toEqual([
      "kconmon-prod",
      "kconmon-prod-m02",
      "kconmon-prod-m10",
    ]);
  });

  it("orders an address by its octets, the way it is read", () => {
    expect(["10.0.0.10", "10.0.0.2", "10.0.0.1"].sort(compareNaturalName)).toEqual([
      "10.0.0.1",
      "10.0.0.2",
      "10.0.0.10",
    ]);
  });

  /* Two names the collator calls equal must still land in ONE order: a sort that
     answers 0 for them lets the engine place them however it likes, and the pane
     would reshuffle between renders of the same data. */
  it("breaks a collator tie by codepoint rather than calling the names equal", () => {
    expect(compareNaturalName("Node-A", "node-a")).not.toBe(0);
    expect(["node-a", "Node-A"].sort(compareNaturalName)).toEqual(["Node-A", "node-a"]);
  });

  it("is 0 only for the same name", () => {
    expect(compareNaturalName("node-a", "node-a")).toBe(0);
  });
});
