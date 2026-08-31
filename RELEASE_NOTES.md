## kconmon-ng v2.3.0

> The sparse mesh changes WHAT "no data for a pair" means: under
> `topology.mode: sparse` most directed pairs are deliberately never probed.
> Everything in this release that reads per-pair series learns to tell "not
> planned" from "went dark" through one new metric,
> `kconmon_ng_probe_intended` — and that metric comes from the AGENT: images
> below appVersion 2.3.0 do not export it. The chart's rules degrade
> honestly on an older fleet (see PairWentSilent below), but do not flip
> `topology.mode: sparse` until controller AND agents run a 2.3.0 image —
> the controller config key is emitted only when sparse precisely because an
> older controller image rejects it and crashloops. The appVersion pin is
> aligned when the app release ships.

### Added

- **`topology.*` — the sparse probe mesh, by values.** `topology.mode:
  sparse` trims the full N×(N−1) probe matrix to a ring over sorted node
  names (`sparse.ringDegree` successors each, the connectivity guarantee)
  plus HRW-chosen cross-zone chords (`sparse.zoneChords` per directed zone
  pair, which keep the zone metric family fully populated), so probed pairs
  — and every per-pair series they export — scale ~linearly with node count
  instead of quadratically. `sparse.autoThreshold` is the floor: fleets
  smaller than it get the full mesh regardless of mode, because sparse only
  pays for itself at scale. Default is `mode: full`, byte-identical
  rendering to 2.2.0.
- **`kconmon_ng_probe_intended` — the plan, scrapable.** A gauge, value 1
  for every directed pair the topology plan assigns
  (`{source_node, destination_node}`, exported by the source agent), preset
  from the peer list at registration and pruned on every plan change —
  stale pairs are deleted, not left at 1. In full-mesh mode it simply marks
  every peer, so dashboards and rules can join on it without caring which
  mode the fleet runs. It is the one honest way to distinguish "this pair
  is not supposed to report" from "this pair went dark", which is why it
  ships in the same release as sparse mode and not one later.
- **`investigateUrl` on the two zone alerts.** `ZoneChecksFailing` and
  `ZoneLossHigh` now annotate a console deep link,
  `/investigate?kind=zone-pair&scope=<source>-><destination>`, straight
  into the Investigate page scoped to the firing zone pair. The link is
  console-RELATIVE on purpose — the chart cannot know the console's
  external URL (ingress is optional), so notification templates prepend
  their own origin; the console normalises the typeable `->` into its
  canonical pair arrow.

### Changed

- **`PairWentSilent` joins on the plan.** The rule now fires only for pairs
  present in the source agent's `kconmon_ng_probe_intended` series — the
  hard rule of the sparse design, shipped in the same release: without the
  join, every pair the plan trims would read as "went silent" for the hour
  its results take to age out of the lookback window. The fallback is per
  SOURCE, not global: a `source_node` exporting no `probe_intended` at all
  keeps the old two-window behaviour, so a pre-2.3.0 agent image alerts
  exactly as before, a mixed fleet mid-rollout gets each behaviour where it
  applies — and an agent that dies outright takes its `probe_intended`
  series with it, which lands its pairs in the same fallback and preserves
  the alert's original purpose: catching an agent that stopped running or
  stopped being scraped.

> This release also carries everything prepared for the never-published 2.2.0
> tag (its pipeline caught two release-tooling defects before anything went
> out); those changes follow below, under their original heading kept for
> upgrade notes.

### Carried over from the unreleased 2.2.0

> Everything in this release reads the new zone-level metric family
> (`kconmon_ng_zone_*`), and that family comes from the AGENT, not the chart:
> agents below appVersion 2.2.0 do not export it (this chart pins 2.2.0, so a
> default install is fine — the warning is for fleets running an older agent
> image behind a newer chart). Until the fleet runs an agent image that does, the two zone
> alerts are silently inert (their expressions match no series), the Zone
> Heatmap dashboard renders empty, and `agent.metrics.detail: zone-only`
> would drop the per-pair series with nothing replacing them — Prometheus
> goes dark on the mesh while the console keeps working. Upgrade the agent
> image first, flip the valve second. The appVersion pin is aligned when the
> app release ships.

### Added

- **`ZoneChecksFailing` and `ZoneLossHigh`.** Two alerts on the zone plane,
  with the same per-rule knobs as the rest
  (`prometheusRule.{zoneChecksFailing,zoneLossHigh}.{enabled,threshold,for,severity}`).
  `ZoneChecksFailing` is the failure ratio of all TCP, UDP and ICMP probes
  between a zone pair, in one expression — the `__name__` union keeps a
  disabled checker from blanking the ratio. `ZoneLossHigh` computes loss as
  `(sent − received) / sent` from the zone packet counters; averaging the
  per-pair loss-ratio gauges into a zone would weight an idle pair the same
  as a busy one, so the chart never does. Its default threshold is `0.1`,
  lower than the per-pair `UDPLossHigh` at `0.5`, because the zone aggregate
  dilutes any single link by the pair count: sustained loss at that level
  means the fabric, not one node. Both survive every `agent.metrics.detail`
  mode — that is the point of alerting on the zone family.
- **`agent.metrics.detail` — the cardinality valve.** A scrape-time knob
  rendered as `metricRelabelings` on the agent ServiceMonitor:
  `full` (default, everything, ~70 series per directed pair),
  `counters-only` (drops the four per-pair histograms, ~10/pair — every pair
  alert keeps firing), `zone-only` (drops every series naming a
  `destination_node`, ~0/pair; the zone family at ~74×Z² series and the
  linear DNS/HTTP/external families remain). At 100 nodes that is ~0.7M →
  ~0.1M → practically N-independent, by configuration alone. Setting it
  without `serviceMonitor.enabled` is refused at render time rather than
  silently dropping nothing; plain-Prometheus equivalents are in
  `docs/metrics.md`.
- **`controller.externalGateway` — the external agent gateway, exposed by the
  chart.** The controller's second gRPC listener (same services, but TLS with
  a bootstrap token, for agents OUTSIDE the cluster) gets a values block and
  three templates. `templates/controller/service-external.yaml` is a
  NodePort/LoadBalancer Service carrying the gateway port ALONE — the
  plaintext in-cluster gRPC port authenticates by network position and never
  appears on it, because a LoadBalancer in front of it would hand the whole
  mesh to anything that can reach the address. The deployment mounts two
  referenced Secrets read-only: `tls.secretName` (a `kubernetes.io/tls`
  serving pair; `tls.clientCaKey` names the CA bundle key in the same Secret
  and switches on client-cert identity pinning — empty is token-only mode,
  where any token holder can impersonate any agent, and NOTES.txt says so at
  install) and `bootstrapToken.{secretName,key}`. With
  `networkPolicy.enabled`, ingress on the gateway port is opened from
  `networkPolicy.externalAgentCidrs` toward the controller pods alone, and an
  empty list is refused at render rather than shipping a gateway no packet
  can reach; missing Secret names and a port colliding with
  `config.{httpPort,grpcPort,metricsPort}` are refused the same way. Two
  operational notes. Rotation: the gateway reads the certificate and token
  ONCE at startup and the chart cannot checksum content it only references,
  so rotating either Secret in place needs
  `kubectl rollout restart deploy/<release>-controller`. Version skew: the
  `externalGateway` config key is emitted only when enabled, because a
  controller image at appVersion 2.0.3 rejects the unknown key and
  crashloops — upgrade the image before flipping the switch, same rule as
  the zone family above.

