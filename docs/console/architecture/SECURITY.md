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

**M6's five permissions follow the same two grooves.** `incidents:read` and
`maintenance:read` are telemetry-class — an incident's record and an active
maintenance window are context every role needs, so all four built-in roles
hold them; `incidents:write` and `maintenance:write` are statement-class and
stop at `operator`/`admin`, exactly like `annotations:write`. `webhooks:manage`
is the exception: each endpoint carries an HMAC signing secret, which makes it
credential-adjacent, so it takes the `rbac:manage`/`tokens:manage` posture —
one combined permission, held by `admin` alone via `AllPermissions`. K8s
events deliberately got NO permission of their own: they are events, and
`events:read` already decides who may read the fleet's history — a second
gate on the same class of data would only drift from the first.

**M7's two permissions take the incidents groove, and bring `AllPermissions`
to 25.** `alerts:read` is telemetry-class — the managed rule list, the
expression the console rendered from each rule, and the set Prometheus is
currently firing are context on charts every role already reads, and the
Overview card showing them is the landing page — so all four built-in roles
hold it, `viewer` (the anonymous default) included. It also gates
`POST /api/v1/alert-rules/preview`, which persists nothing: previewing asks
what a draft expression matches right now, which is a read of Prometheus.
`alerts:manage` is statement-class — creating, editing, deleting or
force-syncing a rule is "this fleet should page someone when X" — and lands on
`operator`, `admin`, **and `alert-editor`**: the one deliberate exception to
the incidents:write groove, because delegated alert editing is that role's
entire charter (it sat as a placeholder from M3 to M6 waiting for exactly this
permission, and a builtin named alert-editor that cannot edit an alert rule
breaks its promise on first click; see `roles.go`'s comment). Alert rules
carry no secret, so nothing here takes `webhooks:manage`'s
combined-permission posture.

**WebSocket authorization is two decisions, not one (M7).** The socket
multiplexes topics whose permissions genuinely differ, so it is authorized at
two layers:

1. **The upgrade.** `GET /ws` is the API's only `anyOf` route: it admits a
   subject holding **`events:read` OR `runs:read`**. Holding neither is still
   `403`, naming both.
2. **Each subscribe.** `httpapi.Server.wsTopicAuthorizer` builds a
   per-connection `ws.TopicAuthorizer` from the subject the auth middleware
   already resolved, and `ws.Hub.ServeWSAuthorized` captures it on that one
   connection. A subject with `events:read` gets `nil` — every topic, no
   per-frame work, behaviour identical to pre-M7. A subject admitted on
   `runs:read` alone gets the ephemeral `run:{id}` topics and nothing else;
   `live`, `topology` and `matrix:*:pod` come back as an error frame naming
   `events:read`, on a socket that stays open and keeps serving its run
   topics.

What that buys, and what it deliberately does not:

- A custom role granted `runs:read` can now **watch the run it started**,
  live, instead of polling `GET /api/v1/runs/{id}`. That was M3 follow-up #10,
  carried from M3 to M7.
- The fleet-wide event stream is **not** widened. `live` still needs
  `events:read` on the socket exactly as on `GET /api/v1/events` — which is
  why simply lowering the route to `runs:read` was rejected in M4, and why the
  route change and the per-topic gate are only correct together.
- `events:read` alone still covers `run:{id}` topics. It is not a narrower
  grant than it looks.
- This is **not** a per-run ownership check. `runs:read` is fleet-wide (its
  holder may already `GET` any run by id), so the authorizer permits the
  *class* of topic, not one subject's own runs.

`ws` itself still knows nothing about permissions: `TopicAuthorizer` is a
`func(topic string) error`, so every permission name stays in `httpapi`, the
package that owns the route table. All of the above is pinned by tests in
`internal/console/httpapi/auth_test.go` (`TestWSAdmitsRunsReadForRunWatching`,
`TestWSRunsReadOnlyIsRefusedTheFleetWideTopics`,
`TestWSRefusesTheUpgradeWithoutEitherPermission`,
`TestWSEventsReadAloneCoversRunTopics`) and
`internal/console/ws/conn_test.go`.

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

- `monitoring.coreos.com/prometheusrules`: **`get`, `list`, `watch`, `create`,
  `update`, `patch`** — a namespaced `Role` in the release namespace
  (`console.alerting.enabled`). **Landed M7** — chart 1.9.0.
- `events`: **`list`, `watch`**, cluster-scoped, for `kubectx`
  (`kubernetesContext.enabled`). **Landed M6** — chart 1.8.0.

**The alerting grant is a `Role`, never a `ClusterRole`, and that is the whole
posture.** The console writes exactly one `PrometheusRule` object into exactly
one namespace — its own — so a `Role` is not a tightening of something wider,
it is the shape the feature actually has. Pointing
`console.alerting.namespace` somewhere else does not widen anything: it makes
every apply fail with a `forbidden` that the reconciler faithfully writes into
each rule's `sync_message`, which is a far better failure than a console that
can quietly create rules in a namespace nobody expected it to touch.

The verbs are what one server-side apply actually needs and no more.
`create`/`update`/`patch` are all three required for a **single** `Apply` call —
SSA is a PATCH with the apply content type, but the apiserver charges `create`
when the object does not exist yet and `update` on some paths; this is not three
code paths. `get`/`list`/`watch` cover reading the live object back for the
drift comparison and walking the namespace for `GET /alert-rules/foreign`.
**`delete` is absent on purpose**: the reconciler converges by applying an
*empty* bundle when no rule is enabled, it never removes the object, and a grant
to delete `PrometheusRule`s is a grant to delete somebody else's alerting.

Both grants bind to the **same** console-only ServiceAccount, each under its own
feature flag: alerting on with `kubernetesContext` off gets the `Role` and no
event grant, and the reverse install gets the event grant and no rule-writing
power. The shared subject (plus `serviceAccountName`, `POD_NAMESPACE` and the
apiserver egress rule, which both features need identically) is why chart 1.9.0
routes all four through one `kconmon-ng.console.k8sIdentity` helper instead of
four copies of the same `or`.

The `kubectx` grant is **narrower than this section originally specified**, and
deliberately. `nodes` and `pods` are NOT granted: the reader calls exactly
`CoreV1().Events(ns).List` and `.Watch` and nothing else, because the node set
it filters against comes from the **controller's topology over the controller's
own HTTP API**, not from the apiserver. `get` is absent for the same reason —
the reader never fetches a single event by name. The grant is what the code
calls, not what the plan predicted.

Cluster-scoped rather than a namespaced `Role` because node events are the
point: the kubelet writes them into whatever namespace it chooses, so the
node-event stream cannot be expressed as a `Role` in the release namespace. The
leak-consciousness therefore lives in the READER's filter, not in the grant —
node events are kept only for nodes present in the topology (and **all** of them
dropped when no topology is available: fail closed), pod events only from the
one configured namespace.

**The console does not share the agent/controller ServiceAccount.** Up to chart
1.7.0 the console Deployment set no `serviceAccountName` at all and ran as the
namespace `default` SA, while one ClusterRole (`nodes: get,list,watch`) was
bound to the SA the agent DaemonSet and the controller Deployment share.
Widening that role would have granted event read to **every agent pod on every
node** and still not reached the console. Chart 1.8.0 therefore renders a
console-only ServiceAccount plus its own ClusterRole and binding, all three
gated on `console.kubernetesContext.enabled` — off by default, so a console
that never calls the apiserver is never handed a token that could. Chart 1.9.0
adds `console.alerting.enabled` as a second reason to mint that identity, and
nothing else changed about it.

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
- Webhook payloads HMAC-signed; secrets encrypted at rest (app-level).
  **Landed M6** — but keyed on **config**, not on the `settings` table this
  line originally promised: that table does not exist and is not pinned to a
  milestone, and inventing it to hold one key was scope creep. See §12.1 and
  MILESTONES.md "Deferred out of M6".
- Console exports `kconmon_ng_console_*` metrics (HTTP/WS, scheduler lag,
  event-stream health, DB pool, proxy latency). The self-monitoring **alert
  rules** ship the way they always have: as `prometheusRule.rules` values in
  the chart, rendered into a static `PrometheusRule` under
  `prometheusRule.enabled` and edited in Git. That set covers the agent and the
  control plane (`KconmonAgentsMissing`, `KconmonControllerDown`,
  `UDPLossHigh`, `TCPChecksFailing`, `DNSChecksFailing`,
  `ExternalChecksFailing`) and **nothing over `kconmon_ng_console_*` — the
  console still does not self-monitor out of the box**.
  M7 did *not* change that. The alert **builder**
  (`console.alerting.enabled`) can now hold rules of this shape — a `raw` rule
  over any console metric family takes thirty seconds to declare — but **nothing
  auto-installs them**: the builder starts empty, the reconciler applies only
  what an operator typed, and the two `PrometheusRule` objects (the chart's
  static one and the console-owned bundle) are separate and neither implies the
  other. "A broken monitor alerts instead of going quiet" remains an
  operator-assembled property, not a default. See ALERTING.md §4 and
  MILESTONES.md.
- Scale target: 200 nodes / 40k pairs. Canvas heatmap; WS deltas coalesced
  (≤1 update/pair/5s); zone roll-up default above 60 nodes; Live feed
  virtualized; event ingestion backpressure documented.
- slog structured logging consistent with existing binaries.

### 12.1 Outbound webhooks — signing, at-rest encryption, and the SSRF posture

**Signing.** Every delivery carries `X-Kconmon-Signature: sha256=<hex>`, an
HMAC-SHA256 over the **raw request body bytes** — the exact bytes on the wire,
not a re-serialization of them, so a receiver that verifies against what it
read cannot disagree with what was signed. The payload is deliberately
**closed** (no `omitempty` anywhere: `toAt` is always a key, `notes` and
`pinned` are always absent), which makes the body **byte-identical across
retries** and therefore usable as a receiver-side dedupe key.

**At rest.** Each endpoint's signing secret is stored as
`nonce ‖ AES-256-GCM(secret)` in `webhooks.secret_enc` (BYTEA), with a fresh
12-byte random nonce per seal. The key is `webhooks.encryptionKey(File)` — one
deployment secret, mounted as a file, never inline in a ConfigMap. The store
layer is typed to accept **ciphertext only** and never sees a plaintext secret;
`httpapi` never returns one (there is no field for it on the response type, so
there is no masked form to un-mask); no log line, metric label or audit row
carries either the secret or its ciphertext, pinned by leak-ban assertions in
the httpapi and webhooks suites. An unreadable secret still runs the full retry
ladder and reports a failure rather than short-circuiting — one exit path, so a
decryption failure cannot be distinguished from a delivery failure by timing.

**The SSRF posture, stated plainly.** A webhook URL is **admin-supplied**
(`webhooks:manage` is admin-only) and the only validation applied to it is the
scheme: it must start with `http://` or `https://`. There is **no allowlist, no
denylist, and no private-range or link-local check in M6**, and the delivery
client follows redirects with Go's default policy (up to 10 hops). An admin can
therefore point an endpoint at `169.254.169.254` or an in-cluster Service and
the console will POST a signed incident payload to it.

That is an accepted risk under one specific assumption: **`webhooks:manage` is
already the most privileged non-`rbac` permission in the system**, an admin who
holds it can create API tokens and rewrite role bindings, and none of those are
harder to abuse than an outbound POST. What would force an allowlist is that
assumption failing — specifically, if `webhooks:manage` is ever delegated below
`admin` (a custom role, a scoped operator), or if webhook creation is ever
exposed to a non-admin path such as an alerting integration that mints endpoints
on a user's behalf. Either change makes the URL attacker-controlled by someone
who cannot already do worse, and the allowlist has to land in the same
milestone.

Two further bounds already limit the blast radius: one attempt is capped at
**10 s**, and a delivery is at most **3 attempts** (0s / 30s / 5m, ±20%
jitter), so a hostile URL cannot be used as an unbounded outbound channel. The
error text is never echoed from the response body, so an endpoint cannot be used
to read the contents of what it points at — only whether it answered.

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
