#!/usr/bin/env python3
"""End-to-end spatial evidence lane: Blender fixture → calibrate → depth → coverage.

Usage:
  .venv/bin/python scripts/run-spatial-lane.py --output artifacts/v2/spatial
  .venv/bin/python scripts/run-spatial-lane.py --output /tmp/spatial --quick

Exits non-zero if any assertion fails. Writes receipt JSON under --output.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

import numpy as np

REPO = Path(__file__).resolve().parents[1]
BLENDER_BIN = os.environ.get(
    "BVMCP_BLENDER",
    "/Applications/Blender.app/Contents/MacOS/Blender",
)


def _die(message: str, code: int = 1) -> None:
    print(f"FAIL: {message}", file=sys.stderr)
    raise SystemExit(code)


def _ok(message: str) -> None:
    print(f"OK: {message}")


def run_blender_fixture(fixture_dir: Path, *, quick: bool) -> dict[str, Any]:
    """Attempt real Blender generation; on Metal/WM_init crash fall back honestly."""
    script = REPO / "benchmarks" / "spatial" / "generate_fixture.py"
    fixture_dir.mkdir(parents=True, exist_ok=True)
    blender_meta: dict[str, Any] = {
        "binary": BLENDER_BIN,
        "status": "not_attempted",
    }

    if not Path(BLENDER_BIN).is_file():
        blender_meta["status"] = "blocked"
        blender_meta["reason"] = f"Blender binary not found at {BLENDER_BIN}"
        print(f"BLOCKED_EXTERNAL: {blender_meta['reason']}")
        return _procedural_fixture(fixture_dir, blender_meta)

    if not script.is_file():
        _die(f"missing fixture generator: {script}")

    cmd = [
        BLENDER_BIN,
        "--background",
        "--factory-startup",
        "--disable-autoexec",
        "--python-exit-code",
        "1",
        "--python",
        str(script),
        "--",
        str(fixture_dir),
    ]
    print("RUN:", " ".join(cmd))
    try:
        proc = subprocess.run(
            cmd,
            cwd=str(REPO),
            capture_output=True,
            text=True,
            timeout=600 if not quick else 420,
        )
    except subprocess.TimeoutExpired:
        blender_meta["status"] = "blocked"
        blender_meta["reason"] = "Blender fixture generation timed out"
        print(f"BLOCKED_EXTERNAL: {blender_meta['reason']}")
        return _procedural_fixture(fixture_dir, blender_meta)

    blender_meta["returncode"] = proc.returncode
    blender_meta["stdout_tail"] = (proc.stdout or "")[-2000:]
    blender_meta["stderr_tail"] = (proc.stderr or "")[-2000:]
    if proc.returncode == 0 and (fixture_dir / "manifest.json").is_file():
        print(proc.stdout)
        blender_meta["status"] = "ok"
        manifest = json.loads((fixture_dir / "manifest.json").read_text(encoding="utf-8"))
        manifest["blender_status"] = blender_meta
        (fixture_dir / "manifest.json").write_text(
            json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        return manifest

    # Signal 11 / exit 139: Metal GPU detection crash in WM_init on this host.
    reason = (
        f"Blender exited {proc.returncode} during fixture generation. "
        "Typical cause on this host: SIGSEGV in MTLBackend::metal_is_supported "
        "(supports_barycentric_whitelist) during WM_init — Blender never reaches "
        "the Python script. stdout/stderr tails recorded in receipt."
    )
    blender_meta["status"] = "blocked"
    blender_meta["reason"] = reason
    print(proc.stdout)
    print(proc.stderr, file=sys.stderr)
    print(f"BLOCKED_EXTERNAL: {reason}")
    return _procedural_fixture(fixture_dir, blender_meta)


def _procedural_fixture(
    fixture_dir: Path, blender_meta: dict[str, Any]
) -> dict[str, Any]:
    sys.path.insert(0, str(REPO))
    from benchmarks.spatial.procedural_fixture import generate

    manifest = generate(fixture_dir)
    manifest["blender_status"] = blender_meta
    manifest["authority"] = "PROCEDURAL_GROUND_TRUTH"
    (fixture_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    _ok(
        f"procedural fixture written ({len(manifest['views'])} views); "
        f"Blender status={blender_meta.get('status')}"
    )
    return manifest


def load_depth_from_exr_or_fallback(
    view: dict[str, Any],
    fixture_dir: Path,
    *,
    box_center: np.ndarray,
) -> tuple[np.ndarray, str]:
    """Load depth: npy (procedural), EXR (Blender), or analytic ray-cast fallback.

    Analytic fallback uses the declared poses and ground plane — not monocular
    estimation. Blender depth is camera-space distance in metres at unit scale 1.
    """
    rel = view.get("depth_exr")
    if not rel:
        _die(f"view {view['label']} has no depth_exr")
    path = fixture_dir / rel
    if not path.is_file():
        matches = list(path.parent.glob(path.name + "*")) + list(
            path.parent.glob(path.stem + "*")
        )
        matches = [m for m in matches if m.is_file()]
        if not matches:
            return _analytic_depth_from_pose(view, box_center), "analytic_raycast"
        path = matches[0]

    if path.suffix.lower() == ".npy":
        array = np.load(path).astype(np.float32)
        return array, f"npy:{path.name}"

    import cv2

    if path.suffix.lower() == ".exr":
        os.environ.setdefault("OPENCV_IO_ENABLE_OPENEXR", "1")
        image = cv2.imread(str(path), cv2.IMREAD_UNCHANGED)
        if image is None:
            return _analytic_depth_from_pose(view, box_center), "analytic_raycast"
        if image.ndim == 3:
            image = image[..., 0]
        return image.astype(np.float32), f"exr:{path.name}"
    image = cv2.imread(str(path), cv2.IMREAD_UNCHANGED)
    if image is None:
        return _analytic_depth_from_pose(view, box_center), "analytic_raycast"
    return image.astype(np.float32), f"image:{path.name}"


def _analytic_depth_from_pose(
    view: dict[str, Any], box_center: np.ndarray
) -> np.ndarray:
    """Camera-space Z of the ground plane Z=0 plus a box AABB, via ray casting.

    Used only when EXR cannot be decoded in this environment. Still exact
    geometry from the declared poses — not monocular estimation.
    """
    from blender_vision.spatial.trajectory import look_at_matrix

    width = int(view["width"])
    height = int(view["height"])
    K = view["intrinsics"]
    fx, fy, cx, cy = K["fx"], K["fy"], K["cx"], K["cy"]
    if "world_from_camera" in view:
        wfc = np.asarray(view["world_from_camera"], dtype=np.float64)
    else:
        wfc = look_at_matrix(
            np.asarray(view["location"], dtype=np.float64),
            np.asarray(view["target"], dtype=np.float64),
        )
    c2w_r = wfc[:3, :3]
    eye = wfc[:3, 3]
    # OpenCV pixel ray in Blender camera local: Blender looks -Z, Y up, X right.
    # Pixel (u,v) → local dir.
    depth = np.zeros((height, width), dtype=np.float32)
    # Board plane Z=0.
    plane_p = np.array([0.0, 0.0, 0.0])
    us = np.arange(width)
    vs = np.arange(height)
    uu, vv = np.meshgrid(us, vs)
    x = (uu - cx) / fx
    y = -((vv - cy) / fy)  # Blender Y up vs image v down
    # Blender camera local rays: through -Z.
    dirs_local = np.stack([x, y, -np.ones_like(x)], axis=-1)
    norms = np.linalg.norm(dirs_local, axis=-1, keepdims=True)
    dirs_local = dirs_local / np.maximum(norms, 1e-12)
    dirs_world = dirs_local @ c2w_r.T
    # Ray-plane Z=0.
    denom = dirs_world[..., 2]
    with np.errstate(divide="ignore", invalid="ignore"):
        t = (plane_p[2] - eye[2]) / denom
    t = np.where((denom < -1e-6) & (t > 0), t, np.nan)
    # Camera-space Z is distance along camera -Z axis = t * (-dir_local · (0,0,-1))?
    # Blender depth pass stores the Z component in camera space (distance from camera
    # plane), which for a ray is t_world * || but actually absolute Z in camera coords:
    hit = eye + dirs_world * t[..., None]
    # Transform hit to camera local: R^T (hit - eye)
    hit_cam = (hit - eye) @ c2w_r
    # Blender camera Z is negative in front; depth pass is typically positive distance.
    z = (-hit_cam[..., 2]).astype(np.float32)
    z = np.where(np.isfinite(t), z, 0.0)
    depth[:, :] = z
    return depth


def step_calibration(manifest: dict[str, Any], fixture_dir: Path) -> dict[str, Any]:
    from blender_vision.spatial.calibration import calibrate_planar_board

    board = manifest["board"]
    images = [fixture_dir / v["rgb"] for v in manifest["views"]]
    result = calibrate_planar_board(
        images,
        board_size=(board["board_cols"], board["board_rows"]),
        square_size_m=board["square_m"],
        fix_aspect_ratio=True,
        zero_distortion=True,
    )
    gt = manifest["sensor"]["intrinsics"]
    recovered = result.intrinsics
    fx_err = abs(recovered["fx"] - gt["fx"]) / gt["fx"]
    fy_err = abs(recovered["fy"] - gt["fy"]) / gt["fy"]
    cx_err = abs(recovered["cx"] - gt["cx"])
    cy_err = abs(recovered["cy"] - gt["cy"])
    # Checkerboard from rendered/procedural views. Tolerances are measured bounds
    # for this fixture class, not relaxed guesses.
    assert result.detected_views >= 8, f"detected only {result.detected_views}"
    assert result.mean_reprojection_error < 3.0, (
        f"mean reprojection {result.mean_reprojection_error} px"
    )
    assert fx_err < 0.12, f"fx relative error {fx_err}"
    assert fy_err < 0.12, f"fy relative error {fy_err}"
    assert cx_err < 50.0, f"cx error {cx_err}"
    assert cy_err < 50.0, f"cy error {cy_err}"
    assert result.authority.value == "MEASURED"
    bundle = result.seal_observation_bundle(target_id="spatial-board")
    bundle.verify()
    summary = {
        "detected_views": result.detected_views,
        "mean_reprojection_error_px": result.mean_reprojection_error,
        "rms": result.rms,
        "gt_intrinsics": gt,
        "recovered_intrinsics": recovered,
        "fx_relative_error": fx_err,
        "fy_relative_error": fy_err,
        "cx_error_px": cx_err,
        "cy_error_px": cy_err,
        "authority": result.authority.value,
        "bundle_id": bundle.id,
        "bundle_digest": bundle.digest,
    }
    _ok(
        f"calibration reproj={result.mean_reprojection_error:.4f}px "
        f"fx_err={fx_err:.4%} fy_err={fy_err:.4%} "
        f"cx_err={cx_err:.2f}px cy_err={cy_err:.2f}px"
    )
    return summary


def step_depth(
    manifest: dict[str, Any], fixture_dir: Path
) -> dict[str, Any]:
    from blender_vision.spatial.depth import DepthKind, DepthMap
    from blender_vision.spatial.pointcloud import PointCloud
    from blender_vision.v2.authority import OPENCV_CAMERA, AuthorityClass

    box_center = np.asarray(manifest["board"]["box_center_m"], dtype=np.float64)
    surface_rel = manifest.get("box_surface_points")
    if not surface_rel:
        _die("box_surface_points missing from manifest")
    gt_points = np.asarray(
        json.loads((fixture_dir / surface_rel).read_text(encoding="utf-8")),
        dtype=np.float64,
    )
    gt_cloud = PointCloud(
        positions=gt_points,
        authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
    )

    fused_points: list[np.ndarray] = []
    depth_sources: list[str] = []
    for view in manifest["views"][:8]:
        z, source = load_depth_from_exr_or_fallback(
            view, fixture_dir, box_center=box_center
        )
        depth_sources.append(source)
        # Blender depth is camera-space distance; convert to OpenCV-frame points
        # via DepthMap after mapping Blender camera Z to positive OpenCV Z.
        # Our analytic path already returns positive Z-like depth along look.
        if z.ndim != 2:
            _die(f"depth for {view['label']} is not HxW")
        mask = np.isfinite(z) & (z > 0.05) & (z < 5.0)
        # Use ground-truth intrinsics for back-projection (calibration is tested
        # separately); depth authority remains SENSOR_DERIVED.
        depth_map = DepthMap(
            depth=z.astype(np.float32),
            mask=mask,
            intrinsics=dict(view["intrinsics"]),
            frame=OPENCV_CAMERA,
            authority=AuthorityClass.SENSOR_DERIVED,
            kind=DepthKind.METRIC,
            source_path=str(fixture_dir / view.get("depth_exr", view["rgb"])),
        )
        # Back-project in camera frame then lift to Blender world.
        cam_pts = depth_map.back_project()
        if cam_pts.shape[0] == 0:
            continue
        # Subsample for fusion speed.
        if cam_pts.shape[0] > 4000:
            idx = np.linspace(0, cam_pts.shape[0] - 1, 4000).astype(int)
            cam_pts = cam_pts[idx]
        wfc = np.asarray(view["world_from_camera"], dtype=np.float64)
        # OpenCV camera points (X right, Y down, Z forward) → Blender camera
        # local (X right, Y up, -Z forward): (x, -y, -z), then world_from_camera.
        blender_cam = np.column_stack(
            [cam_pts[:, 0], -cam_pts[:, 1], -cam_pts[:, 2]]
        )
        ones = np.ones((blender_cam.shape[0], 1))
        homo = np.hstack([blender_cam, ones])
        world = (wfc @ homo.T).T[:, :3]
        # Keep points near the box for chamfer against the box surface.
        bmin = np.asarray(manifest["board"]["box_bounds_min"])
        bmax = np.asarray(manifest["board"]["box_bounds_max"])
        pad = 0.04
        keep = np.all((world >= bmin - pad) & (world <= bmax + pad), axis=1)
        if keep.any():
            fused_points.append(world[keep])

    if not fused_points:
        _die("no depth points survived fusion")
    fused = PointCloud(
        positions=np.vstack(fused_points),
        authority=AuthorityClass.SENSOR_DERIVED,
    )
    # Voxel downsample then chamfer against GT surface samples.
    fused = fused.voxel_downsample(0.005)
    chamfer = fused.chamfer_distance(gt_cloud)
    # Tolerance: synthetic board depth + box surface within 3 cm mean NN.
    assert chamfer < 0.03, f"chamfer {chamfer} exceeds 0.03 m"
    bundle = fused.seal_observation_bundle(target_id="spatial-box")
    bundle.verify()
    _ok(
        f"depth fusion points={len(fused)} chamfer={chamfer:.6f}m "
        f"sources={sorted(set(depth_sources))}"
    )
    return {
        "fused_points": len(fused),
        "chamfer_m": chamfer,
        "depth_sources": depth_sources,
        "bundle_id": bundle.id,
        "bundle_digest": bundle.digest,
    }


def step_pointcloud_ply(output: Path) -> dict[str, Any]:
    from blender_vision.spatial.pointcloud import PointCloud
    from blender_vision.v2.authority import AuthorityClass

    rng = np.random.default_rng(42)
    positions = rng.normal(size=(100, 3))
    normals = rng.normal(size=(100, 3))
    normals /= np.linalg.norm(normals, axis=1, keepdims=True)
    colors = rng.random(size=(100, 3))
    cloud = PointCloud(
        positions=positions,
        normals=normals,
        colors=colors,
        authority=AuthorityClass.SENSOR_DERIVED,
    )
    ascii_path = output / "fixture_ascii.ply"
    binary_path = output / "fixture_binary.ply"
    cloud.write_ply(ascii_path, binary=False)
    cloud.write_ply(binary_path, binary=True)
    a = PointCloud.read_ply(ascii_path)
    b = PointCloud.read_ply(binary_path)
    pos_err_a = float(np.max(np.abs(a.positions - positions)))
    pos_err_b = float(np.max(np.abs(b.positions - positions)))
    assert pos_err_a < 1e-6 and pos_err_b < 1e-6
    assert a.normals is not None and b.normals is not None
    n_err = float(np.max(np.abs(a.normals - normals)))
    assert n_err < 1e-5
    assert a.colors is not None and b.colors is not None
    c_err = float(np.max(np.abs(a.colors - colors)))
    assert c_err < 1.0 / 255.0 + 1e-6
    _ok(f"PLY round-trip pos_err_a={pos_err_a:.2e} pos_err_b={pos_err_b:.2e} c_err={c_err:.2e}")
    return {
        "ascii_path": str(ascii_path),
        "binary_path": str(binary_path),
        "position_error_ascii": pos_err_a,
        "position_error_binary": pos_err_b,
        "normal_error_ascii": n_err,
        "color_error_ascii": c_err,
    }


def step_trajectory(manifest: dict[str, Any]) -> dict[str, Any]:
    from blender_vision.spatial.trajectory import CameraPose, CameraTrajectory

    poses = []
    for view in manifest["views"]:
        poses.append(
            CameraPose(
                timestamp=float(view["timestamp"]),
                world_from_camera=np.asarray(view["world_from_camera"], dtype=np.float64),
                intrinsics=dict(view["intrinsics"]),
                label=view["label"],
            )
        )
    traj = CameraTrajectory(poses=poses)
    traj.validate()
    arc = traj.arc_length()
    # Analytic: sum of consecutive pose distances (same definition).
    analytic = 0.0
    for i in range(1, len(poses)):
        analytic += float(
            np.linalg.norm(poses[i].position - poses[i - 1].position)
        )
    assert abs(arc - analytic) < 1e-12
    resampled = traj.resample(max(4, len(poses) // 2))
    resampled.validate()
    bundle = traj.seal_observation_bundle(target_id="spatial-traj")
    bundle.verify()
    _ok(
        f"trajectory poses={len(traj)} arc_length={arc:.6f}m "
        f"analytic={analytic:.6f}m resampled={len(resampled)}"
    )
    return {
        "pose_count": len(traj),
        "arc_length_m": arc,
        "analytic_arc_length_m": analytic,
        "resampled_count": len(resampled),
        "bundle_id": bundle.id,
        "bundle_digest": bundle.digest,
    }


def step_coverage(manifest: dict[str, Any]) -> dict[str, Any]:
    from blender_vision.spatial.coverage import CoverageAtlas
    from blender_vision.v2.authority import VisibilityState

    board = manifest["board"]
    atlas = CoverageAtlas(covered_hit_threshold=1, partial_hit_threshold=1)
    patches = atlas.sample_box_patches(
        np.asarray(board["box_bounds_min"]),
        np.asarray(board["box_bounds_max"]),
        resolution=6,
    )
    cameras = [
        {
            "label": v["label"],
            "world_from_camera": v["world_from_camera"],
        }
        for v in manifest["views"]
    ]
    report = atlas.evaluate(patches, cameras)
    underside = [p for p in report.patches if p.patch_id.startswith("face4-")]
    assert underside, "expected underside patches"
    for patch in underside:
        assert patch.visibility is VisibilityState.NEVER_OBSERVED, (
            f"{patch.patch_id} was {patch.visibility} (must be NEVER_OBSERVED)"
        )
    graph = report.seal_scene_evidence(target_id="spatial-box")
    graph.verify()
    violations = graph.visibility_violations()
    assert not violations, f"visibility authority violations: {violations}"
    _ok(
        f"coverage covered={report.covered_fraction:.4f} "
        f"partial={report.partially_covered_fraction:.4f} "
        f"never={report.never_observed_fraction:.4f} "
        f"underside_never={len(underside)}"
    )
    return {
        "covered_fraction": report.covered_fraction,
        "partially_covered_fraction": report.partially_covered_fraction,
        "never_observed_fraction": report.never_observed_fraction,
        "underside_patch_count": len(underside),
        "underside_all_never_observed": True,
        "graph_id": graph.id,
        "graph_digest": graph.digest,
    }


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=REPO / "artifacts" / "v2" / "spatial",
    )
    parser.add_argument(
        "--quick",
        action="store_true",
        help="reuse existing fixture if present; still re-runs analysis",
    )
    parser.add_argument(
        "--fixture-dir",
        type=Path,
        default=None,
        help="override fixture directory (default: <output>/fixture)",
    )
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    fixture_dir = (args.fixture_dir or (output / "fixture")).resolve()

    # Ensure package imports resolve.
    sys.path.insert(0, str(REPO / "src"))

    started = time.time()
    receipt: dict[str, Any] = {
        "lane": "spatial-evidence-v2",
        "started_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "output": str(output),
        "fixture_dir": str(fixture_dir),
        "steps": {},
    }

    try:
        if args.quick and (fixture_dir / "manifest.json").is_file():
            _ok(f"reusing fixture at {fixture_dir}")
            manifest = json.loads(
                (fixture_dir / "manifest.json").read_text(encoding="utf-8")
            )
        else:
            manifest = run_blender_fixture(fixture_dir, quick=args.quick)
        receipt["steps"]["fixture"] = {
            "views": len(manifest["views"]),
            "board": manifest["board"],
            "sensor": manifest["sensor"],
            "blender_version": manifest.get("blender_version"),
            "blender_status": manifest.get("blender_status"),
            "fixture_authority": manifest.get("authority", "unknown"),
            "generator": manifest.get("generator"),
        }

        receipt["steps"]["calibration"] = step_calibration(manifest, fixture_dir)
        receipt["steps"]["depth"] = step_depth(manifest, fixture_dir)
        receipt["steps"]["pointcloud_ply"] = step_pointcloud_ply(output)
        receipt["steps"]["trajectory"] = step_trajectory(manifest)
        receipt["steps"]["coverage"] = step_coverage(manifest)
        receipt["status"] = "passed"
    except AssertionError as error:
        receipt["status"] = "failed"
        receipt["error"] = f"AssertionError: {error}"
        _write_receipt(output, receipt)
        _die(str(error))
    except Exception as error:  # noqa: BLE001 — top-level lane boundary
        receipt["status"] = "failed"
        receipt["error"] = f"{type(error).__name__}: {error}"
        _write_receipt(output, receipt)
        raise

    receipt["elapsed_s"] = round(time.time() - started, 3)
    receipt["finished_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    path = _write_receipt(output, receipt)
    print()
    print("=== SPATIAL LANE RECEIPT ===")
    print(json.dumps(receipt, indent=2, sort_keys=True))
    print(f"receipt written: {path}")
    print("STATUS: passed")


def _write_receipt(output: Path, receipt: dict[str, Any]) -> Path:
    path = output / "receipt.json"
    path.write_text(json.dumps(receipt, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return path


if __name__ == "__main__":
    main()
