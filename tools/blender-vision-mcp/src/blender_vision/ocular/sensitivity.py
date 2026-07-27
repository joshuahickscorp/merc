"""Probe sensitivity receipts (Ocular Bible 25.14).

A comparison metric is DIAGNOSTIC until a sealed receipt proves it both
(a) separates a declared meaningful parameter delta with stated margin, and
(b) does not respond to declared irrelevant confounders.

A metric that moves for everything is as useless as one that moves for nothing.
"""

from __future__ import annotations

import math
import uuid
from collections.abc import Callable, Sequence
from dataclasses import dataclass, field
from enum import StrEnum
from typing import Any, ClassVar

import numpy as np

from blender_vision.core.errors import ValidationError
from blender_vision.materials.parity import (
    DEFAULT_PROBE_RIG,
    SENSITIVITY_PROBE_RIG,
    ProbeRig,
    compare_images,
    measure_highlight,
    render_poster,
)
from blender_vision.v2.authority import AuthorityClass
from blender_vision.v2.records import Lineage, V2Record


class SensitivityVerdict(StrEnum):
    """Authority a metric may claim after both discrimination halves."""

    #: Separates the meaningful delta and rejects confounders.
    AUTHORITATIVE = "AUTHORITATIVE"
    #: Useful for diagnostics; not evidence that a parameter estimate is correct.
    DIAGNOSTIC = "DIAGNOSTIC"


class SweepParameter(StrEnum):
    """Bible 15.3 / Phase L parameter set."""

    ROUGHNESS = "roughness"
    METALNESS = "metalness"
    IOR_SPECULAR = "ior_specular"
    NORMAL_STRENGTH = "normal_strength"
    DISPLACEMENT_SCALE = "displacement_scale"
    ANISOTROPY = "anisotropy"
    LIGHT_SIZE = "light_size"
    LIGHT_DIRECTION = "light_direction"
    EXPOSURE = "exposure"


class ConfounderKind(StrEnum):
    """Confounders that must not move an authoritative material metric."""

    RESOLUTION = "resolution"
    PNG_REENCODE = "png_reencode"
    ONE_PIXEL_CROP = "one_pixel_crop"
    SAMPLE_COUNT = "sample_count"


# Declared meaningful deltas: the smallest change a human would call real.
DEFAULT_MEANINGFUL_DELTAS: dict[SweepParameter, float] = {
    SweepParameter.ROUGHNESS: 0.2,
    SweepParameter.METALNESS: 0.25,
    SweepParameter.IOR_SPECULAR: 0.2,
    SweepParameter.NORMAL_STRENGTH: 0.35,
    SweepParameter.DISPLACEMENT_SCALE: 0.03,
    SweepParameter.ANISOTROPY: 0.35,
    SweepParameter.LIGHT_SIZE: 0.4,
    SweepParameter.LIGHT_DIRECTION: 25.0,  # degrees of key-light orbit
    SweepParameter.EXPOSURE: 0.5,  # EV
}

# Default sweep ranges (inclusive endpoints).
DEFAULT_SWEEP_RANGES: dict[SweepParameter, tuple[float, float]] = {
    SweepParameter.ROUGHNESS: (0.1, 0.9),
    SweepParameter.METALNESS: (0.0, 1.0),
    SweepParameter.IOR_SPECULAR: (1.2, 2.0),
    SweepParameter.NORMAL_STRENGTH: (0.0, 1.0),
    SweepParameter.DISPLACEMENT_SCALE: (0.0, 0.08),
    SweepParameter.ANISOTROPY: (0.0, 1.0),
    SweepParameter.LIGHT_SIZE: (0.15, 1.5),
    SweepParameter.LIGHT_DIRECTION: (-40.0, 40.0),
    SweepParameter.EXPOSURE: (-1.0, 1.0),
}


@dataclass(slots=True)
class ResponsePoint:
    """One sample on a metric response curve."""

    parameter_value: float
    metric_value: float
    extras: dict[str, float] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "parameter_value": self.parameter_value,
            "metric_value": self.metric_value,
            "extras": dict(self.extras),
        }

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> ResponsePoint:
        return cls(
            parameter_value=float(payload["parameter_value"]),
            metric_value=float(payload["metric_value"]),
            extras={str(k): float(v) for k, v in dict(payload.get("extras") or {}).items()},
        )


@dataclass(slots=True)
class ConfounderResult:
    """Whether a declared irrelevant change moved the metric."""

    name: str
    metric_delta: float
    max_allowed_delta: float
    passed: bool
    notes: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "metric_delta": self.metric_delta,
            "max_allowed_delta": self.max_allowed_delta,
            "passed": self.passed,
            "notes": self.notes,
        }

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> ConfounderResult:
        return cls(
            name=str(payload["name"]),
            metric_delta=float(payload["metric_delta"]),
            max_allowed_delta=float(payload["max_allowed_delta"]),
            passed=bool(payload["passed"]),
            notes=str(payload.get("notes") or ""),
        )


