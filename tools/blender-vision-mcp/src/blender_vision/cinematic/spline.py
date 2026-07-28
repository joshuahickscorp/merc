"""Centripetal Catmull-Rom position spline and quaternion orientation curve.

Arc-length parameterisation is built by dense numeric integration into a
monotone lookup table so equal scroll delta maps to equal world distance.
"""

from __future__ import annotations

from collections.abc import Sequence
from dataclasses import dataclass, field

import numpy as np
from numpy.typing import NDArray


def _as_points(points: Sequence[Sequence[float]]) -> NDArray[np.float64]:
    array = np.asarray(points, dtype=np.float64)
    if array.ndim != 2 or array.shape[1] != 3:
        raise ValueError("control points must be an (N, 3) sequence of XYZ positions")
    if array.shape[0] < 2:
        raise ValueError("at least two control points are required")
    return array


def _as_quaternions(quats: Sequence[Sequence[float]]) -> NDArray[np.float64]:
    array = np.asarray(quats, dtype=np.float64)
    if array.ndim != 2 or array.shape[1] != 4:
        raise ValueError("orientation points must be (N, 4) wxyz quaternions")
    if array.shape[0] < 2:
        raise ValueError("at least two orientation points are required")
    # Continuously flip so consecutive quats stay in the same hemisphere.
    out = array.copy()
    norms = np.linalg.norm(out, axis=1, keepdims=True)
    if np.any(norms < 1e-12):
        raise ValueError("zero-length quaternion is not a valid orientation")
    out = out / norms
    for index in range(1, len(out)):
        if float(np.dot(out[index - 1], out[index])) < 0.0:
            out[index] = -out[index]
    return out


def _catmull_rom_segment(
    p0: NDArray[np.float64],
    p1: NDArray[np.float64],
    p2: NDArray[np.float64],
    p3: NDArray[np.float64],
    t: float,
    *,
    alpha: float = 0.5,
) -> NDArray[np.float64]:
    """Evaluate one centripetal (alpha=0.5) Catmull-Rom segment at local t in [0,1]."""

    def tj(ti: float, pi: NDArray[np.float64], pj: NDArray[np.float64]) -> float:
        return ti + float(np.linalg.norm(pj - pi) ** alpha)

    t0 = 0.0
    t1 = tj(t0, p0, p1)
    t2 = tj(t1, p1, p2)
    t3 = tj(t2, p2, p3)
    # Degenerate equal points collapse the chord parameter; fall back to uniform.
    if t1 <= t0 + 1e-12 or t2 <= t1 + 1e-12 or t3 <= t2 + 1e-12:
        return (1.0 - t) * p1 + t * p2
    tt = t1 + t * (t2 - t1)

    def lerp(a: NDArray[np.float64], b: NDArray[np.float64], ta: float, tb: float, u: float):
        if abs(tb - ta) < 1e-12:
            return a.copy()
        return a + (b - a) * ((u - ta) / (tb - ta))

    a1 = lerp(p0, p1, t0, t1, tt)
    a2 = lerp(p1, p2, t1, t2, tt)
    a3 = lerp(p2, p3, t2, t3, tt)
    b1 = lerp(a1, a2, t0, t2, tt)
    b2 = lerp(a2, a3, t1, t3, tt)
    return lerp(b1, b2, t1, t2, tt)


@dataclass(slots=True)
class CatmullRomSpline:
    """Piecewise centripetal Catmull-Rom through the given control points."""

    control_points: NDArray[np.float64]
    _segment_count: int = field(init=False, repr=False)

    def __post_init__(self) -> None:
        self.control_points = _as_points(self.control_points)
        self._segment_count = max(1, len(self.control_points) - 1)

    def evaluate(self, u: float) -> NDArray[np.float64]:
        """Evaluate at uniform parameter u in [0, 1] across the whole polyline."""
        u = float(np.clip(u, 0.0, 1.0))
        if self._segment_count == 1:
            p0 = self.control_points[0]
            p1 = self.control_points[0]
            p2 = self.control_points[1]
            p3 = self.control_points[1]
            return _catmull_rom_segment(p0, p1, p2, p3, u)
        scaled = u * self._segment_count
        index = min(int(scaled), self._segment_count - 1)
        local = scaled - index
        points = self.control_points
        p1 = points[index]
        p2 = points[index + 1]
        p0 = points[index - 1] if index > 0 else (2.0 * p1 - p2)
        p3 = points[index + 2] if index + 2 < len(points) else (2.0 * p2 - p1)
        return _catmull_rom_segment(p0, p1, p2, p3, local)

    def sample(self, count: int) -> NDArray[np.float64]:
        if count < 2:
            raise ValueError("sample count must be at least 2")
        us = np.linspace(0.0, 1.0, count, dtype=np.float64)
        return np.vstack([self.evaluate(float(u)) for u in us])


