"""Load deterministic critic fixtures from benchmarks/critics/fixtures."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image

from blender_vision.critics.base import CriticRole, CritiqueEvidence, CritiqueSubject


def fixtures_root() -> Path:
    here = Path(__file__).resolve()
    # src/blender_vision/critics/fixtures.py -> package root tools/blender-vision-mcp
    candidates = [
        here.parents[3] / "benchmarks" / "critics" / "fixtures",
        here.parents[4] / "benchmarks" / "critics" / "fixtures",
    ]
    for path in candidates:
        if path.is_dir():
            return path
    raise FileNotFoundError("benchmarks/critics/fixtures not found; run generate_fixtures.py")


def _load_png(path: Path) -> np.ndarray:
    image = Image.open(path)
    arr = np.asarray(image).astype(np.float64)
    if arr.max() > 1.0:
        arr = arr / 255.0
    return arr


def _load_json(path: Path) -> dict[str, Any]:
    return json.loads(path.read_text(encoding="utf-8"))


def evidence_for(subject_id: str) -> CritiqueEvidence:
    return CritiqueEvidence(references=[f"fixture:{subject_id}", f"sha256:fixture-{subject_id}"])


def load_fault_subject(role: CriticRole | str) -> CritiqueSubject:
    role_name = CriticRole(role).value
    root = fixtures_root() / role_name
    metrics: dict[str, Any] = {}
    media: dict[str, Any] = {}
    metrics_path = root / "metrics.json"
    if metrics_path.is_file():
        metrics = _load_json(metrics_path)
    for name, key in (("image.png", "image"), ("mask.png", "mask"), ("image_clip.png", "image")):
        path = root / name
        if path.is_file() and key not in media:
            media[key] = _load_png(path)
    # Product photographer multi-image: prefer low-separation still for primary subject.
    low_sep = root / "image_low_sep.png"
    if low_sep.is_file():
        media["image"] = _load_png(low_sep)
    if "contour_xy" in metrics:
        media["contour_xy"] = np.asarray(metrics.pop("contour_xy"), dtype=np.float64)
    if metrics.get("use_mask") and "mask" not in media:
        mask_path = root / "mask.png"
        if mask_path.is_file():
            media["mask"] = _load_png(mask_path)
        metrics.pop("use_mask", None)
    if "occupancy_grid" in metrics:
        metrics["occupancy_grid"] = np.asarray(metrics["occupancy_grid"], dtype=np.float64)

    # Ensure organic fault uses perfect circle mask when present.
    if role_name == CriticRole.ORGANIC_ARTIST.value:
        mask_path = root / "mask.png"
        if mask_path.is_file():
            media["mask"] = _load_png(mask_path)

    return CritiqueSubject(
        subject_id=f"fault-{role_name}",
        kind=f"fault:{role_name}",
        metrics=metrics,
        media=media,
        tags=frozenset({role_name, "fault"}),
    )


def load_control_subject(role: CriticRole | str) -> CritiqueSubject:
    role_name = CriticRole(role).value
    root = fixtures_root() / "control"
    metrics = _load_json(root / "metrics.json")
    media: dict[str, Any] = {}
    image_path = root / "image.png"
    mask_path = root / "mask.png"
    if image_path.is_file():
        media["image"] = _load_png(image_path)
    if mask_path.is_file():
        media["mask"] = _load_png(mask_path)
    if "contour_xy" in metrics:
        media["contour_xy"] = np.asarray(metrics["contour_xy"], dtype=np.float64)
    if "occupancy_grid" in metrics:
        metrics = dict(metrics)
        metrics["occupancy_grid"] = np.asarray(metrics["occupancy_grid"], dtype=np.float64)

    # Role-specific control: keep only keys that make the critic apply, with clean values.
    # Full control metrics already pass all thresholds.
    return CritiqueSubject(
        subject_id=f"control-{role_name}",
        kind="control",
        metrics=dict(metrics),
        media=media,
        tags=frozenset({role_name, "control"}),
    )


def fault_fixture_roles() -> list[CriticRole]:
    return list(CriticRole)
