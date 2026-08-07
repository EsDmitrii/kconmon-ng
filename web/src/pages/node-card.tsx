import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { PageShell } from "@/components/page-shell";
import { RecentChanges } from "@/components/recent-changes";
import { Badge, type BadgeProps } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { useMatrix } from "@/hooks/use-matrix";
import { useTopology } from "@/hooks/use-topology";
import { getRun, getRuns } from "@/lib/api";
import { PROTOCOLS, type MatrixCell, type Protocol, type RunDetail } from "@/lib/types";
import { cn } from "@/lib/utils";

const NODE_PATH_PREFIX = "/nodes/";

/**
 * nodeNameFromPath mirrors run-detail.tsx's runIdFromPath (task-24-brief.md):
 * the object id is read straight off window.location.pathname rather than
 * through TanStack Router's own param matching, which keeps this page
 * testable with a plain render and correct for a cold load of a
 * bookmarked/shared link. decodeURIComponent undoes the encodeURIComponent a
 * caller (topology.tsx's node click) applies when building the link, so a
 * node name with characters that need escaping round-trips exactly. A
 * malformed percent-escape falls back to the raw remainder rather than
 * throwing — better a slightly wrong name than a page that crashes on a
 * hand-typed URL.
 */
export function nodeNameFromPath(pathname: string): string {
  if (!pathname.startsWith(NODE_PATH_PREFIX)) return "";
  const rest = pathname.slice(NODE_PATH_PREFIX.length);
  if (rest === "") return "";
  try {
    return decodeURIComponent(rest);
  } catch {
    return rest;
  }
}

type Tier = "ok" | "warn" | "bad" | "unknown";

const TIER_VARIANT: Record<Tier, NonNullable<BadgeProps["variant"]>> = {
  ok: "ok",
  warn: "warn",
  bad: "bad",
  unknown: "unknown",
};
const TIER_LABEL: Record<Tier, string> = {
  ok: "Healthy",
  warn: "Degraded",
  bad: "Failing",
  unknown: "No data",
};

/**
 * nodeHealth derives the header's health% and status tier from the worst
 * OUTBOUND fail ratio this node reports in the matrix — the same worst-case
 * semantics topology.tsx's buildFlow already uses for node colouring, so a
 * node's tier reads identically whether an operator arrived here from
 * Topology or a bookmark. Self-cells are excluded (a node never reports a
 * pair against itself); `ready === false` overrides the ratio and always
 * reads "bad", same as buildFlow.
 */
export function nodeHealth(
  ready: boolean | undefined,
  cells: MatrixCell[],
  nodeName: string,
): { percent: number | null; tier: Tier } {
  const outbound = cells.filter((c) => c.source === nodeName && c.destination !== nodeName && c.failRatio !== null);
  const worst = outbound.reduce((max, c) => Math.max(max, c.failRatio ?? 0), 0);
  const percent = outbound.length > 0 ? Math.max(0, 100 * (1 - worst)) : null;
  if (ready === false) return { percent, tier: "bad" };
  if (outbound.length === 0) return { percent: null, tier: "unknown" };
  const tier: Tier = worst >= 0.1 ? "bad" : worst >= 0.01 ? "warn" : "ok";
  return { percent, tier };
}

function fmtFail(ratio: number): string {
  return `${(100 * ratio).toFixed(1)}%`;
}

function fmtRtt(ns?: number): string {
  return ns === undefined ? "—" : `${(ns / 1e6).toFixed(1)}ms`;
}

function fmtTime(ts?: string): string {
  if (!ts) return "—";
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? ts : d.toLocaleString();
}

type NodeTab = "overview" | "diagnostics";

const TABS: { value: NodeTab; label: string }[] = [
  { value: "overview", label: "Overview" },
  { value: "diagnostics", label: "Diagnostics" },
];

