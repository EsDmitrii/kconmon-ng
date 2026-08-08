import { fmtRttNs, fmtTime, isPlaceholderHop, shortHash } from "@/components/mtr-hop-table";
import type { MTRHop, PathSnapshot } from "@/lib/types";
import { cn } from "@/lib/utils";

/* ── the diff, as a pure function ───────────────────────────────────────── */

export type DiffKind = "same" | "changed" | "added" | "removed";

/**
 * DiffRow is one line of the aligned two-column table: what the OLDER path had
 * there (`aHop`), what the NEWER one has (`bHop`), and how to read the pair.
 *
 * `rttDeltaNs` is present ONLY on `same` rows — it is `bHop.rttNs -
 * aHop.rttNs`, i.e. how much slower (positive) or faster (negative) the very
 * same machine answers now. A `changed` row holds two DIFFERENT addresses, and
 * subtracting one machine's RTT from another's would produce a number that
 * looks like a latency change and is not one.
 */
export interface DiffRow {
  kind: DiffKind;
  aHop?: MTRHop;
  bHop?: MTRHop;
  rttDeltaNs?: number;
}

/** anchorable is the equality the alignment is built on: the hop ADDRESS, and
 *  only for hops that have one. The tracer writes "*" for a hop that never
 *  answered (isPlaceholderHop), and two unanswered hops are two unknowns —
 *  anchoring them to each other would assert that the same silent machine sits
 *  at both positions, which nothing in the payload supports. */
function anchorable(x: MTRHop, y: MTRHop): boolean {
  return !isPlaceholderHop(x.ip) && x.ip === y.ip;
}

/**
 * lcsAnchors is the alignment, stated honestly: a longest common subsequence
 * over hop IPs. Every pair it returns is a hop that appears in BOTH paths, in
 * an order both paths agree on.
 *
 * What that buys and what it costs:
 *
 *  - An inserted or withdrawn hop shifts everything after it, and LCS still
 *    lines the tail up — which position-only alignment (`a[i]` vs `b[i]`) does
 *    not: one extra hop at the top would otherwise report the whole rest of the
 *    trace as changed.
 *  - A REORDER — the same addresses in a different order — has no common
 *    subsequence containing both, so LCS reports it as one removal plus one
 *    addition rather than as a move. That is a real limitation of this
 *    alignment and it is deliberately not papered over: "the route now visits
 *    10.0.0.2 after 10.0.0.3" and "10.0.0.2 left and a new 10.0.0.2 joined" are
 *    indistinguishable from the hop list alone, and the diff says the thing it
 *    can actually see.
 *
 * O(n·m) over ≤64 hops each (the tracer's own cap), so ≤4096 cells — the cost
 * of the table is irrelevant next to the fetch that produced the snapshots.
 */
function lcsAnchors(a: MTRHop[], b: MTRHop[]): [number, number][] {
  const n = a.length;
  const m = b.length;
  const dp: number[][] = Array.from({ length: n + 1 }, () => new Array<number>(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      dp[i][j] = anchorable(a[i], b[j]) ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
    }
  }
  const anchors: [number, number][] = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (anchorable(a[i], b[j])) {
      anchors.push([i, j]);
      i++;
      j++;
    } else if (dp[i + 1][j] >= dp[i][j + 1]) {
      i++;
    } else {
      j++;
    }
  }
  return anchors;
}

/**
 * diffPaths aligns two snapshots' hop lists and says what happened between
 * them. `a` is the OLDER path and `b` the newer one — the caller orders them
 * (by first_seen), and every kind below is phrased from the older path's point
 * of view: `added` means the newer path gained the hop.
 *
 * Alignment = LCS over hop IPs (see lcsAnchors), then, in each gap BETWEEN two
 * anchors, the leftover a-side and b-side hops are zipped positionally into
 * `changed` rows — this is the "same place in the route, different machine"
 * case, which is what a rerouted hop actually looks like — and whichever side
 * runs out first leaves the surplus as plain `removed`/`added` rows.
 *
 * Pure and total: no snapshot metadata is read, nothing is sorted, and an
 * empty list on either side is a legal input.
 */
export function diffPaths(a: MTRHop[], b: MTRHop[]): DiffRow[] {
  const rows: DiffRow[] = [];
  let ai = 0;
  let bi = 0;

  const gap = (aEnd: number, bEnd: number) => {
    const aRun = a.slice(ai, aEnd);
    const bRun = b.slice(bi, bEnd);
    const zipped = Math.min(aRun.length, bRun.length);
    for (let k = 0; k < zipped; k++) rows.push({ kind: "changed", aHop: aRun[k], bHop: bRun[k] });
    for (let k = zipped; k < aRun.length; k++) rows.push({ kind: "removed", aHop: aRun[k] });
    for (let k = zipped; k < bRun.length; k++) rows.push({ kind: "added", bHop: bRun[k] });
    ai = aEnd;
    bi = bEnd;
  };

  for (const [x, y] of lcsAnchors(a, b)) {
    gap(x, y);
    rows.push({ kind: "same", aHop: a[x], bHop: b[y], rttDeltaNs: b[y].rttNs - a[x].rttNs });
    ai = x + 1;
    bi = y + 1;
  }
  gap(a.length, b.length);

  return rows;
}

