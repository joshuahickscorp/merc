from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest
from PIL import Image, ImageDraw

from blender_vision.benchmarks.nocturne import (
    NocturneOneContract,
    NocturnePacketAuthority,
    SealedBuilderRunner,
    load_nocturne_contract,
    nocturne_benchmark_root,
)
from blender_vision.benchmarks.nocturne_3d import _silhouette_iou
from blender_vision.benchmarks.nocturne_app import (
    NocturneCandidateAuthority,
    seal_nocturne_candidate,
)
from blender_vision.cli.main import build_parser
from blender_vision.core.errors import SecurityError
from blender_vision.core.util import atomic_write_json, sha256_file


def _packet(tmp_path: Path) -> tuple[Path, str]:
    contract, contract_path = load_nocturne_contract()
    root = tmp_path / "packet"
    root.mkdir()
    artifacts = []
    for relative in contract.required_packet_files:
        if relative == "packet.manifest.json":
            continue
        path = root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(f"owned fixture for {relative}\n".encode())
        digest, size = sha256_file(path)
        artifacts.append(
            {
                "path": relative,
                "sha256": digest,
                "size": size,
                "media_type": "application/octet-stream",
            }
        )
    manifest = {
        "schema_version": "1",
        "benchmark_id": contract.benchmark_id,
        "authority": "GOVERNED_BUILDER_INPUT",
        "oracle_seed": contract.oracle_seed,
        "contract_sha256": sha256_file(contract_path)[0],
        "governed_spec_sha256": sha256_file(
            nocturne_benchmark_root() / "governed_spec.json"
        )[0],
        "generated_at": "2026-07-25T00:00:00+00:00",
        "artifacts": artifacts,
        "excluded_authority": ["oracle .blend", "hidden holdouts"],
        "rights_state": "SYNTHETIC_OWNED",
    }
    atomic_write_json(root / "packet.manifest.json", manifest)
    return root, sha256_file(root / "packet.manifest.json")[0]


def test_nocturne_contract_is_fixed_before_builder_execution() -> None:
    contract, path = load_nocturne_contract()

    assert path.name == "contract.json"
    assert contract.benchmark_id == "nocturne-one-sealed-v1"
    assert contract.product_name == "NOCTURNE/ONE"
    assert contract.public_view_labels == [
        "front",
        "rear",
        "left",
        "right",
        "top",
        "hero",
    ]
    assert contract.hidden_holdout_count == 4
    assert contract.application_routes == [
        "/",
        "/technology",
        "/configurator",
        "/reserve",
        "/receipt",
    ]
    assert len(contract.application_states) == 14
    assert len(contract.required_parts) == 12
    assert contract.geometry_gates["hidden_silhouette_iou_minimum"] == 0.92
    assert contract.performance_budget["desktop_median_fps_minimum"] == 55


def test_contract_rejects_route_or_state_omission() -> None:
    contract, _path = load_nocturne_contract()
    value = contract.model_dump(mode="json")
    value["application_routes"] = value["application_routes"][:-1]

    with pytest.raises(ValueError, match="fixed five routes"):
        NocturneOneContract.model_validate(value)


def test_packet_authority_requires_exact_digest_bound_artifact_set(
    tmp_path: Path,
) -> None:
    packet, manifest_digest = _packet(tmp_path)

    verification = NocturnePacketAuthority().verify(packet)

    assert verification["valid"] is True
    assert verification["packet_manifest_sha256"] == manifest_digest
    assert verification["verified_artifact_count"] == 18
    (packet / "references" / "front.png").write_bytes(b"substituted")
    with pytest.raises(SecurityError, match="substituted"):
        NocturnePacketAuthority().verify(packet)


def test_packet_authority_rejects_undeclared_file(tmp_path: Path) -> None:
    packet, _manifest_digest = _packet(tmp_path)
    (packet / "oracle-mesh-statistics.json").write_text("{}", encoding="utf-8")

    with pytest.raises(SecurityError, match="undeclared"):
        NocturnePacketAuthority().verify(packet)


