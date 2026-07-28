"""Unit tests for the V2 spatial evidence lane."""

from __future__ import annotations

import os
from pathlib import Path

import numpy as np
import pytest

from blender_vision.spatial.calibration import (
    calibrate_planar_board,
    synthetic_checkerboard_image,
)
from blender_vision.spatial.capture_plan import plan_capture
from blender_vision.spatial.coverage import CoverageAtlas
from blender_vision.spatial.depth import DepthKind, DepthMap, DepthScaleSource, UnscaledDepthError
from blender_vision.spatial.frames import convert_points, transform_matrix
from blender_vision.spatial.pointcloud import PointCloud, umeyama
from blender_vision.spatial.trajectory import (
    CameraPose,
    CameraTrajectory,
    TrajectoryError,
    look_at_matrix,
)
from blender_vision.v2.authority import (
    BLENDER_WORLD,
    GLTF_WORLD,
    OPENCV_CAMERA,
    AuthorityClass,
    AuthorityPromotionError,
    Units,
    VisibilityState,
)

# --------------------------------------------------------------------------- frames


def test_frame_round_trip_exactness() -> None:
    rng = np.random.default_rng(0)
    points = rng.normal(size=(50, 3))
    frames = [BLENDER_WORLD, GLTF_WORLD, OPENCV_CAMERA]
    for source in frames:
        for target in frames:
            matrix = transform_matrix(source, target)
            back = transform_matrix(target, source)
            round_trip = back @ matrix
            assert np.max(np.abs(round_trip - np.eye(3))) < 1e-9
            converted = convert_points(points, source, target)
            restored = convert_points(converted, target, source)
            assert np.max(np.abs(restored - points)) < 1e-9


def test_blender_gltf_known_mapping() -> None:
    # Blender (x, y, z) → glTF (x, z, -y)
    p = np.array([1.0, 2.0, 3.0])
    q = convert_points(p, BLENDER_WORLD, GLTF_WORLD)
    assert np.allclose(q, [1.0, 3.0, -2.0], atol=1e-12)


# --------------------------------------------------------------------------- PLY


def test_ply_binary_and_ascii_round_trip(tmp_path: Path) -> None:
    rng = np.random.default_rng(1)
    positions = rng.normal(size=(40, 3))
    normals = rng.normal(size=(40, 3))
    normals /= np.linalg.norm(normals, axis=1, keepdims=True)
    colors = rng.random(size=(40, 3))
    cloud = PointCloud(
        positions=positions,
        normals=normals,
        colors=colors,
        authority=AuthorityClass.SENSOR_DERIVED,
    )
    ascii_path = tmp_path / "cloud_ascii.ply"
    binary_path = tmp_path / "cloud_binary.ply"
    cloud.write_ply(ascii_path, binary=False)
    cloud.write_ply(binary_path, binary=True)
    ascii_cloud = PointCloud.read_ply(ascii_path)
    binary_cloud = PointCloud.read_ply(binary_path)
    assert np.allclose(ascii_cloud.positions, positions, atol=1e-6)
    assert np.allclose(binary_cloud.positions, positions, atol=1e-6)
    assert ascii_cloud.normals is not None and binary_cloud.normals is not None
    assert np.allclose(ascii_cloud.normals, normals, atol=1e-5)
    assert np.allclose(binary_cloud.normals, normals, atol=1e-5)
    assert ascii_cloud.colors is not None and binary_cloud.colors is not None
    # 8-bit colour quantisation.
    assert np.allclose(ascii_cloud.colors, colors, atol=1.0 / 255.0 + 1e-6)
    assert np.allclose(binary_cloud.colors, colors, atol=1.0 / 255.0 + 1e-6)


# --------------------------------------------------------------------------- Umeyama


def test_umeyama_recovers_known_similarity() -> None:
    rng = np.random.default_rng(2)
    source = rng.normal(size=(30, 3))
    # Known similarity: scale 2.5, 90° about Z, translation (1, -2, 0.5)
    angle = np.pi / 2
    rotation = np.array(
        [
            [np.cos(angle), -np.sin(angle), 0.0],
            [np.sin(angle), np.cos(angle), 0.0],
            [0.0, 0.0, 1.0],
        ]
    )
    scale = 2.5
    translation = np.array([1.0, -2.0, 0.5])
    target = scale * (source @ rotation.T) + translation
    result = umeyama(source, target, with_scale=True)
    assert abs(result.scale - scale) < 1e-9
    assert np.allclose(result.rotation, rotation, atol=1e-9)
    assert np.allclose(result.translation, translation, atol=1e-9)
    assert result.rmse < 1e-12

    cloud_a = PointCloud(positions=source, frame=BLENDER_WORLD)
    cloud_b = PointCloud(positions=target, frame=BLENDER_WORLD)
    aligned = cloud_a.umeyama_align(cloud_b, with_scale=True)
    assert aligned.rmse < 1e-12


