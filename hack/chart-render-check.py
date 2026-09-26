#!/usr/bin/env python3
"""Render-time contracts of the kconmon-ng chart that no other check covers.

  - A value the agent, controller or console refuses at startup must fail `helm install` on the
    schema (or a template fail where JSON Schema cannot express the check), naming the key.
    Otherwise the upgrade renders, the checksum rolls the DaemonSet, and the first new pod exits:
    the rollout stalls with a node unmonitored. The same values must keep rendering where the
    binaries accept them.
  - console.clientAddress.trustedProxyCIDRs and console.auth.header.trustedProxyCIDRs reach the
    console's config.yaml in every auth mode: the console reads the first (or the second while the
    first is empty) for per-address rate limits and the audit remoteAddr whatever the mode, and
    without them every client behind an ingress shares the ingress address. clientAddress stays out
    for a console image older than 2.5.0 (strict decoder).
  - Every object keeps a name of its own under a long release name: a name truncated as a whole
    string loses the suffix that tells two objects apart.
  - console.websocket caps reach config.yaml as set, 0 (cap off) included, and are left out where
    the console would exit on the key: an image older than 2.5.0 (strict decoder) and a
    --reuse-values upgrade over values that carry no websocket block (the console defaults apply).
  - The alerting and leader-election Roles scope every verb that addresses one object to their own
    (resourceNames): the PrometheusRule bundle and the controller's Lease. Only list on
    PrometheusRules and create on Leases (a POST carries no name) stay unscoped.
  - A chart-created Secret holds a number from a -f values file as its digits: Helm reads it as a
    float64, and a MaxMind accountId of 1234567 must not reach geoipupdate as "1.234567e+06".
  - NOTES flags an Ingress with both trust lists empty, and an external gateway whose agents arrive
    from node IPs (externalTrafficPolicy Cluster and no LoadBalancer source filter).
  - networkPolicy.clusterCIDRs entries the apiserver refuses as an except of 0.0.0.0/0 (/0, host
    bits set) fail the render, not the apply.
  - The helm test log keeps its own lines although /healthz answers without a newline.
Usage: hack/chart-render-check.py charts/kconmon-ng
"""
import json
import os
import subprocess
import sys
import tempfile

import yaml

CHART = sys.argv[1] if len(sys.argv) > 1 else "charts/kconmon-ng"
HTTP_TARGET = ["--set", "config.checkers.http.enabled=true",
               "--set", "config.checkers.http.targets[0].url=https://example.com"]
EXTERNAL = ["--set", "config.checkers.external.enabled=true"]
EXTERNAL_ON = [*EXTERNAL, "--set", "config.checkers.external.allowedCidrs[0]=10.0.0.0/8"]
SPARSE = ["--set", "topology.mode=sparse"]
NP_HTTP = ["--set", "networkPolicy.enabled=true", *HTTP_TARGET]
CONSOLE = ["--set", "console.enabled=true"]
ALERTING = [*CONSOLE, "--set", "console.alerting.enabled=true", "--set", "database.existingSecret=dsn"]
WEBHOOK_KEY = ["--set", "console.webhooks.existingSecret=wh"]
SCHEDULER = [*CONSOLE, "--set", "console.scheduler.enabled=true"]
KUBE_CONTEXT = [*CONSOLE, "--set", "console.kubernetesContext.enabled=true"]
ENRICHMENT = [*CONSOLE, "--set", "console.mtr.enrichment.enabled=true",
              "--set", "console.mtr.enrichment.rdns.enabled=true"]
OIDC = [*CONSOLE, "--set", "console.auth.mode=oidc", "--set", "database.existingSecret=dsn",
        "--set", "console.auth.oidc.issuer=https://idp.example.com",
        "--set", "console.auth.oidc.clientID=kconmon",
        "--set", "console.auth.oidc.existingSecret=oidc",
        "--set", "console.auth.oidc.redirectURL=https://console.example.com/api/v1/auth/oidc/callback"]


def helm(args):
    return subprocess.run(["helm", "template", "t", CHART, *args], capture_output=True, text=True)


