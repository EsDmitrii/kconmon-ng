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
