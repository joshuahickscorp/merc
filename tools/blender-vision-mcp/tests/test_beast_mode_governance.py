from __future__ import annotations

import json
import uuid
from pathlib import Path

import pytest
from PIL import Image

from blender_vision.acceptance.lifecycle import audit_scene_lifecycle
from blender_vision.acceptance.receipts import export_receipt
from blender_vision.acceptance.transactions import (
    REQUIRED_GATE_CATEGORIES,
    CandidateTransactionStore,
)
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.benchmarks.beast import BeastBenchmarkAuditor
from blender_vision.blender.passes import GOVERNED_RENDER_PASSES
from blender_vision.cameras.solver import CameraSolver
from blender_vision.cameras.state import scaled_camera_state
from blender_vision.comparison.metrics import compare_silhouettes
from blender_vision.comparison.store import ComparisonStore
from blender_vision.core.models import SceneLifecycleState
from blender_vision.core.util import utc_now
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore


def _gates(score: float = 1.0, baseline: float = 0.9) -> list[dict[str, object]]:
    return [
        {
            "name": f"{category}-gate",
            "category": category,
            "status": "PASS",
            "candidate_value": score,
            "baseline_value": baseline,
            "higher_is_better": True,
        }
        for category in sorted(REQUIRED_GATE_CATEGORIES)
    ]


