# ADR-005 — No plugins and no multi-tenancy in v1

- Status: accepted
- Date: 2026-07-14
- Deciders: @EsDmitrii

## Context

It is tempting to build a public extension API and a tenant dimension early.
Both are large surfaces that, done speculatively, ossify the wrong assumptions
(DESIGN.md §2 non-goals, §14 design reserves). v1 is single-tenant with RBAC
for team separation, and its correlation logic is explicit heuristics, not a
pluggable engine.

## Decision

We will **not** ship a plugin system or multi-tenancy in v1. Instead we keep
the seams clean so either can be carved out later without a rewrite:

- Strict internal module boundaries (§4.3) with narrow interfaces; nothing
  imports across layers except through them.
- An event bus (`internal/console/events`) that is the future plugin seam —
  v2 plugins would be consumers/producers on this bus plus registered UI
  routes. We do **not** build the registration machinery now.
- URL and data-model reserves for multi-cluster (`/api/v1/clusters/{c}/...`, a
  `cluster` column defaulting to `"default"`), but no tenant dimension.

## Consequences

### Positive

- Smaller, sharper v1; no speculative extension/tenant surface to maintain.
- The event bus + module boundaries make a future plugin layer additive.

### Negative / trade-offs

- Third parties cannot extend Console in v1; feature requests route through core.
- A real multi-tenancy need would require an ADR and schema/URL work later.

### Follow-ups

- Revisit multi-cluster federation with a concrete need (ADR when real, §14).
