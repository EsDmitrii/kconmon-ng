import { useMemo, useState } from "react";
import { ChevronRight } from "lucide-react";
import type { EChartsOption, LineSeriesOption } from "echarts";
import { EChart } from "@/components/echart";
import { useTheme } from "@/components/theme-provider";
import { chartColors, seriesColor } from "@/lib/chart-theme";
import type { Enrichment, MTRHop, PathSnapshot } from "@/lib/types";
import { cn } from "@/lib/utils";

/* ── pure helpers (exported for their own tests) ────────────────────────── */

/** fmtRttNs renders the repo's nanosecond wire convention in the milliseconds
 *  an operator reads a traceroute in. One decimal: sub-millisecond hops inside
 *  a rack are real and "0ms" would erase them. */
export function fmtRttNs(ns: number | undefined): string {
  if (ns === undefined || Number.isNaN(ns)) return "—";
  return `${(ns / 1e6).toFixed(1)}ms`;
}

/**
 * isPlaceholderHop is "this row has no address to talk about". The tracer
 * writes "*" for a hop that never answered (internal/checker/mtr.go) and any
 * other producer may leave it empty; either way there is nothing to reverse-
 * resolve, nothing to geolocate and nothing to trend, so the row gets neither
 * an expander nor a trend toggle. Rendering a chevron that can only ever open
 * onto "nothing known" would be a control that lies about having content.
 */
export function isPlaceholderHop(ip: string): boolean {
  const trimmed = ip.trim();
  return trimmed === "" || trimmed === "*";
}

/** shortHash is a snapshot's identity in twelve characters. A full SHA-256 hex
 *  digest is 64 characters of wall; twelve is plenty to tell two routes of one
 *  pair apart by eye, and the full value stays in the title attribute wherever
 *  this is rendered.
 *
 *  It lives here rather than in pages/mtr.tsx (where Task 6 first wrote it, and
 *  from where it is still re-exported) because Task 8's diff table and changes
 *  timeline label snapshots with it too, and a component importing its page
 *  back is a cycle waiting to bite. */
export function shortHash(hash: string): string {
  return hash.slice(0, 12);
}

export type LossTier = "ok" | "warn" | "bad";

/** lossTier reuses pages/matrix.tsx's tierOf thresholds verbatim — under 1% is
 *  healthy, 1–10% degraded, 10%+ failing. One console, one meaning for a loss
 *  number: a hop the matrix would call degraded must not read as fine here. */
export function lossTier(ratio: number): LossTier {
  if (ratio < 0.01) return "ok";
  if (ratio < 0.1) return "warn";
  return "bad";
}

/* Colour is spent on trouble, matrix-style: a clean hop stays muted rather
   than turning the whole column green. Tailwind only sees literal class names,
   hence the map. */
const LOSS_CLASS: Record<LossTier, string> = {
  ok: "text-muted-foreground",
  warn: "text-health-warn",
  bad: "text-health-bad",
};

/**
 * TrendHistory is what the trend chart is allowed to know: the snapshots the
 * history pane has actually FETCHED (newest first, as the API answers), whether
 * the cursor says there are older ones, and the pair's total trace count from
 * the destinations list (null when unknown). The last two exist so the chart
 * can say what it is NOT showing — a trend drawn from three of forty traces is
 * not wrong, but presenting it as the pair's history would be.
 */
export interface TrendHistory {
  snapshots: PathSnapshot[];
  hasOlder: boolean;
  traceTotal: number | null;
}

/**
 * hopTrendSeries is Decision 13 in one function: hop RTTs never became
 * Prometheus metrics (hop_ip cardinality was refused in M4), so the trend is
 * read off the snapshots the browser already holds.
 *
 * Two rules carry the honesty of the chart:
 *
 *  - The list arrives newest-first and a time axis reads oldest-first, so it is
 *    reversed here rather than in the option builder.
 *  - A snapshot whose route does NOT contain the address yields `null`, not 0.
 *    The hop was not measured slow, it was not on the path at all; a zero would
 *    draw a plunge to the axis and read as an outage. ECharts leaves a null
 *    unpainted, which is exactly the gap the route change actually was.
 *
 * A snapshot whose firstSeen cannot be parsed is dropped rather than plotted at
 * an invented instant — there is no honest x for it.
 */
export function hopTrendSeries(snapshots: PathSnapshot[], ip: string): [number, number | null][] {
  const points: [number, number | null][] = [];
  for (let i = snapshots.length - 1; i >= 0; i--) {
    const snapshot = snapshots[i];
    const at = new Date(snapshot.firstSeen).getTime();
    if (Number.isNaN(at)) continue;
    const rttNs = snapshot.hops.find((h) => h.ip === ip)?.rttNs;
    points.push([at, rttNs === undefined || Number.isNaN(rttNs) ? null : rttNs / 1e6]);
  }
  return points;
}

