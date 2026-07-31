#!/usr/bin/env python3
"""Render `go test -json` output the way `go test` would have printed it.

`make ci` records the suite once, as JSON, so that the skip gate can read the
same run instead of executing the whole suite a second time. Two runs cost about
twenty-eight minutes on a host with object storage and a local engine, and the
second one inherited the first one's rows in the shared database -- which is its
own failure mode, not a slower version of the same check.

Nothing here decides anything. It exists so a human reading CI output still sees
package results and failing test names rather than a JSON stream.
"""
import json
import sys
from collections import defaultdict

if len(sys.argv) != 2:
    print("usage: summarize-go-test-json.py <go-test-json-log>", file=sys.stderr)
    raise SystemExit(2)

output = defaultdict(list)
package = {}
failed = []
skipped = 0

with open(sys.argv[1], encoding="utf-8", errors="replace") as log:
    for line in log:
        try:
            event = json.loads(line)
        except ValueError:
            # `go test -json` interleaves non-JSON build errors; show them.
            sys.stdout.write(line)
            continue
        action, test, pkg = event.get("Action"), event.get("Test"), event.get("Package", "")
        if action == "output" and test:
            output[(pkg, test)].append(event.get("Output", ""))
        elif action == "fail" and test:
            failed.append((pkg, test))
        elif action == "skip" and test:
            skipped += 1
        elif action in {"pass", "fail", "skip"} and not test:
            package[pkg] = (action, event.get("Elapsed", 0.0))

for pkg, test in failed:
    print(f"--- FAIL: {test} ({pkg})")
    for chunk in output[(pkg, test)]:
        sys.stdout.write(chunk)

for pkg, (action, elapsed) in sorted(package.items()):
    label = {"pass": "ok  ", "fail": "FAIL", "skip": "?   "}[action]
    print(f"{label}\t{pkg}\t{elapsed:.3f}s")

print(f"({len(failed)} failed, {skipped} skipped)")
