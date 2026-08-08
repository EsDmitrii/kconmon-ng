<!--
Status: current
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §10, §12 in M0 (2026-07-14); §10 rewritten
from the as-built M3 implementation (2026-08-06):
internal/console/authn/{token,password,session,header,oidc}.go,
internal/console/authz/{roles,authz}.go, internal/console/httpapi/{audit,middleware_auth,tokens,rbac}.go.
This document is the source of truth for Security & Observability. Update it (and the ADRs) in the same PR as any deviation.
-->

# Security & Observability

## 10. Authentication & authorization

### 10.1 AuthN modes (all implemented, Helm-selectable)

| Mode        | Notes |
| ----------- | ----- |
| `oidc` (recommended default) | Code flow + PKCE; groups claim → RBAC subjects; server-side refresh; sessions in Valkey/PostgreSQL |
| `local`     | Users in PostgreSQL, argon2id, admin bootstrap via Helm secret |
| `header`    | `X-Remote-User`/`X-Remote-Groups` behind a trusted proxy; explicit opt-in |
| `anonymous` | Dev/demo; fixed role; permanent warning banner |

API tokens (PATs) work in every mode, layered on top of whichever of the four
is configured (`authn.NewTokenFallback` wraps the inner authenticator — a
request carrying a well-formed `Authorization: Bearer kcm_...` never reaches
it at all).

**Header mode's trusted-proxy requirement is enforced, not advisory.**
`console.auth.header.trustedProxyCIDRs` must be a non-empty CIDR list —
`config.HeaderConfig.validate` refuses to boot otherwise, and the chart's
own render-time `fail` guard (CONFIG.md) refuses to render an empty list
first. Trust is decided on `r.RemoteAddr` **only** — the TCP peer that
actually dialed this process — never on `X-Forwarded-For` or any other
header, because those are exactly as attacker-controlled as the identity
headers this mode trusts once it decides to trust the peer at all. A request
from outside the configured CIDRs is treated as "no credentials here", full
stop, even carrying a perfectly well-formed `X-Remote-User`.

**Passwords: argon2id, RFC 9106's memory-conservative profile.**
`internal/console/authn/password.go` hashes with `m=65536 (64 MiB), t=3,
p=2`, RFC 9106's **second** recommended option, not the first-choice 2
GiB/t=1 profile — a console pod's default resource limit
(`console.resources`, 256Mi) would OOM the moment a handful of logins land
concurrently under the 2 GiB profile. Parameters are read back out of the
stored PHC string on verify (`$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>`),
not hardcoded, so a hash produced with different parameters than today's
defaults still verifies. Login pays the same argon2 cost — verified against a
fixed dummy hash — for "unknown username", "disabled account" and "wrong
password" alike, so none of the three is distinguishable by response timing.

**PATs: SHA-256 of the raw secret, not argon2id (Decision 11).** A PAT's
random 32-byte secret is hashed with plain `sha256.Sum256`
(`authn.HashTokenSecret`, the single source of truth both minting
(`httpapi.handleTokensCreate`) and verifying
(`authn.tokenAuthenticator.authenticateToken`) call) and looked up by a
single indexed `WHERE token_hash = $1` equality check — never
`ConstantTimeCompare` against a second hash, because there is no second
hash in this package to compare against; the match is delegated entirely to
Postgres's unique index. This is deliberately **not** argon2id: a PAT's
secret already carries 256 bits of entropy from `crypto/rand`, so there is no
low-entropy-guessing threat argon2id's deliberate slowness would defend
against, and a slow hash on every API-token-authenticated request (every
`kubectl-kconmon` call, every CI pipeline hitting the API) would be a real
latency cost paid for a security property already covered by the secret's
own entropy.