/** fmtRttDeltaNs is fmtRttNs's signed twin: the sign is the whole message, so
 *  a slower hop reads "+2.0ms" and never "2.0ms". Zero keeps its unsigned form
 *  — "+0.0ms" would claim a direction the number does not have. */
export function fmtRttDeltaNs(ns: number | undefined): string {
  if (ns === undefined || Number.isNaN(ns)) return "—";
  const ms = ns / 1e6;
  return `${ms > 0 ? "+" : ""}${ms.toFixed(1)}ms`;
}

/* ── the table ──────────────────────────────────────────────────────────── */

const KIND_MARK: Record<DiffKind, string> = {
  same: "=",
  changed: "~",
  added: "+",
  removed: "−",
};

/* Colour is spent on trouble and on movement, the same posture the hop table
   takes: an unchanged hop stays muted rather than turning the table green. */
const KIND_CLASS: Record<DiffKind, string> = {
  same: "text-muted-foreground",
  changed: "text-health-warn",
  added: "text-health-ok",
  removed: "text-health-bad",
};

const KIND_TITLE: Record<DiffKind, string> = {
  same: "the same address in both paths",
  changed: "a different address at the same place in the route",
  added: "only the newer path visits this hop",
  removed: "only the older path visited this hop",
};

function HopCell({ hop }: { hop: MTRHop | undefined }) {
  if (!hop) return <td className="px-2 py-1.5 text-muted-foreground">—</td>;
  return (
    <td className="px-2 py-1.5">
      <span className="nums font-mono text-muted-foreground">{hop.number}</span>{" "}
      <span className="nums font-mono">{isPlaceholderHop(hop.ip) ? "*" : hop.ip}</span>{" "}
      <span className="nums text-muted-foreground">{fmtRttNs(hop.rttNs)}</span>
    </td>
  );
}

/** deltaClass reads the sign the way an operator does: slower is bad news,
 *  faster is good news, unchanged is not news. */
function deltaClass(ns: number | undefined): string {
  if (ns === undefined || ns === 0) return "text-muted-foreground";
  return ns > 0 ? "text-health-bad" : "text-health-ok";
}

function ColumnHeader({ label, snapshot }: { label: string; snapshot: PathSnapshot }) {
  return (
    <th scope="col" className="px-2 py-1.5 text-left font-medium">
      <span className="block text-[11px] uppercase tracking-wide text-muted-foreground">{label}</span>
      <span className="nums font-mono text-xs" title={snapshot.pathHash}>
        {shortHash(snapshot.pathHash)}
      </span>
      <span className="nums block text-[11px] text-muted-foreground">{fmtTime(snapshot.firstSeen)}</span>
    </th>
  );
}

/**
 * PathDiff is Decision 3's whole UI: two snapshot payloads the API already
 * returned, aligned client-side. `a` is the OLDER of the two and `b` the newer
 * — the caller sorts them by first_seen, and the column headers name both so
 * the direction is never a guess.
 *
 * An all-`same` diff renders as one sentence instead of a wall of "=" rows:
 * two snapshots of one pair have DISTINCT path hashes by construction
 * (mtr_path_snapshots_pair_hash is unique over source+destination+hash), so an
 * identical hop ORDER here means the two routes differ only in the RTTs
 * recorded on them — worth saying, not worth a table.
 */
export function PathDiff({ a, b }: { a: PathSnapshot; b: PathSnapshot }) {
  const rows = diffPaths(a.hops, b.hops);
  const identical = rows.length > 0 && rows.every((r) => r.kind === "same");

  return (
    <div className="mt-4 overflow-x-auto">
      {identical ? (
        <p className="mb-3 text-xs leading-relaxed text-muted-foreground">
          Both paths visit the same hops in the same order — only the recorded round-trip times differ.
        </p>
      ) : null}
      <table aria-label="Path diff" className="w-full min-w-md text-xs">
        <thead>
          <tr className="border-b border-border align-bottom">
            <th scope="col" className="w-6 px-2 py-1.5 text-left font-medium">
              <span className="sr-only">Change</span>
            </th>
            <ColumnHeader label="Older" snapshot={a} />
            <ColumnHeader label="Newer" snapshot={b} />
            <th scope="col" className="px-2 py-1.5 text-right font-medium">
              Δ RTT
            </th>
          </tr>
        </thead>
        <tbody className="divide-y divide-border">
          {rows.map((row, i) => (
            <tr key={i} className={row.kind === "same" ? undefined : "bg-surface-2/40"}>
              <td className="px-2 py-1.5">
                <span aria-label={row.kind} title={KIND_TITLE[row.kind]} className={cn("font-mono", KIND_CLASS[row.kind])}>
                  {KIND_MARK[row.kind]}
                </span>
              </td>
              <HopCell hop={row.aHop} />
              <HopCell hop={row.bHop} />
              <td className={cn("nums px-2 py-1.5 text-right", deltaClass(row.rttDeltaNs))}>
                {fmtRttDeltaNs(row.rttDeltaNs)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
