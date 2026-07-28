"""Surface frequency-band decomposition for material detail authority.

Six declared bands:
  macro geometry, medium displacement, fine normal, micro roughness,
  colour variation, backing/occlusion.

A real Gaussian/Laplacian pyramid exposes per-band energy so critics can detect
flat textures pretending to be depth (no medium-band energy).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import StrEnum
from typing import Any

import numpy as np

from blender_vision.v2.authority import AuthorityClass, derive


class FrequencyBand(StrEnum):
    MACRO_GEOMETRY = "macro_geometry"
    MEDIUM_DISPLACEMENT = "medium_displacement"
    FINE_NORMAL = "fine_normal"
    MICRO_ROUGHNESS = "micro_roughness"
    COLOUR_VARIATION = "colour_variation"
    BACKING_OCCLUSION = "backing_occlusion"


BAND_ORDER: tuple[FrequencyBand, ...] = (
    FrequencyBand.MACRO_GEOMETRY,
    FrequencyBand.MEDIUM_DISPLACEMENT,
    FrequencyBand.FINE_NORMAL,
    FrequencyBand.MICRO_ROUGHNESS,
    FrequencyBand.COLOUR_VARIATION,
    FrequencyBand.BACKING_OCCLUSION,
)


@dataclass(slots=True)
class BandEnergy:
    band: FrequencyBand
    energy: float
    relative_energy: float
    spatial_scale_px: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "band": self.band.value,
            "energy": self.energy,
            "relative_energy": self.relative_energy,
            "spatial_scale_px": self.spatial_scale_px,
        }


@dataclass(slots=True)
class FrequencyDecomposition:
    """Per-band energy plus residual maps for critic inspection."""

    bands: list[BandEnergy]
    residual_energy: float
    pyramid_levels: int
    authority: AuthorityClass = AuthorityClass.INFERRED
    maps: dict[str, np.ndarray] = field(default_factory=dict, repr=False)

    def energy_map(self) -> dict[str, float]:
        return {item.band.value: item.energy for item in self.bands}

    def relative_map(self) -> dict[str, float]:
        return {item.band.value: item.relative_energy for item in self.bands}

    def to_dict(self) -> dict[str, Any]:
        return {
            "bands": [item.to_dict() for item in self.bands],
            "residual_energy": self.residual_energy,
            "pyramid_levels": self.pyramid_levels,
            "authority": self.authority.value,
        }


def _to_float_image(image: np.ndarray) -> np.ndarray:
    arr = np.asarray(image, dtype=np.float64)
    if arr.ndim == 3:
        if arr.shape[2] >= 3:
            arr = 0.2126 * arr[..., 0] + 0.7152 * arr[..., 1] + 0.0722 * arr[..., 2]
        else:
            arr = arr[..., 0]
    if arr.max() > 1.0 + 1e-6:
        arr = arr / 255.0
    return arr.astype(np.float64)


def _gaussian_pyramid(image: np.ndarray, levels: int) -> list[np.ndarray]:
    from skimage.transform import pyramid_gaussian

    pyramid = list(pyramid_gaussian(image, max_layer=levels - 1, channel_axis=None))
    return [np.asarray(level, dtype=np.float64) for level in pyramid]


def _laplacian_from_gaussian(pyramid: list[np.ndarray]) -> list[np.ndarray]:
    from skimage.transform import resize

    laps: list[np.ndarray] = []
    for index in range(len(pyramid) - 1):
        current = pyramid[index]
        coarser = pyramid[index + 1]
        up = resize(coarser, current.shape, order=1, anti_aliasing=False, preserve_range=True)
        laps.append(current - up)
    laps.append(pyramid[-1].copy())
    return laps


def _band_energy(plane: np.ndarray) -> float:
    return float(np.mean(plane * plane))


def decompose_surface(
    image: np.ndarray,
    *,
    levels: int = 5,
    colour_image: np.ndarray | None = None,
    occlusion_image: np.ndarray | None = None,
    authority_inputs: list[AuthorityClass] | None = None,
) -> FrequencyDecomposition:
    """Decompose a surface image into the six declared frequency bands.

    Laplacian levels map onto geometry/displacement/normal/micro bands by
    spatial scale. Colour variation and occlusion are measured separately so a
    painted-flat texture without medium-band structure is visible as energy in
    colour with near-zero medium displacement energy.
    """
    if levels < 3:
        raise ValueError("frequency decomposition requires at least 3 pyramid levels")
    gray = _to_float_image(image)
    pyramid = _gaussian_pyramid(gray, levels)
    laplacian = _laplacian_from_gaussian(pyramid)

    # Finest residual ~ micro roughness; next ~ fine normal; then medium; coarsest residual ~ macro.
    from skimage.transform import resize

    fine = laplacian[0] if laplacian else gray * 0.0
    medium_raw = laplacian[1] if len(laplacian) > 1 else gray * 0.0
    medium = resize(
        medium_raw, gray.shape, order=1, anti_aliasing=False, preserve_range=True
    )
    # Laplacian ends with the coarsest Gaussian residual (DC); exclude it from
    # macro so a flat surface is not scored as huge geometry energy.
    macro = np.zeros_like(gray)
    bandpass_macro = (
        laplacian[2:-1] if len(laplacian) > 3 else ([laplacian[2]] if len(laplacian) > 2 else [])
    )
    for level in bandpass_macro:
        macro = macro + resize(
            level, gray.shape, order=1, anti_aliasing=False, preserve_range=True
        )
    fine = resize(fine, gray.shape, order=1, anti_aliasing=False, preserve_range=True)
    # Micro roughness: high-frequency local variance of the finest band.
    from scipy.ndimage import uniform_filter

    local_mean = uniform_filter(fine, size=3)
    micro = (fine - local_mean) ** 2

    if colour_image is not None:
        colour = np.asarray(colour_image, dtype=np.float64)
        if colour.max() > 1.0 + 1e-6:
            colour = colour / 255.0
        if colour.ndim == 3:
            mean_c = colour.mean(axis=2, keepdims=True)
            colour_var = np.mean((colour - mean_c) ** 2, axis=2)
        else:
            colour_var = (colour - colour.mean()) ** 2
    else:
        # Without a separate colour map, use low-frequency residual of gray as colour proxy.
        colour_var = pyramid[-1] if pyramid else gray * 0.0
        colour_var = (colour_var - colour_var.mean()) ** 2

    if occlusion_image is not None:
        occ = _to_float_image(occlusion_image)
        occlusion = (1.0 - occ) ** 2
    else:
        # Dark valleys in medium/macro as soft AO proxy.
        combined = medium + macro
        occlusion = np.clip(-combined, 0.0, None) ** 2

    raw = {
        FrequencyBand.MACRO_GEOMETRY: _band_energy(macro),
        FrequencyBand.MEDIUM_DISPLACEMENT: _band_energy(medium),
        FrequencyBand.FINE_NORMAL: _band_energy(fine),
        FrequencyBand.MICRO_ROUGHNESS: float(np.mean(micro)),
        FrequencyBand.COLOUR_VARIATION: float(np.mean(colour_var)),
        FrequencyBand.BACKING_OCCLUSION: float(np.mean(occlusion)),
    }
    total = sum(raw.values()) + 1e-12
    scale_px = {
        FrequencyBand.MACRO_GEOMETRY: float(2 ** (levels - 1)),
        FrequencyBand.MEDIUM_DISPLACEMENT: 8.0,
        FrequencyBand.FINE_NORMAL: 2.0,
        FrequencyBand.MICRO_ROUGHNESS: 1.0,
        FrequencyBand.COLOUR_VARIATION: float(2 ** (levels - 1)),
        FrequencyBand.BACKING_OCCLUSION: 4.0,
    }
    bands = [
        BandEnergy(
            band=band,
            energy=raw[band],
            relative_energy=raw[band] / total,
            spatial_scale_px=scale_px[band],
        )
        for band in BAND_ORDER
    ]
    if laplacian:
        residual_map = resize(
            laplacian[-1], gray.shape, order=1, anti_aliasing=False, preserve_range=True
        )
        residual = float(np.mean(residual_map**2))
    else:
        residual = 0.0
    authority = derive(
        authority_inputs or [AuthorityClass.OBSERVED],
        proposed=AuthorityClass.INFERRED,
    )
    return FrequencyDecomposition(
        bands=bands,
        residual_energy=residual,
        pyramid_levels=levels,
        authority=authority,
        maps={
            "macro": macro,
            "medium": medium,
            "fine": fine,
            "micro": micro,
            "colour": colour_var if isinstance(colour_var, np.ndarray) else np.asarray(colour_var),
            "occlusion": occlusion,
        },
    )


def lacks_medium_band(
    decomposition: FrequencyDecomposition,
    *,
    medium_relative_floor: float = 0.04,
    colour_relative_ceiling: float = 0.35,
) -> bool:
    """True when colour/micro dominate while medium displacement energy is absent."""
    rel = decomposition.relative_map()
    return (
        rel[FrequencyBand.MEDIUM_DISPLACEMENT.value] < medium_relative_floor
        and rel[FrequencyBand.COLOUR_VARIATION.value] >= colour_relative_ceiling
    )