**Disabling a user revokes their PATs — on the next request, not
retroactively.** `authn.WithOwnerDisabledCheck` is opt-in (wired only when a
database is configured); with it, `authenticateToken` performs one extra
`GetUserByID(tok.Owner)` lookup after the token itself verifies, and fails
with `ErrDisabled` — collapsed into the same "invalid" response and metric
label as an unknown/revoked/expired token, so a caller cannot use the
response to enumerate *why* a token failed — if that owner is currently
disabled. This is a live re-query, the same shape `local.go`'s login
already pays against `GetUserByUsername`: only a fresh lookup on every
authenticated request catches a disable flip immediately, not just at
token-creation time. Internally the rejection is WARN-logged with the
token id and owner, so an operator can distinguish "disabled" from "never
valid" even though the caller cannot.

**Token owner = creator's stable subject id, and a token minted by a token
inherits its ROOT owner.** `api_tokens.owner` is the creating subject's
`users.id` UUID for a local user, or the literal `"system"` for a
degenerate case. A token minted by presenting *another* token
(`SubjectToken` creating a new PAT) does **not** record the parent token's
own id as owner — `handleTokensCreate` resolves the parent's owner first
(`resolveInheritedOwner`) and attributes the new token directly to whatever
the parent was ultimately attributed to. This collapses an arbitrarily deep
token-mints-token chain to depth 1: disabling the root user immediately
invalidates every token minted anywhere in that chain, not just the
immediate child. Without this inheritance a parent-token-id owner is a real
UUID that would resolve `GetUserByID` to `ErrNotFound` and be wrongly
treated as "allow" (see the residual below).

**Residual, by design: tokens of header/OIDC-created subjects.** A token
minted by a `SubjectUser` under `auth.mode=header` or `auth.mode=oidc` has an
owner UUID that names no `users` row at all — that subject's disable state
lives upstream, at the proxy or the IdP, not in this database. `GetUserByID`
returns `ErrNotFound` for it, and `checkOwnerDisabled` treats that as
"allow" deliberately: `store.UserStore`'s own contract never claims to
answer for a subject this store never provisioned. Revoking such a token
individually (`DELETE /api/v1/tokens/{id}`) is still available; there is no
automatic revoke tied to the proxy/IdP side disabling that identity.

**A PAT carries no scope of its own: its roles are exactly `auth.defaultRole`.**
Authentication and authorization compose, and for a token subject the second
half is deployment-wide rather than per-token. `httpapi.resolveRoles` asks the
`RoleResolver` for the subject's roles; the only implementation
(`roleResolver.RolesFor`, `cmd/console/main.go`) calls
`ListBindingsForSubject`, whose `WHERE` clause has exactly two branches —
`subject_kind = 'user'` and `subject_kind = 'group'` — and no
`subject_kind = 'token'` branch at all. A `SubjectToken`'s id names an
`api_tokens` row, never a `users` row or a group name, so it matches neither
branch: it comes back with zero bindings and `resolveRoles` falls through to
`defaultRoles()` — the single
value of `auth.defaultRole`, or no roles at all when it is unset (which
`authorize` turns into 403). §10.2's rejection of token-kind bindings is the
same fact stated from the write side, so there is no way to grant one PAT more
or less than another in M3 — every PAT in a deployment has identical
permissions. `auth.defaultRole` is validated at boot to be one of the four
**built-in** roles (`viewer|operator|alert-editor|admin`, `config.Validate`);
a custom role cannot be the default. The same fallback also covers any
authenticated OIDC or header subject whose id and groups match no binding, so
`auth.defaultRole` is best read as "the floor for every authenticated
identity", not as a token setting.

> **Corner worth stating plainly: in `auth.mode=anonymous` with a database
> enabled, presenting a valid PAT can grant *less* access than presenting no
> credential at all.** Without a credential the request is the anonymous
> subject and keeps `auth.anonymous.role` (`resolveRoles` returns anonymous
> subjects untouched); with a `Bearer kcm_...` header the token authenticator
> wins before the anonymous one is ever consulted, and the subject drops to
> `auth.defaultRole` — which defaults to *no roles*, i.e. 403 on everything
> gated. Nothing is broken here, but the direction surprises people. Either
> leave PATs unminted in anonymous mode (they are for the authenticated modes),
> or set `auth.defaultRole` deliberately, at or above what
> `auth.anonymous.role` grants.

