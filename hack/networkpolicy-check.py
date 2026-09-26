#!/usr/bin/env python3
"""NetworkPolicy flow checker for the kconmon-ng chart.

Renders the chart with every NetworkPolicy knob that matters set, then evaluates the rendered
policies with plain networking.k8s.io/v1 semantics (additive allow-lists, a pod is isolated for a
direction once any policy selecting it lists that policyType) and asserts:
  - every legitimate flow is allowed on both ends,
  - external peer CIDRs and agent pods cannot reach the controller's API or in-cluster gRPC,
  - the controller does not inherit the agent's egress,
  - networkPolicy.dnsEgress reaches a node-local DNS cache from every component,
  - under Cilium, where no ipBlock selects the kube-apiserver entity or a node IP (remote-node,
    host), the CiliumNetworkPolicies the chart renders (networkPolicy.ciliumKubeAPIEgress) let the
    controller and the console reach the apiserver, host-network agents reach the controller's gRPC
    port and node-forwarded external agents its gateway port, and nothing else; they render exactly
    when that knob and the cilium.io/v2 API say so,
  - networkPolicy.clusterCIDRs carves the cluster's own addresses out of the default ipBlock rules
    that are meant for off-cluster peers (on Calico an ipBlock matches pod IPs), and the default
    OIDC egress reaches an IdP served by an in-cluster ingress controller.
Usage: hack/networkpolicy-check.py charts/kconmon-ng
"""
import ipaddress
import json
import subprocess
import sys

import yaml

CHART = sys.argv[1]
NS = "mon"
REL = "t"
SEL = {"app.kubernetes.io/name": "kconmon-ng", "app.kubernetes.io/instance": REL}
EXT_PEER = "203.0.113.7"      # inside networkPolicy.externalPeerCidrs
EXT_AGENT = "198.51.100.7"    # inside networkPolicy.externalAgentCidrs (gateway)
NODE = "10.0.0.5"             # inside networkPolicy.nodeCidrs
INTERNET = "93.184.216.34"
NODELOCAL_DNS = "169.254.20.10"  # NodeLocal DNSCache on a host-network pod: no selector matches it
HTTP, GRPC, METRICS, GW = 8080, 9090, 9091, 9443
KINDS = ("NetworkPolicy", "CiliumNetworkPolicy")
CILIUM_API = "cilium.io/v2/CiliumNetworkPolicy"


def render(extra, api_versions=()):
    args = ["helm", "template", REL, CHART, "-n", NS,
            "--set", "networkPolicy.enabled=true",
            "--set", "console.enabled=true",
            "--set", "controller.events.enabled=true",
            "--set", "networkPolicy.prometheusNamespace=monitoring",
            "--set", "networkPolicy.externalPeerCidrs={203.0.113.0/24}",
            "--set", "networkPolicy.nodeCidrs={10.0.0.0/16}",
            "--set", "controller.externalGateway.enabled=true",
            "--set", "networkPolicy.externalAgentCidrs={198.51.100.0/24}",
            "--set", f"controller.externalGateway.port={GW}",
            "--set", "controller.externalGateway.tls.secretName=gw-tls",
            "--set", "config.checkers.http.enabled=true",
            "--set", "config.checkers.http.targets[0].url=https://example.com",
            "--set", "controller.externalGateway.bootstrapToken.secretName=gw-token",
            ] + extra
    for v in api_versions:
        args += ["--api-versions", v]
    res = subprocess.run(args, capture_output=True, text=True)
    if res.returncode != 0:
        sys.exit(f"helm template failed:\n{res.stderr}")
    out = res.stdout
    return [d for d in yaml.safe_load_all(out) if d and d.get("kind") in KINDS]


def pod(component, ns=NS, extra=None):
    labels = dict(SEL) if component not in ("test", "other") else {}
    if component not in ("test", "other"):
        labels["app.kubernetes.io/component"] = component
    if component == "test":
        labels.update({"app.kubernetes.io/component": "test", "kconmon-ng.io/role": "test"})
    labels.update(extra or {})
    return {"kind": "pod", "ns": ns, "labels": labels}


def ip(addr):
    return {"kind": "ip", "ip": ipaddress.ip_address(addr)}


def entity(name):
    """A Cilium reserved identity. Under the default policy-cidr-match-mode no ipBlock or selector of
    a networking.k8s.io/v1 policy matches it; only an empty peer list or a CNP toEntities does."""
    return {"kind": "entity", "entity": name}


