<!--
Status: current
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §7.11 in M0 (2026-07-14); rewritten from
the as-built M7 implementation (2026-08-08):
internal/console/alerting/render.go, internal/console/promrules/promrules.go,
internal/console/webhooks/watcher.go, internal/console/httpapi/alertrules.go,
internal/console/store/alertrules.go,
internal/console/store/migrations/00007_alert_rules.sql,
internal/console/config/config.go, web/src/pages/alerting.tsx,
charts/kconmon-ng/templates/{rbac,console/configmap}.yaml.
This document is the source of truth for Alert Rule Management. Update it (and the ADRs) in the same PR as any deviation.
-->

# Alert Rule Management

### 7.11 Alert rule management (PrometheusRule)

**The Console manages rules. Prometheus evaluates them.** Nothing in the
Console ever decides that an alert fired. An operator builds a rule on
`/alerting`, it becomes a row in `alert_rules`, a reconciler renders every
enabled row into **one** `PrometheusRule` object and server-side-applies it,
and from that moment the rule belongs to Prometheus exactly like a rule
somebody committed to Git.

Landed in **M7**. Off by default: `console.alerting.enabled` (Helm) /
`alerting.enabled` (console config).

---

## 1. Where the authority lives

Every number, window, metric family and label name in the rendered PromQL is a
named constant in `internal/console/alerting/render.go`, and the golden tests
in `render_test.go` / `bundle_test.go` pin the exact output bytes. This
document restates those constants so an operator can read them without reading
Go. **If a constant there and a sentence here disagree, the constant wins and
this document is stale.** The same rule the M6 investigation heuristics
follow.

---

## 2. The builder model

One row per rule in `alert_rules` (migration `00007`, 15 columns):

| Column | Meaning |
| --- | --- |
| `id` | UUID, minted by the store. Becomes the `kconmon_ng_rule_id` label on the rendered alert. |
| `name` | Operator handle **and** the seed of the rendered `alertname`. 1–63 chars; charset enforced in Go, uniqueness enforced **case-insensitively** by a `UNIQUE INDEX on lower(name)`. |
| `kind` | One of the closed template set (below). `CHECK`-constrained. |
| `params` | Per-kind builder payload, JSONB. Validated **closed** by the renderer — an unknown key is an error, never a default. The database only guarantees it is an object. |
| `severity` | `info \| warning \| critical`. `CHECK`-constrained; becomes the reserved `severity` label. |
| `for_ns` | The `for` window, in **nanoseconds** (the repo-wide duration convention). `0` omits `for` from the rendered rule entirely. |
| `labels` / `annotations` | Operator-supplied maps, JSONB. |
| `enabled` | Disabled rules stay in the table and are **excluded from the bundle** — the reconciler stops asserting them, it does not delete them. |
| `rendered_expr` | The derived half, written by the same UPDATE as the builder half. It is what drift diffs against without re-running the renderer, and a rule whose template changed under it shows up as stored bytes that no longer match. |
| `sync_status` / `sync_message` / `last_synced_at` | The reconciler's write-back. Updated by **two narrow UPDATEs** that never touch the builder columns: a sync result must not disturb what the operator typed, and an edit must not claim a sync that has not happened. |
| `created_at` / `updated_at` | A sync-status write does **not** bump `updated_at` — a 60-second reconcile loop would otherwise make every rule look freshly edited. |

`alert_rules` is **configuration, not observation** — the same class as
`webhooks`. It is what an operator typed, it does not grow with time, and it is
therefore **never swept by retention**. `TestAlertRulesAreNotARetentionTable`
stops a later reader from "completing" the sweep list with it.

### 2.1 Template kinds

Seven kinds shipped. `KnownKinds()` is the closed set; `kindSchemas` in
`render.go` is the parameter contract.

