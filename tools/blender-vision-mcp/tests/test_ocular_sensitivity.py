"""Sensitivity receipts and critic calibration (Phases L and O)."""

from __future__ import annotations

from blender_vision.materials.parity import (
    DEFAULT_PROBE_RIG,
    SENSITIVITY_PROBE_RIG,
    compare_images,
    measure_highlight,
    render_poster,
)
from blender_vision.ocular.critics_calibration import (
    CRITIC_SPECS,
    CaseKind,
    CriticRole,
    calibrate_all,
    calibrate_role,
    calibration_matrix,
    critic_sensitivity_receipt,
    expected_fire,
    matrix_passed,
)
from blender_vision.ocular.sensitivity import (
    ConfounderResult,
    ProbeSensitivityReceipt,
    ResponsePoint,
    SensitivityVerdict,
    SweepParameter,
    build_receipt,
    classify_sensitivity,
    evaluate_confounders,
    evaluate_discrimination,
    linspace_values,
    offline_parameter_sweep,
    roughness_before_after,
)
from blender_vision.v2.authority import AuthorityClass
from blender_vision.v2.records import MaterialHypothesis


def test_synthetic_sensitive_metric_is_authoritative() -> None:
    """A metric with known linear sensitivity classifies AUTHORITATIVE."""
    values = linspace_values(0.0, 1.0, 9)
    curve = [
        ResponsePoint(parameter_value=v, metric_value=v * 10.0) for v in values
    ]
    # Meaningful delta 0.2 → metric change 2.0; margin 1.0 → pass.
    confounders = [
        ConfounderResult(
            name="resolution",
            metric_delta=0.05,
            max_allowed_delta=0.2,
            passed=True,
        ),
        ConfounderResult(
            name="png_reencode",
            metric_delta=0.01,
            max_allowed_delta=0.2,
            passed=True,
        ),
        ConfounderResult(
            name="one_pixel_crop",
            metric_delta=0.02,
            max_allowed_delta=0.2,
            passed=True,
        ),
        ConfounderResult(
            name="sample_count",
            metric_delta=0.0,
            max_allowed_delta=0.2,
            passed=True,
        ),
    ]
    receipt = build_receipt(
        metric_name="synthetic_linear",
        parameter=SweepParameter.ROUGHNESS,
        curve=curve,
        meaningful_delta=0.2,
        discrimination_margin=1.0,
        confounders=confounders,
    )
    assert receipt.discrimination_passed is True
    assert receipt.confounder_passed is True
    assert receipt.verdict is SensitivityVerdict.AUTHORITATIVE
    receipt.verify()
    assert receipt.digest
    assert receipt.measured_discrimination_threshold is not None


def test_constant_metric_is_diagnostic() -> None:
    """A metric that never moves is DIAGNOSTIC."""
    values = linspace_values(0.1, 0.9, 9)
    curve = [ResponsePoint(parameter_value=v, metric_value=1.0) for v in values]
    confounders = [
        ConfounderResult(
            name="resolution", metric_delta=0.0, max_allowed_delta=0.1, passed=True
        ),
        ConfounderResult(
            name="png_reencode", metric_delta=0.0, max_allowed_delta=0.1, passed=True
        ),
        ConfounderResult(
            name="one_pixel_crop", metric_delta=0.0, max_allowed_delta=0.1, passed=True
        ),
        ConfounderResult(
            name="sample_count", metric_delta=0.0, max_allowed_delta=0.1, passed=True
        ),
    ]
    receipt = build_receipt(
        metric_name="constant",
        parameter=SweepParameter.ROUGHNESS,
        curve=curve,
        meaningful_delta=0.2,
        discrimination_margin=0.5,
        confounders=confounders,
    )
    assert receipt.discrimination_passed is False
    assert receipt.verdict is SensitivityVerdict.DIAGNOSTIC
    receipt.verify()


def test_confounder_responsive_metric_is_diagnostic() -> None:
    """A metric that also moves on a confounder is DIAGNOSTIC even if sensitive."""
    curve = [
        ResponsePoint(parameter_value=v, metric_value=v * 5.0)
        for v in linspace_values(0.0, 1.0, 6)
    ]
    confounders = [
        ConfounderResult(
            name="resolution",
            metric_delta=3.0,
            max_allowed_delta=0.2,
            passed=False,
        ),
        ConfounderResult(
            name="png_reencode", metric_delta=0.0, max_allowed_delta=0.2, passed=True
        ),
        ConfounderResult(
            name="one_pixel_crop", metric_delta=0.0, max_allowed_delta=0.2, passed=True
        ),
        ConfounderResult(
            name="sample_count", metric_delta=0.0, max_allowed_delta=0.2, passed=True
        ),
    ]
    receipt = build_receipt(
        metric_name="confounded",
        parameter=SweepParameter.METALNESS,
        curve=curve,
        meaningful_delta=0.25,
        discrimination_margin=0.5,
        confounders=confounders,
    )
    assert receipt.discrimination_passed is True
    assert receipt.confounder_passed is False
    assert receipt.verdict is SensitivityVerdict.DIAGNOSTIC
    receipt.verify()


def test_receipt_refuses_authoritative_without_both_halves() -> None:
    """Sealing AUTHORITATIVE without both halves must raise."""
    import pytest

    from blender_vision.core.errors import ValidationError

    curve = [ResponsePoint(parameter_value=0.0, metric_value=0.0)]
    receipt = ProbeSensitivityReceipt(
        id="bad-auth",
        metric_name="x",
        parameter_name="roughness",
        response_curve=curve,
        discrimination_passed=True,
        confounder_passed=False,
        verdict=SensitivityVerdict.AUTHORITATIVE,
        authority=AuthorityClass.MODEL_DERIVED,
    )
    with pytest.raises(ValidationError, match="AUTHORITATIVE"):
        receipt.seal()


