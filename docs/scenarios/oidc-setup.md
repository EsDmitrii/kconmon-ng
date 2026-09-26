# OIDC setup

## Goal

Sign the Console into your identity provider: authorization-code flow with
PKCE, group-based roles, and an audit log keyed on a stable identity. Any
provider that serves standard discovery works; the concrete walkthrough below
uses Keycloak, and the pattern transfers: register a confidential client,
make sure a `groups` claim reaches the ID token, map groups to roles. Every
hostname below is a placeholder; substitute your own.

Mode `oidc` requires a database (`database.existingSecret`), and
`redis.existingSecret` as well once `console.replicas > 1`; both violations
fail the chart render with the fix named in the message.

## Provider configuration

Register a **confidential client** (the console keeps a client secret) with:

- **Redirect URI**: `https://console.example.com/api/v1/auth/oidc/callback`.
  The path is fixed; the console refuses a `redirectURL` that does not end
  with `/api/v1/auth/oidc/callback`.
- **Scopes**: `openid profile email groups` (the default request). If your
  provider does not expose a `groups` scope or claim, see the claim mapping
  below.
- **Flow**: authorization code. PKCE is used automatically.

The console discovers endpoints from
`<issuer>/.well-known/openid-configuration`, so the issuer must be an
absolute `https` URL **without a trailing slash**; startup fails otherwise,
naming the rule.

On **Keycloak**, that shape is: issuer
`https://sso.example.com/realms/example` (no trailing slash), a confidential
client with the redirect URI above, and a *Group Membership* mapper on the
client so the `groups` claim lands in the ID token (Keycloak does not send
it by default). Full group path is optional; whatever string the mapper emits
is what you put in `groupRoles`.

## Chart values

```yaml
console:
  auth:
    mode: oidc
    oidc:
      issuer: https://sso.example.com/realms/example
      clientID: kconmon-console
      redirectURL: https://console.example.com/api/v1/auth/oidc/callback
      existingSecret: kconmon-oidc # key: console-oidc-client-secret
      # scopes, usernameClaim (default preferred_username) and groupsClaim
      # (default groups) only when your provider differs from the defaults
    groupRoles:
      platform-oncall: admin
      network-team: operator
      everyone: viewer
```

