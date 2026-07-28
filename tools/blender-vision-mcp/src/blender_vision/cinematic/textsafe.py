"""Text-safe zone solver: contrast and variance gates for scroll-bound copy."""

from __future__ import annotations

from collections.abc import Sequence
from dataclasses import dataclass
from enum import StrEnum
from typing import Any

import numpy as np
from numpy.typing import NDArray


class TextZone(StrEnum):
    CENTRE = "centre"
    LEFT_UPPER = "left_upper"
    RIGHT_UPPER = "right_upper"
    TERMINAL_WALL = "terminal_wall"
    EDGE = "edge"


# Default rectangles in normalised image coordinates (x0, y0, x1, y1), origin top-left.
ZONE_RECTS: dict[TextZone, tuple[float, float, float, float]] = {
    TextZone.CENTRE: (0.30, 0.35, 0.70, 0.55),
    TextZone.LEFT_UPPER: (0.05, 0.08, 0.38, 0.28),
    TextZone.RIGHT_UPPER: (0.62, 0.08, 0.95, 0.28),
    TextZone.TERMINAL_WALL: (0.25, 0.55, 0.75, 0.78),
    TextZone.EDGE: (0.02, 0.40, 0.18, 0.70),
}


@dataclass(slots=True)
class TextSafeResult:
    zone: TextZone
    readable: bool
    contrast_ratio: float
    luminance_variance: float
    mean_background_luminance: float
    threshold_contrast: float
    threshold_variance: float
    rect: tuple[float, float, float, float]
    reason: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "zone": self.zone.value,
            "readable": self.readable,
            "contrast_ratio": self.contrast_ratio,
            "luminance_variance": self.luminance_variance,
            "mean_background_luminance": self.mean_background_luminance,
            "threshold_contrast": self.threshold_contrast,
            "threshold_variance": self.threshold_variance,
            "rect": list(self.rect),
            "reason": self.reason,
        }


def _relative_luminance(rgb: NDArray[np.float64]) -> NDArray[np.float64]:
    # sRGB linearisation then Rec. 709 luminance; input may be float [0,1] or uint8.
    sample = rgb.astype(np.float64)
    if sample.max() > 1.0:
        sample = sample / 255.0
    if sample.ndim == 2:
        return sample
    linear = np.where(sample <= 0.04045, sample / 12.92, ((sample + 0.055) / 1.055) ** 2.4)
    return 0.2126 * linear[..., 0] + 0.7152 * linear[..., 1] + 0.0722 * linear[..., 2]


def _contrast_ratio(l1: float, l2: float) -> float:
    lighter = max(l1, l2)
    darker = min(l1, l2)
    return (lighter + 0.05) / (darker + 0.05)


def _crop_rect(
    frame: NDArray[np.float64], rect: tuple[float, float, float, float]
) -> NDArray[np.float64]:
    height, width = frame.shape[:2]
    x0 = int(np.clip(rect[0], 0.0, 1.0) * (width - 1))
    y0 = int(np.clip(rect[1], 0.0, 1.0) * (height - 1))
    x1 = int(np.clip(rect[2], 0.0, 1.0) * (width - 1)) + 1
    y1 = int(np.clip(rect[3], 0.0, 1.0) * (height - 1)) + 1
    if x1 <= x0 or y1 <= y0:
        raise ValueError(f"degenerate text rectangle {rect}")
    return frame[y0:y1, x0:x1]


def evaluate_text_safe(
    frame: NDArray[np.floating] | NDArray[np.integer],
    *,
    zone: TextZone | str = TextZone.CENTRE,
    text_luminance: float = 1.0,
    rect: Sequence[float] | None = None,
    min_contrast: float = 4.5,
    max_background_variance: float = 0.02,
) -> TextSafeResult:
    """Decide whether a text block is readable against the rendered frame.

    Uses WCAG-style contrast between the declared text luminance and mean
    background luminance inside the zone rectangle, plus a variance gate so
    busy backgrounds still fail even when mean contrast looks fine.
    """
    zone_enum = TextZone(zone)
    resolved_rect = tuple(float(v) for v in (rect or ZONE_RECTS[zone_enum]))
    if len(resolved_rect) != 4:
        raise ValueError("rect must be (x0, y0, x1, y1) in normalised coordinates")
    array = np.asarray(frame)
    if array.ndim not in {2, 3}:
        raise ValueError("frame must be HxW or HxWxC")
    crop = _crop_rect(array, resolved_rect)  # type: ignore[arg-type]
    lum = _relative_luminance(crop)
    mean_bg = float(np.mean(lum))
    variance = float(np.var(lum))
    contrast = _contrast_ratio(float(text_luminance), mean_bg)

    reasons: list[str] = []
    if contrast < min_contrast:
        reasons.append(f"contrast {contrast:.3f} < threshold {min_contrast:.3f}")
    if variance > max_background_variance:
        reasons.append(
            f"background luminance variance {variance:.5f} > {max_background_variance:.5f}"
        )
    readable = not reasons
    reason = "readable" if readable else "; ".join(reasons)
    return TextSafeResult(
        zone=zone_enum,
        readable=readable,
        contrast_ratio=contrast,
        luminance_variance=variance,
        mean_background_luminance=mean_bg,
        threshold_contrast=min_contrast,
        threshold_variance=max_background_variance,
        rect=resolved_rect,  # type: ignore[arg-type]
        reason=reason,
    )
