#!/usr/bin/env python3
"""Generate deterministic critic fixtures (images + JSON metrics). No large binaries."""

from __future__ import annotations

import json
import math
from pathlib import Path

import numpy as np
from PIL import Image

ROOT = Path(__file__).resolve().parents[1]
FIXTURES = ROOT / "fixtures"


def _save_png(path: Path, array: np.ndarray) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    arr = np.asarray(array)
    if arr.ndim == 2:
        mode = "L"
        data = (np.clip(arr, 0, 1) * 255).astype(np.uint8)
    else:
        mode = "RGB"
        data = (np.clip(arr[..., :3], 0, 1) * 255).astype(np.uint8)
    Image.fromarray(data, mode=mode).save(path)


def _write_json(path: Path, payload: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def soft_disk(h: int, w: int, cx: float, cy: float, r: float, soft: float = 0.0) -> np.ndarray:
    yy, xx = np.mgrid[0:h, 0:w]
    dist = np.sqrt((xx - cx) ** 2 + (yy - cy) ** 2)
    if soft <= 0:
        return (dist <= r).astype(np.float64)
    return np.clip((r + soft - dist) / max(soft, 1e-6), 0.0, 1.0)


def generate_all() -> None:
    FIXTURES.mkdir(parents=True, exist_ok=True)
    h = w = 64

    # --- product photographer: soft silhouette / low separation / clipped hero ---
    mask_soft = soft_disk(h, w, 32, 32, 18, soft=10)
    image_low_sep = np.full((h, w, 3), 0.45)
    image_low_sep[mask_soft > 0.5] = 0.48
    image_clip = np.full((h, w, 3), 0.2)
    image_clip[mask_soft > 0.5] = 1.0
    _save_png(FIXTURES / "product_photographer" / "mask.png", mask_soft)
    _save_png(FIXTURES / "product_photographer" / "image_low_sep.png", image_low_sep)
    _save_png(FIXTURES / "product_photographer" / "image_clip.png", image_clip)
    _write_json(
        FIXTURES / "product_photographer" / "metrics.json",
        {"failure_modes": ["silhouette", "background_separation", "clipping"]},
    )

    # --- clean product control (asymmetric organic-safe mask) ---
    mask_hard = soft_disk(h, w, 32, 32, 18, soft=0)
    mask_control = mask_hard.copy()
    mask_control[10:25, 35:50] = 1.0
    mask_control[40:55, 12:28] = 0.0
    image_clean = np.full((h, w, 3), 0.08)
    image_clean[mask_control > 0.5] = (0.72, 0.55, 0.40)
    _save_png(FIXTURES / "control" / "mask.png", mask_control)
    _save_png(FIXTURES / "control" / "image.png", image_clean)

    # --- organic: perfect symmetry circle + smooth contour ---
    theta = np.linspace(0, 2 * math.pi, 64, endpoint=False)
    circle = np.stack([0.5 + 0.3 * np.cos(theta), 0.5 + 0.3 * np.sin(theta)], axis=1)
    _write_json(
        FIXTURES / "organic_artist" / "metrics.json",
        {"contour_xy": circle.tolist(), "use_mask": True},
    )
    # Exact left-right mirror disk so symmetry detector fires.
    sym_mask = np.zeros((h, w), dtype=np.float64)
    yy, xx = np.mgrid[0:h, 0:w]
    sym_mask[((xx - 31.5) ** 2 + (yy - 31.5) ** 2) <= 18**2] = 1.0
    _save_png(FIXTURES / "organic_artist" / "mask.png", sym_mask)

    # --- lighting: flat + overfilled + floating ---
    _write_json(
        FIXTURES / "lighting_artist" / "metrics.json",
        {
            "luminance_variance": 0.001,
            "shadow_floor": 0.45,
            "contact_shadow_strength": 0.01,
            "highlight_clip_fraction": 0.08,
        },
    )

    # --- material: fake metal, wrong pore, flat depth ---
    _write_json(
        FIXTURES / "material_artist" / "metrics.json",
        {
            "metalness": 0.95,
            "roughness": 0.85,
            "specular_peak": 0.15,
            "texture_scale_m": 0.05,
            "feature_scale_m": 0.001,
            "albedo_variance": 0.4,
            "normal_variance": 0.001,
        },
    )

    # --- environment: repetitive racks / empty box ---
    occupancy = np.zeros((8, 8, 8))
    occupancy[0, 0, 0] = 1.0
    _write_json(
        FIXTURES / "environment_artist" / "metrics.json",
        {
            "instance_variations": [0.5] * 24,
            "occupancy_grid": occupancy.tolist(),
            "depth_complexity_samples": [1.0, 1.0, 1.0, 1.0],
        },
    )

    # --- cinematographer: delayed camera / dead scroll ---
    _write_json(
        FIXTURES / "cinematographer" / "metrics.json",
        {
            "shot_positions": [[0, 0, 1]] * 10,
            "camera_lag_vs_scroll": 0.35,
            "dead_scroll_gaps": [[0.2, 0.55], [0.8, 1.0]],
            "turn_intent_score": 0.2,
        },
    )

    # --- industrial designer ---
    _write_json(
        FIXTURES / "industrial_designer" / "metrics.json",
        {
            "declared_dimensions_m": {"x": 0.4, "y": 0.8, "z": 0.4},
            "observed_dimensions_m": {"x": 0.4, "y": 1.5, "z": 0.4},
            "part_count": 1,
            "expected_min_parts": 6,
            "drawer_depth_samples_m": [0.0, 0.0, 0.0, 0.0],
        },
    )

    # --- groom ---
    _write_json(
        FIXTURES / "groom_artist" / "metrics.json",
        {
            "fur_clump_scale_m": 0.4,
            "body_scale_m": 0.5,
            "fur_density_per_m2": 2.0,
        },
    )

    # --- editorial: generic composition / text collision volume ---
    _write_json(
        FIXTURES / "editorial_art_director" / "metrics.json",
        {
            "salient_xy": [0.5, 0.5],
            "template_similarity": 0.92,
            "narrative_beats": [
                {
                    "beat_id": "b0",
                    "scroll_start": 0.0,
                    "scroll_end": 0.2,
                    "text": ["word " * 30],
                }
            ],
        },
    )

    # --- interaction: latency / dead zones / discoverability ---
    _write_json(
        FIXTURES / "interaction_designer" / "metrics.json",
        {
            "response_latency_ms": 280.0,
            "dead_zone_fraction": 0.25,
            "skip_discoverability": 0.1,
            "get_app_discoverability": 0.15,
        },
    )

    # --- accessibility ---
    _write_json(
        FIXTURES / "accessibility_reviewer" / "metrics.json",
        {
            "fg_luminance": 0.4,
            "bg_luminance": 0.45,
            "focus_order_completeness": 0.4,
            "reduced_motion_equivalence": 0.5,
            "textual_equivalent_presence": 0.0,
        },
    )

    # --- performance regression (real sample vector, not simulated flag) ---
    _write_json(
        FIXTURES / "performance_engineer" / "metrics.json",
        {
            "frame_times_ms": [18, 20, 22, 25, 30, 40, 45, 50, 55, 60],
            "long_task_count": 3,
            "javascript_heap_growth_bytes": 20_000_000,
            "cumulative_layout_shift": 0.35,
            "measurements_are_simulated": False,
        },
    )

    # --- adversarial ---
    _write_json(
        FIXTURES / "adversarial_acceptance_reviewer" / "metrics.json",
        {
            "reference_class_declared": "held_out_multiview_v1",
            "reference_class_actual": "training_views_only",
            "hidden_view_score_delta": 0.4,
            "threshold_original": 0.02,
            "threshold_applied": 0.15,
            "threshold_higher_is_easier": True,
            "detail_removed_fraction": 0.55,
            "budget_claim_met": True,
        },
    )

    # --- mobile crop failure (product photographer secondary) ---
    crop = image_clean[:, 20:44]
    _save_png(FIXTURES / "mobile_crop_failure" / "image.png", crop)
    _save_png(FIXTURES / "mobile_crop_failure" / "mask.png", mask_hard[:, 20:44] * 0.3)

    # --- text collision (editorial secondary) ---
    _write_json(
        FIXTURES / "text_collision" / "metrics.json",
        {
            "salient_xy": [0.5, 0.5],
            "template_similarity": 0.2,
            "narrative_beats": [
                {
                    "beat_id": "b0",
                    "scroll_start": 0.0,
                    "scroll_end": 0.05,
                    "text": ["collide " * 40],
                }
            ],
        },
    )

    # --- control metrics for non-image critics ---
    _write_json(
        FIXTURES / "control" / "metrics.json",
        {
            "shot_positions": [
                [0, 0, 1],
                [1, 0, 1],
                [0, 1, 1],
                [-1, 0, 1],
                [0, -1, 1],
                [0.5, 0.5, 1],
            ],
            "camera_lag_vs_scroll": 0.04,
            "dead_scroll_gaps": [],
            "turn_intent_score": 0.9,
            "declared_dimensions_m": {"x": 0.4, "y": 0.8, "z": 0.4},
            "observed_dimensions_m": {"x": 0.4, "y": 0.8, "z": 0.4},
            "part_count": 8,
            "expected_min_parts": 6,
            "drawer_depth_samples_m": [0.0, 0.05, 0.1, 0.15],
            "instance_variations": [float(i) * 0.13 for i in range(32)],
            "occupancy_grid": np.ones((4, 4, 4)).tolist(),
            "depth_complexity_samples": [3.0, 4.0, 3.5, 5.0],
            "metalness": 0.05,
            "roughness": 0.4,
            "specular_peak": 0.8,
            "texture_scale_m": 0.001,
            "feature_scale_m": 0.001,
            "albedo_variance": 0.1,
            "normal_variance": 0.08,
            "luminance_variance": 0.03,
            "shadow_floor": 0.1,
            "contact_shadow_strength": 0.25,
            "highlight_clip_fraction": 0.005,
            "fur_clump_scale_m": 0.04,
            "body_scale_m": 0.5,
            "fur_density_per_m2": 150.0,
            "salient_xy": [1 / 3, 1 / 3],
            "template_similarity": 0.2,
            "narrative_beats": [
                {"beat_id": "b0", "scroll_start": 0.0, "scroll_end": 1.0, "text": ["ok"]}
            ],
            "response_latency_ms": 30.0,
            "dead_zone_fraction": 0.01,
            "skip_discoverability": 0.9,
            "get_app_discoverability": 0.85,
            "fg_luminance": 0.0,
            "bg_luminance": 1.0,
            "contrast_ratio": 21.0,
            "focus_order_completeness": 1.0,
            "reduced_motion_equivalence": 1.0,
            "textual_equivalent_presence": 1.0,
            "frame_times_ms": [14, 15, 15, 16, 16, 16.5, 17, 18],
            "long_task_count": 0,
            "javascript_heap_growth_bytes": 500_000,
            "cumulative_layout_shift": 0.02,
            "measurements_are_simulated": False,
            "reference_class_declared": "held_out_multiview_v1",
            "reference_class_actual": "held_out_multiview_v1",
            "hidden_view_score_delta": 0.01,
            "threshold_original": 0.02,
            "threshold_applied": 0.02,
            "threshold_higher_is_easier": True,
            "detail_removed_fraction": 0.02,
            "budget_claim_met": True,
            "contour_xy": (
                np.stack(
                    [
                        0.5 + 0.3 * np.cos(theta) + 0.08 * np.sin(7 * theta),
                        0.5 + 0.3 * np.sin(theta) + 0.07 * np.cos(5 * theta),
                    ],
                    axis=1,
                ).tolist()
            ),
        },
    )

    print(f"wrote fixtures under {FIXTURES}")


if __name__ == "__main__":
    generate_all()