/** loadedTraces is how much of the pair's recorded traffic the loaded
 *  snapshots speak for: each snapshot row carries the number of traces that
 *  took that path, and the destinations list carries the pair's total. */
function loadedTraces(snapshots: PathSnapshot[]): number {
  return snapshots.reduce((sum, s) => sum + s.traceCount, 0);
}

/** historyIsPartial: the cursor says there is more, or the loaded snapshots
 *  account for fewer traces than the pair has run. Either way the trend is a
 *  window, and the note under the chart says so. */
export function historyIsPartial(history: TrendHistory): boolean {
  if (history.hasOlder) return true;
  return history.traceTotal !== null && loadedTraces(history.snapshots) < history.traceTotal;
}

/* ── enrichment ─────────────────────────────────────────────────────────── */

interface Fact {
  label: string;
  value: string;
}

/** geoText renders the resolver's four-field geo object (enrich.go: country
 *  ISO code, city, lat, lon) as the one line a hop table has room for. The
 *  field is typed as an open JSON object on the wire, so each value is checked
 *  rather than trusted. */
function geoText(geo: Record<string, unknown> | undefined): string {
  const city = typeof geo?.city === "string" ? geo.city : "";
  const country = typeof geo?.country === "string" ? geo.country : "";
  return [city, country].filter(Boolean).join(", ");
}

/**
 * enrichmentFacts is the expanded row's content, and an EMPTY list is the
 * signal that the row has nothing to say. A cache miss is an absent key and a
 * partially-known address is a row with holes, so "what do we actually know"
 * is a filter, never a fixed set of dashed fields.
 */
export function enrichmentFacts(entry: Enrichment | undefined): Fact[] {
  if (!entry) return [];
  const facts: Fact[] = [];
  if (entry.rdns) facts.push({ label: "Reverse DNS", value: entry.rdns });
  const network = [entry.asn ? `AS${entry.asn}` : "", entry.provider ?? ""].filter(Boolean).join(" · ");
  if (network) facts.push({ label: "Network", value: network });
  const where = geoText(entry.geo);
  if (where) facts.push({ label: "Location", value: where });
  return facts;
}

/**
 * EnrichmentDetail is the expanded row's body.
 *
 * The note is worded the way target-card.tsx's History tab words an empty
 * series, and for the same reason: the API cannot tell "enrichment is switched
 * off on this console" apart from "it is on and nothing is known about this
 * address" — GET /api/v1/mtr/snapshots/{id}?enrich=true answers a possibly
 * empty map in both cases, and GET /api/v1/config exposes no enrichment
 * capability to disambiguate with. So the note covers both rather than
 * asserting a cause, and names the knob an operator would go and check.
 */
function EnrichmentDetail({ entry }: { entry: Enrichment | undefined }) {
  const facts = enrichmentFacts(entry);
  if (facts.length === 0) {
    return (
      <p className="max-w-prose text-xs leading-relaxed text-muted-foreground">
        No enrichment recorded for this address — enrichment may be disabled on this console
        (console.mtr.enrichment.enabled, off by default), or it is on and no source knows this address.
      </p>
    );
  }
  return (
    <dl className="flex flex-wrap gap-x-8 gap-y-1 text-xs">
      {facts.map((fact) => (
        <div key={fact.label}>
          <dt className="text-muted-foreground">{fact.label}</dt>
          <dd className="mt-0.5">{fact.value}</dd>
        </div>
      ))}
    </dl>
  );
}

/* ── the trend chart ────────────────────────────────────────────────────── */

function trendOption(points: [number, number | null][], ip: string, dark: boolean): EChartsOption {
  const colors = chartColors(dark ? "dark" : "light");
  const fmt = (ms: number) => `${ms.toFixed(1)}ms`;
  return {
    animation: false,
    textStyle: { color: colors.axis },
    grid: { left: 56, right: 16, top: 12, bottom: 28 },
    tooltip: {
      trigger: "axis",
      axisPointer: { type: "line", lineStyle: { color: colors.grid } },
      valueFormatter: (value) => fmt(Number(value)),
    },
    xAxis: {
      type: "time",
      axisLine: { lineStyle: { color: colors.grid } },
      axisLabel: { color: colors.axis },
      splitLine: { show: false },
    },
    yAxis: {
      type: "value",
      axisLabel: { color: colors.axis, formatter: (value: number) => fmt(value) },
      splitLine: { lineStyle: { color: colors.grid } },
    },
    series: [
      {
        name: ip,
        type: "line",
        // Symbols stay ON, unlike the dense Prometheus charts: this series has
        // one point per stored PATH, so a pair with three snapshots would
        // otherwise be an invisible line, and the points either side of a gap
        // are the whole story of a route change.
        showSymbol: true,
        symbolSize: 6,
        // The load-bearing line: a null is a route the hop was not on, and
        // bridging it would draw a measurement that never happened.
        connectNulls: false,
        smooth: false,
        color: seriesColor(colors, 0),
        lineStyle: { width: 2 },
        data: points as LineSeriesOption["data"],
      },
    ],
  };
}

