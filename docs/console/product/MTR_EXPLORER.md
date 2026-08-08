<!--
Status: current
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §7.5 in M0 (2026-07-14); rewritten from
the as-built M5 implementation (2026-08-08): web/src/pages/mtr.tsx,
web/src/components/{mtr-hop-table,mtr-path-diff,mtr-changes-timeline}.tsx,
internal/console/checks/mtrproject.go, internal/console/enrich/enrich.go,
internal/console/store/migrations/00005_mtr_timemachine.sql.
This document is the source of truth for MTR Explorer. Update it (and the ADRs) in the same PR as any deviation.
-->

# MTR Explorer

### 7.5 MTR Explorer

**Delivered in M5** at `/mtr` (`web/src/pages/mtr.tsx`). Standalone module,
three panes: **destinations** (nodes + targets, grouped, from
`GET /api/v1/mtr/destinations`) → **trace history** for the selection
(`GET /api/v1/mtr/snapshots?source=&destination=`) → **trace detail**
(`GET /api/v1/mtr/snapshots/{id}`). A **Runner** segment sits beside the three,
and a diff view opens over the history pane when two rows are picked.

#### Where the data comes from, and what is therefore missing

Path history is a **projection of results the Console already stored**: the
checks runner hooks the MTR result it persists into `check_results.result` and
writes a normalized, content-hashed row into `mtr_path_snapshots` (Decision 1).
Two consequences the UI cannot paper over:

- **`kubectl-kconmon` traces are invisible to path history.** The CLI talks to
  the controller directly and never touches the Console, so a trace launched
  that way leaves no snapshot. Routing the CLI through the Console is an M7+
  decision, not an oversight. The Runner below exists partly because of this.
- **With `console.database.mode=disabled` history is in-memory only** — a ring
  of 500 snapshots that vanishes on restart, the same posture run history has.
  The page renders one honest line naming `console.database.mode` rather than
  an error.

Dedupe is a SHA-256 over the **ordered hop IP list only** (Decision 2), so one
row is one distinct route with `first_seen`/`last_seen`/`trace_count`
maintained on conflict. Silent hops (`*`, empty, whitespace) are dropped from
the hash while hop **numbering is preserved**, so an ICMP-rate-limited middle
hop does not manufacture a route change.

The history pane flags a row "path changed" when its hash differs from the
**next-older** row's. Worth knowing: `path_hash` is UNIQUE within a pair, so on
a single-pair page every row but the oldest is flagged. That is not a bug — a
new row *is* by construction a route the pair had not taken before — and the
badge is still written as a comparison rather than as "flag all but the last",
because the comparison is what the badge means and it is the same next-older
pairing the diff picks its two sides from. The oldest row is never flagged: it
is where the pair started, not a change away from something.

#### Trace detail — what the hop table actually draws

The table is **#, Address, Hostname, RTT, Loss**, plus an expander column.

This is a **deviation from the design above, recorded rather than quietly
reconciled**: the spec asks for `loss / avg / best / worst / jitter`, and those
five columns are not drawn. The stored `hops` payload carries the hop stats of
**one** trace — `{number, ip, hostname, rttNs, lossRatio}` — because Decision 2
deliberately stores the first trace at a path rather than a running average
across weeks (a number nothing ever measured). Dashing four permanently empty
columns would have looked like missing data instead of a shape that does not
exist. The across-time dimension the spec wanted **is** the per-hop trend chart
below; it just is not a column.

Hostname is whatever the stored trace carried. Enrichment is separate, and
lives in an **expandable row per hop** — rdns, ASN, provider, geo — keyed by
hop **number** rather than address, because a path can legitimately visit the
same address twice. When the API ships no row for an address the expander says
so honestly and names the knob ("enrichment may be disabled on this console"),
because the wire genuinely cannot distinguish "switched off" from "nothing
known about this address". Placeholder hops get no chevron at all.

**Per-hop trend** (Decision 13): clicking a hop charts that address's RTT
across the pair's snapshot history, built client-side from the snapshots
already fetched, with **NULL gaps where the route changed** rather than a line
drawn through two different paths. Hop RTTs never became Console metrics —
`hop_ip` cardinality was refused in M4 — so the trend reads stored history, not
Prometheus. It labels itself partial when the history is paged rather than
complete.

#### Enrichment

Console-side, **synchronous on read**, cached in `mtr_hop_enrichment` with a
TTL (Decision 4), and **off by default**. rDNS resolves through the pod's own
resolver; ASN/provider and geo come from optional MaxMind GeoLite2 mmdb files
mounted from an operator-provided volume (air-gap friendly — the Console never
downloads them). The two sources gate independently, an unreadable file
disables that one source with a warning rather than failing anything, and a
lookup never blocks longer than its budget: the response ships whatever
resolved in time and the cache catches up on the next read. Configuration and
its failure modes are in
[CONFIG.md](../architecture/CONFIG.md) "`mtr.enrichment`".

#### Path diff

Client-side (Decision 3) from two snapshot payloads the API already returned —
both are ≤64 hops and both are already on the page, so a server endpoint would
have duplicated presentation logic for zero authority gain.

Alignment is an **LCS over hop IPs**, then a positional zip inside each gap
between anchors. Placeholder hops never anchor. Two properties worth knowing:

- **A reorder reads as one removal plus one addition**, not as a move. Two hops
  that swapped positions are not a common subsequence containing both, so LCS
  reports them separately. That is honest about what changed and imprecise
  about how — naming it a move would require a heuristic that could be wrong.
- **RTT deltas appear on `same` rows only** — rows where both sides are the
  same address. Subtracting the RTT of one machine from the RTT of a different
  machine is not a delta, it is a coincidence of position.

The table always reads forwards in time, so "+" always means the newer path.

#### Path-changes timeline

Snapshot `first_seen` markers on a time axis, overlaid with the pair's loss
series from the PromQL proxy. There is **no `check_loss_ratio` metric** —
implementation found this while wiring it — so peer loss is
`kconmon_ng_icmp_packet_loss_ratio` **or** `kconmon_ng_udp_packet_loss_ratio`,
each tagged with a synthetic `protocol` label via `label_replace` and merged
with **`max by (protocol)`, never `sum`**: a loss *ratio* is not additive, and
summing two families would draw a number above 1.0 that nothing measured.

Which family applies is decided by the destination: a known node name means
peer, anything else means external
(`kconmon_ng_external_packet_loss_ratio{target=…}`). The known-node list is the
live topology, so the component **waits for topology to resolve** before asking
anything rather than guessing and then correcting itself on screen. The
markers are real DOM elements, so they survive a chart that never renders.

#### Runner

Launches an MTR to a node, a saved target or an ad-hoc address through the
**existing** `POST /api/v1/runs` (`type=mtr` + `destinationKind`), reusing the
Diagnostics page's own request builder rather than a second copy. It is gated
on `runs:create` — launching a trace never got a permission of its own — and
the whole segment **disappears** without it, so nobody is shown a tab they
cannot open. It is the page's one mutation and is therefore disabled (not
hidden) while Time Machine is engaged.

Submitting does **not** navigate away: the operator keeps their place in the
history they were reading, and the new trace lands in that history once the run
finishes.

#### Degraded states

Three, in the house pattern, each with **zero requests made**: no `mtr:read` →
a permission card; `database.mode=disabled` → one line naming
`console.database.mode`; permission and database but no traces yet → an empty
state that points at the Runner.