| Kind | Params | Renders |
| --- | --- | --- |
| `pair-loss` | `protocol` (`tcp\|udp\|icmp`), `thresholdPercent`, optional `scope.{sourceNode,destNode}` | Loss ratio × 100 (or, for TCP, the fail-rate over `_tcp_results_total`) over the peer label set |
| `zone-latency` | `protocol`, `quantile` (`0.5\|0.95\|0.99`), `thresholdMs`, optional `sourceZone`/`destZone` | `histogram_quantile` over the RTT histogram, × 1000 |
| `dns-failures` | `thresholdPercent` | Fail-rate over `_dns_results_total`, by `host,resolver,source_node,source_zone` |
| `http-ttfb` | `thresholdMs`, optional `url` | `histogram_quantile(0.95, …_http_ttfb_seconds)` × 1000 |
| `agent-missing` | *(none)* | `_controller_registered_agents < _controller_expected_agents` |
| `external-target-down` | optional `targetName` | Fail **rate** over `_external_results_total` |
| `raw` | `expr` (required, non-empty) | The operator's own PromQL, verbatim |

**Unit conversions happen at render time so stored params stay in operator
units.** Loss thresholds are percent and the loss metrics are ratios (× 100);
latency thresholds are milliseconds and the histograms are seconds (× 1000).

`RateWindow` is **`5m`** for every `rate()`/`histogram_quantile` template and
is deliberately *not* a builder param: a per-rule window is a second knob that
changes what the threshold means, and the builder has one threshold.
`TTFBQuantile` is fixed at **0.95** — `zone-latency` takes a quantile because a
latency SLO is quantile-shaped, TTFB alerting is not.

`agent-missing` takes **no** params: `forMinutes` lives in `for_ns`, which the
builder owns, and accepting it here would create two places that mean "how long
before this fires".

### 2.2 Three kinds are not what the plan asked for, on evidence

- **`cert-expiry` was DROPPED.** There is no certificate-expiry metric family
  anywhere in this codebase — not in `internal/metrics/prometheus.go` (the
  whole agent/controller surface), not in `internal/console/metrics`, not in
  `docs/metrics.md`. Rendering over an invented series would ship an alert that
  can never fire. Pinned by `TestEveryKnownKindHasAGolden` and by `ValidKind`.
  The `CHECK` constraint in migration `00007` still *lists* `cert-expiry`, so a
  legacy row would store and list fine and would open the builder with a named
  honest note — the builder just will not create one.
- **`agent-missing` is a controller-side count comparison**, not a per-node
  absence expression. There is no per-node agent up/heartbeat family: agent
  liveness is only visible through the peer probe families, which are keyed by
  `source_node`/`destination_node` and go silent for a node that never
  registered — `absent()` cannot enumerate what *should* exist. The controller
  derives `expected_agents` from its node informer, so it is the only series in
  the system that knows the denominator. Same expression as the shipped
  `KconmonAgentsMissing` default rule.
- **`external-target-down` uses a fail RATE, not `success == 0`.** A denied
  probe never reaches `_external_results_total` at all, and a dead target has
  no success series to compare against zero.

### 2.3 The metric prefix

`MetricPrefix` (`kconmon_ng`) is the **default** of `config.metricsPrefix` and
only the default. Rendering hangs off a `Renderer` **value** built from the
configured prefix (`NewRenderer(prefix)`); there is no package-level `Render`,
because a free function would have to pick a prefix for the caller and picking
the default silently is exactly the bug. The API builds the renderer per call
from `cfg.MetricsPrefix`. An install with a custom `config.metricsPrefix` gets
rules over its own families — unlike the Grafana dashboards in `dashboards/`,
which still hardcode the default (`docs/metrics.md`).

### 2.4 Reserved labels

`severity` and `kconmon_ng_rule_id` are stamped on every rendered rule entry and
are therefore **reserved**: a user label of either name is a validation error,
never a silent override.

---

## 3. Validation without a Prometheus parser

**Deviation from the plan, and the most load-bearing one in M7:** the original
design called for validating expressions with the Prometheus rule parser as a
library. That dependency was **not taken**. `prometheus/prometheus` is a very
large dependency to acquire for one function, and it would have to be kept in
step with the server actually evaluating the rules.

What validates instead, and it is two things, not one:

1. **Render goldens.** Every kind has a byte-exact golden expression
   (`render_test.go`), plus a byte-golden bundle (`bundle_test.go`) produced
   through `encoding/json` with `SetEscapeHTML(false)`. The renderer is pure
   and total: the same input renders the same bytes forever. An expression the
   templates can produce is an expression the templates were tested producing.
