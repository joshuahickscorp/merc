from __future__ import annotations

import math
from pathlib import Path
from typing import Any

import cv2
import numpy as np
from PIL import Image, ImageOps

PROJECTION_ENGINE_V1 = "visual_geometry_projection_v1_luminance"
PROJECTION_ENGINE_V2 = "visual_geometry_projection_v2_alpha_aware"


def _binary_mask(path: Path, *, alpha_aware: bool = True) -> np.ndarray:
    with Image.open(path) as image:
        normalized = ImageOps.exif_transpose(image).convert("RGBA")
        alpha = normalized.getchannel("A")
        channel = (
            alpha
            if alpha_aware and alpha.getextrema()[0] < alpha.getextrema()[1]
            else normalized.convert("L")
        )
        value = np.asarray(channel, dtype=np.uint8)
    return value >= 128


def _gray(path: Path, size: tuple[int, int] | None = None) -> np.ndarray:
    with Image.open(path) as image:
        value = ImageOps.exif_transpose(image).convert("L")
        if size is not None and value.size != size:
            value = value.resize(size, Image.Resampling.LANCZOS)
        return np.asarray(value, dtype=np.uint8)


def _boundary(mask: np.ndarray) -> np.ndarray:
    kernel = np.ones((3, 3), dtype=np.uint8)
    return cv2.morphologyEx(mask.astype(np.uint8), cv2.MORPH_GRADIENT, kernel) > 0


def _distances(source: np.ndarray, target: np.ndarray) -> np.ndarray:
    if not source.any():
        return np.empty(0, dtype=np.float32)
    if not target.any():
        return np.full(int(source.sum()), float(max(source.shape)), dtype=np.float32)
    distance = cv2.distanceTransform((~target).astype(np.uint8), cv2.DIST_L2, 5)
    return distance[source]


def projection_metrics(
    reference_mask_path: Path,
    render_mask_path: Path,
    *,
    residual_path: Path | None = None,
    overlay_path: Path | None = None,
    engine: str = PROJECTION_ENGINE_V2,
) -> dict[str, Any]:
    if engine not in {PROJECTION_ENGINE_V1, PROJECTION_ENGINE_V2}:
        raise ValueError(f"unsupported projection metric engine: {engine}")
    alpha_aware = engine == PROJECTION_ENGINE_V2
    reference = _binary_mask(reference_mask_path, alpha_aware=alpha_aware)
    render = _binary_mask(render_mask_path, alpha_aware=alpha_aware)
    if render.shape != reference.shape:
        render = (
            cv2.resize(
                render.astype(np.uint8),
                (reference.shape[1], reference.shape[0]),
                interpolation=cv2.INTER_NEAREST,
            )
            > 0
        )
    true_positive = int(np.logical_and(reference, render).sum())
    false_positive = int(np.logical_and(~reference, render).sum())
    false_negative = int(np.logical_and(reference, ~render).sum())
    true_negative = int(np.logical_and(~reference, ~render).sum())
    union = true_positive + false_positive + false_negative
    precision = true_positive / max(1, true_positive + false_positive)
    recall = true_positive / max(1, true_positive + false_negative)
    iou = true_positive / union if union else 1.0
    dice = 2 * true_positive / max(1, 2 * true_positive + false_positive + false_negative)
    reference_boundary = _boundary(reference)
    render_boundary = _boundary(render)
    reference_to_render = _distances(reference_boundary, render_boundary)
    render_to_reference = _distances(render_boundary, reference_boundary)
    symmetric = np.concatenate((reference_to_render, render_to_reference))
    diagonal = math.hypot(reference.shape[1], reference.shape[0])
    boundary_rmse = float(np.sqrt(np.mean(np.square(symmetric)))) if symmetric.size else 0.0
    boundary_chamfer = float(np.mean(symmetric)) if symmetric.size else 0.0
    boundary_p95 = float(np.percentile(symmetric, 95)) if symmetric.size else 0.0

    if residual_path is not None or overlay_path is not None:
        pixels = np.zeros((*reference.shape, 4), dtype=np.uint8)
        pixels[np.logical_and(reference, render)] = (32, 220, 96, 60)
        pixels[np.logical_and(reference, ~render)] = (48, 96, 255, 230)
        pixels[np.logical_and(~reference, render)] = (255, 48, 32, 230)
        destination = residual_path or overlay_path
        assert destination is not None
        destination.parent.mkdir(parents=True, exist_ok=True)
        Image.fromarray(pixels, "RGBA").save(destination, format="PNG")
        if overlay_path is not None and overlay_path != destination:
            Image.fromarray(pixels, "RGBA").save(overlay_path, format="PNG")

    return {
        "silhouette_iou": round(iou, 8),
        "silhouette_dice": round(dice, 8),
        "foreground_precision": round(precision, 8),
        "foreground_recall": round(recall, 8),
        "boundary_rmse_px": round(boundary_rmse, 8),
        "boundary_chamfer_px": round(boundary_chamfer, 8),
        "boundary_p95_px": round(boundary_p95, 8),
        "boundary_rmse_fraction_of_diagonal": round(boundary_rmse / max(diagonal, 1.0), 10),
        "comparison_size": [int(reference.shape[1]), int(reference.shape[0])],
        "counts": {
            "true_positive": true_positive,
            "false_positive": false_positive,
            "false_negative": false_negative,
            "true_negative": true_negative,
            "reference_boundary_pixels": int(reference_boundary.sum()),
            "render_boundary_pixels": int(render_boundary.sum()),
        },
    }


