// types.ts — the browser's view of the Console API.
import type { components } from "./api-types";

export type Target = components["schemas"]["Target"];
export type TargetKind = components["schemas"]["TargetKind"];
export type TargetRequest = components["schemas"]["TargetRequest"];
export type TargetPage = components["schemas"]["TargetPage"];

export type CheckDefinition = components["schemas"]["CheckDefinition"];
export type CheckDefinitionRequest = components["schemas"]["CheckDefinitionRequest"];
export type CheckDefinitionPage = components["schemas"]["CheckDefinitionPage"];
export type SourceSelection = components["schemas"]["SourceSelection"];
export type DestinationKind = components["schemas"]["DestinationKind"];

// Projection is what POST /api/v1/checks/projection answers: what ONE
// definition would project against the CURRENT topology. `overLimit` true is
// exactly what create/update refuse with 422 for an enabled definition.
export type Projection = components["schemas"]["Projection"];

// Same convention as getEvents/getRuns: a field is present only when the caller supplied.
export interface TargetQuery {
  kind?: TargetKind;
  limit?: number;
  cursor?: string;
}
export interface CheckDefinitionQuery {
  targetId?: string;
  enabled?: boolean;
  limit?: number;
  cursor?: string;
}
export interface ScheduleQuery {
  definitionId?: string;
  limit?: number;
  cursor?: string;
}

export type Schedule = components["schemas"]["Schedule"];
// "cron" is deliberately absent from ScheduleKind — the server refuses it with
// a 422 naming the milestone it lands in, so the form must not offer it.
export type ScheduleKind = components["schemas"]["ScheduleKind"];
export type ScheduleRequest = components["schemas"]["ScheduleRequest"];
export type SchedulePage = components["schemas"]["SchedulePage"];

/* MTRHop and Enrichment are re-exported even though. */
export type MTRDestination = components["schemas"]["MTRDestination"];
export type MTRDestinationList = components["schemas"]["MTRDestinationList"];
export type MTRHop = components["schemas"]["MTRHop"];
export type Enrichment = components["schemas"]["Enrichment"];
export type PathSnapshot = components["schemas"]["PathSnapshot"];
export type PathSnapshotPage = components["schemas"]["PathSnapshotPage"];

/** PathSnapshotQuery is the browser-side request shape for GET /api/v1/mtr/snapshots. */
export interface PathSnapshotQuery {
  source: string;
  destination: string;
  limit?: number;
  cursor?: string;
}

/* The one thing worth reading off the schema rather than guessing. */
export type Annotation = components["schemas"]["Annotation"];
export type AnnotationRequest = components["schemas"]["AnnotationRequest"];
export type AnnotationPage = components["schemas"]["AnnotationPage"];

/**
 * AnnotationQuery is the browser-side request shape for GET /api/v1/annotations — hand-written like
 * every other *Query here.
 */
export interface AnnotationQuery {
  from?: Date;
  to?: Date;
  scope?: string;
  limit?: number;
  cursor?: string;
}

/*
 * Two of them carry the annotations `scope` three-state (undefined = every scope, "" = the GLOBAL
 * ones only, anything else = exact).
 */
export type K8sEvent = components["schemas"]["K8sEvent"];
export type K8sEventKind = components["schemas"]["K8sEventKind"];
export type K8sEventType = components["schemas"]["K8sEventType"];
export type K8sEventPage = components["schemas"]["K8sEventPage"];

export type Incident = components["schemas"]["Incident"];
export type IncidentStatus = components["schemas"]["IncidentStatus"];
export type IncidentRequest = components["schemas"]["IncidentRequest"];
export type IncidentPatchRequest = components["schemas"]["IncidentPatchRequest"];
export type IncidentPage = components["schemas"]["IncidentPage"];
export type PinnedRef = components["schemas"]["PinnedRef"];

export type MaintenanceWindow = components["schemas"]["MaintenanceWindow"];
export type MaintenanceWindowRequest = components["schemas"]["MaintenanceWindowRequest"];
export type MaintenanceWindowPage = components["schemas"]["MaintenanceWindowPage"];

export type AuditEntry = components["schemas"]["AuditEntry"];
export type AuditPage = components["schemas"]["AuditPage"];

