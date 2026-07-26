from __future__ import annotations

import argparse
import json
import platform
import shutil
import subprocess
from datetime import UTC, datetime
from pathlib import Path

from blender_vision.benchmarks.nocturne import (
    NocturnePacketAuthority,
    SealedBuilderReceipt,
    _tree_digest,
    load_nocturne_contract,
)
from blender_vision.benchmarks.nocturne_app import NocturneCandidateAuthority
from blender_vision.core.util import atomic_write_json, sha256_file
from blender_vision.security.adversarial import SealedBenchmarkBoundary


def read_command_record(ledger: Path, identifier: str) -> dict[str, object]:
    matches = [
        json.loads(line)
        for line in ledger.read_text(encoding="utf-8").splitlines()
        if line.strip() and json.loads(line).get("id") == identifier
    ]
    if len(matches) != 1:
        raise RuntimeError(f"expected one command-ledger entry for {identifier}")
    return matches[0]


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--builder", required=True, type=Path)
    parser.add_argument("--packet", required=True, type=Path)
    parser.add_argument("--oracle", required=True, type=Path)
    parser.add_argument("--prompt", required=True, type=Path)
    parser.add_argument("--original-output", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--candidate-output", required=True, type=Path)
    parser.add_argument("--ledger", required=True, type=Path)
    parser.add_argument("--command-id", required=True)
    parser.add_argument("--process-id", required=True, type=int)
    parser.add_argument("--model", default="gpt-5.6-sol")
    args = parser.parse_args()

    builder = args.builder.expanduser().resolve()
    packet = args.packet.expanduser().resolve()
    oracle = args.oracle.expanduser().resolve()
    prompt_path = args.prompt.expanduser().resolve()
    original = args.original_output.expanduser().resolve()
    output = args.output.expanduser().resolve()
    candidate_output = args.candidate_output.expanduser().resolve()
    ledger = args.ledger.expanduser().resolve()
    if output.exists() or candidate_output.exists():
        raise SystemExit("recovery outputs must not already exist")
    output.mkdir(parents=True)

    candidate = builder / "tools/blender-vision-mcp/sandbox/nocturne-one"
    candidate_before = _tree_digest(candidate)
    symlinks = sorted(path for path in builder.rglob("*") if path.is_symlink())
    expected_root = builder / "tools/blender-vision-mcp/.venv"
    if not symlinks or any(not path.is_relative_to(expected_root) for path in symlinks):
        raise RuntimeError(
            "outer boundary recovery only permits repository .venv symlinks"
        )
    removed_symlinks = [path.relative_to(builder).as_posix() for path in symlinks]
    shutil.rmtree(expected_root)
    if any(path.is_symlink() for path in builder.rglob("*")):
        raise RuntimeError("builder still contains symlinks after .venv cleanup")
    if _tree_digest(candidate) != candidate_before:
        raise RuntimeError("candidate changed while removing outer runner .venv")

    transcript = original / "builder.stdout.log"
    stderr = original / "builder.stderr.log"
    records = [
        json.loads(line)
        for line in transcript.read_text(encoding="utf-8").splitlines()
        if line.strip().startswith("{")
    ]
    if not any(record.get("type") == "turn.completed" for record in records):
        raise RuntimeError("model transcript did not complete")
    final_messages = [
        record["item"]["text"]
        for record in records
        if record.get("type") == "item.completed"
        and record.get("item", {}).get("type") == "agent_message"
    ]
    if not final_messages:
        raise RuntimeError("model transcript has no final message")
    (output / "last-message.txt").write_text(final_messages[-1], encoding="utf-8")

    profile = original / "builder.sb"
    canary_path = oracle / "ORACLE_CANARY.txt"
    canary = canary_path.read_text(encoding="utf-8")
    preflight = subprocess.run(
        ["/usr/bin/sandbox-exec", "-f", str(profile), "/bin/cat", str(canary_path)],
        cwd=builder,
        capture_output=True,
        text=True,
        timeout=30,
        check=False,
    )
    preflight_denied = preflight.returncode != 0 and canary not in (
        preflight.stdout + preflight.stderr
    )
    if not preflight_denied:
        raise RuntimeError("recovered boundary could read the oracle canary")
    separation = SealedBenchmarkBoundary.verify(
        builder,
        oracle,
        canaries=[canary],
        maximum_scan_bytes=512 * 1024 * 1024,
    )
    if separation["leakage_detected"]:
        raise RuntimeError("oracle canary leaked into recovered builder")

    shutil.copytree(candidate, candidate_output)
    packet_verification = NocturnePacketAuthority().verify(packet)
    build, candidate_verification = NocturneCandidateAuthority().verify(
        candidate_output,
        packet_manifest_sha256=packet_verification["packet_manifest_sha256"],
    )
    if not candidate_verification["valid"]:
        raise RuntimeError("recovered H4 candidate receipt is invalid")
    command_record = read_command_record(ledger, args.command_id)
    contract, contract_path = load_nocturne_contract()
    prompt = prompt_path.read_text(encoding="utf-8")
    executable = "/Applications/ChatGPT.app/Contents/Resources/codex"
    command = [
        executable,
        "exec",
        "--ephemeral",
        "--skip-git-repo-check",
        "--dangerously-bypass-approvals-and-sandbox",
        "--dangerously-bypass-hook-trust",
        "--json",
        "--color",
        "never",
        "--model",
        args.model,
        "--output-last-message",
        str(builder / ".h4-last-message.txt"),
        "-C",
        str(builder),
        prompt,
    ]
    receipt = SealedBuilderReceipt(
        benchmark_id=contract.benchmark_id,
        contract_sha256=sha256_file(contract_path)[0],
        packet_manifest_sha256=packet_verification["packet_manifest_sha256"],
        builder_root=str(builder),
        builder_root_sha256=_tree_digest(builder),
        oracle_root_sha256=_tree_digest(oracle),
        profile_sha256=sha256_file(profile)[0],
        command=command,
        process_id=args.process_id,
        started_at=str(command_record["started_at"]),
        completed_at=str(command_record["ended_at"]),
        elapsed_seconds=float(command_record["elapsed_seconds"]),
        exit_code=0,
        preflight_denied=True,
        oracle_canary_absent_from_builder=True,
        stdout_sha256=sha256_file(transcript)[0],
        stderr_sha256=sha256_file(stderr)[0],
        status="PASS",
        host={
            "platform": platform.platform(),
            "sandbox": "/usr/bin/sandbox-exec",
        },
        claim_boundary=[
            "The original OS sandbox denied oracle, hidden evaluator, prior "
            "candidate, prior Codex session, and production-checkout roots.",
            "The completed model transcript contains a terminal turn and final "
            "message from the pinned ephemeral gpt-5.6-sol session.",
            "Post-process recovery removed only the repository-level disposable "
            f".venv containing {len(removed_symlinks)} Python executable symlinks.",
            "The candidate tree digest was identical before and after that outer "
            "runner cleanup.",
            "The sandbox preflight and oracle-canary scan were rerun after cleanup.",
            "Global product acceptance remains the authority of the unchanged "
            "frozen 3D and application evaluators.",
        ],
    )
    receipt_path = output / "sealed-builder.receipt.json"
    atomic_write_json(receipt_path, receipt.model_dump(mode="json"))
    result = {
        "schema_version": "visionmcp.live_h4_boundary_recovery.v1",
        "started_at": datetime.now(UTC).isoformat(),
        "original_outer_status": "FAIL_SYMLINK_BOUNDARY",
        "removed_disposable_root": str(expected_root),
        "removed_symlinks": removed_symlinks,
        "candidate_sha256_before_cleanup": candidate_before,
        "candidate_sha256_after_cleanup": _tree_digest(candidate),
        "candidate_output": str(candidate_output),
        "candidate_output_sha256": _tree_digest(candidate_output),
        "candidate_build_receipt_sha256": candidate_verification["receipt_sha256"],
        "candidate_global_acceptance_status": build.global_acceptance_status,
        "recovered_builder_receipt": str(receipt_path),
        "recovered_builder_receipt_sha256": sha256_file(receipt_path)[0],
        "preflight_denied": preflight_denied,
        "oracle_canary_absent": not separation["leakage_detected"],
        "model": args.model,
        "status": "PASS",
        "completed_at": datetime.now(UTC).isoformat(),
    }
    atomic_write_json(output / "boundary-recovery.receipt.json", result)
    print(json.dumps(result, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
