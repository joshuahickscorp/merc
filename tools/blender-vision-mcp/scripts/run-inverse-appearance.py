#!/usr/bin/env python3
"""Run the V2 inverse materials/lighting appearance benchmark end-to-end.

Produces recovery-error tables, parity matrices, lighting recovery, and critic
catch matrices under --output. Exit non-zero if any critic misses its injected
failure.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image

# Allow running without install when executed from the package tree.
_ROOT = Path(__file__).resolve().parents[1]
_SRC = _ROOT / "src"
if str(_SRC) not in sys.path:
    sys.path.insert(0, str(_SRC))

from blender_vision.lighting.critic import (  # noqa: E402
    LIGHTING_CRITICS,
    inject_lighting_failure,
    run_lighting_critics,
)
from blender_vision.lighting.rigs import RIG_NAMES, apply_rig_script, get_rig  # noqa: E402
from blender_vision.lighting.solve import (  # noqa: E402
    GeometryContext,
    LightingObservation,
    angular_error_deg,
    solve_lighting,
)
from blender_vision.materials.critic import (  # noqa: E402
    MATERIAL_CRITICS,
    inject_material_failure,
    run_material_critics,
)
from blender_vision.materials.inverse import (  # noqa: E402
    SurfaceObservation,
    SurfaceRegion,
    infer_materials,
)
from blender_vision.materials.parity import run_parity  # noqa: E402
from blender_vision.v2.authority import AuthorityClass  # noqa: E402
from blender_vision.v2.records import MaterialHypothesis  # noqa: E402


def _load_benchmark() -> dict[str, Any]:
    path = _ROOT / "benchmarks" / "appearance_v2" / "materials.json"
    return json.loads(path.read_text(encoding="utf-8"))


def _blender() -> str:
    env = os.environ.get("BVMCP_BLENDER")
    if env and Path(env).is_file():
        return env
    candidate = Path("/Applications/Blender.app/Contents/MacOS/Blender")
    if candidate.is_file():
        return str(candidate)
    raise SystemExit("Blender not found; set BVMCP_BLENDER")


def _gt_hypothesis(spec: dict[str, Any]) -> MaterialHypothesis:
    return MaterialHypothesis(
        hypothesis_id=spec["id"],
        label=spec["label"],
        base_colour=list(spec["base_colour"]),
        roughness=float(spec["roughness"]),
        metalness=float(spec["metalness"]),
        specular_ior=float(spec.get("specular_ior", 1.45)),
        anisotropy=float(spec.get("anisotropy", 0.0)),
        transmission=float(spec.get("transmission", 0.0)),
        subsurface=float(spec.get("subsurface", 0.0)),
        texture_scale_m=float(spec.get("texture_scale_m", 0.01)),
        confidence=1.0,
        authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
    )


def _render_probe_blender(
    material: dict[str, Any],
    rig_name: str,
    output_path: Path,
    *,
    size: int = 128,
    samples: int = 24,
) -> Path:
    blender = _blender()
    rig_body = apply_rig_script(rig_name)
    script = f"""
import bpy, math, json
from mathutils import Euler

bpy.ops.wm.read_factory_settings(use_empty=True)
scene = bpy.context.scene
scene.render.engine = "CYCLES"
scene.cycles.samples = {int(samples)}
scene.cycles.use_denoising = False
try:
    scene.cycles.device = "CPU"
except Exception:
    pass
scene.render.resolution_x = {int(size)}
scene.render.resolution_y = {int(size)}
scene.render.filepath = {json.dumps(str(output_path))}
scene.render.image_settings.file_format = "PNG"

# Lighting rig (real lights + world nodes)
{rig_body}

bpy.ops.mesh.primitive_uv_sphere_add(segments=48, ring_count=24, radius=1.0)
sphere = bpy.context.active_object
bpy.ops.object.shade_smooth()
# Ground plane for contact shadows
bpy.ops.mesh.primitive_plane_add(size=6.0, location=(0.0, 0.0, -1.0))
ground = bpy.context.active_object
gmat = bpy.data.materials.new("Ground")
gmat.use_nodes = True
gbsdf = gmat.node_tree.nodes.get("Principled BSDF")
if gbsdf:
    gbsdf.inputs["Base Color"].default_value = (0.18, 0.18, 0.2, 1.0)
    gbsdf.inputs["Roughness"].default_value = 0.7
ground.data.materials.append(gmat)