### Changed

- **The Zone Heatmap dashboard reads the zone family.** Every panel that
  aggregated per-pair series into zones at query time now reads the
  pre-aggregated `kconmon_ng_zone_*` metrics, so the dashboard keeps working
  in every `agent.metrics.detail` mode and its queries stop scaling with the
  pair count. Loss panels are packet-weighted from the sent/received counters
  instead of averaging the per-pair ratio gauges. The one exception is the
  "MTR traces triggered" panel: MTR has no zone-level family, its counter is
  per-pair, and in `zone-only` mode that panel reads zero — its description
  now says so.

### Performance and self-observability

- Peer probing fans out with a bounded pool (32 in flight per round): a dead
  peer costs one timeout, not one timeout per peer in sequence, so probe
  cadence holds through partitions.
- Reactive MTR traces are bounded by a global semaphore (4 in flight) on top
  of the existing per-pair cooldown; a mass partition trickles traces out
  instead of forking one per broken pair.
- The agent exports self-metrics under `kconmon_ng_agent_*`: probe cycle
  duration and overruns per checker, controller reconnects, peer-list age,
  reactive-MTR in-flight and coalesced counters. All are fleet-size
  independent.
- The controller coalesces peer-list broadcasts (trailing edge, 200 ms): a
  rollout's burst of registrations produces one broadcast, not one per
  change; the peer message is built once per broadcast and carries only the
  fields agents read.

## kconmon-ng v2.1.0

### Added

- **`agent.updateStrategy` is a value.** The DaemonSet hardcoded
  `maxUnavailable: 1` — the right default and the wrong ceiling: one node at a
  time turns a version rollout across a few hundred nodes into hours of
  half-upgraded fleet. The block passes through verbatim, so
  `maxUnavailable: 10%` — or `OnDelete` — is now a values change instead of a
  fork of the template.
- **`priorityClassName` on every workload.** `agent.priorityClassName`,
  `controller.priorityClassName` and `console.priorityClassName`; empty by
  default, and empty renders nothing. The agent is the one worth setting: under
  node pressure the kubelet evicts lowest priority first, and the first pod
  gone should not be the one reporting on the node.

### Fixed

- **`helm test` passes restricted-PSS admission.** The connection-test Pod ran
  curl with no securityContext at all, so a namespace enforcing the restricted
  Pod Security Standard rejected it at admission — the test failed before it
  made the one request it exists to make. The container now declares the four
  fields the profile checks: `runAsNonRoot`, a `RuntimeDefault` seccomp
  profile, no privilege escalation, all capabilities dropped. The image already
  runs as uid 100.

## kconmon-ng v2.0.3

### Fixed

- **A config change restarts the pods that read it.** The agent and the
  controller share one ConfigMap and read it once at startup, and a mounted
  ConfigMap changes under a running process without telling it — so
  `controller.events.enabled: true` applied to a live release updated the object
  and left the controller on the old file. It went on advertising no
  capabilities, the Console's realtime ingester retried against a stream that was
  configured but never started, and nothing anywhere reported an error: the Live
  page was simply empty. Both workloads now carry `checksum/config`, so a values
  change rolls them; a change the ConfigMap does not carry still does not.

## kconmon-ng v2.0.2

### Fixed

- **The matrix no longer opens at half size.** The grid measured the height its
  own content had produced and fed that back into the fit, so a fresh render
  saw the container's 256px minimum, decided the grid did not fit, shrank to
  50%, and the smaller grid then held the box at 256px — a loop with no way out.
  It measures the space available instead, and a seven-node fleet opens at 100%.
- **Zooming in gives the node names back.** The shared prefix every node name
  begins with is dropped to buy column width, which is right while the column is
  narrower than the names and wrong the moment it is not: at 125% a label column
  holds `adm-kuber-01` with room over and still read `…01`. The elision is now
  decided per axis at the current scale, and the note above the grid appears only
  while an axis is actually eliding.

### Added

- **A favicon.** The console had none, so every tab showed the browser's blank
  square; it now wears the mark it wears in its own sidebar.

## kconmon-ng v2.0.1

> Fixes a hole in 2.0.0: `auth.mode=oidc` and `auth.mode=header` shipped with no
> way to grant anybody a role. Both modes worked, and neither was usable.

### Fixed

- **An OIDC or header install can grant roles at deploy time.** Role bindings
  live in the database and are created through an API that already requires
  `rbac:manage`, so a fresh install had nobody able to make the first binding;
  the only alternative was `auth.defaultRole`, which is one role for every
  authenticated subject. The way out that 2.0.0 left was to bring the console up
  in local mode, log in, create a binding by hand and only then switch — a
  workaround, published as if it were a procedure.

  `console.auth.groupRoles` maps a group the identity provider asserts onto a
  role this console grants, in the values file:

  ```yaml
  console:
    auth:
      groupRoles:
        platform-oncall: admin
        everyone: viewer
  ```

  Roles resolve as the union of that map and any binding made through the API, so
  a grant by hand still adds to what the provider's groups carry. A group absent
  from the map grants nothing. What the map grants cannot be revoked through the
  API — that is what makes it declarative.
- **A role store outage no longer costs an operator their access.** The store's
  half still fails closed, because an unreadable database is no evidence a
  subject holds anything; a grant that came from the claim and the config was
  never in doubt, and an outage is when the console is most needed.

## kconmon-ng v2.0.0

> A chart that installs monitoring and nothing else, a console that survives more
> than one replica, and one that tells the truth about time. The chart no longer
> ships a database or a cache — point it at the ones you already run. The Time
> Machine moved out of the top bar and into each page's own time controls, and
> the charts pin their axis to the window you asked for rather than to the data
> that happened to arrive. MTR gained a Runner, path history that reads as a
> timeline, and external targets.

### Breaking

- **The chart no longer installs PostgreSQL or Valkey.** `database.mode`,
  `database.cnpg.*` and the bundled subcharts are gone: set
  `database.existingSecret` to a Secret holding a `postgres://` DSN and
  `redis.existingSecret` to one holding a `redis://` DSN, and any managed
  instance works — RDS, a StatefulSet, a CloudNativePG cluster you run yourself.
  Every removed key fails the render with a message naming its replacement
  (`templates/_migrations.tpl`), so no old value is silently honoured.
- **`console.database.*` moved to the top-level `database.*`**, and
  `console.*` keys that described the bundled datastores went with it.

### Added

- **Chart 2.0.0, templates split per component** — agent, controller, console,
  shared and observability each own their directory, with a NetworkPolicy set
  covering every component, fail-closed on external egress. Render-time guards
  refuse a port collision, an OIDC `redirectURL` the console would not start on,
  and more than one console replica without a shared cache.