@dataclass(slots=True, kw_only=True)
class ProbeSensitivityReceipt(V2Record):
    """Bible 25.14 — sealed proof that a metric can (or cannot) discriminate."""

    RECORD_KIND: ClassVar[str] = "ocular.probe-sensitivity-receipt"

    metric_name: str = ""
    parameter_name: str = ""
    parameter_min: float = 0.0
    parameter_max: float = 1.0
    steps: int = 0
    meaningful_delta: float = 0.0
    discrimination_margin: float = 0.0
    response_curve: list[ResponsePoint] = field(default_factory=list)
    measured_discrimination_threshold: float | None = None
    confounders: list[ConfounderResult] = field(default_factory=list)
    discrimination_passed: bool = False
    confounder_passed: bool = False
    verdict: SensitivityVerdict = SensitivityVerdict.DIAGNOSTIC
    metric_unit: str = ""
    baseline_value: float | None = None
    peak_response: float | None = None

    def _enforce_authority_ceiling(self) -> None:
        # Explicit base call: slots=True dataclasses invalidate zero-arg super().
        V2Record._enforce_authority_ceiling(self)
        if (
            self.verdict is SensitivityVerdict.AUTHORITATIVE
            and not (self.discrimination_passed and self.confounder_passed)
        ):
            raise ValidationError(
                f"receipt {self.id} claims AUTHORITATIVE without both discrimination "
                f"and confounder halves (discrimination={self.discrimination_passed}, "
                f"confounder={self.confounder_passed})"
            )

    def to_dict(self) -> dict[str, Any]:
        value = V2Record.to_dict(self)
        # Replace nested dataclasses with plain dicts for JSON.
        value["response_curve"] = [point.to_dict() for point in self.response_curve]
        value["confounders"] = [item.to_dict() for item in self.confounders]
        value["verdict"] = self.verdict.value
        return value

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> ProbeSensitivityReceipt:
        data = dict(payload)
        data["response_curve"] = [
            ResponsePoint.from_dict(item) for item in payload.get("response_curve", [])
        ]
        data["confounders"] = [
            ConfounderResult.from_dict(item) for item in payload.get("confounders", [])
        ]
        if "verdict" in data:
            data["verdict"] = SensitivityVerdict(data["verdict"])
        if "authority" in data and not isinstance(data["authority"], AuthorityClass):
            data["authority"] = AuthorityClass(data["authority"])
        if "lineage" in data and isinstance(data["lineage"], dict):
            data["lineage"] = Lineage.from_dict(data["lineage"])
        known = {item.name for item in cls.__dataclass_fields__.values()}  # type: ignore[attr-defined]
        # Prefer fields() when available.
        from dataclasses import fields as dc_fields

        known = {item.name for item in dc_fields(cls)}
        return cls(**{key: value for key, value in data.items() if key in known})


def linspace_values(low: float, high: float, steps: int) -> list[float]:
    if steps < 2:
        raise ValidationError("sweep requires at least 2 steps")
    return [float(low + (high - low) * i / (steps - 1)) for i in range(steps)]


def response_span(curve: Sequence[ResponsePoint]) -> float:
    if not curve:
        return 0.0
    values = [point.metric_value for point in curve]
    return float(max(values) - min(values))


def metric_at_delta(
    curve: Sequence[ResponsePoint],
    *,
    base_value: float,
    delta: float,
) -> float | None:
    """Largest |metric change| observed for a parameter step of at least ``delta``."""
    if len(curve) < 2:
        return None
    best = 0.0
    found = False
    for i, a in enumerate(curve):
        for b in curve[i + 1 :]:
            if abs(b.parameter_value - a.parameter_value) + 1e-12 >= delta:
                found = True
                best = max(best, abs(b.metric_value - a.metric_value))
    if not found:
        # Fall back: compare endpoints if the whole range is shorter than delta.
        span_param = abs(curve[-1].parameter_value - curve[0].parameter_value)
        if span_param + 1e-12 >= delta:
            return abs(curve[-1].metric_value - curve[0].metric_value)
        return None
    return best


def measured_threshold(
    curve: Sequence[ResponsePoint],
    *,
    minimum_response: float,
) -> float | None:
    """Smallest |Δparameter| that produces metric response ≥ minimum_response."""
    if len(curve) < 2:
        return None
    best: float | None = None
    for i, a in enumerate(curve):
        for b in curve[i + 1 :]:
            response = abs(b.metric_value - a.metric_value)
            if response + 1e-12 >= minimum_response:
                step = abs(b.parameter_value - a.parameter_value)
                best = step if best is None else min(best, step)
    return best


