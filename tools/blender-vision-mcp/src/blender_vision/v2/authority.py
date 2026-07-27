"""Authority classes, coordinate frames, and units for VisionMCP V2 records.

The single rule this module exists to enforce: a derived claim is never stronger
than the weakest evidence it was derived from, and nothing raises its own
authority without an explicit, recorded reviewer decision.
"""

from __future__ import annotations

from dataclasses import asdict, dataclass, field
from enum import StrEnum
from typing import Any

from blender_vision.core.errors import ValidationError


class AuthorityClass(StrEnum):
    """Bible section 6 authority classes."""

    OBSERVED = "OBSERVED"
    MEASURED = "MEASURED"
    MANUFACTURER_SPEC = "MANUFACTURER_SPEC"
    SENSOR_DERIVED = "SENSOR_DERIVED"
    MODEL_DERIVED = "MODEL_DERIVED"
    PROCEDURAL_GROUND_TRUTH = "PROCEDURAL_GROUND_TRUTH"
    RUNTIME_OBSERVED = "RUNTIME_OBSERVED"
    HUMAN_REVIEWED = "HUMAN_REVIEWED"
    INFERRED = "INFERRED"
    HYPOTHETICAL = "HYPOTHETICAL"
    UNRESOLVED = "UNRESOLVED"
    REJECTED = "REJECTED"


# Strength ordering. Higher binds harder. REJECTED is absorbing, not weak:
# it is handled separately so a rejected input can never be laundered by fusion.
_STRENGTH: dict[AuthorityClass, int] = {
    AuthorityClass.REJECTED: -1,
    AuthorityClass.UNRESOLVED: 0,
    AuthorityClass.HYPOTHETICAL: 1,
    AuthorityClass.INFERRED: 2,
    AuthorityClass.MODEL_DERIVED: 3,
    AuthorityClass.SENSOR_DERIVED: 4,
    AuthorityClass.RUNTIME_OBSERVED: 5,
    AuthorityClass.OBSERVED: 6,
    AuthorityClass.PROCEDURAL_GROUND_TRUTH: 7,
    AuthorityClass.MANUFACTURER_SPEC: 8,
    AuthorityClass.MEASURED: 9,
    AuthorityClass.HUMAN_REVIEWED: 10,
}

# Classes that may only be reached through an explicit external act, never by
# derivation, fusion, optimisation, or a critic deciding it likes the result.
_REVIEW_ONLY: frozenset[AuthorityClass] = frozenset(
    {
        AuthorityClass.MEASURED,
        AuthorityClass.MANUFACTURER_SPEC,
        AuthorityClass.HUMAN_REVIEWED,
        AuthorityClass.OBSERVED,
        AuthorityClass.RUNTIME_OBSERVED,
        AuthorityClass.PROCEDURAL_GROUND_TRUTH,
    }
)


class AuthorityPromotionError(ValidationError):
    """Raised when a record attempts to strengthen its own authority."""


def strength(authority: AuthorityClass | str) -> int:
    return _STRENGTH[AuthorityClass(authority)]


def is_review_only(authority: AuthorityClass | str) -> bool:
    """True when the class can only be assigned by an external, recorded act."""
    return AuthorityClass(authority) in _REVIEW_ONLY


def derive(
    inputs: list[AuthorityClass | str],
    *,
    proposed: AuthorityClass | str = AuthorityClass.INFERRED,
) -> AuthorityClass:
    """Return the authority a derived claim is allowed to carry.

    The result is capped at the weakest input. A single REJECTED input poisons
    the whole derivation. An empty input list is HYPOTHETICAL: something was
    asserted with no evidence behind it at all.
    """
    if not inputs:
        return AuthorityClass.HYPOTHETICAL
    resolved = [AuthorityClass(item) for item in inputs]
    if AuthorityClass.REJECTED in resolved:
        return AuthorityClass.REJECTED
    ceiling = min(resolved, key=strength)
    wanted = AuthorityClass(proposed)
    if strength(wanted) > strength(ceiling):
        return ceiling
    return wanted


def assert_no_promotion(
    current: AuthorityClass | str,
    target: AuthorityClass | str,
    *,
    reviewer: str | None = None,
    reason: str | None = None,
) -> AuthorityClass:
    """Guard every authority transition. Returns the accepted target class.

    Downgrades and same-class transitions are always allowed. Upgrades require a
    named reviewer and a reason; upgrades into a review-only class additionally
    require that the reviewer is not the system itself.
    """
    source = AuthorityClass(current)
    destination = AuthorityClass(target)
    if strength(destination) <= strength(source):
        return destination
    if not reviewer or not reason:
        raise AuthorityPromotionError(
            f"cannot promote {source.value} to {destination.value} "
            "without a recorded reviewer and reason"
        )
    if is_review_only(destination) and reviewer.strip().lower() in {
        "system",
        "auto",
        "automatic",
        "visionmcp",
        "",
    }:
        raise AuthorityPromotionError(
            f"{destination.value} is review-only and cannot be granted by '{reviewer}'"
        )
    return destination


class Units(StrEnum):
    METRE = "m"
    MILLIMETRE = "mm"
    PIXEL = "px"
    NORMALIZED = "normalized"
    DEGREE = "deg"
    RADIAN = "rad"
    SECOND = "s"
    UNITLESS = "unitless"