REFUSE = [
    ("mtr.cooldown 0s", ["--set", "config.checkers.mtr.cooldown=0s"], "cooldown"),
    ("mtr.cooldown 0m0s", ["--set", "config.checkers.mtr.cooldown=0m0s"], "cooldown"),
    ("mtr.cooldown negative", ["--set", "config.checkers.mtr.cooldown=-1m"], "cooldown"),
    ("mtr.cooldown without a unit", ["--set-string", "config.checkers.mtr.cooldown=60"], "cooldown"),
    ("expectStatus 700", [*HTTP_TARGET, "--set", "config.checkers.http.targets[0].expectStatus=700"],
     "expectStatus"),
    ("expectStatus 99", [*HTTP_TARGET, "--set", "config.checkers.http.targets[0].expectStatus=99"],
     "expectStatus"),
    ("expectStatus negative", [*HTTP_TARGET, "--set", "config.checkers.http.targets[0].expectStatus=-1"],
     "expectStatus"),
    ("tcp.interval 0s, tcp enabled", ["--set", "config.checkers.tcp.interval=0s"], "interval"),
    ("udp.timeout 0ms, udp enabled", ["--set", "config.checkers.udp.timeout=0ms"], "timeout"),
    ("pmtu.interval 0s, pmtu enabled", ["--set", "config.checkers.pmtu.interval=0s"], "interval"),
    ("tcp.interval 5ns, tcp enabled", ["--set", "config.checkers.tcp.interval=5ns"], "interval"),
    ("udp.interval 50ms, udp enabled", ["--set", "config.checkers.udp.interval=50ms"], "interval"),
    ("icmp.interval 99.9ms, icmp enabled", ["--set", "config.checkers.icmp.interval=99.9ms"], "interval"),
    ("pmtu.interval 500us, pmtu enabled", ["--set", "config.checkers.pmtu.interval=500us"], "interval"),
    ("dns.interval 0.05s, dns enabled", ["--set", "config.checkers.dns.interval=0.05s"], "interval"),
    ("http.interval 0m5ms, http enabled", [*HTTP_TARGET, "--set", "config.checkers.http.interval=0m5ms"],
     "interval"),
    ("http.timeout 0s, http enabled", [*HTTP_TARGET, "--set", "config.checkers.http.timeout=0s"], "timeout"),
    ("tcp.timeout 5ns, tcp enabled", ["--set", "config.checkers.tcp.timeout=5ns"], "timeout"),
    ("udp.timeout 999999ns, udp enabled", ["--set", "config.checkers.udp.timeout=999999ns"], "timeout"),
    ("icmp.timeout 999us, icmp enabled", ["--set", "config.checkers.icmp.timeout=999us"], "timeout"),
    ("dns.timeout 0.5ms, dns enabled", ["--set", "config.checkers.dns.timeout=0.5ms"], "timeout"),
    ("http.timeout 0.0009s, http enabled", [*HTTP_TARGET, "--set", "config.checkers.http.timeout=0.0009s"], "timeout"),
    ("pmtu.timeout 500us, pmtu enabled", ["--set", "config.checkers.pmtu.timeout=500us"], "timeout"),
    ("external.timeout 500us", [*EXTERNAL_ON, "--set", "config.checkers.external.timeout=500us"], "timeout"),
    ("external.timeout negative", [*EXTERNAL_ON, "--set", "config.checkers.external.timeout=-1s"], "timeout"),
    ("external.timeout empty", [*EXTERNAL_ON, "--set-string", "config.checkers.external.timeout="], "timeout"),
    ("topology.sparse.zoneChords 65", [*SPARSE, "--set", "topology.sparse.zoneChords=65"], "zoneChords"),
    ("websocket.maxConnections negative", [*CONSOLE, "--set", "console.websocket.maxConnections=-1"],
     "maxConnections"),
    ("websocket.maxConnectionsPerAddress negative",
     [*CONSOLE, "--set", "console.websocket.maxConnectionsPerAddress=-1"], "maxConnectionsPerAddress"),
    ("websocket.maxConnectionsPerSubject negative",
     [*CONSOLE, "--set", "console.websocket.maxConnectionsPerSubject=-1"], "maxConnectionsPerSubject"),
    ("controllerAgentTtl 5s", ["--set", "config.controllerAgentTtl=5s"], "controllerAgentTtl"),
    ("controllerAgentTtl 9999ms", ["--set", "config.controllerAgentTtl=9999ms"], "controllerAgentTtl"),
    ("controllerAgentTtl 0m9s", ["--set", "config.controllerAgentTtl=0m9s"], "controllerAgentTtl"),
    ("controllerAgentTtl 0.1m", ["--set", "config.controllerAgentTtl=0.1m"], "controllerAgentTtl"),
    ("controllerAgentTtl .1m", ["--set", "config.controllerAgentTtl=.1m"], "controllerAgentTtl"),
    ("controllerAgentTtl 9999999µs", ["--set", "config.controllerAgentTtl=9999999µs"], "controllerAgentTtl"),
    ("external allowedCidrs bare IP", [*EXTERNAL, "--set", "config.checkers.external.allowedCidrs[0]=10.0.0.1"],
     "allowedCidrs"),
    ("external deniedCidrs bare IP", [*EXTERNAL, "--set", "config.checkers.external.allowedCidrs[0]=10.0.0.0/8",
                                      "--set", "config.checkers.external.deniedCidrs[0]=10.0.0.1"], "deniedCidrs"),
    ("external allowedCidrs octet 256", [*EXTERNAL, "--set", "config.checkers.external.allowedCidrs[0]=10.0.0.256/32"],
     "allowedCidrs"),
    ("http url without a scheme", ["--set", "config.checkers.http.enabled=true",
                                   "--set", "config.checkers.http.targets[0].url=example.com"], "url"),
    ("http url ftp scheme", ["--set", "config.checkers.http.enabled=true",
                             "--set", "config.checkers.http.targets[0].url=ftp://example.com"], "url"),
    ("http bodyPattern not a regex", [*HTTP_TARGET, "--set", "config.checkers.http.targets[0].bodyPattern=("],
     "bodyPattern"),
    ("dns resolver port 99999", ["--set", "config.checkers.dns.resolvers[0]=8.8.8.8:99999"], "resolvers"),
    ("dns resolver port 0", ["--set", "config.checkers.dns.resolvers[0]=[2001:db8::1]:0"], "resolvers"),
    ("dns resolver empty port", ["--set", "config.checkers.dns.resolvers[0]=dns.google:"], "resolvers"),
    ("trustedProxyCIDRs bare IP, anonymous", ["--set", "console.enabled=true",
                                              "--set", "console.auth.header.trustedProxyCIDRs[0]=10.0.0.5"],
     "trustedProxyCIDRs"),
    ("clientAddress.trustedProxyCIDRs bare IP", ["--set", "console.enabled=true",
                                                 "--set", "console.clientAddress.trustedProxyCIDRs[0]=10.0.0.5"],
     "clientAddress/trustedProxyCIDRs"),
    ("alerting bundleName is the chart's own PrometheusRule", [*ALERTING, "--set", "prometheusRule.enabled=true",
                                                             "--set", "console.alerting.bundleName=t-kconmon-ng"],
     "bundleName"),
    ("alerting.bundleName empty", [*ALERTING, "--set-string", "console.alerting.bundleName="], "bundleName"),
    ("alerting.bundleName Kc_Rules", [*ALERTING, "--set", "console.alerting.bundleName=Kc_Rules"], "bundleName"),
    ("alerting.bundleName ends in a dot", [*ALERTING, "--set", "console.alerting.bundleName=kc-rules."], "bundleName"),
    ("alerting.namespace Mon_NS, rbac.create=false",
     [*ALERTING, "--set", "rbac.create=false", "--set", "console.alerting.namespace=Mon_NS"], "alerting/namespace"),
    ("alerting.syncInterval 0s", [*ALERTING, "--set", "console.alerting.syncInterval=0s"], "syncInterval"),
    ("webhooks.alertPollInterval 0s, alerting and a webhook key", [*ALERTING, *WEBHOOK_KEY,
                                                                    "--set", "console.webhooks.alertPollInterval=0s"],
     "alertPollInterval"),
    ("scheduler.tickInterval 0s", [*SCHEDULER, "--set", "console.scheduler.tickInterval=0s"], "tickInterval"),
    ("kubernetesContext.resyncInterval 0s", [*KUBE_CONTEXT, "--set", "console.kubernetesContext.resyncInterval=0s"],
     "resyncInterval"),
    ("mtr.enrichment.ttl 0s", [*ENRICHMENT, "--set", "console.mtr.enrichment.ttl=0s"], "enrichment/ttl"),
    ("prometheus.queryTimeout 0s", [*CONSOLE, "--set", "console.prometheus.queryTimeout=0s"], "queryTimeout"),
    ("prometheus.maxRange 0s", [*CONSOLE, "--set", "console.prometheus.maxRange=0s"], "maxRange"),
    ("console controller.timeout 0s", [*CONSOLE, "--set", "console.controller.timeout=0s"], "controller/timeout"),
    ("auth.session.ttl 0s", [*CONSOLE, "--set", "console.auth.session.ttl=0s"], "session/ttl"),
    ("auth.session.idleTimeout negative", [*CONSOLE, "--set", "console.auth.session.idleTimeout=-1h"], "idleTimeout"),
    ("auth.session.cookieName __Host- without secure", [*CONSOLE, "--set", "console.auth.session.secure=false"],
     "secure"),
    ("redis.dialTimeout 0s", [*CONSOLE, "--set", "redis.dialTimeout=0s"], "dialTimeout"),
    ("database.connectTimeout 0s", [*CONSOLE, "--set", "database.existingSecret=dsn",
                                    "--set", "database.connectTimeout=0s"], "connectTimeout"),
    ("console controller.url with a trailing slash", [*CONSOLE, "--set", "console.controller.url=http://ctl:8080/"],
     "controller/url"),
    ("prometheus.url without a scheme", [*CONSOLE, "--set", "console.prometheus.url=prometheus:9090"],
     "prometheus/url"),
    ("oidc issuer over http", [*OIDC, "--set", "console.auth.oidc.issuer=http://idp.example.com"], "issuer"),
    ("oidc issuer with a trailing slash", [*OIDC, "--set", "console.auth.oidc.issuer=https://idp.example.com/"],
     "issuer"),
    ("oidc clientID empty", [*OIDC, "--set-string", "console.auth.oidc.clientID="], "clientID"),
    ("header mode, userHeader empty", [*CONSOLE, "--set", "console.auth.mode=header",
                                       "--set", "console.auth.header.trustedProxyCIDRs[0]=10.0.0.0/8",
                                       "--set-string", "console.auth.header.userHeader="], "userHeader"),
    ("groupRoles role blank", [*CONSOLE, "--set-json", 'console.auth.groupRoles={"ops":" "}'], "groupRoles"),
    ("networkPolicy.nodeCidrs bare IP", ["--set", "networkPolicy.enabled=true", "--set", "agent.hostNetwork=true",
                                         "--set", "networkPolicy.nodeCidrs[0]=10.0.0.5"], "nodeCidrs"),
    # The apiserver refuses an except entry that is not a strict subset of 0.0.0.0/0 or has host bits.
    ("clusterCIDRs 0.0.0.0/0", [*NP_HTTP, "--set", "networkPolicy.clusterCIDRs[0]=0.0.0.0/0"], "clusterCIDRs"),
    ("clusterCIDRs host bits 10.244.1.0/16", [*NP_HTTP, "--set", "networkPolicy.clusterCIDRs[0]=10.244.1.0/16"],
     "clusterCIDRs"),
    ("clusterCIDRs host bits 10.96.0.1/12", [*NP_HTTP, "--set", "networkPolicy.clusterCIDRs[0]=10.96.0.0/16",
                                             "--set", "networkPolicy.clusterCIDRs[1]=10.96.0.1/12"], "clusterCIDRs"),
    ("clusterCIDRs host bits 192.168.1.129/25", [*NP_HTTP,
                                                 "--set", "networkPolicy.clusterCIDRs[0]=192.168.1.129/25"],
     "clusterCIDRs"),
]
ACCEPT = [
    ("mtr.cooldown 5m", ["--set", "config.checkers.mtr.cooldown=5m"]),
    ("mtr.cooldown 1m30s", ["--set", "config.checkers.mtr.cooldown=1m30s"]),
    ("mtr.cooldown 0.5s", ["--set", "config.checkers.mtr.cooldown=0.5s"]),
    ("expectStatus 0 (unset)", [*HTTP_TARGET, "--set", "config.checkers.http.targets[0].expectStatus=0"]),
    ("expectStatus 100", [*HTTP_TARGET, "--set", "config.checkers.http.targets[0].expectStatus=100"]),
    ("expectStatus 599", [*HTTP_TARGET, "--set", "config.checkers.http.targets[0].expectStatus=599"]),
    # validateTiming runs only for an enabled checker, so a disabled one keeps any timing.
    ("tcp disabled with interval 0s", ["--set", "config.checkers.tcp.enabled=false",
                                       "--set", "config.checkers.tcp.interval=0s"]),
    ("pmtu.interval empty (agent default)", ["--set", "config.checkers.pmtu.interval="]),
    ("tcp.interval 100ms", ["--set", "config.checkers.tcp.interval=100ms"]),
    ("udp.interval 0.1s", ["--set", "config.checkers.udp.interval=0.1s"]),
    ("icmp.interval 100000us", ["--set", "config.checkers.icmp.interval=100000us"]),
    ("dns.interval 60ms60ms", ["--set", "config.checkers.dns.interval=60ms60ms"]),
    ("tcp disabled with interval 5ns", ["--set", "config.checkers.tcp.enabled=false",
                                       "--set", "config.checkers.tcp.interval=5ns"]),
    ("tcp.timeout 1ms", ["--set", "config.checkers.tcp.timeout=1ms"]),
    ("udp.timeout 1000us", ["--set", "config.checkers.udp.timeout=1000us"]),
    ("icmp.timeout 0.001s", ["--set", "config.checkers.icmp.timeout=0.001s"]),
    ("dns.timeout 1000000ns", ["--set", "config.checkers.dns.timeout=1000000ns"]),
    ("pmtu.timeout empty (agent default)", ["--set", "config.checkers.pmtu.timeout="]),
    ("tcp disabled with timeout 5ns", ["--set", "config.checkers.tcp.enabled=false",
                                      "--set", "config.checkers.tcp.timeout=5ns"]),
    ("external.timeout 0 (agent default)", [*EXTERNAL_ON, "--set-string", "config.checkers.external.timeout=0"]),
    ("external.timeout 0s (agent default)", [*EXTERNAL_ON, "--set", "config.checkers.external.timeout=0s"]),
    ("external.timeout 1ms", [*EXTERNAL_ON, "--set", "config.checkers.external.timeout=1ms"]),
    ("external disabled with timeout empty", ["--set-string", "config.checkers.external.timeout="]),
    ("topology.sparse.zoneChords 64", [*SPARSE, "--set", "topology.sparse.zoneChords=64"]),
    ("controllerAgentTtl 10s", ["--set", "config.controllerAgentTtl=10s"]),
    ("controllerAgentTtl 10000ms", ["--set", "config.controllerAgentTtl=10000ms"]),
    ("controllerAgentTtl 0.5m", ["--set", "config.controllerAgentTtl=0.5m"]),
    ("controllerAgentTtl 1m30s", ["--set", "config.controllerAgentTtl=1m30s"]),
    ("controllerAgentTtl .5m", ["--set", "config.controllerAgentTtl=.5m"]),
    ("controllerAgentTtl 1.m", ["--set", "config.controllerAgentTtl=1.m"]),
    ("controllerAgentTtl 10000000µs", ["--set", "config.controllerAgentTtl=10000000µs"]),
    ("external allowedCidrs v4 and v6", [*EXTERNAL, "--set", "config.checkers.external.allowedCidrs[0]=10.0.0.0/8",
                                         "--set", "config.checkers.external.allowedCidrs[1]=fd00::/8",
                                         "--set", "config.checkers.external.deniedCidrs[0]=10.1.2.3/32"]),
    ("http url upper-case scheme with port", ["--set", "config.checkers.http.enabled=true",
                                              "--set", "config.checkers.http.targets[0].url=HTTPS://example.com:8443/x"]),
    ("http bodyPattern regex", [*HTTP_TARGET, "--set", "config.checkers.http.targets[0].bodyPattern=ok|healthy"]),
    ("alerting bundleName = fullname, no chart PrometheusRule", [*ALERTING,
                                                                 "--set", "console.alerting.bundleName=t-kconmon-ng"]),
    ("every console feature on, defaults", [*ALERTING, *WEBHOOK_KEY, *SCHEDULER, *KUBE_CONTEXT, *ENRICHMENT]),
    ("console features off with zero intervals and an empty bundleName",
     [*CONSOLE, "--set", "console.scheduler.tickInterval=0s", "--set", "console.kubernetesContext.resyncInterval=0s",
      "--set", "console.mtr.enrichment.ttl=0s", "--set", "console.alerting.syncInterval=0s",
      "--set-string", "console.alerting.bundleName="]),
    ("webhooks.alertPollInterval 0s without a webhook key",
     [*ALERTING, "--set", "console.webhooks.alertPollInterval=0s"]),
    ("webhooks.alertPollInterval 0s with alerting off", [*CONSOLE, *WEBHOOK_KEY,
                                                         "--set", "console.webhooks.alertPollInterval=0s"]),
    ("alerting.bundleName kc.rules-1 in namespace mon-1, rbac.create=false",
     [*ALERTING, "--set", "rbac.create=false", "--set", "console.alerting.bundleName=kc.rules-1",
      "--set", "console.alerting.namespace=mon-1"]),
    ("auth.session.idleTimeout 0", [*CONSOLE, "--set", "console.auth.session.idleTimeout=0"]),
    ("auth.session.idleTimeout 0s", [*CONSOLE, "--set", "console.auth.session.idleTimeout=0s"]),
    ("auth.session.cookieName without __Host-, secure=false", [*CONSOLE, "--set", "console.auth.session.secure=false",
                                                               "--set", "console.auth.session.cookieName=kc_session"]),
    ("console controller.url and prometheus.url with a path",
     [*CONSOLE, "--set", "console.controller.url=http://ctl:8080/base",
      "--set", "console.prometheus.url=HTTPS://prom.example.com:9090/prometheus"]),
    ("oidc issuer with a path", [*OIDC, "--set", "console.auth.oidc.issuer=https://idp.example.com/realms/kc"]),
    ("dns resolvers in every spelling", ["--set", "config.checkers.dns.resolvers[0]=8.8.8.8:53",
                                         "--set", "config.checkers.dns.resolvers[1]=[2001:db8::1]:53",
                                         "--set", "config.checkers.dns.resolvers[2]=2001:4860:4860::8888",
                                         "--set", "config.checkers.dns.resolvers[3]=dns.google",
                                         "--set", "config.checkers.dns.resolvers[4]=1.1.1.1"]),
    ("clusterCIDRs pod, Service, /25 and /32", [*NP_HTTP, "--set-json", "networkPolicy.clusterCIDRs="
                                                 + json.dumps(["10.244.0.0/16", "10.96.0.0/12", "192.168.1.128/25",
                                                               "10.0.0.1/32", "128.0.0.0/1"])]),
]