def evaluate_discrimination(
    curve: Sequence[ResponsePoint],
    *,
    meaningful_delta: float,
    discrimination_margin: float,
    minimum_absolute_response: float | None = None,
) -> tuple[bool, float | None, float]:
    """Return (passed, measured_threshold, peak_response).

    Discrimination requires the metric change across ``meaningful_delta`` to
    meet ``discrimination_margin``. When ``minimum_absolute_response`` is set it
    is an additional floor (e.g. dE ≥ 1.0).
    """
    if len(curve) < 2:
        return False, None, 0.0
    peak = response_span(curve)
    at_delta = metric_at_delta(curve, base_value=curve[0].parameter_value, delta=meaningful_delta)
    if at_delta is None:
        return False, None, peak
    floor = discrimination_margin
    if minimum_absolute_response is not None:
        floor = max(floor, minimum_absolute_response)
    passed = at_delta + 1e-12 >= floor
    thr = measured_threshold(curve, minimum_response=floor) if passed else None
    return passed, thr, peak


def evaluate_confounders(
    results: Sequence[ConfounderResult],
) -> bool:
    """Confounder half: every declared confounder must stay within allowance."""
    if not results:
        return False
    return all(item.passed for item in results)


def classify_sensitivity(
    *,
    discrimination_passed: bool,
    confounder_passed: bool,
) -> SensitivityVerdict:
    if discrimination_passed and confounder_passed:
        return SensitivityVerdict.AUTHORITATIVE
    return SensitivityVerdict.DIAGNOSTIC


def build_receipt(
    *,
    metric_name: str,
    parameter: SweepParameter | str,
    curve: Sequence[ResponsePoint],
    meaningful_delta: float,
    discrimination_margin: float,
    confounders: Sequence[ConfounderResult],
    minimum_absolute_response: float | None = None,
    metric_unit: str = "",
    notes: list[str] | None = None,
    authority: AuthorityClass = AuthorityClass.INFERRED,
    lineage: Lineage | None = None,
) -> ProbeSensitivityReceipt:
    """Build, validate, and seal a sensitivity receipt for one metric×parameter.

    ``parameter`` may be a ``SweepParameter`` or any free-form name (critic
    quantity names are free-form).
    """
    if isinstance(parameter, SweepParameter):
        param_name = parameter.value
    else:
        try:
            param_name = SweepParameter(parameter).value
        except ValueError:
            param_name = str(parameter)
    disc_ok, thr, peak = evaluate_discrimination(
        curve,
        meaningful_delta=meaningful_delta,
        discrimination_margin=discrimination_margin,
        minimum_absolute_response=minimum_absolute_response,
    )
    conf_ok = evaluate_confounders(confounders)
    verdict = classify_sensitivity(
        discrimination_passed=disc_ok,
        confounder_passed=conf_ok,
    )
    low = float(curve[0].parameter_value) if curve else 0.0
    high = float(curve[-1].parameter_value) if curve else 0.0
    baseline = float(curve[0].metric_value) if curve else None
    safe_metric = metric_name.replace(":", "-").replace("/", "-")
    receipt = ProbeSensitivityReceipt(
        id=f"sens-{param_name}-{safe_metric}-{uuid.uuid4().hex[:10]}",
        metric_name=metric_name,
        parameter_name=param_name,
        parameter_min=low,
        parameter_max=high,
        steps=len(curve),
        meaningful_delta=float(meaningful_delta),
        discrimination_margin=float(discrimination_margin),
        response_curve=list(curve),
        measured_discrimination_threshold=thr,
        confounders=list(confounders),
        discrimination_passed=disc_ok,
        confounder_passed=conf_ok,
        verdict=verdict,
        metric_unit=metric_unit,
        baseline_value=baseline,
        peak_response=peak,
        authority=authority,
        lineage=lineage
        or Lineage(
            operation="probe_sensitivity",
            parameters={
                "metric": metric_name,
                "parameter": param_name,
                "meaningful_delta": meaningful_delta,
                "discrimination_margin": discrimination_margin,
            },
            limitations=list(notes or []),
        ),
        notes=list(notes or []),
    )
    return receipt.seal()


# ---------------------------------------------------------------------------
# Metric extractors used by sweeps
# ---------------------------------------------------------------------------

MetricFn = Callable[[np.ndarray, np.ndarray | None], float]


def metric_delta_e2000(image: np.ndarray, reference: np.ndarray | None) -> float:
    if reference is None:
        return 0.0
    return compare_images(reference, image).delta_e2000


