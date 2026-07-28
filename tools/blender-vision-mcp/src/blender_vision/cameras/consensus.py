from __future__ import annotations

import json
import math
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.models import RegistrationClass
from blender_vision.core.util import atomic_write_json, utc_now
from blender_vision.projects.store import ProjectStore

REGISTRATION_RANK = {
    RegistrationClass.BODY_BOUNDING_BOX.value: 0,
    RegistrationClass.APPROXIMATE_VISUAL.value: 1,
    RegistrationClass.FEATURE_BASED.value: 2,
    RegistrationClass.METRIC.value: 3,
}


class CameraConsensus:
    """Compare complete camera hypotheses without averaging incompatible coordinate systems."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def compare(self, solution_ids: list[str] | None = None) -> dict[str, Any]:
        solutions = self._solutions(solution_ids)
        if len(solutions) < 2:
            raise ValueError("camera consensus requires at least two camera solutions")
        reference_ids = self._reference_ids()
        ranked = sorted(solutions, key=lambda item: self._score(item, reference_ids), reverse=True)
        authority = ranked[0]
        pairwise = [
            self._compare_pair(left, right)
            for left_index, left in enumerate(solutions)
            for right in solutions[left_index + 1 :]
        ]
        authority_classes = {
            camera.get("registration_class") for camera in authority["document"]["cameras"]
        }
        authority_is_metric = authority_classes == {RegistrationClass.METRIC.value}
        report = {
            "schema_version": 1,
            "policy": (
                "rank complete hypotheses by approval, registration authority, reference coverage, "
                "feature support, and reprojection error"
            ),
            "averaging_performed": False,
            "selected_solution_id": authority["id"],
            "selected_backend": authority["backend"],
            "selected_score": list(self._score(authority, reference_ids)),
            "selected_is_approved_metric": bool(authority["approved"] and authority_is_metric),
            "pairwise": pairwise,
            "bundle_adjustment": {
                "required_before_metric_promotion": not authority_is_metric,
                "recommended_backend": "COLMAP or an imported calibrated optimizer result",
            },
            "warning": (
                None
                if authority["approved"] and authority_is_metric
                else "selected hypothesis remains initialization evidence, not metric authority"
            ),
        }
        return self._store([item["id"] for item in solutions], report)

    def latest(self) -> dict[str, Any] | None:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM camera_consensus ORDER BY created_at DESC LIMIT 1"
            ).fetchone()
        if row is None:
            return None
        return {
            "id": row["id"],
            "solution_ids": json.loads(row["solution_ids_json"]),
            "report": json.loads(row["report_json"]),
            "artifact_digest": row["record_digest"],
            "created_at": row["created_at"],
        }

    def _solutions(self, solution_ids: list[str] | None) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT id,backend,solution_json,approved,created_at "
                "FROM camera_solutions ORDER BY created_at"
            ).fetchall()
        requested = set(solution_ids or [])
        solutions = [
            {
                "id": row["id"],
                "backend": row["backend"],
                "document": json.loads(row["solution_json"]),
                "approved": bool(row["approved"]),
                "created_at": row["created_at"],
            }
            for row in rows
            if not requested or row["id"] in requested
        ]
        if requested - {item["id"] for item in solutions}:
            raise ValueError("camera consensus references unknown solutions")
        return solutions

    def _reference_ids(self) -> set[str]:
        with self.project.connection() as connection:
            return {row[0] for row in connection.execute("SELECT id FROM reference_items")}

    @staticmethod
    def _score(
        solution: dict[str, Any], reference_ids: set[str]
    ) -> tuple[int, int, int, int, float]:
        cameras = solution["document"].get("cameras", [])
        classes = [
            REGISTRATION_RANK.get(camera.get("registration_class"), -1) for camera in cameras
        ]
        covered = {camera.get("reference_id") for camera in cameras}
        feature_count = sum(
            int(camera.get("diagnostics", {}).get("quality", {}).get("registered_feature_count", 0))
            for camera in cameras
        )
        rmses = [
            float(camera["diagnostics"]["quality"]["reprojection_rmse_px"])
            for camera in cameras
            if isinstance(
                camera.get("diagnostics", {}).get("quality", {}).get("reprojection_rmse_px"),
                (int, float),
            )
        ]
        mean_rmse = sum(rmses) / len(rmses) if rmses else math.inf
        return (
            int(solution["approved"]),
            min(classes, default=-1),
            int(covered == reference_ids),
            feature_count,
            -mean_rmse,
        )

    @staticmethod
    def _compare_pair(left: dict[str, Any], right: dict[str, Any]) -> dict[str, Any]:
        left_cameras = {item["reference_id"]: item for item in left["document"].get("cameras", [])}
        right_cameras = {
            item["reference_id"]: item for item in right["document"].get("cameras", [])
        }
        shared = sorted(set(left_cameras) & set(right_cameras))
        left_metric = all(
            item.get("registration_class") == RegistrationClass.METRIC.value
            for item in left_cameras.values()
        )
        right_metric = all(
            item.get("registration_class") == RegistrationClass.METRIC.value
            for item in right_cameras.values()
        )
        metric_compatible = bool(shared and left_metric and right_metric)
        deltas = []
        if metric_compatible:
            for reference_id in shared:
                left_matrix = left_cameras[reference_id]["world_from_camera"]
                right_matrix = right_cameras[reference_id]["world_from_camera"]
                deltas.append(
                    math.sqrt(
                        sum(
                            (float(left_matrix[axis][3]) - float(right_matrix[axis][3])) ** 2
                            for axis in range(3)
                        )
                    )
                )
        return {
            "left_solution_id": left["id"],
            "right_solution_id": right["id"],
            "shared_reference_ids": shared,
            "metric_frame_compatible": metric_compatible,
            "camera_center_rmse_mm": (
                math.sqrt(sum(value * value for value in deltas) / len(deltas)) if deltas else None
            ),
            "decision": (
                "comparable_without_averaging"
                if metric_compatible
                else "retain_separate_hypotheses"
            ),
        }

    def _store(self, solution_ids: list[str], report: dict[str, Any]) -> dict[str, Any]:
        consensus_id = str(uuid.uuid4())
        created_at = utc_now()
        record = {
            "schema_version": 1,
            "id": consensus_id,
            "solution_ids": solution_ids,
            "report": report,
            "created_at": created_at,
        }
        relative = Path("cameras") / f"consensus_{consensus_id}.json"
        atomic_write_json(self.project.root / relative, record)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.camera-consensus+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO camera_consensus("
                "id,solution_ids_json,report_json,record_digest,created_at) VALUES(?,?,?,?,?)",
                (
                    consensus_id,
                    json.dumps(solution_ids),
                    json.dumps(report),
                    artifact.digest,
                    created_at,
                ),
            )
        return {**record, "path": str(relative), "artifact": artifact.to_dict()}
