from __future__ import annotations

import json
import math
import uuid
from pathlib import Path
from typing import Any

from PIL import Image, ImageOps

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.blender.runner import BlenderRunner
from blender_vision.cameras.solver import CameraSolver
from blender_vision.comparison.metrics import (
    _mask_contacts_adjacent_borders,
    _reference_mask,
    _render_mask,
)
from blender_vision.core.util import atomic_write_json, sha256_file, utc_now
from blender_vision.evidence.masks import ReferenceMaskStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore
from blender_vision.security.paths import confined_path


def _clamp(value: float, lower: float, upper: float) -> float:
    return max(lower, min(upper, value))


def _view_angles(direction: list[float]) -> tuple[float, float]:
    length = math.sqrt(sum(float(value) ** 2 for value in direction))
    if length <= 1e-12:
        raise ValueError("camera view direction is degenerate")
    x, y, z = (float(value) / length for value in direction)
    return math.degrees(math.atan2(x, y)), math.degrees(math.asin(_clamp(-z, -1.0, 1.0)))


def _view_direction(yaw_degrees: float, elevation_degrees: float) -> list[float]:
    yaw = math.radians(yaw_degrees)
    elevation = math.radians(elevation_degrees)
    return [
        math.sin(yaw) * math.cos(elevation),
        math.cos(yaw) * math.cos(elevation),
        -math.sin(elevation),
    ]


def _candidate(
    yaw: float,
    elevation: float,
    horizontal_fov: float,
    fit_margin: float,
    lens_shift_x: float,
    lens_shift_y: float,
    camera_roll_degrees: float = 0.0,
) -> dict[str, Any]:
    return {
        "view_direction": _view_direction(yaw, elevation),
        "horizontal_fov_degrees": round(_clamp(horizontal_fov, 5.0, 150.0), 8),
        "fit_margin": round(_clamp(fit_margin, 0.25, 8.0), 8),
        "lens_shift_x": round(_clamp(lens_shift_x, -1.0, 1.0), 8),
        "lens_shift_y": round(_clamp(lens_shift_y, -1.0, 1.0), 8),
        "camera_roll_degrees": round(
            _clamp(camera_roll_degrees, -360.0, 360.0), 8
        ),
    }


def _phase_candidates(phase: int, best: dict[str, Any]) -> list[dict[str, Any]]:
    yaw, elevation = _view_angles(best["view_direction"])
    fov = float(best["horizontal_fov_degrees"])
    margin = float(best["fit_margin"])
    shift_x = float(best["lens_shift_x"])
    shift_y = float(best["lens_shift_y"])
    roll = float(best.get("camera_roll_degrees", 0.0))
    values: list[dict[str, Any]] = []
    if phase == 1:
        # The bootstrap framing margin is deliberately generic, so the first
        # phase must be able to correct a substantial subject-scale mismatch.
        # Multipliers keep the sweep proportional for both tightly and loosely
        # framed sources while retaining the same bounded 125 candidates.
        for margin_scale in (0.6, 0.8, 1.0, 1.25, 1.5):
            for shift_x_offset in (-0.04, -0.02, 0.0, 0.02, 0.04):
                for shift_y_offset in (-0.04, -0.02, 0.0, 0.02, 0.04):
                    values.append(
                        _candidate(
                            yaw,
                            elevation,
                            fov,
                            margin * margin_scale,
                            shift_x + shift_x_offset,
                            shift_y + shift_y_offset,
                            roll,
                        )
                    )
    elif phase == 2:
        for yaw_offset in (-10.0, -5.0, 0.0, 5.0, 10.0):
            for elevation_offset in (-10.0, -5.0, 0.0, 5.0, 10.0):
                for margin_offset in (-0.1, -0.05, 0.0, 0.05, 0.1):
                    values.append(
                        _candidate(
                            yaw + yaw_offset,
                            _clamp(elevation + elevation_offset, -85.0, 85.0),
                            fov,
                            margin + margin_offset,
                            shift_x,
                            shift_y,
                            roll,
                        )
                    )
    elif phase == 3:
        for yaw_offset in (-2.0, -1.0, 0.0, 1.0, 2.0):
            for elevation_offset in (-2.0, -1.0, 0.0, 1.0, 2.0):
                for margin_offset in (-0.02, -0.01, 0.0, 0.01, 0.02):
                    values.append(
                        _candidate(
                            yaw + yaw_offset,
                            _clamp(elevation + elevation_offset, -85.0, 85.0),
                            fov,
                            margin + margin_offset,
                            shift_x,
                            shift_y,
                            roll,
                        )
                    )
    elif phase == 4:
        # Pose changes alter the projected principal point.  An optional final
        # phase therefore polishes framing after pose has converged, without
        # reopening the wider orientation search.
        for margin_offset in (-0.02, -0.01, 0.0, 0.01, 0.02):
            for shift_x_offset in (-0.02, -0.01, 0.0, 0.01, 0.02):
                for shift_y_offset in (-0.02, -0.01, 0.0, 0.01, 0.02):
                    values.append(
                        _candidate(
                            yaw,
                            elevation,
                            fov,
                            margin + margin_offset,
                            shift_x + shift_x_offset,
                            shift_y + shift_y_offset,
                            roll,
                        )
                    )
    else:
        raise ValueError("camera refinement phase must be between one and four")
    unique: dict[bytes, dict[str, Any]] = {}
    for value in values:
        key = json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
        unique.setdefault(key, value)
    return list(unique.values())


