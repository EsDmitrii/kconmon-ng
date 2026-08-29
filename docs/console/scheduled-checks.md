# Scheduled checks

<!-- screenshot: console-scheduled-checks.png pending post-redesign reshoot -->

The configuration page for recurring probes. It answers: **what does the fleet probe on its own, from where, and how
often?**

## What this page shows

Three tabs over three object kinds, and the page's own empty states define them best:

- **Targets** — "A target names a host or URL outside the fleet, and external checks probe what is listed here."
- **Definitions** — "A definition says what the fleet probes: a check type, which agents send, and where the probes
  go — a target or the nodes themselves."
- **Schedules** — "A schedule is the cadence that fires a check definition: once, at an interval, or continuously on
  the agents. A definition without a schedule never fires on its own."

This is configuration, not telemetry: everything here lives in the database, reading needs `checks:read` /
`targets:read` (granted to the operator and admin roles, deliberately not viewer), and writing needs the
corresponding write permission. See [Concepts: checks, runs and schedules](../concepts/checks-runs-schedules.md) for
how these objects relate to one-off runs.

## Creating a schedule

The path is target → definition → schedule:

1. **New target** (Targets tab): *Name*, *Kind* (`host` / `url`), *Address*, optional *Labels* (`key=value` pairs).
   Each target also has its own [target card](pair-and-node-pages.md) at `/targets/<id>` with probe history.
2. **New definition** (Definitions tab): *Name*, *Check type*, *Source selection* (`all` / `per-zone` /
   `one-per-zone`), *Destination kind* (`node` / `target` / `adhoc`), *Params (JSON)*, *Enabled*. Before saving, the
   form projects the metric cost — "~{series} series ({agents} agents × {protocols} protocols)" — and refuses a
   projection above the series limit, suggesting `one-per-zone` or saving disabled.
3. **New schedule** (Schedules tab): pick the definition, then *Kind* — `once` (with *Run at*), `interval` (seconds;
   values below the floor are raised to it), or `continuous`.

You can also seed a definition from a filled-in [Run checks](run-checks.md) form via **Save as definition**.

## Cadence and scope

- `once` and `interval` schedules are fired by the **console scheduler loop** — off by default
  (`console.scheduler.enabled`, see [Helm values](../reference/helm-values.md)). When it is off, the page warns:
  "These schedules will not fire: the scheduler loop is disabled on this install."
- `continuous` schedules are pushed to the agents and run there — they carry no interval, no run-at, and do not
  depend on the scheduler loop.
- A schedule whose definition is disabled shows *paused: definition disabled* — the cadence keeps its place and
  resumes when the definition is re-enabled.
- External destinations additionally require the fleet-side allowlist: `checkers.external.enabled` and
  `allowedCidrs` (see [External targets](../scenarios/external-targets.md)).

## Reviewing past runs

Each schedule row shows its state (*enabled* / *disabled* / *paused*), *next {at}* / *last {at}*, and — when the last
fire failed — the scheduler's own recorded message verbatim ("failing: {message}"). Fired runs land in
[run history](run-checks.md) and their results in [Metrics](metrics.md); a target's own card collects the runs and
probe series against that target.

## Use it when

- You want standing probes to something outside the fleet — a VIP, an external DNS, a SaaS healthcheck.
- You need a recurring mesh check beyond the built-in per-protocol checkers, with an explicit metric-cost preview.
- You are auditing "what probes what": the three tabs are the complete declarative inventory, exportable from
  [Settings](settings.md#configuration-export-import).

Verified against `web/src/pages/targets.tsx`, `web/src/lib/i18n/dict/targets.ts`, `web/src/pages/target-card.tsx`
(API: `/api/v1/targets`, `/api/v1/checks`, `/api/v1/checks/projection`, `/api/v1/schedules`), and
`charts/kconmon-ng/values.yaml` (`console.scheduler.*`, `checkers.external.*`).
