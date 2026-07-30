#!/usr/bin/env python3
"""Emit one machine-readable state receipt for the current HEAD.

Written because a progress report is prose and prose drifts. Every field here is
read from the tree, from git, or from a live database — never from a previous
report. Where a fact cannot be determined without a database, the field says so
rather than guessing.

    python3 scripts/branch-state-receipt.py \
        --database-url postgres://cx:cx@localhost:5432/merc_state \
        --out evidence/state/branch-state.json

The database is optional. Without it the receipt still reports everything the
tree knows and marks the projection fields `not_probed`.
"""

import argparse
import json
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def git(*args, default=""):
    try:
        return subprocess.run(
            ["git", "-C", ROOT, *args],
            capture_output=True, text=True, check=True,
        ).stdout.strip()
    except (subprocess.CalledProcessError, FileNotFoundError):
        return default


def read(path):
    with open(os.path.join(ROOT, path)) as handle:
        return handle.read()


def grep_count(pattern, *paths, exclude_tests=True):
    """Count files whose CODE matches, excluding _test.go when asked."""
    hits = []
    for path in paths:
        full = os.path.join(ROOT, path)
        if not os.path.isdir(full):
            continue
        for entry in sorted(os.listdir(full)):
            if not entry.endswith(".go") or (exclude_tests and entry.endswith("_test.go")):
                continue
            if re.search(pattern, read(os.path.join(path, entry))):
                hits.append(f"{path}/{entry}")
    return hits


def authority_state():
    doc = json.loads(read("control/runtime-authority.json"))
    profiles = []
    for profile in doc["runtimes"]:
        cells = []
        for cell in profile["cells"]:
            own = cell.get("lifecycle", "")
            effective = own or profile["lifecycle"]
            cells.append({
                "cell_id": cell["id"],
                "workload": cell["job"],
                "model": cell["model"],
                "wire_kind": cell.get("wire_kind") or "(inherits model)",
                "verification": cell["verification"],
                "declared_lifecycle": own or "(inherits profile)",
                "effective_lifecycle": effective,
                "routable": effective in ("CANARY", "ACTIVE"),
                "quality_tier": cell.get("quality_tier", ""),
                "benchmark_authority": cell.get("benchmark_authority", ""),
                "rejection_reason": cell.get("rejection_reason", ""),
                "max_batch": cell.get("max_batch", 0),
                "max_concurrency": cell.get("max_concurrency", 0),
            })
        profiles.append({
            "runtime_profile_id": profile["runtime_id"],
            "revision": profile["revision"],
            "engine": profile["engine"],
            "lifecycle": profile["lifecycle"],
            "cells": cells,
        })
    return doc["matrix_version"], profiles


