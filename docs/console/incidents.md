# Incidents

Investigation Mode: one window over one scope, with every source the console can read merged into a single timeline and the correlation rules written down rather than guessed at. A pair went bad at 14:32; this page puts probes, fleet events, Kubernetes events, config changes, route changes and alerts on one axis so you can see what moved first.

<figure markdown>
![Incidents page over the pair scope kc-accept-worker3 → kc-accept-worker5, 1h range: the scope and range controls, the action row with 0 maintenance windows in this window, a Timeline of 338 entries whose newest rows are tcp diagnostic timeout and dispatched events between two audit entries, and a Signals panel with the fail ratio going from 0.0% to 100.0% and a packet-loss chart at 100% since about 06:17](../img/console-incidents-timeline.png){ loading=lazy }
<figcaption>A pair investigation mid-break: scope and range in the URL, the action row (Run MTR now, Run TCP now, Compare in Metrics, Export JSON, Save as incident, Create maintenance), a 338-entry timeline led by the pair's diagnostic timeouts, and the Signals panel stating the fail ratio rose 100 percentage points across the window.</figcaption>
</figure>

## Scope and window

The entry form commits two things:

- **Scope kind**: *Pair*, *Node*, *Target*, *Zone pair*, or *Cluster*.
- **Range**: the presets `15m` / `1h` / `6h`, or *Custom* with explicit start and end.

Everything lands in the URL (`?kind=&scope=&from=&to=`), which is what makes an investigation shareable and an incident permalink free: copy the address, everything travels. The note under the form says only that; the parameter names are listed here. A link with unreadable parameters degrades loudly: the page names the keys it could not read and corrects the address bar.

## The timeline and its sources

The centre pane merges every readable source. Row badges: fleet **event**, **k8s** event, **audit** row, **annotation**, **maintenance** window, diagnostic **run**, MTR **path change**, derived **threshold** crossing, and firing **alert**.

Every absent, bounded or failed source gets one disclosure line naming the source, why it contributed nothing, and where the answer is bounded rather than absent, exactly what the bound is. The bounds, with their numbers:

- **Audit rows** are the newest **200** fetched and filtered client-side. `GET /api/v1/audit` has no time filter, so a busy console can push older in-range rows off that page, and there is no scope filter either, so these rows are every subject's requests, not just this pair's.
- **Fleet events** and **Kubernetes events** are fetched up to 200 per source for the window.
- **Runs** come from a client-side scan of the 20 most recent, the same bound the [object pages](pair-and-node-pages.md) disclose.
- **Path changes** need a pair, node or target scope, because `GET /api/v1/mtr/snapshots` requires a source and destination.
- **Firing alerts** are only the rules this console manages, and only the set firing *now*, since Prometheus keeps no firing history.

Sources gate individually on `events:read`, `audit:read`, `annotations:read`, `mtr:read`, `runs:read`, `maintenance:read`, `alerts:read` and `promql:query`, and most need the database. Without one, a single line says so and none of the stored sources is requested. When the console cannot read its own configuration (`GET /api/v1/config` failed), that line reads "Could not read the console configuration, so none of the stored sources was requested: {error}" instead.