def sel_match(selector, labels):
    if selector is None:
        return False
    for k, v in (selector.get("matchLabels") or {}).items():
        if labels.get(k) != v:
            return False
    for e in selector.get("matchExpressions") or []:
        val, op, vals = labels.get(e["key"]), e["operator"], e.get("values") or []
        if op == "In" and val not in vals:
            return False
        if op == "NotIn" and val in vals:
            return False
        if op == "Exists" and e["key"] not in labels:
            return False
        if op == "DoesNotExist" and e["key"] in labels:
            return False
    return True


def ns_labels(ns):
    return {"kubernetes.io/metadata.name": ns}


def peer_match(peers, who, policy_ns):
    if not peers:
        return True  # from/to omitted or empty: every peer
    for p in peers:
        if "ipBlock" in p:
            if who["kind"] != "ip":
                continue
            net = ipaddress.ip_network(p["ipBlock"]["cidr"])
            if who["ip"] in net and not any(who["ip"] in ipaddress.ip_network(x) for x in p["ipBlock"].get("except") or []):
                return True
            continue
        if who["kind"] != "pod":
            continue
        nss, pss = p.get("namespaceSelector"), p.get("podSelector")
        ns_ok = sel_match(nss, ns_labels(who["ns"])) if nss is not None else who["ns"] == policy_ns
        pod_ok = sel_match(pss, who["labels"]) if pss is not None else True
        if ns_ok and pod_ok:
            return True
    return False


def port_match(ports, proto, port):
    if not ports:
        return True
    if proto == "ICMP":
        return False
    for p in ports:
        if p.get("protocol", "TCP") != proto:
            continue
        if "port" not in p:
            return True
        lo = p["port"]
        hi = p.get("endPort", lo)
        if lo <= port <= hi:
            return True
    return False


def cnp_port_match(to_ports, proto, port):
    if not to_ports:
        return True
    for tp in to_ports:
        unknown = set(tp) - {"ports"}
        if unknown:
            sys.exit(f"checker does not model CiliumNetworkPolicy toPorts keys {sorted(unknown)}")
        for p in tp.get("ports") or []:
            if p.get("protocol", "ANY") not in ("ANY", proto):
                continue
            lo = int(p["port"])
            if lo == 0 or lo <= (port or 0) <= int(p.get("endPort", lo)):
                return True
    return False


def cnp_rule_match(rule, who, proto, port, direction):
    peers = "toEntities" if direction == "Egress" else "fromEntities"
    unknown = set(rule) - {peers, "toPorts"}
    if unknown:
        sys.exit(f"checker does not model CiliumNetworkPolicy {direction.lower()} keys {sorted(unknown)}")
    for e in rule.get(peers) or []:
        if e not in ("kube-apiserver", "remote-node", "host", "all"):
            sys.exit(f"checker does not model the Cilium entity {e!r}")
    ents = rule.get(peers) or []
    hit = "all" in ents or (who["kind"] == "entity" and who["entity"] in ents)
    return hit and cnp_port_match(rule.get("toPorts"), proto, port)


def selects(p, target, direction):
    if p["metadata"].get("namespace", NS) != target["ns"]:
        return False
    if p["kind"] == "CiliumNetworkPolicy":
        spec = p["spec"]
        unknown = set(spec) - {"endpointSelector", "egress", "ingress"}
        if unknown:
            sys.exit(f"checker does not model CiliumNetworkPolicy spec keys {sorted(unknown)}")
        return direction.lower() in spec and sel_match(spec["endpointSelector"], target["labels"])
    return (sel_match(p["spec"]["podSelector"], target["labels"])
            and direction in p["spec"].get("policyTypes", ["Ingress"]))


def allowed(policies, target, peer, direction, proto, port):
    key = "ingress" if direction == "Ingress" else "egress"
    peerkey = "from" if key == "ingress" else "to"
    selecting = [p for p in policies if selects(p, target, direction)]
    if not selecting:
        return True
    for p in selecting:
        for rule in p["spec"].get(key) or []:
            if p["kind"] == "CiliumNetworkPolicy":
                if cnp_rule_match(rule, peer, proto, port, direction):
                    return True
            elif peer_match(rule.get(peerkey), peer, NS) and port_match(rule.get("ports"), proto, port):
                return True
    return False


def flow(policies, src, dst, proto, port):
    """A flow needs egress on the source (if it is a pod) and ingress on the destination (if a pod)."""
    eg = True if src["kind"] != "pod" else allowed(policies, src, dst, "Egress", proto, port)
    ing = True if dst["kind"] != "pod" else allowed(policies, dst, src, "Ingress", proto, port)
    return eg and ing


