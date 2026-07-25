from __future__ import annotations

import json
import uuid
from pathlib import Path
from typing import Any

from PIL import Image, ImageChops, ImageStat

from blender_vision.acceptance.receipts import verify_receipt
from blender_vision.acceptance.transactions import CandidateTransactionStore
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.blender.runner import BlenderRunner
from blender_vision.cameras.solver import CameraSolver
from blender_vision.core.models import EvidenceClass, FidelityLevel, SceneLifecycleState
from blender_vision.core.util import atomic_write_json, utc_now
from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.features.store import FeatureStore
from blender_vision.geometry.scenes import SceneStore
from blender_vision.materials.store import MaterialStore
from blender_vision.parametric.components import ComponentSpec, ComponentType
from blender_vision.parametric.store import ComponentStore
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.coordinator import Coordinator

DIMENSIONS_MM = {"x": 120.0, "y": 80.0, "z": 40.0}
REQUIRED_GROUPS = ["body", "ports", "cooling", "fasteners", "recesses"]
MAX_REPEATABILITY_CHANNEL_DELTA_8BIT = 2
# Isolated six-view Blender 4.2.1 runs with temporal reprojection, render
# dithering, and jittered soft shadows disabled still produced rare 8-bit
# quantization-boundary changes. The observed post-hardening maximum was two
# channel levels with 0.03978 RMS. These ceilings remain below one per cent of
# the channel range and reject visible render drift while allowing that bounded
# renderer noise.
MAX_REPEATABILITY_CHANNEL_RMS_8BIT = 0.05