- **The console scales past one replica** — sessions, the fixed-window rate-limit
  counters and the realtime fan-out live in the Redis-compatible server, and the
  controller elects a leader so exactly one replica drives the reconcilers.
- **MTR Runner and path history** — start a trace from the Explorer itself,
  with a settable cadence and duration; every distinct route the fleet has taken
  is kept, diffed and drawn on a timeline of when it changed.
- **External targets** — probe a destination that is not a fleet peer, gated by
  `config.checkers.external.allowedCidrs` and the cluster's own egress policy.
  The console refuses at create time a target no agent could ever reach.
- **Time in the Console's result table** — every figure says when it was read.

### Changed

- **The Time Machine lives with the page's time filters**, not in a strip across
  the top of every route. It is offered only on the pages that resolve their
  reads through `?at=`, and the engaged banner stays global because writes are
  disabled console-wide.
- **Explore's axis is the window you picked** — a 24h view draws 24 hours even
  when Prometheus holds less, instead of quietly redrawing three.
- **MTR Explorer is sorted by name**, both destinations and their sources, with
  numbers read as numbers (`m9` before `m10`).
- **OIDC identity is the `sub` claim**, namespaced as `oidc:<sub>` — the only
  claim OIDC Core §5.7 allows as an identifier. `auth.oidc.usernameClaim` now
  decides the display name alone, so renaming a person no longer moves their
  roles (Grafana's CVE-2023-3128 is what the old shape risked). Group membership
  is re-read on every token refresh. **Bindings made against a username stop
  granting**; the console names them at boot so they can be remapped.
- **The configuration bundle carries access control** — custom roles and the
  grant list, but only for a caller who holds `rbac:manage`. Roles import;
  bindings never do, because a grant names a person in the source console's own
  identity namespace.
- **Only the chart under the cursor shows a tooltip.** Its neighbours keep the
  shared crosshair and mark their own samples with a dot, instead of each
  covering its own curves with a box of numbers.

### Fixed

- WebSocket topics are authorized per topic: `events:read` no longer carries the
  topology and matrix snapshots that `topology:read` and `matrix:read` gate, and
  a permission taken away reaches a socket that is already open — the topics it
  may no longer have are dropped, the rest of the connection is left alone.
- **The audit row describes the mutation that happened.** A body could name one
  thing for the handler and another for the audit log by spelling a key in a
  different case, and a value carrying a NUL made the whole row unwritable — in
  both directions the caller chose whether their own privileged action was
  recorded. The extraction now matches keys the way `encoding/json` matches
  struct fields, and is bounded before it is decoded, so a wide body on the
  public login route can no longer take the replica past its memory limit.
- **A broken alert rule no longer freezes the whole bundle.** Editing a deployed
  rule into PromQL the apiserver rejects used to stop every other rule from
  being applied, while the API answered 2xx and Prometheus kept evaluating the
  stale set. The quarantine now keys on rendered content rather than rule ids,
  offers each suspect to the cluster on its own, and removes the object only
  when every rule was offered and every one refused.
- **`auth.mode=anonymous` is not exempt from CSRF.** Any page an operator's
  browser visited could POST into a console kept off the internet; a
  cross-origin write is now refused, while a script that sends no `Origin` is
  unaffected.
- **The node-local HTTP checker verifies certificates.** An expired certificate,
  one issued for another hostname or an interceptor's CA all used to pass, so an
  https check could not fail on the condition it was added to notice; opt out per
  target with `insecureSkipVerify`.
- **External metrics separate the checks on one target** — the series carry
  `check_type`, so an icmp and a tcp check on the same target no longer average
  each other's failures away under the `ExternalChecksFailing` rule.
- **A check no agent could run is refused when it is written**, instead of being
  dropped by every agent with nothing but a log line while the console listed it
  as enabled.
- **The MTR destination listing is complete.** It is paged behind a keyset
  cursor rather than capped, so no pair is missing from the Explorer and no
  per-destination total is short.
- A subscriber that stops reading its peer-update stream is torn down rather than
  holding a controller goroutine and its connection slot until TCP notices.
- Shutdown finishes in-flight runs before tearing down the pipeline they publish
  onto, so a rolling update no longer logs dropped frames that were delivered.
- Every request body is capped, so one oversized POST can no longer take a
  console replica past its memory limit.
- The OIDC callback binds its `state` to the browser that started the flow.
- A role-store failure now refuses rather than granting the default role.
- External TCP and UDP checks probe what was asked for instead of speaking the
  agent's own protocol to something that is not an agent.
- A user binding can no longer be resolved by a subject of another kind: role
  resolution matches the caller's kind as well as their id.
- Revoking a role binding is auditable — the audit row names the role and the
  subject, read before the row is destroyed rather than after.
- Path history says when it has reached the end instead of leaving a "Load
  older" button that can never be pressed, and counts the routes it is showing
  against the traces folded into them.
- A probe tick on a diagnostics run leads with that probe — its sequence, its
  clock, its latency or its error — so two ticks on an unchanged route are no
  longer indistinguishable.

## kconmon-ng v1.9.0

> Console release, and the last planned milestone. Alert rules you build in the
> Console and Prometheus evaluates; alert webhooks; configuration
> export/import; a command palette; a Settings page. Plus one long-carried fix:
> the controller finally attributes topology changes, so Time Machine
> reconstructs a real cluster. Everything new is off by default or
> read-only-additive: a 1.8.0 install that upgrades and changes nothing renders
> the same manifests.

### Added

- **Alert rule management** — `/alerting` builds a Prometheus alert rule from
  six typed templates (pair loss, zone latency, DNS failures, HTTP TTFB,
  agent missing, external target down) or raw PromQL — seven kinds in all —
  and the Console
  reconciles every enabled rule into **one** `PrometheusRule` object by
  server-side apply. **The Console manages; Prometheus evaluates.** Nothing
  here decides that an alert fired.
- **Validation by running it, not by parsing it** — there is deliberately **no
  `prometheus/prometheus` parser dependency**. Every template has a byte-exact
  render golden, and `POST /api/v1/alert-rules/preview` runs the expression as
  an instant query against your actual Prometheus and reports how many series
  it matches. The render and the query fail independently: a render failure is
  a `422`, a query failure is a `200` carrying the expression and the error.
- **Drift is recorded, then fixed** — a reconcile always re-asserts the
  Console's bytes. A rule showing `drift` also carries a fresh `lastSyncedAt`,
  and both are true: the divergence was observed and corrected in the same
  pass. Failures never crash the loop; they land per rule as `sync_status=error`
  with a closed cause class (`crd-missing`, `forbidden`, `other`).
- **Foreign rules and explicit adoption** — `PrometheusRule` objects the
  Console did not write are listed read-only, and
  `POST /api/v1/alert-rules/import` **copies** one into builder rows. The
  foreign object is never mutated, which means **the same alerts then exist
  twice until you remove one copy**. The import report says so, and names every
  skipped rule with its reason.
- **`GET /api/v1/alerts`** — the firing set, projected onto this API's
  vocabulary, with `?managedOnly=`. With no Prometheus configured it answers
  `200` and `promConfigured: false` rather than `503`: "nothing is firing" and
  "nobody is watching" are different sentences.
- **Alert webhooks** — `alert.fired` and `alert.resolved`, their own payload
  family, dispatched from a poller that diffs Prometheus' alert state on
  `console.webhooks.alertPollInterval` (30s). It baselines on boot rather than
  paging the fleet about what was already broken, freezes on a failed or
  undecodable poll rather than "resolving" everything, and ignores rules the
  Console does not manage. The M6 incident payload bytes are unchanged.
- **Configuration export/import** — `GET /api/v1/export` and
  `POST /api/v1/import`, versioned bundle v1, admin-only under
  `settings:write`, **dry-run first**. Webhook endpoints export with
  `hasSecret` only and therefore cannot be created by import — a sealed secret
  never leaves this API.
- **Settings page** — webhook CRUD, export/import with a per-collection dry-run
  report, and read-only deployment info that renders only what
  `GET /api/v1/config` actually serves.
- **Command palette** (`⌘K` / `Ctrl-K`) — hand-rolled, zero dependencies, over
  navigation (generated from the sidebar so it cannot drift), five actions and
  the Time Machine pair. It does **not** jump to an arbitrary node, target or
  pair: that needs a live object search, not a static registry.
- **Overview** — the "Firing alerts" placeholder carried since M1 is now the
  real panel, severity-sorted with oldest-first ties, and the Investigate
  timeline gained an alert row.
- **Permissions** — `alerts:read` (all built-in roles) and `alerts:manage`
  (operator, admin, **and `alert-editor`** — the builtin has waited for exactly
  this permission since M3, and a role by that name that cannot edit an alert
  rule breaks its promise on first click). `AllPermissions` is now **25**.

### Fixed

- **Topology events are attributed** — the controller now emits one
  `topology_changed` event **per affected agent**, carrying `nodeName`,
  `agentId` and `zone`, from all four sites (register, zone update,
  deregister, stale eviction). Time Machine's topology fold reconstructs a real
  node set with real zone lanes instead of an honest empty one. Events written
  by an earlier controller are counted as unfoldable and age out with
  retention; the page reports both numbers rather than rendering an empty
  cluster.
- **WebSocket topics are authorized individually** — `/ws` admits
  `events:read` **or** `runs:read`, and a subject admitted on `runs:read` alone
  gets `run:{id}` topics and an error frame for the fleet-wide ones, on a socket
  that stays open. A custom role can finally watch the run it started. Carried
  from M3.
- **A `null` `console.database.cnpg` override no longer crashes rendering** —
  nor does a null sub-block. Real nil-pointer class, found by the schema work.
- One redundant token listing on the ownership-resolution path is now a
  targeted lookup.

### Chart

- 1.8.0 → 1.9.0. New value blocks: `console.alerting.*`
  (`enabled`/`namespace`/`syncInterval`/`bundleName`, rendered into the console
  ConfigMap only when enabled) and `console.webhooks.alertPollInterval`
  (rendered only when a key Secret is named **and** alerting is on — the key
  existed in the binary since M7 but was unreachable from Helm).
- Enabling `console.alerting` renders a **namespaced `Role`** and `RoleBinding`
  over `monitoring.coreos.com/prometheusrules`
  (`get,list,watch,create,update,patch` — **never `delete`**), bound to the
  console-only ServiceAccount. Not a ClusterRole: the Console writes one object
  into one namespace, and pointing `namespace` elsewhere fails with a
  `forbidden` rather than widening anything.
- The console ServiceAccount, `serviceAccountName`, `POD_NAMESPACE` and the
  apiserver egress rule are now shared between `console.kubernetesContext` and
  `console.alerting` through one helper, so either flag renders them.
- `values.schema.json` closes **44 chart-owned levels** with
  `additionalProperties: false`, and gained the `nameOverride`,
  `fullnameOverride`, `agent.nodeSelector` and `agent.affinity` keys the
  templates always used and the schema never declared. Pod/container
  `securityContext` stay deliberately **open** — Kubernetes grows union members
  every release, and closing them would turn a cluster upgrade into an install
  failure.
- Three new ci profiles: `console-alerting-values.yaml` (the fullest console),
  `console-auth-local-values.yaml` and `console-auth-header-values.yaml`.
  Default render is key-identical to 1.8.0.

### Upgrade notes

- Nothing to do. `alert_rules` is created by migration `00007` on first start
  with `console.database.mode=cnpg|external`; with the database disabled the
  new routes answer 503 exactly as M3–M6's do.
- **Turning on `console.alerting.enabled` needs three things the chart cannot
  check**: the Prometheus Operator's `PrometheusRule` CRD, a database, and a
  Prometheus whose `ruleSelector`/`ruleNamespaceSelector` actually selects the
  object the Console writes. A rule that syncs cleanly and never fires is
  almost always the third one.
- **Changing `console.alerting.bundleName` on a live install orphans the
  previous object.** The reconciler owns what it is pointed at and deletes
  nothing.
- There is **no leader election** on the alert-webhook watcher: N console
  replicas deliver N copies of every edge. The payload carries a stable
  `(event, ruleId, labels, firedAt)` tuple so a receiver can dedupe.
- `alert-editor` gained `alerts:manage`. If you granted that builtin to
  somebody expecting it to stay inert, it is now able to create, edit and
  delete alert rules.
- If you set values the schema never declared, `helm upgrade` may now reject
  them. That is the typo protection working — check the key against
  `values.yaml`.

## kconmon-ng v1.8.0

> Console release. Investigation Mode with an honest, documented correlation
> panel; saveable incidents with shareable permalinks; maintenance windows;
> outbound webhooks; and optional Kubernetes event capture (M6). Everything
> new is off by default or read-only-additive: a 1.7.0 install that upgrades
> and changes nothing renders the same manifests.

### Added

- **Investigation Mode** — `/investigate` assembles a merged timeline, synced
  signal panels (loss/RTT with a matrix delta chip and an MTR path diff) and
  an actions rail for a scope and a time range. Entry is the URL and only the
  URL — `?kind=&scope=&from=&to=` — from any node/pair/target card, any matrix
  cell, or the page's own form. Every source is permission-gated with **zero
  requests** when denied, and each absent or bounded one leaves a muted line
  rather than blanking the page.
- **Correlation v1, documented rather than magic** — edge-triggered threshold
  crossings (loss > 1%, RTT > 2× the range **median**), an onset, a 300-second
  candidate window and a linear proximity decay against published class
  weights. The panel links the scoring source itself, so the operator reads
  exactly the constants the code executes. No ML, and nothing you cannot
  reproduce by hand.
- **Incidents** — save an investigation (`/api/v1/incidents`), pin findings
  from six source kinds, write notes, resolve and reopen. The permalink
  `/investigate?incident={id}` rehydrates scope and range **from the row**, so
  the link cannot drift from the incident it names. Open incidents appear on
  Overview and beside the charts on every object card. `PATCH` is deliberate
  and is this API's only one: an incident evolves under collaboration, and a
  full replace would let one writer discard another's notes.
- **Maintenance windows** — `/api/v1/maintenance`, drawn as `markArea` on
  Explore, the Pair card and the Target card and as timeline rows. M6 **renders**
  declared windows; it does not suppress anything, because nothing evaluates
  alerts until M7.
- **Outbound webhooks** — `/api/v1/webhooks` (admin-only `webhooks:manage`),
  firing on incident lifecycle. Deliveries are signed
  `X-Kconmon-Signature: sha256=<hmac>` over the raw body, retried 3 times
  (0s / 30s / 5m, ±20% jitter, 10s per attempt), with the outcome kept on the
  endpoint row. Each endpoint's signing secret is **write-only** over the API
  and sealed at rest with AES-256-GCM under
  `console.webhooks.encryptionKeySecret`. `POST /{id}/test` sends one signed
  ping. Without a key, create and test answer 503 and everything else keeps
  working.
- **Kubernetes event capture** — `console.kubernetesContext.*` (off by
  default) list+watches core/v1 Events into `k8s_events`, filtered to nodes in
  the fleet topology and pods in one namespace, and read back through
  `GET /api/v1/k8s-events` under `events:read`. **Zero new Go dependencies** —
  it reuses the client-go the controller already pulls in. Without a topology
  it fails closed and drops node events rather than storing an unfiltered
  firehose.
- **Overview** — the "Recent events" placeholder that had been carried since
  M2 is now the real panel, and an "Open incidents" card sits beside it.
  "Firing alerts" stays an honest placeholder until M7.
- **Permissions** — `incidents:read` and `maintenance:read` (all built-in
  roles), `incidents:write` and `maintenance:write` (operator/admin), and
  `webhooks:manage` (**admin only** — an endpoint carries a signing secret).
- **Metrics** — `kconmon_ng_console_k8s_events_total{result}` and
  `kconmon_ng_console_webhook_deliveries_total{result}`, plus three new table
  values on `kconmon_ng_console_retention_deleted_total`.

### Chart

- 1.7.0 → 1.8.0. New value blocks: `console.kubernetesContext.*` (rendered into
  the console ConfigMap only when enabled) and
  `console.webhooks.encryptionKeySecret.{name,key}` (referenced by name,
  mounted as a file beside the DSN — there is deliberately no inline-key
  value). Enabling `kubernetesContext` also renders a **console-only**
  ServiceAccount, its own ClusterRole (`events: list, watch`) and binding,
  `serviceAccountName` on the console Deployment, a `POD_NAMESPACE` downward-API
  variable and an apiserver egress rule (`console.networkPolicy.kubeAPIEgress`).
  The agent/controller ServiceAccount and its grant are **untouched**. A ninth
  ci profile (`console-investigation-values.yaml`). Default render is
  key-identical to 1.7.0.

### Upgrade notes

- Nothing to do. All four M6 tables are created by migration `00006` on first
  start with `console.database.mode=cnpg|external`; with the database disabled
  the new routes answer 503 exactly as M3–M5's do.
- Turning on `console.kubernetesContext.enabled` is a deliberate act with an
  RBAC consequence — read SECURITY.md §10.3 first.
- Webhook endpoints are **API-only** in M6: there is no Settings page yet.
- Rotating `console.webhooks.encryptionKeySecret` does not re-seal existing
  rows. An admin must `PUT` each endpoint with a fresh secret afterwards.

## kconmon-ng v1.7.0

> Console release. MTR Explorer with durable path history and optional hop
> enrichment, the Time Machine global time context, and chart annotations
> (M5). Everything is off by default or read-only-additive; a 1.6.0 install
> that upgrades and changes nothing renders the same manifests.

### Added

- **MTR path history** — every MTR result the Console records is projected
  into `mtr_path_snapshots`: hop lists content-hashed per (source,
  destination) route, deduplicated at ingest, with first/last-seen and trace
  counts. `kconmon_ng_console_mtr_snapshots_total{result="new-path"}` is the
  "route changed" alerting primitive.
- **MTR Explorer** — `/mtr` three panes (destinations → path history →
  trace detail), client-side path diff between any two snapshots, a "path
  changes" timeline overlaid with the pair's loss series, per-hop RTT
  trends, and a Runner tab (gated on `runs:create`).
- **Hop enrichment** — `console.mtr.enrichment.*` (off by default): reverse
  DNS and/or MaxMind GeoLite2 ASN/City mmdb files mounted read-only at
  `/geoip` from an operator-supplied volume; resolved server-side into a
  TTL cache (`mtr_hop_enrichment`), air-gap friendly, per-source
  degradation.
- **Time Machine** — a top-bar control and shareable `?at=` URL state:
  topology reconstructed from `topology_events` up to `t` (`GET
  /api/v1/topology?at=`), PromQL surfaces evaluated at `t`, the Live feed
  becomes scrollback, and every mutating control is disabled behind the
  banner while engaged. Note: today's controller does not yet attribute
  `topology_changed` events to nodes, so historical topology reconstruction
  reports honestly-empty results until it does (named deferral).
- **Annotations** — notes pinned to instants or time ranges
  (`/api/v1/annotations`), rendered as chart markers on Explore and the
  object cards and inline in the Live scrollback. `annotations:write` stops
  at operator/admin; reads are telemetry (every role).
- **Explore A/B** — a Compare panel: a second curated metric on the same
  axes, or the same metric time-shifted (1h/24h/7d) against itself.
- **Permissions** — `mtr:read`, `annotations:read` (all built-in roles) and
  `annotations:write` (operator/admin).

### Chart

- 1.6.0 → 1.7.0. New value blocks: `console.mtr.enrichment.*` (rendered
  into the console ConfigMap only when enabled; mmdb volume via an opaque
  `geoip.volume` VolumeSource passthrough with a render-time guard). An
  eighth ci profile (`console-mtr-values.yaml`). Default render is
  key-identical to 1.6.0.

## kconmon-ng v1.6.0

> Console release. External probe targets, saved check definitions and
> schedules, continuous external checks with agent-side CIDR enforcement,
> and diagnostics v2 (M4). Off by default throughout: without
> `console.scheduler.enabled`, `config.checkers.external.enabled` or a
> database, a 1.5.0 install behaves identically.

### Added

- **Targets, checks, schedules** — CRUD `/api/v1/{targets,checks,schedules}`
  (PostgreSQL-backed, 503 without a database), with a cardinality projection
  guard: an enabled definition may project at most 400 series, enforced
  server-side and mirrored live in the UI.
- **Console scheduler** — `console.scheduler.{enabled,tickInterval}` (off by
  default): `once`/`interval` schedules fire diagnostics runs from exactly
  one replica under a PostgreSQL advisory lock; a stuck-run reaper finishes
  abandoned runs as `cancelled`; `POST /api/v1/runs/{id}/cancel` cancels an
  in-flight run.
- **Continuous external checks** — check definitions of kind `continuous`
  are reconciled to the controller (`PUT /api/v1/external-checks`,
  `WatchExternalChecks` stream) and probed by agents on their own cadence.
  The **agent** is authoritative: `config.checkers.external.enabled` plus a
  mandatory `allowedCidrs` allowlist (deny-wins, resolved-IP matching,
  re-checked on every probe); an agent without the opt-in refuses external
  work and the controller answers 501 for it.
- **External metrics** — the `kconmon_ng_external_*` family
  (`{source_node,source_zone,target,target_kind}` labels; the target NAME,
  never an address) plus `ExternalChecksFailing` in the default rules.
- **Diagnostics v2** — `POST /api/v1/runs` accepts
  `destinationKind=node|target|adhoc`; the diagnostics form grows a
  destination selector and Save-as-definition.
- **Rate limits** — `console.rateLimit.{runsPerMinute,loginPerMinute}`
  (fixed-window on the shared KV; fail-open on a Valkey outage; the login
  limit runs before argon2id).
- **OpenAPI** — a committed spec (`docs/console-api.yaml`), generated TS
  types, and a router-walking test that fails on any drift in either
  direction.
- **UI** — the Targets & Schedules page and the Target card
  (`/targets/{id}`).

### Chart

- 1.5.0 → 1.6.0. New value blocks: `config.checkers.external.*` (agent,
  rendered only when enabled), `console.scheduler.*`,
  `console.rateLimit.*`; a seventh ci profile; the external-mode Valkey
  egress NetworkPolicy now derives its port from the configured address.

## kconmon-ng v1.5.0

> Console release. Adds durable persistence, authentication/RBAC, and an
> on-demand diagnostics runner (M3) on top of the M1/M2 Console. Off by
> default (`console.enabled: false`, `console.database.mode: disabled`,
> `console.auth.mode: anonymous`); existing installs — including existing
> Console installs on `anonymous` auth — are functionally unaffected until
> you turn these on.

### Added

- **PostgreSQL persistence** — `console.database.mode=cnpg|external|disabled`
  (ADR-001): CloudNativePG-provisioned or externally-supplied PostgreSQL for
  event history, RBAC, audit, and diagnostics run history. `GET /api/v1/events`
  now serves durable scrollback for the Live page, backed by `topology_events`
  (all five WebSocket event types, not only topology ones).
- **Authentication and RBAC** — `console.auth.mode=anonymous|local|header|oidc`:
  local users (PostgreSQL, argon2id), header-based trusted-proxy auth (explicit
  CIDR opt-in), and OIDC (code flow + PKCE). Built-in roles
  (`viewer`/`operator`/`alert-editor`/`admin`) are compiled-in and work with
  `database.mode=disabled`; a custom-role admin API layers on top when a
  database is configured. `__Host-` session cookies, CSRF double-submit for
  cookie-authenticated mutations, and an async best-effort audit log
  (`GET /api/v1/audit`).
- **API tokens (PATs)** — `/api/v1/tokens`: SHA-256-hashed bearer tokens that
  work in every auth mode. A PAT is **not** individually scoped in M3: its
  effective permissions are exactly `auth.defaultRole`, deployment-wide — a
  token subject resolves no role bindings of its own, and token-kind bindings
  are rejected by the RBAC API by design (they would silently grant nothing).
  Where a database is configured, disabling a **local** user's account revokes
  the tokens they own on that user's next request; subjects created by header
  or OIDC mode have no `users` row, so their disable state lives upstream at
  the proxy/IdP and only `DELETE /api/v1/tokens/{id}` revokes their tokens.
- **Diagnostics runner** — `POST`/`GET /api/v1/runs`, `GET /api/v1/runs/{id}`:
  bounded on-demand check fan-out (up to 400 pairs) with persisted run history
  and shareable permalinks. Live per-pair progress streams over a new
  ephemeral `run:{id}` WebSocket topic, opened per run, with an automatic
  REST-polling fallback on any console replica other than the one executing
  the run.
- **Object cards v1** — Node and Pair cards with a shared "Recent changes"
  event rail, linked from Topology and the Matrix heatmap.
- **NetworkPolicy** — opens console↔database (CNPG pod-selector rule, or a
  namespace-wide default for `mode=external`) and console→OIDC IdP on both
  layers, alongside the existing console→controller/Valkey rules.

### Fixed

- Chart mounts every console secret (database DSN, local-admin bootstrap
  password, OIDC client secret) under one sibling directory,
  `/etc/kconmon-ng-console-secrets/`, group-readable (`0440`) with
  `console.podSecurityContext.fsGroup` matching the distroless nonroot gid —
  the milestone's originally planned nested path and owner-only mode were
  both unworkable (a read-only ConfigMap volume cannot host a mountpoint
  inside itself, and `0400` is unreadable by the nonroot process without
  matching UID ownership).

### Upgrade Notes

1. This release is safe to roll out with every new feature left at its
   default (`console.database.mode: disabled`, `console.auth.mode: anonymous`):
   the M1/M2 surface behaves identically.
2. **Every existing Console install rolls once on upgrade, even one that
   changes nothing in `values.yaml`.** `auth.defaultRole` and the
   `auth.session.*` block are now rendered into the console ConfigMap
   unconditionally (previously absent keys), so the ConfigMap's content —
   and therefore its checksum annotation on the Deployment's pod template —
   changes for every install, triggering one rollout. There is no data or
   config migration behind it; it is a one-time restart.
3. To turn on persistence and auth, set `console.database.mode` to `cnpg` (this
   chart does **not** install the CloudNativePG operator or its CRDs —
   install those first, or `helm install` fails with a clear error) or
   `external` (supply `console.database.existingSecret`), then set
   `console.auth.mode` and its mode-specific block. See the chart README and
   the commented `values.yaml` for the full validation matrix and
   secret-mount layout.

### Install

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.5.0 \
  --namespace kconmon-ng \
  --create-namespace
```

### Images

```
ghcr.io/esdmitrii/kconmon-ng-agent:1.5.0
ghcr.io/esdmitrii/kconmon-ng-controller:1.5.0
ghcr.io/esdmitrii/kconmon-ng-console:1.5.0
```

---

## kconmon-ng v1.4.0

> Console release. Adds an optional read-only web Console (M1) with a realtime
> event pipeline (M2). The Console is off by default (`console.enabled: false`);
> existing agent/controller installs are unaffected until it is turned on.

### Added

- **Console web UI** — new `kconmon-ng-console` binary and image with an embedded
  SPA: Overview, Matrix, Topology, Explore, PromQL console and a Live event feed.
  Read-only in this release (anonymous viewer role, banner shown in the UI).
- **Realtime pipeline** — the controller exposes a leader-gated
  `EventStream.WatchEvents` gRPC stream (`controller.events.enabled`); the console
  ingests it and fans events out to browsers over WebSocket (`/ws`) with per-topic
  sequencing, snapshot replay and duplicate suppression. The matrix switches from
  polling to push when the stream is healthy and falls back to REST automatically.
- **Cross-replica fan-out via Valkey** — `console.valkey.mode` supports
  `off` (in-process, single replica), `bundled` (ephemeral Valkey Deployment, no
  PVC by design) and `external`. NetworkPolicies open console→controller gRPC and
  console→Valkey on both sides.
- **Chart** — new `console.*` values (deployment, service, ingress, PDB,
  NetworkPolicy, bundled Valkey), documented in `docs/configuration.md` and
  covered by `values.schema.json` and a `ci/console-values.yaml` lint profile.

### Fixed

- **Controller graceful shutdown hang with active streaming subscribers** —
  `WatchEvents`/`WatchTasks`/`WatchPeers` handlers now terminate on shutdown and
  `GracefulStop` is bounded with a hard-stop fallback (the Tasks/Peers case was
  latent since v1.3.0, masked by Kubernetes killing the pod after the grace
  period).
- **Fleet-safe events config** — the `events` key is omitted from the controller
  ConfigMap when disabled, so controller images without M2 support keep starting
  under strict config parsing.

### Security

- grpc-go bumped to v1.82.1 (GO-2026-6061, reachable via the event stream).

### Install

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.4.0 \
  --namespace kconmon-ng \
  --create-namespace
```

kubectl plugin (via krew, from the release manifest):

```bash
kubectl krew install --manifest-url \
  https://github.com/EsDmitrii/kconmon-ng/releases/download/v1.4.0/kconmon.yaml
```

### Images

```
ghcr.io/esdmitrii/kconmon-ng-agent:1.4.0
ghcr.io/esdmitrii/kconmon-ng-controller:1.4.0
ghcr.io/esdmitrii/kconmon-ng-console:1.4.0
```

---

## kconmon-ng v1.3.3

> Chart-focused release. The Go agent/controller code is unchanged from v1.3.2;
> the `:1.3.3` images are a version-synchronized rebuild (the release tag drives
> both the chart version and the image tag).

### Fixes

- **ICMP checker on runtimes with a closed `net.ipv4.ping_group_range`** — the ICMP
  checker opens an unprivileged ICMP "ping" socket (`SOCK_DGRAM`), which the kernel
  gates on `net.ipv4.ping_group_range`, not on `NET_RAW`. Some container runtimes
  leave this at the closed kernel default (`1 0`), so the checker failed with
  `socket: permission denied` on those nodes. The agent Pod now sets the safe,
  namespaced sysctl `net.ipv4.ping_group_range=0 2147483647`, so ping sockets work
  regardless of the runtime default.

### Chart

- New `agent.podSecurityContext` value exposes the agent Pod-level `securityContext`
  (defaults to opening `ping_group_range` for the ICMP checker). Set
  `agent.podSecurityContext: {}` to opt out. Documented in the chart README and
  `values.schema.json`.
- `values.schema.json`: HTTP target field corrected from `expectedStatus` to
  `expectStatus` to match the checker's config (the schema key never matched the
  code, so a schema-guided value was silently ignored).