2. **Live preview.** `POST /api/v1/alert-rules/preview` renders the rule and
   then **runs the expression as an instant query against the real
   Prometheus**. "Is this valid PromQL" is answered by the thing that will
   evaluate it, which is a stronger answer than a parser gives.

The two halves fail **independently**, and that is the whole design:

- render fails → **422**, and no preview body at all;
- render succeeds, query fails or does not run → **200** with
  `{expr, series: 0, error}` — a partial, honest answer.

The UI never prints "0 series" for an expression that was not evaluated; that
is asserted negatively in the frontend tests.

For `raw` rules this is the only validation there is, and it is enough: an
expression Prometheus refuses to run is an expression the preview reports as an
error before the rule is ever saved.

---

## 4. The bundle: one object, one apply target

Every enabled rule renders into **one** `PrometheusRule` object
(`monitoring.coreos.com/v1`), in **one** group (`kconmon-ng-console`), named
`console.alerting.bundleName` (default `kconmon-ng-console-rules`).

One object and not one per rule, deliberately: a single apply target means
drift is one comparison and a partial apply is impossible. The consequences are
worth stating:

- **Zero enabled rules applies an EMPTY bundle**, not nothing. Yesterday's rules
  must stop evaluating when the last one is disabled.
- **A bundle-level name collision marks every survivor**, because they share the
  object. A single bad row is different: attribution is resolved with a
  per-rule `RenderBundle` probe, so the bad row gets the error and is dropped
  and the rest still syncs.
- **Changing `bundleName` on a live install ORPHANS the previous object.** The
  reconciler owns what it is pointed at and deletes nothing. This is why the
  default is spelled out in `values.yaml` rather than derived from the release
  name.

Ownership is a label plus an annotation:

| Marker | Value |
| --- | --- |
| `app.kubernetes.io/managed-by` (label, on the object) | `kconmon-ng-console` |
| `kconmon-ng.io/rule-ids` (annotation, on the object) | comma-joined sorted UUIDs of the rules it was rendered from — the sync layer diffs on it without parsing the spec |
| `kconmon_ng_rule_id` (label, on each rule entry) | the `alert_rules` row id |

`for` is **omitted** when `for_ns == 0` rather than rendered as `for: 0s`, so
the drift comparison stays silent about a field the operator never set.

---

## 5. Sync: a convergence loop, not an event pipeline

`internal/console/promrules` renders the table and server-side-applies it. One
`Apply` call with `Force: true`, no read-modify-write, no create-then-update
fallback — SSA creates an absent object and merges a present one, and a
fallback would only be a second code path a real apiserver never takes.

**Cadence:** `console.alerting.syncInterval` (default **60s**), jittered ±20% so
N replicas do not apply in lockstep, plus a `Kick()` on every create, update,
delete and successful import. The interval only ever covers drift somebody
introduced out of band; reacting is what the kick is for. The kick channel has
capacity 1 and coalesces.

**Every replica runs the loop. There is no leader election and no advisory
lock**, and that is not an oversight — it is the opposite of the scheduler's
choice, for a stated reason. The scheduler *fires side effects*: two replicas
ticking together run one check twice and nothing can undo the second run. This
loop *asserts state*: every replica renders the same bytes from the same rows
and applies them with the same field manager, so N replicas racing produce
exactly the object one replica would have. A lock here would make a wedged
holder a silent sync outage, to save a few redundant PATCHes a minute.

### 5.1 Drift: RECORD, THEN FIX

A reconcile **always** re-asserts our bytes. Drift is what was observed in the
live object immediately *before* re-asserting, recorded so an operator learns
somebody hand-edited the CRD — not so the console can leave the edit in place.
**There is no mode in which this package sees drift and declines to fix it.**

That has one consequence that looks like a bug in the status column and is not:

> A rule reporting `sync_status=drift` also carries a **fresh**
> `last_synced_at`. Both are true. The drift was observed, the apply then
> happened, and the very next reconcile reports the rule as `synced`. `drift`
> means "the cluster had diverged as of `last_synced_at`, and we corrected it",
> never "the cluster is diverged right now and we left it".

