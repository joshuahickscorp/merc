from __future__ import annotations

from blender_vision.core.config import discover_executable
from blender_vision.core.models import BackendCapability, BackendState


class BackendRegistry:
    """Runtime registry that never downloads model weights implicitly."""

    def capabilities(self) -> list[BackendCapability]:
        colmap = discover_executable("colmap", ["-h"])
        return [
            BackendCapability(
                name="exif",
                version="1",
                revision="builtin",
                license="Apache-2.0",
                commercial_use=True,
                redistribution="included",
                state=BackendState.AVAILABLE,
                outputs=["approximate_intrinsics", "approximate_extrinsics", "diagnostics"],
                hardware=["cpu"],
                quality_tier="initialization-only",
                precision=["float64"],
                input_limits={"media": ["image/*"], "requires_35mm_equivalent_for_focal": True},
            ),
            BackendCapability(
                name="heuristic-pinhole",
                version="1",
                revision="builtin",
                license="Apache-2.0",
                commercial_use=True,
                redistribution="included",
                state=BackendState.AVAILABLE,
                outputs=["camera_intrinsics", "camera_extrinsics", "diagnostics"],
                hardware=["cpu"],
                quality_tier="initialization-only",
                precision=["float64"],
                input_limits={"minimum_images": 1},
            ),
            BackendCapability(
                name="colmap",
                version=colmap.version or "unknown",
                revision="system",
                license="BSD-3-Clause",
                commercial_use=True,
                redistribution="external executable",
                state=BackendState.AVAILABLE if colmap.available else BackendState.UNAVAILABLE,
                outputs=[
                    "camera_intrinsics",
                    "camera_extrinsics",
                    "correspondences",
                    "sparse_points",
                ],
                hardware=["cpu", "cuda"],
                quality_tier="feature-based",
                download_source="https://github.com/colmap/colmap",
                precision=["float64", "float32"],
                input_limits={"minimum_images": 2, "formats": ["JPEG", "PNG", "TIFF"]},
            ),
            BackendCapability(
                name="turntable_fallback",
                version="1",
                revision="builtin",
                license="Apache-2.0",
                commercial_use=True,
                redistribution="included",
                state=BackendState.AVAILABLE,
                outputs=["approximate_intrinsics", "approximate_extrinsics", "diagnostics"],
                hardware=["cpu"],
                quality_tier="initialization-only",
                precision=["float64"],
                input_limits={"minimum_images": 1},
            ),
            BackendCapability(
                name="visual_hull",
                version="1",
                revision="builtin",
                license="Apache-2.0",
                commercial_use=True,
                redistribution="included",
                state=BackendState.AVAILABLE,
                outputs=[
                    "voxel_occupancy",
                    "silhouette_volume",
                    "editable_ply_mesh",
                    "governance_report",
                ],
                hardware=["cpu"],
                quality_tier="coarse-observed-topology",
                precision=["float64"],
                input_limits={
                    "minimum_reviewed_full_object_masks": 2,
                    "maximum_grid_resolution": 128,
                    "requires_complete_immutable_cameras": True,
                    "distorted_references": False,
                },
            ),
            BackendCapability(
                name="vggt-commercial",
                version="unconfigured",
                revision="unconfigured",
                license="VGGT-1B-Commercial upstream license; military use excluded",
                commercial_use=True,
                redistribution="application required; no bundled weights or silent download",
                state=BackendState.DOWNLOAD_REQUIRED,
                outputs=["cameras", "depth", "point_map", "confidence"],
                hardware=["cuda", "mps"],
                quality_tier="geometry-foundation",
                download_source="https://github.com/facebookresearch/vggt",
                precision=["bfloat16", "float32"],
                input_limits={"checkpoint": "operator-approved", "maximum_images": "hardware"},
            ),
            BackendCapability(
                name="vggt-original-research",
                version="unconfigured",
                revision="unconfigured",
                license="non-commercial upstream checkpoint terms",
                commercial_use=False,
                redistribution="no bundled weights or silent download",
                state=BackendState.RESEARCH_ONLY,
                outputs=["cameras", "depth", "point_map", "confidence"],
                hardware=["cuda", "mps"],
                quality_tier="geometry-foundation-research-only",
                download_source="https://github.com/facebookresearch/vggt",
                precision=["bfloat16", "float32"],
                input_limits={"checkpoint": "operator-approved", "maximum_images": "hardware"},
            ),
        ]

    def as_dict(self) -> list[dict[str, object]]:
        return [backend.to_dict() for backend in self.capabilities()]