function BreakdownTable({ nodeName, cells }: { nodeName: string; cells: MatrixCell[] }) {
  const outbound = cells.filter((c) => c.source === nodeName && c.destination !== nodeName);
  if (outbound.length === 0) {
    return <p className="px-4 py-10 text-center text-xs text-muted-foreground">No probe data for this node yet.</p>;
  }
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <caption className="sr-only">Per-destination breakdown for {nodeName}</caption>
        <thead>
          <tr className="border-b border-border text-left text-[11px] uppercase tracking-[0.07em] text-muted-foreground">
            <th scope="col" className="py-3 pl-4 pr-4 font-semibold">
              Destination
            </th>
            <th scope="col" className="py-3 pr-4 text-right font-semibold">
              Fail ratio
            </th>
            <th scope="col" className="py-3 pr-4 text-right font-semibold">
              RTT p95
            </th>
          </tr>
        </thead>
        <tbody className="divide-y divide-border">
          {outbound.map((c) => (
            <tr key={c.destination}>
              <td className="max-w-[16rem] truncate py-3 pl-4 pr-4" title={c.destination}>
                {c.destination}
              </td>
              <td
                className={cn(
                  "nums py-3 pr-4 text-right",
                  c.failRatio !== null && c.failRatio >= 0.01 ? "text-health-bad" : "text-muted-foreground",
                )}
              >
                {c.failRatio === null ? "—" : fmtFail(c.failRatio)}
              </td>
              <td className="nums py-3 pr-4 text-right text-muted-foreground">{fmtRtt(c.rttP95)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function OverviewTab({
  nodeName,
  cells,
  zone,
  ready,
  agentId,
  podIP,
}: {
  nodeName: string;
  cells: MatrixCell[];
  zone?: string;
  ready?: boolean;
  agentId?: string;
  podIP?: string;
}) {
  return (
    <div className="flex flex-col gap-5">
      <Card className="p-5">
        <h3 className="text-sm font-semibold">Agent identity</h3>
        <dl className="mt-3 grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
          <div>
            <dt className="text-xs text-muted-foreground">Zone</dt>
            <dd className="mt-0.5">{zone ?? "—"}</dd>
          </div>
          <div>
            <dt className="text-xs text-muted-foreground">Agent ID</dt>
            <dd className="mt-0.5 truncate" title={agentId}>
              {agentId ?? "—"}
            </dd>
          </div>
          <div>
            <dt className="text-xs text-muted-foreground">Pod IP</dt>
            <dd className="nums mt-0.5">{podIP ?? "—"}</dd>
          </div>
          <div>
            <dt className="text-xs text-muted-foreground">Ready</dt>
            <dd className="mt-0.5">{ready === undefined ? "—" : ready ? "yes" : "no"}</dd>
          </div>
        </dl>
      </Card>

      <Card asChild className="overflow-hidden p-0">
        <section>
          <div className="border-b border-border px-4 py-3">
            <h3 className="text-sm font-semibold">Per-destination breakdown</h3>
          </div>
          <BreakdownTable nodeName={nodeName} cells={cells} />
        </section>
      </Card>
    </div>
  );
}

// RUN_SCAN_LIMIT bounds the client-side scan below (see useNodeDiagnostics'
// own doc comment) -- kept small because each candidate costs one extra GET
// /api/v1/runs/{id}.
const RUN_SCAN_LIMIT = 20;

/** runsTouchingNode filters already-fetched run details down to the ones
 * with at least one pair result touching `nodeName`, either side. */
export function runsTouchingNode(details: RunDetail[], nodeName: string): RunDetail[] {
  return details.filter((d) => d.results.some((r) => r.sourceNode === nodeName || r.destinationNode === nodeName));
}

/**
 * useNodeDiagnostics is the Diagnostics tab's data source. GET /api/v1/runs
 * (RunQuery in lib/api.ts) has no source/destination filter, and a run's
 * per-pair results only come back from GET /api/v1/runs/{id} — so "runs
 * touching this node" is a client-side filter over the most recent
 * RUN_SCAN_LIMIT runs' full details, fetched here, not a server-side query.
 * A run older than that page is silently not considered; the tab says so
 * rather than implying complete history.
 */
function useNodeDiagnostics(nodeName: string) {
  const runsQuery = useQuery({ queryKey: ["runs", "recent-scan"], queryFn: () => getRuns({ limit: RUN_SCAN_LIMIT }) });
  const ids = useMemo(() => runsQuery.data?.runs.map((r) => r.id) ?? [], [runsQuery.data]);
  const detailsQuery = useQuery({
    queryKey: ["runs", "recent-scan", "details", ids.join(",")],
    queryFn: () => Promise.all(ids.map((id) => getRun(id))),
    enabled: ids.length > 0,
  });
  const runs = useMemo(
    () => (detailsQuery.data ? runsTouchingNode(detailsQuery.data, nodeName) : []),
    [detailsQuery.data, nodeName],
  );
  return {
    runs,
    isLoading: runsQuery.isLoading || (ids.length > 0 && detailsQuery.isLoading),
    error: runsQuery.error ?? detailsQuery.error,
  };
}

const STATUS_VARIANT: Record<string, NonNullable<BadgeProps["variant"]>> = {
  pending: "neutral",
  running: "neutral",
  succeeded: "ok",
  failed: "bad",
  partial: "warn",
};

function DiagnosticsTab({ nodeName }: { nodeName: string }) {
  const { runs, isLoading, error } = useNodeDiagnostics(nodeName);

  return (
    <Card asChild className="overflow-hidden p-0">
      <section>
        <div className="border-b border-border px-4 py-3">
          <h3 className="text-sm font-semibold">Runs touching this node</h3>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
            No server-side node filter on GET /api/v1/runs yet — this scans the most recent {RUN_SCAN_LIMIT} runs'
            results client-side. An older run touching this node may exist but will not show up here.
          </p>
        </div>

        {error ? (
          <p role="alert" className="px-4 py-4 text-sm text-health-bad">
            Run history is unavailable.
          </p>
        ) : null}

        {isLoading && runs.length === 0 && !error ? (
          <div role="status" aria-live="polite" className="flex flex-col gap-2 p-4">
            <span className="sr-only">Scanning recent runs…</span>
            {Array.from({ length: 3 }, (_, i) => (
              <Skeleton key={i} className="h-8 w-full" />
            ))}
          </div>
        ) : null}

        {!isLoading && !error && runs.length === 0 ? (
          <p className="px-4 py-10 text-center text-xs text-muted-foreground">
            No runs touching this node in the most recent {RUN_SCAN_LIMIT} runs.
          </p>
        ) : null}

        {runs.length > 0 ? (
          <ul className="divide-y divide-border">
            {runs.map((r) => {
              const touching = r.results.filter((res) => res.sourceNode === nodeName || res.destinationNode === nodeName);
              return (
                <li key={r.id} className="flex flex-wrap items-center gap-3 px-4 py-3 text-sm">
                  <a href={`/diagnostics/runs/${r.id}`} className="font-medium text-primary hover:underline">
                    {r.id}
                  </a>
                  <Badge variant={STATUS_VARIANT[r.status] ?? "unknown"} dot>
                    {r.status}
                  </Badge>
                  <span className="text-xs uppercase tracking-wide text-muted-foreground">{r.type}</span>
                  <span className="nums ml-auto text-xs text-muted-foreground">
                    {touching.length} pair{touching.length === 1 ? "" : "s"}
                  </span>
                  <span className="text-xs text-muted-foreground">{fmtTime(r.createdAt)}</span>
                </li>
              );
            })}
          </ul>
        ) : null}
      </section>
    </Card>
  );
}

function NotFound({ nodeName }: { nodeName: string }) {
  return (
    <PageShell title="Node" description={nodeName ? `No node name in the URL for “${nodeName}”.` : "No node name in the URL."}>
      <Card role="status" className="px-6 py-10 text-center text-sm text-muted-foreground">
        This link is missing a node name.
      </Card>
    </PageShell>
  );
}

export function NodeCardPage() {
  const nodeName = nodeNameFromPath(window.location.pathname);
  const topo = useTopology();
  const [protocol, setProtocol] = useState<Protocol>("tcp");
  const matrix = useMatrix(protocol);
  const [tab, setTab] = useState<NodeTab>("overview");

  if (nodeName === "") return <NotFound nodeName={nodeName} />;

  const node = topo.data?.nodes.find((n) => n.name === nodeName);
  const agent = topo.data?.agents.find((a) => a.nodeName === nodeName);
  const cells = matrix.data?.cells ?? [];
  const health = nodeHealth(node?.ready, cells, nodeName);
  const loadingIdentity = (topo.isLoading && !topo.data) || (matrix.isLoading && !matrix.data);

  return (
    <PageShell
      title={nodeName}
      description={node ? `Zone ${node.zone}` : "Node"}
      actions={
        <>
          <Segmented
            aria-label="Protocol"
            options={PROTOCOLS.map((p) => ({ value: p, label: p.toUpperCase() }))}
            value={protocol}
            onChange={setProtocol}
          />
          <span className="nums text-sm text-muted-foreground">
            {health.percent === null ? "—" : `${health.percent.toFixed(1)}%`} healthy
          </span>
          <Badge variant={TIER_VARIANT[health.tier]} dot>
            {TIER_LABEL[health.tier]}
          </Badge>
        </>
      }
    >
      {topo.error ? (
        <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
          <p className="text-sm font-medium">Topology is unavailable</p>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{topo.error.message}</p>
        </Card>
      ) : null}

      {matrix.error ? (
        <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
          <p className="text-sm font-medium">Matrix is unavailable</p>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{matrix.error.message}</p>
        </Card>
      ) : null}

      {loadingIdentity ? (
        <Card role="status" aria-live="polite" className="p-6">
          <span className="sr-only">Loading node…</span>
          <Skeleton className="h-4 w-48" />
          <div className="mt-4 flex flex-col gap-2">
            {Array.from({ length: 4 }, (_, i) => (
              <Skeleton key={i} className="h-8 w-full" />
            ))}
          </div>
        </Card>
      ) : (
        <div className="grid grid-cols-1 gap-5 lg:grid-cols-[minmax(0,1fr)_20rem]">
          <div className="flex flex-col gap-5">
            <Segmented aria-label="Tab" options={TABS} value={tab} onChange={setTab} />
            {tab === "overview" ? (
              <OverviewTab
                nodeName={nodeName}
                cells={cells}
                zone={node?.zone}
                ready={node?.ready}
                agentId={agent?.id}
                podIP={agent?.podIP}
              />
            ) : (
              <DiagnosticsTab nodeName={nodeName} />
            )}
          </div>
          <RecentChanges scope={nodeName} />
        </div>
      )}
    </PageShell>
  );
}
