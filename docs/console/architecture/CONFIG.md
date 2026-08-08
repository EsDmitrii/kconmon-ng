<!--
Status: current
Owner: @EsDmitrii
Source: written from the as-built M0–M3 implementation (2026-08-06):
internal/console/config/config.go, cmd/console/main.go, and the chart's
templates/console/configmap.yaml + templates/console/deployment.yaml +
values.yaml at chart 1.5.0. The mtr.enrichment section and the fifth render
guard added from the as-built M5 implementation (2026-08-08): config.go's
MTRConfig/EnrichmentConfig, internal/console/enrich/enrich.go,
cmd/console/main.go's enricherDep switch, at chart 1.7.0.
This document is the source of truth for Console configuration. Update it (and the ADRs) in the same PR as any deviation.
-->

# Console configuration

The Console binary reads its **own** file, separate from the agent/controller
config documented in `docs/configuration.md`.

| | |
| --- | --- |
| Default path | `/etc/kconmon-ng-console/config.yaml` |
| Override | `KCONMON_NG_CONSOLE_CONFIG` |
| Parsing | strict — `KnownFields(true)`, so an unknown key fails startup |
| Missing file | **not** an error: defaults are loaded and validated |
| Hot reload | **no**. Unlike the agent/controller file this one is read once, at boot |

`KCONMON_NG_CONSOLE_CONFIG` is **still the only environment variable the
console binary reads** — that was true at chart 1.4.0 and M3 deliberately
kept it true. Every new M3 setting (`database.*`, `auth.*`, and all their
secrets) is expressed in `config.yaml` like everything else. Secrets
themselves never ride in that file as inline values: `database.dsnFile`,
`auth.local.bootstrapAdminPasswordFile` and `auth.oidc.clientSecretFile` are
paths, and the chart mounts the Kubernetes Secrets they point at as files —
see "Secret mounts" below.

Rendered by the chart from `console.*` values (`templates/console/configmap.yaml`)
and mounted into the Deployment.

## Full file, with the code's defaults

```yaml
httpPort: 8080
logLevel: info # debug | info | warn | error
logFormat: json # json | text
metricsPrefix: kconmon_ng # Console self-metrics are <prefix>_console_*
auth:
  mode: anonymous # anonymous | local | header | oidc
  anonymous:
    role: viewer # must not be empty
  defaultRole: "" # role for an authenticated subject with no binding; empty = none (403)
  local:
    bootstrapAdmin: "" # username created on first start when the users table is empty
    bootstrapAdminPasswordFile: "" # file path; NEVER an inline password
  header:
    userHeader: X-Remote-User
    groupsHeader: X-Remote-Groups
    groupsDelimiter: ","
    trustedProxyCIDRs: [] # REQUIRED non-empty in header mode
  oidc:
    issuer: ""
    clientID: ""
    clientSecretFile: ""
    redirectURL: ""
    scopes: [openid, profile, email, groups]
    usernameClaim: preferred_username
    groupsClaim: groups
  session:
    ttl: 12h
    cookieName: __Host-kconmon_session
    secure: true # false ONLY for local http:// development
controller:
  url: "" # e.g. http://kconmon-ng-controller:8080; empty = topology API answers 503
  timeout: 10s # per-request, console -> controller HTTP
  grpcAddr: "" # e.g. kconmon-ng-controller:9090; empty = realtime ingestion off
prometheus:
  url: "" # empty = matrix and PromQL APIs answer 503
  queryTimeout: 30s
  maxRange: 24h # query_range end-start cap
  maxResponseBytes: 8388608 # 8 MiB
valkey:
  address: "" # host:port; empty = in-process bus (single replica only)
  dialTimeout: 5s # initial dial only
database:
  dsn: "" # postgres://... ; MUST NOT embed a password
  dsnFile: "" # path to a file holding the full DSN; WINS over dsn when set
  maxConns: 10
  connectTimeout: 10s
  migrateOnStart: true # ADR-001: embedded goose migrations, advisory-locked, run on start
  retentionDays: 90 # 0 disables pruning
rateLimit:
  runsPerMinute: 10 # POST /api/v1/runs, per subject; 0 disables THIS limit
  loginPerMinute: 5 # POST /api/v1/auth/login, per username AND per source IP; 0 disables
scheduler:
  enabled: false # gates the schedule loop, the stuck-run reaper AND the continuous reconciler
  tickInterval: 5s # poll cadence; the advisory lock is taken and released per tick
mtr:
  enrichment:
    enabled: false # master gate; OFF by default (this is the console's only extra egress)
    rdns:
      enabled: false
      timeoutMs: 500 # bounds ONE lookup, not the batch
    geoip:
      asnPath: "" # e.g. /geoip/GeoLite2-ASN.mmdb; empty = ASN/provider lookups off
      cityPath: "" # e.g. /geoip/GeoLite2-City.mmdb; empty = geo lookups off
    ttl: 24h # cache row lifetime in mtr_hop_enrichment
```