The comparison is a pure two-argument `Compare(desired, live)` over **only the
rendered fields**. The diff shown to an operator is a line diff with prefix/
suffix elision — not an LCS diff; it names the changed region, it is not a
patch.

### 5.2 Failure is a status, never a crash

The `PrometheusRule` CRD may not exist (the Prometheus Operator is **not** a
dependency of this chart), the Role may not have been applied, the apiserver may
be down. Each of those is written back onto every enabled rule as
`sync_status=error` with a message whose **first token is a closed cause class**
— `crd-missing`, `forbidden`, `other` — so an operator and a future UI can
branch on the cause without parsing a Kubernetes error string. The loop keeps
its cadence. The builder, the list and the API all keep working: the rules live
in PostgreSQL, so a degraded sync costs the alerting and nothing else.

**Without a database the reconciler is SKIPPED, not degraded.** `cmd/console`
refuses to start it, because applying an empty bundle over rules that are
already evaluating is destructive in a way a warning cannot fix.

---

## 6. Foreign rules and adoption

`GET /api/v1/alert-rules/foreign` lists `PrometheusRule` objects in the
namespace that this console did **not** write. The filter is applied
**client-side on the label VALUE**, not as a `!label` selector: a negated
selector would drop objects written by *other* tools that happen to carry the
same label key, and those are still foreign.

`POST /api/v1/alert-rules/import` **adopts** one foreign object by name. The
semantics matter more than the endpoint:

> **Adoption COPIES. It never mutates the foreign object.** The object is read
> and its alerting entries become new builder rows. The original stays exactly
> as it was — pinned by a `reflect.DeepEqual` assertion.
>
> **Consequence, stated plainly: until you remove one of them, the same alerts
> exist twice** — once in the object you adopted from, once in the
> console-owned bundle. Both evaluate. The API says so, the UI says so, and
> this is the sentence to read before pressing Import.

There is no self-re-adopt path: an object this console already owns answers
**404** from import, like any other name that is not in the foreign list.

The report *is* the result. `0 created` with a dozen skips is a **200**, because
"every rule in that object is a recording rule" is the useful answer and no
status code can carry it. Three lists come back, always non-null:

**Skipped** — not in your console, and why:

| Reason | Note |
| --- | --- |
| recording rule | The builder has no recording model, only alerting rules. |
| no `alert` and no `record` field | Not a rule entry. |
| no `expr`, or `expr` is not a string | |
| name already taken | Including taken *within the same import*. |
| validation failed | The name is reported **as the object spells it** — never sanitize-renamed, so the operator can find that line. |
| labels are not strings | |
| `for` could not be parsed | **Deviation, accepted:** a skip, not `for_ns = 0`. A misread `5m` silently becoming fire-instantly is a 3am page nobody asked for. |
| render failed | |

**Notes** — imported, but the console had to choose:

| Note | |
| --- | --- |
| `labels.severity` was outside `info\|warning\|critical` | imported as `warning` |
| the rule carried no `severity` label | imported as `warning` |
| the object carried the reserved `kconmon_ng_rule_id` label | dropped |

Every adopted rule is stored as `kind=raw` carrying the object's own `expr`:
adoption preserves the expression, it does not reverse-engineer a template out
of it.

`ParsePromDuration` (in `alerting`) is strict: descending units `y…ms`, each at
most once, whole-string, `int64`-overflow-checked, with `"0"` as the one bare
special case. It round-trips both ways and is deliberately **not onto** —
`1h30m` formats back as `90m`, pinned.

---

## 7. Reading alert STATE

`GET /api/v1/alerts` projects Prometheus' own `/api/v1/alerts` onto this API's
vocabulary. `?managedOnly=true` filters to alerts carrying
`kconmon_ng_rule_id`; an unparseable value is a **400**, never a silent "no
filter" (guessing in that direction returns *more* than was asked for).

`name` and `severity` are lifted off the label set because every consumer needs
them, and `labels` still carries them verbatim — quietly deleting keys would
make this a different alert than the one Prometheus is firing. `value` stays a
**string** (`"7e+00"`): the upstream field is a string precisely because it
carries `NaN` and `±Inf`.

### 7.1 The 200-vs-503 divergence, and why

