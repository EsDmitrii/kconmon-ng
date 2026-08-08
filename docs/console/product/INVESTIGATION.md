<!--
Status: draft
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §7.6 in M0 (2026-07-14).
This document is the source of truth for Investigation Mode. Update it (and the ADRs) in the same PR as any deviation.
-->

# Investigation Mode

> **As built in M6 — read this before §7.6 below.** §7.6 is the original design
> text and is kept for the intent; these five points are what actually shipped.
>
> 1. **The three panes are assembled client-side.** There is no
>    `internal/console/investigate` package and no assembly endpoint. Every
>    source is an existing read API plus two new ones (`/api/v1/k8s-events`,
>    `/api/v1/maintenance`); the merge, the threshold detector and the ranking
>    are pure TypeScript in `web/src/lib/investigation.ts` over
>    `web/src/lib/investigation-sources.ts` (M6 Decision 1 — a server-side
>    assembler would re-expose five APIs behind a sixth for no authority gain).
> 2. **The alert row ships empty, and says so.** Nothing evaluates rules until
>    M7, so the page carries a permanent line — *"Alert state arrives with
>    alerting (M7)… That is a missing engine, not a quiet fleet"* — rather than
>    an absence the operator has to interpret (M6 Decision 12).
> 3. **DNS-resolution changes were never built.** They have no source: nothing
>    in the fleet records a resolution result over time, so there is no table,
>    no API and no timeline row. The bullet in §7.6 below is design intent, not
>    a shipped feature (MILESTONES.md "Deferred out of M6").
> 4. **K8s events come from `kubectx`**, off by default
>    (`console.kubernetesContext.enabled`), filtered to nodes in the fleet
>    topology and pods in one namespace, and read back under `events:read` —
>    they are events, and they got no permission of their own.
> 5. **Every source is permission-gated with ZERO requests when denied**, and
>    each absent or bounded one contributes one muted line to the source list
>    instead of blanking the page. Two sources are bounded even when granted
>    (the audit scan and the run scan read a newest-N page and filter client-
>    side, because neither endpoint has the filter the timeline wants) — the
>    page states both bounds rather than implying completeness.

### 7.6 Investigation Mode (flagship)

Entry: `Investigate` page, or "Investigate" on any card/matrix cell, or the
palette. Input: **scope** (pair | node | target | zone pair | whole
cluster) + **time range** (with presets: "last hour", "around alert X").

Console assembles, in one view:

1. **Timeline** (center): merged, time-ordered events — alert fired/
   resolved (rules eval via Prometheus + optional Alertmanager API),
   metric threshold crossings (latency/loss/jitter derived from
   `query_range` with baseline deltas), MTR path changes, DNS resolution
   changes, topology events (agent restarts, node NotReady), **K8s events**
   for nodes/pods in scope (`kubectx`), config changes (audit), maintenance
   windows.
2. **Signal panels**: latency/loss charts for the scope with the timeline
   cursor synced; matrix delta (window vs baseline); MTR path diff if
   snapshots exist on both sides.
