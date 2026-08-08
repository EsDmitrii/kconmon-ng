import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { EChart } from "@/components/echart";
import { fmtTime, shortHash } from "@/components/mtr-hop-table";
import { useTheme } from "@/components/theme-provider";
import { useTopology } from "@/hooks/use-topology";
import { getConfig, promqlQueryRange } from "@/lib/api";
import { type CuratedChart, toSeriesOption } from "@/lib/curated-metrics";
import type { PathSnapshot } from "@/lib/types";
import { escapeLabelValue } from "@/lib/utils";

/* ── which loss family describes this pair ──────────────────────────────── */

/** LossFamily names the two metric families a pair's loss can live in. They
 *  are not variants of one metric: the peer family is labelled by
 *  {source_node, destination_node} and the external one by {source_node,
 *  target}, so picking the wrong one yields an empty series, not a wrong
 *  number. */
export type LossFamily = "peer" | "external";

/**
 * lossFamilyFor decides which family to query for a (source, destination)
 * pair, and the rule is deliberately the simplest honest one: a destination
 * that appears in the controller's node list is a MESH PEER and its loss lives
 * in the peer gauges; anything else is an external target NAME (that is the
 * only other thing a snapshot's `destination` can hold — store rows carry a
 * node name or a target name, never an address) and its loss lives in
 * kconmon_ng_external_packet_loss_ratio{target=...}.
 *
 * The known-node list is the live topology, which means the rule is only as
 * good as what the controller reports right now: a node that has since left
 * the fleet reads as external and its series comes back empty. The caller
 * therefore waits for topology to RESOLVE before asking anything — guessing
 * from an empty node list would send every pair to the external family and
 * quietly draw nothing. An empty list here still answers "external" rather
 * than throwing, because a component that has snapshots but no topology yet
 * must render, and "external" is the answer that at least names something the
 * store can hold.
 */
export function lossFamilyFor(destination: string, nodeNames: string[]): LossFamily {
  return nodeNames.includes(destination) ? "peer" : "external";
}

/**
 * pairLossQuery builds the pair's packet-loss series. Metric names and label
 * sets are the agent's own, verified against internal/metrics/prometheus.go:
 *
 *  - peer: `_icmp_packet_loss_ratio` and `_udp_packet_loss_ratio` over
 *    {source_node, destination_node, source_zone, destination_zone}. BOTH are
 *    asked for, tagged with a synthetic `protocol` label the way
 *    pair-card.tsx's own series query does, because which of them exists for a
 *    given pair depends on which check types the fleet runs — an ICMP-only
 *    query would read as "no loss" on a UDP-only mesh. There is no MTR loss
 *    metric to prefer: the MTR family is `_mtr_hops` and `_mtr_hop_rtt`, and
 *    per-hop loss deliberately never became a metric (hop_ip cardinality,
 *    M4).
 *  - external: `_external_packet_loss_ratio` over {source_node, source_zone,
 *    target, target_kind} — no destination_node exists there, because an
 *    external destination is not a peer.
 *
 * `max by (protocol)` rather than `sum`: a loss RATIO is not additive, and two
 * zones' series summed would happily exceed 1.0. The worst observation for the
 * pair is the honest one-number summary of "was this route losing packets
 * when the path changed?".
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

/** stepSecondsFor keeps the proxy's point budget the same one target-card.tsx
 *  and pair-card.tsx use: about TARGET_POINTS samples, rounded up to a whole
 *  scrape interval, never finer than MIN_STEP_SECONDS. A pair whose history
 *  spans weeks therefore costs the same query as one that spans an hour. */
export function stepSecondsFor(rangeSeconds: number): number {
  return Math.max(1, Math.ceil(rangeSeconds / TARGET_POINTS / MIN_STEP_SECONDS)) * MIN_STEP_SECONDS;
}

function msOf(ts: string): number {
  const ms = new Date(ts).getTime();
  return Number.isNaN(ms) ? Date.now() : ms;
}

