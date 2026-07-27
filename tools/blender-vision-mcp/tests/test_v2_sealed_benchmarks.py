"""Leakage and contract tests for the V2 sealed benchmark harness."""

from __future__ import annotations

import json
import shutil
from pathlib import Path

import pytest

from blender_vision.benchmarks.sealed import (
    TARGET_IDS,
    BuilderWorkspace,
    EvaluatorWorkspace,
    LeakageBlocked,
    OracleWorkspace,
    SealedContract,
    SealedManifest,
    freeze_contract,
    load_all_contracts,
    load_all_manifests,
    load_manifest,
    probe_builder_path_escape,
    probe_builder_reads_oracle_file,
    probe_builder_symlink_escape,
    probe_evaluator_swap,
    probe_hidden_camera_in_builder_inputs,
    probe_threshold_edit_after_seal,
    run_leakage_matrix,
    run_sealed_benchmark,
    sealed_benchmarks_root,
    tree_digest,
)
from blender_vision.core.errors import SecurityError


@pytest.fixture
def isolation(tmp_path: Path) -> dict[str, Path]:
    oracle = tmp_path / "oracle"
    builder = tmp_path / "builder"
    evaluator = tmp_path / "evaluator"
    inputs = tmp_path / "builder_inputs"
    for path in (oracle, builder, evaluator, inputs):
        path.mkdir()
    canary = "TEST-ORACLE-CANARY-sealed-v2"
    (oracle / "hidden").mkdir()
    (oracle / "hidden" / "ORACLE_CANARY.txt").write_text(canary + "\n", encoding="utf-8")
    (oracle / "hidden" / "cameras").mkdir()
    (oracle / "hidden" / "cameras" / "holdout_01.json").write_text(
        json.dumps({"hidden": True, "canary": canary}), encoding="utf-8"
    )
    (oracle / "builder_inputs").mkdir()
    (oracle / "builder_inputs" / "public.json").write_text(
        json.dumps({"public": True}), encoding="utf-8"
    )
    shutil.copy2(oracle / "builder_inputs" / "public.json", inputs / "public.json")
    (evaluator / "gates.json").write_text(
        json.dumps({"public_silhouette_iou_minimum": 0.95}), encoding="utf-8"
    )
    return {
        "oracle": oracle,
        "builder": builder,
        "evaluator": evaluator,
        "inputs": inputs,
        "canary": canary,  # type: ignore[dict-item]
        "tmp": tmp_path,
    }


def _workspaces(isolation: dict[str, Path]) -> tuple[
    OracleWorkspace, BuilderWorkspace, EvaluatorWorkspace
]:
    oracle = OracleWorkspace(
        root=isolation["oracle"],
        hidden_relative_paths=[
            "hidden/ORACLE_CANARY.txt",
            "hidden/cameras/holdout_01.json",
        ],
        builder_input_relative_paths=["builder_inputs/public.json"],
        canary=str(isolation["canary"]),
    )
    builder = BuilderWorkspace(
        root=isolation["builder"],
        oracle_root=isolation["oracle"],
        inputs_root=isolation["inputs"],
    )
    evaluator = EvaluatorWorkspace(root=isolation["evaluator"])
    evaluator.freeze()
    return oracle, builder, evaluator


# ---------------------------------------------------------------------------
# Five leakage attempts — each must be blocked
# ---------------------------------------------------------------------------


def test_builder_cannot_read_oracle_file_directly(isolation: dict[str, Path]) -> None:
    _oracle, builder, _evaluator = _workspaces(isolation)

    with pytest.raises(LeakageBlocked, match="cannot read oracle"):
        probe_builder_reads_oracle_file(builder)

    with pytest.raises(LeakageBlocked, match="cannot read oracle"):
        builder.attempt_read_oracle("hidden/cameras/holdout_01.json")


def test_builder_path_escape_via_dotdot_is_blocked(isolation: dict[str, Path]) -> None:
    _oracle, builder, _evaluator = _workspaces(isolation)

    with pytest.raises(LeakageBlocked, match="path escape"):
        probe_builder_path_escape(builder, "../oracle/hidden/ORACLE_CANARY.txt")

    with pytest.raises((LeakageBlocked, SecurityError)):
        builder.confined(builder.root / ".." / "oracle" / "hidden" / "ORACLE_CANARY.txt")


def test_builder_symlink_escape_into_oracle_is_blocked(isolation: dict[str, Path]) -> None:
    _oracle, builder, _evaluator = _workspaces(isolation)

    with pytest.raises(LeakageBlocked, match="symlink"):
        probe_builder_symlink_escape(builder)


def test_evaluator_swap_after_freeze_is_blocked(isolation: dict[str, Path]) -> None:
    _oracle, _builder, evaluator = _workspaces(isolation)
    assert evaluator.digest_at_freeze
    before = evaluator.digest_at_freeze

    with pytest.raises(LeakageBlocked, match="evaluator was swapped"):
        probe_evaluator_swap(evaluator)

    # Probe restores the inject file; digest must match the freeze again.
    assert evaluator.current_digest() == before
    assert evaluator.reverify() == before