def test_camera_solution_persists_complete_immutable_state(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Complete camera")
    image = tmp_path / "front.png"
    Image.new("RGB", (320, 200), "gray").save(image)
    reference = ReferenceIngestor(project).import_file(
        image, rights_state="TEST_FIXTURE", viewpoint_label="front"
    )

    solution = CameraSolver(project).solve("turntable_fallback")
    camera = solution["cameras"][0]

    assert camera["resolution"] == {"width": 320, "height": 200}
    assert camera["extrinsics"]["world_from_camera"] == camera["world_from_camera"]
    assert camera["camera_source_identity"]["reference_id"] == reference["id"]
    assert camera["camera_source_identity"]["artifact_digest"] == reference["artifact"]["digest"]
    assert camera["solve_method"]["backend"] == "turntable_fallback"
    assert camera["approval_state"] == "pending"
    assert len(camera["immutable_sha256"]) == 64
    scaled = scaled_camera_state(camera, 160)
    assert scaled["resolution"] == {"width": 160, "height": 100}
    assert scaled["extrinsics"] == camera["extrinsics"]
    assert scaled["intrinsics"]["fx"] == camera["intrinsics"]["fx"] / 2


def test_scene_lifecycle_keeps_candidate_from_becoming_authoritative(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Lifecycle")
    store = SceneStore(project)
    baseline_source = tmp_path / "baseline.blend"
    baseline_source.write_bytes(b"baseline")
    baseline = store.import_blend(baseline_source)
    assert baseline["state"] == "DRAFT"
    store.transition(baseline["id"], "CANDIDATE", reviewer="Builder", reason="Ready for evaluation")
    baseline_evaluation = CandidateTransactionStore(project).evaluate(
        baseline["id"], gates=_gates()
    )
    store.transition(
        baseline["id"],
        "ACCEPTED",
        reviewer="Acceptance QA",
        reason="All mandatory gates passed",
        evaluation_id=baseline_evaluation["id"],
    )
    store.transition(
        baseline["id"],
        "PROMOTED",
        reviewer="Owner",
        reason="Approved baseline",
        evaluation_id=baseline_evaluation["id"],
    )

    candidate_path = project.root / "scene" / "candidate.blend"
    candidate_path.write_bytes(b"candidate")
    candidate = store.register_generated(candidate_path, original_name="candidate.blend")

    assert candidate["state"] == "CANDIDATE"
    assert store.get()["id"] == baseline["id"]
    assert store.get(candidate["id"])["is_authoritative"] == 0


def test_promotion_requires_the_verified_passed_evaluation(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Promotion evaluation")
    store = SceneStore(project)
    source = tmp_path / "baseline.blend"
    source.write_bytes(b"baseline")
    scene = store.import_blend(source)
    store.transition(scene["id"], "CANDIDATE", reviewer="Builder", reason="Evaluate")
    evaluation = CandidateTransactionStore(project).evaluate(scene["id"], gates=_gates())
    store.transition(
        scene["id"],
        "ACCEPTED",
        reviewer="QA",
        reason="Passed",
        evaluation_id=evaluation["id"],
    )

    with pytest.raises(ValueError, match="requires a passed transactional evaluation"):
        store.transition(scene["id"], "PROMOTED", reviewer="Owner", reason="Promote")

    assert store.get(scene["id"])["state"] == "ACCEPTED"
    assert len(store.transitions(scene["id"])) == 2


def test_promotion_atomically_receipts_every_displaced_authority(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Receipt-complete supersession")
    store = SceneStore(project)
    baseline_path = tmp_path / "baseline.blend"
    baseline_path.write_bytes(b"baseline")
    baseline = store.import_blend(baseline_path)
    store.transition(baseline["id"], "CANDIDATE", reviewer="Builder", reason="Evaluate")
    baseline_evaluation = CandidateTransactionStore(project).evaluate(
        baseline["id"], gates=_gates()
    )
    store.transition(
        baseline["id"],
        "ACCEPTED",
        reviewer="QA",
        reason="Passed",
        evaluation_id=baseline_evaluation["id"],
    )
    store.transition(
        baseline["id"],
        "PROMOTED",
        reviewer="Owner",
        reason="Initial authority",
        evaluation_id=baseline_evaluation["id"],
    )

    candidate_path = project.root / "scene" / "candidate.blend"
    candidate_path.write_bytes(b"candidate")
    candidate = store.register_generated(candidate_path, original_name="candidate.blend")
    evaluation = CandidateTransactionStore(project).evaluate(
        candidate["id"], baseline_scene_id=baseline["id"], gates=_gates()
    )
    store.transition(
        candidate["id"],
        "ACCEPTED",
        reviewer="QA",
        reason="Improved",
        evaluation_id=evaluation["id"],
    )
    promotion = store.transition(
        candidate["id"],
        "PROMOTED",
        reviewer="Owner",
        reason="Replace baseline",
        evaluation_id=evaluation["id"],
    )

    assert store.get()["id"] == candidate["id"]
    assert store.get(baseline["id"])["state"] == "SUPERSEDED"
    assert store.get(baseline["id"])["is_authoritative"] == 0
    assert len(promotion["superseded_transitions"]) == 1
    supersession = promotion["superseded_transitions"][0]
    payload = json.loads((project.root / supersession["path"]).read_text(encoding="utf-8"))
    assert payload["scene_id"] == baseline["id"]
    assert payload["to_state"] == "SUPERSEDED"
    assert payload["superseded_by_scene_id"] == candidate["id"]
    assert payload["evaluation_id"] == evaluation["id"]
    lifecycle = audit_scene_lifecycle(project)
    assert lifecycle["authoritative_promotion_chain_valid"] is True
    assert lifecycle["unreceipted_superseded_scene_ids"] == []
    assert lifecycle["invalid_transition_ids"] == []


def test_auditors_reject_promoted_state_without_lifecycle_receipts(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Tampered lifecycle")
    scene_path = tmp_path / "scene.blend"
    scene_path.write_bytes(b"fixture")
    scene = SceneStore(project).import_blend(scene_path)
    with project.connection() as connection:
        connection.execute(
            "UPDATE scene_assets SET state='PROMOTED',is_authoritative=1 WHERE id=?",
            (scene["id"],),
        )

    lifecycle = audit_scene_lifecycle(project)
    assert lifecycle["authoritative_promotion_chain_valid"] is False
    benchmark = BeastBenchmarkAuditor(project).audit(1)
    lifecycle_check = next(
        check
        for check in benchmark["checks"]
        if check["name"]
        == "authoritative candidate promoted with verified lifecycle receipts"
    )
    assert lifecycle_check["passed"] is False
    acceptance = export_receipt(project)["acceptance"]
    assert any("receipt-complete promotion chain" in item for item in acceptance["blockers"])


def test_transaction_regression_rejects_candidate_and_preserves_baseline(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Regression")
    store = SceneStore(project)
    baseline_source = tmp_path / "baseline.blend"
    baseline_source.write_bytes(b"baseline")
    baseline = store.import_blend(baseline_source)
    store.transition(baseline["id"], "CANDIDATE", reviewer="Builder", reason="Evaluate baseline")
    passed = CandidateTransactionStore(project).evaluate(baseline["id"], gates=_gates())
    store.transition(
        baseline["id"],
        "ACCEPTED",
        reviewer="QA",
        reason="Passed",
        evaluation_id=passed["id"],
    )
    store.transition(
        baseline["id"],
        "PROMOTED",
        reviewer="Owner",
        reason="Promote",
        evaluation_id=passed["id"],
    )

    candidate_path = project.root / "scene" / "regression.blend"
    candidate_path.write_bytes(b"regression")
    candidate = store.register_generated(candidate_path, original_name="regression.blend")
    failed = CandidateTransactionStore(project).evaluate(
        candidate["id"],
        baseline_scene_id=baseline["id"],
        gates=_gates(score=0.8, baseline=0.9),
    )

    assert failed["status"] == "FAILED"
    assert len(failed["regressions"]) == len(REQUIRED_GATE_CATEGORIES)
    assert store.get(candidate["id"])["state"] == SceneLifecycleState.REJECTED.value
    assert store.get()["id"] == baseline["id"]
    assert project.status()["counts"]["scene_transitions"] >= 4
    lifecycle = audit_scene_lifecycle(project)
    assert failed["id"] not in lifecycle["invalid_evaluation_ids"]
    assert failed["automatic_rejection"]["id"] not in lifecycle["invalid_transition_ids"]


def test_failed_evaluation_and_automatic_rejection_are_atomic(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Atomic rejection")
    store = SceneStore(project)
    scene_path = tmp_path / "candidate.blend"
    scene_path.write_bytes(b"candidate")
    scene = store.import_blend(scene_path)
    store.transition(scene["id"], "CANDIDATE", reviewer="Builder", reason="Evaluate")

    def fail_transition_insert(_connection: object, _result: object) -> None:
        raise RuntimeError("simulated transition insert failure")

    monkeypatch.setattr(
        SceneStore, "_insert_transition", staticmethod(fail_transition_insert)
    )
    with pytest.raises(RuntimeError, match="simulated transition insert failure"):
        CandidateTransactionStore(project).evaluate(
            scene["id"], gates=_gates(score=0.8, baseline=0.9)
        )

    assert store.get(scene["id"])["state"] == "CANDIDATE"
    with project.connection() as connection:
        assert connection.execute("SELECT COUNT(*) FROM candidate_evaluations").fetchone()[0] == 0
        assert connection.execute("SELECT COUNT(*) FROM scene_transitions").fetchone()[0] == 1


def test_lifecycle_audit_recomputes_forged_evaluation_outcome(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Forged evaluation")
    store = SceneStore(project)
    scene_path = tmp_path / "candidate.blend"
    scene_path.write_bytes(b"candidate")
    scene = store.import_blend(scene_path)
    store.transition(scene["id"], "CANDIDATE", reviewer="Builder", reason="Evaluate")
    evaluation = CandidateTransactionStore(project).evaluate(scene["id"], gates=_gates())

    payload = json.loads((project.root / evaluation["path"]).read_text(encoding="utf-8"))
    payload["gates"][0]["status"] = "FAIL"
    payload["failed_gates"] = [payload["gates"][0]["name"]]
    forged_path = tmp_path / "forged-evaluation.json"
    forged_path.write_text(json.dumps(payload), encoding="utf-8")
    forged_artifact = ArtifactStore(project).ingest_file(
        forged_path, media_type="application/vnd.bvmcp.candidate-evaluation+json"
    )
    with project.connection() as connection:
        connection.execute(
            "UPDATE candidate_evaluations SET gates_json=?,receipt_digest=? WHERE id=?",
            (json.dumps(payload["gates"]), forged_artifact.digest, evaluation["id"]),
        )

    lifecycle = audit_scene_lifecycle(project)
    assert lifecycle["invalid_evaluation_ids"] == [evaluation["id"]]
    acceptance = export_receipt(project)["acceptance"]
    assert "one or more candidate evaluation receipts are invalid" in acceptance["blockers"]


def test_beast_auditor_replays_authoritative_transaction_and_comparison_suite(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Replay-bound Beast audit")
    reference_path = tmp_path / "front.png"
    Image.new("RGBA", (96, 64), (80, 80, 80, 255)).save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path, rights_state="SYNTHETIC_OWNED", viewpoint_label="front"
    )
    camera = CameraSolver(project).solve("turntable_fallback")
    CameraSolver(project).approve(
        camera["id"], reviewer="Camera reviewer", reason="Fixture camera reviewed"
    )

    scene_path = tmp_path / "candidate.blend"
    scene_path.write_bytes(b"candidate")
    scenes = SceneStore(project)
    scene = scenes.import_blend(scene_path)
    scenes.transition(scene["id"], "CANDIDATE", reviewer="Builder", reason="Evaluate")
    evaluation = CandidateTransactionStore(project).evaluate(scene["id"], gates=_gates())
    scenes.transition(
        scene["id"],
        "ACCEPTED",
        reviewer="Acceptance reviewer",
        reason="All gates passed",
        evaluation_id=evaluation["id"],
    )
    scenes.transition(
        scene["id"],
        "PROMOTED",
        reviewer="Owner",
        reason="Promote verified fixture",
        evaluation_id=evaluation["id"],
    )

    render_source = tmp_path / "render.png"
    Image.new("RGBA", (96, 64), (80, 80, 80, 255)).save(render_source)
    artifacts = ArtifactStore(project)
    render_artifact = artifacts.ingest_file(render_source, media_type="image/png")
    materialized = project.root / "renders" / "verified.png"
    artifacts.materialize(render_artifact.digest, materialized)
    render_id = str(uuid.uuid4())
    outputs = [
        {
            "reference_id": reference["id"],
            "artifact_digest": render_artifact.digest,
            "pass_artifact_digests": {
                name: render_artifact.digest for name in GOVERNED_RENDER_PASSES
            },
            "relative_path": str(materialized.relative_to(project.root)),
        }
    ]
    with project.connection() as connection:
        connection.execute(
            "INSERT INTO render_runs(id,scene_id,camera_solution_id,config_json,outputs_json,"
            "created_at) VALUES(?,?,?,?,?,?)",
            (
                render_id,
                scene["id"],
                camera["id"],
                "{}",
                json.dumps(outputs),
                utc_now(),
            ),
        )

    residual_path = tmp_path / "residual.png"
    metrics = compare_silhouettes(reference_path, render_source, residual_path)
    residual = artifacts.ingest_file(residual_path, media_type="image/png")
    comparison_id = str(uuid.uuid4())
    ComparisonStore(project).record(
        comparison_id,
        reference_id=reference["id"],
        render_digest=render_artifact.digest,
        residual_digest=residual.digest,
        metrics=metrics,
        engine="compare_silhouettes_v2",
    )

    unrelated_scene_path = tmp_path / "unrelated.blend"
    unrelated_scene_path.write_bytes(b"unrelated")
    unrelated_scene = scenes.import_blend(unrelated_scene_path)
    unrelated_export_source = tmp_path / "unrelated.glb"
    unrelated_export_source.write_bytes(b"unrelated export")
    unrelated_export = artifacts.ingest_file(
        unrelated_export_source, media_type="model/gltf-binary"
    )
    unrelated_export_path = project.root / "exports" / "unrelated.glb"
    artifacts.materialize(unrelated_export.digest, unrelated_export_path)
    with project.connection() as connection:
        connection.execute(
            "INSERT INTO exports(id,scene_id,artifact_digest,format,relative_path,config_json,"
            "worker_json,created_at) VALUES(?,?,?,?,?,?,?,?)",
            (
                str(uuid.uuid4()),
                unrelated_scene["id"],
                unrelated_export.digest,
                "glb",
                str(unrelated_export_path.relative_to(project.root)),
                "{}",
                "{}",
                utc_now(),
            ),
        )

    auditor = BeastBenchmarkAuditor(project)
    complete = auditor._facts()
    assert complete["passed_authoritative_transaction"] is True
    assert complete["mandatory_render_suite_count"] == 1
    assert complete["comparison_suite_count"] == 1
    assert complete["invalid_comparison_ids"] == []
    assert complete["export_formats"] == []
    complete_checks = {item["name"]: item["passed"] for item in auditor._checks(1, complete)}
    assert complete_checks["all-gate candidate transaction passed"]

    with project.connection() as connection:
        connection.execute(
            "UPDATE candidate_evaluations SET metrics_json=? WHERE id=?",
            (json.dumps({"forged": True}), evaluation["id"]),
        )
    assert auditor._facts()["passed_authoritative_transaction"] is False

    with project.connection() as connection:
        connection.execute(
            "UPDATE candidate_evaluations SET metrics_json='{}' WHERE id=?",
            (evaluation["id"],),
        )
        forged_metrics = {**metrics, "silhouette_iou": 0.5}
        connection.execute(
            "UPDATE comparisons SET metrics_json=? WHERE id=?",
            (json.dumps(forged_metrics), comparison_id),
        )
    tampered = auditor._facts()
    assert tampered["passed_authoritative_transaction"] is True
    assert tampered["comparison_suite_count"] == 0
    assert tampered["invalid_comparison_ids"] == [comparison_id]
    tampered_checks = {item["name"]: item["passed"] for item in auditor._checks(1, tampered)}
    assert not tampered_checks["all-gate candidate transaction passed"]


def test_transaction_requires_aggregate_improvement_over_baseline(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "No-op candidate")
    store = SceneStore(project)
    baseline_path = tmp_path / "baseline.blend"
    baseline_path.write_bytes(b"baseline")
    baseline = store.import_blend(baseline_path)
    store.transition(baseline["id"], "CANDIDATE", reviewer="Builder", reason="Evaluate")
    evaluation = CandidateTransactionStore(project).evaluate(
        baseline["id"], gates=_gates(score=0.9, baseline=0.8)
    )
    store.transition(
        baseline["id"],
        "ACCEPTED",
        reviewer="QA",
        reason="Improved",
        evaluation_id=evaluation["id"],
    )
    store.transition(
        baseline["id"],
        "PROMOTED",
        reviewer="Owner",
        reason="Baseline",
        evaluation_id=evaluation["id"],
    )
    candidate_path = project.root / "scene" / "same.blend"
    candidate_path.write_bytes(b"same")
    candidate = store.register_generated(candidate_path, original_name="same.blend")
    result = CandidateTransactionStore(project).evaluate(
        candidate["id"],
        baseline_scene_id=baseline["id"],
        gates=_gates(score=0.9, baseline=0.9),
    )
    assert result["status"] == "FAILED"
    assert result["aggregate_improvement"] is False
    assert store.get(candidate["id"])["state"] == "REJECTED"
