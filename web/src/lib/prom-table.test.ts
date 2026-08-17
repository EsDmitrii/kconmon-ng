import { describe, expect, it } from "vitest";
import { LAST_COL, POINTS_COL, TIME_COL, VALUE_COL, toTable } from "./prom-table";
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
    expect(table.columns).toEqual(["destination_node", "source_node", VALUE_COL]);
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

    expect(table.columns).toEqual(["host", POINTS_COL, LAST_COL]);
    expect(table.rows).toEqual([["example.com", "3", "0.3"]]);
  });

  it("maps a scalar result to a single value column/row", () => {
    const res: PromResult = {
      status: "success",
      data: { resultType: "scalar", result: [1700000000, "42"] },
    };

    const table = toTable(res);

    expect(table.columns).toEqual([VALUE_COL]);
    expect(table.rows).toEqual([["42"]]);
  });

  it("returns an empty table for a Prometheus error envelope", () => {
    const res: PromResult = { status: "error", errorType: "bad_data", error: "parse error" };

    expect(toTable(res)).toEqual({ columns: [], rows: [], at: null, kind: "instant" });
  });

  it("returns an empty table when result entries have no labels at all", () => {
    const res: PromResult = {
      status: "success",
      data: { resultType: "vector", result: [] },
    };

    expect(toTable(res)).toEqual({ columns: [VALUE_COL], rows: [], at: null, kind: "instant" });
  });
});

/*
Every figure Prometheus returns is stamped, and the table dropped the stamp: a page of numbers
with no way to tell when they were read (owner report). One instant for the whole table is a
sentence above it; instants that disagree are a column.
*/
describe("toTable timestamps", () => {
  const iso = (ms: number) => new Date(ms).toISOString();

  it("carries the instant a vector was read at, once for the table", () => {
    const table = toTable({
      status: "success",
      data: {
        resultType: "vector",
        result: [
          { metric: { job: "a" }, value: [1700000000, "1"] },
          { metric: { job: "b" }, value: [1700000000, "0"] },
        ],
      },
    });

    expect(table.at).toBe(1700000000000);
    expect(table.kind).toBe("instant");
    // One instant for every row means no column for it.
    expect(table.columns).toEqual(["job", VALUE_COL]);
  });

  it("stamps a range table with the instant its LAST values came from", () => {
    const table = toTable({
      status: "success",
      data: {
        resultType: "matrix",
        result: [{ metric: { host: "a" }, values: [[1700000000, "0.1"], [1700000060, "0.3"]] }],
      },
    });

    expect(table.at).toBe(1700000060000);
    expect(table.kind).toBe("series");
  });

  it("puts a time column in instead when the rows disagree — a series that stopped early", () => {
    const table = toTable(
      {
        status: "success",
        data: {
          resultType: "matrix",
          result: [
            { metric: { host: "live" }, values: [[1700000060, "0.3"]] },
            { metric: { host: "stopped" }, values: [[1699996400, "0.9"]] },
          ],
        },
      },
      iso,
    );

    expect(table.at).toBeNull();
    expect(table.columns).toEqual(["host", POINTS_COL, TIME_COL, LAST_COL]);
    expect(table.rows).toEqual([
      ["live", "1", iso(1700000060000), "0.3"],
      ["stopped", "1", iso(1699996400000), "0.9"],
    ]);
  });

  it("adds no time column when the caller passed no formatter, rather than inventing a format", () => {
    const table = toTable({
      status: "success",
      data: {
        resultType: "matrix",
        result: [
          { metric: { host: "live" }, values: [[1700000060, "0.3"]] },
          { metric: { host: "stopped" }, values: [[1699996400, "0.9"]] },
        ],
      },
    });

    expect(table.at).toBeNull();
    expect(table.columns).toEqual(["host", POINTS_COL, LAST_COL]);
  });

  it("stamps a scalar, and claims no instant for a series with no points at all", () => {
    expect(toTable({ status: "success", data: { resultType: "scalar", result: [1700000000, "42"] } }).at).toBe(
      1700000000000,
    );
    expect(
      toTable({ status: "success", data: { resultType: "matrix", result: [{ metric: { host: "a" }, values: [] }] } })
        .at,
    ).toBeNull();
  });
});

/* ── Swarm finding: a finite stamp is not the same as a representable one ── */

/*
 * A vector whose stamp is 1e13 SECONDS is finite, so the old guard passed it through: `new Date(1e16)`
 * is an Invalid Date, and the page printed "Read at Invalid Date" above the figures — a claim about
 * when the data was read that is visibly broken. An absent stamp already produces no sentence and no
 * column; an unrepresentable one now does the same.
 */
describe("a stamp outside the Date range is treated as no stamp at all", () => {
  const huge = 1e13; // seconds -> 1e16 ms, past ECMA-262's ±8.64e15

  it("drops it from a vector's shared reading", () => {
    const table = toTable(
      { status: "success", data: { resultType: "vector", result: [{ metric: { job: "a" }, value: [huge, "1"] }] } },
      String,
    );
    expect(table.at).toBeNull();
    expect(table.columns).not.toContain(TIME_COL);
  });

  it("drops it from a scalar", () => {
    const table = toTable({ status: "success", data: { resultType: "scalar", result: [huge, "42"] } }, String);
    expect(table.at).toBeNull();
  });

  it("drops it from a matrix's last point", () => {
    const table = toTable(
      {
        status: "success",
        data: { resultType: "matrix", result: [{ metric: { job: "a" }, values: [[huge, "9"]] }] },
      },
      String,
    );
    expect(table.at).toBeNull();
  });

  it("keeps a representable stamp — the bound must not eat real data", () => {
    const table = toTable(
      { status: "success", data: { resultType: "vector", result: [{ metric: { job: "a" }, value: [1_754_000_000, "1"] }] } },
      String,
    );
    expect(table.at).toBe(1_754_000_000_000);
  });
});
