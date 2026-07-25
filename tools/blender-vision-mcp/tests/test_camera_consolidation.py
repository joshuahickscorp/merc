from __future__ import annotations

import json
from pathlib import Path

import pytest
from PIL import Image

from blender_vision.cameras.solver import CameraSolver
from blender_vision.cameras.state import validate_complete_camera_state
from blender_vision.core.util import canonical_json
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore


def _camera(reference_id: str, x: float) -> dict:
    return {
        "reference_id": reference_id,
        "model": "PINHOLE",
        "width": 100,
        "height": 80,
        "intrinsics": {"fx": 90.0, "fy": 90.0, "cx": 50.0, "cy": 40.0},
        "world_from_camera": [
            [1.0, 0.0, 0.0, x],
            [0.0, 1.0, 0.0, 500.0],
            [0.0, 0.0, 1.0, 50.0],
            [0.0, 0.0, 0.0, 1.0],
        ],
        "confidence": 0.55,
        "registration_class": "approximate_visual_registration",
        "evidence_class": "SINGLE_VIEW_OBSERVED",
        "diagnostics": {"authority": "unapproved fixture hypothesis"},
    }


def test_camera_consolidation_preserves_pose_authority_and_exact_coverage(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Camera consolidation")
    references = []
    for index in range(2):
        source = tmp_path / f"view-{index}.png"
        Image.new("RGB", (100, 80), "gray").save(source)
        references.append(
            ReferenceIngestor(project).import_file(
                source, rights_state="SYNTHETIC_OWNED", viewpoint_label=f"view-{index}"
            )
        )
    first = CameraSolver(project).import_manual([_camera(references[0]["id"], 0.0)])
    second = CameraSolver(project).import_manual([_camera(references[1]["id"], 100.0)])

    result = CameraSolver(project).consolidate_solutions([first["id"], second["id"]])

    assert result["approved"] is False
    assert result["consolidation"]["authority_upgraded"] is False
    assert result["consolidation"]["exact_acceptance_reference_coverage"] is True
    assert len(result["cameras"]) == 2
    assert all(item["immutable_sha256"] for item in result["cameras"])
    assert [item["world_from_camera"][0][3] for item in result["cameras"]] == [0.0, 100.0]
    assert all(
        item["registration_class"] == "approximate_visual_registration"
        for item in result["cameras"]
    )


def test_camera_consolidation_refuses_incomplete_acceptance_coverage(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Incomplete consolidation")
    reference_ids = []
    for index in range(2):
        source = tmp_path / f"view-{index}.png"
        Image.new("RGB", (100, 80), "gray").save(source)
        reference_ids.append(
            ReferenceIngestor(project)
            .import_file(source, rights_state="SYNTHETIC_OWNED")
            ["id"]
        )
    partial = CameraSolver(project).import_manual([_camera(reference_ids[0], 0.0)])

    with pytest.raises(ValueError, match="exactly cover acceptance references"):
        CameraSolver(project).consolidate_solutions([partial["id"]])


def test_camera_approval_recomputes_snapshot_hash_and_rejects_tampering(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Camera tamper boundary")
    source = tmp_path / "view.png"
    Image.new("RGB", (100, 80), "gray").save(source)
    reference = ReferenceIngestor(project).import_file(
        source, rights_state="SYNTHETIC_OWNED"
    )
    solution = CameraSolver(project).import_manual([_camera(reference["id"], 0.0)])
    with project.connection() as connection:
        row = connection.execute(
            "SELECT solution_json FROM camera_solutions WHERE id=?", (solution["id"],)
        ).fetchone()
        document = json.loads(row["solution_json"])
        document["cameras"][0]["intrinsics"]["fx"] = 91.0
        connection.execute(
            "UPDATE camera_solutions SET solution_json=? WHERE id=?",
            (json.dumps(document), solution["id"]),
        )

    with pytest.raises(ValueError, match="immutable SHA-256"):
        CameraSolver(project).approve(
            solution["id"], reviewer="Camera QA", reason="Tampered fixture"
        )
    with project.connection() as connection:
        approved = connection.execute(
            "SELECT approved FROM camera_solutions WHERE id=?", (solution["id"],)
        ).fetchone()[0]
    assert approved == 0


def test_camera_validator_rejects_self_rehashed_inconsistent_extrinsics(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Camera structural boundary")
    source = tmp_path / "view.png"
    Image.new("RGB", (100, 80), "gray").save(source)
    reference = ReferenceIngestor(project).import_file(
        source, rights_state="SYNTHETIC_OWNED"
    )
    camera = CameraSolver(project).import_manual([_camera(reference["id"], 0.0)])["cameras"][0]
    camera["extrinsics"]["world_from_camera"][0][3] = 12.0
    immutable = {key: value for key, value in camera.items() if key != "immutable_sha256"}
    camera["immutable_sha256"] = __import__("hashlib").sha256(
        canonical_json(immutable)
    ).hexdigest()

    with pytest.raises(ValueError, match="extrinsics disagree"):
        validate_complete_camera_state(camera)
