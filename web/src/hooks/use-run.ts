import { useCallback, useEffect, useMemo, useState } from "react";
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

/**
 * isTerminalRunStatus answers "is there anything left to poll for"; the set it reads
 * (RUN_TERMINAL_STATUSES) includes "cancelled": a cancelled run is FINISHED.
 */
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
 * mergeRunPairs combines the REST snapshot's recorded results with whatever per-pair progress
 * frames this tab has seen over the socket into one row per (source, destination).
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
  /** Exists for POST /api/v1/runs/{id}/cancel: the 204 means only "accepted". */
  refetch: () => Promise<unknown>;
}

/**
 * useRun is the run permalink's data source; this subscribes directly through getWsClient --
 * live.tsx's own pattern, for the same reason.
 */
export function useRun(runId: string): UseRunResult {
  const { realtime } = useCapabilities();
  const queryClient = useQueryClient();
  const [frames, setFrames] = useState<Map<string, RunProgressFrame>>(() => new Map());
  const [socketDone, setSocketDone] = useState(false);
  /* Whether the socket is actually CONNECTED. `live` used to be the subscription's existence alone,
     so the permalink's badge said "Live" while the socket was down and the page was really being
     carried by the 5s poll — the one place a reader looks to know which of the two they are seeing.
     useWsTopic tracks the same state for the matrix and topology surfaces. */
  const [connected, setConnected] = useState(false);

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
    // Stops on its own once the run is terminal (nothing left to poll for) and on any error (a 404
    // does not become fetchable by retrying on a timer -- see notFound below; this is what keeps
    // that state from being an infinite spinner).
    refetchInterval: (q) => (isTerminalRunStatus(q.state.data?.status) || q.state.error ? false : RUN_POLL_MS),
  });

  const terminal = isTerminalRunStatus(query.data?.status);
  // Gated on query.data !== undefined too, not just !terminal.
  const socketEnabled = enabled && realtime && query.data !== undefined && !terminal && !socketDone;

  useEffect(() => {
    if (!socketEnabled) {
      setConnected(false);
      return;
    }
    const topic = runTopic(runId);
    const ws = getWsClient();
    // The badge follows the CONNECTION, not the subscription; see `connected`.
    const offState = ws.onStateChange((state) => setConnected(state === "open"));
    const off = ws.subscribe<RunProgressFrame | RunFinishedFrame>(topic, (env) => {
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
    return () => {
      off();
      offState();
    };
  }, [socketEnabled, runId, queryClient]);

  const pairs = useMemo(() => mergeRunPairs(query.data?.results ?? [], frames), [query.data?.results, frames]);

  const notFound = query.error instanceof ApiError && query.error.problem.status === 404;

  const refetch = useCallback(() => query.refetch(), [query]);

  return {
    run: query.data,
    pairs,
    isLoading: query.isLoading,
    notFound,
    error: query.error,
    live: socketEnabled && connected,
    refetch,
  };
}
