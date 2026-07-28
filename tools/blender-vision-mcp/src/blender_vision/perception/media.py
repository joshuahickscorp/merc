from __future__ import annotations

import csv
import io
import json
import mimetypes
import platform
import shutil
import subprocess
import tempfile
import time
import uuid
from pathlib import Path
from typing import Any

import cv2
import numpy as np
from PIL import Image, ImageOps

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.perception.contracts import ArtifactSink, CaptureOutcome
from blender_vision.perception.query import ObservationQueryService
from blender_vision.projects.store import ProjectStore

_IMAGE_SUFFIXES = {".png", ".jpg", ".jpeg", ".webp", ".bmp", ".tif", ".tiff"}
_VIDEO_SUFFIXES = {".mp4", ".mov", ".mkv", ".webm", ".avi", ".m4v"}


def _source_target(
    target: dict[str, Any],
    *,
    suffixes: set[str],
    kind: str,
) -> dict[str, Any]:
    path = Path(str(target.get("path", ""))).expanduser().resolve()
    if not path.is_file() or path.suffix.lower() not in suffixes:
        raise ValueError(f"{kind} target must be an existing supported file")
    digest, size = sha256_file(path)
    if size > 2 * 1024 * 1024 * 1024:
        raise ValueError(f"{kind} source exceeds the 2 GiB capture bound")
    return {
        "id": str(target.get("id") or digest),
        "kind": kind,
        "path": str(path),
        "digest": digest,
        "size": size,
    }


class ImageFileAdapter:
    name = "image.file"
    version = "1"

    def normalize_target(self, target: dict[str, Any]) -> dict[str, Any]:
        return _source_target(target, suffixes=_IMAGE_SUFFIXES, kind="image")

    def normalize_config(
        self, target: dict[str, Any], config: dict[str, Any]
    ) -> dict[str, Any]:
        del target
        return {
            "maximum_dimension": max(
                64, min(int(config.get("maximum_dimension", 2048)), 8192)
            ),
            "ocr": bool(config.get("ocr", True)),
            "maximum_regions": max(
                1, min(int(config.get("maximum_regions", 128)), 1024)
            ),
            "depth": _normalize_depth(config),
        }

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        return {
            "platform": platform.platform(),
            "python": platform.python_version(),
            "pillow": Image.__version__,
            "opencv": cv2.__version__,
            "tesseract": self._tesseract_version() if config["ocr"] else None,
            "adapter": self.name,
            "adapter_version": self.version,
        }

    def capture(
        self,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
    ) -> CaptureOutcome:
        path = Path(target["path"])
        source = sink(
            "image.source",
            path.read_bytes(),
            mimetypes.guess_type(path.name)[0] or "application/octet-stream",
            {"source_digest": target["digest"]},
        )
        analysis, normalized_png = analyze_image(path, config)
        normalized = sink(
            "image.normalized",
            normalized_png,
            "image/png",
            {
                "coordinate_space": "normalized-image-pixels",
                "source_digest": source["digest"],
            },
        )
        depth = _capture_depth(config["depth"], analysis, sink)
        graph = image_graph(
            analysis,
            source_digest=source["digest"],
            normalized_digest=normalized["digest"],
            source_kind=target["kind"],
            depth=depth,
        )
        sink("image.graph", canonical_json(graph), "application/json", None)
        return CaptureOutcome(
            summary={
                "width": analysis["width"],
                "height": analysis["height"],
                "region_count": len(analysis["regions"]),
                "text_symbol_count": len(analysis["ocr"]),
                "perceptual_hash": analysis["perceptual_hash"],
            },
            limitations=[
                "Contour regions and OCR symbols are DERIVED interpretations of observed pixels.",
                (
                    "Depth authority is limited to the supplied governed depth artifact."
                    if depth
                    else "Single-image depth and occluded structure remain unobserved."
                ),
            ],
            graphs=[
                {
                    "graph_type": "ImageGraph",
                    "role": "image.graph",
                    "node_count": len(graph["nodes"]),
                    "edge_count": len(graph["edges"]),
                    "authority": "MIXED",
                }
            ],
        )

    @staticmethod
    def _tesseract_version() -> str | None:
        executable = shutil.which("tesseract")
        if not executable:
            return None
        try:
            return subprocess.run(
                [executable, "--version"],
                check=True,
                capture_output=True,
                text=True,
                timeout=5,
            ).stdout.splitlines()[0]
        except (OSError, subprocess.SubprocessError):
            return None


class CameraFrameAdapter(ImageFileAdapter):
    name = "camera.frame"

    def normalize_target(self, target: dict[str, Any]) -> dict[str, Any]:
        return _source_target(target, suffixes=_IMAGE_SUFFIXES, kind="camera-frame")

    def normalize_config(
        self, target: dict[str, Any], config: dict[str, Any]
    ) -> dict[str, Any]:
        normalized = super().normalize_config(target, config)
        if not config.get("user_authorized"):
            raise PermissionError("camera frame ingestion requires explicit user_authorized=true")
        normalized["device_label"] = str(config.get("device_label", "user-authorized-camera"))
        normalized["calibration"] = config.get("calibration")
        return normalized

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        return {
            **super().environment(config),
            "device_label": config["device_label"],
            "calibration_recorded": config["calibration"] is not None,
        }


