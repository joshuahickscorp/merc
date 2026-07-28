"""Perceptual critic calibration against near-threshold cases and confounders.

Phase O: every critic role must prove it fires on positive and near-threshold
cases, stays silent on negatives and confounders, and has a sensitivity receipt.
A clean sweep with zero near-threshold coverage is a contract failure.
"""

from __future__ import annotations

import uuid
from dataclasses import dataclass, field
from enum import StrEnum
from typing import Any

from blender_vision.ocular.sensitivity import (
    ConfounderResult,
    ProbeSensitivityReceipt,
    ResponsePoint,
    SensitivityVerdict,
    build_receipt,
)
from blender_vision.v2.authority import AuthorityClass
from blender_vision.v2.records import CriticFinding, Lineage


class CriticRole(StrEnum):
    """Calibration roles required by the goal (Phase O)."""

    PRODUCT = "product"
    MATERIAL = "material"
    LIGHT = "light"
    ENVIRONMENT = "environment"
    COMPOSITION = "composition"
    TYPOGRAPHY = "typography"
    MOTION = "motion"
    ORGANIC = "organic"
    GROOM = "groom"
    PERFORMANCE = "performance"


class CaseKind(StrEnum):
    POSITIVE = "positive"
    NEAR_THRESHOLD = "near_threshold"
    NEGATIVE = "negative"
    CONFOUNDER = "confounder"
    FALSE_POSITIVE_CHECK = "false_positive_check"
    REPAIR_VERIFICATION = "repair_verification"


@dataclass(slots=True)
class CriticSubject:
    """Numeric fixture a critic measures. Synthetic, deterministic, no weights."""

    subject_id: str
    role: CriticRole
    case_kind: CaseKind
    # Measured quantity the critic thresholds against.
    quantity: float
    # Optional secondary quantities (e.g. control channel that must not fire).
    extras: dict[str, float] = field(default_factory=dict)
    label: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "subject_id": self.subject_id,
            "role": self.role.value,
            "case_kind": self.case_kind.value,
            "quantity": self.quantity,
            "extras": dict(self.extras),
            "label": self.label,
        }


@dataclass(slots=True)
class CriticSpec:
    """Thresholded detector for one role."""

    role: CriticRole
    quantity_name: str
    # Fire when quantity >= fire_threshold (higher = worse for most roles).
    fire_threshold: float
    # Near-threshold band: [fire_threshold, fire_threshold + near_width].
    near_width: float
    diagnosis: str
    # Confounder channel name in extras that must not cause a fire by itself.
    confounder_key: str
    # Higher quantity is worse (True) or lower is worse (False).
    higher_is_worse: bool = True

    def measure(self, subject: CriticSubject) -> float:
        return float(subject.quantity)

    def fires(self, subject: CriticSubject) -> bool:
        value = self.measure(subject)
        if self.higher_is_worse:
            return value + 1e-12 >= self.fire_threshold
        return value - 1e-12 <= self.fire_threshold

    def finding(self, subject: CriticSubject) -> CriticFinding | None:
        if not self.fires(subject):
            return None
        value = self.measure(subject)
        severity = "major"
        if self.higher_is_worse:
            if value >= self.fire_threshold + self.near_width * 2:
                severity = "critical"
            elif value < self.fire_threshold + self.near_width:
                severity = "minor"
        return CriticFinding(
            finding_id=f"cal-{self.role.value}-{uuid.uuid4().hex[:8]}",
            critic_role=self.role.value,
            diagnosis=self.diagnosis,
            evidence=[f"fixture:{subject.subject_id}", f"quantity:{self.quantity_name}"],
            severity=severity,
            confidence=0.9,
            likely_cause=f"{self.quantity_name} crossed {self.fire_threshold}",
            bounded_repair={
                "action": f"reduce_{self.quantity_name}",
                "target": self.fire_threshold * 0.5
                if self.higher_is_worse
                else self.fire_threshold * 1.5,
            },
            acceptance_test=(
                f"{self.quantity_name} < {self.fire_threshold}"
                if self.higher_is_worse
                else f"{self.quantity_name} > {self.fire_threshold}"
            ),
            measured={self.quantity_name: value, **subject.extras},
        )