@pytest.mark.skipif(
    sys.platform != "darwin" or not Path("/usr/bin/sandbox-exec").is_file(),
    reason="sealed builder access test requires macOS sandbox-exec",
)
def test_sealed_builder_process_is_denied_oracle_and_holdout_reads(
    tmp_path: Path,
) -> None:
    packet, _manifest_digest = _packet(tmp_path)
    builder = tmp_path / "builder"
    oracle = tmp_path / "sealed-oracle"
    oracle_source = tmp_path / "oracle-source"
    output = tmp_path / "run"
    builder.mkdir()
    oracle.mkdir()
    oracle_source.mkdir()
    canary = "NOCTURNE-TEST-ORACLE-CANARY-77bbdc"
    (oracle / "ORACLE_CANARY.txt").write_text(canary, encoding="utf-8")
    (oracle / "holdout.png").write_bytes(b"hidden")
    (oracle_source / "scene.py").write_text("ORACLE = True\n", encoding="utf-8")

    receipt = SealedBuilderRunner().run(
        builder_root=builder,
        packet_root=packet,
        oracle_root=oracle,
        oracle_source_root=oracle_source,
        oracle_canary=canary,
        command=["/usr/bin/touch", "builder-ran.txt"],
        output_root=output,
        timeout_seconds=30,
    )

    assert receipt.status == "PASS"
    assert receipt.preflight_denied is True
    assert receipt.oracle_canary_absent_from_builder is True
    assert (builder / "builder-ran.txt").is_file()


@pytest.mark.skipif(
    sys.platform != "darwin" or not Path("/usr/bin/sandbox-exec").is_file(),
    reason="sealed builder access test requires macOS sandbox-exec",
)
def test_sealed_builder_denies_additional_declared_prior_state(
    tmp_path: Path,
) -> None:
    packet, _manifest_digest = _packet(tmp_path)
    builder = tmp_path / "builder"
    oracle = tmp_path / "sealed-oracle"
    oracle_source = tmp_path / "oracle-source"
    prior_state = tmp_path / "prior-state"
    output = tmp_path / "run"
    builder.mkdir()
    oracle.mkdir()
    oracle_source.mkdir()
    prior_state.mkdir()
    canary = "NOCTURNE-TEST-ORACLE-CANARY-additional"
    (oracle / "ORACLE_CANARY.txt").write_text(canary, encoding="utf-8")
    secret = prior_state / "prior-candidate.txt"
    secret.write_text("must-not-read", encoding="utf-8")

    receipt = SealedBuilderRunner().run(
        builder_root=builder,
        packet_root=packet,
        oracle_root=oracle,
        oracle_source_root=oracle_source,
        oracle_canary=canary,
        command=[
            "/bin/sh",
            "-c",
            f"if /bin/cat {secret} >/dev/null 2>&1; then exit 9; fi; "
            "/usr/bin/touch additional-denial-passed.txt",
        ],
        output_root=output,
        timeout_seconds=30,
        additional_denied_roots=[prior_state],
    )

    assert receipt.status == "PASS"
    assert (builder / "additional-denial-passed.txt").is_file()


def test_nocturne_oracle_cli_requires_explicit_output() -> None:
    args = build_parser().parse_args(
        ["benchmark", "bootstrap-nocturne-oracle", "--output", "oracle"]
    )

    assert args.benchmark_command == "bootstrap-nocturne-oracle"
    assert args.output == "oracle"


def test_nocturne_evaluator_cli_contracts_are_explicit() -> None:
    parser = build_parser()
    three_d = parser.parse_args(
        [
            "benchmark",
            "evaluate-nocturne-3d",
            "--packet",
            "packet",
            "--oracle",
            "oracle",
            "--candidate",
            "candidate",
            "--builder-receipt",
            "builder.json",
            "--output",
            "evaluation",
        ]
    )
    app = parser.parse_args(
        [
            "benchmark",
            "evaluate-nocturne-app",
            "--packet",
            "packet",
            "--candidate",
            "candidate",
            "--builder-receipt",
            "builder.json",
            "--hidden-mobile-trace",
            "trace.json",
            "--output",
            "evaluation",
        ]
    )

    assert three_d.benchmark_command == "evaluate-nocturne-3d"
    assert app.benchmark_command == "evaluate-nocturne-app"


