"""Tests for the V2 reconstruction ensemble."""

from __future__ import annotations

import os
from pathlib import Path

import numpy as np
import pytest

from blender_vision.core.models import BackendState
from blender_vision.reconstruction.base import (
    CameraView,
    DepthFrame,
    ReconstructionInputs,
    unavailable_candidate,
)
from blender_vision.reconstruction.colmap_sfm import (
    DENSE_UNAVAILABLE_REASON,
    ColmapSfMBackend,
)
from blender_vision.reconstruction.depth_fusion import (
    DepthFusionBackend,
    analytic_plane_depth,
)
from blender_vision.reconstruction.fusion import FusionError, fuse_candidates
from blender_vision.reconstruction.mesh_ops import (
    box_mesh,
)
from blender_vision.reconstruction.parametric import (
    ParametricBackend,
    fit_primitive,
    sample_sphere_points,
)
from blender_vision.reconstruction.portfolio import build_portfolio
from blender_vision.reconstruction.retrieval import RetrievalBackend
from blender_vision.reconstruction.visual_hull import (
    VisualHullBackend,
    synthetic_silhouette_masks,
)
from blender_vision.v2.authority import (
    AuthorityClass,
    CoordinateFrame,
    Units,
    VisibilityState,
)
from blender_vision.v2.records import ReconstructionCandidate
from blender_vision.v2.validation import verify_payload

REPO = Path(__file__).resolve().parents[1]
FIXTURES = REPO / "benchmarks" / "reconstruction" / "fixtures"
LIBRARY = FIXTURES / "archetypes"


def _look_at_blender(
    position: list[float],
    target: list[float] | None = None,
) -> np.ndarray:
    target_arr = np.asarray(target or [0.0, 0.0, 0.0], dtype=np.float64)
    pos = np.asarray(position, dtype=np.float64)
    back = pos - target_arr
    back = back / np.linalg.norm(back)
    provisional_up = (
        np.array([0.0, 1.0, 0.0]) if abs(back[2]) > 0.98 else np.array([0.0, 0.0, 1.0])
    )
    right = np.cross(provisional_up, back)
    right = right / np.linalg.norm(right)
    up = np.cross(back, right)
    up = up / np.linalg.norm(up)
    mat = np.eye(4)
    mat[:3, 0] = right
    mat[:3, 1] = up
    mat[:3, 2] = back
    mat[:3, 3] = pos
    return mat


def _orbit_cameras(count: int = 8, radius: float = 2.0, size: int = 64) -> list[CameraView]:
    cameras = []
    for i in range(count):
        angle = 2 * np.pi * i / count
        pos = [radius * np.cos(angle), radius * np.sin(angle), 0.4]
        cameras.append(
            CameraView(
                name=f"cam{i}",
                width=size,
                height=size,
                fx=size * 1.2,
                fy=size * 1.2,
                cx=size / 2,
                cy=size / 2,
                world_from_camera=_look_at_blender(pos),
            )
        )
    return cameras


def test_visual_hull_recovers_sphere_volume(tmp_path: Path) -> None:
    cameras = _orbit_cameras(12, radius=2.5, size=96)
    radius = 0.5
    masks = synthetic_silhouette_masks(
        cameras=cameras,
        solid="sphere",
        solid_params={"center": [0.0, 0.0, 0.0], "radius": radius},
    )
    inputs = ReconstructionInputs(
        target_id="sphere",
        work_dir=tmp_path / "vh_sphere",
        masks=masks,
        cameras=cameras,
        bounds_min=np.array([-0.8, -0.8, -0.8]),
        bounds_max=np.array([0.8, 0.8, 0.8]),
        parameters={"grid_resolution": 40},
        frame=CoordinateFrame(
            name="test",
            scale_authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
        ),
        input_authorities=[AuthorityClass.SENSOR_DERIVED],
    )
    candidate = VisualHullBackend().run(inputs)
    assert candidate.executed is True
    truth = (4.0 / 3.0) * np.pi * radius**3
    recovered = float(candidate.topology_state["volume"])
    # Visual hull overestimates; allow 40% relative error at this coarse grid.
    assert recovered > 0
    assert abs(recovered - truth) / truth < 0.4


