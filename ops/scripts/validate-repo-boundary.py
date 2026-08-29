#!/usr/bin/env python3
"""merc's repository boundary: what merc owns, and what it must not absorb.

VisionMCP is a separate product with its own repository and history. It is not a
dependency of merc and merc does not build, test or ship it. The risk is not
that someone deliberately merges it -- it is that a `git add -A` run from the
wrong directory, or a helpful "move this here", quietly pulls thousands of files
into merc's tree, where they would be counted as merc's owned LOC and reviewed
as merc's code.

This asserts the boundary holds, and reports merc's owned LOC so the figure has
one source rather than being recounted by hand each time it is quoted.
"""

import json
import re
import subprocess
import sys

# Paths and identifiers that belong to VisionMCP. Matching is on path segments so
# an unrelated file that merely contains the substring (ops/monitoring/grafana/
# proVISIONing/) does not trip it.
FOREIGN_SEGMENTS = re.compile(
    r"(^|/)(visionmcp|VisionMCP|vision-mcp|blender-vision-mcp|ocular)(/|$|[-_.])",
    re.IGNORECASE,
)


def main() -> int:
    tracked = [p for p in subprocess.run(
        ["git", "ls-files"], capture_output=True, text=True, check=True
    ).stdout.split("\n") if p]

    foreign = [p for p in tracked if FOREIGN_SEGMENTS.search(p)]

    try:
        with open("evidence/census/CODEBASE_CENSUS.json") as fh:
            census = json.load(fh)
        owned_loc = census["ownership"]["global_owned_loc"]
        scope = census["scope"]
    except (OSError, KeyError, json.JSONDecodeError) as exc:
        print(f"repo boundary: cannot read the census: {exc}", file=sys.stderr)
        return 2

    report = {
        "schema_version": 1,
        "kind": "repo_boundary",
        "tracked_files": len(tracked),
        "owned_loc": owned_loc,
        "census_scope": scope,
        "foreign_paths": foreign,
        "boundary_clean": not foreign,
        "note": ("VisionMCP is a separate repository. merc neither tracks nor builds it, "
                 "and its LOC is not merc's."),
    }
    with open("ops/repo-boundary.json", "w") as fh:
        json.dump(report, fh, indent=2)
        fh.write("\n")

    print(f"repo boundary: {len(tracked)} tracked files, owned LOC {owned_loc}, "
          f"VisionMCP paths {len(foreign)}")
    if foreign:
        for p in foreign[:20]:
            print(f"  FOREIGN {p}")
        print("  merc must not track VisionMCP; it is a separate repository and "
              "its lines are not merc's owned LOC")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
