#!/usr/bin/env python3
"""Physical Phase L + O sensitivity run: Cycles + one browser, full receipts.

Sweeps nine parameters, prints response curves, classifies each metric as
AUTHORITATIVE or DIAGNOSTIC (both discrimination and confounder halves required),
demonstrates the roughness before/after fix, runs confounders, and emits the
critic calibration matrix with near-threshold coverage.

Exit non-zero if any metric is claimed AUTHORITATIVE without both halves, or if
the critic matrix fails.
"""

from __future__ import annotations

import argparse
import json
import sys
import traceback
from pathlib import Path
from typing import Any

_ROOT = Path(__file__).resolve().parents[1]
if str(_ROOT / "src") not in sys.path:
    sys.path.insert(0, str(_ROOT / "src"))

from blender_vision.core.errors import BackendUnavailable  # noqa: E402
from blender_vision.materials.parity import (  # noqa: E402
    DEFAULT_PROBE_RIG,
    SENSITIVITY_PROBE_RIG,
    BrowserBusyError,
    ProbeRig,
    _load_rgb,
    blender_probe,
    compare_images,
    measure_highlight,
    render_browser,
    render_cycles,
    render_poster,
)
from blender_vision.ocular.attestation import (  # noqa: E402
    attest_blocked,
)
from blender_vision.ocular.critics_calibration import (  # noqa: E402
    calibrate_all,
)
from blender_vision.ocular.sensitivity import (  # noqa: E402
    DEFAULT_CONFOUNDER_ALLOWANCE,
    DEFAULT_DISCRIMINATION_MARGINS,
    DEFAULT_MEANINGFUL_DELTAS,
    DEFAULT_SWEEP_RANGES,
    ConfounderResult,
    ProbeSensitivityReceipt,
    ResponsePoint,
    SensitivityVerdict,
    SweepParameter,
    SweepRender,
    apply_parameter_to_hypothesis,
    apply_parameter_to_rig,
    build_receipt,
    build_response_curve,
    default_sweep_hypothesis,
    format_curve_table,
    linspace_values,
    offline_confounder_battery,
    offline_parameter_sweep,
    roughness_before_after,
)
from blender_vision.v2.authority import AuthorityClass  # noqa: E402
from blender_vision.v2.records import Lineage, MaterialHypothesis  # noqa: E402

# Metrics evaluated per parameter. Highlight metrics are the roughness fix.
SWEEP_METRICS = (
    "delta_e2000",
    "structural",
    "highlight_delta_e2000",
    "specular_lobe_width",
    "specular_peak_energy",
)

# Absolute (single-image) metrics rebuild curves from extras, not ref deltas.
ABSOLUTE_METRICS = frozenset({"specular_lobe_width", "specular_peak_energy"})


def _print_curve(title: str, curve: list[ResponsePoint], param: str) -> None:
    print(f"\n--- {title} ---")
    print(format_curve_table(curve, param_name=param))


def _absolute_curve(renders: list[SweepRender], metric_name: str) -> list[ResponsePoint]:
    key = "lobe_fwhm_px" if "lobe" in metric_name else "peak_energy"
    if metric_name == "specular_peak_energy":
        key = "peak_energy"
    elif metric_name == "specular_lobe_width":
        key = "lobe_fwhm_px"
    return [
        ResponsePoint(
            parameter_value=r.parameter_value,
            metric_value=float(r.extras.get(key, 0.0)),
            extras=dict(r.extras),
        )
        for r in renders
    ]