def test_visual_hull_recovers_box_volume(tmp_path: Path) -> None:
    cameras = _orbit_cameras(10, radius=3.0, size=80)
    minimum = np.array([-0.4, -0.3, -0.2])
    maximum = np.array([0.4, 0.3, 0.2])
    masks = synthetic_silhouette_masks(
        cameras=cameras,
        solid="box",
        solid_params={"minimum": minimum.tolist(), "maximum": maximum.tolist()},
    )
    inputs = ReconstructionInputs(
        target_id="box",
        work_dir=tmp_path / "vh_box",
        masks=masks,
        cameras=cameras,
        bounds_min=minimum - 0.3,
        bounds_max=maximum + 0.3,
        parameters={"grid_resolution": 36},
        input_authorities=[AuthorityClass.SENSOR_DERIVED],
    )
    candidate = VisualHullBackend().run(inputs)
    assert candidate.executed is True
    truth = float(np.prod(maximum - minimum))
    recovered = float(candidate.topology_state["volume"])
    assert recovered > 0
    assert abs(recovered - truth) / truth < 0.45


def test_tsdf_fusion_reproduces_plane(tmp_path: Path) -> None:
    # OpenCV camera above z=0 plane looking down +Z world... place camera at z=1
    # looking toward origin with +Z forward in camera = -world Z.
    # Simpler: camera at (0,0,1) with world_from_camera that maps camera +Z to world -Z.
    world_from_camera = np.array(
        [
            [1.0, 0.0, 0.0, 0.0],
            [0.0, -1.0, 0.0, 0.0],
            [0.0, 0.0, -1.0, 1.0],
            [0.0, 0.0, 0.0, 1.0],
        ]
    )
    CameraView(
        name="top",
        width=48,
        height=48,
        fx=40.0,
        fy=40.0,
        cx=24.0,
        cy=24.0,
        world_from_camera=world_from_camera,
    )
    # For OpenCV convention test, build depth with camera looking +Z in world.
    # Re-define camera as identity at z=-1 looking +Z toward plane z=0.
    cam_opencv = CameraView(
        name="opencv",
        width=48,
        height=48,
        fx=40.0,
        fy=40.0,
        cx=24.0,
        cy=24.0,
        world_from_camera=np.array(
            [
                [1.0, 0.0, 0.0, 0.0],
                [0.0, 1.0, 0.0, 0.0],
                [0.0, 0.0, 1.0, -1.0],
                [0.0, 0.0, 0.0, 1.0],
            ]
        ),
    )
    depth = analytic_plane_depth(cam_opencv, plane_z=0.0)
    assert depth[24, 24] > 0.5
    frame = DepthFrame(name="plane", depth=depth, camera=cam_opencv)
    inputs = ReconstructionInputs(
        target_id="plane",
        work_dir=tmp_path / "tsdf",
        depth_frames=[frame],
        bounds_min=np.array([-0.4, -0.4, -0.15]),
        bounds_max=np.array([0.4, 0.4, 0.15]),
        parameters={"voxel_size": 0.02, "truncation": 0.06},
        frame=CoordinateFrame(name="opencv-world", scale_authority=AuthorityClass.SENSOR_DERIVED),
        input_authorities=[AuthorityClass.SENSOR_DERIVED],
    )
    candidate = DepthFusionBackend().run(inputs)
    assert candidate.executed is True
    assert candidate.topology_state["voxel_size"] == 0.02
    assert candidate.topology_state["truncation"] == 0.06
    # Surface should exist near z=0.
    from blender_vision.reconstruction.mesh_ops import read_ply_mesh

    mesh = read_ply_mesh(Path(candidate.artifacts["mesh_ply"]))
    assert len(mesh.vertices) > 10
    mean_z = float(mesh.vertices[:, 2].mean())
    assert abs(mean_z) < 0.08


def test_ransac_recovers_sphere_parameters() -> None:
    cloud = sample_sphere_points([1.0, -2.0, 0.5], 0.35, 800, noise=0.002, seed=3)
    fit = fit_primitive(
        cloud.positions, kind="sphere", iterations=500, threshold=0.01, seed=3
    )
    assert fit.kind == "sphere"
    assert fit.inlier_ratio > 0.85
    center = np.asarray(fit.parameters["center"])
    assert np.linalg.norm(center - np.array([1.0, -2.0, 0.5])) < 0.03
    assert abs(fit.parameters["radius"] - 0.35) < 0.02


def test_ransac_backend_executes(tmp_path: Path) -> None:
    cloud = sample_sphere_points([0, 0, 0], 0.2, 400, noise=0.001, seed=1)
    inputs = ReconstructionInputs(
        target_id="sphere",
        work_dir=tmp_path / "param",
        points=cloud,
        primitive_kind="sphere",
        input_authorities=[AuthorityClass.MODEL_DERIVED],
    )
    candidate = ParametricBackend().run(inputs)
    assert candidate.executed is True
    assert candidate.topology_state["primitive_kind"] == "sphere"


