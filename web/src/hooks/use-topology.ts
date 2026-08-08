import { useQuery } from "@tanstack/react-query";
import { getTopology } from "@/lib/api";
import { formatAtParam, useTimeContext } from "@/lib/timemachine";

export const TOPOLOGY_POLL_MS = 15_000;

/**
 * useTopology serves the node/agent set every map, card and picker reads.
 *
 * Live: unchanged — GET /api/v1/topology, polled every TOPOLOGY_POLL_MS.
 *
 * Engaged (Time Machine at `t`): GET /api/v1/topology?at=t, and the poll is
 * OFF. Not as an optimisation — as correctness. A past instant's answer cannot
 * change, so a refetch can only re-fetch the identical body; leaving the
 * interval on would spend a request every 15s to learn nothing, and worse, it
 * would keep the page LOOKING live (a spinner ticking over a frozen view) when
 * the whole point of the mode is that it is not.
 *
 * The `at` is in the query KEY, not just the request: two instants are two
 * different answers and must not share a cache entry, and moving back to Live
 * lands on the untouched ["topology"] entry rather than invalidating it.
 */
export function useTopology() {
  const { at } = useTimeContext();
  const stamp = at ? formatAtParam(at) : null;
  return useQuery({
    queryKey: stamp ? ["topology", "at", stamp] : ["topology"],
    queryFn: () => getTopology(at ?? undefined),
    refetchInterval: at ? false : TOPOLOGY_POLL_MS,
  });
}
