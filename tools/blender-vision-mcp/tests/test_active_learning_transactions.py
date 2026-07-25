from __future__ import annotations

import json
from pathlib import Path

import pytest

from blender_vision.acceptance.receipts import export_receipt, verify_receipt
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.datasets.store import DatasetStore, TrainingStore
from blender_vision.intelligence.active_learning import (
    ActiveLearningStore,
    audit_active_learning,
)
from blender_vision.projects.store import ProjectStore


def _predictions(
    path: Path,
    *,
    correct: int,
    incorrect: int,
    intersection: int,
    sample_count: int = 10,
) -> Path:
    path.write_text(
        json.dumps(
            {
                "sample_count": sample_count,
                "counts": {
                    "true_positive": correct,
                    "false_positive": incorrect,
                    "false_negative": incorrect,
                    "mask_intersection": intersection,
                    "mask_union": 100,
                },
            }
        ),
        encoding="utf-8",
    )
    return path


def _planned_cycle(tmp_path: Path) -> tuple[ProjectStore, ActiveLearningStore, dict, dict]:
    project = ProjectStore.create(tmp_path / "project", "Active-learning transaction")
    benchmark = DatasetStore(project).register(
        "immutable fixed benchmark",
        "benchmark",
        {
            "sample_count": 10,
            "artifact_digests": [],
            "execution": {"state": "generated", "generated_sample_count": 10},
        },
        rights_state="SYNTHETIC_OWNED",
        status="generated",
    )
    store = ActiveLearningStore(project)
    cycle = store.start(
        model_level="category_head",
        model_identity={"name": "hardware-components", "revision": "baseline-v1"},
        predictions=[
            {"id": "fan-1", "confidence": 0.25, "impact": 0.95},
            {"id": "port-1", "confidence": 0.9, "impact": 0.1},
        ],
        correction_budget=1,
    )
    store.record_corrections(
        cycle["id"],
        [{"prediction_id": "fan-1", "corrected_class": "axial_fan"}],
        corrected_by="Feature benchmark reviewer",
    )
    planned = store.plan_retraining(
        cycle["id"],
        backend="offline-pytorch",
        benchmark_dataset_id=benchmark["id"],
        training_configuration={"epochs": 2},
    )
    return project, store, benchmark, planned


def _complete_and_evaluate(
    project: ProjectStore,
    benchmark: dict,
    planned: dict,
    tmp_path: Path,
    *,
    candidate_correct: int = 85,
    candidate_incorrect: int = 15,
    candidate_intersection: int = 85,
) -> tuple[dict, dict]:
    del tmp_path
    plan = planned["retraining_plan"]
    checkpoint = project.root / "training" / f"checkpoint-{planned['id']}.bin"
    checkpoint.write_bytes(b"trained-active-learning-checkpoint")
    TrainingStore(project).import_result(
        plan["training_run_id"],
        checkpoint,
        metrics={"training_loss": 0.1},
        license_record={"license": "Apache-2.0", "commercial_use": True},
        model_revision=f"candidate-{planned['id'][:8]}",
    )
    baseline = TrainingStore(project).evaluate(
        benchmark["id"],
        _predictions(
            project.root / "training" / "baseline-predictions.json",
            correct=80,
            incorrect=20,
            intersection=80,
        ),
    )
    candidate = TrainingStore(project).evaluate(
        benchmark["id"],
        _predictions(
            project.root / "training" / "candidate-predictions.json",
            correct=candidate_correct,
            incorrect=candidate_incorrect,
            intersection=candidate_intersection,
        ),
        training_run_id=plan["training_run_id"],
    )
    return baseline, candidate


def test_active_learning_uses_stored_evaluations_and_named_model_activation(
    tmp_path: Path,
) -> None:
    project, store, benchmark, planned = _planned_cycle(tmp_path)
    baseline, candidate = _complete_and_evaluate(
        project, benchmark, planned, tmp_path
    )

    compared = store.compare_evaluations(
        planned["id"],
        baseline_evaluation_id=baseline["id"],
        candidate_evaluation_id=candidate["id"],
    )
    promoted = store.promote(
        planned["id"],
        reviewed_by="Model acceptance reviewer",
        reason="All fixed-benchmark metrics improved without regression.",
    )

    assert compared["status"] == "IMPROVED"
    assert compared["benchmark_comparison"]["improved_metrics"] == [
        "f1",
        "mask_iou",
        "precision",
        "recall",
    ]
    assert promoted["status"] == "PROMOTED"
    assert promoted["activation"]["checkpoint_digest"] == TrainingStore(project).get(
        planned["retraining_plan"]["training_run_id"]
    )["checkpoint_digest"]
    assert store.active_models()[0]["status"] == "ACTIVE"
    audit = audit_active_learning(project)
    assert audit["invalid_cycle_ids"] == []
    assert audit["invalid_model_revision_ids"] == []
    receipt = export_receipt(project)
    acceptance = receipt["acceptance"]
    assert acceptance["metrics"]["active_learning"]["active_model_count"] == 1
    assert acceptance["metrics"]["active_learning"]["invalid_cycle_ids"] == []
    assert verify_receipt(project.root / receipt["path"], project=project)["valid"] is True


