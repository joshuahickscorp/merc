"""RANSAC primitive fitting to point clouds.

Geometric RANSAC lives here. The existing ``parametric.fitting.ComponentFitter``
handles scalar measurement-to-component parameter fits against project storage;
this backend does free-space primitive recovery (plane/box/cylinder/sphere) and
can optionally emit dimension candidates compatible with that store.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Any

import numpy as np

from blender_vision.core.models import BackendState
from blender_vision.reconstruction.base import (
    BackendAvailability,
    MeshGeometry,
    PointCloud,
    ReconstructionInputs,
    TimedRun,
    finalize_candidate,
    unavailable_candidate,
)
from blender_vision.reconstruction.mesh_ops import (
    box_mesh,
    sphere_mesh,
    topology_report,
    write_ply_mesh,
)
from blender_vision.v2.authority import AuthorityClass, derive
from blender_vision.v2.records import ReconstructionCandidate


@dataclass(slots=True)
class PrimitiveFit:
    kind: str
    parameters: dict[str, Any]
    inlier_ratio: float
    residual_rmse: float
    inlier_count: int
    sample_count: int


class ParametricBackend:
    name = "parametric"

    def availability(self) -> BackendAvailability:
        return BackendAvailability(
            state=BackendState.AVAILABLE,
            reason="numpy RANSAC primitive fitting available",
        )

    def run(self, inputs: ReconstructionInputs) -> ReconstructionCandidate:
        if inputs.points is None or len(inputs.points.positions) < 10:
            return unavailable_candidate(
                backend=self.name,
                reason="parametric fitting requires a point cloud with >= 10 points",
                inputs=inputs,
            )
        kind = (inputs.primitive_kind or inputs.parameters.get("primitive_kind") or "auto").lower()
        with TimedRun() as timer:
            fit = fit_primitive(
                inputs.points.positions,
                kind=kind,
                iterations=int(inputs.parameters.get("iterations", 400)),
                threshold=float(inputs.parameters.get("threshold", 0.01)),
                seed=int(inputs.parameters.get("seed", 0)),
            )
            mesh = primitive_to_mesh(fit)
            work = inputs.ensure_work_dir()
            mesh_path = write_ply_mesh(work / f"parametric_{fit.kind}.ply", mesh)
            params_path = work / f"parametric_{fit.kind}.json"
            params_path.write_text(
                json.dumps(
                    {
                        "kind": fit.kind,
                        "parameters": fit.parameters,
                        "inlier_ratio": fit.inlier_ratio,
                        "residual_rmse": fit.residual_rmse,
                        "inlier_count": fit.inlier_count,
                        "sample_count": fit.sample_count,
                        "note": (
                            "Geometric RANSAC fit. Scalar project measurement fitting "
                            "remains in blender_vision.parametric.fitting.ComponentFitter."
                        ),
                    },
                    indent=2,
                    sort_keys=True,
                )
                + "\n",
                encoding="utf-8",
            )
            report = topology_report(mesh)
            report["primitive_kind"] = fit.kind
            report["inlier_ratio"] = fit.inlier_ratio
            report["residual_rmse"] = fit.residual_rmse

        authority = derive(
            inputs.input_authorities or [AuthorityClass.MODEL_DERIVED],
            proposed=AuthorityClass.MODEL_DERIVED,
        )
        return finalize_candidate(
            backend=self.name,
            inputs=inputs,
            authority=authority,
            scale_authority=inputs.frame.scale_authority,
            scale_state=(
                "metric"
                if inputs.frame.scale_authority != AuthorityClass.UNRESOLVED
                else "data-units"
            ),
            coverage={
                "point_count": int(len(inputs.points.positions)),
                "inlier_ratio": fit.inlier_ratio,
                "primitive_kind": fit.kind,
            },
            topology_state=report,
            editability="parametric-primitive",
            hidden_surface_assumptions=[
                f"surface assumed to be a single {fit.kind}; outliers discarded",
            ],
            artifacts={"mesh_ply": str(mesh_path), "parameters_json": str(params_path)},
            runtime_seconds=timer.seconds,
            execution_log=(
                f"RANSAC {fit.kind}: inlier_ratio={fit.inlier_ratio:.4f} "
                f"rmse={fit.residual_rmse:.6g} n={fit.sample_count}"
            ),
            failure_modes=[
                "RANSAC fails on multi-primitive or non-primitive geometry",
                "threshold sensitivity on noisy clouds",
            ],
            dimensional_score=fit.inlier_ratio,
            executed=True,
        )


def fit_primitive(
    points: np.ndarray,
    *,
    kind: str = "auto",
    iterations: int = 400,
    threshold: float = 0.01,
    seed: int = 0,
) -> PrimitiveFit:
    points = np.asarray(points, dtype=np.float64)
    if kind == "auto":
        candidates = [
            fit_plane(points, iterations=iterations, threshold=threshold, seed=seed),
            fit_sphere(points, iterations=iterations, threshold=threshold, seed=seed),
            fit_box(points, iterations=iterations, threshold=threshold, seed=seed),
            fit_cylinder(points, iterations=iterations, threshold=threshold, seed=seed),
        ]
        return max(candidates, key=lambda item: (item.inlier_ratio, -item.residual_rmse))
    dispatch = {
        "plane": fit_plane,
        "sphere": fit_sphere,
        "box": fit_box,
        "cylinder": fit_cylinder,
    }
    if kind not in dispatch:
        raise ValueError(f"unknown primitive kind {kind!r}")
    return dispatch[kind](points, iterations=iterations, threshold=threshold, seed=seed)


def fit_plane(
    points: np.ndarray, *, iterations: int, threshold: float, seed: int
) -> PrimitiveFit:
    rng = np.random.default_rng(seed)
    n = len(points)
    best_inliers = np.zeros(n, dtype=bool)
    best_model = (np.array([0.0, 0.0, 1.0]), 0.0)
    for _ in range(iterations):
        idx = rng.choice(n, size=3, replace=False)
        p0, p1, p2 = points[idx]
        normal = np.cross(p1 - p0, p2 - p0)
        norm = np.linalg.norm(normal)
        if norm < 1e-12:
            continue
        normal = normal / norm
        d = float(np.dot(normal, p0))
        dist = np.abs(points @ normal - d)
        inliers = dist <= threshold
        if inliers.sum() > best_inliers.sum():
            best_inliers = inliers
            best_model = (normal, d)
    normal, d = best_model
    if best_inliers.any():
        # Refine with SVD on inliers.
        pts = points[best_inliers]
        centroid = pts.mean(axis=0)
        _, _, vh = np.linalg.svd(pts - centroid, full_matrices=False)
        normal = vh[-1]
        normal = normal / np.linalg.norm(normal)
        d = float(np.dot(normal, centroid))
        residuals = points[best_inliers] @ normal - d
        rmse = float(np.sqrt(np.mean(residuals**2)))
    else:
        rmse = float("inf")
        centroid = points.mean(axis=0)
    return PrimitiveFit(
        kind="plane",
        parameters={
            "normal": normal.tolist(),
            "offset": d,
            "point": centroid.tolist(),
        },
        inlier_ratio=float(best_inliers.mean()) if n else 0.0,
        residual_rmse=rmse,
        inlier_count=int(best_inliers.sum()),
        sample_count=n,
    )


def fit_sphere(
    points: np.ndarray, *, iterations: int, threshold: float, seed: int
) -> PrimitiveFit:
    rng = np.random.default_rng(seed + 1)
    n = len(points)
    best_inliers = np.zeros(n, dtype=bool)
    best = (points.mean(axis=0), 1.0)
    for _ in range(iterations):
        idx = rng.choice(n, size=4, replace=False)
        model = _sphere_from_points(points[idx])
        if model is None:
            continue
        center, radius = model
        dist = np.abs(np.linalg.norm(points - center, axis=1) - radius)
        inliers = dist <= threshold
        if inliers.sum() > best_inliers.sum():
            best_inliers = inliers
            best = (center, radius)
    center, radius = best
    if best_inliers.any():
        refined = _sphere_least_squares(points[best_inliers])
        if refined is not None:
            center, radius = refined
        residuals = np.linalg.norm(points[best_inliers] - center, axis=1) - radius
        rmse = float(np.sqrt(np.mean(residuals**2)))
    else:
        rmse = float("inf")
    return PrimitiveFit(
        kind="sphere",
        parameters={"center": center.tolist(), "radius": float(radius)},
        inlier_ratio=float(best_inliers.mean()) if n else 0.0,
        residual_rmse=rmse,
        inlier_count=int(best_inliers.sum()),
        sample_count=n,
    )


def fit_box(
    points: np.ndarray, *, iterations: int, threshold: float, seed: int
) -> PrimitiveFit:
    """Axis-aligned box via RANSAC on extent percentiles of random subsets."""
    rng = np.random.default_rng(seed + 2)
    n = len(points)
    best_inliers = np.zeros(n, dtype=bool)
    best_min = points.min(axis=0)
    best_max = points.max(axis=0)
    sample_size = max(10, n // 5)
    for _ in range(iterations):
        idx = rng.choice(n, size=min(sample_size, n), replace=False)
        sample = points[idx]
        minimum = sample.min(axis=0) - threshold
        maximum = sample.max(axis=0) + threshold
        inside = np.all((points >= minimum - threshold) & (points <= maximum + threshold), axis=1)
        # Distance to box surface for residual ranking.
        if inside.sum() > best_inliers.sum():
            best_inliers = inside
            best_min, best_max = minimum + threshold, maximum - threshold
    # Final AABB of inliers.
    if best_inliers.any():
        best_min = points[best_inliers].min(axis=0)
        best_max = points[best_inliers].max(axis=0)
        residuals = _box_distances(points[best_inliers], best_min, best_max)
        rmse = float(np.sqrt(np.mean(residuals**2)))
    else:
        rmse = float("inf")
    return PrimitiveFit(
        kind="box",
        parameters={
            "minimum": best_min.tolist(),
            "maximum": best_max.tolist(),
            "size": (best_max - best_min).tolist(),
        },
        inlier_ratio=float(best_inliers.mean()) if n else 0.0,
        residual_rmse=rmse,
        inlier_count=int(best_inliers.sum()),
        sample_count=n,
    )


def fit_cylinder(
    points: np.ndarray, *, iterations: int, threshold: float, seed: int
) -> PrimitiveFit:
    """Vertical-ish cylinder: axis from PCA, radius RANSAC in the plane."""
    rng = np.random.default_rng(seed + 3)
    n = len(points)
    centroid = points.mean(axis=0)
    _, _, vh = np.linalg.svd(points - centroid, full_matrices=False)
    axis = vh[0]
    axis = axis / np.linalg.norm(axis)
    # Project to plane perpendicular to axis.
    proj = points - np.outer((points - centroid) @ axis, axis)
    # 2D circle fit via RANSAC in an arbitrary plane basis.
    b1 = vh[1]
    b2 = vh[2]
    coords = np.stack([(proj - centroid) @ b1, (proj - centroid) @ b2], axis=1)
    best_inliers = np.zeros(n, dtype=bool)
    best_center2 = np.zeros(2)
    best_radius = 1.0
    for _ in range(iterations):
        idx = rng.choice(n, size=3, replace=False)
        circle = _circle_from_points(coords[idx])
        if circle is None:
            continue
        c2, radius = circle
        dist = np.abs(np.linalg.norm(coords - c2, axis=1) - radius)
        inliers = dist <= threshold
        if inliers.sum() > best_inliers.sum():
            best_inliers = inliers
            best_center2, best_radius = c2, radius
    center = centroid + best_center2[0] * b1 + best_center2[1] * b2
    heights = (points - center) @ axis
    if best_inliers.any():
        radial = np.linalg.norm(coords[best_inliers] - best_center2, axis=1)
        residuals = np.abs(radial - best_radius)
        rmse = float(np.sqrt(np.mean(residuals**2)))
        z0, z1 = float(heights[best_inliers].min()), float(heights[best_inliers].max())
    else:
        rmse = float("inf")
        z0, z1 = float(heights.min()), float(heights.max())
    return PrimitiveFit(
        kind="cylinder",
        parameters={
            "axis_point": center.tolist(),
            "axis_direction": axis.tolist(),
            "radius": float(best_radius),
            "height_min": z0,
            "height_max": z1,
        },
        inlier_ratio=float(best_inliers.mean()) if n else 0.0,
        residual_rmse=rmse,
        inlier_count=int(best_inliers.sum()),
        sample_count=n,
    )


def _sphere_from_points(pts: np.ndarray) -> tuple[np.ndarray, float] | None:
    if len(pts) < 4:
        return None
    A = np.hstack((2 * pts, np.ones((len(pts), 1))))
    b = (pts**2).sum(axis=1)
    try:
        sol, *_ = np.linalg.lstsq(A, b, rcond=None)
    except np.linalg.LinAlgError:
        return None
    center = sol[:3]
    radius_sq = float(sol[3] + np.dot(center, center))
    if radius_sq <= 0:
        return None
    return center, float(np.sqrt(radius_sq))


def _sphere_least_squares(pts: np.ndarray) -> tuple[np.ndarray, float] | None:
    return _sphere_from_points(pts)


def _circle_from_points(pts: np.ndarray) -> tuple[np.ndarray, float] | None:
    if len(pts) < 3:
        return None
    A = np.hstack((2 * pts, np.ones((len(pts), 1))))
    b = (pts**2).sum(axis=1)
    try:
        sol, *_ = np.linalg.lstsq(A, b, rcond=None)
    except np.linalg.LinAlgError:
        return None
    center = sol[:2]
    radius_sq = float(sol[2] + np.dot(center, center))
    if radius_sq <= 0:
        return None
    return center, float(np.sqrt(radius_sq))


def _box_distances(points: np.ndarray, minimum: np.ndarray, maximum: np.ndarray) -> np.ndarray:
    # Distance to AABB: 0 inside, positive outside.
    below = np.maximum(minimum - points, 0.0)
    above = np.maximum(points - maximum, 0.0)
    return np.linalg.norm(below + above, axis=1)


def primitive_to_mesh(fit: PrimitiveFit) -> MeshGeometry:
    if fit.kind == "sphere":
        return sphere_mesh(fit.parameters["center"], fit.parameters["radius"], subdivisions=2)
    if fit.kind == "box":
        return box_mesh(fit.parameters["minimum"], fit.parameters["maximum"])
    if fit.kind == "plane":
        # Finite patch around the fit point.
        point = np.asarray(fit.parameters["point"], dtype=np.float64)
        normal = np.asarray(fit.parameters["normal"], dtype=np.float64)
        normal = normal / np.linalg.norm(normal)
        tmp = np.array([1.0, 0.0, 0.0]) if abs(normal[0]) < 0.9 else np.array([0.0, 1.0, 0.0])
        u = np.cross(normal, tmp)
        u = u / np.linalg.norm(u)
        v = np.cross(normal, u)
        size = 0.5
        corners = [
            point + size * u + size * v,
            point - size * u + size * v,
            point - size * u - size * v,
            point + size * u - size * v,
        ]
        vertices = np.asarray(corners, dtype=np.float64)
        faces = np.array([[0, 1, 2], [0, 2, 3]], dtype=np.int64)
        return MeshGeometry(vertices=vertices, faces=faces)
    if fit.kind == "cylinder":
        # Approximate with a prism for export.
        axis_point = np.asarray(fit.parameters["axis_point"], dtype=np.float64)
        axis = np.asarray(fit.parameters["axis_direction"], dtype=np.float64)
        axis = axis / np.linalg.norm(axis)
        radius = float(fit.parameters["radius"])
        z0 = float(fit.parameters["height_min"])
        z1 = float(fit.parameters["height_max"])
        tmp = np.array([1.0, 0.0, 0.0]) if abs(axis[0]) < 0.9 else np.array([0.0, 1.0, 0.0])
        u = np.cross(axis, tmp)
        u = u / np.linalg.norm(u)
        v = np.cross(axis, u)
        segments = 24
        verts = []
        faces = []
        for i in range(segments):
            ang = 2 * np.pi * i / segments
            radial = radius * (np.cos(ang) * u + np.sin(ang) * v)
            verts.append(axis_point + axis * z0 + radial)
            verts.append(axis_point + axis * z1 + radial)
        for i in range(segments):
            a = 2 * i
            b = 2 * i + 1
            c = 2 * ((i + 1) % segments)
            d = c + 1
            faces.append([a, c, d])
            faces.append([a, d, b])
        return MeshGeometry(
            vertices=np.asarray(verts, dtype=np.float64),
            faces=np.asarray(faces, dtype=np.int64),
        )
    raise ValueError(f"cannot mesh primitive {fit.kind}")


def sample_sphere_points(
    center: np.ndarray | list[float],
    radius: float,
    count: int,
    *,
    noise: float = 0.0,
    seed: int = 0,
) -> PointCloud:
    rng = np.random.default_rng(seed)
    dirs = rng.normal(size=(count, 3))
    dirs /= np.linalg.norm(dirs, axis=1, keepdims=True)
    pts = np.asarray(center, dtype=np.float64) + radius * dirs
    if noise:
        pts = pts + rng.normal(scale=noise, size=pts.shape)
    return PointCloud(positions=pts, normals=dirs)