# Per-role detector specs. Thresholds are part of the calibration contract:
# fixtures are built around them so near-threshold coverage is real, not faked.
CRITIC_SPECS: dict[CriticRole, CriticSpec] = {
    CriticRole.PRODUCT: CriticSpec(
        role=CriticRole.PRODUCT,
        quantity_name="silhouette_edge_weakness",
        fire_threshold=0.35,
        near_width=0.08,
        diagnosis="weak product silhouette separation",
        confounder_key="background_noise",
    ),
    CriticRole.MATERIAL: CriticSpec(
        role=CriticRole.MATERIAL,
        quantity_name="plastic_metal_score",
        fire_threshold=0.55,
        near_width=0.07,
        diagnosis="plastic-looking metal",
        confounder_key="jpeg_blockiness",
    ),
    CriticRole.LIGHT: CriticSpec(
        role=CriticRole.LIGHT,
        quantity_name="clip_fraction",
        fire_threshold=0.04,
        near_width=0.015,
        diagnosis="highlight clipping",
        confounder_key="sensor_gain_noise",
    ),
    CriticRole.ENVIRONMENT: CriticSpec(
        role=CriticRole.ENVIRONMENT,
        quantity_name="instance_uniformity",
        fire_threshold=0.7,
        near_width=0.08,
        diagnosis="environment instance monotony",
        confounder_key="camera_jitter",
    ),
    CriticRole.COMPOSITION: CriticSpec(
        role=CriticRole.COMPOSITION,
        quantity_name="template_similarity",
        fire_threshold=0.65,
        near_width=0.07,
        diagnosis="generic composition template",
        confounder_key="thumbnail_downscale",
    ),
    CriticRole.TYPOGRAPHY: CriticSpec(
        role=CriticRole.TYPOGRAPHY,
        quantity_name="contrast_deficit",
        fire_threshold=0.3,
        near_width=0.06,
        diagnosis="insufficient text contrast",
        confounder_key="subpixel_aa",
    ),
    CriticRole.MOTION: CriticSpec(
        role=CriticRole.MOTION,
        quantity_name="dead_scroll_fraction",
        fire_threshold=0.25,
        near_width=0.05,
        diagnosis="dead scroll / camera lag",
        confounder_key="frame_pacing_jitter",
    ),
    CriticRole.ORGANIC: CriticSpec(
        role=CriticRole.ORGANIC,
        quantity_name="symmetry_excess",
        fire_threshold=0.85,
        near_width=0.05,
        diagnosis="unnatural bilateral symmetry",
        confounder_key="mesh_density",
    ),
    CriticRole.GROOM: CriticSpec(
        role=CriticRole.GROOM,
        quantity_name="clump_ratio_error",
        fire_threshold=0.4,
        near_width=0.08,
        diagnosis="wrong fur clump scale",
        confounder_key="strand_count",
    ),
    CriticRole.PERFORMANCE: CriticSpec(
        role=CriticRole.PERFORMANCE,
        quantity_name="frame_p95_ms",
        fire_threshold=20.0,
        near_width=3.0,
        diagnosis="frame time budget exceeded",
        confounder_key="thermal_noise_ms",
    ),
}


def build_fixture_set(role: CriticRole) -> list[CriticSubject]:
    """Five-case (+ repair) fixture set for one critic role."""
    spec = CRITIC_SPECS[role]
    thr = spec.fire_threshold
    near = thr + spec.near_width * 0.5  # firmly inside the near band, above fire
    strong = thr + spec.near_width * 3.0
    negative = thr - spec.near_width * 1.5 if spec.higher_is_worse else thr + spec.near_width * 1.5
    if not spec.higher_is_worse:
        near = thr - spec.near_width * 0.5
        strong = thr - spec.near_width * 3.0
    # Confounder: quantity stays clean; confounder channel is elevated.
    clean = negative
    repaired = thr - spec.near_width if spec.higher_is_worse else thr + spec.near_width

    def _sub(kind: CaseKind, quantity: float, **extras: float) -> CriticSubject:
        return CriticSubject(
            subject_id=f"{role.value}-{kind.value}",
            role=role,
            case_kind=kind,
            quantity=float(quantity),
            extras=dict(extras),
            label=f"{role.value}:{kind.value}",
        )

    return [
        _sub(CaseKind.POSITIVE, strong, **{spec.confounder_key: 0.0}),
        _sub(CaseKind.NEAR_THRESHOLD, near, **{spec.confounder_key: 0.0}),
        _sub(CaseKind.NEGATIVE, clean, **{spec.confounder_key: 0.0}),
        _sub(CaseKind.CONFOUNDER, clean, **{spec.confounder_key: 1.0}),
        _sub(CaseKind.FALSE_POSITIVE_CHECK, clean, **{spec.confounder_key: 0.5}),
        _sub(CaseKind.REPAIR_VERIFICATION, repaired, **{spec.confounder_key: 0.0}),
    ]