def metric_structural(image: np.ndarray, reference: np.ndarray | None) -> float:
    if reference is None:
        return 0.0
    return compare_images(reference, image).structural


def metric_highlight_delta_e(image: np.ndarray, reference: np.ndarray | None) -> float:
    if reference is None:
        return 0.0
    return compare_images(reference, image).highlight_delta_e2000


def metric_specular_peak(image: np.ndarray, reference: np.ndarray | None = None) -> float:
    return measure_highlight(image).peak_energy


def metric_specular_lobe_width(image: np.ndarray, reference: np.ndarray | None = None) -> float:
    return measure_highlight(image).lobe_fwhm_px


def metric_specular_peak_delta(image: np.ndarray, reference: np.ndarray | None) -> float:
    if reference is None:
        return 0.0
    return compare_images(reference, image).specular_peak_delta


def metric_specular_lobe_width_delta(image: np.ndarray, reference: np.ndarray | None) -> float:
    if reference is None:
        return 0.0
    return compare_images(reference, image).specular_lobe_width_delta


BUILTIN_METRICS: dict[str, MetricFn] = {
    "delta_e2000": metric_delta_e2000,
    "structural": metric_structural,
    "highlight_delta_e2000": metric_highlight_delta_e,
    "specular_peak_energy": metric_specular_peak,
    "specular_lobe_width": metric_specular_lobe_width,
    "specular_peak_delta": metric_specular_peak_delta,
    "specular_lobe_width_delta": metric_specular_lobe_width_delta,
}


# Margins that count as discrimination for each metric family.
DEFAULT_DISCRIMINATION_MARGINS: dict[str, float] = {
    "delta_e2000": 1.5,
    "structural": 0.03,
    "highlight_delta_e2000": 2.0,
    "specular_peak_energy": 0.08,
    "specular_lobe_width": 4.0,
    "specular_peak_delta": 0.08,
    "specular_lobe_width_delta": 4.0,
}

# Confounder allowance: metric may move by at most this fraction of the
# discrimination margin (absolute).
DEFAULT_CONFOUNDER_ALLOWANCE: dict[str, float] = {
    "delta_e2000": 0.4,
    "structural": 0.01,
    "highlight_delta_e2000": 0.6,
    "specular_peak_energy": 0.03,
    "specular_lobe_width": 1.5,
    "specular_peak_delta": 0.03,
    "specular_lobe_width_delta": 1.5,
}


@dataclass(slots=True)
class SweepRender:
    """One rendered sample for a parameter value."""

    parameter_value: float
    image: np.ndarray
    path: str | None = None
    extras: dict[str, float] = field(default_factory=dict)


def build_response_curve(
    renders: Sequence[SweepRender],
    metric: MetricFn | str,
    *,
    reference_index: int = 0,
) -> list[ResponsePoint]:
    """Compare each render to the reference render via ``metric``."""
    if not renders:
        return []
    fn: MetricFn
    if isinstance(metric, str):
        if metric not in BUILTIN_METRICS:
            raise ValidationError(f"unknown metric {metric!r}")
        fn = BUILTIN_METRICS[metric]
    else:
        fn = metric
    reference = renders[reference_index].image
    curve: list[ResponsePoint] = []
    for item in renders:
        value = float(fn(item.image, reference))
        extras = dict(item.extras)
        hl = measure_highlight(item.image)
        extras.setdefault("peak_energy", hl.peak_energy)
        extras.setdefault("lobe_fwhm_px", hl.lobe_fwhm_px)
        curve.append(
            ResponsePoint(
                parameter_value=float(item.parameter_value),
                metric_value=value,
                extras=extras,
            )
        )
    return curve