function HopTrend({ ip, history }: { ip: string; history: TrendHistory }) {
  const { theme } = useTheme();
  const points = useMemo(() => hopTrendSeries(history.snapshots, ip), [history.snapshots, ip]);
  const option = useMemo(() => trendOption(points, ip, theme === "dark"), [points, ip, theme]);
  const measured = points.filter(([, v]) => v !== null).length;
  const partial = historyIsPartial(history);
  const traces = loadedTraces(history.snapshots);

  return (
    <section aria-label={`RTT trend for ${ip}`} className="mt-4 rounded-md bg-surface-2/50 p-3">
      <h4 className="text-xs font-medium">RTT trend for {ip}</h4>
      {measured === 0 ? (
        <p className="mt-2 text-xs leading-relaxed text-muted-foreground">
          None of the loaded paths carries an RTT for this address.
        </p>
      ) : (
        <EChart option={option} className="mt-2 h-48 w-full" />
      )}
      {/* What the chart is NOT showing. The counts are the pair's own numbers —
          snapshots fetched so far, and the traces they account for out of the
          destinations list's total — so the reader can judge the window rather
          than trust it. */}
      {partial ? (
        <p className="mt-2 text-xs leading-relaxed text-muted-foreground">
          Trend covers the {history.snapshots.length} paths loaded here
          {history.traceTotal !== null && traces < history.traceTotal
            ? ` (${traces} of the pair's ${history.traceTotal} traces)`
            : ""}{" "}
          — use Load older in the path history to widen it.
        </p>
      ) : null}
    </section>
  );
}

/* ── the hop table ──────────────────────────────────────────────────────── */

/**
 * TraceDetail renders ONE stored path: the snapshot's own header numbers, its
 * hop table, an expandable enrichment row per addressable hop, and — on
 * demand — that hop's RTT across the paths the history pane has loaded.
 *
 * The wire truth the table is honest about: a snapshot carries the payload of
 * the FIRST trace that took the path, i.e. a SINGLE sample per hop (`rttNs`
 * plus `lossRatio`). MTR_EXPLORER.md's `avg / best / worst / jitter` columns
 * have nothing feeding them per snapshot, so they are not drawn — a row of
 * dashes under four invented headers would claim the data exists and is
 * missing, which is worse than a table that shows what was stored. The
 * across-snapshots dimension the spec wants is the trend chart, which is real.
 *
 * `history` is optional: without it the table renders exactly as before and
 * offers no trend toggle, which keeps the component usable anywhere a single
 * snapshot is in hand.
 */
export function TraceDetail({ snapshot, history }: { snapshot: PathSnapshot; history?: TrendHistory }) {
  // Keyed by hop NUMBER, not by array index or IP: a path may legitimately
  // repeat an address (a routing loop) and the number is the row's identity.
  const [expanded, setExpanded] = useState<number | null>(null);
  const [trendHop, setTrendHop] = useState<number | null>(null);

  const toggle = (set: (n: number | null) => void, current: number | null, n: number) =>
    set(current === n ? null : n);

  return (
    <div className="mt-3">
      <dl className="flex flex-wrap gap-x-6 gap-y-1 text-xs text-muted-foreground">
        <div>
          <dt className="inline">First seen </dt>
          <dd className="nums inline text-foreground">{fmtTime(snapshot.firstSeen)}</dd>
        </div>
        <div>
          <dt className="inline">Last seen </dt>
          <dd className="nums inline text-foreground">{fmtTime(snapshot.lastSeen)}</dd>
        </div>
        <div>
          <dt className="inline">Traces </dt>
          <dd className="nums inline text-foreground">{snapshot.traceCount}</dd>
        </div>
      </dl>

      <div className="mt-4 overflow-x-auto">
        <table aria-label="Hops" className="w-full text-sm">
          <thead>
            <tr className="border-b border-border text-xs text-muted-foreground">
              <th scope="col" className="w-6 py-2">
                <span className="sr-only">Expand</span>
              </th>
              <th scope="col" className="py-2 pr-3 text-left font-medium">
                #
              </th>
              <th scope="col" className="py-2 pr-3 text-left font-medium">
                Address
              </th>
              <th scope="col" className="py-2 pr-3 text-left font-medium">
                Hostname
              </th>
              <th scope="col" className="py-2 pr-3 text-right font-medium">
                RTT
              </th>
              <th scope="col" className="py-2 text-right font-medium">
                Loss
              </th>
            </tr>
          </thead>
          <tbody className="divide-y divide-border">
            {snapshot.hops.map((h) => (
              <HopRows
                key={`${h.number}-${h.ip}`}
                hop={h}
                enrichment={snapshot.enrichment?.[h.ip]}
                expanded={expanded === h.number}
                onToggleExpanded={() => toggle(setExpanded, expanded, h.number)}
                trending={trendHop === h.number}
                onToggleTrend={history ? () => toggle(setTrendHop, trendHop, h.number) : undefined}
              />
            ))}
          </tbody>
        </table>
      </div>

      {history && trendHop !== null ? (
        <HopTrend ip={snapshot.hops.find((h) => h.number === trendHop)?.ip ?? ""} history={history} />
      ) : null}
    </div>
  );
}

