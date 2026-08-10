import type {
  Annotation,
  AnnotationPage,
  AnnotationQuery,
  AnnotationRequest,
  AuditPage,
  AuditQuery,
  CheckDefinition,
  CheckDefinitionPage,
  CheckDefinitionQuery,
  CheckDefinitionRequest,
  Config,
  ConfigBundle,
  ConfigImportRequest,
  ConfigImportResult,
  EventPage,
  EventQuery,
  Incident,
  IncidentPage,
  IncidentPatchRequest,
  IncidentQuery,
  IncidentRequest,
  K8sEventPage,
  K8sEventQuery,
  MTRDestinationList,
  MaintenanceQuery,
  MaintenanceWindow,
  MaintenanceWindowPage,
  MaintenanceWindowRequest,
  Matrix,
  Me,
  PathSnapshot,
  PathSnapshotPage,
  PathSnapshotQuery,
  Problem,
  Projection,
  PromResult,
  Protocol,
  RunCreateRequest,
  RunCreateResponse,
  RunDetail,
  RunPage,
  RunQuery,
  Schedule,
  SchedulePage,
  ScheduleQuery,
  ScheduleRequest,
  Target,
  TargetPage,
  TargetQuery,
  TargetRequest,
  TokenCreateRequest,
  TokenCreateResponse,
  TokenList,
  Topology,
  Version,
  Webhook,
  WebhookList,
  WebhookRequest,
} from "./types";
// The one instant formatter (RFC 3339, UTC, seconds) the URL's ?at= and every
// request built from it share — see getTopology below.
import { formatAtParam } from "./timemachine";

export class ApiError extends Error {
  constructor(public problem: Problem) {
    super(problem.detail ?? problem.title);
    this.name = "ApiError";
  }
}

/** retryUnlessClientError is the app-wide react-query retry predicate (main.tsx). */
export function retryUnlessClientError(failureCount: number, error: unknown): boolean {
  if (error instanceof ApiError) {
    const s = error.problem.status;
    if (s !== undefined && s >= 400 && s < 500 && s !== 429) return false;
  }
  return failureCount < 1;
}

// LOGIN_PATH is the SPA route (web/src/pages/login.tsx), distinct from
// Config.auth.loginPath (the backend endpoint/redirect target a submitted
// login POSTs to or navigates to). redirectToLogin below always lands here.
const LOGIN_PATH = "/login";

const CSRF_COOKIE_NAME = "csrf";
const CSRF_HEADER_NAME = "X-CSRF-Token";

// MUTATING_METHODS mirrors the server's own isMutatingMethod
// (internal/console/httpapi/middleware_auth.go) exactly: the same set gets
// the CSRF header attached here that requires it there.
const MUTATING_METHODS = new Set(["POST", "PUT", "PATCH", "DELETE"]);

// POST /api/v1/auth/login's own 401 means "wrong credentials", not "your session is gone".
const NO_REDIRECT_ON_401 = new Set(["/api/v1/auth/login"]);

function readCookie(name: string): string | undefined {
  for (const part of document.cookie.split("; ")) {
    const eq = part.indexOf("=");
    if (eq === -1) continue;
    if (decodeURIComponent(part.slice(0, eq)) === name) return decodeURIComponent(part.slice(eq + 1));
  }
  return undefined;
}

function browserNavigate(path: string): void {
  window.location.assign(path);
}

// navigate is the one seam every full-page browser navigation in this module (and login.tsx's
// post-login/oidc-redirect-home navigations) goes through; it exists ONLY because jsdom's
// window.location.assign is an own, non-configurable.
let navigate: (path: string) => void = browserNavigate;

/** Test-only seam: see `navigate`'s doc comment. */
export function setNavigateForTest(fn: (path: string) => void): void {
  navigate = fn;
}

/** Test-only seam: restores the real browser navigation after a test. */
export function resetNavigateForTest(): void {
  navigate = browserNavigate;
}

// goTo is the one function pages call to perform a full browser navigation (pages/login.tsx:
// redirect home in header/anonymous mode, redirect to returnTo after a successful local login).
export function goTo(path: string): void {
  navigate(path);
}