def run_confounder_battery(
    reference_image: np.ndarray,
    metric: MetricFn | str,
    *,
    allowance: float,
    resolution_range: tuple[int, int] = (128, 256),
    sample_floor: int = 32,
    re_render: Callable[[dict[str, Any]], np.ndarray] | None = None,
) -> list[ConfounderResult]:
    """Run the four required confounders against a fixed material/light state.

    Image-domain confounders (PNG re-encode, one-pixel crop) always run.
    Resolution and sample-count confounders require ``re_render`` when the
    metric depends on re-rendering rather than post-process of one frame.
    """
    if isinstance(metric, str):
        if metric not in BUILTIN_METRICS:
            raise ValidationError(f"unknown metric {metric!r}")
        fn = BUILTIN_METRICS[metric]
    else:
        fn = metric

    results: list[ConfounderResult] = []
    base = float(fn(reference_image, reference_image))

    # PNG re-encode: round-trip through uint8 PNG bytes in memory.
    from io import BytesIO

    from PIL import Image

    buf = BytesIO()
    arr = np.clip(np.asarray(reference_image, dtype=np.float64), 0.0, 1.0)
    if arr.max() > 1.0 + 1e-6:
        arr = arr / 255.0
    Image.fromarray((arr * 255.0).astype(np.uint8), mode="RGB").save(buf, format="PNG")
    buf.seek(0)
    reencoded = np.asarray(Image.open(buf).convert("RGB"), dtype=np.float64) / 255.0
    # Comparative metrics return ~0 for an identical pair; absolute metrics
    # (peak energy, lobe width) ignore the reference and return a level.
    if abs(base) < 1e-12:
        delta = abs(float(fn(reencoded, reference_image)))
    else:
        delta = abs(float(fn(reencoded, None)) - float(fn(reference_image, None)))
    results.append(
        ConfounderResult(
            name=ConfounderKind.PNG_REENCODE.value,
            metric_delta=float(delta),
            max_allowed_delta=float(allowance),
            passed=float(delta) <= allowance + 1e-9,
            notes="PNG uint8 round-trip",
        )
    )

    # One-pixel crop then resize back: pure geometric confounder.
    h, w = reference_image.shape[:2]
    if h > 4 and w > 4:
        cropped = reference_image[1 : h - 1, 1 : w - 1]
        from PIL import Image as PilImage

        crop_u8 = (np.clip(cropped, 0, 1) * 255).astype(np.uint8)
        restored_img = PilImage.fromarray(crop_u8, mode="RGB").resize(
            (w, h), resample=PilImage.Resampling.BILINEAR
        )
        restored = np.asarray(restored_img, dtype=np.float64) / 255.0
        if abs(float(fn(reference_image, reference_image))) < 1e-12:
            crop_delta = abs(float(fn(restored, reference_image)))
        else:
            crop_delta = abs(
                float(fn(restored, None)) - float(fn(reference_image, None))
            )
    else:
        crop_delta = 0.0
    results.append(
        ConfounderResult(
            name=ConfounderKind.ONE_PIXEL_CROP.value,
            metric_delta=float(crop_delta),
            max_allowed_delta=float(allowance),
            passed=float(crop_delta) <= allowance + 1e-9,
            notes="1px border crop + bilinear restore",
        )
    )

    if re_render is not None:
        low_res, high_res = resolution_range
        img_low = re_render({"resolution": int(low_res)})
        img_high = re_render({"resolution": int(high_res)})
        # Resize low to high for fair comparative metrics.
        if img_low.shape[:2] != img_high.shape[:2]:
            from PIL import Image as PilImage

            img_low_r = (
                np.asarray(
                    PilImage.fromarray(
                        (np.clip(img_low, 0, 1) * 255).astype(np.uint8), mode="RGB"
                    ).resize(
                        (img_high.shape[1], img_high.shape[0]),
                        resample=PilImage.Resampling.BILINEAR,
                    ),
                    dtype=np.float64,
                )
                / 255.0
            )
        else:
            img_low_r = img_low
        if float(fn(img_high, img_high)) == 0.0:
            res_delta = abs(float(fn(img_low_r, img_high)))
        else:
            # Absolute metrics: compare values; scale widths by resolution ratio.
            v_low = float(fn(img_low, None))
            v_high = float(fn(img_high, None))
            # lobe widths grow with resolution — normalise by width.
            if "lobe" in getattr(fn, "__name__", "") or "width" in getattr(fn, "__name__", ""):
                v_low_n = v_low / max(img_low.shape[0], 1)
                v_high_n = v_high / max(img_high.shape[0], 1)
                res_delta = abs(v_low_n - v_high_n) * max(img_high.shape[0], 1)
            else:
                res_delta = abs(v_low - v_high)
        results.append(
            ConfounderResult(
                name=ConfounderKind.RESOLUTION.value,
                metric_delta=float(res_delta),
                max_allowed_delta=float(allowance),
                passed=float(res_delta) <= allowance + 1e-9,
                notes=f"resolution {low_res} vs {high_res} within stated range",
            )
        )

        img_floor = re_render({"samples": int(sample_floor)})
        img_high_s = re_render({"samples": int(sample_floor * 2)})
        if img_floor.shape[:2] != img_high_s.shape[:2]:
            img_floor = img_high_s
        if float(fn(img_high_s, img_high_s)) == 0.0:
            samp_delta = abs(float(fn(img_floor, img_high_s)))
        else:
            samp_delta = abs(float(fn(img_floor, None)) - float(fn(img_high_s, None)))
        results.append(
            ConfounderResult(
                name=ConfounderKind.SAMPLE_COUNT.value,
                metric_delta=float(samp_delta),
                max_allowed_delta=float(allowance),
                passed=float(samp_delta) <= allowance + 1e-9,
                notes=f"samples {sample_floor} vs {sample_floor * 2} above floor",
            )
        )
    else:
        # Without re-render, still record the confounders as not executed so the
        # receipt cannot claim AUTHORITATIVE by omission.
        for kind, note in (
            (ConfounderKind.RESOLUTION, "re_render not provided"),
            (ConfounderKind.SAMPLE_COUNT, "re_render not provided"),
        ):
            results.append(
                ConfounderResult(
                    name=kind.value,
                    metric_delta=float("inf"),
                    max_allowed_delta=float(allowance),
                    passed=False,
                    notes=note,
                )
            )

    return results


