from __future__ import annotations

import hashlib
import json
import sqlite3
from pathlib import Path

from PIL import Image

from blender_vision.acceptance.receipts import export_receipt
from blender_vision.cameras.decisions import CameraDecisionStore
from blender_vision.cameras.solver import CameraSolver
from blender_vision.core.util import canonical_json
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore


def _camera(reference_id: str) -> dict:
    return {
        "reference_id": reference_id,
        "model": "PINHOLE",
        "width": 120,
        "height": 80,
        "intrinsics": {"fx": 100.0, "fy": 100.0, "cx": 60.0, "cy": 40.0},
        "world_from_camera": [
            [1.0, 0.0, 0.0, 0.0],
            [0.0, 1.0, 0.0, 500.0],
            [0.0, 0.0, 1.0, 50.0],
            [0.0, 0.0, 0.0, 1.0],
        ],
        "confidence": 0.8,
        "registration_class": "approximate_visual_registration",
        "evidence_class": "SINGLE_VIEW_OBSERVED",
    }


def _project_with_camera(tmp_path: Path) -> tuple[ProjectStore, dict]:
    project = ProjectStore.create(tmp_path / "project", "Camera decision transaction")
    image_path = tmp_path / "reference.png"
    Image.new("RGB", (120, 80), "gray").save(image_path)
    reference = ReferenceIngestor(project).import_file(
        image_path, rights_state="USER_OWNED", viewpoint_label="front"
    )
    solution = CameraSolver(project).import_manual([_camera(reference["id"])])
    return project, solution


def test_camera_approval_has_immutable_semantically_verified_decision(
    tmp_path: Path,
) -> None:
    project, solution = _project_with_camera(tmp_path)
    approved = CameraSolver(project).approve(
        solution["id"], reviewer="Camera reviewer", reason="Matrix and source view reviewed"
    )

    assert approved["approval"]["decision_digest"] == approved["decision_artifact"][
        "digest"
    ]
    verification = CameraDecisionStore(project).verify(solution["id"])
    assert verification["valid"] is True
    assert verification["decision"]["acceptance_performed"] is False
    with project.connection() as connection:
        assert connection.execute("SELECT COUNT(*) FROM camera_decisions").fetchone()[0] == 1

    with project.connection() as connection:
        row = connection.execute(
            "SELECT decision_json FROM camera_decisions WHERE solution_id=?",
            (solution["id"],),
        ).fetchone()
        decision = json.loads(row["decision_json"])
        decision["reviewer"] = "Forged reviewer"
        connection.execute(
            "UPDATE camera_decisions SET decision_json=? WHERE solution_id=?",
            (json.dumps(decision), solution["id"]),
        )

    assert CameraDecisionStore(project).verify(solution["id"])["valid"] is False
    acceptance = export_receipt(project)["acceptance"]
    assert solution["id"] in acceptance["metrics"]["camera"][
        "invalid_decision_solution_ids"
    ]
    assert "one or more camera review decisions lack a valid immutable receipt" in acceptance[
        "blockers"
    ]


def test_camera_decision_update_failure_rolls_back_ledger_and_solution(
    tmp_path: Path,
) -> None:
    project, solution = _project_with_camera(tmp_path)
    with project.connection() as connection:
        connection.execute(
            "CREATE TRIGGER fail_camera_decision_update BEFORE UPDATE ON camera_solutions "
            "BEGIN SELECT RAISE(ABORT, 'simulated camera decision failure'); END"
        )

    try:
        CameraSolver(project).approve(
            solution["id"], reviewer="Camera reviewer", reason="Simulated interruption"
        )
    except sqlite3.IntegrityError as error:
        assert "simulated camera decision failure" in str(error)
    else:
        raise AssertionError("camera decision failure trigger did not abort")

    with project.connection() as connection:
        row = connection.execute(
            "SELECT approved,decision_id,decision_digest,solution_json "
            "FROM camera_solutions WHERE id=?",
            (solution["id"],),
        ).fetchone()
        decision_count = connection.execute(
            "SELECT COUNT(*) FROM camera_decisions"
        ).fetchone()[0]
    assert decision_count == 0
    assert row["approved"] == 0
    assert row["decision_id"] is None
    assert row["decision_digest"] is None
    assert json.loads(row["solution_json"])["approval"]["state"] == "pending"


def test_legacy_named_camera_decision_migration_preserves_original_authority(
    tmp_path: Path,
) -> None:
    project, solution = _project_with_camera(tmp_path)
    reviewed_at = "2025-01-02T03:04:05+00:00"
    with project.connection() as connection:
        row = connection.execute(
            "SELECT solution_json FROM camera_solutions WHERE id=?", (solution["id"],)
        ).fetchone()
        document = json.loads(row["solution_json"])
        document["approved"] = True
        document["approval"] = {
            "state": "approved",
            "reviewer": "Legacy camera reviewer",
            "reason": "Original camera matrix review",
            "reviewed_at": reviewed_at,
        }
        connection.execute(
            "UPDATE camera_solutions SET solution_json=?,approved=1 WHERE id=?",
            (json.dumps(document), solution["id"]),
        )

    migrated = CameraDecisionStore(project).migrate_legacy(solution["id"])
    assert migrated["decision"]["reviewed_at"] == reviewed_at
    assert migrated["decision"]["reviewer"] == "Legacy camera reviewer"
    migration = migrated["decision"]["migration"]
    assert migration["authority"] == "EXISTING_NAMED_DECISION_ONLY"
    assert migration["new_human_review_performed"] is False
    assert migration["schema_version"] == 2
    assert migration["legacy_solution_snapshot_sha256"] == migration[
        "completed_solution_snapshot_sha256"
    ]
    assert migration["camera_completion"][0]["populated_fields"] == []
    assert migration["camera_completion"][0]["geometry_unchanged"] is True
    assert CameraDecisionStore(project).verify(solution["id"])["valid"] is True