`database` is entirely omitted from the rendered config when
`console.database.mode=disabled` (the chart's default) — an absent block,
not an explicit `dsn: ""` — so the empty-DSN meaning below is what
`Config.defaults()` already assumes with no database block present at all.

## Validation, key by key

Startup fails — loudly, before serving — on any of these:

| Rule | Failure |
| ---- | ------- |
| `httpPort` outside 1–65535 | error |
| `logLevel` not one of `debug\|info\|warn\|error` | error |
| `logFormat` not `json\|text` | error |
| `metricsPrefix` empty | error |
| `auth.mode` not one of `anonymous\|local\|header\|oidc` | error |
| `controller.url` / `prometheus.url` not `""` and not an absolute http(s) URL, or with a trailing slash | error |
| `controller.timeout`, `prometheus.queryTimeout`, `prometheus.maxRange`, `prometheus.maxResponseBytes`, `valkey.dialTimeout` non-positive | error |
| `valkey.address` not `""` and not `host:port` | error |
| `database.dsn` and `database.dsnFile` both set | error ("set either ... not both") |
| `database.dsn` set and not a `postgres://`/`postgresql://` URL with a host | error |
| `database.dsn` embeds a password | error ("it would land in a ConfigMap: use database.dsnFile") |
| `database.maxConns` < 1 | error |
| `database.connectTimeout` non-positive | error |
| `database.retentionDays` < 0 | error (0 is legal: "disables pruning") |
| `auth.defaultRole` set and not a known built-in role (`viewer\|operator\|alert-editor\|admin`) | error |
| `auth.session.ttl` non-positive | error |
| `auth.session.cookieName` starts with `__Host-` and `auth.session.secure=false` | error (browsers reject `__Host-` without `Secure`) |
| `rateLimit.runsPerMinute` or `rateLimit.loginPerMinute` < 0 | error (0 is legal: "disables the limit") |
| `scheduler.tickInterval` non-positive **while `scheduler.enabled=true`** | error (a disabled loop's interval is not inspected) |
| `mtr.enrichment.enabled=true` with `rdns.enabled=false` **and** both `geoip` paths empty | error naming all three knobs |
| `mtr.enrichment.ttl` non-positive **while `mtr.enrichment.enabled=true`** | error |
| `mtr.enrichment.rdns.timeoutMs` non-positive **while `rdns.enabled=true`** | error |

### `rateLimit` — what it actually bounds, and where it stops

`runsPerMinute` (default **10**) caps `POST /api/v1/runs` per **subject**. One
run fans out to up to 400 agent pairs, so an unbounded caller is a
controller-load amplifier, not just a noisy one.

`loginPerMinute` (default **5**) caps `POST /api/v1/auth/login` per
**username** and, counted independently, per **source IP**. It is an
availability control as much as an anti-brute-force one: argon2id is
deliberately 64 MiB per verification, and unlimited concurrent logins against
a 256Mi console pod is an unauthenticated OOM.

`0` disables that one limit. A negative value is an error, not a second way to
say "off" — otherwise a typo would silently disable a security control.

Two honest limitations:

- **Per-replica with `console.valkey.mode=disabled`.** The window lives in
  `cache.KV`, so with Valkey the limit is cluster-wide and without it each
  replica counts its own (ADR-002: the in-process KV has no cross-replica
  visibility). N replicas then admit up to N times the configured rate. That
  is weaker than configured, never stronger — but if the number in your values
  file is the number you are relying on, run Valkey.
- **Fail-open on a KV outage.** If the KV cannot be read the request is
  **admitted**, not refused (Decision 8: a Valkey outage must not become a
  login outage). Every such admission increments
  `kconmon_ng_console_rate_limit_failopen_total{limit=...}`, so the gap is
  visible rather than silent. Alert on it if you care.

### `scheduler` — one flag, three consumers

`scheduler.enabled` is **false by default**, deliberately: schedules can be
created and stored without anything acting on them, so upgrading the chart
must not by itself start dispatching fleet traffic from rows an operator
entered while nothing was consuming them. Turning it on is a deliberate act.

The one flag gates all three of:

1. the **schedule loop** (fires due `check_schedules` rows as ordinary runs),
2. the **stuck-run reaper** (`kconmon_ng_console_runs_reaped_total`),
3. the **continuous external-check reconciler** — which pushes target
   assignments to agents.

There is no separate `console.externalChecks.*` block. The reconciler shares
this flag and the scheduler's PostgreSQL advisory lock, so a Console with
`scheduler.enabled=false` has continuous targets stored and nothing probing
them.

Enabling it also needs a resolved database DSN (`check_schedules` and the
advisory lock both live in PostgreSQL) and a controller (a fired schedule
becomes an ordinary diagnostics run). With either missing the console **logs
and skips the loop** rather than failing to start — the rest of the Console is
still useful.

`scheduler_ticks_total{result="not-leader"}` is the normal case on every
replica but one. The lock is taken and released per tick, so a replica dying
mid-tick delays exactly one tick.

### `mtr.enrichment` — the console's only extra egress, and why it is off

Path history itself has **no configuration**. The checks runner projects every
MTR result into `mtr_path_snapshots` whenever a database is resolved; there is
no knob to turn that on, off or down. This block is only about **enriching**
the hop addresses in a stored trace.

`enabled` is `false` by default for a stronger reason than `scheduler.enabled`.
Enrichment is the only part of the Console that makes the pod talk to something
other than the controller, Prometheus, Valkey and PostgreSQL: `rdns` sends
every hop address the fleet ever traced to whatever resolver the pod's
`/etc/resolv.conf` names. That is a deliberate act with an egress footprint,
not a default.

**The two sources gate independently.** An air-gapped cluster with mounted mmdb
files and no reachable resolver runs geoip-only; a cluster with internal DNS
and no MaxMind licence runs rdns-only. Consequences worth stating:

- **Enabled with every source off is a startup error, not a no-op**, and the
  message names all three knobs. It is the one misconfiguration that would
  otherwise be invisible: the resolver would start, every lookup would resolve
  to an empty row, and the cache would fill with authoritative-looking nothing
  that `ttl` then protects for a day.
- **An empty geoip path is that source switched off** — the same "empty means
  disabled" convention `controller.url`, `prometheus.url` and `valkey.address`
  already use.
- **An UNREADABLE mmdb file is not a boot failure.** `enrich.New` warns,
  disables that one source, and the console serves trace history exactly as
  before. A bad mount must never cost an operator their history. A corrupt ASN
  file leaves the City file working, and vice versa.
- **A source that is off, or whose file failed to open, is never counted** in
  `kconmon_ng_console_enrichment_lookups_total`. A series pinned at zero would
  read as "working and finding nothing" (docs/metrics.md).

**With `database.mode=disabled` the whole block is inert.** The cache *is*
`mtr_hop_enrichment`, so a resolver without a database would re-resolve every
address on every single read. `cmd/console` logs

```
WARN mtr.enrichment.enabled is set but no database is configured — hop enrichment is off
     (the TTL cache lives in PostgreSQL; set console.database.mode)
```

and wires the handler's cache-only path instead. That path answers `200` with
an empty `enrichment` map — never a `503` — so a trace still renders. It is a
warn-and-skip rather than a fatal deliberately: taking the whole Console down
over an optional decoration would be the wrong trade. The one enrichment
failure that *is* fatal is a resolver that cannot be **constructed** at all —
that is a composition bug, not an environment, so it exits 1 rather than
serving a console whose enrichment silently is not what the config says.

`ttl` is a cache row's lifetime, and 24h because the answers are slow-moving:
a hop's PTR record and its ASN change on the order of months. A row past its
TTL is re-resolved **on the next read that wants it**. There is no background
refresher in M5 (Decision 4), which is what makes an unread address free.
Resolution happens **synchronously on the request** that missed the cache, with
a per-lookup timeout under the caller's own context and misses bounded at 8 in
flight — the snapshot response ships whatever resolved in time and the cache
catches up on the next read.

`timeoutMs` is milliseconds rather than a duration string on purpose: the
useful range is 100–1000 ms, and an operator who writes `timeoutMs: 500` cannot
accidentally mean 500 ns. A resolver budget that quietly rounded to nothing
would make every hop look unresolvable.

Helm mounts the mmdb files: `console.mtr.enrichment.geoip.volume` is an opaque
VolumeSource passthrough mounted read-only at the fixed path **`/geoip`**, so
both paths above must live under it. Setting a path with no volume **fails
rendering** rather than shipping a config that names a file nothing mounts —
see "Chart behaviours worth knowing" below.

### The other half lives in the agent's config, not this file

The Console decides *what* to probe; the agent decides *whether it may*. The
agent half is `config.checkers.external` in the **shared** agent/controller
config (Helm: `config.checkers.external.*`), and it is not reachable from any
Console setting:

```yaml
checkers:
  external:
    enabled: false # OFF by default; the block is not even parsed while false
    allowedCidrs: [] # REQUIRED non-empty when enabled — an empty list is a startup error
    deniedCidrs: [] # carve-outs; denied wins
    maxTargets: 100 # defaulted when enabled and left at 0
    timeout: 10s # bounds resolve-and-authorise, NOT the probe itself
```

Three things worth stating plainly:

- **An empty `allowedCidrs` with `enabled: true` fails startup.** It is never
  read as "allow everything" — the same posture `auth.header.trustedProxyCIDRs`
  takes here, for the same reason: an empty list that means "everything" is a
  bypass wearing a config key's clothes.
- **`maxTargets` is validated but not yet enforced.** Startup rejects a
  negative value and fills in 100 when the feature is on and the key was left
  at 0, but nothing currently checks the assignment the controller pushes
  against it. Treat it as a declared intent, not a running control, until it
  is wired up — this is on the M4 deferral list in MILESTONES.md.
- **`timeout` is not the probe timeout.** It bounds resolution and
  authorisation of one destination. The probe stays governed by
  `checkers.<type>.timeout`, so enabling external checks cannot silently
  truncate a long MTR trace.

A `NetworkPolicy` cannot substitute for this. `allowedCidrs`/`deniedCidrs` are
enforced by the agent, in-process, after DNS resolution; see
[SECURITY.md](SECURITY.md) for why the agent rather than the Console is the
authority.

### Continuous probe cadence is not operator-configurable yet

A continuous external check is pushed to agents with a **30s interval and a 5s
per-probe timeout**, and both are compile-time constants in
`internal/console/checks/reconciler.go` — `defaultContinuousInterval` and
`defaultContinuousTimeout`. There is no config key, no Helm value and no API
field for either.

That is a data-model gap, not an oversight in the config schema:
`check_schedules` is the only row carrying a cadence, and `kind='continuous'`
is explicitly forbidden from carrying one (`ScheduleInput.Validate` rejects a
non-zero `interval_ns` on it) because a continuous check is never *fired*.
That rule was written about the Console-side firing cadence and left the
**agent-side probe** cadence with nowhere to live. `check_definitions.params`
is free-form JSON, but inventing an unvalidated magic key there would turn an
operator's typo into a silently-wrong probe rate with no feedback anywhere.

30s matches the agents' own normal checker-loop cadence, and 5s is a
deliberate small fraction of it so a hung probe cannot overlap the next one.
The natural home for a real field is `check_schedules.interval_ns` with the
continuous prohibition relaxed to mean "the probe interval" — one
migration-free validation change plus a read in the reconciler. Carried
forward out of M4.

### The cardinality bound is a compile-time constant

`POST /api/v1/checks/projection` reports, and create/update enforce, a
**per-definition** ceiling of **400** projected Prometheus series
(`maxProjectedSeries`, httpapi Decision 12). It is not configurable — not in
the config file, not in Helm — and 400 is not arbitrary: it is `checks.maxPairs`,
the bound the diagnostics runner already applies to one run's fan-out, reused
so the Console's two cardinality guards tell an operator the same story.

The bound is per definition, never fleet-wide, and that is load-bearing: the
projection endpoint reports the number for the single definition in its body,
the UI displays exactly that number, and the write path enforces exactly that
number, so the warning can never disagree with the enforcement.

The guard **fails open** when the topology cannot be read — a controller
outage must not become a configuration-write outage — counted by
`kconmon_ng_console_projection_guard_failopen_total`, with a WARN naming the
definition that slipped through.

### The per-mode auth matrix

`auth.mode` selects exactly one of these branches; the other three blocks are
parsed but ignored:

| Mode | Extra requirement | Failure if unmet |
| --- | --- | --- |
| `anonymous` | `auth.anonymous.role` non-empty | error |
| `header` | `auth.header.userHeader` non-empty, `auth.header.trustedProxyCIDRs` non-empty and every entry a valid CIDR | error — an empty list is an auth bypass, not a default to fall back on (SECURITY.md §10.1) |
| `local` | a resolved `database.dsn`/`dsnFile` (users live in PostgreSQL) | error |
| `oidc` | a resolved `database.dsn`/`dsnFile` (sessions live in PostgreSQL), plus `auth.oidc.issuer` an absolute `https://` URL with no trailing slash, `clientID` non-empty, `clientSecretFile` non-empty, `redirectURL` an absolute http(s) URL ending in `config.OIDCCallbackPath` (`/api/v1/auth/oidc/callback`) | error |

`controller.grpcAddr` is deliberately **unvalidated** beyond being a string. Like
`controller.url` it is operator config, and an unreachable address just fails to
dial at runtime, surfaced by the ingester's reconnect logs — not by refusing to
boot.

## Empty means off, and off is a tested state

These keys are feature switches whose empty/absent value is a supported deployment, not a
misconfiguration:

| Key empty | Effect |
| --------- | ------ |
| `controller.url` | `GET /api/v1/topology` answers `503 application/problem+json`; the topology pusher is not started |
| `prometheus.url` | matrix and PromQL APIs answer `503`; the matrix pusher is not started |
| `controller.grpcAddr` | realtime event ingestion is off. `/ws` still serves snapshot topics; `GET /api/v1/version` advertises no `events`; the UI badges itself "Delayed data" and stays on 15 s polling |
| `valkey.address` | the console runs on `cache.InProcessBus` — no cross-replica fan-out, correct only for one replica (ADR-002) |
| `database.dsn` **and** `database.dsnFile` (the resolved DSN) | the console runs with no store at all: `GET /api/v1/events` and `GET /api/v1/audit` answer `503`, RBAC admin (`/api/v1/rbac/*`) and tokens (`/api/v1/tokens`) answer `503`, run history is in-memory only (`checks.MemoryStore`), and `auth.mode` is restricted to `anonymous`/`header` (validation rejects `local`/`oidc` — see the per-mode matrix above) |

One combination is downgraded rather than accepted: `controller.grpcAddr` set
while `controller.url` is empty logs a warning and disables ingestion, because
the capability precheck needs the controller's HTTP API.

### Known M3 limitation: a Valkey dial failure at boot degrades sessions silently

The table row above describes `valkey.address` being *empty*. A configured
address that simply **fails to dial at startup** takes the same fallback path,
and this is the case worth alerting on. `newBus` (`cmd/console/main.go`) logs

```
WARN valkey unreachable at startup — falling back to the in-process bus;
     realtime fan-out is single-replica only until the console is restarted
```

and returns `cache.NewInProcessBus()`. Because the session/OIDC-state `cache.KV`
is built from that same bus object — `kv = cache.NewValkeyKVFromBus(vb)` only
when the bus type-asserts to `*cache.ValkeyBus`, otherwise
`cache.NewInProcessKV()` — the fallback silently takes **sessions** with it, not
just realtime fan-out, and the WARN text mentions only the latter. The pod then
reports **Ready**: nothing about the degraded bus is reflected in the readiness
probe, and there is no retry — the in-process bus is kept until the process
restarts.

The consequence, with `auth.mode=local|oidc` and `replicas > 1`: that one
replica holds its sessions in its own memory. Users load-balanced onto it are
logged out at random and cannot stay logged in, while the other replicas behave
normally. The Helm render guard (`auth.mode=local|oidc` + `valkey.mode=disabled`
+ `replicas > 1`, below) does **not** cover this — it is a render-time check on
the *declared* mode, and a `bundled`/`external` Valkey that is merely down at
the moment the console boots renders fine.

Operationally, until a readiness reflection lands (post-M3):

- Alert on the log line `valkey unreachable at startup` from any console pod;
  treat it as a paging-grade event in a multi-replica auth deployment, and
  restart the affected pod once Valkey is reachable again.
- Order the rollout so Valkey is up before the console (`console.replicas=1`
  during a Valkey outage is the crude but effective mitigation — one replica
  makes the in-process session KV correct rather than merely survivable).

The WebSocket hub itself is constructed **unconditionally**, since snapshot topics
do not depend on the event pipeline.

## Secret mounts

`database.dsnFile`, `auth.local.bootstrapAdminPasswordFile` and
`auth.oidc.clientSecretFile` — whichever apply — all resolve to paths under
one **sibling** directory, `/etc/kconmon-ng-console-secrets/`, mounted
alongside (not nested inside) the ConfigMap-backed
`/etc/kconmon-ng-console/`:

| Config key | Mounted path |
| --- | --- |
| `database.dsnFile` | `/etc/kconmon-ng-console-secrets/database-dsn` |
| `auth.local.bootstrapAdminPasswordFile` | `/etc/kconmon-ng-console-secrets/local-admin-password` |
| `auth.oidc.clientSecretFile` | `/etc/kconmon-ng-console-secrets/oidc-client-secret` |

This deviates from the milestone's original plan, which nested a `secrets/`
subdirectory under the ConfigMap-backed config directory itself: a container
runtime cannot create a mountpoint inside an already-mounted **read-only**
volume (`CreateContainerError`), so the two paths had to become siblings,
one mount, multiple projected `Secret` sources.

The projected volume renders with `defaultMode: 0440`, and
`console.podSecurityContext` (default `{fsGroup: 65532}`, matching the
distroless nonroot gid) sets the pod's `fsGroup` so the nonroot console
process can actually read a Secret file the kubelet writes root-owned. The
milestone's original plan called for `0400`; that mode is owner-only and
unreadable by the nonroot process without matching UID ownership, which a
projected Secret volume does not give you — `0440` (group-readable) plus
`fsGroup` is what actually works. `console.podSecurityContext` renders **only
when `console.database.mode != disabled`**, so the default manifest
(database off) stays byte-identical to chart 1.4.0.

Secrets are read once at boot; rotating one is an operator-initiated restart
(the Deployment rolls on ConfigMap changes only, never on Secret changes).

## Helm mapping (chart 1.7.0)

| Config key | Helm value |
| ---------- | ---------- |
| `httpPort` | `console.service.port` |
| `logLevel`, `logFormat`, `metricsPrefix` | shared `config.*` (same values as agent/controller) |
| `auth.*` | `console.auth.*` (secret-backed fields resolve to the mounted paths above) |
| `controller.url` | `console.controller.url`, or derived from this release's controller Service when empty |
| `controller.timeout` | `console.controller.timeout` |
| `controller.grpcAddr` | `console.controller.grpcAddr`, else this release's controller Service **only when `controller.events.enabled=true`**, else empty |
| `prometheus.*` | `console.prometheus.*` |
| `valkey.address` | derived from `console.valkey.mode`: bundled → the rendered Valkey Service, external → `console.valkey.address` (rendering fails if unset), disabled → empty |
| `valkey.dialTimeout` | `console.valkey.dialTimeout` |
| `database.*` | `console.database.*`; the whole `database:` block is omitted from the rendered config when `console.database.mode=disabled` |
| `database.dsnFile` | derived: `console.database.mode=cnpg` → the CNPG-operator-generated `<cluster>-app` Secret; `mode=external` → `console.database.existingSecret`/`existingSecretKey` |
| `rateLimit.*` | `console.rateLimit.*`; emitted unconditionally (the limiter always runs) |
| `scheduler.*` | `console.scheduler.*`; the whole `scheduler:` block is omitted from the rendered config when `console.scheduler.enabled=false` |
| `mtr.enrichment.*` | `console.mtr.enrichment.*`; the whole `mtr:` block is omitted from the rendered config when `console.mtr.enrichment.enabled=false` |
| `mtr.enrichment.geoip.{asnPath,cityPath}` | the same values, but they must name files under `/geoip` — the chart mounts `console.mtr.enrichment.geoip.volume` read-only there and offers no way to put a file anywhere else |

Realtime therefore needs **two** flags, one on each side:

```yaml
controller:
  events:
    enabled: true # serve EventStream.WatchEvents + advertise "events". Leader-only. Default false

console:
  enabled: true
  replicas: 2
  valkey:
    mode: bundled # bundled | external | disabled; with disabled, keep replicas: 1
```

### Chart behaviours worth knowing before you debug one

- **`controller.events` is emitted into the shared ConfigMap only when it is
  `true`.** The shared config is parsed with `KnownFields(true)` and Go's zero
  value makes an omitted key identical to an explicit `false`, so an
  unconditional key would crashloop a pre-M2 controller image as Pods roll. The
  chart's rendered output for a fleet with events off, `auth.mode=anonymous` and
  `database.mode=disabled` is byte-identical to 1.3.x's — the pre-events chart,
  before M2's Valkey/WebSocket additions and M3's auth/database additions both
  landed. 1.3.x is the comparison point because it PRE-dates those additions,
  which is the only thing that makes a byte-identical claim meaningful; 1.5.0
  is a *released* chart that already contains them, so it was never the right
  baseline. (This sentence previously called 1.5.0 a future release. It has
  shipped; the current chart is 1.7.0.)
- **`config.checkers.external` is emitted into the shared ConfigMap only when
  it is enabled**, for exactly the `controller.events` reason above: a pre-M4
  agent image has no `External` field and `KnownFields(true)` would crashloop
  it on an unconditional key as the DaemonSet rolls node by node. Off by
  default also means an install that never touched the block renders an agent
  `config.yaml` byte-identical to chart 1.5.0's.
- **`console.scheduler` is emitted only when enabled**, and that one flag gates
  three consumers: the schedule loop, the stuck-run reaper, and the continuous
  external-check reconciler. They share the flag *and* the PostgreSQL advisory
  lock, so there is no way to run the reconciler without the scheduler.
- **`console.mtr` is emitted only when `console.mtr.enrichment.enabled`**, for
  the same rolling-image reason as the two above: a pre-M5 console binary has
  no `MTRConfig` field at all, so `KnownFields(true)` would crashloop the old
  Pod as the Deployment rolls. Worth being precise about, because it is easy to
  state the reason wrongly: the *current* console parses `mtr:` happily — the
  gate is justified by the image being replaced, not by this one. Off by
  default also means an install that never touched the block renders a console
  `config.yaml` key-identical to chart 1.6.0's.
- **`console.rateLimit` is emitted unconditionally**, unlike the three above.
  There is no "off" state to gate on — the limiter always runs, and `0` is a
  value that means "this limit is off", not "this block is absent".
- **The render-time guard keys on the RESOLVED gRPC address**, not on
  `controller.events.enabled`: an explicit `grpcAddr` with events off is still
  realtime-on-with-no-bus. Resolved address + `valkey.mode=disabled` +
  `replicas > 1` fails rendering with an explanatory message.
- **The console→controller gRPC NetworkPolicy egress rule is gated the same
  way**, for the same reason: egress missing on an explicitly configured dial
  target is a silent packet drop with no diagnostic. The controller-side *ingress*
  half lives in the shared root policy and is keyed on `events.enabled` — that
  flag belongs to the controller.
- **`grpcAddr` pointed outside this namespace is the operator's own policy to
  write**, on both the egress and the ingress side. There is no `grpcEgress`
  override list.
- **Three more render-time `fail` guards were added in M3**
  (`templates/console/configmap.yaml`), all keyed on RESOLVED values the same
  way the M2 guard above is:
  - `auth.mode=local|oidc` with `valkey.mode=disabled` and `replicas > 1` fails:
    sessions for local/oidc live in Valkey (DATA.md §5.3), not the in-process
    KV, once more than one replica is running.
  - `auth.mode=local|oidc` with `database.mode=disabled` fails: users, tokens
    and the audit log live in PostgreSQL (SECURITY.md §10.1), so a database is
    not optional the way it is for anonymous/header.
  - `auth.mode=header` with an empty `auth.header.trustedProxyCIDRs` fails: an
    empty list would make an unauthenticated request header an authentication
    bypass.
- **A fifth `fail` guard landed in M5**, keyed the same way on resolved values:
  `console.mtr.enrichment.enabled` with a geoip path set and
  `console.mtr.enrichment.geoip.volume` empty fails rendering. The chart mounts
  that volume read-only at the fixed path `/geoip` and nothing else can put a
  file there, so the alternative is a console that boots, warns once and runs
  with geoip silently off. The volume is an **opaque VolumeSource passthrough**
  — the chart deliberately does not model the
  `{configMap|secret|hostPath|persistentVolumeClaim}` union: GeoLite2 files run
  to ~10 MB (past what many clusters accept in a ConfigMap), operators keep
  them in genuinely different places, and a union re-declared in
  `values.schema.json` goes stale the first time Kubernetes grows a source.
  The API server validates what you wrote. The mount path is fixed rather than
  a value because the chart would otherwise own two halves of the same fact —
  a `mountPath` here and an `asnPath` in the ConfigMap — with nothing keeping
  them agreeing, and a drift between them renders perfectly and fails only at
  runtime.
- **Two NetworkPolicy paths were added in M3**, both requiring both layers
  (egress on the console side, ingress on the destination side) exactly like
  the M2 controller-gRPC and Valkey rules:
  - **console↔database**: `console.database.mode=cnpg` renders a precise
    pod-selector egress rule (`templates/console/networkpolicy.yaml`) plus a
    matching ingress rule on the CNPG cluster's own NetworkPolicy
    (`templates/console/database-networkpolicy.yaml`, which also opens CNPG's
    own inter-instance replication/status ports and the Prometheus scrape port
    9187). `mode=external` renders a namespace-wide default (tighten via
    `console.networkPolicy.databaseEgress`).
  - **console→OIDC IdP**: `auth.mode=oidc` renders an egress rule defaulting to
    `ipBlock: 0.0.0.0/0` on TCP 443 (an IdP is almost always outside the
    cluster, so a `namespaceSelector` would silently match nothing), tighten
    via `console.networkPolicy.oidcEgress`.
- **A NetworkPolicy is only one of two layers.** If the destination sits behind a
  host firewall (iptables/nftables, a puppet-managed ruleset, a cloud security
  group), that layer must be opened separately. Egress allowed in the chart and
  still refused on the wire is almost always the host-firewall layer.
- **Whether kubelet probes are subject to the ingress policy is CNI-dependent.**
  This is chart-wide and pre-existing, not specific to the console.
- **The bundled Valkey is ephemeral by design**: a Deployment with no PVC, no
  volumeClaimTemplates, no writable data dir (ADR-002).
- **CNPG is a CRD chicken-and-egg** (ADR-001): `console.database.mode=cnpg`
  renders a `Cluster` CR but the chart never installs the CNPG operator or its
  CRDs. `helm install` fails outright with a clear "no matches for kind
  Cluster" error if the operator is not already present — there is no silent
  partial-render.