_TO_METRES: dict[Units, float] = {
    Units.METRE: 1.0,
    Units.MILLIMETRE: 0.001,
}


def to_metres(value: float, units: Units | str) -> float:
    resolved = Units(units)
    if resolved not in _TO_METRES:
        raise ValidationError(f"{resolved.value} is not a metric length unit")
    return value * _TO_METRES[resolved]


class Handedness(StrEnum):
    RIGHT = "right"
    LEFT = "left"


@dataclass(slots=True)
class CoordinateFrame:
    """An explicit spatial contract. Records may not be compared without one."""

    name: str
    up_axis: str = "+Z"
    forward_axis: str = "-Y"
    handedness: Handedness = Handedness.RIGHT
    units: Units = Units.METRE
    origin_semantics: str = "scene-origin"
    scale_authority: AuthorityClass = AuthorityClass.UNRESOLVED

    _AXES = ("+X", "-X", "+Y", "-Y", "+Z", "-Z")

    def __post_init__(self) -> None:
        for label, axis in (("up_axis", self.up_axis), ("forward_axis", self.forward_axis)):
            if axis not in self._AXES:
                raise ValidationError(f"{label} must be one of {self._AXES}, got {axis!r}")
        if self.up_axis.lstrip("+-") == self.forward_axis.lstrip("+-"):
            raise ValidationError("up_axis and forward_axis cannot share an axis")

    def compatible_with(self, other: CoordinateFrame) -> bool:
        return (
            self.up_axis == other.up_axis
            and self.forward_axis == other.forward_axis
            and self.handedness == other.handedness
            and self.units == other.units
        )

    def require_compatible(self, other: CoordinateFrame) -> None:
        if not self.compatible_with(other):
            raise ValidationError(
                f"incompatible coordinate frames: {self.name} vs {other.name}; "
                "normalize before fusing"
            )

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        value["handedness"] = self.handedness.value
        value["units"] = self.units.value
        value["scale_authority"] = self.scale_authority.value
        return value

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> CoordinateFrame:
        return cls(
            name=payload["name"],
            up_axis=payload.get("up_axis", "+Z"),
            forward_axis=payload.get("forward_axis", "-Y"),
            handedness=Handedness(payload.get("handedness", "right")),
            units=Units(payload.get("units", "m")),
            origin_semantics=payload.get("origin_semantics", "scene-origin"),
            scale_authority=AuthorityClass(payload.get("scale_authority", "UNRESOLVED")),
        )


#: Blender's native frame, and the V2 canonical world frame.
BLENDER_WORLD = CoordinateFrame(name="blender-world", up_axis="+Z", forward_axis="-Y")
#: glTF / three.js frame. Y-up, right handed, -Z forward.
GLTF_WORLD = CoordinateFrame(name="gltf-world", up_axis="+Y", forward_axis="-Z")
#: OpenCV camera frame: +X right, +Y down, +Z into the scene.
OPENCV_CAMERA = CoordinateFrame(
    name="opencv-camera", up_axis="-Y", forward_axis="+Z", origin_semantics="camera-centre"
)


@dataclass(slots=True)
class Uncertainty:
    """What a claim does not know, in the claim's own units."""

    kind: str = "unspecified"
    sigma: float | None = None
    interval: list[float] = field(default_factory=list)
    units: Units = Units.UNITLESS
    basis: str = "not-estimated"
    samples: int = 0

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        value["units"] = self.units.value
        return value

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> Uncertainty:
        return cls(
            kind=payload.get("kind", "unspecified"),
            sigma=payload.get("sigma"),
            interval=list(payload.get("interval", [])),
            units=Units(payload.get("units", "unitless")),
            basis=payload.get("basis", "not-estimated"),
            samples=int(payload.get("samples", 0)),
        )


class VisibilityState(StrEnum):
    """Bible section 7.3."""

    DIRECTLY_VISIBLE = "DIRECTLY_VISIBLE"
    PARTIALLY_VISIBLE = "PARTIALLY_VISIBLE"
    INFERRED_SURFACE = "INFERRED_SURFACE"
    NEVER_OBSERVED = "NEVER_OBSERVED"
    SYMMETRY_DERIVED = "SYMMETRY_DERIVED"
    TOPOLOGY_PRIOR = "TOPOLOGY_PRIOR"
    RETRIEVED_MODEL = "RETRIEVED_MODEL"


#: Visibility states that may never be described as observed truth.
UNOBSERVED_VISIBILITY: frozenset[VisibilityState] = frozenset(
    {
        VisibilityState.INFERRED_SURFACE,
        VisibilityState.NEVER_OBSERVED,
        VisibilityState.SYMMETRY_DERIVED,
        VisibilityState.TOPOLOGY_PRIOR,
        VisibilityState.RETRIEVED_MODEL,
    }
)


def visibility_authority_ceiling(state: VisibilityState | str) -> AuthorityClass:
    """The strongest authority a surface in this visibility state may carry."""
    resolved = VisibilityState(state)
    if resolved is VisibilityState.DIRECTLY_VISIBLE:
        return AuthorityClass.OBSERVED
    if resolved is VisibilityState.PARTIALLY_VISIBLE:
        return AuthorityClass.SENSOR_DERIVED
    if resolved is VisibilityState.RETRIEVED_MODEL:
        return AuthorityClass.MODEL_DERIVED
    return AuthorityClass.INFERRED
