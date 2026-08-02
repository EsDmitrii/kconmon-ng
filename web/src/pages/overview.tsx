import type { ReactNode } from "react";
import { PageShell } from "@/components/page-shell";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { useMatrix } from "@/hooks/use-matrix";
import { useTopology } from "@/hooks/use-topology";
import type { Matrix, MatrixCell, Topology } from "@/lib/types";
import { cn } from "@/lib/utils";

export interface OverviewSummary {
  totalNodes: number;
  readyNodes: number;
  pairsTotal: number;
  pairsFailing: number;
  pairsDegraded: number;
  worstPairs: MatrixCell[]; // top 5 by failRatio desc, failing/degraded only
}

// Health tiers mirror the matrix/topology thresholds: fail ≥ 10% is "failing",
// 1%–10% is "degraded". Cells with a null failRatio have no probe data and are
// excluded from every count so an unmeasured pair never reads as healthy.
export function summarize(matrix: Matrix, topo?: Topology): OverviewSummary {
  const scored = matrix.cells.filter((c) => c.failRatio !== null);
  const failing = scored.filter((c) => (c.failRatio ?? 0) >= 0.1);
  const degraded = scored.filter((c) => (c.failRatio ?? 0) >= 0.01 && (c.failRatio ?? 0) < 0.1);
  return {
    totalNodes: topo?.nodes.length ?? matrix.nodes.length,
    readyNodes: topo ? topo.nodes.filter((n) => n.ready).length : matrix.nodes.length,
    pairsTotal: scored.length,
    pairsFailing: failing.length,
    pairsDegraded: degraded.length,
    worstPairs: [...failing, ...degraded]
      .sort((a, b) => (b.failRatio ?? 0) - (a.failRatio ?? 0))
      .slice(0, 5),
  };
}

function fmtRtt(ns?: number): string {
  if (ns === undefined) return "—";
  return `${(ns / 1e6).toFixed(1)}ms`;
}

type Tone = "warn" | "bad";

/* Stat tile: the number is the hero, the label is a quiet caption above it. A
   tone only appears when the value itself means trouble, and it arrives on three
   channels at once — a left rail, the figure's colour and a worded badge — so
   the state survives greyscale and colour-blind readers. */
function StatTile({
  label,
  value,
  tone,
  hint,
  toneLabel,
}: {
  label: string;
  value: ReactNode;
  tone?: Tone;
  hint?: string;
  toneLabel?: string;
}) {
  return (
    <Card className="relative overflow-hidden p-5">
      {tone ? (
        <span
          aria-hidden="true"
          className={cn(
            "absolute inset-y-0 left-0 w-1",
            tone === "bad" ? "bg-health-bad" : "bg-health-warn",
          )}
        />
      ) : null}
      <div className="text-[11px] font-semibold uppercase tracking-[0.09em] text-muted-foreground">
        {label}
      </div>
      <div className="mt-3 flex flex-wrap items-baseline gap-x-3 gap-y-2">
        <span
          className={cn(
            "nums text-4xl font-semibold leading-none tracking-tight",
            tone === "bad" && "text-health-bad",
            tone === "warn" && "text-health-warn",
          )}
        >
          {value}
        </span>
        {tone && toneLabel ? (
          <Badge variant={tone === "bad" ? "bad" : "warn"}>{toneLabel}</Badge>
        ) : null}
      </div>
      {hint ? <p className="mt-2 text-xs text-muted-foreground">{hint}</p> : null}
    </Card>
  );
}

/* Blank Slate: a short sentence saying why the panel is empty plus the next
   action, never a bare "No data." */
function BlankSlate({ title, body }: { title: string; body: string }) {
  return (
    <div className="flex flex-col items-center gap-2 px-6 py-10 text-center">
      <span
        aria-hidden="true"
        className="mb-1 flex size-10 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
      >
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" className="size-5">
          <circle cx="12" cy="12" r="9" />
          <path d="M9 12h6" strokeLinecap="round" />
        </svg>
      </span>
      <p className="text-sm font-medium">{title}</p>
      <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">{body}</p>
    </div>
  );
}

function SkeletonBar({ className }: { className?: string }) {
  return <span aria-hidden="true" className={cn("block animate-pulse rounded-sm bg-muted", className)} />;
}

/* Loading state mirrors the shape of the loaded page — three tiles and a table —
   so nothing jumps when the data lands. */
function OverviewSkeleton() {
  return (
    <div role="status" aria-live="polite" className="flex flex-col gap-6">
      <span className="sr-only">Loading overview…</span>
      <div className="grid gap-4 sm:grid-cols-3">
        {[0, 1, 2].map((i) => (
          <Card key={i} className="p-5">
            <SkeletonBar className="h-2.5 w-24" />
            <SkeletonBar className="mt-4 h-8 w-20" />
          </Card>
        ))}
      </div>
      <Card className="p-6">
        <SkeletonBar className="h-3 w-28" />
        <div className="mt-6 flex flex-col gap-4">
          {[0, 1, 2, 3].map((i) => (
            <SkeletonBar key={i} className="h-4 w-full" />
          ))}
        </div>
      </Card>
    </div>
  );
}

// Sections whose data plane lands in a later milestone (firing alerts →
// Alertmanager, M2+; recent events → events store, M7). Rendered as honest
// placeholders rather than fabricated rows.
function LaterMilestone({ title, note }: { title: string; note: string }) {
  return (
    <Card asChild className="p-6">
      <section>
        <h2 className="text-sm font-semibold">{title}</h2>
        <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{note}</p>
      </section>
    </Card>
  );
}

