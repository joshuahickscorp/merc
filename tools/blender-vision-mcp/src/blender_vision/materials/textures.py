"""Procedural tileable PBR texture set generation from material hypotheses."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image

from blender_vision.v2.records import MaterialHypothesis


@dataclass(slots=True)
class TextureSet:
    """Tileable maps with world scale recorded in metres."""

    size: int
    world_scale_m: float
    paths: dict[str, Path]
    hypothesis_id: str
    metadata: dict[str, Any]

    def to_dict(self) -> dict[str, Any]:
        return {
            "size": self.size,
            "world_scale_m": self.world_scale_m,
            "paths": {key: str(path) for key, path in self.paths.items()},
            "hypothesis_id": self.hypothesis_id,
            "metadata": self.metadata,
        }


def _tileable_noise(size: int, scale: float, seed: int) -> np.ndarray:
    """Value noise on a toroidal grid so opposite edges match."""
    rng = np.random.default_rng(seed)
    grid = max(2, int(round(size / max(scale, 1.0))))
    # Periodic lattice of random values.
    lattice = rng.random((grid, grid))
    # Tile by Fourier upsampling of the lattice.
    spectrum = np.fft.fft2(lattice)
    padded = np.zeros((size, size), dtype=np.complex128)
    half = grid // 2
    padded[:half, :half] = spectrum[:half, :half]
    padded[:half, -half:] = spectrum[:half, -half:]
    padded[-half:, :half] = spectrum[-half:, :half]
    padded[-half:, -half:] = spectrum[-half:, -half:]
    field = np.fft.ifft2(padded).real
    field = field - field.min()
    denom = field.max() - field.min()
    if denom < 1e-12:
        return np.zeros((size, size), dtype=np.float64)
    return field / denom


def _save_rgb(path: Path, rgb: np.ndarray) -> None:
    arr = np.clip(rgb * 255.0, 0, 255).astype(np.uint8)
    Image.fromarray(arr, mode="RGB").save(path)


def _save_gray(path: Path, gray: np.ndarray) -> None:
    arr = np.clip(gray * 255.0, 0, 255).astype(np.uint8)
    Image.fromarray(arr, mode="L").save(path)


def _normal_from_height(height: np.ndarray, strength: float = 1.0) -> np.ndarray:
    dy, dx = np.gradient(height)
    nx = -dx * strength
    ny = -dy * strength
    nz = np.ones_like(height)
    length = np.sqrt(nx * nx + ny * ny + nz * nz) + 1e-8
    nx, ny, nz = nx / length, ny / length, nz / length
    return np.stack([(nx + 1.0) * 0.5, (ny + 1.0) * 0.5, (nz + 1.0) * 0.5], axis=-1)


def generate_texture_set(
    hypothesis: MaterialHypothesis,
    size: int = 256,
    *,
    output_dir: Path | None = None,
    seed: int = 0,
) -> TextureSet:
    """Generate tileable base colour / roughness / metalness / normal / displacement / AO."""
    if size < 16 or size > 4096:
        raise ValueError("texture size must be within 16..4096")
    world_scale_m = float(hypothesis.texture_scale_m)
    if world_scale_m <= 0.0:
        raise ValueError("texture_scale_m must be positive metres")

    out = Path(output_dir) if output_dir is not None else Path.cwd() / "texture_set"
    out.mkdir(parents=True, exist_ok=True)

    # Feature density scales with world scale: smaller metres => finer features.
    feature_px = max(2.0, min(size / 4.0, world_scale_m * 1000.0 * 2.0))
    height = _tileable_noise(size, feature_px, seed=seed + 1)
    micro = _tileable_noise(size, max(2.0, feature_px * 0.25), seed=seed + 2)
    colour_noise = _tileable_noise(size, feature_px * 1.5, seed=seed + 3)

    base = np.array(hypothesis.base_colour[:3], dtype=np.float64)
    colour_variation = 0.08 * (colour_noise[..., None] - 0.5)
    base_colour = np.clip(base + colour_variation, 0.0, 1.0)

    roughness = np.clip(
        hypothesis.roughness + 0.12 * (micro - 0.5) * (1.0 - 0.5 * hypothesis.metalness),
        0.02,
        1.0,
    )
    metalness = np.clip(
        np.full((size, size), hypothesis.metalness, dtype=np.float64)
        + 0.03 * (colour_noise - 0.5) * hypothesis.metalness,
        0.0,
        1.0,
    )
    displacement = (height - 0.5) * (0.002 + 0.01 * (1.0 - hypothesis.roughness))
    # Encode displacement as 0.5-centred grayscale for PNG storage.
    displacement_png = np.clip(displacement / 0.02 + 0.5, 0.0, 1.0)
    normal = _normal_from_height(height * (0.4 + 0.6 * (1.0 - hypothesis.roughness)))
    occlusion = np.clip(1.0 - 0.35 * np.maximum(0.0, 0.55 - height), 0.0, 1.0)

    paths = {
        "base_colour": out / "base_colour.png",
        "roughness": out / "roughness.png",
        "metalness": out / "metalness.png",
        "normal": out / "normal.png",
        "displacement": out / "displacement.png",
        "occlusion": out / "occlusion.png",
    }
    _save_rgb(paths["base_colour"], base_colour)
    _save_gray(paths["roughness"], roughness)
    _save_gray(paths["metalness"], metalness)
    _save_rgb(paths["normal"], normal)
    _save_gray(paths["displacement"], displacement_png)
    _save_gray(paths["occlusion"], occlusion)

    metadata = {
        "world_scale_m": world_scale_m,
        "size_px": size,
        "metres_per_pixel": world_scale_m / size,
        "hypothesis_id": hypothesis.hypothesis_id,
        "seed": seed,
        "maps": sorted(paths),
    }
    (out / "texture_set.json").write_text(
        json.dumps(metadata, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return TextureSet(
        size=size,
        world_scale_m=world_scale_m,
        paths=paths,
        hypothesis_id=hypothesis.hypothesis_id,
        metadata=metadata,
    )


def load_texture_set_metadata(path: Path) -> dict[str, Any]:
    payload = json.loads(Path(path).read_text(encoding="utf-8"))
    if "world_scale_m" not in payload:
        raise ValueError("texture set metadata missing world_scale_m")
    return payload