**Where the k8s rows come from.** The console runs its own watcher that captures cluster events into the database and serves them back through `GET /api/v1/k8s-events`. It is off by default (`console.kubernetesContext.enabled` in the Helm values), because enabling it adds apiserver egress and an RBAC grant; it watches one namespace (empty means the release's own) with a 10-minute relist backstop against a silently wedged watch.

## Pinned findings and notes

Timeline rows can be **pinned** (needs `incidents:write`) into a *Pinned findings* pane, each with a "why this matters" note and the timeline's own badge word for its kind (a pinned route change reads *path change*). Three row classes cannot be pinned (maintenance windows, threshold crossings and firing alerts), and the pane explains why instead of hiding the control.

Below the causes panel sits a separate **Notes on this scope** card. It is the annotation surface for the investigated scope, frozen to the investigation window: the notes operators dropped on this object during this period, with the same *＋ annotate* control the [Metrics](metrics.md#annotations-and-maintenance-windows) page carries. Pinned findings belong to an incident; these notes belong to the scope.

## Likely causes

The panel ranks candidate rows by how close they sit before the detected onset. The method is four arithmetic steps, no model, and the panel links its own scoring source:

1. **Onset** is the earliest non-info threshold crossing in the window: loss above 1%, or RTT above twice the range median. No onset in range means nothing is ranked: a ranking needs an anchor, and the page will not invent one.
2. **Candidates** are rows in the **5 minutes** before the onset. Long enough to catch a rollout that started before the probes noticed; short enough that an unrelated change an hour earlier cannot claim credit.
3. **Weights** by row kind: path change 3, k8s event 3, fleet event 2, audit row 2, maintenance window 1, everything else 0. A maintenance window scores low deliberately, because it *explains* a degradation rather than implicating anyone. Read-only audit rows are excluded outright: the audit log records authorization decisions, so without that rule the console's own GETs (including the two PromQL queries this very page fires to draw its charts) would arrive as weight-2 "config changes" and out-rank the real causes. Four POST routes count as reads too, because they store nothing: the two PromQL queries and the draft previews the forms on Scheduled checks and Alerting send while you type (`POST /api/v1/checks/projection`, `POST /api/v1/alert-rules/preview`). Audit rows of requests the server refused or that failed (outcome `denied` or `error`), and of sign-in, sign-out and password changes, are not ranked either, since none of them changed the configuration; unlike read-only calls, which fold into one row, they stay in the timeline as rows of their own.
4. **Score** decays linearly across the window: `weight × (1 − delta/window)`. Linear rather than exponential because it is the shape you can verify by eye against the "{delta}s before the onset · weight {weight}" label on each candidate.

## Working an incident

The actions rail: **Run MTR now**, **Run TCP now** (both start a run via `POST /api/v1/runs`, needs `runs:create`), **Compare in Metrics** (opens [Metrics](metrics.md); its A/B slots stay bound to curated metrics, so the window is chosen there), **Export JSON**, **Save as incident**, **Create maintenance**. A maintenance window created here holds back the console's alert webhooks in its scope while it is open ([Alerting](alerting.md#maintenance-windows)). On a pair, node or target scope that is the object's alerts; a zone-pair or cluster scope creates a global window, which holds every console alert webhook, and the form warns about it.

**Export JSON** downloads the whole investigation, not just the pins: the committed parameters (kind, scope, from, to), every assembled timeline entry (timestamp, kind, severity, title, detail, reference), and the ranked causes with their scores. The filename is derived from the window start, with the ISO colons made filesystem-safe.

**Save as incident** (needs `incidents:write`) stores the scope, window, title and notes; a zone-pair or cluster scope saves as the *global* scope and the dialog says so. A saved incident gets a permalink, `/investigate?incident=<id>`, and reopening it shows the incident strip: status (*Open* / *Resolved*), who opened it ("opened by {who}", your display name when it was you, with the raw subject id on hover; anyone else's raw id, since only `users:manage` can resolve other subjects), *Copy permalink*, *Resolve* / *Reopen*, *Delete* (with confirm), and editable notes, which keep line breaks and tabs (any other control character is refused with 422). Notes typed while a save is in flight are kept, and a save that lands after you switched to another incident does not write into that incident's editor. Open incidents also surface on the [Overview](overview.md#firing-alerts-open-incidents-recent-events). An incident permalink carries only `?incident=<id>`; the stored row, not the URL, decides what the page frames.

<figure markdown>
![A saved incident opened by permalink: Cluster scope over a 1h range, the action row with one global maintenance window listed (Sep 26, 06:15 → Sep 26, 07:30), and the incident strip for acc-worker2-worker5 cut drill with Open status, opened by admin at 9/26/2026, 06:25:49, Copy permalink, Resolve, Delete and editable notes (269/16384) with Save notes, above the Pinned findings card](../img/console-incidents-permalink.png){ loading=lazy }
<figcaption>An incident reopened by its permalink: the strip carries status, sharing and lifecycle actions and the editable notes, above the investigation it froze (cluster scope, the saved one-hour window).</figcaption>
</figure>

## Saved incidents

With no incident open, the page shows a **Saved incidents** card above the entry form: every saved incident, newest first, each row linking to its permalink and showing its status, its scope (*global* for a zone-pair or cluster save) and when it was opened and resolved. The *All* / *Open* / *Resolved* filter is the `status` parameter of `GET /api/v1/incidents`. The card fetches 50 at a time: *Load older* follows the list's keyset cursor for the next 50, and the pager pages through what has been loaded. It needs `incidents:read` and the database, and says which one is missing instead of requesting. On an incident permalink the card is hidden.

Under the [Time Machine](time-machine.md) the card lists only the incidents saved by the instant on screen, each with its status at that instant: *Resolved* only if it was resolved by then. The filter then applies on the page, because the server's `status` is a fact about now.

## Getting here

Every *investigate* affordance in the console lands here pre-scoped: [Overview](overview.md)'s worst-pair and alert rows, [Matrix](matrix.md) cells, and the object pages' entry points. For a worked example, see [Diagnose a slow pair](../scenarios/diagnose-a-slow-pair.md).

<!-- verified against: web/src/pages/investigate.tsx (AUDIT_SCAN_LIMIT=200, EVENT_LIMIT=200, RUN_SCAN_LIMIT=20,
     SavedIncidents only while incidentId === null), web/src/pages/investigate-incidents.tsx (SAVED_INCIDENTS_PAGE=50,
     server status filter only while live, statusAt),
     downloadJson/exportFileName/buildExportPayload, notes card L2281), web/src/lib/investigation.ts (CAUSE_WEIGHTS,
     DEFAULT_CAUSE_WINDOW_SECONDS=300, rankCauses linear decay, readOnly and notACause exclusion, anomalyOnset),
     web/src/lib/investigation-sources.ts (source bounds, exportFileName, buildExportPayload, READ_ONLY_AUDIT_POSTS),
     web/src/lib/i18n/dict/investigate.ts (source.*, notes.*, pin gating), web/src/lib/api.ts getK8sEvents,
     internal/console/httpapi/server.go L415 (GET /api/v1/k8s-events), charts/kconmon-ng/values.yaml
     console.kubernetesContext (enabled off by default, namespace, resyncInterval 10m). -->