/**
 * PathChangesTimeline is MTR_EXPLORER.md's "'path changes' timeline overlaid
 * with the pair's loss series from Prometheus": one marker per snapshot at its
 * first_seen, on the same time axis as the loss chart underneath, so "the
 * route changed" and "the pair started losing packets" can be read as one
 * picture instead of two.
 *
 * The window is [oldest loaded snapshot's first_seen, now]. It is derived from
 * the snapshots the history pane has ALREADY loaded, so pressing "Load older"
 * widens it and nothing else refetches — and it is memoised on that one
 * timestamp so a re-render does not mint a new `now`, a new query key and a
 * new request every time React feels like it.
 *
 * Two honest degradations, the same pair target-card.tsx's History tab makes:
 * Prometheus unconfigured on this replica (GET /api/v1/config's
 * `prometheus.configured`, the signal the proxy's own 503 gate reads) keeps
 * the markers, says so once and issues ZERO requests; an empty series says so
 * rather than drawing a flat line at zero, which would read as "no loss" when
 * it means "nothing measured".
 */
export function PathChangesTimeline({
  source,
  destination,
  snapshots,
}: {
  source: string;
  destination: string;
  snapshots: PathSnapshot[];
}) {
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

  // Snapshots arrive newest-first; the oldest one opens the window.
  const oldestFirstSeen = snapshots.length > 0 ? snapshots[snapshots.length - 1].firstSeen : "";
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

  const { data, isLoading } = useQuery({
    queryKey: ["mtr", "pair-loss", source, destination, family, oldestFirstSeen],
    queryFn: () => {
      const start = new Date(window?.start ?? Date.now());
      const end = new Date(window?.end ?? Date.now());
      const stepSeconds = stepSecondsFor((end.getTime() - start.getTime()) / 1000);
      return promqlQueryRange(chart.query, start, end, stepSeconds * 1e9);
    },
    enabled: window !== null && promResolved && promConfigured && topologyResolved,
  });

  const option = useMemo(() => (data ? toSeriesOption(chart, data, theme === "dark") : undefined), [chart, data, theme]);
  // promqlQueryRange RESOLVES Prometheus's own error envelope rather than
  // throwing (lib/api.ts's `handle`), so a query-level failure shows up here.
  const queryError = data?.status === "error" ? (data.error ?? "query failed") : undefined;
  const empty =
    data?.status === "success" && (data.data?.resultType !== "matrix" || (data.data?.result ?? []).length === 0);

  if (window === null) return null;
  const span = window.end - window.start;

  return (
    <section aria-label="Path changes over time" className="mt-3">
      <div className="relative h-28 w-full">
        {option && !empty && !queryError ? <EChart option={option} className="absolute inset-0 h-full w-full" /> : null}

        {/* The markers are real DOM, not chart marklines: they must survive the
            two cases where there IS no chart (Prometheus off, empty series),
            and "when did this route change?" is the question this strip exists
            to answer. */}
        <ul aria-label="Path changes" className="pointer-events-none absolute inset-x-0 top-0 h-full">
          {snapshots.map((s) => {
            const pct = Math.min(100, Math.max(0, ((msOf(s.firstSeen) - window.start) / span) * 100));
            return (
              <li key={s.id} className="absolute top-0 h-full" style={{ left: `${pct}%` }}>
                <span
                  aria-label={`Path ${shortHash(s.pathHash)} first seen ${fmtTime(s.firstSeen)}`}
                  title={`${s.pathHash}\nfirst seen ${fmtTime(s.firstSeen)}`}
                  className="block h-full w-px bg-health-warn/70"
                />
              </li>
            );
          })}
        </ul>
      </div>

      {promResolved && !promConfigured ? (
        <p role="status" className="mt-1 text-xs leading-relaxed text-muted-foreground">
          Path changes only — set console.prometheus.address to overlay this pair's packet loss.
        </p>
      ) : null}
      {queryError ? (
        <p role="alert" className="mt-1 text-xs text-health-bad">
          {queryError}
        </p>
      ) : null}
      {empty && !queryError ? (
        <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
          No loss series for this pair over the window — nothing is probing it, or the {family} metric family carries no
          samples for it yet.
        </p>
      ) : null}
      {promConfigured && isLoading && !data ? (
        <p role="status" className="mt-1 text-xs text-muted-foreground">
          Loading the pair's loss series…
        </p>
      ) : null}
    </section>
  );
}
