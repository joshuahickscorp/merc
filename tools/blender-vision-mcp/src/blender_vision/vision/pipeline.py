from __future__ import annotations

import json
import math
import shutil
from typing import Any

from PIL import Image

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.backends.registry import BackendRegistry
from blender_vision.cameras.solver import CameraSolver
from blender_vision.comparison.images import silhouette_mask
from blender_vision.core.errors import BlenderVisionError
from blender_vision.core.models import EvidenceClass
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore
from blender_vision.vision.base import GeometryEvidence
from blender_vision.vision.store import ARTIFACT_FIELDS, GeometryEvidenceStore

REGISTRATION_RANK = {
    "body_bounding_box_alignment": 0,
    "approximate_visual_registration": 1,
    "feature_based_camera_solution": 2,
    "metric_camera_solution": 3,
}


class GeometryPipeline:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.store = GeometryEvidenceStore(project)

    def run(
        self, backend: str = "auto", configuration: dict[str, Any] | None = None
    ) -> dict[str, Any]:
        configuration = dict(configuration or {})
        if backend == "auto":
            references = self._references()
            if shutil.which("colmap") and len(references) >= 2:
                try:
                    return self._run_colmap(configuration)
                except Exception as error:
                    configuration["colmap_fallback_reason"] = f"{type(error).__name__}: {error}"
            return self._run_silhouettes(configuration)
        if backend in {"silhouette", "turntable_fallback", "heuristic-pinhole"}:
            return self._run_silhouettes(configuration)
        if backend == "colmap":
            return self._run_colmap(configuration)
        if backend == "visual_hull":
            from blender_vision.vision.visual_hull import VisualHullReconstructor

            return VisualHullReconstructor(self.project).run(configuration)
        if backend in {"vggt", "vggt-commercial", "vggt-original-research"}:
            from blender_vision.vision.vggt import VGGTAdapter

            selected = "vggt-commercial" if backend == "vggt" else backend
            return VGGTAdapter(self.project).run(selected, configuration)
        raise ValueError(f"unknown geometry backend: {backend}")

    def import_external(
        self,
        *,
        backend: str,
        backend_version: str,
        evidence: dict[str, Any],
        evidence_class: str,
        license_record: dict[str, Any],
        configuration: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        allowed = set(GeometryEvidence.__dataclass_fields__)
        unknown = sorted(set(evidence) - allowed)
        if unknown:
            raise ValueError("unknown geometry evidence fields: " + ", ".join(unknown))
        for field in ARTIFACT_FIELDS:
            values = evidence.get(field, [])
            if not isinstance(values, list) or any(not isinstance(item, str) for item in values):
                raise ValueError(f"{field} must be a list of artifact SHA-256 digests")
        research_only = bool(license_record.get("research_only", False))
        commercial_use = license_record.get("commercial_use") is True
        if not license_record.get("license"):
            raise ValueError("external geometry evidence requires a license identifier")
        if license_record.get("checkpoint_required") and not license_record.get("weight_hash"):
            raise ValueError("checkpoint-backed geometry evidence requires a weight hash")
        geometry = GeometryEvidence(**evidence)
        self._validate_frame(geometry)
        return self.store.create(
            backend,
            backend_version,
            geometry,
            evidence_class=EvidenceClass(evidence_class),
            configuration=configuration,
            license_record=license_record,
            commercial_eligible=commercial_use and not research_only,
        )

    def compare(self, run_ids: list[str] | None = None) -> dict[str, Any]:
        runs = self.store.list()
        if run_ids:
            selected = set(run_ids)
            runs = [run for run in runs if run["id"] in selected]
        if len(runs) < 2:
            raise ValueError("backend comparison requires at least two geometry runs")
        pairwise = []
        for left_index, left in enumerate(runs):
            for right in runs[left_index + 1 :]:
                pairwise.append(self._compare_pair(left, right))
        ranked = sorted(runs, key=self._authority_score, reverse=True)
        authority = next((run for run in ranked if run["commercial_eligible"]), None)
        excluded = [
            {
                "run_id": run["id"],
                "reason": "backend evidence is research-only or lacks commercial-use clearance",
            }
            for run in runs
            if not run["commercial_eligible"]
        ]
        report = {
            "policy": "select authority by license, camera class, coverage, and evidence outputs",
            "averaging_performed": False,
            "selected_authority_run_id": authority["id"] if authority else None,
            "selected_backend": authority["backend"] if authority else None,
            "authority_score": self._authority_score(authority) if authority else None,
            "pairwise": pairwise,
            "commercial_release_exclusions": excluded,
            "recommendation": (
                "Use the selected run as initialization/authority according to its evidence class; "
                "run classical bundle adjustment before promoting camera scale."
                if authority
                else "No run is eligible for commercial authority; complete license review."
            ),
        }
        return self.store.add_consensus([run["id"] for run in runs], report)

    def _references(self) -> list[dict[str, Any]]:
        references = [
            item
            for item in ReferenceIngestor(self.project).list()
            if item["media_type"].startswith("image/")
            and item["quality"].get("decode_ok")
            and item.get("acceptance_eligible", True)
        ]
        if not references:
            raise BlenderVisionError("no decodable image references are available")
        return references

    def _latest_camera_document(self) -> dict[str, Any] | None:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT solution_json FROM camera_solutions ORDER BY created_at DESC LIMIT 1"
            ).fetchone()
        return json.loads(row[0]) if row else None

    @staticmethod
    def _camera_evidence(
        document: dict[str, Any] | None,
    ) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
        cameras = document.get("cameras", []) if document else []
        intrinsics = [
            {
                "reference_id": item["reference_id"],
                "model": item["model"],
                "width": item["width"],
                "height": item["height"],
                "intrinsics": item["intrinsics"],
            }
            for item in cameras
        ]
        extrinsics = [
            {
                "reference_id": item["reference_id"],
                "world_from_camera": item["world_from_camera"],
                "registration_class": item["registration_class"],
                "confidence": item["confidence"],
            }
            for item in cameras
        ]
        return intrinsics, extrinsics

    def _run_silhouettes(self, configuration: dict[str, Any]) -> dict[str, Any]:
        references = self._references()
        camera_document = self._latest_camera_document()
        if camera_document is None and configuration.get("solve_cameras", True):
            camera_document = CameraSolver(self.project).solve("auto")
        mask_digests = []
        mask_metrics = []
        for reference in references:
            source = self.project.root / reference["relative_path"]
            output = self.project.root / "geometry" / "masks" / f"{reference['id']}.png"
            output.parent.mkdir(parents=True, exist_ok=True)
            with Image.open(source) as image:
                mask = silhouette_mask(image)
                mask.save(output)
                occupied = sum(mask.tobytes()) / (255.0 * mask.width * mask.height)
            artifact = self.artifacts.ingest_file(output, media_type="image/png")
            mask_digests.append(artifact.digest)
            mask_metrics.append(
                {
                    "reference_id": reference["id"],
                    "artifact_digest": artifact.digest,
                    "occupied_fraction": round(occupied, 8),
                    "size": [mask.width, mask.height],
                }
            )
        intrinsics, extrinsics = self._camera_evidence(camera_document)
        registration_classes = sorted(
            {item["registration_class"] for item in extrinsics if item.get("registration_class")}
        )
        metric_scale = registration_classes == ["metric_camera_solution"]
        evidence = GeometryEvidence(
            camera_intrinsics=intrinsics,
            camera_extrinsics=extrinsics,
            mask_artifacts=mask_digests,
            diagnostics={
                "method": "alpha_or_corner_background_silhouette",
                "masks": mask_metrics,
                "camera_solution_id": camera_document.get("id") if camera_document else None,
                "registration_classes": registration_classes,
                "colmap_fallback_reason": configuration.get("colmap_fallback_reason"),
            },
            source_frame="reference_pixels_and_stored_camera_frames",
            transform_to_canonical=(
                [
                    [1.0, 0.0, 0.0, 0.0],
                    [0.0, 1.0, 0.0, 0.0],
                    [0.0, 0.0, 1.0, 0.0],
                    [0.0, 0.0, 0.0, 1.0],
                ]
                if extrinsics
                else None
            ),
            scale_factor=1.0 if metric_scale else None,
            uncertainty={
                "geometry": "silhouette-only",
                "camera": registration_classes or ["unavailable"],
                "metric_authority": False,
            },
        )
        return self.store.create(
            "silhouette",
            "1",
            evidence,
            evidence_class=(
                EvidenceClass.MULTI_VIEW_OBSERVED
                if len(references) > 1
                else EvidenceClass.SINGLE_VIEW_OBSERVED
            ),
            configuration=configuration,
            license_record={"license": "Apache-2.0", "commercial_use": True},
            commercial_eligible=True,
        )

    def _run_colmap(self, configuration: dict[str, Any]) -> dict[str, Any]:
        solution = CameraSolver(self.project).solve("colmap")
        return self.import_colmap_solution(solution["id"], configuration=configuration)

    def import_colmap_solution(
        self, solution_id: str, *, configuration: dict[str, Any] | None = None
    ) -> dict[str, Any]:
        """Register geometry evidence from an existing governed COLMAP solution."""
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT backend,solution_json FROM camera_solutions WHERE id=?", (solution_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown camera solution: {solution_id}")
        solution = json.loads(row["solution_json"])
        if row["backend"] != "colmap":
            raise ValueError("classical photogrammetry lane requires a COLMAP source solution")
        intrinsics, extrinsics = self._camera_evidence(solution)
        workspace = self.project.root / solution["diagnostics"]["workspace"]
        point_digests = []
        correspondence_digests = []
        points = workspace / "text" / "points3D.txt"
        database = workspace / "database.db"
        if points.is_file():
            point_digests.append(self.artifacts.ingest_file(points, media_type="text/plain").digest)
        if database.is_file():
            correspondence_digests.append(
                self.artifacts.ingest_file(database, media_type="application/x-sqlite3").digest
            )
        evidence = GeometryEvidence(
            camera_intrinsics=intrinsics,
            camera_extrinsics=extrinsics,
            point_artifacts=point_digests,
            correspondence_artifacts=correspondence_digests,
            diagnostics={
                **solution["diagnostics"],
                "camera_solution_id": solution["id"],
                "bundle_adjustment": "COLMAP incremental mapper bundle adjustment",
            },
            source_frame="colmap_world",
            transform_to_canonical=None,
            scale_factor=None,
            uncertainty={
                "scale": "unresolved_without_authoritative_alignment",
                "camera": "feature_based",
                "metric_authority": False,
            },
        )
        capability = next(
            item for item in BackendRegistry().capabilities() if item.name == "colmap"
        )
        return self.store.create(
            "colmap",
            capability.version,
            evidence,
            evidence_class=EvidenceClass.MULTI_VIEW_OBSERVED,
            configuration=configuration,
            license_record={"license": capability.license, "commercial_use": True},
            commercial_eligible=True,
        )

    @staticmethod
    def _validate_frame(evidence: GeometryEvidence) -> None:
        matrix = evidence.transform_to_canonical
        if matrix is not None and (len(matrix) != 4 or any(len(row) != 4 for row in matrix)):
            raise ValueError("transform_to_canonical must be a 4x4 matrix")
        if evidence.scale_factor is not None and (
            not math.isfinite(evidence.scale_factor) or evidence.scale_factor <= 0
        ):
            raise ValueError("geometry scale_factor must be finite and positive")

    @staticmethod
    def _authority_score(run: dict[str, Any]) -> tuple[int, int, int, int]:
        evidence = run["evidence"]
        classes = [
            REGISTRATION_RANK.get(item.get("registration_class", ""), -1)
            for item in evidence.get("camera_extrinsics", [])
        ]
        modality_count = sum(bool(evidence.get(field)) for field in ARTIFACT_FIELDS)
        coverage = len({item.get("reference_id") for item in evidence.get("camera_extrinsics", [])})
        return (
            int(run["commercial_eligible"]),
            max(classes, default=-1),
            coverage,
            modality_count,
        )

    @staticmethod
    def _compare_pair(left: dict[str, Any], right: dict[str, Any]) -> dict[str, Any]:
        left_cameras = {
            item["reference_id"]: item for item in left["evidence"].get("camera_extrinsics", [])
        }
        right_cameras = {
            item["reference_id"]: item for item in right["evidence"].get("camera_extrinsics", [])
        }
        shared = sorted(set(left_cameras) & set(right_cameras))
        scale_left = left["evidence"].get("scale_factor")
        scale_right = right["evidence"].get("scale_factor")
        scale_compatible = (
            isinstance(scale_left, (int, float))
            and isinstance(scale_right, (int, float))
            and abs(float(scale_left) - float(scale_right))
            <= 0.05 * max(float(scale_left), float(scale_right))
        )
        center_deltas = []
        if scale_compatible:
            for reference_id in shared:
                left_matrix = left_cameras[reference_id]["world_from_camera"]
                right_matrix = right_cameras[reference_id]["world_from_camera"]
                delta = math.sqrt(
                    sum(
                        (float(left_matrix[axis][3]) - float(right_matrix[axis][3])) ** 2
                        for axis in range(3)
                    )
                )
                center_deltas.append(delta)
        return {
            "left_run_id": left["id"],
            "right_run_id": right["id"],
            "shared_reference_ids": shared,
            "scale_compatible": scale_compatible,
            "camera_center_rmse": (
                math.sqrt(sum(value * value for value in center_deltas) / len(center_deltas))
                if center_deltas
                else None
            ),
            "decision": (
                "comparable_without_averaging"
                if scale_compatible and shared
                else "retain_separate_hypotheses"
            ),
        }