class LiveCameraAdapter(ImageFileAdapter):
    name = "camera.live"

    def normalize_target(self, target: dict[str, Any]) -> dict[str, Any]:
        device_index = int(target.get("device_index", 0))
        if device_index not in range(0, 33):
            raise ValueError("camera device_index must be between 0 and 32")
        label = str(target.get("label", f"camera-{device_index}")).strip()
        session_id = str(target.get("session_id", "")).strip()
        if not label or not session_id:
            raise ValueError("live camera requires a non-empty label and session_id")
        return {
            "id": f"live-camera:{device_index}:{label}:{session_id}",
            "kind": "live-camera",
            "device_index": device_index,
            "label": label,
            "session_id": session_id,
        }

    def normalize_config(
        self, target: dict[str, Any], config: dict[str, Any]
    ) -> dict[str, Any]:
        del target
        if not config.get("user_authorized"):
            raise PermissionError("live camera requires explicit user_authorized=true")
        return {
            "frame_count": max(1, min(int(config.get("frame_count", 3)), 120)),
            "interval_ms": max(0, min(int(config.get("interval_ms", 100)), 10_000)),
            "width": max(0, min(int(config.get("width", 0)), 8192)),
            "height": max(0, min(int(config.get("height", 0)), 8192)),
            "maximum_dimension": max(
                64, min(int(config.get("maximum_dimension", 1280)), 4096)
            ),
            "maximum_regions": max(
                1, min(int(config.get("maximum_regions", 64)), 256)
            ),
            "ocr": bool(config.get("ocr", False)),
            "calibration": config.get("calibration"),
        }

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        return {
            "platform": platform.platform(),
            "python": platform.python_version(),
            "opencv": cv2.__version__,
            "adapter": self.name,
            "adapter_version": self.version,
            "authorization": "explicit",
            "frame_count": config["frame_count"],
            "calibration_recorded": config["calibration"] is not None,
        }

    def capture(
        self,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
    ) -> CaptureOutcome:
        device = cv2.VideoCapture(target["device_index"])
        if not device.isOpened():
            device.release()
            raise RuntimeError("authorized camera device could not be opened")
        if config["width"]:
            device.set(cv2.CAP_PROP_FRAME_WIDTH, config["width"])
        if config["height"]:
            device.set(cv2.CAP_PROP_FRAME_HEIGHT, config["height"])
        frames = []
        try:
            with tempfile.TemporaryDirectory(prefix="vision-camera-") as directory:
                root = Path(directory)
                for index in range(config["frame_count"]):
                    ok, array = device.read()
                    if not ok or array is None:
                        raise RuntimeError(f"camera failed to emit authorized frame {index}")
                    frame_path = root / f"frame-{index:03d}.png"
                    if not cv2.imwrite(str(frame_path), array):
                        raise RuntimeError(f"camera frame {index} could not be encoded")
                    analysis, normalized_png = analyze_image(frame_path, config)
                    timestamp = index * config["interval_ms"] / 1000
                    artifact = sink(
                        f"camera.frame.{index:03d}",
                        normalized_png,
                        "image/png",
                        {
                            "timestamp_seconds": timestamp,
                            "device_label": target["label"],
                        },
                    )
                    frames.append(
                        {
                            "index": index,
                            "timestamp_seconds": timestamp,
                            "artifact_digest": artifact["digest"],
                            "analysis": analysis,
                            "evidence_role": f"camera.frame.{index:03d}",
                        }
                    )
                    if index + 1 < config["frame_count"] and config["interval_ms"]:
                        time.sleep(config["interval_ms"] / 1000)
        finally:
            device.release()
        source_digest = __import__("hashlib").sha256(
            canonical_json([frame["artifact_digest"] for frame in frames])
        ).hexdigest()
        graph = video_graph(
            frames,
            {
                "live_camera": {
                    "device_index": target["device_index"],
                    "label": target["label"],
                    "calibration": config["calibration"],
                }
            },
            source_digest,
        )
        graph["schema"] = "vision.camera-sequence-graph/v1"
        graph["graph_type"] = "CameraSequenceGraph"
        sink("camera.graph", canonical_json(graph), "application/json", None)
        return CaptureOutcome(
            summary={
                "frame_count": len(frames),
                "duration_seconds": frames[-1]["timestamp_seconds"],
                "calibration_recorded": config["calibration"] is not None,
            },
            limitations=[
                "Acquisition is bounded to the explicitly authorized frame count.",
                "2D global motion is not a calibrated 3D trajectory.",
                (
                    "Camera calibration was not supplied."
                    if config["calibration"] is None
                    else "Metric use is limited to the supplied calibration record."
                ),
            ],
            graphs=[
                {
                    "graph_type": "CameraSequenceGraph",
                    "role": "camera.graph",
                    "node_count": len(graph["nodes"]),
                    "edge_count": len(graph["edges"]),
                    "authority": "MIXED",
                }
            ],
        )


