import { describe, expect, it } from "vitest";
import { LAST_COL, POINTS_COL, TIME_COL, VALUE_COL, formatValue, isFigureColumn, isNumericColumn, toTable } from "./prom-table";
import type { PromResult } from "./types";

describe("toTable", () => {
  /* The value is the answer the query was run for. Behind every label column of `up` (ten of them)
     it started past the right edge of a 1440px desktop, so it leads and the labels scroll. */
  it("puts the value and its stamp before the label columns", () => {
    const iso = (ms: number) => new Date(ms).toISOString();
    const vector = toTable(
      {
        status: "success",
        data: {
          resultType: "vector",
          result: [
            { metric: { job: "a", instance: "x" }, value: [1700000000, "1"] },
            { metric: { job: "b", instance: "y" }, value: [1699999000, "0"] },
          ],
        },
      },
      iso,
    );
    expect(vector.columns).toEqual([VALUE_COL, TIME_COL, "instance", "job"]);
    expect(vector.rows[0]).toEqual(["1", iso(1700000000000), "x", "a"]);

    const series = toTable({
      status: "success",
      data: { resultType: "matrix", result: [{ metric: { host: "h" }, values: [[1700000000, "0.1"], [1700000060, "0.3"]] }] },
    });
    expect(series.columns).toEqual([LAST_COL, POINTS_COL, "host"]);
    expect(series.rows).toEqual([["0.3", "2", "h"]]);
  });

  it("maps a vector result to a value column plus sorted label columns", () => {
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
    expect(table.columns).toEqual([VALUE_COL, "destination_node", "source_node"]);
    expect(table.rows).toEqual([
      ["0.5", "a", "b"],
      ["0.1", "c", "a"],
    ]);
  });

  it("maps a matrix result to the last value and points count plus label columns", () => {
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

    expect(table.columns).toEqual([LAST_COL, POINTS_COL, "host"]);
    expect(table.rows).toEqual([["0.3", "3", "example.com"]]);
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
    expect(table.columns).toEqual([VALUE_COL, "job"]);
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
    expect(table.columns).toEqual([LAST_COL, TIME_COL, POINTS_COL, "host"]);
    expect(table.rows).toEqual([
      ["0.3", iso(1700000060000), "1", "live"],
      ["0.9", iso(1699996400000), "1", "stopped"],
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
    expect(table.columns).toEqual([LAST_COL, POINTS_COL, "host"]);
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

/* ── how a column sits, and how a figure reads ───────────────────────────── */

describe("isNumericColumn", () => {
  it("tags this module's own columns as figures, the stamp column under any name it stepped aside to", () => {
    expect(isNumericColumn(VALUE_COL)).toBe(true);
    expect(isNumericColumn(POINTS_COL)).toBe(true);
    expect(isNumericColumn(LAST_COL)).toBe(true);
    expect(isNumericColumn(TIME_COL)).toBe(true);
    expect(isNumericColumn(`${TIME_COL}_`)).toBe(true);
  });

  it("leaves a label column alone — an identifier is not a figure", () => {
    for (const label of ["instance", "pod", "value", "time", "__name__"]) expect(isNumericColumn(label)).toBe(false);
  });

  it("narrows to the two columns that carry a SAMPLE for formatValue", () => {
    expect(isFigureColumn(VALUE_COL)).toBe(true);
    expect(isFigureColumn(LAST_COL)).toBe(true);
    expect(isFigureColumn(POINTS_COL)).toBe(false);
    expect(isFigureColumn(TIME_COL)).toBe(false);
  });
});

/**
 * The listing printed `0.004947212522190138` in every last-value cell (audit
 * frame console-range-table): sixteen digits of a p95 that moves in its third.
 * Six significant figures on screen; the exact string stays on the cell's
 * title and in the JSON tab.
 */
describe("formatValue", () => {
  it("prints six significant figures with the trailing zeros trimmed", () => {
    expect(formatValue("0.004947212522190138")).toBe("0.00494721");
    expect(formatValue("0.0000012345678")).toBe("0.00000123457");
    expect(formatValue("1.000000")).toBe("1");
    expect(formatValue("0.5")).toBe("0.5");
    expect(formatValue("2.50")).toBe("2.5");
  });

  it("prints a count whole — an integer is not rounded away", () => {
    expect(formatValue("146")).toBe("146");
    expect(formatValue("1234567")).toBe("1234567");
    expect(formatValue("-3")).toBe("-3");
    expect(formatValue("0")).toBe("0");
  });

  it("keeps the exponent form readable past what six figures can hold", () => {
    expect(formatValue("1e21")).toBe("1e+21");
    expect(formatValue("1234567890123456789")).toBe("1.23457e+18");
  });

  it("leaves Prometheus's own tokens and a string result exactly as they arrived", () => {
    for (const raw of ["NaN", "+Inf", "-Inf", "hello", "", "  "]) expect(formatValue(raw)).toBe(raw);
  });
});