def _slerp(q0: NDArray[np.float64], q1: NDArray[np.float64], t: float) -> NDArray[np.float64]:
    q0 = q0 / np.linalg.norm(q0)
    q1 = q1 / np.linalg.norm(q1)
    dot = float(np.clip(np.dot(q0, q1), -1.0, 1.0))
    if dot < 0.0:
        q1 = -q1
        dot = -dot
    if dot > 0.9995:
        result = q0 + t * (q1 - q0)
        return result / np.linalg.norm(result)
    theta_0 = float(np.arccos(dot))
    sin_theta_0 = float(np.sin(theta_0))
    theta = theta_0 * t
    s0 = float(np.sin(theta_0 - theta) / sin_theta_0)
    s1 = float(np.sin(theta) / sin_theta_0)
    return s0 * q0 + s1 * q1


def _squad(
    q0: NDArray[np.float64],
    q1: NDArray[np.float64],
    q2: NDArray[np.float64],
    q3: NDArray[np.float64],
    t: float,
) -> NDArray[np.float64]:
    """Spherical cubic interpolation using intermediate tangents (squad)."""

    def log_diff(a: NDArray[np.float64], b: NDArray[np.float64]) -> NDArray[np.float64]:
        # Log of a^{-1} * b as pure-vector quaternion part.
        inv = np.array([a[0], -a[1], -a[2], -a[3]], dtype=np.float64)
        prod = _quat_mul(inv, b)
        if prod[0] < 0.0:
            prod = -prod
        prod = prod / np.linalg.norm(prod)
        w = float(np.clip(prod[0], -1.0, 1.0))
        vec = prod[1:]
        n = float(np.linalg.norm(vec))
        if n < 1e-12:
            return np.zeros(3, dtype=np.float64)
        angle = float(np.arccos(w))
        return vec * (angle / n)

    def exp_map(v: NDArray[np.float64]) -> NDArray[np.float64]:
        n = float(np.linalg.norm(v))
        if n < 1e-12:
            return np.array([1.0, 0.0, 0.0, 0.0], dtype=np.float64)
        half = n
        s = float(np.sin(half) / n)
        return np.array([float(np.cos(half)), v[0] * s, v[1] * s, v[2] * s], dtype=np.float64)

    def intermediate(
        prev: NDArray[np.float64], curr: NDArray[np.float64], nxt: NDArray[np.float64]
    ):
        # Softened squad control point; falls back to identity tangent when neighbours coincide.
        log_term = log_diff(curr, prev) + log_diff(curr, nxt)
        return _quat_mul(curr, exp_map(-0.25 * log_term))

    a = intermediate(q0, q1, q2)
    b = intermediate(q1, q2, q3)
    slerp_q = _slerp(q1, q2, t)
    slerp_ab = _slerp(a, b, t)
    return _slerp(slerp_q, slerp_ab, 2.0 * t * (1.0 - t))


def _quat_mul(a: NDArray[np.float64], b: NDArray[np.float64]) -> NDArray[np.float64]:
    w1, x1, y1, z1 = a
    w2, x2, y2, z2 = b
    return np.array(
        [
            w1 * w2 - x1 * x2 - y1 * y2 - z1 * z2,
            w1 * x2 + x1 * w2 + y1 * z2 - z1 * y2,
            w1 * y2 - x1 * z2 + y1 * w2 + z1 * x2,
            w1 * z2 + x1 * y2 - y1 * x2 + z1 * w2,
        ],
        dtype=np.float64,
    )