function WorstPairsTable({ pairs }: { pairs: MatrixCell[] }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <caption className="sr-only">Worst pairs by failure ratio</caption>
        <thead>
          <tr className="border-b border-border text-left text-[11px] uppercase tracking-[0.07em] text-muted-foreground">
            <th scope="col" className="w-10 py-3 pr-4 font-semibold">
              #
            </th>
            <th scope="col" className="py-3 pr-6 font-semibold">
              Pair
            </th>
            <th scope="col" className="py-3 pr-6 text-right font-semibold">
              Fail %
            </th>
            <th scope="col" className="py-3 pr-6 text-right font-semibold">
              p95 RTT
            </th>
            <th scope="col" className="py-3 font-semibold">
              Status
            </th>
          </tr>
        </thead>
        <tbody className="divide-y divide-border">
          {pairs.map((c, i) => {
            const fail = c.failRatio ?? 0;
            const failing = fail >= 0.1;
            return (
              <tr
                key={`${c.source} ${c.destination}`}
                className="transition-colors duration-(--dur) ease-(--ease) hover:bg-accent/40"
              >
                <td className="nums py-4 pr-4 text-xs text-muted-foreground">{i + 1}</td>
                <td className="max-w-[22rem] py-4 pr-6">
                  <span className="flex items-center gap-2">
                    <span className="truncate" title={c.source}>
                      {c.source}
                    </span>
                    <span aria-hidden="true" className="shrink-0 text-muted-foreground">
                      →
                    </span>
                    <span className="truncate" title={c.destination}>
                      {c.destination}
                    </span>
                  </span>
                </td>
                <td
                  className={cn(
                    "nums py-4 pr-6 text-right text-base font-semibold tracking-tight",
                    failing ? "text-health-bad" : "text-health-warn",
                  )}
                >
                  {(100 * fail).toFixed(1)}%
                </td>
                <td className="nums py-4 pr-6 text-right text-muted-foreground">{fmtRtt(c.rttP95)}</td>
                <td className="py-4">
                  <Badge variant={failing ? "bad" : "warn"} dot>
                    {failing ? "Failing" : "Degraded"}
                  </Badge>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

export function OverviewPage() {
  const topo = useTopology();
  const matrix = useMatrix("tcp");

  const error = matrix.error ?? topo.error;
  const summary = matrix.data ? summarize(matrix.data, topo.data) : undefined;
  // summarize()'s topology-absent fallback (readyNodes = totalNodes) is meant
  // for "no data at all" — it must not be read as "all nodes ready" while the
  // topology query is still in flight or has genuinely errored (e.g. the
  // controller isn't configured, per Task 11). The tile shows an explicit
  // loading/unknown state in those cases instead of a silently optimistic count.
  const nodesReadyDisplay = topo.data
    ? `${summary?.readyNodes ?? 0}/${summary?.totalNodes ?? 0}`
    : topo.isLoading
      ? "…"
      : "—";

  return (
    <PageShell
      title="Overview"
      description="Cluster health at a glance, recomputed from Prometheus every 15s."
    >
      <div className="flex flex-col gap-6">
        {error ? (
          <Card
            role="alert"
            className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5"
          >
            <p className="text-sm font-medium">Overview data is unavailable</p>
            <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{error.message}</p>
            <p className="mt-2 text-xs leading-relaxed text-muted-foreground">
              The page keeps retrying every 15s. If it persists, check that the console can reach
              Prometheus.
            </p>
          </Card>
        ) : null}

        {!summary && matrix.isLoading ? <OverviewSkeleton /> : null}

        {summary ? (
          <>
            <div className="grid gap-4 sm:grid-cols-3">
              <StatTile
                label="Nodes ready"
                value={nodesReadyDisplay}
                hint={topo.data ? undefined : "Topology unavailable"}
              />
              <StatTile
                label="Failing pairs"
                value={summary.pairsFailing}
                tone={summary.pairsFailing > 0 ? "bad" : undefined}
                toneLabel="Fail ≥ 10%"
              />
              <StatTile
                label="Degraded pairs"
                value={summary.pairsDegraded}
                tone={summary.pairsDegraded > 0 ? "warn" : undefined}
                toneLabel="Fail 1–10%"
              />
            </div>

            <Card asChild className="p-6">
              <section>
                <div className="flex flex-wrap items-baseline justify-between gap-2">
                  <h2 className="text-sm font-semibold">Worst pairs</h2>
                  <p className="nums text-xs text-muted-foreground">
                    {summary.pairsTotal} measured pair{summary.pairsTotal === 1 ? "" : "s"}
                  </p>
                </div>
                {summary.worstPairs.length === 0 ? (
                  summary.pairsTotal === 0 ? (
                    <BlankSlate
                      title="No probe data in Prometheus yet"
                      body="Pairs appear here once the agents have completed a probe round and Prometheus has scraped them — usually within a minute of the DaemonSet becoming ready."
                    />
                  ) : (
                    <BlankSlate
                      title="No failing or degraded pairs"
                      body="Every measured pair is under a 1% failure ratio. Anything that crosses that line shows up here, worst first."
                    />
                  )
                ) : (
                  <div className="mt-4">
                    <WorstPairsTable pairs={summary.worstPairs} />
                  </div>
                )}
              </section>
            </Card>

            <div className="grid gap-4 sm:grid-cols-2">
              <LaterMilestone
                title="Firing alerts"
                note="Arrives with a later milestone (Alertmanager wiring)."
              />
              <LaterMilestone
                title="Recent events"
                note="Arrives with a later milestone (events store)."
              />
            </div>
          </>
        ) : null}
      </div>
    </PageShell>
  );
}
