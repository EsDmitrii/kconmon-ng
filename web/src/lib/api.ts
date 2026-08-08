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

// POST /api/v1/auth/login's own 401 means "wrong credentials", not "your
// session is gone" — the login form already renders that inline (see
// pages/login.tsx), and redirecting to /login FROM /login on a rejected
// submit is nonsensical (and would blow away the just-typed error message).
// Every other endpoint's 401 goes through the normal apiFetch redirect.
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

// navigate is the one seam every full-page browser navigation in this
// module (and login.tsx's post-login/oidc-redirect-home navigations) goes
// through. It exists ONLY because jsdom's window.location.assign is an own,
// non-configurable, non-writable property in the jsdom version this repo
// pins — Object.defineProperty (which is what vi.spyOn does under the hood)
// throws "Cannot redefine property: assign", so a test cannot stub the real
// thing the way ws.test.ts's comment already notes for window.location as a
// whole. setNavigateForTest below swaps this out instead; production code
// never calls it.
let navigate: (path: string) => void = browserNavigate;

/** Test-only seam: see `navigate`'s doc comment. */
export function setNavigateForTest(fn: (path: string) => void): void {
  navigate = fn;
}

/** Test-only seam: restores the real browser navigation after a test. */
export function resetNavigateForTest(): void {
  navigate = browserNavigate;
}

// goTo is the one function pages call to perform a full browser navigation
// (pages/login.tsx: redirect home in header/anonymous mode, redirect to
// returnTo after a successful local login) — same `navigate` seam
// redirectToLogin uses below, so a test only ever has to stub one thing.
export function goTo(path: string): void {
  navigate(path);
}

// redirectToLogin sends the browser to /login with the current location
// preserved as ?returnTo=, EXCEPT when it is already there — otherwise a
// login page that itself calls getMe (to know whether it should redirect
// home instead) would 401 into an infinite loop. In auth.mode=anonymous a
// 401 cannot happen at all (handleAuthMe/authenticate never fail for the
// anonymous subject), so reaching here in that mode is itself a bug; this
// still surfaces it as a single redirect rather than looping, and the
// thrown ApiError (see apiFetch/handle) keeps it diagnosable.
function redirectToLogin(): void {
  if (window.location.pathname === LOGIN_PATH) return;
  const returnTo = encodeURIComponent(window.location.pathname + window.location.search);
  navigate(`${LOGIN_PATH}?returnTo=${returnTo}`);
}

/**
 * apiFetch is the one fetch call every function below goes through
 * (task-19-brief.md: attach `credentials: "same-origin"` and the CSRF
 * header "in the shared fetch path, once, so no future endpoint can forget
 * it"). It also owns the 401 -> /login redirect. A 403 is deliberately left
 * alone here: it renders inline on whatever page triggered it (callers just
 * catch the ApiError this eventually throws via `handle`), never a
 * redirect — bouncing an operator to a login page they are already logged
 * into is the worst version of this.
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
 * getTopology is GET /api/v1/topology: the LIVE controller snapshot, or — with
 * `at` — the historical fold of `topology_events` up to that instant (M5
 * Decision 6, handled server-side; client-side replay would ship the whole
 * event history to the browser on every slider move).
 *
 * The instant is rendered by formatAtParam, the SAME formatter the URL's `?at=`
 * uses, deliberately: a shared link and the request it produces must name the
 * identical second, or a reader and the person they sent the link to would be
 * looking at two different topologies. A future `at` is the server's 400, but
 * lib/timemachine.tsx clamps before it ever gets here.
 */
export function getTopology(at?: Date): Promise<Topology> {
  const suffix = at ? `?at=${encodeURIComponent(formatAtParam(at))}` : "";
  return apiFetch(`/api/v1/topology${suffix}`).then((r) => handle<Topology>(r));
}

export function getVersion(): Promise<Version> {
  return apiFetch("/api/v1/version").then((r) => handle<Version>(r));
}

export function getConfig(): Promise<Config> {
  return apiFetch("/api/v1/config").then((r) => handle<Config>(r));
}

