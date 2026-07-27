"""Compose and validate a CameraPathGraph from cinematic controls."""

from __future__ import annotations

from collections.abc import Sequence
from dataclasses import dataclass
from typing import Any

import numpy as np

from blender_vision.cinematic.spline import ArcLengthSpline, CatmullRomSpline, QuaternionCurve
from blender_vision.core.errors import ValidationError
from blender_vision.v2.authority import BLENDER_WORLD, AuthorityClass, Uncertainty, Units, derive
from blender_vision.v2.records import CameraPathGraph, Lineage, NarrativeBeat


class CameraPathCompositionError(ValidationError):
    """Raised when a path fails structural or geometric validation."""


@dataclass(slots=True)
class SolidGeometry:
    """Axis-aligned solid used to reject camera-through-geometry paths."""

    name: str
    min_xyz: tuple[float, float, float]
    max_xyz: tuple[float, float, float]

    def contains(self, point: Sequence[float], *, margin: float = 0.0) -> bool:
        x, y, z = (float(point[0]), float(point[1]), float(point[2]))
        return (
            self.min_xyz[0] - margin <= x <= self.max_xyz[0] + margin
            and self.min_xyz[1] - margin <= y <= self.max_xyz[1] + margin
            and self.min_xyz[2] - margin <= z <= self.max_xyz[2] + margin
        )


def _beat_overlaps(beats: Sequence[NarrativeBeat]) -> list[tuple[str, str, float, float]]:
    ordered = sorted(beats, key=lambda item: (item.scroll_start, item.scroll_end))
    overlaps: list[tuple[str, str, float, float]] = []
    for left, right in zip(ordered, ordered[1:], strict=False):
        start = max(left.scroll_start, right.scroll_start)
        end = min(left.scroll_end, right.scroll_end)
        if end - start > 1e-6:
            overlaps.append((left.beat_id, right.beat_id, start, end))
    return overlaps


def _look_at_quaternion(
    position: Sequence[float], target: Sequence[float], up: Sequence[float] = (0.0, 0.0, 1.0)
) -> list[float]:
    """Blender-style camera: local -Y looks forward, +Z is up."""
    pos = np.asarray(position, dtype=np.float64)
    tgt = np.asarray(target, dtype=np.float64)
    world_up = np.asarray(up, dtype=np.float64)
    forward = tgt - pos
    norm = float(np.linalg.norm(forward))
    if norm < 1e-9:
        return [1.0, 0.0, 0.0, 0.0]
    forward = forward / norm
    # Camera local -Y = world forward, so camera Y axis = -forward.
    y_axis = -forward
    x_axis = np.cross(world_up, y_axis)
    x_norm = float(np.linalg.norm(x_axis))
    if x_norm < 1e-9:
        # Looking nearly along up; pick an arbitrary lateral axis.
        x_axis = np.array([1.0, 0.0, 0.0], dtype=np.float64)
        x_norm = 1.0
    x_axis = x_axis / x_norm
    z_axis = np.cross(x_axis, y_axis)
    z_axis = z_axis / np.linalg.norm(z_axis)
    # Rotation matrix columns are the camera axes in world space.
    rot = np.column_stack([x_axis, y_axis, z_axis])
    # Matrix to quaternion (wxyz), Shepperd's method.
    trace = float(rot[0, 0] + rot[1, 1] + rot[2, 2])
    if trace > 0.0:
        s = 0.5 / np.sqrt(trace + 1.0)
        w = 0.25 / s
        x = (rot[2, 1] - rot[1, 2]) * s
        y = (rot[0, 2] - rot[2, 0]) * s
        z = (rot[1, 0] - rot[0, 1]) * s
    elif rot[0, 0] > rot[1, 1] and rot[0, 0] > rot[2, 2]:
        s = 2.0 * np.sqrt(1.0 + rot[0, 0] - rot[1, 1] - rot[2, 2])
        w = (rot[2, 1] - rot[1, 2]) / s
        x = 0.25 * s
        y = (rot[0, 1] + rot[1, 0]) / s
        z = (rot[0, 2] + rot[2, 0]) / s
    elif rot[1, 1] > rot[2, 2]:
        s = 2.0 * np.sqrt(1.0 + rot[1, 1] - rot[0, 0] - rot[2, 2])
        w = (rot[0, 2] - rot[2, 0]) / s
        x = (rot[0, 1] + rot[1, 0]) / s
        y = 0.25 * s
        z = (rot[1, 2] + rot[2, 1]) / s
    else:
        s = 2.0 * np.sqrt(1.0 + rot[2, 2] - rot[0, 0] - rot[1, 1])
        w = (rot[1, 0] - rot[0, 1]) / s
        x = (rot[0, 2] + rot[2, 0]) / s
        y = (rot[1, 2] + rot[2, 1]) / s
        z = 0.25 * s
    quat = np.array([w, x, y, z], dtype=np.float64)
    quat = quat / np.linalg.norm(quat)
    return [float(v) for v in quat]


