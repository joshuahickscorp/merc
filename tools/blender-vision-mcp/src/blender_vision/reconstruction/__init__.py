"""V2 reconstruction ensemble: multiple independent geometry candidates.

This package never trusts a single reconstruction method. Backends produce
candidates; portfolio/compare measure them honestly; fusion only merges
compatible geometry and refuses the rest.
"""

from __future__ import annotations

from blender_vision.reconstruction.base import (
    BackendAvailability,
    CameraView,
    DepthFrame,
    MeshGeometry,
    PointCloud,
    ReconstructionBackend,
    ReconstructionInputs,
    unavailable_candidate,
)
from blender_vision.reconstruction.browser_runtime import BrowserRuntimeBackend
from blender_vision.reconstruction.colmap_sfm import ColmapSfMBackend
from blender_vision.reconstruction.compare import compare_candidates
from blender_vision.reconstruction.depth_fusion import DepthFusionBackend
from blender_vision.reconstruction.fusion import FusionError, fuse_candidates
from blender_vision.reconstruction.parametric import ParametricBackend
from blender_vision.reconstruction.point_representation import PointRepresentationBackend
from blender_vision.reconstruction.portfolio import build_portfolio
from blender_vision.reconstruction.retrieval import RetrievalBackend
from blender_vision.reconstruction.visual_hull import VisualHullBackend

__all__ = [
    "BackendAvailability",
    "BrowserRuntimeBackend",
    "CameraView",
    "ColmapSfMBackend",
    "DepthFrame",
    "DepthFusionBackend",
    "FusionError",
    "MeshGeometry",
    "ParametricBackend",
    "PointCloud",
    "PointRepresentationBackend",
    "ReconstructionBackend",
    "ReconstructionInputs",
    "RetrievalBackend",
    "VisualHullBackend",
    "build_portfolio",
    "compare_candidates",
    "fuse_candidates",
    "unavailable_candidate",
]