The client secret rides a Secret you create (`existingSecret`), or let the
chart render one for a secrets injector with
`console.auth.oidc.secret.create: true` and a `${vault:...}` placeholder,
never a literal in values. Since 2.5.0 the console also derives from it the
key that seals each sign-in's PKCE verifier and return path into the OIDC
`state`, so nothing is stored per sign-in until the IdP answers. Rotating the
secret therefore fails the sign-ins started before it, for at most the five
minutes a sign-in may take; those users sign in again. With `networkPolicy` narrowed, remember the
console must reach the IdP. `console.networkPolicy.oidcEgress` defaults to
TCP 443 off the cluster and to any pod on 443, which covers an IdP outside
the cluster and one served through an in-cluster ingress controller
listening on 443. An IdP pod reached through its own Service, such as
Keycloak on 8080 or 8443, needs a rule on the pod's port: on Calico and
Antrea policy sees the pod port after kube-proxy's DNAT, and Cilium never
matches a pod against an `ipBlock`. A list you set replaces the default, so
keep an `ipBlock` rule on 443 if the IdP's discovery or keys live outside
the cluster. Naming your IdP there is also the tightening; see
[NetworkPolicy on Cilium, Calico and Antrea](../configuration.md#networkpolicy-and-cilium).

Behind an Ingress or a NAT, also set `console.clientAddress.trustedProxyCIDRs`
to the proxy's addresses, the ingress controller's pod CIDR at the widest. Sign-in is
budgeted per client address: the start of a sign-in and the IdP's redirect
back to the callback each get `console.rateLimit.loginPerMinute` x 20 a
minute. Without the list every browser shares the ingress's address and one
budget, so one noisy client makes everyone's sign-in answer 429. The console
logs a one-time warning naming the key the first time the start budget trips
with no trusted proxies set. The audit log's
`remoteAddr` comes from the same address. The list gives the client address
only and never identity; before 2.5.0 the same job fell to
`console.auth.header.trustedProxyCIDRs`, which the console still reads for
it while the new list is empty. More in
[Configuration](../configuration.md#console).

## Identity: why `oidc:<sub>` and nothing else

A person's identity is `oidc:<sub>`. `sub` is the only claim OIDC Core §5.7
permits as an identifier; `preferred_username` and `email` are explicitly
forbidden as one, because an IdP may reassign them, which is how Grafana's
CVE-2023-3128 (CVSS 9.4) let a leaver's address inherit their roles.
`console.auth.oidc.usernameClaim` therefore decides only the **display** name
(falling back to `name`, then `email`, then the sub itself): the label in the
header menu, not an identity. The audit log is keyed on the identity and
records `oidc:<sub>`; the display name is stored beside it as
`subjectDisplay`, a label for whoever reads the row and never the key.
Changing this claim renames a person in the UI and moves nothing else.

Two logins are refused outright: an ID token with no `sub`, and one whose
`sub` sits inside a reserved namespace (`oidc:`, `local:`, `header:`,
`token:`). An issuer minting `sub = "local:<uuid>"` would otherwise be handed
that local user's bindings.

## Roles

Roles resolve as the **union** of two sources:

- **`console.auth.groupRoles`** maps groups the IdP asserts onto console
  roles. A group absent from the map grants nothing. It is an allow-list,
  and it is what makes a fresh install usable before anyone can create
  bindings through the API (binding creation itself needs `rbac:manage`, a
  chicken-and-egg the map breaks).
- **API bindings** (`/api/v1/rbac/bindings`, needs `rbac:manage`) for
  per-person grants, bound to `oidc:<sub>`.

`defaultRole` (empty by default) is the role for an authenticated subject
nothing matched; leave it empty to make "no group, no binding" mean `403`.

What the four built-ins actually grant:

| Role | Grants | Held back |
| --- | --- | --- |
| `viewer` | the telemetry reads: topology, matrix, events, PromQL queries, runs, MTR, annotations, incidents, maintenance windows, alert state | any write, and three reads: `targets:read`, `checks:read`, `audit:read`; viewer must never gain configuration authority |
| `operator` | everything viewer has, plus: create runs; read and manage targets, check definitions and schedules; write annotations, incidents, maintenance windows; manage alert rules | `audit:read`, `settings:write`, `users:manage`, `webhooks:manage`, `tokens:manage`, `rbac:manage`: the credential and settings posture stays admin-only |
| `alert-editor` | viewer's reads, run creation, and `alerts:manage` (alerting is this role's charter) | `targets:read`, `checks:read`, `audit:read` and the operator's targets/checks/schedules authority |
| `admin` | every permission this build knows | nothing |

Group membership is re-read on every token refresh, so removing someone from
a group at the IdP takes effect within the access token's lifetime, not at
their next login. One asymmetry is deliberate: a provider that returns no ID
token on refresh (most do not) leaves the session's groups as they were,
because an empty group list is a silent, total deauthorization, and inventing
one out of a missing optional field would be worse than the staleness.

## Migrating from local or header mode

Bindings created before the `oidc:<sub>` scheme name a bare username
(`alice`) and now resolve to nothing: the correct direction to fail, but an
invisible one. At boot in `oidc` mode the console logs a WARN naming every
user binding that is not `oidc:`-prefixed, with its role, so each can be
remapped against the IdP's own sub values. This is a report rather than an
automatic rewrite on purpose: rewriting `alice` to `oidc:<sub>` means
trusting the username claim to say who `alice` was, and not trusting that
claim is the entire reason the scheme changed. Budget the remap step into the
migration; until it is done, those people have whatever `groupRoles` grants
them and nothing more.

## Sessions, logout, and the IdP going down

A session is bounded twice. `console.auth.session.ttl` (default 12h) is the
absolute lifetime: counted from login, never extended, so a session ends 12h
after sign-in no matter how busy it was. `console.auth.session.idleTimeout`
(default 1h) slides forward as the session is used but never past the
absolute bound, which is the whole reason there are two numbers. A session
idle longer than that is refused with `401` and purged on its next use.
`idleTimeout: 0` disables the idle bound and leaves `ttl` alone in charge.
A mid-session `401` that routes to the login page is one of these bounds
expiring, not a broken IdP.

The session cookie is `__Host-kconmon_session` by default: `HttpOnly`,
`SameSite=Lax`, `Secure` on. Its `Max-Age` is the absolute lifetime, so a
browser may hold a cookie the server has stopped honouring; that is the
ordinary case behind the mid-session `401`. Behind a TLS-terminating proxy
nothing changes: the browser still speaks https. A console genuinely served
over plain HTTP needs `console.auth.session.secure: false` *and* a
`cookieName` without the `__Host-` prefix, because the console refuses a
`__Host-` name with `secure: false` at startup (browsers reject that cookie
anyway).

**Logout** is `POST /api/v1/auth/logout`: it deletes the session server-side
and clears both cookies (session and CSRF), works in every mode, and is
idempotent. There is no RP-initiated logout: the console never calls the
IdP's end-session endpoint, so the IdP session survives and a fresh
`/oidc/start` may sign you straight back in.

**If the IdP goes down**, there is no fallback: `auth.mode` selects exactly
one of `anonymous | local | header | oidc`, so oidc mode has no local
break-glass account. Live sessions degrade in two tiers. A session holding a
refresh token is proactively refreshed ~2 minutes before its access token
expires, detached from the request and bounded at 15 s. Only an IdP that
refuses the refresh (`invalid_grant`, or a 4xx other than 408 and 429) gets
the session deleted and the user a `401`. An IdP that is unreachable, times
out or answers 5xx, 408 or 429 leaves the session in place: it works until
its access token actually expires, is refused from then on, and resumes once
the IdP answers again. A session the IdP never gave a refresh token rides out
the outage until its own ttl/idle bounds. Getting locked-out operators back in
during a long outage means changing `auth.mode` and rolling the console.

## Verify login

1. Open the console in a browser. You land on the IdP's login page and come
   back through the callback.
2. Check what the server thinks you are:

    ```bash
    curl -s -b "$SESSION_COOKIE" https://console.example.com/api/v1/auth/me
    ```

    The response carries your subject, display name, groups and resolved
    roles; if the roles are empty, compare the `groups` array against your
    `groupRoles` keys byte for byte (Keycloak's full group paths start with
    `/`).

3. Confirm the audit log records your writes as `oidc:<sub>`
   (`GET /api/v1/audit`): the log is keyed on the identity, and a display
   name appears only beside it, as `subjectDisplay`. Since 2.5.0 the sign-in
   itself is there too: `GET /api/v1/auth/oidc/callback` with outcome
   `allowed` as the identity that signed in, and an `error` row with no
   subject for each refused callback, up to 120 such rows a minute from one
   client address.

<!-- screenshot oidc-setup-roles.png: needs a real OIDC login against a live IdP; not stageable on the docs stand without personal credentials -->