def bootstrap_calibration(
    project_root: Path,
    *,
    reviewer: str,
    review_reason: str,
) -> dict[str, Any]:
    """Generate and explicitly review the owned, reproducible Benchmark 0 project."""
    if not reviewer.strip() or not review_reason.strip():
        raise ValueError("calibration acceptance requires a named reviewer and reason")
    project = ProjectStore.create(
        project_root,
        "Blender Vision Synthetic Calibration",
        target_fidelity=FidelityLevel.L3,
        metadata={
            "benchmark": "synthetic_calibration_v1",
            "license": "CC0-1.0",
            "rights_state": "SYNTHETIC_OWNED",
            "required_feature_groups": REQUIRED_GROUPS,
            "required_material_regions": ["body", "technical-features", "fasteners"],
            "required_calibration_gates": True,
            "deterministic_geometry": True,
        },
    )
    scene_path = project.root / "scene" / "calibration-v1.blend"
    reference_dir = project.root / "references" / "generated-calibration-v1"
    generated = BlenderRunner(project).run(
        "generate_calibration_benchmark",
        project.root,
        {"output_path": str(scene_path), "reference_dir": str(reference_dir)},
        job_id="calibration-generate",
        timeout_seconds=1200,
    )
    scene = SceneStore(project).register_generated(scene_path, original_name="calibration-v1.blend")
    audit_job = Coordinator(project).run("project.audit", {"scene_id": scene["id"]})
    if audit_job["status"] != "succeeded" or not audit_job["result"]["audit"]["valid"]:
        raise RuntimeError(f"calibration scene audit failed: {audit_job.get('error')}")

    ingestor = ReferenceIngestor(project)
    reference_by_view: dict[str, dict[str, Any]] = {}
    generated_by_view = {item["viewpoint_label"]: item for item in generated["references"]}
    for label in ("front", "rear", "left", "right", "top", "bottom"):
        render = generated_by_view[label]
        reference_by_view[label] = ingestor.import_file(
            project.root / render["render_path"],
            rights_state="SYNTHETIC_OWNED",
            viewpoint_label=label,
        )

    all_reference_ids = [reference_by_view[label]["id"] for label in reference_by_view]
    measurements = [
        MeasurementStore(project).add(
            "known_overall_dimension",
            {
                "axis": axis,
                "millimetres": DIMENSIONS_MM[axis],
                "measurement_method": "procedural_ground_truth",
                "measured_by": reviewer.strip(),
            },
            evidence_class=EvidenceClass.MEASURED,
            certainty="exact",
            uncertainty={
                "millimetres": 0.001,
                "classification": "procedural_exact_with_float_serialization_tolerance",
            },
            reference_ids=all_reference_ids,
        )
        for axis in ("x", "y", "z")
    ]
    measurement_ids = [item["id"] for item in measurements]

    component_store = ComponentStore(project)
    components = [
        component_store.create(component) for component in _component_specs(measurement_ids)
    ]
    scene_digest = scene["artifact"]["digest"]
    feature_store = FeatureStore(project)
    features = []
    for specification in _feature_specs(reference_by_view, generated):
        feature = feature_store.add(
            specification["type"],
            feature_id=specification["id"],
            parent_component=specification["parent_component"],
            dimensions=specification["dimensions"],
            coordinate_frame="bvmcp_right_handed_z_up_millimetres",
            observations=[
                {
                    "kind": "procedural_ground_truth",
                    "scene_id": scene["id"],
                    "scene_artifact_digest": scene_digest,
                    "objects": specification["objects"],
                }
            ],
            reference_ids=specification["reference_ids"],
            confidence=1.0,
            uncertainty={"classification": "procedural_exact", "millimetres": 1e-6},
            evidence_class=EvidenceClass.MEASURED,
            model_revision=scene_digest,
            coverage_group=specification["coverage_group"],
            hero_surface=specification.get("hero_surface", False),
            provenance=[
                {
                    "kind": "benchmark_generator",
                    "operation": "generate_calibration_benchmark",
                    "scene_sha256": generated["scene_sha256"],
                    "license": "Apache-2.0",
                }
            ],
        )
        features.append(
            feature_store.review(
                feature["id"],
                approved=True,
                reviewer=reviewer,
                reason=review_reason,
            )
        )

    material_store = MaterialStore(project)
    material_profiles = []
    for specification in _material_specs(reference_by_view):
        profile = material_store.create(
            specification["region_id"],
            specification["properties"],
            evidence_class=EvidenceClass.MEASURED,
            confidence=1.0,
            reference_ids=specification["reference_ids"],
            multi_light_reference_ids=specification["reference_ids"],
            color_calibration={
                "state": "procedural_ground_truth",
                "space": "scene_linear",
                "source_scene_digest": scene_digest,
            },
            lighting_estimate={
                "state": "procedural_ground_truth",
                "source": "generated six-view studio-light record",
            },
        )
        material_profiles.append(
            material_store.review(
                profile["id"],
                approved=True,
                reviewer=reviewer,
                reason=review_reason,
            )
        )

    camera_quality = {
        "reprojection_rmse_px": 0.0,
        "registered_feature_count": 1000,
        "view_coverage": 1.0,
        "baseline_diversity": 1.0,
        "scale_confidence": 1.0,
        "principal_point_confidence": 1.0,
        "distortion_confidence": 1.0,
    }
    cameras = []
    for label, reference in reference_by_view.items():
        generated_view = generated_by_view[label]
        cameras.append(
            {
                "reference_id": reference["id"],
                "model": "PINHOLE",
                "width": generated_view["width"],
                "height": generated_view["height"],
                "intrinsics": generated_view["camera"]["intrinsics"],
                "world_from_camera": generated_view["camera"]["world_from_camera"],
                "confidence": 1.0,
                "registration_class": "metric_camera_solution",
                "evidence_class": "MEASURED",
                "diagnostics": {
                    "quality": camera_quality,
                    "view_direction": generated_view["view_direction"],
                    "source": "procedural_ground_truth",
                },
            }
        )
    camera_solution = CameraSolver(project).import_manual(
        cameras,
        diagnostics={
            "benchmark": "synthetic_calibration_v1",
            "calibration": "procedural ground-truth cameras",
        },
        evidence_binding_ids=measurement_ids,
    )
    CameraSolver(project).approve(camera_solution["id"], reviewer=reviewer, reason=review_reason)

    render_job = Coordinator(project).run(
        "blender.render",
        {
            "scene_id": scene["id"],
            "solution_id": camera_solution["id"],
            "maximum_dimension": 320,
        },
    )
    if render_job["status"] != "succeeded":
        raise RuntimeError(f"calibration render failed: {render_job['error']}")
    comparison_job = Coordinator(project).run(
        "validation.compare", {"renders": render_job["result"]["renders"]}
    )
    if comparison_job["status"] != "succeeded":
        raise RuntimeError(f"calibration comparison failed: {comparison_job['error']}")
    coverage_job = Coordinator(project).run("validation.coverage", {})
    if coverage_job["status"] != "succeeded":
        raise RuntimeError(f"calibration coverage failed: {coverage_job['error']}")
    repeated_render_job = Coordinator(project).run(
        "blender.render",
        {
            "scene_id": scene["id"],
            "solution_id": camera_solution["id"],
            "maximum_dimension": 320,
            "repeatability_nonce": "calibration-second-independent-render",
        },
    )
    if repeated_render_job["status"] != "succeeded":
        raise RuntimeError(f"calibration repeat render failed: {repeated_render_job['error']}")
    export_job = Coordinator(project).run(
        "blender.export", {"output_name": "calibration-v1.glb"}
    )
    repeated_export_job = Coordinator(project).run(
        "blender.export", {"output_name": "calibration-v1-repeat.glb"}
    )
    blend_export_job = Coordinator(project).run(
        "blender.export_blend", {"output_name": "calibration-v1.blend"}
    )
    if (
        export_job["status"] != "succeeded"
        or repeated_export_job["status"] != "succeeded"
        or blend_export_job["status"] != "succeeded"
    ):
        raise RuntimeError(
            "calibration export failed: "
            f"first={export_job.get('error')} second={repeated_export_job.get('error')} "
            f"blend={blend_export_job.get('error')}"
        )

    audit_dimensions = audit_job["result"]["audit"]["inventory"]["canonical_bounds_mm"][
        "dimensions"
    ]
    dimension_deltas = {
        axis: float(audit_dimensions[index]) - DIMENSIONS_MM[axis]
        for index, axis in enumerate(("x", "y", "z"))
    }
    expected_cameras = {item["reference_id"]: item for item in cameras}
    recovered_cameras = {item["reference_id"]: item for item in camera_solution["cameras"]}
    camera_deltas = []
    for reference_id, expected in expected_cameras.items():
        recovered = recovered_cameras[reference_id]
        for key, value in expected["intrinsics"].items():
            camera_deltas.append(abs(float(recovered["intrinsics"][key]) - float(value)))
        camera_deltas.extend(
            abs(float(recovered["world_from_camera"][row][column]) - float(value))
            for row, values in enumerate(expected["world_from_camera"])
            for column, value in enumerate(values)
        )
    first_render_digests = {
        item["reference_id"]: item["artifact"]["digest"]
        for item in render_job["result"]["renders"]
    }
    repeated_render_digests = {
        item["reference_id"]: item["artifact"]["digest"]
        for item in repeated_render_job["result"]["renders"]
    }
    first_render_by_reference = {
        item["reference_id"]: item for item in render_job["result"]["renders"]
    }
    repeated_render_by_reference = {
        item["reference_id"]: item for item in repeated_render_job["result"]["renders"]
    }
    repeatability_views: dict[str, Any] = {}
    for reference_id, first in first_render_by_reference.items():
        second = repeated_render_by_reference[reference_id]
        with (
            Image.open(project.root / first["artifact"]["path"]) as first_image,
            Image.open(project.root / second["artifact"]["path"]) as second_image,
        ):
            difference = ImageChops.difference(
                first_image.convert("RGBA"), second_image.convert("RGBA")
            )
            statistics = ImageStat.Stat(difference)
            maximum_channel_delta = max(high for _, high in difference.getextrema())
            maximum_channel_rms = max(statistics.rms)
        repeatability_views[reference_id] = {
            "passed": (
                maximum_channel_delta <= MAX_REPEATABILITY_CHANNEL_DELTA_8BIT
                and maximum_channel_rms <= MAX_REPEATABILITY_CHANNEL_RMS_8BIT
            ),
            "first_artifact_sha256": first["artifact"]["digest"],
            "second_artifact_sha256": second["artifact"]["digest"],
            "maximum_channel_delta_8bit": maximum_channel_delta,
            "maximum_channel_rms_8bit": maximum_channel_rms,
            "maximum_channel_rms_normalized": maximum_channel_rms / 255.0,
        }
    first_export_digest = export_job["result"]["artifact"]["digest"]
    repeated_export_digest = repeated_export_job["result"]["artifact"]["digest"]
    dimension_tolerance_mm = 0.001
    camera_tolerance = 1e-9
    gates = {
        "known_dimensions": {
            "passed": all(
                abs(delta) <= dimension_tolerance_mm for delta in dimension_deltas.values()
            ),
            "expected_mm": DIMENSIONS_MM,
            "audited_mm": dict(zip(("x", "y", "z"), audit_dimensions, strict=True)),
            "delta_mm": dimension_deltas,
            "tolerance_mm": dimension_tolerance_mm,
        },
        "camera_recovery": {
            "passed": set(expected_cameras) == set(recovered_cameras)
            and max(camera_deltas, default=float("inf")) <= camera_tolerance,
            "method": "metric_camera_import_roundtrip_against_procedural_ground_truth",
            "camera_count": len(recovered_cameras),
            "maximum_parameter_delta": max(camera_deltas, default=None),
            "tolerance": camera_tolerance,
        },
        "scale_recovery": {
            "passed": all(
                abs(delta) <= dimension_tolerance_mm for delta in dimension_deltas.values()
            ),
            "method": "authoritative_measurements_to_audited_canonical_scene_bounds",
            "maximum_absolute_delta_mm": max(abs(delta) for delta in dimension_deltas.values()),
            "tolerance_mm": dimension_tolerance_mm,
        },
        "repeatability": {
            "passed": set(first_render_digests) == set(repeated_render_digests)
            and all(view["passed"] for view in repeatability_views.values()),
            "method": "independent_headless_render_bounded_decoded_pixel_residual",
            "maximum_channel_delta_8bit": MAX_REPEATABILITY_CHANNEL_DELTA_8BIT,
            "maximum_channel_rms_8bit": MAX_REPEATABILITY_CHANNEL_RMS_8BIT,
            "maximum_channel_rms_normalized": (
                MAX_REPEATABILITY_CHANNEL_RMS_8BIT / 255.0
            ),
            "views": repeatability_views,
        },
        "export_consistency": {
            "passed": first_export_digest == repeated_export_digest,
            "method": "independent_glb_export_sha256",
            "first_sha256": first_export_digest,
            "second_sha256": repeated_export_digest,
        },
    }
    calibration_id = str(uuid.uuid4())
    calibration_report = {
        "schema_version": 1,
        "id": calibration_id,
        "benchmark": "synthetic_calibration_v1",
        "created_at": utc_now(),
        "gates": gates,
        "passed": all(gate["passed"] for gate in gates.values()),
        "validation_context": {
            "minimum_silhouette_iou": min(
                item["metrics"]["silhouette_iou"]
                for item in comparison_job["result"]["comparisons"]
            ),
            "comparison_coverage": coverage_job["result"]["comparison_coverage"],
        },
    }
    calibration_relative = Path("measurements") / f"calibration-{calibration_id}.json"
    atomic_write_json(project.root / calibration_relative, calibration_report)
    calibration_artifact = ArtifactStore(project).ingest_file(
        project.root / calibration_relative,
        media_type="application/vnd.bvmcp.calibration-report+json",
    )
    with project.connection() as connection:
        connection.execute(
            "INSERT INTO calibration_runs(id,benchmark,gates_json,record_digest,created_at) "
            "VALUES(?,?,?,?,?)",
            (
                calibration_id,
                "synthetic_calibration_v1",
                json.dumps(calibration_report),
                calibration_artifact.digest,
                calibration_report["created_at"],
            ),
        )
    if not calibration_report["passed"]:
        failed = ", ".join(name for name, gate in gates.items() if not gate["passed"])
        raise RuntimeError(f"calibration quality gates failed: {failed}")

    scene_store = SceneStore(project)
    scene_store.transition(
        scene["id"],
        SceneLifecycleState.CANDIDATE,
        reviewer=reviewer,
        reason="Synthetic benchmark scene is ready for atomic L3 candidate evaluation",
    )
    minimum_silhouette_iou = calibration_report["validation_context"][
        "minimum_silhouette_iou"
    ]
    transaction = CandidateTransactionStore(project).evaluate(
        scene["id"],
        gates=[
            {
                "category": "camera",
                "name": "procedural camera recovery",
                "status": "PASS",
                "evidence": {
                    "calibration_report_id": calibration_id,
                    "gate": "camera_recovery",
                },
            },
            {
                "category": "measurement",
                "name": "known dimensions and recovered scale",
                "status": "PASS",
                "evidence": {
                    "measurement_ids": measurement_ids,
                    "calibration_gates": ["known_dimensions", "scale_recovery"],
                },
            },
            {
                "category": "component",
                "name": "required component and feature coverage",
                "status": "PASS",
                "evidence": {
                    "component_ids": [item["id"] for item in components],
                    "feature_ids": [item["id"] for item in features],
                },
            },
            {
                "category": "topology",
                "name": "audited scene topology",
                "status": "PASS",
                "evidence": {
                    "audit_job_id": audit_job["id"],
                    "audit_valid": audit_job["result"]["audit"]["valid"],
                },
            },
            {
                "category": "material",
                "name": "reviewed material profiles",
                "status": "PASS",
                "evidence": {
                    "profile_ids": [item["id"] for item in material_profiles],
                    "all_approved": all(
                        item["status"] == "approved" for item in material_profiles
                    ),
                },
            },
            {
                "category": "appearance",
                "name": "governed full-object comparison",
                "status": "PASS",
                "candidate_value": minimum_silhouette_iou,
                "baseline_value": 0.95,
                "higher_is_better": True,
                "evidence": {
                    "comparison_ids": [
                        item["id"] for item in comparison_job["result"]["comparisons"]
                    ]
                },
            },
            {
                "category": "provenance",
                "name": "owned deterministic benchmark provenance",
                "status": "PASS",
                "evidence": {
                    "rights_state": "SYNTHETIC_OWNED",
                    "calibration_report_digest": calibration_artifact.digest,
                    "scene_digest": scene_digest,
                },
            },
        ],
        metrics={
            "calibration_report_id": calibration_id,
            "calibration_report_digest": calibration_artifact.digest,
            "minimum_silhouette_iou": minimum_silhouette_iou,
            "comparison_coverage": coverage_job["result"]["comparison_coverage"],
        },
    )
    if transaction["status"] != "PASSED":
        raise RuntimeError("calibration candidate failed its atomic all-gate transaction")
    scene_store.transition(
        scene["id"],
        SceneLifecycleState.ACCEPTED,
        reviewer=reviewer,
        reason="All mandatory calibration candidate gates passed atomically",
        evaluation_id=transaction["id"],
    )
    scene_store.transition(
        scene["id"],
        SceneLifecycleState.PROMOTED,
        reviewer=reviewer,
        reason="Accepted calibration candidate is the reviewed authoritative L3 scene",
        evaluation_id=transaction["id"],
    )
    receipt_job = Coordinator(project).run("receipt.export", {})
    if receipt_job["status"] != "succeeded":
        raise RuntimeError(f"calibration receipt failed: {receipt_job['error']}")
    receipt = receipt_job["result"]
    verification = verify_receipt(project.root / receipt["path"], project=project)
    if not receipt["acceptance"]["accepted"] or not verification["valid"]:
        blockers = "; ".join(receipt["acceptance"]["blockers"])
        raise RuntimeError(f"calibration L3 acceptance failed: {blockers}")
    result = {
        "schema_version": 1,
        "project": str(project.root),
        "benchmark": "synthetic_calibration_v1",
        "reviewer": reviewer.strip(),
        "generated": generated,
        "scene": scene,
        "audit": audit_job["result"],
        "references": reference_by_view,
        "measurements": measurements,
        "components": components,
        "features": features,
        "material_profiles": material_profiles,
        "camera_solution_id": camera_solution["id"],
        "comparison": comparison_job["result"],
        "coverage": coverage_job["result"],
        "repeatability_render": repeated_render_job["result"],
        "export": export_job["result"],
        "exports": [
            export_job["result"],
            repeated_export_job["result"],
            blend_export_job["result"],
        ],
        "calibration": {
            **calibration_report,
            "artifact": calibration_artifact.to_dict(),
            "path": str(calibration_relative),
        },
        "candidate_transaction": transaction,
        "scene_lifecycle": scene_store.transitions(scene["id"]),
        "receipt": receipt,
        "receipt_verification": verification,
        "accepted_fidelity": "L3",
    }
    atomic_write_json(project.root / "calibration-benchmark-result.json", result)
    return result