// redirectToLogin sends the browser to /login with the current location preserved as ?returnTo=.
function redirectToLogin(): void {
  if (window.location.pathname === LOGIN_PATH) return;
  const returnTo = encodeURIComponent(window.location.pathname + window.location.search);
  navigate(`${LOGIN_PATH}?returnTo=${returnTo}`);
}

/**
 * apiFetch is the one fetch call every function below goes through; a 403 is deliberately left
 * alone here: it renders inline on whatever page triggered it (callers just catch the ApiError this
 * eventually throws via `handle`).
 */
async function apiFetch(input: string, init: RequestInit = {}): Promise<Response> {
  const method = (init.method ?? "GET").toUpperCase();
  const headers = new Headers(init.headers);
  if (MUTATING_METHODS.has(method)) {
    const csrf = readCookie(CSRF_COOKIE_NAME);
    if (csrf) headers.set(CSRF_HEADER_NAME, csrf);
  }
  const resp = await fetch(input, { ...init, credentials: "same-origin", headers });
  if (resp.status === 401 && !NO_REDIRECT_ON_401.has(input)) redirectToLogin();
  return resp;
}

async function handle<T>(resp: Response): Promise<T> {
  if (resp.ok) return (await resp.json()) as T;
  const ct = resp.headers.get("Content-Type") ?? "";
  if (ct.includes("problem+json")) throw new ApiError((await resp.json()) as Problem);
  // PromQL upstream errors come back as Prometheus's own envelope with 4xx.
  if (ct.includes("json")) {
    const body = (await resp.json()) as PromResult;
    if (body.status === "error") return body as T;
  }
  throw new ApiError({ type: "about:blank", title: resp.statusText, status: resp.status });
}

// handleVoid is `handle`'s counterpart for endpoints that answer 204 No
// Content on success (login, logout) — `handle` would try to JSON-parse an
// empty body and throw a SyntaxError instead of the real problem+json.
async function handleVoid(resp: Response): Promise<void> {
  if (resp.ok) return;
  const ct = resp.headers.get("Content-Type") ?? "";
  if (ct.includes("problem+json")) throw new ApiError((await resp.json()) as Problem);
  throw new ApiError({ type: "about:blank", title: resp.statusText, status: resp.status });
}

/**
 * getTopology is GET /api/v1/topology: the LIVE controller snapshot; the instant is rendered by
 * formatAtParam, the SAME formatter the URL's `?at=` uses.
 */
export function getTopology(at?: Date): Promise<Topology> {
  const suffix = at ? `?at=${encodeURIComponent(formatAtParam(at))}` : "";
  // Go marshals a nil slice as JSON null, and a controller with agents but no node informer data
  // answers {"nodes":null,...}.
  return apiFetch(`/api/v1/topology${suffix}`)
    .then((r) => handle<Topology>(r))
    .then((t) => ({ ...t, nodes: t.nodes ?? [], agents: t.agents ?? [] }));
}

export function getVersion(): Promise<Version> {
  return apiFetch("/api/v1/version").then((r) => handle<Version>(r));
}

export function getConfig(): Promise<Config> {
  return apiFetch("/api/v1/config").then((r) => handle<Config>(r));
}

// getMe is GET /api/v1/auth/me: "who am I"; public route (no permission decision), but the
// anonymous exemption lives server-side.
export function getMe(): Promise<Me> {
  return apiFetch("/api/v1/auth/me").then((r) => handle<Me>(r));
}

// login is POST /api/v1/auth/login (auth.mode=local only).
export function login(username: string, password: string): Promise<void> {
  return apiFetch("/api/v1/auth/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username, password }),
  }).then(handleVoid);
}

// logout is POST /api/v1/auth/logout — a mutation, so it goes through the
// same CSRF path as everything else in apiFetch; idempotent and mode-
// agnostic server-side (safe to call with no session at all).
export function logout(): Promise<void> {
  return apiFetch("/api/v1/auth/logout", { method: "POST" }).then(handleVoid);
}

/**
 * getEvents serves GET /api/v1/events (internal/console/httpapi handleEvents); `q.types` becomes
 * one repeated `type=` param per entry (the server's own `q["type"]` reads a Go http.Request the
 * same way).
 */