export interface K8sEventQuery {
  /** Node or pod name; an EXACT match server-side, never a prefix. */
  name?: string;
  kind?: K8sEventKind;
  type?: K8sEventType;
  from?: Date;
  to?: Date;
  limit?: number;
  cursor?: string;
}

export interface IncidentQuery {
  status?: IncidentStatus;
  /** Three-state, as on /api/v1/annotations — see AnnotationQuery above. */
  scope?: string;
  from?: Date;
  to?: Date;
  limit?: number;
  cursor?: string;
}

export interface MaintenanceQuery {
  /** Three-state, as on /api/v1/annotations — see AnnotationQuery above. */
  scope?: string;
  from?: Date;
  to?: Date;
  limit?: number;
  cursor?: string;
}

/**
 * AuditQuery is the shape GET /api/v1/audit actually takes; a caller that wants a window asks for a
 * page and filters it client-side — and must say.
 */
export interface AuditQuery {
  subjectKind?: SubjectKind;
  subjectId?: string;
  limit?: number;
  cursor?: string;
}

/* Two things about these shapes are contracts rather than incidental typing, and the Settings page leans on both. */
export type Webhook = components["schemas"]["Webhook"];
export type WebhookEvent = components["schemas"]["WebhookEvent"];
export type WebhookRequest = components["schemas"]["WebhookRequest"];
export type WebhookList = components["schemas"]["WebhookList"];

/* API tokens. `Token` is metadata only — the schema structurally cannot carry a
 * hash or a secret, and TokenCreateResponse is the one body that ever does. */
export type Token = components["schemas"]["Token"];
export type TokenList = components["schemas"]["TokenList"];
export type TokenCreateRequest = components["schemas"]["TokenCreateRequest"];
export type TokenCreateResponse = components["schemas"]["TokenCreateResponse"];