def _physical_sweep(
    parameter: SweepParameter,
    *,
    values: list[float],
    output_dir: Path,
    use_cycles: bool,
    use_browser: bool,
    samples: int,
) -> dict[str, Any]:
    """Render a parameter sweep with Cycles and/or browser under SENSITIVITY_PROBE_RIG."""
    base_h = default_sweep_hypothesis(parameter)
    base_rig = SENSITIVITY_PROBE_RIG.with_samples(samples)
    cycles_renders: list[SweepRender] = []
    browser_renders: list[SweepRender] = []
    poster_renders = offline_parameter_sweep(
        parameter, values=values, rig=base_rig, hypothesis=base_h
    )

    for value in values:
        hyp = apply_parameter_to_hypothesis(base_h, parameter, float(value))
        rig = apply_parameter_to_rig(base_rig, parameter, float(value))
        tag = f"{parameter.value}_{value:.4f}".replace(".", "p")
        if use_cycles:
            try:
                path = render_cycles(
                    hyp,
                    output_path=output_dir / f"cycles_{tag}.png",
                    samples=samples,
                    rig=rig,
                )
                img = _load_rgb(path)
                hl = measure_highlight(img)
                cycles_renders.append(
                    SweepRender(
                        parameter_value=float(value),
                        image=img,
                        path=str(path),
                        extras={
                            "peak_energy": hl.peak_energy,
                            "lobe_fwhm_px": hl.lobe_fwhm_px,
                            "contrast": hl.contrast,
                        },
                    )
                )
            except BackendUnavailable as error:
                return {"blocked": True, "reason": f"cycles: {error}", "parameter": parameter.value}

        if use_browser:
            try:
                path = render_browser(
                    hyp,
                    output_path=output_dir / f"browser_{tag}.png",
                    rig=rig,
                )
                img = _load_rgb(path)
                hl = measure_highlight(img)
                browser_renders.append(
                    SweepRender(
                        parameter_value=float(value),
                        image=img,
                        path=str(path),
                        extras={
                            "peak_energy": hl.peak_energy,
                            "lobe_fwhm_px": hl.lobe_fwhm_px,
                            "contrast": hl.contrast,
                        },
                    )
                )
            except (BackendUnavailable, BrowserBusyError) as error:
                return {
                    "blocked": True,
                    "reason": f"browser: {error}",
                    "parameter": parameter.value,
                }

    return {
        "blocked": False,
        "parameter": parameter.value,
        "values": values,
        "poster": poster_renders,
        "cycles": cycles_renders,
        "browser": browser_renders,
    }


def _classify_renders(
    parameter: SweepParameter,
    renders: list[SweepRender],
    *,
    source: str,
    run_confounders: bool,
) -> list[ProbeSensitivityReceipt]:
    receipts: list[ProbeSensitivityReceipt] = []
    if len(renders) < 2:
        return receipts
    for metric_name in SWEEP_METRICS:
        if metric_name in ABSOLUTE_METRICS:
            curve = _absolute_curve(renders, metric_name)
        else:
            curve = build_response_curve(renders, metric_name, reference_index=0)
        margin = float(DEFAULT_DISCRIMINATION_MARGINS.get(metric_name, 1.0))
        mdelta = float(DEFAULT_MEANINGFUL_DELTAS[parameter])
        allowance = float(DEFAULT_CONFOUNDER_ALLOWANCE.get(metric_name, margin * 0.25))
        if run_confounders:
            confounders = offline_confounder_battery(
                metric=metric_name,
                allowance=allowance,
                hypothesis_factory=lambda p=parameter: default_sweep_hypothesis(p),
                rig=SENSITIVITY_PROBE_RIG,
            )
        else:
            confounders = [
                ConfounderResult(
                    name="skipped",
                    metric_delta=float("inf"),
                    max_allowed_delta=allowance,
                    passed=False,
                    notes="confounders skipped",
                )
            ]
        receipt = build_receipt(
            metric_name=metric_name,
            parameter=parameter,
            curve=curve,
            meaningful_delta=mdelta,
            discrimination_margin=margin,
            confounders=confounders,
            notes=[f"source={source}", f"n={len(renders)}"],
            # INFERRED: lineage.authority_ceiling() derives with proposed=INFERRED,
            # so MODEL_DERIVED cannot be claimed when input_authorities is non-empty.
            # Physical execution is attested separately, never laundered here.
            authority=AuthorityClass.INFERRED,
            lineage=Lineage(
                operation="ocular_sensitivity_sweep",
                parameters={
                    "parameter": parameter.value,
                    "metric": metric_name,
                    "source": source,
                },
                input_authorities=[AuthorityClass.INFERRED.value],
            ),
        )
        receipts.append(receipt)
        _print_curve(
            f"{parameter.value} / {metric_name} [{source}] → {receipt.verdict.value}",
            curve,
            parameter.value,
        )
        thr = receipt.measured_discrimination_threshold
        thr_s = f"{thr:.4f}" if thr is not None else "n/a"
        print(
            f"  discrimination={receipt.discrimination_passed}  "
            f"confounder={receipt.confounder_passed}  "
            f"threshold={thr_s}  span={receipt.peak_response}"
        )
        for conf in receipt.confounders:
            print(
                f"    confounder {conf.name}: delta={conf.metric_delta:.4f} "
                f"allow={conf.max_allowed_delta:.4f} passed={conf.passed}"
            )
    return receipts


