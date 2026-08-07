import { useEffect, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, getRun } from "@/lib/api";
import {
  RUN_TERMINAL_STATUSES,
  type RunDetail,
  type RunFinishedFrame,
  type RunProgressFrame,
  type RunResult,
} from "@/lib/types";
import { useCapabilities } from "./use-capabilities";
import { getWsClient } from "./use-ws-topic";

export const RUN_POLL_MS = 5_000;

/** runTopic mirrors internal/console/ws.RunTopic("run:" + id) exactly. */
export function runTopic(runId: string): string {
  return `run:${runId}`;
}

export function isTerminalRunStatus(status: string | undefined): boolean {
  return status !== undefined && (RUN_TERMINAL_STATUSES as string[]).includes(status);
}

/** One merged row of the run's pair table -- see mergeRunPairs. */
export interface RunPairRow {
  source: string;
  destination: string;
  state: string;
  success?: boolean;
  durationNs?: number;
  error?: string;
}

// NUL keeps the composite key unambiguous, same convention as matrix.tsx's
// own pairKey.
function pairKey(source: string, destination: string): string {
  return `${source}\0${destination}`;
}

function fromResult(r: RunResult): RunPairRow {
  return {
    source: r.sourceNode,
    destination: r.destinationNode,
    state: r.success ? "succeeded" : "failed",
    success: r.success,
    durationNs: r.durationNs,
    error: r.error,
  };
}

function fromFrame(f: RunProgressFrame): RunPairRow {
  return { source: f.source, destination: f.destination, state: f.state, success: f.success, durationNs: f.durationNs, error: f.error };
}

/**
 * mergeRunPairs combines the REST snapshot's recorded results with whatever
 * per-pair progress frames this tab has seen over the socket into one row
 * per (source, destination) -- never two. REST wins on overlap: a result
 * the server has actually persisted is authoritative over a frame this tab
 * merely observed in transit, and it is what a page must fall back to
 * entirely when the socket contributed nothing at all -- a direct load of
 * the permalink (nothing but the REST payload, ever) or a run whose socket
 * frames were never received (the polling-only path). Frames fill in ONLY
 * the pairs REST has not caught up to yet, typically ones still
 * "dispatched".
 */
export function mergeRunPairs(results: RunResult[], frames: Map<string, RunProgressFrame>): RunPairRow[] {
  const rows = new Map<string, RunPairRow>();
  for (const f of frames.values()) rows.set(pairKey(f.source, f.destination), fromFrame(f));
  for (const r of results) rows.set(pairKey(r.sourceNode, r.destinationNode), fromResult(r));
  return [...rows.values()];
}

function isProgressFrame(data: unknown): data is RunProgressFrame {
  return typeof data === "object" && data !== null && "source" in data && "destination" in data;
}

export interface UseRunResult {
  run: RunDetail | undefined;
  pairs: RunPairRow[];
  isLoading: boolean;
  notFound: boolean;
  error: Error | null;
  live: boolean;
}

/**
 * useRun is the run permalink's (task-24-brief.md) data source: GET
 * /api/v1/runs/{id} (task-23-brief.md) polled every RUN_POLL_MS until the
 * run reaches a terminal status, PLUS the run's own run:{id} WebSocket
 * topic (task-22-brief.md) for live per-pair progress -- identical in
 * spirit to useMatrix's push-with-polling-fallback (use-matrix.ts).
 *
 * Unlike useMatrix's snapshot topics, run:{id} is NOT whole-state: every
 * progress frame describes exactly one pair. This subscribes directly
 * through getWsClient() -- live.tsx's own pattern, for the same reason:
 * useWsTopic keeps only the latest envelope, which would collapse every
 * pair's frame but the most recent one into a single state update and drop
 * the rest. It is also the one seam that needs the raw WsEnvelope.type to
 * tell a progress/finished frame ("event") from the topic's own terminal
 * control frame ("closed", ws.TypeClosed) apart, which useWsTopic does not
 * expose to callers at all.
 *
 * REST polling is the correctness backstop, not a nicety: a replica this
 * tab is not streaming the run from answers the run:{id} subscribe with
 * the M2 "unknown topic" error frame (registry-full or an old run), and
 * polling alone must still drive the page to completion in that case --
 * `socketEnabled` below goes false and RUN_POLL_MS keeps firing until the
 * REST status itself is terminal. The same fallback covers `realtime`
 * being off entirely (no "events" capability on this replica).
 */
export function useRun(runId: string): UseRunResult {
  const { realtime } = useCapabilities();
  const queryClient = useQueryClient();
  const [frames, setFrames] = useState<Map<string, RunProgressFrame>>(() => new Map());
  const [socketDone, setSocketDone] = useState(false);

  // A run switch must not carry over the previous run's accumulated frames
  // or its socket-done latch -- each permalink id gets its own fresh state.
  useEffect(() => {
    setFrames(new Map());
    setSocketDone(false);
  }, [runId]);

  const enabled = runId !== "";
  const query = useQuery({
    queryKey: ["run", runId],
    queryFn: () => getRun(runId),
    enabled,
    retry: false,
    // Stops on its own once the run is terminal (nothing left to poll for)
    // and on any error (a 404 does not become fetchable by retrying on a
    // timer -- see notFound below; this is what keeps that state from
    // being an infinite spinner).
    refetchInterval: (q) => (isTerminalRunStatus(q.state.data?.status) || q.state.error ? false : RUN_POLL_MS),
  });

  const terminal = isTerminalRunStatus(query.data?.status);
  // Gated on query.data !== undefined too, not just !terminal: opening the
  // socket while the FIRST REST response is still in flight would, for an
  // already-finished run loaded directly (the permalink guarantee), open a
  // socket for a fraction of a second before terminal ever has a chance to
  // become true. Waiting for that first response costs nothing -- it is
  // already the very thing every render is waiting on anyway.
  const socketEnabled = enabled && realtime && query.data !== undefined && !terminal && !socketDone;

  useEffect(() => {
    if (!socketEnabled) return;
    const topic = runTopic(runId);
    const off = getWsClient().subscribe<RunProgressFrame | RunFinishedFrame>(topic, (env) => {
      if (env.type === "error") {
        // M2's "unknown topic" rejection (registry full, or this run's
        // topic was already reaped) -- polling alone takes over from here.
        setSocketDone(true);
        return;
      }
      if (env.type === "closed") {
        // ws.TypeClosed: nothing more is ever coming on this topic.
        setSocketDone(true);
        return;
      }
      if (isProgressFrame(env.data)) {
        const frame = env.data;
        setFrames((prev) => {
          const next = new Map(prev);
          next.set(pairKey(frame.source, frame.destination), frame);
          return next;
        });
        return;
      }
      // The finished frame carries only {state:"finished",status} -- no
      // results -- so the authoritative run+results still comes from one
      // more REST read, not from this bare frame standing in for it.
      void queryClient.refetchQueries({ queryKey: ["run", runId] });
    });
    return () => off();
  }, [socketEnabled, runId, queryClient]);

  const pairs = useMemo(() => mergeRunPairs(query.data?.results ?? [], frames), [query.data?.results, frames]);

  const notFound = query.error instanceof ApiError && query.error.problem.status === 404;

  return {
    run: query.data,
    pairs,
    isLoading: query.isLoading,
    notFound,
    error: query.error,
    live: socketEnabled,
  };
}
