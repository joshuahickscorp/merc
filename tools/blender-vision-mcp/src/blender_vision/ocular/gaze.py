"""Gaze control: fixation, saccade, zoom, and inhibition of return.

A region is not re-fixated unless uncertainty stayed high, evidence changed,
or a critic asked. Attention budgets record cost, latency, and gain.
"""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from typing import Any

import cv2
import numpy as np

from blender_vision.core.errors import ValidationError
from blender_vision.ocular.records import (
    AttentionBudget,
    Fixation,
    FixationOutcome,
    FixationReason,
    SaccadePlan,
    default_lineage,
)
from blender_vision.ocular.retina import foveal_crop
from blender_vision.v2.authority import AuthorityClass


def _region_key(region: list[float], quantize: float = 16.0) -> str:
    x, y, w, h = (float(v) for v in region[:4])
    return (
        f"{int(x // quantize)}:{int(y // quantize)}:"
        f"{int(w // quantize)}:{int(h // quantize)}"
    )


def _region_centre(region: list[float]) -> tuple[float, float]:
    x, y, w, h = (float(v) for v in region[:4])
    return (x + w / 2.0, y + h / 2.0)


def _region_distance(a: list[float], b: list[float]) -> float:
    ca = _region_centre(a)
    cb = _region_centre(b)
    return float(np.hypot(ca[0] - cb[0], ca[1] - cb[1]))


