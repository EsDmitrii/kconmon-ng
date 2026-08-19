#!/usr/bin/env python3
"""Reject a chart value that nothing in the chart reads.

Helm accepts any --set path and any -f key, whether or not the chart defines
it. A renamed value therefore fails SILENTLY: `--set console.database.mode=disabled`
survived chart 2.0.0's move to a top-level `database:` and the E2E job spent
every run asserting degraded-mode behaviour against a console that still had its
database. values.schema.json does not catch it either — it validates the merged
result, and an unknown key is simply not mentioned there.

So this walks the other way: every key the CI workflows and the E2E values files
set must exist in charts/kconmon-ng/values.yaml.

Lists are not descended into: a value under a list index is data the chart
copies through (annotations, tolerations), not a path the chart names.
"""
import pathlib
import re
import sys

import yaml

CHART_VALUES = pathlib.Path("charts/kconmon-ng/values.yaml")
WORKFLOWS = sorted(pathlib.Path(".github/workflows").glob("*.yaml"))
VALUES_FILES = sorted(pathlib.Path("e2e/testdata").glob("*values*.yaml"))

SET_KEY = re.compile(r"--set\s+([A-Za-z0-9_.]+)=")


def defined(base: dict, path: str) -> bool:
    node = base
    for part in path.split("."):
        if not isinstance(node, dict) or part not in node:
            return False
        node = node[part]
    return True


def keys_of(node, prefix: str = ""):
    if not isinstance(node, dict):
        return
    for key, value in node.items():
        path = f"{prefix}.{key}".lstrip(".")
        yield path
        yield from keys_of(value, path)


def main() -> int:
    base = yaml.safe_load(CHART_VALUES.read_text())
    rc = 0

    for workflow in WORKFLOWS:
        for path in sorted(set(SET_KEY.findall(workflow.read_text()))):
            if not defined(base, path):
                print(f"{workflow}: --set {path} names no value in {CHART_VALUES}; helm accepts it "
                      f"and the chart ignores it", file=sys.stderr)
                rc = 1

    for values in VALUES_FILES:
        doc = yaml.safe_load(values.read_text()) or {}
        for path in keys_of(doc):
            if not defined(base, path):
                print(f"{values}: {path} names no value in {CHART_VALUES}; helm accepts it and the "
                      f"chart ignores it", file=sys.stderr)
                rc = 1

    if rc == 0:
        print(f"every --set path and {CHART_VALUES.name} override is a value the chart defines")
    return rc


if __name__ == "__main__":
    sys.exit(main())