agent, agent2, ctrl, console = pod("agent"), pod("agent", extra={"x": "2"}), pod("controller"), pod("console")
testpod = pod("test")
prom = pod("other", ns="monitoring", extra={"app": "prometheus"})
other_ns_pod = pod("other", ns="default")
kubedns = pod("other", ns="kube-system", extra={"k8s-app": "kube-dns"})
apiserver = ip("10.96.0.1")

MUST = [
    ("agent -> controller gRPC", agent, ctrl, "TCP", GRPC),
    ("host-network agent (node IP) -> controller gRPC", ip(NODE), ctrl, "TCP", GRPC),
    ("console -> controller API", console, ctrl, "TCP", HTTP),
    ("console -> controller gRPC (events)", console, ctrl, "TCP", GRPC),
    ("helm test pod -> controller API", testpod, ctrl, "TCP", HTTP),
    ("prometheus -> controller metrics", prom, ctrl, "TCP", METRICS),
    ("prometheus -> agent metrics", prom, agent, "TCP", METRICS),
    # serviceMonitor.enabled stays off here: a PodMonitor or a plain scrape config needs the rule too.
    ("prometheus -> console metrics", prom, console, "TCP", METRICS),
    ("external agent -> controller gateway", ip(EXT_AGENT), ctrl, "TCP", GW),
    ("agent -> agent UDP", agent, agent2, "UDP", GRPC),
    ("agent -> agent TCP", agent, agent2, "TCP", HTTP),
    ("agent -> agent ICMP/MTR", agent, agent2, "ICMP", None),
    ("external peer -> agent UDP", ip(EXT_PEER), agent, "UDP", GRPC),
    ("external peer -> agent TCP", ip(EXT_PEER), agent, "TCP", HTTP),
    ("external peer -> agent ICMP", ip(EXT_PEER), agent, "ICMP", None),
    ("agent -> external peer UDP", agent, ip(EXT_PEER), "UDP", GRPC),
    ("agent -> external peer TCP", agent, ip(EXT_PEER), "TCP", HTTP),
    ("agent -> external peer ICMP", agent, ip(EXT_PEER), "ICMP", None),
    ("agent -> DNS", agent, kubedns, "UDP", 53),
    ("controller -> DNS", ctrl, kubedns, "UDP", 53),
    ("console -> DNS", console, kubedns, "UDP", 53),
    ("controller -> apiserver", ctrl, apiserver, "TCP", 443),
    ("agent -> http checker target (default httpEgress)", agent, ip(INTERNET), "TCP", 443),
]
MUST_NOT = [
    ("external peer -> controller gRPC", ip(EXT_PEER), ctrl, "TCP", GRPC),
    ("external peer -> controller API", ip(EXT_PEER), ctrl, "TCP", HTTP),
    ("external peer -> controller metrics", ip(EXT_PEER), ctrl, "TCP", METRICS),
    ("external peer -> controller any (ICMP rule)", ip(EXT_PEER), ctrl, "TCP", 12345),
    ("agent -> controller API", agent, ctrl, "TCP", HTTP),
    ("agent -> controller arbitrary port", agent, ctrl, "TCP", 12345),
    ("external agent CIDR -> controller gRPC", ip(EXT_AGENT), ctrl, "TCP", GRPC),
    ("external agent CIDR -> agent", ip(EXT_AGENT), agent, "TCP", HTTP),
    ("node IP -> agent API", ip(NODE), agent, "UDP", GRPC),
    ("pod in another namespace -> controller API", other_ns_pod, ctrl, "TCP", HTTP),
    ("pod in another namespace -> console metrics", other_ns_pod, console, "TCP", METRICS),
    ("controller -> internet 80/443 (agent httpEgress)", ctrl, ip(INTERNET), "TCP", 80),
    ("controller -> external peer (agent peer egress)", ctrl, ip(EXT_PEER), "UDP", GRPC),
    ("controller -> agent", ctrl, agent, "TCP", HTTP),
    ("console -> agent API", console, agent, "TCP", HTTP),
]


NODELOCAL = [
    (f"{c} -> node-local DNS cache {proto}", p, ip(NODELOCAL_DNS), proto, 53)
    for c, p in (("agent", agent), ("controller", ctrl), ("console", console))
    for proto in ("UDP", "TCP")
]


def check(label, extra, must=(), must_not=(), api_versions=()):
    pols = render(extra, api_versions)
    bad = []
    for name, s, d, proto, port in [*MUST, *must]:
        if not flow(pols, s, d, proto, port):
            bad.append(f"BLOCKED (must be open): {name}")
    for name, s, d, proto, port in [*MUST_NOT, *must_not]:
        if flow(pols, s, d, proto, port):
            bad.append(f"OPEN (must be closed): {name}")
    print(f"[{label}] {len(pols)} policies: {', '.join(p['kind'] + '/' + p['metadata']['name'] for p in pols)}")
    for b in bad:
        print("  " + b)
    return not bad


