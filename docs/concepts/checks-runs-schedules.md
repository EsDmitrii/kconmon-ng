# Checks, runs and schedules

Five words do the work in kconmon-ng, and they are close enough to blur. In
one sentence: a **checker** is a prober built into every agent that runs
continuously against the peer mesh; a **check** is a stored definition of a
probe you declared in the Console; a **target** is the named external
destination a check can point at; a **schedule** decides *when* a check
executes; and a **run** is one recorded execution with a status and results.

Everything below unpacks that sentence.

## Checks

Two layers, configured in two places:

- **Built-in checkers** live in the agents and are configured through Helm
  (`config.checkers.*`): TCP, UDP, ICMP and DNS run out of the box on a 5s
  interval against every peer; HTTP is opt-in because it needs URLs only you
  know; MTR fires reactively on probe failure. They need no database and no
  Console — this is the measurement core described in
  [Architecture](architecture.md).
- **Check definitions** are Console objects (stored in PostgreSQL, managed on
  the [Scheduled checks](../console/scheduled-checks.md) page or via
  `/api/v1/checks`). A definition says *which agents* probe (`all`,
  `per-zone`, or the default `one-per-zone`), *what* they probe (a cluster
  node, a stored **target**, or an ad-hoc address), with which check type and
  parameters. A guard bounds what one definition may fan out to — at most 400
  projected series — and the same function answers the UI's preview, so the
  number you see is the number that is enforced.

**Targets** (`/api/v1/targets`, the [external targets
scenario](../scenarios/external-targets.md)) name destinations that are not
fleet peers — a `host` or a `url` — so that metrics and events can carry a
stable operator-chosen name instead of an address. Probing them additionally
requires the agent-side external checker and its CIDR allowlist
(`config.checkers.external`).

## Runs

A run is one execution: it is created, fans out over sources × destinations
(bounded to 400 pairs), and ends in a terminal status — `succeeded`,
`failed`, `partial` or `cancelled`. Runs come from three places:

- **On demand** from the [Run checks](../console/run-checks.md) page (or
  `POST /api/v1/runs`) — probe a pair right now, mid-incident, with a
  permalink to the result.
- **From a schedule**, when the scheduler fires a due check definition.
- **From the terminal**: `kubectl kconmon check node-1 node-2 --type udp`
  drives the controller's [diagnostics endpoint](../api.md) directly — same
  probes, no Console required.

Run history (`GET /api/v1/runs`) is what the Run checks page lists, and what
the retention pruner sweeps after `database.retentionDays`.

## Schedules

A schedule attaches a cadence to one check definition: `once` (a future
timestamp), `interval` (a repeat period, clamped up to a 10s minimum), or
`continuous` — the special kind that does not create runs at all but tells the
reconciler to push the definition to agents as a continuously-executing
external check. `cron` is deliberately refused for now, with an error naming
the milestone it lands in.

Nothing fires until the schedule loop is on: it is opt-in
(`console.scheduler.enabled`, default `false`, so an upgrade never silently
starts dispatching fleet traffic) and needs both a database and a reachable
controller. With more than one console replica the loop is advisory-locked, so
exactly one replica dispatches each tick.

## Results and events

Two output streams, deliberately separate:

- **Results** are measurements. Continuous checker results become Prometheus
  series scraped from each agent; run results are stored rows the Console
  shows next to each run.
- **Events** are facts about the fleet: agents joining and leaving, topology
  changes, diagnostic runs starting and finishing. The controller streams them
  when `controller.events.enabled` is on; the Console's
  [Events](../console/events.md) page is the live feed, the ingested history
  is what the [Time Machine](../console/time-machine.md) folds topology from,
  and Kubernetes core events can be captured alongside
  (`console.kubernetesContext.enabled`) into the
  [incident timeline](../console/incidents.md).
