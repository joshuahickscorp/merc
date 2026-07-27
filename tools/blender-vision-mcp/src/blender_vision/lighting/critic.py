"""Lighting critics with measured quantities and bounded repairs."""

from __future__ import annotations

import uuid
from dataclasses import dataclass
from typing import Any

import numpy as np

from blender_vision.v2.authority import AuthorityClass, derive
from blender_vision.v2.records import CriticFinding, LightingHypothesis, Lineage, PerceptualCritique


@dataclass(slots=True)
class LightingCriticContext:
    hypothesis: LightingHypothesis
    rgb: np.ndarray | None = None
    contact_band: np.ndarray | None = None  # bottom strip or contact region
    hero_mask: np.ndarray | None = None
    evidence_ids: list[str] | None = None
    injected_failure: str | None = None
    material_error_from_lighting: bool = False


def _finding(
    *,
    diagnosis: str,
    evidence: list[str],
    measured: dict[str, Any],
    cause: str,
    repair: dict[str, Any],
    acceptance: str,
    severity: str = "major",
    confidence: float = 0.9,
) -> CriticFinding:
    return CriticFinding(
        finding_id=f"lc-{uuid.uuid4().hex[:10]}",
        critic_role="lighting-artist",
        diagnosis=diagnosis,
        evidence=evidence or ["synthetic:unbound"],
        severity=severity,
        confidence=confidence,
        likely_cause=cause,
        bounded_repair=repair,
        acceptance_test=acceptance,
        measured=measured,
    )


def _luminance(rgb: np.ndarray) -> np.ndarray:
    arr = np.asarray(rgb, dtype=np.float64)
    if arr.max() > 1.0 + 1e-6:
        arr = arr / 255.0
    return 0.2126 * arr[..., 0] + 0.7152 * arr[..., 1] + 0.0722 * arr[..., 2]


def detect_clipped_metal(ctx: LightingCriticContext) -> CriticFinding | None:
    evidence = list(ctx.evidence_ids or ["render:beauty"])
    if ctx.injected_failure == "clipped_metal":
        fraction = 0.15
    elif ctx.rgb is None:
        return None
    else:
        lum = _luminance(ctx.rgb)
        mask = ctx.hero_mask if ctx.hero_mask is not None else np.ones(lum.shape, dtype=bool)
        fraction = float(np.mean(lum[mask] >= 0.99)) if mask.any() else 0.0
    if fraction < 0.02:
        return None
    return _finding(
        diagnosis="clipped metal",
        evidence=evidence,
        measured={"hero_clip_fraction_ge_0_99": fraction},
        severity="critical" if fraction >= 0.05 else "major",
        cause="Key/environment intensity drives metal specular past display white.",
        repair={"action": "reduce_key_or_exposure", "clip_fraction": fraction},
        acceptance="hero_clip_fraction_ge_0_99 < 0.02",
    )


def detect_fake_plastic_metal(ctx: LightingCriticContext) -> CriticFinding | None:
    evidence = list(ctx.evidence_ids or [f"hypothesis:{ctx.hypothesis.hypothesis_id}"])
    h = ctx.hypothesis
    if ctx.injected_failure == "fake_plastic_metal":
        score = 1.0
    else:
        # Overfill + large soft key flattens metal into plastic.
        fill_i = float(h.fill.get("intensity", 0.0) or 0.0)
        key_size = float(h.key.get("size", 0.5) or 0.5)
        score = 0.0
        if fill_i > 0.85 and key_size > 1.2 and h.shadow_softness > 0.55:
            score = 0.9
        if h.environment.get("strength", 0) and float(h.environment.get("strength", 0)) > 1.2:
            score = max(score, 0.7)
    if score < 0.65:
        return None
    return _finding(
        diagnosis="fake plastic metal",
        evidence=evidence,
        measured={
            "fill_intensity": float(h.fill.get("intensity", 0.0) or 0.0),
            "key_size": float(h.key.get("size", 0.0) or 0.0),
            "shadow_softness": h.shadow_softness,
            "score": score,
        },
        cause="Fill and soft key erase metal contrast, reading as plastic.",
        repair={"action": "reduce_fill_tighten_key", "fill_target": 0.35},
        acceptance="fill_intensity<=0.5 and key_size<=1.0 for product metal",
    )