function HopRows({
  hop,
  enrichment,
  expanded,
  onToggleExpanded,
  trending,
  onToggleTrend,
}: {
  hop: MTRHop;
  enrichment: Enrichment | undefined;
  expanded: boolean;
  onToggleExpanded: () => void;
  trending: boolean;
  onToggleTrend?: () => void;
}) {
  const placeholder = isPlaceholderHop(hop.ip);
  const detailId = `hop-${hop.number}-detail`;
  const tier = lossTier(hop.lossRatio);

  return (
    <>
      <tr>
        <td className="py-2 align-top">
          {placeholder ? null : (
            <button
              type="button"
              aria-expanded={expanded}
              aria-controls={detailId}
              aria-label={`Enrichment for hop ${hop.number}, ${hop.ip}`}
              onClick={onToggleExpanded}
              className={cn(
                "flex size-5 items-center justify-center rounded text-muted-foreground",
                "transition-colors duration-(--dur-fast) ease-(--ease) hover:text-foreground",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
              )}
            >
              <ChevronRight
                aria-hidden="true"
                className={cn("size-3.5 transition-transform duration-(--dur-fast) ease-(--ease)", expanded && "rotate-90")}
              />
            </button>
          )}
        </td>
        <td className="nums py-2 pr-3 align-top text-muted-foreground">{hop.number}</td>
        <td className="py-2 pr-3 align-top font-mono text-xs">{hop.ip}</td>
        <td className="truncate py-2 pr-3 align-top text-xs text-muted-foreground">{hop.hostname || "—"}</td>
        <td className="nums py-2 pr-3 text-right align-top">
          {/* The RTT cell IS the trend affordance (Decision 13): the number the
              reader is looking at is the one the chart puts in time. Without a
              loaded history there is nothing to plot, so it stays plain text
              rather than becoming a button that answers nothing. */}
          {onToggleTrend && !placeholder ? (
            <button
              type="button"
              aria-pressed={trending}
              aria-label={`RTT trend for ${hop.ip}`}
              onClick={onToggleTrend}
              className={cn(
                "nums rounded px-1 underline decoration-dotted underline-offset-4",
                "transition-colors duration-(--dur-fast) ease-(--ease) hover:text-primary",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                trending && "text-primary",
              )}
            >
              {fmtRttNs(hop.rttNs)}
            </button>
          ) : (
            fmtRttNs(hop.rttNs)
          )}
        </td>
        <td className={cn("nums py-2 text-right align-top", LOSS_CLASS[tier])}>
          {(hop.lossRatio * 100).toFixed(0)}%
        </td>
      </tr>
      {expanded && !placeholder ? (
        <tr>
          <td id={detailId} colSpan={6} className="bg-surface-2/40 px-2 py-3">
            <EnrichmentDetail entry={enrichment} />
          </td>
        </tr>
      ) : null}
    </>
  );
}

/** fmtTime renders a wire timestamp in the reader's own locale and timezone.
 *  Exported (M5 Task 8) because the diff table and the changes timeline label
 *  snapshots exactly the way the hop table's footer already does, and three
 *  copies of one four-line formatter is how "8:00" and "08:00" end up on the
 *  same screen. */
export function fmtTime(ts?: string | null): string {
  if (!ts) return "—";
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? ts : d.toLocaleString();
}
