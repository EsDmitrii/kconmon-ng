# kconmon-ng

**When someone says "the network is fine", answer with data.**

kconmon-ng turns inter-node connectivity into a measured fact. An agent runs
on every Kubernetes node and probes every other node over TCP, UDP and ICMP
every five seconds; it also resolves DNS from each node and can check HTTP
endpoints you configure. Each probe is a specific act: a UDP probe sends a
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
N×(N−1) and each directed pair keeps roughly 75 series, so read
[Scaling and cardinality](metrics.md#scaling-and-cardinality) before pointing
this at a large cluster.

On top of the measurements sit an N×N matrix, a topology map, MTR path
history, incident timelines, Prometheus-evaluated alert rules, and a Time
Machine: a `?at=` on the URL rewinds every console page to the minute it
broke.

[Install in 15 minutes](getting-started/install-15-min.md){ .md-button .md-button--primary }

<figure markdown="span">
  ![Console Overview on a kind cluster with an external agent: all 110 pairs healthy, 11/11 nodes ready plus one external agent, no failing or degraded pairs, no firing alert, one open incident](img/console-overview.png){ loading=lazy }
  <figcaption>The Overview page on a kind cluster with ten in-cluster agents and the external agent edge-host-01: all 110 pairs healthy, 11/11 nodes ready plus one external agent, an empty worst-pairs panel over 110 measured pairs, no firing alert and one open incident, a zone-c blackhole drill.</figcaption>
</figure>

<div class="grid" markdown>

<figure markdown="span">
  ![Console Matrix, TCP, live, during a staged break: worker2, worker5, worker6 and worker7 red at 100% as rows and columns, 68 of 110 cells red, the external agent edge-host-01 green as a row and a column everywhere except toward the four broken nodes](img/console-matrix.png){ loading=lazy }
  <figcaption>The Matrix on TCP, live, mid-break: worker2, worker5, worker6 and worker7 are red at 100% as rows and as columns, 68 of 110 cells. The external agent edge-host-01 is the top row and the first column, green against every healthy node and red only where the break is.</figcaption>
</figure>

<figure markdown="span">
  ![Console Time Machine: the Matrix resolved at 9/15/2026 09:36:00 instead of now, the amber banner and time control marking the instant, all 110 cells green including the edge-host-01 row and column](img/console-timemachine.png){ loading=lazy }
  <figcaption>The Matrix rewound with <code>?at=</code> to 9/15/2026 09:36:00, a minute before the break in the frame above: the amber banner and time control mark the viewed instant, and every one of the 110 pairs is green, the external agent's row and column included.</figcaption>
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
