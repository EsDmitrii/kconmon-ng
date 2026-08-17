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
    # openssl: the webhook encryption key. python3: JSON parsing in the smoke
    # step (the console's API answers are objects, not greppable lines).
    for cmd in minikube docker helm kubectl openssl python3 curl; do
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

# Unique tag per build: minikube's image-load cache silently keeps the old
# image when the tag is unchanged, so re-running `up` would deploy stale code.
IMAGE_TAG="local-$(date +%Y%m%d%H%M%S)"

build_images() {
    log "Building Docker images locally (tag: $IMAGE_TAG)..."
    docker build --target agent -t "kconmon-ng-agent:$IMAGE_TAG" "$PROJECT_DIR"
    docker build --target controller -t "kconmon-ng-controller:$IMAGE_TAG" "$PROJECT_DIR"
    # The console lives in its OWN Dockerfile (Makefile's docker-build does the same).
    docker build -f "$PROJECT_DIR/Dockerfile.console" \
        --target console -t "kconmon-ng-console:$IMAGE_TAG" "$PROJECT_DIR"

    log "Loading images into minikube (all nodes)..."
    minikube image load "kconmon-ng-agent:$IMAGE_TAG" -p "$PROFILE"
    minikube image load "kconmon-ng-controller:$IMAGE_TAG" -p "$PROFILE"
    minikube image load "kconmon-ng-console:$IMAGE_TAG" -p "$PROFILE"

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

# Both console prerequisites, applied BEFORE the Helm install.
install_console_secrets() {
    log "Applying the local Postgres fixture (console database)..."
    kubectl apply -n "$NAMESPACE_APP" -f "$PROJECT_DIR/hack/postgres-local.yaml"
    kubectl wait --for=condition=ready pod \
        -l app=kconmon-local-postgres \
        -n "$NAMESPACE_APP" \
        --timeout=180s

    # Generated here and thrown away with the cluster — a fixed key checked into the repo would be a real key in
    # version control.
    log "Creating the webhook encryption key Secret..."
    kubectl create secret generic kconmon-local-webhooks-key \
        --from-literal=encryptionKey="$(openssl rand -base64 32)" \
        -n "$NAMESPACE_APP" \
        --dry-run=client -o yaml | kubectl apply -f -
}

install_kconmon() {
    log "Installing kconmon-ng..."
    helm upgrade -i kconmon-ng "$PROJECT_DIR/charts/kconmon-ng" \
        -f "$PROJECT_DIR/hack/values-local.yaml" \
        --set-string agent.image.tag="$IMAGE_TAG" \
        --set-string controller.image.tag="$IMAGE_TAG" \
        --set-string console.image.tag="$IMAGE_TAG" \
        -n "$NAMESPACE_APP" \
        --wait \
        --timeout 5m

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
    # || true: head closes the pipe early -> SIGPIPE (141) would kill the script under pipefail
    curl -sf http://localhost:18080/metrics | grep "^kconmon_ng" | head -30 || true
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
    curl -sf http://localhost:18081/metrics | grep "^kconmon_ng" | head -30 || true
    echo

    kill "$agent_pf" 2>/dev/null; wait "$agent_pf" 2>/dev/null || true

    log "Agent logs (last 20 lines):"
    kubectl logs "$agent_pod" -n "$NAMESPACE_APP" --tail=20
}

# --- Console smoke -----------------------------------------------------------

# Local port for the console port-forward (18080 controller, 18081 agent,
# 13000 Grafana are already taken above).
CONSOLE_PORT=18082
# The ONE PrometheusRule object the console owns. The chart default
# (console.alerting.bundleName), left unset in hack/values-local.yaml on
# purpose -- changing it there means changing it here.
CONSOLE_BUNDLE="kconmon-ng-console-rules"
# Alert-rule sync budget: 30 polls x 3s, the same 90s e2e/console_test.go
# allows. It is wide because the reconcile is jittered and the FIRST pass after
# a write legitimately reports drift.
SYNC_POLLS=30
SYNC_SLEEP=3

# Failures are counted, never fatal: a smoke run that dies on its first
# problem hides the other five, and `up` still has URLs to print. The count is
# turned into the exit code at the very end.
SMOKE_FAILURES=0
pass() { printf '  PASS: %s\n' "$1"; }
fail() { printf '  FAIL: %s\n' "$1"; SMOKE_FAILURES=$((SMOKE_FAILURES + 1)); }

# console_api METHOD PATH [JSON_BODY] -- prints the HTTP status code, writes the
# response body to $API_BODY. curl's own "000" stands for "never got an answer".
API_BODY=""
console_api() {
    local method="$1" path="$2" body="${3:-}"
    local url="http://localhost:$CONSOLE_PORT$path"
    if [[ -n "$body" ]]; then
        curl -sS -o "$API_BODY" -w '%{http_code}' \
            -X "$method" -H 'Content-Type: application/json' -d "$body" "$url" 2>/dev/null || true
    else
        curl -sS -o "$API_BODY" -w '%{http_code}' -X "$method" "$url" 2>/dev/null || true
    fi
}

# json_field KEY -- one top-level string field out of $API_BODY, or "" if the
# body is not an object / the key is absent. python3 because the console
# answers objects, and grep on JSON is how a smoke test starts lying.
json_field() {
    python3 -c 'import json,sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
print(d.get(sys.argv[1], "") if isinstance(d, dict) else "")' "$1" <"$API_BODY" 2>/dev/null || true
}

smoke_console() {
    log "Console smoke tests..."

    local console_pod
    console_pod=$(kubectl get pods \
        -l app.kubernetes.io/component=console,app.kubernetes.io/name=kconmon-ng \
        -n "$NAMESPACE_APP" \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    if [[ -z "$console_pod" ]]; then
        fail "no console pod found (is console.enabled still true in hack/values-local.yaml?)"
        return 0
    fi

    if kubectl wait --for=condition=ready "pod/$console_pod" \
        -n "$NAMESPACE_APP" --timeout=120s >/dev/null 2>&1; then
        pass "console pod $console_pod is Ready"
    else
        fail "console pod $console_pod never became Ready"
        log "Console logs (last 40 lines):"
        kubectl logs "$console_pod" -n "$NAMESPACE_APP" --tail=40 || true
        return 0
    fi

    API_BODY=$(mktemp)
    lsof -ti:"$CONSOLE_PORT" | xargs kill -9 2>/dev/null || true
    kubectl port-forward -n "$NAMESPACE_APP" "$console_pod" "$CONSOLE_PORT:8080" >/dev/null 2>&1 &
    local console_pf=$!
    sleep 3

    local status probe
    for probe in healthz readyz; do
        status=$(console_api GET "/$probe")
        if [[ "$status" == "200" ]]; then
            pass "console /$probe -> 200"
        else
            fail "console /$probe -> $status"
        fi
    done

    # Proxied through to the controller, so a 200 here proves the console
    # resolved console.controller.url (chart-derived) and the controller
    # answered -- two links of the chain in one call.
    status=$(console_api GET /api/v1/topology)
    if [[ "$status" == "200" ]]; then
        pass "console /api/v1/topology -> 200"
        head -c 400 "$API_BODY"; echo
    else
        fail "console /api/v1/topology -> $status"
        head -c 400 "$API_BODY"; echo
    fi

    alerting_round

    kill "$console_pf" 2>/dev/null; wait "$console_pf" 2>/dev/null || true
    rm -f "$API_BODY"
}

# The round trip, end to end against a REAL prometheus-operator (not just its CRD, which is all the e2e job has).
alerting_round() {
    log "Alerting round trip (POST rule -> syncStatus=synced -> PrometheusRule)..."

    local rule_name
    rule_name="local-smoke-$(date +%s)"
    # kind "raw" is verbatim -- there is no PromQL parser in the console.
    local body
    body=$(printf '{"name":"%s","kind":"raw","params":{"expr":"vector(1) > 0"},%s}' \
        "$rule_name" '"severity":"warning","forNs":0,"enabled":true')

    local status
    status=$(console_api POST /api/v1/alert-rules "$body")
    if [[ "$status" != "201" ]]; then
        if [[ "$status" == "503" ]]; then
            fail "POST /api/v1/alert-rules -> 503: the console has no database. Check the \
kconmon-local-postgres pod and database.* in hack/values-local.yaml"
        else
            fail "POST /api/v1/alert-rules -> $status (expected 201)"
        fi
        head -c 400 "$API_BODY"; echo
        return 0
    fi

    local rule_id
    rule_id=$(json_field id)
    if [[ -z "$rule_id" ]]; then
        fail "POST /api/v1/alert-rules answered 201 with no rule id"
        return 0
    fi
    pass "alert rule $rule_name created (id $rule_id)"

    local sync_status="" sync_message="" synced="" i
    for i in $(seq 1 "$SYNC_POLLS"); do
        status=$(console_api GET "/api/v1/alert-rules/$rule_id")
        if [[ "$status" == "200" ]]; then
            sync_status=$(json_field syncStatus)
            sync_message=$(json_field syncMessage)
            if [[ "$sync_status" == "synced" ]]; then
                synced="yes"
                break
            fi
            if [[ "$sync_status" == "error" ]]; then
                echo "    sync error (attempt $i): $sync_message"
            fi
        fi
        # Re-kicking is the model, not impatience: the reconciler compares the live bundle against the desired one
        # BEFORE applying.
        console_api POST "/api/v1/alert-rules/$rule_id/sync" >/dev/null
        sleep "$SYNC_SLEEP"
    done

    if [[ -n "$synced" ]]; then
        pass "alert rule reached syncStatus=synced"
    else
        fail "alert rule stuck at syncStatus=${sync_status:-<unknown>} after \
$((SYNC_POLLS * SYNC_SLEEP))s: ${sync_message:-no message}"
    fi

    if kubectl get prometheusrule -n "$NAMESPACE_APP" "$CONSOLE_BUNDLE" >/dev/null 2>&1; then
        pass "PrometheusRule/$CONSOLE_BUNDLE exists in namespace $NAMESPACE_APP"
        log "Bundle expressions:"
        kubectl get prometheusrule -n "$NAMESPACE_APP" "$CONSOLE_BUNDLE" \
            -o jsonpath='{range .spec.groups[*].rules[*]}{.alert}{"  "}{.expr}{"\n"}{end}' || true
        echo
    else
        fail "PrometheusRule/$CONSOLE_BUNDLE not found in namespace $NAMESPACE_APP \
(the console never applied its bundle -- check the console logs for a forbidden or crd-missing cause)"
    fi

    # Housekeeping: a fresh name per run would otherwise pile up rules in the
    # database across repeated `smoke` invocations. The bundle object survives
    # (it is the console's, not this run's) and simply loses this group.
    console_api DELETE "/api/v1/alert-rules/$rule_id" >/dev/null
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

    lsof -ti:13000 | xargs kill -9 2>/dev/null || true
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
            ok=$((ok+1))  # not ((ok++)): returns pre-increment value, so 0 -> exit 1 under set -e
        else
            echo "  ✗ $name ($status)"
            fail=$((fail+1))
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
    echo "  kconmon-ng Console (anonymous auth, admin role - no login):"
    echo "    kubectl port-forward -n $NAMESPACE_APP svc/kconmon-ng-console 8081:8080"
    echo "    http://localhost:8081/            # overview"
    echo "    http://localhost:8081/matrix      # node-to-node matrix"
    echo "    http://localhost:8081/investigate # per-pair drilldown, MTR traces"
    echo "    http://localhost:8081/alerting    # alert rules -> PrometheusRule"
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
    log "Console Postgres fixture:"
    kubectl get pods -l app=kconmon-local-postgres -o wide 2>/dev/null || true
    echo
    log "Monitoring pods:"
    kubectl get pods -n "$NAMESPACE_MONITORING" 2>/dev/null || true
}

usage() {
    echo "Usage: $0 {up|down|status|smoke|urls|dashboards}"
    echo
    echo "  up         - Start cluster, build images, install monitoring, Postgres + secrets,"
    echo "               kconmon-ng (agents + controller + console), run smoke tests"
    echo "  down       - Delete the minikube cluster"
    echo "  status     - Show cluster and pod status"
    echo "  smoke      - Run smoke tests against running cluster (includes the console"
    echo "               health checks and the alerting round trip)"
    echo "  urls       - Show access URLs for Grafana, Prometheus, kconmon-ng, Console"
    echo "  dashboards - Import Grafana dashboards via API"
    exit 1
}

# Non-zero exit when any PASS/FAIL check failed. Reported at the very end so
# `up` still gets to print its URLs and `smoke` still runs every check.
smoke_verdict() {
    if [[ "$SMOKE_FAILURES" -gt 0 ]]; then
        printf '\n\033[1;31m%s smoke check(s) FAILED\033[0m\n' "$SMOKE_FAILURES" >&2
        exit 1
    fi
    printf '\n\033[1;32mAll smoke checks passed\033[0m\n'
}

# --- Main ---
check_deps

case "${1:-}" in
    up)
        cluster_up
        build_images
        install_monitoring
        install_console_secrets
        install_kconmon
        sleep 15  # let agents register and run a few check cycles
        smoke_test
        smoke_console
        check_prometheus
        import_dashboards
        show_access
        log "Local test environment is ready!"
        smoke_verdict
        ;;
    down)
        cluster_down
        ;;
    status)
        status
        ;;
    smoke)
        smoke_test
        smoke_console
        check_prometheus
        smoke_verdict
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
