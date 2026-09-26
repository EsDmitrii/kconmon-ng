#!/usr/bin/env bash
# Fails a `go test -v` run (an e2e leg, CI's integration suite) whose log shows it tested nothing.
# Every env-gated test skips itself when its variable is missing, and a -run pattern that matches
# no test prints "no tests to run" and exits 0, so either would otherwise pass the run as a green
# no-op. With the leg's -run pattern, every '|' alternative must also have a passing top-level test:
# go test runs whatever the other alternatives match, so a renamed test would otherwise drop out of
# the leg without a trace. An alternative matching several tests is satisfied by any one of them,
# so a leg that must keep a particular test names it in full.
#
# Usage: e2e/leg-log-check.sh <go-test-v-log> <leg name> [flat Name|Name -run pattern]
set -euo pipefail

log=$1
leg=$2
pattern=${3:-}

if grep -- '--- SKIP' "$log"; then
  echo "::error::${leg}: a test skipped itself; check the step's env and -skip pattern"
  exit 1
fi
if grep -q 'no tests to run' "$log" || ! grep -q -- '--- PASS' "$log"; then
  echo "::error::${leg}: no test ran; check the step's -run pattern against the test names"
  exit 1
fi

if [ -n "$pattern" ]; then
  if [[ ! $pattern =~ ^[A-Za-z0-9_]+(\|[A-Za-z0-9_]+)*$ ]]; then
    echo "::error::${leg}: can only check a flat Name|Name -run pattern, got '${pattern}'"
    exit 1
  fi
  missing=()
  IFS='|' read -ra alternatives <<< "$pattern"
  for alt in "${alternatives[@]}"; do
    # -run matches unanchored, so the alternative may sit anywhere in a top-level test name;
    # subtest results are indented and do not count.
    grep -Eq -- "^--- PASS: [A-Za-z0-9_]*${alt}" "$log" || missing+=("$alt")
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    echo "::error::${leg}: no test passed for -run alternative(s) ${missing[*]}; check them against the test names"
    exit 1
  fi
fi
