# External targets

## Goal

Probe destinations that are not fleet peers — the corporate DNS server, a
storage array, a VPN far end, a SaaS endpoint — continuously, from the nodes'
point of view. External checks reuse the same agents and the same metric
pipeline, but with a deliberately different shape: the destination is a
**named target** (`host` or `url`), never a raw address, and nothing is
probeable until you allow it twice — once in the agent's own CIDR allowlist,
once in the cluster's egress policy.

!!! note "This is the *reverse* of external agents"
    External checks are in-cluster agents probing outward, with no new trust
    surface. To move the vantage point itself outside the cluster — an agent
    on a bare host joining the mesh through the controller's TLS gateway —
    see [External agents](../external-agents.md).

## Configure external checks

Three layers, each refusing loudly when skipped:

**1. The agent-side gate.** Off by default; the agent refuses every external
destination until `allowedCidrs` names one:

```yaml
config:
  checkers:
    external:
      enabled: true
      allowedCidrs: ["10.20.0.0/16"] # matched against the RESOLVED address
      deniedCidrs: []                # carve-outs; denied wins
networkPolicy:
  externalEgress: # required when networkPolicy.enabled — see below
    - to:
        - ipBlock:
            cidr: 10.20.0.0/16 # no ports: key, so ICMP and MTR pass too
```

With `networkPolicy.enabled=true` the chart **fails the render** if
`externalEgress` is empty while the external checker is on — the only default
it could write is allow-everything, and it will not do that silently. Keep
the egress rule scoped to the same range as `allowedCidrs`: one layer lets
the agent try, the other lets the packet leave.

**2. Targets and checks, in the Console.** Define a target (a name plus a
`host` or `url`), then a check definition pointing at it: check type
(`icmp`/`tcp`/`udp`/`dns`/`http`), which agents probe (`one-per-zone` by
default — one vantage point per zone is usually enough and N× cheaper), and
parameters. See [Checks, runs and schedules](../concepts/checks-runs-schedules.md)
for how the objects relate.

**3. The dispatch loop.** Continuous external checks are pushed to agents by
the console's scheduler/reconciler, which is opt-in and needs a database:

```yaml
console:
  scheduler:
    enabled: true
```

A one-shot external probe needs none of layer 3: the
[Run checks](../console/run-checks.md) page and the controller's
[diagnostics endpoint](../api.md) accept external destinations directly —
still subject to layer 1.

## Verify probes

External metrics carry `target` (your name for it), `target_kind` and
`check_type` — no `destination_node`, because the destination is not a peer:

```promql
# results flowing per target
sum by (target, check_type, result) (rate(kconmon_ng_external_results_total[5m]))

# probes REFUSED before they happened, by reason
sum by (target, reason) (rate(kconmon_ng_external_denied_total[5m]))
```

If a probe never happens, `external_denied_total` is where the answer is:
`reason="cidr"` means the resolved address fell outside `allowedCidrs` (or
inside `deniedCidrs`), `resolve` means the name did not resolve, `disabled`
means a check arrived while the agent's external checker was off. A denied
probe increments this counter and **not** the results counter — a
misconfigured allowlist is a visible zero, not a failure rate. The full
family is in the [metrics reference](../metrics.md#agent-external).

## Alert on external failures

The bundled `ExternalChecksFailing` rule (`prometheusRule.enabled`) fires
when a target's failure ratio exceeds 10% for 5 minutes — deliberately looser
than the in-cluster 5%, because external paths cross networks you do not run.
Its annotation names the source node, the `target` and the measured
percentage. Tune it like every built-in:

```yaml
prometheusRule:
  externalChecksFailing:
    threshold: 0.05
    severity: critical
```

The ratio rule cannot see a probe that never runs, so pair it with a denial
alert of your own (via `prometheusRule.additionalRules` or the
[Console rule editor](set-up-alerting.md)) on
`rate(kconmon_ng_external_denied_total[5m]) > 0` — that is the alert that
catches an allowlist typo or a NetworkPolicy that quietly ate your probes.