CIDRS = ["10.42.0.0/16", "fd00:10:42::/48"]
CLIENT_CIDRS = ["10.244.0.0/16", "fd00:10:244::/56"]
CLIENT_ADDRESS = ["--set-json", "console.clientAddress.trustedProxyCIDRs=" + json.dumps(CLIENT_CIDRS)]
PROXIES = ["--set", "console.enabled=true", "--set", "console.replicas=1",
           "--set-json", "console.auth.header.trustedProxyCIDRs=" + json.dumps(CIDRS), *CLIENT_ADDRESS]
MODES = [
    ("anonymous", []),
    ("local", ["--set", "console.auth.mode=local", "--set", "database.existingSecret=dsn"]),
    ("oidc", OIDC),
    ("header", ["--set", "console.auth.mode=header"]),
]

CLIENT_ADDRESS_RENDERS = [
    ("unset", [], None),
    ("no clientAddress block (--reuse-values over 2.4.x values)", ["--set", "console.clientAddress=null"], None),
    ("console image 2.4.1", [*CLIENT_ADDRESS, "--set", "console.image.tag=2.4.1"], None),
]
WS_CAPS = ("maxConnections", "maxConnectionsPerAddress", "maxConnectionsPerSubject")


def ws_set(*values):
    return [a for k, v in zip(WS_CAPS, values) for a in ("--set", f"console.websocket.{k}={v}")]


