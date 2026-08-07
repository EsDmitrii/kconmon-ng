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
// EventPage mirrors GET /api/v1/events's body (internal/console/httpapi
// eventsResponse). NextCursor is never absent on the wire — an exhausted page
// still answers "" — so the mirror keeps it required rather than optional;
// "" is the sentinel a consumer checks for "no more pages", not a missing key.
export interface EventPage {
  events: LiveEvent[];
  nextCursor: string;
}

// EventQuery is the browser-side request shape for GET /api/v1/events. Dates
// are Date objects here (not RFC3339 strings) so callers build them the same
// way promqlQuery/promqlQueryRange already do; getEvents does the
// toISOString() conversion at the wire boundary.
export interface EventQuery {
  types?: LiveEventType[];
  scope?: string;
  from?: Date;
  to?: Date;
  limit?: number;
  cursor?: string;
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

// Config mirrors GET /api/v1/config (internal/console/httpapi handleConfig).
// The Live page's scrollback reads only `database.configured` — whether this
// replica has console.database.mode wired up (Deps.Events non-nil), the same
// signal GET /api/v1/events' own 503 gate uses — but the mirror carries the
// whole object rather than a narrowed slice, the same convention Version and
// Topology already follow.
export interface Config {
  auth: { mode: string; role: string; loginPath: string };
  anonymousBanner: boolean;
  controller: { configured: boolean };
  prometheus: { configured: boolean };
  database: { configured: boolean };
}

export const PROTOCOLS: Protocol[] = ["tcp", "udp", "icmp"];

// Me mirrors GET /api/v1/auth/me (internal/console/httpapi handleAuthMe):
// {"subject": {...}, "permissions": [...]}. `subject.kind` mirrors Go's
// authz.SubjectKind ("anonymous" | "user" | "token"); `permissions` is
// authz.Policy.PermissionsFor(subject) — the flattened, already-resolved
// list `can()` checks against, never a set of role names to re-resolve
// client-side.
export type SubjectKind = "anonymous" | "user" | "token";
export interface Me {
  subject: { kind: SubjectKind; id: string; displayName: string; groups: string[]; roles: string[] };
  permissions: string[];
}

// CheckType mirrors checks.Spec's own comment (internal/console/checks/checks.go)
// -- the controller's validCheckTypes, copied there deliberately rather than
// imported (task-22-brief.md: no controller-package dependency).
export type CheckType = "tcp" | "udp" | "icmp" | "dns" | "http" | "mtr";
export const CHECK_TYPES: CheckType[] = ["tcp", "udp", "icmp", "dns", "http", "mtr"];

// RunStatus mirrors checks.Runner's lifecycle (memory.go: "pending" ->
// "running" -> finalStatus's "succeeded" | "failed" | "partial").
export type RunStatus = "pending" | "running" | "succeeded" | "failed" | "partial";
export const RUN_TERMINAL_STATUSES: RunStatus[] = ["succeeded", "failed", "partial"];

// RunCreateRequest is POST /api/v1/runs's body (internal/console/httpapi
// runCreateRequest). An absent/empty sources or destinations means "every
// node in the current topology" (checks.Spec's own doc comment); timeoutNs
// stays optional here so an unset value lets the server apply its own
// per-pair default/clamp (checks.clampTimeout).
export interface RunCreateRequest {
  type: CheckType;
  plane?: string;
  sources?: string[];
  destinations?: string[];
  timeoutNs?: number;
}

// RunCreateResponse mirrors POST /api/v1/runs's 202 body (httpapi
// runCreateResponse). wsTopic is server-chosen (ws.RunTopic's own canonical
// format) -- the browser subscribes to the name it was told, never one it
// builds itself by string concatenation.
export interface RunCreateResponse {
  id: string;
  status: string;
  pairTotal: number;
  wsTopic: string;
}

// RunSummary mirrors httpapi's runSummary: one row of GET /api/v1/runs, and
// the run half of GET /api/v1/runs/{id}'s body.
export interface RunSummary {
  id: string;
  createdAt: string;
  startedAt?: string;
  finishedAt?: string;
  status: string;
  type: string;
  plane: string;
  initiatorKind: string;
  initiatorId: string;
  pairTotal: number;
  pairOk: number;
  pairFailed: number;
}

// RunResult mirrors httpapi's runResultResponse: one row of GET
// /api/v1/runs/{id}'s "results" array.
export interface RunResult {
  sourceNode: string;
  destinationNode: string;
  success: boolean;
  durationNs: number;
  error?: string;
  result?: unknown;
  recordedAt: string;
}

// RunDetail mirrors httpapi's runDetailResponse -- the run plus its per-pair
// results, exactly what the permalink route (pages/run-detail.tsx) renders
// from alone (task-24-brief.md's permalink guarantee).
export interface RunDetail extends RunSummary {
  spec: unknown;
  results: RunResult[];
}

// RunPage mirrors httpapi's runsListResponse: GET /api/v1/runs's body, the
// same keyset-cursor shape as EventPage.
export interface RunPage {
  runs: RunSummary[];
  nextCursor: string;
}

// RunQuery is the browser-side request shape for GET /api/v1/runs.
export interface RunQuery {
  type?: string;
  status?: string;
  cursor?: string;
  limit?: number;
}

// RunProgressFrame mirrors checks.progressFrame -- one per-pair frame on
// run:{id}, states dispatched -> succeeded|failed|timeout.
export interface RunProgressFrame {
  runId: string;
  source: string;
  destination: string;
  state: string;
  success?: boolean;
  durationNs?: number;
  error?: string;
  completed: number;
  total: number;
}

// RunFinishedFrame mirrors checks.finishedFrame -- the terminal frame on
// run:{id}, delivered via hub.CloseTopicWithFinal strictly before the
// topic's own TypeClosed control frame.
export interface RunFinishedFrame {
  state: "finished";
  status: string;
}