class VideoFileAdapter:
    name = "video.file"
    version = "1"

    def normalize_target(self, target: dict[str, Any]) -> dict[str, Any]:
        return _source_target(target, suffixes=_VIDEO_SUFFIXES, kind="video")

    def normalize_config(
        self, target: dict[str, Any], config: dict[str, Any]
    ) -> dict[str, Any]:
        del target
        if not shutil.which("ffmpeg") or not shutil.which("ffprobe"):
            raise RuntimeError("video capture requires ffmpeg and ffprobe")
        return {
            "sample_count": max(2, min(int(config.get("sample_count", 8)), 60)),
            "maximum_dimension": max(
                64, min(int(config.get("maximum_dimension", 1280)), 4096)
            ),
            "maximum_regions": max(
                1, min(int(config.get("maximum_regions", 64)), 256)
            ),
            "ocr": bool(config.get("ocr", False)),
        }

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        return {
            "platform": platform.platform(),
            "python": platform.python_version(),
            "opencv": cv2.__version__,
            "ffmpeg": self._command_version("ffmpeg"),
            "ffprobe": self._command_version("ffprobe"),
            "adapter": self.name,
            "adapter_version": self.version,
            "sample_count": config["sample_count"],
        }

    def capture(
        self,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
    ) -> CaptureOutcome:
        path = Path(target["path"])
        source = sink(
            "video.source",
            path.read_bytes(),
            mimetypes.guess_type(path.name)[0] or "video/mp4",
            {"source_digest": target["digest"]},
        )
        probe = self._probe(path)
        duration = float(probe.get("format", {}).get("duration") or 0)
        if duration <= 0:
            raise ValueError("video duration is unavailable or zero")
        timestamps = [
            duration * index / config["sample_count"]
            for index in range(config["sample_count"])
        ]
        frames: list[dict[str, Any]] = []
        with tempfile.TemporaryDirectory(prefix="vision-video-") as directory:
            root = Path(directory)
            for index, timestamp in enumerate(timestamps):
                frame_path = root / f"frame-{index:03d}.png"
                subprocess.run(
                    [
                        shutil.which("ffmpeg") or "ffmpeg",
                        "-loglevel",
                        "error",
                        "-ss",
                        f"{timestamp:.6f}",
                        "-i",
                        str(path),
                        "-frames:v",
                        "1",
                        "-vf",
                        (
                            f"scale='min({config['maximum_dimension']},iw)':"
                            f"'min({config['maximum_dimension']},ih)':"
                            "force_original_aspect_ratio=decrease"
                        ),
                        "-y",
                        str(frame_path),
                    ],
                    check=True,
                    capture_output=True,
                    timeout=60,
                )
                if not frame_path.is_file():
                    raise RuntimeError(f"ffmpeg did not emit frame {index}")
                analysis, normalized_png = analyze_image(frame_path, config)
                evidence = sink(
                    f"video.frame.{index:03d}",
                    normalized_png,
                    "image/png",
                    {"timestamp_seconds": timestamp, "source_digest": source["digest"]},
                )
                frames.append(
                    {
                        "index": index,
                        "timestamp_seconds": timestamp,
                        "artifact_digest": evidence["digest"],
                        "analysis": analysis,
                    }
                )
        graph = video_graph(frames, probe, source["digest"])
        sink("video.metadata", canonical_json(probe), "application/json", None)
        sink("video.graph", canonical_json(graph), "application/json", None)
        return CaptureOutcome(
            summary={
                "duration_seconds": duration,
                "sample_count": len(frames),
                "track_count": len(graph["tracks"]),
                "scene_count": len(graph["scenes"]),
                "camera_motion_sample_count": len(graph["camera_motion"]),
            },
            limitations=[
                "Frame sampling is bounded and can miss events between sampled timestamps.",
                "2D global motion is not promoted to a calibrated 3D camera trajectory.",
                "Monocular depth remains unavailable unless a governed depth backend is added.",
            ],
            graphs=[
                {
                    "graph_type": "VideoNarrativeGraph",
                    "role": "video.graph",
                    "node_count": len(graph["nodes"]),
                    "edge_count": len(graph["edges"]),
                    "authority": "MIXED",
                }
            ],
        )

    @staticmethod
    def _probe(path: Path) -> dict[str, Any]:
        result = subprocess.run(
            [
                shutil.which("ffprobe") or "ffprobe",
                "-v",
                "error",
                "-show_format",
                "-show_streams",
                "-of",
                "json",
                str(path),
            ],
            check=True,
            capture_output=True,
            text=True,
            timeout=30,
        )
        return json.loads(result.stdout)

    @staticmethod
    def _command_version(name: str) -> str:
        result = subprocess.run(
            [shutil.which(name) or name, "-version"],
            check=True,
            capture_output=True,
            text=True,
            timeout=5,
        )
        return result.stdout.splitlines()[0]


