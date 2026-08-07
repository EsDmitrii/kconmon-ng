<!--
Status: draft
Owner: @EsDmitrii
Source: extracted from root DESIGN.md §7.11 in M0 (2026-07-14).
This document is the source of truth for Alert Rule Management. Update it (and the ADRs) in the same PR as any deviation.
-->

# Alert Rule Management

### 7.11 Alert rule management (PrometheusRule)

- Console owns rules it creates: label
  `app.kubernetes.io/managed-by: kconmon-ng-console` + rule-ID annotation.
- Builder model in `alert_rules` (JSONB); rendering is a pure function →
  deterministic YAML; sync via server-side apply; drift detection and diff
  view before apply.
- Templates: pair loss, zone latency, DNS failures, HTTP TTFB, cert expiry,
  agent missing, external target down, + raw PromQL. Live preview ("this
  expression currently returns 3 series").
- Validation with the Prometheus rule parser as a library before apply.
- Foreign rules listed read-only; adoption is explicit import.
- `alerting.enabled=false` removes the RBAC verbs and hides the UI.
