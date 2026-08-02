import { describe, expect, it } from "vitest";
import { toTable } from "./prom-table";
import type { PromResult } from "./types";

describe("toTable", () => {
  it("maps a vector result to sorted label columns plus a value column", () => {
    const res: PromResult = {
      status: "success",
      data: {
        resultType: "vector",
        result: [
          { metric: { source_node: "b", destination_node: "a" }, value: [1700000000, "0.5"] },
          { metric: { source_node: "a", destination_node: "c" }, value: [1700000000, "0.1"] },
        ],
      },
    };

    const table = toTable(res);

    // Labels sorted alphabetically: destination_node before source_node.
    expect(table.columns).toEqual(["destination_node", "source_node", "value"]);
    expect(table.rows).toEqual([
      ["a", "b", "0.5"],
      ["c", "a", "0.1"],
    ]);
  });

  it("maps a matrix result to label columns plus points count and last value", () => {
    const res: PromResult = {
      status: "success",
      data: {
        resultType: "matrix",
        result: [
          {
            metric: { host: "example.com" },
            values: [
              [1700000000, "0.1"],
              [1700000030, "0.2"],
              [1700000060, "0.3"],
            ],
          },
        ],
      },
    };

    const table = toTable(res);

    expect(table.columns).toEqual(["host", "points", "last value"]);
    expect(table.rows).toEqual([["example.com", "3", "0.3"]]);
  });

  it("maps a scalar result to a single value column/row", () => {
    const res: PromResult = {
      status: "success",
      data: { resultType: "scalar", result: [1700000000, "42"] },
    };

    const table = toTable(res);

    expect(table.columns).toEqual(["value"]);
    expect(table.rows).toEqual([["42"]]);
  });

  it("returns an empty table for a Prometheus error envelope", () => {
    const res: PromResult = { status: "error", errorType: "bad_data", error: "parse error" };

    expect(toTable(res)).toEqual({ columns: [], rows: [] });
  });

  it("returns an empty table when result entries have no labels at all", () => {
    const res: PromResult = {
      status: "success",
      data: { resultType: "vector", result: [] },
    };

    expect(toTable(res)).toEqual({ columns: ["value"], rows: [] });
  });
});