def test_candidate_authority_binds_exact_source_and_attempt_history(
    tmp_path: Path,
) -> None:
    packet, packet_digest = _packet(tmp_path)
    del packet
    candidate = tmp_path / "candidate"
    (candidate / ".visionmcp").mkdir(parents=True)
    (candidate / "failed-attempts").mkdir()
    (candidate / "src").mkdir()
    failed = candidate / "failed-attempts" / "attempt-001.json"
    accepted = candidate / ".visionmcp" / "attempt-002.json"
    source = candidate / "src" / "app.ts"
    failed.write_text('{"status":"FAILED"}\n', encoding="utf-8")
    accepted.write_text('{"status":"ACCEPTED"}\n', encoding="utf-8")
    source.write_text("export const product = 'NOCTURNE/ONE';\n", encoding="utf-8")
    files = []
    for path in (failed, accepted, source):
        digest, size = sha256_file(path)
        files.append(
            {
                "path": path.relative_to(candidate).as_posix(),
                "sha256": digest,
                "size": size,
            }
        )
    _contract, contract_path = load_nocturne_contract()
    receipt = {
        "schema_version": "1",
        "benchmark_id": "nocturne-one-sealed-v1",
        "authority": "VISIONMCP_BUILDER_OUTPUT",
        "contract_sha256": sha256_file(contract_path)[0],
        "packet_manifest_sha256": packet_digest,
        "builder_condition": "H3-test",
        "generated_at": "2026-07-25T00:00:00+00:00",
        "files": files,
        "attempts": [
            {
                "id": "attempt-001",
                "status": "FAILED",
                "retained_path": "failed-attempts/attempt-001.json",
                "receipt_sha256": sha256_file(failed)[0],
            },
            {
                "id": "attempt-002",
                "status": "ACCEPTED",
                "retained_path": ".visionmcp/attempt-002.json",
                "receipt_sha256": sha256_file(accepted)[0],
            },
        ],
        "reproduction_commands": ["npm ci", "npm run verify"],
        "manual_edits_outside_receipt_chain": False,
        "oracle_source_access": False,
    }
    atomic_write_json(
        candidate / ".visionmcp" / "nocturne-build.receipt.json",
        receipt,
    )

    _build, verification = NocturneCandidateAuthority().verify(
        candidate,
        packet_manifest_sha256=packet_digest,
    )

    assert verification["valid"] is True
    assert verification["file_count"] == 3
    source.write_text("substituted\n", encoding="utf-8")
    with pytest.raises(SecurityError, match="substituted"):
        NocturneCandidateAuthority().verify(
            candidate,
            packet_manifest_sha256=packet_digest,
        )


def test_candidate_sealer_creates_verifiable_exact_receipt(tmp_path: Path) -> None:
    packet, packet_digest = _packet(tmp_path)
    candidate = tmp_path / "candidate"
    for relative in (
        "3d/nocturne-one.blend",
        "public/assets/nocturne-one-hero.glb",
        "public/assets/nocturne-one-low.glb",
        "package.json",
        "package-lock.json",
        ".visionmcp/attempt-001.json",
    ):
        path = candidate / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(f"fixture: {relative}\n", encoding="utf-8")

    sealed = seal_nocturne_candidate(
        candidate_root=candidate,
        packet_root=packet,
        builder_condition="H3-test",
        attempts=[("attempt-001", "ACCEPTED", ".visionmcp/attempt-001.json")],
    )
    _receipt, verification = NocturneCandidateAuthority().verify(
        candidate,
        packet_manifest_sha256=packet_digest,
    )

    assert sealed.authority == "VISIONMCP_BUILDER_OUTPUT"
    assert verification["valid"] is True
    assert verification["file_count"] == 6


def test_silhouette_iou_uses_alpha_and_penalizes_shape_drift(tmp_path: Path) -> None:
    reference = Image.new("RGBA", (64, 64), (0, 0, 0, 0))
    candidate = Image.new("RGBA", (64, 64), (0, 0, 0, 0))
    ImageDraw.Draw(reference).rectangle((12, 12, 51, 51), fill=(255, 255, 255, 255))
    ImageDraw.Draw(candidate).rectangle((16, 12, 55, 51), fill=(255, 255, 255, 255))
    reference_path = tmp_path / "reference.png"
    candidate_path = tmp_path / "candidate.png"
    reference.save(reference_path)
    candidate.save(candidate_path)

    score = _silhouette_iou(reference_path, candidate_path)

    assert score == pytest.approx(0.8181818, rel=1e-5)


def test_contract_and_governed_spec_are_strict_json() -> None:
    for path in (
        nocturne_benchmark_root() / "contract.json",
        nocturne_benchmark_root() / "governed_spec.json",
    ):
        value = json.loads(path.read_text(encoding="utf-8"))
        assert value["schema_version"] == "1"
