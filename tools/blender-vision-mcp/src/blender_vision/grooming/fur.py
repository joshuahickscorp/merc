"""Fur grooming service and its critics.

The groom itself runs in Blender (`organic/build_scripts/groom_fur.py`). This
module owns the parameters, the delivery-form contract, and the checks that
decide whether a groom is plausible — clump scale against body scale, density
against surface area, and whether the web LOD still reads as the same animal.
"""

from __future__ import annotations

import json
from dataclasses import asdict, dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import Any

from blender_vision.blender.v2_executor import V2BlenderExecutor
from blender_vision.core.errors import ValidationError
from blender_vision.v2.authority import AuthorityClass
from blender_vision.v2.records import CriticFinding

BUILD_SCRIPTS = Path(__file__).resolve().parent.parent / "organic" / "build_scripts"


class FurDeliveryForm(StrEnum):
    OFFLINE_CURVES = "offline_curves"
    WEB_SHELLS = "web_shells"
    MOBILE_CARDS = "mobile_cards"


@dataclass(slots=True)
class GroomParameters:
    """Every knob the groom actually uses. No hidden defaults in Blender."""

    guide_count: int = 900
    segments: int = 8
    length_m: float = 0.022
    guide_radius_m: float = 0.00035
    strand_radius_m: float = 0.00018
    children_per_guide: int = 6
    undercoat_children_per_guide: int = 10
    clump: float = 0.62
    undercoat_clump: float = 0.35
    frizz: float = 0.0016
    gravity: float = 0.55
    comb_direction: tuple[float, float, float] = (0.0, 1.0, -0.25)
    comb_strength: float = 0.45
    root_scatter_m: float = 0.0025
    shell_layers: int = 6
    card_stride: int = 3

    def __post_init__(self) -> None:
        if not 0.0 <= self.clump <= 1.0:
            raise ValidationError("clump must be within 0..1")
        if not 0.0 <= self.undercoat_clump <= 1.0:
            raise ValidationError("undercoat_clump must be within 0..1")
        if self.guide_count < 1 or self.segments < 2:
            raise ValidationError("a groom needs at least one guide of two segments")
        if self.length_m <= 0 or self.strand_radius_m <= 0:
            raise ValidationError("length and strand radius must be positive")
        if self.strand_radius_m > self.length_m * 0.1:
            raise ValidationError(
                "strand radius exceeds a tenth of strand length; that is rope, not fur"
            )

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        value["comb_direction"] = list(self.comb_direction)
        return value


@dataclass(slots=True)
class GroomResult:
    parameters: GroomParameters
    report: dict[str, Any]
    offline_blend: Path
    web_glb: Path
    script_sha256: str
    blender_version: str

    @property
    def clump_to_body_ratio(self) -> float:
        return float(self.report["clump_to_body_ratio"])

    @property
    def density_per_m2(self) -> float:
        return float(self.report["density_per_m2"])

    @property
    def strand_count(self) -> int:
        return int(self.report["guard_strands"]) + int(self.report["undercoat_strands"])

    def to_dict(self) -> dict[str, Any]:
        return {
            "parameters": self.parameters.to_dict(),
            "report": self.report,
            "offline_blend": str(self.offline_blend),
            "web_glb": str(self.web_glb),
            "script_sha256": self.script_sha256,
            "blender_version": self.blender_version,
            "authority": AuthorityClass.PROCEDURAL_GROUND_TRUTH.value,
        }


class FurGroomer:
    def __init__(self, executor: V2BlenderExecutor | None = None) -> None:
        self.executor = executor or V2BlenderExecutor()

    def groom(
        self,
        source_blend: Path,
        object_name: str,
        output_dir: Path,
        *,
        parameters: GroomParameters | None = None,
        seed: int = 20260726,
    ) -> GroomResult:
        parameters = parameters or GroomParameters()
        output_dir.mkdir(parents=True, exist_ok=True)
        run = self.executor.run(
            BUILD_SCRIPTS / "groom_fur.py",
            {
                "source_blend": str(source_blend.resolve()),
                "object": object_name,
                "output_dir": str(output_dir.resolve()),
                "seed": seed,
                "groom": parameters.to_dict(),
            },
            expect_marker="V2_FUR_GROOM_OK",
        )
        report = json.loads((output_dir / "groom-report.json").read_text(encoding="utf-8"))
        return GroomResult(
            parameters=parameters,
            report=report,
            offline_blend=Path(report["offline_blend"]),
            web_glb=Path(report["web_glb"]),
            script_sha256=run.script_sha256,
            blender_version=run.blender_version,
        )