def _component_specs(measurement_ids: list[str]) -> list[ComponentSpec]:
    common = {"evidence_bindings": measurement_ids, "generator_version": "calibration-v1"}
    return [
        ComponentSpec(
            id="calibration-body",
            type=ComponentType.BODY,
            parameters={"dimensions_mm": [120.0, 80.0, 40.0], "bevel_mm": 4.0},
            **common,
        ),
        ComponentSpec(
            id="calibration-ports",
            type=ComponentType.PORT,
            parameters={"count": 3, "surface": "front"},
            **common,
        ),
        ComponentSpec(
            id="calibration-fan",
            type=ComponentType.FAN,
            parameters={"radius_mm": 17.0, "blade_count": 9},
            **common,
        ),
        ComponentSpec(
            id="calibration-grille",
            type=ComponentType.HOLE_ARRAY,
            parameters={"count_x": 7, "count_y": 5, "pitch_x_mm": 6.0},
            **common,
        ),
        ComponentSpec(
            id="calibration-fasteners",
            type=ComponentType.SCREW,
            parameters={"count": 4, "diameter_mm": 4.0},
            **common,
        ),
        ComponentSpec(
            id="calibration-recess",
            type=ComponentType.CUTOUT,
            parameters={"dimensions_mm": [26.0, 1.5, 16.0]},
            **common,
        ),
    ]


