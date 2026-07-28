from __future__ import annotations

import fnmatch
import json
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.models import EvidenceClass, FidelityLevel
from blender_vision.core.util import atomic_write_json
from blender_vision.evidence.measurements import MeasurementCertainty, MeasurementStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.features.store import FeatureStore
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.coordinator import Coordinator

BENCHMARK_DIRECTORIES = {
    "dgx_spark": "dgx",
    "rtx_5090_fe": "rtx_5090",
}
AXES = ("x", "y", "z")


def load_device_manifest(benchmark: str, repository_root: Path) -> dict[str, Any]:
    directory = BENCHMARK_DIRECTORIES.get(benchmark)
    if directory is None:
        raise ValueError(f"unsupported device benchmark: {benchmark}")
    repository_root = repository_root.expanduser().resolve()
    candidates = [
        repository_root
        / "tools"
        / "blender-vision-mcp"
        / "benchmarks"
        / directory
        / "benchmark.json",
        repository_root / "benchmarks" / directory / "benchmark.json",
        Path(__file__).with_name("data") / f"{benchmark}.json",
    ]
    for path in candidates:
        if path.is_file():
            return json.loads(path.read_text(encoding="utf-8"))
    raise FileNotFoundError(
        f"{benchmark} manifest is missing from repository and package locations: {candidates}"
    )


def _matches(name: str, patterns: list[str]) -> bool:
    return any(fnmatch.fnmatchcase(name, pattern) for pattern in patterns)


def _combined_bounds(objects: list[dict[str, Any]], scale: float) -> dict[str, Any]:
    bounded = [item for item in objects if isinstance(item.get("world_bounds"), dict)]
    if not bounded:
        raise ValueError("device dimensional audit requires per-object world bounds")
    minimum = [
        min(float(item["world_bounds"]["minimum"][index]) for item in bounded) * scale
        for index in range(3)
    ]
    maximum = [
        max(float(item["world_bounds"]["maximum"][index]) for item in bounded) * scale
        for index in range(3)
    ]
    return {
        "minimum_mm": dict(zip(AXES, minimum, strict=True)),
        "maximum_mm": dict(zip(AXES, maximum, strict=True)),
        "dimensions_mm": {
            axis: maximum[index] - minimum[index] for index, axis in enumerate(AXES)
        },
        "object_count": len(bounded),
        "objects": sorted(item["name"] for item in bounded),
    }


def evaluate_device_dimensions(
    manifest: dict[str, Any], inventory: dict[str, Any]
) -> dict[str, Any]:
    scale = float(inventory.get("canonical_transform", {}).get("scale_to_millimetres", 1000.0))
    mesh_objects = [item for item in inventory.get("objects", []) if item.get("type") == "MESH"]
    patterns = list(manifest["body_envelope"]["object_patterns"])
    selected = [item for item in mesh_objects if _matches(str(item["name"]), patterns)]
    if not selected:
        raise ValueError(f"body envelope patterns matched no scene objects: {patterns}")
    body = _combined_bounds(selected, scale)
    expected = manifest["manufacturer_spec"]["dimensions_mm"]
    tolerance = float(manifest["body_envelope"]["tolerance_mm"])
    checks = {}
    for axis in AXES:
        actual = float(body["dimensions_mm"][axis])
        target = float(expected[axis])
        checks[axis] = {
            "actual_mm": actual,
            "manufacturer_mm": target,
            "delta_mm": actual - target,
            "absolute_delta_mm": abs(actual - target),
            "tolerance_mm": tolerance,
            "passed": abs(actual - target) <= tolerance,
        }
    anchors = []
    by_name = {str(item["name"]): item for item in mesh_objects}
    for specification in manifest.get("anchor_object_checks", []):
        item = by_name.get(specification["object"])
        if item is None:
            anchors.append({**specification, "passed": False, "reason": "object missing"})
            continue
        dimensions = [float(value) * scale for value in item["world_bounds"]["dimensions"]]
        index = AXES.index(specification["axis"])
        actual = dimensions[index]
        target = float(specification["millimetres"])
        allowed = float(specification.get("tolerance_mm", tolerance))
        anchors.append(
            {
                **specification,
                "actual_mm": actual,
                "delta_mm": actual - target,
                "passed": abs(actual - target) <= allowed,
            }
        )
    return {
        "scope": manifest["body_envelope"]["scope"],
        "body_bounds": body,
        "checks": checks,
        "anchor_checks": anchors,
        "passed": all(item["passed"] for item in checks.values())
        and all(item["passed"] for item in anchors),
    }


