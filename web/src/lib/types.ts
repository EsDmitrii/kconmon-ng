// types.ts — hand-written mirrors of the Go JSON types (no codegen in M1)
export interface TopologyNode {
  name: string;
  zone: string;
  ready: boolean;
}
export interface TopologyAgent {
  id: string;
  nodeName: string;
  podIP: string;
  zone: string;
}
export interface Topology {
  nodes: TopologyNode[];
  agents: TopologyAgent[];
  timestamp: string;
}
export interface MatrixCell {
  source: string;
  destination: string;
  failRatio: number | null;
  rttP95?: number;
  lossRatio?: number;
}
export interface Matrix {
  protocol: string;
  plane: string;
  nodes: string[];
  cells: MatrixCell[];
  timestamp: string;
}
export type Protocol = "tcp" | "udp" | "icmp";
export interface PromResult {
  status: "success" | "error";
  data?: { resultType: string; result: unknown[] };
  errorType?: string;
  error?: string;
}
export interface Problem {
  type: string;
  title: string;
  status: number;
  detail?: string;
}

// LiveEvent mirrors internal/console/events.LiveEvent — the browser-facing
// projection of the controller's pb.Event. `id` is "<seq>-<unixNano>" and is
// identical on every console replica, which is what makes it both the hub's
// dedupe key and a stable React list key.
export type LiveEventType =
  | "topology_changed"
  | "check_observed"
  | "mtr_triggered"
  | "mtr_completed"
  | "diagnostic_progress";
export type LiveEventSeverity = "info" | "warn" | "error";
export interface LiveEvent {
  id: string;
  // Controller-assigned, gapless per controller. This is the loss signal for
  // the live feed: the WebSocket envelope's own seq is gapless by construction
  // (a bus-side drop never shows up there), so a gap HERE is what tells a
  // consumer it missed events.
  seq: number;
  type: LiveEventType;
  severity: LiveEventSeverity;
  scope: string;
  timestamp: string;
  summary: string;
  // Type-specific object; the Live page renders `summary`/`scope` and does not
  // decode this in M2, so it stays unknown rather than a premature union.
  details: unknown;
}
export const LIVE_EVENT_TYPES: LiveEventType[] = [
  "topology_changed",
  "check_observed",
  "mtr_triggered",
  "mtr_completed",
  "diagnostic_progress",
];
export const LIVE_EVENT_SEVERITIES: LiveEventSeverity[] = ["info", "warn", "error"];

// Version mirrors GET /api/v1/version (internal/console/httpapi handleVersion).
// From M2 `capabilities` is computed per replica: ["events"] while this
// replica's realtime pipeline is healthy, [] otherwise. The key is always
// emitted by the Go handler, so the mirror declares it non-optional — but an
// M1 console pod rolled beside an M2 one can still answer without it, and
// consumers must read a missing list as "no realtime", not crash.
export interface Version {
  version: string;
  commit: string;
  capabilities: string[];
}

export const PROTOCOLS: Protocol[] = ["tcp", "udp", "icmp"];
