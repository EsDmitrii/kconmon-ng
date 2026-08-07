<!--
Status: draft
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §6.3 in M0 (2026-07-14).
This document is the source of truth for Time Machine. Update it (and the ADRs) in the same PR as any deviation.
-->

# Time Machine

### 6.3 Time Machine (global time context)

A top-bar control with two states: **Live** and **@ timestamp**. It is a
single piece of global state (`timemachine` store) that every data hook
resolves through:

- Prometheus reads become instant/range queries evaluated at/around `t`.
- Topology is reconstructed from `topology_events` up to `t`.
- Matrix renders the historical snapshot; Live feed becomes a scrollback
  around `t`; object cards show state-as-of-`t` with "Recent changes"
  relative to `t`.
- Mutating actions (run check, edit rule) are disabled with a clear banner
  ("You are viewing 15:34 yesterday — return to Live to act").
- The state is in the URL (`?at=`), so a Time Machine view is shareable.

Implementation note: this is why §4.1 persists `topology_events` — the
controller only knows *now*.