### Install

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.3.3 \
  --namespace kconmon-ng \
  --create-namespace
```

kubectl plugin (via krew, from the release manifest):

```bash
kubectl krew install --manifest-url \
  https://github.com/EsDmitrii/kconmon-ng/releases/download/v1.3.3/kconmon.yaml
```

### Images

```
ghcr.io/esdmitrii/kconmon-ng-agent:1.3.3
ghcr.io/esdmitrii/kconmon-ng-controller:1.3.3
```

---

## kconmon-ng v1.3.2

> Note: this is the first fully working release of the on-demand diagnostics
> feature set. v1.3.0 was aborted mid-release (GitHub immutable releases sealed
> it before all assets were attached); v1.3.1 published but shipped a krew
> manifest with an invalid version string, so `kubectl krew install
> --manifest-url` rejected it. v1.3.2 carries the same content with a valid
> krew manifest. The v1.3.0/v1.3.1 tags are retired.

### Features

- **`kubectl-kconmon` plugin (on-demand diagnostics)** — a new kubectl plugin talks to the
  controller's HTTP API through a client-go port-forward, so operators can inspect topology
  (`kubectl kconmon topology` / `agents`) and run one-shot connectivity checks
  (`kubectl kconmon check SRC DST --type …`, `kubectl kconmon mtr SRC DST`) between any two nodes
  without opening Grafana. Table or `-o json` output; a failed check exits `2` (distinct from `1`
  for CLI/API errors) so it composes in shell pipelines. Install via krew from the release manifest
  (see Install below).

- **On-demand diagnostics API** — new `POST /api/v1/diagnostics` controller endpoint runs a single
  check (`tcp`/`udp`/`icmp`/`dns`/`http`/`mtr`) from a source node's agent to a destination and
  returns the `CheckResult` verbatim. Served by the leader only; `?timeout=` caps the wait
  (default 60s, max 120s). This is the endpoint the plugin drives. See `docs/api.md`.

- **Graceful agent deregistration on SIGTERM** — a restarting agent now deregisters from the
  controller on shutdown, so peers drop it immediately instead of waiting out the heartbeat TTL.
  This removes the transient false-loss window that a rolling agent restart used to leave in its
  own metrics.

### Security

- **Toolchain and dependency bumps** — Go toolchain `go1.26.4`; `google.golang.org/grpc`
  1.79.1 → 1.82.0, `golang.org/x/net` 0.51 → 0.56, `golang.org/x/sys` 0.41 → 0.46, and OpenTelemetry
  1.41 → 1.44. This clears the CVE findings behind the previous Artifact Hub security-report grade.
- **`govulncheck` in CI** — a dedicated CI job runs `govulncheck ./...` on every PR and tag;
  Dependabot (gomod / github-actions / docker, weekly) keeps dependencies current so CVE fixes land
  as normal PRs instead of accumulating until the next scan.

### Supply chain

- The Helm chart is now signed with cosign (keyless, by digest) — v1.3.2 is the first signed
  release. Artifact Hub repository metadata continues to be published as an ORAS artifact.

### Docs

- README reworked with an "On-demand diagnostics (kubectl plugin)" section and real command output.
- `docs/api.md` documents the full `POST /api/v1/diagnostics` contract (request fields, status
  codes, `?timeout=` cap, and ICMP / MTR response examples).

### Install

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.3.2 \
  --namespace kconmon-ng \
  --create-namespace
```

