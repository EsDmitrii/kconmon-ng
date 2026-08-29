# Incidents

Investigation Mode: "one window over one scope: every source the console can read, merged into a timeline, with the
correlation rules written down rather than guessed at" — the page's own description. It answers: **what happened to
this scope, in this window, in one place?**

![Incidents](../img/console-investigate.png)

## What this page shows

The entry form commits a **scope** and a **range**:

- **Scope kind** — *Pair*, *Node*, *Target*, *Zone pair*, or *Cluster*.
- **Range** — the presets `15m` / `1h` / `6h`, or *Custom* with explicit start and end.

Everything is in the URL (`?kind=&scope=&from=&to=`) — "this page is shareable as it stands, which is also what makes
an incident permalink free". A link with unreadable parameters degrades loudly: the page says which keys it could not
read and corrects the address bar.

## Incident timeline

The centre pane merges every source the console can read into one timeline. Row kinds (badge vocabulary): fleet
**event**, **k8s** event, **audit** row, **annotation**, **maintenance** window, diagnostic **run**, MTR
**path change**, derived **threshold** crossing, and firing **alert**.

The pane is the console's most honesty-dense surface: every absent, bounded or failed source gets one line saying
which source, why it contributed nothing, and what the bound is. Examples straight from the page: audit rows are the
newest N filtered client-side because `GET /api/v1/audit` has no time or scope filter; path changes need a pair, node
or target scope because `GET /api/v1/mtr/snapshots` requires a source and destination; firing alerts are only the
rules this console manages, and only the set firing *now* — Prometheus keeps no firing history. Sources gate
individually on `events:read`, `audit:read`, `annotations:read`, `mtr:read`, `runs:read`, `maintenance:read`,
`alerts:read` and `promql:query`, and most need the database.

Timeline rows can be **pinned** (needs `incidents:write`) into a *Pinned findings* pane, each with a "why this
matters" note. Three row classes cannot be pinned — maintenance windows, threshold crossings and firing alerts — and
the pane explains why rather than hiding the control.

## Causes panel

**Likely causes** ranks candidate rows by temporal proximity to the detected onset — the first crossing of loss above
1% or RTT above twice the range median. No onset in range means nothing is ranked: "inventing an anchor is how these
panels start lying." Each candidate reads "{delta}s before the onset · weight {weight}", and the method line links to
the scoring source — four arithmetic steps, no model.

## Working an incident

The actions rail: **Run MTR now**, **Run TCP now** (both start a run via `POST /api/v1/runs`, needs `runs:create`),
**Compare in Explore** (opens [Metrics](metrics.md); its A/B slots are bound to curated metrics, so the window is
chosen there), **Export JSON**, **Save as incident**, **Create maintenance**.

**Save as incident** (needs `incidents:write`) stores the scope, window, title and notes; a zone-pair or cluster
scope saves as the *global* scope and the dialog says so. A saved incident gets a permalink —
`/investigate?incident=<id>` — and reopening it shows the incident strip: status (*Open* / *Resolved*), *Copy
permalink*, *Resolve* / *Reopen*, *Delete* (with confirm), and editable notes. Open incidents also surface on the
[Overview](overview.md).

## Deep links

- Every *investigate* affordance in the console lands here pre-scoped: Overview worst-pair and alert rows,
  [Matrix](matrix.md) cells, and the object cards' entry points.
- Incident permalinks carry only `?incident=<id>` — "the row, not the URL, decides what this page frames."

## Use it when

- A pair or node went bad and you want probes, events, config changes, route changes and alerts on one axis.
- You are handing an investigation to a colleague — copy the URL, everything travels.
- You want a durable record: save it as an incident, pin the findings, resolve it when done.

See the walkthrough: [Diagnose a slow pair](../scenarios/diagnose-a-slow-pair.md).

Verified against `web/src/pages/investigate.tsx`, `web/src/components/investigation-timeline.tsx`,
`web/src/lib/investigation-sources.ts`, `web/src/lib/i18n/dict/investigate.ts`.
