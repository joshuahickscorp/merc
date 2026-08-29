"""Frozen Cycles quality contract shared by the CPU and Metal arms.

These values are the comparison contract. A Metal speedup that changes
samples, denoising, color management, bounces, precision, resolution,
adaptive mode, or shader features is not an architecture win.

use_adaptive_sampling is OFF so N spp means N spp on both arms. Blender's
factory default is ON; that knob is measured only when the harness
explicitly asks, and then on BOTH arms.
"""

from __future__ import annotations

from typing import Any

# Factory Blender 4.2.1 LTS color management. Do not swap AgX for Standard
# to manufacture a cheaper path.
COLOR_CONFIG: dict[str, Any] = {
    "display_device": "sRGB",
    "view_transform": "AgX",
    "look": "None",
    "exposure": 0.0,
    "gamma": 1.0,
}

BOUNCES: dict[str, int] = {
    "max": 12,
    "diffuse": 4,
    "glossy": 4,
    "transmission": 12,
    "volume": 0,
    "transparent": 8,
}

# Quality knobs identical on CPU and Metal. Device selection is NOT here.
CYCLES_QUALITY: dict[str, Any] = {
    "engine": "CYCLES",
    "use_denoising": False,
    "use_adaptive_sampling": False,
    "seed": 1,
    "use_animated_seed": False,
    "use_persistent_data": False,
    "filter_glossy": 1.0,
    "sample_clamp_direct": 0.0,
    "sample_clamp_indirect": 10.0,
    "caustics_reflective": False,
    "caustics_refractive": False,
    "use_fast_gi": False,
    "pixel_filter_type": "BLACKMAN_HARRIS",
    "filter_width": 1.5,
    "threads": 0,
    "film_transparent": False,
    "image_format": "PNG",
    "color_mode": "RGB",
    "color_depth": "8",
    "compression": 15,
}

ENGINE_REFUSE = ("EEVEE", "BLENDER_EEVEE", "BLENDER_EEVEE_NEXT")

# Historical alias used by the resident scene builders. Quality knobs only;
# it does not name a device.
CYCLES_CPU = CYCLES_QUALITY


def scene_bounces(scene_record: dict[str, Any]) -> dict[str, int]:
    out = dict(BOUNCES)
    override = scene_record.get("bounce_overrides") or {}
    for key, value in override.items():
        out[key] = int(value)
    return out