def test_hidden_camera_cannot_appear_in_builder_inputs(isolation: dict[str, Path]) -> None:
    oracle, _builder, _evaluator = _workspaces(isolation)
    dest = isolation["tmp"] / "leaked"

    with pytest.raises(LeakageBlocked, match="hidden"):
        probe_hidden_camera_in_builder_inputs(
            oracle, dest, "hidden/cameras/holdout_01.json"
        )


def test_threshold_edit_after_seal_is_blocked(isolation: dict[str, Path]) -> None:
    oracle, _builder, evaluator = _workspaces(isolation)
    contract = freeze_contract(
        target_id="datacenter_film",
        oracle=oracle,
        evaluator=evaluator,
        builder_inputs_root=isolation["inputs"],
        builder_input_paths=["public.json"],
        acceptance_thresholds={"public_silhouette_iou_minimum": 0.95},
    )
    assert contract.digest
    contract.verify()  # baseline ok

    with pytest.raises(LeakageBlocked, match="threshold edit"):
        probe_threshold_edit_after_seal(contract)

    # Original thresholds restored by the probe's finally block.
    contract.verify()


# ---------------------------------------------------------------------------
# Evaluator freeze before/after + orchestration
# ---------------------------------------------------------------------------


def test_evaluator_freeze_digest_matches_before_and_after_builder(
    isolation: dict[str, Path],
) -> None:
    oracle, builder, evaluator = _workspaces(isolation)
    contract = freeze_contract(
        target_id="soft_object",
        oracle=oracle,
        evaluator=evaluator,
        builder_inputs_root=isolation["inputs"],
        builder_input_paths=["public.json"],
        acceptance_thresholds={"public_silhouette_iou_minimum": 0.92},
    )

    def _builder(ws: BuilderWorkspace) -> dict:
        out = ws.confined(ws.root / "candidate.json")
        out.write_text(json.dumps({"status": "ok"}), encoding="utf-8")
        return {"status": "PASS"}

    receipt = run_sealed_benchmark(
        contract=contract,
        oracle=oracle,
        builder=builder,
        evaluator=evaluator,
        output_root=isolation["tmp"] / "run",
        builder_fn=_builder,
    )
    assert receipt.status == "PASS"
    assert receipt.evaluator_digest == receipt.evaluator_digest_after
    assert receipt.evaluator_digest == contract.evaluator_digest
    receipt_path = isolation["tmp"] / "run" / "sealed.receipt.json"
    assert receipt_path.is_file()
    payload = json.loads(receipt_path.read_text(encoding="utf-8"))
    assert payload["digest"] == receipt.digest


def test_failed_attempts_are_preserved(isolation: dict[str, Path]) -> None:
    oracle, builder, evaluator = _workspaces(isolation)
    contract = freeze_contract(
        target_id="organic",
        oracle=oracle,
        evaluator=evaluator,
        builder_inputs_root=isolation["inputs"],
        builder_input_paths=["public.json"],
        acceptance_thresholds={"lod_silhouette_iou_minimum": 0.9},
    )

    def _builder(ws: BuilderWorkspace) -> dict:
        (ws.root / "partial.txt").write_text("partial work\n", encoding="utf-8")
        raise RuntimeError("deliberate builder failure")

    receipt = run_sealed_benchmark(
        contract=contract,
        oracle=oracle,
        builder=builder,
        evaluator=evaluator,
        output_root=isolation["tmp"] / "run-fail",
        builder_fn=_builder,
    )
    assert receipt.status == "FAIL"
    assert receipt.failed_attempt_paths
    attempt = Path(receipt.failed_attempt_paths[0])
    assert (attempt / "error.txt").is_file()
    assert (attempt / "builder" / "partial.txt").is_file()
    assert "deliberate builder failure" in (attempt / "error.txt").read_text(
        encoding="utf-8"
    )


def test_run_blocks_when_evaluator_changes_mid_run(isolation: dict[str, Path]) -> None:
    oracle, builder, evaluator = _workspaces(isolation)
    contract = freeze_contract(
        target_id="browser_round_trip",
        oracle=oracle,
        evaluator=evaluator,
        builder_inputs_root=isolation["inputs"],
        builder_input_paths=["public.json"],
        acceptance_thresholds={"max_playwright_browsers": 1},
    )

    def _builder(ws: BuilderWorkspace) -> dict:
        # Mutate evaluator during the builder window — must be caught on reverify.
        (evaluator.root / "tamper.json").write_text("{}", encoding="utf-8")
        return {"status": "PASS"}

    with pytest.raises(LeakageBlocked, match="evaluator was swapped"):
        run_sealed_benchmark(
            contract=contract,
            oracle=oracle,
            builder=builder,
            evaluator=evaluator,
            output_root=isolation["tmp"] / "run-swap",
            builder_fn=_builder,
        )
    # Failure was preserved.
    failed = list((isolation["tmp"] / "run-swap" / "failed-attempts").iterdir())
    assert failed


