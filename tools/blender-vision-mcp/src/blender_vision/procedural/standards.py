"""EIA/ECA-310 rack dimensional standards in metres.

Values are manufacturer-spec ground truth. 1U and 19-inch mounting width are
fixed by the standard; frame widths and depths are common industry practice.
"""

from __future__ import annotations

# EIA-310-D / IEC 60297
U_HEIGHT_M = 0.04445  # 1.75 in
MOUNTING_WIDTH_M = 0.4826  # 19.00 in equipment mounting aperture
RAIL_HOLE_SPACING_M = 0.4651  # 18.31 in centre-to-centre of mounting rails

FRAME_WIDTH_600_M = 0.600
FRAME_WIDTH_800_M = 0.800
COMMON_DEPTHS_M = (0.800, 1.000, 1.200)

DEFAULT_U_COUNT = 42
DEFAULT_FRAME_WIDTH_M = FRAME_WIDTH_600_M
DEFAULT_DEPTH_M = 1.000

# Raised-floor / ceiling grid (common data-centre module)
FLOOR_TILE_M = 0.600
CEILING_PANEL_M = 0.600
AISLE_WIDTH_HOT_M = 1.200
AISLE_WIDTH_COLD_M = 1.200


def rack_height_m(u_count: int) -> float:
    """External usable bay height for ``u_count`` rack units (no plinth)."""
    if u_count < 1:
        raise ValueError(f"u_count must be >= 1, got {u_count}")
    return u_count * U_HEIGHT_M


def u_to_z(u_index: int, *, origin: str = "bottom") -> float:
    """Z of the bottom face of the U slot (1-based U index)."""
    if u_index < 1:
        raise ValueError(f"u_index must be >= 1, got {u_index}")
    if origin != "bottom":
        raise ValueError("only bottom origin is supported")
    return (u_index - 1) * U_HEIGHT_M


def drawer_height_m(u_height: int) -> float:
    """Equipment height for a drawer of ``u_height`` U, with 0.5 mm clearance."""
    if u_height < 1:
        raise ValueError(f"u_height must be >= 1, got {u_height}")
    return u_height * U_HEIGHT_M - 0.0005