def path_geometry_intersections(
    control_points: Sequence[Sequence[float]],
    solids: Sequence[SolidGeometry],
    *,
    samples: int = 512,
    margin: float = 0.05,
) -> list[dict[str, Any]]:
    """Return samples where the camera path enters declared solid geometry."""
    if not solids:
        return []
    spline = ArcLengthSpline(CatmullRomSpline(control_points))
    hits: list[dict[str, Any]] = []
    for index, s in enumerate(np.linspace(0.0, 1.0, samples)):
        point = spline.evaluate(float(s))
        for solid in solids:
            if solid.contains(point, margin=margin):
                hits.append(
                    {
                        "sample_index": index,
                        "scroll": float(s),
                        "position": [float(v) for v in point],
                        "solid": solid.name,
                    }
                )
                break
    return hits


def _interpolate_scalar_curve(curve: Sequence[Sequence[float]], s: float) -> float:
    if not curve:
        return 0.0
    ordered = sorted((float(item[0]), float(item[1])) for item in curve)
    if s <= ordered[0][0]:
        return ordered[0][1]
    if s >= ordered[-1][0]:
        return ordered[-1][1]
    for (s0, v0), (s1, v1) in zip(ordered, ordered[1:], strict=False):
        if s0 <= s <= s1:
            if s1 <= s0:
                return v1
            t = (s - s0) / (s1 - s0)
            return v0 + t * (v1 - v0)
    return ordered[-1][1]