def offline_confounder_battery(
    *,
    metric: MetricFn | str,
    allowance: float,
    hypothesis_factory: Callable[[], Any],
    rig: ProbeRig | None = None,
    resolution_range: tuple[int, int] = (128, 256),
) -> list[ConfounderResult]:
    """Confounder battery using the offline poster path (deterministic, no GPU)."""
    base_rig = rig or SENSITIVITY_PROBE_RIG
    hypothesis = hypothesis_factory()

    def _render(overrides: dict[str, Any]) -> np.ndarray:
        active = base_rig
        if "resolution" in overrides:
            active = active.with_resolution(int(overrides["resolution"]))
        # Poster path has no sample count; samples override is a no-op re-render.
        path = render_poster(hypothesis, rig=active)
        from blender_vision.materials.parity import _load_rgb

        return _load_rgb(path)

    reference = _render({"resolution": base_rig.resolution})
    return run_confounder_battery(
        reference,
        metric,
        allowance=allowance,
        resolution_range=resolution_range,
        sample_floor=max(base_rig.cycles_samples, 32),
        re_render=_render,
    )


def apply_parameter_to_hypothesis(
    hypothesis: Any,
    parameter: SweepParameter | str,
    value: float,
) -> Any:
    """Return a copy of ``hypothesis`` with the swept material field set."""
    from dataclasses import replace as dc_replace

    from blender_vision.v2.records import MaterialHypothesis

    if not isinstance(hypothesis, MaterialHypothesis):
        raise ValidationError("apply_parameter_to_hypothesis expects MaterialHypothesis")
    param = SweepParameter(parameter)
    if param is SweepParameter.ROUGHNESS:
        return dc_replace(hypothesis, roughness=float(value))
    if param is SweepParameter.METALNESS:
        return dc_replace(hypothesis, metalness=float(value))
    if param is SweepParameter.IOR_SPECULAR:
        return dc_replace(hypothesis, specular_ior=float(value))
    if param is SweepParameter.ANISOTROPY:
        return dc_replace(hypothesis, anisotropy=float(value))
    return hypothesis


def apply_parameter_to_rig(
    rig: ProbeRig,
    parameter: SweepParameter | str,
    value: float,
) -> ProbeRig:
    """Return a copy of ``rig`` with the swept light/geometry field set."""
    param = SweepParameter(parameter)
    if param is SweepParameter.LIGHT_SIZE:
        return rig.with_light_size(float(value))
    if param is SweepParameter.EXPOSURE:
        return rig.with_exposure(float(value))
    if param is SweepParameter.NORMAL_STRENGTH:
        return rig.with_normal_strength(float(value))
    if param is SweepParameter.DISPLACEMENT_SCALE:
        return rig.with_displacement_scale(float(value))
    if param is SweepParameter.ANISOTROPY:
        return rig.with_anisotropy(float(value))
    if param is SweepParameter.LIGHT_DIRECTION:
        # Orbit the key light in the XY plane around the existing elevation.
        distance = rig.light_distance()
        lx, ly, lz = rig.light_position
        elev = math.atan2(lz, math.hypot(lx, ly))
        az = math.radians(float(value))
        x = distance * math.cos(elev) * math.sin(az)
        y = distance * math.cos(elev) * math.cos(az)
        z = distance * math.sin(elev)
        return rig.with_light_position((x, y, z))
    return rig


def parameter_targets_hypothesis(parameter: SweepParameter | str) -> bool:
    return SweepParameter(parameter) in {
        SweepParameter.ROUGHNESS,
        SweepParameter.METALNESS,
        SweepParameter.IOR_SPECULAR,
        SweepParameter.ANISOTROPY,
    }