export type ConfigBundle = components["schemas"]["ConfigBundle"];
export type ConfigImportRequest = components["schemas"]["ConfigImportRequest"];
export type ConfigImportResult = components["schemas"]["ConfigImportResult"];
export type ConfigImportCollectionResult = components["schemas"]["ConfigImportCollectionResult"];
export type ConfigImportItemNote = components["schemas"]["ConfigImportItemNote"];

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
/** Topology is GET /api/v1/topology's body; the five optional fields are the `?at=` (Time Machine) half. */
export interface Topology {
  nodes: TopologyNode[];
  agents: TopologyAgent[];
  timestamp: string;
  historical?: boolean;
  asOf?: string;
  eventsFolded?: number;
  unfoldableEvents?: number;
  truncated?: boolean;
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

// LiveEvent mirrors internal/console/events.LiveEvent — the browser-facing projection of the
// controller's pb.Event.
export type LiveEventType =
  | "topology_changed"
  | "check_observed"
  | "mtr_triggered"
  | "mtr_completed"
  | "diagnostic_progress";
export type LiveEventSeverity = "info" | "warn" | "error";
export interface LiveEvent {
  id: string;
  // Controller-assigned, gapless per controller; this is the loss signal for the live feed.
  seq: number;
  type: LiveEventType;
  severity: LiveEventSeverity;
  scope: string;
  timestamp: string;
  summary: string;
  details: unknown;
}
// EventPage mirrors GET /api/v1/events's body (internal/console/httpapi eventsResponse); NextCursor
// is never absent on the wire — an exhausted page still answers "".
export interface EventPage {
  events: LiveEvent[];
  nextCursor: string;
}

// EventQuery is the browser-side request shape for GET /api/v1/events.
export interface EventQuery {
  types?: LiveEventType[];
  scope?: string;
  // scopeNode matches a node/target NAME on either side of the scope: the bare
  // scope, or a pair scope "<source>→<destination>" naming it. Mutually
  // exclusive with `scope` — the server answers 422 when both are sent.
  scopeNode?: string;
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

// From `capabilities` is computed per replica: ["events"] while this replica's realtime pipeline is
// healthy, [] otherwise.
export interface Version {
  version: string;
  commit: string;
  capabilities: string[];
}

// The Live page's scrollback reads only `database.configured`.
export interface Config {
  auth: { mode: string; role: string; loginPath: string };
  anonymousBanner: boolean;
  controller: { configured: boolean };
  prometheus: { configured: boolean };
  database: { configured: boolean };
}

export const PROTOCOLS: Protocol[] = ["tcp", "udp", "icmp"];

// `subject.kind` mirrors Go's authz.SubjectKind ("anonymous" | "user" | "token").
export type SubjectKind = "anonymous" | "user" | "token";
export interface Me {
  subject: { kind: SubjectKind; id: string; displayName: string; groups: string[]; roles: string[] };
  permissions: string[];
}

// CheckType mirrors checks.Spec's own comment (internal/console/checks/checks.go) -- the
// controller's validCheckTypes.
export type CheckType = "tcp" | "udp" | "icmp" | "dns" | "http" | "mtr";
export const CHECK_TYPES: CheckType[] = ["tcp", "udp", "icmp", "dns", "http", "mtr"];

// RunStatus mirrors checks.Runner's lifecycle (memory.go: "pending" -> "running" -> finalStatus's
// "succeeded" | "failed" | "partial"); NOTE: the spec's own RunStatus enum does NOT list
// "cancelled".
export type RunStatus = "pending" | "running" | "succeeded" | "failed" | "partial" | "cancelled";
export const RUN_TERMINAL_STATUSES: RunStatus[] = ["succeeded", "failed", "partial", "cancelled"];

// RunCreateRequest is POST /api/v1/runs's body (internal/console/httpapi runCreateRequest).
export interface RunCreateRequest {
  type: CheckType;
  plane?: string;
  sources?: string[];
  destinations?: string[];
  timeoutNs?: number;
  destinationKind?: DestinationKind;
  destinationTargetId?: string;
  destinationAddress?: string;
  /** Absent or 0 probes each pair ONCE (an instant run, the default). */
  durationNs?: number;
}

// RunCreateResponse mirrors POST /api/v1/runs's 202 body (httpapi runCreateResponse).
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
  /** 0-based probe number within this pair. Always 0 for an instant run;
   *  0..n-1 for an interval run. (sourceNode, destinationNode, sampleSeq)
   *  identifies one sample. */
  sampleSeq: number;
}

// RunDetail mirrors httpapi's runDetailResponse -- the run plus its per-pair results.
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
  /** Which probe of this pair the frame reports, 0-based. Always 0 for an
   *  instant run; an interval run counts up per round. */
  sampleSeq?: number;
}

// RunFinishedFrame mirrors checks.finishedFrame -- the terminal frame on
// run:{id}, delivered via hub.CloseTopicWithFinal strictly before the
// topic's own TypeClosed control frame.
export interface RunFinishedFrame {
  state: "finished";
  status: string;
}

/*
 * alert rules (the Alerting page's three sections) Generated half again; this block is APPENDED
 * rather than filed next to the webhook/bundle aliases above on purpose.
 */
export type AlertRule = components["schemas"]["AlertRule"];
export type AlertRuleKind = components["schemas"]["AlertRuleKind"];
export type AlertRuleRequest = components["schemas"]["AlertRuleRequest"];
export type AlertRuleList = components["schemas"]["AlertRuleList"];
export type AlertRulePreview = components["schemas"]["AlertRulePreview"];
export type AlertSeverity = components["schemas"]["AlertSeverity"];
export type AlertSyncStatus = components["schemas"]["AlertSyncStatus"];
export type SyncKick = components["schemas"]["SyncKick"];

export type ForeignRule = components["schemas"]["ForeignRule"];
export type ForeignRuleList = components["schemas"]["ForeignRuleList"];
export type AlertRuleImportRequest = components["schemas"]["AlertRuleImportRequest"];
export type AlertRuleImportItem = components["schemas"]["AlertRuleImportItem"];
export type AlertRuleImportNote = components["schemas"]["AlertRuleImportNote"];
export type AlertRuleImportReport = components["schemas"]["AlertRuleImportReport"];

/*
 * alert STATE The two aliases the block above deliberately left out, appended after it for the same
 * reason it was appended.
 */
export type Alert = components["schemas"]["Alert"];
export type AlertList = components["schemas"]["AlertList"];