def _mask_metrics(reference_mask: Image.Image, render_path: Path) -> dict[str, Any]:
    with Image.open(render_path) as render_image:
        render_mask = _render_mask(render_image).resize(
            reference_mask.size, Image.Resampling.NEAREST
        )
    reference_values = reference_mask.tobytes()
    render_values = render_mask.tobytes()
    true_positive = false_positive = false_negative = 0
    for observed, predicted in zip(reference_values, render_values, strict=True):
        reference_on = observed >= 128
        render_on = predicted >= 128
        true_positive += reference_on and render_on
        false_positive += not reference_on and render_on
        false_negative += reference_on and not render_on
    union = true_positive + false_positive + false_negative
    iou = true_positive / union if union else 1.0
    dice = 2 * true_positive / max(
        1, 2 * true_positive + false_positive + false_negative
    )
    return {
        "silhouette_iou": round(iou, 8),
        "silhouette_dice": round(dice, 8),
        "true_positive": true_positive,
        "false_positive": false_positive,
        "false_negative": false_negative,
    }


class CameraRefiner:
    """Bounded silhouette camera correction that never promotes metric authority."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def refine(
        self,
        *,
        source_solution_id: str | None = None,
        reference_id: str | None = None,
        scene_id: str | None = None,
        maximum_dimension: int = 256,
        stages: int = 3,
        evidence_binding_ids: list[str] | None = None,
        job_id: str | None = None,
    ) -> dict[str, Any]:
        if not 64 <= maximum_dimension <= 1024:
            raise ValueError("camera refinement dimension must be between 64 and 1024")
        if not 1 <= stages <= 4:
            raise ValueError("camera refinement stages must be between one and four")
        source = self._source_solution(source_solution_id)
        cameras = source["document"].get("cameras", [])
        if reference_id is not None:
            cameras = [camera for camera in cameras if camera["reference_id"] == reference_id]
        if len(cameras) != 1:
            raise ValueError(
                "camera refinement requires exactly one camera; specify a reference_id"
            )
        camera = cameras[0]
        reference_id = camera["reference_id"]
        reference = next(
            (
                item
                for item in ReferenceIngestor(self.project).list()
                if item["id"] == reference_id
            ),
            None,
        )
        if reference is None:
            raise KeyError(f"camera references unknown evidence: {reference_id}")
        scene = SceneStore(self.project).get(scene_id)
        reference_mask, segmentation = self._reference_mask(reference)
        if _mask_contacts_adjacent_borders(reference_mask):
            raise ValueError(
                "camera refinement requires a complete-object silhouette; reference "
                "foreground contacts adjacent image boundaries and appears cropped"
            )
        width = int(camera["width"])
        height = int(camera["height"])
        scale = min(1.0, maximum_dimension / max(width, height))
        search_width = max(64, round(width * scale))
        search_height = max(64, round(height * scale))
        reference_mask = reference_mask.resize(
            (search_width, search_height), Image.Resampling.NEAREST
        )
        initial = self._initial_candidate(camera)
        run_id = str(uuid.uuid4())
        output_root = self.project.root / "cameras" / "refinement" / run_id
        evaluations: list[dict[str, Any]] = []
        workers: list[dict[str, Any]] = []
        best: dict[str, Any] = {"candidate": initial, "metrics": {"silhouette_iou": -1.0}}
        for phase in range(1, stages + 1):
            candidates = _phase_candidates(phase, best["candidate"])
            result = BlenderRunner(self.project).run(
                "evaluate_camera_candidates",
                Path(scene["absolute_path"]),
                {
                    "output_directory": str(output_root / f"phase-{phase}"),
                    "width": search_width,
                    "height": search_height,
                    "candidates": candidates,
                    "review_exposure": -0.5,
                },
                job_id=f"{job_id or run_id}-phase-{phase}",
                timeout_seconds=1800,
                cancelled=(
                    (lambda: self.project.cancellation_requested(job_id)) if job_id else None
                ),
            )
            workers.append(result["worker"])
            phase_evaluations = self._score_phase(
                phase, result["evaluations"], reference_mask
            )
            evaluations.extend(phase_evaluations)
            best = max(
                phase_evaluations,
                key=lambda item: (item["metrics"]["silhouette_iou"], -item["index"]),
            )
        created_at = utc_now()
        best_render_path = confined_path(
            self.project.root, self.project.root / best["render_path"], must_exist=True
        )
        best_render_artifact = self.artifacts.ingest_file(
            best_render_path, media_type="image/png"
        )
        configuration = {
            "source_solution_id": source["id"],
            "reference_id": reference_id,
            "scene_id": scene["id"],
            "maximum_dimension": maximum_dimension,
            "search_dimensions": [search_width, search_height],
            "stages": stages,
            "candidate_count": len(evaluations),
            "evidence_binding_ids": sorted(set(evidence_binding_ids or [])),
        }
        report = {
            "schema_version": 1,
            "id": run_id,
            "created_at": created_at,
            "authority": (
                "automatic silhouette camera proposal; non-metric and pending named review"
            ),
            "configuration": configuration,
            "reference_segmentation": segmentation,
            "best": best,
            "evaluations": evaluations,
            "worker_batches": workers,
        }
        report_path = output_root / "report.json"
        atomic_write_json(report_path, report)
        report_artifact = self.artifacts.ingest_file(
            report_path, media_type="application/vnd.bvmcp.camera-refinement+json"
        )
        bindings = sorted(
            set(evidence_binding_ids or [])
            | set(camera.get("diagnostics", {}).get("evidence_binding_ids", []))
        )
        result_solution = self._import_result(
            camera,
            best,
            source_solution_id=source["id"],
            width=width,
            height=height,
            run_id=run_id,
            report_digest=report_artifact.digest,
            segmentation=segmentation,
            evidence_binding_ids=bindings,
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO camera_refinement_runs("
                "id,source_solution_id,result_solution_id,reference_id,scene_id,status,"
                "config_json,report_digest,best_render_digest,best_silhouette_iou,"
                "segmentation_method,segmentation_confidence,created_at) "
                "VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    run_id,
                    source["id"],
                    result_solution["id"],
                    reference_id,
                    scene["id"],
                    "completed",
                    json.dumps(configuration),
                    report_artifact.digest,
                    best_render_artifact.digest,
                    best["metrics"]["silhouette_iou"],
                    segmentation["method"],
                    segmentation["confidence"],
                    created_at,
                ),
            )
        return {
            "id": run_id,
            "status": "completed",
            "authority": report["authority"],
            "source_solution_id": source["id"],
            "result_solution": result_solution,
            "reference_segmentation": segmentation,
            "best": best,
            "evaluation_count": len(evaluations),
            "report": report_artifact.to_dict(),
            "report_path": str(report_path.relative_to(self.project.root)),
            "best_render": best_render_artifact.to_dict(),
            "workers": workers,
        }

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM camera_refinement_runs ORDER BY created_at,id"
            ).fetchall()
        values = []
        for row in rows:
            value = dict(row)
            value["configuration"] = json.loads(value.pop("config_json"))
            values.append(value)
        return values

    def _source_solution(self, solution_id: str | None) -> dict[str, Any]:
        with self.project.connection() as connection:
            if solution_id:
                row = connection.execute(
                    "SELECT * FROM camera_solutions WHERE id=?", (solution_id,)
                ).fetchone()
            else:
                row = connection.execute(
                    "SELECT * FROM camera_solutions ORDER BY created_at DESC,id DESC LIMIT 1"
                ).fetchone()
        if row is None:
            raise FileNotFoundError("project has no camera solution")
        document = json.loads(row["solution_json"])
        if isinstance(document, list):
            document = {"id": row["id"], "cameras": document}
        return {**dict(row), "document": document}

    def _reference_mask(
        self, reference: dict[str, Any]
    ) -> tuple[Image.Image, dict[str, Any]]:
        reviewed = ReferenceMaskStore(self.project).latest(reference["id"])
        if reviewed:
            path = self.artifacts.path_for(reviewed["artifact_digest"])
            with Image.open(path) as image:
                mask = ImageOps.exif_transpose(image).convert("L").point(
                    lambda value: 255 if value >= 128 else 0
                )
            return mask, {
                "method": "reviewed_manual_mask",
                "confidence": "high",
                "reference_mask_id": reviewed["id"],
                "artifact_digest": reviewed["artifact_digest"],
            }
        with Image.open(self.project.root / reference["relative_path"]) as image:
            mask, method, confidence = _reference_mask(image)
        return mask, {"method": method, "confidence": confidence}

    @staticmethod
    def _initial_candidate(camera: dict[str, Any]) -> dict[str, Any]:
        diagnostics = camera.get("diagnostics", {})
        direction = diagnostics.get("view_direction")
        if direction is None:
            center = [float(camera["world_from_camera"][axis][3]) for axis in range(3)]
            length = math.sqrt(sum(value * value for value in center)) or 1.0
            direction = [-value / length for value in center]
        width = int(camera["width"])
        fx = float(camera["intrinsics"]["fx"])
        horizontal_fov = math.degrees(2.0 * math.atan(width / (2.0 * fx)))
        framing = diagnostics.get("render_framing", {})
        if not isinstance(framing, dict):
            raise ValueError("camera render_framing diagnostics must be an object")
        yaw, elevation = _view_angles([float(value) for value in direction])
        return _candidate(
            yaw,
            elevation,
            horizontal_fov,
            float(framing.get("fit_margin", 1.25)),
            float(framing.get("lens_shift_x", 0.0)),
            float(framing.get("lens_shift_y", 0.0)),
            float(diagnostics.get("camera_roll_degrees", 0.0)),
        )

    def _score_phase(
        self,
        phase: int,
        worker_evaluations: list[dict[str, Any]],
        reference_mask: Image.Image,
    ) -> list[dict[str, Any]]:
        scored = []
        for item in worker_evaluations:
            path = confined_path(
                self.project.root,
                self.project.root / item["render_path"],
                must_exist=True,
            )
            digest, size = sha256_file(path)
            scored.append(
                {
                    "phase": phase,
                    "index": int(item["index"]),
                    "candidate": item["candidate"],
                    "camera": item["camera"],
                    "render_path": str(path.relative_to(self.project.root)),
                    "render_sha256": digest,
                    "render_bytes": size,
                    "metrics": _mask_metrics(reference_mask, path),
                }
            )
        return scored

    def _import_result(
        self,
        source_camera: dict[str, Any],
        best: dict[str, Any],
        *,
        source_solution_id: str,
        width: int,
        height: int,
        run_id: str,
        report_digest: str,
        segmentation: dict[str, Any],
        evidence_binding_ids: list[str],
    ) -> dict[str, Any]:
        candidate = best["candidate"]
        fov = float(candidate["horizontal_fov_degrees"])
        focal_pixels = width / (2.0 * math.tan(math.radians(fov) / 2.0))
        confidence_ceiling = {"high": 0.8, "medium": 0.65, "low": 0.5}.get(
            segmentation["confidence"], 0.4
        )
        confidence = min(confidence_ceiling, best["metrics"]["silhouette_iou"])
        diagnostics = {
            "authority": "automatic silhouette refinement; non-metric and unapproved",
            "source_solution_id": source_solution_id,
            "source_registration_class": source_camera["registration_class"],
            "camera_refinement_run_id": run_id,
            "camera_refinement_report_digest": report_digest,
            "view_direction": candidate["view_direction"],
            "camera_roll_degrees": candidate["camera_roll_degrees"],
            "render_framing": {
                "fit_margin": candidate["fit_margin"],
                "lens_shift_x": candidate["lens_shift_x"],
                "lens_shift_y": candidate["lens_shift_y"],
            },
            "search_silhouette_iou": best["metrics"]["silhouette_iou"],
            "reference_segmentation": segmentation["method"],
            "reference_segmentation_confidence": segmentation["confidence"],
        }
        camera = {
            "reference_id": source_camera["reference_id"],
            "model": "PINHOLE",
            "width": width,
            "height": height,
            "intrinsics": {
                "fx": focal_pixels,
                "fy": focal_pixels,
                "cx": width * (0.5 - float(candidate["lens_shift_x"])),
                "cy": height / 2.0 + width * float(candidate["lens_shift_y"]),
            },
            "world_from_camera": best["camera"]["world_from_camera"],
            "confidence": confidence,
            "registration_class": "approximate_visual_registration",
            "evidence_class": "SINGLE_VIEW_OBSERVED",
            "diagnostics": diagnostics,
        }
        return CameraSolver(self.project).import_manual(
            [camera],
            diagnostics={
                "authority": "automatic silhouette refinement proposal",
                "camera_refinement_run_id": run_id,
                "camera_refinement_report_digest": report_digest,
                "best_search_silhouette_iou": best["metrics"]["silhouette_iou"],
                "reference_segmentation": segmentation,
            },
            evidence_binding_ids=evidence_binding_ids,
        )