def test_fusion_refuses_mismatched_frames(tmp_path: Path) -> None:
    left = ReconstructionCandidate(
        candidate_id="a",
        backend="depth_fusion",
        frame=CoordinateFrame(name="blender", up_axis="+Z", forward_axis="-Y"),
        scale_authority=AuthorityClass.SENSOR_DERIVED,
        executed=True,
        artifacts={"mesh_ply": str(_write_box(tmp_path / "a.ply"))},
        authority=AuthorityClass.SENSOR_DERIVED,
    )
    right = ReconstructionCandidate(
        candidate_id="b",
        backend="retrieval",
        frame=CoordinateFrame(name="gltf", up_axis="+Y", forward_axis="-Z"),
        scale_authority=AuthorityClass.SENSOR_DERIVED,
        executed=True,
        artifacts={"mesh_ply": str(_write_box(tmp_path / "b.ply"))},
        authority=AuthorityClass.MODEL_DERIVED,
    )
    with pytest.raises(FusionError, match="coordinate_frame"):
        fuse_candidates(
            left,
            right,
            target_id_left="rack",
            target_id_right="rack",
            mode="retrieved_plus_observed_face",
            work_dir=tmp_path / "fuse",
        )


def test_fusion_refuses_mismatched_units(tmp_path: Path) -> None:
    left = ReconstructionCandidate(
        candidate_id="a",
        backend="depth_fusion",
        frame=CoordinateFrame(name="m", units=Units.METRE),
        scale_authority=AuthorityClass.SENSOR_DERIVED,
        executed=True,
        artifacts={"mesh_ply": str(_write_box(tmp_path / "a.ply"))},
        authority=AuthorityClass.SENSOR_DERIVED,
    )
    right = ReconstructionCandidate(
        candidate_id="b",
        backend="parametric",
        frame=CoordinateFrame(name="mm", units=Units.MILLIMETRE),
        scale_authority=AuthorityClass.MEASURED,
        executed=True,
        artifacts={"mesh_ply": str(_write_box(tmp_path / "b.ply"))},
        authority=AuthorityClass.MEASURED,
    )
    with pytest.raises(FusionError, match="units"):
        fuse_candidates(
            left,
            right,
            target_id_left="obj",
            target_id_right="obj",
            mode="depth_plus_measured_dimensions",
            work_dir=tmp_path / "fuse",
        )


def test_fusion_refuses_mismatched_scale_authority(tmp_path: Path) -> None:
    left = ReconstructionCandidate(
        candidate_id="a",
        backend="depth_fusion",
        frame=CoordinateFrame(name="m", units=Units.METRE),
        scale_authority=AuthorityClass.UNRESOLVED,
        executed=True,
        artifacts={"mesh_ply": str(_write_box(tmp_path / "a.ply"))},
        authority=AuthorityClass.SENSOR_DERIVED,
    )
    right = ReconstructionCandidate(
        candidate_id="b",
        backend="parametric",
        frame=CoordinateFrame(name="m2", units=Units.METRE),
        scale_authority=AuthorityClass.UNRESOLVED,
        executed=True,
        artifacts={"mesh_ply": str(_write_box(tmp_path / "b.ply"))},
        authority=AuthorityClass.MODEL_DERIVED,
        coverage={"dimensions": {"height": 0.5}},
    )
    with pytest.raises(FusionError, match="scale_authority"):
        fuse_candidates(
            left,
            right,
            target_id_left="obj",
            target_id_right="obj",
            mode="depth_plus_measured_dimensions",
            work_dir=tmp_path / "fuse",
        )


def test_fusion_refuses_mismatched_target_identity(tmp_path: Path) -> None:
    left = ReconstructionCandidate(
        candidate_id="a",
        backend="retrieval",
        frame=CoordinateFrame(name="m"),
        scale_authority=AuthorityClass.MODEL_DERIVED,
        executed=True,
        artifacts={"mesh_ply": str(_write_box(tmp_path / "a.ply"))},
        authority=AuthorityClass.MODEL_DERIVED,
        topology_state={"visibility_state": VisibilityState.RETRIEVED_MODEL.value},
    )
    right = ReconstructionCandidate(
        candidate_id="b",
        backend="visual_hull",
        frame=CoordinateFrame(name="m2"),
        scale_authority=AuthorityClass.SENSOR_DERIVED,
        executed=True,
        artifacts={"mesh_ply": str(_write_box(tmp_path / "b.ply"))},
        authority=AuthorityClass.SENSOR_DERIVED,
    )
    with pytest.raises(FusionError, match="target_identity"):
        fuse_candidates(
            left,
            right,
            target_id_left="rack",
            target_id_right="calibration",
            mode="retrieved_plus_observed_face",
            work_dir=tmp_path / "fuse",
        )