export function getEvents(q: EventQuery = {}): Promise<EventPage> {
  const qs = new URLSearchParams();
  for (const t of q.types ?? []) qs.append("type", t);
  if (q.scope) qs.set("scope", q.scope);
  if (q.scopeNode) qs.set("scopeNode", q.scopeNode);
  if (q.from) qs.set("from", q.from.toISOString());
  if (q.to) qs.set("to", q.to.toISOString());
  if (q.limit !== undefined) qs.set("limit", String(q.limit));
  if (q.cursor) qs.set("cursor", q.cursor);
  const suffix = qs.toString();
  return apiFetch(`/api/v1/events${suffix ? `?${suffix}` : ""}`).then((r) => handle<EventPage>(r));
}

export function getMatrix(protocol: Protocol, plane = "pod"): Promise<Matrix> {
  const qs = new URLSearchParams({ protocol, plane });
  // Same nil-slice normalization as getTopology — a fleet with no cells yet
  // must arrive as [], not null (types.ts promises arrays).
  return apiFetch(`/api/v1/matrix?${qs}`)
    .then((r) => handle<Matrix>(r))
    .then((m) => ({ ...m, nodes: m.nodes ?? [], cells: m.cells ?? [] }));
}

export function promqlQuery(query: string, time?: Date): Promise<PromResult> {
  return apiFetch("/api/v1/promql/query", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ query, ...(time ? { time: time.toISOString() } : {}) }),
  }).then((r) => handle<PromResult>(r));
}

export function promqlQueryRange(query: string, start: Date, end: Date, stepNs: number): Promise<PromResult> {
  return apiFetch("/api/v1/promql/query_range", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ query, start: start.toISOString(), end: end.toISOString(), step: stepNs }),
  }).then((r) => handle<PromResult>(r));
}

// createRun is POST /api/v1/runs: starts a new diagnostics run.
export function createRun(req: RunCreateRequest): Promise<RunCreateResponse> {
  return apiFetch("/api/v1/runs", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<RunCreateResponse>(r));
}

// getRun is GET /api/v1/runs/{id}: the run plus its per-pair results.
export function getRun(id: string): Promise<RunDetail> {
  return apiFetch(`/api/v1/runs/${encodeURIComponent(id)}`).then((r) => handle<RunDetail>(r));
}

// cancelRun is POST /api/v1/runs/{id}/cancel: 204 (so handleVoid, not
// handle), and 204 for the two outcomes the server deliberately treats as
// non-errors too — cancelling a run that reached a terminal status a moment
// Cancellation is ASYNCHRONOUS: the 204 only means "accepted".
export function cancelRun(id: string): Promise<void> {
  return apiFetch(`/api/v1/runs/${encodeURIComponent(id)}/cancel`, { method: "POST" }).then(handleVoid);
}

// getRuns is GET /api/v1/runs: one page of run history, newest first,
// behind an opaque keyset cursor, filtered by ?type=&status= -- the
// Diagnostics page's history list. Same "absent field means server
// default" convention as getEvents.
export function getRuns(q: RunQuery = {}): Promise<RunPage> {
  const qs = new URLSearchParams();
  if (q.type) qs.set("type", q.type);
  if (q.status) qs.set("status", q.status);
  if (q.cursor) qs.set("cursor", q.cursor);
  if (q.limit !== undefined) qs.set("limit", String(q.limit));
  const suffix = qs.toString();
  return apiFetch(`/api/v1/runs${suffix ? `?${suffix}` : ""}`).then((r) => handle<RunPage>(r));
}

/*
 * targets, check definitions, schedules All ten functions below ride the same apiFetch (credentials
 * + CSRF header on every mutation) and the same handle<T>/handleVoid pair everything above uses.
 */

// listTargets is GET /api/v1/targets: one page of external probe targets,
// newest first, behind the same opaque keyset cursor getRuns/getEvents use.
// Requires targets:read.
export function listTargets(q: TargetQuery = {}): Promise<TargetPage> {
  const qs = new URLSearchParams();
  if (q.kind) qs.set("kind", q.kind);
  if (q.limit !== undefined) qs.set("limit", String(q.limit));
  if (q.cursor) qs.set("cursor", q.cursor);
  const suffix = qs.toString();
  return apiFetch(`/api/v1/targets${suffix ? `?${suffix}` : ""}`).then((r) => handle<TargetPage>(r));
}

