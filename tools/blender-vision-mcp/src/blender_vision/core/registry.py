from __future__ import annotations

import json
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any

from blender_vision.core.config import discover_blender, discover_executable
from blender_vision.core.models import BackendState


@dataclass(slots=True)
class BackendCapability:
    name: str
    version: str | None
    revision: str | None
    license: str
    commercial_use: bool | None
    redistribution: bool | None
    state: BackendState
    outputs: list[str]
    hardware: list[str]
    path: str | None = None
    notes: str | None = None


def builtin_registry() -> list[BackendCapability]:
    blender = discover_blender()
    colmap = discover_executable("colmap", ["-h"])
    return [
        BackendCapability(
            name="blender",
            version=blender.version,
            revision=None,
            license="GPL-3.0-or-later",
            commercial_use=True,
            redistribution=True,
            state=BackendState.AVAILABLE if blender.available else BackendState.UNAVAILABLE,
            outputs=["scene_inventory", "render_passes", "blend", "glb"],
            hardware=["cpu", "metal", "cuda", "optix"],
            path=blender.path,
        ),
        BackendCapability(
            name="colmap",
            version=colmap.version,
            revision=None,
            license="BSD-3-Clause",
            commercial_use=True,
            redistribution=True,
            state=BackendState.AVAILABLE if colmap.available else BackendState.UNAVAILABLE,
            outputs=["intrinsics", "extrinsics", "sparse_point_cloud", "correspondences"],
            hardware=["cpu", "cuda"],
            path=colmap.path,
        ),
        BackendCapability(
            name="turntable_fallback",
            version="1",
            revision=None,
            license="Apache-2.0",
            commercial_use=True,
            redistribution=True,
            state=BackendState.AVAILABLE,
            outputs=["approximate_intrinsics", "approximate_extrinsics"],
            hardware=["cpu"],
            notes="Explicitly non-metric fallback; never accepted as feature-based calibration.",
        ),
        BackendCapability(
            name="vggt",
            version=None,
            revision=None,
            license="CHECKPOINT_DEPENDENT",
            commercial_use=None,
            redistribution=None,
            state=BackendState.DOWNLOAD_REQUIRED,
            outputs=["intrinsics", "extrinsics", "depth", "point_map", "confidence"],
            hardware=["cuda", "mps"],
            notes="No checkpoint is downloaded without explicit license approval.",
        ),
    ]


def registry_report(model_licenses: Path | None = None) -> dict[str, Any]:
    models: list[dict[str, Any]] = []
    if model_licenses and model_licenses.is_file():
        models = json.loads(model_licenses.read_text(encoding="utf-8")).get("models", [])
    return {"backends": [asdict(backend) for backend in builtin_registry()], "models": models}