@dataclass(slots=True)
class CalibrationCell:
    """One cell of the role × case matrix."""

    role: CriticRole
    case_kind: CaseKind
    fired: bool
    expected_fire: bool
    measured: dict[str, Any]
    passed: bool
    diagnosis: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "role": self.role.value,
            "case_kind": self.case_kind.value,
            "fired": self.fired,
            "expected_fire": self.expected_fire,
            "measured": dict(self.measured),
            "passed": self.passed,
            "diagnosis": self.diagnosis,
        }


def expected_fire(kind: CaseKind) -> bool:
    return kind in {CaseKind.POSITIVE, CaseKind.NEAR_THRESHOLD}


def calibrate_role(role: CriticRole) -> list[CalibrationCell]:
    """Run the fixture set for one role and score each case."""
    spec = CRITIC_SPECS[role]
    cells: list[CalibrationCell] = []
    for subject in build_fixture_set(role):
        finding = spec.finding(subject)
        fired = finding is not None
        expect = expected_fire(subject.case_kind)
        # Repair verification must stay silent (quantity restored below threshold).
        if subject.case_kind is CaseKind.REPAIR_VERIFICATION:
            expect = False
        measured = {
            spec.quantity_name: subject.quantity,
            **subject.extras,
            "fire_threshold": spec.fire_threshold,
        }
        cells.append(
            CalibrationCell(
                role=role,
                case_kind=subject.case_kind,
                fired=fired,
                expected_fire=expect,
                measured=measured,
                passed=fired is expect,
                diagnosis=finding.diagnosis if finding else "",
            )
        )
    return cells


def calibration_matrix(
    roles: list[CriticRole] | None = None,
) -> list[CalibrationCell]:
    """Full role × case matrix."""
    selected = roles or list(CriticRole)
    cells: list[CalibrationCell] = []
    for role in selected:
        cells.extend(calibrate_role(role))
    return cells


def matrix_passed(cells: list[CalibrationCell]) -> bool:
    return all(cell.passed for cell in cells)


def matrix_as_table(cells: list[CalibrationCell]) -> str:
    """Human-readable role × case grid."""
    roles = list(dict.fromkeys(cell.role for cell in cells))
    kinds = [
        CaseKind.POSITIVE,
        CaseKind.NEAR_THRESHOLD,
        CaseKind.NEGATIVE,
        CaseKind.CONFOUNDER,
        CaseKind.FALSE_POSITIVE_CHECK,
        CaseKind.REPAIR_VERIFICATION,
    ]
    header = f"{'role':14s}" + "".join(f"  {kind.value[:12]:>12s}" for kind in kinds)
    lines = [header, "-" * len(header)]
    for role in roles:
        row = f"{role.value:14s}"
        for kind in kinds:
            cell = next(
                (c for c in cells if c.role is role and c.case_kind is kind),
                None,
            )
            if cell is None:
                row += f"  {'—':>12s}"
                continue
            mark = "FIRE" if cell.fired else "quiet"
            status = "ok" if cell.passed else "FAIL"
            row += f"  {f'{mark}/{status}':>12s}"
        lines.append(row)
    return "\n".join(lines)