# --------------------------------------------------------------------------- depth


def test_depth_back_project_analytic_plane() -> None:
    width, height = 64, 48
    fx = fy = 100.0
    cx, cy = width / 2.0, height / 2.0
    # Plane Z = 2 metres, parallel to image plane.
    depth = np.full((height, width), 2.0, dtype=np.float32)
    mask = np.ones((height, width), dtype=bool)
    depth_map = DepthMap(
        depth=depth,
        mask=mask,
        intrinsics={"fx": fx, "fy": fy, "cx": cx, "cy": cy},
        authority=AuthorityClass.SENSOR_DERIVED,
        kind=DepthKind.METRIC,
    )
    points = depth_map.back_project()
    assert points.shape[0] == width * height
    assert np.allclose(points[:, 2], 2.0, atol=1e-6)
    # Centre pixel maps to (0, 0, 2); np.where order is row-major by y then x.
    ys, xs = np.where(mask)
    centre_idx = np.where((ys == height // 2) & (xs == width // 2))[0][0]
    assert np.allclose(points[centre_idx], [0.0, 0.0, 2.0], atol=1e-6)
    # Corner pixel (0,0): X = (0-cx)*Z/fx
    corner_idx = np.where((ys == 0) & (xs == 0))[0][0]
    expected_x = (0 - cx) * 2.0 / fx
    expected_y = (0 - cy) * 2.0 / fy
    assert np.allclose(points[corner_idx], [expected_x, expected_y, 2.0], atol=1e-6)


def test_unscaled_depth_cannot_promote_to_measured() -> None:
    depth = np.ones((8, 8), dtype=np.float32)
    mask = np.ones((8, 8), dtype=bool)
    relative = DepthMap(
        depth=depth,
        mask=mask,
        intrinsics={"fx": 10.0, "fy": 10.0, "cx": 4.0, "cy": 4.0},
        authority=AuthorityClass.SENSOR_DERIVED,
        kind=DepthKind.RELATIVE,
        units=Units.UNITLESS,
    )
    with pytest.raises(UnscaledDepthError):
        relative.back_project()
    # Even after a weak scale, derive() must not yield MEASURED.
    scaled = relative.to_metric(
        DepthScaleSource(
            kind="guess",
            scale_m=1.0,
            authority=AuthorityClass.HYPOTHETICAL,
            description="unit guess",
        )
    )
    assert scaled.authority is not AuthorityClass.MEASURED
    assert scaled.authority is AuthorityClass.HYPOTHETICAL
    # Direct MEASURED claim on relative depth is refused at seal time.
    bad = DepthMap(
        depth=depth,
        mask=mask,
        intrinsics={"fx": 10.0, "fy": 10.0, "cx": 4.0, "cy": 4.0},
        authority=AuthorityClass.MEASURED,
        kind=DepthKind.RELATIVE,
    )
    with pytest.raises(UnscaledDepthError):
        bad.seal_observation_bundle(target_id="t")
    # promote() from SENSOR_DERIVED to MEASURED without reviewer must fail.
    metric = DepthMap(
        depth=depth,
        mask=mask,
        intrinsics={"fx": 10.0, "fy": 10.0, "cx": 4.0, "cy": 4.0},
        authority=AuthorityClass.SENSOR_DERIVED,
        kind=DepthKind.METRIC,
    )
    bundle = metric.seal_observation_bundle(target_id="t")
    with pytest.raises(AuthorityPromotionError):
        bundle.promote(AuthorityClass.MEASURED, reviewer="", reason="")


def test_depth_npy_and_pfm_round_trip(tmp_path: Path) -> None:
    depth = np.linspace(0.5, 3.0, 16 * 12, dtype=np.float32).reshape(12, 16)
    npy_path = tmp_path / "depth.npy"
    np.save(npy_path, depth)
    loaded = DepthMap.from_npy(
        npy_path,
        intrinsics={"fx": 50.0, "fy": 50.0, "cx": 8.0, "cy": 6.0},
    )
    assert loaded.depth.shape == (12, 16)
    assert np.allclose(loaded.depth, depth)

    # Minimal grayscale PFM (top-to-bottom after flip).
    pfm_path = tmp_path / "depth.pfm"
    with pfm_path.open("wb") as stream:
        stream.write(b"Pf\n16 12\n-1.0\n")
        # bottom-to-top storage: write flipped
        stream.write(np.flipud(depth).astype("<f4").tobytes())
    pfm = DepthMap.from_pfm(
        pfm_path,
        intrinsics={"fx": 50.0, "fy": 50.0, "cx": 8.0, "cy": 6.0},
    )
    assert np.allclose(pfm.depth, depth, atol=1e-6)


# --------------------------------------------------------------------------- trajectory


def test_trajectory_rejects_non_orthonormal_rotation() -> None:
    bad_rot = np.array(
        [
            [1.0, 0.1, 0.0],
            [0.0, 1.0, 0.0],
            [0.0, 0.0, 1.0],
        ]
    )
    matrix = np.eye(4)
    matrix[:3, :3] = bad_rot
    traj = CameraTrajectory(
        poses=[CameraPose(timestamp=0.0, world_from_camera=matrix)],
    )
    with pytest.raises(TrajectoryError, match="orthonormal"):
        traj.validate()


def test_trajectory_rejects_non_monotonic_timestamp() -> None:
    a = look_at_matrix(np.array([1.0, 0.0, 0.0]), np.array([0.0, 0.0, 0.0]))
    b = look_at_matrix(np.array([0.0, 1.0, 0.0]), np.array([0.0, 0.0, 0.0]))
    traj = CameraTrajectory(
        poses=[
            CameraPose(timestamp=1.0, world_from_camera=a),
            CameraPose(timestamp=0.5, world_from_camera=b),
        ]
    )
    with pytest.raises(TrajectoryError, match="monotonic"):
        traj.validate()


def test_trajectory_arc_length_and_resample() -> None:
    # Straight line along +X of length 3.
    poses = []
    for i, x in enumerate([0.0, 1.0, 2.0, 3.0]):
        matrix = np.eye(4)
        matrix[0, 3] = x
        poses.append(CameraPose(timestamp=float(i), world_from_camera=matrix))
    traj = CameraTrajectory(poses=poses)
    traj.validate()
    assert abs(traj.arc_length() - 3.0) < 1e-12
    resampled = traj.resample(7)
    assert len(resampled) == 7
    assert abs(resampled.arc_length() - 3.0) < 1e-9


# --------------------------------------------------------------------------- coverage


def test_coverage_never_observed_unseen_hemisphere() -> None:
    atlas = CoverageAtlas(covered_hit_threshold=1, partial_hit_threshold=1)
    patches = atlas.sample_sphere_patches(
        np.array([0.0, 0.0, 0.0]),
        1.0,
        n_lat=10,
        n_lon=20,
    )
    # Cameras only above the equator looking at the origin.
    cameras = []
    for angle in np.linspace(0, 2 * np.pi, 8, endpoint=False):
        cameras.append(
            {
                "label": f"top-{angle:.2f}",
                "position": [2.0 * np.cos(angle), 2.0 * np.sin(angle), 1.5],
                "target": [0.0, 0.0, 0.0],
            }
        )
    report = atlas.evaluate(patches, cameras)
    assert report.never_observed_fraction > 0.2
    # Every patch on the lower hemisphere (normal z < -0.3) must be NEVER_OBSERVED.
    lower = [p for p in report.patches if p.normal[2] < -0.3]
    assert lower, "expected lower-hemisphere patches"
    for patch in lower:
        assert patch.visibility is VisibilityState.NEVER_OBSERVED, (
            f"{patch.patch_id} normal={patch.normal} vis={patch.visibility}"
        )
    # Upper patches facing cameras should not all be never-observed.
    upper = [p for p in report.patches if p.normal[2] > 0.3]
    observed_upper = [
        p
        for p in upper
        if p.visibility
        in {VisibilityState.DIRECTLY_VISIBLE, VisibilityState.PARTIALLY_VISIBLE}
    ]
    assert len(observed_upper) > 0


def test_coverage_box_underside_never_observed() -> None:
    atlas = CoverageAtlas(covered_hit_threshold=1, partial_hit_threshold=1)
    patches = atlas.sample_box_patches(
        np.array([-0.5, -0.5, 0.0]),
        np.array([0.5, 0.5, 1.0]),
        resolution=4,
    )
    # Cameras above, looking down at the box centre.
    cameras = [
        {
            "label": "above",
            "position": [0.0, 0.0, 3.0],
            "target": [0.0, 0.0, 0.5],
        },
        {
            "label": "side",
            "position": [2.0, 0.0, 1.5],
            "target": [0.0, 0.0, 0.5],
        },
    ]
    report = atlas.evaluate(patches, cameras)
    underside = [p for p in report.patches if p.patch_id.startswith("face4-")]
    # face4 is -Z in sample_box_patches ordering: faces 0..5 = -X +X -Y +Y -Z +Z
    assert underside
    for patch in underside:
        assert patch.visibility is VisibilityState.NEVER_OBSERVED


# --------------------------------------------------------------------------- capture plan


def test_plan_capture_greedy_gain() -> None:
    target = {
        "center": [0.0, 0.0, 0.0],
        "radius": 1.0,
        "look_at": [0.0, 0.0, 0.0],
    }
    plan = plan_capture(target, existing_views=[], budget=4, n_candidates=16, resolution=5)
    assert len(plan.proposed_views) <= 4
    assert plan.proposed_views, "expected at least one proposed view"
    # Marginal gains should be non-increasing in a greedy set-cover sense
    # (not strictly required mathematically with incidence thresholds, but
    # cumulative coverage must be non-decreasing).
    fractions = [v.cumulative_covered_fraction for v in plan.proposed_views]
    assert all(fractions[i] <= fractions[i + 1] + 1e-12 for i in range(len(fractions) - 1))
    assert plan.final_covered_fraction >= plan.existing_covered_fraction
    bundle = plan.seal_observation_bundle(target_id="sphere")
    assert bundle.digest
    bundle.verify()


# ------------------------------------------------------------------ calibration


def test_calibrate_planar_board_synthetic(tmp_path: Path) -> None:
    import cv2

    board = (6, 5)
    square_px = 40
    base = synthetic_checkerboard_image(
        width=480,
        height=360,
        board_size=board,
        square_px=square_px,
        origin=(40, 30),
    )
    images = []
    # Perspective warps give non-degenerate multi-view geometry.
    warps = [
        np.eye(3, dtype=np.float64),
        np.array([[1.05, 0.02, -5.0], [0.01, 0.98, 3.0], [1e-4, 0.0, 1.0]]),
        np.array([[0.97, -0.03, 8.0], [0.02, 1.04, -4.0], [0.0, 1.5e-4, 1.0]]),
        np.array([[1.02, 0.04, -3.0], [-0.02, 0.96, 6.0], [1.2e-4, 5e-5, 1.0]]),
        np.array([[0.95, 0.01, 10.0], [0.03, 1.03, -2.0], [-1e-4, 1e-4, 1.0]]),
        np.array([[1.08, -0.01, -8.0], [-0.01, 0.94, 5.0], [8e-5, -5e-5, 1.0]]),
        np.array([[0.99, 0.05, 2.0], [-0.04, 1.01, -7.0], [5e-5, 1.2e-4, 1.0]]),
        np.array([[1.03, -0.04, -6.0], [0.04, 0.99, 4.0], [-8e-5, 8e-5, 1.0]]),
    ]
    h, w = base.shape
    for index, matrix in enumerate(warps):
        warped = cv2.warpPerspective(base, matrix, (w, h), borderValue=200)
        path = tmp_path / f"board_{index:02d}.png"
        cv2.imwrite(str(path), warped)
        images.append(path)
    # Without physical size → SENSOR_DERIVED.
    result_rel = calibrate_planar_board(images, board_size=board, square_size_m=None)
    assert result_rel.authority is AuthorityClass.SENSOR_DERIVED
    assert result_rel.detected_views >= 3
    # With physical size → MEASURED.
    square_m = 0.025
    result = calibrate_planar_board(images, board_size=board, square_size_m=square_m)
    assert result.authority is AuthorityClass.MEASURED
    assert result.mean_reprojection_error < 2.0
    bundle = result.seal_observation_bundle(target_id="board")
    assert bundle.authority is AuthorityClass.MEASURED
    bundle.verify()


# ----------------------------------------------------------- Blender-gated


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BLENDER_TESTS") != "1",
    reason="set BVMCP_RUN_BLENDER_TESTS=1 for real Blender integration",
)
def test_spatial_lane_blender_smoke(tmp_path: Path) -> None:
    """Thin gate: the full lane script runs against a tiny fixture."""
    import subprocess
    import sys

    repo = Path(__file__).resolve().parents[1]
    script = repo / "scripts" / "run-spatial-lane.py"
    assert script.is_file()
    output = tmp_path / "spatial-out"
    proc = subprocess.run(
        [sys.executable, str(script), "--output", str(output), "--quick"],
        cwd=str(repo),
        capture_output=True,
        text=True,
        timeout=300,
        env={**os.environ, "BVMCP_RUN_BLENDER_TESTS": "1"},
    )
    assert proc.returncode == 0, proc.stdout + "\n" + proc.stderr
    receipt = output / "receipt.json"
    assert receipt.is_file()
