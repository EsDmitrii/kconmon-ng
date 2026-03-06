# Local Development & Testing

Step-by-step guide for running kconmon-ng locally on Minikube with Prometheus and Grafana.

## Prerequisites

| Tool | Minimum version | Install |
|------|----------------|---------|
| [Minikube](https://minikube.sigs.k8s.io/) | 1.32+ | `brew install minikube` |
| [Docker](https://www.docker.com/) | 24+ | OrbStack / Docker Desktop |
| [Helm](https://helm.sh/) | 3.14+ | `brew install helm` |
| [kubectl](https://kubernetes.io/docs/tasks/tools/) | 1.29+ | `brew install kubectl` |
| [Go](https://go.dev/) | 1.25+ | `brew install go` |

## Quick Start (automated)

The `local-test.sh` script handles everything in one command:

```bash
./hack/local-test.sh up
```

This will:
1. Create a 3-node Minikube cluster
2. Build Docker images locally
3. Install kube-prometheus-stack (Prometheus + Grafana)
4. Deploy kconmon-ng with local values
5. Run smoke tests
6. Print access URLs

Other commands:

```bash
./hack/local-test.sh status   # cluster and pod status
./hack/local-test.sh smoke    # re-run smoke tests
./hack/local-test.sh urls     # show Grafana/Prometheus/kconmon-ng URLs
./hack/local-test.sh down     # delete the cluster
```

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

From the project root:

```bash
docker build --target agent      -t kconmon-ng-agent:local      .
docker build --target controller -t kconmon-ng-controller:local  .
```

### 3. Load images into Minikube

Minikube nodes run their own container runtime, so images built on the host need to be loaded explicitly:

```bash
minikube image load kconmon-ng-agent:local      -p kconmon-test
minikube image load kconmon-ng-controller:local  -p kconmon-test
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

The `*SelectorNilUsesHelmValues=false` flags are important — without them Prometheus will only scrape ServiceMonitors created by the kube-prometheus-stack chart itself and will ignore kconmon-ng's ServiceMonitor.

Wait for pods:

```bash
kubectl wait --for=condition=ready pod \
    -l app.kubernetes.io/name=prometheus \
    -n monitoring --timeout=120s

kubectl wait --for=condition=ready pod \
    -l app.kubernetes.io/name=grafana \
    -n monitoring --timeout=120s
```

### 5. Deploy kconmon-ng

```bash
helm install kconmon-ng ./charts/kconmon-ng \
    -f hack/values-local.yaml \
    --wait \
    --timeout 3m
```

Verify pods are running with 0 restarts:

```bash
kubectl get pods -l app.kubernetes.io/name=kconmon-ng -o wide
```

Expected output: 1 controller pod + 3 agent pods (one per node), all `Running`, `RESTARTS 0`.

### 6. Verify metrics

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

### 7. Import Grafana dashboards

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

### 8. Verify dashboards

After importing, check the **kconmon-ng Overview** dashboard:

- **Controller** section (top): Registered Agents = 3, gRPC Connections = 3, Leader Status = 1
- **TCP/UDP/ICMP** panels: Success rates should be 100%, loss 0%
- **DNS/HTTP** panels: Success rate 100% (if checkers are enabled in values)

If panels show "No data", wait 1-2 minutes for metrics to accumulate and scrape intervals to pass.

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
EOF
```

This allows only the controller to reach agents. All agent-to-agent traffic (TCP, UDP, ICMP) is blocked.

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

Remove the policy to restore normal connectivity:

```bash
kubectl delete networkpolicy block-agent-traffic
```

After 1-2 minutes all metrics should return to green (100% success, 0% loss).

## Rebuilding After Code Changes

When you modify Go code:

```bash
# Pick a unique tag to force pod replacement
TAG="local-v2"

# Build
docker build --target agent      -t kconmon-ng-agent:$TAG      .
docker build --target controller -t kconmon-ng-controller:$TAG  .

# Load into Minikube
minikube image load kconmon-ng-agent:$TAG      -p kconmon-test
minikube image load kconmon-ng-controller:$TAG  -p kconmon-test

# Update tag in values and upgrade
sed -i '' "s/tag: local-.*/tag: $TAG/" hack/values-local.yaml
helm upgrade kconmon-ng ./charts/kconmon-ng -f hack/values-local.yaml --timeout 5m
```

Watch the rollout:

```bash
kubectl get pods -l app.kubernetes.io/name=kconmon-ng -w
```

All pods should reach `Running` with 0 restarts within 2-3 minutes.

## Useful Commands

```bash
# Cluster status
minikube status -p kconmon-test

# Pod logs
kubectl logs -l app.kubernetes.io/component=controller --tail=30
kubectl logs -l app.kubernetes.io/component=agent --tail=30

# Check for failed checks
kubectl logs -l app.kubernetes.io/component=agent --since=1m | grep "check failed"

# Topology API
CTRL=$(kubectl get pods -l app.kubernetes.io/component=controller -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward "$CTRL" 8080:8080 &
curl -s http://localhost:8080/api/v1/topology | python3 -m json.tool

# Helm release info
helm list
helm get values kconmon-ng

# ServiceMonitor and PrometheusRule status
kubectl get servicemonitor -A | grep kconmon
kubectl get prometheusrule -A | grep kconmon
```

## Cleanup

```bash
# Delete kconmon-ng release
helm uninstall kconmon-ng

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

## Files

| File | Purpose |
|------|---------|
| `local-test.sh` | Automated setup/teardown script |
| `values-local.yaml` | Helm values override for local testing (local images, all checkers enabled, debug logging) |
