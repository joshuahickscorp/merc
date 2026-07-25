from __future__ import annotations

from pathlib import Path

import pytest
from PIL import Image, ImageDraw

from blender_vision.cameras.solver import CameraSolver
from blender_vision.core.models import EvidenceClass, RegistrationClass
from blender_vision.evidence.measurements import MeasurementGridStore, MeasurementStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.features.store import FeatureStore
from blender_vision.projects.store import ProjectStore
from blender_vision.review.service import ReviewService


def _reference(project: ProjectStore, source: Path, *, label: str = "front") -> dict:
    Image.new("RGB", (800, 600), "gray").save(source)
    return ReferenceIngestor(project).import_file(
        source, rights_state="TEST_FIXTURE", viewpoint_label=label
    )


def test_measurement_grid_and_correction_preserve_evidence_history(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Measurement review")
    reference = _reference(project, tmp_path / "front.png")
    measurements = MeasurementStore(project)
    measurement = measurements.add(
        "known_overall_dimension",
        {"axis": "x", "millimetres": 100.0},
        evidence_class=EvidenceClass.MEASURED,
        certainty="bounded",
        uncertainty={"millimetres": 0.5},
        reference_ids=[reference["id"]],
    )
    grid = MeasurementGridStore(project).create(
        reference["id"],
        {
            "normalized_coordinates": True,
            "vanishing_points": {"x": [1.5, 0.5], "y": [-0.5, 3.1666667], "z": [-0.5, -0.8333333]},
            "vanishing_lines": [{"start": [0.1, 0.2], "end": [0.9, 0.2]}],
            "rulers": [{"start": [0.2, 0.3], "end": [0.8, 0.3]}],
            "calibration_targets": [{"kind": "known_edge", "measurement_id": measurement["id"]}],
            "symmetry_axes": [{"start": [0.5, 0.1], "end": [0.5, 0.9]}],
            "snap": {"enabled": True, "tolerance_px": 6},
            "multi_view_links": [],
            "millimetre_conversion": {"method": "bound_measurement"},
        },
        created_by="Metrology reviewer",
        uncertainty={"pixels": 0.5},
        scale_measurement_id=measurement["id"],
    )

    corrected = measurements.correct(
        measurement["id"],
        {"axis": "x", "millimetres": 101.0},
        uncertainty={"millimetres": 0.1},
        corrected_by="Metrology reviewer",
        reason="Caliper reading rechecked",
    )

    assert grid["coordinate_space"] == "normalized_image"
    assert grid["definition"]["snap"]["enabled"] is True
    assert corrected["value"]["millimetres"] == 101.0
    assert corrected["value"]["correction_history"][0]["prior_value"]["millimetres"] == 100.0
    assert project.status()["counts"]["measurement_grids"] == 1


def test_vanishing_point_camera_is_constrained_but_explicitly_nonmetric(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Vanishing points")
    reference = _reference(project, tmp_path / "front.png")
    grid = MeasurementGridStore(project).create(
        reference["id"],
        {
            "normalized_coordinates": True,
            "vanishing_points": {
                "x": [1.5, 0.5],
                "y": [-0.5, 3.1666666666666665],
                "z": [-0.5, -0.8333333333333334],
            },
        },
        created_by="Perspective reviewer",
    )

    solution = CameraSolver(project).solve_vanishing_points([grid["id"]])

    camera = solution["cameras"][0]
    assert camera["intrinsics"]["fx"] == pytest.approx(800.0, rel=1e-6)
    assert camera["registration_class"] == RegistrationClass.APPROXIMATE_VISUAL
    assert camera["diagnostics"]["translation_authority"] == "approximate_only"
    assert solution["approved"] is False


def test_calibration_board_recovers_metric_camera_when_vision_extra_is_available(
    tmp_path: Path,
) -> None:
    pytest.importorskip("cv2")
    project = ProjectStore.create(tmp_path / "project", "Calibration board")
    source = tmp_path / "board.png"
    image = Image.new("RGB", (600, 500), "white")
    draw = ImageDraw.Draw(image)
    square_px = 50
    for row in range(7):
        for column in range(8):
            color = "black" if (row + column) % 2 == 0 else "white"
            left, top = 100 + column * square_px, 75 + row * square_px
            draw.rectangle((left, top, left + square_px, top + square_px), fill=color)
    image.save(source)
    ReferenceIngestor(project).import_file(
        source, rights_state="TEST_FIXTURE", viewpoint_label="calibration"
    )
    pitch = MeasurementStore(project).add(
        "array_pitch",
        {"millimetres": 10.0, "kind": "chessboard_square"},
        evidence_class=EvidenceClass.MEASURED,
        certainty="exact",
        uncertainty={"millimetres": 0.01},
    )

    solution = CameraSolver(project).solve_calibration_board(
        columns=7,
        rows=6,
        square_size_measurement_id=pitch["id"],
    )

    camera = solution["cameras"][0]
    assert camera["registration_class"] == RegistrationClass.METRIC
    assert camera["intrinsics"]["fx"] > 0
    assert camera["diagnostics"]["quality"]["registered_feature_count"] == 42
    assert solution["approved"] is False


def test_review_actions_link_features_request_views_and_gate_tier_decisions(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Operator decisions")
    first = _reference(project, tmp_path / "front.png")
    second = _reference(project, tmp_path / "rear.png", label="rear")
    feature = FeatureStore(project).add(
        "USB-C",
        parent_component="rear-panel",
        observations=[{"reference_id": first["id"], "normalized_bbox": [0.1, 0.1, 0.2, 0.2]}],
        reference_ids=[first["id"]],
        confidence=0.7,
        evidence_class=EvidenceClass.SINGLE_VIEW_OBSERVED,
        model_revision="manual-v1",
    )
    review = ReviewService(project)

    linked = review.action(
        "feature.link",
        {
            "id": feature["id"],
            "reference_id": second["id"],
            "observation": {"normalized_bbox": [0.3, 0.3, 0.4, 0.4]},
            "reviewer": "Evidence reviewer",
            "reason": "Same connector is visible from the rear",
        },
    )
    request = review.action(
        "capture.request",
        {
            "direction": "detail",
            "region": "rear I/O",
            "instructions": "Capture normal to the panel",
            "reviewer": "Capture planner",
            "reason": "Port recess depth remains uncertain",
        },
    )
    tier = review.action(
        "tier.review",
        {
            "fidelity": "L3",
            "accepted": True,
            "reviewer": "Acceptance reviewer",
            "reason": "Requested before a receipt exists to verify the safety gate",
        },
    )

    assert linked["reference_ids"] == sorted([first["id"], second["id"]])
    assert request["status"] == "requested"
    assert tier["accepted"] is False
    assert tier["receipt_satisfies"] is False
    snapshot = review.snapshot()
    assert len(snapshot["capture_requests"]) == 1
    assert len(snapshot["tier_reviews"]) == 1