WEBSOCKET = [
    ("defaults", [], dict(zip(WS_CAPS, (1024, 256, 32)))),
    ("every cap off", ws_set(0, 0, 0), dict(zip(WS_CAPS, (0, 0, 0)))),
    ("tuned", ws_set(4096, 64, 8), dict(zip(WS_CAPS, (4096, 64, 8)))),
    ("no websocket block (--reuse-values over 2.4.x values)", ["--set", "console.websocket=null"], None),
    ("console image 2.4.1", ["--set", "console.image.tag=2.4.1"], None),
    ("console image v2.5.0", ["--set", "console.image.tag=v2.5.0"], dict(zip(WS_CAPS, (1024, 256, 32)))),
]


# 46 characters without "kconmon-ng", so fullname is <release>-kconmon-ng at 57.
LONG_RELEASE = "monitoring-network-connectivity-prod-eu-west1a"
LONG_NAME_VALUES = ["--set", "dashboards.enabled=true", "--set", "console.enabled=true",
                    "--set", "prometheusRule.enabled=true", "--set", "networkPolicy.enabled=true",
                    "--set", "serviceMonitor.enabled=true"]


def duplicate_names():
    res = subprocess.run(["helm", "template", LONG_RELEASE, CHART, *LONG_NAME_VALUES],
                         capture_output=True, text=True)
    if res.returncode != 0:
        return [f"long release name did not render: {res.stderr.strip()}"]
    seen, bad = set(), []
    for d in yaml.safe_load_all(res.stdout):
        if not d:
            continue
        key = (d["kind"], d["metadata"].get("namespace", ""), d["metadata"]["name"])
        if key in seen:
            bad.append(f"long release name renders {key[0]} {key[2]} twice")
        seen.add(key)
    return bad


