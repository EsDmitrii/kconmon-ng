#!/usr/bin/env python3
"""Reject duplicate keys in a JSON document.

json.load() silently keeps the LAST value for a repeated key, so a duplicate
turns an earlier definition into dead text that validates nothing. Only an
object_pairs_hook can see it.
"""
import json
import sys

def main(paths: list[str]) -> int:
    rc = 0
    for path in paths:
        dups: list[str] = []

        def hook(pairs, _dups=dups):
            seen = {}
            for k, v in pairs:
                if k in seen:
                    _dups.append(k)
                seen[k] = v
            return seen

        try:
            with open(path) as fh:
                json.load(fh, object_pairs_hook=hook)
        except json.JSONDecodeError as exc:
            print(f"{path}: invalid JSON: {exc}", file=sys.stderr)
            rc = 1
            continue
        if dups:
            for k in dups:
                print(f"{path}: duplicate key {k!r} — JSON keeps the last one, so an earlier "
                      f"definition is dead", file=sys.stderr)
            rc = 1
        else:
            print(f"{path}: no duplicate keys")
    return rc

if __name__ == "__main__":
    sys.exit(main(sys.argv[1:] or ["charts/kconmon-ng/values.schema.json"]))