// getMe is GET /api/v1/auth/me: "who am I". Public route (no permission
// decision), but the anonymous exemption lives server-side, not here — this
// still 401s like any other call would for a genuinely unauthenticated
// non-anonymous subject, and apiFetch's redirect handles it the same way.
export function getMe(): Promise<Me> {
  return apiFetch("/api/v1/auth/me").then((r) => handle<Me>(r));
}

// login is POST /api/v1/auth/login (auth.mode=local only). 204 on success
// (session + csrf cookies set by the server response, nothing to read here);
// 401 problem+json on bad credentials, surfaced as ApiError so the login
// form can show it inline without navigating.
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
 * getEvents serves GET /api/v1/events (internal/console/httpapi handleEvents),
 * the REST scrollback counterpart to the "live" WebSocket topic — same
 * LiveEvent shape, so a page merges into the socket-fed ring with no
 * translation. `q.types` becomes one repeated `type=` param per entry (the
 * server's own `q["type"]` reads a Go http.Request the same way); every other
 * field is a single key, present only when the caller supplied it — an absent
 * field means "server default", never an explicit empty value on the wire.
 * A 400 (malformed cursor/params) or 503 (history disabled) both come back as
 * problem+json and surface as ApiError via the shared `handle`.
 */
export function getEvents(q: EventQuery = {}): Promise<EventPage> {
  const qs = new URLSearchParams();
  for (const t of q.types ?? []) qs.append("type", t);
  if (q.scope) qs.set("scope", q.scope);
  if (q.from) qs.set("from", q.from.toISOString());
  if (q.to) qs.set("to", q.to.toISOString());
  if (q.limit !== undefined) qs.set("limit", String(q.limit));
  if (q.cursor) qs.set("cursor", q.cursor);
  const suffix = qs.toString();
  return apiFetch(`/api/v1/events${suffix ? `?${suffix}` : ""}`).then((r) => handle<EventPage>(r));
}

export function getMatrix(protocol: Protocol, plane = "pod"): Promise<Matrix> {
  const qs = new URLSearchParams({ protocol, plane });
  return apiFetch(`/api/v1/matrix?${qs}`).then((r) => handle<Matrix>(r));
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

// createRun is POST /api/v1/runs (task-23-brief.md): starts a new
// diagnostics run. 202 on success (Location: /api/v1/runs/{id}); 422
// problem+json when the spec is well-formed but refused for what it would
// produce (too many pairs, no pairs); 400 for an unrecognized check type;
// 503 when no controller is configured (s.runner nil). requires runs:create
// server-side -- the Diagnostics page hides the form instead of calling
// this without it (useAuth().can), but this function itself enforces
// nothing; the server is the only real gate.
export function createRun(req: RunCreateRequest): Promise<RunCreateResponse> {
  return apiFetch("/api/v1/runs", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<RunCreateResponse>(r));
}

// getRun is GET /api/v1/runs/{id}: the run plus its per-pair results --
// exactly what the run permalink route (pages/run-detail.tsx) renders from
// alone. 404 problem+json for an unknown id; 503 when no controller is
// configured.
export function getRun(id: string): Promise<RunDetail> {
  return apiFetch(`/api/v1/runs/${encodeURIComponent(id)}`).then((r) => handle<RunDetail>(r));
}

// cancelRun is POST /api/v1/runs/{id}/cancel: 204 (so handleVoid, not
// handle), and 204 for the two outcomes the server deliberately treats as
// non-errors too — cancelling a run that reached a terminal status a moment
// earlier, and cancelling one another replica started. Cancellation is
// ASYNCHRONOUS: the 204 only means "accepted", and the run's own goroutine
// writes the terminal "cancelled" status once its in-flight pairs settle, so
// the caller re-reads GET /api/v1/runs/{id} rather than assuming the new
// status. Requires runs:create — starting fleet-wide probe traffic and
// stopping it are the same operational class (middleware_auth.go).
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

/* ── M4: targets, check definitions, schedules ──────────────────────────────
   All ten functions below ride the same apiFetch (credentials + CSRF header
   on every mutation) and the same handle<T>/handleVoid pair everything above
   uses — no second fetch path, so a future endpoint cannot forget the CSRF
   header by being written against a different wrapper.

   None of them enforces a permission. The Targets page hides affordances it
   has no permission for (useAuth().can), but the server is the only real gate
   — every one of these routes is re-checked in
   internal/console/httpapi/middleware_auth.go's route→permission table, and a
   403 surfaces here as an ApiError like any other problem+json. */

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
// Target card permalink (pages/target-card.tsx) renders its header from on a
// cold, bookmarked load. It is a separate call rather than a find() over
// listTargets' first page on purpose: that page is cursor-paginated, so a
// target beyond it would render as "not found" on a link that is perfectly
// valid. An unknown id and a malformed one are both 404 problem+json
// (docs/console-api.yaml), i.e. indistinguishable here — the card says "this
// target does not exist" for either. Requires targets:read.
export function getTarget(id: string): Promise<Target> {
  return apiFetch(`/api/v1/targets/${encodeURIComponent(id)}`).then((r) => handle<Target>(r));
}

// createTarget is POST /api/v1/targets (201 + Location). A duplicate name is
// 422, not 409 (docs/console-api.yaml: "a rejected field value in an
// otherwise well-formed body"), which is why the Targets page renders every
// 422 detail at a FIELD rather than as a page-level banner.
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

// createCheck is POST /api/v1/checks. The projection guard runs server-side
// before the write and ONLY for a definition arriving enabled: over the limit
// it is 422, while the same definition saved with enabled:false is accepted
// (httpapi's enforceProjection). checksProjection below previews that same
// number, but this call is the arbiter.
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

// checksProjection is POST /api/v1/checks/projection: what a DRAFT definition
// would project against the current topology. Persists nothing, and takes the
// very same CheckDefinitionRequest body create/update take, so the form can
// send the draft it is about to submit unchanged.
//
// Gated on checks:WRITE, not checks:read (middleware_auth.go: "a caller who
// cannot create a definition has no question to ask it"), which is why the
// page only ever calls this from behind a can("checks:write") check — without
// it the call would be a guaranteed 403.
export function checksProjection(req: CheckDefinitionRequest): Promise<Projection> {
  return apiFetch("/api/v1/checks/projection", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<Projection>(r));
}

// listSchedules is GET /api/v1/schedules. Note the permission: reading
// schedules rides on checks:read, since there is no schedules:read at all
// (middleware_auth.go: "reading a cadence tells you nothing the definition it
// belongs to does not already tell you"); only mutations need
// schedules:write. M4 Task 7 uses the read side only.
export function listSchedules(q: ScheduleQuery = {}): Promise<SchedulePage> {
  const qs = new URLSearchParams();
  if (q.definitionId) qs.set("definitionId", q.definitionId);
  if (q.limit !== undefined) qs.set("limit", String(q.limit));
  if (q.cursor) qs.set("cursor", q.cursor);
  const suffix = qs.toString();
  return apiFetch(`/api/v1/schedules${suffix ? `?${suffix}` : ""}`).then((r) => handle<SchedulePage>(r));
}

// createSchedule is POST /api/v1/schedules (201). Requires schedules:write.
// The body's cross-field rules are the server's (store.ScheduleInput.Validate
// + httpapi's decodeScheduleRequest): kind "interval" requires a positive
// intervalNs, kind "once" requires a runAt in the FUTURE, kind "continuous"
// must carry neither — a rejected combination is a 422 problem+json, i.e. an
// ApiError here, not a silent no-op. `nextFireAt` is deliberately not part of
// the body: it is scheduler bookkeeping the server seeds itself.
export function createSchedule(req: ScheduleRequest): Promise<Schedule> {
  return apiFetch("/api/v1/schedules", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<Schedule>(r));
}

// updateSchedule is PUT /api/v1/schedules/{id}: a FULL replace, like every
// other write in this API — an omitted field means empty, never "leave
// as-is". definitionId is not updatable: a body naming a DIFFERENT definition
// is a 422, while an omitted or matching one is fine, so callers send the
// stored row's own definitionId back.
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

/* ── M5: MTR path history ───────────────────────────────────────────────────
   All three ride the same apiFetch and the same handle<T> everything above
   uses. All three require mtr:read, which — unlike M4's five config
   permissions — EVERY built-in role holds, viewer included (M5 Decision 11:
   path history is telemetry, not configuration). The permission card on the
   /mtr page therefore exists for hand-rolled roles only; the far more common
   degraded state is 503 from a console with no database. */

// getMTRDestinations is GET /api/v1/mtr/destinations: every (source,
// destination) pair path history knows about, most-recently-traced first, in
// ONE unpaginated body — the pair count is bounded by the fleet's own size,
// not by trace volume. The page groups it by destination client-side
// (pages/mtr.tsx's groupDestinations); the server ships it flat.
export function getMTRDestinations(): Promise<MTRDestinationList> {
  return apiFetch("/api/v1/mtr/destinations").then((r) => handle<MTRDestinationList>(r));
}

// getMTRSnapshots is GET /api/v1/mtr/snapshots: one page of the DISTINCT
// routes a pair has taken, newest last_seen first, behind the same opaque
// keyset cursor getRuns/getEvents use. Unlike every other list function here
// the two filters are not optional — the server answers 422, not a full
// table, when either is missing (docs/console-api.yaml), which is why
// PathSnapshotQuery makes them required rather than letting a caller discover
// it at runtime.
export function getMTRSnapshots(q: PathSnapshotQuery): Promise<PathSnapshotPage> {
  const qs = new URLSearchParams({ source: q.source, destination: q.destination });
  if (q.limit !== undefined) qs.set("limit", String(q.limit));
  if (q.cursor) qs.set("cursor", q.cursor);
  return apiFetch(`/api/v1/mtr/snapshots?${qs}`).then((r) => handle<PathSnapshotPage>(r));
}

// getMTRSnapshot is GET /api/v1/mtr/snapshots/{id}: one stored path with its
// full hop payload. `enrich` is only ever sent as the literal "true" the
// server keys on, and ONLY when asked for: without it the response's
// `enrichment` field is ABSENT — never null, never an empty object — so "did
// not ask" stays distinguishable from "asked and nothing is known". M5 Task 6
// never asks; Task 7's hop table is what flips it on.
export function getMTRSnapshot(id: string, enrich = false): Promise<PathSnapshot> {
  const suffix = enrich ? "?enrich=true" : "";
  return apiFetch(`/api/v1/mtr/snapshots/${encodeURIComponent(id)}${suffix}`).then((r) => handle<PathSnapshot>(r));
}

/* ── M5: annotations ────────────────────────────────────────────────────────
   Same apiFetch (credentials + CSRF on the two mutations) and the same
   handle<T>/handleVoid pair as everything above. Reading needs
   annotations:read, which EVERY built-in role holds; creating and deleting
   need annotations:write, which only operator and admin do (M5 Decision 11).
   Neither of those is enforced here — the server is the only real gate, and
   the surfaces hide their affordances with useAuth().can purely so an operator
   is not offered a button that is guaranteed to 403. */

/**
 * listAnnotations is GET /api/v1/annotations: the marks OVERLAPPING [from,to)
 * — not the ones whose start falls inside it. A span that began before `from`
 * and is still running at `from` is exactly the mark a chart needs to draw, and
 * the server's own filter says so (docs/console-api.yaml).
 *
 * Read `q.scope`'s serialisation carefully: it is `!== undefined`, NOT the
 * truthiness test every other filter in this file uses, and the difference is
 * the whole contract. The endpoint reads three states out of the one
 * parameter — absent = every scope, present-but-empty (`?scope=`) = the GLOBAL
 * ones only, anything else = an exact match — because "" is a real scope value
 * here rather than a missing one. A `q.scope ? ...` guard would silently
 * collapse "global only" into "everything", which is the exact bug that would
 * scatter a node's private notes across every chart in the console.
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

// createAnnotation is POST /api/v1/annotations (201 + Location). There is
// deliberately no createdBy in the body: attribution is the SERVER's view of
// the authenticated subject, never a client claim. An endAt at or after
// startAt makes a span; omitting it entirely makes an instant mark — so
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

/* ── M6: the Investigate page's read sources ────────────────────────────────
   Same apiFetch and the same handle<T> as everything above. All four are READS
   and each rides a DIFFERENT permission — events:read for the K8s events,
   incidents:read, maintenance:read, audit:read — which is the whole reason
   pages/investigate.tsx calls them one per source behind its own can() check
   rather than through a single "load the timeline" helper: a subject missing
   one permission must lose exactly one row of the timeline, not the page (M6
   Global Constraints: "ZERO requests for sources the operator's role cannot
   read").

   Incidents' write half (POST/PATCH/DELETE) lands below with M6 Task 8, the
   task that owns the UI calling it. */

/**
 * getK8sEvents is GET /api/v1/k8s-events: captured cluster events, newest
 * first, behind the same opaque keyset cursor every other list here uses.
 *
 * Worth knowing before reading an empty page as an outage: with a database but
 * no cluster reader enabled (console.kubernetesContext.enabled) this answers an
 * EMPTY PAGE rather than 503 — "nothing was captured" and "this endpoint is
 * unavailable" are different facts and the API keeps them apart. `name` is an
 * EXACT match on a node or pod name, so a caller that wants both ends of a pair
 * genuinely issues two requests.
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

/**
 * getIncidents is GET /api/v1/incidents. `scope` is serialised with the same
 * `!== undefined` test listAnnotations uses, for the same reason: the endpoint
 * reads three states out of one parameter and "" is the GLOBAL scope, not a
 * missing filter.
 *
 * from/to bound the window an incident's OWN RANGE must OVERLAP — one that
 * began before `from` and is still open is exactly the one an operator looking
 * at that window needs to see.
 */
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
 * createIncident is POST /api/v1/incidents (201 + Location + the row).
 *
 * No createdBy and no status in the body, exactly as with createAnnotation:
 * attribution is the server's view of the authenticated subject, and an
 * incident is ALWAYS created open — resolving it is the PATCH below, the path
 * that stamps resolvedAt from the server's own clock.
 *
 * `toAt` must be OMITTED rather than sent empty for an open-ended incident:
 * its absence means "still going" (note this is the opposite of an
 * annotation's absent endAt, which means an instant).
 */
export function createIncident(req: IncidentRequest): Promise<Incident> {
  return apiFetch("/api/v1/incidents", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<Incident>(r));
}

/**
 * patchIncident is PATCH /api/v1/incidents/{id} — the ONE PATCH in this API,
 * and the reason it is a PATCH rather than this codebase's usual full-replace
 * PUT is collaboration: one operator types notes while another pins a finding
 * and a third resolves it, and a full replace would let the last writer discard
 * the other two.
 *
 * Each field PRESENT replaces that field wholesale. `pinned` in particular is
 * "the list is now this" — which is exactly what a UI holding the rendered list
 * knows — so a caller adding one pin sends the whole array, never a delta. An
 * EMPTY body is 422 rather than a no-op.
 */
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
 * getMaintenance is GET /api/v1/maintenance: declared change windows whose own
 * span OVERLAPS [from,to), newest-starting first. Same three-state `scope` as
 * getIncidents/listAnnotations.
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
 * createMaintenance is POST /api/v1/maintenance (201 + Location + the row).
 *
 * No createdBy, exactly as with createAnnotation and createIncident:
 * attribution is the server's view of the authenticated subject, never a client
 * claim. `endAt` is REQUIRED and must be strictly after `startAt` — the store
 * carries that as a CHECK and the API answers 422 for equal-or-before, so the
 * form mirrors the rule client-side and this call is the last line rather than
 * the first (a maintenance window with no end is an annotation, and that
 * endpoint already exists).
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

/**
 * getAuditEntries is GET /api/v1/audit, newest first.
 *
 * Note what is NOT in AuditQuery: a time window. The endpoint has no from/to
 * (docs/console-api.yaml), so a caller wanting "the config changes inside this
 * investigation's range" asks for a page and filters it by `at` client-side —
 * and owes the operator a sentence saying that a busy console can push older
 * in-range rows off the end of that page. pages/investigate.tsx says exactly
 * that next to the rows.
 */
export function getAuditEntries(q: AuditQuery = {}): Promise<AuditPage> {
  const qs = new URLSearchParams();
  if (q.subjectKind) qs.set("subjectKind", q.subjectKind);
  if (q.subjectId) qs.set("subjectId", q.subjectId);
  if (q.limit !== undefined) qs.set("limit", String(q.limit));
  if (q.cursor) qs.set("cursor", q.cursor);
  const suffix = qs.toString();
  return apiFetch(`/api/v1/audit${suffix ? `?${suffix}` : ""}`).then((r) => handle<AuditPage>(r));
}

/* ── M6 webhooks / M7 configuration export-import ───────────────────────────
   The Settings page's transport (M7 Task 10, plan Decision 10). Same apiFetch
   — credentials + the CSRF header on every mutation — and the same
   handle<T>/handleVoid pair as everything above; there is deliberately no
   second fetch path for the one page whose bodies carry a secret.

   Permissions: all six webhook routes take webhooks:manage and BOTH bundle
   routes take settings:write, each of them admin-only and with no read/write
   split (internal/console/httpapi/middleware_auth.go). Neither is enforced
   here — the page HIDES the sections it has no permission for, and the server
   remains the only real gate. */

/**
 * listWebhooks is GET /api/v1/webhooks. UNPAGED and therefore carrying no
 * cursor: the row count is endpoints an operator typed, not a function of time
 * (docs/console-api.yaml's WebhookList).
 *
 * No secret comes back, at any layer. `hasSecret` is all a reader learns, and
 * it is always true for a stored row — the store refuses an empty one — so it
 * reads as the contract statement "this endpoint signs its deliveries" rather
 * than as a question.
 */
export function listWebhooks(): Promise<WebhookList> {
  return apiFetch("/api/v1/webhooks").then((r) => handle<WebhookList>(r));
}

/**
 * createWebhook is POST /api/v1/webhooks (201 + Location + the row, minus the
 * secret it was just given). `secret` is REQUIRED here: every delivery is
 * signed (M6 Decision 5), so a body without one is 422 and there is no such
 * thing as an endpoint that cannot sign.
 */
export function createWebhook(req: WebhookRequest): Promise<Webhook> {
  return apiFetch("/api/v1/webhooks", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<Webhook>(r));
}

/**
 * updateWebhook is PUT /api/v1/webhooks/{id}: a FULL replace like every other
 * write in this API, with exactly ONE field that is not — `secret`.
 *
 * OMIT the key to keep the stored ciphertext; send a non-empty string to
 * replace it. Sending "" is 422 on purpose (neither "keep" nor "clear" would
 * be more than a guess about what an operator meant by a blank box), which is
 * why callers must delete the key rather than pass an empty string through.
 * JSON.stringify drops an `undefined` property, so a WebhookRequest built
 * without the field puts no `secret` on the wire at all.
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

/**
 * testWebhook is POST /api/v1/webhooks/{id}/test: 202 with no body, so
 * handleVoid rather than handle.
 *
 * 202 and not 200 is the whole contract. Delivery is asynchronous behind a
 * retry ladder, so the only honest thing this can report is that the work was
 * ACCEPTED; the outcome lands on the endpoint row and is read back with
 * listWebhooks — lastStatus/lastAttempt/failures. A caller that renders "test
 * succeeded" off this resolved promise would be inventing a result.
 */
export function testWebhook(id: string): Promise<void> {
  return apiFetch(`/api/v1/webhooks/${encodeURIComponent(id)}/test`, { method: "POST" }).then(handleVoid);
}

/**
 * exportConfig is GET /api/v1/export: the whole declarative configuration as
 * one versioned JSON bundle (plan Decision 9).
 *
 * It returns the PARSED bundle rather than triggering a browser download,
 * deliberately: the download is an anchor + object URL built by the caller
 * (pages/settings.tsx), so the request rides this module's apiFetch like every
 * other — credentials, the 401 redirect, problem+json surfaced as an ApiError.
 * Navigating the tab to /api/v1/export instead would leave a 403 or a 503
 * rendering as raw JSON in place of the console.
 *
 * No secret ever travels in the body: webhooks arrive as a name/url/events
 * triple plus a `hasSecret` boolean about the SOURCE row.
 */
export function exportConfig(): Promise<ConfigBundle> {
  return apiFetch("/api/v1/export").then((r) => handle<ConfigBundle>(r));
}

/**
 * importConfig is POST /api/v1/import. `dryRun` is a BODY flag rather than a
 * query parameter because the flag and the bundle it applies to are one
 * indivisible statement — a parameter dropped by a proxy would turn a preview
 * into an apply — and it is REQUIRED here rather than defaulted, so no caller
 * can write an apply by forgetting an argument.
 *
 * The response shape is byte-identical for a dry run and an apply. That is the
 * entire point of the dry run: what it predicts is what the apply does.
 */
export function importConfig(bundle: ConfigBundle, dryRun: boolean): Promise<ConfigImportResult> {
  const req: ConfigImportRequest = { dryRun, bundle };
  return apiFetch("/api/v1/import", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<ConfigImportResult>(r));
}

/* ── M7 alert rules (pages/alerting.tsx) ─────────────────────────────────────
   An APPEND-ONLY block, imports included. The second `import type` statement
   below is not an accident and not a style slip: M7 Task 7 lands concurrently
   with two other implementers, one of which regenerates api-types.ts, and
   growing the import list at the top of this file would put an edit in the one
   region every other change to this module also touches. TypeScript is happy
   with a second type-only import from the same module, it costs nothing at
   runtime (type imports are erased), and the whole of this task's transport is
   then one contiguous block that cannot conflict with anything above it. */
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
 * listAlertRules is GET /api/v1/alert-rules: every console-managed rule,
 * ordered by name. UNPAGED, listWebhooks' reasoning — the row count is rules an
 * operator configured, not a function of time.
 *
 * 503 (problem+json, surfaced as an ApiError) is the answer on a console with
 * no database: alert rules are persisted CONFIGURATION with no in-memory
 * fallback. That is the ONLY dependency this route has — it does not need the
 * reconciler, and a console with alerting switched off still lists, creates and
 * edits rules perfectly well. See syncAlertRules for the other half of that
 * split.
 */
export function listAlertRules(): Promise<AlertRuleList> {
  return apiFetch("/api/v1/alert-rules").then((r) => handle<AlertRuleList>(r));
}

/**
 * createAlertRule is POST /api/v1/alert-rules (201 + the stored row, including
 * the `renderedExpr` the SERVER produced from the builder fields).
 *
 * The expression is never sent: for a template kind the console does not accept
 * one at all, and for `raw` it travels as params.expr like any other parameter.
 * A body the renderer cannot turn into PromQL is a 422 naming the param.
 */
export function createAlertRule(req: AlertRuleRequest): Promise<AlertRule> {
  return apiFetch("/api/v1/alert-rules", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<AlertRule>(r));
}

/**
 * updateAlertRule is PUT /api/v1/alert-rules/{id} — a FULL replace, with no
 * PATCH counterpart anywhere on this resource (the incidents PATCH stays the
 * one exception in this API).
 *
 * Which makes one omission dangerous enough to state here: `enabled` OMITTED
 * means TRUE, not false, so a PUT built by spreading a partial draft ENABLES a
 * rule somebody deliberately turned off. pages/alerting.tsx's
 * alertRuleRequestFrom always writes the field explicitly, and it is the single
 * place any body on this route is built.
 */
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
 * syncAlertRules is POST /api/v1/alert-rules/{id}/sync: 202 with `{"status":
 * "kicked"}`, which is the whole of what it can honestly say.
 *
 * The kick is a non-blocking, coalescing nudge at a reconciler that applies the
 * WHOLE bundle (one PrometheusRule object holds every enabled rule), so the id
 * is not a filter — it is the rule the operator was looking at when they
 * pressed the button. The OUTCOME lands on the rules themselves and is read
 * back with listAlertRules as syncStatus/syncMessage/lastSyncedAt. A caller
 * that renders "synced" off this resolved promise is inventing a result.
 *
 * 409 — not 503 — is the answer when console.alerting.enabled is false. This is
 * the one place in the API where those two come apart: the database is fine and
 * every rule is right where it was, so 503 ("the dependency this route reads
 * from is not configured") would send an operator looking at their database for
 * a reconciler nobody asked to start.
 */
export function syncAlertRules(id: string): Promise<SyncKick> {
  return apiFetch(`/api/v1/alert-rules/${encodeURIComponent(id)}/sync`, { method: "POST" }).then((r) =>
    handle<SyncKick>(r),
  );
}

/**
 * previewAlertRule is POST /api/v1/alert-rules/preview: render the builder
 * fields into PromQL, then run that expression as an instant query and count
 * the series.
 *
 * A POST that writes nothing and is gated on alerts:READ — the mirror image of
 * the checks projection route. It takes the SAME body as create/update, which
 * is the point: what it previews is what a save would store.
 *
 * The two halves fail independently, so a 200 here is not "it worked". Read
 * `error` first: when it is set the evaluation half did not run or did not
 * answer, and `series` is 0 because nobody counted, not because nothing
 * matched.
 */
export function previewAlertRule(req: AlertRuleRequest): Promise<AlertRulePreview> {
  return apiFetch("/api/v1/alert-rules/preview", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<AlertRulePreview>(r));
}

/**
 * listForeignAlertRules is GET /api/v1/alert-rules/foreign: the PrometheusRule
 * objects in the console's namespace that it does NOT own, projected down to
 * four facts (name, group count, rule count, managed-by label).
 *
 * It needs no database — the answer comes from the cluster — but it does need
 * the reconciler, so a console with alerting off answers 409 naming the flag
 * while the rules list beside it keeps working. That asymmetry is exactly what
 * the Alerting page renders as two different section states.
 */
export function listForeignAlertRules(): Promise<ForeignRuleList> {
  return apiFetch("/api/v1/alert-rules/foreign").then((r) => handle<ForeignRuleList>(r));
}

/**
 * importForeignAlertRules is POST /api/v1/alert-rules/import: adopt a foreign
 * PrometheusRule by COPYING its alerting rules into console-managed rows.
 *
 * COPYING is the whole contract. The named object is never mutated and never
 * deleted — there is no code path from this route to a write against somebody
 * else's object — so after a successful import the same alerts are defined
 * TWICE in the cluster: once by the object its owner still controls, once by
 * the console's own bundle. Removing the original is that owner's decision.
 * pages/alerting.tsx prints that consequence next to the button.
 *
 * The response IS the result: per-item created/skipped/notes, non-transactional
 * (one refused entry does not roll back the ones before it). There is no dryRun
 * because the report is the preview and the apply in one round trip.
 */
export function importForeignAlertRules(name: string): Promise<AlertRuleImportReport> {
  const req: AlertRuleImportRequest = { name };
  return apiFetch("/api/v1/alert-rules/import", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  }).then((r) => handle<AlertRuleImportReport>(r));
}

/* ── M7 alert STATE (pages/overview.tsx + pages/investigate.tsx) ─────────────
   Task 8's transport, appended after Task 7's block for the reason Task 7's own
   header gives: an append-only block cannot collide with a concurrent edit. */
import type { AlertList } from "./types";

/**
 * listAlerts is GET /api/v1/alerts: what Prometheus is firing RIGHT NOW,
 * projected onto this console's vocabulary. There is no history endpoint and
 * this is not one — the answer is a snapshot, and both consumers say so.
 *
 * THREE outcomes, and telling them apart is this route's whole design:
 *   - 200 with `promConfigured:false` and an empty list — nobody is watching.
 *     NOT an error and NOT a 503: "nothing is firing" and "nothing is
 *     evaluating" are two different sentences the Overview card must render.
 *   - 502 (an ApiError carrying the detail) — Prometheus IS wired and did not
 *     answer. Rendering that as an empty firing list would be the most
 *     dangerous lie either surface can tell, so both surface the error.
 *   - 200 with the set.
 *
 * `managedOnly` is deliberately NOT plumbed through. Both callers want the
 * FLEET's firing state: the Overview card is an operator's morning view, and
 * hiding a firing alert because somebody else wrote its rule would make the
 * console's silence mean less than it does. Foreign alerts arrive with no
 * `ruleId` and are tagged in the UI instead. The parameter exists on the route
 * for the webhook watcher, which has the opposite requirement.
 */
export function listAlerts(): Promise<AlertList> {
  return apiFetch("/api/v1/alerts").then((r) => handle<AlertList>(r));
}
