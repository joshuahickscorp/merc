"""Material critics: measured failures with bound evidence and bounded repairs."""

from __future__ import annotations

import uuid
from dataclasses import dataclass
from typing import Any

import numpy as np

from blender_vision.materials.frequency import (
    FrequencyBand,
    FrequencyDecomposition,
    lacks_medium_band,
)
from blender_vision.v2.authority import AuthorityClass, derive
from blender_vision.v2.records import CriticFinding, Lineage, MaterialHypothesis, PerceptualCritique


@dataclass(slots=True)
class MaterialCriticContext:
    """Inputs a material critic may measure against."""

    hypothesis: MaterialHypothesis
    rgb: np.ndarray | None = None
    reference_rgb: np.ndarray | None = None
    frequency: FrequencyDecomposition | None = None
    pore_scale_m: float | None = None
    expected_pore_scale_m: float | None = None
    fur_clump_scale_m: float | None = None
    expected_fur_clump_scale_m: float | None = None
    evidence_ids: list[str] | None = None
    injected_failure: str | None = None


def _finding(
    *,
    role: str,
    diagnosis: str,
    evidence: list[str],
    measured: dict[str, Any],
    severity: str = "major",
    cause: str,
    repair: dict[str, Any],
    acceptance: str,
    confidence: float = 0.9,
) -> CriticFinding:
    return CriticFinding(
        finding_id=f"mc-{uuid.uuid4().hex[:10]}",
        critic_role=role,
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


def detect_plastic_looking_metal(ctx: MaterialCriticContext) -> CriticFinding | None:
    h = ctx.hypothesis
    evidence = list(ctx.evidence_ids or [f"hypothesis:{h.hypothesis_id}"])
    # Metals with low metalness or dielectric-like neutral specular response look plastic.
    plastic_score = 0.0
    if h.metalness >= 0.6 and h.roughness >= 0.35 and h.specular_ior >= 1.4:
        plastic_score = max(plastic_score, 0.7)
    if ctx.injected_failure == "plastic_looking_metal":
        plastic_score = 1.0
    if h.metalness >= 0.75 and h.roughness > 0.55:
        plastic_score = max(plastic_score, 0.85)
    if plastic_score < 0.65:
        return None
    return _finding(
        role="material-artist",
        diagnosis="plastic-looking metal",
        evidence=evidence,
        measured={
            "metalness": h.metalness,
            "roughness": h.roughness,
            "specular_ior": h.specular_ior,
            "plastic_score": plastic_score,
        },
        cause="Metalness/roughness pairing produces dielectric lobe shape on a metal base.",
        repair={
            "action": "raise_metalness_lower_roughness_or_tint_specular",
            "metalness_delta": max(0.0, 0.9 - h.metalness),
            "roughness_delta": min(0.0, 0.35 - h.roughness),
        },
        acceptance="metalness>=0.85 and roughness<=0.4 for bead/anodized metal classes",
    )


def detect_white_clipping(ctx: MaterialCriticContext) -> CriticFinding | None:
    if ctx.rgb is None and ctx.injected_failure != "white_clipping":
        return None
    evidence = list(ctx.evidence_ids or ["render:beauty"])
    if ctx.injected_failure == "white_clipping":
        fraction = 0.12
    else:
        assert ctx.rgb is not None
        lum = _luminance(ctx.rgb)
        fraction = float(np.mean(lum >= 0.99))
    if fraction < 0.02:
        return None
    return _finding(
        role="material-artist",
        diagnosis="white clipping",
        evidence=evidence,
        measured={"clip_fraction_ge_0_99": fraction},
        severity="critical" if fraction >= 0.05 else "major",
        cause="Specular or base response exceeds displayable range on hero surface.",
        repair={"action": "reduce_specular_or_exposure", "clip_fraction": fraction},
        acceptance="clip_fraction_ge_0_99 < 0.02 on hero surfaces",
    )


def detect_environment_smear(ctx: MaterialCriticContext) -> CriticFinding | None:
    evidence = list(ctx.evidence_ids or ["render:beauty"])
    if ctx.injected_failure == "environment_smear":
        smear = 0.4
    elif ctx.rgb is None:
        return None
    else:
        rgb = np.asarray(ctx.rgb, dtype=np.float64)
        if rgb.max() > 1.0 + 1e-6:
            rgb = rgb / 255.0
        # Horizontal gradient coherence: env smear shows stretched streaks.
        dx = np.abs(np.diff(rgb, axis=1)).mean()
        dy = np.abs(np.diff(rgb, axis=0)).mean()
        smear = float(dx / max(dy, 1e-6))
        if smear < 2.5:
            return None
        smear = min(smear / 8.0, 1.0)
    if smear < 0.25:
        return None
    return _finding(
        role="material-artist",
        diagnosis="environment smear",
        evidence=evidence,
        measured={"smear_score": smear},
        cause="Environment reflections are stretched or incorrectly filtered on the surface.",
        repair={"action": "increase_roughness_or_fix_env_mip", "smear_score": smear},
        acceptance="smear_score < 0.25",
    )


def detect_wrong_pore_scale(ctx: MaterialCriticContext) -> CriticFinding | None:
    evidence = list(ctx.evidence_ids or [f"hypothesis:{ctx.hypothesis.hypothesis_id}"])
    if ctx.injected_failure == "wrong_pore_scale":
        ratio = 4.0
        observed = ctx.pore_scale_m or 0.004
        expected = ctx.expected_pore_scale_m or 0.001
    else:
        if ctx.pore_scale_m is None or ctx.expected_pore_scale_m is None:
            return None
        observed = ctx.pore_scale_m
        expected = max(ctx.expected_pore_scale_m, 1e-9)
        ratio = observed / expected
    if 0.5 <= ratio <= 2.0:
        return None
    return _finding(
        role="material-artist",
        diagnosis="wrong pore scale",
        evidence=evidence,
        measured={
            "pore_scale_m": observed,
            "expected_pore_scale_m": expected,
            "scale_ratio": ratio,
        },
        cause="Micro-feature world scale disagrees with material class reference scale.",
        repair={
            "action": "rescale_texture_world_scale",
            "target_scale_m": expected,
            "current_scale_m": observed,
        },
        acceptance="0.5 <= pore_scale / expected <= 2.0",
    )


def detect_flat_texture_as_depth(ctx: MaterialCriticContext) -> CriticFinding | None:
    evidence = list(ctx.evidence_ids or ["frequency:decomposition"])
    if ctx.injected_failure == "flat_texture_as_depth":
        medium_rel = 0.01
        colour_rel = 0.55
        flat = True
    elif ctx.frequency is None:
        return None
    else:
        rel = ctx.frequency.relative_map()
        medium_rel = rel[FrequencyBand.MEDIUM_DISPLACEMENT.value]
        colour_rel = rel[FrequencyBand.COLOUR_VARIATION.value]
        flat = lacks_medium_band(ctx.frequency)
    if not flat:
        return None
    return _finding(
        role="material-artist",
        diagnosis="flat texture pretending to be depth",
        evidence=evidence,
        measured={
            "medium_relative_energy": medium_rel,
            "colour_relative_energy": colour_rel,
        },
        cause="Colour variation without medium-band displacement energy.",
        repair={"action": "add_medium_displacement_or_drop_fake_depth_claim"},
        acceptance="medium_relative_energy >= 0.04 when claiming depth",
    )


def detect_sparkling_noise(ctx: MaterialCriticContext) -> CriticFinding | None:
    evidence = list(ctx.evidence_ids or ["render:beauty"])
    if ctx.injected_failure == "sparkling_noise":
        sparkle = 0.08
    elif ctx.rgb is None:
        return None
    else:
        lum = _luminance(ctx.rgb)
        # Isolated bright pixels relative to local mean.
        from scipy.ndimage import uniform_filter

        local = uniform_filter(lum, size=5)
        residual = lum - local
        sparkle = float(np.mean(residual > 0.25))
    if sparkle < 0.01:
        return None
    return _finding(
        role="material-artist",
        diagnosis="sparkling noise",
        evidence=evidence,
        measured={"sparkle_fraction": sparkle},
        cause="Under-filtered specular microfacet fireflies or noisy normal map.",
        repair={"action": "increase_roughness_filter_or_sample_count", "sparkle_fraction": sparkle},
        acceptance="sparkle_fraction < 0.01",
    )


def detect_mirror_like_bead_blast(ctx: MaterialCriticContext) -> CriticFinding | None:
    h = ctx.hypothesis
    evidence = list(ctx.evidence_ids or [f"hypothesis:{h.hypothesis_id}"])
    if ctx.injected_failure == "mirror_like_bead_blast":
        score = 1.0
    else:
        # Bead-blast aluminium should be rough metal, not mirror.
        label = (h.label or "").lower()
        is_bead = "bead" in label or "blast" in label
        score = 1.0 if is_bead and h.metalness >= 0.5 and h.roughness < 0.25 else 0.0
        if h.metalness >= 0.7 and h.roughness < 0.12 and "mirror" not in label:
            score = max(score, 0.7)
    if score < 0.6:
        return None
    return _finding(
        role="material-artist",
        diagnosis="mirror-like bead blast",
        evidence=evidence,
        measured={"metalness": h.metalness, "roughness": h.roughness, "score": score},
        cause="Bead-blast / matte metal assigned mirror roughness.",
        repair={"action": "raise_roughness", "roughness_target": 0.45},
        acceptance="bead-blast roughness in [0.35, 0.65]",
    )


def detect_incorrect_subsurface(ctx: MaterialCriticContext) -> CriticFinding | None:
    h = ctx.hypothesis
    evidence = list(ctx.evidence_ids or [f"hypothesis:{h.hypothesis_id}"])
    if ctx.injected_failure == "incorrect_subsurface":
        score = 1.0
    else:
        label = (h.label or "").lower()
        organic = any(token in label for token in ("skin", "wax", "plastic", "rubber", "fabric"))
        metal = h.metalness >= 0.5
        score = 0.0
        if metal and h.subsurface > 0.05:
            score = 1.0
        if organic and h.subsurface < 0.02 and "glass" not in label:
            score = max(score, 0.7)
        if h.subsurface > 0.8 and h.transmission < 0.05 and h.metalness < 0.2:
            score = max(score, 0.75)
    if score < 0.6:
        return None
    return _finding(
        role="material-artist",
        diagnosis="incorrect subsurface",
        evidence=evidence,
        measured={"subsurface": h.subsurface, "metalness": h.metalness, "score": score},
        cause="Subsurface weight inconsistent with material class.",
        repair={"action": "retarget_subsurface", "subsurface": h.subsurface},
        acceptance="metals subsurface≈0; organics within class range",
    )


def detect_incorrect_fur_clump_scale(ctx: MaterialCriticContext) -> CriticFinding | None:
    evidence = list(ctx.evidence_ids or [f"hypothesis:{ctx.hypothesis.hypothesis_id}"])
    if ctx.injected_failure == "incorrect_fur_clump_scale":
        ratio = 5.0
        observed = ctx.fur_clump_scale_m or 0.01
        expected = ctx.expected_fur_clump_scale_m or 0.002
    else:
        if ctx.fur_clump_scale_m is None or ctx.expected_fur_clump_scale_m is None:
            return None
        observed = ctx.fur_clump_scale_m
        expected = max(ctx.expected_fur_clump_scale_m, 1e-9)
        ratio = observed / expected
    if 0.5 <= ratio <= 2.0:
        return None
    return _finding(
        role="material-artist",
        diagnosis="incorrect fur clump scale",
        evidence=evidence,
        measured={
            "fur_clump_scale_m": observed,
            "expected_fur_clump_scale_m": expected,
            "scale_ratio": ratio,
        },
        cause="Fur clump scale disagrees with species/material reference.",
        repair={"action": "rescale_fur_clumps", "target_m": expected},
        acceptance="0.5 <= fur_clump_scale / expected <= 2.0",
    )


MATERIAL_CRITICS: dict[str, Any] = {
    "plastic_looking_metal": detect_plastic_looking_metal,
    "white_clipping": detect_white_clipping,
    "environment_smear": detect_environment_smear,
    "wrong_pore_scale": detect_wrong_pore_scale,
    "flat_texture_as_depth": detect_flat_texture_as_depth,
    "sparkling_noise": detect_sparkling_noise,
    "mirror_like_bead_blast": detect_mirror_like_bead_blast,
    "incorrect_subsurface": detect_incorrect_subsurface,
    "incorrect_fur_clump_scale": detect_incorrect_fur_clump_scale,
}


def run_material_critics(ctx: MaterialCriticContext) -> PerceptualCritique:
    findings: list[CriticFinding] = []
    for name, detector in MATERIAL_CRITICS.items():
        if ctx.injected_failure is not None and ctx.injected_failure != name:
            continue
        finding = detector(ctx)
        if finding is not None:
            findings.append(finding)
    authority = derive(
        [ctx.hypothesis.authority],
        proposed=AuthorityClass.INFERRED,
    )
    critique = PerceptualCritique(
        id=f"pc-mat-{uuid.uuid4().hex[:10]}",
        subject_id=ctx.hypothesis.hypothesis_id,
        subject_kind="material",
        findings=findings,
        critics_run=sorted(MATERIAL_CRITICS),
        passed=len(findings) == 0,
        authority=authority,
        lineage=Lineage(
            operation="run_material_critics",
            inputs=[ctx.hypothesis.hypothesis_id],
            input_authorities=[ctx.hypothesis.authority.value],
        ),
    )
    return critique.seal()


def inject_material_failure(name: str, base: MaterialHypothesis) -> MaterialCriticContext:
    """Build a context that should trigger the named material critic."""
    if name not in MATERIAL_CRITICS:
        raise ValueError(f"unknown material failure: {name}")
    h = MaterialHypothesis(
        hypothesis_id=f"inject-{name}",
        label=base.label,
        base_colour=list(base.base_colour),
        roughness=base.roughness,
        metalness=base.metalness,
        specular_ior=base.specular_ior,
        anisotropy=base.anisotropy,
        transmission=base.transmission,
        subsurface=base.subsurface,
        confidence=base.confidence,
        evidence_views=list(base.evidence_views),
        authority=base.authority,
        texture_scale_m=base.texture_scale_m,
    )
    rgb = None
    frequency = None
    pore = expected_pore = fur = expected_fur = None
    if name == "plastic_looking_metal":
        h.label = "bead-blasted-aluminium"
        h.metalness = 0.8
        h.roughness = 0.62
        h.specular_ior = 1.5
    elif name == "white_clipping":
        rgb = np.ones((64, 64, 3), dtype=np.float64)
    elif name == "environment_smear":
        rgb = np.zeros((64, 128, 3), dtype=np.float64)
        for x in range(128):
            rgb[:, x, :] = (x / 127.0) ** 0.5
    elif name == "wrong_pore_scale":
        pore, expected_pore = 0.008, 0.001
    elif name == "flat_texture_as_depth":
        from blender_vision.materials.frequency import BandEnergy, FrequencyDecomposition

        frequency = FrequencyDecomposition(
            bands=[
                BandEnergy(FrequencyBand.MACRO_GEOMETRY, 0.01, 0.05, 16.0),
                BandEnergy(FrequencyBand.MEDIUM_DISPLACEMENT, 0.002, 0.01, 8.0),
                BandEnergy(FrequencyBand.FINE_NORMAL, 0.02, 0.1, 2.0),
                BandEnergy(FrequencyBand.MICRO_ROUGHNESS, 0.03, 0.15, 1.0),
                BandEnergy(FrequencyBand.COLOUR_VARIATION, 0.12, 0.55, 16.0),
                BandEnergy(FrequencyBand.BACKING_OCCLUSION, 0.01, 0.14, 4.0),
            ],
            residual_energy=0.0,
            pyramid_levels=5,
        )
    elif name == "sparkling_noise":
        rng = np.random.default_rng(0)
        rgb = np.full((64, 64, 3), 0.2, dtype=np.float64)
        spikes = rng.random((64, 64)) > 0.97
        rgb[spikes] = 1.0
    elif name == "mirror_like_bead_blast":
        h.label = "bead-blasted-aluminium"
        h.metalness = 0.95
        h.roughness = 0.08
    elif name == "incorrect_subsurface":
        h.label = "bead-blasted-aluminium"
        h.metalness = 0.9
        h.subsurface = 0.4
    elif name == "incorrect_fur_clump_scale":
        h.label = "hair-fur"
        fur, expected_fur = 0.02, 0.002
    return MaterialCriticContext(
        hypothesis=h,
        rgb=rgb,
        frequency=frequency,
        pore_scale_m=pore,
        expected_pore_scale_m=expected_pore,
        fur_clump_scale_m=fur,
        expected_fur_clump_scale_m=expected_fur,
        evidence_ids=[f"inject:{name}"],
        injected_failure=name,
    )
