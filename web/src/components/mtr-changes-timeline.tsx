import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { EChart } from "@/components/echart";
import { fmtTime, shortHash } from "@/components/mtr-hop-table";
import { useTheme } from "@/components/theme-provider";
import { useTopology } from "@/hooks/use-topology";
import { getConfig, promqlQueryRange } from "@/lib/api";
import { type CuratedChart, toSeriesOption } from "@/lib/curated-metrics";
import { useLocale, useT } from "@/lib/i18n";
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
}: {
  source: string;
  destination: string;
  snapshots: PathSnapshot[];
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
  const queryError = data?.status === "error" ? (data.error ?? t("changes.queryFailed")) : undefined;
  const empty =
    data?.status === "success" && (data.data?.resultType !== "matrix" || (data.data?.result ?? []).length === 0);

  if (window === null) return null;
  const span = window.end - window.start;
  /*
   * Is there a chart to frame at all?; the 112px box was unconditional, so Prometheus-off and
   * empty-series both drew an empty grey rectangle above the "no loss series" note.
   */
  const hasChart = option !== undefined && !empty && !queryError;

  return (
    <section aria-label={t("changes.aria")} className="mt-3">
      <div className={cn("relative w-full", hasChart ? "h-28" : "h-6")}>
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
            (px-1) because a 1px pointer target is not one. */}
        <ul aria-label={t("changes.list.aria")} className="pointer-events-none absolute inset-x-0 top-0 h-full">
          {snapshots.map((s) => {
            const pct = Math.min(100, Math.max(0, ((msOf(s.firstSeen) - window.start) / span) * 100));
            return (
              <li key={s.id} className="absolute top-0 h-full -translate-x-1/2" style={{ left: `${pct}%` }}>
                <span
                  tabIndex={0}
                  aria-label={t("changes.marker.aria", { hash: shortHash(s.pathHash), at: fmtTime(s.firstSeen, locale) })}
                  title={t("changes.marker.title", { hash: s.pathHash, at: fmtTime(s.firstSeen, locale) })}
                  className={cn(
                    "pointer-events-auto flex h-full cursor-help items-stretch px-1",
                    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                  )}
                >
                  <span aria-hidden="true" className="block h-full w-px bg-health-warn/70" />
                </span>
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
      {empty && !queryError ? (
        <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{t("changes.empty", { family })}</p>
      ) : null}
      {promConfigured && isLoading && !data ? (
        <p role="status" className="mt-1 text-xs text-muted-foreground">{t("changes.loading")}</p>
      ) : null}
    </section>
  );
}