mat = bpy.data.materials.new({json.dumps(material["id"])})
mat.use_nodes = True
nodes = mat.node_tree.nodes
links = mat.node_tree.links
nodes.clear()
out = nodes.new("ShaderNodeOutputMaterial")
bsdf = nodes.new("ShaderNodeBsdfPrincipled")
bc = {material["base_colour"]!r}
bsdf.inputs["Base Color"].default_value = (bc[0], bc[1], bc[2], 1.0)
bsdf.inputs["Roughness"].default_value = {float(material["roughness"])}
bsdf.inputs["Metallic"].default_value = {float(material["metalness"])}
if "IOR" in bsdf.inputs:
    bsdf.inputs["IOR"].default_value = {float(material.get("specular_ior", 1.45))}
if "Anisotropic" in bsdf.inputs:
    bsdf.inputs["Anisotropic"].default_value = {float(material.get("anisotropy", 0.0))}
if "Transmission Weight" in bsdf.inputs:
    bsdf.inputs["Transmission Weight"].default_value = {float(material.get("transmission", 0.0))}
elif "Transmission" in bsdf.inputs:
    bsdf.inputs["Transmission"].default_value = {float(material.get("transmission", 0.0))}
if "Subsurface Weight" in bsdf.inputs:
    bsdf.inputs["Subsurface Weight"].default_value = {float(material.get("subsurface", 0.0))}
links.new(bsdf.outputs["BSDF"], out.inputs["Surface"])
sphere.data.materials.append(mat)

bpy.ops.object.camera_add(location=(0.0, -3.4, 1.0))
cam = bpy.context.active_object
cam.rotation_euler = (math.radians(74), 0.0, 0.0)
scene.camera = cam