kubectl plugin (via krew, from the release manifest):

```bash
kubectl krew install --manifest-url \
  https://github.com/EsDmitrii/kconmon-ng/releases/download/v1.3.2/kconmon.yaml
```

### Images

```
ghcr.io/esdmitrii/kconmon-ng-agent:1.3.2
ghcr.io/esdmitrii/kconmon-ng-controller:1.3.2
```

---

## kconmon-ng v1.2.0

### Features

- **Automatic zone discovery** — agents no longer need a statically configured zone. The
  controller enriches each agent registration with the node's failure-domain zone taken from its
  node informer (`failureDomainLabel`, default `topology.kubernetes.io/zone`) and returns the
  resolved metadata in `RegisterResponse.agent`; the agent adopts it for all `source_zone` /
  `destination_zone` metric labels. `KCONMON_NG_ZONE` (`agent.zone` in Helm values) remains an
  explicit override and always wins. Node zone relabels propagate to peers via a FULL_SYNC peer
  update; the relabeled node's own `source_zone` refreshes on its next re-registration.
  Per-zone metrics and the Zone Heatmap dashboard now work out of the box on multi-zone clusters.

- **Self-monitoring** — new gauge `kconmon_ng_controller_expected_agents` (count of schedulable
  nodes from the controller's node informer) and two PrometheusRule alerts:
  `KconmonAgentsMissing` (warning: registered < expected for 10m) and `KconmonControllerDown`
  (critical: `absent(kconmon_ng_controller_leader == 1)` for 5m). Degradation of kconmon-ng
  itself now alerts instead of failing silently. Requires `controller.leaderElection: true`
  (default) for the node informer.

### Breaking-ish Changes

- **Strict config parsing** — the application config (ConfigMap / `--config` file) is now decoded
  with unknown-field rejection and per-checker semantic validation (intervals/timeouts > 0 for
  enabled checkers, HTTP target URL scheme/host, DNS resolver host[:port], non-empty DNS hosts).
  A typo'd or invalid config now fails startup and is rejected on hot-reload (the previous config
  stays active) instead of being silently ignored. Review your values overrides before upgrading:
  a config that previously "worked" by accident will now fail loudly. `timeout >= interval` logs
  a warning but does not fail.