def compose_camera_path(
    *,
    path_id: str,
    control_points: Sequence[Sequence[float]],
    orientation_points: Sequence[Sequence[float]] | None = None,
    focus_targets: Sequence[dict[str, Any]] | None = None,
    focal_length_mm: Sequence[Sequence[float]] | None = None,
    exposure_curve: Sequence[Sequence[float]] | None = None,
    light_state_transitions: Sequence[dict[str, Any]] | None = None,
    beats: Sequence[NarrativeBeat],
    solids: Sequence[SolidGeometry] | None = None,
    skip_points: Sequence[float] | None = None,
    reduced_motion_views: Sequence[dict[str, Any]] | None = None,
    damping: float = 0.12,
    sample_count: int = 256,
    input_authorities: Sequence[str] | None = None,
    allow_geometry_intersection: bool = False,
) -> CameraPathGraph:
    """Build a sealed-ready CameraPathGraph with arc-length samples and checks."""
    if len(control_points) < 2:
        raise CameraPathCompositionError("at least two control points are required")
    if not beats:
        raise CameraPathCompositionError("at least one narrative beat is required")

    beat_list = list(beats)
    for beat in beat_list:
        if not 0.0 <= beat.scroll_start < beat.scroll_end <= 1.0:
            raise CameraPathCompositionError(
                f"beat {beat.beat_id} has invalid scroll interval "
                f"[{beat.scroll_start}, {beat.scroll_end}]"
            )
    gaps = CameraPathGraph(
        id="tmp", control_points=list(control_points), beats=beat_list
    ).beat_coverage_gaps()
    if gaps:
        raise CameraPathCompositionError(f"beat coverage gaps: {gaps}")
    overlaps = _beat_overlaps(beat_list)
    if overlaps:
        raise CameraPathCompositionError(f"beat coverage overlaps: {overlaps}")

    position_spline = CatmullRomSpline(control_points)
    arc = ArcLengthSpline(position_spline)
    if orientation_points is None:
        # Dense look-ahead samples so the turn is continuous, not a cut.
        look_ahead = 0.015
        sample_n = max(32, len(control_points) * 4)
        derived: list[list[float]] = []
        prev: list[float] | None = None
        for s in np.linspace(0.0, 1.0, sample_n):
            here = arc.evaluate(float(s))
            ahead = arc.evaluate(min(1.0, float(s) + look_ahead))
            if float(np.linalg.norm(ahead - here)) < 1e-9:
                ahead = here + np.array([0.0, 1.0, 0.0], dtype=np.float64)
            quat = _look_at_quaternion(here, ahead)
            # Keep the control sequence in one hemisphere before curve fit.
            if prev is not None and float(np.dot(prev, quat)) < 0.0:
                quat = [-v for v in quat]
            derived.append(quat)
            prev = quat
        orientation_points = derived
    orientation = QuaternionCurve(orientation_points)
    dots = orientation.consecutive_dot_products(512)
    if float(np.min(dots)) <= 0.0:
        raise CameraPathCompositionError(
            "orientation curve has a quaternion flip (non-positive consecutive dot product)"
        )

    solids = list(solids or [])
    hits = path_geometry_intersections(control_points, solids)
    if hits and not allow_geometry_intersection:
        first = hits[0]
        raise CameraPathCompositionError(
            f"camera path intersects solid {first['solid']!r} at scroll={first['scroll']:.4f}"
        )

    focus_targets = list(focus_targets or [])
    focal_curve = list(focal_length_mm or [[0.0, 35.0], [1.0, 35.0]])
    exposure = list(exposure_curve or [[0.0, 0.0], [1.0, 0.0]])
    lights = list(light_state_transitions or [])
    skips = [float(v) for v in (skip_points or [])]
    reduced = list(reduced_motion_views or [])

    samples: list[dict[str, Any]] = []
    for s in np.linspace(0.0, 1.0, sample_count):
        s_f = float(s)
        pos = arc.evaluate(s_f)
        quat = orientation.evaluate(s_f)
        samples.append(
            {
                "scroll": s_f,
                "position": [float(v) for v in pos],
                "orientation_wxyz": [float(v) for v in quat],
                "focal_length_mm": _interpolate_scalar_curve(focal_curve, s_f),
                "exposure": _interpolate_scalar_curve(exposure, s_f),
            }
        )

    authorities = list(input_authorities or [AuthorityClass.PROCEDURAL_GROUND_TRUTH.value])
    authority = derive(authorities, proposed=AuthorityClass.INFERRED)

    graph = CameraPathGraph(
        id=path_id,
        authority=authority,
        lineage=Lineage(
            operation="compose_camera_path",
            inputs=[path_id],
            input_authorities=[str(item) for item in authorities],
            parameters={
                "control_point_count": len(control_points),
                "sample_count": sample_count,
                "damping": damping,
            },
            limitations=[
                "Orientation without measured capture is derived from look-ahead; not OBSERVED."
            ],
        ),
        uncertainty=Uncertainty(
            kind="arc_length_reparameterisation",
            sigma=arc.relative_arc_length_error(1000),
            units=Units.UNITLESS,
            basis="1000-sample relative arc-length residual",
            samples=1000,
        ),
        frame=BLENDER_WORLD,
        control_points=[[float(v) for v in point] for point in control_points],
        orientation_points=[[float(v) for v in quat] for quat in orientation_points],
        focus_targets=focus_targets,
        focal_length_mm=[[float(a), float(b)] for a, b in focal_curve],
        exposure_curve=[[float(a), float(b)] for a, b in exposure],
        light_state_transitions=lights,
        beats=beat_list,
        arc_length_m=float(arc.arc_length_m),
        samples=samples,
        damping=float(damping),
        reduced_motion_views=reduced,
        skip_points=skips,
    )
    graph.notes.append(
        f"arc_length_m={graph.arc_length_m:.6f}; "
        f"max_scroll_distance_deviation={arc.max_scroll_distance_deviation(1000):.3e}"
    )
    return graph.seal()


FLAGSHIP_BEAT_SPECS: tuple[tuple[str, str, float, float, str], ...] = (
    ("00", "THRESHOLD", 0.00, 0.08, "centre"),
    ("01", "CAPACITY", 0.08, 0.18, "left_upper"),
    ("02", "INFERENCE", 0.18, 0.30, "right_upper"),
    ("03", "DISPATCH", 0.30, 0.42, "edge"),
    ("04", "EXECUTION", 0.42, 0.55, "centre"),
    ("05", "TURN", 0.55, 0.68, "centre"),
    ("06", "VERIFY", 0.68, 0.80, "right_upper"),
    ("07", "RECEIPT", 0.80, 0.90, "terminal_wall"),
    ("08", "ACCESS", 0.90, 1.00, "terminal_wall"),
)