def edge_structure_metrics(
    reference_path: Path,
    candidate_path: Path,
    *,
    object_mask_path: Path | None = None,
    overlay_path: Path | None = None,
    tolerance_px: int = 2,
) -> dict[str, Any]:
    reference = _gray(reference_path)
    candidate = _gray(candidate_path, (reference.shape[1], reference.shape[0]))
    reference_edges = cv2.Canny(reference, 50, 150) > 0
    candidate_edges = cv2.Canny(candidate, 50, 150) > 0
    if object_mask_path is not None:
        mask = _binary_mask(object_mask_path)
        if mask.shape != reference_edges.shape:
            mask = (
                cv2.resize(
                    mask.astype(np.uint8),
                    (reference.shape[1], reference.shape[0]),
                    interpolation=cv2.INTER_NEAREST,
                )
                > 0
            )
        reference_edges &= mask
        candidate_edges &= mask
    kernel_size = max(1, tolerance_px * 2 + 1)
    kernel = np.ones((kernel_size, kernel_size), dtype=np.uint8)
    reference_near = cv2.dilate(reference_edges.astype(np.uint8), kernel) > 0
    candidate_near = cv2.dilate(candidate_edges.astype(np.uint8), kernel) > 0
    matched_candidate = np.logical_and(candidate_edges, reference_near)
    matched_reference = np.logical_and(reference_edges, candidate_near)
    precision = float(matched_candidate.sum()) / max(1, int(candidate_edges.sum()))
    recall = float(matched_reference.sum()) / max(1, int(reference_edges.sum()))
    f1 = 2 * precision * recall / max(1e-12, precision + recall)
    distances = np.concatenate(
        (_distances(reference_edges, candidate_edges), _distances(candidate_edges, reference_edges))
    )
    displacement = float(np.mean(distances)) if distances.size else 0.0
    displacement_p95 = float(np.percentile(distances, 95)) if distances.size else 0.0

    reference_x = cv2.Sobel(reference, cv2.CV_32F, 1, 0, ksize=3)
    reference_y = cv2.Sobel(reference, cv2.CV_32F, 0, 1, ksize=3)
    candidate_x = cv2.Sobel(candidate, cv2.CV_32F, 1, 0, ksize=3)
    candidate_y = cv2.Sobel(candidate, cv2.CV_32F, 0, 1, ksize=3)
    exact = np.logical_and(reference_edges, candidate_edges)
    orientation_error = None
    if exact.any():
        reference_angle = np.arctan2(reference_y[exact], reference_x[exact])
        candidate_angle = np.arctan2(candidate_y[exact], candidate_x[exact])
        difference = np.abs(np.angle(np.exp(1j * (reference_angle - candidate_angle))))
        difference = np.minimum(difference, np.pi - np.minimum(difference, np.pi))
        orientation_error = float(np.degrees(np.mean(difference)))

    if overlay_path is not None:
        overlay = np.zeros((*reference.shape, 4), dtype=np.uint8)
        overlay[reference_edges] = (255, 64, 48, 210)
        overlay[candidate_edges] = (32, 180, 255, 210)
        overlay[np.logical_and(reference_edges, candidate_edges)] = (64, 255, 128, 230)
        overlay_path.parent.mkdir(parents=True, exist_ok=True)
        Image.fromarray(overlay, "RGBA").save(overlay_path, format="PNG")

    return {
        "edge_precision": round(precision, 8),
        "edge_recall": round(recall, 8),
        "edge_f1": round(f1, 8),
        "edge_displacement_px": round(displacement, 8),
        "edge_displacement_p95_px": round(displacement_p95, 8),
        "edge_orientation_error_degrees": (
            round(orientation_error, 8) if orientation_error is not None else None
        ),
        "tolerance_px": tolerance_px,
        "reference_edge_pixels": int(reference_edges.sum()),
        "candidate_edge_pixels": int(candidate_edges.sum()),
        "authority": "DIAGNOSTIC_UNCLASSIFIED_EDGES",
        "limitations": [
            "reference edges may include reflection, shadow, texture, or material boundaries",
            "edge metrics cannot establish geometry authority without reviewed edge classification",
        ],
    }


def perceptual_diagnostic(reference_path: Path, candidate_path: Path) -> dict[str, Any]:
    reference = _gray(reference_path)
    candidate = _gray(candidate_path, (reference.shape[1], reference.shape[0]))
    reference_float = reference.astype(np.float32) / 255.0
    candidate_float = candidate.astype(np.float32) / 255.0
    absolute = np.abs(reference_float - candidate_float)
    ref_gradient = cv2.Laplacian(reference_float, cv2.CV_32F).ravel()
    candidate_gradient = cv2.Laplacian(candidate_float, cv2.CV_32F).ravel()
    correlation = 0.0
    if float(ref_gradient.std()) > 1e-9 and float(candidate_gradient.std()) > 1e-9:
        correlation = float(np.corrcoef(ref_gradient, candidate_gradient)[0, 1])
    reference_hist = cv2.calcHist([reference], [0], None, [32], [0, 256]).ravel()
    candidate_hist = cv2.calcHist([candidate], [0], None, [32], [0, 256]).ravel()
    reference_hist /= max(1.0, float(reference_hist.sum()))
    candidate_hist /= max(1.0, float(candidate_hist.sum()))
    return {
        "mean_absolute_luminance_error": round(float(absolute.mean()), 8),
        "p95_absolute_luminance_error": round(float(np.percentile(absolute, 95)), 8),
        "gradient_correlation": round(correlation, 8),
        "histogram_l1_distance": round(float(np.abs(reference_hist - candidate_hist).sum()), 8),
        "authority": "SUPPLEMENTAL_DIAGNOSTIC_ONLY",
    }