def test_evaluate_discrimination_and_confounders_helpers() -> None:
    ok, thr, peak = evaluate_discrimination(
        [ResponsePoint(0.0, 0.0), ResponsePoint(0.2, 2.0), ResponsePoint(0.4, 4.0)],
        meaningful_delta=0.2,
        discrimination_margin=1.5,
    )
    assert ok is True
    assert thr is not None
    assert peak == 4.0
    assert evaluate_confounders(
        [ConfounderResult("a", 0.0, 0.1, True), ConfounderResult("b", 0.05, 0.1, True)]
    )
    assert not evaluate_confounders(
        [ConfounderResult("a", 1.0, 0.1, False)]
    )
    assert classify_sensitivity(discrimination_passed=True, confounder_passed=True) is (
        SensitivityVerdict.AUTHORITATIVE
    )
    assert classify_sensitivity(discrimination_passed=True, confounder_passed=False) is (
        SensitivityVerdict.DIAGNOSTIC
    )


def test_highlight_metrics_discriminate_roughness() -> None:
    """Specular lobe width on metal must move under a meaningful roughness step."""
    metal_low = MaterialHypothesis(
        hypothesis_id="hl-low",
        label="metal",
        base_colour=[0.88, 0.88, 0.90],
        roughness=0.15,
        metalness=1.0,
        authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
    )
    metal_high = MaterialHypothesis(
        hypothesis_id="hl-high",
        label="metal",
        base_colour=[0.88, 0.88, 0.90],
        roughness=0.55,
        metalness=1.0,
        authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
    )
    from blender_vision.materials.parity import _load_rgb

    img_low = _load_rgb(render_poster(metal_low, rig=SENSITIVITY_PROBE_RIG))
    img_high = _load_rgb(render_poster(metal_high, rig=SENSITIVITY_PROBE_RIG))
    hl_low = measure_highlight(img_low)
    hl_high = measure_highlight(img_high)
    metrics = compare_images(img_low, img_high)
    # Either lobe width or peak (or highlight dE) must register a real change.
    moved = (
        abs(hl_high.lobe_fwhm_px - hl_low.lobe_fwhm_px) >= 3.0
        or abs(hl_high.peak_energy - hl_low.peak_energy) >= 0.05
        or metrics.highlight_delta_e2000 >= 1.5
        or metrics.delta_e2000 >= 1.0
    )
    assert moved, (
        f"roughness 0.15→0.55 produced no highlight response: "
        f"fwhm {hl_low.lobe_fwhm_px}→{hl_high.lobe_fwhm_px}, "
        f"peak {hl_low.peak_energy}→{hl_high.peak_energy}, "
        f"dE={metrics.delta_e2000}, hl_dE={metrics.highlight_delta_e2000}"
    )


def test_roughness_before_after_reports_both_curves() -> None:
    report = roughness_before_after(steps=5)
    assert "before" in report and "after" in report
    assert report["before"]["span"] >= 0.0
    assert "specular_lobe_width" in report["after"]["metrics"]
    assert report["after"]["metrics"]["specular_lobe_width"]["span"] >= 0.0


def test_sensitivity_probe_rig_differs_from_default() -> None:
    assert SENSITIVITY_PROBE_RIG.resolution >= DEFAULT_PROBE_RIG.resolution
    assert SENSITIVITY_PROBE_RIG.light_size < DEFAULT_PROBE_RIG.light_size


def test_each_critic_fires_positive_and_near_threshold() -> None:
    for role in CriticRole:
        cells = {cell.case_kind: cell for cell in calibrate_role(role)}
        assert cells[CaseKind.POSITIVE].fired is True
        assert cells[CaseKind.POSITIVE].passed is True
        assert cells[CaseKind.NEAR_THRESHOLD].fired is True
        assert cells[CaseKind.NEAR_THRESHOLD].passed is True


def test_each_critic_silent_on_negative_and_confounder() -> None:
    for role in CriticRole:
        cells = {cell.case_kind: cell for cell in calibrate_role(role)}
        assert cells[CaseKind.NEGATIVE].fired is False
        assert cells[CaseKind.NEGATIVE].passed is True
        assert cells[CaseKind.CONFOUNDER].fired is False
        assert cells[CaseKind.CONFOUNDER].passed is True
        assert cells[CaseKind.FALSE_POSITIVE_CHECK].fired is False
        assert cells[CaseKind.FALSE_POSITIVE_CHECK].passed is True
        assert cells[CaseKind.REPAIR_VERIFICATION].fired is False
        assert cells[CaseKind.REPAIR_VERIFICATION].passed is True


def test_calibration_matrix_complete_and_passing() -> None:
    cells = calibration_matrix()
    # 10 roles × 6 cases
    assert len(cells) == len(CriticRole) * 6
    assert matrix_passed(cells)
    payload = calibrate_all()
    assert payload["all_passed"] is True
    for role in CriticRole:
        receipt = critic_sensitivity_receipt(role)
        receipt.verify()
        assert receipt.verdict is SensitivityVerdict.AUTHORITATIVE
        assert expected_fire(CaseKind.POSITIVE) is True
        assert expected_fire(CaseKind.NEGATIVE) is False
        assert role in CRITIC_SPECS


def test_offline_roughness_sweep_produces_curve() -> None:
    renders = offline_parameter_sweep(SweepParameter.ROUGHNESS, steps=5)
    assert len(renders) == 5
    assert renders[0].parameter_value == 0.1
    assert renders[-1].parameter_value == 0.9
    # Highlights should exist on metal base.
    hl = measure_highlight(renders[0].image)
    assert hl.peak_energy > 0.0
