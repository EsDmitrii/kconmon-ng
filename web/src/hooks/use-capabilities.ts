import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { getConfig, getVersion } from "@/lib/api";

// Same cadence as MATRIX_POLL_MS/TOPOLOGY_POLL_MS. It is what makes the
// realtime→polling fallback self-healing: when this console replica's ingester
// drops, capabilities loses "events" and every push-aware hook is back on its
// M1 REST interval within one poll.
export const CAPABILITIES_POLL_MS = 15_000;

/**
 * useCapabilities reports what this console replica can actually do right now.
 * `realtime` is true only while GET /api/v1/version advertises the "events"
 * capability, i.e. while this replica holds a live controller event stream
 * (internal/console/httpapi handleVersion + events.Ingester.Healthy).
 *
 * A response with no capabilities field at all (an older replica) reads as
 * false, mirroring Go's nil-safe Version.HasCapability.
 *
 * `resolved` separates "this replica has no realtime" from "we have not asked
 * yet". Both read `realtime === false`, and a consumer that cannot tell them
 * apart flashes a "realtime is unavailable" warning on every cold load, before
 * the very first /api/v1/version response. Callers that only ever pick a
 * transport (useMatrix) can keep ignoring it — polling is the right thing to do
 * while the answer is unknown anyway.
 *
 * The result is memoised on the booleans, so a poll that re-fetches an unchanged
 * capability list yields the very same object: consumers keyed on `realtime`
 * (useWsTopic's `enabled`, useMatrix's refetchInterval) do not re-run their
 * effects even if a caller spreads the result into a dependency array, and
 * server-side flap damping is a nicety rather than a requirement.
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
 * useDatabaseAvailable reports whether GET /api/v1/events has anything behind
 * it — GET /api/v1/config's `database.configured` (internal/console/httpapi
 * handleConfig), which is `true` exactly when `s.events != nil`, the same
 * signal handleEvents' own 503 gate reads. The Live page uses this to decide
 * whether to fetch scrollback at all: fetching against a replica that answers
 * 503 would just be a request that always fails.
 *
 * Unlike `realtime` this is not expected to flap while the page is open —
 * console.database.mode is a deploy-time setting, not a per-replica health
 * signal — so it is fetched once (`staleTime: Infinity`) rather than polled
 * on CAPABILITIES_POLL_MS.
 *
 * `resolved` is the same "have we asked yet" split useCapabilities makes:
 * `available === false` before the first answer must not be read as "history
 * is off", or the Live page would skip the scrollback fetch on every cold
 * load before ever finding out it should have made it.
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