# ---------------------------------------------------------------------------
# Fixed six-target manifests and contracts
# ---------------------------------------------------------------------------


def test_all_six_targets_present() -> None:
    root = sealed_benchmarks_root()
    for target_id in TARGET_IDS:
        assert (root / target_id / "manifest.json").is_file()
        assert (root / target_id / "contract.json").is_file()
        assert (root / target_id / "evaluator").is_dir()
        assert (root / target_id / "oracle").is_dir()


def test_manifests_schema_valid() -> None:
    manifests = load_all_manifests()
    assert set(manifests) == set(TARGET_IDS)
    for target_id, manifest in manifests.items():
        manifest.validate_against_schema()
        assert manifest.target_id == target_id
        assert manifest.acceptance_thresholds
        assert all(not item.builder_visible for item in manifest.hidden_evidence)
        assert all(item.builder_visible for item in manifest.builder_inputs)


def test_contracts_verify_and_bind_live_trees() -> None:
    contracts = load_all_contracts()
    assert set(contracts) == set(TARGET_IDS)
    root = sealed_benchmarks_root()
    for target_id, contract in contracts.items():
        contract.verify()
        base = root / target_id
        assert contract.oracle_digest == tree_digest(base / "oracle")
        assert contract.evaluator_digest == tree_digest(base / "evaluator")
        manifest = load_manifest(target_id)
        assert contract.acceptance_thresholds == manifest.acceptance_thresholds
        assert contract.blocked_requirements == [
            item.to_dict() for item in manifest.blocked_requirements
        ]


def test_blocked_targets_declare_exact_requirements() -> None:
    remote = load_manifest("remote")
    fur = load_manifest("fur_animal")
    assert remote.evidence_status.value == "blocked"
    assert fur.evidence_status.value == "blocked"
    assert remote.blocked_requirements
    assert fur.blocked_requirements
    remote_ids = {item.id for item in remote.blocked_requirements}
    fur_ids = {item.id for item in fur.blocked_requirements}
    assert "real_remote_multiview_capture" in remote_ids
    assert "colmap_dense_mvs_cuda" in remote_ids
    assert "real_animal_capture" in fur_ids
    assert "synthetic_bust_substitution" in fur.acceptance_thresholds
    assert fur.acceptance_thresholds["synthetic_bust_substitution"] == "forbidden"
    for item in [*remote.blocked_requirements, *fur.blocked_requirements]:
        assert item.reason.strip()
        assert item.required_to_unblock.strip()
        # Must not silently point at a synthetic substitute as the unblock path.
        assert "silently" not in item.required_to_unblock.lower() or "not" in item.reason.lower()


def test_available_targets_have_no_blocked_requirements() -> None:
    for target_id in ("datacenter_film", "soft_object", "organic", "browser_round_trip"):
        manifest = load_manifest(target_id)
        assert manifest.evidence_status.value == "available"
        assert manifest.blocked_requirements == []


def test_contract_rejects_unknown_kind() -> None:
    with pytest.raises(Exception, match="sealed-benchmark-contract"):
        SealedContract.from_dict(
            {
                "kind": "not-a-contract",
                "target_id": "remote",
                "oracle_digest": "0" * 64,
                "evaluator_digest": "0" * 64,
                "builder_inputs_digest": "0" * 64,
                "frozen_at": "2026-07-26T00:00:00+00:00",
                "acceptance_thresholds": {"x": 1},
            }
        )


def test_manifest_rejects_hidden_evidence_marked_visible() -> None:
    base = load_manifest("organic").to_dict()
    base["hidden_evidence"][0]["builder_visible"] = True
    with pytest.raises(Exception, match="builder_visible"):
        SealedManifest.from_dict(base)


def test_leakage_matrix_blocks_all_probes(tmp_path: Path) -> None:
    matrix = run_leakage_matrix(work_root=tmp_path / "matrix")
    assert len(matrix) >= 5
    failed = [row for row in matrix if row["result"] != "PASS"]
    assert not failed, failed
    assert all(row["blocked"] for row in matrix)


def test_materialize_builder_inputs_excludes_hidden(isolation: dict[str, Path]) -> None:
    oracle, _builder, _evaluator = _workspaces(isolation)
    dest = isolation["tmp"] / "packet"
    copied = oracle.materialize_builder_inputs(dest)
    assert copied
    for path in dest.rglob("*"):
        if path.is_file():
            data = path.read_bytes()
            assert b"TEST-ORACLE-CANARY" not in data
            assert b"holdout" not in data or b"public" in data
