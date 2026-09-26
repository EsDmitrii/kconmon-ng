# Scheduled checks

The configuration page for recurring probes: what the fleet probes on its own, from where, and how often. Three tabs over three object kinds, and the path through them is always target → definition → schedule.

The three objects, defined:

- A **target** names a host or URL outside the fleet. Its address is validated against its kind (`host` takes a hostname or IP, optionally with a port; `url` takes an `http(s)://` URL) and is at most 2048 bytes, the bound an ad-hoc address has too, and external checks probe what is listed here. Each target also has its own page at `/targets/<id>` (the **target card**), documented with the other [object pages](pair-and-node-pages.md#the-target-card).
- A **definition** says what the fleet probes: a check type, which agents send (the source selection), and where the probes go, whether the nodes themselves, a saved target, or an ad-hoc address.
- A **schedule** is the cadence that fires a definition: once, at an interval, or continuously on the agents. A definition without a schedule never fires on its own.

This is configuration, not telemetry. Everything here lives in the database, reading needs `checks:read` / `targets:read` (granted to the operator and admin roles, deliberately not viewer), and writing needs the corresponding write permission. Without a database the page says where to set one and requests nothing. When the console cannot read its own configuration (`GET /api/v1/config` failed), it cannot tell whether a database exists, so the page shows that failure instead, with the server's detail: "Could not read the console configuration, so targets, definitions and schedules were not requested: {error}". See [Concepts: checks, runs and schedules](../concepts/checks-runs-schedules.md) for how these objects relate to one-off runs.

A tab with nothing in it yet shows a titled slate ("No targets yet", "No check definitions yet", "No schedules yet") that says what the object is, and with the write permission the create button sits inside that slate. Once the list has rows, the button moves above it. While the list is still loading neither shows, so the button does not jump from one place to the other under the pointer.

## The definition form

<figure markdown>
![Scheduled checks, Definitions tab with the New definition form open: Name acc-billing-db-tcp-per-zone, Check type tcp, Source selection one-per-zone, Destination kind target, Destination target acc-legacy-billing-db, Plane pod, Params (JSON) with the placeholder {"port": 443}, Enabled ticked, the projection ~3 series (3 agents × 1 protocol), limit 400, Create definition and Cancel, and the Check definitions list below starting with acc-hooks-receiver-mtr](../img/console-scheduled-checks-definitions.png){ loading=lazy }
<figcaption>The definition form, open above the list of existing definitions: name, check type, source selection, destination kind and target, the fixed <em>pod</em> plane, params, Enabled, and the projection guard reading "~3 series (3 agents × 1 protocol), limit 400", one agent for each of the stand's three zones.</figcaption>
</figure>

Fields: *Name*, *Check type*, *Source selection*, *Destination kind* (`node` / `target` / `adhoc`), *Plane*, *Params (JSON)*, *Enabled*. You can also seed one from a filled-in [Run checks](run-checks.md) form via **Save as definition**.

**Source selection** decides which agents run the check, and the three values are not three sizes:

| Value | Agents that probe | Series cost |
| --- | --- | --- |
| `all` | Every agent | One per agent |
| `per-zone` | Every agent, grouped by zone | Same as `all`; grouping shrinks nothing |
| `one-per-zone` | The first agent (sorted by node name) in each zone | One per zone |

Only `one-per-zone` reduces the count, which is why the form defaults to it.

**Plane** is a fixed field, not a choice: definitions probe from the pod network, and this release ships no second plane, so the form states the scope instead of offering an option. The API refuses any other value: `POST` and `PUT /api/v1/checks` answer 422 `invalid check definition` (`definition: plane must be "pod": agents probe the pod network only`).

**Params (JSON)** is interpreted by the agent per check type, and unknown keys are warned about and ignored rather than failing the probe (malformed JSON, on the other hand, gets the whole spec rejected at assignment, and rejections are counted in an agent metric so a definition every agent refuses is visible without reading pod logs). Params over 4096 bytes are refused at save time with 422 (`definition: params is N bytes, limit is 4096`), since every agent's assignment carries a copy:

| Check type | Params |
| --- | --- |
| `tcp`, `icmp` | None accepted; leave it empty |
| `dns` | `{"query": "<name to resolve>"}`; required, non-empty |
| `http` | `{"method": "GET"\|"HEAD", "expectStatus": <100–599>, "insecureSkipVerify": <bool>}`; all optional, method defaults to GET |

Two check types never run toward a target or ad-hoc destination, and the API refuses such a definition at write time. `udp` probes kconmon nodes only: the probe counts a reply only when the far end echoes its sequence number, which only an agent does, so `POST` and `PUT /api/v1/checks` answer 422 `check definition cannot run` and an import reports it per item. `pmtu` is refused for any destination kind other than `node`: it speaks the kconmon echo protocol, which only an agent answers. The form offers neither combination: with *Target* or *Ad-hoc* chosen, `udp` and `pmtu` are greyed out in *Check type*, and with either type chosen, *Destination kind* offers only `node` and a hint under *Check type* says why. A `udp` definition toward a target or ad-hoc address that is already stored can still be paused by unticking *Enabled*, as the API allows; a `pmtu` one cannot be saved at all. `mtr` toward a target or ad-hoc destination saves, but only for `once` and `interval` schedules (see [below](#schedules)).

**The projection guard.** Before an enabled definition is saved, the form projects its metric cost against the live topology: "~{series} series ({agents} agents × {protocols} protocols)". Agents is what the source selection resolves to right now; protocols is 1 today, since a definition names exactly one check type. The bound is **400 projected series per definition**, the same number that caps one run's fan-out, and a projection above it refuses to save enabled, suggesting `one-per-zone` or saving disabled. Saving disabled is the deliberate escape hatch, not a loophole. When the server refuses a submit on any of the three forms, focus moves to the refused field, or to the submit button when the refusal names no field. A few refusals never reach the server: a blank name on a target or a definition gets "A name is required.", a schedule with no definition picked gets "Pick a definition to schedule.", and a `once` schedule with no *Run at* gets "Pick when the run happens." Because the projection is computed against the live topology, the endpoint answers 503 when `console.controller.url` is not set, and the same definition can project differently as the cluster scales.

## Schedules

<figure markdown>
![Schedules tab with five rows: acc-hooks-receiver-mtr once at 9/26/2026 10:00:00, enabled; acc-hooks-receiver-tcp and acc-legacy-billing-db-tcp continuous, enabled; acc-hooks-receiver-tcp every 5m, enabled, with next 06:06:08 and last 06:01:08 stamps; acc-legacy-billing-db-tcp every 15m, disabled](../img/console-scheduled-checks-schedules.png){ loading=lazy }
<figcaption>Schedule rows: a <em>once</em> schedule with its next stamp, two continuous schedules, which have neither stamp, an enabled interval schedule with its next and last stamps, and a disabled interval schedule. Every row is a combination the API accepts toward a target: <code>mtr</code> once, <code>tcp</code> continuous and <code>tcp</code> every 5m or 15m.</figcaption>
</figure>

Three kinds:

- `once` fires at *Run at*, which must be in the future.
- `interval` fires every N seconds. A value below the floor of **10 seconds** is clamped up server-side rather than rejected; the form says so in advance.[^ceiling]
- `continuous` is pushed to the agents and runs there. No interval, no run-at, and no dependency on the scheduler loop.

A schedule belongs to its definition permanently: to point a cadence at a different definition, create a new schedule. The form states this next to the fixed definition field. *Kind* offers only the kinds that can run the chosen definition (the table below) and says why the others are missing; a stored `udp` or `pmtu` definition toward a target or ad-hoc destination leaves no kind at all, and the form will not save a schedule for it until the definition points at nodes.

Toward a target or ad-hoc destination the kind decides which check types can run, because a `continuous` schedule becomes an external check on the agents and the other two fire one-off runs:

| Kind | Check types toward a target or ad-hoc destination |
| --- | --- |
| `continuous` | `tcp`, `icmp`, `dns`, `http` |
| `once`, `interval` | `tcp`, `icmp`, `mtr`, the set a one-off [run](run-checks.md) takes |

`POST /api/v1/schedules` and `PUT /api/v1/schedules/{id}` refuse any other combination with 422 `invalid schedule`, for example `schedule: check type http cannot run toward destination kind "target" as a one-off or repeating run: the agents run only tcp, icmp and mtr checks toward one`. A node destination is not judged. `PUT /api/v1/checks/{id}` refuses an edit of the check type or destination kind that would leave one of the definition's own schedules unable to run it, with 422 `check definition cannot run` and `definition: its interval schedule could not run the edited check: ...; change or delete that schedule first`. A schedule of that kind saved before 2.5.0 starts no run: when it comes due its row shows *failing* with `check type http cannot run toward destination kind "target" as a one-off or repeating run: the agents run only tcp, icmp and mtr checks toward one, so no run was started`, the API's refusal plus its last clause, with a *last* stamp. An `interval` schedule records this at each due time; a `once` schedule records it once and retires rather than coming due again every minute. A stored definition with a plane other than `pod` fails its schedules the same way, with the plane refusal as the message. Disable such a schedule from its row, or delete it; a `dns` or `http` definition toward a target runs as a `continuous` schedule instead. With `"enabled": false` neither `PUT /api/v1/schedules/{id}` nor `PUT /api/v1/checks/{id}` applies the runnability check, so a pre-2.5.0 row can always be disabled; enabling it again is judged, and so is every create. A definition edit is still held to its schedules either way.

`once` and `interval` schedules are fired by the **console scheduler loop**, and that loop is **off by default** (`console.scheduler.enabled`, tick every 5 s when on; see the [Helm values](../reference/helm-values.md)), so that a chart upgrade can never start dispatching fleet traffic by itself.

!!! warning "Schedules that will never fire"
    With the scheduler loop disabled, the page banners: "These schedules will not fire: the scheduler loop is disabled on this install (console.scheduler.enabled). Continuous schedules are unaffected — they run on the agents."

Each schedule row shows its state (*enabled*, *disabled*, or *paused: definition disabled*) with *next {at}* / *last {at}* stamps; a disabled or paused schedule reads *next —*, since nothing is due. The cadence keeps its own column with its full text as a tooltip, so a long definition name never cuts it short. Paused means the schedule is on but its definition is switched off: nothing fires, the cadence keeps its place, and firing resumes when the definition is re-enabled. When the last fire failed, the row carries the scheduler's own recorded message verbatim ("failing: {message}"), and it stays visible even if the schedule is later disabled, because switching a cadence off does not unmake the failure.

[^ceiling]: There is also a ceiling of roughly 104 days. The box takes seconds and the wire takes nanoseconds; past that product a JavaScript number can no longer represent the value exactly, and the form refuses to store a cadence nobody asked for.

## Where the results land

Fired runs appear in [run history](run-checks.md#run-history) and their series in [Metrics](metrics.md). A target's own card collects everything about that target (the definitions probing it, its probe-duration series, the runs against it) and is documented in [Pair, node and target pages](pair-and-node-pages.md#the-target-card).

External destinations additionally require the fleet-side allowlist: `checkers.external.enabled` and `allowedCidrs` (see [External targets](../scenarios/external-targets.md)). The whole declarative inventory here, targets, definitions and schedules alike, is exportable from [Settings](settings.md#configuration-export-import).

<!-- verified against: web/src/pages/targets.tsx (one-per-zone default L816, MIN_INTERVAL_SECONDS=10,
     MAX_INTERVAL_SECONDS comment, enabled-default comment), web/src/lib/i18n/dict/targets.ts (planeNote,
     definitionFixed, projection.ok/over), web/src/lib/api-types.ts L2110-2114 (SourceSelection why-comment),
     internal/console/checks/assign.go (assignedAgents, protocolsPerDefinitionMirror=1, sorted-first per zone),
     internal/console/httpapi/definitions.go (maxProjectedSeries=400, projectionUnavailableDetail 503,
     guard runs only for definitions arriving enabled), internal/console/httpapi/definitions_runnable.go
     (errUDPNodesOnly, scheduleCannotRun, scheduleBrokenByDefinitionEdit), internal/console/scheduler/
     scheduler.go specFor (no run toward target/adhoc for udp/dns/http/pmtu) + cannotRun (a once row retires with
     last_error), definitions.go/schedules.go update paths (enabled:false skips the runnability guard,
     ValidatePlane, maxDefinitionParamsBytes), web/src/pages/targets.tsx probesNodesOnly + scheduleKindsFor + the DefinitionForm unrunnable rule,
     web/src/hooks/use-focus-on-refusal.ts (useFocusOnRefusal), internal/console/store/targets.go (addressMaxLen 2048),
     internal/checker/external.go (ParseExternalSpec:
     per-type params, dns query required, http method/expectStatus/insecureSkipVerify, udp/mtr refused,
     lenient unknown keys + rejection metric via internal/agent/agent.go), charts/kconmon-ng/values.yaml
     (console.scheduler.enabled off, tickInterval 5s, checkers.external.*), web/src/pages/target-card.tsx,
     web/src/lib/api-types.ts TargetRequest (address validated against kind). -->
