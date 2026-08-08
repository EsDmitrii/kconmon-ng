import { useMemo, useState } from "react";
import {
  Background,
  Controls,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  type Edge,
  type Node,
  type NodeProps,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { Network } from "lucide-react";
import { PageShell } from "@/components/page-shell";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { useMatrix } from "@/hooks/use-matrix";
import { useTopology } from "@/hooks/use-topology";
import { useTheme } from "@/components/theme-provider";
import { goTo } from "@/lib/api";
import type { Matrix, Topology } from "@/lib/types";

import { cn } from "@/lib/utils";

const ZONE_W = 300;
const NODE_H = 64;

/* Edge policy — Progressive Disclosure. Every cell with fail ≥ 1% is a
   candidate, but drawing all of them collapses into spaghetti the moment a
   node degrades (N² paths over the same handles). So: worst EDGE_CAP paths by
   failure ratio, no permanent labels (the percentage appears on hover), and
   the toolbar says honestly how many were drawn out of how many qualified. */
const EDGE_CAP = 10;

export function buildFlow(
  topo: Topology,
  matrix?: Matrix,
): { nodes: Node[]; edges: Edge[]; problemTotal: number } {
  const zones = [...new Set(topo.nodes.map((n) => n.zone))].sort();
  const worstOut = new Map<string, number>();
  for (const c of matrix?.cells ?? []) {
    if (c.failRatio === null) continue;
    worstOut.set(c.source, Math.max(worstOut.get(c.source) ?? 0, c.failRatio));
  }

  const nodes: Node[] = zones.map((z, i) => {
    const count = topo.nodes.filter((n) => n.zone === z).length;
    return {
      id: `zone:${z}`,
      type: "zone",
      position: { x: i * (ZONE_W + 80), y: 0 },
      data: { label: z, count },
      className: "topo-zone",
      style: { width: ZONE_W, height: 64 + count * NODE_H },
    };
  });

  for (const z of zones) {
    topo.nodes.filter((n) => n.zone === z).forEach((n, j) => {
      const fail = worstOut.get(n.name) ?? 0;
      const health = !n.ready ? "failing" : fail >= 0.1 ? "failing" : fail >= 0.01 ? "degraded" : "ok";
      nodes.push({
        id: n.name,
        type: "topoNode",
        parentId: `zone:${z}`,
        extent: "parent",
        position: { x: 20, y: 52 + j * NODE_H },
        data: { label: n.name, ready: n.ready, health },
        className: `topo-node topo-node--${health}`,
      });
    });
  }

  // Only draw edges whose both endpoints are real topology nodes, and never a
  // self-loop — a matrix cell can reference a name absent from topo.nodes
  // (stale series) which would make React Flow log a missing-endpoint warning.
  const known = new Set(topo.nodes.map((n) => n.name));
  const problems = (matrix?.cells ?? [])
    .filter(
      (c) =>
        c.failRatio !== null &&
        c.failRatio >= 0.01 &&
        c.source !== c.destination &&
        known.has(c.source) &&
        known.has(c.destination),
    )
    .sort((a, b) => (b.failRatio ?? 0) - (a.failRatio ?? 0));

  const edges: Edge[] = problems.slice(0, EDGE_CAP).map((c) => ({
    id: `${c.source}->${c.destination}`,
    source: c.source,
    target: c.destination,
    type: "smoothstep",
    animated: false,
    data: { failLabel: `${(100 * (c.failRatio ?? 0)).toFixed(0)}%` },
    markerEnd: { type: MarkerType.ArrowClosed, width: 14, height: 14 },
    className: (c.failRatio ?? 0) >= 0.1 ? "topo-edge--failing" : "topo-edge--degraded",
  }));

  return { nodes, edges, problemTotal: problems.length };
}

/**
 * nodeNavigationPath decides the click destination for one React Flow node
 * (task-25-brief.md: clicking a topology node navigates to /nodes/<name>).
 * Pulled out as a pure function, tested directly, rather than only inline in
 * onNodeClick below -- @xyflow/react needs ResizeObserver, which jsdom does
 * not provide (this repo's test environment has no polyfill for it either),
 * so a full page render+click here is not viable; this is the actual
 * decision logic that render makes, exercised without React Flow at all.
 * Only `topoNode` ids are real node names -- a `zone:<name>` container click
 * must not navigate anywhere.
 */
export function nodeNavigationPath(node: Pick<Node, "id" | "type">): string | undefined {
  return node.type === "topoNode" ? `/nodes/${encodeURIComponent(node.id)}` : undefined;
}

/* Custom node renderers. The wrapper element already carries the .topo-zone /
   .topo-node classes from buildFlow, so these only render the content; the
   handles are invisible anchors at fixed sides so parallel edges never
   collapse onto one point. */
function ZoneNode({ data }: NodeProps) {
  const d = data as { label: string; count: number };
  return (
    <div className="topo-zone__label">
      {d.label} · {d.count} node{d.count === 1 ? "" : "s"}
    </div>
  );
}

function TopoNode({ data }: NodeProps) {
  const d = data as { label: string; ready: boolean };
  return (
    <>
      <Handle type="target" position={Position.Left} className="!size-1.5 !opacity-0" />
      <span className="min-w-0 flex-1 truncate" title={d.label}>
        {d.label}
      </span>
      {d.ready === false ? <Badge variant="unknown">not ready</Badge> : null}
      <Handle type="source" position={Position.Right} className="!size-1.5 !opacity-0" />
    </>
  );
}

const NODE_TYPES = { zone: ZoneNode, topoNode: TopoNode };

const LEGEND = [
  { dot: "bg-health-ok", label: "Healthy" },
  { dot: "bg-health-warn", label: "Degraded · worst path 1–10%" },
  { dot: "bg-health-bad", label: "Failing · ≥ 10% or not ready" },
] as const;

/**
 * unfoldableEmpty is the Time Machine's honest empty state, DERIVED from the
 * response rather than assumed.
 *
 * Deriving it is what made this survive the controller learning to attribute
 * its topology events (M7). Before that, every event carried a reason and no
 * node identity, so any fold came back empty; now events name their agent, node
 * and zone and history reconstructs for real. The only history that still folds
 * to nothing is what an OLDER controller wrote and retention has not yet cut
 * away — a shrinking window, not a permanent property. A hardcoded "your
 * history is probably empty" would have had to be deleted; a note driven by the
 * server's own two numbers — `unfoldableEvents` counted, `eventsFolded` folded
 * — simply stops appearing on its own.
 *
 * The three conditions are all load-bearing: `historical` (a live body has none
 * of these fields), an EMPTY node set (a fold that produced nodes is a success
 * whatever it also skipped), and at least one unfoldable event (with zero, the
 * honest answer is simply "nothing had happened yet" — a different note).
 */
export function unfoldableEmpty(
  topo: Topology | undefined,
): { unfoldableEvents: number; eventsFolded: number } | null {
  if (!topo?.historical) return null;
  if (topo.nodes.length > 0) return null;
  const unfoldableEvents = topo.unfoldableEvents ?? 0;
  if (unfoldableEvents === 0) return null;
  return { unfoldableEvents, eventsFolded: topo.eventsFolded ?? 0 };
}

export function TopologyPage() {
  const topo = useTopology();
  const matrix = useMatrix("tcp");
  const { theme } = useTheme();
  const [hoveredEdge, setHoveredEdge] = useState<string | null>(null);
  const unfoldable = unfoldableEmpty(topo.data);

  const flow = useMemo(
    () => (topo.data ? buildFlow(topo.data, matrix.data) : { nodes: [], edges: [], problemTotal: 0 }),
    [topo.data, matrix.data],
  );

  // The percentage label exists only while its edge is hovered — Progressive
  // Disclosure instead of the permanent label pile-up this page used to have.
  const edges = useMemo(
    () =>
      flow.edges.map((e) =>
        e.id === hoveredEdge ? { ...e, label: (e.data as { failLabel: string }).failLabel } : e,
      ),
    [flow.edges, hoveredEdge],
  );

  const shown = Math.min(flow.edges.length, flow.problemTotal);

  return (
    <PageShell
      title="Topology"
      description={
        topo.data?.asOf
          ? `Zone/node map as of ${new Date(topo.data.asOf).toLocaleString()}, reconstructed from topology events. Problem paths (TCP fail ≥ 1%) are drawn worst-first; hover an edge for its failure ratio.`
          : "Live zone/node map. Problem paths (TCP fail ≥ 1%) are drawn worst-first; hover an edge for its failure ratio."
      }
    >
      {topo.error ? (
        <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
          <p className="text-sm font-medium">Topology is unavailable</p>
          <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{topo.error.message}</p>
        </Card>
      ) : null}

      {/* The fold hit its own bound, so the node set below is built from a
          SUFFIX of the history — say so rather than presenting a partial
          reconstruction as a complete one. */}
      {topo.data?.truncated ? (
        <Card role="status" className="border-l-4 border-l-health-warn bg-health-warn-soft/40 p-5">
          <p className="text-sm font-medium">This reconstruction is incomplete</p>
          <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">
            More topology events precede this instant than the fold will read in one pass, so nodes whose only event is
            older than the window may be missing from the map.
          </p>
        </Card>
      ) : null}

      {topo.isLoading && !topo.data ? (
        <Card role="status" aria-live="polite" className="p-6">
          <span className="sr-only">Loading topology…</span>
          <div className="flex gap-6">
            {[0, 1, 2].map((i) => (
              <div key={i} className="flex flex-1 flex-col gap-3">
                <Skeleton className="h-3 w-24" />
                <Skeleton className="h-12 w-full" />
                <Skeleton className="h-12 w-full" />
              </div>
            ))}
          </div>
        </Card>
      ) : null}

      {unfoldable ? (
        <Card className="flex flex-col items-center gap-3 px-6 py-14 text-center">
          <span
            aria-hidden="true"
            className="flex size-12 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
          >
            <Network className="size-5" />
          </span>
          <p className="text-sm font-medium">Nothing to reconstruct at this time</p>
          <p className="max-w-prose text-xs leading-relaxed text-muted-foreground">
            {`This console found ${unfoldable.unfoldableEvents} topology event${
              unfoldable.unfoldableEvents === 1 ? "" : "s"
            } at or before this instant and could fold ${unfoldable.eventsFolded} of them into a node set: `}
            the rest name no node, so there is nothing to rebuild the map from. Those were recorded before the
            controller started attributing topology changes, so that stretch of history cannot be reconstructed — pick
            a more recent instant, where events name the node they are about.
          </p>
        </Card>
      ) : null}

      {topo.data && !unfoldable && topo.data.nodes.length === 0 ? (
        <Card className="flex flex-col items-center gap-3 px-6 py-14 text-center">
          <span
            aria-hidden="true"
            className="flex size-12 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
          >
            <Network className="size-5" />
          </span>
          <p className="text-sm font-medium">
            {topo.data.historical ? "No nodes existed at this time" : "No nodes reported by the controller yet"}
          </p>
          <p className="max-w-sm text-xs leading-relaxed text-muted-foreground">
            {topo.data.historical
              ? "No topology event at or before this instant put a node into the cluster — try a later time, or check that it is inside console.database.retentionDays."
              : "Nodes appear as soon as agents register with the controller — check that the DaemonSet is running and the controller is reachable."}
          </p>
        </Card>
      ) : null}

      {topo.data && topo.data.nodes.length > 0 ? (
        <div className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-x-5 gap-y-2 text-xs text-muted-foreground">
            {LEGEND.map((l) => (
              <span key={l.label} className="flex items-center gap-1.5">
                <span aria-hidden="true" className={cn("size-2.5 rounded-full", l.dot)} />
                {l.label}
              </span>
            ))}
            <span className="nums ml-auto" data-testid="edge-caption">
              {flow.problemTotal === 0
                ? "no problem paths right now"
                : flow.problemTotal > shown
                  ? `showing ${shown} worst of ${flow.problemTotal} problem paths`
                  : `${shown} problem path${shown === 1 ? "" : "s"}`}
            </span>
          </div>

          <Card className="overflow-hidden p-0">
            <div className="h-[calc(100vh-19rem)] min-h-[420px]">
              <ReactFlow
                nodes={flow.nodes}
                edges={edges}
                nodeTypes={NODE_TYPES}
                colorMode={theme}
                fitView
                fitViewOptions={{ padding: 0.15 }}
                onNodeClick={(_, node) => {
                  const path = nodeNavigationPath(node);
                  if (path) goTo(path);
                }}
                onEdgeMouseEnter={(_, e) => setHoveredEdge(e.id)}
                onEdgeMouseLeave={() => setHoveredEdge(null)}
                proOptions={{ hideAttribution: true }}
              >
                <Background gap={24} size={1.5} />
                <Controls showInteractive={false} />
              </ReactFlow>
            </div>
          </Card>
        </div>
      ) : null}
    </PageShell>
  );
}