class DesktopSnapshotAdapter:
    name = "desktop.authorized_snapshot"
    version = "1"

    def normalize_target(self, target: dict[str, Any]) -> dict[str, Any]:
        normalized = _source_target(
            target, suffixes=_IMAGE_SUFFIXES, kind="desktop-snapshot"
        )
        normalized["application"] = str(target.get("application", "unknown"))
        normalized["window_title"] = str(target.get("window_title", "unknown"))
        return normalized

    def normalize_config(
        self, target: dict[str, Any], config: dict[str, Any]
    ) -> dict[str, Any]:
        del target
        if not config.get("user_authorized"):
            raise PermissionError(
                "desktop capture requires explicit user_authorized=true"
            )
        accessibility_path = config.get("accessibility_path")
        accessibility = None
        if accessibility_path:
            path = Path(str(accessibility_path)).expanduser().resolve()
            if not path.is_file() or path.suffix.lower() != ".json":
                raise ValueError("accessibility_path must be an existing JSON file")
            digest, size = sha256_file(path)
            accessibility = {"path": str(path), "digest": digest, "size": size}
        return {
            "accessibility": accessibility,
            "maximum_dimension": max(
                64, min(int(config.get("maximum_dimension", 2048)), 8192)
            ),
            "maximum_regions": max(
                1, min(int(config.get("maximum_regions", 128)), 512)
            ),
            "ocr": bool(config.get("ocr", True)),
        }

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        return {
            "platform": platform.platform(),
            "python": platform.python_version(),
            "adapter": self.name,
            "adapter_version": self.version,
            "accessibility_digest": (
                config["accessibility"]["digest"]
                if config["accessibility"]
                else None
            ),
            "authorization": "explicit",
        }

    def capture(
        self,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
    ) -> CaptureOutcome:
        path = Path(target["path"])
        screenshot = sink(
            "desktop.screenshot",
            path.read_bytes(),
            mimetypes.guess_type(path.name)[0] or "image/png",
            {
                "application": target["application"],
                "window_title": target["window_title"],
            },
        )
        analysis, normalized_png = analyze_image(path, config)
        normalized = sink(
            "desktop.normalized",
            normalized_png,
            "image/png",
            {"source_digest": screenshot["digest"]},
        )
        accessibility = {"nodes": [], "windows": []}
        accessibility_record = None
        if config["accessibility"]:
            accessibility_path = Path(config["accessibility"]["path"])
            accessibility = json.loads(
                accessibility_path.read_text(encoding="utf-8")
            )
            accessibility_record = sink(
                "desktop.accessibility",
                canonical_json(accessibility),
                "application/json",
                {"source_digest": config["accessibility"]["digest"]},
            )
        graph = desktop_graph(
            target,
            analysis,
            accessibility,
            screenshot["digest"],
            normalized["digest"],
            accessibility_record["digest"] if accessibility_record else None,
        )
        sink("desktop.graph", canonical_json(graph), "application/json", None)
        return CaptureOutcome(
            summary={
                "application": target["application"],
                "window_title": target["window_title"],
                "visual_region_count": len(analysis["regions"]),
                "accessibility_node_count": len(accessibility.get("nodes", [])),
                "correspondence_count": sum(
                    edge["type"] == "CORRESPONDS_TO" for edge in graph["edges"]
                ),
            },
            limitations=[
                "This adapter consumes a user-authorized synchronized snapshot, not ambient UI.",
                "Accessibility-to-pixel correspondences are spatial DERIVED matches.",
            ],
            graphs=[
                {
                    "graph_type": "DesktopExperienceGraph",
                    "role": "desktop.graph",
                    "node_count": len(graph["nodes"]),
                    "edge_count": len(graph["edges"]),
                    "authority": "MIXED",
                }
            ],
        )


def analyze_image(
    path: Path, config: dict[str, Any]
) -> tuple[dict[str, Any], bytes]:
    with Image.open(path) as image:
        image = ImageOps.exif_transpose(image).convert("RGB")
        width, height = image.size
        scale = min(1.0, config["maximum_dimension"] / max(width, height))
        if scale < 1.0:
            image = image.resize(
                (max(1, round(width * scale)), max(1, round(height * scale))),
                Image.Resampling.LANCZOS,
            )
        normalized_width, normalized_height = image.size
        array = np.asarray(image)
        grayscale = cv2.cvtColor(array, cv2.COLOR_RGB2GRAY)
        edges = cv2.Canny(grayscale, 80, 180)
        contours, _hierarchy = cv2.findContours(
            edges, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE
        )
        minimum_area = normalized_width * normalized_height * 0.001
        boxes = []
        for contour in contours:
            x, y, box_width, box_height = cv2.boundingRect(contour)
            area = box_width * box_height
            if area >= minimum_area:
                boxes.append((area, x, y, box_width, box_height))
        boxes.sort(key=lambda item: (-item[0], item[1], item[2]))
        regions = []
        for index, (area, x, y, box_width, box_height) in enumerate(
            boxes[: config["maximum_regions"]]
        ):
            crop = array[y : y + box_height, x : x + box_width]
            mean = crop.mean(axis=(0, 1)).tolist() if crop.size else [0, 0, 0]
            regions.append(
                {
                    "id": f"region:{index}",
                    "bounds": {
                        "x": x,
                        "y": y,
                        "width": box_width,
                        "height": box_height,
                    },
                    "area": area,
                    "mean_rgb": [round(float(value), 3) for value in mean],
                    "authority": "DERIVED",
                    "confidence": min(0.95, area / max(1, normalized_width * normalized_height)),
                }
            )
        ocr = (
            _ocr(path, scale_x=normalized_width / width, scale_y=normalized_height / height)
            if config.get("ocr") and shutil.which("tesseract")
            else []
        )
        reduced = cv2.resize(grayscale, (8, 8), interpolation=cv2.INTER_AREA)
        average = float(reduced.mean())
        perceptual_hash = "".join(
            "1" if value >= average else "0" for value in reduced.flatten()
        )
        output = io.BytesIO()
        image.save(output, format="PNG", optimize=False)
        return (
            {
                "width": normalized_width,
                "height": normalized_height,
                "original_width": width,
                "original_height": height,
                "scale": scale,
                "mean_rgb": [
                    round(float(value), 3)
                    for value in array.mean(axis=(0, 1)).tolist()
                ],
                "edge_fraction": float(np.count_nonzero(edges) / edges.size),
                "perceptual_hash": f"{int(perceptual_hash, 2):016x}",
                "regions": regions,
                "ocr": ocr,
            },
            output.getvalue(),
        )


