import { fmtRttNs } from "@/components/mtr-hop-table";
import { useState } from "react";
import { ChevronRight } from "lucide-react";
import { useInfiniteQuery } from "@tanstack/react-query";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { listPathTraces } from "@/lib/api";
import { stampFull, useLocale, useT, type Locale } from "@/lib/i18n";
import { mtrDetailDict } from "@/lib/i18n/dict/mtr-detail";
import type { MTRHop, PathTrace } from "@/lib/types";
import { cn } from "@/lib/utils";

/**
 * TraceList is the answer to «а как их посмотреть???».
 *
 * A route row in the path history says "147 traces" and shows ONE hop table — the last reading
 * folded into that route. The traces themselves were in the database all along, each with its own
 * clock and its own RTTs, and nothing in the console led to them: the count was a dead end.
 *
 * So this lists them. Newest first, one row per trace, expandable into that trace's own hop
 * readings. A route is an IDENTITY (the sequence of addresses); the numbers on it move trace by
 * trace, and this is where that movement is visible.
 *
 * It is deliberately lazy: the query runs when this mounts, which is when a reader opened the trace
 * detail. A route that held for a day can carry thousands of traces, so the list is paged and says
 * how many it is showing.
 */
export function TraceList({ snapshotID, traceCount }: { snapshotID: string; traceCount: number }) {
  const t = useT(mtrDetailDict);
  const { locale } = useLocale();
  const [expanded, setExpanded] = useState<number | null>(null);

  /* useInfiniteQuery, and NOT a local accumulator fed from inside queryFn. That shape looked
     equivalent and was not: react-query does not re-run queryFn for a cache hit, so re-opening a
     route within the 10s staleTime remounted this component with an empty list and no fetch to fill
     it — and an empty list here renders the "these traces were swept by retention" note. A route
     with hundreds of traces read as a route with none. The rendered list is now DERIVED from the
     cache, so a warm cache paints the same thing a cold one does. */
  const query = useInfiniteQuery({
    queryKey: ["mtr", "traces", snapshotID],
    queryFn: ({ pageParam }) => listPathTraces(snapshotID, { limit: 50, cursor: pageParam || undefined }),
    initialPageParam: "",
    getNextPageParam: (page) => page.nextCursor || undefined,
  });

  const traces = query.data?.pages.flatMap((page) => page.traces ?? []) ?? [];

  if (query.isPending) {
    return (
      <div role="status" aria-live="polite" className="mt-4">
        <span className="sr-only">{t("traces.loading")}</span>
        <Skeleton className="h-16 w-full" />
      </div>
    );
  }

  /* Only when there is NOTHING on screen. A failed "Load older" used to replace fifty traces the
     reader was already reading with one line of apology — the pages were still in the cache and
     simply stopped being rendered, with no way back but closing the modal. A page that failed is a
     note beside the button, not the loss of the page that succeeded. */
  if (query.isError && traces.length === 0) {
    return (
      <p role="alert" className="mt-4 text-xs text-health-bad">
        {t("traces.error")}
      </p>
    );
  }

  return (
    <section className="mt-6 border-t border-border pt-4">
      <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
        <h3 className="text-sm font-semibold">{t("traces.title")}</h3>
        <p className="nums text-xs text-muted-foreground">
          {t("traces.count", {
            shown: traces.length,
            total: Number.isFinite(traceCount) ? traceCount : traces.length,
          })}
        </p>
      </div>

      {traces.length === 0 ? (
        /* A route can outlive the traces behind it: they live with the RUN retention sweep, not the
           path history's. An empty list under a non-zero count is the honest answer, and saying why
           is the difference between that and a broken panel. */
        <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{t("traces.empty")}</p>
      ) : (
        <ul aria-label={t("traces.aria")} className="mt-2 divide-y divide-border">
          {traces.map((trace, i) => (
            <TraceRow
              key={trace.id}
              trace={trace}
              locale={locale}
              expanded={expanded === i}
              onToggle={() => setExpanded(expanded === i ? null : i)}
            />
          ))}
        </ul>
      )}

      {query.hasNextPage ? (
        <div className="mt-3 flex flex-col items-center gap-2">
          {/* The failed page, named where it happened, with the button still there to retry it. */}
          {query.isError ? (
            <p role="alert" className="text-xs text-health-bad">
              {t("traces.olderError")}
            </p>
          ) : null}
          <Button
            variant="outline"
            size="sm"
            disabled={query.isFetchingNextPage}
            onClick={() => void query.fetchNextPage()}
          >
            {query.isFetchingNextPage ? t("traces.loadingOlder") : t("traces.loadOlder")}
          </Button>
        </div>
      ) : null}
    </section>
  );
}

