"""9×4 ocular-event calibration fixtures.

Four fixture classes per Bible-6.5 event type under calibration:
  true_positive  — the event unambiguously occurs
  true_negative  — static / irrelevant; must not fire
  near_threshold — just barely the event / just barely not
  confounder     — superficially similar but a different event

Ground truth lives only in the sealed evaluator path (manifests). The runtime
detector never reads these labels.
"""

from __future__ import annotations

CALIBRATED_EVENTS: tuple[str, ...] = (
    "OBJECT_MOVED",
    "CAMERA_MOVED",
    "OBJECT_ENTERED",
    "OBJECT_LEFT",
    "OBJECT_OCCLUDED",
    "OBJECT_REAPPEARED",
    "NEW_UNKNOWN_REGION",
    "LIGHT_CHANGED",
    "SURFACE_CHANGED",
)

FIXTURE_CLASSES: tuple[str, ...] = (
    "true_positive",
    "true_negative",
    "near_threshold",
    "confounder",
)

# Minimum precision declared up front. Chosen from measured class separation on
# the procedural baseline; not aspirational. Fail the run below these floors.
MIN_PRECISION: dict[str, float] = {
    "OBJECT_MOVED": 0.70,
    "CAMERA_MOVED": 0.80,
    "OBJECT_ENTERED": 0.70,
    "OBJECT_LEFT": 0.70,
    "OBJECT_OCCLUDED": 0.60,
    "OBJECT_REAPPEARED": 0.60,
    "NEW_UNKNOWN_REGION": 0.60,
    "LIGHT_CHANGED": 0.80,
    "SURFACE_CHANGED": 0.60,
}