bpy.ops.render.render(write_still=True)
"""
    output_path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile("w", suffix=".py", delete=False) as handle:
        handle.write(script)
        script_path = handle.name
    try:
        completed = subprocess.run(
            [
                blender,
                "--background",
                "--factory-startup",
                "--python-exit-code",
                "1",
                "--python",
                script_path,
            ],
            capture_output=True,
            text=True,
            timeout=240,
            check=False,
        )
        if completed.returncode != 0 or not output_path.is_file():
            raise RuntimeError(
                f"Blender render failed for {material['id']}/{rig_name}: "
                + (completed.stderr or completed.stdout or "")[:1200]
            )
        return output_path
    finally:
        Path(script_path).unlink(missing_ok=True)


def _synthetic_render(material: dict[str, Any], rig_name: str, size: int = 128) -> np.ndarray:
    """Fallback offline GGX sphere when Blender is unavailable."""
    rig = get_rig(rig_name)
    key_loc = np.array(rig.key.location, dtype=np.float64)
    light = key_loc / (np.linalg.norm(key_loc) + 1e-8)
    # Flip to incoming light direction-ish
    light = np.array([light[0], light[2], -light[1]])
    light = light / (np.linalg.norm(light) + 1e-8)
    y, x = np.mgrid[0:size, 0:size].astype(np.float64)
    nx = (x + 0.5) / size * 2.0 - 1.0
    ny = 1.0 - (y + 0.5) / size * 2.0
    r2 = nx * nx + ny * ny
    mask = r2 <= 1.0
    nz = np.sqrt(np.clip(1.0 - r2, 0.0, 1.0))
    normal = np.stack([nx, ny, nz], axis=-1)
    view = np.array([0.0, 0.0, 1.0])
    half = light + view
    half = half / (np.linalg.norm(half) + 1e-8)
    ndl = np.clip(normal @ light, 0.0, 1.0)
    ndh = np.clip(normal @ half, 0.0, 1.0)
    alpha = max(0.02, float(material["roughness"])) ** 2
    denom = (ndh * ndh * (alpha - 1.0) + 1.0) ** 2
    d = alpha / (math.pi * denom + 1e-8)
    base = np.array(material["base_colour"], dtype=np.float64)
    metal = float(material["metalness"])
    diffuse = base * (1.0 - metal) / math.pi
    f0 = base * metal + (1.0 - metal) * 0.04
    specular = f0[None, None, :] * d[..., None]
    ambient = 0.05 + 0.15 * rig.environment_strength
    colour = ambient * base + ndl[..., None] * (diffuse + specular)
    colour = np.clip(colour * (2.0 ** rig.exposure), 0.0, 1.0)
    colour[~mask] = 0.1
    return colour


def _load_rgb(path: Path) -> np.ndarray:
    return np.asarray(Image.open(path).convert("RGB"), dtype=np.float64) / 255.0


def _sphere_mask(size: int) -> np.ndarray:
    y, x = np.mgrid[0:size, 0:size]
    nx = (x + 0.5) / size * 2.0 - 1.0
    ny = 1.0 - (y + 0.5) / size * 2.0
    return (nx * nx + ny * ny) <= 1.0


def _print_table(headers: list[str], rows: list[list[Any]]) -> None:
    widths = [len(h) for h in headers]
    for row in rows:
        for i, cell in enumerate(row):
            widths[i] = max(widths[i], len(str(cell)))
    fmt = "  ".join(f"{{:{w}}}" for w in widths)
    print(fmt.format(*headers))
    print(fmt.format(*("-" * w for w in widths)))
    for row in rows:
        print(fmt.format(*[str(c) for c in row]))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=Path("artifacts/v2/appearance"),
        help="Output directory for renders and reports",
    )
    parser.add_argument(
        "--size",
        type=int,
        default=96,
        help="Render resolution (square)",
    )
    parser.add_argument(
        "--samples",
        type=int,
        default=16,
        help="Cycles samples when Blender is used",
    )
    parser.add_argument(
        "--offline-only",
        action="store_true",
        help="Skip Blender/browser even if available",
    )
    args = parser.parse_args()
    out = args.output
    if not out.is_absolute():
        out = (_ROOT / out).resolve()
    out.mkdir(parents=True, exist_ok=True)

    bench = _load_benchmark()
    materials = bench["materials"]
    rigs = list(bench.get("rigs") or RIG_NAMES)
    use_blender = not args.offline_only
    if use_blender:
        try:
            _blender()
            from blender_vision.materials.parity import blender_probe

            ok, reason = blender_probe()
            if not ok:
                use_blender = False
                print(f"WARN: Blender blocked — {reason}")
                print("WARN: using offline synthetic renders.")
        except SystemExit:
            use_blender = False
            print("WARN: Blender unavailable — using offline synthetic renders.")

    # ------------------------------------------------------------------ renders
    print("\n=== 1. Probe renders (9 materials × 4 rigs) ===")
    render_index: dict[str, dict[str, Path | np.ndarray]] = {}
    for mat in materials:
        render_index[mat["id"]] = {}
        for rig in rigs:
            target = out / "renders" / mat["id"] / f"{rig}.png"
            if use_blender:
                try:
                    path = _render_probe_blender(
                        mat, rig, target, size=args.size, samples=args.samples
                    )
                    render_index[mat["id"]][rig] = path
                    print(f"  rendered {mat['id']} / {rig} -> {path}")
                    continue
                except Exception as error:  # noqa: BLE001 — report and fall back
                    print(f"  WARN Blender failed {mat['id']}/{rig}: {error}")
            rgb = _synthetic_render(mat, rig, size=args.size)
            target.parent.mkdir(parents=True, exist_ok=True)
            Image.fromarray((rgb * 255).astype(np.uint8), mode="RGB").save(target)
            render_index[mat["id"]][rig] = target
            print(f"  synthetic {mat['id']} / {rig} -> {target}")

    # ------------------------------------------------------- material recovery
    print("\n=== 2. Material recovery vs ground truth ===")
    recovery_rows: list[list[Any]] = []
    recovery_payload: list[dict[str, Any]] = []
    for mat in materials:
        # Use neutral_documentation multi-view: primary + slight synthetic second view.
        paths = render_index[mat["id"]]
        primary = paths[rigs[0]]
        rgb = _load_rgb(primary) if isinstance(primary, Path) else primary
        mask = _sphere_mask(rgb.shape[0])
        lum = 0.2126 * rgb[..., 0] + 0.7152 * rgb[..., 1] + 0.0722 * rgb[..., 2]
        highlight = mask & (lum >= np.percentile(lum[mask], 92))
        obs = [
            SurfaceObservation(
                view_id=f"{mat['id']}-{rigs[0]}",
                rgb=rgb,
                mask=mask,
                highlight_mask=highlight,
                authority=AuthorityClass.OBSERVED,
            )
        ]
        # Second view from another rig if available.
        if len(rigs) > 1:
            secondary = paths[rigs[1]]
            rgb2 = _load_rgb(secondary) if isinstance(secondary, Path) else secondary
            lum2 = 0.2126 * rgb2[..., 0] + 0.7152 * rgb2[..., 1] + 0.0722 * rgb2[..., 2]
            obs.append(
                SurfaceObservation(
                    view_id=f"{mat['id']}-{rigs[1]}",
                    rgb=rgb2,
                    mask=mask,
                    highlight_mask=mask & (lum2 >= np.percentile(lum2[mask], 92)),
                    authority=AuthorityClass.OBSERVED,
                )
            )
        surface = SurfaceRegion(surface_id=mat["id"], label=mat["label"])
        result = infer_materials(obs, [surface])
        selected = next(
            item
            for item in result.hypotheses
            if item.hypothesis_id == result.selected_hypothesis_id
        )
        gt = _gt_hypothesis(mat)
        rough_err = abs(selected.roughness - gt.roughness)
        metal_err = abs(selected.metalness - gt.metalness)
        colour_err = float(
            np.mean(np.abs(np.array(selected.base_colour[:3]) - np.array(gt.base_colour[:3])))
        )
        recovery_rows.append(
            [
                mat["id"],
                f"{selected.roughness:.3f}",
                f"{gt.roughness:.3f}",
                f"{rough_err:.3f}",
                f"{selected.metalness:.3f}",
                f"{gt.metalness:.3f}",
                f"{metal_err:.3f}",
                f"{colour_err:.3f}",
            ]
        )
        recovery_payload.append(
            {
                "material_id": mat["id"],
                "recovered": {
                    "roughness": selected.roughness,
                    "metalness": selected.metalness,
                    "base_colour": selected.base_colour,
                },
                "ground_truth": {
                    "roughness": gt.roughness,
                    "metalness": gt.metalness,
                    "base_colour": gt.base_colour,
                },
                "errors": {
                    "roughness": rough_err,
                    "metalness": metal_err,
                    "base_colour_mae": colour_err,
                },
                "hypothesis_count": len(result.hypotheses),
            }
        )
    _print_table(
        [
            "material",
            "r_hat",
            "r_gt",
            "r_err",
            "m_hat",
            "m_gt",
            "m_err",
            "c_err",
        ],
        recovery_rows,
    )
    (out / "material_recovery.json").write_text(
        json.dumps(recovery_payload, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )

    # ------------------------------------------------------------------- parity
    print("\n=== 3. Parity harness (ΔE2000 / structural / browser gate) ===")
    run_cycles = use_blender and os.environ.get("BVMCP_RUN_BLENDER_TESTS", "1") != "0"
    run_browser = (not args.offline_only) and os.environ.get("BVMCP_RUN_BROWSER_TESTS", "1") != "0"
    # Default: try browser if playwright works.
    if run_browser:
        try:
            from playwright.sync_api import sync_playwright  # noqa: F401
        except ImportError:
            run_browser = False
            print("WARN: playwright missing — browser parity blocked.")

    parity_rows: list[list[Any]] = []
    parity_payload: list[dict[str, Any]] = []
    for mat in materials:
        hyp = _gt_hypothesis(mat)
        report = run_parity(
            hyp,
            output_dir=out / "parity" / mat["id"],
            size=min(args.size, 96),
            run_cycles=run_cycles,
            run_browser=run_browser,
        )
        by_target = {item.target.value: item for item in report.results}

        def _metric(name: str, targets: dict[str, Any] = by_target) -> str:
            item = targets.get(name)
            if item is None or item.metrics_vs_reference is None:
                if item and item.blocked:
                    return f"blocked:{item.block_reason[:24]}"
                return "ref"
            m = item.metrics_vs_reference
            return f"dE={m.delta_e2000:.2f}/s={m.structural:.3f}"

        gate = "FAIL" if report.browser_gate_failed else (
            "PASS" if report.overall_passed else "MIXED"
        )
        parity_rows.append(
            [
                mat["id"],
                _metric("poster"),
                _metric("cycles"),
                _metric("browser"),
                _metric("mobile_lod"),
                gate,
            ]
        )
        parity_payload.append(report.to_dict())
        print(
            f"  {mat['id']}: browser_gate_failed={report.browser_gate_failed} "
            f"overall={report.overall_passed}"
        )
    _print_table(
        ["material", "poster", "cycles", "browser", "mobile_lod", "gate"],
        parity_rows,
    )
    (out / "parity_matrix.json").write_text(
        json.dumps(parity_payload, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )

    # ---------------------------------------------------------- lighting solve
    print("\n=== 4. Lighting recovery (known key direction) ===")
    true_dir = np.array([0.45, 0.55, 0.70], dtype=np.float64)
    true_dir = true_dir / np.linalg.norm(true_dir)
    # Analytic shaded sphere as "render from known rig"
    size = args.size
    y, x = np.mgrid[0:size, 0:size].astype(np.float64)
    nx = (x + 0.5) / size * 2.0 - 1.0
    ny = 1.0 - (y + 0.5) / size * 2.0
    r2 = nx * nx + ny * ny
    mask = r2 <= 1.0
    nz = np.sqrt(np.clip(1.0 - r2, 0.0, 1.0))
    normals = np.stack([nx, ny, nz], axis=-1)
    ndl = np.clip(normals @ true_dir, 0.0, 1.0)
    half = true_dir + np.array([0.0, 0.0, 1.0])
    half = half / (np.linalg.norm(half) + 1e-8)
    ndh = np.clip(normals @ half, 0.0, 1.0)
    rgb = np.zeros((size, size, 3))
    rgb[mask] = 0.5 * (0.15 + 0.85 * ndl[mask, None])
    rgb[mask] = np.clip(rgb[mask] + 0.4 * (ndh[mask, None] ** 50), 0.0, 1.0)
    light_path = out / "lighting" / "known_key.png"
    light_path.parent.mkdir(parents=True, exist_ok=True)
    Image.fromarray((rgb * 255).astype(np.uint8)).save(light_path)
    lighting = solve_lighting(
        [
            LightingObservation(
                view_id="known-key",
                rgb=rgb,
                mask=mask,
                normals=normals,
                authority=AuthorityClass.OBSERVED,
            )
        ],
        GeometryContext(
            scene_id="lighting-probe",
            authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
        ),
    )
    selected_l = next(
        item
        for item in lighting.hypotheses
        if item.hypothesis_id == lighting.selected_hypothesis_id
    )
    est_dir = selected_l.key["direction"]
    dir_err = angular_error_deg(est_dir, true_dir)
    true_intensity = 1.2
    est_intensity = float(selected_l.key.get("intensity", 0.0))
    intensity_err = abs(est_intensity - true_intensity)
    true_k = 6500.0
    kelvin_err = abs(selected_l.white_balance_k - true_k)
    print(f"  true key direction: {true_dir.tolist()}")
    print(f"  recovered direction: {est_dir}")
    print(f"  direction error (deg): {dir_err:.3f}")
    print(
        f"  intensity error: {intensity_err:.3f} "
        f"(est={est_intensity:.3f}, true~{true_intensity})"
    )
    print(f"  white-balance error (K): {kelvin_err:.1f}")
    lighting_report = {
        "true_direction": true_dir.tolist(),
        "recovered_direction": est_dir,
        "direction_error_deg": dir_err,
        "intensity_error": intensity_err,
        "white_balance_error_k": kelvin_err,
        "hypothesis": selected_l.to_dict(),
    }
    (out / "lighting_recovery.json").write_text(
        json.dumps(lighting_report, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )

    # ---------------------------------------------------------- critic matrix
    print("\n=== 5. Critic catch matrix (9 material + 7 lighting) ===")
    catch_rows: list[list[Any]] = []
    failures = 0
    base_mat = MaterialHypothesis(
        hypothesis_id="critic-base",
        label="generic",
        base_colour=[0.5, 0.5, 0.5],
        roughness=0.4,
        metalness=0.0,
        authority=AuthorityClass.INFERRED,
    )
    for name in sorted(MATERIAL_CRITICS):
        ctx = inject_material_failure(name, base_mat)
        critique = run_material_critics(ctx)
        caught = bool(critique.findings)
        if not caught:
            failures += 1
        catch_rows.append(["material", name, "CATCH" if caught else "MISS", len(critique.findings)])
    for name in sorted(LIGHTING_CRITICS):
        ctx = inject_lighting_failure(name)
        critique = run_lighting_critics(ctx)
        caught = bool(critique.findings)
        if not caught:
            failures += 1
        catch_rows.append(["lighting", name, "CATCH" if caught else "MISS", len(critique.findings)])
    _print_table(["domain", "failure", "result", "findings"], catch_rows)
    catch_payload = [
        {"domain": r[0], "failure": r[1], "result": r[2], "findings": r[3]} for r in catch_rows
    ]
    (out / "critic_catch_matrix.json").write_text(
        json.dumps(catch_payload, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )

    # Rig script evidence
    rig_evidence = {name: get_rig(name).to_hypothesis_fields() for name in RIG_NAMES}
    (out / "lighting_rigs.json").write_text(
        json.dumps(rig_evidence, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )

    summary = {
        "materials": len(materials),
        "rigs": list(rigs),
        "used_blender": use_blender,
        "used_browser": run_browser,
        "critic_misses": failures,
        "direction_error_deg": dir_err,
    }
    (out / "summary.json").write_text(
        json.dumps(summary, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    print(f"\n=== Summary ===\n{json.dumps(summary, indent=2)}")
    if failures:
        print(f"FAIL: {failures} critic(s) missed injected failures", file=sys.stderr)
        return 1
    print("OK: all injected critic failures caught")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
