from __future__ import annotations

import json
from pathlib import Path

import pytest
from PIL import Image

from blender_vision.acceptance.receipts import export_receipt
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.benchmarks.beast import BeastBenchmarkAuditor
from blender_vision.benchmarks.external import bootstrap_external_benchmark
from blender_vision.cameras.solver import CameraSolver
from blender_vision.core.models import EvidenceClass
from blender_vision.core.util import utc_now
from blender_vision.datasets.store import DatasetStore
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.coverage_atlas import SurfaceCoverageAtlas
from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.evidence.targets import TargetResolver
from blender_vision.geometry.synthetic_views import SyntheticViewStore
from blender_vision.intelligence.active_learning import ActiveLearningStore
from blender_vision.intelligence.packs import CategoryPackRegistry
from blender_vision.orchestration.services import WarmServiceRegistry
from blender_vision.parametric.components import ComponentType
from blender_vision.projects.store import ProjectStore
from blender_vision.workflows.service import ReconstructionService


def test_large_scale_component_dsl_declares_required_editable_constructs() -> None:
    assert {
        "SplineBodySection",
        "LoftedSurface",
        "WheelArch",
        "PanelCut",
        "PanelGap",
        "SurfaceCrease",
        "Aerofoil",
        "Duct",
        "Vent",
        "LightHousing",
        "GlassPanel",
        "TireProfile",
        "WheelSpokeArray",
        "BrakeAssembly",
        "DiffuserChannel",
        "UnderbodyPanel",
        "Bezier",
        "NURBS",
        "CurveNetwork",
        "Sweep",
        "PatchSurface",
        "ControlledShrinkwrap",
        "RetopologyCage",
    } <= {item.value for item in ComponentType}


