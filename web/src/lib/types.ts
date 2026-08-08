// types.ts — the browser's view of the Console API, in two halves.
//
// M1–M3 shapes (below) stay HAND-WRITTEN mirrors of the Go JSON types: they
// were checked field by field in review as they landed, they carry the
// narrowing the wire cannot express (LiveEventType, RunStatus, the browser-only
// EventQuery/RunQuery request shapes), and rewriting them onto generated
// aliases would be churn with no correctness gain (M4 Decision 4).
//
// M4 shapes — targets, check definitions, schedules, the projection — are
// RE-EXPORTED from ./api-types, which `npm run gen:api` generates from
// docs/console-api.yaml. That is the half where hand-checking had started to
// miss things: five CRUD resources with near-identical, repetitive bodies.
// A router-walking test (internal/console/httpapi/openapi_test.go) keeps the
// spec's route list joined to the live chi router, and CI regenerates
// api-types.ts and fails on a diff, so neither half can drift silently.
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

// TargetQuery/CheckDefinitionQuery/ScheduleQuery are the browser-side request
// shapes for the three M4 list endpoints — the same half of this file
// EventQuery/RunQuery live in, and for the same reason: a request shape is not
// in the OpenAPI components map (its fields are individual query PARAMETERS,
// not a schema), so it cannot be re-exported from api-types.ts. Same
// convention as getEvents/getRuns: a field is present only when the caller
// supplied it, and an absent field means "server default", never an explicit
// empty value on the wire.
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

/* ── M5: MTR path history ───────────────────────────────────────────────────
   Same half of this file the M4 shapes live in, for the same reason: these
   are generated from docs/console-api.yaml, and CI fails on a regeneration
   diff, so a server-side field rename cannot pass silently as a browser-side
   `undefined`.

   MTRHop and Enrichment are re-exported even though M5 Task 6 only reads
   `hops` off a PathSnapshot: Task 7's hop table and enrichment row are typed
   against exactly these names, and introducing them once here is the
   convention the plan's type-consistency note asks for. */
export type MTRDestination = components["schemas"]["MTRDestination"];
export type MTRDestinationList = components["schemas"]["MTRDestinationList"];
export type MTRHop = components["schemas"]["MTRHop"];
export type Enrichment = components["schemas"]["Enrichment"];
export type PathSnapshot = components["schemas"]["PathSnapshot"];
export type PathSnapshotPage = components["schemas"]["PathSnapshotPage"];

/**
 * PathSnapshotQuery is the browser-side request shape for GET
 * /api/v1/mtr/snapshots — hand-written for the same reason
 * TargetQuery/EventQuery are: query PARAMETERS are not a schema in the
 * OpenAPI components map, so they cannot be re-exported from api-types.ts.
 *
 * `source` and `destination` are REQUIRED here, not optional-with-a-default,
 * because the server refuses an unfiltered listing with 422 (console-api.yaml:
 * "the whole table has no UI and no bound"). Making them required in the type
 * turns that 422 into a compile error instead of a runtime surprise.
 */
export interface PathSnapshotQuery {
  source: string;
  destination: string;
  limit?: number;
  cursor?: string;
}

/* ── M5: annotations ────────────────────────────────────────────────────────
   Generated half again (docs/console-api.yaml), same reasoning as the M4 and
   MTR shapes above.

   The one thing worth reading off the schema rather than guessing: `endAt` is
   OPTIONAL and its absence is the whole INSTANT/RANGE distinction — an
   annotation with no endAt is a mark at a moment, not a span that is "still
   open". lib/annotations.ts turns exactly that field into markLine vs
   markArea, and nothing else decides it. */
export type Annotation = components["schemas"]["Annotation"];
export type AnnotationRequest = components["schemas"]["AnnotationRequest"];
export type AnnotationPage = components["schemas"]["AnnotationPage"];

/**
 * AnnotationQuery is the browser-side request shape for GET
 * /api/v1/annotations — hand-written like every other *Query here, because
 * query parameters are not a schema in the OpenAPI components map.
 *
 * `scope` is the field that does NOT follow this file's usual "absent means
 * server default" convention, and the difference is load-bearing: the endpoint
 * reads THREE states out of one parameter (docs/console-api.yaml's
 * listAnnotations).
 *
 *   scope ABSENT (undefined here)       → every scope
 *   scope PRESENT-BUT-EMPTY ("" here)   → the GLOBAL annotations only
 *   scope = anything else               → exact match
 *
 * So `undefined` and `""` mean genuinely different things on the wire, which is
 * why lib/api.ts's listAnnotations serialises it with `!== undefined` rather
 * than the truthiness test every other filter in that file uses.
 */
export interface AnnotationQuery {
  from?: Date;
  to?: Date;
  scope?: string;
  limit?: number;
  cursor?: string;
}

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
/**
 * Topology is GET /api/v1/topology's body. The five optional fields are the
 * `?at=` (Time Machine) half ONLY — httpapi's `historicalTopology` adds them
 * and the live controller snapshot omits them entirely, so `historical` being
 * undefined is itself the signal "this is live", never a default that could be
 * confused with a historical answer.
 *
 * They are the fold's honesty budget (internal/console/httpapi/data.go):
 * `eventsFolded` events were replayed into this node set, `unfoldableEvents`
 * more were seen and could NOT be — they carry no node identity — and
 * `truncated` says the fold hit its own bound. An EMPTY node set next to a
 * large `unfoldableEvents` is therefore not a broken response: it is the
 * console saying the events do not name anybody. pages/topology.tsx renders
 * exactly that case, from these numbers rather than from a hardcoded guess.
 */
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
// "running" -> finalStatus's "succeeded" | "failed" | "partial"), plus
// "cancelled" — the status a run reaches after POST /api/v1/runs/{id}/cancel
// (docs/console-api.yaml's cancelRun: "the run reaches status `cancelled`").
// NOTE: the spec's own RunStatus enum does NOT list "cancelled" yet, so
// api-types.ts cannot supply it; this hand-written mirror is the only place
// the browser learns that a cancelled run is finished, which is what stops
// useRun's poll.
export type RunStatus = "pending" | "running" | "succeeded" | "failed" | "partial" | "cancelled";
export const RUN_TERMINAL_STATUSES: RunStatus[] = ["succeeded", "failed", "partial", "cancelled"];

// RunCreateRequest is POST /api/v1/runs's body (internal/console/httpapi
// runCreateRequest). An absent/empty sources or destinations means "every
// node in the current topology" (checks.Spec's own doc comment); timeoutNs
// stays optional here so an unset value lets the server apply its own
// per-pair default/clamp (checks.clampTimeout).
//
// The three destination* fields are M4's external-destination half and mirror
// docs/console-api.yaml's RunCreateRequest exactly: destinationKind defaults
// to "node" (the pre-M4 contract, which is why a node run still sends none of
// them at all — an absent field, not an explicit "node"), "target" resolves a
// saved target row by destinationTargetId, "adhoc" probes destinationAddress,
// and both external kinds require `destinations` to be empty
// (resolveRunDestination in internal/console/httpapi/runs.go).
export interface RunCreateRequest {
  type: CheckType;
  plane?: string;
  sources?: string[];
  destinations?: string[];
  timeoutNs?: number;
  destinationKind?: DestinationKind;
  destinationTargetId?: string;
  destinationAddress?: string;
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