| Prometheus | `/api/v1/alerts` | `/api/v1/matrix` |
| --- | --- | --- |
| not configured | **200** `{alerts: [], promConfigured: false}` | **503** |
| configured, failing | **502** | 502 |
| configured, answering | 200 | 200 |

This is deliberate and it is not an inconsistency to be tidied up. The matrix
**is** the Prometheus data — without it there is no answer at all. The firing
set is a **list that is legitimately empty**, and its emptiness has two causes
the Overview card must be able to say apart: "nothing is firing" and "nobody is
watching". `promConfigured` is in the **body**, not implied by a status code,
so the card can render both without treating one as an error.

**`GET /alert-rules/{id}/state` was NOT built.** The M7 plan sketched it;
`/alerts?managedOnly=true` covers it set-wise, and Decision 6 was amended
rather than shipping a second read of the same upstream call.

---

## 8. Alert webhooks

Two new events join the closed webhook vocabulary: **`alert.fired`** and
**`alert.resolved`**. Widening needed no migration — M6 closed the vocabulary
in code precisely so this could be one const-list edit.

They are their **own payload family (v2)**, not the incident shape. The M6
incident payload bytes are **frozen** and their pins are untouched. One type,
`AlertPayload{event, sentAt, alert}`, serves both the internal seam and the
wire — no drift twin. `resolvedAt` is **present and null** on `alert.fired`
(pinned at the raw-byte level; null and missing decode identically into
`*time.Time`, so the wire shape is the thing being pinned). `labels` and
`annotations` are normalized to `{}`, never null.

The delivery path does **not** fork: signing, the 0s/30s/5m retry ladder,
jitter, the worker pool and outcome recording are one implementation shared
with incident deliveries. `POST /webhooks/{id}/test` stays incident-shaped — it
answers "can I reach you", which is not an alerts question.

### 8.1 The watcher

`internal/console/webhooks.AlertWatcher` polls Prometheus' alert set every
`console.webhooks.alertPollInterval` (**30s** default) and diffs consecutive
observations. It lives beside the dispatcher because it exists *only* to fire
deliveries: `alerting` stays clockless and I/O-free, `promrules` stays
webhook-free.

An alert instance is identified by a **fingerprint** = rule id +
`sha256` over the **length-prefixed** sorted label pairs. Length prefixing is
not decoration: without it `{"a": "b=c"}` and `{"a=b": "c"}` hash the same, and
those are different alerts. Pinned.

Four postures, each a deliberate trade:

- **Baseline on boot.** The first *successful* poll records the firing set and
  dispatches nothing. Whatever was already firing was already somebody's
  problem, and a console restart that paged the fleet about it would make every
  rolling update a false alarm. The cost: a genuine transition inside the
  restart window is missed. That is the right way round. Note "first
  **successful** poll" — a console that starts while Prometheus is down still
  baselines when it comes back, rather than paging.
- **Freeze on failure.** A poll that fails — *or answers a body the console
  cannot decode* — changes nothing. The last known firing set is kept and no
  resolution is dispatched. **A dead Prometheus must never "resolve" the
  fleet**; that is the most dangerous lie this component could tell. One poll is
  bounded at 15s, shorter than its own cadence, so a slow Prometheus loses a
  cycle instead of queueing behind itself.
- **Managed only.** An alert with no `kconmon_ng_rule_id` label was written by
  somebody else and is somebody else's to route. `GET /api/v1/alerts` still
  serves it; it is not webhook material.
- **Per replica, no leader election.** N replicas deliver N copies of every
  edge. Deliberate: a lock trades duplicate deliveries for **missed** ones
  whenever the holder is the replica that dies. `(event, ruleId, labels,
  firedAt)` is documented as a deduplication key that is stable across
  replicas — **a receiver that dedupes on it sees one alert.**

**`pending` is excluded.** Only `state: firing` counts. Delivering inside a
rule's `for` window would make every `for` a lie. The same exclusion holds on
the Overview card and the Investigate timeline.

A resolution delivers the **remembered** alert — `firedAt` survives — with
`resolvedAt = now`. Resolution is detected by *absence from a poll*, so:

