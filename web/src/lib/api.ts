import type {
  Config,
  EventPage,
  EventQuery,
  Matrix,
  Me,
  Problem,
  PromResult,
  Protocol,
  RunCreateRequest,
  RunCreateResponse,
  RunDetail,
  RunPage,
  RunQuery,
  Topology,
  Version,
} from "./types";

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

export function getTopology(): Promise<Topology> {
  return apiFetch("/api/v1/topology").then((r) => handle<Topology>(r));
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
