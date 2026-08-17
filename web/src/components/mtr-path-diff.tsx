import { fmtRttNs, fmtTime, isPlaceholderHop, shortHash } from "@/components/mtr-hop-table";
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
export function diffPaths(aHops: MTRHop[], bHops: MTRHop[]): DiffRow[] {
  /* Both sides are LISTS or they are nothing; a `null` hops field reaching the
     alignment threw out of the whole page (hostile-QA probe E). */
  const a = Array.isArray(aHops) ? aHops : [];
  const b = Array.isArray(bHops) ? bHops : [];
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
    /* Only a delta between two REAL readings is a delta; subtracting an absent
       RTT produced NaN, which fmtRttDeltaNs then had to print as an em dash
       anyway — this says the same thing one step earlier. */
    const both = Number.isFinite(b[y].rttNs) && Number.isFinite(a[x].rttNs);
    rows.push({ kind: "same", aHop: a[x], bHop: b[y], rttDeltaNs: both ? b[y].rttNs - a[x].rttNs : undefined });
    ai = x + 1;
    bi = y + 1;
  }
  gap(a.length, b.length);

  return rows;
}

/* ── the change, in words ────────────────────────────────────────────────── */

/** What one path-to-path change WAS, structured so the sentence stays in the dictionary. */
export type PathChangeKind = "same" | "changed" | "added" | "removed" | "several";

export interface PathChangeSummary {
  kind: PathChangeKind;
  /** 1-based hop position, present only when a single hop accounts for it. */
  hop?: number;
  from?: string;
  to?: string;
  /** How many hops moved in all. */
  total: number;
}

/**
 * summarisePathChange turns two hop lists into a sentence a reader can act on.
 *
 * The Explorer badged a row "path changed" and put two twelve-character hashes
 * beside it, which says that something moved and nothing about what (owner:
 * «ничего не понятно»). An MTR exists to show a ROUTE, so a route change is
 * described as one: which hop, from what, to what.
 *
 * It reads diffPaths rather than re-deriving an alignment, so the words and the
 * diff table can never disagree about what happened. RTT is deliberately not a
 * change: the hash is over the hop ADDRESSES, and a slower hop is the same hop.
 */
export function summarisePathChange(a: MTRHop[], b: MTRHop[]): PathChangeSummary {
  const moved = diffPaths(a, b).filter((r) => {
    if (r.kind === "same") return false;
    /* A hop that did not answer is an ABSENCE, not an address, so diffPaths
       refuses to anchor on it (correctly — two stars are not evidence of the
       same router). For a SENTENCE, though, "hop 2: * → *" describes no
       observable change and is exactly the noise this function exists to
       replace. The diff table keeps its own, stricter reading. */
    const bothSilent =
      r.aHop !== undefined &&
      r.bHop !== undefined &&
      isPlaceholderHop(r.aHop.ip) &&
      isPlaceholderHop(r.bHop.ip);
    return !bothSilent;
  });
  if (moved.length === 0) return { kind: "same", total: 0 };
  if (moved.length > 1) return { kind: "several", total: moved.length };

  const [row] = moved;
  const hop = row.bHop?.number ?? row.aHop?.number;
  if (row.kind === "changed") {
    return { kind: "changed", hop, from: row.aHop?.ip, to: row.bHop?.ip, total: 1 };
  }
  if (row.kind === "added") return { kind: "added", hop, to: row.bHop?.ip, total: 1 };
  return { kind: "removed", hop, from: row.aHop?.ip, total: 1 };
}

/** fmtRttDeltaNs is fmtRttNs's signed twin: the sign is the whole message, so
 *  a slower hop reads "+2.0ms" and never "2.0ms". Zero keeps its unsigned form
 *  — "+0.0ms" would claim a direction the number does not have. */
export function fmtRttDeltaNs(ns: number | undefined): string {
  if (typeof ns !== "number" || !Number.isFinite(ns)) return "—";
  const shown = (ns / 1e6).toFixed(1);
  /* A delta of a few microseconds ROUNDS to nothing, and "-0.0ms" is the same
     claim about a direction that "+0.0" was already refused for (hostile-QA
     probe N). Read the sign off the number that will actually be printed. */
  if (Number(shown) === 0) return "0.0ms";
  return `${ns > 0 ? "+" : ""}${shown}ms`;
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
  if (!hop) return <td className="px-1.5 py-1.5 text-muted-foreground">—</td>;
  return (
    /* Address over RTT rather than beside it: the two columns have to sit side
       by side inside the narrowest pane on the page, and a single line of
       "2 10.244.9.17 1.2ms" twice over is what used to push the newer path off
       the right edge. The address is the thing being compared, so it leads. */
    <td className="px-1.5 py-1.5 align-top">
      <span className="flex items-baseline gap-1">
        <span className="nums font-mono text-[10px] text-muted-foreground">{hop.number}</span>
        <span className="nums min-w-0 break-all font-mono">{isPlaceholderHop(hop.ip) ? "*" : hop.ip}</span>
      </span>
      <span className="nums block text-[10px] text-muted-foreground">{fmtRttNs(hop.rttNs)}</span>
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
    <th scope="col" className="px-1.5 py-1.5 text-left font-medium">
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
  /* diffPaths tolerates a hops field that is not a list; the caller — pages/
     mtr.tsx's DiffPane — is the one that catches the pair with nothing to
     align at all and says so in words instead of drawing an empty table. */
  const rows = diffPaths(a.hops, b.hops);
  const identical = rows.length > 0 && rows.every((r) => r.kind === "same");

  return (
    /* The four-column diff has a min-width and lives in the narrowest pane, so
       it is the likeliest table in the console to run off its card — same
       affordance as the hop table (QA scope 4, finding #6). */
    <div className="mt-4">
      {identical ? (
        <p className="mb-3 text-xs leading-relaxed text-muted-foreground">{t("diff.identical")}</p>
      ) : null}
      {/* The KEY the marks always needed. `~`, `+` and `−` carried a title and
          an aria-label, which is to say they were legible to a screen reader and
          to nobody looking at the screen (owner: «ничего не понятно»). */}
      <ul aria-label={t("diff.legend")} className="mb-2 flex flex-wrap gap-x-4 gap-y-1 text-[11px]">
        {(["changed", "added", "removed"] as const).map((kind) => (
          <li key={kind} className="flex items-center gap-1.5 text-muted-foreground">
            <span aria-hidden="true" className={cn("font-mono", KIND_CLASS[kind])}>{KIND_MARK[kind]}</span>
            {t(KIND_KEY[kind])}
          </li>
        ))}
      </ul>
      <table aria-label={t("diff.aria")} className="w-full text-xs">
        <thead>
          <tr className="border-b border-border align-bottom">
            <th scope="col" className="w-5 px-1.5 py-1.5 text-left font-medium">
              <span className="sr-only">{t("diff.change")}</span>
            </th>
            <ColumnHeader label={t("diff.older")} snapshot={a} />
            <ColumnHeader label={t("diff.newer")} snapshot={b} />
            <th scope="col" className="px-1.5 py-1.5 text-right font-medium">
              Δ RTT
            </th>
          </tr>
        </thead>
        <tbody className="divide-y divide-border">
          {rows.map((row, i) => (
            <tr key={i} className={row.kind === "same" ? undefined : "bg-surface-2/40"}>
              <td className="px-1.5 py-1.5 align-top">
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
              <td className={cn("nums px-1.5 py-1.5 align-top text-right", deltaClass(row.rttDeltaNs))}>
                {fmtRttDeltaNs(row.rttDeltaNs)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