ok = check("defaults+cidrs", [])
ok &= check("hostNetwork", ["--set", "agent.hostNetwork=true"])
# dnsEgress replaces the namespaceSelector DNS rule, which cannot match a host-network cache. The
# kube-dns pod stays in the list here, so the MUST flow to it is kept as well.
ok &= check("nodeLocalDNS", ["--set-json", "networkPolicy.dnsEgress=" + json.dumps([
    {"to": [{"ipBlock": {"cidr": f"{NODELOCAL_DNS}/32"}}, {"namespaceSelector": {}}],
     "ports": [{"protocol": "UDP", "port": 53}, {"protocol": "TCP", "port": 53}]}])], must=NODELOCAL)

# Cilium resolves the apiserver Service to the kube-apiserver entity before policy, and under the
# default policy-cidr-match-mode no ipBlock selects that entity: the controller's -apiserver policy
# and console.networkPolicy.kubeAPIEgress default to rules that never match there.
KUBE_APISERVER = entity("kube-apiserver")
CILIUM_MUST = [
    ("controller -> kube-apiserver 443", ctrl, KUBE_APISERVER, "TCP", 443),
    ("controller -> kube-apiserver 6443", ctrl, KUBE_APISERVER, "TCP", 6443),
    ("console -> kube-apiserver 443", console, KUBE_APISERVER, "TCP", 443),
    ("console -> kube-apiserver 6443", console, KUBE_APISERVER, "TCP", 6443),
]
CILIUM_MUST_NOT = [
    ("agent -> kube-apiserver", agent, KUBE_APISERVER, "TCP", 443),
    ("controller -> kube-apiserver kubelet port", ctrl, KUBE_APISERVER, "TCP", 10250),
    ("controller -> kube-apiserver UDP", ctrl, KUBE_APISERVER, "UDP", 443),
]
# A host-network agent on another node registers from its node IP, which Cilium tags remote-node
# (host on the controller's own node), and a gateway reached through a NodePort or an
# externalTrafficPolicy: Cluster LoadBalancer arrives SNATed to a node IP the same way.
REMOTE_NODE, HOST = entity("remote-node"), entity("host")
CILIUM_NODE_MUST = [
    ("host-network agent on another node (remote-node) -> controller gRPC", REMOTE_NODE, ctrl, "TCP", GRPC),
    ("host-network agent on the controller's node (host) -> controller gRPC", HOST, ctrl, "TCP", GRPC),
    ("external agent via a node (remote-node) -> controller gateway", REMOTE_NODE, ctrl, "TCP", GW),
]
CILIUM_NODE_MUST_NOT = [
    ("remote-node -> controller API", REMOTE_NODE, ctrl, "TCP", HTTP),
    ("remote-node -> controller metrics", REMOTE_NODE, ctrl, "TCP", METRICS),
    ("remote-node -> console gRPC port", REMOTE_NODE, console, "TCP", GRPC),
]
CONSOLE_K8S = ["--set", "console.kubernetesContext.enabled=true"]
ok &= check("cilium auto", CONSOLE_K8S, must=CILIUM_MUST, must_not=CILIUM_MUST_NOT,
            api_versions=[CILIUM_API])
ok &= check("cilium, host-network agents", [*CONSOLE_K8S, "--set", "agent.hostNetwork=true"],
            must=[*CILIUM_MUST, *CILIUM_NODE_MUST], must_not=[*CILIUM_MUST_NOT, *CILIUM_NODE_MUST_NOT],
            api_versions=[CILIUM_API])
ok &= check("cilium, pod-network agents", CONSOLE_K8S, must=[*CILIUM_MUST, CILIUM_NODE_MUST[2]],
            must_not=[*CILIUM_MUST_NOT, *CILIUM_NODE_MUST_NOT, *CILIUM_NODE_MUST[:2]],
            api_versions=[CILIUM_API])

# On Calico and Antrea an ipBlock matches pod IPs, so 0.0.0.0/0 opens every pod on those ports too.
POD_IP, SVC_IP = "10.244.3.7", "10.96.12.34"
CLUSTER = ["--set-json", 'networkPolicy.clusterCIDRs=["10.244.0.0/16", "10.96.0.0/12"]']
OIDC = ["--set", "console.auth.mode=oidc", "--set", "database.existingSecret=dsn",
        "--set", "console.auth.oidc.issuer=https://idp.example.com", "--set", "console.auth.oidc.clientID=k",
        "--set", "console.auth.oidc.existingSecret=oidc",
        "--set", "console.auth.oidc.redirectURL=https://console.example.com/api/v1/auth/oidc/callback",
        "--set", "console.webhooks.existingSecret=wh", "--set", "console.alerting.enabled=true"]