3. **Correlation (v1 = honest heuristics)**: rank by temporal proximity to
   the anomaly onset, surface "N seconds before loss started: hop #7 path
   change / node cordoned". No ML, no pretending — the ranking rules are
   documented below. *(The plan's "bucket events into 30s windows" step was
   dropped in implementation: quantizing to 30s blurs an onset the samples
   resolve more finely than that, so detection is edge-triggered on the raw
   series instead. See the next section and MILESTONES.md's M6 deviations.)*
4. **Actions rail**: Run MTR now, Run TCP/HTTP now, Compare vs 1h earlier /
   same window yesterday, Show all degradations in range, Add annotation,
   Create maintenance, Export.

An investigation can be **saved as an Incident** (`incidents`): pinned
findings, notes, status (open/resolved), shareable permalink. Incidents
appear on Overview and on related object cards. This is deliberately
lightweight incident tracking — not a ticketing system; a webhook can
notify external systems (§7.9).

#### Correlation v1 — the ranking rules (as implemented)

There is no model here. The correlation panel runs four arithmetic steps
over the merged timeline, and all four are written down below so an
operator can reproduce a ranking by hand and disagree with it on the
merits. Everything lives in `web/src/lib/investigation.ts`.

**1. Threshold crossings (`thresholdCrossings`, `DEFAULT_THRESHOLDS`).**
Two signals get derived timeline entries, with `DEFAULT_THRESHOLDS =
{ lossPct: 1, rttFactor: 2 }`:

| signal | crosses when | baseline |
| --- | --- | --- |
| packet loss | ratio **strictly above** `lossPct / 100` (1%) | fixed |
| RTT | **strictly above** `rttFactor ×` the median RTT (2×) | median of the samples **in the selected range** |

The RTT baseline is the **median**, not the mean, and deliberately so: a
mean is dragged upward by the very spike it is supposed to catch, so a
mean-based bar rises with the anomaly and can shrug it off. A median of a
mostly-healthy range stays at the healthy level however violent the
excursion. Even-length series take the mean of the two middle samples. A
median of zero produces no RTT entries at all — a zero baseline is not a
baseline, and "everything is an anomaly" is not a useful answer.

Detection is **edge-triggered, not level-triggered**: a degradation that
persists for forty samples produces **one** entry when the signal goes
above the bar and **one** `info` "recovered" entry when it comes back —
not forty rows burying the causes among them. A dip below and a second
breach produce a second crossing. A range that ends while still degraded
gets no "recovered" entry, because it has not recovered.

**2. Onset (`anomalyOnset`).** The onset is the `at` of the **earliest
threshold crossing** in the timeline. Recovery entries carry severity
`info` and are excluded — "when did it get better" is not "when did it
start". No crossing means no onset (`null`) and the panel ranks nothing
rather than inventing an anchor.

**3. Candidates (`DEFAULT_CAUSE_WINDOW_SECONDS = 300`, `CAUSE_WEIGHTS`).**
An entry is a candidate cause when all three hold:

- its class weight is greater than zero (table below);
- it happened **at or before** the onset — something that happened after
  the loss started did not start it;
- it happened **no more than 300 seconds** before the onset. Five minutes
  is long enough to catch a rollout that began before the probes noticed,
  short enough that an unrelated change an hour earlier cannot claim
  credit. An entry sitting exactly on the 300s edge is still a candidate;
  one second earlier is not.

`CAUSE_WEIGHTS`, verbatim:

| kind | weight | why |
| --- | --- | --- |
| `path-change` | 3 | the route itself moved — nothing explains a network symptom more directly |
| `k8s` | 3 | the cluster moved the infrastructure under the probe (node/pod events) |
| `event` | 2 | the fleet changed: an agent restarted, a node went NotReady |
| `audit` | 2 | the configuration changed: someone wrote to the console |
| `maintenance` | 1 | a window **explains** a degradation rather than implicating anything — ranked so the operator stops looking, but never above a real suspect |
| `annotation` | 0 | **never a cause** — a human note *about* the problem |
| `run` | 0 | **never a cause** — a probe fired *at* the problem |
| `threshold` | 0 | **never a cause** — the symptom itself; a symptom is not its own cause |

**4. Score (`rankCauses`) — linear proximity decay.** With `delta` = the
seconds between the candidate and the onset, and `window` = 300:

```
score = weight × (1 − delta / window)
```

The class weight is a ceiling reached only by an entry landing exactly on
the onset; an entry at the far edge of the window scores `0` — listed,
present, claiming nothing. The decay is linear rather than exponential
because linear is the shape an operator can check by eye against the "N
seconds before" label next to the row.

Results sort by score descending. **Ties break newest-first**, then by
kind and then by ref id, so the order is total: two people opening the
same permalink see the same ranking in the same order.

The constants in `web/src/lib/investigation.ts` are the authority; this
section restates them and drifts only by bug.