### Helm Chart / Artifact Hub

- Chart README is now packaged into the chart archive — the Artifact Hub package page renders
  description, install instructions, values and metrics reference instead of
  "This package version does not provide a README file".
- `home` and `sources` added to `Chart.yaml`; Artifact Hub repository metadata
  (`artifacthub-repo.yml`) is published as an ORAS artifact on release for repository
  verification.
- `agent.zone` is now documented as an optional override (auto-discovery is the default).

### Dashboards

- **Overview / MTR Triggers Count** — switched from `increase(...[$__range])` to a plain
  `sum(...)`: `increase()` misses counter births on freshly restarted agent pods and chronically
  undercounted exactly when MTR fires most (pod churn).

### Local Development

- `hack/local-test.sh` hardening: unique image tag per build (minikube's image-load cache
  silently kept stale same-tag images on re-runs), `set -e`/`pipefail` fixes (`((ok++))`
  pre-increment exit, SIGPIPE on `head`-truncated pipes), port-forward cleanup.

### Upgrade Notes

1. Validate your config overrides against the stricter parser before rolling out (a quick check:
   `helm template ... | <render your config>` and run the controller/agent with `--config` locally,
   or just watch pod readiness on a staging cluster first).
2. If you previously set `agent.zone` to force a zone, you can keep it (it still wins) or drop it
   to switch to automatic discovery.
