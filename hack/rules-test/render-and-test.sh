#!/usr/bin/env bash
# Renders the chart's PrometheusRule with every alert enabled and runs promtool test rules against
# synthetic series. Catches expressions that fail only at evaluation time ("vector cannot contain
# metrics with the same labelset"), which helm template and promtool check never see.
set -euo pipefail
cd "$(dirname "$0")/../.."
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

# Prints the rule groups the chart at $1 renders with prometheusRule.enabled and any extra flags.
render_rules() {
  local chart=$1; shift
  helm template "$chart" --set prometheusRule.enabled=true "$@" | python3 -c '
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d and d.get("kind") == "PrometheusRule"]
if not docs:
    sys.exit("no PrometheusRule rendered")
print(yaml.safe_dump({"groups": [g for d in docs for g in d["spec"]["groups"]]}))'
}

# The external-agent ScrapeConfig runs under a custom job name that lacks "agent-external", so
# KconmonExternalAgentDown must cover it as well as the default and plain-Prometheus jobs.
render_rules charts/kconmon-ng \
  --set prometheusRule.externalAgentDown.enabled=true \
  --set controller.externalGateway.enabled=true \
  --set controller.externalGateway.tls.secretName=gw-tls \
  --set controller.externalGateway.bootstrapToken.secretName=gw-token \
  --set scrapeConfig.externalAgents.enabled=true \
  --set scrapeConfig.externalAgents.jobName=bare.hosts \
  > "$tmp/rules.yaml"

sed "s|RULES_FILE|$tmp/rules.yaml|" hack/rules-test/tests.yaml > "$tmp/tests.yaml"
promtool test rules "$tmp/tests.yaml"

# `helm upgrade --reuse-values` renders the new templates over the OLD release's values: rule
# blocks added after 2.4.x are absent, not defaulted. Emulate that by deleting them from a copy of
# the chart's values.yaml; the rendered rules must not change. Add every rule block a release
# introduces to NEW_BLOCKS.
NEW_BLOCKS=(pathMtuBlackHole nodeUnreachable nodeIsolated)
cp -R charts/kconmon-ng "$tmp/chart-reuse"
python3 - "$tmp/chart-reuse/values.yaml" "${NEW_BLOCKS[@]}" <<'PY'
import sys, yaml
path, blocks = sys.argv[1], sys.argv[2:]
values = yaml.safe_load(open(path))
for b in blocks:
    del values['prometheusRule'][b]
yaml.safe_dump(values, open(path, 'w'), sort_keys=False)
PY
render_rules charts/kconmon-ng > "$tmp/rules-fresh.yaml"
render_rules "$tmp/chart-reuse" > "$tmp/rules-reuse.yaml"
if ! diff -u "$tmp/rules-fresh.yaml" "$tmp/rules-reuse.yaml"; then
  echo "rules rendered over values without [${NEW_BLOCKS[*]}] differ from a fresh install: a --reuse-values upgrade from 2.4.x would lose them" >&2
  exit 1
fi
echo "reuse-values render: rules identical without [${NEW_BLOCKS[*]}]"