def test_equal_or_regressed_benchmark_cannot_activate_candidate(tmp_path: Path) -> None:
    project, store, benchmark, planned = _planned_cycle(tmp_path)
    baseline, candidate = _complete_and_evaluate(
        project,
        benchmark,
        planned,
        tmp_path,
        candidate_correct=80,
        candidate_incorrect=20,
        candidate_intersection=80,
    )

    compared = store.compare_evaluations(
        planned["id"],
        baseline_evaluation_id=baseline["id"],
        candidate_evaluation_id=candidate["id"],
    )

    assert compared["status"] == "REJECTED"
    assert compared["benchmark_comparison"]["improved_metrics"] == []
    with pytest.raises(ValueError, match="non-regressing improved"):
        store.promote(planned["id"], reviewed_by="Reviewer", reason="No improvement")
    assert store.active_models() == []


def test_caller_asserted_metrics_and_unbound_candidate_evaluations_are_refused(
    tmp_path: Path,
) -> None:
    project, store, benchmark, planned = _planned_cycle(tmp_path)
    baseline, _candidate = _complete_and_evaluate(project, benchmark, planned, tmp_path)
    unrelated = TrainingStore(project).evaluate(
        benchmark["id"],
        _predictions(
            project.root / "training" / "unrelated.json",
            correct=90,
            incorrect=10,
            intersection=90,
        ),
    )

    with pytest.raises(ValueError, match="not authoritative"):
        store.compare(planned["id"], before={"f1": 0.8}, after={"f1": 0.9})
    with pytest.raises(ValueError, match="planned retraining run"):
        store.compare_evaluations(
            planned["id"],
            baseline_evaluation_id=baseline["id"],
            candidate_evaluation_id=unrelated["id"],
        )
    incomplete = TrainingStore(project).evaluate(
        benchmark["id"],
        _predictions(
            project.root / "training" / "incomplete.json",
            correct=85,
            incorrect=15,
            intersection=85,
            sample_count=9,
        ),
        training_run_id=planned["retraining_plan"]["training_run_id"],
    )
    with pytest.raises(ValueError, match="complete matching sample set"):
        store.compare_evaluations(
            planned["id"],
            baseline_evaluation_id=baseline["id"],
            candidate_evaluation_id=incomplete["id"],
        )


def test_active_learning_event_tampering_blocks_acceptance(tmp_path: Path) -> None:
    project, store, benchmark, planned = _planned_cycle(tmp_path)
    baseline, candidate = _complete_and_evaluate(project, benchmark, planned, tmp_path)
    store.compare_evaluations(
        planned["id"],
        baseline_evaluation_id=baseline["id"],
        candidate_evaluation_id=candidate["id"],
    )
    with project.connection() as connection:
        connection.execute(
            "UPDATE active_learning_events SET to_status='REJECTED' "
            "WHERE cycle_id=? AND revision=4",
            (planned["id"],),
        )

    audit = audit_active_learning(project)
    assert audit["invalid_cycle_ids"] == [planned["id"]]
    acceptance = export_receipt(project)["acceptance"]
    assert "one or more active-learning cycle receipts are invalid" in acceptance["blockers"]


def test_semantically_forged_hash_valid_comparison_blocks_acceptance(
    tmp_path: Path,
) -> None:
    project, store, benchmark, planned = _planned_cycle(tmp_path)
    baseline, candidate = _complete_and_evaluate(project, benchmark, planned, tmp_path)
    compared = store.compare_evaluations(
        planned["id"],
        baseline_evaluation_id=baseline["id"],
        candidate_evaluation_id=candidate["id"],
    )
    forged = {**compared}
    forged.pop("artifact", None)
    forged.pop("path", None)
    forged["benchmark_comparison"] = {
        **forged["benchmark_comparison"],
        "after": {**forged["benchmark_comparison"]["after"], "precision": 1.0},
    }
    forged_path = project.root / "training" / "active-learning" / "forged-cycle.json"
    forged_path.write_text(json.dumps(forged), encoding="utf-8")
    forged_artifact = ArtifactStore(project).ingest_file(
        forged_path, media_type="application/vnd.bvmcp.active-learning-cycle+json"
    )
    with project.connection() as connection:
        connection.execute(
            "UPDATE active_learning_cycles SET record_json=?,artifact_digest=? WHERE id=?",
            (json.dumps(forged), forged_artifact.digest, planned["id"]),
        )
        connection.execute(
            "UPDATE active_learning_events SET snapshot_digest=? "
            "WHERE cycle_id=? AND revision=4",
            (forged_artifact.digest, planned["id"]),
        )

    audit = audit_active_learning(project)
    assert audit["invalid_cycle_ids"] == [planned["id"]]
    acceptance = export_receipt(project)["acceptance"]
    assert "one or more active-learning cycle receipts are invalid" in acceptance["blockers"]


