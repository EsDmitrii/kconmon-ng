import { useQuery } from "@tanstack/react-query";
import { getTopology } from "@/lib/api";

export const TOPOLOGY_POLL_MS = 15_000;

export function useTopology() {
  return useQuery({
    queryKey: ["topology"],
    queryFn: getTopology,
    refetchInterval: TOPOLOGY_POLL_MS,
  });
}
