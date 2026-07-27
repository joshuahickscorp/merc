#!/usr/bin/env python3
"""Prove the browser parity gate discriminates on real Cycles + real browser.

Runs the nine appearance-v2 benchmark materials through the shared ProbeRig,
compares browser WebGL against Cycles (published limits dE≤8, structural≤0.15),
injects a deliberately wrong browser material, and sweeps roughness so the
discrimination threshold is visible.

Exit non-zero if fewer than five materials pass, if the wrong material passes,
or if Cycles/browser are blocked in this environment.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

# Allow running as scripts/… without installing on PYTHONPATH.
_ROOT = Path(__file__).resolve().parents[1]
if str(_ROOT / "src") not in sys.path:
    sys.path.insert(0, str(_ROOT / "src"))

from blender_vision.core.errors import BackendUnavailable  # noqa: E402
from blender_vision.materials.parity import (  # noqa: E402
    DEFAULT_PROBE_RIG,
    BrowserBusyError,
    _load_rgb,
    blender_probe,
    compare_images,
    render_browser,
    render_cycles,
)
from blender_vision.v2.authority import AuthorityClass  # noqa: E402
from blender_vision.v2.records import MaterialHypothesis  # noqa: E402

DELTA_E_LIMIT = 8.0
STRUCTURAL_LIMIT = 0.15
MIN_PASSING = 5

# Plausible non-matches under a microfacet GGX vs Cycles Principled: transmission
# and strong anisotropy are not modelled in the browser probe.
NAMED_EXCEPTION_HINTS = {
    "glass": "transmission/refraction not modelled in the browser GGX probe",
    "hair_fur": "anisotropic fur BSDF not modelled in the isotropic GGX probe",
}


def _load_benchmark_materials() -> list[dict]:
    path = _ROOT / "benchmarks" / "appearance_v2" / "materials.json"
    payload = json.loads(path.read_text(encoding="utf-8"))
    return list(payload["materials"])


def _hypothesis_from_bench(entry: dict) -> MaterialHypothesis:
    return MaterialHypothesis(
        hypothesis_id=str(entry["id"]),
        label=str(entry.get("label", entry["id"])),
        base_colour=list(entry["base_colour"]),
        roughness=float(entry["roughness"]),
        metalness=float(entry["metalness"]),
        specular_ior=float(entry.get("specular_ior", 1.45)),
        anisotropy=float(entry.get("anisotropy", 0.0)),
        transmission=float(entry.get("transmission", 0.0)),
        subsurface=float(entry.get("subsurface", 0.0)),
        texture_scale_m=float(entry.get("texture_scale_m", 0.01)),
        authority=AuthorityClass.INFERRED,
    )


def _fmt(metrics) -> str:
    return (
        f"dE={metrics.delta_e2000:6.3f}  "
        f"struct={metrics.structural:6.4f}  "
        f"mae={metrics.mean_abs_error:7.4f}"
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=_ROOT / "artifacts" / "v2" / "appearance" / "parity-check",
    )
    parser.add_argument("--size", type=int, default=128)
    parser.add_argument("--samples", type=int, default=64)
    args = parser.parse_args()

    out: Path = args.output
    out.mkdir(parents=True, exist_ok=True)
    rig = DEFAULT_PROBE_RIG.with_resolution(args.size)

    print("=== ProbeRig (shared) ===")
    print(json.dumps(rig.to_dict(), indent=2, sort_keys=True))
    (out / "probe_rig.json").write_text(
        json.dumps(rig.to_dict(), indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )

    ok_b, reason_b = blender_probe()
    if not ok_b:
        print(f"BLOCKED Cycles: {reason_b}")
        (out / "blocked.json").write_text(
            json.dumps({"cycles": reason_b, "browser": None}, indent=2) + "\n",
            encoding="utf-8",
        )
        return 2

    materials = _load_benchmark_materials()
    rows: list[dict] = []
    print()
    print(
        f"{'material':24s}  {'passed':6s}  {'dE2000':>8s}  {'struct':>8s}  {'mae':>8s}  notes"
    )
    print("-" * 90)

    for entry in materials:
        hyp = _hypothesis_from_bench(entry)
        mat_dir = out / hyp.hypothesis_id
        mat_dir.mkdir(parents=True, exist_ok=True)
        try:
            cycles_path = render_cycles(
                hyp,
                size=args.size,
                output_path=mat_dir / "cycles.png",
                samples=args.samples,
                rig=rig,
            )
            browser_path = render_browser(
                hyp,
                size=args.size,
                output_path=mat_dir / "browser.png",
                rig=rig,
            )
            metrics = compare_images(_load_rgb(cycles_path), _load_rgb(browser_path))
            passed = (
                metrics.delta_e2000 <= DELTA_E_LIMIT
                and metrics.structural <= STRUCTURAL_LIMIT
            )
            hint = NAMED_EXCEPTION_HINTS.get(hyp.hypothesis_id, "")
            note = ""
            if not passed and hint:
                note = f"NAMED EXCEPTION: {hint}"
            elif not passed:
                note = "FAIL (no named exception)"
            elif hint:
                note = f"pass (was candidate exception: {hint})"
            else:
                note = "pass"
            row = {
                "id": hyp.hypothesis_id,
                "passed": passed,
                "delta_e2000": metrics.delta_e2000,
                "structural": metrics.structural,
                "mean_abs_error": metrics.mean_abs_error,
                "note": note,
            }
            rows.append(row)
            print(
                f"{hyp.hypothesis_id:24s}  {str(passed):6s}  "
                f"{metrics.delta_e2000:8.3f}  {metrics.structural:8.4f}  "
                f"{metrics.mean_abs_error:8.4f}  {note}"
            )
        except (BackendUnavailable, BrowserBusyError) as error:
            print(f"{hyp.hypothesis_id:24s}  BLOCKED  {error}")
            (out / "blocked.json").write_text(
                json.dumps({"material": hyp.hypothesis_id, "error": str(error)}, indent=2)
                + "\n",
                encoding="utf-8",
            )
            return 2

    n_pass = sum(1 for r in rows if r["passed"])
    exceptions = [r for r in rows if not r["passed"] and r["id"] in NAMED_EXCEPTION_HINTS]
    unexplained = [r for r in rows if not r["passed"] and r["id"] not in NAMED_EXCEPTION_HINTS]

    print()
    print(f"Matched pass count: {n_pass} / {len(rows)} (need ≥ {MIN_PASSING})")
    if exceptions:
        print("Named exceptions:")
        for r in exceptions:
            print(f"  - {r['id']}: {_fmt_row(r)} — {NAMED_EXCEPTION_HINTS[r['id']]}")

    # Deliberately wrong browser material (anodized reference, browser perturbed).
    print()
    print("=== Deliberately wrong browser material ===")
    ref_entry = next(m for m in materials if m["id"] == "anodized_metal")
    ref_h = _hypothesis_from_bench(ref_entry)
    wrong_dir = out / "deliberately_wrong"
    wrong_dir.mkdir(parents=True, exist_ok=True)
    try:
        cycles_path = render_cycles(
            ref_h,
            size=args.size,
            output_path=wrong_dir / "cycles.png",
            samples=args.samples,
            rig=rig,
        )
        browser_path = render_browser(
            ref_h,
            size=args.size,
            output_path=wrong_dir / "browser_wrong.png",
            force_wrong=True,
            rig=rig,
        )
        wrong_metrics = compare_images(_load_rgb(cycles_path), _load_rgb(browser_path))
        wrong_pass = (
            wrong_metrics.delta_e2000 <= DELTA_E_LIMIT
            and wrong_metrics.structural <= STRUCTURAL_LIMIT
        )
        print(f"anodized_metal browser_force_wrong  passed={wrong_pass}  {_fmt(wrong_metrics)}")
        print(
            f"  limits: dE≤{DELTA_E_LIMIT}, structural≤{STRUCTURAL_LIMIT}  "
            f"→ gate {'MISSED (bad)' if wrong_pass else 'FIRED (good)'}"
        )
    except (BackendUnavailable, BrowserBusyError) as error:
        print(f"BLOCKED wrong-material check: {error}")
        return 2

    # Sensitivity: Cycles fixed (matte plastic), browser roughness sweep.
    print()
    print("=== Sensitivity sweep (Cycles fixed, browser roughness 0.1→0.9) ===")
    base_entry = next(m for m in materials if m["id"] == "matte_plastic")
    base_h = _hypothesis_from_bench(base_entry)
    sens_dir = out / "sensitivity"
    sens_dir.mkdir(parents=True, exist_ok=True)
    try:
        cycles_path = render_cycles(
            base_h,
            size=args.size,
            output_path=sens_dir / "cycles_fixed.png",
            samples=args.samples,
            rig=rig,
        )
        cycles_img = _load_rgb(cycles_path)
        sweep: list[dict] = []
        print(f"{'rough':>7s}  {'dE2000':>8s}  {'struct':>8s}  {'mae':>8s}  gate")
        for rough in [0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9]:
            hyp = MaterialHypothesis(
                hypothesis_id=f"sweep-{rough:.1f}",
                label=base_h.label,
                base_colour=list(base_h.base_colour),
                roughness=float(rough),
                metalness=float(base_h.metalness),
                specular_ior=base_h.specular_ior,
                authority=AuthorityClass.INFERRED,
            )
            bpath = render_browser(
                hyp,
                size=args.size,
                output_path=sens_dir / f"browser_r{rough:.1f}.png",
                rig=rig,
            )
            m = compare_images(cycles_img, _load_rgb(bpath))
            gate = (
                "pass"
                if m.delta_e2000 <= DELTA_E_LIMIT and m.structural <= STRUCTURAL_LIMIT
                else "fail"
            )
            print(
                f"{rough:7.1f}  {m.delta_e2000:8.3f}  {m.structural:8.4f}  "
                f"{m.mean_abs_error:8.4f}  {gate}"
            )
            sweep.append(
                {
                    "roughness": rough,
                    "delta_e2000": m.delta_e2000,
                    "structural": m.structural,
                    "mean_abs_error": m.mean_abs_error,
                    "gate": gate,
                }
            )
    except (BackendUnavailable, BrowserBusyError) as error:
        print(f"BLOCKED sensitivity sweep: {error}")
        return 2

    summary = {
        "delta_e_limit": DELTA_E_LIMIT,
        "structural_limit": STRUCTURAL_LIMIT,
        "min_passing": MIN_PASSING,
        "n_pass": n_pass,
        "n_total": len(rows),
        "rows": rows,
        "named_exceptions": exceptions,
        "unexplained_failures": unexplained,
        "wrong_material": {
            "passed": wrong_pass,
            "metrics": wrong_metrics.to_dict(),
        },
        "sensitivity": sweep,
        "probe_rig": rig.to_dict(),
    }
    (out / "discrimination_report.json").write_text(
        json.dumps(summary, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )

    print()
    exit_ok = n_pass >= MIN_PASSING and not wrong_pass and not unexplained
    if unexplained:
        print("FAIL: unexplained material failures (not named exceptions):")
        for r in unexplained:
            print(f"  - {r['id']}: dE={r['delta_e2000']:.3f} struct={r['structural']:.4f}")
    if n_pass < MIN_PASSING:
        print(f"FAIL: only {n_pass} materials passed (need ≥ {MIN_PASSING})")
    if wrong_pass:
        print("FAIL: deliberately wrong browser material incorrectly passed the gate")
    if exit_ok:
        print("OK: gate discriminates (matched pass, wrong fails, sweep recorded)")
        return 0
    return 1


def _fmt_row(row: dict) -> str:
    return (
        f"dE={row['delta_e2000']:.3f} struct={row['structural']:.4f} "
        f"mae={row['mean_abs_error']:.4f}"
    )


if __name__ == "__main__":
    raise SystemExit(main())