// getTarget is GET /api/v1/targets/{id}: ONE target, which is exactly what the
// Target card permalink (pages/target-card.tsx) renders its header from on a cold, bookmarked load.
export function getTarget(id: string): Promise<Target> {
  return apiFetch(`/api/v1/targets/${encodeURIComponent(id)}`).then((r) => handle<Target>(r));
}

// createTarget is POST /api/v1/targets (201 + Location); a duplicate name is 422, not 409
// (docs/console-api.yaml: "a rejected field value in an otherwise well-formed body").
export function createTarget(req: TargetRequest): Promise<Target> {
  return apiFetch("/api/v1/targets", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<Target>(r));
}

// updateTarget is PUT /api/v1/targets/{id}: a FULL replace — an omitted field
// means empty, never "leave as-is" — answering the stored row, not an echo of
// the request. Callers must therefore send every field, including labels.
export function updateTarget(id: string, req: TargetRequest): Promise<Target> {
  return apiFetch(`/api/v1/targets/${encodeURIComponent(id)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<Target>(r));
}

// deleteTarget is DELETE /api/v1/targets/{id}: 204, so handleVoid rather than
// handle. 409 while any check definition still references the target — a
// problem+json like any other, surfaced to the caller as an ApiError.
export function deleteTarget(id: string): Promise<void> {
  return apiFetch(`/api/v1/targets/${encodeURIComponent(id)}`, { method: "DELETE" }).then(handleVoid);
}

// listChecks is GET /api/v1/checks: one page of check definitions, newest
// first. `enabled` is a real tri-state here — absent means "no filter", and
// the server treats anything that is not "true"/"false" as unset — so it is
// only serialised when the caller passed a boolean.
export function listChecks(q: CheckDefinitionQuery = {}): Promise<CheckDefinitionPage> {
  const qs = new URLSearchParams();
  if (q.targetId) qs.set("targetId", q.targetId);
  if (q.enabled !== undefined) qs.set("enabled", String(q.enabled));
  if (q.limit !== undefined) qs.set("limit", String(q.limit));
  if (q.cursor) qs.set("cursor", q.cursor);
  const suffix = qs.toString();
  return apiFetch(`/api/v1/checks${suffix ? `?${suffix}` : ""}`).then((r) => handle<CheckDefinitionPage>(r));
}

// The projection guard runs server-side before the write and ONLY for a definition arriving
// enabled: over the limit it is 422.
export function createCheck(req: CheckDefinitionRequest): Promise<CheckDefinition> {
  return apiFetch("/api/v1/checks", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<CheckDefinition>(r));
}

// updateCheck is PUT /api/v1/checks/{id}: a full replace, same contract and
// same enabled-only projection guard as createCheck.
export function updateCheck(id: string, req: CheckDefinitionRequest): Promise<CheckDefinition> {
  return apiFetch(`/api/v1/checks/${encodeURIComponent(id)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<CheckDefinition>(r));
}

export function deleteCheck(id: string): Promise<void> {
  return apiFetch(`/api/v1/checks/${encodeURIComponent(id)}`, { method: "DELETE" }).then(handleVoid);
}

// checksProjection is POST /api/v1/checks/projection: what a DRAFT definition would project against
// the current topology.
export function checksProjection(req: CheckDefinitionRequest): Promise<Projection> {
  return apiFetch("/api/v1/checks/projection", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<Projection>(r));
}

// listSchedules is GET /api/v1/schedules; note the permission: reading schedules rides on
// checks:read.
export function listSchedules(q: ScheduleQuery = {}): Promise<SchedulePage> {
  const qs = new URLSearchParams();
  if (q.definitionId) qs.set("definitionId", q.definitionId);
  if (q.limit !== undefined) qs.set("limit", String(q.limit));
  if (q.cursor) qs.set("cursor", q.cursor);
  const suffix = qs.toString();
  return apiFetch(`/api/v1/schedules${suffix ? `?${suffix}` : ""}`).then((r) => handle<SchedulePage>(r));
}

// createSchedule is POST /api/v1/schedules (201); the body's cross-field rules are the server's
// (store.ScheduleInput.Validate + httpapi's decodeScheduleRequest).
export function createSchedule(req: ScheduleRequest): Promise<Schedule> {
  return apiFetch("/api/v1/schedules", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<Schedule>(r));
}

// updateSchedule is PUT /api/v1/schedules/{id}: a FULL replace, like every other write in this API.
export function updateSchedule(id: string, req: ScheduleRequest): Promise<Schedule> {
  return apiFetch(`/api/v1/schedules/${encodeURIComponent(id)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<Schedule>(r));
}

// deleteSchedule is DELETE /api/v1/schedules/{id}: 204, so handleVoid.
export function deleteSchedule(id: string): Promise<void> {
  return apiFetch(`/api/v1/schedules/${encodeURIComponent(id)}`, { method: "DELETE" }).then(handleVoid);
}

/*
 * MTR path history All three ride the same apiFetch and the same handle<T> everything above uses;
 * the permission card on the /mtr page therefore exists for hand-rolled roles.
 */

// getMTRDestinations is GET /api/v1/mtr/destinations: every (source, destination) pair path history
// knows about.
export function getMTRDestinations(): Promise<MTRDestinationList> {
  return apiFetch("/api/v1/mtr/destinations").then((r) => handle<MTRDestinationList>(r));
}

// getMTRSnapshots is GET /api/v1/mtr/snapshots: one page of the DISTINCT
// routes a pair has taken, newest last_seen first, behind the same opaque
// keyset cursor getRuns/getEvents use; unlike every other list function here the two filters are
// not optional.
export function getMTRSnapshots(q: PathSnapshotQuery): Promise<PathSnapshotPage> {
  const qs = new URLSearchParams({ source: q.source, destination: q.destination });
  if (q.limit !== undefined) qs.set("limit", String(q.limit));
  if (q.cursor) qs.set("cursor", q.cursor);
  return apiFetch(`/api/v1/mtr/snapshots?${qs}`).then((r) => handle<PathSnapshotPage>(r));
}

// getMTRSnapshot is GET /api/v1/mtr/snapshots/{id}: one stored path with its full hop payload;
// `enrich` is only ever sent as the literal "true" the server keys.
export function getMTRSnapshot(id: string, enrich = false): Promise<PathSnapshot> {
  const suffix = enrich ? "?enrich=true" : "";
  return apiFetch(`/api/v1/mtr/snapshots/${encodeURIComponent(id)}${suffix}`).then((r) => handle<PathSnapshot>(r));
}

/* Reading needs annotations:read, which EVERY built-in role holds; creating and deleting need annotations:write. */

/**
 * listAnnotations is GET /api/v1/annotations: the marks OVERLAPPING [from,to); the endpoint reads
 * three states out of the one parameter — absent = every scope.
 */
export function listAnnotations(q: AnnotationQuery = {}): Promise<AnnotationPage> {
  const qs = new URLSearchParams();
  if (q.from) qs.set("from", q.from.toISOString());
  if (q.to) qs.set("to", q.to.toISOString());
  if (q.scope !== undefined) qs.set("scope", q.scope);
  if (q.limit !== undefined) qs.set("limit", String(q.limit));
  if (q.cursor) qs.set("cursor", q.cursor);
  const suffix = qs.toString();
  return apiFetch(`/api/v1/annotations${suffix ? `?${suffix}` : ""}`).then((r) => handle<AnnotationPage>(r));
}

// createAnnotation is POST /api/v1/annotations (201 + Location); there is deliberately no createdBy
// in the body.
// callers must OMIT the key rather than send "" or null for an instant.
export function createAnnotation(req: AnnotationRequest): Promise<Annotation> {
  return apiFetch("/api/v1/annotations", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<Annotation>(r));
}

// deleteAnnotation is DELETE /api/v1/annotations/{id}: 204, so handleVoid.
// Deleting one that is not there is 404, not success — there is no update in
// M5, so delete-then-create is the only way to correct a mark.
export function deleteAnnotation(id: string): Promise<void> {
  return apiFetch(`/api/v1/annotations/${encodeURIComponent(id)}`, { method: "DELETE" }).then(handleVoid);
}

/*
 * All four are READS and each rides a DIFFERENT permission — events:read for the K8s events,
 * incidents:read, maintenance:read.
 */

/**
 * getK8sEvents is GET /api/v1/k8s-events: captured cluster events; worth knowing before reading an
 * empty page as an outage.
 */
export function getK8sEvents(q: K8sEventQuery = {}): Promise<K8sEventPage> {
  const qs = new URLSearchParams();
  if (q.name) qs.set("name", q.name);
  if (q.kind) qs.set("kind", q.kind);
  if (q.type) qs.set("type", q.type);
  if (q.from) qs.set("from", q.from.toISOString());
  if (q.to) qs.set("to", q.to.toISOString());
  if (q.limit !== undefined) qs.set("limit", String(q.limit));
  if (q.cursor) qs.set("cursor", q.cursor);
  const suffix = qs.toString();
  return apiFetch(`/api/v1/k8s-events${suffix ? `?${suffix}` : ""}`).then((r) => handle<K8sEventPage>(r));
}

/** getIncidents is GET /api/v1/incidents. */
export function getIncidents(q: IncidentQuery = {}): Promise<IncidentPage> {
  const qs = new URLSearchParams();
  if (q.status) qs.set("status", q.status);
  if (q.scope !== undefined) qs.set("scope", q.scope);
  if (q.from) qs.set("from", q.from.toISOString());
  if (q.to) qs.set("to", q.to.toISOString());
  if (q.limit !== undefined) qs.set("limit", String(q.limit));
  if (q.cursor) qs.set("cursor", q.cursor);
  const suffix = qs.toString();
  return apiFetch(`/api/v1/incidents${suffix ? `?${suffix}` : ""}`).then((r) => handle<IncidentPage>(r));
}

// getIncident is GET /api/v1/incidents/{id} — the permalink's own read
// (/investigate?incident={id} hydrates scope and range from this body, M6 Task
// 8). An unknown id is 404 problem+json, i.e. an ApiError here.
export function getIncident(id: string): Promise<Incident> {
  return apiFetch(`/api/v1/incidents/${encodeURIComponent(id)}`).then((r) => handle<Incident>(r));
}

/**
 * createIncident is POST /api/v1/incidents (201 + Location + the row); `toAt` must be OMITTED
 * rather than sent empty for an open-ended incident.
 */
export function createIncident(req: IncidentRequest): Promise<Incident> {
  return apiFetch("/api/v1/incidents", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<Incident>(r));
}

/** patchIncident is PATCH /api/v1/incidents/{id} — the ONE PATCH in this API. */
export function patchIncident(id: string, req: IncidentPatchRequest): Promise<Incident> {
  return apiFetch(`/api/v1/incidents/${encodeURIComponent(id)}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<Incident>(r));
}

// deleteIncident is DELETE /api/v1/incidents/{id}: 204, so handleVoid.
// Deleting one that is not there is 404, not success.
export function deleteIncident(id: string): Promise<void> {
  return apiFetch(`/api/v1/incidents/${encodeURIComponent(id)}`, { method: "DELETE" }).then(handleVoid);
}

/**
 * getMaintenance is GET /api/v1/maintenance: declared change windows whose own span OVERLAPS
 * [from,to), newest-starting first.
 */
export function getMaintenance(q: MaintenanceQuery = {}): Promise<MaintenanceWindowPage> {
  const qs = new URLSearchParams();
  if (q.scope !== undefined) qs.set("scope", q.scope);
  if (q.from) qs.set("from", q.from.toISOString());
  if (q.to) qs.set("to", q.to.toISOString());
  if (q.limit !== undefined) qs.set("limit", String(q.limit));
  if (q.cursor) qs.set("cursor", q.cursor);
  const suffix = qs.toString();
  return apiFetch(`/api/v1/maintenance${suffix ? `?${suffix}` : ""}`).then((r) => handle<MaintenanceWindowPage>(r));
}

/**
 * createMaintenance is POST /api/v1/maintenance (201 + Location + the row); no createdBy, exactly
 * as with createAnnotation and createIncident.
 */
export function createMaintenance(req: MaintenanceWindowRequest): Promise<MaintenanceWindow> {
  return apiFetch("/api/v1/maintenance", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<MaintenanceWindow>(r));
}

// deleteMaintenance is DELETE /api/v1/maintenance/{id}: 204, so handleVoid.
// There is no update: a window declared wrong is deleted and re-declared, the
// same correction path annotations have.
export function deleteMaintenance(id: string): Promise<void> {
  return apiFetch(`/api/v1/maintenance/${encodeURIComponent(id)}`, { method: "DELETE" }).then(handleVoid);
}

/** getAuditEntries is GET /api/v1/audit. */
export function getAuditEntries(q: AuditQuery = {}): Promise<AuditPage> {
  const qs = new URLSearchParams();
  if (q.subjectKind) qs.set("subjectKind", q.subjectKind);
  if (q.subjectId) qs.set("subjectId", q.subjectId);
  if (q.limit !== undefined) qs.set("limit", String(q.limit));
  if (q.cursor) qs.set("cursor", q.cursor);
  const suffix = qs.toString();
  return apiFetch(`/api/v1/audit${suffix ? `?${suffix}` : ""}`).then((r) => handle<AuditPage>(r));
}

/* Permissions: all six webhook routes take webhooks:manage and BOTH bundle routes take settings:write. */

/**
 * listWebhooks is GET /api/v1/webhooks; `hasSecret` is all a reader learns, and it is always true
 * for a stored row.
 */
export function listWebhooks(): Promise<WebhookList> {
  return apiFetch("/api/v1/webhooks").then((r) => handle<WebhookList>(r));
}

/**
 * createWebhook is POST /api/v1/webhooks (201 + Location + the row, minus the secret it was just
 * given); `secret` is REQUIRED here: every delivery is signed.
 */
export function createWebhook(req: WebhookRequest): Promise<Webhook> {
  return apiFetch("/api/v1/webhooks", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<Webhook>(r));
}

/**
 * updateWebhook is PUT /api/v1/webhooks/{id}: a FULL replace like every other write in this API;
 * sending "" is 422 on purpose (neither "keep" nor "clear" would be more than a guess about what an
 * operator meant by a blank box).
 */
export function updateWebhook(id: string, req: WebhookRequest): Promise<Webhook> {
  return apiFetch(`/api/v1/webhooks/${encodeURIComponent(id)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<Webhook>(r));
}

// deleteWebhook is DELETE /api/v1/webhooks/{id}: 204, so handleVoid. Deleting
// one that is not there is 404, not success.
export function deleteWebhook(id: string): Promise<void> {
  return apiFetch(`/api/v1/webhooks/${encodeURIComponent(id)}`, { method: "DELETE" }).then(handleVoid);
}

/** testWebhook is POST /api/v1/webhooks/{id}/test: 202 with no body. */
export function testWebhook(id: string): Promise<void> {
  return apiFetch(`/api/v1/webhooks/${encodeURIComponent(id)}/test`, { method: "POST" }).then(handleVoid);
}

/**
 * listTokens is GET /api/v1/tokens: metadata only, and the list includes revoked rows — `revokedAt`
 * is what tells them apart.
 */
export function listTokens(): Promise<TokenList> {
  // Same nil-slice normalization getTopology explains: a console that has
  // minted nothing answers `tokens: null`.
  return apiFetch("/api/v1/tokens")
    .then((r) => handle<TokenList>(r))
    .then((l) => ({ ...l, tokens: l.tokens ?? [] }));
}

/**
 * createToken is POST /api/v1/tokens. Its response is the ONLY place the raw wire-form secret ever
 * exists on the client, so nothing here caches it.
 */
export function createToken(req: TokenCreateRequest): Promise<TokenCreateResponse> {
  return apiFetch("/api/v1/tokens", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<TokenCreateResponse>(r));
}

/** deleteToken is DELETE /api/v1/tokens/{id}: a REVOKE (204), and 404 for one already gone. */
export function deleteToken(id: string): Promise<void> {
  return apiFetch(`/api/v1/tokens/${encodeURIComponent(id)}`, { method: "DELETE" }).then(handleVoid);
}

/**
 * exportConfig is GET /api/v1/export: the whole declarative configuration as one versioned JSON
 * bundle; it returns the PARSED bundle rather than triggering a browser download.
 */
export function exportConfig(): Promise<ConfigBundle> {
  return apiFetch("/api/v1/export").then((r) => handle<ConfigBundle>(r));
}

/**
 * `dryRun` is a BODY flag rather than a query parameter because the flag and the bundle it applies
 * to are one indivisible statement.
 */
export function importConfig(bundle: ConfigBundle, dryRun: boolean): Promise<ConfigImportResult> {
  const req: ConfigImportRequest = { dryRun, bundle };
  return apiFetch("/api/v1/import", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  })
    .then((r) => handle<ConfigImportResult>(r))
    .then((res) => {
      // Go marshals nil slices as null: a collection with no errors arrives as
      // {"errors":null,"warnings":null} and took the whole import renderer down (QA scope 6 #1 —
      // the same trap getTopology normalizes).
      const out = { ...res } as Record<string, unknown>;
      for (const [k, v] of Object.entries(out)) {
        if (v !== null && typeof v === "object" && "created" in (v as object)) {
          const c = v as { errors: unknown; warnings: unknown };
          out[k] = { ...c, errors: c.errors ?? [], warnings: c.warnings ?? [] };
        }
      }
      return out as unknown as ConfigImportResult;
    });
}

/* alert rules (pages/alerting.tsx) An APPEND-ONLY block, imports included. */
import type {
  AlertRule,
  AlertRuleImportReport,
  AlertRuleImportRequest,
  AlertRuleList,
  AlertRulePreview,
  AlertRuleRequest,
  ForeignRuleList,
  SyncKick,
} from "./types";

/**
 * listAlertRules is GET /api/v1/alert-rules: every console-managed rule, ordered by name; that is
 * the ONLY dependency this route has — it does not need the reconciler.
 */
export function listAlertRules(): Promise<AlertRuleList> {
  return apiFetch("/api/v1/alert-rules").then((r) => handle<AlertRuleList>(r));
}

/**
 * createAlertRule is POST /api/v1/alert-rules (201 + the stored row, including the `renderedExpr`
 * the SERVER produced from the builder fields); the expression is never sent: for a template kind
 * the console does not accept.
 */
export function createAlertRule(req: AlertRuleRequest): Promise<AlertRule> {
  return apiFetch("/api/v1/alert-rules", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<AlertRule>(r));
}

/** updateAlertRule is PUT /api/v1/alert-rules/{id} — a FULL replace. */
export function updateAlertRule(id: string, req: AlertRuleRequest): Promise<AlertRule> {
  return apiFetch(`/api/v1/alert-rules/${encodeURIComponent(id)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<AlertRule>(r));
}

// deleteAlertRule is DELETE /api/v1/alert-rules/{id}: 204, so handleVoid.
// Deleting a rule that is not there is 404, not success.
export function deleteAlertRule(id: string): Promise<void> {
  return apiFetch(`/api/v1/alert-rules/${encodeURIComponent(id)}`, { method: "DELETE" }).then(handleVoid);
}

/**
 * syncAlertRules is POST /api/v1/alert-rules/{id}/sync: 202 with `{"status": "kicked"}`; this is
 * the one place in the API where those two come apart.
 */
export function syncAlertRules(id: string): Promise<SyncKick> {
  return apiFetch(`/api/v1/alert-rules/${encodeURIComponent(id)}/sync`, { method: "POST" }).then((r) =>
    handle<SyncKick>(r),
  );
}

/**
 * previewAlertRule is POST /api/v1/alert-rules/preview: render the builder fields into PromQL; it
 * takes the SAME body as create/update, which is the point.
 */
export function previewAlertRule(req: AlertRuleRequest): Promise<AlertRulePreview> {
  return apiFetch("/api/v1/alert-rules/preview", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<AlertRulePreview>(r));
}

/** listForeignAlertRules is GET /api/v1/alert-rules/foreign. */
export function listForeignAlertRules(): Promise<ForeignRuleList> {
  return apiFetch("/api/v1/alert-rules/foreign").then((r) => handle<ForeignRuleList>(r));
}

/**
 * importForeignAlertRules is POST /api/v1/alert-rules/import; the named object is never mutated and
 * never deleted.
 */
export function importForeignAlertRules(name: string): Promise<AlertRuleImportReport> {
  const req: AlertRuleImportRequest = { name };
  return apiFetch("/api/v1/alert-rules/import", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<AlertRuleImportReport>(r));
}

/* alert STATE (pages/overview.tsx + pages/investigate.tsx) the transport. */
import type { AlertList } from "./types";

/**
 * listAlerts is GET /api/v1/alerts: what Prometheus is firing RIGHT NOW; NOT an error and NOT a
 * 503: "nothing is firing" and "nothing is evaluating" are two different sentences the Overview
 * card must render.
 */
export function listAlerts(): Promise<AlertList> {
  return apiFetch("/api/v1/alerts").then((r) => handle<AlertList>(r));
}