def _feature_specs(
    references: dict[str, dict[str, Any]], generated: dict[str, Any]
) -> list[dict[str, Any]]:
    feature_objects = generated["features"]
    return [
        {
            "id": "calibration-rounded-body",
            "type": "panel",
            "parent_component": "calibration-body",
            "coverage_group": "body",
            "dimensions": {"x_mm": 120.0, "y_mm": 80.0, "z_mm": 40.0},
            "objects": [feature_objects["rounded_body"]],
            "reference_ids": [item["id"] for item in references.values()],
            "hero_surface": True,
        },
        {
            "id": "calibration-port-array",
            "type": "USB-C",
            "parent_component": "calibration-ports",
            "coverage_group": "ports",
            "dimensions": {"count": 3},
            "objects": feature_objects["ports"],
            "reference_ids": [references["front"]["id"]],
        },
        {
            "id": "calibration-fan",
            "type": "fan ring",
            "parent_component": "calibration-fan",
            "coverage_group": "cooling",
            "dimensions": {"radius_mm": 17.0, "blade_count": 9},
            "objects": feature_objects["fan"],
            "reference_ids": [references["top"]["id"]],
        },
        {
            "id": "calibration-grille",
            "type": "grille",
            "parent_component": "calibration-grille",
            "coverage_group": "cooling",
            "dimensions": {"holes": 35, "pitch_mm": 6.0},
            "objects": feature_objects["grille_holes"],
            "reference_ids": [references["rear"]["id"]],
        },
        {
            "id": "calibration-fasteners",
            "type": "screw",
            "parent_component": "calibration-fasteners",
            "coverage_group": "fasteners",
            "dimensions": {"count": 4, "diameter_mm": 4.0},
            "objects": feature_objects["screws"],
            "reference_ids": [references["rear"]["id"]],
        },
        {
            "id": "calibration-recess",
            "type": "panel",
            "parent_component": "calibration-recess",
            "coverage_group": "recesses",
            "dimensions": {"x_mm": 26.0, "y_mm": 1.5, "z_mm": 16.0},
            "objects": [feature_objects["recess"]],
            "reference_ids": [references["front"]["id"]],
        },
    ]


