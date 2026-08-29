# Run checks

<!-- screenshot: console-run-checks.png pending post-redesign reshoot -->

On-demand diagnostics. It answers: **does this path work right now, from these nodes, with this protocol?**

## What this page shows

A run form on top, run history underneath. Starting a run needs `runs:create`; without it the form is replaced by a
card and the history stays readable.

## Starting a run

- **Check type** — `tcp`, `udp`, `icmp`, `dns`, `http`, `mtr`.
- **Duration** — *Instant* ("One probe per pair, right now.") or `1m` / `5m` / `15m` / `1h` / `6h` / `24h`. For
  interval runs the caption spells out the plan before you press **Start run**: probe cadence, run length, expected
  samples per pair, and that the run stays cancellable throughout. MTR runs plan their own slower cadence — a trace
  walks up to 30 hops in sequence.
- **Sample interval** — *Auto* or a preset; a cadence the server cannot keep is adjusted, and the form says why
  (500-samples-per-pair ceiling, or one round over this many pairs cannot finish faster).
- **Destination** — *Nodes* (node picker), *Target* (a saved [external target](scheduled-checks.md)), or *Ad-hoc*
  (a typed address). Ad-hoc labels adapt to the check type: tcp takes `host` or `host:port` (port defaults to 80),
  udp requires `host:port`, icmp and mtr take a host only, and dns/http one-off runs cannot go external at all —
  the form points you at saving a definition instead, which is the continuous external checker.
- **Sources** / **Destinations** — node pickers, with "All nodes ({count})" as the default and an *All ↔ All* reset.

The form shows a live "~{count} pairs" estimate. A run fans out to at most **400 pairs** (server-enforced on the raw
sources×destinations product); anything above is refused up front, and the form warns before you try.

**Save as definition** stores the current form as a reusable check definition — "saved enabled and probing from all
agents", editable afterwards on [Scheduled checks](scheduled-checks.md).

## Run plans

Every started run gets a permalink — `/diagnostics/runs/<id>` — showing the spec (**Type**, **Plane**, **Pairs**,
**Started**), a live summary (**Duration**, **Cadence** — planned vs measured, **Sent**, **Failed**, **Min**,
**Avg**, **p95 / max**) and a **Cancel run** button while it is in flight. The page carries a **Live** badge and
updates as results arrive.

## Reading results

The Pairs table shows one row per pair with its most recent probe ("{ok}/{total} ok"), expandable to every probe.
For MTR runs each pair row can show the recorded route ("Show the route") and open it in the
[MTR Explorer](routes-mtr.md).

**Run history** lists past runs with server-side **type** and **status** filters and a *Load older* pager. History
needs the database; without one the page says "History is not persisted". Under the
[Time Machine](time-machine.md), the history is cut to the viewed instant client-side — and the page discloses the
bound: `GET /api/v1/runs` has no time filter.

## Limits

- 400 pairs per run; 500 samples per pair; cadence floors as computed by the server's planner.
- One-off external (ad-hoc) probes: `tcp`, `udp`, `icmp`, `mtr` only.
- External destinations must also be allowed fleet-side: `checkers.external.enabled` plus `allowedCidrs`
  (see [Configuration](../configuration.md)), and the cluster must let the packet out.

## Use it when

- You want an immediate answer for one pair without waiting for the next scheduled round.
- You are bisecting a problem: same destination, different sources (or vice versa), side by side.
- You need a longer evidence window — a 15m interval run with samples per pair, cancellable when you have enough.

Verified against `web/src/pages/diagnostics.tsx`, `web/src/pages/run-detail.tsx`,
`web/src/lib/i18n/dict/diagnostics.ts`, `web/src/lib/i18n/dict/run-detail.ts`, and
`internal/console/checks/checks.go` (`maxPairs = 400`).
