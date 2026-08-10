import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { getConfig, getVersion } from "@/lib/api";

// Same cadence as MATRIX_POLL_MS/TOPOLOGY_POLL_MS.
export const CAPABILITIES_POLL_MS = 15_000;

/**
 * useCapabilities reports what this console replica can actually do right now; `realtime` is true
 * only while GET /api/v1/version advertises the "events" capability.
 */
export function useCapabilities(): { realtime: boolean; resolved: boolean } {
  const { data, isPending } = useQuery({
    queryKey: ["version"],
    queryFn: getVersion,
    refetchInterval: CAPABILITIES_POLL_MS,
  });
  const realtime = data?.capabilities?.includes("events") ?? false;
  const resolved = !isPending;
  return useMemo(() => ({ realtime, resolved }), [realtime, resolved]);
}

/**
 * useDatabaseAvailable reports whether GET /api/v1/events has anything behind it; the Live page
 * uses this to decide whether to fetch scrollback.
 */
export function useDatabaseAvailable(): { available: boolean; resolved: boolean } {
  const { data, isPending } = useQuery({
    queryKey: ["config"],
    queryFn: getConfig,
    staleTime: Infinity,
  });
  const available = data?.database?.configured ?? false;
  const resolved = !isPending;
  return useMemo(() => ({ available, resolved }), [available, resolved]);
}