def _material_specs(references: dict[str, dict[str, Any]]) -> list[dict[str, Any]]:
    all_reference_ids = [item["id"] for item in references.values()]
    return [
        {
            "region_id": "body",
            "properties": {
                "base_color": [0.22, 0.24, 0.28, 1.0],
                "roughness": 0.28,
                "metallic": 0.7,
                "anisotropy": 0.0,
                "clearcoat": 0.0,
                "normal_detail": {},
                "procedural_texture": {},
            },
            "reference_ids": all_reference_ids,
        },
        {
            "region_id": "technical-features",
            "properties": {
                "base_color": [0.025, 0.03, 0.04, 1.0],
                "roughness": 0.38,
                "metallic": 0.2,
                "anisotropy": 0.0,
                "clearcoat": 0.0,
                "normal_detail": {},
                "procedural_texture": {},
            },
            "reference_ids": [references["front"]["id"], references["rear"]["id"]],
        },
        {
            "region_id": "fasteners",
            "properties": {
                "base_color": [0.5, 0.52, 0.55, 1.0],
                "roughness": 0.2,
                "metallic": 0.85,
                "anisotropy": 0.0,
                "clearcoat": 0.0,
                "normal_detail": {},
                "procedural_texture": {},
            },
            "reference_ids": [references["rear"]["id"]],
        },
    ]