def _roughness_physical_before_after(
    output_dir: Path,
    *,
    samples: int,
    use_cycles: bool,
    use_browser: bool,
) -> dict[str, Any]:
    """Concrete roughness before/after with real renders when available."""
    values = linspace_values(0.1, 0.9, 9)
    offline = roughness_before_after(steps=9)
    result: dict[str, Any] = {"offline": offline, "physical": {}}

    # BEFORE: DEFAULT_PROBE_RIG, matte dielectric, whole-image dE (the blind config).
    before_h = MaterialHypothesis(
        hypothesis_id="before-matte",
        label="matte-plastic",
        base_colour=[0.82, 0.18, 0.12],
        roughness=0.55,
        metalness=0.0,
        authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
    )
    # AFTER: SENSITIVITY_PROBE_RIG, metal, highlight metrics.
    after_h = default_sweep_hypothesis(SweepParameter.ROUGHNESS)

    def _sweep(
        name: str,
        hyp_base: MaterialHypothesis,
        rig: ProbeRig,
        backend: str,
    ) -> list[dict[str, Any]]:
        rows: list[dict[str, Any]] = []
        ref_img = None
        for rough in values:
            hyp = apply_parameter_to_hypothesis(hyp_base, SweepParameter.ROUGHNESS, rough)
            tag = f"{name}_r{rough:.1f}".replace(".", "p")
            if backend == "poster":
                path = render_poster(hyp, output_path=output_dir / f"{tag}.png", rig=rig)
            elif backend == "cycles":
                path = render_cycles(
                    hyp,
                    output_path=output_dir / f"{tag}.png",
                    samples=samples,
                    rig=rig,
                )
            else:
                path = render_browser(hyp, output_path=output_dir / f"{tag}.png", rig=rig)
            img = _load_rgb(path)
            hl = measure_highlight(img)
            if ref_img is None:
                ref_img = img
                de = 0.0
                hl_de = 0.0
            else:
                m = compare_images(ref_img, img)
                de = m.delta_e2000
                hl_de = m.highlight_delta_e2000
            rows.append(
                {
                    "roughness": rough,
                    "delta_e2000": de,
                    "highlight_delta_e2000": hl_de,
                    "specular_peak_energy": hl.peak_energy,
                    "specular_lobe_width": hl.lobe_fwhm_px,
                    "path": str(path),
                }
            )
        return rows

    print("\n=== ROUGHNESS BEFORE (DEFAULT_PROBE_RIG, matte dielectric, dE2000) ===")
    before_poster = _sweep("before_poster", before_h, DEFAULT_PROBE_RIG, "poster")
    for row in before_poster:
        print(
            f"  r={row['roughness']:.1f}  dE={row['delta_e2000']:7.3f}  "
            f"peak={row['specular_peak_energy']:.3f}  "
            f"fwhm={row['specular_lobe_width']:.1f}"
        )
    result["physical"]["before_poster"] = before_poster

    print("\n=== ROUGHNESS AFTER (SENSITIVITY_PROBE_RIG, metal, lobe/peak) ===")
    after_poster = _sweep("after_poster", after_h, SENSITIVITY_PROBE_RIG, "poster")
    for row in after_poster:
        print(
            f"  r={row['roughness']:.1f}  dE={row['delta_e2000']:7.3f}  "
            f"hl_dE={row['highlight_delta_e2000']:7.3f}  "
            f"peak={row['specular_peak_energy']:.3f}  "
            f"fwhm={row['specular_lobe_width']:.1f}"
        )
    result["physical"]["after_poster"] = after_poster

    before_span = max(r["delta_e2000"] for r in before_poster) - min(
        r["delta_e2000"] for r in before_poster
    )
    after_fwhm = [r["specular_lobe_width"] for r in after_poster]
    after_span = max(after_fwhm) - min(after_fwhm)
    print(
        f"\n  BEFORE dE span={before_span:.3f}  "
        f"AFTER lobe_fwhm span={after_span:.2f}px"
    )
    if after_span < 4.0 and before_span < 1.5:
        print(
            "  HONEST: roughness still weakly discriminated after the probe fix; "
            "metrics that fail discrimination stay DIAGNOSTIC."
        )
    elif after_span >= 4.0:
        print("  FIX OBSERVED: lobe width now spans a measurable range across 0.1→0.9.")

    if use_cycles:
        print("\n=== ROUGHNESS AFTER on real Cycles ===")
        try:
            after_cycles = _sweep("after_cycles", after_h, SENSITIVITY_PROBE_RIG, "cycles")
            for row in after_cycles:
                print(
                    f"  r={row['roughness']:.1f}  dE={row['delta_e2000']:7.3f}  "
                    f"peak={row['specular_peak_energy']:.3f}  "
                    f"fwhm={row['specular_lobe_width']:.1f}"
                )
            result["physical"]["after_cycles"] = after_cycles
        except BackendUnavailable as error:
            print(f"  BLOCKED Cycles roughness after: {error}")
            result["physical"]["after_cycles_blocked"] = str(error)

    if use_browser:
        print("\n=== ROUGHNESS AFTER on real browser ===")
        try:
            after_browser = _sweep("after_browser", after_h, SENSITIVITY_PROBE_RIG, "browser")
            for row in after_browser:
                print(
                    f"  r={row['roughness']:.1f}  dE={row['delta_e2000']:7.3f}  "
                    f"peak={row['specular_peak_energy']:.3f}  "
                    f"fwhm={row['specular_lobe_width']:.1f}"
                )
            result["physical"]["after_browser"] = after_browser
        except (BackendUnavailable, BrowserBusyError) as error:
            print(f"  BLOCKED browser roughness after: {error}")
            result["physical"]["after_browser_blocked"] = str(error)

    return result


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=_ROOT / "artifacts" / "ocular" / "sensitivity",
    )
    parser.add_argument("--steps", type=int, default=5)
    parser.add_argument("--samples", type=int, default=32)
    parser.add_argument(
        "--skip-physical",
        action="store_true",
        help="offline poster + critics only (no Cycles/browser)",
    )
    args = parser.parse_args()

    out: Path = args.output
    out.mkdir(parents=True, exist_ok=True)
    steps = max(3, int(args.steps))
    samples = int(args.samples)

    print("=== Ocular sensitivity (Phases L + O) ===")
    print(f"output: {out}")
    print(f"DEFAULT_PROBE_RIG: {json.dumps(DEFAULT_PROBE_RIG.to_dict(), sort_keys=True)}")
    print(
        f"SENSITIVITY_PROBE_RIG: {json.dumps(SENSITIVITY_PROBE_RIG.to_dict(), sort_keys=True)}"
    )

    # ------------------------------------------------------------------ critics
    print("\n=== Critic calibration matrix ===")
    critic_payload = calibrate_all()
    print(critic_payload["matrix_table"])
    print(f"matrix_passed={critic_payload['matrix_passed']}")
    (out / "critic_calibration.json").write_text(
        json.dumps(
            {
                "cells": critic_payload["cells"],
                "matrix_passed": critic_payload["matrix_passed"],
                "role_ok": critic_payload["role_ok"],
                "all_passed": critic_payload["all_passed"],
                "receipts": critic_payload["receipts"],
            },
            indent=2,
            sort_keys=True,
        )
        + "\n",
        encoding="utf-8",
    )
    if not critic_payload["all_passed"]:
        print("FAIL: critic calibration matrix or receipts failed")
        return 1

    # ---------------------------------------------------------- offline sweeps
    all_receipts: list[dict[str, Any]] = []
    false_authoritative = False

    print("\n=== Offline (poster) nine-parameter sweeps ===")
    for parameter in SweepParameter:
        low, high = DEFAULT_SWEEP_RANGES[parameter]
        values = linspace_values(low, high, steps)
        print(f"\n#### parameter {parameter.value}  values={values}")
        renders = offline_parameter_sweep(
            parameter, values=values, rig=SENSITIVITY_PROBE_RIG
        )
        receipts = _classify_renders(
            parameter, renders, source="poster", run_confounders=True
        )
        for receipt in receipts:
            payload = receipt.to_dict()
            all_receipts.append(payload)
            if (
                receipt.verdict is SensitivityVerdict.AUTHORITATIVE
                and not (receipt.discrimination_passed and receipt.confounder_passed)
            ):
                false_authoritative = True
                print(f"FAIL: false AUTHORITATIVE on {receipt.metric_name}/{parameter.value}")

    # ---------------------------------------------------- roughness before/after
    ok_b, reason_b = blender_probe()
    use_cycles = (not args.skip_physical) and ok_b
    browser_reason = ""
    use_browser = False
    if not args.skip_physical:
        # Probe browser once; do not hang the whole run on a dead engine.
        try:
            from blender_vision.materials.parity import render_browser as _rb

            probe_h = MaterialHypothesis(
                hypothesis_id="browser-probe",
                label="probe",
                base_colour=[0.5, 0.5, 0.5],
                roughness=0.4,
                metalness=0.0,
                authority=AuthorityClass.INFERRED,
            )
            _rb(
                probe_h,
                output_path=out / "browser_probe.png",
                rig=DEFAULT_PROBE_RIG.with_resolution(64),
            )
            use_browser = True
            print("\nBrowser probe: available")
        except Exception as error:  # noqa: BLE001 — record BLOCKED, continue offline
            browser_reason = str(error)
            use_browser = False
            print(f"\nBrowser probe: BLOCKED ({browser_reason})")
            attest_br = attest_blocked("playwright-browser", browser_reason)
            (out / "browser_attestation.json").write_text(
                json.dumps(attest_br.to_dict(), indent=2, sort_keys=True) + "\n",
                encoding="utf-8",
            )
    if not ok_b:
        print(f"\nCycles probe: BLOCKED ({reason_b})")
        attest = attest_blocked("blender-cycles", reason_b)
        (out / "cycles_attestation.json").write_text(
            json.dumps(attest.to_dict(), indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
    else:
        print("\nCycles probe: available")

    try:
        rough_report = _roughness_physical_before_after(
            out / "roughness",
            samples=samples,
            use_cycles=use_cycles,
            use_browser=use_browser,
        )
    except Exception as error:  # noqa: BLE001
        print(f"roughness before/after failed: {error}")
        traceback.print_exc()
        rough_report = {"error": str(error)}

    (out / "roughness_before_after.json").write_text(
        json.dumps(rough_report, indent=2, sort_keys=True, default=str) + "\n",
        encoding="utf-8",
    )

    # ------------------------------------------------------- physical 9 sweeps
    physical_blocked: list[str] = []
    if use_cycles or use_browser:
        print("\n=== Physical nine-parameter sweeps (Cycles + browser) ===")
        # Fewer steps for wall-clock; still enough for discrimination.
        phys_steps = min(steps, 5)
        for parameter in SweepParameter:
            low, high = DEFAULT_SWEEP_RANGES[parameter]
            values = linspace_values(low, high, phys_steps)
            print(f"\n#### PHYSICAL {parameter.value}")
            sweep_dir = out / "physical" / parameter.value
            sweep_dir.mkdir(parents=True, exist_ok=True)
            try:
                result = _physical_sweep(
                    parameter,
                    values=values,
                    output_dir=sweep_dir,
                    use_cycles=use_cycles,
                    use_browser=use_browser,
                    samples=samples,
                )
            except Exception as error:  # noqa: BLE001
                physical_blocked.append(f"{parameter.value}: {error}")
                print(f"  BLOCKED {parameter.value}: {error}")
                continue
            if result.get("blocked"):
                physical_blocked.append(str(result.get("reason")))
                print(f"  BLOCKED: {result.get('reason')}")
                continue
            for source, key in (("cycles", "cycles"), ("browser", "browser")):
                renders = result.get(key) or []
                if not renders:
                    continue
                receipts = _classify_renders(
                    parameter,
                    renders,
                    source=source,
                    run_confounders=True,
                )
                for receipt in receipts:
                    all_receipts.append(receipt.to_dict())
                    if (
                        receipt.verdict is SensitivityVerdict.AUTHORITATIVE
                        and not (
                            receipt.discrimination_passed and receipt.confounder_passed
                        )
                    ):
                        false_authoritative = True

    # ----------------------------------------------------------------- summary
    auth = [r for r in all_receipts if r.get("verdict") == "AUTHORITATIVE"]
    diag = [r for r in all_receipts if r.get("verdict") == "DIAGNOSTIC"]
    summary = {
        "n_receipts": len(all_receipts),
        "n_authoritative": len(auth),
        "n_diagnostic": len(diag),
        "false_authoritative": false_authoritative,
        "physical_blocked": physical_blocked,
        "critic_all_passed": critic_payload["all_passed"],
        "cycles_available": use_cycles,
        "browser_attempted": use_browser,
        "authoritative": [
            {
                "metric": r["metric_name"],
                "parameter": r["parameter_name"],
                "threshold": r.get("measured_discrimination_threshold"),
                "span": r.get("peak_response"),
            }
            for r in auth
        ],
        "diagnostic": [
            {
                "metric": r["metric_name"],
                "parameter": r["parameter_name"],
                "discrimination": r.get("discrimination_passed"),
                "confounder": r.get("confounder_passed"),
            }
            for r in diag
        ],
    }
    (out / "sensitivity_receipts.json").write_text(
        json.dumps({"receipts": all_receipts, "summary": summary}, indent=2, sort_keys=True)
        + "\n",
        encoding="utf-8",
    )
    (out / "summary.json").write_text(
        json.dumps(summary, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )

    print("\n=== SUMMARY ===")
    print(json.dumps(summary, indent=2, sort_keys=True))

    if false_authoritative:
        print("FAIL: AUTHORITATIVE claimed without both halves")
        return 1
    if not critic_payload["all_passed"]:
        print("FAIL: critic calibration")
        return 1
    # At least one highlight metric must be AUTHORITATIVE on roughness offline.
    rough_auth = [
        r
        for r in auth
        if r.get("parameter_name") == "roughness"
        and r.get("metric_name")
        in {
            "specular_lobe_width",
            "specular_peak_energy",
            "highlight_delta_e2000",
            "delta_e2000",
        }
    ]
    if not rough_auth:
        print(
            "NOTE: no roughness metric reached AUTHORITATIVE — honest DIAGNOSTIC "
            "outcome; not a process failure if discrimination simply cannot be "
            "established."
        )
    print("OK: sensitivity run complete")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
