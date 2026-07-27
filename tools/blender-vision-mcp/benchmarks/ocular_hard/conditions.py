"""Hard tracking condition catalogue.

Ten conditions + one permanence sequence. Ground truth is sealed outside the
builder-visible frame directory; the tracker only ever sees pixels.
"""

from __future__ import annotations

from enum import StrEnum


class Condition(StrEnum):
    VISUALLY_SIMILAR = "visually_similar"
    CROSSING_PATHS = "crossing_paths"
    PARTIAL_OCCLUSION = "partial_occlusion"
    FULL_OCCLUSION = "full_occlusion"
    LIGHTING_CHANGE = "lighting_change"
    SCALE_CHANGE = "scale_change"
    CAMERA_MOTION = "camera_motion"
    LEAVE_RETURN = "leave_return"
    DISTRACTOR_REPLACEMENT = "distractor_replacement"
    UNKNOWN_ENTERING = "unknown_entering"
    PERMANENCE = "permanence"


# Object ids used across fixtures. Similar objects share near-identical albedo
# and shape; textures differ subtly so colour histograms alone cannot separate them.
PRIMARY_IDS = ("obj_a", "obj_b", "obj_c")
DISTRACTOR_ID = "obj_distractor"
UNKNOWN_ID = "obj_unknown"
OCCLUDER_ID = "occluder_slab"

CONDITION_DESCRIPTIONS: dict[Condition, str] = {
    Condition.VISUALLY_SIMILAR: (
        "Three confusable objects (same shape, near-identical albedo, subtle texture)."
    ),
    Condition.CROSSING_PATHS: "Two similar objects cross paths mid-sequence.",
    Condition.PARTIAL_OCCLUSION: "Occluder covers ~half of one object then clears.",
    Condition.FULL_OCCLUSION: "Occluder fully hides one object for several frames.",
    Condition.LIGHTING_CHANGE: "Global illumination shifts mid-sequence.",
    Condition.SCALE_CHANGE: "One object approaches the camera (apparent scale change).",
    Condition.CAMERA_MOTION: "Camera pans; objects are world-stationary.",
    Condition.LEAVE_RETURN: "One object exits the frame and later returns.",
    Condition.DISTRACTOR_REPLACEMENT: (
        "One object leaves permanently; a similar distractor enters its place."
    ),
    Condition.UNKNOWN_ENTERING: "A previously unseen object enters while others stay.",
    Condition.PERMANENCE: (
        "Three similar objects; one moves, one fully occluded, one leaves; "
        "distractor enters; original returns. Proves permanence + refusal."
    ),
}

# Frames per condition (diagnostic synthetic and Blender share the same length).
FRAME_COUNT = 32
RESOLUTION = (320, 240)

# Near-identical base BGR (OpenCV) so colour alone cannot separate primaries.
BASE_BGR = (118, 148, 176)  # beige-ish
DISTRACTOR_BGR = (112, 142, 170)  # subtle shift
UNKNOWN_BGR = (130, 150, 165)
OCCLUDER_BGR = (22, 22, 28)
TABLE_BGR = (38, 34, 30)
BG_BGR = (26, 26, 28)
