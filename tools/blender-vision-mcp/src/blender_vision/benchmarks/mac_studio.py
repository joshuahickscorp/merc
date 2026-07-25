from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.models import EvidenceClass, FidelityLevel
from blender_vision.core.util import atomic_write_json
from blender_vision.evidence.measurements import MeasurementCertainty, MeasurementStore
from blender_vision.features.store import FeatureStore
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.coordinator import Coordinator


def load_manifest(repository_root: Path) -> dict[str, Any]:
    repository_path = (
        repository_root
        / "tools"
        / "blender-vision-mcp"
        / "benchmarks"
        / "mac_studio"
        / "benchmark.json"
    )
    packaged_path = Path(__file__).with_name("data") / "mac_studio.json"
    path = repository_path if repository_path.is_file() else packaged_path
    if not path.is_file():
        raise FileNotFoundError(
            "Mac Studio benchmark manifest is missing from both the repository and package: "
            f"{repository_path}, {packaged_path}"
        )
    return json.loads(path.read_text(encoding="utf-8"))


def import_feature_candidates(
    project: ProjectStore,
    manifest: dict[str, Any],
    inventory: dict[str, Any],
    *,
    scene_id: str,
    scene_artifact_digest: str,
) -> dict[str, Any]:
    """Bind manifest hypotheses to audited Blender objects without approving them."""
    objects = {item["name"]: item for item in inventory.get("objects", [])}
    scale = float(inventory.get("canonical_transform", {}).get("scale_to_millimetres", 1000.0))
    store = FeatureStore(project)
    features: list[dict[str, Any]] = []
    missing_objects: list[str] = []
    source = manifest["manufacturer_spec"]
    for candidate in manifest.get("feature_candidates", []):
        names = candidate.get("scene_objects", [])
        for index, object_name in enumerate(names):
            scene_object = objects.get(object_name)
            if scene_object is None:
                missing_objects.append(object_name)
                continue
            dimensions_mm = [float(value) * scale for value in scene_object["dimensions"]]
            position_mm = [float(value) * scale for value in scene_object["location"]]
            provenance = [
                {
                    "kind": "scene_audit",
                    "scene_id": scene_id,
                    "scene_artifact_digest": scene_artifact_digest,
                    "object_name": object_name,
                }
            ]
            if candidate.get("manufacturer_claim"):
                provenance.append(
                    {
                        "kind": "manufacturer_spec",
                        "source": source["source"],
                        "retrieved_date": source["retrieved_date"],
                        "claim": candidate["manufacturer_claim"],
                    }
                )
            if candidate.get("known_issue"):
                provenance.append(
                    {
                        "kind": "strict_audit_finding",
                        "source": "review/pass6/gate_report.json",
                        "finding": candidate["known_issue"],
                    }
                )
            suffix = f"-{index + 1}" if len(names) > 1 else ""
            features.append(
                store.add(
                    candidate["type"],
                    feature_id=f"mac-studio-{candidate['id_prefix']}{suffix}",
                    parent_component=candidate["parent_component"],
                    dimensions={
                        "x_mm": dimensions_mm[0],
                        "y_mm": dimensions_mm[1],
                        "z_mm": dimensions_mm[2],
                    },
                    coordinate_frame="bvmcp_right_handed_z_up_millimetres",
                    observations=[
                        {
                            "kind": "scene_object",
                            "scene_id": scene_id,
                            "scene_artifact_digest": scene_artifact_digest,
                            "object_name": object_name,
                            "position_mm": position_mm,
                            "dimensions_mm": dimensions_mm,
                        }
                    ],
                    confidence=float(candidate["confidence"]),
                    uncertainty={
                        "classification": "unreviewed_model_hypothesis",
                        "known_issue": candidate.get("known_issue"),
                    },
                    evidence_class=EvidenceClass(candidate["evidence_class"]),
                    model_revision=scene_artifact_digest,
                    coverage_group=candidate["coverage_group"],
                    hero_surface=bool(candidate.get("hero_surface", False)),
                    provenance=provenance,
                )
            )
    return {
        "features": features,
        "feature_count": len(features),
        "missing_scene_objects": sorted(set(missing_objects)),
        "human_approval": False,
    }