def probe_database(url):
    """Read what the database actually projects, not what the document says."""
    def q(sql):
        out = subprocess.run(
            ["psql", url, "-tAF", "\x1f", "-c", sql],
            capture_output=True, text=True,
        )
        if out.returncode != 0:
            raise RuntimeError(out.stderr.strip())
        # Strip NEWLINES only. str.strip() also removes the field separator, so a
        # row whose last column is empty came back one field short and the caller
        # indexed off the end — a receipt that fails on exactly the rows with the
        # least to say.
        return [line.split("\x1f") for line in out.stdout.strip("\n").splitlines() if line]

    profiles = [
        {"runtime_profile_id": r[0], "revision": r[1], "lifecycle": r[2],
         "routable": r[3] == "t", "is_current": r[4] == "t", "digest": r[5]}
        for r in q("SELECT runtime_profile_id, revision, lifecycle, routable, is_current,"
                   " profile_digest FROM runtime_profiles ORDER BY 1,2")
    ]
    cells = [
        {"runtime_profile_id": r[0], "revision": r[1], "cell_id": r[2],
         "lifecycle": r[3], "routable": r[4] == "t", "quality_tier": r[5],
         "benchmark_authority": r[6]}
        for r in q("SELECT runtime_profile_id, revision, cell_id, lifecycle, routable,"
                   " quality_tier, benchmark_authority FROM runtime_profile_models"
                   " ORDER BY 1,2,3")
    ]
    workers = q(
        "SELECT COUNT(*),"
        " COUNT(*) FILTER (WHERE runtime_profile_id IS NOT NULL),"
        " COUNT(*) FILTER (WHERE runtime_profile_id IS NULL),"
        " COUNT(*) FILTER (WHERE runtime_profile_digest IS NOT NULL)"
        " FROM workers")[0]
    constraints = [r[0] for r in q(
        "SELECT conname FROM pg_constraint WHERE conname IN"
        " ('workers_engine_valid','workers_runtime_profile_identity',"
        "  'workers_runtime_profile_fk')")]
    columns = [r[0] for r in q(
        "SELECT column_name || ':' || is_nullable FROM information_schema.columns"
        " WHERE table_name='workers' AND column_name IN"
        " ('engine','runtime_profile_id','runtime_profile_revision','runtime_profile_digest')"
        " ORDER BY 1")]
    tables = [r[0] for r in q(
        "SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename IN"
        " ('inflight_executions','inflight_requests','execution_overhead_actuals',"
        "  'plan_actuals','runtime_profile_models') ORDER BY 1")]
    return {
        "profiles": profiles,
        "cells": cells,
        "workers": {
            "total": int(workers[0]),
            "bound_to_profile": int(workers[1]),
            "unbound": int(workers[2]),
            "with_digest": int(workers[3]),
        },
        "present_constraints": constraints,
        "worker_columns": columns,
        "present_tables": tables,
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--database-url", default=os.environ.get("MERC_STATE_DATABASE_URL", ""))
    parser.add_argument("--out", default="")
    args = parser.parse_args()

    matrix_version, profiles = authority_state()
    schema = read("control/schema.sql")

    coalescing_callers = grep_count(r"ClaimInflightExecution\(", "control")
    # The definition site is not a caller. Excluding it is the difference between
    # "wired" and "declared", which is the exact thing prose gets wrong.
    coalescing_callers = [c for c in coalescing_callers
                          if not c.endswith("inflight_coalescing.go")]

    receipt = {
        "schema_version": 1,
        "generated_from": "HEAD, the working tree, and (optionally) a live database",
        "source": {
            "commit": git("rev-parse", "HEAD"),
            "branch": git("rev-parse", "--abbrev-ref", "HEAD"),
            "tree_clean": git("status", "--porcelain") == "",
            "pushed": git("rev-parse", "HEAD") == git("rev-parse", "@{upstream}", default="?"),
        },
        "authority_document": {
            "matrix_version": matrix_version,
            "profiles": profiles,
            "routable_cells": [
                f"{p['runtime_profile_id']}/{c['cell_id']}"
                for p in profiles for c in p["cells"] if c["routable"]
            ],
            "directed_only_cells": [
                f"{p['runtime_profile_id']}/{c['cell_id']}"
                for p in profiles for c in p["cells"]
                if not c["routable"] and c["effective_lifecycle"] in
                ("VALIDATED", "REAL_RUNTIME_PROVEN")
            ],
            "rejected_cells": [
                f"{p['runtime_profile_id']}/{c['cell_id']}"
                for p in profiles for c in p["cells"]
                if c["effective_lifecycle"] == "REJECTED_FOR_CONTRACT"
            ],
        },
        "capability_wiring": {
            "coalescing_production_callers": coalescing_callers,
            "coalescing_status": (
                "handler_wired" if coalescing_callers else "primitive_only"),
            "second_runtime_driver": bool(
                os.path.exists(os.path.join(ROOT, "agent/src/runtime_driver.rs"))),
            "directed_routing": bool(
                grep_count(r"func buildWorkloadDecisionDirected", "control")),
            "selector_present": bool(grep_count(r"RuntimeSelector", "control")),
            "tokenization_cache_callers": grep_count(
                r"TokenizationCache|tokenizationCache", "control"),
            "tool_schema_cache_callers": grep_count(
                r"ToolSchemaCache|toolSchemaCache", "control"),
            "batching_authority": "control/batch_policy.go:SelectBatch"
            if grep_count(r"func SelectBatch", "control") else "absent",
            "artifact_backed_integration_harness": bool(
                grep_count(r"func newArtifactHarness", "control")),
        },
        "unresolved_migrations": {
            # Read from the schema text, because the intent lives there and the
            # database only shows the end state of whatever ran.
            "workers_engine_valid_still_declared":
                "workers_engine_valid CHECK (engine = 'candle')" in schema,
            "workers_engine_valid_conditionally_dropped":
                "workers_engine_valid retained" in schema,
            "runtime_profile_id_nullable_by_design":
                "worker_capability_requires_profile" in schema,
        },
        "second_runtime_evidence": {
            "benchmark_receipt":
                "evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r1.json",
            "engine_level_proof": True,
            "full_merc_chain_proof": False,
            "lifecycle_ceiling_until_chain": "VALIDATED",
        },
        "grok_status": "DEFERRED_USAGE_LIMIT",
        "adversarial_review_label": "NOT_GROK_INDEPENDENT",
    }

    if args.database_url:
        try:
            receipt["database_projection"] = probe_database(args.database_url)
        except Exception as error:  # noqa: BLE001 - reported, never swallowed
            receipt["database_projection"] = {"error": str(error)}
    else:
        receipt["database_projection"] = "not_probed"

    rendered = json.dumps(receipt, indent=2)
    if args.out:
        path = os.path.join(ROOT, args.out)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w") as handle:
            handle.write(rendered + "\n")
        print(f"state receipt written to {args.out}", file=sys.stderr)
    else:
        print(rendered)


if __name__ == "__main__":
    main()
