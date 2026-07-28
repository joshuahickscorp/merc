"""Real COLMAP sparse SfM driver and binary model parser.

Dense MVS (patch_match_stereo) is unavailable when COLMAP is built without CUDA
and is reported as such — never substituted.
"""

from __future__ import annotations

import json
import shutil
import struct
import subprocess
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from blender_vision.core.models import BackendState
from blender_vision.reconstruction.base import (
    BackendAvailability,
    PointCloud,
    ReconstructionInputs,
    TimedRun,
    finalize_candidate,
    unavailable_candidate,
)
from blender_vision.reconstruction.mesh_ops import write_ply_points
from blender_vision.v2.authority import AuthorityClass, derive
from blender_vision.v2.records import ReconstructionCandidate

DENSE_UNAVAILABLE_REASON = (
    "COLMAP built without CUDA; patch_match_stereo unavailable"
)

# COLMAP camera model ids (colmap/src/colmap/sensor/models.h).
_CAMERA_MODEL_IDS: dict[int, tuple[str, int]] = {
    0: ("SIMPLE_PINHOLE", 3),
    1: ("PINHOLE", 4),
    2: ("SIMPLE_RADIAL", 4),
    3: ("RADIAL", 5),
    4: ("OPENCV", 8),
    5: ("OPENCV_FISHEYE", 8),
    6: ("FULL_OPENCV", 12),
    7: ("FOV", 5),
    8: ("SIMPLE_RADIAL_FISHEYE", 4),
    9: ("RADIAL_FISHEYE", 5),
    10: ("THIN_PRISM_FISHEYE", 12),
}


@dataclass(slots=True)
class ColmapCamera:
    camera_id: int
    model: str
    width: int
    height: int
    params: list[float]


@dataclass(slots=True)
class ColmapImage:
    image_id: int
    qvec: np.ndarray
    tvec: np.ndarray
    camera_id: int
    name: str
    points2d: np.ndarray


@dataclass(slots=True)
class ColmapPoint3D:
    point_id: int
    xyz: np.ndarray
    rgb: np.ndarray
    error: float


@dataclass(slots=True)
class ColmapModel:
    cameras: dict[int, ColmapCamera]
    images: dict[int, ColmapImage]
    points3d: dict[int, ColmapPoint3D]


