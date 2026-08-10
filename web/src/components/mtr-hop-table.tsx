import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { ChevronRight } from "lucide-react";
import type { EChartsOption, LineSeriesOption } from "echarts";
import { EChart } from "@/components/echart";
import { useTheme } from "@/components/theme-provider";
import { chartColors, seriesColor } from "@/lib/chart-theme";
import { formatMillis } from "@/lib/curated-metrics";
import { stampFull, translate, useLocale, useT, type Locale, type Translate } from "@/lib/i18n";
import { countForm, mtrDetailDict, type MTRDetailKey } from "@/lib/i18n/dict/mtr-detail";
import type { Enrichment, MTRHop, PathSnapshot } from "@/lib/types";
import { cn } from "@/lib/utils";

/* ── pure helpers (exported for their own tests) ────────────────────────── */

/** enT is the ENGLISH translator the one PURE, exported helper below defaults to. */
const enT: Translate<MTRDetailKey> = (key, vars) => translate(mtrDetailDict, "en", key, vars);

/* Under this, milliseconds have run out of resolution: 22µs is 0.022ms, and
   one decimal renders it "0.0ms" — a first hop inside the same node reported as
   no time at all (QA scope 4, finding #14). */
const RTT_MICROSECOND_FLOOR_NS = 100_000;

/** fmtRttNs renders the repo's nanosecond wire convention in the unit that can
 *  actually hold the number: milliseconds with one decimal down to 0.1ms, and
 *  MICROSECONDS below that, because a same-node hop really does answer in tens
 *  of µs and rounding it to "0.0ms" erases the measurement rather than
 *  reporting it. */
export function fmtRttNs(ns: number | undefined): string {
  if (ns === undefined || Number.isNaN(ns)) return "—";
  if (ns > 0 && ns < RTT_MICROSECOND_FLOOR_NS) return `${Math.round(ns / 1e3)}µs`;
  return `${(ns / 1e6).toFixed(1)}ms`;
}

/**
 * isPlaceholderHop is "this row has no address to talk about"; the tracer writes "*" for a hop that
 * never answered (internal/checker/mtr.go) and any other producer may leave it empty.
 */
export function isPlaceholderHop(ip: string): boolean {
  const trimmed = ip.trim();
  return trimmed === "" || trimmed === "*";
}

/**
 * shortHash is a snapshot's identity in twelve characters; it lives here rather than in
 * pages/mtr.tsx because the diff table and changes timeline label snapshots with it too.
 */
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
 * TrendHistory is what the trend chart is allowed to know; the last two exist so the chart can say
 * what it is NOT showing.
 */
export interface TrendHistory {
  snapshots: PathSnapshot[];
  hasOlder: boolean;
  traceTotal: number | null;
}

/** hopTrendSeries is in one function: hop RTTs never became Prometheus metrics. */
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

/** Either way the trend is a window, and the note under the chart says so. */
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
 * enrichmentFacts is the expanded row's content; a cache miss is an absent key and a
 * partially-known address is a row with holes.
 */
export function enrichmentFacts(entry: Enrichment | undefined, t: Translate<MTRDetailKey> = enT): Fact[] {
  if (!entry) return [];
  const facts: Fact[] = [];
  if (entry.rdns) facts.push({ label: t("enrichment.rdns"), value: entry.rdns });
  const network = [entry.asn ? `AS${entry.asn}` : "", entry.provider ?? ""].filter(Boolean).join(" · ");
  if (network) facts.push({ label: t("enrichment.network"), value: network });
  const where = geoText(entry.geo);
  if (where) facts.push({ label: t("enrichment.location"), value: where });
  return facts;
}

/**
 * EnrichmentDetail is the expanded row's body; the note is worded the way target-card.tsx's History
 * tab words an empty series.
 */