def _ocr(path: Path, *, scale_x: float, scale_y: float) -> list[dict[str, Any]]:
    try:
        result = subprocess.run(
            [
                shutil.which("tesseract") or "tesseract",
                str(path),
                "stdout",
                "--psm",
                "11",
                "tsv",
            ],
            check=True,
            capture_output=True,
            text=True,
            timeout=30,
        )
    except (OSError, subprocess.SubprocessError):
        return []
    symbols = []
    reader = csv.DictReader(io.StringIO(result.stdout), delimiter="\t")
    for row in reader:
        text = str(row.get("text") or "").strip()
        try:
            confidence = float(row.get("conf") or -1)
        except ValueError:
            confidence = -1
        if not text or confidence < 0:
            continue
        symbols.append(
            {
                "id": f"text:{len(symbols)}",
                "text": text,
                "confidence": confidence / 100.0,
                "bounds": {
                    "x": round(float(row["left"]) * scale_x, 3),
                    "y": round(float(row["top"]) * scale_y, 3),
                    "width": round(float(row["width"]) * scale_x, 3),
                    "height": round(float(row["height"]) * scale_y, 3),
                },
                "authority": "DERIVED",
            }
        )
    return symbols


def image_graph(
    analysis: dict[str, Any],
    *,
    source_digest: str,
    normalized_digest: str,
    source_kind: str,
    depth: dict[str, Any] | None = None,
) -> dict[str, Any]:
    evidence = [
        {"role": "image.source", "artifact_digest": source_digest},
        {"role": "image.normalized", "artifact_digest": normalized_digest},
    ]
    root_id = f"{source_kind}:canvas"
    nodes = [
        {
            "id": root_id,
            "domain_type": "ImageCanvas",
            "spatial_bounds": {
                "x": 0,
                "y": 0,
                "width": analysis["width"],
                "height": analysis["height"],
            },
            "temporal_validity": "static",
            "evidence_references": evidence,
            "authority": "OBSERVED",
            "confidence": 1.0,
            "source_restrictions": ["governed-media-source"],
            "uncertainty": [],
            "revision_lineage": [],
            "perceptual_hash": analysis["perceptual_hash"],
            "mean_rgb": analysis["mean_rgb"],
        }
    ]
    edges = []
    for region in analysis["regions"]:
        node_id = f"{source_kind}:{region['id']}"
        nodes.append(
            {
                "id": node_id,
                "domain_type": "VisualRegion",
                "spatial_bounds": region["bounds"],
                "bounds": region["bounds"],
                "temporal_validity": "static",
                "evidence_references": evidence,
                "authority": "DERIVED",
                "confidence": region["confidence"],
                "source_restrictions": ["governed-media-source"],
                "uncertainty": ["contour-is-not-semantic-object-identification"],
                "revision_lineage": [],
                "mean_rgb": region["mean_rgb"],
            }
        )
        edges.append(
            {
                "source": root_id,
                "target": node_id,
                "type": "CONTAINS",
                "authority": "DERIVED",
                "evidence_references": evidence,
            }
        )
    for symbol in analysis["ocr"]:
        node_id = f"{source_kind}:{symbol['id']}"
        nodes.append(
            {
                "id": node_id,
                "domain_type": "TextSymbol",
                "spatial_bounds": symbol["bounds"],
                "bounds": symbol["bounds"],
                "temporal_validity": "static",
                "evidence_references": evidence,
                "authority": "DERIVED",
                "confidence": symbol["confidence"],
                "source_restrictions": ["governed-media-source"],
                "uncertainty": ["ocr-can-be-incorrect"],
                "revision_lineage": [],
                "text": symbol["text"],
            }
        )
        edges.append(
            {
                "source": root_id,
                "target": node_id,
                "type": "CONTAINS",
                "authority": "DERIVED",
                "evidence_references": evidence,
            }
        )
    if depth is not None:
        depth_id = f"{source_kind}:depth"
        depth_evidence = [
            {"role": "image.depth", "artifact_digest": depth["artifact_digest"]}
        ]
        nodes.append(
            {
                "id": depth_id,
                "domain_type": "DepthMap",
                "spatial_bounds": {
                    "x": 0,
                    "y": 0,
                    "width": analysis["width"],
                    "height": analysis["height"],
                },
                "temporal_validity": "static",
                "evidence_references": depth_evidence,
                "authority": depth["authority"],
                "confidence": depth["confidence"],
                "source_restrictions": ["governed-depth-source"],
                "uncertainty": depth["uncertainty"],
                "revision_lineage": [],
                "depth_kind": depth["kind"],
                "encoding": depth["encoding"],
                "calibration": depth["calibration"],
                "model_identity": depth["model_identity"],
                "license": depth["license"],
            }
        )
        edges.append(
            {
                "source": root_id,
                "target": depth_id,
                "type": "ALIGNS_DEPTH",
                "authority": depth["authority"],
                "evidence_references": depth_evidence,
            }
        )
    return {
        "schema": "vision.image-graph/v1",
        "graph_type": "ImageGraph",
        "authority": "MIXED",
        "nodes": nodes,
        "edges": edges,
        "analysis": analysis,
        "depth": (
            {
                key: value
                for key, value in depth.items()
                if key not in {"artifact_digest", "path"}
            }
            if depth
            else {"status": "UNAVAILABLE"}
        ),
    }


