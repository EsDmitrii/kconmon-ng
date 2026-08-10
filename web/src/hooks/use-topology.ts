import { useQuery } from "@tanstack/react-query";
import { getTopology } from "@/lib/api";
import { formatAtParam, useTimeContext } from "@/lib/timemachine";

export const TOPOLOGY_POLL_MS = 15_000;

/**
 * useTopology serves the node/agent set every map, card and picker reads; a past instant's answer
 * cannot change, so a refetch can only re-fetch the identical body.
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
