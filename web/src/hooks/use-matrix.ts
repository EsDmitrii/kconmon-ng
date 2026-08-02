import { useEffect } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { getMatrix } from "@/lib/api";
import type { Matrix, Protocol } from "@/lib/types";
import { useCapabilities } from "./use-capabilities";
import { useWsTopic } from "./use-ws-topic";

export const MATRIX_POLL_MS = 15_000;

/**
 * matrixTopic mirrors internal/console/ws.MatrixTopic. The ":pod" suffix is
 * part of the WEBSOCKET.md topic shape, not a query argument — matrix.Compute
 * has no plane parameter, and pod is the only plane implemented.
 */
export function matrixTopic(protocol: Protocol): string {
  return `matrix:${protocol}:pod`;
}

/**
 * useMatrix serves the matrix from one query key regardless of transport.
 *
 * - M1 path (not live): getMatrix() + refetchInterval MATRIX_POLL_MS —
 *   unchanged, and still the first paint even when realtime is up, because the
 *   socket has nothing to give until the next MatrixPusher tick.
 * - M2 path (live): polling is switched off and pushed snapshots are written
 *   into the same key with setQueryData, so MatrixPage never learns which
 *   transport fed it.
 *
 * A pushed frame is a whole matrix, written over the cache entry wholesale —
 * the same JSON shape GET /api/v1/matrix returns (both sides serialize
 * matrix.Matrix), so nothing downstream can tell the two apart, and envelope
 * seq is never used to order anything across connections.
 *
 * "live" is the capability AND a socket that actually reached open, not the
 * capability alone. The capability only says this replica's ingester is
 * healthy; it cannot see a browser-side reason the socket never establishes (a
 * CSP connect-src denial, a proxy that strips the Upgrade header). Gating on
 * the capability alone would silence polling and freeze the matrix forever in
 * exactly those cases. Either half going away re-arms polling by itself: the
 * capability within one CAPABILITIES_POLL_MS, the socket immediately on close.
 */
export function useMatrix(protocol: Protocol) {
  const { realtime } = useCapabilities();
  const queryClient = useQueryClient();
  const push = useWsTopic<Matrix>(matrixTopic(protocol), { enabled: realtime });
  const live = realtime && push.connected;

  useEffect(() => {
    // The protocol check is belt-and-braces on top of useWsTopic's topic tag:
    // a snapshot is only ever allowed into the key for its OWN protocol, so no
    // sequence of protocol switches can render one protocol's numbers under
    // another's label.
    if (!realtime || push.data?.protocol !== protocol) return;
    queryClient.setQueryData(["matrix", protocol, "pod"], push.data);
  }, [realtime, push.data, protocol, queryClient]);

  const query = useQuery({
    queryKey: ["matrix", protocol, "pod"],
    queryFn: () => getMatrix(protocol),
    refetchInterval: live ? false : MATRIX_POLL_MS,
  });

  return { data: query.data, isLoading: query.isLoading, error: query.error, live };
}
