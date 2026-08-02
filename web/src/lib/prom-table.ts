import type { PromResult } from "./types";

export interface PromTable { columns: string[]; rows: string[][] }

interface VectorEntry { metric: Record<string, string>; value?: [number, string]; values?: [number, string][] }

export function toTable(res: PromResult): PromTable {
  if (res.status !== "success" || !res.data) return { columns: [], rows: [] };
  const { resultType, result } = res.data;
  if (resultType === "scalar") {
    const [, v] = result as unknown as [number, string];
    return { columns: ["value"], rows: [[v]] };
  }
  const entries = result as unknown as VectorEntry[];
  const labels = [...new Set(entries.flatMap((e) => Object.keys(e.metric)))].sort();
  if (resultType === "vector") {
    return {
      columns: [...labels, "value"],
      rows: entries.map((e) => [...labels.map((l) => e.metric[l] ?? ""), e.value?.[1] ?? ""]),
    };
  }
  if (resultType === "matrix") {
    return {
      columns: [...labels, "points", "last value"],
      rows: entries.map((e) => {
        const vs = e.values ?? [];
        return [...labels.map((l) => e.metric[l] ?? ""), String(vs.length), vs.at(-1)?.[1] ?? ""];
      }),
    };
  }
  return { columns: [], rows: [] };
}
