from __future__ import annotations

from blender_vision.active_perception import (
    NextBestViewPlanner,
    PerceptionTarget,
    PlannerConfig,
    StopReason,
    SurfaceCell,
    estimate_information_gain,
    quantify_uncertainty,
)
from blender_vision.active_perception.information_gain import ProposedView
from blender_vision.v2.authority import AuthorityClass
from blender_vision.v2.validation import validate_record


def _partial_target() -> PerceptionTarget:
    return PerceptionTarget(
        target_id="mug-partial",
        cells=[
            SurfaceCell(
                region="front",
                area_m2=1.0,
                covered=True,
                occlusion_fraction=0.0,
                resolution_px=1600,
                candidate_predictions=[0.1, 0.12],
            ),
            SurfaceCell(
                region="underside",
                area_m2=1.0,
                covered=False,
                candidate_predictions=[0.0, 0.8, 0.9],
            ),
            SurfaceCell(
                region="left",
                area_m2=0.8,
                covered=False,
                candidate_predictions=[0.2, 0.7],
            ),
            SurfaceCell(
                region="right",
                area_m2=0.8,
                covered=True,
                occlusion_fraction=0.0,
                resolution_px=1600,
            ),
        ],
        scale_authority=AuthorityClass.UNRESOLVED,
        material_confidences=[0.4, 0.35, 0.3],
        has_scale_reference=False,
        has_diffuse_light_view=False,
        has_grazing_light_view=False,
        has_lens_metadata=False,
        has_calibration_target=False,
    )


def test_planner_orders_by_expected_gain() -> None:
    planner = NextBestViewPlanner()
    result = planner.plan(_partial_target())
    assert result.requests
    gains = [r.expected_reduction for r in result.requests]
    assert gains == sorted(gains, reverse=True)
    kinds = " ".join(r.id for r in result.requests)
    for token in (
        "underside",
        "side",
        "scale-reference",
        "diffuse-light",
        "grazing-light",
        "lens-metadata",
        "calibration-target",
    ):
        assert token in kinds


def test_next_view_request_validates_against_schema() -> None:
    request = NextBestViewPlanner().ask_next_view(_partial_target())[0]
    request.verify()
    validate_record(request)
    assert 0 <= request.priority <= 10
    assert request.expected_reduction >= 0
    assert request.capture_instructions
    assert request.human_instructions
    assert request.reason


def test_redundant_view_suppressed_on_full_coverage() -> None:
    target = PerceptionTarget(
        target_id="mug-full",
        cells=[
            SurfaceCell(
                region=name,
                area_m2=1.0,
                covered=True,
                occlusion_fraction=0.0,
                resolution_px=2000,
                candidate_predictions=[0.5, 0.5],
            )
            for name in ("front", "rear", "left", "right", "top", "underside")
        ],
        scale_authority=AuthorityClass.MEASURED,
        material_confidences=[0.95],
        has_scale_reference=True,
        has_diffuse_light_view=True,
        has_grazing_light_view=True,
        has_lens_metadata=True,
        has_calibration_target=True,
    )
    result = NextBestViewPlanner().plan(target)
    assert result.requests == []
    assert result.stop_reason is StopReason.GAIN_TOO_LOW


def test_stop_gates_satisfied() -> None:
    target = _partial_target()
    target.gates_satisfied = True
    result = NextBestViewPlanner().plan(target)
    assert result.requests == []
    assert result.stop_reason is StopReason.GATES_SATISFIED


def test_stop_user_declined() -> None:
    target = _partial_target()
    target.user_declined = True
    result = NextBestViewPlanner().plan(target)
    assert result.requests == []
    assert result.stop_reason is StopReason.USER_DECLINED


def test_stop_gain_too_low() -> None:
    target = PerceptionTarget(
        target_id="tiny",
        cells=[
            SurfaceCell(
                region="front",
                area_m2=1.0,
                covered=True,
                occlusion_fraction=0.0,
                resolution_px=2000,
            )
        ],
        scale_authority=AuthorityClass.MEASURED,
        material_confidences=[0.99],
        has_scale_reference=True,
        has_diffuse_light_view=True,
        has_grazing_light_view=True,
        has_lens_metadata=True,
        has_calibration_target=True,
    )
    result = NextBestViewPlanner(PlannerConfig(gain_threshold=0.5)).plan(target)
    assert result.requests == []
    assert result.stop_reason is StopReason.GAIN_TOO_LOW


def test_uncertainty_monotonically_decreases_as_coverage_increases() -> None:
    target = _partial_target()
    planner = NextBestViewPlanner()
    before = quantify_uncertainty(target).total
    totals = [before]
    for _ in range(3):
        result = planner.plan(target)
        if not result.requests:
            break
        planner.satisfy(target, result.requests[0])
        totals.append(quantify_uncertainty(target).total)
    assert len(totals) >= 2
    for earlier, later in zip(totals, totals[1:], strict=False):
        assert later <= earlier + 1e-12


def test_satisfy_top_request_drops_it_from_next_plan() -> None:
    target = _partial_target()
    planner = NextBestViewPlanner()
    first = planner.plan(target)
    top = first.requests[0]
    u0 = first.uncertainty_before
    planner.satisfy(target, top)
    second = planner.plan(target)
    assert second.uncertainty_before < u0
    assert all(r.id != top.id for r in second.requests)


def test_duplicate_signature_is_redundant() -> None:
    target = _partial_target()
    view = ProposedView(
        view_id="dup",
        kind="underside",
        regions=["underside"],
        signature="already",
    )
    target.existing_view_signatures.add("already")
    estimate = estimate_information_gain(target, view)
    assert estimate.redundant
    assert estimate.expected_reduction == 0.0