def test_retrieval_tags_retrieved_model(tmp_path: Path) -> None:
    inputs = ReconstructionInputs(
        target_id="datacentre-rack-module",
        work_dir=tmp_path / "retrieval",
        library_dir=LIBRARY,
        archetype_id="rack_module",
        adaptation_scale=(1.0, 1.0, 1.0),
        input_authorities=[AuthorityClass.MODEL_DERIVED],
    )
    candidate = RetrievalBackend().run(inputs)
    assert candidate.executed is True
    assert candidate.topology_state["visibility_state"] == VisibilityState.RETRIEVED_MODEL.value
    assert candidate.licensing == "PROCEDURAL_OWNED"
    assert candidate.authority is AuthorityClass.MODEL_DERIVED


def test_retrieval_refuses_unreviewed_rights(tmp_path: Path) -> None:
    inputs = ReconstructionInputs(
        target_id="x",
        work_dir=tmp_path / "bad",
        library_dir=LIBRARY,
        archetype_id="unreviewed_asset",
    )
    candidate = RetrievalBackend().run(inputs)
    assert candidate.executed is False
    assert "unreviewed" in candidate.execution_log.lower() or any(
        "unreviewed" in m.lower() for m in candidate.failure_modes
    )


def test_portfolio_round_trips_verify_payload(tmp_path: Path) -> None:
    cameras = _orbit_cameras(6, radius=2.0, size=48)
    masks = synthetic_silhouette_masks(
        cameras=cameras,
        solid="box",
        solid_params={
            "minimum": [-0.2, -0.2, -0.2],
            "maximum": [0.2, 0.2, 0.2],
        },
    )
    base = ReconstructionInputs(
        target_id="box",
        work_dir=tmp_path / "portfolio",
        masks=masks,
        cameras=cameras,
        bounds_min=np.array([-0.5, -0.5, -0.5]),
        bounds_max=np.array([0.5, 0.5, 0.5]),
        parameters={"grid_resolution": 24},
        points=sample_sphere_points([0, 0, 0], 0.2, 200, seed=0),
        primitive_kind="sphere",
        library_dir=LIBRARY,
        archetype_id="box_unit",
        input_authorities=[AuthorityClass.SENSOR_DERIVED],
    )
    portfolio = build_portfolio(
        target_id="box",
        backends=[VisualHullBackend(), ParametricBackend(), RetrievalBackend()],
        inputs_for=base,
    )
    payload = portfolio.to_dict()
    restored = verify_payload(payload)
    assert restored.digest == portfolio.digest
    assert restored.RECORD_KIND == "v2.reconstruction-portfolio"
    assert len(restored.candidates) == 3


def test_unexecuted_backend_cannot_report_executed_true(tmp_path: Path) -> None:
    inputs = ReconstructionInputs(target_id="x", work_dir=tmp_path / "none")
    candidate = unavailable_candidate(
        backend="colmap_sfm",
        reason="no images",
        inputs=inputs,
    )
    assert candidate.executed is False
    # Manually forging executed=True without artifacts is cleared by portfolio.
    forged = ReconstructionCandidate(
        candidate_id="forged",
        backend="ghost",
        executed=True,
        artifacts={},
        topology_state={},
        authority=AuthorityClass.HYPOTHETICAL,
    )

    class GhostBackend:
        name = "ghost"

        def availability(self):
            from blender_vision.reconstruction.base import BackendAvailability

            return BackendAvailability(state=BackendState.AVAILABLE, reason="ok")

        def run(self, _inputs):
            return forged

    portfolio = build_portfolio(
        target_id="x",
        backends=[GhostBackend()],
        inputs_for=inputs,
    )
    assert portfolio.candidates[0].executed is False


def test_colmap_dense_reports_cuda_blocker() -> None:
    backend = ColmapSfMBackend()
    dense = backend.dense_availability()
    assert dense.state is BackendState.UNAVAILABLE
    assert dense.reason == DENSE_UNAVAILABLE_REASON


