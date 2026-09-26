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
 * useConsoleConfig is GET /api/v1/config, read once per session: staleTime is per observer on the
 * shared key, so a caller spelling its own options without Infinity would refetch on every mount.
 */
export function useConsoleConfig() {
  return useQuery({ queryKey: ["config"], queryFn: getConfig, staleTime: Infinity });
}

/**
 * useDatabaseAvailable reports whether the console has a database (config.database.configured);
 * every history-backed surface gates on it. `error` is set when GET /api/v1/config itself failed
 * and there is no cached answer: a caller shows that failure (its dictionary's config.failed)
 * instead of its no-database line.
 */
export function useDatabaseAvailable(): { available: boolean; resolved: boolean; error: Error | null } {
  const { data, isPending, error: queryError } = useConsoleConfig();
  const available = data?.database?.configured ?? false;
  const resolved = !isPending;
  // A failed /config also reads as resolved && !available; `error` is what tells the two apart.
  const error = data === undefined ? queryError : null;
  return useMemo(() => ({ available, resolved, error }), [available, resolved, error]);
}