### 10.2 RBAC

Roles = named permission sets; bindings map subjects (user/group; **not**
token — see below) to roles.

**Built-in roles are compiled-in code, not database rows (Decision 7).**
`internal/console/authz/roles.go`'s `builtinRoles` map (`viewer`, `operator`,
`alert-editor`, `admin` = `authz.AllPermissions`) is what makes RBAC work
with `database.mode=disabled`: `viewer` holds exactly the M1/M2 endpoint
permissions, so `auth.mode=anonymous` + `auth.anonymous.role=viewer` (the
defaults) stays byte-identical to the pre-M3 surface with no database at
all. The `roles` table (custom roles) only ever adds alongside the
built-ins; it never redefines them, and a custom role named like a built-in
is rejected outright (`422 "reserved role name"` — storing it would create a
row that silently never takes effect, since the authz layer resolves
built-ins first).

**M4's five permissions stop at `operator` (Decision 3).** `targets:read`,
`targets:write`, `checks:read`, `checks:write` and `schedules:write` are
granted to `operator` and — via `AllPermissions` — to `admin`. They are
granted to neither `viewer` nor `alert-editor`. `viewer` is deliberate:
it is what `auth.anonymous.role` defaults to, so granting it `targets:read`
would hand an unauthenticated console the fleet's probe configuration, and
`targets:write` would hand it the authority to point N agents at an
operator-chosen address — the highest-blast-radius action in the product.
The visible consequence with shipped defaults: an anonymous console renders
the Targets page as a permission-explained empty state, not a 500.
`alert-editor` was identical to `operator` through M3 and diverges here;
reconfiguring what the fleet probes is not what that role's name promises.

**M5's three permissions split telemetry from statements (Plan Decision
11).** `mtr:read` and `annotations:read` are TELEMETRY — path history the
fleet already recorded and notes pinned to charts anyone may see — so every
built-in role holds them, including `viewer` (and therefore the anonymous
default): this widens the anonymous surface only by new read-only M5 routes
and changes nothing that existed before. `annotations:write` stops at
`operator` and `admin`: a note pinned to the fleet's history is an operator
statement. Launching a new MTR trace never got its own permission — it is
`runs:create`, the same authority every other on-demand probe requires.

**WebSocket authorization is per-connection, not per-topic.** `GET /ws`
requires exactly one permission, `events:read`, and that single decision
covers every topic multiplexed over the socket — `live`, `topology`,
`matrix:*:pod`, and the ephemeral `run:{id}` topics alike. `ws.Hub` never
receives an `authz.Subject` (`ServeWS` takes only the request; `subscribe`
and `topicAllowed` decide on the topic *name* alone), so there is no layer
that could gate one topic differently from another. Two consequences worth
knowing before you write a custom role:

- A role holding only `runs:read` **cannot open the socket** to watch its
  own run's progress. It gets `403 missing permission: events:read` and must
  fall back to polling `GET /api/v1/runs/{id}`. Grant `events:read`
  alongside `runs:read` for any role that should watch runs live.
- Conversely, `events:read` alone already covers `run:{id}` topics. It is
  not a narrower grant than it looks.

Lowering `/ws` to `runs:read` was considered in M4 and rejected: with
per-connection granularity it would hand every run watcher the `live`
events stream too, which is a genuine widening of exactly what `events:read`
gates on `GET /api/v1/events`. Splitting the two properly means teaching the
hub subject-aware subscribe authorization — a hub change, not a route-table
change — and is not scheduled. Both directions are pinned by tests in
`internal/console/httpapi/auth_test.go`.

**Custom-role API guard rails**, both `422 Unprocessable Entity`:

- `POST /api/v1/rbac/roles` with a name colliding with a built-in
  (`viewer`/`operator`/`alert-editor`/`admin`), or any permission string
  outside the closed `authz.AllPermissions` set.
- `POST /api/v1/rbac/bindings` with `subjectKind="token"`. The `role_bindings`
  schema (migration 00002) declares `token` as a legal `subject_kind`, but
  nothing resolves a token-kind binding yet — `ListBindingsForSubject` only
  ever queries `user`/`group` — so accepting the write would silently store
  a binding that grants nothing. Only `user` and `group` are accepted until
  token subject resolution lands post-M3.

**OIDC subjects resolve group bindings and `defaultRole` only — never
per-user bindings.** `Subject.ID` for an OIDC-authenticated request carries
the resolved **username claim**, not a `users.id` UUID — OIDC user
provisioning does not exist yet, so there is no row a per-user binding could
match against. `role_bindings` rows keyed on `subjectKind="user"` therefore
never apply to an OIDC subject; only `subjectKind="group"` bindings against
the groups claim, plus `auth.defaultRole` as the floor, can grant it
anything. This fails **closed**: an OIDC user with no matching group
binding and no `defaultRole` authenticates successfully but holds zero
permissions (403 on everything), never a silent grant.

**Bootstrap admin: `CountUsers==0` gate, with auto-repair on every
restart.** `auth.local.bootstrapAdmin` (username) +
`auth.local.bootstrapAdminPasswordFile` (Helm: `console.auth.local.existingSecret`)
creates that user, from the password file, the first time `cmd/console`
observes `CountUsers()==0` — a race between replicas on first boot is
resolved by the unique `users.username` constraint, the losing replica logs
and moves on. On **every** subsequent boot, whether or not a user was just
created, `reconcileBootstrapAdminBinding` re-creates that user's `admin`
role binding if it is missing — so an admin who was accidentally unbound
(or a binding lost to manual DB surgery) is auto-repaired on the next
restart. **Unsetting `bootstrapAdmin` is what stops the re-grant**: leaving
it set means the console will keep re-granting `admin` to that username
forever, even after an operator has deliberately demoted or disabled it.

### 10.2.1 External-check destinations: the AGENT is the authority

RBAC decides who may *write* a target. It does not decide what an agent will
*probe*. Those are two separate gates on purpose, and the second one does not
live in the Console at all.

`config.checkers.external.allowedCidrs` / `deniedCidrs` are evaluated by the
**agent**, in-process, against the **resolved** address, immediately before the
probe. The Console never sends a "this destination is approved" flag, and the
agent never trusts one: it re-derives the answer from its own config every
time.

**Why the agent and not the Console.** Consider the blast radius of a
compromised Console — a stolen `targets:write` token, an authz bug, a
supply-chain problem in the console image. If the Console were authoritative,
that single compromise turns every agent in the fleet into an outbound probe
source aimed wherever the attacker chooses: cloud metadata endpoints
(`169.254.169.254`), internal admin planes the cluster can reach but the
attacker cannot, or an external host being flooded from N nodes at once. With
the agent authoritative, the same compromise gets the attacker rows in a
database and 403s on the wire. The blast radius stops at configuration.

That is the same reasoning as `auth.header.trustedProxyCIDRs`: the component
that would be *used* by the attack is the component that must hold the
allowlist.

Consequences an operator should expect, rather than debug:

- **An empty `allowedCidrs` with the feature enabled fails agent startup.** It
  is never read as allow-everything. An empty list denies everything, and an
  operator who wanted "everything" has to type a CIDR that says so.
- **A refused probe is `kconmon_ng_external_denied_total`, not a failed
  check.** `external_results_total` counts probes that reached the network;
  denials are counted separately with `reason=cidr|resolve|disabled`, so
  "the Console assigned something the agents will not probe" is a distinct,
  alertable signal rather than an indistinguishable failure.