def video_graph(
    frames: list[dict[str, Any]],
    probe: dict[str, Any],
    source_digest: str,
) -> dict[str, Any]:
    nodes = []
    edges = []
    tracks: list[dict[str, Any]] = []
    previous_regions: list[dict[str, Any]] = []
    next_track = 0
    active_tracks: dict[int, dict[str, Any]] = {}
    scene_boundaries = [0]
    camera_motion = []
    previous_gray = None
    previous_hash = None
    for frame in frames:
        frame_id = f"video:frame:{frame['index']}"
        evidence = [
            {
                "role": frame.get(
                    "evidence_role", f"video.frame.{frame['index']:03d}"
                ),
                "artifact_digest": frame["artifact_digest"],
            }
        ]
        nodes.append(
            {
                "id": frame_id,
                "domain_type": "VideoFrame",
                "spatial_bounds": {
                    "x": 0,
                    "y": 0,
                    "width": frame["analysis"]["width"],
                    "height": frame["analysis"]["height"],
                },
                "temporal_validity": {
                    "timestamp_seconds": frame["timestamp_seconds"]
                },
                "evidence_references": evidence,
                "authority": "OBSERVED",
                "confidence": 1.0,
                "source_restrictions": ["governed-media-source"],
                "uncertainty": [],
                "revision_lineage": [],
                "perceptual_hash": frame["analysis"]["perceptual_hash"],
            }
        )
        if frame["index"]:
            edges.append(
                {
                    "source": f"video:frame:{frame['index'] - 1}",
                    "target": frame_id,
                    "type": "TEMPORALLY_FOLLOWS",
                    "authority": "OBSERVED",
                    "evidence_references": evidence,
                }
            )
        current_assignments = []
        for region in frame["analysis"]["regions"]:
            best = None
            best_iou = 0.0
            for previous in previous_regions:
                overlap = _iou(region["bounds"], previous["bounds"])
                if overlap > best_iou:
                    best, best_iou = previous, overlap
            if best is not None and best_iou >= 0.2:
                track_id = best["track_id"]
            else:
                track_id = next_track
                next_track += 1
                active_tracks[track_id] = {
                    "id": f"track:{track_id}",
                    "samples": [],
                    "authority": "DERIVED",
                    "confidence": 0.5,
                }
            active_tracks[track_id]["samples"].append(
                {
                    "frame_id": frame_id,
                    "timestamp_seconds": frame["timestamp_seconds"],
                    "bounds": region["bounds"],
                    "iou_from_previous": best_iou if best else None,
                }
            )
            current_assignments.append({**region, "track_id": track_id})
        previous_regions = current_assignments
        current_hash = int(frame["analysis"]["perceptual_hash"], 16)
        if previous_hash is not None:
            hamming = (current_hash ^ previous_hash).bit_count() / 64
            if hamming >= 0.35:
                scene_boundaries.append(frame["index"])
        previous_hash = current_hash
        frame_path_array = np.zeros(
            (frame["analysis"]["height"], frame["analysis"]["width"]), dtype=np.float32
        )
        for region in frame["analysis"]["regions"]:
            bounds = region["bounds"]
            cv2.rectangle(
                frame_path_array,
                (int(bounds["x"]), int(bounds["y"])),
                (
                    int(bounds["x"] + bounds["width"]),
                    int(bounds["y"] + bounds["height"]),
                ),
                255,
                1,
            )
        if previous_gray is not None:
            shift, response = cv2.phaseCorrelate(previous_gray, frame_path_array)
            camera_motion.append(
                {
                    "from_frame": frame["index"] - 1,
                    "to_frame": frame["index"],
                    "translation_pixels": {"x": shift[0], "y": shift[1]},
                    "confidence": response,
                    "classification": "camera_or_global_image_motion_2d",
                    "authority": "DERIVED",
                }
            )
        previous_gray = frame_path_array
    tracks = list(active_tracks.values())
    scenes = []
    boundaries = scene_boundaries + [len(frames)]
    for index, (start, end) in enumerate(zip(boundaries, boundaries[1:], strict=False)):
        scenes.append(
            {
                "id": f"scene:{index}",
                "start_frame": start,
                "end_frame_exclusive": end,
                "start_seconds": frames[start]["timestamp_seconds"],
                "end_seconds": frames[end - 1]["timestamp_seconds"],
                "authority": "DERIVED",
            }
        )
    for track in tracks:
        for sample in track["samples"]:
            edges.append(
                {
                    "source": track["id"],
                    "target": sample["frame_id"],
                    "type": "TRACKS",
                    "authority": "DERIVED",
                    "evidence_references": [],
                }
            )
    return {
        "schema": "vision.video-narrative-graph/v1",
        "graph_type": "VideoNarrativeGraph",
        "authority": "MIXED",
        "source_digest": source_digest,
        "nodes": nodes,
        "edges": edges,
        "tracks": tracks,
        "scenes": scenes,
        "camera_motion": camera_motion,
        "depth": {
            "status": "UNAVAILABLE",
            "reason": "no governed depth backend configured",
        },
        "metadata": probe,
    }


