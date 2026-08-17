import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { EChart } from "@/components/echart";
import { fmtTime, shortHash } from "@/components/mtr-hop-table";
import { useTheme } from "@/components/theme-provider";
import { useTopology } from "@/hooks/use-topology";
import { getConfig, promqlQueryRange } from "@/lib/api";
import { type CuratedChart, toSeriesOption } from "@/lib/curated-metrics";
import { useLocale, useT } from "@/lib/i18n";
import { PROMQL_MAX_RANGE_MS } from "@/lib/investigation-sources";
import { mtrDetailDict } from "@/lib/i18n/dict/mtr-detail";
import type { PathSnapshot } from "@/lib/types";
import { cn, escapeLabelValue } from "@/lib/utils";

/* ── which loss family describes this pair ──────────────────────────────── */

/** LossFamily names the two metric families a pair's loss can live in. */
export type LossFamily = "peer" | "external";

/** lossFamilyFor decides which family to query for a (source, destination) pair. */
export function lossFamilyFor(destination: string, nodeNames: string[]): LossFamily {
  return nodeNames.includes(destination) ? "peer" : "external";
}

/**
 * pairLossQuery builds the pair's packet-loss series; BOTH are asked for, tagged with a synthetic
 * `protocol` label the way pair-card.tsx's own series query does.
 */
export function pairLossQuery(source: string, destination: string, family: LossFamily): string {
  const src = `source_node="${escapeLabelValue(source)}"`;
  const tag = (expr: string, protocol: string) => `label_replace(${expr}, "protocol", "${protocol}", "", "")`;

  if (family === "external") {
    const sel = `{${src},target="${escapeLabelValue(destination)}"}`;
    return `max by (protocol) (${tag(`kconmon_ng_external_packet_loss_ratio${sel}`, "external")})`;
  }
  const sel = `{${src},destination_node="${escapeLabelValue(destination)}"}`;
  return (
    "max by (protocol) (" +
    `${tag(`kconmon_ng_icmp_packet_loss_ratio${sel}`, "icmp")} or ` +
    `${tag(`kconmon_ng_udp_packet_loss_ratio${sel}`, "udp")}` +
    ")"
  );
}

/* ── the strip ──────────────────────────────────────────────────────────── */

const TARGET_POINTS = 120;
const MIN_STEP_SECONDS = 15;

/** stepSecondsFor keeps the proxy's point budget the same one target-card.tsx and pair-card.tsx use. */
export function stepSecondsFor(rangeSeconds: number): number {
  return Math.max(1, Math.ceil(rangeSeconds / TARGET_POINTS / MIN_STEP_SECONDS)) * MIN_STEP_SECONDS;
}

/**
 * PLOT_GRID is the strip's plot rectangle, in pixels from the box's edges — and
 * it is used TWICE on purpose: once as the chart's grid, once as the inset of
 * the marker track.
 *
 * The markers are DOM over the chart, positioned by percentage. A percentage of
 * the BOX is not a percentage of the PLOT: the chart reserves this much room for
 * its y-axis labels and its legend, so a marker at t=window.start stood 44px
 * left of the plot's own x=0, and the ones nearest the two ends of the window
 * left the plot entirely. One constant, two consumers, no drift.
 */
const PLOT_GRID = { left: 44, right: 10, top: 24, bottom: 22 } as const;
/** No chart, no grid to stand on: the track takes the whole box, as it always did. */
const FULL_BOX = { left: 0, right: 0, top: 0, bottom: 0 } as const;

function msOf(ts: string): number {
  const ms = new Date(ts).getTime();
  return Number.isNaN(ms) ? Date.now() : ms;
}

/**
 * PathChangesTimeline is MTR_EXPLORER.md's "'path changes' timeline overlaid with the pair's loss
 * series from Prometheus".
 */