def bootstrap_mac_studio(
    project_root: Path,
    repository_root: Path,
    *,
    include_marketing_reference: bool = False,
) -> dict[str, Any]:
    repository_root = repository_root.expanduser().resolve()
    manifest = load_manifest(repository_root)
    scene_path = repository_root / manifest["authoritative_scene"]
    project = ProjectStore.create(
        project_root,
        "Mac Studio",
        target_fidelity=FidelityLevel.L3,
        metadata={
            "benchmark": "mac_studio",
            "benchmark_schema_version": manifest["schema_version"],
            "manufacturer_spec_source": manifest["manufacturer_spec"]["source"],
            "known_blockers": manifest["known_blockers"],
            "required_feature_groups": manifest["required_feature_groups"],
        },
    )
    coordinator = Coordinator(project)
    scene_job = coordinator.run("scene.import", {"source": str(scene_path)})
    if scene_job["status"] != "succeeded":
        raise RuntimeError(f"Mac Studio scene import failed: {scene_job['error']}")
    measurement_store = MeasurementStore(project)
    spec = manifest["manufacturer_spec"]
    measurements = []
    for axis, key in (("x", "x_width"), ("y", "y_depth"), ("z", "z_height")):
        measurements.append(
            measurement_store.add(
                "known_overall_dimension",
                {
                    "axis": axis,
                    "millimetres": spec["dimensions_mm"][key],
                    "source": spec["source"],
                    "retrieved_date": spec["retrieved_date"],
                    "scope": "renderable_scene_envelope",
                },
                evidence_class=EvidenceClass.MANUFACTURER_SPEC,
                certainty=MeasurementCertainty.BOUNDED,
                uncertainty=spec["uncertainty"],
            )
        )
    grille = manifest["rear_grille_evidence"]
    grille_measurements = (
        ("line", "field_width_mm", "width"),
        ("line", "field_height_mm", "height"),
        ("point", "z_center_mm", "z_center"),
        ("array_pitch", "pitch_mm", "pitch"),
        ("circle", "hole_diameter_mm", "diameter"),
    )
    for measurement_type, key, role in grille_measurements:
        measurements.append(
            measurement_store.add(
                measurement_type,
                {
                    "role": f"rear_hero_grille_{role}",
                    "millimetres": grille[key],
                    "source": grille["source"],
                    "source_reference": grille["source_reference"],
                    "source_available": grille["source_available"],
                },
                evidence_class=EvidenceClass.SINGLE_VIEW_OBSERVED,
                certainty=MeasurementCertainty.DERIVED,
                uncertainty=grille["uncertainty"],
            )
        )
    legacy_artifacts = []
    artifacts = ArtifactStore(project)
    for relative in manifest["legacy_truth"]:
        source = repository_root / relative
        if source.is_file():
            legacy_artifacts.append(
                {"source": relative, "artifact": artifacts.ingest_file(source).to_dict()}
            )
    reference_job = None
    if include_marketing_reference:
        marketing = manifest["marketing_reference"]
        reference_job = coordinator.run(
            "reference.import",
            {
                "source": str(repository_root / marketing["path"]),
                "rights_state": "INTERNAL_REPOSITORY",
                "viewpoint_label": "front marketing render non-authoritative",
            },
        )
    audit_job = coordinator.run("project.audit", {})
    if audit_job["status"] != "succeeded":
        raise RuntimeError(f"Mac Studio audit failed: {audit_job['error']}")
    inventory = audit_job["result"]["audit"]["inventory"]
    scene = SceneStore(project).get(scene_job["result"]["id"])
    feature_candidates = import_feature_candidates(
        project,
        manifest,
        inventory,
        scene_id=scene["id"],
        scene_artifact_digest=scene["artifact_digest"],
    )
    actual = inventory["canonical_bounds_mm"]["dimensions"]
    expected = [
        spec["dimensions_mm"]["x_width"],
        spec["dimensions_mm"]["y_depth"],
        spec["dimensions_mm"]["z_height"],
    ]
    dimensional_residual = {
        axis: {
            "scene_mm": actual[index],
            "manufacturer_mm": expected[index],
            "delta_mm": actual[index] - expected[index],
            "relative_percent": (actual[index] - expected[index]) / expected[index] * 100.0,
        }
        for index, axis in enumerate(("x", "y", "z"))
    }
    bootstrap = {
        "schema_version": 1,
        "project": str(project.root),
        "manifest": manifest,
        "scene_job_id": scene_job["id"],
        "audit_job_id": audit_job["id"],
        "reference_job_id": reference_job["id"] if reference_job else None,
        "measurements": measurements,
        "feature_candidates": feature_candidates,
        "legacy_artifacts": legacy_artifacts,
        "dimensional_residual": dimensional_residual,
        "accepted": False,
        "reason": "Bootstrap records evidence and blockers; it does not claim L3 acceptance.",
    }
    atomic_write_json(project.root / "benchmark-bootstrap.json", bootstrap)
    return bootstrap
