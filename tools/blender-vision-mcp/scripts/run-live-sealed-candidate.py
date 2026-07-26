#!/usr/bin/env python3
"""Create, rebuild, sandbox, and seal a fresh NOCTURNE acceptance candidate."""

from __future__ import annotations

import argparse
import json
import shutil
from pathlib import Path

from blender_vision.benchmarks.nocturne import SealedBuilderRunner
from blender_vision.benchmarks.nocturne_app import seal_nocturne_candidate


def ignore(path: str, names: list[str]) -> set[str]:
    current = Path(path)
    ignored = {
        name
        for name in names
        if name in {"node_modules", "dist", "data", ".DS_Store"}
    }
    if current.name == ".visionmcp":
        ignored.add("nocturne-build.receipt.json")
    return ignored


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--candidate", type=Path, required=True)
    parser.add_argument("--packet", type=Path, required=True)
    parser.add_argument("--oracle", type=Path, required=True)
    parser.add_argument("--oracle-source", type=Path, required=True)
    parser.add_argument("--builder-output", type=Path, required=True)
    parser.add_argument("--contract", type=Path, required=True)
    args = parser.parse_args()
    source = args.source.expanduser().resolve()
    candidate = args.candidate.expanduser().resolve()
    packet = args.packet.expanduser().resolve()
    oracle = args.oracle.expanduser().resolve()
    oracle_source = args.oracle_source.expanduser().resolve()
    builder_output = args.builder_output.expanduser().resolve()
    contract = args.contract.expanduser().resolve()
    if candidate.exists() or builder_output.exists():
        raise SystemExit("candidate and builder output must both be new")
    shutil.copytree(source, candidate, ignore=ignore)
    canary = (oracle / "ORACLE_CANARY.txt").read_text(encoding="utf-8")
    command = [
        "/bin/zsh",
        "-lc",
        (
            "trap '/bin/rm -rf node_modules dist data' EXIT; "
            "/Applications/Blender.app/Contents/MacOS/Blender "
            "--background --factory-startup --disable-autoexec --python-exit-code 1 "
            "--python 3d/build_candidate.py "
            "&& npm ci "
            "&& npm run verify"
        ),
    ]
    builder = SealedBuilderRunner(contract).run(
        builder_root=candidate,
        packet_root=packet,
        oracle_root=oracle,
        oracle_source_root=oracle_source,
        oracle_canary=canary,
        command=command,
        output_root=builder_output,
        timeout_seconds=1200,
    )
    if builder.status != "PASS":
        print(json.dumps(builder.model_dump(mode="json"), indent=2, sort_keys=True))
        return 1
    attempts = [
        (f"attempt-{index:03d}", "FAILED", f"failed-attempts/attempt-{index:03d}.json")
        for index in range(1, 8)
    ]
    attempts.append(("attempt-008", "ACCEPTED", ".visionmcp/attempt-008.json"))
    receipt = seal_nocturne_candidate(
        candidate_root=candidate,
        packet_root=packet,
        builder_condition="live-sandbox-acceptance-h3-reproduction",
        attempts=attempts,
        contract_path=contract,
    )
    print(
        json.dumps(
            {
                "passed": True,
                "candidate": str(candidate),
                "sealed_builder_receipt": str(
                    builder_output / "sealed-builder.receipt.json"
                ),
                "candidate_build_receipt": str(
                    candidate / ".visionmcp/nocturne-build.receipt.json"
                ),
                "builder": builder.model_dump(mode="json"),
                "candidate_receipt": receipt.model_dump(mode="json"),
            },
            indent=2,
            sort_keys=True,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
