<!--
Status: draft
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §7.5 in M0 (2026-07-14).
This document is the source of truth for MTR Explorer. Update it (and the ADRs) in the same PR as any deviation.
-->

# MTR Explorer

### 7.5 MTR Explorer

Standalone module, three panes: destinations list (nodes + targets) →
trace history for the selection → trace detail.

Trace detail: hop table with per-hop `loss / avg / best / worst / jitter`
(from the MTR payload) and an expandable enrichment row: reverse DNS, ASN,
provider, GeoIP — resolved server-side, cached in `mtr_hop_enrichment`.
Enrichment sources are pluggable and **off by default**: rDNS via
configurable resolver; ASN/GeoIP via optional MaxMind GeoLite2 mmdb mounted
from a volume (air-gap friendly, no runtime external calls unless
explicitly enabled). Per-hop historical trend chart where the hop recurs
across snapshots.

Path diff: select any two snapshots → added/removed/changed hops with RTT
deltas; "path changes" timeline overlaid with the pair's loss series from
Prometheus. Runner tab launches MTR to any node/target/ad-hoc host.