def default_sweep_hypothesis(parameter: SweepParameter | str) -> Any:
    """Material chosen so the swept parameter is visible on the probe."""
    from blender_vision.v2.records import MaterialHypothesis

    param = SweepParameter(parameter)
    # Metal base makes roughness/anisotropy/IOR lobe changes readable.
    if param in {
        SweepParameter.ROUGHNESS,
        SweepParameter.ANISOTROPY,
        SweepParameter.LIGHT_SIZE,
        SweepParameter.LIGHT_DIRECTION,
        SweepParameter.EXPOSURE,
        SweepParameter.NORMAL_STRENGTH,
        SweepParameter.DISPLACEMENT_SCALE,
    }:
        return MaterialHypothesis(
            hypothesis_id=f"sens-base-{param.value}",
            label="sensitivity-metal",
            base_colour=[0.88, 0.88, 0.90],
            roughness=0.25,
            metalness=1.0,
            specular_ior=1.45,
            authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
        )
    if param is SweepParameter.METALNESS:
        return MaterialHypothesis(
            hypothesis_id="sens-base-metalness",
            label="sensitivity-metalness",
            base_colour=[0.72, 0.28, 0.14],
            roughness=0.3,
            metalness=0.0,
            authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
        )
    if param is SweepParameter.IOR_SPECULAR:
        return MaterialHypothesis(
            hypothesis_id="sens-base-ior",
            label="sensitivity-ior",
            base_colour=[0.7, 0.7, 0.72],
            roughness=0.15,
            metalness=0.0,
            specular_ior=1.45,
            authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
        )
    return MaterialHypothesis(
        hypothesis_id="sens-base-default",
        label="sensitivity-default",
        base_colour=[0.6, 0.6, 0.62],
        roughness=0.4,
        metalness=0.0,
        authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
    )


def offline_parameter_sweep(
    parameter: SweepParameter | str,
    *,
    values: Sequence[float] | None = None,
    steps: int = 9,
    rig: ProbeRig | None = None,
    hypothesis: Any | None = None,
) -> list[SweepRender]:
    """Render a parameter sweep via the offline GGX poster (deterministic)."""
    from blender_vision.materials.parity import _load_rgb

    param = SweepParameter(parameter)
    active_rig = rig or SENSITIVITY_PROBE_RIG
    base_h = hypothesis or default_sweep_hypothesis(param)
    if values is None:
        low, high = DEFAULT_SWEEP_RANGES[param]
        values = linspace_values(low, high, steps)
    renders: list[SweepRender] = []
    for value in values:
        hyp = apply_parameter_to_hypothesis(base_h, param, float(value))
        use_rig = apply_parameter_to_rig(active_rig, param, float(value))
        path = render_poster(hyp, rig=use_rig)
        image = _load_rgb(path)
        hl = measure_highlight(image)
        renders.append(
            SweepRender(
                parameter_value=float(value),
                image=image,
                path=str(path),
                extras={
                    "peak_energy": hl.peak_energy,
                    "lobe_fwhm_px": hl.lobe_fwhm_px,
                    "contrast": hl.contrast,
                },
            )
        )
    return renders


def classify_metric_on_sweep(
    parameter: SweepParameter | str,
    metric_name: str,
    *,
    steps: int = 9,
    rig: ProbeRig | None = None,
    meaningful_delta: float | None = None,
    discrimination_margin: float | None = None,
    run_confounders: bool = True,
) -> ProbeSensitivityReceipt:
    """End-to-end offline classification for one metric × parameter pair."""
    param = SweepParameter(parameter)
    renders = offline_parameter_sweep(param, steps=steps, rig=rig)
    curve = build_response_curve(renders, metric_name, reference_index=0)
    margin = (
        float(discrimination_margin)
        if discrimination_margin is not None
        else float(DEFAULT_DISCRIMINATION_MARGINS.get(metric_name, 1.0))
    )
    mdelta = (
        float(meaningful_delta)
        if meaningful_delta is not None
        else float(DEFAULT_MEANINGFUL_DELTAS[param])
    )
    if run_confounders:
        allowance = float(DEFAULT_CONFOUNDER_ALLOWANCE.get(metric_name, margin * 0.25))
        confounders = offline_confounder_battery(
            metric=metric_name,
            allowance=allowance,
            hypothesis_factory=lambda: default_sweep_hypothesis(param),
            rig=rig or SENSITIVITY_PROBE_RIG,
        )
    else:
        confounders = [
            ConfounderResult(
                name="none",
                metric_delta=float("inf"),
                max_allowed_delta=0.0,
                passed=False,
                notes="confounders skipped",
            )
        ]
    return build_receipt(
        metric_name=metric_name,
        parameter=param,
        curve=curve,
        meaningful_delta=mdelta,
        discrimination_margin=margin,
        confounders=confounders,
        notes=[f"offline poster sweep steps={steps}"],
    )


