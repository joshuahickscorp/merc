"""Retopology, UV generation, and LOD for organic surfaces.

Everything here runs through real headless Blender. The measurements returned
are read back from the generated mesh, not predicted from the parameters, so a
retopology that silently failed shows up as a failed measurement rather than a
confident number.
"""

from __future__ import annotations

import json
from dataclasses import asdict, dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import Any

from blender_vision.blender.v2_executor import V2BlenderExecutor

BUILD_SCRIPTS = Path(__file__).resolve().parent / "build_scripts"


class RemeshMode(StrEnum):
    QUAD = "quad"
    VOXEL = "voxel"


@dataclass(slots=True)
class TopologyReport:
    """What the mesh actually is after an operation."""

    vertices: int
    edges: int
    faces: int
    triangles: int
    quads: int
    ngons: int
    non_manifold_edges: int
    boundary_edges: int
    genus_estimate: int
    is_watertight: bool
    surface_area_m2: float
    volume_m3: float
    bounds_m: list[list[float]]

    @property
    def quad_fraction(self) -> float:
        total = self.triangles + self.quads + self.ngons
        return self.quads / total if total else 0.0

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        value["quad_fraction"] = self.quad_fraction
        return value

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> TopologyReport:
        known = {"quad_fraction"}
        return cls(**{k: v for k, v in payload.items() if k not in known})


@dataclass(slots=True)
class UVReport:
    """UV quality, measured from the generated layout."""

    island_count: int
    packing_efficiency: float
    max_area_distortion: float
    mean_area_distortion: float
    max_angle_distortion_deg: float
    overlapping_faces: int
    texel_density_variance: float
    p99_angle_distortion_deg: float = 0.0
    p95_angle_distortion_deg: float = 0.0
    median_angle_distortion_deg: float = 0.0
    angle_corner_count: int = 0
    degenerate_corner_count: int = 0
    corners_over_70deg_fraction: float = 0.0

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> UVReport:
        return cls(**payload)


@dataclass(slots=True)
class LODLevel:
    name: str
    ratio: float
    triangles: int
    silhouette_iou: float
    hausdorff_m: float
    retained_parts: list[str] = field(default_factory=list)
    lost_parts: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


class TopologyService:
    """Runs retopology, unwrapping and LOD generation in real Blender."""

    def __init__(self, executor: V2BlenderExecutor | None = None) -> None:
        self.executor = executor or V2BlenderExecutor()

    def process(
        self,
        source_blend: Path,
        object_name: str,
        *,
        output_dir: Path,
        remesh: dict[str, Any] | None = None,
        unwrap: dict[str, Any] | None = None,
        lods: list[dict[str, Any]] | None = None,
        export_glb: bool = True,
    ) -> dict[str, Any]:
        output_dir.mkdir(parents=True, exist_ok=True)
        report_path = output_dir / f"{object_name}-topology.json"
        config = {
            "source_blend": str(source_blend.resolve()),
            "object": object_name,
            "remesh": remesh,
            "unwrap": unwrap,
            "lods": lods or [],
            "report": str(report_path),
            "output_blend": str(output_dir / f"{object_name}-retopo.blend"),
            "output_glb": str(output_dir / f"{object_name}.glb") if export_glb else None,
        }
        self.executor.run(
            BUILD_SCRIPTS / "topology_pass.py",
            config,
            expect_marker="V2_TOPOLOGY_OK",
            timeout_seconds=2400,
        )
        payload = json.loads(report_path.read_text(encoding="utf-8"))
        return {
            "source": TopologyReport.from_dict(payload["source"]),
            "retopologized": (
                TopologyReport.from_dict(payload["retopologized"])
                if "retopologized" in payload
                else None
            ),
            "uv": UVReport.from_dict(payload["uv"]) if payload.get("uv") else None,
            "lods": [
                LODLevel(
                    name=item["name"],
                    ratio=item["ratio"],
                    triangles=item["triangles"],
                    silhouette_iou=item["silhouette_iou"],
                    hausdorff_m=item["hausdorff_m"],
                    retained_parts=item["retained_parts"],
                    lost_parts=item["lost_parts"],
                )
                for item in payload.get("lods", [])
            ],
            "report_path": report_path,
            "blend_path": Path(config["output_blend"]),
            "glb_path": Path(config["output_glb"]) if config["output_glb"] else None,
        }


def lod_identity_violations(
    lods: list[LODLevel],
    *,
    minimum_silhouette_iou: float = 0.9,
) -> list[str]:
    """LOD names whose silhouette drifted far enough to be a different object."""
    return [item.name for item in lods if item.silhouette_iou < minimum_silhouette_iou]