- **Agents can disagree with each other, legitimately.** The allowlist is per
  agent config. Denials on one node and clean probes on its peers means that
  node's DaemonSet pod is running different config — which is a real finding,
  and the Node Detail dashboard's denial panel is where it shows up.
- **A NetworkPolicy is not a substitute and cannot be one.** A Kubernetes
  NetworkPolicy has no useful expression of "the whole internet except these
  ranges" for agent egress, and default-deny egress at the node/CNI layer is a
  *separate* gate the chart does not manage. Egress permitted by the agent's
  allowlist and still refused on the wire almost always means that layer was
  missed; the reverse — open at the node layer, denied by the agent — is the
  posture this design wants.

`maxTargets` belongs to this same gate in intent but **is not enforced yet**:
it is validated at agent startup and nothing checks the pushed assignment
against it. Do not count it as a control (see MILESTONES.md's M4 deferral
list).

### 10.3 Console ServiceAccount (K8s RBAC, Helm-gated)

- `monitoring.coreos.com/prometheusrules`: CRUD in its namespace
  (`alerting.enabled`).
- `events`, `nodes`, `pods`: **read-only**, for `kubectx`
  (`kubernetesContext.enabled`; node/event reads are cluster-scoped —
  document this clearly).

Neither is implemented yet (M6/M7).

## 11. Session and CSRF

**`__Host-` session cookie.** `auth.session.cookieName` defaults to
`__Host-kconmon_session`; config validation refuses a `__Host-`-prefixed name
with `auth.session.secure=false` (browsers reject `__Host-` cookies without
`Secure` anyway). The cookie is `HttpOnly`, `SameSite=Lax`, `Path=/`, no
`Domain` — `__Host-`'s own guarantee is "this exact host, this exact path,
Secure required", the strongest cookie-scoping Chrome/Firefox offer.

**Known M3 limitation: sessions follow the bus into the in-process fallback.**
Sessions and the OIDC login-flow state live in a `cache.KV` that is built from
the bus `newBus` returned — `cache.NewValkeyKVFromBus` when that bus is a
`*cache.ValkeyBus`, `cache.NewInProcessKV()` otherwise. So a Valkey that is
merely **unreachable at boot** (as opposed to `valkey.mode=disabled`, which the
Helm guard catches at render time) drops that replica onto in-process sessions
with nothing louder than a WARN — `valkey unreachable at startup — falling back
to the in-process bus; realtime fan-out is single-replica only until the console
is restarted`, whose text does not even mention sessions. The pod still reports
Ready and never retries. Under `auth.mode=local|oidc` with `replicas > 1`, users
balanced onto that replica see apparently random logouts. Alert on that log line
and see CONFIG.md for the operational handling.

**CSRF is keyed on subject KIND, not on cookie presence.** The double-submit
pair (`csrf` cookie, non-`HttpOnly` so the SPA can read it, echoed back as
`X-CSRF-Token`) is required for every mutating request (`POST`/`PUT`/`PATCH`/
`DELETE`) from an `authz.SubjectUser` — session-cookie-authenticated
(local/oidc) **or** header-injected. `authz.SubjectToken` (a PAT) is exempt
unconditionally: a bearer token is never sent ambiently by a browser, so
nothing can forge it cross-site, and requiring the header would break every
CLI/script caller. `authz.SubjectAnonymous` is exempt only under
`auth.mode=anonymous` itself (a genuine no-auth deployment with no login
flow to mint the cookie from). **Header mode gets its CSRF cookie lazily**,
minted on the first authenticated `GET` rather than at a login handler (it
has none — a trusted proxy injects identity from its own cookie on every
request) — this closes a gap local/oidc do not have, since those two mint
the pair at login/callback.

The `csrf` cookie is deliberately **not** `__Host-`-prefixed: `__Host-`
guards against a write (a malicious sibling subdomain planting a
same-named cookie with an attacker-known value), not against a cross-site
page reading it — same-origin cookie rules already block that. What
actually closes the subdomain-tossing gap here is `SameSite=Lax` on the
*session* cookie (stops it riding a cross-site simple request) plus
`X-CSRF-Token` being a non-simple header that a cross-origin fetch cannot
attach without a CORS preflight this server never grants.