def import_device_feature_candidates(
    project: ProjectStore,
    manifest: dict[str, Any],
    inventory: dict[str, Any],
    *,
    scene_id: str,
    scene_digest: str,
    reference_by_label: dict[str, dict[str, Any]],
) -> dict[str, Any]:
    scale = float(inventory.get("canonical_transform", {}).get("scale_to_millimetres", 1000.0))
    mesh_objects = [item for item in inventory.get("objects", []) if item.get("type") == "MESH"]
    store = FeatureStore(project)
    features = []
    missing_groups = []
    for candidate in manifest.get("feature_candidates", []):
        patterns = list(candidate["object_patterns"])
        matched = [item for item in mesh_objects if _matches(str(item["name"]), patterns)]
        if not matched:
            missing_groups.append(candidate["id"])
            continue
        bounds = _combined_bounds(matched, scale)
        labels = candidate.get("reference_viewpoints", [])
        reference_ids = [
            reference_by_label[label]["id"] for label in labels if label in reference_by_label
        ]
        feature = store.add(
            candidate["type"],
            feature_id=f"{manifest['benchmark']}-{candidate['id']}",
            parent_component=candidate["parent_component"],
            dimensions=bounds["dimensions_mm"],
            coordinate_frame="bvmcp_right_handed_z_up_millimetres",
            observations=[
                {
                    "kind": "scene_object_group",
                    "scene_id": scene_id,
                    "scene_artifact_digest": scene_digest,
                    "objects": bounds["objects"],
                    "object_count": bounds["object_count"],
                    "world_bounds": bounds,
                }
            ],
            reference_ids=reference_ids,
            confidence=float(candidate["confidence"]),
            uncertainty={
                "classification": "unreviewed_model_hypothesis",
                "expected_count": candidate.get("expected_count"),
                "actual_object_count": len(matched),
                "known_issue": candidate.get("known_issue"),
            },
            evidence_class=EvidenceClass(candidate["evidence_class"]),
            model_revision=scene_digest,
            coverage_group=candidate["coverage_group"],
            hero_surface=bool(candidate.get("hero_surface", False)),
            provenance=[
                {
                    "kind": "benchmark_manifest",
                    "benchmark": manifest["benchmark"],
                    "schema_version": manifest["schema_version"],
                    "manufacturer_claim": candidate.get("manufacturer_claim"),
                }
            ],
        )
        features.append(feature)
    return {
        "features": features,
        "feature_count": len(features),
        "missing_feature_groups": missing_groups,
        "human_approval": False,
    }