def compose_flagship_datacentre_path(
    *,
    path_id: str = "flagship-datacentre-path",
    solids: Sequence[SolidGeometry] | None = None,
) -> CameraPathGraph:
    """Nine-beat flagship path: threshold through one continuous left turn to terminal."""
    # Corridor along +Y, single continuous left turn into +X second aisle.
    control_points = [
        [0.0, -18.0, 1.6],  # threshold exterior
        [0.0, -12.0, 1.6],  # approach
        [0.0, -6.0, 1.55],  # entry
        [0.0, 0.0, 1.5],  # main aisle
        [0.0, 6.0, 1.5],  # activation / mid aisle
        [0.0, 10.0, 1.5],  # deceleration before turn
        [-0.5, 13.5, 1.5],  # enter left turn (continuous, not a cut)
        [-4.0, 15.0, 1.5],  # mid turn
        [-8.0, 15.0, 1.5],  # second aisle
        [-14.0, 15.0, 1.45],  # verification terminal
    ]
    # Rack rows sit beside the aisle; camera stays in the clear volume.
    default_solids = solids
    if default_solids is None:
        default_solids = [
            SolidGeometry("rack_row_left", (-3.2, -10.0, 0.0), (-1.2, 12.0, 2.4)),
            SolidGeometry("rack_row_right", (1.2, -10.0, 0.0), (3.2, 12.0, 2.4)),
            SolidGeometry("rack_row_second", (-12.0, 16.2, 0.0), (-4.0, 18.2, 2.4)),
        ]

    beats = [
        NarrativeBeat(
            beat_id=beat_id,
            label=label,
            scroll_start=start,
            scroll_end=end,
            text_zone=zone,
            dwell=0.05 if label in {"THRESHOLD", "TURN", "ACCESS"} else 0.02,
            skippable=label not in {"THRESHOLD", "ACCESS"},
            reduced_motion_view={"scroll": (start + end) / 2.0, "static": True},
            text=[f"{beat_id} {label}"],
        )
        for beat_id, label, start, end, zone in FLAGSHIP_BEAT_SPECS
    ]

    focus_targets = [
        {"scroll": 0.0, "target": [0.0, -6.0, 1.4], "label": "entry"},
        {"scroll": 0.42, "target": [0.0, 8.0, 1.4], "label": "aisle_activation"},
        {"scroll": 0.62, "target": [-6.0, 15.0, 1.4], "label": "turn_apex"},
        {"scroll": 1.0, "target": [-16.0, 15.0, 1.3], "label": "terminal"},
    ]
    light_states = [
        {"scroll": 0.0, "state": "threshold_dim", "key_intensity": 0.4},
        {"scroll": 0.18, "state": "aisle_work", "key_intensity": 0.9},
        {"scroll": 0.55, "state": "turn_accent", "key_intensity": 0.7},
        {"scroll": 0.80, "state": "terminal_verify", "key_intensity": 1.0},
    ]

    return compose_camera_path(
        path_id=path_id,
        control_points=control_points,
        focus_targets=focus_targets,
        focal_length_mm=[[0.0, 28.0], [0.55, 35.0], [1.0, 40.0]],
        exposure_curve=[[0.0, -0.3], [0.3, 0.0], [0.8, 0.2], [1.0, 0.1]],
        light_state_transitions=light_states,
        beats=beats,
        solids=default_solids,
        skip_points=[0.55, 0.80],
        reduced_motion_views=[
            {"beat_id": "00", "position": [0.0, -18.0, 1.6], "label": "threshold_static"},
            {"beat_id": "08", "position": [-14.0, 15.0, 1.45], "label": "terminal_static"},
        ],
        damping=0.12,
        sample_count=256,
        input_authorities=[AuthorityClass.PROCEDURAL_GROUND_TRUTH.value],
    )


def arc_length_tables(graph: CameraPathGraph) -> tuple[ArcLengthSpline, QuaternionCurve]:
    """Rebuild evaluation curves from a sealed graph (deterministic inputs only)."""
    position = CatmullRomSpline(graph.control_points)
    arc = ArcLengthSpline(position)
    orientation = QuaternionCurve(graph.orientation_points)
    return arc, orientation