## 12. Security, observability, performance

- TLS at ingress; optional in-pod TLS; optional controller gRPC mTLS
  (flag-gated follow-up, not yet implemented).
- CSP, `__Host-` cookies, SameSite=Lax, CSRF tokens for cookie-authed
  mutations, same-origin CORS default. See §11 above for the as-built detail.
- PromQL proxy guards: timeout, max range/step, response size cap,
  per-role gating. Response cache is not yet implemented (DATA.md §5.3).
- Webhook payloads HMAC-signed; secrets encrypted at rest (app-level,
  `settings`-keyed). Not yet implemented (M6/M7 — `webhooks`/`settings`
  tables are still pending, DATA.md §5.2).
- Console exports `kconmon_ng_console_*` metrics (HTTP/WS, scheduler lag,
  event-stream health, DB pool, proxy latency) **and ships self-monitoring
  alert rules** — a broken monitor alerts instead of going quiet. The alert
  rules themselves are not yet implemented (M7 — alerting sync).
- Scale target: 200 nodes / 40k pairs. Canvas heatmap; WS deltas coalesced
  (≤1 update/pair/5s); zone roll-up default above 60 nodes; Live feed
  virtualized; event ingestion backpressure documented.
- slog structured logging consistent with existing binaries.

### The audit log's documented lossiness

The audit log (`audit_log`, written via `httpapi.Auditor.InsertAuditEntry`,
read via `GET /api/v1/audit`) is deliberately **best-effort, not a durable
guarantee**, on both ends:

- **Async, buffered, drop-and-count.** `recordAudit` enqueues onto a small
  fixed-capacity channel (`auditBufferSize = 64`) with a non-blocking send —
  a full buffer means the single drain goroutine (or the database underneath
  it) cannot keep up, and the entry is **dropped**, counted
  (`metrics.AuditDropped`), and WARN-logged, rather than adding latency to
  (or failing) the request that triggered it. A best-effort audit trail must
  never become a backpressure mechanism the rest of the console pays request
  latency for.
- **One drain goroutine, `auditWriteTimeout = 5s` per write.** Writes are
  strictly serialized — one slow or stuck write only delays the writes
  queued behind it, and can never block a live request, since the request
  it describes has already been answered by the time the drain goroutine
  even dequeues it.
- **Lossy at shutdown.** `s.auditCh` is never closed and never drained on
  shutdown — any rows still sitting in the buffer when the process exits are
  lost, uncounted. This is the same "no explicit Stop" lifecycle convention
  `ws.Hub.Run` and the other realtime components use.
- **Lossy on a handler panic.** The chi router chain is `r.Use(s.instrument)`
  only — there is **no `chi.Recoverer`** (or equivalent) registered, so a
  panicking mutation handler never reaches `auditMutation`'s post-handler
  `recordAudit` call at all. This is a real, currently-unmitigated gap, not
  a deliberate trade-off documented elsewhere; it is recorded here as an
  honest limit.
- **Detail is default-deny, allow-listed by top-level JSON key, per route.**
  `auditDetailAllowlist` maps `"METHOD route-pattern"` to the specific body
  keys permitted into `audit_log.detail` (e.g. `POST /api/v1/auth/login` →
  `["username"]` only, never `"password"`; `POST /api/v1/runs` →
  `["type","plane"]`, deliberately excluding the `sources`/`destinations`
  node-name arrays). A mutating route with **no** allow-list entry — every
  PromQL route included — records `{}`, always. Omission is enforced as
  "allow nothing", never "allow everything": a future mutating route added
  without an entry fails safe. No request body, password, raw token, or
  PromQL query string is ever eligible to reach the audit log, by
  construction.
