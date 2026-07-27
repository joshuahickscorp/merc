#!/usr/bin/env python3
"""Real execution driver for the VisionMCP V2 procedural world engine.

Builds every data-centre archetype through headless Blender, asserts declared
vs measured dimensions within 1 mm, constructs the flagship threshold→turn→
terminal scene, exports editable .blend + GLB, renders four viewpoints, runs
LOD identity checks, and writes a sealed ProceduralSceneGraph record.
"""

from __future__ import annotations

import argparse
import json
import sys
import traceback
from pathlib import Path

# Allow running from a source checkout without install when needed.
_ROOT = Path(__file__).resolve().parents[1]
_SRC = _ROOT / "src"
if _SRC.is_dir() and str(_SRC) not in sys.path:
    sys.path.insert(0, str(_SRC))


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=_ROOT / "artifacts" / "v2" / "procedural",
        help="Output directory for blends, GLBs, renders, and records",
    )
    parser.add_argument(
        "--skip-blender",
        action="store_true",
        help="Only run CPU-side checks (not accepted for completion evidence)",
    )
    args = parser.parse_args(argv)
    output: Path = args.output
    output.mkdir(parents=True, exist_ok=True)

    from blender_vision.procedural.library import default_library, list_archetypes
    from blender_vision.procedural.lod import check_lod_identity, generate_lods
    from blender_vision.procedural.scene import build_flagship_scene
    from blender_vision.procedural.standards import U_HEIGHT_M, rack_height_m
    from blender_vision.v2.validation import verify_payload, write_record

    library = default_library()
    names = list_archetypes()
    print("=" * 72)
    print("VisionMCP V2 Procedural World Engine")
    print("=" * 72)
    print(f"output: {output}")
    print(f"archetypes: {len(names)}")
    print(f"1U = {U_HEIGHT_M * 1000:.2f} mm; 42U height = {rack_height_m(42) * 1000:.2f} mm")
    print()

    failures: list[str] = []
    archetype_report: dict[str, object] = {}

    # ------------------------------------------------------------------ CPU
    print("--- CPU: dimension + LOD identity ---")
    for name in names:
        arch = library.create(name)
        try:
            arch.assert_dimensions(tolerance_m=0.001)
            deltas = arch.dimension_deltas_m()
        except Exception as error:  # noqa: BLE001 — report and continue
            failures.append(f"{name}: dimension assert failed: {error}")
            deltas = {}
            print(f"  FAIL {name}: {error}")
            continue
        variants = generate_lods(arch.build())
        lod_report = check_lod_identity(name, variants, require_all_parts=False)
        if not lod_report.bbox_ok or not lod_report.silhouette_ok:
            failures.append(f"{name}: LOD identity bbox/silhouette failed: {lod_report.notes}")
        part_loss = lod_report.lost_parts
        archetype_report[name] = {
            "declared_m": arch.declared_dimensions().to_dict(),
            "measured_m": arch.measured_dimensions().to_dict(),
            "deltas_m": deltas,
            "deltas_mm": {k: v * 1000.0 for k, v in deltas.items()},
            "part_count": len(arch.part_names()),
            "lod_identity": lod_report.to_dict(),
            "lod_triangle_estimates": {
                level: {
                    "parts": len(variant.part_names),
                    "budget": variant.triangle_budget,
                }
                for level, variant in variants.items()
            },
        }
        loss_note = f" part_loss={part_loss}" if part_loss else ""
        print(
            f"  OK {name}: Δmm="
            f"W{deltas['width_m']*1000:+.3f} "
            f"D{deltas['depth_m']*1000:+.3f} "
            f"H{deltas['height_m']*1000:+.3f}"
            f"{loss_note}"
        )

    # Demonstrate LOD identity catching a regression.
    from blender_vision.procedural.archetype import measure_parts_bounds
    from blender_vision.procedural.lod import intentionally_broken_far_lod, part_name_set

    probe = library.create("server_drawer", {"u_height": 2})
    variants = generate_lods(probe.build())
    broken = intentionally_broken_far_lod(probe.build())
    variants["far"].parts = broken
    variants["far"].dimensions = measure_parts_bounds(broken)
    variants["far"].part_names = part_name_set(broken)
    broken_report = check_lod_identity(
        "server_drawer", variants, require_all_parts=True, bbox_tolerance_m=0.02
    )
    print()
    print("--- LOD identity regression probe (intentionally broken far) ---")
    print(f"  passed={broken_report.passed} lost_parts={broken_report.lost_parts}")
    print(f"  notes={broken_report.notes}")
    if broken_report.passed:
        failures.append("LOD identity failed to catch intentionally broken far LOD")

    # ------------------------------------------------------------------ Scene
    print()
    print("--- Scene: threshold → aisle → junction → aisle → terminal ---")
    compiled = build_flagship_scene(
        aisle_length_m=12.0,
        rack_count_per_side=12,
        rack_pitch_m=0.6,
        aisle_width_m=1.2,
    )
    record_path = write_record(output / "procedural_scene_graph.json", compiled.record)
    verified = verify_payload(json.loads(record_path.read_text(encoding="utf-8")))
    print(f"  instances: {compiled.plan.instance_count()}")
    print(f"  unique meshes: {compiled.plan.unique_mesh_count()}")
    print(f"  modules: {len(compiled.modules)}")
    print(f"  bounds_m: {compiled.bounds_m}")
    print(f"  record: {record_path} digest={verified.digest[:16]}…")
    print(f"  record authority: {compiled.record.authority.value}")

    library_manifest = library.manifest()
    manifest_path = output / "archetype_manifest.json"
    manifest_path.write_text(
        json.dumps(library_manifest.to_dict(), indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    print(f"  manifest: {manifest_path}")

    blender_summary: dict[str, object] = {}
    blender_blocked = False
    blender_block_reason = ""
    if args.skip_blender:
        print()
        print("SKIP Blender emission (--skip-blender)")
    else:
        from blender_vision.procedural.emit import emit_archetype, emit_scene_plan
        from blender_vision.procedural.mesh_offline import blender_probe

        probe = blender_probe()
        print()
        print("--- Backend probe ---")
        print(f"  blender_blocked={probe.get('blocked')} path={probe.get('path')}")
        if probe.get("blocked"):
            blender_blocked = True
            blender_block_reason = str(probe.get("reason", "unknown"))
            print(f"  BLOCKED: {blender_block_reason}")
            print("  Falling back to trimesh-offline for real GLB/metrics (not a silent stub).")
            print("  Editable .blend and EEVEE/Cycles renders remain unavailable.")

        print()
        print("--- Geometry emit: per-archetype build + GLB ---")
        arch_dir = output / "archetypes"
        arch_dir.mkdir(parents=True, exist_ok=True)
        for name in names:
            dest = arch_dir / name
            try:
                result = emit_archetype(name, dest, timeout_seconds=300)
                metrics = result.metrics
                if metrics.get("blender_status") == "BLOCKED":
                    blender_blocked = True
                    blender_block_reason = str(
                        metrics.get("blocked_reason") or blender_block_reason
                    )
                arch_metrics = metrics.get("archetypes", {})
                lod_rows = {}
                for key, row in arch_metrics.items():
                    lod_rows[key] = {
                        "triangles": row.get("triangle_count"),
                        "bbox_size_m": row.get("bbox_size"),
                        "objects": row.get("object_count"),
                    }
                near_key = f"{name}:near"
                near = arch_metrics.get(near_key)
                if near is None and arch_metrics:
                    near = next(iter(arch_metrics.values()))
                declared = library.create(name).declared_dimensions()
                if near:
                    size = near["bbox_size"]
                    deltas_mm = {
                        "width_mm": (size[0] - declared.width_m) * 1000.0,
                        "depth_mm": (size[1] - declared.depth_m) * 1000.0,
                        "height_mm": (size[2] - declared.height_m) * 1000.0,
                    }
                    for axis, delta in deltas_mm.items():
                        if abs(delta) > 1.0:
                            failures.append(
                                f"{name}: mesh bbox {axis} delta {delta:.3f} mm exceeds 1 mm"
                            )
                else:
                    deltas_mm = {}
                    failures.append(f"{name}: no archetype mesh metrics")
                glb_bytes = (
                    result.glb_path.stat().st_size if result.glb_path.is_file() else 0
                )
                if glb_bytes <= 0:
                    failures.append(f"{name}: GLB missing or empty")
                blend_bytes = int(metrics.get("blend_bytes") or 0)
                backend = metrics.get("backend", "blender")
                blender_summary[name] = {
                    "backend": backend,
                    "blend_bytes": blend_bytes,
                    "glb_bytes": glb_bytes,
                    "glb_path": str(result.glb_path),
                    "lods": lod_rows,
                    "mesh_deltas_mm": deltas_mm,
                    "near_triangles": (near or {}).get("triangle_count"),
                }
                print(
                    f"  OK {name} [{backend}]: "
                    f"tris={blender_summary[name]['near_triangles']} "
                    f"Δmm={deltas_mm} "
                    f"glb={glb_bytes}B"
                )
            except Exception as error:  # noqa: BLE001
                failures.append(f"{name}: emit failed: {error}")
                print(f"  FAIL {name}: {error}")
                traceback.print_exc()

        print()
        print("--- Geometry emit: flagship scene + 4 previews ---")
        scene_dir = output / "flagship_scene"
        renders = [
            {
                "filename": "view_01_threshold.png",
                "location": [0.0, -4.5, 1.7],
                "target": [0.0, 3.0, 1.2],
            },
            {
                "filename": "view_02_aisle.png",
                "location": [0.15, 2.0, 1.5],
                "target": [0.0, 10.0, 1.3],
            },
            {
                "filename": "view_03_junction.png",
                "location": [-3.0, 11.0, 2.5],
                "target": [1.0, 13.5, 1.2],
            },
            {
                "filename": "view_04_terminal.png",
                "location": [4.0, 11.5, 1.8],
                "target": [8.0, 13.2, 1.4],
            },
        ]
        try:
            scene_result = emit_scene_plan(
                compiled.plan,
                scene_dir,
                renders=renders,
                timeout_seconds=1200,
            )
            sm = scene_result.metrics
            if sm.get("blender_status") == "BLOCKED":
                blender_blocked = True
                blender_block_reason = str(
                    sm.get("blocked_reason") or blender_block_reason
                )
            print(f"  backend: {sm.get('backend', 'blender')}")
            print(f"  blend: {scene_result.blend_path} ({sm.get('blend_bytes')} bytes)")
            print(f"  glb:   {scene_result.glb_path} ({sm.get('glb_bytes')} bytes)")
            print(f"  instances: {sm.get('instances')}")
            print(f"  unique_meshes: {sm.get('unique_meshes')}")
            proto_tris = sum(
                int(row.get("triangle_count", 0))
                for row in (sm.get("prototypes") or {}).values()
            )
            scene_tris = (sm.get("scene") or {}).get("triangle_count", proto_tris)
            print(f"  prototype_triangles_total: {proto_tris}")
            print(f"  scene_triangles: {scene_tris}")
            render_label = (
                "CPU orthographic previews"
                if sm.get("backend") == "trimesh-offline"
                else "EEVEE renders"
            )
            print(f"  renders ({render_label}):")
            for item in sm.get("renders", []):
                path = Path(item["path"])
                size = item.get("bytes", path.stat().st_size if path.is_file() else 0)
                print(f"    {path} ({size} bytes)")
                if not path.is_file() or size <= 0:
                    failures.append(f"missing render {path}")
            if int(sm.get("glb_bytes") or 0) <= 0:
                failures.append("flagship GLB missing or empty")
            blender_summary["flagship_scene"] = {
                "backend": sm.get("backend"),
                "blend_path": str(scene_result.blend_path),
                "glb_path": str(scene_result.glb_path),
                "blend_bytes": sm.get("blend_bytes"),
                "glb_bytes": sm.get("glb_bytes"),
                "instances": sm.get("instances"),
                "unique_meshes": sm.get("unique_meshes"),
                "prototype_triangles_total": proto_tris,
                "scene_triangles": scene_tris,
                "renders": sm.get("renders"),
                "blender_status": sm.get("blender_status", "OK"),
            }
        except Exception as error:  # noqa: BLE001
            failures.append(f"flagship scene emit failed: {error}")
            print(f"  FAIL flagship: {error}")
            traceback.print_exc()

    if blender_blocked:
        failures.append(
            "Blender headless backend BLOCKED; "
            f"reason: {blender_block_reason}"
        )

    summary = {
        "archetypes_cpu": archetype_report,
        "geometry_emit": blender_summary,
        "blender_blocked": blender_blocked,
        "blender_block_reason": blender_block_reason,
        "scene": {
            "instance_count": compiled.plan.instance_count(),
            "unique_mesh_count": compiled.plan.unique_mesh_count(),
            "bounds_m": compiled.bounds_m,
            "record_path": str(record_path),
            "record_digest": verified.digest,
        },
        "lod_regression_caught": not broken_report.passed,
        "failures": failures,
    }
    summary_path = output / "engine_report.json"
    summary_path.write_text(json.dumps(summary, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print()
    print(f"report: {summary_path}")

    if failures:
        print()
        print(f"FAILED ({len(failures)}):")
        for item in failures:
            print(f"  - {item}")
        return 1

    print()
    print("ALL ASSERTIONS PASSED")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
