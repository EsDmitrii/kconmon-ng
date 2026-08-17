import { useMemo, useState } from "react";
import {
  Background,
  ControlButton,
  Controls,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  useReactFlow,
  type Edge,
  type Node,
  type NodeProps,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { Maximize, Minus, Network, Plus } from "lucide-react";
import { PageShell } from "@/components/page-shell";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { useRouter } from "@tanstack/react-router";
import { useMatrix } from "@/hooks/use-matrix";
import { useTopology } from "@/hooks/use-topology";
import { useTheme } from "@/components/theme-provider";
import { goTo } from "@/lib/api";
import { localeTag, useLocale, useT, type Translate } from "@/lib/i18n";
import { enT, topologyDict, type TopologyKey } from "@/lib/i18n/dict/topology";
import { DEGRADED_AT, FAILING_AT, fmtRatio, isProblemCell, severityRatio } from "@/lib/matrix-cells";
import { compareNaturalName } from "@/lib/natural-name";
import { formatAtParam, useTimeContext } from "@/lib/timemachine";
import type { Matrix, MatrixCell, Topology } from "@/lib/types";
/* The matrix payload gate, imported from the page that owns the matrix rather
   than re-stated here: this map colours its boxes and draws its edges from the
   same cells the grid does, and a figure the grid refuses to print is not one
   this map may put on an edge label. */
import { measuredCells } from "./matrix";

import { cn } from "@/lib/utils";

/* A zone is a GRID of node boxes, not a single column.
   One column per zone made the drawing tall and thin, so React Flow's fitView had to solve for
   height: it settled well under scale 1, shrank every label, and left about four fifths of the
   canvas blank. The column count grows with the fleet (roughly square, capped) so a zone stays a
   shape a pane can hold. */
const NODE_W = 260;
const NODE_GAP = 20;
const ZONE_PAD = 20;
const NODE_H = 64;
const NODE_MAX_W = NODE_W;
/** ZONE_MAX_COLS keeps a big zone from becoming a wide, one-row strip — the opposite failure. */
const ZONE_MAX_COLS = 4;

/** zoneColumns is how many node boxes a zone lays out side by side. */
function zoneColumns(count: number): number {
  if (count <= 1) return 1;
  return Math.min(ZONE_MAX_COLS, Math.max(1, Math.ceil(Math.sqrt(count))));
}

/** zoneWidth is the box a zone of `count` nodes draws at. */
function zoneWidth(count: number): number {
  const cols = zoneColumns(count);
  return ZONE_PAD * 2 + cols * NODE_W + (cols - 1) * NODE_GAP;
}

/* Edge policy — Progressive Disclosure. */
const EDGE_CAP = 10;

/* NUL, the same separator pages/matrix.tsx keys its cell map on: a composite key
   built with a printable separator is only unambiguous while no name contains
   it. Internal only — it never reaches the DOM. */
const pairKey = (source: string, destination: string) => `${source}\0${destination}`;

/**
 * edgeId is the DOM-visible half of the same question. React Flow writes this id
 * into `data-id` and keys the edge on it, so it cannot carry a NUL — and a bare
 * `${source}->${destination}` collided the moment a name contained the arrow
 * ("a->b" → "c" and "a" → "b->c" are both "a->b->c"). Percent-encoding each half
 * makes the join injective and leaves the ordinary case ("n1->n2") untouched.
 */
const edgeId = (source: string, destination: string) =>
  `${encodeURIComponent(source)}->${encodeURIComponent(destination)}`;

/** health → the word the node's aria-label announces. "ok" is spoken as
 *  "healthy" rather than in the class name's vocabulary (QA round 2, #22). */
const HEALTH_KEYS: Readonly<Record<"ok" | "degraded" | "failing", TopologyKey>> = {
  ok: "health.ok",
  degraded: "health.degraded",
  failing: "health.failing",
};

/**
 * MapNode is one box this map draws. `ready` is `undefined` — not `false` —
 * when the box came from an AGENT: readiness is a Kubernetes node condition,
 * and a registered agent is not evidence of it either way.
 */
export interface MapNode {
  name: string;
  zone: string;
  ready: boolean | undefined;
}

/** Which half of the topology response the boxes were built from. */
export type MapSource = "nodes" | "agents";

/**
 * mapNodes is what this page can honestly DRAW, which is not the same question
 * as "did the controller see Kubernetes".
 *
 * Off-cluster (or without node permissions) `nodes` comes back empty while
 * `agents` is full, and every agent carries the two fields a lane needs: the
 * node it runs on and the zone it registered with. Drawing that is the same
 * move the Overview's own nodesTile already makes for its count — one agent per
 * node (DaemonSet), so distinct nodeNames IS the node set. What does NOT
 * survive the fallback is readiness, and `ready: undefined` is how that is
 * carried rather than guessed (QA scope 2, findings #1 and #2).
 */
/* The controller answers in REGISTRATION order — whichever agent happened to
   come up first — so a lane redrew itself differently after every rollout and no
   node was ever where it had been (owner report). lib/natural-name is the fleet's
   one reading order. */

/** A zone name is printed verbatim, so it has to BE a string; a node whose zone
 *  label the informer never set comes through as "" (and a reconstructed one can
 *  come through missing entirely, which used to print the word "undefined" into
 *  a lane header). Both land on the same unnamed-lane case, resolved to a word
 *  in buildFlow where the dictionary is. */
const zoneOf = (zone: unknown): string => (typeof zone === "string" ? zone : "");

export function mapNodes(topo: Topology | undefined): { nodes: MapNode[]; source: MapSource } {
  if (!topo) return { nodes: [], source: "nodes" };
  const sorted = (nodes: MapNode[]) => nodes.sort((a, b) => compareNaturalName(a.name, b.name));
  /* Deduplicated BY NAME, the way the agents branch below already was. A node
     name is this map's React Flow id and its /nodes/{name} link, so a repeated
     one is not a second box — it is two boxes React Flow cannot tell apart,
     with a duplicate-key warning for each. The first wins, which is the same
     rule the agents branch keeps. */
  const collect = (rows: readonly MapNode[]) => {
    const byName = new Map<string, MapNode>();
    for (const r of rows) {
      if (typeof r.name !== "string" || r.name === "" || byName.has(r.name)) continue;
      byName.set(r.name, { name: r.name, zone: zoneOf(r.zone), ready: r.ready });
    }
    return sorted([...byName.values()]);
  };

  const rows = (xs: unknown): { name: unknown; zone: unknown; ready?: unknown }[] =>
    Array.isArray(xs) ? xs.filter((x) => x !== null && typeof x === "object") : [];

  const nodes = collect(
    rows(topo.nodes).map((n) => ({ name: n.name, zone: n.zone, ready: n.ready }) as MapNode),
  );
  if (nodes.length > 0) return { nodes, source: "nodes" };

  return {
    nodes: collect(
      rows(topo.agents).map(
        (a) => ({ name: (a as { nodeName?: unknown }).nodeName, zone: a.zone, ready: undefined }) as MapNode,
      ),
    ),
    source: "agents",
  };
}

/**
 * buildFlow takes its TRANSLATOR as an argument; it is a pure function with direct unit tests, so
 * it cannot call a hook.
 */
export function buildFlow(
  topo: Topology,
  matrix?: Matrix,
  t: Translate<TopologyKey> = enT,
): { nodes: Node[]; edges: Edge[]; problemTotal: number; source: MapSource } {
  const { nodes: mapped, source } = mapNodes(topo);
  const zones = [...new Set(mapped.map((n) => n.zone))].sort();
  /* The lane's HEADING, which is not always the lane's key: a node the informer
     never gave a zone label sits in the "" lane, and a header reading « · 3
     nodes» names nothing at all. The id stays the raw string — it is a key, not
     a word — and only what is READ gets the fallback. */
  /* The word "zone" belongs to the LABEL, not to the value: a real zone reads
     "zone eu-1", an absent one reads "no zone reported" and must not become
     "zone no zone reported". */
  const zoneLabel = (z: string) => (z === "" ? t("zone.none") : z);
  const zoneSpoken = (z: string) => (z === "" ? t("zone.none") : t("node.aria.zone", { zone: z }));
  /* The figures are read through pages/matrix.tsx's gate, exactly as the grid
     reads them: a null, a string or the Infinity a JSON `1e999` parses into is
     not a measurement, and an edge labelled "NaN%" is worse than no edge. */
  const cells = measuredCells(matrix?.cells);
  /* severityRatio, not failRatio: on a fleet that has never failed the fail-ratio series has no samples at all. */
  const worstOut = new Map<string, number>();
  for (const c of cells) {
    const r = severityRatio(c);
    if (r === null) continue;
    worstOut.set(c.source, Math.max(worstOut.get(c.source) ?? 0, r));
  }

  // Zones are laid left to right, each as wide as its own grid needs.
  let zoneX = 0;
  const nodes: Node[] = zones.map((z) => {
    const count = mapped.filter((n) => n.zone === z).length;
    const width = zoneWidth(count);
    const rows = Math.ceil(count / zoneColumns(count));
    const node: Node = {
      id: `zone:${z}`,
      type: "zone",
      position: { x: zoneX, y: 0 },
      data: { label: zoneLabel(z), count },
      className: "topo-zone",
      style: { width, height: 64 + rows * NODE_H },
    };
    zoneX += width + 80;
    return node;
  });

  for (const z of zones) {
    const zoneNodes = mapped.filter((n) => n.zone === z);
    const cols = zoneColumns(zoneNodes.length);
    zoneNodes.forEach((n, j) => {
      const fail = worstOut.get(n.name) ?? 0;
      /* `ready === false` is the only readiness that can condemn a node.
         `undefined` (the agents-built map) leaves the verdict to the matrix,
         which is the one signal that survives the fallback. */
      const health =
        n.ready === false ? "failing" : fail >= FAILING_AT ? "failing" : fail >= DEGRADED_AT ? "degraded" : "ok";
      nodes.push({
        id: n.name,
        type: "topoNode",
        parentId: `zone:${z}`,
        extent: "parent",
        position: { x: ZONE_PAD + (j % cols) * (NODE_W + NODE_GAP), y: 52 + Math.floor(j / cols) * NODE_H },
        data: { label: n.name, ready: n.ready, health },
        /* The map is a picture, and a picture with no text is nothing to a screen reader. */
        ariaLabel: t(
          n.ready === undefined ? "node.aria.readyUnknown" : n.ready ? "node.aria" : "node.aria.notReady",
          {
            node: n.name,
            zone: zoneSpoken(z),
            health: t(HEALTH_KEYS[health]),
          },
        ),
        className: `topo-node topo-node--${health}`,
        style: { maxWidth: NODE_MAX_W },
      });
    });
  }

  // Only draw edges whose both endpoints are boxes on this map, and never a self-loop.
  const known = new Set(mapped.map((n) => n.name));
  /*
   * ONE edge per ordered pair, keeping the worst reading of it. Two cells for
   * the same A→B is a matrix the console did not write and cannot arbitrate, and
   * drawing both produced two React Flow edges under one id — a duplicate key,
   * and a "capped 10 of 12" caption counting the same path twice. Worst-of
   * rather than last-wins, which is the rule every other severity read here uses.
   */
  const worstPair = new Map<string, MatrixCell>();
  for (const c of cells) {
    if (!isProblemCell(c) || c.source === c.destination) continue;
    if (!known.has(c.source) || !known.has(c.destination)) continue;
    const key = pairKey(c.source, c.destination);
    const prev = worstPair.get(key);
    if (!prev || (severityRatio(c) ?? 0) > (severityRatio(prev) ?? 0)) worstPair.set(key, c);
  }
  const problems = [...worstPair.values()].sort((a, b) => (severityRatio(b) ?? 0) - (severityRatio(a) ?? 0));

  const drawn = problems.slice(0, EDGE_CAP);
  /*
   * A→B and B→A are two independent measurements and both get drawn; applied ONLY to a mutual pair
   * — a lone edge stays on the default routing.
   */
  const drawnKeys = new Set(drawn.map((c) => pairKey(c.source, c.destination)));
  const edges: Edge[] = drawn.map((c) => {
    const ratio = severityRatio(c) ?? 0;
    const mutual = drawnKeys.has(pairKey(c.destination, c.source));
    const forward = c.source < c.destination;
    return {
      id: edgeId(c.source, c.destination),
      source: c.source,
      target: c.destination,
      type: "smoothstep",
      animated: false,
      ...(mutual
        ? { pathOptions: { offset: forward ? 16 : 44, stepPosition: forward ? 0.35 : 0.65, borderRadius: 10 } }
        : {}),
      // The label names the vector it came from: a path drawn because of
      // packet loss must not read as a failure percentage.
      data: {
        /* ONE formatter, the same the grid and the cards use. toFixed(0) here printed a path measured
           at 9.6% as "10%" while the edge was drawn (and the legend read) as degraded, because the
           threshold compares 0.096 < 0.1 -- an operator saw a 10% edge in the sub-10% colour. It also
           collapsed 1.0%, 1.4% and 1.9% into one indistinguishable "1%". */
        failLabel: t(c.failRatio === null ? "edge.label.loss" : "edge.label", {
          pct: fmtRatio(ratio),
        }),
      },
      /* React Flow writes ariaLabel onto the edge's own <g>, which is already a
         tab stop. Without it the ratio lived only in a hover tooltip, so the one
         number the edge exists to carry was unreachable from the keyboard and
         unspoken by a screen reader. */
      ariaLabel: t("edge.aria", {
        source: c.source,
        destination: c.destination,
        detail: t(c.failRatio === null ? "edge.label.loss" : "edge.label", { pct: fmtRatio(ratio) }),
      }),
      markerEnd: { type: MarkerType.ArrowClosed, width: 14, height: 14 },
      className: ratio >= FAILING_AT ? "topo-edge--failing" : "topo-edge--degraded",
    };
  });

  return { nodes, edges, problemTotal: problems.length, source };
}

/**
 * nodeNavigationPath decides the click destination for one React Flow node; pulled out as a pure
 * function, tested directly.
 */
export function nodeNavigationPath(node: Pick<Node, "id" | "type">, at?: Date | null): string | undefined {
  if (node.type !== "topoNode") return undefined;
  const path = `/nodes/${encodeURIComponent(node.id)}`;
  return at ? `${path}?at=${encodeURIComponent(formatAtParam(at))}` : path;
}

/* Custom node renderers; the wrapper element already carries the .topo-zone / .topo-node classes from buildFlow. */
function ZoneNode({ data }: NodeProps) {
  const t = useT(topologyDict);
  const d = data as { label: string; count: number };
  return (
    <div className="topo-zone__label">
      {t(d.count === 1 ? "zone.one" : "zone.many", { zone: d.label, count: d.count })}
    </div>
  );
}

function TopoNode({ data }: NodeProps) {
  const t = useT(topologyDict);
  /* `ready` is undefined on the agents-built map — the box carries no badge
     there, and the readiness gap is stated once, above the map. */
  const d = data as { label: string; ready: boolean | undefined; health: "ok" | "degraded" | "failing" };
  return (
    <>
      <Handle type="target" position={Position.Left} className="!size-1.5 !opacity-0" />
      {/* min-w-0 is what lets `truncate` actually bite inside a flex row, and
          the box itself is capped at NODE_MAX_W (index.css) — together they
          keep a 60-character node name inside its zone lane, with the whole
          name on `title`. */}
      <span className="min-w-0 flex-1 truncate" title={d.label}>
        {d.label}
      </span>
      {/* The health is a WORD as well as a border colour. The colour was the only channel: under
          deuteranopia healthy and failing simulate to 1.16:1 luminance, so a red/green-blind
          operator could not tell one box from another anywhere on the map — and the legend above
          maps the same three hues to words, which does not help. `ok` stays unlabelled: a map where
          every box carries a chip is a map of chips, and the two states worth naming are the two
          that mean something is wrong. */}
      {d.health !== "ok" ? (
        <Badge variant={d.health === "failing" ? "bad" : "warn"}>{t(HEALTH_KEYS[d.health])}</Badge>
      ) : null}
      {d.ready === false ? <Badge variant="unknown">{t("node.notReady")}</Badge> : null}
      <Handle type="source" position={Position.Right} className="!size-1.5 !opacity-0" />
    </>
  );
}

const NODE_TYPES = { zone: ZoneNode, topoNode: TopoNode };

/* The dot's colour and the row order are this page's; the words are
   dict/topology.ts's. */
const LEGEND = [
  { dot: "bg-health-ok", key: "legend.ok" },
  { dot: "bg-health-warn", key: "legend.warn" },
  { dot: "bg-health-bad", key: "legend.bad" },
] as const satisfies readonly { dot: string; key: TopologyKey }[];

/** unfoldableEmpty is the Time Machine's honest empty state, DERIVED from the
 *  response rather than assumed. Gated on what the map can DRAW rather than on
 *  `nodes` alone: "nothing to reconstruct" beside a drawn map would be a
 *  contradiction on screen. */
export function unfoldableEmpty(
  topo: Topology | undefined,
): { unfoldableEvents: number; eventsFolded: number } | null {
  if (!topo?.historical) return null;
  if (mapNodes(topo).nodes.length > 0) return null;
  const unfoldableEvents = topo.unfoldableEvents ?? 0;
  if (unfoldableEvents === 0) return null;
  return { unfoldableEvents, eventsFolded: topo.eventsFolded ?? 0 };
}

/**
 * MapControls replaces React Flow's default trio. The library bakes English
 * aria-label/title into those buttons and exposes no per-button override — only
 * the container's own `aria-label` — so the three are switched off and rebuilt
 * here, where they can read this page's dictionary. `useReactFlow` is legal
 * because this renders INSIDE <ReactFlow> (QA scope 2, finding #13).
 */
function MapControls() {
  const t = useT(topologyDict);
  const flow = useReactFlow();
  return (
    <Controls
      aria-label={t("controls.aria")}
      showZoom={false}
      showFitView={false}
      showInteractive={false}
    >
      <ControlButton aria-label={t("controls.zoomIn")} title={t("controls.zoomIn")} onClick={() => flow.zoomIn()}>
        <Plus aria-hidden="true" />
      </ControlButton>
      <ControlButton aria-label={t("controls.zoomOut")} title={t("controls.zoomOut")} onClick={() => flow.zoomOut()}>
        <Minus aria-hidden="true" />
      </ControlButton>
      <ControlButton
        aria-label={t("controls.fitView")}
        title={t("controls.fitView")}
        onClick={() => void flow.fitView({ padding: 0.15 })}
      >
        <Maximize aria-hidden="true" />
      </ControlButton>
    </Controls>
  );
}

export function TopologyPage() {
  const t = useT(topologyDict);
  const { locale } = useLocale();
  const topo = useTopology();
  const matrix = useMatrix("tcp");
  const { theme } = useTheme();
  const { at } = useTimeContext();
  /* warn:false, so the page still renders in the unit tests that drive it
     without a RouterProvider (the empty-state cases — React Flow itself never
     mounts in jsdom). With a router present this is a client-side navigation;
     without one the click path below falls back to goTo's full load. */
  const router = useRouter({ warn: false });
  const [hoveredEdge, setHoveredEdge] = useState<string | null>(null);
  const unfoldable = unfoldableEmpty(topo.data);
  /* How many agents the fallback map stands on — the count its notice quotes. */
  const agentCount = topo.data ? mapNodes(topo.data).nodes.length : 0;

  const flow = useMemo(
    () =>
      topo.data
        ? buildFlow(topo.data, matrix.data, t)
        : { nodes: [], edges: [], problemTotal: 0, source: "nodes" as const },
    [topo.data, matrix.data, t],
  );
  /* The map has boxes when buildFlow produced at least one NODE box (a zone
     container alone is not a map) — the one condition the empty state and the
     grid are keyed on, so they can never both be true. */
  const drawn = flow.nodes.some((n) => n.type === "topoNode");

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

  /**
   * stamp is the instant the header quotes, or null for the Live sentence.
   *
   * The server's `asOf` used to go straight into `new Date(...)`, and `new Date`
   * answers an Invalid Date for anything it cannot read rather than throwing —
   * so a stamp the controller mangled was printed, verbatim, as "as of Invalid
   * Date". A stamp that does not parse is not a stamp: the header falls back to
   * the instant the OPERATOR asked for, and to the Live sentence when there is
   * no such instant either.
   */
  const stamp = useMemo(() => {
    const raw = topo.data?.asOf;
    if (typeof raw === "string") {
      const parsed = new Date(raw);
      if (!Number.isNaN(parsed.getTime())) return parsed;
    }
    return at;
  }, [topo.data?.asOf, at]);

  /* One opener for the click and the keyboard: `at` rides along, and the router
     does the navigating. */
  const openNode = (node: Node) => {
    const path = nodeNavigationPath(node, at);
    if (!path) return;
    if (router) router.history.push(path);
    else goTo(path);
  };

  return (
    <PageShell
      timeMachine
      title={t("title")}
      /* The "as of" copy is keyed on the ENGAGED STATE, not on a successful response. */
      description={
        stamp
          ? t("description.engaged", {
              /* Interpolated into a TRANSLATED sentence, so it takes the
                 sentence's own language — lib/i18n's localeTag. */
              at: stamp.toLocaleString(localeTag(locale)),
            })
          : t("description.live")
      }
    >
      {/* The server's OWN detail, verbatim — problem+json's `detail` is what
          ApiError's message carries, so the 422's retention sentence (the only
          actionable one this endpoint answers with) arrives intact instead of
          being flattened into a blank page. */}
      {topo.error && !topo.data ? (
        <Card role="alert" className="border-l-4 border-l-health-bad bg-health-bad-soft/40 p-5">
          <p className="text-sm font-medium">{t("error.title")}</p>
          <p data-testid="topology-problem" className="mt-1 text-xs leading-relaxed text-muted-foreground">
            {topo.error.message}
          </p>
        </Card>
      ) : null}

      {/* The same failure with a node set behind it is a NOTICE about the
          refresh, not a claim that the page has nothing: the map below is real,
          it is just the last one that arrived. */}
      {topo.error && topo.data ? (
        <Card role="status" className="border-l-4 border-l-health-warn bg-health-warn-soft/40 p-5">
          <p className="text-sm font-medium">{t("stale.title")}</p>
          <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{t("stale.body")}</p>
          <p data-testid="topology-problem" className="mt-1 text-xs leading-relaxed text-muted-foreground">
            {topo.error.message}
          </p>
        </Card>
      ) : null}

      {/* The fold hit its own bound, so the node set below is built from a
          SUFFIX of the history — say so rather than presenting a partial
          reconstruction as a complete one. */}
      {topo.data?.truncated ? (
        <Card role="status" className="border-l-4 border-l-health-warn bg-health-warn-soft/40 p-5">
          <p className="text-sm font-medium">{t("truncated.title")}</p>
          <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{t("truncated.body")}</p>
        </Card>
      ) : null}

      {/* Gated on having NEITHER half rather than on isLoading, because a query
          react-query has paused is pending without fetching — isLoading false,
          no data, no error — and that combination used to leave this page with
          a heading and an empty column under it. */}
      {!topo.data && !topo.error ? (
        topo.fetchStatus === "paused" ? (
          <Card role="alert" className="border-l-4 border-l-health-warn bg-health-warn-soft/40 p-5">
            <p className="text-sm font-medium">{t("offline.title")}</p>
            <p className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">{t("offline.body")}</p>
          </Card>
        ) : (
          <Card role="status" aria-live="polite" className="p-6">
            <span className="sr-only">{t("loading")}</span>
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
        )
      ) : null}

      {unfoldable ? (
        <Card className="flex flex-col items-center gap-3 px-6 py-14 text-center">
          <span
            aria-hidden="true"
            className="flex size-12 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
          >
            <Network className="size-5" />
          </span>
          <p className="text-sm font-medium">{t("unfoldable.title")}</p>
          <p className="max-w-prose text-xs leading-relaxed text-muted-foreground">
            {t(unfoldable.unfoldableEvents === 1 ? "unfoldable.body.one" : "unfoldable.body.many", {
              count: unfoldable.unfoldableEvents,
              folded: unfoldable.eventsFolded,
            })}
          </p>
        </Card>
      ) : null}

      {/* The map is real and its provenance is not the usual one. Said ABOVE
          the map rather than instead of it: the boxes, the zones and every
          edge below are as measured as they ever are, and readiness is the one
          column that is missing (QA scope 2, findings #1 and #2). */}
      {topo.data && drawn && flow.source === "agents" ? (
        <Card role="status" className="border-l-4 border-l-health-warn bg-health-warn-soft/40 p-5">
          <p className="text-sm font-medium">{t("fromAgents.title")}</p>
          <p data-testid="topology-from-agents" className="mt-1 max-w-prose text-xs leading-relaxed text-muted-foreground">
            {t(agentCount === 1 ? "fromAgents.body.one" : "fromAgents.body.many", { count: agentCount })}
          </p>
        </Card>
      ) : null}

      {/* Empty only when BOTH halves are: no Kubernetes nodes AND no agent to
          reconstruct a lane from. */}
      {topo.data && !unfoldable && !drawn ? (
        <Card className="flex flex-col items-center gap-3 px-6 py-14 text-center">
          <span
            aria-hidden="true"
            className="flex size-12 items-center justify-center rounded-full bg-surface-2 text-muted-foreground"
          >
            <Network className="size-5" />
          </span>
          <p className="text-sm font-medium">
            {t(topo.data.historical ? "empty.historical.title" : "empty.live.title")}
          </p>
          <p className="max-w-prose text-xs leading-relaxed text-muted-foreground">
            {t(topo.data.historical ? "empty.historical.body" : "empty.live.body")}
          </p>
        </Card>
      ) : null}

      {topo.data && drawn ? (
        <div className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-x-5 gap-y-2 text-xs text-muted-foreground">
            {LEGEND.map((l) => (
              <span key={l.key} className="flex items-center gap-1.5">
                <span aria-hidden="true" className={cn("size-2.5 rounded-full", l.dot)} />
                {t(l.key)}
              </span>
            ))}
            <span className="nums ml-auto" data-testid="edge-caption">
              {flow.problemTotal === 0
                ? t("edges.none")
                : flow.problemTotal > shown
                  ? t("edges.capped", { shown, total: flow.problemTotal })
                  : t(shown === 1 ? "edges.one" : "edges.many", { count: shown })}
            </span>
          </div>

          <Card className="overflow-hidden p-0">
            <div className="h-[calc(100dvh-19rem)] min-h-[420px]">
              <ReactFlow
                nodes={flow.nodes}
                edges={edges}
                nodeTypes={NODE_TYPES}
                colorMode={theme}
                fitView
                fitViewOptions={{ padding: 0.15 }}
                /* This map is a READ-ONLY picture of what the controller reports. */
                nodesDraggable={false}
                nodesConnectable={false}
                elementsSelectable={false}
                onNodeClick={(_, node) => openNode(node)}
                /* React Flow makes every node a tab stop and activates none of
                   them, so the map's only navigation was mouse-only. It exposes
                   no per-node key handler, but it does stamp `data-id` on the
                   wrapper that holds the focus — so the focused node is
                   resolvable from the event itself. */
                onKeyDown={(event) => {
                  if (event.key !== "Enter" && event.key !== " ") return;
                  const wrapper = (event.target as HTMLElement | null)?.closest?.(".react-flow__node");
                  const id = wrapper?.getAttribute("data-id");
                  const node = id ? flow.nodes.find((n) => n.id === id) : undefined;
                  if (!node) return;
                  event.preventDefault();
                  openNode(node);
                }}
                onEdgeMouseEnter={(_, e) => setHoveredEdge(e.id)}
                onEdgeMouseLeave={() => setHoveredEdge(null)}
                /* FOCUS reveals the same label hover does. The failure percentage on an edge existed
                   only under a pointer, so a keyboard user — and anyone on a touch screen, where
                   there is no hover at all — could see that a path was bad and never how bad.
                   React Flow has no onEdgeFocus prop, so the focus is caught where it lands: the
                   edge's own DOM element carries data-id, exactly as the node handler above uses. */
                onFocusCapture={(event) => {
                  const edge = (event.target as HTMLElement | null)?.closest?.(".react-flow__edge");
                  const id = edge?.getAttribute("data-id");
                  if (id) setHoveredEdge(id);
                }}
                onBlurCapture={(event) => {
                  if ((event.target as HTMLElement | null)?.closest?.(".react-flow__edge")) {
                    setHoveredEdge(null);
                  }
                }}
                /* React Flow's built-in a11y strings are English and speak about
                   dragging a node that is not draggable here. Both node keys are
                   set on purpose: with keyboard a11y ENABLED the library renders
                   the `keyboardDisabled` variant, so overriding only `.default`
                   would change nothing on screen. */
                ariaLabelConfig={{
                  "node.a11yDescription.default": t("flow.node.a11y"),
                  "node.a11yDescription.keyboardDisabled": t("flow.node.a11y"),
                  "edge.a11yDescription.default": t("flow.edge.a11y"),
                }}
                proOptions={{ hideAttribution: true }}
              >
                <Background gap={24} size={1.5} />
                <MapControls />
              </ReactFlow>
            </div>
          </Card>
        </div>
      ) : null}
    </PageShell>
  );
}