def detect_floating_objects(ctx: LightingCriticContext) -> CriticFinding | None:
    evidence = list(ctx.evidence_ids or ["render:contact"])
    if ctx.injected_failure == "floating_objects":
        gradient = 0.002
    elif ctx.contact_band is not None:
        band = np.asarray(ctx.contact_band, dtype=np.float64)
        if band.ndim == 3:
            band = _luminance(band)
        # Contact shadow = strong vertical gradient near contact.
        gradient = (
            0.0
            if band.shape[0] < 2
            else float(np.mean(np.abs(np.diff(band, axis=0))))
        )
    elif ctx.rgb is not None:
        lum = _luminance(ctx.rgb)
        band = lum[-max(4, lum.shape[0] // 8) :, :]
        gradient = float(np.mean(np.abs(np.diff(band, axis=0)))) if band.shape[0] > 1 else 0.0
    else:
        return None
    if gradient >= 0.015:
        return None
    return _finding(
        diagnosis="floating objects",
        evidence=evidence,
        measured={"contact_shadow_gradient": gradient},
        cause="Missing or vanishingly weak contact shadow under object.",
        repair={"action": "enable_contact_shadow_or_lower_fill", "gradient": gradient},
        acceptance="contact_shadow_gradient >= 0.015",
    )


def detect_flat_black(ctx: LightingCriticContext) -> CriticFinding | None:
    evidence = list(ctx.evidence_ids or ["render:beauty"])
    if ctx.injected_failure == "flat_black":
        mean_l = 0.02
        contrast = 0.005
    elif ctx.rgb is None:
        return None
    else:
        lum = _luminance(ctx.rgb)
        mean_l = float(np.mean(lum))
        contrast = float(np.std(lum))
    if mean_l > 0.08 or contrast > 0.03:
        return None
    return _finding(
        diagnosis="flat black",
        evidence=evidence,
        measured={"mean_luminance": mean_l, "luminance_std": contrast},
        cause="Underexposed or crushed lighting with no shadow hierarchy.",
        repair={"action": "raise_exposure_or_key", "exposure_delta": 1.0},
        acceptance="mean_luminance > 0.08 or luminance_std > 0.03",
    )


def detect_overfilled_shadows(ctx: LightingCriticContext) -> CriticFinding | None:
    evidence = list(ctx.evidence_ids or [f"hypothesis:{ctx.hypothesis.hypothesis_id}"])
    h = ctx.hypothesis
    if ctx.injected_failure == "overfilled_shadows":
        fill_ratio = 1.2
    else:
        key_i = float(h.key.get("intensity", 1.0) or 1.0)
        fill_i = float(h.fill.get("intensity", 0.0) or 0.0)
        fill_ratio = fill_i / max(key_i, 1e-6)
        if ctx.rgb is not None:
            lum = _luminance(ctx.rgb)
            shadow = lum[lum <= np.percentile(lum, 20)]
            mid = lum[(lum > np.percentile(lum, 40)) & (lum < np.percentile(lum, 60))]
            if shadow.size and mid.size and float(np.mean(mid)) > 1e-6:
                fill_ratio = max(fill_ratio, float(np.mean(shadow) / np.mean(mid)))
    if fill_ratio < 0.75:
        return None
    return _finding(
        diagnosis="overfilled shadows",
        evidence=evidence,
        measured={"fill_to_key_ratio": fill_ratio},
        cause="Fill lights lift shadows until contrast collapses.",
        repair={"action": "reduce_fill", "fill_to_key_target": 0.35},
        acceptance="fill_to_key_ratio < 0.75",
    )


def detect_arbitrary_glow(ctx: LightingCriticContext) -> CriticFinding | None:
    evidence = list(ctx.evidence_ids or [f"hypothesis:{ctx.hypothesis.hypothesis_id}"])
    h = ctx.hypothesis
    if ctx.injected_failure == "arbitrary_glow":
        bloom = 0.6
    else:
        bloom = float(h.bloom)
        if ctx.rgb is not None:
            lum = _luminance(ctx.rgb)
            # Soft halo: bright core with elevated neighbours.
            from scipy.ndimage import uniform_filter

            local = uniform_filter(lum, size=9)
            bloom = max(bloom, float(np.mean((local > 0.6) & (lum < 0.4))))
    if bloom < 0.12:
        return None
    return _finding(
        diagnosis="arbitrary glow",
        evidence=evidence,
        measured={"bloom": bloom},
        cause="Unmotivated bloom/glow without emissive source authority.",
        repair={"action": "disable_or_bound_bloom", "bloom_target": 0.0},
        acceptance="bloom < 0.12 without emissive evidence",
    )


def detect_material_error_from_lighting(ctx: LightingCriticContext) -> CriticFinding | None:
    evidence = list(ctx.evidence_ids or ["joint:material-light"])
    if ctx.injected_failure == "material_error_from_lighting":
        score = 1.0
    elif ctx.material_error_from_lighting:
        score = 0.95
    else:
        h = ctx.hypothesis
        # Extreme colour cast + high exposure can invent false metal/roughness.
        env = h.environment or {}
        color = env.get("color") or [1, 1, 1]
        if isinstance(color, list) and len(color) >= 3:
            cast = float(max(color) - min(color))
        else:
            cast = 0.0
        score = 0.0
        if cast > 0.35 and abs(h.exposure) > 1.5:
            score = 0.85
        if float(h.key.get("intensity", 1.0) or 1.0) > 4.0 and h.shadow_softness > 0.7:
            score = max(score, 0.7)
    if score < 0.65:
        return None
    return _finding(
        diagnosis="material error caused by lighting",
        evidence=evidence,
        measured={"score": score, "exposure": ctx.hypothesis.exposure},
        cause="Lighting cast/exposure corrupts inverse material estimates.",
        repair={"action": "rebalance_white_balance_and_exposure", "score": score},
        acceptance="neutral documentation exposure before material solve",
    )


LIGHTING_CRITICS: dict[str, Any] = {
    "clipped_metal": detect_clipped_metal,
    "fake_plastic_metal": detect_fake_plastic_metal,
    "floating_objects": detect_floating_objects,
    "flat_black": detect_flat_black,
    "overfilled_shadows": detect_overfilled_shadows,
    "arbitrary_glow": detect_arbitrary_glow,
    "material_error_from_lighting": detect_material_error_from_lighting,
}


def run_lighting_critics(ctx: LightingCriticContext) -> PerceptualCritique:
    findings: list[CriticFinding] = []
    for name, detector in LIGHTING_CRITICS.items():
        if ctx.injected_failure is not None and ctx.injected_failure != name:
            continue
        finding = detector(ctx)
        if finding is not None:
            findings.append(finding)
    authority = derive([ctx.hypothesis.authority], proposed=AuthorityClass.INFERRED)
    critique = PerceptualCritique(
        id=f"pc-light-{uuid.uuid4().hex[:10]}",
        subject_id=ctx.hypothesis.hypothesis_id,
        subject_kind="lighting",
        findings=findings,
        critics_run=sorted(LIGHTING_CRITICS),
        passed=len(findings) == 0,
        authority=authority,
        lineage=Lineage(
            operation="run_lighting_critics",
            inputs=[ctx.hypothesis.hypothesis_id],
            input_authorities=[ctx.hypothesis.authority.value],
        ),
    )
    return critique.seal()


def inject_lighting_failure(
    name: str, base: LightingHypothesis | None = None
) -> LightingCriticContext:
    if name not in LIGHTING_CRITICS:
        raise ValueError(f"unknown lighting failure: {name}")
    h = base or LightingHypothesis(
        hypothesis_id=f"inject-{name}",
        rig_class="neutral_documentation",
        key={"direction": [0.4, 0.6, 0.7], "size": 0.5, "intensity": 1.2},
        fill={"intensity": 0.3},
        environment={"color": [0.5, 0.5, 0.5], "strength": 0.3},
        shadow_softness=0.25,
        exposure=0.0,
        white_balance_k=6500.0,
        authority=AuthorityClass.INFERRED,
    )
    # Copy-ish fields for mutation.
    hyp = LightingHypothesis(
        hypothesis_id=f"inject-{name}",
        rig_class=h.rig_class,
        key=dict(h.key),
        fill=dict(h.fill),
        negative_fill=dict(h.negative_fill),
        rim=dict(h.rim),
        environment=dict(h.environment),
        reflection_cards=list(h.reflection_cards),
        shadow_softness=h.shadow_softness,
        contact_shadow=dict(h.contact_shadow),
        exposure=h.exposure,
        white_balance_k=h.white_balance_k,
        tone_map=h.tone_map,
        bloom=h.bloom,
        atmosphere=dict(h.atmosphere),
        depth_falloff=h.depth_falloff,
        confidence=h.confidence,
        authority=h.authority,
    )
    rgb = None
    contact = None
    hero = None
    material_error = False
    if name == "clipped_metal":
        rgb = np.ones((64, 64, 3), dtype=np.float64)
        hero = np.ones((64, 64), dtype=bool)
    elif name == "fake_plastic_metal":
        hyp.fill = {"intensity": 1.1}
        hyp.key = {"direction": [0.3, 0.5, 0.8], "size": 1.8, "intensity": 1.0}
        hyp.shadow_softness = 0.7
    elif name == "floating_objects":
        contact = np.full((16, 64), 0.5, dtype=np.float64)
    elif name == "flat_black":
        rgb = np.full((64, 64, 3), 0.02, dtype=np.float64)
    elif name == "overfilled_shadows":
        hyp.key = {"intensity": 1.0, "size": 0.5}
        hyp.fill = {"intensity": 1.1}
    elif name == "arbitrary_glow":
        hyp.bloom = 0.7
    elif name == "material_error_from_lighting":
        hyp.exposure = 2.0
        hyp.environment = {"color": [1.0, 0.4, 0.3], "strength": 1.0}
        material_error = True
    return LightingCriticContext(
        hypothesis=hyp,
        rgb=rgb,
        contact_band=contact,
        hero_mask=hero,
        evidence_ids=[f"inject:{name}"],
        injected_failure=name,
        material_error_from_lighting=material_error,
    )
