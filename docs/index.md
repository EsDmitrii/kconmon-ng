# kconmon-ng

**When someone says "the network is fine", answer with data.**

kconmon-ng turns inter-node connectivity into a measured fact. An agent runs on
every Kubernetes node and probes every other node over TCP, UDP and ICMP every
five seconds, resolves DNS from each node, and can check HTTP endpoints too.
Every ordered node pair gets its own latency, jitter and packet-loss series,
per protocol — so a partial failure (UDP dropping on one pair while TCP stays
clean, DNS timing out from a single node) shows up as exactly that instead of
vanishing into a green aggregate. When a TCP, UDP or ICMP probe fails, the
agent fires an MTR trace to that peer, so the bad hop is on record before
anyone starts looking.

On top of the measurements sit an N×N matrix, a topology map, MTR path
history, incident timelines, Prometheus-evaluated alert rules, and a Time
Machine: a `?at=` on the URL rewinds every console page to the minute it
broke.

![Console Overview: cluster health summary, worst node pairs, firing alerts and open incidents](img/console-overview.png)

<div class="grid" markdown>

![Console Matrix: N×N heatmap of node-to-node loss and latency, one cell per ordered pair](img/console-matrix.png)

![Console Time Machine: the same matrix resolved at a past instant instead of now](img/console-timemachine.png)

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

</div>
