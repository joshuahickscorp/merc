"""Shared measurable detectors used by specialist critics."""

from __future__ import annotations

import math
from typing import Any

import numpy as np


def as_float_array(value: Any) -> np.ndarray:
    array = np.asarray(value, dtype=np.float64)
    if array.size == 0:
        raise ValueError("empty array cannot be measured")
    return array


def luminance(rgb: np.ndarray) -> np.ndarray:
    """Relative luminance from sRGB-like arrays shaped HxWx3 or HxWx4."""
    arr = as_float_array(rgb)
    if arr.ndim == 2:
        return arr
    if arr.ndim != 3 or arr.shape[2] < 3:
        raise ValueError("expected HxW or HxWx3/4 image")
    channels = arr[..., :3]
    if channels.max() > 1.0:
        channels = channels / 255.0
    return 0.2126 * channels[..., 0] + 0.7152 * channels[..., 1] + 0.0722 * channels[..., 2]


def silhouette_edge_strength(mask: np.ndarray) -> float:
    """Mean absolute gradient magnitude on the binary silhouette boundary."""
    m = (as_float_array(mask) > 0.5).astype(np.float64)
    if m.ndim != 2:
        raise ValueError("silhouette mask must be 2D")
    gy, gx = np.gradient(m)
    edge = np.hypot(gx, gy)
    boundary = edge > 0
    if not np.any(boundary):
        return 0.0
    # Strength is mean edge magnitude on boundary pixels; higher is more readable.
    return float(edge[boundary].mean())


def background_separation(image: np.ndarray, mask: np.ndarray) -> float:
    """Absolute mean luminance difference between foreground and background."""
    lum = luminance(image)
    m = as_float_array(mask) > 0.5
    if not np.any(m) or np.all(m):
        return 0.0
    return float(abs(lum[m].mean() - lum[~m].mean()))


def highlight_clip_fraction(image: np.ndarray, threshold: float = 0.98) -> float:
    lum = luminance(image)
    return float(np.mean(lum >= threshold))


def shannon_entropy(values: list[float] | np.ndarray, bins: int = 16) -> float:
    arr = as_float_array(values).ravel()
    if arr.size == 0:
        return 0.0
    hist, _ = np.histogram(arr, bins=bins, density=True)
    hist = hist[hist > 0]
    if hist.size == 0:
        return 0.0
    # Density * bin_width is not probability; renormalize to a discrete pmf.
    probs = hist / hist.sum()
    return float(-(probs * np.log2(probs)).sum())


def left_right_symmetry(mask: np.ndarray) -> float:
    """Correlation of left/right halves; 1.0 is perfect mirror symmetry."""
    m = (as_float_array(mask) > 0.5).astype(np.float64)
    if m.ndim != 2:
        raise ValueError("mask must be 2D")
    h, w = m.shape
    half = w // 2
    if half < 1:
        return 1.0
    left = m[:, :half]
    right = np.fliplr(m[:, w - half :])
    left_z = left - left.mean()
    right_z = right - right.mean()
    denom = float(np.linalg.norm(left_z) * np.linalg.norm(right_z))
    if denom < 1e-12:
        return 1.0
    return float(np.clip(np.vdot(left_z, right_z) / denom, -1.0, 1.0))


def curvature_irregularity(contour_xy: np.ndarray) -> float:
    """Variance of discrete turning angles along a closed 2D contour."""
    pts = as_float_array(contour_xy)
    if pts.ndim != 2 or pts.shape[1] != 2 or pts.shape[0] < 4:
        return 0.0
    deltas = np.diff(np.vstack([pts, pts[:1]]), axis=0)
    angles = np.arctan2(deltas[:, 1], deltas[:, 0])
    turns = np.diff(np.unwrap(np.append(angles, angles[0])))
    return float(np.var(turns))


def contrast_ratio(fg_luminance: float, bg_luminance: float) -> float:
    """WCAG relative-luminance contrast ratio."""
    l1 = max(float(fg_luminance), float(bg_luminance))
    l2 = min(float(fg_luminance), float(bg_luminance))
    return (l1 + 0.05) / (l2 + 0.05)


def percentile(values: list[float] | np.ndarray, q: float) -> float:
    arr = as_float_array(values).ravel()
    if arr.size == 0:
        return 0.0
    return float(np.percentile(arr, q))


def depth_variance(depths: list[float] | np.ndarray) -> float:
    arr = as_float_array(depths).ravel()
    if arr.size < 2:
        return 0.0
    return float(np.var(arr))


def occupied_volume_fraction(occupancy: np.ndarray) -> float:
    grid = as_float_array(occupancy)
    return float(np.mean(grid > 0.5))


def dead_scroll_fraction(gaps: list[tuple[float, float]]) -> float:
    total = 0.0
    for start, end in gaps:
        total += max(0.0, float(end) - float(start))
    return float(min(1.0, total))


def text_volume_per_beat(beats: list[dict[str, Any]]) -> float:
    if not beats:
        return 0.0
    volumes = []
    for beat in beats:
        text = beat.get("text") or []
        if isinstance(text, str):
            words = len(text.split())
        else:
            words = sum(len(str(item).split()) for item in text)
        span = max(1e-6, float(beat.get("scroll_end", 1.0)) - float(beat.get("scroll_start", 0.0)))
        volumes.append(words / span)
    return float(sum(volumes) / len(volumes))


def composition_center_bias(salient_xy: tuple[float, float] | list[float]) -> float:
    """Distance of salient point from nearest rule-of-thirds intersection (0..~0.47)."""
    x, y = float(salient_xy[0]), float(salient_xy[1])
    thirds = (1.0 / 3.0, 2.0 / 3.0)
    best = min(math.hypot(x - tx, y - ty) for tx in thirds for ty in thirds)
    return float(best)


def plastic_metal_score(metalness: float, roughness: float, specular_peak: float) -> float:
    """Higher means more 'plastic pretending to be metal': high metal + soft broad specular."""
    # Real metals tend to low roughness with sharp specular; plastic-metal is high metalness
    # with high roughness and a broad, weak specular peak.
    return float(max(0.0, metalness) * max(0.0, roughness) * max(0.0, 1.0 - specular_peak))


def pore_scale_ratio(texture_scale_m: float, feature_scale_m: float) -> float:
    if feature_scale_m <= 0:
        return float("inf")
    return float(texture_scale_m / feature_scale_m)


def flat_depth_pretence(albedo_var: float, normal_var: float) -> float:
    """High when albedo varies a lot but normals are nearly constant (painted depth)."""
    return float(albedo_var / max(normal_var, 1e-8))