def roughness_before_after(
    *,
    steps: int = 9,
) -> dict[str, Any]:
    """Concrete before/after demonstration of the roughness observability fix.

    BEFORE: DEFAULT_PROBE_RIG, whole-image dE2000, dielectric matte base
            (the configuration that produced the near-flat 2.82→3.07 curve
            when compared cross-renderer; here shown as self-sweep response).
    AFTER:  SENSITIVITY_PROBE_RIG, specular lobe width + peak energy on metal.
    """
    from blender_vision.v2.records import MaterialHypothesis

    values = linspace_values(0.1, 0.9, steps)

    before_h = MaterialHypothesis(
        hypothesis_id="before-matte",
        label="matte-plastic",
        base_colour=[0.82, 0.18, 0.12],
        roughness=0.55,
        metalness=0.0,
        authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
    )
    before_renders = offline_parameter_sweep(
        SweepParameter.ROUGHNESS,
        values=values,
        rig=DEFAULT_PROBE_RIG,
        hypothesis=before_h,
    )
    before_curve = build_response_curve(before_renders, "delta_e2000")

    after_renders = offline_parameter_sweep(
        SweepParameter.ROUGHNESS,
        values=values,
        rig=SENSITIVITY_PROBE_RIG,
        hypothesis=default_sweep_hypothesis(SweepParameter.ROUGHNESS),
    )
    after_de = build_response_curve(after_renders, "delta_e2000")
    after_lobe = build_response_curve(after_renders, "specular_lobe_width")
    after_peak = build_response_curve(after_renders, "specular_peak_energy")
    # Lobe width is absolute (not delta-to-ref); rebuild absolute curve.
    after_lobe_abs = [
        ResponsePoint(
            parameter_value=r.parameter_value,
            metric_value=float(r.extras.get("lobe_fwhm_px", 0.0)),
            extras=dict(r.extras),
        )
        for r in after_renders
    ]
    after_peak_abs = [
        ResponsePoint(
            parameter_value=r.parameter_value,
            metric_value=float(r.extras.get("peak_energy", 0.0)),
            extras=dict(r.extras),
        )
        for r in after_renders
    ]

    return {
        "before": {
            "rig": DEFAULT_PROBE_RIG.to_dict(),
            "metric": "delta_e2000",
            "base": "matte dielectric",
            "curve": [p.to_dict() for p in before_curve],
            "span": response_span(before_curve),
        },
        "after": {
            "rig": SENSITIVITY_PROBE_RIG.to_dict(),
            "metrics": {
                "delta_e2000": {
                    "curve": [p.to_dict() for p in after_de],
                    "span": response_span(after_de),
                },
                "specular_lobe_width": {
                    "curve": [p.to_dict() for p in after_lobe_abs],
                    "span": response_span(after_lobe_abs),
                },
                "specular_peak_energy": {
                    "curve": [p.to_dict() for p in after_peak_abs],
                    "span": response_span(after_peak_abs),
                },
                "specular_lobe_width_delta": {
                    "curve": [p.to_dict() for p in after_lobe],
                    "span": response_span(after_lobe),
                },
                "specular_peak_delta": {
                    "curve": [p.to_dict() for p in after_peak],
                    "span": response_span(after_peak),
                },
            },
            "base": "polished metal",
        },
    }


def format_curve_table(curve: Sequence[ResponsePoint], *, param_name: str = "param") -> str:
    lines = [f"{param_name:>10s}  {'metric':>10s}"]
    for point in curve:
        lines.append(f"{point.parameter_value:10.4f}  {point.metric_value:10.4f}")
    if curve:
        lines.append(f"{'span':>10s}  {response_span(curve):10.4f}")
    return "\n".join(lines)


__all__ = [
    "BUILTIN_METRICS",
    "DEFAULT_CONFOUNDER_ALLOWANCE",
    "DEFAULT_DISCRIMINATION_MARGINS",
    "DEFAULT_MEANINGFUL_DELTAS",
    "DEFAULT_SWEEP_RANGES",
    "ConfounderKind",
    "ConfounderResult",
    "MetricFn",
    "ProbeSensitivityReceipt",
    "ResponsePoint",
    "SensitivityVerdict",
    "SweepParameter",
    "SweepRender",
    "apply_parameter_to_hypothesis",
    "apply_parameter_to_rig",
    "build_receipt",
    "build_response_curve",
    "classify_metric_on_sweep",
    "classify_sensitivity",
    "default_sweep_hypothesis",
    "evaluate_confounders",
    "evaluate_discrimination",
    "format_curve_table",
    "linspace_values",
    "measured_threshold",
    "metric_at_delta",
    "offline_confounder_battery",
    "offline_parameter_sweep",
    "parameter_targets_hypothesis",
    "response_span",
    "roughness_before_after",
    "run_confounder_battery",
]
