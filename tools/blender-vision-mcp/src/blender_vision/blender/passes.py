from __future__ import annotations

GOVERNED_RENDER_PASSES = frozenset(
    {
        "beauty",
        "appearance",
        "exposure_minus_2",
        "exposure_0",
        "exposure_plus_2",
        "neutral_grey_background",
        "white_background",
        "black_background",
        "neutral_clay",
        "material_neutral",
        "grazing_left",
        "grazing_right",
        "grazing_top",
        "silhouette",
        "depth",
        "normal",
        "world_normal",
        "geometric_normal",
        "curvature",
        "object_id",
        "component_id",
        "feature_id",
        "wireframe",
        "zebra",
        "reflected_line",
    }
)

# Frozen V1 baselines and accepted calibration receipts retain the governed
# minimum above. New visual-geometry work requests this superset so industrial
# surface diagnostics can advance without retroactively rewriting old evidence.
INDUSTRIAL_SURFACE_RENDER_PASSES = frozenset(
    {
        "normal_discontinuity",
        "highlight_flow",
    }
)

MAXIMAL_VISUAL_RENDER_PASSES = (
    GOVERNED_RENDER_PASSES | INDUSTRIAL_SURFACE_RENDER_PASSES
)