def alerting_role():
    """The reconciler reads, applies and deletes one object, its bundle; only list stays open. The
    bundle is created by server-side apply, a PATCH the apiserver authorizes as create on its name."""
    res = helm([*ALERTING, "--set", "console.alerting.bundleName=kc-rules"])
    if res.returncode != 0:
        return [f"alerting did not render: {res.stderr.strip()}"]
    roles = [d for d in yaml.safe_load_all(res.stdout)
             if d and d["kind"] == "Role" and d["metadata"]["name"].endswith("-alerting")]
    if len(roles) != 1:
        return [f"want one alerting Role, got {len(roles)}"]
    bad = []
    for rule in roles[0]["rules"]:
        named = set(rule["verbs"]) & {"get", "create", "patch", "delete", "update"}
        if named and rule.get("resourceNames") != ["kc-rules"]:
            bad.append(f"alerting Role grants {sorted(named)} on {rule.get('resourceNames') or 'every'} "
                       "PrometheusRule, want only the bundle kc-rules")
    return bad


GEOIP = {"console": {"enabled": True, "mtr": {"enrichment": {"enabled": True, "geoip": {
    "mode": "auto", "secret": {"create": True, "licenseKey": "abcdef"}}}}}}
# accountId as written in a values file (-f), and the string the Secret must carry.
SECRET_NUMBERS = [(123456, "123456"), (1000000, "1000000"), (1234567, "1234567"),
                  ("0012345", "0012345"), ("${MAXMIND_ACCOUNT_ID}", "${MAXMIND_ACCOUNT_ID}")]


