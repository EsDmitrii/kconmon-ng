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
never a literal in values. With `networkPolicy` narrowed, remember the
console must reach the IdP: `console.networkPolicy.oidcEgress` defaults to
TCP 443 anywhere, and naming your IdP there is the tightening.

## Identity: why `oidc:<sub>` and nothing else

A person's identity is `oidc:<sub>`. `sub` is the only claim OIDC Core §5.7
permits as an identifier; `preferred_username` and `email` are explicitly
forbidden as one, because an IdP may reassign them, which is how Grafana's
CVE-2023-3128 (CVSS 9.4) let a leaver's address inherit their roles.
`console.auth.oidc.usernameClaim` therefore decides only the **display** name
(falling back to `name`, then `email`, then the sub itself): the label in the
header menu, not an identity. The audit log is keyed on the identity and
records `oidc:<sub>`, so a display name never appears there at all; changing
this claim renames a person in the UI and moves nothing else.

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
| `viewer` | every read: topology, matrix, events, PromQL queries, runs, MTR, annotations, incidents, maintenance windows, alert state | any write: viewer must never gain configuration authority |
| `operator` | everything viewer has, plus: create runs; manage targets, check definitions and schedules; write annotations, incidents, maintenance windows; manage alert rules | `webhooks:manage`, `tokens:manage`, `rbac:manage`: credential-posture permissions stay admin-only |
| `alert-editor` | the read set, run creation, and `alerts:manage` (alerting is this role's charter) | the operator's targets/checks/schedules authority |
| `admin` | every permission this build knows | — |

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
expires; when that refresh fails at the IdP, the session is deleted and the
user gets `401`. A session the IdP never gave a refresh token rides out the
outage until its own ttl/idle bounds. Getting locked-out operators back in
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
   (`GET /api/v1/audit`): display names never appear there; the log is keyed
   on the identity.

<!-- screenshot oidc-setup-roles.png: needs a real OIDC login against a live IdP; not stageable on the docs stand without personal credentials -->