def test_physical_measurement_requires_calibration_provenance(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Physical inputs")
    measurement = MeasurementStore(project).add_physical(
        "manual_caliper",
        {"millimetres": 24.8, "measured_feature": "mounting boss"},
        evidence_class=EvidenceClass.MEASURED,
        uncertainty={"millimetres": 0.02, "confidence": 0.95},
        calibration_state={
            "state": "calibrated",
            "calibration_id": "CAL-2026-07",
            "method": "gauge block",
        },
    )
    assert measurement["value"]["physical_source"] == "manual_caliper"
    assert measurement["value"]["calibration"]["state"] == "calibrated"
    assert measurement["uncertainty"]["millimetres"] == 0.02


def test_synthetic_views_never_become_acceptance_evidence(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Synthetic views")
    image = tmp_path / "hypothesis.png"
    image.write_bytes(b"hypothetical-view")
    artifact = ArtifactStore(project).ingest_file(image, media_type="image/png")
    view = SyntheticViewStore(project).register(
        artifact.digest,
        source_kind="current_candidate",
        generator={"backend": "Blender", "revision": "4.2.1"},
        input_reference_ids=[],
        view_identity={"azimuth_degrees": 45.0, "elevation_degrees": 15.0},
        consistency={"cross_view_geometry": 0.94, "cycle_consistency": 0.91},
    )
    assert view["evidence_class"] == "SYNTHETIC_HYPOTHESIS"
    assert view["acceptance_eligible"] is False
    receipt = export_receipt(project)
    assert receipt["acceptance"]["metrics"]["synthetic_views"] == {
        "count": 1,
        "coherent_count": 1,
        "acceptance_eligible_count": 0,
    }


def test_active_learning_persists_correction_and_non_regression_cycle(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Active learning")
    dataset = DatasetStore(project).register(
        "fixed benchmark",
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
        model_identity={"name": "vehicle-components", "revision": "v3"},
        predictions=[
            {"id": "wheel-1", "confidence": 0.35, "impact": 0.95},
            {"id": "door-1", "confidence": 0.98, "impact": 0.2},
        ],
        correction_budget=1,
    )
    assert cycle["correction_requests"][0]["id"] == "wheel-1"
    corrected = store.record_corrections(
        cycle["id"],
        [{"prediction_id": "wheel-1", "corrected_class": "front_wheel"}],
        corrected_by="Benchmark annotator",
    )
    assert corrected["status"] == "READY_TO_RETRAIN"
    planned = store.plan_retraining(
        cycle["id"], backend="offline-pytorch", benchmark_dataset_id=dataset["id"]
    )
    assert planned["status"] == "RETRAINING_PLANNED"
    assert planned["retraining_plan"]["correction_dataset_id"]
    assert planned["retraining_plan"]["training_run_id"]
    assert store.plan_retraining(
        cycle["id"], backend="offline-pytorch", benchmark_dataset_id=dataset["id"]
    )["retraining_plan"] == planned["retraining_plan"]


def test_surface_atlas_reports_unseen_regions_and_observation_quality(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Coverage atlas")
    atlas = SurfaceCoverageAtlas(project)
    cells = atlas.bootstrap(target_id="target-1", regions=["front", "underbody"])["cells"]
    report = atlas.analyze("target-1")
    assert report["coverage_fraction"] == 0.0
    assert report["unresolved_regions"] == ["front", "underbody"]
    atlas.observe(
        cells[0]["id"],
        observation_id="reference-front",
        incidence_angle_degrees=12.0,
        resolution_pixels=2048,
        occlusion_fraction=0.05,
        reflection_risk="low",
        evidence_class="MULTI_VIEW_OBSERVED",
        uncertainty={"classification": "bounded"},
    )
    report = atlas.analyze("target-1")
    assert report["coverage_fraction"] == 0.5
    assert report["unresolved_regions"] == ["underbody"]


def test_surface_atlas_synchronization_retires_wrong_category_cells(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Coverage ontology change")
    atlas = SurfaceCoverageAtlas(project)
    atlas.bootstrap(target_id="target-1", regions=["door", "wheel"])
    synchronized = atlas.bootstrap(
        target_id="target-1", regions=["wheel", "belly_pan"], synchronize=True
    )
    report = atlas.analyze("target-1")
    assert synchronized["cell_count"] == 2
    assert report["cell_count"] == 2
    assert report["retired_cell_count"] == 1
    assert report["unresolved_regions"] == ["belly_pan", "wheel"]


def test_governed_source_observation_requires_acquisition_and_is_idempotent(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Governed atlas observation")
    target = TargetResolver(project).resolve(
        {"manufacturer": "NASA", "model": "Perseverance Rover"}
    )
    atlas = SurfaceCoverageAtlas(project)
    atlas.bootstrap(target_id=target["id"], regions=["belly_pan"])
    source = EvidenceAcquisitionStore(project).register_source(
        target["id"],
        {
            "origin": "user://reviewed-underbody",
            "publisher": "user",
            "page_title": "underbody",
            "authority_class": "user_owned",
            "target_variant": {},
            "viewpoint": "underbody",
            "quality_score": 1.0,
        },
        rights={"status": "USER_OWNED", "internal_use": True, "redistribution": False},
        reviewed_by="Evidence owner",
    )
    observation = [
        {
            "region": "belly_pan",
            "incidence_angle_degrees": 8.0,
            "resolution_pixels": 2048,
            "occlusion_fraction": 0.05,
            "reflection_risk": "low",
            "evidence_class": "USER_CAPTURE",
            "uncertainty": {"classification": "bounded"},
        }
    ]
    with pytest.raises(ValueError, match="acquired source"):
        atlas.observe_governed_source(source["id"], observations=observation)
    image_path = tmp_path / "underbody.png"
    Image.new("RGB", (64, 64), "gray").save(image_path)
    EvidenceAcquisitionStore(project).acquire_local(source["id"], image_path)
    first = atlas.observe_governed_source(source["id"], observations=observation)
    repeated = atlas.observe_governed_source(source["id"], observations=observation)
    assert first["cells"][0]["observation_count"] == 1
    assert repeated["cells"][0]["observation_count"] == 1
    assert repeated["coverage"]["coverage_fraction"] == 1.0


def test_warm_services_evict_low_reuse_first(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Warm services")
    registry = WarmServiceRegistry(project)
    registry.update(
        "vggt", status="WARM", memory_gb=10.0, expected_reuse=0.9
    )
    registry.update(
        "segmentation", status="WARM", memory_gb=4.0, expected_reuse=0.1
    )
    eviction = registry.evict_for_pressure(required_free_gb=3.0)
    assert eviction["freed_gb"] == 4.0
    assert [item["name"] for item in eviction["evicted"]] == ["segmentation"]


def test_beast_stage_audit_records_incomplete_gates_instead_of_overclaiming(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Stage audit")
    report = BeastBenchmarkAuditor(project).audit(4)
    assert report["status"] == "INCOMPLETE"
    assert any(not check["passed"] for check in report["checks"])
    assert (project.root / report["path"]).is_file()


def test_stage_three_requires_nonempty_semantics_and_acquired_governed_evidence(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Strict Stage 3 audit")
    target = TargetResolver(project).resolve(
        {"manufacturer": "NVIDIA", "model": "GeForce RTX 5090 Founders Edition"}
    )
    source = EvidenceAcquisitionStore(project).register_source(
        target["id"],
        {
            "origin": "https://example.test/rtx-front.png",
            "publisher": "Fixture publisher",
            "page_title": "RTX front",
            "authority_class": "manufacturer_authoritative",
            "target_variant": target["target"],
            "viewpoint": "front",
            "quality_score": 0.9,
        },
        rights={"status": "FIXTURE", "internal_use": True, "redistribution": False},
    )
    EvidenceAcquisitionStore(project).review_governance(
        source["id"],
        reviewed_by="Fixture rights reviewer",
        source_terms_review="approved",
        privacy_review="not_applicable",
    )

    report = BeastBenchmarkAuditor(project).audit(3)
    checks = {item["name"]: item["passed"] for item in report["checks"]}

    assert checks["target variant resolved"] is True
    assert checks["autonomous evidence acquisition recorded"] is False
    assert checks["semantic model complete"] is False


def test_beast_auditor_rejects_tampered_camera_and_missing_export_artifact(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Artifact-bound audit")
    reference_path = tmp_path / "front.png"
    Image.new("RGB", (100, 80), "gray").save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path, rights_state="FIXTURE", viewpoint_label="front"
    )
    camera = CameraSolver(project).import_manual(
        [
            {
                "reference_id": reference["id"],
                "model": "PINHOLE",
                "width": 100,
                "height": 80,
                "intrinsics": {"fx": 90.0, "fy": 90.0, "cx": 50.0, "cy": 40.0},
                "world_from_camera": [
                    [1.0, 0.0, 0.0, 0.0],
                    [0.0, 1.0, 0.0, 500.0],
                    [0.0, 0.0, 1.0, 50.0],
                    [0.0, 0.0, 0.0, 1.0],
                ],
                "confidence": 0.7,
                "registration_class": "approximate_visual_registration",
                "evidence_class": "INFERRED_LOW_CONFIDENCE",
            }
        ]
    )
    CameraSolver(project).approve(
        camera["id"], reviewer="Fixture camera reviewer", reason="Fixture matrix reviewed"
    )
    initial_facts = BeastBenchmarkAuditor(project)._facts()
    assert initial_facts["fixed_camera_solution_count"] == 1
    assert initial_facts["fixed_metric_camera_solution_count"] == 0
    stage_three_checks = {
        item["name"]: item["passed"]
        for item in BeastBenchmarkAuditor._checks(3, initial_facts)
    }
    assert stage_three_checks["approved metric camera solution recorded"] is False

    blend = tmp_path / "candidate.blend"
    blend.write_bytes(b"editable fixture")
    scene = ArtifactStore(project).ingest_file(blend)
    with project.connection() as connection:
        row = connection.execute(
            "SELECT solution_json FROM camera_solutions WHERE id=?", (camera["id"],)
        ).fetchone()
        document = json.loads(row["solution_json"])
        document["cameras"][0]["intrinsics"]["fx"] = 91.0
        connection.execute(
            "UPDATE camera_solutions SET solution_json=? WHERE id=?",
            (json.dumps(document), camera["id"]),
        )

        missing_digest = "0" * 64
        connection.execute(
            "INSERT INTO scene_assets(id,artifact_digest,original_name,relative_path,created_at) "
            "VALUES(?,?,?,?,?)",
            ("scene-1", scene.digest, "candidate.blend", scene.path, utc_now()),
        )
        connection.execute(
            "INSERT INTO artifacts(digest,size,media_type,relative_path,source_name,created_at) "
            "VALUES(?,?,?,?,?,?)",
            (
                missing_digest,
                12,
                "model/gltf-binary",
                "artifacts/sha256/00/00/" + missing_digest,
                "missing.glb",
                utc_now(),
            ),
        )
        connection.execute(
            "INSERT INTO exports(id,scene_id,artifact_digest,format,relative_path,config_json,"
            "worker_json,created_at) VALUES(?,?,?,?,?,?,?,?)",
            (
                "export-1",
                "scene-1",
                missing_digest,
                "glb",
                "exports/missing.glb",
                "{}",
                "{}",
                utc_now(),
            ),
        )

    facts = BeastBenchmarkAuditor(project)._facts()
    assert facts["fixed_camera_solution_count"] == 0
    assert facts["fixed_metric_camera_solution_count"] == 0
    assert facts["export_formats"] == []


def test_external_stage_bootstrap_has_no_private_model_and_stays_incomplete(
    tmp_path: Path,
) -> None:
    repository_root = Path(__file__).parents[1]
    result = bootstrap_external_benchmark(
        tmp_path / "perseverance",
        repository_root,
        reviewed_by="Benchmark evidence reviewer",
    )
    project = ProjectStore.open(Path(result["project"]))
    report = BeastBenchmarkAuditor(project).audit(4)

    assert project.project()["metadata"]["private_starting_model"] is False
    assert result["rights_audit"]["governance_complete"] is True
    model_source = next(
        source
        for source in result["sources"]
        if source["source"]["viewpoint"].startswith("public 3D landmark")
    )
    assert model_source["source"]["known_scale"]["binding"] == "official_dimensions_mm"
    assert "independent acceptance" in model_source["source"]["excluded_evidence"]
    assert model_source["status"] == "DISCOVERED"
    assert len(result["measurements"]) == 3
    assert result["surface_atlas"]["cell_count"] == len(
        CategoryPackRegistry().get("space_rovers")["ontology"]
    )
    assert SurfaceCoverageAtlas(project).analyze(result["target"]["id"])[
        "coverage_fraction"
    ] == 0.0
    assert report["status"] == "INCOMPLETE"
    dimension_check = next(
        check for check in report["checks"] if check["name"] == "official dimensions recorded"
    )
    assert dimension_check["passed"] is True


def test_vehicle_parametric_seed_plans_editable_six_wheel_rover() -> None:
    specs = ReconstructionService._seed_component_specs(
        {"x": 3000.0, "y": 2700.0, "z": 2200.0},
        category="vehicles",
        target={"model": "Mars 2020 Perseverance Rover"},
        evidence_bindings=["dimension-x", "dimension-y", "dimension-z"],
    )

    tires = [item for item in specs if item.type == ComponentType.TIRE_PROFILE]
    assert len(tires) == 6
    assert all(item.parameters["axis"] == "y" for item in tires)
    assert {item.id for item in specs} >= {"seed_body", "seed_underbody", "seed_mast"}
    assert {item.id for item in specs} >= {
        "seed_rocker_bogie_left",
        "seed_rocker_bogie_right",
        "seed_robotic_arm",
        "seed_navigation_camera",
        "seed_hazard_camera_front",
        "seed_hazard_camera_rear",
        "seed_sample_caching",
        "seed_radioisotope_power_source",
        "seed_high_gain_antenna",
    }

    space_rover_specs = ReconstructionService._seed_component_specs(
        {"x": 3000.0, "y": 2700.0, "z": 2200.0},
        category="space_rovers",
        target={"model": "Mars 2020 Perseverance Rover"},
        evidence_bindings=["dimension-x", "dimension-y", "dimension-z"],
    )
    assert [item.id for item in space_rover_specs] == [item.id for item in specs]
    assert len(specs) == 18
    assert all(item.evidence_bindings for item in specs)
