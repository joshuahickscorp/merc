from __future__ import annotations

import json
from pathlib import Path

from PIL import Image

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.cameras.solver import CameraSolver
from blender_vision.core.models import EvidenceClass
from blender_vision.datasets.store import DatasetStore, TrainingStore
from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.features.detector import FeatureDetectionImporter, detect_label_mask
from blender_vision.geometry.scenes import SceneStore
from blender_vision.optimization.engine import OptimizationEngine
from blender_vision.parametric.components import ComponentSpec, ComponentType
from blender_vision.parametric.store import ComponentStore
from blender_vision.projects.store import ProjectStore
from blender_vision.visual.oracle import VisualOracleStore


def _project_with_scene(tmp_path: Path) -> ProjectStore:
    project = ProjectStore.create(tmp_path / "project", "Intelligence")
    blend = tmp_path / "fixture.blend"
    blend.write_bytes(b"BLENDER-v300-fixture")
    SceneStore(project).import_blend(blend)
    return project


def test_dataset_training_and_evaluation_are_hash_and_license_bound(tmp_path: Path) -> None:
    project = _project_with_scene(tmp_path)
    dataset = DatasetStore(project).plan_synthetic(
        "Technical product synthetic", sample_count=2, seed=17
    )
    assert dataset["status"] == "planned"
    assert dataset["manifest"]["deterministic_preview"][0]["sample_index"] == 0
    rendered = project.root / "training" / "synthetic-index.json"
    rendered.write_text('{"sample_count":2}', encoding="utf-8")
    artifact = ArtifactStore(project).ingest_file(rendered)
    generated = DatasetStore(project).mark_generated(
        dataset["id"], artifact_digests=[artifact.digest], sample_count=2
    )
    assert generated["status"] == "generated"

    training = TrainingStore(project).plan(
        dataset["id"], backend="hardware-feature-baseline", configuration={"epochs": 3}
    )
    assert training["configuration"]["network_allowed"] is False
    checkpoint = project.root / "training" / "model.bin"
    checkpoint.write_bytes(b"fixture-model-weights")
    completed = TrainingStore(project).import_result(
        training["id"],
        checkpoint,
        metrics={"validation_f1": 0.9},
        license_record={"license": "Apache-2.0", "commercial_use": True},
        model_revision="fixture-v1",
    )
    assert completed["status"] == "completed"
    assert completed["result"]["checkpoint_sha256"] == completed["checkpoint_digest"]
    predictions = project.root / "training" / "predictions.json"
    predictions.write_text(
        json.dumps(
            {
                "sample_count": 2,
                "counts": {
                    "true_positive": 8,
                    "false_positive": 2,
                    "false_negative": 1,
                    "mask_intersection": 90,
                    "mask_union": 100,
                },
            }
        ),
        encoding="utf-8",
    )
    evaluation = TrainingStore(project).evaluate(
        dataset["id"], predictions, training_run_id=training["id"]
    )
    assert evaluation["metrics"]["precision"] == 0.8
    assert evaluation["metrics"]["mask_iou"] == 0.9


def test_synthetic_label_detector_and_import_remain_unapproved(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Detector")
    mask = tmp_path / "labels.png"
    image = Image.new("RGB", (20, 20), "black")
    for x in range(4, 12):
        for y in range(6, 10):
            image.putpixel((x, y), (12, 34, 56))
    image.save(mask)
    detections = detect_label_mask(mask, {"USB-C": [12, 34, 56]})
    assert detections[0]["bounding_box_xyxy"] == [4, 6, 12, 10]
    reference = ReferenceIngestor(project).import_file(
        mask, rights_state="SYNTHETIC_OWNED", viewpoint_label="label pass"
    )
    imported = FeatureDetectionImporter(project).import_detections(
        reference["id"],
        [{**detections[0], "parent_component": "rear-panel"}],
        model_revision="detector-fixture-v1",
        license_record={"license": "Apache-2.0", "commercial_use": True},
    )
    assert imported["features"][0]["evidence_class"] == "INFERRED_HIGH_CONFIDENCE"
    assert imported["features"][0]["human_approval"] is False


def test_visual_oracle_and_multi_tier_optimization_preserve_authority(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Oracle")
    reference_path = tmp_path / "reference.png"
    Image.new("RGB", (32, 32), "white").save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path, rights_state="SYNTHETIC_OWNED"
    )
    camera = CameraSolver(project).import_manual(
        [
            {
                "reference_id": reference["id"],
                "model": "PINHOLE",
                "width": 32,
                "height": 32,
                "intrinsics": {"fx": 30.0, "fy": 30.0, "cx": 16.0, "cy": 16.0},
                "world_from_camera": [
                    [1.0, 0.0, 0.0, 0.0],
                    [0.0, 1.0, 0.0, 100.0],
                    [0.0, 0.0, 1.0, 10.0],
                    [0.0, 0.0, 0.0, 1.0],
                ],
                "confidence": 0.4,
                "registration_class": "approximate_visual_registration",
                "evidence_class": "INFERRED_LOW_CONFIDENCE",
            }
        ]
    )
    oracle_path = project.root / "geometry" / "oracle.ply"
    oracle_path.write_text(
        "ply\nformat ascii 1.0\nelement vertex 0\nend_header\n", encoding="utf-8"
    )
    oracle = VisualOracleStore(project).register(
        oracle_path,
        kind="gaussian_splat",
        camera_solution_ids=[camera["id"]],
        training_configuration={"iterations": 100},
        license_record={"license": "Apache-2.0", "commercial_use": True},
    )
    assert oracle["commercial_eligible"] is True
    assert "cannot establish dimensions" in oracle["configuration"]["authority"]

    ComponentStore(project).create(
        ComponentSpec(id="panel", type=ComponentType.PANEL, parameters={"width_mm": 10.0})
    )
    evidence = MeasurementStore(project).add(
        "line",
        {"millimetres": 12.0},
        evidence_class=EvidenceClass.MEASURED,
        uncertainty={"millimetres": 0.1},
    )
    proposal = OptimizationEngine(project).propose(
        "panel",
        tier="black_box",
        method="bounded_candidate_search",
        candidates=[
            {"parameters": {"width_mm": 10.0}, "terms": {"measurement": 2.0}, "baseline": True},
            {"parameters": {"width_mm": 12.0}, "terms": {"measurement": 0.0}},
        ],
        evidence_binding_ids=[evidence["id"]],
    )
    assert proposal["status"] == "proposed"
    assert ComponentStore(project).get("panel")["parameters"]["width_mm"] == 10.0
    accepted = OptimizationEngine(project).review(
        proposal["id"], accepted=True, reviewer="Optimization QA", reason="Loss trace reviewed"
    )
    assert accepted["status"] == "accepted"
    assert ComponentStore(project).get("panel")["parameters"]["width_mm"] == 12.0
