import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { AnnotationBar, useAnnotations } from "@/components/annotations";
import { InvestigateLink, RelatedIncidents } from "@/components/investigate-entry";
import { PageShell } from "@/components/page-shell";
import { RecentChanges } from "@/components/recent-changes";
import { Badge, type BadgeProps } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Segmented } from "@/components/ui/segmented";
import { Skeleton } from "@/components/ui/skeleton";
import { useMatrix } from "@/hooks/use-matrix";
import { useTopology } from "@/hooks/use-topology";
import { getRun, getRuns } from "@/lib/api";
import type { InvestigationScope } from "@/lib/investigation-sources";
import { DEGRADED_AT, FAILING_AT, isMeasured, severityRatio } from "@/lib/matrix-cells";
import { useTimeContext } from "@/lib/timemachine";
import { PROTOCOLS, type MatrixCell, type Protocol, type RunDetail } from "@/lib/types";
import { cn, runsAtOrBefore } from "@/lib/utils";

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
 * OUTBOUND severity this node reports in the matrix — the same worst-case
 * semantics topology.tsx's buildFlow already uses for node colouring, so a
 * node's tier reads identically whether an operator arrived here from
 * Topology or a bookmark. Self-cells are excluded (a node never reports a
 * pair against itself); `ready === false` overrides the ratio and always
 * reads "bad", same as buildFlow.
 *
 * MEASURED and SCORED are two different questions and the return type carries
 * both answers (QA round 2, finding #1). The old filter — `failRatio !== null`
 * — asked only the second and answered the first with it, so a node whose
 * every pair reported latency and had never failed read "No data" in its own
 * header. Now: no measured pair at all is "unknown"; measured pairs with no
 * ratio to rank are healthy WITHOUT a percentage (there is no failure ratio to
 * subtract from 100, and inventing one would be the fabrication); and packet
 * loss can carry the tier on its own.
 */
export function nodeHealth(
  ready: boolean | undefined,
  cells: MatrixCell[],
  nodeName: string,
): { percent: number | null; tier: Tier } {
  const outbound = cells.filter((c) => c.source === nodeName && c.destination !== nodeName && isMeasured(c));
  const ratios = outbound.map(severityRatio).filter((r): r is number => r !== null);
  const worst = ratios.length > 0 ? Math.max(...ratios) : null;
  const percent = worst === null ? null : Math.max(0, 100 * (1 - worst));
  if (ready === false) return { percent, tier: "bad" };
  if (outbound.length === 0) return { percent: null, tier: "unknown" };
  if (worst === null) return { percent: null, tier: "ok" };
  const tier: Tier = worst >= FAILING_AT ? "bad" : worst >= DEGRADED_AT ? "warn" : "ok";
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

/** READY_SOURCE_NOTE is why this one field cannot fall back to the agent the
 *  way Zone does: readiness is a KUBERNETES NODE condition, read by the
 *  controller's node informer. A registered agent proves a pod is running, not
 *  that the node it sits on is Ready — so with no node entry the honest answer
 *  is an em-dash that says where the fact would have come from. */
const READY_SOURCE_NOTE = "node readiness comes from the Kubernetes node informer";

function OverviewTab({
  nodeName,
  cells,
  zone,
  ready,
  agentId,
  podIP,
  topologyProblem,
}: {
  nodeName: string;
  cells: MatrixCell[];
  zone?: string;
  ready?: boolean;
  agentId?: string;
  podIP?: string;
  /** The topology query's own failure detail, when it FAILED. Absent for a
   *  successful read — including a successful read that simply does not know
   *  this node. */
  topologyProblem?: string;
}) {
  return (
    <div className="flex flex-col gap-5">
      <Card className="p-5">
        <h3 className="text-sm font-semibold">Agent identity</h3>
        {/* Four em-dashes are the answer to "the topology knows nothing about
            this node". They are NOT the answer to "the topology request
            failed" — that reads as a node that exists and has no identity,
            when in fact nobody was asked and the server said why (QA round 2,
            finding #3). One honest line, carrying the problem verbatim. */}
        {topologyProblem !== undefined ? (
          <p data-testid="identity-problem" className="mt-3 text-xs leading-relaxed text-muted-foreground">
            {topologyProblem}
          </p>
        ) : (
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
              <dd className="mt-0.5" title={ready === undefined ? READY_SOURCE_NOTE : undefined}>
                {ready === undefined ? "—" : ready ? "yes" : "no"}
              </dd>
            </div>
          </dl>
        )}
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
  const { at } = useTimeContext();
  const runsQuery = useQuery({ queryKey: ["runs", "recent-scan"], queryFn: () => getRuns({ limit: RUN_SCAN_LIMIT }) });
  /* Cut to the viewed instant BEFORE fetching details: a run that started
     after `t` has no place in a view of `t`, and it should not cost a GET
     either. GET /api/v1/runs has no time filter to push this down to — see
     runsAtOrBefore's own note. */
  const ids = useMemo(
    () => runsAtOrBefore(runsQuery.data?.runs ?? [], at).map((r) => r.id),
    [runsQuery.data, at],
  );
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
  const { at } = useTimeContext();

  return (
    <Card asChild className="overflow-hidden p-0">
      <section>
        <div className="border-b border-border px-4 py-3">
          <h3 className="text-sm font-semibold">Runs touching this node</h3>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
            No server-side node filter on GET /api/v1/runs yet — this scans the most recent {RUN_SCAN_LIMIT} runs'
            results client-side. An older run touching this node may exist but will not show up here
            {/* The engaged half of the same limitation: the endpoint has no
                time filter either, so the page it returns is the newest page
                NOW and the cut to `t` happens here. Both bounds compose, and
                saying only the first one would understate what is missing. */}
            {at ? ", and only runs started at or before the viewed instant are listed" : ""}.
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

/* NODE_ANNOTATION_RANGE_SECONDS is a day, not an hour, and it is a choice this
   card has to make alone: unlike Explore, the pair card and the target card,
   the node card has NO chart and therefore no plotted window to inherit. A day
   is what an operator arriving at a node after an incident is looking back
   over. */
const NODE_ANNOTATION_RANGE_SECONDS = 24 * 60 * 60;

/**
 * NodeAnnotations is this card's annotation surface. It is a LIST rather than a
 * chart overlay for the plain reason that this page draws no chart at all — the
 * node card is identity, a breakdown table and a run scan. The same marks show
 * up as markLine/markArea wherever a chart's window covers them (the pair card
 * for a pair this node is an endpoint of, Explore for the global ones); here
 * they are what they are, notes with times.
 */
function NodeAnnotations({ nodeName }: { nodeName: string }) {
  const { annotations, error, refresh } = useAnnotations(nodeName, NODE_ANNOTATION_RANGE_SECONDS);
  return (
    <Card asChild className="p-5">
      <section>
        <h3 className="text-sm font-semibold">Annotations</h3>
        <p className="mt-1 text-xs leading-relaxed text-muted-foreground">
          Notes pinned to this node over the last 24 hours, plus the fleet-wide ones.
        </p>
        <AnnotationBar scope={nodeName} annotations={annotations} error={error} onChanged={() => void refresh()} />
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
  const { at } = useTimeContext();
  const topo = useTopology();
  const [protocol, setProtocol] = useState<Protocol>("tcp");
  const matrix = useMatrix(protocol);
  const [tab, setTab] = useState<NodeTab>("overview");

  if (nodeName === "") return <NotFound nodeName={nodeName} />;

  const node = topo.data?.nodes.find((n) => n.name === nodeName);
  const agent = topo.data?.agents.find((a) => a.nodeName === nodeName);
  /* Zone falls back to the AGENT's own copy (QA round 2, finding #4). The two
     collections are filled from different places — `nodes` from the
     controller's Kubernetes node informer, `agents` from what each DaemonSet
     pod reported — and a controller outside a cluster (or without node
     permissions) answers with agents and no nodes. The agent carries the zone
     it registered with, so the field has an answer; readiness does not, and
     stays an em-dash with READY_SOURCE_NOTE on it rather than being guessed
     from "an agent is registered". */
  const zone = node?.zone ?? agent?.zone;
  const cells = matrix.data?.cells ?? [];
  const health = nodeHealth(node?.ready, cells, nodeName);
  const loadingIdentity = (topo.isLoading && !topo.data) || (matrix.isLoading && !matrix.data);
  const investigationScope: InvestigationScope = { kind: "node", a: nodeName, b: "" };

  return (
    <PageShell
      title={nodeName}
      /* The card's whole body — identity from useTopology, health from
         useMatrix, both already resolved through the Time Machine — is
         state-as-of-t while engaged, and the description is where that is said
         once rather than on each panel. asOf comes from the topology response
         (the server's own echo of the instant it folded to) and falls back to
         the requested instant when this console has no historical topology to
         answer with at all. */
      description={
        at
          ? `${zone ? `Zone ${zone} · ` : ""}state as of ${new Date(topo.data?.asOf ?? at).toLocaleString()}`
          : zone
            ? `Zone ${zone}`
            : "Node"
      }
      actions={
        <>
          <Segmented
            aria-label="Protocol"
            options={PROTOCOLS.map((p) => ({ value: p, label: p.toUpperCase() }))}
            value={protocol}
            onChange={setProtocol}
          />
          {/* No percentage, no sentence. "— healthy" read as a claim with a
              missing number in front of it; the badge beside it already says
              the state in words, and it is the honest one (QA round 2, #16). */}
          {health.percent === null ? null : (
            <span className="nums text-sm text-muted-foreground">{health.percent.toFixed(1)}% healthy</span>
          )}
          <Badge variant={TIER_VARIANT[health.tier]} dot>
            {TIER_LABEL[health.tier]}
          </Badge>
          {/* The entry point into Investigation Mode (plan Decision 11): the
              URL is the whole contract, built by the one helper the matrix and
              the other two cards also use. */}
          <InvestigateLink scope={investigationScope} />
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
                zone={zone}
                ready={node?.ready}
                agentId={agent?.id}
                podIP={agent?.podIP}
                topologyProblem={topo.error?.message}
              />
            ) : (
              <DiagnosticsTab nodeName={nodeName} />
            )}
          </div>
          <div className="flex flex-col gap-5">
            <RelatedIncidents scope={investigationScope} />
            {/* scopeNode, not scope: an equality filter on the bare node name
                never saw the pair-scoped rows ("node-a→node-b") that every
                check run and path change writes, so the rail looked idle on a
                busy node (QA scope 2 #21). */}
            <RecentChanges scopeNode={nodeName} />
            <NodeAnnotations nodeName={nodeName} />
          </div>
        </div>
      )}
    </PageShell>
  );
}
