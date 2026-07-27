#!/usr/bin/env python3
"""Execute the sealed-benchmark framework gates for real.

1. Run the leakage matrix (builder/oracle/evaluator isolation probes).
2. Validate all six target manifests against their structural schema.
3. Verify each frozen contract digest and print it.

Exit non-zero if any leakage probe fails to block the cheat, or if any
manifest/contract is invalid.

Usage:
  .venv/bin/python scripts/run-sealed-framework.py --output artifacts/v2/sealed
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from blender_vision.benchmarks.sealed import (  # noqa: E402
    TARGET_IDS,
    load_all_contracts,
    load_all_manifests,
    run_leakage_matrix,
    sealed_benchmarks_root,
    tree_digest,
)
from blender_vision.core.util import atomic_write_json, utc_now  # noqa: E402


def _print_matrix(matrix: list[dict[str, Any]]) -> int:
    print()
    print("LEAKAGE MATRIX")
    print("-" * 72)
    print(f"{'probe':<40} {'blocked':<10} {'result':<8}")
    print("-" * 72)
    failures = 0
    for row in matrix:
        print(
            f"{row['probe']:<40} {str(row['blocked']):<10} {row['result']:<8}"
        )
        if row["result"] != "PASS":
            failures += 1
            print(f"  detail: {row['detail']}")
        else:
            # One-line summary of why it was blocked.
            detail = row.get("detail", "")
            if detail:
                print(f"  blocked: {detail[:120]}")
    print("-" * 72)
    print(f"leakage probes: {len(matrix) - failures}/{len(matrix)} blocked as required")
    return failures


def _print_manifests(manifests: dict) -> int:
    print()
    print("MANIFEST VALIDATION")
    print("-" * 72)
    failures = 0
    for target_id in TARGET_IDS:
        manifest = manifests[target_id]
        try:
            manifest.validate_against_schema()
        except Exception as error:  # noqa: BLE001
            print(f"FAIL  {target_id}: {error}")
            failures += 1
            continue
        status = manifest.evidence_status.value
        blocked_n = len(manifest.blocked_requirements)
        print(
            f"OK    {target_id:<22} status={status:<9} "
            f"inputs={len(manifest.builder_inputs)} "
            f"hidden={len(manifest.hidden_evidence)} "
            f"blocked_reqs={blocked_n}"
        )
        for item in manifest.blocked_requirements:
            print(f"        BLOCKED {item.id}: {item.reason[:90]}...")
    print("-" * 72)
    return failures


def _print_contracts(contracts: dict) -> int:
    print()
    print("FROZEN CONTRACT DIGESTS")
    print("-" * 72)
    failures = 0
    root = sealed_benchmarks_root()
    for target_id in TARGET_IDS:
        contract = contracts[target_id]
        try:
            contract.verify()
            live_oracle = tree_digest(root / target_id / "oracle")
            live_eval = tree_digest(root / target_id / "evaluator")
            if live_oracle != contract.oracle_digest:
                raise ValueError("oracle tree no longer matches sealed digest")
            if live_eval != contract.evaluator_digest:
                raise ValueError("evaluator tree no longer matches sealed digest")
        except Exception as error:  # noqa: BLE001
            print(f"FAIL  {target_id}: {error}")
            failures += 1
            continue
        print(f"OK    {target_id:<22} {contract.digest}")
        print(f"        oracle     {contract.oracle_digest}")
        print(f"        evaluator  {contract.evaluator_digest}")
        print(f"        inputs     {contract.builder_inputs_digest}")
        print(f"        frozen_at  {contract.frozen_at}")
    print("-" * 72)
    return failures


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=ROOT / "artifacts" / "v2" / "sealed",
        help="Directory for the framework receipt (default: artifacts/v2/sealed)",
    )
    arguments = parser.parse_args()
    output = arguments.output.resolve()
    output.mkdir(parents=True, exist_ok=True)

    print("Sealed benchmark framework")
    print(f"benchmarks root: {sealed_benchmarks_root()}")
    print(f"output:          {output}")

    matrix = run_leakage_matrix(work_root=output / "leakage-work")
    leakage_failures = _print_matrix(matrix)

    manifests = load_all_manifests()
    manifest_failures = _print_manifests(manifests)

    contracts = load_all_contracts()
    contract_failures = _print_contracts(contracts)

    total_failures = leakage_failures + manifest_failures + contract_failures
    receipt = {
        "schema": "v2.sealed-framework-receipt/1",
        "completed_at": utc_now(),
        "leakage_matrix": matrix,
        "manifests": {
            target_id: {
                "evidence_status": manifests[target_id].evidence_status.value,
                "blocked_requirements": [
                    item.to_dict() for item in manifests[target_id].blocked_requirements
                ],
                "acceptance_thresholds": manifests[target_id].acceptance_thresholds,
            }
            for target_id in TARGET_IDS
        },
        "contracts": {
            target_id: {
                "digest": contracts[target_id].digest,
                "oracle_digest": contracts[target_id].oracle_digest,
                "evaluator_digest": contracts[target_id].evaluator_digest,
                "builder_inputs_digest": contracts[target_id].builder_inputs_digest,
                "frozen_at": contracts[target_id].frozen_at,
            }
            for target_id in TARGET_IDS
        },
        "failures": {
            "leakage": leakage_failures,
            "manifests": manifest_failures,
            "contracts": contract_failures,
            "total": total_failures,
        },
        "status": "PASS" if total_failures == 0 else "FAIL",
    }
    atomic_write_json(output / "framework.receipt.json", receipt)
    print()
    print(f"receipt: {output / 'framework.receipt.json'}")
    print(f"STATUS: {receipt['status']}")
    if total_failures:
        print(f"FAIL: {total_failures} gate(s) failed", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