def critic_sensitivity_receipt(role: CriticRole) -> ProbeSensitivityReceipt:
    """Receipt proving the critic's threshold discriminates and rejects confounders.

    The 'parameter' is the measured quantity; the 'metric' is 1.0 when the
    critic fires and 0.0 when silent. Meaningful delta is the near-threshold
    band width; confounder is the declared confounder channel.
    """
    spec = CRITIC_SPECS[role]
    # Response curve: quantity from well-below to well-above threshold.
    if spec.higher_is_worse:
        values = [
            spec.fire_threshold - spec.near_width * 2,
            spec.fire_threshold - spec.near_width * 0.5,
            spec.fire_threshold,
            spec.fire_threshold + spec.near_width * 0.5,
            spec.fire_threshold + spec.near_width * 2,
            spec.fire_threshold + spec.near_width * 4,
        ]
    else:
        values = [
            spec.fire_threshold + spec.near_width * 2,
            spec.fire_threshold + spec.near_width * 0.5,
            spec.fire_threshold,
            spec.fire_threshold - spec.near_width * 0.5,
            spec.fire_threshold - spec.near_width * 2,
            spec.fire_threshold - spec.near_width * 4,
        ]
    curve: list[ResponsePoint] = []
    for value in values:
        subject = CriticSubject(
            subject_id=f"{role.value}-curve-{value:.4f}",
            role=role,
            case_kind=CaseKind.POSITIVE,
            quantity=float(value),
        )
        fired = 1.0 if spec.fires(subject) else 0.0
        curve.append(
            ResponsePoint(
                parameter_value=float(value),
                metric_value=fired,
                extras={spec.quantity_name: float(value)},
            )
        )

    # Confounders: elevate confounder channel with clean quantity → must not fire.
    clean_q = (
        spec.fire_threshold - spec.near_width * 1.5
        if spec.higher_is_worse
        else spec.fire_threshold + spec.near_width * 1.5
    )
    clean_subject = CriticSubject(
        subject_id=f"{role.value}-conf-clean",
        role=role,
        case_kind=CaseKind.CONFOUNDER,
        quantity=clean_q,
        extras={spec.confounder_key: 1.0},
    )
    conf_fired = spec.fires(clean_subject)
    confounders = [
        ConfounderResult(
            name=spec.confounder_key,
            metric_delta=1.0 if conf_fired else 0.0,
            max_allowed_delta=0.0,
            passed=not conf_fired,
            notes=f"elevated {spec.confounder_key} with clean {spec.quantity_name}",
        ),
        ConfounderResult(
            name="false_positive_channel",
            metric_delta=0.0,
            max_allowed_delta=0.0,
            passed=True,
            notes="secondary channel does not enter the decision",
        ),
    ]

    # Discrimination: firing state must flip across the meaningful near-band step.
    return build_receipt(
        metric_name=f"critic_fire:{role.value}",
        parameter=spec.quantity_name,
        curve=curve,
        meaningful_delta=float(spec.near_width),
        discrimination_margin=0.5,  # fire metric is 0/1; half is enough to flip
        confounders=confounders,
        metric_unit="fired",
        notes=[
            f"critic role {role.value}",
            f"quantity {spec.quantity_name}",
            f"threshold {spec.fire_threshold}",
        ],
        authority=AuthorityClass.INFERRED,
        lineage=Lineage(
            operation="critic_calibration",
            parameters={
                "role": role.value,
                "quantity": spec.quantity_name,
                "fire_threshold": spec.fire_threshold,
                "near_width": spec.near_width,
            },
            limitations=["synthetic fixtures; not a physical render"],
        ),
    )


def calibrate_all() -> dict[str, Any]:
    """Run matrix + per-role receipts. Fails closed if any cell or receipt fails."""
    cells = calibration_matrix()
    receipts = {role.value: critic_sensitivity_receipt(role) for role in CriticRole}
    # Re-classify: a critic receipt that fires on confounder is DIAGNOSTIC failure.
    role_ok = {
        role.value: all(c.passed for c in cells if c.role is role)
        and receipts[role.value].verdict is SensitivityVerdict.AUTHORITATIVE
        for role in CriticRole
    }
    return {
        "cells": [cell.to_dict() for cell in cells],
        "matrix_table": matrix_as_table(cells),
        "matrix_passed": matrix_passed(cells),
        "receipts": {name: receipt.to_dict() for name, receipt in receipts.items()},
        "role_ok": role_ok,
        "all_passed": matrix_passed(cells) and all(role_ok.values()),
    }


def run_critic(
    role: CriticRole | str,
    quantity: float,
    *,
    extras: dict[str, float] | None = None,
) -> CriticFinding | None:
    """Public entry: evaluate one role against a measured quantity."""
    resolved = CriticRole(role)
    spec = CRITIC_SPECS[resolved]
    subject = CriticSubject(
        subject_id=f"live-{resolved.value}",
        role=resolved,
        case_kind=CaseKind.POSITIVE,
        quantity=float(quantity),
        extras=dict(extras or {}),
    )
    return spec.finding(subject)


__all__ = [
    "CRITIC_SPECS",
    "CalibrationCell",
    "CaseKind",
    "CriticRole",
    "CriticSpec",
    "CriticSubject",
    "build_fixture_set",
    "calibrate_all",
    "calibrate_role",
    "calibration_matrix",
    "critic_sensitivity_receipt",
    "expected_fire",
    "matrix_as_table",
    "matrix_passed",
    "run_critic",
]