3. Metric label sets are unchanged; the new alerts ship in the chart's default
   `prometheusRule.rules` and are inert unless `prometheusRule.enabled: true`.

### Install

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.2.0 \
  --namespace kconmon-ng \
  --create-namespace
```

### Images

```
ghcr.io/esdmitrii/kconmon-ng-agent:1.2.0
ghcr.io/esdmitrii/kconmon-ng-controller:1.2.0
```

---

## kconmon-ng v1.1.0

### Bug Fixes

- **MTR memory leak** — `lastRun` map in `MTRChecker` could grow unboundedly in long-running agents
  on large clusters where node pairs come and go. Expired entries are now purged inline on each
  `TryAcquire` call while the lock is already held, keeping the map size proportional to active
  pairs within the current cooldown window.

- **HTTP body pattern mismatch counted as success** — when a `bodyPattern` check failed, the
  checker set `StatusCode = -1`, which was not caught by the result handler's `>= 400` guard and
  was silently recorded as `result="success"` in Prometheus. The status code field now always
  carries the real HTTP status. A dedicated `BodyMismatch bool` field signals pattern failure, and
  the result handler correctly marks such checks as `result="fail"`.

### Improvements

- **Configurable DNS resolver dial timeout** — the dialer timeout for custom DNS resolvers was
  previously hard-coded to 5 seconds and could not be adjusted for slow or distant resolvers.
  A new `timeout` field has been added to the DNS checker config (default: `5s`). Update your
  Helm values or config file to override:
  ```yaml
  checkers:
    dns:
      timeout: 3s
  ```

- **Jitter in agent re-registration backoff** — when the controller restarts, all agents
  previously retried at exactly the same interval, causing a thundering herd. Up to 25% random
  jitter is now added to each retry wait, spreading reconnect load across agents.

- **MTR buffer allocation** — the 1500-byte read buffer in the traceroute loop was allocated
  once per hop. It is now allocated once per trace, reducing GC pressure under frequent MTR runs.

### Helm Chart

- `config.checkers.dns.timeout` added to `values.yaml` (default: `5s`).

### Tests

- Updated `TestHTTPCheckerBodyPatternMismatch`: verifies `BodyMismatch=true` and real HTTP status
  code instead of the former `-1` sentinel.
- Added `TestHTTPCheckerBodyPatternMatch`: verifies `BodyMismatch=false` on a successful pattern.
- Added `TestDNSCheckerTimeoutPropagated`: verifies the configured timeout is stored on the checker.
- Added `TestMTRCheckerExpiredEntriesPurged`: verifies stale entries are removed from `lastRun`
  after cooldown expiry.

### Upgrade Notes

The `HTTPDetails.StatusCode` field no longer returns `-1` for body pattern mismatches — it now
always holds the actual HTTP response status code. If you have alerting or dashboards that rely
on `statusCode == -1` to detect body mismatch failures, update them to use the new
`bodyMismatch` field in the JSON result or the `result="fail"` label in Prometheus metrics.

### Install

```bash
helm upgrade --install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.1.0 \
  --namespace kconmon-ng \
  --create-namespace
