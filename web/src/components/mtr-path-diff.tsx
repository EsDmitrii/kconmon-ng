import { fmtRttNs, fmtTime, isPlaceholderHop, ScrollableX, shortHash } from "@/components/mtr-hop-table";
import { useLocale, useT, type Translate } from "@/lib/i18n";
import { mtrDetailDict, type MTRDetailKey } from "@/lib/i18n/dict/mtr-detail";
import type { MTRHop, PathSnapshot } from "@/lib/types";
import { cn } from "@/lib/utils";

/* ── the diff, as a pure function ───────────────────────────────────────── */

export type DiffKind = "same" | "changed" | "added" | "removed";

/**
 * DiffRow is one line of the aligned two-column table: what the OLDER path had there (`aHop`);
 * `rttDeltaNs` is present ONLY on `same` rows — it is `bHop.rttNs - aHop.rttNs`.
 */
export interface DiffRow {
  kind: DiffKind;
  aHop?: MTRHop;
  bHop?: MTRHop;
  rttDeltaNs?: number;
}

/** anchorable is the equality the alignment is built on: the hop ADDRESS, and only for hops that have one. */
function anchorable(x: MTRHop, y: MTRHop): boolean {
  return !isPlaceholderHop(x.ip) && x.ip === y.ip;
}

/** lcsAnchors is the alignment, stated honestly: a longest common subsequence over hop IPs. */
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

/** diffPaths aligns two snapshots' hop lists and says what happened between them. */
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

/* DiffKind is a TYPE before it is a word — the same call dict/palette.ts made for CommandGroup. */
const KIND_KEY: Record<DiffKind, MTRDetailKey> = {
  same: "diff.kind.same",
  changed: "diff.kind.changed",
  added: "diff.kind.added",
  removed: "diff.kind.removed",
};

const KIND_TITLE_KEY: Record<DiffKind, MTRDetailKey> = {
  same: "diff.kind.same.title",
  changed: "diff.kind.changed.title",
  added: "diff.kind.added.title",
  removed: "diff.kind.removed.title",
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
  const { locale } = useLocale();
  return (
    <th scope="col" className="px-2 py-1.5 text-left font-medium">
      <span className="block text-[11px] uppercase tracking-wide text-muted-foreground">{label}</span>
      <span className="nums font-mono text-xs" title={snapshot.pathHash}>
        {shortHash(snapshot.pathHash)}
      </span>
      <span className="nums block text-[11px] text-muted-foreground">{fmtTime(snapshot.firstSeen, locale)}</span>
    </th>
  );
}

/**
 * PathDiff is 's whole UI: two snapshot payloads the API already returned, aligned client-side; `a`
 * is the OLDER of the two and `b` the newer — the caller sorts them by first_seen.
 */
export function PathDiff({ a, b }: { a: PathSnapshot; b: PathSnapshot }) {
  const t: Translate<MTRDetailKey> = useT(mtrDetailDict);
  const rows = diffPaths(a.hops, b.hops);
  const identical = rows.length > 0 && rows.every((r) => r.kind === "same");

  return (
    /* The four-column diff has a min-width and lives in the narrowest pane, so
       it is the likeliest table in the console to run off its card — same
       affordance as the hop table (QA scope 4, finding #6). */
    <ScrollableX className="mt-4">
      {identical ? (
        <p className="mb-3 text-xs leading-relaxed text-muted-foreground">{t("diff.identical")}</p>
      ) : null}
      <table aria-label={t("diff.aria")} className="w-full min-w-md text-xs">
        <thead>
          <tr className="border-b border-border align-bottom">
            <th scope="col" className="w-6 px-2 py-1.5 text-left font-medium">
              <span className="sr-only">{t("diff.change")}</span>
            </th>
            <ColumnHeader label={t("diff.older")} snapshot={a} />
            <ColumnHeader label={t("diff.newer")} snapshot={b} />
            <th scope="col" className="px-2 py-1.5 text-right font-medium">
              Δ RTT
            </th>
          </tr>
        </thead>
        <tbody className="divide-y divide-border">
          {rows.map((row, i) => (
            <tr key={i} className={row.kind === "same" ? undefined : "bg-surface-2/40"}>
              <td className="px-2 py-1.5">
                <span
                  aria-label={t(KIND_KEY[row.kind])}
                  title={t(KIND_TITLE_KEY[row.kind])}
                  className={cn("font-mono", KIND_CLASS[row.kind])}
                >
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
    </ScrollableX>
  );
}