def test_legacy_incomplete_camera_migration_only_adds_derived_state(
    tmp_path: Path,
) -> None:
    project, solution = _project_with_camera(tmp_path)
    reviewed_at = "2025-01-02T03:04:05+00:00"
    derived_fields = {
        "extrinsics",
        "distortion_model",
        "sensor_model",
        "crop",
        "resolution",
        "clipping",
        "coordinate_transform",
        "camera_source_identity",
        "solve_method",
        "approval_state",
        "immutable_sha256",
    }
    with project.connection() as connection:
        row = connection.execute(
            "SELECT solution_json FROM camera_solutions WHERE id=?", (solution["id"],)
        ).fetchone()
        document = json.loads(row["solution_json"])
        original_camera = dict(document["cameras"][0])
        legacy_camera = {
            key: value for key, value in original_camera.items() if key not in derived_fields
        }
        document["cameras"] = [legacy_camera]
        document["approved"] = True
        document["approval"] = {
            "state": "approved",
            "reviewer": "Legacy camera reviewer",
            "reason": "Original camera matrix review",
            "reviewed_at": reviewed_at,
        }
        legacy_snapshot_sha256 = hashlib.sha256(canonical_json(document)).hexdigest()
        connection.execute(
            "UPDATE camera_solutions SET solution_json=?,approved=1 WHERE id=?",
            (json.dumps(document), solution["id"]),
        )

    migrated = CameraDecisionStore(project).migrate_legacy(solution["id"])
    completed_camera = migrated["cameras"][0]
    migration = migrated["decision"]["migration"]
    completion = migration["camera_completion"][0]
    assert completed_camera["intrinsics"] == legacy_camera["intrinsics"]
    assert completed_camera["world_from_camera"] == legacy_camera["world_from_camera"]
    assert set(completion["populated_fields"]) == derived_fields
    assert completion["geometry_sha256_before"] == completion["geometry_sha256_after"]
    assert completion["geometry_unchanged"] is True
    assert migration["legacy_solution_snapshot_sha256"] == legacy_snapshot_sha256
    assert migration["legacy_solution_snapshot"] == document
    assert migration["legacy_solution_snapshot_sha256"] != migration[
        "completed_solution_snapshot_sha256"
    ]
    assert migrated["approval"]["reviewed_at"] == reviewed_at
    assert CameraDecisionStore(project).verify(solution["id"])["valid"] is True


def test_approving_legacy_pending_camera_completes_only_derived_state(
    tmp_path: Path,
) -> None:
    project, solution = _project_with_camera(tmp_path)
    derived_fields = {
        "extrinsics",
        "distortion_model",
        "sensor_model",
        "crop",
        "resolution",
        "clipping",
        "coordinate_transform",
        "camera_source_identity",
        "solve_method",
        "approval_state",
        "immutable_sha256",
    }
    with project.connection() as connection:
        row = connection.execute(
            "SELECT solution_json FROM camera_solutions WHERE id=?", (solution["id"],)
        ).fetchone()
        document = json.loads(row["solution_json"])
        legacy_camera = {
            key: value
            for key, value in document["cameras"][0].items()
            if key not in derived_fields
        }
        document["cameras"] = [legacy_camera]
        connection.execute(
            "UPDATE camera_solutions SET solution_json=? WHERE id=?",
            (json.dumps(document), solution["id"]),
        )

    approved = CameraSolver(project).approve(
        solution["id"],
        reviewer="Current camera reviewer",
        reason="Reviewed the legacy camera framing",
    )
    migration = approved["decision"]["migration"]
    completed_camera = approved["cameras"][0]
    assert migration["authority"] == "DETERMINISTIC_PENDING_SNAPSHOT_COMPLETION"
    assert migration["new_human_review_performed"] is True
    assert completed_camera["intrinsics"] == legacy_camera["intrinsics"]
    assert completed_camera["world_from_camera"] == legacy_camera["world_from_camera"]
    assert set(migration["camera_completion"][0]["populated_fields"]) == derived_fields
    assert CameraDecisionStore(project).verify(solution["id"])["valid"] is True


def test_rejecting_legacy_pending_camera_uses_same_completion_receipt(
    tmp_path: Path,
) -> None:
    project, solution = _project_with_camera(tmp_path)
    with project.connection() as connection:
        row = connection.execute(
            "SELECT solution_json FROM camera_solutions WHERE id=?", (solution["id"],)
        ).fetchone()
        document = json.loads(row["solution_json"])
        for field in ("extrinsics", "immutable_sha256"):
            document["cameras"][0].pop(field)
        connection.execute(
            "UPDATE camera_solutions SET solution_json=? WHERE id=?",
            (json.dumps(document), solution["id"]),
        )

    rejected = CameraSolver(project).reject(
        solution["id"],
        reviewer="Current camera reviewer",
        reason="Framing does not match the source",
    )
    assert rejected["approval"]["state"] == "rejected"
    assert rejected["decision"]["migration"]["authority"] == (
        "DETERMINISTIC_PENDING_SNAPSHOT_COMPLETION"
    )
    assert CameraDecisionStore(project).verify(solution["id"])["valid"] is True