ingress_nginx = pod("other", ns="ingress-nginx", extra={"app.kubernetes.io/name": "ingress-nginx"})
EXTERNAL_ONLY = [
    ("console -> IdP on the internet 443", console, ip(INTERNET), "TCP", 443),
    ("console -> webhook receiver on the internet 443", console, ip(INTERNET), "TCP", 443),
]
ok &= check("oidc, in-cluster ingress controller", OIDC, must=[
    *EXTERNAL_ONLY, ("console -> IdP behind an in-cluster ingress controller 443", console, ingress_nginx, "TCP", 443)])
ok &= check("clusterCIDRs", [*OIDC, *CLUSTER], must=EXTERNAL_ONLY, must_not=[
    ("agent -> pod IP 443 (default httpEgress)", agent, ip(POD_IP), "TCP", 443),
    ("agent -> Service IP 80 (default httpEgress)", agent, ip(SVC_IP), "TCP", 80),
    ("console -> pod IP 80 (default webhookEgress)", console, ip(POD_IP), "TCP", 80),
])
ok &= check("cilium, console without a Kubernetes identity", [],
            must=CILIUM_MUST[:2],
            must_not=[*CILIUM_MUST_NOT, ("console -> kube-apiserver without a Kubernetes identity",
                                         console, KUBE_APISERVER, "TCP", 443)],
            api_versions=[CILIUM_API])

# console.prometheus.url names a Service whose targetPort differs (Bitnami Thanos: 9090 -> 10902).
# Calico, Cilium and Antrea match egress after the Service DNAT, on the pod port.
thanos = pod("other", ns="monitoring", extra={"app.kubernetes.io/name": "thanos"})
THANOS = ["--set", "console.prometheus.url=http://thanos-query.monitoring:9090"]
ok &= check("prometheus Service port 9090 -> pod port 10902", [
    *THANOS, "--set", "console.networkPolicy.prometheusTargetPort=10902"], must=[
    ("console -> Thanos pod port behind the Service", console, thanos, "TCP", 10902),
    ("console -> the URL's port", console, thanos, "TCP", 9090)])
ok &= check("prometheus URL port only", THANOS, must=[("console -> the URL's port", console, thanos, "TCP", 9090)],
            must_not=[("console -> Thanos pod port, none set", console, thanos, "TCP", 10902)])


def cnps(extra, api_versions=()):
    return [d["metadata"]["name"] for d in render(extra, api_versions) if d["kind"] == "CiliumNetworkPolicy"]


# A CNP isolates every pod it selects for its direction, so it must never render without the plain
# policies that already isolate them. render() turns the gateway on, so the node-ingress one renders
# next to the kube-apiserver one.
for label, extra, api, want in [
    ("auto, API present", [], [CILIUM_API], 2),
    ("auto, API present, no gateway", ["--set", "controller.externalGateway.enabled=false"], [CILIUM_API], 1),
    ("auto, API absent", [], [], 0),
    ("true, API absent", ["--set", "networkPolicy.ciliumKubeAPIEgress=true"], [], 2),
    ("false, API present", ["--set", "networkPolicy.ciliumKubeAPIEgress=false"], [CILIUM_API], 0),
    ("networkPolicy off, API present", ["--set", "networkPolicy.enabled=false"], [CILIUM_API], 0),
    ("networkPolicy off, forced true", ["--set", "networkPolicy.enabled=false",
                                        "--set", "networkPolicy.ciliumKubeAPIEgress=true"], [], 0),
]:
    got = cnps(extra, api)
    status = "ok" if len(got) == want else "FAIL"
    ok &= status == "ok"
    print(f"[ciliumKubeAPIEgress {label}] {status}: {len(got)} CiliumNetworkPolicy, want {want}")

# Without a gateway or host-network agents nothing opens the controller to node identities.
no_node = render(["--set", "controller.externalGateway.enabled=false"], [CILIUM_API])
for name, src, port in [("remote-node -> controller gRPC, pod-network agents, no gateway", REMOTE_NODE, GRPC),
                        ("remote-node -> controller gateway port, no gateway", REMOTE_NODE, GW)]:
    if flow(no_node, src, ctrl, "TCP", port):
        ok = False
        print(f"[cilium, no gateway] OPEN (must be closed): {name}")

print("PASS" if ok else "FAIL")
sys.exit(0 if ok else 1)
