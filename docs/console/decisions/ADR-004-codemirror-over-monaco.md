# ADR-004 — CodeMirror 6 over Monaco for the PromQL console

- Status: accepted
- Date: 2026-07-14
- Deciders: @EsDmitrii

## Context

The PromQL Console (DESIGN.md §7.12) needs a code editor with PromQL syntax
highlighting, autocomplete fed by `/labels` + metadata, multi-tab, history, and
`Ctrl+Enter` execution. It is an editor, not a terminal (§2 non-goals reject
xterm.js). Bundle weight matters for a data-dense SPA.

## Decision

We will use **CodeMirror 6** with a PromQL language extension. Monaco is
rejected: it is roughly an order of magnitude heavier and buys us no PromQL
capability, since PromQL support is a first-class CodeMirror extension.

## Consequences

### Positive

- Purpose-built PromQL editing (highlighting + completion) with a small bundle.
- Modular CodeMirror 6 architecture keeps only what we use.

### Negative / trade-offs

- Fewer batteries-included IDE features than Monaco (acceptable — we need an
  editor, not an IDE).

### Follow-ups

- Editor lands with the PromQL Console (M1). Wire completion to the guarded
  `/labels` proxy.