def secret_numbers():
    bad = []
    for account, want in SECRET_NUMBERS:
        values = json.loads(json.dumps(GEOIP))
        values["console"]["mtr"]["enrichment"]["geoip"]["secret"]["accountId"] = account
        with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as f:
            yaml.safe_dump(values, f)
        try:
            res = helm(["-f", f.name])
        finally:
            os.unlink(f.name)
        if res.returncode != 0:
            bad.append(f"geoip accountId {account!r} from a values file did not render: {res.stderr.strip()}")
            continue
        got = [d["stringData"].get("console-maxmind-account-id") for d in yaml.safe_load_all(res.stdout)
               if d and d["kind"] == "Secret" and "console-maxmind-account-id" in d.get("stringData", {})]
        if got != [want]:
            bad.append(f"geoip accountId {account!r} from a values file: Secret carries {got!r}, want [{want!r}]")
    return bad


def leader_election_role():
    """The controller reads and renews one Lease, its own; only create (a POST carries no name) stays open."""
    res = helm([])
    if res.returncode != 0:
        return [f"default values did not render: {res.stderr.strip()}"]
    docs = [d for d in yaml.safe_load_all(res.stdout) if d]
    roles = [d for d in docs if d["kind"] == "Role" and d["metadata"]["name"].endswith("-leader-election")]
    lease = [e["value"] for d in docs if d["kind"] == "Deployment"
             for c in d["spec"]["template"]["spec"]["containers"] for e in c.get("env", [])
             if e["name"] == "KCONMON_NG_LEASE_NAME"]
    if len(roles) != 1 or len(lease) != 1:
        return [f"want one leader-election Role and one KCONMON_NG_LEASE_NAME, got {len(roles)} and {len(lease)}"]
    bad = []
    for rule in roles[0]["rules"]:
        named = set(rule["verbs"]) - {"create"}
        if named and rule.get("resourceNames") != lease:
            bad.append(f"leader-election Role grants {sorted(named)} on {rule.get('resourceNames') or 'every'} "
                       f"Lease, want only the controller's {lease}")
    return bad