def test_model_activation_rolls_back_if_cycle_event_cannot_commit(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, store, benchmark, planned = _planned_cycle(tmp_path)
    baseline, candidate = _complete_and_evaluate(project, benchmark, planned, tmp_path)
    store.compare_evaluations(
        planned["id"],
        baseline_evaluation_id=baseline["id"],
        candidate_evaluation_id=candidate["id"],
    )

    def fail_event(*_args, **_kwargs) -> None:
        raise RuntimeError("simulated event failure")

    monkeypatch.setattr(ActiveLearningStore, "_insert_event", staticmethod(fail_event))
    with pytest.raises(RuntimeError, match="simulated event failure"):
        store.promote(
            planned["id"],
            reviewed_by="Model reviewer",
            reason="Candidate passed benchmark policy.",
        )

    assert store.get(planned["id"])["status"] == "IMPROVED"
    assert store.active_models() == []


def test_new_activation_atomically_supersedes_prior_model_revision(tmp_path: Path) -> None:
    project, store, benchmark, first_plan = _planned_cycle(tmp_path)
    baseline, candidate = _complete_and_evaluate(
        project, benchmark, first_plan, tmp_path
    )
    store.compare_evaluations(
        first_plan["id"],
        baseline_evaluation_id=baseline["id"],
        candidate_evaluation_id=candidate["id"],
    )
    first = store.promote(
        first_plan["id"],
        reviewed_by="Model reviewer",
        reason="First fixed-benchmark improvement.",
    )

    second = store.start(
        model_level="category_head",
        model_identity={"name": "hardware-components", "revision": "candidate-v2"},
        predictions=[{"id": "fan-2", "confidence": 0.2, "impact": 1.0}],
        correction_budget=1,
    )
    store.record_corrections(
        second["id"],
        [{"prediction_id": "fan-2", "corrected_class": "centrifugal_fan"}],
        corrected_by="Feature benchmark reviewer",
    )
    second_plan = store.plan_retraining(
        second["id"],
        backend="offline-pytorch",
        benchmark_dataset_id=benchmark["id"],
    )
    second_baseline, second_candidate = _complete_and_evaluate(
        project,
        benchmark,
        second_plan,
        tmp_path,
        candidate_correct=90,
        candidate_incorrect=10,
        candidate_intersection=90,
    )
    store.compare_evaluations(
        second["id"],
        baseline_evaluation_id=second_baseline["id"],
        candidate_evaluation_id=second_candidate["id"],
    )
    promoted = store.promote(
        second["id"],
        reviewed_by="Model reviewer",
        reason="Second fixed-benchmark improvement.",
    )

    models = store.active_models()
    assert [(item["cycle_id"], item["status"]) for item in models] == [
        (first["id"], "SUPERSEDED"),
        (promoted["id"], "ACTIVE"),
    ]
    assert promoted["activation"]["supersedes_active_revision_id"] == models[0]["id"]
    audit = audit_active_learning(project)
    assert audit["invalid_cycle_ids"] == []
    assert audit["invalid_model_revision_ids"] == []

    rollback = store.rollback(
        models[1]["id"],
        reviewed_by="Model rollback reviewer",
        reason="Deployment telemetry requires restoring the last verified revision.",
    )
    after_rollback = store.active_models()
    assert [(item["cycle_id"], item["status"]) for item in after_rollback] == [
        (first["id"], "ACTIVE"),
        (promoted["id"], "ROLLED_BACK"),
    ]
    assert rollback["rolled_back_revision"]["id"] == models[1]["id"]
    assert rollback["restored_revision"]["id"] == models[0]["id"]
    assert store.rollback(
        models[1]["id"],
        reviewed_by="Model rollback reviewer",
        reason="Deployment telemetry requires restoring the last verified revision.",
    )["reused"] is True
    rollback_audit = audit_active_learning(project)
    assert rollback_audit["invalid_rollback_ids"] == []
    receipt = export_receipt(project)
    assert receipt["acceptance"]["metrics"]["active_learning"][
        "invalid_rollback_ids"
    ] == []
    assert verify_receipt(project.root / receipt["path"], project=project)["valid"] is True