> **`resolvedAt` is only as precise as the poll interval.** "Resolved at" means
> "resolved somewhere in the interval ending here". Shortening
> `alertPollInterval` sharpens it and costs one more GET per interval per
> replica; lengthening it blunts it.

The poll is **unjittered**, unlike the reconcile loop, so an operator can reason
about that error bar.

Expression enrichment (the rendered PromQL and the console row's own name) comes
from an **optional** rule source: Prometheus' `/api/v1/alerts` carries neither.
An enrichment failure **degrades to `expr: ""`, it never drops the delivery** —
a missing expression is worth less than a missed page, and a guessed one is
worth less than nothing.

---

## 9. Permissions

Two permissions, bringing the build total to **25**.

| Permission | Class | viewer | operator | alert-editor | admin |
| --- | --- | --- | --- | --- | --- |
| `alerts:read` | telemetry — the rule list, the expression, the firing set, preview | ✅ | ✅ | ✅ | ✅ |
| `alerts:manage` | statement — create, edit, delete, sync, import | — | ✅ | ✅ | ✅ |

`POST /alert-rules/preview` is a **read** row despite being a POST (a body is
how you send an unsaved rule). `POST /alert-rules/{id}/sync` is a **write** row
despite writing nothing itself. `POST /alert-rules/import` is `alerts:manage`.
`GET /api/v1/alerts` rides `alerts:read` rather than `promql:query`: what it
serves is this API's DTO of the firing set, not a query surface, and a role that
can see the rules should see whether they are firing.

**`alert-editor` holds `alerts:manage`** — the one deliberate exception to the
"statement-class writes stop at operator and admin" groove, and the exception is
the role's entire charter. The builtin sat as a placeholder from M3 through M6
(M4 explicitly withheld the targets/checks/schedules permissions from it) so
that when alerting landed, an on-call engineer could be delegated rule editing
*without* operator's wider fleet authority. A role named `alert-editor` that
cannot edit an alert rule breaks its promise on first click; renaming the
builtin instead would break every existing `role_bindings` row and
`auth.anonymous.role` reference that names it. Decided by the M7 coordinator
over the uniform-groove alternative and pinned by
`TestM7AlertPermissionsFollowTheIncidentsPosture`, so narrowing it back has to
happen in that test's diff, consciously.

Audit records the rule **name** for `POST`/`PUT`/`import` and nothing more.
`params`, `labels` and `annotations` are banned from the audit payload: they
carry operator-authored free text, and an audit log is not a place to leak it.
`preview`, `sync` and `delete` are deliberately absent from the name allowlist.

---

## 10. Enabling it

```yaml
console:
  enabled: true
  database:
    mode: external          # or cnpg — the rules are rows
  alerting:
    enabled: true
    namespace: ""           # empty = this release's namespace, via POD_NAMESPACE
    syncInterval: 60s
    bundleName: kconmon-ng-console-rules
  webhooks:
    encryptionKeySecret:
      name: my-webhook-key  # only needed for alert.fired/alert.resolved delivery
    alertPollInterval: 30s
```

`console.alerting.enabled` renders, under that one flag: the `alerting:` config
block, the console-only ServiceAccount, a **namespaced** `Role` +
`RoleBinding` over `monitoring.coreos.com/prometheusrules`,
`serviceAccountName` on the console Deployment, the `POD_NAMESPACE` downward-API
env var and the apiserver egress rule. See SECURITY.md §10.3 for the grant and
CONFIG.md for every key.

Two things the chart cannot check for you:

1. **The Prometheus Operator's `PrometheusRule` CRD must exist.** Without it
   every apply reports `crd-missing` per rule.
2. **Your Prometheus must actually select the object.** That is a
   `ruleNamespaceSelector`/`ruleSelector` question on the `Prometheus` CR, in
   your monitoring stack, not something this chart can reach.

`alerting.enabled=false` renders no RBAC and emits no config block, and it does
**not** delete an object a previous install applied. What it does *not* do is
hide the builder: `/alerting` still fully creates, edits, previews and deletes
rules with the feature off, because those are database writes and the operator
is plainly preparing an install. Exactly two routes answer **409** naming
`console.alerting.enabled` — `GET /alert-rules/foreign` and
`POST /alert-rules/{id}/sync` — and only the foreign section of the page
carries that banner.