INGRESS = [*CONSOLE, "--set", "console.ingress.enabled=true",
           "--set", "console.ingress.hosts[0].host=console.example.com",
           "--set", "console.ingress.hosts[0].paths[0].path=/",
           "--set", "console.ingress.hosts[0].paths[0].pathType=Prefix"]
SHARED_ADDRESS = "console.clientAddress.trustedProxyCIDRs \u2014 empty behind an Ingress"
# The entry must not steer the operator to trust more than the ingress controller.
SPOOF_WARNING = "a pod inside the list can name any client address"
GATEWAY = ["--set", "controller.externalGateway.enabled=true",
           "--set", "controller.externalGateway.tls.secretName=gw-tls",
           "--set", "controller.externalGateway.bootstrapToken.secretName=gw-token"]
GATEWAY_SOURCE_RANGES = ["--set", "controller.externalGateway.service.loadBalancerSourceRanges[0]=203.0.113.0/24"]
NODE_SOURCES = "controller.externalGateway.service.externalTrafficPolicy \u2014 Cluster"
NOTES = [
    ("ingress, both trust lists empty", INGRESS, SHARED_ADDRESS, True),
    ("ingress, no clientAddress block (--reuse-values over 2.4.x values)",
     [*INGRESS, "--set", "console.clientAddress=null"], SHARED_ADDRESS, True),
    ("ingress, clientAddress set", [*INGRESS, *CLIENT_ADDRESS], SHARED_ADDRESS, False),
    ("ingress, auth.header list set", [*INGRESS, "--set-json", "console.auth.header.trustedProxyCIDRs="
                                       + json.dumps(CIDRS)], SHARED_ADDRESS, False),
    ("no ingress", CONSOLE, SHARED_ADDRESS, False),
    ("gateway, default traffic policy", GATEWAY, NODE_SOURCES, True),
    ("gateway, NodePort (source ranges do not apply)",
     [*GATEWAY, *GATEWAY_SOURCE_RANGES, "--set", "controller.externalGateway.service.type=NodePort"],
     NODE_SOURCES, True),
    ("gateway, externalTrafficPolicy Local",
     [*GATEWAY, "--set", "controller.externalGateway.service.externalTrafficPolicy=Local"], NODE_SOURCES, False),
    ("gateway, LoadBalancer with source ranges", [*GATEWAY, *GATEWAY_SOURCE_RANGES], NODE_SOURCES, False),
    ("no gateway", [], NODE_SOURCES, False),
]


def notes():
    """Each NOTES entry is listed exactly where its setting is still open."""
    bad = []
    for name, args, entry, want in NOTES:
        res = subprocess.run(["helm", "install", "t", CHART, "--dry-run=client", *args],
                             capture_output=True, text=True)
        if res.returncode != 0:
            bad.append(f"NOTES {name}: did not render: {res.stderr.strip()}")
            continue
        got = entry in res.stdout
        if got != want:
            bad.append(f"NOTES {name}: entry {entry!r} {'present' if got else 'missing'}, want "
                       f"{'present' if want else 'absent'}")
        if got and entry == SHARED_ADDRESS and SPOOF_WARNING not in res.stdout:
            bad.append(f"NOTES {name}: shared-address entry lacks the warning {SPOOF_WARNING!r}")
    return bad


