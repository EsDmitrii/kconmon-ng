/**
 * matrix-plan — the matrix grid's reading of the topology's `probePlan` (M10 sparse mesh).
 *
 * Under a sparse plan "no data for a pair" is either a failure or the plan, and the grid must not
 * paint the second as the first. The plan arrives as `probePlan` on GET /api/v1/topology — present
 * ONLY while a sparse plan is in force — and the two readers here fail in ONE direction on any
 * doubt: toward "planned", i.e. toward the alarming 'no data' cell, never toward the calming
 * 'not probed'. A wrongly-calm cell hides an outage; a wrongly-alarming one merely asks for a look.
 */

/** ProbePlan is the validated form the grid queries: source node → the set it probes. */
export type ProbePlan = Map<string, Set<string>>;

/**
 * readProbePlan validates the wire field. `null` means "no plan is in force" — the full-mesh
 * reading — and is the answer for an absent field, a historical (`?at=`) topology, and any shape
 * that is not a plain object (the payload may arrive over a websocket frame no normalizer saw).
 *
 * An entry whose value is not an array is DROPPED rather than read as empty: an unreadable row
 * must not calm that node's whole line into 'not probed'. Non-string members are skipped.
 */
export function readProbePlan(raw: unknown): ProbePlan | null {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) return null;
  const plan: ProbePlan = new Map();
  for (const [source, dests] of Object.entries(raw)) {
    if (!Array.isArray(dests)) continue;
    const set = new Set<string>();
    for (const d of dests) {
      if (typeof d === "string" && d !== "") set.add(d);
    }
    plan.set(source, set);
  }
  return plan;
}

/**
 * isPlanExcluded answers "does the plan say nobody probes src → dst?" — the ONLY question the
 * 'not probed' cell state may be painted from.
 *
 * A source the plan does not mention answers false: the controller keys every node that has a
 * registered agent, so a missing key means the agent is gone — an incident that must keep reading
 * as 'no data', not an exclusion. The self pair is the diagonal's business, never this state's.
 */
export function isPlanExcluded(plan: ProbePlan | null, src: string, dst: string): boolean {
  if (!plan || src === dst) return false;
  const dests = plan.get(src);
  if (!dests) return false;
  return !dests.has(dst);
}