def desktop_graph(
    target: dict[str, Any],
    analysis: dict[str, Any],
    accessibility: dict[str, Any],
    screenshot_digest: str,
    normalized_digest: str,
    accessibility_digest: str | None,
) -> dict[str, Any]:
    visual = image_graph(
        analysis,
        source_digest=screenshot_digest,
        normalized_digest=normalized_digest,
        source_kind="desktop",
    )
    nodes = list(visual["nodes"])
    edges = list(visual["edges"])
    ax_evidence = (
        [{"role": "desktop.accessibility", "artifact_digest": accessibility_digest}]
        if accessibility_digest
        else []
    )
    ax_nodes = []
    for index, item in enumerate(accessibility.get("nodes", [])):
        node_id = f"desktop:ax:{item.get('id', index)}"
        bounds = item.get("bounds")
        node = {
            "id": node_id,
            "domain_type": "AccessibilityNode",
            "spatial_bounds": bounds,
            "bounds": bounds,
            "temporal_validity": "synchronized-snapshot",
            "evidence_references": ax_evidence,
            "authority": "OBSERVED",
            "confidence": 1.0,
            "source_restrictions": ["user-authorized-desktop-snapshot"],
            "uncertainty": [],
            "revision_lineage": [],
            "role": item.get("role"),
            "name": item.get("name"),
            "value": item.get("value"),
        }
        nodes.append(node)
        ax_nodes.append(node)
    visual_regions = [
        node for node in nodes if node["domain_type"] == "VisualRegion"
    ]
    for region in visual_regions:
        center = {
            "x": region["bounds"]["x"] + region["bounds"]["width"] / 2,
            "y": region["bounds"]["y"] + region["bounds"]["height"] / 2,
        }
        for ax_node in ax_nodes:
            bounds = ax_node.get("bounds")
            if bounds and _contains(bounds, center):
                edges.append(
                    {
                        "source": region["id"],
                        "target": ax_node["id"],
                        "type": "CORRESPONDS_TO",
                        "authority": "DERIVED",
                        "confidence": 0.6,
                        "evidence_references": (
                            region["evidence_references"] + ax_node["evidence_references"]
                        ),
                    }
                )
    return {
        "schema": "vision.desktop-experience-graph/v1",
        "graph_type": "DesktopExperienceGraph",
        "authority": "MIXED",
        "application": target["application"],
        "window_title": target["window_title"],
        "nodes": nodes,
        "edges": edges,
        "windows": accessibility.get("windows", []),
    }


def _contains(bounds: dict[str, Any], point: dict[str, float]) -> bool:
    return (
        float(bounds["x"]) <= point["x"] <= float(bounds["x"]) + float(bounds["width"])
        and float(bounds["y"])
        <= point["y"]
        <= float(bounds["y"]) + float(bounds["height"])
    )


def _iou(left: dict[str, Any], right: dict[str, Any]) -> float:
    x1 = max(float(left["x"]), float(right["x"]))
    y1 = max(float(left["y"]), float(right["y"]))
    x2 = min(
        float(left["x"]) + float(left["width"]),
        float(right["x"]) + float(right["width"]),
    )
    y2 = min(
        float(left["y"]) + float(left["height"]),
        float(right["y"]) + float(right["height"]),
    )
    intersection = max(0.0, x2 - x1) * max(0.0, y2 - y1)
    left_area = float(left["width"]) * float(left["height"])
    right_area = float(right["width"]) * float(right["height"])
    return intersection / max(1.0, left_area + right_area - intersection)


def _normalize_depth(config: dict[str, Any]) -> dict[str, Any] | None:
    raw_path = config.get("depth_path")
    if raw_path is None:
        return None
    path = Path(str(raw_path)).expanduser().resolve()
    if not path.is_file() or path.suffix.lower() not in _IMAGE_SUFFIXES:
        raise ValueError("depth_path must be an existing supported image file")
    kind = str(config.get("depth_kind", "")).lower()
    if kind not in {"sensor", "model"}:
        raise ValueError("depth_kind must be sensor or model")
    calibration = config.get("depth_calibration")
    model_identity = config.get("depth_model_identity")
    license_record = config.get("depth_license")
    if kind == "sensor" and not calibration:
        raise ValueError("sensor depth requires depth_calibration")
    if kind == "model" and (not model_identity or not license_record):
        raise ValueError("model depth requires model identity and license")
    digest, size = sha256_file(path)
    return {
        "path": str(path),
        "digest": digest,
        "size": size,
        "kind": kind,
        "calibration": calibration,
        "model_identity": model_identity,
        "license": license_record,
        "encoding": str(config.get("depth_encoding", "relative-grayscale")),
    }


