# Settings

The console's own administration: local users, API tokens, webhook endpoints, configuration export/import, and what this instance is running as. Sections appear per permission (users need `users:manage`, API tokens `tokens:manage`, webhook endpoints `webhooks:manage`, export/import `settings:write`, all four admin-only in the built-in roles), while **Language** and **About** are visible to everyone. Maintenance windows are not here; they moved to [Alerting](alerting.md#maintenance-windows), and the page says so.

## General

**Language** switches the interface between English and Русский. It applies immediately and is remembered in this browser; server data (node and target names, metric names, API messages) is never translated.

**About this console** reports what this instance runs as: *Auth mode*, *Your roles*, *Your subject*, *Console build*, *Commit*, and whether *Controller*, *Prometheus* and *Database* are configured. In anonymous mode it names the fixed role every request gets; with a database it states the retention window (90 days by default) or that pruning is disabled.

## Authentication and roles

Auth is configured in the [Helm values](../reference/helm-values.md), not on this page; what this page administers starts in the next section. `console.auth.mode` picks one of four modes:

`anonymous` (the default)
:   No authentication. Every request gets the fixed role from `console.auth.anonymous.role` (`viewer` unless changed), and a warning [banner](overview.md#the-console-chrome) spans every page. For evaluation and demo installs; not for production.

`local`
:   Username/password accounts stored in the console database (one is required). On first start, while the users table is empty, the account named by `local.bootstrapAdmin` is created with the password from its Secret. The mode for a small install with no identity provider to lean on.

`header`
:   Identity asserted by a trusted in-cluster reverse proxy via headers (`X-Remote-User`, `X-Remote-Groups`). `console.auth.header.trustedProxyCIDRs` must be set and non-empty, and should name only that authenticating proxy: headers from an untrusted source would be an authentication bypass, so the console refuses the mode without it. The ingress in front of the console, if it is not that proxy, belongs in `console.clientAddress.trustedProxyCIDRs`, which gives the client address and never identity. Pick this mode when something like oauth2-proxy already fronts your services.

`oidc`
:   Full OIDC login. Requires the database, and Redis once `console.replicas` exceeds 1. Identity is always `oidc:<sub>`, the one claim OIDC allows as an identifier, so RBAC bindings and the audit log survive a display-name change; `usernameClaim` only affects what the user menu shows. Setup walkthrough: [OIDC](../scenarios/oidc-setup.md).

Roles resolve as the union of `groupRoles` (a declarative group→role map in values; an allow-list, and what it grants cannot be revoked through the API, which is the point), any bindings made through the API, and `defaultRole` for an authenticated subject nothing matched. Built-in roles: `viewer`, `operator`, `alert-editor`, `admin`. Sessions for every non-anonymous mode last 12 h absolute with a 1 h sliding idle timeout. See [Configuration](../configuration.md) for the full reference.

## Users

In `local` mode the console is its own identity provider, and this section manages its accounts: list them with the roles they resolve to, create one with a role, change a user's role, disable, re-enable or delete them, and set a new password. It needs `users:manage` and exists only in `local` mode; under `header` and `oidc` the provider owns the accounts and the API answers 404.

- **Passwords** are at least 12 characters. A reset by an administrator, or a user changing their own password from the user menu (which needs the current one), signs that user out of every other session; the user who changed their own keeps working. On your own row the section offers *Change password* instead of *Reset password*, because a reset would sign you out of the session you are using. A password hash made before 2.5.0 uses older argon2id parameters; the account's next successful sign-in rewrites it at the current ones, keeping the salt, so no session ends. An account that never signs in keeps the old hash, and until it does a wrong password on it takes a different time than one on an unknown username.
- **Roles.** A user holds one direct role; changing it replaces the old binding in one step, and bindings made for their groups stay. Custom roles are listed next to the built-in ones when you also hold `rbac:manage`; without it one line says that only the built-in roles are offered. If the custom roles cannot be loaded, an error line with *Retry* says so and the built-in roles stay available. Changing your own role asks for confirmation first: a role without `users:manage` takes this section away, and you cannot grant it back yourself.
- **`users:manage` is as strong as `admin`.** Its holder can create an account with any role, `admin` included, or move their own account to `admin`. Add it to a custom role only where you would hand out `admin`.
- **The last administrator** cannot be disabled, deleted or given a role without `users:manage`: the console refuses with 409 rather than leave nobody able to manage users. Grant it to someone else first. In `local` mode the same 409 answers a role or binding change through the API that would do it indirectly: dropping `users:manage` from a custom role, deleting the binding that gives the last such user the permission, or creating a binding for a user who holds `users:manage` only through `defaultRole` (a binding replaces the default role, so that user would lose it).
- **Disabled** users are refused on their next request, and disabling also ends their sessions: after a re-enable the user signs in again, and a session opened before the disable does not come back. The exception is a session opened before 2.5.0. It carries no stamp, so it survives a password change and comes back after a re-enable, until its 12 h absolute or 1 h idle expiry. Disabling also revokes every API token the user owns, and the tokens those tokens minted. A re-enable revokes any token still active first, so tokens must be minted again after it; if the token store cannot be written, the re-enable answers 502 `tokens unavailable` and the user stays disabled.
- **Delete** removes a local account for good, after a second press to confirm. Every API token the user owns, and the tokens those tokens minted, is revoked first; if the token store cannot be written, the delete answers 502 `tokens unavailable` and the user stays. The account and its direct role bindings then go in one step, and the user's sessions end on their next request. The username is free again, and a new account under it gets none of the old one's tokens or sessions. The exception is again a session opened before 2.5.0: it carries no stamp and would pass to a new account of the same name, so in the first hours after the upgrade reuse a deleted name only once such a session has expired (12 h absolute by default).
- **The sign-in form** asks for both fields: an empty username or password is refused in the browser, before any request, so it spends no attempt. Whitespace counts as typed and is sent, as the server judges it.
- **Sign-in lock-out.** Sign-in is rate-limited per username: `console.rateLimit.loginPerMinute` attempts a minute (5 by default), right or wrong, then 429 until the minute is over. *Change password* spends the same budget. Each source address also gets 20 times that across all usernames. The flip side is that anyone who can reach the login page and knows a username can keep that account locked out by sending five wrong passwords a minute, and while that goes on the right password is refused too. Other usernames are not affected, so another user with `users:manage` can still sign in. To get the account back, stop the source at your ingress or firewall (the per-address limit trips only at 100 attempts a minute, and behind an ingress it counts real clients only when `console.clientAddress.trustedProxyCIDRs` names the ingress; sign-in spends no budget the whole ingress shares, so no client behind it can lock the others out), or sign in through `oidc` or `header` mode, which do not use this form. Once the attempts stop, the next minute lets the user in.

## API tokens

Bearer tokens for calling the [HTTP API](../api.md) without a session. A token holds exactly `console.auth.defaultRole`, whoever minted it: role bindings name users and groups, never tokens. With the default (empty) a token gets 403 on every route that needs a permission, so set `defaultRole` before handing tokens to automation, and remember that it also goes to every signed-in user without a binding. The console stores only a hash of the secret: the value is shown once, at creation ("Copy {name} now — this is the only time it is shown"), and a lost token cannot be read back, only revoked and replaced. Rows show owner, created, last used and expiry; live tokens are *revoked*, spent ones deleted.

<figure markdown>
![Settings: the Language switch, then the token-created panel telling you to copy acc-demo-ci now because this is the only time it is shown, with the token value painted over, Copy token and I have saved it, above the API tokens table listing three tokens owned by the admin user's ID: acc-demo-ci active and an older acc-demo-ci revoked, both never used with no expiry, and acc-ci-nightly active, never used, expiring 10/26/2026, with Revoke or Delete actions](../img/console-settings-tokens.png){ loading=lazy }
<figcaption>A token just created: the one moment its secret is visible (the value is painted over in this frame), above the table that will only ever show its metadata: owner, created, last used, expiry, state.</figcaption>
</figure>

## Webhooks

Outbound endpoints the console signs and POSTs incident and alert events to. Every endpoint requires a signing secret; each row shows the last delivery outcome, any consecutive-failure streak, and a *Send test* action. Secrets are encrypted at rest with a key the chart supplies (`console.webhooks.existingSecret`); without that key, create and test answer 503, naming `webhooks.encryptionKeyFile` in the console config and `console.webhooks.existingSecret` in Helm (base64 of 32 random bytes).

### The delivery contract

What a receiver's author needs, in one place. The scenario walkthrough is [Set up alerting](../scenarios/set-up-alerting.md).

**Events.** The subscribable set is closed, in two families: `incident.created` / `incident.resolved` / `incident.reopened` are lifecycle changes the console was told about by the request that caused them; `alert.fired` / `alert.resolved` are Prometheus transitions the console *detected* by polling. Each family has its own stable payload shape, every key present on every delivery. One endpoint may subscribe to both; read `event` first, then pick the parser.

**Signature.** Every delivery is a `POST` with `Content-Type: application/json` and `X-Kconmon-Signature: sha256=<hex>`, an HMAC-SHA256 with the endpoint's secret over the raw body bytes, the exact bytes on the wire. Verify the signature before parsing.

**Redirects are not followed.** A 3xx from a receiver is recorded as `failed: HTTP 3xx` and retried along the ladder like any other non-2xx answer, *Send test* included, so configure each endpoint with its final URL. Following a redirect would let a receiver make the console re-send the signed body to any host.

**Retries.** Up to three attempts, delayed 0 / ~30 s / ~5 m with ±20% jitter, 10 s timeout each, until the endpoint answers 2xx. The body (and therefore the signature) is identical across retries, so a receiver must be idempotent. *Send test* is deliberately a single attempt: an operator clicking it is asking a question and waiting for the answer on the endpoint row. Delivery lives in the console process: a restart during the retry window loses the remaining attempts, and the ledger for a miss is the row's last status and failure streak, not a replay queue.

**Replicas and deduplication.** Every console replica polls alert state independently, so N replicas deliver N copies of each alert edge. Deduplicate on `(event, alert.ruleId, alert.labels, alert.firedAt)`; all four are stable across replicas and across the retry ladder. `sentAt` is not; it is per delivery. For the incident family, `(event, incident.id, at)` serves the same purpose. A held edge delivered when its maintenance window closes carries the original `firedAt`, so it dedupes across replicas like any other.

**Timing, and what a pager should expect.** `alert.resolved` is stamped when the console *noticed* the alert was gone, at the granularity of `console.webhooks.alertPollInterval` (30 s by default): an absence has no timestamp of its own, so read it as "resolved at some point in the poll interval ending here". `firedAt`, by contrast, is Prometheus's own `activeAt` and identical on every replica, which is what makes it usable in the dedupe key. Two more deliberate properties: the first successful poll after a console restart is a baseline that delivers nothing (a restart never pages about alerts already firing), and a failed poll freezes the firing set, so nothing resolves while Prometheus is unreachable. Only alerts this console manages (those carrying `kconmon_ng_rule_id`) are delivered, and `pending` alerts are not: pending is not fired.

## Configuration export / import

**Export configuration** downloads a JSON bundle of everything *declared* (targets, check definitions, schedules, alert rules, webhook endpoints, maintenance windows), never anything observed. With `rbac:manage`, custom roles too; bindings are exported for the record and never imported. Since 2.5.0 each section also needs the permission of its own page: `targets:read` for targets, `checks:read` for definitions and schedules, `alerts:read` for alert rules, `webhooks:manage` for webhook endpoints (a URL can itself carry a credential), `maintenance:read` for maintenance windows and `rbac:manage` for roles and bindings. A section you cannot read is left out of the file and named in its top-level `omitted` list; an admin's export is complete. After such an export the section names the withheld sections under the button: "Exported without these sections: {sections}. This account cannot read them, and importing the file leaves them as they are." Such a file still imports: the import ignores `omitted` and imports nothing for a missing section.

Choosing a bundle file runs an immediate **dry run** that predicts, per collection, exactly what *Apply import* would do; pick another file while it runs and only the newer file's plan is shown. The dry run checks the last-admin guard against the roles the bundle changes, role by role, so the preview reports the same refusals the real import would. After *Apply import* the Webhooks list and the custom roles in the Users section read again, and so do the cached target and maintenance-window lists that Alerting, Incidents and the target card hold; the other pages' lists refresh when you next open them. Two limits to know going in: each section applies only if you hold that page's own write permission (otherwise it is skipped with one warning naming the permission, "skipped: importing this section requires {permission}, which this caller does not hold; ..."), and webhook endpoints are never *created* by an import, since a bundle carries no secrets; create the endpoint first and the import applies its url, events and enabled flag on top.

The import holds bundle items to the same rules as the API. A check definition no agent could run against its target (for example an http check on a `host:port` target) is a per-item error, the refusal `POST /api/v1/checks` gives, and the dry run judges it against the targets the bundle would write. A target update that would leave a definition on that target unrunnable is refused too, unless the same bundle rewrites that definition and the rewrite itself imports. As on the PUT routes, a new definition or schedule is always judged for runnability, and an update only when it leaves the row enabled, so an import can pause a row that could not run. So is a schedule whose kind cannot run its definition's check (the table on [Scheduled checks](scheduled-checks.md#schedules)), and a definition update that would leave one of its schedules unable to run it, as `PUT /api/v1/checks/{id}` refuses it. The dry run also reports an alert rule whose Prometheus alert name another rule already has, which the real import refuses. Field bounds apply too: a plane other than `pod`, params over 4096 bytes or an address over 2048 bytes is a per-item error. The import reads the targets and definitions this console already holds even for a section the bundle lacks, so when that read fails the whole import answers 502 and writes nothing further. While *Apply import* runs, the file picker is disabled. An Apply that fails on a 5xx or a network error may have written part of the bundle, so the lists it touches are read again.

## Retention

History retention is a config value, not a control here: `database.retentionDays` (default `90`; `0` keeps everything, and the daily pruner then never sweeps). The About section reports the active value.

<!-- verified against: web/src/pages/settings.tsx, web/src/lib/i18n/dict/settings.ts, internal/console/authz/roles.go
     (admin-only manage permissions), charts/kconmon-ng/values.yaml console.auth block (mode semantics, groupRoles
     allow-list comment, trustedProxyCIDRs "else headers are an auth bypass", oidc sub comment, session ttl/idle,
     anonymous.role viewer), web/src/lib/api-types.ts L1604-1609 (WebhookEvent two-families comment), L1828-1860
     (WebhookPayload/WebhookAlertPayload transport, ladder, baseline, freeze, N-replica dedupe tuple), L1893-1901
     (firedAt=activeAt, resolvedAt=noticed), internal/console/webhooks/dispatcher.go (retryLadder {0,30s,5m},
     jitterFraction 0.2, attemptTimeout 10s, singleAttempt for /test, sign over raw bytes L619-623),
     docs/scenarios/set-up-alerting.md L98 (linked), charts values (webhooks encryption key -> 503,
     database.retentionDays), internal/console/webhooks/dispatcher.go CheckRedirect (3xx recorded, never followed),
     internal/console/httpapi/users.go + rbac.go (requireKnownRole, last-admin guard, handleAuthPassword budget,
     handleUsersDelete),
     web/src/pages/login.tsx (empty field refused before login()),
     internal/console/httpapi/rbac.go (rbacLastAdminGuard 409), internal/console/authn/local.go + password.go
     (SessionStamp with session_epoch, empty stamp = pre-2.5.0 session never checked), internal/console/httpapi/auth.go
     + ratelimit.go (per-username loginPerMinute counted before verification, x20 per address, clientIP via
     trustedProxyCIDRs; sign-in charges no trusted-proxy budget), web/src/lib/i18n/dict/users.ts
     (row.changeOwn, self.confirmRole), internal/console/config/config.go (auth.defaultRole built-in only),
     internal/console/httpapi/export.go exportSectionGates + Omitted.
     APIs: /api/v1/tokens, /api/v1/webhooks, /api/v1/export, /api/v1/import, /api/v1/config. -->
