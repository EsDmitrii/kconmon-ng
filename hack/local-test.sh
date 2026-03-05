#!/usr/bin/env bash
# Local E2E test: minikube cluster + Prometheus/Grafana + kconmon-ng
set -euo pipefail

PROFILE="kconmon-test"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
NAMESPACE_MONITORING="monitoring"
NAMESPACE_APP="default"

log() { printf '\n\033[1;34m>>> %s\033[0m\n' "$1"; }
err() { printf '\033[1;31mERROR: %s\033[0m\n' "$1" >&2; exit 1; }

check_deps() {
    local missing=()
    for cmd in minikube docker helm kubectl; do
        command -v "$cmd" >/dev/null 2>&1 || missing+=("$cmd")
    done
    if [[ ${#missing[@]} -gt 0 ]]; then
        err "Missing required tools: ${missing[*]}"
    fi
}

cluster_up() {
    log "Starting minikube cluster (3 nodes)..."
    if minikube status -p "$PROFILE" >/dev/null 2>&1; then
        log "Cluster '$PROFILE' already running"
        return
    fi
    minikube start \
        --nodes=3 \
        --cpus=2 \
        --memory=4096 \
        --driver=docker \
        --profile="$PROFILE"

    log "Waiting for nodes to be ready..."
    kubectl wait --for=condition=Ready node --all --timeout=120s
}

build_images() {
    log "Building Docker images locally..."
    docker build --target agent -t kconmon-ng-agent:local "$PROJECT_DIR"
    docker build --target controller -t kconmon-ng-controller:local "$PROJECT_DIR"

    log "Loading images into minikube (all nodes)..."
    minikube image load kconmon-ng-agent:local -p "$PROFILE"
    minikube image load kconmon-ng-controller:local -p "$PROFILE"

    log "Images loaded into minikube"
}

install_monitoring() {
    log "Installing kube-prometheus-stack..."
    if helm status monitoring -n "$NAMESPACE_MONITORING" >/dev/null 2>&1; then
        log "kube-prometheus-stack already installed"
        return
    fi

    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts 2>/dev/null || true
    helm repo update prometheus-community

    helm upgrade -i monitoring prometheus-community/kube-prometheus-stack \
        --namespace "$NAMESPACE_MONITORING" \
        --create-namespace \
        --set grafana.adminPassword=admin \
        --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
        --set prometheus.prometheusSpec.ruleSelectorNilUsesHelmValues=false \
        --set prometheus.prometheusSpec.podMonitorSelectorNilUsesHelmValues=false \
        --set alertmanager.enabled=false \
        --set nodeExporter.enabled=false \
        --set kubeStateMetrics.enabled=true \
        --set grafana.defaultDashboardsEnabled=false \
        --set grafana.defaultDashboardsTimezone=browser \
        --set coreDns.enabled=false \
        --set kubeControllerManager.enabled=false \
        --set kubeEtcd.enabled=false \
        --set kubeScheduler.enabled=false \
        --set kubeProxy.enabled=false \
        --set kubeApiServer.enabled=false \
        --set kubelet.enabled=false \
        --set defaultRules.create=false \
        --set grafana.sidecar.dashboards.searchNamespace=ALL \
        --wait \
        --timeout 5m

    log "Waiting for Prometheus pods..."
    kubectl wait --for=condition=ready pod \
        -l app.kubernetes.io/name=prometheus \
        -n "$NAMESPACE_MONITORING" \
        --timeout=120s

    log "Waiting for Grafana pod..."
    kubectl wait --for=condition=ready pod \
        -l app.kubernetes.io/name=grafana \
        -n "$NAMESPACE_MONITORING" \
        --timeout=120s
}

install_kconmon() {
    log "Installing kconmon-ng..."
    helm upgrade -i kconmon-ng "$PROJECT_DIR/charts/kconmon-ng" \
        -f "$PROJECT_DIR/hack/values-local.yaml" \
        -n "$NAMESPACE_APP" \
        --wait \
        --timeout 3m

    log "Waiting for all kconmon-ng pods..."
    kubectl wait --for=condition=ready pod \
        -l app.kubernetes.io/name=kconmon-ng \
        -n "$NAMESPACE_APP" \
        --timeout=120s
}

smoke_test() {
    log "Running smoke tests..."

    local controller_pod
    controller_pod=$(kubectl get pods \
        -l app.kubernetes.io/component=controller,app.kubernetes.io/name=kconmon-ng \
        -n "$NAMESPACE_APP" \
        -o jsonpath='{.items[0].metadata.name}')

    log "Pods:"
    kubectl get pods -n "$NAMESPACE_APP" -l app.kubernetes.io/name=kconmon-ng -o wide

    log "Controller logs (last 20 lines):"
    kubectl logs "$controller_pod" -n "$NAMESPACE_APP" --tail=20

    # Port-forward to controller (distroless has no shell utils)
    lsof -ti:18080 | xargs kill -9 2>/dev/null || true
    kubectl port-forward -n "$NAMESPACE_APP" "$controller_pod" 18080:8080 &
    local ctrl_pf=$!
    sleep 2

    log "Testing /healthz..."
    curl -sf http://localhost:18080/healthz && echo " OK" || echo " FAIL"

    log "Testing /readyz..."
    curl -sf http://localhost:18080/readyz && echo " OK" || echo " FAIL"

    log "Testing /api/v1/topology..."
    curl -sf http://localhost:18080/api/v1/topology | python3 -m json.tool 2>/dev/null | head -40 || echo " FAIL"
    echo

    log "Testing controller /metrics (first 30 kconmon_ng lines)..."
    curl -sf http://localhost:18080/metrics | grep "^kconmon_ng" | head -30
    echo

    kill "$ctrl_pf" 2>/dev/null; wait "$ctrl_pf" 2>/dev/null || true

    # Port-forward to first agent
    local agent_pod
    agent_pod=$(kubectl get pods \
        -l app.kubernetes.io/component=agent,app.kubernetes.io/name=kconmon-ng \
        -n "$NAMESPACE_APP" \
        -o jsonpath='{.items[0].metadata.name}')

    lsof -ti:18081 | xargs kill -9 2>/dev/null || true
    kubectl port-forward -n "$NAMESPACE_APP" "$agent_pod" 18081:8080 &
    local agent_pf=$!
    sleep 2

    log "Agent $agent_pod metrics (first 30 kconmon_ng lines)..."
    curl -sf http://localhost:18081/metrics | grep "^kconmon_ng" | head -30
    echo

    kill "$agent_pf" 2>/dev/null; wait "$agent_pf" 2>/dev/null || true

    log "Agent logs (last 20 lines):"
    kubectl logs "$agent_pod" -n "$NAMESPACE_APP" --tail=20
}

check_prometheus() {
    log "Checking Prometheus targets..."

    log "ServiceMonitors:"
    kubectl get servicemonitor -A 2>/dev/null | grep -E "kconmon|NAME" || echo "No kconmon-ng ServiceMonitors found"

    log "PrometheusRules:"
    kubectl get prometheusrule -A 2>/dev/null | grep -E "kconmon|NAME" || echo "No kconmon-ng PrometheusRules found"
}

import_dashboards() {
    log "Importing Grafana dashboards..."

    kubectl port-forward -n "$NAMESPACE_MONITORING" svc/monitoring-grafana 13000:80 &
    local pf_pid=$!
    sleep 3

    local grafana_url="http://localhost:13000"
    local ok=0 fail=0

    for f in "$PROJECT_DIR"/dashboards/*.json; do
        local name
        name=$(basename "$f")
        local status
        status=$(curl -s --max-time 30 -X POST "$grafana_url/api/dashboards/db" \
            -H "Content-Type: application/json" \
            -u admin:admin \
            -d "{\"dashboard\": $(cat "$f"), \"overwrite\": true}" \
            | python3 -c "import sys,json; print(json.load(sys.stdin).get('status','error'))" 2>/dev/null)

        if [[ "$status" == "success" ]]; then
            echo "  ✓ $name"
            ((ok++))
        else
            echo "  ✗ $name ($status)"
            ((fail++))
        fi
    done

    kill "$pf_pid" 2>/dev/null; wait "$pf_pid" 2>/dev/null || true

    log "Dashboards imported: $ok ok, $fail failed"
}

show_access() {
    log "Access URLs (run these in separate terminals):"
    echo
    echo "  Grafana (admin/admin):"
    echo "    kubectl port-forward -n $NAMESPACE_MONITORING svc/monitoring-grafana 3000:80"
    echo "    http://localhost:3000"
    echo
    echo "  Prometheus:"
    echo "    kubectl port-forward -n $NAMESPACE_MONITORING svc/monitoring-kube-prometheus-prometheus 9090:9090"
    echo "    http://localhost:9090"
    echo
    echo "  kconmon-ng Controller:"
    echo "    kubectl port-forward -n $NAMESPACE_APP svc/kconmon-ng-controller 8080:8080"
    echo "    http://localhost:8080/api/v1/topology"
    echo "    http://localhost:8080/metrics"
    echo
}

cluster_down() {
    log "Deleting minikube cluster '$PROFILE'..."
    minikube delete -p "$PROFILE"
}

status() {
    log "Cluster status:"
    minikube status -p "$PROFILE" 2>/dev/null || echo "Cluster not running"
    echo
    log "Nodes:"
    kubectl get nodes 2>/dev/null || true
    echo
    log "kconmon-ng pods:"
    kubectl get pods -l app.kubernetes.io/name=kconmon-ng -o wide 2>/dev/null || true
    echo
    log "Monitoring pods:"
    kubectl get pods -n "$NAMESPACE_MONITORING" 2>/dev/null || true
}

usage() {
    echo "Usage: $0 {up|down|status|smoke|urls|dashboards}"
    echo
    echo "  up         - Start cluster, build images, install monitoring + kconmon-ng, run smoke tests"
    echo "  down       - Delete the minikube cluster"
    echo "  status     - Show cluster and pod status"
    echo "  smoke      - Run smoke tests against running cluster"
    echo "  urls       - Show access URLs for Grafana, Prometheus, kconmon-ng"
    echo "  dashboards - Import Grafana dashboards via API"
    exit 1
}

# --- Main ---
check_deps

case "${1:-}" in
    up)
        cluster_up
        build_images
        install_monitoring
        install_kconmon
        sleep 15  # let agents register and run a few check cycles
        smoke_test
        check_prometheus
        import_dashboards
        show_access
        log "Local test environment is ready!"
        ;;
    down)
        cluster_down
        ;;
    status)
        status
        ;;
    smoke)
        smoke_test
        check_prometheus
        ;;
    urls)
        show_access
        ;;
    dashboards)
        import_dashboards
        ;;
    *)
        usage
        ;;
esac
