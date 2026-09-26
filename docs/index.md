# kconmon-ng

![The console matrix of a 10-node cluster: on TCP all 90 pairs are green; on PMTU the pairs between zone-b and zone-c, every path into worker2 except worker3's and three single pairs turn red as black holes at 1400 and 1280 bytes and worker3's row, its path into worker2 included, turns amber on a reduced 1400-byte path, 38 of 90 pairs in all, while TCP stays green; last, the worker6 to worker2 pair card shows 0.0% TCP failures next to a 1400 of 1500 byte black hole one way and full size the other](img/pmtu-black-hole.gif)

**When someone says "the network is fine", answer with data.**

kconmon-ng turns inter-node connectivity into a measured fact. An agent runs
on every Kubernetes node and probes every other node over TCP, UDP and ICMP
every five seconds, and once a minute checks that a full-size datagram with
Don't Fragment set still crosses each pair (the path MTU probe, since 2.5.0);
it also resolves DNS from each node and can check HTTP endpoints you
configure. Each probe is a specific act: a UDP probe sends a
burst of 5 packets with a 250 ms reply timeout and computes loss as sent
minus received over sent, while a TCP probe dials the peer with a 1 s
timeout. The default DNS check resolves
`kubernetes.default.svc.cluster.local` through the pod's own resolver, and
explicit upstreams can be named instead.

Every ordered node pair gets its own latency, jitter and packet-loss series,
per protocol. A partial failure shows up as exactly that, instead of
vanishing into a green aggregate: UDP dropping on one pair while TCP stays
clean, or DNS timing out from a single node. When a TCP, UDP or ICMP probe
fails, the agent fires an MTR trace to that peer, so the bad hop is on record
before anyone starts looking. One caution before a big rollout: pairs grow as
N×(N−1) and each directed pair keeps 78 series, so read
[Scaling and cardinality](metrics.md#scaling-and-cardinality) before pointing
this at a large cluster.

On top of the measurements sit an N×N matrix, a topology map, MTR path
history, incident timelines, Prometheus-evaluated alert rules, and a Time
Machine: a `?at=` on the URL rewinds every console page to the minute it
broke.

[Install in 15 minutes](getting-started/install-15-min.md){ .md-button .md-button--primary }

<figure markdown="span">
  ![Console Overview on a six-node kind cluster during a staged outage: 10 pairs failing (TCP) in the header, 6/6 nodes ready, 10 failing and 0 degraded pairs, the Worst pairs table with five worker4 pairs at 100.0%, the console rules NodeTcpUnreachable (critical) and PairPmtuBlackHole (warning) firing, and three open incidents: PMTU black hole to worker5, Zone b network degradation and worker4 TCP outage](img/console-overview.png){ loading=lazy }
  <figcaption>The Overview page mid-incident on a six-node kind cluster: worker4 has lost TCP to and from every peer, so 10 of 30 pairs fail and the worst-pairs table ranks five of them at 100.0%. Two console rules fire, NodeTcpUnreachable (critical) for worker4 and PairPmtuBlackHole (warning) for a path MTU black hole from worker3 to worker5, and three incidents are open: PMTU black hole to worker5, Zone b network degradation and worker4 TCP outage.</figcaption>
</figure>

<div class="grid" markdown>

<figure markdown="span">
  ![Console Matrix, TCP, live, zoomed to 150% during a staged break: the worker4 row and column red at 100.0%, 10 of 30 cells, the other 20 green at 0.0%](img/console-matrix.png){ loading=lazy }
  <figcaption>The Matrix on TCP, live, mid-break and zoomed to 150%: worker4 is red at 100% as a row and as a column, 10 of 30 cells, and the 20 pairs among the other five nodes stay green.</figcaption>
</figure>

<figure markdown="span">
  ![Console Time Machine: the Matrix resolved at 9/26/2026 08:37:30 instead of now, the amber banner and time control marking the instant, all 30 cells green](img/console-timemachine.png){ loading=lazy }
  <figcaption>The Matrix rewound with <code>?at=</code> to 9/26/2026 08:37:30, about a minute and a half before the break in the frame above: the amber banner and time control mark the viewed instant, and every one of the 30 pairs is green, worker4 included.</figcaption>
</figure>

</div>

## Where to go

<div class="grid cards" markdown>

-   **[Install in 15 minutes](getting-started/install-15-min.md)**

    ---

    From `helm install` to first metrics, then
    [enable the console](getting-started/enable-the-console.md) and
    [catch a breakage](getting-started/catch-a-breakage.md) on a test
    cluster.

-   **[Concepts](concepts/architecture.md)**

    ---

    Agent, controller and console; the [probe mesh and
    zones](concepts/mesh-and-planes.md);
    [checks vs runs vs schedules](concepts/checks-runs-schedules.md).

-   **[Console guide](console/overview.md)**

    ---

    One page per screen, from the [Matrix](console/matrix.md) to the
    [Time Machine](console/time-machine.md).

-   **[Scenarios](scenarios/diagnose-a-slow-pair.md)**

    ---

    Task-oriented walkthroughs:
    [diagnose a slow pair](scenarios/diagnose-a-slow-pair.md),
    [set up alerting](scenarios/set-up-alerting.md),
    [probe external targets](scenarios/external-targets.md),
    [wire up OIDC](scenarios/oidc-setup.md).

-   **[External agents](external-agents.md)**

    ---

    Since v2.3.0 the same agent runs on a host outside the cluster: deb or
    rpm on the host, a TLS gateway on the controller, a bearer token and
    optional client-certificate pinning. The trust model and
    [what v1 does not do](external-agents.md#what-v1-does-not-do) are
    stated up front.

-   **[Reference](reference/helm-values.md)**

    ---

    [Helm values](reference/helm-values.md),
    [configuration file](configuration.md), [HTTP API](api.md),
    [Console API](reference/console-api.md), and the full
    [metrics and alerting reference](metrics.md).

-   **[FAQ](faq.md)**

    ---

    The questions that come up: privileges, controller outages, scale
    limits, what is safe to expose.

-   **[Project](https://github.com/EsDmitrii/kconmon-ng)**

    ---

    Source on [GitHub](https://github.com/EsDmitrii/kconmon-ng); the chart
    on [Artifact Hub](https://artifacthub.io/packages/helm/kconmon-ng/kconmon-ng)
    and as `oci://ghcr.io/esdmitrii/charts/kconmon-ng`; images
    `ghcr.io/esdmitrii/kconmon-ng-{agent,controller,console}` on
    [GHCR](https://github.com/EsDmitrii?tab=packages&repo_name=kconmon-ng);
    the [release notes](reference/release-notes.md).

</div>
