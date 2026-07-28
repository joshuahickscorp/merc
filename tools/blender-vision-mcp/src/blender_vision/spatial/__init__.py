"""VisionMCP V2 spatial evidence lane.

Governed ingestion and normalisation of depth maps, point clouds, camera
trajectories, calibration targets, measured anchors, and capture coverage.
"""

from __future__ import annotations

from blender_vision.spatial.calibration import CalibrationResult, calibrate_planar_board
from blender_vision.spatial.capture_plan import CapturePlan, ProposedView, plan_capture
from blender_vision.spatial.coverage import CoverageAtlas, CoverageReport, SurfacePatch
from blender_vision.spatial.depth import DepthMap, DepthScaleSource, UnscaledDepthError
from blender_vision.spatial.frames import (
    convert_points,
    convert_transform,
    frame_basis,
    transform_matrix,
)
from blender_vision.spatial.pointcloud import PointCloud, SimilarityTransform
from blender_vision.spatial.trajectory import CameraPose, CameraTrajectory, TrajectoryError

__all__ = [
    "CalibrationResult",
    "CameraPose",
    "CameraTrajectory",
    "CapturePlan",
    "CoverageAtlas",
    "CoverageReport",
    "DepthMap",
    "DepthScaleSource",
    "PointCloud",
    "ProposedView",
    "SimilarityTransform",
    "SurfacePatch",
    "TrajectoryError",
    "UnscaledDepthError",
    "calibrate_planar_board",
    "convert_points",
    "convert_transform",
    "frame_basis",
    "plan_capture",
    "transform_matrix",
]