function EnrichmentDetail({ entry }: { entry: Enrichment | undefined }) {
  const t = useT(mtrDetailDict);
  const facts = enrichmentFacts(entry, t);
  if (facts.length === 0) {
    return (
      <p className="max-w-prose text-xs leading-relaxed text-muted-foreground">{t("enrichment.empty")}</p>
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

/**
 * SINGLE_POINT_PAD_MS is the window a lone sample gets: an hour either side; a time axis whose min
 * equals its max is degenerate.
 */
const SINGLE_POINT_PAD_MS = 60 * 60 * 1000;
/** How much air a multi-point extent gets at each end, as a fraction of its
 *  own span: enough that the first and last symbol are not drawn half over the
 *  axis line. */
const EXTENT_PAD_RATIO = 0.05;

/** trendExtent pins the trend chart's x-axis; pinning min/max to the data's own extent. */
export function trendExtent(points: [number, number | null][]): { min: number; max: number } | undefined {
  const xs = points.filter(([, v]) => v !== null).map(([ts]) => ts);
  if (xs.length === 0) return undefined;
  const lo = Math.min(...xs);
  const hi = Math.max(...xs);
  if (lo === hi) return { min: lo - SINGLE_POINT_PAD_MS, max: hi + SINGLE_POINT_PAD_MS };
  const pad = (hi - lo) * EXTENT_PAD_RATIO;
  return { min: lo - pad, max: hi + pad };
}

function trendOption(points: [number, number | null][], ip: string, dark: boolean): EChartsOption {
  const colors = chartColors(dark ? "dark" : "light");
  // The console's ONE millisecond rule.
  const fmt = formatMillis;
  const extent = trendExtent(points);
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
      axisLabel: { color: colors.axis, hideOverlap: true },
      splitLine: { show: false },
      ...(extent ? { min: extent.min, max: extent.max } : {}),
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
        // Symbols stay ON, unlike the dense Prometheus charts: this series has one point per stored
        // PATH.
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
  const t = useT(mtrDetailDict);
  const { locale } = useLocale();
  const { theme } = useTheme();
  const points = useMemo(() => hopTrendSeries(history.snapshots, ip), [history.snapshots, ip]);
  const option = useMemo(() => trendOption(points, ip, theme === "dark"), [points, ip, theme]);
  const measured = points.filter(([, v]) => v !== null).length;
  const partial = historyIsPartial(history);
  const traces = loadedTraces(history.snapshots);

  return (
    <section aria-label={t("trend.aria", { ip })} className="mt-4 rounded-md bg-surface-2/50 p-3">
      <h4 className="text-xs font-medium">{t("trend.title", { ip })}</h4>
      {measured === 0 ? (
        <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{t("trend.noRtt")}</p>
      ) : (
        <EChart option={option} className="mt-2 h-48 w-full" />
      )}
      {/* What the chart is NOT showing. The counts are the pair's own numbers —
          snapshots fetched so far, and the traces they account for out of the
          destinations list's total — so the reader can judge the window rather
          than trust it. */}
      {partial ? (
        <p className="mt-2 text-xs leading-relaxed text-muted-foreground">
          {t("trend.partial", {
            paths: t(`trend.paths.${countForm(locale, history.snapshots.length)}` as MTRDetailKey, {
              count: history.snapshots.length,
            }),
            traces:
              history.traceTotal !== null && traces < history.traceTotal
                ? t("trend.partial.traces", {
                    loaded: traces,
                    total: t(`trend.traces.${countForm(locale, history.traceTotal)}` as MTRDetailKey, {
                      count: history.traceTotal,
                    }),
                  })
                : "",
          })}
        </p>
      ) : null}
    </section>
  );
}

/* ── horizontal overflow ────────────────────────────────────────────────── */

/**
 * ScrollableX wraps a table that can still be wider than the card holding it. The card already
 * clipped its overflow silently: RTT and Loss simply were not there, with nothing saying they had
 * gone anywhere (QA scope 4, finding #6). Both halves of the fix matter — the affordance is drawn
 * ONLY while something is actually off to the right, because an edge fade that is always there is
 * decoration and teaches the reader to ignore it.
 */
export function ScrollableX({ children, className }: { children: ReactNode; className?: string }) {
  const t = useT(mtrDetailDict);
  const ref = useRef<HTMLDivElement>(null);
  const [more, setMore] = useState(false);

  const measure = useCallback(() => {
    const el = ref.current;
    if (!el) return;
    setMore(el.scrollWidth - el.clientWidth - el.scrollLeft > 1);
  }, []);

  /* Deliberately dep-less: the answer depends on laid-out width, which changes
     when the CONTENT changes as well as when the window does, and React bails
     out of a setState that lands on the same value — so re-measuring every
     render converges instead of looping. */
  useEffect(() => {
    measure();
    window.addEventListener("resize", measure);
    return () => window.removeEventListener("resize", measure);
  });

  return (
    <div className={cn("relative", className)}>
      <div ref={ref} onScroll={measure} className="overflow-x-auto">
        {children}
      </div>
      {more ? (
        <>
          <span
            aria-hidden="true"
            className="pointer-events-none absolute right-0 top-0 h-full w-10 bg-gradient-to-l from-card to-transparent"
          />
          <p role="note" className="mt-1 text-[11px] text-muted-foreground">
            {t("table.scrollHint")}
          </p>
        </>
      ) : null}
    </div>
  );
}

/* ── the hop table ──────────────────────────────────────────────────────── */

/**
 * TraceDetail renders ONE stored path: the snapshot's own header numbers; MTR_EXPLORER.md's `avg /
 * best / worst / jitter` columns have nothing feeding them per snapshot.
 */
export function TraceDetail({ snapshot, history }: { snapshot: PathSnapshot; history?: TrendHistory }) {
  const t = useT(mtrDetailDict);
  const { locale } = useLocale();
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
          <dt className="inline">{t("snapshot.firstSeen")} </dt>
          <dd className="nums inline text-foreground">{fmtTime(snapshot.firstSeen, locale)}</dd>
        </div>
        <div>
          <dt className="inline">{t("snapshot.lastSeen")} </dt>
          <dd className="nums inline text-foreground">{fmtTime(snapshot.lastSeen, locale)}</dd>
        </div>
        <div>
          <dt className="inline">{t("snapshot.traces")} </dt>
          <dd className="nums inline text-foreground">{snapshot.traceCount}</dd>
        </div>
      </dl>

      <ScrollableX className="mt-4">
        <table aria-label={t("table.aria")} className="w-full text-sm">
          <thead>
            <tr className="border-b border-border text-xs text-muted-foreground">
              <th scope="col" className="w-6 py-2">
                <span className="sr-only">{t("table.expand")}</span>
              </th>
              <th scope="col" className="py-2 pr-3 text-left font-medium">
                #
              </th>
              <th scope="col" className="py-2 pr-3 text-left font-medium">
                {t("table.address")}
              </th>
              <th scope="col" className="py-2 pr-3 text-left font-medium">
                {t("table.hostname")}
              </th>
              <th scope="col" className="py-2 pr-3 text-right font-medium">
                {t("table.rtt")}
              </th>
              <th scope="col" className="py-2 text-right font-medium">
                {t("table.loss")}
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
      </ScrollableX>

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
  const t = useT(mtrDetailDict);
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
              aria-label={t("hop.enrichment.aria", { number: hop.number, ip: hop.ip })}
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
        {/* The truncation lives on a block INSIDE the cell: `truncate` on a
            <td> does nothing — the cell just grows, and a long rDNS name used
            to push RTT and Loss off the right edge of the card entirely (QA
            scope 4, finding #6). The full name stays reachable as a title. */}
        <td className="py-2 pr-3 align-top text-xs text-muted-foreground">
          <span className="block max-w-[14rem] truncate" title={hop.hostname || undefined}>
            {hop.hostname || "—"}
          </span>
        </td>
        <td className="nums py-2 pr-3 text-right align-top">
          {/* The RTT cell IS the trend affordance (Decision 13): the number the
              reader is looking at is the one the chart puts in time. Without a
              loaded history there is nothing to plot, so it stays plain text
              rather than becoming a button that answers nothing. */}
          {onToggleTrend && !placeholder ? (
            <button
              type="button"
              aria-pressed={trending}
              aria-label={t("trend.aria", { ip: hop.ip })}
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

/**
 * fmtTime renders a wire timestamp in the INTERFACE language and the reader's own timezone. The
 * locale is a required argument, not an optional one: a bare toLocaleString() reorders the date and
 * swaps in AM/PM from whatever the browser was installed in, so a Russian page was printing
 * "8/10/2026 3:47 AM". Digits are digits; their ORDER is not.
 */
export function fmtTime(ts: string | null | undefined, locale: Locale): string {
  if (!ts) return "—";
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? ts : stampFull(d, locale);
}