class ColmapSfMBackend:
    """Sparse feature_extractor / exhaustive_matcher / mapper pipeline."""

    name = "colmap_sfm"

    def __init__(self, executable: str | None = None) -> None:
        self.executable = executable or shutil.which("colmap")

    def availability(self) -> BackendAvailability:
        if not self.executable:
            return BackendAvailability(
                state=BackendState.UNAVAILABLE,
                reason="COLMAP executable not found on PATH",
            )
        version = self._version_line()
        return BackendAvailability(
            state=BackendState.AVAILABLE,
            reason=f"COLMAP available: {version}",
            details={
                "executable": self.executable,
                "version": version,
                "dense_mvs": self.dense_availability().to_dict(),
            },
        )

    def dense_availability(self) -> BackendAvailability:
        """Dense MVS requires CUDA; this machine's COLMAP is built without it."""
        if not self.executable:
            return BackendAvailability(
                state=BackendState.UNAVAILABLE,
                reason="COLMAP executable not found on PATH",
            )
        version = self._version_line()
        if "without CUDA" in version or "without cuda" in version.lower():
            return BackendAvailability(
                state=BackendState.UNAVAILABLE,
                reason=DENSE_UNAVAILABLE_REASON,
                details={"version": version},
            )
        # Even if the banner does not say so, refuse to claim dense works without a
        # successful probe — never silently substitute another densifier.
        probe = subprocess.run(
            [self.executable, "patch_match_stereo", "-h"],
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
        banner = (probe.stdout or "") + (probe.stderr or "")
        if "without CUDA" in banner:
            return BackendAvailability(
                state=BackendState.UNAVAILABLE,
                reason=DENSE_UNAVAILABLE_REASON,
                details={"version": version},
            )
        return BackendAvailability(
            state=BackendState.UNAVAILABLE,
            reason=DENSE_UNAVAILABLE_REASON,
            details={
                "version": version,
                "note": "dense MVS not claimed without verified CUDA-capable COLMAP",
            },
        )

    def run(self, inputs: ReconstructionInputs) -> ReconstructionCandidate:
        mode = str(inputs.parameters.get("mode", "sparse")).lower()
        if mode == "dense":
            dense = self.dense_availability()
            return unavailable_candidate(
                backend=self.name,
                reason=dense.reason,
                inputs=inputs,
                authority=AuthorityClass.UNRESOLVED,
            )

        avail = self.availability()
        if not avail.available:
            return unavailable_candidate(
                backend=self.name, reason=avail.reason, inputs=inputs
            )
        if inputs.image_dir is None or not Path(inputs.image_dir).is_dir():
            return unavailable_candidate(
                backend=self.name,
                reason="colmap_sfm requires image_dir with multiview images",
                inputs=inputs,
            )
        images = sorted(
            p
            for p in Path(inputs.image_dir).iterdir()
            if p.suffix.lower() in {".jpg", ".jpeg", ".png", ".tif", ".tiff"}
        )
        if len(images) < 2:
            return unavailable_candidate(
                backend=self.name,
                reason="colmap_sfm requires at least two images",
                inputs=inputs,
            )

        work = inputs.ensure_work_dir() / "colmap"
        work.mkdir(parents=True, exist_ok=True)
        image_path = Path(inputs.image_dir)
        database = work / "database.db"
        sparse = work / "sparse"
        sparse.mkdir(exist_ok=True)
        text_model = work / "text"
        logs: list[dict[str, Any]] = []

        with TimedRun() as timer:
            assert self.executable is not None
            feature_gpu, matcher_gpu = self._gpu_flags()
            commands = [
                [
                    self.executable,
                    "feature_extractor",
                    "--database_path",
                    str(database),
                    "--image_path",
                    str(image_path),
                    "--ImageReader.single_camera",
                    "1",
                    feature_gpu,
                    "0",
                ],
                [
                    self.executable,
                    "exhaustive_matcher",
                    "--database_path",
                    str(database),
                    matcher_gpu,
                    "0",
                ],
                [
                    self.executable,
                    "mapper",
                    "--database_path",
                    str(database),
                    "--image_path",
                    str(image_path),
                    "--output_path",
                    str(sparse),
                ],
            ]
            for command in commands:
                result = subprocess.run(
                    command, capture_output=True, text=True, timeout=1800, check=False
                )
                logs.append(
                    {
                        "command": " ".join(command[:2]),
                        "returncode": result.returncode,
                        "stdout_tail": (result.stdout or "")[-4000:],
                        "stderr_tail": (result.stderr or "")[-4000:],
                    }
                )
                if result.returncode != 0:
                    log_path = work / "commands.json"
                    log_path.write_text(json.dumps(logs, indent=2), encoding="utf-8")
                    return unavailable_candidate(
                        backend=self.name,
                        reason=(
                            f"COLMAP {command[1]} failed with exit {result.returncode}"
                        ),
                        inputs=inputs,
                    )

            models = sorted(p for p in sparse.iterdir() if p.is_dir())
            # Some COLMAP builds write model files directly into sparse/.
            if not models and (
                (sparse / "cameras.bin").is_file() or (sparse / "cameras.txt").is_file()
            ):
                models = [sparse]
            if not models:
                return unavailable_candidate(
                    backend=self.name,
                    reason="COLMAP mapper produced no sparse model",
                    inputs=inputs,
                )
            model_dir = models[0]
            model = read_colmap_model(model_dir)
            # Prefer binary parse; also emit text for inspection.
            text_model.mkdir(exist_ok=True)
            subprocess.run(
                [
                    self.executable,
                    "model_converter",
                    "--input_path",
                    str(model_dir),
                    "--output_path",
                    str(text_model),
                    "--output_type",
                    "TXT",
                ],
                capture_output=True,
                text=True,
                timeout=120,
                check=False,
            )
            mean_error = _mean_reprojection_error(model)
            cloud = _points_cloud(model)
            ply_path = write_ply_points(work / "sparse_points.ply", cloud)
            summary = {
                "registered_images": len(model.images),
                "input_images": len(images),
                "num_cameras": len(model.cameras),
                "num_points3d": len(model.points3d),
                "mean_reprojection_error": mean_error,
                "model_dir": str(model_dir),
                "dense_mvs": self.dense_availability().reason,
                "commands": logs,
            }
            summary_path = work / "summary.json"
            summary_path.write_text(json.dumps(summary, indent=2), encoding="utf-8")
            cameras_path = work / "cameras.json"
            cameras_path.write_text(
                json.dumps(_cameras_export(model), indent=2), encoding="utf-8"
            )

        scale_authority = (
            AuthorityClass.MEASURED
            if inputs.metric_anchor_m is not None
            else AuthorityClass.UNRESOLVED
        )
        authority = derive(
            inputs.input_authorities or [AuthorityClass.SENSOR_DERIVED],
            proposed=AuthorityClass.SENSOR_DERIVED,
        )
        return finalize_candidate(
            backend=self.name,
            inputs=inputs,
            authority=authority,
            scale_authority=scale_authority,
            scale_state="metric" if inputs.metric_anchor_m is not None else "unresolved",
            coverage={
                "registered_images": len(model.images),
                "input_images": len(images),
                "num_points3d": len(model.points3d),
                "mean_reprojection_error_px": mean_error,
            },
            topology_state={
                "representation": "sparse_point_cloud",
                "point_count": len(model.points3d),
                "watertight": False,
                "manifold": False,
                "surface": False,
            },
            editability="sparse-points; not a mesh",
            hidden_surface_assumptions=[
                "sparse SfM only reconstructs triangulated feature points",
                "no dense surface; dense MVS blocked without CUDA",
            ],
            artifacts={
                "sparse_ply": str(ply_path),
                "summary_json": str(summary_path),
                "cameras_json": str(cameras_path),
                "model_dir": str(model_dir),
            },
            runtime_seconds=timer.seconds,
            execution_log=(
                f"COLMAP sparse: registered {len(model.images)}/{len(images)} images, "
                f"{len(model.points3d)} points, mean reprojection error "
                f"{mean_error:.4f} px"
            ),
            failure_modes=[
                DENSE_UNAVAILABLE_REASON,
                "textureless or reflective surfaces reduce registration",
                "scale is free without a metric anchor",
            ],
            executed=True,
        )

    def _version_line(self) -> str:
        if not self.executable:
            return "missing"
        result = subprocess.run(
            [self.executable, "-h"],
            capture_output=True,
            text=True,
            timeout=15,
            check=False,
        )
        text = (result.stdout or "") + (result.stderr or "")
        for line in text.splitlines():
            if "COLMAP" in line:
                return line.strip()
        return "unknown"

    def _gpu_flags(self) -> tuple[str, str]:
        assert self.executable is not None
        feature_help = subprocess.run(
            [self.executable, "feature_extractor", "--help"],
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
        matcher_help = subprocess.run(
            [self.executable, "exhaustive_matcher", "--help"],
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
        feature_text = feature_help.stdout + feature_help.stderr
        matcher_text = matcher_help.stdout + matcher_help.stderr
        feature_gpu = (
            "--FeatureExtraction.use_gpu"
            if "--FeatureExtraction.use_gpu" in feature_text
            else "--SiftExtraction.use_gpu"
        )
        matcher_gpu = (
            "--FeatureMatching.use_gpu"
            if "--FeatureMatching.use_gpu" in matcher_text
            else "--SiftMatching.use_gpu"
        )
        return feature_gpu, matcher_gpu


def read_colmap_model(path: Path) -> ColmapModel:
    """Parse a COLMAP model from binary files, falling back to text."""
    if (path / "cameras.bin").is_file():
        cameras = read_cameras_bin(path / "cameras.bin")
        images = read_images_bin(path / "images.bin")
        points = read_points3d_bin(path / "points3D.bin")
        return ColmapModel(cameras=cameras, images=images, points3d=points)
    if (path / "cameras.txt").is_file():
        return read_colmap_model_text(path)
    raise FileNotFoundError(f"no COLMAP model at {path}")


def read_colmap_model_text(path: Path) -> ColmapModel:
    cameras: dict[int, ColmapCamera] = {}
    for line in (path / "cameras.txt").read_text(encoding="utf-8").splitlines():
        if not line or line.startswith("#"):
            continue
        fields = line.split()
        cameras[int(fields[0])] = ColmapCamera(
            camera_id=int(fields[0]),
            model=fields[1],
            width=int(fields[2]),
            height=int(fields[3]),
            params=[float(v) for v in fields[4:]],
        )
    images: dict[int, ColmapImage] = {}
    lines = [
        line
        for line in (path / "images.txt").read_text(encoding="utf-8").splitlines()
        if line and not line.startswith("#")
    ]
    for index in range(0, len(lines), 2):
        fields = lines[index].split()
        if len(fields) < 10:
            continue
        image_id = int(fields[0])
        qvec = np.array([float(v) for v in fields[1:5]], dtype=np.float64)
        tvec = np.array([float(v) for v in fields[5:8]], dtype=np.float64)
        camera_id = int(fields[8])
        name = fields[9]
        pts_fields = lines[index + 1].split() if index + 1 < len(lines) else []
        pts = []
        for i in range(0, len(pts_fields), 3):
            if i + 2 >= len(pts_fields):
                break
            pts.append(
                [float(pts_fields[i]), float(pts_fields[i + 1]), int(float(pts_fields[i + 2]))]
            )
        images[image_id] = ColmapImage(
            image_id=image_id,
            qvec=qvec,
            tvec=tvec,
            camera_id=camera_id,
            name=name,
            points2d=np.asarray(pts, dtype=np.float64) if pts else np.zeros((0, 3)),
        )
    points3d: dict[int, ColmapPoint3D] = {}
    points_path = path / "points3D.txt"
    if points_path.is_file():
        for line in points_path.read_text(encoding="utf-8").splitlines():
            if not line or line.startswith("#"):
                continue
            fields = line.split()
            points3d[int(fields[0])] = ColmapPoint3D(
                point_id=int(fields[0]),
                xyz=np.array([float(fields[1]), float(fields[2]), float(fields[3])]),
                rgb=np.array([int(fields[4]), int(fields[5]), int(fields[6])]),
                error=float(fields[7]),
            )
    return ColmapModel(cameras=cameras, images=images, points3d=points3d)


def read_cameras_bin(path: Path) -> dict[int, ColmapCamera]:
    cameras: dict[int, ColmapCamera] = {}
    data = path.read_bytes()
    offset = 0
    (num_cameras,) = struct.unpack_from("<Q", data, offset)
    offset += 8
    for _ in range(num_cameras):
        camera_id, model_id = struct.unpack_from("<ii", data, offset)
        offset += 8
        width, height = struct.unpack_from("<QQ", data, offset)
        offset += 16
        model_name, num_params = _CAMERA_MODEL_IDS.get(model_id, (f"MODEL_{model_id}", 0))
        # Prefer known param count; otherwise stop at next camera is impossible —
        # use table always for standard models.
        if model_id not in _CAMERA_MODEL_IDS:
            raise ValueError(f"unsupported COLMAP camera model id {model_id}")
        params = list(struct.unpack_from("<" + "d" * num_params, data, offset))
        offset += 8 * num_params
        cameras[camera_id] = ColmapCamera(
            camera_id=camera_id,
            model=model_name,
            width=int(width),
            height=int(height),
            params=params,
        )
    return cameras


def read_images_bin(path: Path) -> dict[int, ColmapImage]:
    images: dict[int, ColmapImage] = {}
    data = path.read_bytes()
    offset = 0
    (num_images,) = struct.unpack_from("<Q", data, offset)
    offset += 8
    for _ in range(num_images):
        image_id = struct.unpack_from("<I", data, offset)[0]
        offset += 4
        qvec = np.array(struct.unpack_from("<4d", data, offset), dtype=np.float64)
        offset += 32
        tvec = np.array(struct.unpack_from("<3d", data, offset), dtype=np.float64)
        offset += 24
        camera_id = struct.unpack_from("<I", data, offset)[0]
        offset += 4
        name_chars = []
        while offset < len(data):
            ch = data[offset]
            offset += 1
            if ch == 0:
                break
            name_chars.append(chr(ch))
        name = "".join(name_chars)
        (num_points2d,) = struct.unpack_from("<Q", data, offset)
        offset += 8
        pts = []
        for _p in range(num_points2d):
            x, y, point3d_id = struct.unpack_from("<ddq", data, offset)
            offset += 24
            pts.append([x, y, point3d_id])
        images[image_id] = ColmapImage(
            image_id=image_id,
            qvec=qvec,
            tvec=tvec,
            camera_id=camera_id,
            name=name,
            points2d=np.asarray(pts, dtype=np.float64) if pts else np.zeros((0, 3)),
        )
    return images


def read_points3d_bin(path: Path) -> dict[int, ColmapPoint3D]:
    points: dict[int, ColmapPoint3D] = {}
    data = path.read_bytes()
    offset = 0
    (num_points,) = struct.unpack_from("<Q", data, offset)
    offset += 8
    for _ in range(num_points):
        point_id = struct.unpack_from("<Q", data, offset)[0]
        offset += 8
        xyz = np.array(struct.unpack_from("<3d", data, offset), dtype=np.float64)
        offset += 24
        rgb = np.array(struct.unpack_from("<3B", data, offset), dtype=np.uint8)
        offset += 3
        (error,) = struct.unpack_from("<d", data, offset)
        offset += 8
        (track_length,) = struct.unpack_from("<Q", data, offset)
        offset += 8
        offset += track_length * 8  # image_id uint32 + point2D_idx uint32
        points[point_id] = ColmapPoint3D(
            point_id=point_id, xyz=xyz, rgb=rgb, error=float(error)
        )
    return points


def _mean_reprojection_error(model: ColmapModel) -> float:
    if not model.points3d:
        return float("nan")
    errors = [point.error for point in model.points3d.values()]
    return float(sum(errors) / len(errors))


def _points_cloud(model: ColmapModel) -> PointCloud:
    if not model.points3d:
        return PointCloud(positions=np.zeros((0, 3)))
    positions = np.stack([p.xyz for p in model.points3d.values()])
    colours = np.stack([p.rgb for p in model.points3d.values()]).astype(np.float64) / 255.0
    return PointCloud(positions=positions, colours=colours)


def _cameras_export(model: ColmapModel) -> list[dict[str, Any]]:
    result = []
    for image in model.images.values():
        camera = model.cameras[image.camera_id]
        result.append(
            {
                "image_id": image.image_id,
                "name": image.name,
                "qvec": image.qvec.tolist(),
                "tvec": image.tvec.tolist(),
                "camera": {
                    "camera_id": camera.camera_id,
                    "model": camera.model,
                    "width": camera.width,
                    "height": camera.height,
                    "params": camera.params,
                },
            }
        )
    return result