@dataclass(slots=True)
class GazeController:
    """Stateful gaze with inhibition-of-return and an attention budget ledger."""

    ior_radius_px: float = 40.0
    ior_decay_frames: int = 30
    uncertainty_revisit_threshold: float = 0.55
    stream_id: str = ""
    _fixations: list[Fixation] = field(default_factory=list)
    _ior: dict[str, dict[str, Any]] = field(default_factory=dict)
    _frame_counter: int = 0
    _current_region: list[float] = field(default_factory=lambda: [0.0, 0.0, 0.0, 0.0])
    _budgets: list[AttentionBudget] = field(default_factory=list)

    def tick(self) -> None:
        self._frame_counter += 1
        expired = [
            key
            for key, meta in self._ior.items()
            if self._frame_counter - int(meta["frame"]) > self.ior_decay_frames
        ]
        for key in expired:
            del self._ior[key]

    def _ior_blocks(
        self,
        region: list[float],
        *,
        uncertainty: float,
        evidence_changed: bool,
        critic_requested: bool,
    ) -> tuple[bool, str]:
        if critic_requested:
            return False, ""
        if evidence_changed:
            return False, ""
        if uncertainty >= self.uncertainty_revisit_threshold:
            return False, ""
        centre = _region_centre(region)
        for key, meta in self._ior.items():
            prev = meta["centre"]
            if float(np.hypot(centre[0] - prev[0], centre[1] - prev[1])) <= self.ior_radius_px:
                return True, f"inhibition_of_return:{key}"
        return False, ""

    def fixate(
        self,
        region: list[float],
        *,
        reason: FixationReason | str = FixationReason.SALIENCE,
        expected_information: float = 0.5,
        duration_ms: float = 100.0,
        resolution: list[int] | None = None,
        models_requested: list[str] | None = None,
        target: str = "",
        frame_id: str = "",
        uncertainty: float = 0.0,
        evidence_changed: bool = False,
        critic_requested: bool = False,
        compute_cost_ms: float = 0.0,
        actual_gain: float | None = None,
    ) -> Fixation:
        if len(region) < 4:
            raise ValidationError("region must be [x, y, w, h]")
        reason_e = FixationReason(reason)
        res = list(resolution or [0, 0])
        models = list(models_requested or [])

        blocked, block_reason = self._ior_blocks(
            region,
            uncertainty=uncertainty,
            evidence_changed=evidence_changed,
            critic_requested=critic_requested,
        )

        t0 = time.perf_counter()
        latency_ms = (time.perf_counter() - t0) * 1000.0 + compute_cost_ms
        gain = float(actual_gain) if actual_gain is not None else (
            0.0 if blocked else expected_information
        )

        budget = AttentionBudget(
            id=f"budget-{self.stream_id}-{self._frame_counter}-{len(self._budgets)}",
            compute_cost_ms=compute_cost_ms,
            latency_ms=latency_ms,
            expected_gain=expected_information,
            actual_gain=gain,
            redundant_observations=1 if blocked else 0,
            resolution=res,
            models_requested=models,
            authority=AuthorityClass.SENSOR_DERIVED,
            lineage=default_lineage("ocular.gaze.budget"),
        ).seal()
        self._budgets.append(budget)

        if blocked:
            fixation = Fixation(
                id=f"fix-{self.stream_id}-{len(self._fixations)}",
                target=target or _region_key(region),
                region=[float(v) for v in region[:4]],
                reason=reason_e,
                expected_information=expected_information,
                duration_ms=duration_ms,
                resolution=res,
                models_requested=models,
                outcome=FixationOutcome.SUPPRESSED_IOR,
                stream_id=self.stream_id,
                frame_id=frame_id,
                budget=budget.to_dict(),
                uncertainty_at_start=uncertainty,
                uncertainty_at_end=uncertainty,
                evidence_changed=evidence_changed,
                critic_requested=critic_requested,
                notes=[block_reason],
                authority=AuthorityClass.SENSOR_DERIVED,
                lineage=default_lineage("ocular.gaze.fixate"),
            ).seal()
            self._fixations.append(fixation)
            return fixation

        fixation = Fixation(
            id=f"fix-{self.stream_id}-{len(self._fixations)}",
            target=target or _region_key(region),
            region=[float(v) for v in region[:4]],
            reason=reason_e,
            expected_information=expected_information,
            duration_ms=duration_ms,
            resolution=res,
            models_requested=models,
            outcome=FixationOutcome.OBSERVED,
            stream_id=self.stream_id,
            frame_id=frame_id,
            budget=budget.to_dict(),
            uncertainty_at_start=uncertainty,
            uncertainty_at_end=max(0.0, uncertainty - gain),
            evidence_changed=evidence_changed,
            critic_requested=critic_requested,
            authority=AuthorityClass.SENSOR_DERIVED,
            lineage=default_lineage("ocular.gaze.fixate"),
        ).seal()
        self._fixations.append(fixation)
        self._current_region = [float(v) for v in region[:4]]
        centre = _region_centre(region)
        self._ior[_region_key(region)] = {
            "centre": centre,
            "frame": self._frame_counter,
            "fixation_id": fixation.id,
        }
        return fixation

    def saccade(
        self,
        to_region: list[float],
        *,
        reason: FixationReason | str = FixationReason.SALIENCE,
        expected_information: float = 0.5,
        from_region: list[float] | None = None,
        uncertainty: float = 0.0,
        evidence_changed: bool = False,
        critic_requested: bool = False,
        duration_ms: float = 40.0,
    ) -> SaccadePlan:
        if len(to_region) < 4:
            raise ValidationError("to_region must be [x, y, w, h]")
        origin = list(from_region or self._current_region or [0.0, 0.0, 0.0, 0.0])
        reason_e = FixationReason(reason)
        amplitude = _region_distance(origin, to_region)
        blocked, block_reason = self._ior_blocks(
            to_region,
            uncertainty=uncertainty,
            evidence_changed=evidence_changed,
            critic_requested=critic_requested,
        )
        from_id = self._fixations[-1].id if self._fixations else ""
        plan = SaccadePlan(
            id=f"sac-{self.stream_id}-{len(self._fixations)}",
            from_region=[float(v) for v in origin[:4]],
            to_region=[float(v) for v in to_region[:4]],
            reason=reason_e,
            expected_information=expected_information,
            amplitude_norm=amplitude,
            duration_ms=duration_ms,
            stream_id=self.stream_id,
            from_fixation_id=from_id,
            to_fixation_id="",
            inhibited=blocked,
            inhibition_reason=block_reason,
            authority=AuthorityClass.SENSOR_DERIVED,
            lineage=default_lineage("ocular.gaze.saccade"),
        ).seal()
        if not blocked:
            # Commit the landing fixation so IOR records the destination.
            landed = self.fixate(
                to_region,
                reason=reason_e,
                expected_information=expected_information,
                duration_ms=duration_ms,
                uncertainty=uncertainty,
                evidence_changed=evidence_changed,
                critic_requested=critic_requested,
            )
            # Update sealed plan's to_fixation via a new sealed record would be
            # cleaner; for accounting we note the id in notes of the plan.
            object.__setattr__(plan, "to_fixation_id", landed.id)
        return plan

    def zoom(
        self,
        image: np.ndarray,
        region: list[float],
        *,
        out_resolution: tuple[int, int] = (128, 128),
    ) -> np.ndarray:
        """Foveated zoom: crop region and resample to the requested resolution."""
        if len(region) < 4:
            raise ValidationError("region must be [x, y, w, h]")
        x, y, w, h = (int(round(float(v))) for v in region[:4])
        x = max(0, x)
        y = max(0, y)
        h_img, w_img = image.shape[:2]
        w = max(1, min(w, w_img - x))
        h = max(1, min(h, h_img - y))
        crop = image[y : y + h, x : x + w]
        if crop.size == 0:
            centre = _region_centre(region)
            return foveal_crop(
                image, centre, size=out_resolution, out_resolution=out_resolution
            )
        return cv2.resize(crop, out_resolution, interpolation=cv2.INTER_LINEAR)

    def history(self) -> list[Fixation]:
        return list(self._fixations)

    def budgets(self) -> list[AttentionBudget]:
        return list(self._budgets)


def fixate(controller: GazeController, region: list[float], **kwargs: Any) -> Fixation:
    return controller.fixate(region, **kwargs)


def saccade(controller: GazeController, to_region: list[float], **kwargs: Any) -> SaccadePlan:
    return controller.saccade(to_region, **kwargs)


def zoom(
    controller: GazeController,
    image: np.ndarray,
    region: list[float],
    **kwargs: Any,
) -> np.ndarray:
    return controller.zoom(image, region, **kwargs)
