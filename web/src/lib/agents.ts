import { matrixDict } from "@/lib/i18n/dict/matrix";
import { CHECK_TYPES, type CheckType, type Matrix, type Topology, type TopologyAgent } from "@/lib/types";

/**
 * agents — what the console reads off a TopologyAgent beyond its name: is it
 * a bare host (the external label), and which probe planes did it say it
 * runs (the plane:* capabilities). Both fields are new in 2.4.0 and both may
 * be missing, null or malformed on the wire, so every reader here is total —
 * no throw for any input — and, like lib/matrix-plan.ts, fails in ONE
 * direction on any doubt: toward the alarming answer. Not external, planes
 * unknown, no scrape gap claimed.
 */

/** The label a bare-host agent stamps on itself; mirrors internal/model/agent.go's LabelExternal. */
export const EXTERNAL_LABEL = "kconmon-ng.io/external";

/** Prefix of the capabilities naming a probe plane; mirrors model.CapabilityPlanePrefix. */
export const PLANE_CAPABILITY_PREFIX = "plane:";

/** A probe plane as the agent spells it after "plane:" — the same names as a check type. */
export type Plane = CheckType;
export const PLANES: readonly Plane[] = CHECK_TYPES;

/** Where every "see External agents docs" hint points. Read from the matrix table so the
 *  target changes in one place, the rule lib/investigation-sources.ts's constants follow. */
export const EXTERNAL_SCRAPE_DOCS_URL: string = matrixDict.en["docs.scrapeExternal"];

/** A plain object — the only shape a JSON map or a JSON struct arrives as. */
function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/**
 * isExternalAgent answers "did this agent register as a bare host?" — the ONLY question the
 * external badge may be painted from. Exactly the string "true": the agent writes that
 * literal, and "True" or a boolean is something else claiming to be it.
 */
export function isExternalAgent(agent: TopologyAgent | null | undefined): boolean {
  if (!isRecord(agent)) return false;
  const labels: unknown = agent.labels;
  if (!isRecord(labels)) return false;
  return Object.hasOwn(labels, EXTERNAL_LABEL) && labels[EXTERNAL_LABEL] === "true";
}

/**
 * agentPlanes reads the plane:* capabilities into a set, or answers null for "unknown".
 *
 * FAIL-OPEN, stated once: an agent advertising no readable plane at all — every agent older
 * than 2.4.0, and any shape this cannot parse — is read as running EVERY plane, never none.
 * Reading absence as "unsupported" would turn each of its grey 'no data' cells into a calming
 * dashed frame for the length of a rolling upgrade, which is the outage-hiding direction
 * lib/matrix-plan.ts guards against. An unknown plane name is skipped, not counted.
 */
export function agentPlanes(agent: TopologyAgent | null | undefined): Set<Plane> | null {
  if (!isRecord(agent)) return null;
  const capabilities: unknown = agent.capabilities;
  if (!Array.isArray(capabilities)) return null;
  const planes = new Set<Plane>();
  for (const cap of capabilities as unknown[]) {
    if (typeof cap !== "string" || !cap.startsWith(PLANE_CAPABILITY_PREFIX)) continue;
    const name = cap.slice(PLANE_CAPABILITY_PREFIX.length);
    if ((PLANES as readonly string[]).includes(name)) planes.add(name as Plane);
  }
  return planes.size === 0 ? null : planes;
}

/** runsPlane is agentPlanes with the fail-open rule applied: unknown answers true. */
export function runsPlane(agent: TopologyAgent | null | undefined, plane: Plane): boolean {
  const planes = agentPlanes(agent);
  return planes === null || planes.has(plane);
}

/**
 * externalByNode indexes the external agents by the node name they registered under — the
 * name every other surface (matrix rows, worst pairs, node cards) keys on. First one wins
 * when two claim a name; an agent with no usable name has nothing to be looked up by.
 */
export function externalByNode(topo: Topology | null | undefined): Map<string, TopologyAgent> {
  const out = new Map<string, TopologyAgent>();
  const agents: unknown = isRecord(topo) ? topo.agents : undefined;
  if (!Array.isArray(agents)) return out;
  for (const raw of agents as unknown[]) {
    if (!isRecord(raw)) continue;
    const agent = raw as unknown as TopologyAgent;
    if (!isExternalAgent(agent)) continue;
    const name: unknown = agent.nodeName;
    if (typeof name !== "string" || name === "" || out.has(name)) continue;
    out.set(name, agent);
  }
  return out;
}

/**
 * unscrapedExternalNodes names the external agents whose metrics Prometheus is evidently not
 * reading: their name is never a cell's SOURCE while the matrix has cells at all. An external
 * agent's column fills from the in-cluster agents' probes, so only its own row can show the
 * gap. An empty matrix answers nothing — silence everywhere is a young deployment, not a
 * scrape job somebody forgot.
 *
 * Unscraped is silence with NO other known cause, so it is read per plane: an agent that
 * advertised its planes and left `plane` out has a cause of its own (its row is 'unsupported'
 * on that grid), and a scrape job would not fill it. Only the agents that run the plane — or
 * advertised nothing, which runsPlane reads as every plane — can be listed. The matrix handed
 * in must be the one for that same plane.
 */
export function unscrapedExternalNodes(
  topo: Topology | null | undefined,
  matrix: Matrix | null | undefined,
  plane: Plane,
): string[] {
  const cells: unknown = isRecord(matrix) ? matrix.cells : undefined;
  if (!Array.isArray(cells) || cells.length === 0) return [];
  const external = externalByNode(topo);
  if (external.size === 0) return [];
  const sources = new Set<string>();
  for (const cell of cells as unknown[]) {
    if (isRecord(cell) && typeof cell.source === "string") sources.add(cell.source);
  }
  return [...external].filter(([name, agent]) => runsPlane(agent, plane) && !sources.has(name)).map(([name]) => name);
}