def _capture_depth(
    depth: dict[str, Any] | None,
    analysis: dict[str, Any],
    sink: ArtifactSink,
) -> dict[str, Any] | None:
    if depth is None:
        return None
    path = Path(depth["path"])
    with Image.open(path) as image:
        image = ImageOps.exif_transpose(image)
        if image.size not in {
            (analysis["original_width"], analysis["original_height"]),
            (analysis["width"], analysis["height"]),
        }:
            raise ValueError("depth map dimensions must align with the source image")
    artifact = sink(
        "image.depth",
        path.read_bytes(),
        mimetypes.guess_type(path.name)[0] or "image/png",
        {
            "source_digest": depth["digest"],
            "depth_kind": depth["kind"],
            "encoding": depth["encoding"],
        },
    )
    authority = "OBSERVED" if depth["kind"] == "sensor" else "DERIVED"
    return {
        **depth,
        "artifact_digest": artifact["digest"],
        "authority": authority,
        "confidence": 1.0 if depth["kind"] == "sensor" else 0.7,
        "uncertainty": (
            []
            if depth["kind"] == "sensor"
            else ["model-depth-is-not-direct-metric-observation"]
        ),
        "status": "AVAILABLE",
    }


class MediaReconstructionService:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.query = ObservationQueryService(project)

    def reconstruct_interface(self, capture_id: str) -> dict[str, Any]:
        existing = self._existing(capture_id, "media_to_interface")
        if existing is not None:
            return existing
        graph = self._media_graph(capture_id)
        regions = []
        texts = []
        for node in graph.get("nodes", []):
            bounds = node.get("bounds") or node.get("spatial_bounds")
            if node.get("domain_type") == "VisualRegion" and bounds:
                regions.append(
                    {
                        "id": node["id"],
                        "bounds": bounds,
                        "mean_rgb": node.get("mean_rgb"),
                        "authority": "HYPOTHESIS",
                        "source_evidence": node.get("evidence_references", []),
                    }
                )
            if node.get("domain_type") == "TextSymbol" and bounds:
                texts.append(
                    {
                        "id": node["id"],
                        "text": node.get("text"),
                        "bounds": bounds,
                        "confidence": node.get("confidence"),
                        "source_evidence": node.get("evidence_references", []),
                    }
                )
        record_id = str(uuid.uuid4())
        now = utc_now()
        record = {
            "schema": "vision.media-interface-ir/v1",
            "id": record_id,
            "capture_id": capture_id,
            "mode": "media_to_interface",
            "status": "CANDIDATE",
            "authority": "HYPOTHESIS",
            "coordinate_space": "observed media pixels",
            "regions": regions,
            "text_symbols": texts,
            "temporal_tracks": graph.get("tracks", []),
            "implementation_contract": {
                "editable": True,
                "copy_reference_source": False,
                "copy_reference_assets": False,
                "layout_strategy": "constraint-candidates-from-observed-bounds",
                "mandatory_verification": [
                    "rendered pixel comparison",
                    "semantic accessibility review",
                    "global regression gate",
                ],
            },
            "citation": graph["citation"],
            "limitations": [
                "Visual regions are not automatically semantic components.",
                "The reconstruction is a clean-room editable candidate, not accepted fidelity.",
            ],
            "created_at": now,
        }
        relative = Path("observations") / "reconstructions" / f"{record_id}.json"
        atomic_write_json(self.project.root / relative, record)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.visionmcp.media-interface-ir+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO media_reconstructions("
                "id,capture_id,mode,status,record_digest,created_at"
                ") VALUES(?,?,?,?,?,?)",
                (
                    record_id,
                    capture_id,
                    "media_to_interface",
                    "CANDIDATE",
                    artifact.digest,
                    now,
                ),
            )
        return {
            **record,
            "record_digest": artifact.digest,
            "path": str(relative),
            "reused": False,
        }

    def _media_graph(self, capture_id: str) -> dict[str, Any]:
        for graph_type in (
            "ImageGraph",
            "DesktopExperienceGraph",
            "VideoNarrativeGraph",
            "CameraSequenceGraph",
        ):
            try:
                return self.query.graph(capture_id, graph_type)
            except KeyError:
                continue
        raise ValueError(
            "media-to-interface requires image, desktop, video, or camera evidence"
        )

    def _existing(self, capture_id: str, mode: str) -> dict[str, Any] | None:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT record_digest FROM media_reconstructions "
                "WHERE capture_id=? AND mode=?",
                (capture_id, mode),
            ).fetchone()
        if row is None:
            return None
        record = json.loads(
            self.artifacts.path_for(row["record_digest"]).read_text(encoding="utf-8")
        )
        return {
            **record,
            "record_digest": row["record_digest"],
            "reused": True,
        }
