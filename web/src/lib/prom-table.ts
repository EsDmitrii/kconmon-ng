import { MAX_TIME_MS } from "./investigation-sources";
import type { PromResult } from "./types";

/**
 * prom-table.ts — one Prometheus result, read as a table.
 *
 * Prometheus answers an instant query with FOUR result types, not two, and this
 * builder used to reach for `.metric` on whatever it was handed: a `string`
 * result — which the perfectly legal expression `"hello"` returns, and which the
 * console's guarded proxy passes through verbatim — is `[timestamp, "hello"]`,
 * so the walk hit `Object.keys(undefined)` and threw a TypeError out of render.
 * The Console page went white on a valid query.
 *
 * The shape checks below are that lesson generalised: this module reads somebody
 * else's JSON, so every branch says what it expects and hands back an empty
 * table rather than an exception when it does not get it.
 *
 * Every value Prometheus returns is stamped, and the table used to drop the stamp:
 * a page of figures with no way to tell WHEN they were read (owner report). The
 * stamp comes back either as `at` — one instant the whole table shares, said once
 * above it — or, when the rows disagree, as a column.
 */

export interface PromTable {
  /** Label names verbatim, and this module's OWN columns as dictionary keys
   *  (`table.col.*`): the header is read by a person, so the words belong to the
   *  interface language, while a label name is an identifier and is not. */
  columns: string[];
  rows: string[][];
  /** ms. The one instant every row was read at; null when the rows disagree (a
   *  `time` column is then in the table) or the result carried no stamp at all. */
  at: number | null;
  /** What the value column IS — one reading, or the last of a series. The two
   *  need different words above them. */
  kind: "instant" | "series";
}

interface VectorEntry { metric?: Record<string, string>; value?: [number, string]; values?: [number, string][] }

/* This module's own column names travel as DICTIONARY KEYS; the page turns them
   into words. A label named "value" is a different string and stays verbatim. */
export const VALUE_COL = "table.col.value";
export const POINTS_COL = "table.col.points";
export const LAST_COL = "table.col.last";
export const TIME_COL = "table.col.time";


const EMPTY: PromTable = { columns: [], rows: [], at: null, kind: "instant" };

/*
 * Prometheus stamps in float seconds; the console works in ms. NaN for anything else — and
 * "anything else" includes a number that is FINITE but outside what a Date can hold.
 *
 * `Number.isFinite` alone let 1e13 seconds through: `new Date(1e16)` is an Invalid Date, and this
 * module's own product is then printed as "Read at Invalid Date" above the figures, or as
 * "Invalid Date" in the time cell of every row that carries one. An absent stamp is handled
 * gracefully here (no sentence, no column); an unrepresentable one has to be handled the same way,
 * which is the whole contract of a module that exists to absorb somebody else's JSON.
 */
function stampMs(ts: unknown): number {
  if (typeof ts !== "number" || !Number.isFinite(ts)) return NaN;
  const ms = ts * 1000;
  return Math.abs(ms) <= MAX_TIME_MS ? ms : NaN;
}

/** Both `scalar` and `string` are ONE reading at one instant: `[ts, "value"]`. */
function singleValue(result: unknown): PromTable {
  const value = Array.isArray(result) ? result[1] : undefined;
  const at = stampMs(Array.isArray(result) ? result[0] : undefined);
  return {
    columns: [VALUE_COL],
    rows: [[value === undefined || value === null ? "" : String(value)]],
    at: Number.isNaN(at) ? null : at,
    kind: "instant",
  };
}

/**
 * shared answers "is there ONE instant here", which decides between a sentence
 * above the table and a column inside it. A stamp the result did not carry is
 * NaN and disagrees with everything, including itself — so a mixed set falls to
 * the column, which is the honest one.
 */
function shared(stamps: number[]): number | null {
  if (stamps.length === 0) return null;
  const first = stamps[0];
  if (Number.isNaN(first)) return null;
  return stamps.every((s) => s === first) ? first : null;
}

/**
 * toTable reads a result into columns and rows.
 *
 * `formatTime` is passed in rather than chosen here: the time column is written in the INTERFACE
 * language, and this module has no business knowing which one that is. Without it a ragged result
 * simply carries no time column — the caller asked for no times.
 */
export function toTable(res: PromResult, formatTime?: (ms: number) => string): PromTable {
  if (res.status !== "success" || !res.data) return EMPTY;
  const { resultType, result } = res.data;
  if (resultType === "scalar" || resultType === "string") return singleValue(result);
  if (resultType !== "vector" && resultType !== "matrix") return EMPTY;

  // A `result` that is not a list is not a result set, whatever the envelope
  // claims the type is.
  const entries: VectorEntry[] = Array.isArray(result) ? (result as VectorEntry[]) : [];
  const labels = [...new Set(entries.flatMap((e) => Object.keys(e?.metric ?? {})))].sort();
  const kind = resultType === "vector" ? "instant" : "series";

  /* The instant each row's VALUE was read at: an instant query's own timestamp,
     or — for a series — the timestamp of the last point, which is the point the
     row prints. Series that stopped early legitimately disagree, and that
     disagreement is worth seeing. */
  const stamps = entries.map((e) =>
    resultType === "vector" ? stampMs(e?.value?.[0]) : stampMs((e?.values ?? []).at(-1)?.[0]),
  );
  const at = shared(stamps);
  const withTime = at === null && formatTime !== undefined && stamps.some((s) => !Number.isNaN(s));
  const timeCell = (i: number) =>
    withTime ? [Number.isNaN(stamps[i]) ? "" : formatTime!(stamps[i])] : [];
  /* A label really can be called `time`, and two identical headers is both a
     lie on screen and a duplicate React key. The stamp column steps aside with a
     suffix rather than renaming the operator's own label. */
  const timeName = (() => {
    let name = TIME_COL;
    while (labels.includes(name)) name += "_";
    return name;
  })();
  const timeColumn = withTime ? [timeName] : [];

  if (resultType === "vector") {
    return {
      columns: [...labels, ...timeColumn, VALUE_COL],
      rows: entries.map((e, i) => [...labels.map((l) => e?.metric?.[l] ?? ""), ...timeCell(i), e?.value?.[1] ?? ""]),
      at,
      kind,
    };
  }
  return {
    columns: [...labels, POINTS_COL, ...timeColumn, LAST_COL],
    rows: entries.map((e, i) => {
      const vs = e?.values ?? [];
      return [...labels.map((l) => e?.metric?.[l] ?? ""), String(vs.length), ...timeCell(i), vs.at(-1)?.[1] ?? ""];
    }),
    at,
    kind,
  };
}
