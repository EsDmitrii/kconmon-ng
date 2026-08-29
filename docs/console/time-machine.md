# Time Machine

View the console at a past instant. It answers: **what did this page say at 03:12 last night?**

![Time Machine](../img/console-timemachine.png)

## The ?at= parameter

The Time Machine owns exactly one URL key: `?at=`, an RFC 3339 instant (`2026-08-29T14:32:00Z`). Every in-app link on
a time-aware page carries it, so navigating stays inside the same instant, and **any console URL with `?at=` is a
shareable snapshot address**. The parameter is strict on purpose — a malformed value degrades to Live with a console
warning, and a future instant is clamped to now — so a shared link means the same thing to the browser and to the Go
API (`time.RFC3339`).

## Entering and leaving Time Machine

The control lives in the **page header, next to the range presets** — the presets pick how long the window is, this
picks where it ends. Idle, it reads **Time Machine: Now**; engaged, it shows the viewed instant and turns amber,
and a banner appears: "You are viewing {at} — return to Live to act", with a **Return to Live** button.

Other routes in and out:

- The [command palette](command-palette.md): *Toggle Time Machine — pick a time…* and *Return to Live*.
- Pasting or following any URL with `?at=`.

While engaged, **writes are disabled everywhere**: every mutating control is disabled with the same sentence — "Time
Machine is engaged — return to Live to act." Disabled-by-time is deliberately distinct from missing-permission:
permissions *hide* controls, the Time Machine *disables* them.

## What is and is not rewound

The control is offered only on pages that actually resolve their reads through `?at=` — a page that ignores the
past never invites it. Time-aware: [Overview](overview.md), [Events](events.md), [Matrix](matrix.md),
[Topology](topology.md), [Incidents](incidents.md), [Routes · MTR](routes-mtr.md), [Run checks](run-checks.md),
[Metrics](metrics.md), [PromQL](promql.md), and the [pair, node, target and run pages](pair-and-node-pages.md).
Not time-aware (configuration, always now): [Scheduled checks](scheduled-checks.md), [Alerting](alerting.md),
[Settings](settings.md).

Even on time-aware pages, some data cannot honestly time-travel, and each page states its own bound in place:

- **Alert firing state** is live-only — Prometheus keeps no firing history (Overview, Incidents).
- The **MTR Explorer's** route panes are live — the endpoints take no time parameter ([Routes · MTR](routes-mtr.md)).
- **Run history** is cut to the instant client-side over loaded pages — `GET /api/v1/runs` has no time filter.
- A **Topology** past view is a reconstruction from topology events, bounded by the database retention window.
- Live polling stops on every engaged page — nothing refreshes while you are in the past.

How far back you can go is bounded by the database retention (`database.retentionDays`, default 90 days) for
event-backed views, and by Prometheus's own retention for metric-backed ones.

## Use it when

- Post-mortem: open the console *as of* the incident and read every page in that moment's terms.
- An alert fired while you slept — rewind to its window, then walk forward.
- You want a before/after of a change: two tabs, two `?at=` values, same page.

Verified against `web/src/lib/timemachine.tsx`, `web/src/components/timemachine-control.tsx`,
`web/src/components/timemachine-bar.tsx`, `web/src/lib/i18n/dict/chrome.ts`,
`web/src/pages/timemachine-surfaces.test.ts` (the per-page opt-in list).
