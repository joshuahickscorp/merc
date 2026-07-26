from __future__ import annotations

import argparse
import hashlib
import json
import shutil
import tempfile
from datetime import UTC, datetime
from pathlib import Path

from blender_vision.benchmarks.nocturne import SealedBuilderRunner
from blender_vision.core.util import atomic_write_json, sha256_file


def tree_digest(root: Path) -> str:
    records = []
    for path in sorted(root.rglob("*")):
        if path.is_file() and not path.is_symlink():
            digest, size = sha256_file(path)
            records.append(
                {
                    "path": path.relative_to(root).as_posix(),
                    "sha256": digest,
                    "size": size,
                }
            )
    return hashlib.sha256(
        json.dumps(records, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()


def repository_ignore(root: Path):
    def ignore(path: str, names: list[str]) -> set[str]:
        current = Path(path)
        relative = current.relative_to(root) if current != root else Path()
        ignored = {
            name
            for name in names
            if name
            in {
                ".git",
                ".venv",
                "node_modules",
                "dist",
                "data",
                "test-results",
                "playwright-report",
                "__pycache__",
                ".pytest_cache",
                ".ruff_cache",
                ".mypy_cache",
            }
        }
        if relative == Path("tools/blender-vision-mcp"):
            ignored.update({"artifacts", "sandbox"})
        return ignored

    return ignore


def candidate_ignore(_path: str, names: list[str]) -> set[str]:
    return {
        name
        for name in names
        if name
        in {
            "node_modules",
            "dist",
            "data",
            "test-results",
            "playwright-report",
            "__pycache__",
        }
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repository", required=True, type=Path)
    parser.add_argument("--packet", required=True, type=Path)
    parser.add_argument("--oracle", required=True, type=Path)
    parser.add_argument("--oracle-source", required=True, type=Path)
    parser.add_argument("--prompt", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--model", default="gpt-5.6-sol")
    parser.add_argument("--timeout-seconds", type=int, default=5400)
    args = parser.parse_args()

    repository = args.repository.expanduser().resolve()
    packet_source = args.packet.expanduser().resolve()
    oracle = args.oracle_source.expanduser().resolve()
    sealed_oracle = args.oracle.expanduser().resolve()
    prompt_path = args.prompt.expanduser().resolve()
    output = args.output.expanduser().resolve()
    if output.exists():
        raise SystemExit("H4 output must not already exist")
    output.mkdir(parents=True)
    prompt = prompt_path.read_text(encoding="utf-8")
    prompt_digest = sha256_file(prompt_path)[0]

    builder = Path(tempfile.mkdtemp(prefix="nocturne-h4-live-builder-")).resolve()
    packet = Path(tempfile.mkdtemp(prefix="nocturne-h4-live-packet-")).resolve()
    shutil.copytree(
        repository,
        builder,
        dirs_exist_ok=True,
        ignore=repository_ignore(repository),
    )
    shutil.copytree(packet_source, packet, dirs_exist_ok=True)
    last_message = builder / ".h4-last-message.txt"
    executable = Path("/Applications/ChatGPT.app/Contents/Resources/codex")
    if not executable.is_file():
        raise SystemExit(f"fixed-model Codex executable is unavailable: {executable}")
    canary = (sealed_oracle / "ORACLE_CANARY.txt").read_text(encoding="utf-8")
    previous_temp_roots = [
        path
        for path in Path("/private/tmp").glob("nocturne-one-*")
        if path.resolve() not in {builder, packet}
    ]
    codex_root = Path("/Users/scammermike/.codex")
    prior_codex_state = [
        codex_root / "sessions",
        codex_root / "archived_sessions",
        codex_root / "shell_snapshots",
        codex_root / "history.jsonl",
        codex_root / "memories_1.sqlite",
        codex_root / "goals_1.sqlite",
        codex_root / "state_5.sqlite",
        codex_root / ".codex-global-state.json",
        *codex_root.glob("memories_*.sqlite*"),
        *codex_root.glob("goals_*.sqlite*"),
        *codex_root.glob("state_*.sqlite*"),
        *codex_root.glob("logs_*.sqlite*"),
    ]
    additional_denied = [
        Path("/Users/scammermike/Downloads"),
        *prior_codex_state,
        *previous_temp_roots,
    ]
    command = [
        str(executable),
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
        str(last_message),
        "-C",
        str(builder),
        prompt,
    ]
    started_at = datetime.now(UTC).isoformat()
    sealed = SealedBuilderRunner().run(
        builder_root=builder,
        packet_root=packet,
        oracle_root=sealed_oracle,
        oracle_source_root=oracle,
        oracle_canary=canary,
        command=command,
        output_root=output / "builder",
        timeout_seconds=args.timeout_seconds,
        additional_denied_roots=additional_denied,
    )
    candidate = builder / "tools/blender-vision-mcp/sandbox/nocturne-one"
    evidence = output / "candidate-evidence"
    if candidate.is_dir():
        shutil.copytree(candidate, evidence, ignore=candidate_ignore)
    if last_message.is_file():
        shutil.copy2(last_message, output / "last-message.txt")
    result = {
        "schema_version": "visionmcp.live_h4_attempt.v1",
        "started_at": started_at,
        "completed_at": datetime.now(UTC).isoformat(),
        "model": args.model,
        "clean_model_session": True,
        "prompt_sha256": prompt_digest,
        "packet_manifest_sha256": sealed.packet_manifest_sha256,
        "builder_root": str(builder),
        "packet_root": str(packet),
        "builder_receipt": str(output / "builder/sealed-builder.receipt.json"),
        "builder_status": sealed.status,
        "candidate_root": str(candidate) if candidate.is_dir() else None,
        "candidate_evidence": str(evidence) if evidence.is_dir() else None,
        "candidate_evidence_sha256": (
            tree_digest(evidence) if evidence.is_dir() else None
        ),
        "oracle_preflight_denied": sealed.preflight_denied,
        "oracle_canary_absent": sealed.oracle_canary_absent_from_builder,
        "additional_denied_roots": [str(path) for path in additional_denied],
        "frozen_evaluator_unchanged": True,
        "status": "BUILDER_PASS" if sealed.status == "PASS" else "BUILDER_FAIL",
    }
    atomic_write_json(output / "h4-attempt.receipt.json", result)
    print(json.dumps(result, indent=2, sort_keys=True), flush=True)
    if sealed.status != "PASS":
        raise SystemExit(1)


if __name__ == "__main__":
    main()
