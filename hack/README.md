# Local Development & Testing

Step-by-step guide for running the **whole** of kconmon-ng locally on Minikube: agents, controller
and the Console, against a real kube-prometheus-stack.

"Whole" is the point of this stand. The console here is not a stub — it gets a PostgreSQL database,
a webhook encryption key, Kubernetes event capture and the PrometheusRule reconciler, and the
Prometheus Operator that ships with kube-prometheus-stack is **installed, not just its CRD**. That
is the one thing CI cannot show you: in the e2e job only the `PrometheusRule` CRD exists, so a rule
the console applies is stored and never evaluated. Here it is picked up and evaluated for real.

## Prerequisites

| Tool | Minimum version | Install |
|------|----------------|---------|
| [Minikube](https://minikube.sigs.k8s.io/) | 1.36+ | `brew install minikube` |
| [Docker](https://www.docker.com/) | 24+ | OrbStack / Docker Desktop |
| [Helm](https://helm.sh/) | 4.x (≥3.14 works) | `brew install helm` |
| [kubectl](https://kubernetes.io/docs/tasks/tools/) | 1.31+ | `brew install kubectl` |
| [Go](https://go.dev/) | 1.26+ | `brew install go` |

## Quick Start (automated)

The `local-test.sh` script handles everything in one command:

```bash
./hack/local-test.sh up
```

This will:
1. Create a 3-node Minikube cluster
2. Build three Docker images (agent, controller, console) and load them into Minikube
3. Install kube-prometheus-stack (Prometheus Operator + Prometheus + Grafana)
4. Apply the console's two prerequisites: the PostgreSQL fixture and the webhook encryption key Secret
5. Deploy kconmon-ng with local values — agents, controller **and** console
6. Run smoke tests, including the alerting round trip (POST a rule → `syncStatus=synced` →
   a real `PrometheusRule` object in the cluster)
7. Import the Grafana dashboards and print access URLs

Expect the first `up` to take a while: the console image builds the SPA in a Node stage before the
Go binary, so it is by far the slowest of the three.

Other commands:

```bash
./hack/local-test.sh status   # cluster and pod status
./hack/local-test.sh smoke    # re-run smoke tests (agents, controller, console, alerting)
./hack/local-test.sh urls     # show Grafana/Prometheus/kconmon-ng/Console URLs
./hack/local-test.sh down     # delete the cluster
```

`up` and `smoke` print `PASS:` / `FAIL:` lines per console check and exit non-zero if any of them
failed — the URLs and dashboards still get printed first, so a failure never costs you the rest of
the output.

## Manual Step-by-Step

### 1. Create the Minikube cluster

```bash
minikube start \
    --nodes=3 \
    --cpus=2 \
    --memory=4096 \
    --driver=docker \
    --profile=kconmon-test
```

Wait for all nodes:

```bash
kubectl wait --for=condition=Ready node --all --timeout=120s
kubectl get nodes -o wide
```

### 2. Build Docker images

From the project root. Note the console has its **own** Dockerfile — it needs a Node stage to build
the SPA before the Go stage, which the agent/controller image has no reason to carry:

```bash
docker build --target agent      -t kconmon-ng-agent:local      .
docker build --target controller -t kconmon-ng-controller:local  .
docker build -f Dockerfile.console --target console -t kconmon-ng-console:local .
```

### 3. Load images into Minikube

Minikube nodes run their own container runtime, so images built on the host need to be loaded explicitly:

```bash
minikube image load kconmon-ng-agent:local      -p kconmon-test
minikube image load kconmon-ng-controller:local  -p kconmon-test
minikube image load kconmon-ng-console:local     -p kconmon-test
```

This can take 1-2 minutes per image depending on the image size.

### 4. Install Prometheus & Grafana

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update prometheus-community

helm install monitoring prometheus-community/kube-prometheus-stack \
    --namespace monitoring \
    --create-namespace \
    --set grafana.adminPassword=admin \
    --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
    --set prometheus.prometheusSpec.ruleSelectorNilUsesHelmValues=false \
    --set prometheus.prometheusSpec.podMonitorSelectorNilUsesHelmValues=false \
    --wait \
    --timeout 5m
```

The `*SelectorNilUsesHelmValues=false` flags are important — without them Prometheus will only scrape ServiceMonitors created by the kube-prometheus-stack chart itself and will ignore kconmon-ng's ServiceMonitor. `ruleSelectorNilUsesHelmValues=false` matters twice over here: it is also what makes Prometheus load the `PrometheusRule` object the **console** writes in step 8.

This chart is also what installs the Prometheus Operator and its CRDs. The console's alerting
reconciler needs `prometheusrules.monitoring.coreos.com` to exist before it can apply anything — with
the CRD missing, every rule reports `syncStatus=error` with a `crd-missing` cause rather than failing
at boot. So this step must come before the kconmon-ng install, not after.

Wait for pods:

```bash
kubectl wait --for=condition=ready pod \
    -l app.kubernetes.io/name=prometheus \
    -n monitoring --timeout=120s

kubectl wait --for=condition=ready pod \
    -l app.kubernetes.io/name=grafana \
    -n monitoring --timeout=120s
```

### 5. Create the console's prerequisites

The console mounts both of these as **files** at boot, so both must exist before the Helm install:
a missing DSN file leaves the pod short of readiness, and an unreadable encryption key file is fatal
outright. Neither is templated by the chart — it names Secrets, it never holds secret material.

```bash
# PostgreSQL: Deployment + Service + a Secret holding the DSN under key "dsn"
kubectl apply -f hack/postgres-local.yaml
kubectl wait --for=condition=ready pod -l app=kconmon-local-postgres --timeout=180s

# AES-256-GCM key that seals each webhook endpoint's HMAC secret at rest
kubectl create secret generic kconmon-local-webhooks-key \
    --from-literal=encryptionKey="$(openssl rand -base64 32)" \
    --dry-run=client -o yaml | kubectl apply -f -
```

`hack/postgres-local.yaml` is a plain Deployment on an `emptyDir`, deliberately not a CloudNativePG
`Cluster`: `database.existingSecret` needs the CNPG operator's CRDs to already exist and the chart
does not install the operator, so a local stand would have to run a second operator just to hand the
console a database. `mode=external` drives the identical code path. Both the credentials and the
storage are throwaway — do not copy either into anything real.

Regenerating the webhook key **rotates** it: any webhook secret already stored under the previous key
becomes undecryptable. Fine for a stand, not fine anywhere else.

### 6. Deploy kconmon-ng (agents + controller + console)

```bash
helm install kconmon-ng ./charts/kconmon-ng \
    -f hack/values-local.yaml \
    --wait \
    --timeout 5m
```

Verify pods are running with 0 restarts:

```bash
kubectl get pods -l app.kubernetes.io/name=kconmon-ng -o wide
kubectl get pods -l app=kconmon-local-postgres
```

Expected output: 1 controller pod + 3 agent pods (one per node) + 1 console pod, all `Running`,
`RESTARTS 0`, plus the Postgres fixture pod alongside them.

`hack/values-local.yaml` runs the console at **one replica** on purpose and the chart enforces it:
`controller.events.enabled` gives the console a realtime ingester, and with Valkey left disabled the
fan-out bus is in-process, so rendering with more than one replica fails the install with a message
naming that exact pair.

Auth is `anonymous` with role `admin` — there is no login, and every request carries the highest
role. That is a decision for a cluster that lives for an afternoon, never for anything shared.

### 7. Verify metrics

Check that agents export kconmon-ng metrics:

```bash
AGENT_POD=$(kubectl get pods -l app.kubernetes.io/component=agent -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward "$AGENT_POD" 8080:8080 &
sleep 2

curl -s http://localhost:8080/metrics | grep "^kconmon_ng" | head -20

# Stop port-forward
kill %1 2>/dev/null
```

Check that Prometheus is scraping:

```bash
kubectl port-forward -n monitoring svc/monitoring-kube-prometheus-prometheus 9090:9090 &
sleep 2

# Query UDP loss — should show 6 series (3 nodes × 2 peers each), all 0
curl -s 'http://localhost:9090/api/v1/query?query=kconmon_ng_udp_packet_loss_ratio' | python3 -m json.tool

kill %1 2>/dev/null
```

### 8. Verify the console

```bash
CONSOLE=$(kubectl get pods -l app.kubernetes.io/component=console -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward "$CONSOLE" 8081:8080 &
sleep 3

curl -sf http://localhost:8081/healthz && echo " healthz OK"
curl -sf http://localhost:8081/readyz  && echo " readyz OK"

# Proxied through to the controller — a 200 here proves the console resolved
# console.controller.url and the controller answered.
curl -s http://localhost:8081/api/v1/topology | python3 -m json.tool | head -30
```

Then open <http://localhost:8081> in a browser. Anonymous auth means no login screen. Worth a look
straight away:

| Page | What it shows |
|------|---------------|
| `/` | Health summary, worst pairs, firing alerts, recent events |
| `/matrix` | Live N×N node-to-node heatmap |
| `/investigate` | Per-pair drilldown, MTR traces, saved incidents |
| `/alerting` | Rule list and builder — the same rules step 9 drives via the API |

`kubernetesContext.enabled` is on, so `GET /api/v1/k8s-events` (and the events strip on the overview)
should fill up within a minute or two — Minikube churns Pods constantly during an install, and the
console reads events from its own release namespace.

### 9. Validate the alerting round trip

This is the part CI cannot do. The operator is installed here, so a rule the console applies is one
Prometheus actually loads.

```bash
# 1. Declare a rule. kind "raw" is verbatim: the expression lands in the bundle unchanged.
RULE_ID=$(curl -s -X POST http://localhost:8081/api/v1/alert-rules \
    -H 'Content-Type: application/json' \
    -d '{"name":"local-smoke","kind":"raw","params":{"expr":"vector(1) > 0"},"severity":"warning","forNs":0,"enabled":true}' \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

# 2. Wait for the reconciler. `synced` is the state of the SECOND pass — the reconciler compares the
#    live bundle against the desired one BEFORE applying, so the pass right after a write honestly
#    reports `drift` while already applying the correction. Kicking again is the way to wait for it.
for i in $(seq 1 30); do
    curl -s -X POST "http://localhost:8081/api/v1/alert-rules/$RULE_ID/sync" >/dev/null
    sleep 3
    STATE=$(curl -s "http://localhost:8081/api/v1/alert-rules/$RULE_ID" \
        | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["syncStatus"], d.get("syncMessage",""))')
    echo "$STATE"
    [[ "$STATE" == synced* ]] && break
done

# 3. Read the object back out of the cluster.
kubectl get prometheusrule -n default kconmon-ng-console-rules -o yaml
```

`kconmon-ng-console-rules` is the ONE object the console owns — one apply target, so drift is one
comparison and a partial apply is impossible. It sits next to the chart's own `kconmon-ng`
PrometheusRule; both should be listed by `kubectl get prometheusrule -n default`.

`vector(1) > 0` is chosen because it is well-formed PromQL that depends on no scrape target and no
series, so the rule fires immediately and proves the whole chain rather than your cluster's health.
Check it landed in Prometheus itself at <http://localhost:9090/alerts> (port-forward from step 7).

If the rule sticks on `syncStatus=error`, the first token of `syncMessage` is the cause class:
`crd-missing` means kube-prometheus-stack is not installed (or not yet), `forbidden` means the
console's Role/RoleBinding for `prometheusrules` did not land or is bound to the wrong subject.

Clean up when done:

```bash
curl -s -X DELETE "http://localhost:8081/api/v1/alert-rules/$RULE_ID"
kill %1 2>/dev/null   # the console port-forward
```

### 10. Import Grafana dashboards

Open Grafana:

```bash
kubectl port-forward -n monitoring svc/monitoring-grafana 3000:80 &
```

Go to http://localhost:3000 (login: `admin` / `admin`).

Import each dashboard:

1. Navigate to **Dashboards → New → Import**
2. Click **Upload JSON file**
3. Select the file and click **Import**

Dashboard files:

| File | Description |
|------|-------------|
| `dashboards/overview.json` | Cluster-wide success rates, latencies, controller status |
| `dashboards/node-detail.json` | Per-node breakdown by destination |
| `dashboards/zone-heatmap.json` | Cross-zone latency and loss heatmap |

Alternatively, upload all three via the Grafana API:

```bash
GRAFANA_URL="http://localhost:3000"

for f in dashboards/*.json; do
    echo "Importing $(basename "$f")..."
    curl -s -X POST "$GRAFANA_URL/api/dashboards/db" \
        -H "Content-Type: application/json" \
        -u admin:admin \
        -d "{\"dashboard\": $(cat "$f"), \"overwrite\": true}" \
        | python3 -c "import sys,json; r=json.load(sys.stdin); print(f'  {r.get(\"status\",\"?\")} -> {r.get(\"url\",\"\")}')";
done
```

### 11. Verify dashboards

After importing, check the **kconmon-ng Overview** dashboard:

- **Controller** section (top): Registered Agents = 3, gRPC Connections = 3, Leader Status = 1
- **TCP/UDP/ICMP** panels: Success rates should be 100%, loss 0%
- **DNS/HTTP** panels: Success rate 100% — `values-local.yaml` enables every checker, so a blank
  panel here is a real gap, not a disabled feature

If panels show "No data", wait 1-2 minutes for metrics to accumulate and scrape intervals to pass.

The console's `/` and `/matrix` pages read the same data from the same Prometheus, so they are the
faster cross-check: if Grafana is empty and the console is not (or the other way round), the problem
is in one of the two consumers rather than in the metrics.

## Chaos Testing (breaking connectivity)

To verify that failure detection, alerting, and MTR tracing work correctly, you can intentionally break inter-agent connectivity with a NetworkPolicy.

Apply the policy:

```bash
kubectl apply -f - <<'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: block-agent-traffic
  namespace: default
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: kconmon-ng
      app.kubernetes.io/component: agent
  policyTypes:
    - Ingress
  ingress:
    - from:
        - podSelector:
            matchLabels:
              app.kubernetes.io/component: controller
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: monitoring
EOF
```

This allows only the controller and Prometheus to reach agents. All agent-to-agent traffic (TCP, UDP, ICMP) is blocked.

The `monitoring` namespace exception is load-bearing: the loss metrics you are about to look at are exported by the agents themselves, so if the policy also cuts off Prometheus's scrapes, the agent series go stale after ~5 minutes and the break you created disappears from every dashboard — the chaos blinds its own observer. (Verified live: without the exception, the two agents not colocated with Prometheus went `down` in the targets list within one scrape interval.)

Within 10-30 seconds you should see in agent logs:

```bash
kubectl logs -l app.kubernetes.io/component=agent --since=1m | grep -E "check failed|triggering MTR"
```

Expected output:
- `check failed` with `type: tcp/udp/icmp` and `i/o timeout` or `100% loss`
- `triggering MTR trace` followed by `MTR trace completed`

On the Grafana Overview dashboard:
- TCP/UDP/ICMP success rates drop
- UDP/ICMP loss ratios go red
- MTR Triggers Count shows a non-zero value (orange/red)

The same break is visible on the console: `/matrix` turns the affected cells red as the pairs fail,
and `/investigate` on one of those pairs shows the failure timeline with the MTR trace the agent
captured when it tripped.

Remove the policy to restore normal connectivity:

```bash
kubectl delete networkpolicy block-agent-traffic
```

After 1-2 minutes all metrics should return to green (100% success, 0% loss).

## Rebuilding After Code Changes

When you modify Go code (or, for the console, anything under `web/`):

```bash
# A unique tag every time. Minikube's image-load cache silently keeps the old
# image when the tag is unchanged, so reusing one deploys stale code.
TAG="local-$(date +%Y%m%d%H%M%S)"

# Build only what you changed
docker build --target agent      -t kconmon-ng-agent:$TAG      .
docker build --target controller -t kconmon-ng-controller:$TAG  .
docker build -f Dockerfile.console --target console -t kconmon-ng-console:$TAG .

# Load into Minikube
minikube image load kconmon-ng-agent:$TAG      -p kconmon-test
minikube image load kconmon-ng-controller:$TAG  -p kconmon-test
minikube image load kconmon-ng-console:$TAG     -p kconmon-test

# Upgrade. --set-string on the command line, not an edit to values-local.yaml:
# the tag is a property of this build, not of the configuration.
helm upgrade kconmon-ng ./charts/kconmon-ng \
    -f hack/values-local.yaml \
    --set-string agent.image.tag="$TAG" \
    --set-string controller.image.tag="$TAG" \
    --set-string console.image.tag="$TAG" \
    --timeout 5m
```

This is exactly what `./hack/local-test.sh up` does — re-running it is the shorter path unless you
want to skip rebuilding one of the images.

Watch the rollout:

```bash
kubectl get pods -l app.kubernetes.io/name=kconmon-ng -w
```

All pods should reach `Running` with 0 restarts within 2-3 minutes. The Postgres fixture and the two
Secrets survive an upgrade untouched — nothing above recreates them, so the console keeps its data.

## Useful Commands

```bash
# Cluster status
minikube status -p kconmon-test

# Pod logs
kubectl logs -l app.kubernetes.io/component=controller --tail=30
kubectl logs -l app.kubernetes.io/component=agent --tail=30
kubectl logs -l app.kubernetes.io/component=console --tail=30

# Check for failed checks
kubectl logs -l app.kubernetes.io/component=agent --since=1m | grep "check failed"

# Topology API
CTRL=$(kubectl get pods -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward "$CTRL" 8080:8080 &
curl -s http://localhost:8080/api/v1/topology | python3 -m json.tool

# Console UI + API
CONSOLE=$(kubectl get pods -l app.kubernetes.io/component=console -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward "$CONSOLE" 8081:8080 &
open http://localhost:8081
curl -s http://localhost:8081/api/v1/alert-rules | python3 -m json.tool
curl -s http://localhost:8081/api/v1/k8s-events  | python3 -m json.tool | head -40

# Console database (psql inside the fixture pod)
PG=$(kubectl get pods -l app=kconmon-local-postgres -o jsonpath='{.items[0].metadata.name}')
kubectl exec -it "$PG" -- psql -U kconmon -d kconmon -c '\dt'

# Helm release info
helm list
helm get values kconmon-ng

# ServiceMonitor and PrometheusRule status
kubectl get servicemonitor -A | grep kconmon
kubectl get prometheusrule -A | grep kconmon

# The console's own rule bundle (separate from the chart's kconmon-ng rules)
kubectl get prometheusrule -n default kconmon-ng-console-rules -o yaml
```

## Cleanup

```bash
# Delete kconmon-ng release
helm uninstall kconmon-ng

# Delete the console's prerequisites (the Helm release does not own them)
kubectl delete -f hack/postgres-local.yaml
kubectl delete secret kconmon-local-webhooks-key

# Delete monitoring stack
helm uninstall monitoring -n monitoring

# Delete the entire cluster
minikube delete -p kconmon-test
```

## Troubleshooting

**Pods in CrashLoopBackOff at startup**
- Check controller logs first — agents need a running controller to register
- If controller is crashlooping, check if the ServiceAccount/RBAC was created: `kubectl get sa,clusterrole,clusterrolebinding | grep kconmon`

**Metrics show "No data" in Grafana**
- Confirm ServiceMonitor exists: `kubectl get servicemonitor -A | grep kconmon`
- Check Prometheus targets: go to http://localhost:9090/targets and look for kconmon-ng
- Wait 1-2 scrape intervals (default 10s in local values)

**Images not updating after rebuild**
- Minikube caches images. Use a new tag every time (e.g., `local-v2`, `local-v3`)
- Verify the image is loaded: `minikube image ls -p kconmon-test | grep kconmon`
- Ensure `pullPolicy: Never` is set in `values-local.yaml`

**Port-forward conflicts**
- Kill stale port-forwards: `lsof -ti:8080 | xargs kill -9 2>/dev/null`
- The script uses 18080 (controller), 18081 (agent), 18082 (console) and 13000 (Grafana) so it does
  not collide with hand-run forwards on 8080/8081/3000

**Console pod never becomes Ready**
- It mounts two Secrets as files at boot. Check both exist *before* the install:
  `kubectl get secret kconmon-local-postgres kconmon-local-webhooks-key`
- `kubectl logs -l app.kubernetes.io/component=console` — `permission denied` on the DSN file means
  `console.podSecurityContext.fsGroup` was overridden; the projected volume is mode 0440 and the
  distroless nonroot gid is 65532
- `kubectl get pods -l app=kconmon-local-postgres` — the console retries the database but will not
  report ready without it

**Console API answers 503**
- `/api/v1/alert-rules`, `/api/v1/webhooks`, `/api/v1/targets` and friends are database-backed. A 503
  there means `database.existingSecret` is not set or the DSN does not resolve
- `/matrix` and the PromQL pages 503 when `console.prometheus.url` is empty or unreachable — the
  Service name is `monitoring-kube-prometheus-prometheus` in namespace `monitoring`, which is derived
  from the Helm release name `monitoring`. Install the stack under a different name and this value
  has to change with it

**Alert rules stuck at `syncStatus=error`**
- The first token of `syncMessage` is the cause class. `crd-missing`: kube-prometheus-stack is not
  installed. `forbidden`: `kubectl get role,rolebinding | grep console-alerting` — the grant is a
  namespaced Role bound to the console's own ServiceAccount
- Rules written while `console.alerting.enabled` was false stay `unsynced` until something kicks the
  reconciler: `curl -X POST .../api/v1/alert-rules/$ID/sync`

**Alert rule is `synced` but Prometheus does not show it**
- That is a Prometheus-side selector question, not a console one. `ruleSelectorNilUsesHelmValues=false`
  must be set on the kube-prometheus-stack install (step 4), otherwise Prometheus only loads rule
  objects that chart created itself

## Files

| File | Purpose |
|------|---------|
| `local-test.sh` | Automated setup/teardown script |
| `values-local.yaml` | Helm values override for local testing (local images, all checkers enabled, debug logging, console with database + alerting + webhooks + Prometheus) |
| `postgres-local.yaml` | Throwaway PostgreSQL for the console: Deployment on an `emptyDir`, Service, and the Secret holding the DSN under key `dsn` |