def helm_test_log():
    """The helm test pod prints each step on its own line, though /healthz answers "ok" without a newline."""
    res = helm([])
    if res.returncode != 0:
        return [f"default values did not render: {res.stderr.strip()}"]
    pods = [d for d in yaml.safe_load_all(res.stdout)
            if d and d["kind"] == "Pod" and "helm.sh/hook" in d["metadata"].get("annotations", {})]
    if len(pods) != 1:
        return [f"want one helm test Pod, got {len(pods)}"]
    script = pods[0]["spec"]["containers"][0]["command"][-1]
    with tempfile.TemporaryDirectory() as bin_dir:
        curl = os.path.join(bin_dir, "curl")
        with open(curl, "w") as f:
            f.write("#!/bin/sh\nprintf ok\n")
        os.chmod(curl, 0o755)
        run = subprocess.run(["sh", "-c", script], capture_output=True, text=True,
                             env={**os.environ, "PATH": bin_dir + os.pathsep + os.environ["PATH"]})
    if run.returncode != 0 or not any(line.startswith("Health check passed") for line in run.stdout.splitlines()):
        return [f"helm test log {run.stdout!r} (exit {run.returncode}): want a line starting 'Health check passed'"]
    return []


def console_config(out):
    for d in yaml.safe_load_all(out):
        if (d and d.get("kind") == "ConfigMap"
                and d["metadata"]["labels"].get("app.kubernetes.io/component") == "console"):
            return yaml.safe_load(d["data"]["config.yaml"])
    return None


def main():
    bad = []
    for name, args, key in REFUSE:
        res = helm(args)
        if res.returncode == 0:
            bad.append(f"RENDERED (the binary refuses it at startup): {name}")
        elif key not in res.stderr:
            bad.append(f"refused for another reason than {key}: {name}: {res.stderr.strip()}")
    for name, args in ACCEPT:
        res = helm(args)
        if res.returncode != 0:
            bad.append(f"REFUSED (the binary accepts it): {name}: {res.stderr.strip()}")
    for mode, args in MODES:
        res = helm([*PROXIES, *args])
        if res.returncode != 0:
            bad.append(f"console auth.mode={mode} did not render: {res.stderr.strip()}")
            continue
        cfg = console_config(res.stdout) or {}
        got = ((cfg.get("auth") or {}).get("header") or {}).get("trustedProxyCIDRs")
        if got != CIDRS:
            bad.append(f"auth.mode={mode}: auth.header.trustedProxyCIDRs in config.yaml is {got!r}, want {CIDRS!r}")
        got = (cfg.get("clientAddress") or {}).get("trustedProxyCIDRs")
        if got != CLIENT_CIDRS:
            bad.append(f"auth.mode={mode}: clientAddress.trustedProxyCIDRs in config.yaml is {got!r}, "
                       f"want {CLIENT_CIDRS!r}")
    for name, args, want in CLIENT_ADDRESS_RENDERS:
        res = helm([*CONSOLE, *args])
        if res.returncode != 0:
            bad.append(f"clientAddress {name}: did not render: {res.stderr.strip()}")
            continue
        got = (console_config(res.stdout) or {}).get("clientAddress")
        if got != want:
            bad.append(f"clientAddress {name}: clientAddress in config.yaml is {got!r}, want {want!r}")
    for name, args, want in WEBSOCKET:
        res = helm([*CONSOLE, *args])
        if res.returncode != 0:
            bad.append(f"websocket {name}: did not render: {res.stderr.strip()}")
            continue
        cfg = console_config(res.stdout)
        if cfg is None:
            bad.append(f"websocket {name}: no console config.yaml rendered")
            continue
        got = cfg.get("websocket")
        if got != want:
            bad.append(f"websocket {name}: websocket in config.yaml is {got!r}, want {want!r}")
    bad += duplicate_names()
    bad += alerting_role()
    bad += leader_election_role()
    bad += secret_numbers()
    bad += notes()
    bad += helm_test_log()
    for b in bad:
        print("  " + b)
    print(f"{len(REFUSE)} refusals, {len(ACCEPT)} accepts, {len(MODES)} auth modes, "
          f"{len(CLIENT_ADDRESS_RENDERS)} clientAddress renders, {len(WEBSOCKET)} websocket renders, "
          f"{len(SECRET_NUMBERS)} secret numbers, {len(NOTES)} NOTES renders: "
          + ("PASS" if not bad else "FAIL"))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