export function PathChangesTimeline({
  source,
  destination,
  snapshots,
  selectedId,
  onSelect,
}: {
  source: string;
  destination: string;
  snapshots: PathSnapshot[];
  /** The route the panes below are showing, so the strip can mark its own hairline. */
  selectedId?: string | null;
  /** Selecting a marker selects that route — the same click the list rows make. */
  onSelect?: (snapshot: PathSnapshot) => void;
}) {
  const t = useT(mtrDetailDict);
  const { locale } = useLocale();
  const { theme } = useTheme();
  const topo = useTopology();
  const configQuery = useQuery({ queryKey: ["config"], queryFn: getConfig, staleTime: Infinity });

  const promResolved = configQuery.data !== undefined;
  const promConfigured = configQuery.data?.prometheus?.configured ?? false;
  // Resolved, not "has nodes": an answered topology with an empty node list is
  // a real (if sad) fleet, and waiting forever for one to appear would leave
  // the chart permanently blank.
  const topologyResolved = !topo.isPending;
  const nodeNames = useMemo(() => topo.data?.nodes?.map((n) => n.name) ?? [], [topo.data]);

  /* The EARLIEST first_seen across the loaded routes, not the last row's.
     Snapshots arrive ordered by last_seen (the store's ORDER BY last_seen DESC,
     id DESC), which says nothing about first_seen: a route the pair keeps
     reverting to has both the oldest first_seen and the newest last_seen, so it
     sorts FIRST. Reading the last row opened the window at a route that began
     yesterday while another began a week ago, and every marker then collapsed
     onto the left edge (owner report: the strip's hairline sits outside the
     plot). */
  const oldestFirstSeen = useMemo(() => {
    let oldest = "";
    for (const s of snapshots) {
      if (!s.firstSeen) continue;
      if (oldest === "" || msOf(s.firstSeen) < msOf(oldest)) oldest = s.firstSeen;
    }
    return oldest;
  }, [snapshots]);
  const window = useMemo(() => {
    if (oldestFirstSeen === "") return null;
    const start = msOf(oldestFirstSeen);
    // A snapshot stamped in the future would invert the axis; one second is
    // enough to keep every position finite and inside the track.
    const end = Math.max(Date.now(), start + 1000);
    return { start, end };
  }, [oldestFirstSeen]);

  const family = lossFamilyFor(destination, nodeNames);
  const chart = useMemo<CuratedChart>(
    () => ({
      id: "mtr-pair-loss",
      title: "Packet loss",
      unit: "ratio",
      query: pairLossQuery(source, destination, family),
    }),
    [source, destination, family],
  );

  /* The QUERY window, which is not the marker window. Route history can span
     weeks; the console's PromQL proxy refuses a range over console.prometheus.
     maxRange (24h by default) with a 422, and lib/api.ts THROWS on problem+json
     rather than returning an envelope — so the strip lost its chart entirely for
     exactly the pairs that have a route history worth looking at. The axis stays
     on the full window; only the request is clamped, and the note below says so. */
  const queryStart = window ? Math.max(window.start, window.end - PROMQL_MAX_RANGE_MS) : 0;
  const clampedHours = window && queryStart > window.start ? Math.round(PROMQL_MAX_RANGE_MS / 3_600_000) : 0;

  const { data, isLoading, error } = useQuery({
    queryKey: ["mtr", "pair-loss", source, destination, family, oldestFirstSeen, queryStart],
    queryFn: () => {
      const start = new Date(queryStart);
      const end = new Date(window?.end ?? Date.now());
      const stepSeconds = stepSecondsFor((end.getTime() - start.getTime()) / 1000);
      return promqlQueryRange(chart.query, start, end, stepSeconds * 1e9);
    },
    enabled: window !== null && promResolved && promConfigured && topologyResolved,
  });

  /* The window is passed so the axis spans it: every marker below is positioned
     as a percentage of [window.start, window.end], and an axis ECharts fitted to
     whatever samples Prometheus happened to return put the two on different
     scales — the markers then pointed at the wrong place on the plot. */
  const full = useMemo(
    () =>
      data
        ? toSeriesOption(
            chart,
            data,
            theme === "dark",
            window ? { start: new Date(window.start), end: new Date(window.end) } : undefined,
          )
        : undefined,
    [chart, data, theme, window],
  );
  /* toSeriesOption's grid reserves 46px at the bottom for a scrollable legend —
     right for a 256px card, catastrophic in this strip, where it left the plot
     ~54px tall with the y-axis labels crammed into a corner. Here the legend
     moves to a single line at the top and the grid takes the rest. */
  const option = useMemo(() => {
    if (!full) return undefined;
    const legend = full.legend && !Array.isArray(full.legend)
      ? { ...full.legend, bottom: undefined, top: 0, left: "center" as const, itemHeight: 2, itemWidth: 12 }
      : full.legend;
    const yAxis = full.yAxis && !Array.isArray(full.yAxis) ? { ...full.yAxis, splitNumber: 3 } : full.yAxis;
    return { ...full, grid: { ...PLOT_GRID }, legend, yAxis };
  }, [full]);
  // promqlQueryRange RESOLVES Prometheus's own error envelope rather than
  // throwing (lib/api.ts's `handle`), so a query-level failure shows up here.
  /* Both failure shapes, not just Prometheus's own envelope: the proxy answers
     problem+json for a refused range and lib/api.ts THROWS on that, so a rejected
     request used to leave the strip silently chartless with nothing said. */
  const queryError =
    data?.status === "error"
      ? (data.error ?? t("changes.queryFailed"))
      : error
        ? (error.message === "" ? t("changes.queryFailed") : error.message)
        : undefined;
  const empty =
    data?.status === "success" && (data.data?.resultType !== "matrix" || (data.data?.result ?? []).length === 0);

  if (window === null) return null;
  const span = window.end - window.start;
  /*
  The markers, in AXIS order with a hit area that cannot overlap a neighbour's.
  Sorted ascending so the tab order walks the strip left to right — the snapshots
  arrive newest-first, which is the right order for the list below and the wrong
  one for a timeline.
  */
  const MAX_PAD_PCT = 2;
  const positions = snapshots
    .map((snap) => ({ snap, pct: Math.min(100, Math.max(0, ((msOf(snap.firstSeen) - window.start) / span) * 100)) }))
    .sort((a, b) => a.pct - b.pct || (a.snap.id < b.snap.id ? -1 : 1));
  const placed = positions.map((p, i) => {
    const gaps = [
      i > 0 ? p.pct - positions[i - 1].pct : Infinity,
      i < positions.length - 1 ? positions[i + 1].pct - p.pct : Infinity,
    ];
    return { ...p, padPct: Math.min(MAX_PAD_PCT, Math.min(...gaps) / 2) };
  });
  /*
   * Is there a chart to frame at all?; the 112px box was unconditional, so Prometheus-off and
   * empty-series both drew an empty grey rectangle above the "no loss series" note.
   */
  const hasChart = option !== undefined && !empty && !queryError;

  return (
    <section aria-label={t("changes.aria")} className="mt-3">
      <div className={cn("relative w-full", hasChart ? "h-36" : "h-6")}>
        {hasChart ? <EChart option={option} className="absolute inset-0 h-full w-full" /> : null}

        {/* The markers are real DOM, not chart marklines: they must survive the
            two cases where there IS no chart (Prometheus off, empty series),
            and "when did this route change?" is the question this strip exists
            to answer.

            pointer-events-auto on the marker itself, inside a
            pointer-events-none track (QA round 4, finding #8): the track spans
            the full width and would otherwise swallow every hover meant for
            the chart underneath, while the 1px marker it exists to position
            could not be hovered at all — so its title, the only place the
            full path hash and the first-seen stamp are written, was
            unreachable. The hit area is padded out to 9px around the hairline
            (px-1) because a 1px pointer target is not one.

            The track itself sits on the CHART'S PLOT RECTANGLE (PLOT_GRID) —
            see that constant for why a percentage of the box was the wrong
            percentage. Without a chart there is no plot, and it falls back to
            the whole box. */}
        <ul
          aria-label={t("changes.list.aria")}
          style={hasChart ? PLOT_GRID : FULL_BOX}
          className="pointer-events-none absolute"
        >
          {placed.map(({ snap: s, pct, padPct }) => {
            return (
              <li key={s.id} className="absolute top-0 h-full -translate-x-1/2" style={{ left: `${pct}%` }}>
                {/* A BUTTON, because the reader tried to click one and nothing
                    happened (owner report). It selects the same route the list
                    row below selects, so the strip is a way INTO the history
                    rather than a picture of it. */}
                <button
                  type="button"
                  aria-pressed={s.id === selectedId}
                  aria-label={t("changes.marker.aria", { hash: shortHash(s.pathHash), at: fmtTime(s.firstSeen, locale) })}
                  title={t("changes.marker.title", { hash: s.pathHash, at: fmtTime(s.firstSeen, locale) })}
                  onClick={() => onSelect?.(s)}
                  /* The hit area reaches HALFWAY to the nearest neighbour and no
                     further. A flat 4px padding was wider than the gap between two
                     routes that changed within minutes of each other on a 222px
                     track, so the top button covered its neighbour's hairline and
                     the click selected the wrong route. */
                  style={{ paddingLeft: `${padPct}%`, paddingRight: `${padPct}%` }}
                  className={cn(
                    "pointer-events-auto flex h-full items-stretch",
                    onSelect ? "cursor-pointer" : "cursor-help",
                    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                  )}
                >
                  <span
                    aria-hidden="true"
                    className={cn(
                      "block h-full w-px",
                      s.id === selectedId ? "w-0.5 bg-health-warn" : "bg-health-warn/70",
                    )}
                  />
                </button>
              </li>
            );
          })}
        </ul>
      </div>

      {promResolved && !promConfigured ? (
        <p role="status" className="mt-1 text-xs leading-relaxed text-muted-foreground">
          {t("changes.promUnset")}
        </p>
      ) : null}
      {queryError ? (
        <p role="alert" className="mt-1 text-xs text-health-bad">
          {queryError}
        </p>
      ) : null}
      {clampedHours > 0 && !queryError ? (
        <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
          {t("changes.clamped", { hours: clampedHours })}
        </p>
      ) : null}
      {empty && !queryError ? (
        <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{t("changes.empty", { family })}</p>
      ) : null}
      {promConfigured && isLoading && !data ? (
        <p role="status" className="mt-1 text-xs text-muted-foreground">{t("changes.loading")}</p>
      ) : null}
    </section>
  );
}
