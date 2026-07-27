from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

from blender_vision.core.errors import SecurityError, ValidationError
from blender_vision.grooming.fur import (
    CLUMP_TO_BODY_RANGE,
    DENSITY_PER_M2_RANGE,
    GroomParameters,
    GroomResult,
    critique_groom,
)
from blender_vision.organic.topology import (
    LODLevel,
    TopologyReport,
    UVReport,
    lod_identity_violations,
)

REPO = Path(__file__).resolve().parents[1]
BLENDER_GATE = pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BLENDER_TESTS") != "1",
    reason="set BVMCP_RUN_BLENDER_TESTS=1 for the real Blender organic lane",
)


def _groom_result(**report_overrides) -> GroomResult:
    report = {
        "guides": {"guide_count": 600, "mean_length_m": 0.020, "body_scale_m": 0.25},
        "guard_strands": 3600,
        "undercoat_strands": 6000,
        "clump_to_body_ratio": 0.05,
        "density_per_m2": 60_000.0,
        "surface_area_m2": 0.12,
        "counts": {"shells_triangles": 60_000, "cards_triangles": 20_000},
        "offline_blend": "fur-offline.blend",
        "web_glb": "fur-web.glb",
    }
    report.update(report_overrides)
    return GroomResult(
        parameters=GroomParameters(),
        report=report,
        offline_blend=Path("fur-offline.blend"),
        web_glb=Path("fur-web.glb"),
        script_sha256="a" * 64,
        blender_version="Blender 4.2.1 LTS",
    )


# ------------------------------------------------------------------ parameters


def test_groom_parameters_reject_impossible_clump() -> None:
    with pytest.raises(ValidationError):
        GroomParameters(clump=1.4)
    with pytest.raises(ValidationError):
        GroomParameters(undercoat_clump=-0.1)


def test_groom_parameters_reject_rope_thick_strands() -> None:
    with pytest.raises(ValidationError, match="rope, not fur"):
        GroomParameters(length_m=0.02, strand_radius_m=0.01)


def test_groom_parameters_reject_a_degenerate_groom() -> None:
    with pytest.raises(ValidationError):
        GroomParameters(guide_count=0)
    with pytest.raises(ValidationError):
        GroomParameters(segments=1)


# ------------------------------------------------------------------ critic


def test_groom_critic_requires_evidence() -> None:
    with pytest.raises(ValidationError):
        critique_groom(_groom_result(), evidence=[])


def test_groom_critic_accepts_a_plausible_groom() -> None:
    critique = critique_groom(_groom_result(), evidence=["sha256:x"])
    assert critique.passed
    assert critique.findings == []


def test_groom_critic_catches_clump_scale_larger_than_the_animal() -> None:
    critique = critique_groom(
        _groom_result(clump_to_body_ratio=CLUMP_TO_BODY_RANGE[1] * 3), evidence=["sha256:x"]
    )
    assert not critique.passed
    finding = next(item for item in critique.findings if item.finding_id == "groom.clump_scale")
    assert finding.severity == "major"
    assert finding.measured["clump_to_body_ratio"] > CLUMP_TO_BODY_RANGE[1]
    assert finding.evidence == ["sha256:x"]


def test_groom_critic_catches_bald_density() -> None:
    critique = critique_groom(
        _groom_result(density_per_m2=DENSITY_PER_M2_RANGE[0] / 10), evidence=["sha256:x"]
    )
    assert not critique.passed
    assert any(item.finding_id == "groom.density" for item in critique.findings)


def test_groom_critic_catches_a_missing_undercoat() -> None:
    critique = critique_groom(
        _groom_result(guard_strands=4000, undercoat_strands=100), evidence=["sha256:x"]
    )
    finding = next(
        item for item in critique.findings if item.finding_id == "groom.undercoat_missing"
    )
    assert finding.measured["ratio"] < 0.5


# ------------------------------------------------------------------ topology


def test_lod_identity_violation_is_reported() -> None:
    lods = [
        LODLevel(name="L1", ratio=0.5, triangles=1000, silhouette_iou=0.99, hausdorff_m=0.001),
        LODLevel(name="L3", ratio=0.05, triangles=60, silhouette_iou=0.42, hausdorff_m=0.09),
    ]
    assert lod_identity_violations(lods) == ["L3"]


def test_topology_report_quad_fraction() -> None:
    report = TopologyReport(
        vertices=100, edges=200, faces=98, triangles=8, quads=90, ngons=0,
        non_manifold_edges=0, boundary_edges=0, genus_estimate=0, is_watertight=True,
        surface_area_m2=1.0, volume_m3=0.1, bounds_m=[[0, 0, 0], [1, 1, 1]],
    )
    assert report.quad_fraction == pytest.approx(90 / 98)
    assert TopologyReport.from_dict(report.to_dict()) == report


def test_uv_report_round_trips() -> None:
    report = UVReport(
        island_count=34, packing_efficiency=0.62, max_area_distortion=0.34,
        mean_area_distortion=0.09, max_angle_distortion_deg=52.5,
        overlapping_faces=0, texel_density_variance=0.01,
    )
    assert UVReport.from_dict(report.to_dict()) == report


# ------------------------------------------------------------------ security


def test_the_executor_refuses_scripts_from_outside_the_repository(tmp_path: Path) -> None:
    from blender_vision.blender.v2_executor import V2BlenderExecutor

    stray = tmp_path / "evil.py"
    stray.write_text("import bpy\n")
    executor = V2BlenderExecutor(executable="/bin/echo")
    with pytest.raises(SecurityError, match="outside the repository"):
        executor.run(stray)


# ------------------------------------------------------------------ real runtime


@BLENDER_GATE
def test_real_organic_lane_receipt_is_present_and_passed() -> None:
    """The lane must have been executed; this asserts on its real receipt."""
    receipt_path = REPO / "artifacts" / "v2" / "organic" / "organic-fur-receipt.json"
    if not receipt_path.is_file():
        pytest.fail(
            "run `python scripts/run-organic-fur-lane.py` before this test; "
            f"{receipt_path} is missing"
        )
    receipt = json.loads(receipt_path.read_text(encoding="utf-8"))

    assert receipt["failures"] == []
    assert set(receipt["targets"]) == {
        "organic_sculpture",
        "plant",
        "draped_cloth",
        "animal_bust",
    }

    bust = receipt["targets"]["animal_bust"]
    assert bust["retopologized"]["is_watertight"] is True
    assert bust["retopologized"]["quad_fraction"] > 0.95
    assert bust["uv"]["island_count"] > 1
    assert bust["lod_identity_violations"] == []

    fur = receipt["fur"]
    assert fur["critique_passed"] is True
    assert fur["report"]["guard_strands"] > 0
    assert fur["report"]["undercoat_strands"] > 0
    assert "not evidence about any real animal" in fur["claim"]

    # The injected regression must have been caught, or the critic is decorative.
    regression = receipt["groom_regression"]
    assert regression["passed"] is False
    assert regression["caught"]
