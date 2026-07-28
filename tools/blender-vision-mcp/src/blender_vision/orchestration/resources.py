from __future__ import annotations

import os
import shutil
from typing import Any

PROFILES = {
    "compact": {
        "minimum_ram_gb": 16,
        "minimum_vram_gb": 8,
        "cpu_fallback": True,
        "maximum_parallel_candidates": 1,
        "maximum_working_resolution_px": 1024,
        "features": ["blender", "manual_cameras", "colmap", "limited_resolution"],
    },
    "standard": {
        "minimum_ram_gb": 32,
        "minimum_vram_gb": 16,
        "cpu_fallback": True,
        "maximum_parallel_candidates": 2,
        "maximum_working_resolution_px": 2048,
        "features": ["blender", "vggt", "colmap", "moderate_video", "optimization"],
    },
    "beast": {
        "minimum_ram_gb": 64,
        "minimum_vram_gb": 24,
        "cpu_fallback": False,
        "maximum_parallel_candidates": 8,
        "maximum_working_resolution_px": 4096,
        "features": [
            "persistent_vision_workers",
            "generative_3d",
            "gaussian_oracle",
            "high_resolution_depth",
            "parallel_candidates",
            "training",
        ],
    },
    "distributed_beast": {
        "minimum_ram_gb": 64,
        "minimum_vram_gb": 24,
        "cpu_fallback": False,
        "maximum_parallel_candidates": 32,
        "maximum_working_resolution_px": 4096,
        "features": [
            "coordinator",
            "mac_blender_worker",
            "gpu_vision_worker",
            "dgx_workers",
            "content_addressed_store",
        ],
    },
}


def discover_resources() -> dict[str, Any]:
    try:
        pages = os.sysconf("SC_PHYS_PAGES")
        page_size = os.sysconf("SC_PAGE_SIZE")
        ram_gb = pages * page_size / 1024**3
    except (ValueError, OSError, AttributeError):
        ram_gb = 0.0
    vram_gb = float(os.environ.get("BVMCP_VRAM_GB", "0") or 0)
    distributed = os.environ.get("BVMCP_DISTRIBUTED", "0") == "1"
    if distributed and ram_gb >= 64 and vram_gb >= 24:
        selected = "distributed_beast"
    elif ram_gb >= 64 and vram_gb >= 24:
        selected = "beast"
    elif ram_gb >= 32 and vram_gb >= 16:
        selected = "standard"
    else:
        selected = "compact"
    available = {
        "blender": bool(shutil.which("blender") or os.environ.get("BLENDER_PATH")),
        "ffmpeg": bool(shutil.which("ffmpeg")),
        "colmap": bool(shutil.which("colmap")),
    }
    return {
        "selected_profile": selected,
        "ram_gb": round(ram_gb, 2),
        "vram_gb": vram_gb,
        "distributed": distributed,
        "available_tools": available,
        "profile": PROFILES[selected],
        "degradation_policy": (
            "run the strongest available pipeline; omit unavailable optional lanes and "
            "record them instead of failing the campaign"
        ),
    }


def profile(name: str) -> dict[str, Any]:
    if name == "auto":
        return discover_resources()
    if name not in PROFILES:
        raise ValueError(
            "resource profile must be auto, compact, standard, beast, or distributed_beast"
        )
    discovered = discover_resources()
    return {
        **discovered,
        "selected_profile": name,
        "profile": PROFILES[name],
        "forced": True,
    }