```

### Images

```
ghcr.io/esdmitrii/kconmon-ng-agent:1.1.0
ghcr.io/esdmitrii/kconmon-ng-controller:1.1.0
```

---

## kconmon-ng v1.0.0 — Initial Release

Kubernetes Node Connectivity Monitor, next-generation rewrite with a gRPC-based agent/controller architecture and rich observability out of the box.

### Features

**Core**
- Agent/controller architecture with gRPC streaming peer updates
- TCP, UDP, ICMP, DNS, and HTTP checkers with configurable timeouts and thresholds
- Per-node and per-zone Prometheus metrics for all check types
- Reactive MTR traceroute on check failure with per-pair cooldown
- Self-probe prevention: peers filtered by agent ID, node name, and pod IP
- Atomic gauge reset on peer topology changes to prevent stale metrics

**Scheduler**
- Pause/resume support, per-check jitter, and NodeLocal checker mode
- NodeWatcher: live Kubernetes node info exposed via `/api/v1/topology`

**Observability**
- Grafana dashboards: Overview, Node Detail, Cross-Zone Heatmap
- Helm chart with ServiceMonitor, PrometheusRule, NetworkPolicy, PDB, and RBAC

**Operations**
- Multi-arch Docker images (linux/amd64, linux/arm64) published to GHCR
- Local dev tooling: `hack/local-test.sh` with Minikube + Prometheus + Grafana stack
- Chaos testing guide with NetworkPolicy example

### Install

```bash
helm install kconmon-ng oci://ghcr.io/esdmitrii/charts/kconmon-ng \
  --version 1.0.0 \
  --namespace kconmon-ng \
  --create-namespace
```

### Images

```
ghcr.io/esdmitrii/kconmon-ng-agent:1.0.0
ghcr.io/esdmitrii/kconmon-ng-controller:1.0.0
```
