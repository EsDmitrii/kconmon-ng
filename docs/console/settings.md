# Settings

<!-- screenshot: console-settings.png pending post-redesign reshoot -->

The console's own administration page: "API tokens, webhook endpoints, configuration export/import, and what this
console is running as."

## What this page shows

Sections appear per permission: API tokens need `tokens:manage`, webhook endpoints `webhooks:manage`, configuration
export/import `settings:write` — all three admin-only in the built-in roles. **Language** and **About** are visible
to everyone. Maintenance windows are *not* here — they moved to [Alerting](alerting.md#maintenance-windows), and the
page says so.

## General

**Language** switches the interface between English and Русский. It applies immediately and is remembered in this
browser; server data — node and target names, metric names, API messages — is never translated.

**About this console** reports what this instance runs as: *Auth mode*, *Your roles*, *Your subject*, *Console
build*, *Commit*, and whether *Controller*, *Prometheus* and *Database* are configured. In anonymous mode it names
the fixed role every request gets; with a database it states the retention window (90 days by
default) or that pruning is disabled.

## Authentication and roles

Auth is configured in the [Helm values](../reference/helm-values.md), not on this page: `console.auth.mode` is
`anonymous` (default) | `local` | `header` | `oidc`, with roles resolved from `groupRoles`, bindings and
`defaultRole` — see [Configuration](../configuration.md) and [OIDC setup](../scenarios/oidc-setup.md). Built-in
roles: `viewer`, `operator`, `alert-editor`, `admin`. Roles and role bindings are not administered from this page;
what *is* administered here is below.

## API tokens

Bearer tokens for calling the [HTTP API](../api.md) without a session. The console stores only a
hash: the secret is shown once at creation ("Copy {name} now — this is the only time it is shown") and a lost one is
replaced, not read. Rows show owner, created, last used and expiry; live tokens are *revoked*, spent ones deleted.

## Webhooks

Outbound endpoints the console signs and POSTs incident and alert events to. Every endpoint requires
a signing secret; delivery is asynchronous with a retry ladder, and each row shows the last real outcome and any
consecutive-failure streak, plus a *Send test* action. Webhook secrets are encrypted at rest — the encryption key
comes from the chart (`console.webhooks.existingSecret` or the chart-managed secret); without it, create and test
answer 503.

## Configuration export / import

**Export configuration** downloads a JSON bundle of everything *declared*: targets, check definitions, schedules,
alert rules, webhook endpoints, maintenance windows — "what was declared, never what was observed" (with
`rbac:manage`, custom roles too; bindings are exported for the record and never imported). Choosing a bundle file
runs an immediate **dry run** that predicts, per collection, exactly what *Apply import* would do. Two honest limits,
straight from the page: each section applies only if you hold that page's own permission, and webhook endpoints are
never *created* by an import — a bundle carries no secrets, so create the endpoint first and the import applies its
url, events and enabled flag.

## Retention

History retention is a config value, not a control: the chart's `database.retentionDays` (default `90`; `0` keeps
everything — the daily pruner then never sweeps). The About section reports the active value.

## Use it when

- You are wiring automation against the API and need a token with a name and an expiry.
- You want incident/alert events pushed into chat or an incident tracker — create a signed webhook endpoint.
- You are promoting configuration between installs, or snapshotting it before a change — export, dry-run, apply.

Verified against `web/src/pages/settings.tsx`, `web/src/lib/i18n/dict/settings.ts`
(API: `/api/v1/tokens`, `/api/v1/webhooks`, `/api/v1/export`, `/api/v1/import`, `/api/v1/config`), and
`charts/kconmon-ng/values.yaml` (`console.auth.*`, `console.webhooks.*`, `database.retentionDays`).
