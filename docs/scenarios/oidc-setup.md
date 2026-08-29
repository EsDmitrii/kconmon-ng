# OIDC setup

## Goal

Sign the Console into your identity provider: authorization-code flow with
PKCE, group-based roles, and an audit log keyed on a stable identity. Works
with any OIDC provider that serves standard discovery — Keycloak, Dex,
Authentik, Entra ID, Okta. Every hostname below is a placeholder; substitute
your own.

Mode `oidc` requires a database (`database.existingSecret`), and
`redis.existingSecret` as well once `console.replicas > 1` — the chart
refuses to render otherwise, with a message naming the fix.

## Provider configuration

Register a **confidential client** (the console keeps a client secret) with:

- **Redirect URI**: `https://console.example.com/api/v1/auth/oidc/callback` —
  the path is fixed; the console refuses a `redirectURL` that does not end
  with `/api/v1/auth/oidc/callback`.
- **Scopes**: `openid profile email groups` (the default request). If your
  provider does not expose a `groups` scope or claim, see the claim mapping
  below.
- **Flow**: authorization code. PKCE is used automatically.

The console discovers endpoints from
`<issuer>/.well-known/openid-configuration`, so the issuer must be an
absolute `https` URL **without a trailing slash** — startup fails otherwise,
naming the rule.

On **Keycloak**, that shape is: issuer
`https://sso.example.com/realms/example` (no trailing slash), a confidential
client with the redirect URI above, and a *Group Membership* mapper on the
client so the `groups` claim lands in the ID token — Keycloak does not send
it by default. Full group path is optional; whatever string the mapper emits
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
`console.auth.oidc.secret.create: true` and a `${vault:...}` placeholder —
never a literal in values. With `networkPolicy` narrowed, remember the
console must reach the IdP: `console.networkPolicy.oidcEgress` defaults to
TCP 443 anywhere, and naming your IdP there is the tightening.

## Role bindings

Identity is `oidc:<sub>` — the one claim OIDC Core permits as an identifier.
`usernameClaim` picks only the *display* name in the header menu; changing it
renames a person and moves nothing else. Roles resolve as the **union** of
two sources:

- **`console.auth.groupRoles`** maps groups the IdP asserts onto console
  roles (built-ins `viewer` / `operator` / `alert-editor` / `admin`, or a
  custom role's name). A group absent from the map grants nothing — it is an
  allow-list, and it is what makes a fresh install usable before anyone can
  create bindings through the API.
- **API bindings** (`/api/v1/rbac/bindings`, needs `rbac:manage`) for
  per-person grants, bound to `oidc:<sub>`.

Group membership is re-read on every token refresh, so removing someone from
a group at the IdP takes effect within the access token's lifetime, not at
their next login. `defaultRole` (empty by default) is the role for an
authenticated subject nothing matched — leave it empty to make "no group, no
binding" mean `403`.

## Verify login

1. Open the console in a browser — you land on the IdP's login page and come
   back through the callback.
2. Check what the server thinks you are:

    ```bash
    curl -s -b "$SESSION_COOKIE" https://console.example.com/api/v1/auth/me
    ```

    The response carries your subject, display name, groups and resolved
    roles — if the roles are empty, compare the `groups` array against your
    `groupRoles` keys byte for byte (Keycloak's full group paths start with
    `/`).

3. Confirm the audit log records your writes as `oidc:<sub>`
   (`GET /api/v1/audit`) — display names never appear there; the log is keyed
   on the identity.

Sessions are bounded twice: `console.auth.session.ttl` (12h, absolute,
counted from login) and `idleTimeout` (1h, sliding). A mid-session `401` that
routes to the login page is one of those bounds expiring, not a broken IdP.