# --------------------------------------------------------------------------
# groom critic
# --------------------------------------------------------------------------

#: Plausible ranges, stated up front so a failing groom fails against a
#: published expectation rather than against a number chosen after the fact.
CLUMP_TO_BODY_RANGE = (0.002, 0.20)
DENSITY_PER_M2_RANGE = (2_000.0, 4_000_000.0)
MIN_UNDERCOAT_RATIO = 0.5


@dataclass(slots=True)
class GroomCritique:
    findings: list[CriticFinding] = field(default_factory=list)

    @property
    def passed(self) -> bool:
        return not any(item.severity in {"major", "critical"} for item in self.findings)


def critique_groom(result: GroomResult, *, evidence: list[str]) -> GroomCritique:
    """Measured groom checks. Every finding carries the number that triggered it."""
    if not evidence:
        raise ValidationError("groom critique requires evidence references")

    findings: list[CriticFinding] = []
    ratio = result.clump_to_body_ratio
    low, high = CLUMP_TO_BODY_RANGE
    if not low <= ratio <= high:
        findings.append(
            CriticFinding(
                finding_id="groom.clump_scale",
                critic_role="groom artist",
                diagnosis=(
                    "clump scale is implausible against body scale: "
                    f"{ratio:.5f} outside [{low}, {high}]"
                ),
                evidence=evidence,
                severity="major",
                confidence=0.85,
                likely_cause="clump or length_m set without reference to the body bounds",
                bounded_repair={
                    "parameters": ["clump", "length_m"],
                    "target_ratio": [low, high],
                },
                blast_radius=["fur guides", "guard hair", "undercoat"],
                acceptance_test="clump_to_body_ratio within CLUMP_TO_BODY_RANGE",
                measured={"clump_to_body_ratio": ratio, "range": [low, high]},
            )
        )

    density = result.density_per_m2
    dlow, dhigh = DENSITY_PER_M2_RANGE
    if not dlow <= density <= dhigh:
        findings.append(
            CriticFinding(
                finding_id="groom.density",
                critic_role="groom artist",
                diagnosis=f"strand density {density:.0f}/m2 outside [{dlow:.0f}, {dhigh:.0f}]",
                evidence=evidence,
                severity="major" if density < dlow else "minor",
                confidence=0.8,
                likely_cause="guide_count or children_per_guide not scaled to surface area",
                bounded_repair={"parameters": ["guide_count", "children_per_guide"]},
                blast_radius=["guard hair", "undercoat", "web shells"],
                acceptance_test="density_per_m2 within DENSITY_PER_M2_RANGE",
                measured={"density_per_m2": density, "range": [dlow, dhigh]},
            )
        )

    guard = int(result.report["guard_strands"])
    under = int(result.report["undercoat_strands"])
    if guard and under / guard < MIN_UNDERCOAT_RATIO:
        findings.append(
            CriticFinding(
                finding_id="groom.undercoat_missing",
                critic_role="groom artist",
                diagnosis=(
                    f"undercoat is thin relative to guard hair: {under}/{guard} = "
                    f"{under / guard:.2f}"
                ),
                evidence=evidence,
                severity="minor",
                confidence=0.7,
                likely_cause="undercoat_children_per_guide too low",
                bounded_repair={"parameters": ["undercoat_children_per_guide"]},
                blast_radius=["undercoat"],
                acceptance_test=f"undercoat/guard >= {MIN_UNDERCOAT_RATIO}",
                measured={"guard": guard, "undercoat": under, "ratio": under / guard},
            )
        )

    return GroomCritique(findings=findings)