def test_fusion_accepts_compatible_retrieved_and_observed(tmp_path: Path) -> None:
    left = ReconstructionCandidate(
        candidate_id="ret",
        backend="retrieval",
        frame=CoordinateFrame(name="m", units=Units.METRE),
        scale_authority=AuthorityClass.MODEL_DERIVED,
        executed=True,
        artifacts={"mesh_ply": str(_write_box(tmp_path / "ret.ply", scale=0.5))},
        authority=AuthorityClass.MODEL_DERIVED,
        topology_state={"visibility_state": VisibilityState.RETRIEVED_MODEL.value},
    )
    right = ReconstructionCandidate(
        candidate_id="obs",
        backend="visual_hull",
        frame=CoordinateFrame(name="m2", units=Units.METRE),
        scale_authority=AuthorityClass.SENSOR_DERIVED,
        executed=True,
        artifacts={"mesh_ply": str(_write_box(tmp_path / "obs.ply", scale=0.2))},
        authority=AuthorityClass.SENSOR_DERIVED,
    )
    result = fuse_candidates(
        left,
        right,
        target_id_left="rack",
        target_id_right="rack",
        mode="retrieved_plus_observed_face",
        work_dir=tmp_path / "fuse_ok",
    )
    assert result.mode == "retrieved_plus_observed_face"
    assert any(
        e["visibility_state"] == VisibilityState.RETRIEVED_MODEL.value
        for e in result.hidden_surface_ledger
    )
    assert Path(result.artifacts["mesh_ply"]).is_file()


def _write_box(path: Path, scale: float = 0.1) -> Path:
    from blender_vision.reconstruction.mesh_ops import write_ply_mesh

    mesh = box_mesh([-scale] * 3, [scale] * 3)
    return write_ply_mesh(path, mesh)


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_COLMAP_TESTS") != "1",
    reason="set BVMCP_RUN_COLMAP_TESTS=1 to run real COLMAP",
)
def test_colmap_sparse_on_synthetic_views(tmp_path: Path) -> None:
    """Render a textured box with numpy and run real COLMAP sparse SfM."""
    image_dir = tmp_path / "images"
    image_dir.mkdir()
    _write_textured_turntable(image_dir, count=16)
    inputs = ReconstructionInputs(
        target_id="colmap-box",
        work_dir=tmp_path / "colmap",
        image_dir=image_dir,
        input_authorities=[AuthorityClass.SENSOR_DERIVED],
    )
    candidate = ColmapSfMBackend().run(inputs)
    assert candidate.executed is True, candidate.execution_log
    assert candidate.coverage["registered_images"] >= 2
    assert candidate.coverage["num_points3d"] >= 1
    print(
        "COLMAP registered_images=",
        candidate.coverage["registered_images"],
        "mean_reprojection_error_px=",
        candidate.coverage["mean_reprojection_error_px"],
    )


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BLENDER_TESTS") != "1",
    reason="set BVMCP_RUN_BLENDER_TESTS=1 to run Blender-backed ensemble pieces",
)
def test_blender_available_for_ensemble() -> None:
    blender = Path("/Applications/Blender.app/Contents/MacOS/Blender")
    assert blender.is_file()


def _write_textured_turntable(image_dir: Path, count: int = 16) -> None:
    """Synthesize multiview-like images of a high-contrast cube for COLMAP."""
    try:
        import cv2
    except ImportError:  # pragma: no cover
        pytest.skip("opencv required")
    size = 320
    for i in range(count):
        angle = 2 * np.pi * i / count
        img = np.full((size, size, 3), 30, dtype=np.uint8)
        # Project a cube with simple orthographic-ish shading that changes per view.
        cx, cy = size // 2, size // 2
        half = int(70 + 10 * np.sin(angle * 2))
        offset_x = int(40 * np.cos(angle))
        offset_y = int(20 * np.sin(angle))
        pts = np.array(
            [
                [cx - half + offset_x, cy - half + offset_y],
                [cx + half + offset_x, cy - half + offset_y],
                [cx + half + offset_x, cy + half + offset_y],
                [cx - half + offset_x, cy + half + offset_y],
            ],
            dtype=np.int32,
        )
        color = (
            int(80 + 100 * (0.5 + 0.5 * np.cos(angle))),
            int(80 + 100 * (0.5 + 0.5 * np.sin(angle))),
            int(80 + 80 * (0.5 + 0.5 * np.cos(angle * 1.5))),
        )
        cv2.fillConvexPoly(img, pts, color)
        # High-frequency texture for SIFT.
        for y in range(0, size, 8):
            for x in range(0, size, 8):
                if (x // 8 + y // 8 + i) % 2 == 0:
                    img[y : y + 4, x : x + 4] = (
                        (int(img[y, x, 0]) + 40) % 255,
                        (int(img[y, x, 1]) + 60) % 255,
                        (int(img[y, x, 2]) + 20) % 255,
                    )
        cv2.putText(
            img,
            f"V{i:02d}",
            (20, 40),
            cv2.FONT_HERSHEY_SIMPLEX,
            1.0,
            (255, 255, 255),
            2,
            cv2.LINE_AA,
        )
        cv2.imwrite(str(image_dir / f"view_{i:03d}.png"), img)