def bootstrap_device_benchmark(
    project_root: Path,
    repository_root: Path,
    *,
    benchmark: str,
    scene_path: Path,
    source_revision: str,
    reference_root: Path | None = None,
    source_artifacts: list[Path] | None = None,
) -> dict[str, Any]:
    if not source_revision.strip():
        raise ValueError("device benchmark bootstrap requires a source revision")
    manifest = load_device_manifest(benchmark, repository_root)
    scene_path = scene_path.expanduser().resolve()
    project = ProjectStore.create(
        project_root,
        manifest["display_name"],
        target_fidelity=FidelityLevel(manifest["target_fidelity"]),
        metadata={
            "benchmark": benchmark,
            "benchmark_schema_version": manifest["schema_version"],
            "source_revision": source_revision.strip(),
            "manufacturer_spec_source": manifest["manufacturer_spec"]["source"],
            "body_envelope": manifest["body_envelope"],
            "required_feature_groups": manifest["required_feature_groups"],
            "required_material_regions": manifest["required_material_regions"],
            "known_blockers": manifest["known_blockers"],
            "camera_hints_by_viewpoint": {
                item["viewpoint_label"]: item["camera_hint"]
                for item in manifest["references"]
                if item.get("camera_hint")
            },
        },
    )
    coordinator = Coordinator(project)
    scene_job = coordinator.run("scene.import", {"source": str(scene_path)})
    if scene_job["status"] != "succeeded":
        raise RuntimeError(f"{benchmark} scene import failed: {scene_job['error']}")
    scene = SceneStore(project).get(scene_job["result"]["id"])

    artifact_store = ArtifactStore(project)
    governed_sources = []
    for path in source_artifacts or []:
        source = path.expanduser().resolve()
        governed_sources.append(
            {"source": str(source), "artifact": artifact_store.ingest_file(source).to_dict()}
        )

    reference_by_label: dict[str, dict[str, Any]] = {}
    missing_references = []
    if reference_root is not None:
        root = reference_root.expanduser().resolve()
        ingestor = ReferenceIngestor(project)
        for specification in manifest["references"]:
            path = root / specification["path"]
            if not path.is_file():
                missing_references.append(specification["path"])
                continue
            reference_by_label[specification["viewpoint_label"]] = ingestor.import_file(
                path,
                rights_state=specification["rights_state"],
                viewpoint_label=specification["viewpoint_label"],
            )
    else:
        missing_references = [item["path"] for item in manifest["references"]]

    measurement_store = MeasurementStore(project)
    measurement_ids = []
    for axis in AXES:
        measurement = measurement_store.add(
            "known_overall_dimension",
            {
                "axis": axis,
                "millimetres": manifest["manufacturer_spec"]["dimensions_mm"][axis],
                "source": manifest["manufacturer_spec"]["source"],
                "retrieved_date": manifest["manufacturer_spec"]["retrieved_date"],
                "scope": manifest["body_envelope"]["scope"],
                "object_patterns": manifest["body_envelope"]["object_patterns"],
            },
            evidence_class=EvidenceClass.MANUFACTURER_SPEC,
            certainty=MeasurementCertainty.BOUNDED,
            uncertainty=manifest["manufacturer_spec"]["uncertainty"],
            reference_ids=[item["id"] for item in reference_by_label.values()],
        )
        measurement_ids.append(measurement["id"])

    audit_job = coordinator.run("project.audit", {"scene_id": scene["id"]})
    if audit_job["status"] != "succeeded":
        raise RuntimeError(f"{benchmark} audit failed: {audit_job['error']}")
    audit = audit_job["result"]["audit"]
    inventory = audit["inventory"]
    dimensions = evaluate_device_dimensions(manifest, inventory)
    features = import_device_feature_candidates(
        project,
        manifest,
        inventory,
        scene_id=scene["id"],
        scene_digest=scene["artifact_digest"],
        reference_by_label=reference_by_label,
    )
    bootstrap = {
        "schema_version": 1,
        "benchmark": benchmark,
        "project": str(project.root),
        "manifest": manifest,
        "source_revision": source_revision.strip(),
        "governed_sources": governed_sources,
        "scene_id": scene["id"],
        "scene_artifact_digest": scene["artifact_digest"],
        "scene_job_id": scene_job["id"],
        "audit_job_id": audit_job["id"],
        "audit_valid": audit["valid"],
        "audit_summary": audit["summary"],
        "reference_ids": {
            label: reference["id"] for label, reference in reference_by_label.items()
        },
        "missing_references": missing_references,
        "measurement_ids": measurement_ids,
        "dimensional_evaluation": dimensions,
        "feature_candidates": features,
        "accepted": False,
        "reason": (
            "Bootstrap establishes provenance and measured candidate state; camera, feature, "
            "material, repair, and named human review gates remain independent."
        ),
    }
    atomic_write_json(project.root / "benchmark-bootstrap.json", bootstrap)
    return bootstrap
