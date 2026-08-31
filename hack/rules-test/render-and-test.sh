#!/usr/bin/env bash
# Renders the chart's PrometheusRule with EVERY alert enabled and runs promtool test rules
# against synthetic series. Catches what helm template and syntax lints cannot: expressions
# that only fail at EVALUATION time (v2.3.1's "vector cannot contain metrics with the same
# labelset" shipped to both production clusters through exactly that gap).
set -euo pipefail
cd "$(dirname "$0")/../.."
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

# NOTE: rendered to a file first — piping into a python that reads its PROGRAM from a heredoc
# would silently hand yaml an empty stdin (the heredoc wins the stdin fight).
helm template charts/kconmon-ng \
  --set prometheusRule.enabled=true \
  --set prometheusRule.zoneChecksFailing.enabled=true \
  --set prometheusRule.zoneLossHigh.enabled=true \
  > "$tmp/manifests.yaml"
python3 - "$tmp/manifests.yaml" "$tmp/rules.yaml" <<'PY'
import sys, yaml
src, out = sys.argv[1], sys.argv[2]
docs = [d for d in yaml.safe_load_all(open(src)) if d and d.get('kind') == 'PrometheusRule']
if not docs:
    sys.exit("no PrometheusRule rendered — the toggles above no longer enable it")
groups = [g for d in docs for g in d['spec']['groups']]
yaml.safe_dump({'groups': groups}, open(out, 'w'))
print(f"rendered {sum(len(g['rules']) for g in groups)} rules in {len(groups)} group(s)")
PY

sed "s|RULES_FILE|$tmp/rules.yaml|" hack/rules-test/tests.yaml > "$tmp/tests.yaml"
promtool test rules "$tmp/tests.yaml"