@dataclass(slots=True)
class QuaternionCurve:
    """Squad/slerp orientation curve over wxyz quaternions, no hemisphere flips."""

    quaternions: NDArray[np.float64]
    _segment_count: int = field(init=False, repr=False)

    def __post_init__(self) -> None:
        self.quaternions = _as_quaternions(self.quaternions)
        self._segment_count = max(1, len(self.quaternions) - 1)

    def evaluate(self, u: float, *, use_squad: bool = False) -> NDArray[np.float64]:
        """Evaluate orientation at uniform parameter u in [0, 1].

        Default is piecewise slerp, which preserves hemisphere continuity when
        control quaternions are already flipped-consistent. Optional squad is
        available for smoother cubic segments but can overshoot near long arcs.
        """
        u = float(np.clip(u, 0.0, 1.0))
        if self._segment_count == 1:
            return _slerp(self.quaternions[0], self.quaternions[1], u)
        scaled = u * self._segment_count
        index = min(int(scaled), self._segment_count - 1)
        local = scaled - index
        qs = self.quaternions
        q1 = qs[index]
        q2 = qs[index + 1]
        if not use_squad:
            return _slerp(q1, q2, local)
        q0 = qs[index - 1] if index > 0 else q1
        q3 = qs[index + 2] if index + 2 < len(qs) else q2
        return _squad(q0, q1, q2, q3, local)

    def consecutive_dot_products(self, count: int = 256) -> NDArray[np.float64]:
        samples = np.vstack(
            [self.evaluate(float(u)) for u in np.linspace(0.0, 1.0, count, dtype=np.float64)]
        )
        dots = np.sum(samples[:-1] * samples[1:], axis=1)
        return dots


@dataclass(slots=True)
class ArcLengthSpline:
    """Position spline reparameterised by normalised arc length s in [0, 1]."""

    position: CatmullRomSpline
    table_samples: int = 4096
    _u_table: NDArray[np.float64] = field(init=False, repr=False)
    _s_table: NDArray[np.float64] = field(init=False, repr=False)
    arc_length_m: float = field(init=False)

    def __post_init__(self) -> None:
        if self.table_samples < 64:
            raise ValueError("table_samples must be at least 64")
        us = np.linspace(0.0, 1.0, self.table_samples, dtype=np.float64)
        points = np.vstack([self.position.evaluate(float(u)) for u in us])
        deltas = np.linalg.norm(np.diff(points, axis=0), axis=1)
        cumulative = np.concatenate([[0.0], np.cumsum(deltas)])
        total = float(cumulative[-1])
        if total < 1e-12:
            raise ValueError("control points are coincident; arc length is zero")
        # Enforce strict monotonicity for searchsorted.
        for index in range(1, len(cumulative)):
            if cumulative[index] <= cumulative[index - 1]:
                cumulative[index] = cumulative[index - 1] + 1e-15
        self.arc_length_m = total
        self._u_table = us
        self._s_table = cumulative / cumulative[-1]

    def u_from_s(self, s: float) -> float:
        s = float(np.clip(s, 0.0, 1.0))
        index = int(np.searchsorted(self._s_table, s, side="left"))
        if index <= 0:
            return float(self._u_table[0])
        if index >= len(self._s_table):
            return float(self._u_table[-1])
        s0 = float(self._s_table[index - 1])
        s1 = float(self._s_table[index])
        u0 = float(self._u_table[index - 1])
        u1 = float(self._u_table[index])
        if s1 <= s0:
            return u1
        blend = (s - s0) / (s1 - s0)
        return u0 + blend * (u1 - u0)

    def evaluate(self, s: float) -> NDArray[np.float64]:
        return self.position.evaluate(self.u_from_s(s))

    def sample(self, count: int) -> NDArray[np.float64]:
        if count < 2:
            raise ValueError("sample count must be at least 2")
        ss = np.linspace(0.0, 1.0, count, dtype=np.float64)
        return np.vstack([self.evaluate(float(s)) for s in ss])

    def measured_arc_length(self, count: int = 1000) -> float:
        points = self.sample(count)
        return float(np.sum(np.linalg.norm(np.diff(points, axis=0), axis=1)))

    def relative_arc_length_error(self, count: int = 1000) -> float:
        measured = self.measured_arc_length(count)
        return abs(measured - self.arc_length_m) / self.arc_length_m

    def max_scroll_distance_deviation(self, count: int = 1000) -> float:
        """Max |Δscroll − Δnormalised-distance| between consecutive equal samples.

        With arc-length parameterisation both should be ~1/(count-1); residual is
        the reparameterisation error after table interpolation.
        """
        ss = np.linspace(0.0, 1.0, count, dtype=np.float64)
        points = np.vstack([self.evaluate(float(s)) for s in ss])
        seg = np.linalg.norm(np.diff(points, axis=0), axis=1)
        total = float(np.sum(seg))
        if total < 1e-12:
            return 1.0
        norm_dist = seg / total
        scroll_delta = np.diff(ss)
        return float(np.max(np.abs(norm_dist - scroll_delta)))
