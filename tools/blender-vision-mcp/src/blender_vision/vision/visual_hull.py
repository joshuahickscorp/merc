from __future__ import annotations

import json
import math
import statistics
import uuid
from pathlib import Path
from typing import Any

from PIL import Image

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.cameras.state import validate_complete_camera_state
from blender_vision.core.errors import EvidenceUnavailable
from blender_vision.core.models import EvidenceClass
from blender_vision.core.util import atomic_write_json, atomic_write_text, sha256_file
from blender_vision.projects.store import ProjectStore
from blender_vision.vision.base import GeometryEvidence
from blender_vision.vision.store import GeometryEvidenceStore

RELEASE_ELIGIBLE_RIGHTS = {
    "CC0",
    "CC-BY",
    "LICENSED_REUSABLE",
    "PUBLIC_DOMAIN",
    "SYNTHETIC_OWNED",
    "USER_OWNED",
}
FULL_OBJECT_LABELS = {"complete_object", "full_object", "whole_object"}


class VisualHullReconstructor:
    """Create a conservative voxel visual hull from governed masks and camera snapshots."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.store = GeometryEvidenceStore(project)

    def run(self, configuration: dict[str, Any] | None = None) -> dict[str, Any]:
        configuration = dict(configuration or {})
        resolution = int(configuration.get("grid_resolution", 32))
        if not 12 <= resolution <= 128:
            raise ValueError("visual-hull grid resolution must be between 12 and 128")
        threshold = int(configuration.get("foreground_threshold", 128))
        if not 1 <= threshold <= 255:
            raise ValueError("visual-hull foreground threshold must be between 1 and 255")
        minimum_views = int(configuration.get("minimum_views", 2))
        if not 2 <= minimum_views <= 32:
            raise ValueError("visual-hull minimum views must be between 2 and 32")

        solution = self._camera_solution(configuration.get("camera_solution_id"))
        cameras = self._validated_cameras(solution)
        views = self._governed_views(cameras, threshold)
        if len(views) < minimum_views:
            raise EvidenceUnavailable(
                "visual-hull reconstruction requires at least "
                f"{minimum_views} reviewed full-object masks matched to immutable cameras"
            )
        center = self._viewing_ray_center([view["camera"] for view in views])
        bounds, bounds_source = self._bounds(configuration, views, center)
        occupied = self._carve(bounds, resolution, views)
        total_voxels = resolution**3
        if not occupied:
            raise EvidenceUnavailable(
                "reviewed masks and cameras produce an empty visual-hull intersection"
            )
        if len(occupied) == total_voxels:
            raise EvidenceUnavailable(
                "reviewed masks do not constrain the configured visual-hull volume"
            )
        vertices, faces = self._surface_mesh(occupied, bounds, resolution)
        if not faces:
            raise EvidenceUnavailable("visual-hull occupancy has no extractable surface")

        token = str(uuid.uuid4())
        mesh_relative = Path("geometry") / "visual_hull" / f"visual-hull-{token}.ply"
        mesh_path = self.project.root / mesh_relative
        mesh_path.parent.mkdir(parents=True, exist_ok=True)
        atomic_write_text(mesh_path, self._ply(vertices, faces))
        mesh_artifact = self.artifacts.ingest_file(mesh_path, media_type="model/ply")

        metric_frame = bool(solution["approved"]) and all(
            view["camera"]["registration_class"] == "metric_camera_solution"
            for view in views
        )
        report = {
            "schema_version": 1,
            "kind": "governed_voxel_visual_hull",
            "camera_solution_id": solution["id"],
            "camera_solution_approved": bool(solution["approved"]),
            "metric_frame": metric_frame,
            "bounds": bounds,
            "bounds_source": bounds_source,
            "grid_resolution": resolution,
            "voxel_count": len(occupied),
            "total_voxel_count": total_voxels,
            "occupancy_fraction": len(occupied) / total_voxels,
            "vertex_count": len(vertices),
            "triangle_count": len(faces),
            "mesh_artifact_digest": mesh_artifact.digest,
            "views": [
                {
                    "reference_id": view["camera"]["reference_id"],
                    "camera_immutable_sha256": view["camera"]["immutable_sha256"],
                    "mask_id": view["mask"]["id"],
                    "mask_artifact_digest": view["mask"]["artifact_digest"],
                    "mask_revision": view["mask"]["revision"],
                    "mask_reviewer": view["mask"]["reviewer"],
                    "rights_state": view["rights_state"],
                }
                for view in views
            ],
            "authority": {
                "evidence_class": "MULTI_VIEW_OBSERVED",
                "metric_authority": False,
                "concavity_authority": False,
                "semantic_authority": False,
                "policy": (
                    "silhouette intersection is a coarse topology hypothesis; hidden concavities "
                    "and metric accuracy require independent evidence"
                ),
            },
        }
        report_relative = Path("geometry") / "visual_hull" / f"visual-hull-{token}.json"
        report_path = self.project.root / report_relative
        atomic_write_json(report_path, report)
        report_artifact = self.artifacts.ingest_file(
            report_path, media_type="application/vnd.bvmcp.visual-hull+json"
        )

        rights_states = sorted({view["rights_state"] for view in views})
        commercial_eligible = all(state in RELEASE_ELIGIBLE_RIGHTS for state in rights_states)
        evidence = GeometryEvidence(
            camera_intrinsics=[
                {
                    "reference_id": view["camera"]["reference_id"],
                    "model": view["camera"]["model"],
                    "width": view["camera"]["width"],
                    "height": view["camera"]["height"],
                    "intrinsics": view["camera"]["intrinsics"],
                    "immutable_sha256": view["camera"]["immutable_sha256"],
                }
                for view in views
            ],
            camera_extrinsics=[
                {
                    "reference_id": view["camera"]["reference_id"],
                    "world_from_camera": view["camera"]["world_from_camera"],
                    "registration_class": view["camera"]["registration_class"],
                    "confidence": view["camera"]["confidence"],
                    "immutable_sha256": view["camera"]["immutable_sha256"],
                }
                for view in views
            ],
            mask_artifacts=[view["mask"]["artifact_digest"] for view in views],
            occupancy_artifacts=[report_artifact.digest],
            silhouette_volume_artifacts=[report_artifact.digest],
            visual_hull_artifacts=[mesh_artifact.digest],
            diagnostics={
                "method": "reviewed_mask_voxel_carving",
                "camera_solution_id": solution["id"],
                "camera_solution_approved": bool(solution["approved"]),
                "view_count": len(views),
                "bounds_source": bounds_source,
                "occupancy_fraction": report["occupancy_fraction"],
                "source_rights_states": rights_states,
            },
            source_frame="immutable_camera_solution_world",
            transform_to_canonical=(
                [
                    [1.0, 0.0, 0.0, 0.0],
                    [0.0, 1.0, 0.0, 0.0],
                    [0.0, 0.0, 1.0, 0.0],
                    [0.0, 0.0, 0.0, 1.0],
                ]
                if metric_frame
                else None
            ),
            scale_factor=1.0 if metric_frame else None,
            uncertainty={
                "geometry": "silhouette_visual_hull_excludes_unobserved_concavities",
                "camera": "approved" if solution["approved"] else "unapproved_hypothesis",
                "metric_authority": False,
            },
        )
        return self.store.create(
            "visual_hull",
            "1",
            evidence,
            evidence_class=EvidenceClass.MULTI_VIEW_OBSERVED,
            configuration={
                **configuration,
                "resolved_bounds": bounds,
                "bounds_source": bounds_source,
            },
            license_record={
                "license": "Apache-2.0",
                "commercial_use": commercial_eligible,
                "source_rights_states": rights_states,
            },
            commercial_eligible=commercial_eligible,
        )

    def _camera_solution(self, solution_id: Any) -> dict[str, Any]:
        with self.project.connection() as connection:
            if solution_id:
                row = connection.execute(
                    "SELECT id,solution_json,approved FROM camera_solutions WHERE id=?",
                    (str(solution_id),),
                ).fetchone()
            else:
                row = connection.execute(
                    "SELECT id,solution_json,approved FROM camera_solutions "
                    "ORDER BY created_at DESC,id DESC LIMIT 1"
                ).fetchone()
        if row is None:
            raise EvidenceUnavailable("visual-hull reconstruction requires a camera solution")
        document = json.loads(row["solution_json"])
        return {**document, "id": row["id"], "approved": bool(row["approved"])}

    @staticmethod
    def _validated_cameras(solution: dict[str, Any]) -> list[dict[str, Any]]:
        cameras = solution.get("cameras", [])
        if not cameras:
            raise EvidenceUnavailable("camera solution contains no camera snapshots")
        for camera in cameras:
            validate_complete_camera_state(camera)
            distortion = camera["distortion_model"]
            parameters = distortion.get("parameters", {})
            if any(abs(float(value)) > 1e-12 for value in parameters.values()):
                raise EvidenceUnavailable(
                    "visual-hull carving requires governed undistorted pinhole references"
                )
            crop = camera["crop"]
            if crop != {
                "x": 0,
                "y": 0,
                "width": camera["width"],
                "height": camera["height"],
                "source": "full_frame",
            }:
                raise EvidenceUnavailable(
                    "visual-hull carving requires complete full-frame camera snapshots"
                )
        return cameras

    def _governed_views(
        self, cameras: list[dict[str, Any]], threshold: int
    ) -> list[dict[str, Any]]:
        views: list[dict[str, Any]] = []
        with self.project.connection() as connection:
            references = {
                row["id"]: dict(row)
                for row in connection.execute(
                    "SELECT id,artifact_digest,rights_state FROM reference_items"
                )
            }
            masks = [
                dict(row)
                for row in connection.execute(
                    "SELECT * FROM reference_masks WHERE approval_state='approved' "
                    "ORDER BY revision DESC,created_at DESC,id DESC"
                )
            ]
        latest_masks: dict[str, dict[str, Any]] = {}
        for mask in masks:
            latest_masks.setdefault(mask["reference_id"], mask)
        for camera in cameras:
            reference_id = camera["reference_id"]
            reference = references.get(reference_id)
            mask = latest_masks.get(reference_id)
            if not reference or not mask:
                continue
            if camera["camera_source_identity"].get("artifact_digest") != reference[
                "artifact_digest"
            ]:
                continue
            visible = set(json.loads(mask["visible_components_json"]))
            excluded = set(json.loads(mask["excluded_components_json"]))
            if excluded or (visible and not visible <= FULL_OBJECT_LABELS):
                continue
            if mask["intended_use"] not in {
                "geometry_evaluation",
                "silhouette_evaluation",
                "visual_hull_reconstruction",
            }:
                continue
            try:
                path = self.artifacts.path_for(mask["artifact_digest"])
                digest, _size = sha256_file(path)
                if digest != mask["artifact_digest"]:
                    continue
                with Image.open(path) as source:
                    image = source.convert("L")
                    image.load()
            except (FileNotFoundError, OSError, ValueError):
                continue
            roi = json.loads(mask["roi_json"])
            if roi != {
                "x": 0,
                "y": 0,
                "width": image.width,
                "height": image.height,
            }:
                continue
            binary = image.point(lambda value: 255 if value >= threshold else 0)
            if binary.getbbox() is None:
                continue
            views.append(
                {
                    "camera": camera,
                    "mask": mask,
                    "image": binary,
                    "pixels": binary.load(),
                    "rights_state": reference["rights_state"],
                }
            )
        return views

    @staticmethod
    def _viewing_ray_center(cameras: list[dict[str, Any]]) -> list[float]:
        matrix = [[0.0, 0.0, 0.0] for _ in range(3)]
        vector = [0.0, 0.0, 0.0]
        for camera in cameras:
            transform = camera["world_from_camera"]
            center = [float(transform[axis][3]) for axis in range(3)]
            direction = [-float(transform[axis][2]) for axis in range(3)]
            length = math.sqrt(sum(value * value for value in direction))
            if length <= 1e-12:
                raise EvidenceUnavailable("camera contains a zero-length viewing direction")
            direction = [value / length for value in direction]
            projector = [
                [float(row == column) - direction[row] * direction[column] for column in range(3)]
                for row in range(3)
            ]
            for row in range(3):
                for column in range(3):
                    matrix[row][column] += projector[row][column]
                vector[row] += sum(projector[row][column] * center[column] for column in range(3))
        try:
            return VisualHullReconstructor._solve_three(matrix, vector)
        except ValueError as error:
            raise EvidenceUnavailable(
                "visual-hull cameras lack sufficient directional diversity"
            ) from error

    def _bounds(
        self,
        configuration: dict[str, Any],
        views: list[dict[str, Any]],
        center: list[float],
    ) -> tuple[dict[str, list[float]], str]:
        configured = configuration.get("bounds")
        if configured is None:
            configured = self.project.project().get("metadata", {}).get("visual_hull_bounds")
        if configured is not None:
            return self._validated_bounds(configured), "configured"
        radii = []
        for view in views:
            camera = view["camera"]
            transform = camera["world_from_camera"]
            camera_center = [float(transform[axis][3]) for axis in range(3)]
            forward = [-float(transform[axis][2]) for axis in range(3)]
            depth = sum((center[axis] - camera_center[axis]) * forward[axis] for axis in range(3))
            box = view["image"].getbbox()
            if depth <= 0.0 or box is None:
                continue
            half_pixels = max((box[2] - box[0]) / 2.0, (box[3] - box[1]) / 2.0)
            scale = camera["width"] / view["image"].width
            focal = min(
                float(camera["intrinsics"]["fx"]), float(camera["intrinsics"]["fy"])
            )
            radii.append(depth * half_pixels * scale / focal)
        if not radii or not all(math.isfinite(value) and value > 0.0 for value in radii):
            raise EvidenceUnavailable("visual-hull bounds cannot be inferred from governed masks")
        half_extent = statistics.median(radii) * 1.35
        bounds = {
            "minimum": [value - half_extent for value in center],
            "maximum": [value + half_extent for value in center],
        }
        return self._validated_bounds(bounds), "inferred_from_reviewed_masks_and_camera_rays"

    @staticmethod
    def _validated_bounds(value: Any) -> dict[str, list[float]]:
        if not isinstance(value, dict):
            raise ValueError("visual-hull bounds must contain minimum and maximum vectors")
        try:
            minimum = [float(item) for item in value["minimum"]]
            maximum = [float(item) for item in value["maximum"]]
        except (KeyError, TypeError, ValueError) as error:
            raise ValueError("visual-hull bounds are malformed") from error
        if (
            len(minimum) != 3
            or len(maximum) != 3
            or not all(math.isfinite(item) for item in [*minimum, *maximum])
            or any(minimum[axis] >= maximum[axis] for axis in range(3))
        ):
            raise ValueError("visual-hull bounds require finite increasing 3D coordinates")
        return {"minimum": minimum, "maximum": maximum}

    @staticmethod
    def _carve(
        bounds: dict[str, list[float]],
        resolution: int,
        views: list[dict[str, Any]],
    ) -> set[tuple[int, int, int]]:
        minimum, maximum = bounds["minimum"], bounds["maximum"]
        steps = [(maximum[axis] - minimum[axis]) / resolution for axis in range(3)]
        occupied: set[tuple[int, int, int]] = set()
        for x_index in range(resolution):
            x = minimum[0] + (x_index + 0.5) * steps[0]
            for y_index in range(resolution):
                y = minimum[1] + (y_index + 0.5) * steps[1]
                for z_index in range(resolution):
                    z = minimum[2] + (z_index + 0.5) * steps[2]
                    if all(VisualHullReconstructor._inside_view((x, y, z), view) for view in views):
                        occupied.add((x_index, y_index, z_index))
        return occupied

    @staticmethod
    def _inside_view(point: tuple[float, float, float], view: dict[str, Any]) -> bool:
        camera = view["camera"]
        transform = camera["world_from_camera"]
        offset = [point[axis] - float(transform[axis][3]) for axis in range(3)]
        camera_point = [
            sum(
                float(transform[world_axis][camera_axis]) * offset[world_axis]
                for world_axis in range(3)
            )
            for camera_axis in range(3)
        ]
        depth = -camera_point[2]
        if depth < float(camera["clipping"]["near"]) or depth > float(
            camera["clipping"]["far"]
        ):
            return False
        intrinsics = camera["intrinsics"]
        u = float(intrinsics["fx"]) * camera_point[0] / depth + float(intrinsics["cx"])
        v = float(intrinsics["cy"]) - float(intrinsics["fy"]) * camera_point[1] / depth
        image = view["image"]
        u *= image.width / camera["width"]
        v *= image.height / camera["height"]
        column, row = int(round(u)), int(round(v))
        return bool(
            0 <= column < image.width
            and 0 <= row < image.height
            and view["pixels"][column, row] > 0
        )

    @staticmethod
    def _surface_mesh(
        occupied: set[tuple[int, int, int]],
        bounds: dict[str, list[float]],
        resolution: int,
    ) -> tuple[list[tuple[float, float, float]], list[tuple[int, int, int]]]:
        minimum, maximum = bounds["minimum"], bounds["maximum"]
        steps = [(maximum[axis] - minimum[axis]) / resolution for axis in range(3)]
        vertices: list[tuple[float, float, float]] = []
        vertex_indices: dict[tuple[int, int, int], int] = {}
        faces: list[tuple[int, int, int]] = []
        sides = (
            ((-1, 0, 0), ((0, 0, 0), (0, 0, 1), (0, 1, 1), (0, 1, 0))),
            ((1, 0, 0), ((1, 0, 0), (1, 1, 0), (1, 1, 1), (1, 0, 1))),
            ((0, -1, 0), ((0, 0, 0), (1, 0, 0), (1, 0, 1), (0, 0, 1))),
            ((0, 1, 0), ((0, 1, 0), (0, 1, 1), (1, 1, 1), (1, 1, 0))),
            ((0, 0, -1), ((0, 0, 0), (0, 1, 0), (1, 1, 0), (1, 0, 0))),
            ((0, 0, 1), ((0, 0, 1), (1, 0, 1), (1, 1, 1), (0, 1, 1))),
        )

        def vertex_index(key: tuple[int, int, int]) -> int:
            if key not in vertex_indices:
                vertex_indices[key] = len(vertices)
                vertices.append(
                    tuple(minimum[axis] + key[axis] * steps[axis] for axis in range(3))
                )
            return vertex_indices[key]

        for voxel in sorted(occupied):
            for direction, corners in sides:
                neighbor = tuple(voxel[axis] + direction[axis] for axis in range(3))
                if neighbor in occupied:
                    continue
                indices = [
                    vertex_index(tuple(voxel[axis] + corner[axis] for axis in range(3)))
                    for corner in corners
                ]
                faces.append((indices[0], indices[1], indices[2]))
                faces.append((indices[0], indices[2], indices[3]))
        return vertices, faces

    @staticmethod
    def _ply(
        vertices: list[tuple[float, float, float]], faces: list[tuple[int, int, int]]
    ) -> str:
        lines = [
            "ply",
            "format ascii 1.0",
            "comment VisionMCP governed visual hull",
            f"element vertex {len(vertices)}",
            "property float x",
            "property float y",
            "property float z",
            f"element face {len(faces)}",
            "property list uchar int vertex_indices",
            "end_header",
        ]
        lines.extend(f"{x:.9g} {y:.9g} {z:.9g}" for x, y, z in vertices)
        lines.extend(f"3 {a} {b} {c}" for a, b, c in faces)
        return "\n".join(lines) + "\n"

    @staticmethod
    def _solve_three(matrix: list[list[float]], vector: list[float]) -> list[float]:
        augmented = [matrix[row][:] + [vector[row]] for row in range(3)]
        for column in range(3):
            pivot = max(range(column, 3), key=lambda row: abs(augmented[row][column]))
            if abs(augmented[pivot][column]) <= 1e-10:
                raise ValueError("singular camera-ray system")
            augmented[column], augmented[pivot] = augmented[pivot], augmented[column]
            scale = augmented[column][column]
            augmented[column] = [value / scale for value in augmented[column]]
            for row in range(3):
                if row == column:
                    continue
                factor = augmented[row][column]
                augmented[row] = [
                    augmented[row][item] - factor * augmented[column][item]
                    for item in range(4)
                ]
        return [augmented[row][3] for row in range(3)]