/** One trace: when it ran, how long it took, and — on demand — what each hop read. */
function TraceRow({
  trace,
  locale,
  expanded,
  onToggle,
}: {
  trace: PathTrace;
  locale: Locale;
  expanded: boolean;
  onToggle: () => void;
}) {
  const t = useT(mtrDetailDict);
  const hops = Array.isArray(trace.hops) ? (trace.hops as MTRHop[]) : [];
  const detailID = `trace-${trace.id}-hops`;

  return (
    <li>
      <button
        type="button"
        aria-expanded={expanded}
        aria-controls={detailID}
        onClick={onToggle}
        className={cn(
          "flex w-full items-baseline gap-3 py-2 text-left",
          "transition-colors duration-(--dur-fast) ease-(--ease) hover:bg-accent/40",
          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
        )}
      >
        <ChevronRight
          aria-hidden="true"
          className={cn(
            "mt-0.5 size-3.5 shrink-0 text-muted-foreground transition-transform duration-(--dur-fast) ease-(--ease)",
            expanded && "rotate-90",
          )}
        />
        <span className="nums text-xs text-foreground">{stampFull(new Date(trace.recordedAt), locale)}</span>
        {trace.success ? (
          <span className="nums text-xs text-muted-foreground">{fmtMs(trace.durationNs)}</span>
        ) : (
          /* A failed trace shows what went wrong INSTEAD of a duration: the elapsed time of a probe
             that never came back is dispatch overhead, and printing it as a latency is the
             masquerade the rest of this console refuses. */
          <span className="truncate text-xs text-health-bad">{trace.error || t("traces.failed")}</span>
        )}
      </button>

      {expanded ? (
        <div id={detailID} className="pb-3 pl-6">
          <table aria-label={t("traces.hops.aria")} className="w-full table-fixed text-xs">
            <colgroup>
              <col className="w-8" />
              <col className="w-[9rem]" />
              <col />
              <col className="w-[5.5rem]" />
              <col className="w-[3.75rem]" />
            </colgroup>
            <thead>
              <tr className="text-muted-foreground">
                <th scope="col" className="py-1 pr-3 text-left font-medium">
                  #
                </th>
                <th scope="col" className="py-1 pr-3 text-left font-medium">
                  {t("table.address")}
                </th>
                <th scope="col" className="py-1 pr-3 text-left font-medium">
                  {t("table.hostname")}
                </th>
                <th scope="col" className="py-1 pr-3 text-right font-medium">
                  {t("table.rtt")}
                </th>
                <th scope="col" className="py-1 text-right font-medium">
                  {t("table.loss")}
                </th>
              </tr>
            </thead>
            <tbody>
              {hops.map((hop, i) => (
                <tr key={i}>
                  <td className="nums py-1 pr-3 align-top text-muted-foreground">{hop.number}</td>
                  <td className="py-1 pr-3 align-top break-all font-mono">{hop.ip || "*"}</td>
                  <td className="py-1 pr-3 align-top break-all text-muted-foreground" title={hop.hostname}>
                    {hop.hostname || "—"}
                  </td>
                  {/* An RTT on a hop that lost every packet is dispatch time, not a round trip. */}
                  <td className="nums py-1 pr-3 align-top text-right">
                    {hop.lossRatio >= 1 ? "—" : fmtMs(hop.rttNs)}
                  </td>
                  <td className={cn("nums py-1 text-right align-top", hop.lossRatio > 0 && "text-health-bad")}>
                    {fmtLoss(hop.lossRatio)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
    </li>
  );
}

/**
 * fmtMs is the hop table's own formatter, not a second opinion about the same number.
 *
 * This used to stop at one decimal of a millisecond, so a same-node first hop — tens of
 * microseconds — printed "0.0ms" here while the hop table directly above printed "22us" for the
 * identical reading. Two components on one screen giving two answers about one measurement is worse
 * than either answer being imprecise.
 */
const fmtMs = fmtRttNs;

function fmtLoss(ratio: number | undefined): string {
  if (typeof ratio !== "number" || !Number.isFinite(ratio)) return "—";
  return `${Math.round(ratio * 100)}%`;
}
