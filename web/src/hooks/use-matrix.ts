import { useEffect } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { getMatrix } from "@/lib/api";
import { getMatrixAt } from "@/lib/matrix-promql";
import { formatAtParam, useTimeContext } from "@/lib/timemachine";
import type { Matrix, Protocol } from "@/lib/types";
import { useCapabilities } from "./use-capabilities";
import { useWsTopic } from "./use-ws-topic";

export const MATRIX_POLL_MS = 15_000;

/**
 * matrixTopic mirrors internal/console/ws.MatrixTopic; the ":pod" suffix is part of the
 * WEBSOCKET.md topic shape, not a query argument.
 */
export function matrixTopic(protocol: Protocol): string {
  return `matrix:${protocol}:pod`;
}

/**
 * useMatrix serves the matrix from one query key regardless of transport; a pushed frame is a whole
 * matrix, written over the cache entry wholesale.
 */
export function useMatrix(protocol: Protocol) {
  const { at } = useTimeContext();
  const engaged = at !== null;
  const { realtime } = useCapabilities();
  const queryClient = useQueryClient();
  const push = useWsTopic<Matrix>(matrixTopic(protocol), { enabled: realtime && !engaged });
  const live = realtime && !engaged && push.connected;

  useEffect(() => {
    // The protocol check is belt-and-braces on top of useWsTopic's topic tag.
    if (!realtime || engaged || push.data?.protocol !== protocol) return;
    queryClient.setQueryData(["matrix", protocol, "pod"], push.data);
  }, [realtime, engaged, push.data, protocol, queryClient]);

  // A separate cache key per instant, and the live key left completely
  // untouched — returning to Live re-reads the entry the poll/push pair was
  // already maintaining instead of refetching it.
  const query = useQuery({
    queryKey: at ? ["matrix", protocol, "pod", "at", formatAtParam(at)] : ["matrix", protocol, "pod"],
    queryFn: () => (at ? getMatrixAt(protocol, at) : getMatrix(protocol)),
    refetchInterval: engaged || live ? false : MATRIX_POLL_MS,
  });

  /* isPending as well as isLoading: a query whose retry is PAUSED (the tab lost focus after a
     failure) is pending-but-not-fetching, so isLoading is false while there is nothing to draw and
     no error to show — the surface rendered a heading and blank space, indefinitely. */
  return { data: query.data, isLoading: query.isLoading, isPending: query.isPending, error: query.error, live };
}
