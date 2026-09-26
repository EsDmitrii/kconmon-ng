#!/usr/bin/env bash
# Waits for the kind-cluster state the e2e workflow needs and `helm --wait` does not give.
#
#   single-pod <component>...  exactly one pod per app.kubernetes.io/component: helm --wait returns
#                              while an old pod may still be Terminating, and a later items[0]
#                              would pick it.
#   crd <name>                 the CRD is Established: kubectl wait errors while a fresh CRD has no
#                              status.conditions yet, so it runs only once they exist.
#
# Usage: e2e/kind-wait.sh single-pod <component>... | crd <name>
set -euo pipefail

single_pod() {
  local component pods
  for component in "$@"; do
    for _ in $(seq 60); do
      pods=$(kubectl get pods -l "app.kubernetes.io/component=${component}" -o name | wc -l) || true
      [ "$pods" -eq 1 ] && continue 2
      sleep 2
    done
    echo "::error::${pods} ${component} pods after 120s, want exactly one"
    kubectl get pods -l "app.kubernetes.io/component=${component}" -o wide
    exit 1
  done
}

crd() {
  local established
  for _ in $(seq 60); do
    established=$(kubectl get crd "$1" \
      -o jsonpath='{.status.conditions[?(@.type=="Established")].status}' 2>/dev/null) || true
    [ "$established" = "True" ] && break
    sleep 1
  done
  kubectl wait --for condition=established "crd/$1" --timeout=60s
}

case "${1:-}" in
  single-pod) shift; single_pod "$@" ;;
  crd) crd "${2:?usage: $0 crd <name>}" ;;
  *) echo "usage: $0 single-pod <component>... | crd <name>" >&2; exit 2 ;;
esac
