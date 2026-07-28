#!/usr/bin/env python3
"""End-to-end cinematic + delivery compiler evidence run."""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from blender_vision.cinematic.emit import bake_blender_camera, export_motion_table  # noqa: E402
from blender_vision.cinematic.path import compose_flagship_datacentre_path  # noqa: E402
from blender_vision.cinematic.replay import replay_camera_state, replay_digest  # noqa: E402
from blender_vision.cinematic.spline import ArcLengthSpline, CatmullRomSpline  # noqa: E402
from blender_vision.core.util import atomic_write_json, sha256_file  # noqa: E402
from blender_vision.delivery.compress import (  # noqa: E402
    measure_and_select_compression,
    write_selected_payload,
)
from blender_vision.delivery.lods import (  # noqa: E402
    LodBudget,
    build_procedural_rack_glb,
    generate_lods,
)
from blender_vision.delivery.manifest import FROZEN_BUDGETS, build_delivery_manifest  # noqa: E402
from blender_vision.delivery.stream import build_streaming_plan  # noqa: E402
from blender_vision.v2.records import DeliveryAsset  # noqa: E402
from blender_vision.v2.validation import write_record  # noqa: E402


def _cross_process_replay_digests(graph_path: Path, scroll: float = 0.42) -> tuple[str, str]:
    script = f"""
import json, hashlib, sys
sys.path.insert(0, {str(ROOT / "src")!r})
from blender_vision.core.util import canonical_json
from blender_vision.v2.records import CameraPathGraph
from blender_vision.cinematic.replay import replay_camera_state
graph = CameraPathGraph.from_dict(json.loads(open({str(graph_path)!r}).read()))
state = replay_camera_state(graph, {scroll!r}).to_dict()
print(hashlib.sha256(canonical_json(state)).hexdigest())
"""
    digests: list[str] = []
    for _ in range(2):
        completed = subprocess.run(
            [sys.executable, "-c", script],
            capture_output=True,
            text=True,
            check=True,
            cwd=str(ROOT),
        )
        digests.append(completed.stdout.strip().splitlines()[-1])
    return digests[0], digests[1]


def _print_compression_table(selections: list) -> None:
    print("\n=== COMPRESSION MEASUREMENT TABLE ===")
    header = (
        f"{'asset':<16} {'codec':<22} {'avail':<6} {'bytes':>10} "
        f"{'decode_ms':>12} {'main_ms':>10} {'vis_diff':>10}"
    )
    print(header)
    print("-" * len(header))
    for selection in selections:
        for candidate in selection.candidates:
            print(
                f"{selection.asset_id:<16} {candidate.codec:<22} "
                f"{str(candidate.available):<6} {candidate.bytes:>10} "
                f"{candidate.decode_ms:>12.4f} {candidate.main_thread_ms:>10.4f} "
                f"{candidate.visual_difference:>10.6f}"
            )
        print(
            f"  -> SELECTED {selection.selected_codec}: {selection.selection_reason}"
        )
        if not selection.brotli_available:
            print("  note: brotli unavailable in this environment")
        if not selection.draco_available:
            print("  note: Draco encode blocked (no encoder / extension not assumed)")
        if not selection.meshopt_available:
            print("  note: meshopt encode blocked (no meshoptimizer binding)")
    print("=== END COMPRESSION TABLE ===\n")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=ROOT / "artifacts" / "v2" / "cinematic",
        help="output directory for path, motion table, LODs, manifest",
    )
    parser.add_argument("--skip-blender", action="store_true", help="skip Blender bake/LOD steps")
    args = parser.parse_args()
    out: Path = args.output
    out.mkdir(parents=True, exist_ok=True)

    exit_code = 0
    print("VisionMCP V2 cinematic + delivery compiler")
    print(f"output: {out}")

    # 1. Compose flagship path
    graph = compose_flagship_datacentre_path()
    path_file = write_record(out / "camera_path_graph.json", graph)
    print(f"\n[1] Flagship path composed: {graph.id}")
    print(f"    beats: {[beat.label for beat in graph.beats]}")
    print(f"    arc_length_m: {graph.arc_length_m:.6f}")
    print(f"    digest: {graph.digest}")
    print(f"    wrote: {path_file}")

    # 2. Arc-length proof
    arc = ArcLengthSpline(CatmullRomSpline(graph.control_points), table_samples=8192)
    rel_err = arc.relative_arc_length_error(1000)
    max_dev = arc.max_scroll_distance_deviation(1000)
    print("\n[2] Arc-length parameterisation")
    print(f"    relative_arc_length_error(1000) = {rel_err:.3e}")
    print(f"    max_scroll_distance_deviation(1000) = {max_dev:.3e}")
    if rel_err >= 1e-6:
        print("    FAIL: relative error exceeds 1e-6")
        exit_code = 1
    else:
        print("    PASS: relative error < 1e-6")

    # 3. Cross-process deterministic replay
    d1, d2 = _cross_process_replay_digests(path_file, 0.42)
    print("\n[3] Deterministic replay across two Python processes (scroll=0.42)")
    print(f"    process_a_digest = {d1}")
    print(f"    process_b_digest = {d2}")
    if d1 != d2:
        print("    FAIL: digests differ")
        exit_code = 1
    else:
        print("    PASS: byte-identical")
    # Also print in-process digest for the sealed graph
    print(f"    in_process_digest = {replay_digest(graph, 0.42)}")

    # 4. Beat coverage
    gaps = graph.beat_coverage_gaps()
    # Overlaps: adjacent ordered beats
    ordered = sorted(graph.beats, key=lambda b: b.scroll_start)
    overlaps = []
    for left, right in zip(ordered, ordered[1:], strict=False):
        start = max(left.scroll_start, right.scroll_start)
        end = min(left.scroll_end, right.scroll_end)
        if end - start > 1e-6:
            overlaps.append((left.beat_id, right.beat_id, start, end))
    print("\n[4] Beat coverage")
    print(f"    gaps = {gaps}")
    print(f"    overlaps = {overlaps}")
    if gaps or overlaps:
        print("    FAIL: gaps or overlaps present")
        exit_code = 1
    else:
        print("    PASS: empty gaps and overlaps")

    # 5. Bake Blender camera + motion table
    motion = export_motion_table(
        graph, out / "motion_table.json", sample_rate_hz=30.0, duration_s=20.0
    )
    print("\n[5] Motion table + Blender camera bake")
    print(
        f"    motion_table bytes={motion['bytes']} samples={motion['sample_count']} "
        f"rate_hz={motion['sample_rate_hz']} interpolation={motion['interpolation']}"
    )
    if not args.skip_blender:
        try:
            bake = bake_blender_camera(
                graph, out / "flagship_path_camera.blend", sample_count=120, frame_end=120
            )
            print(
                f"    blender_camera bytes={bake['bytes']} "
                f"keys={bake['key_count']} path={bake['path']}"
            )
        except Exception as error:  # noqa: BLE001 — evidence run must report blockers
            print(f"    BLOCKED Blender camera bake: {error}")
            # Not a contract-fatal exit (only gaps / replay / arc-length are fatal).
    else:
        print("    skipped Blender camera bake (--skip-blender)")

    # 6. Mesh + LODs + measured compression
    print("\n[6] Mesh LODs + measured compression")
    print(
        "    note: src/blender_vision/procedural/ not on branch; "
        "building Blender substitute rack"
    )
    selections = []
    assets: list[DeliveryAsset] = []

    # Always measure compression on synthetic assets that force divergent codec picks,
    # plus the real mesh when Blender is available.
    compressible = (b"RACK-PANEL-METAL-" * 8000) + bytes(4000)
    random_payload = os.urandom(60_000)
    (out / "assets").mkdir(exist_ok=True)
    comp_path = out / "assets" / "compressible.bin"
    rand_path = out / "assets" / "random.bin"
    comp_path.write_bytes(compressible)
    rand_path.write_bytes(random_payload)
    sel_comp = measure_and_select_compression(
        comp_path, asset_id="synthetic_compressible", include_position_quantize=False
    )
    sel_rand = measure_and_select_compression(
        rand_path, asset_id="synthetic_random", include_position_quantize=False
    )
    selections.extend([sel_comp, sel_rand])
    write_selected_payload(sel_comp, out / "assets" / f"compressible.{sel_comp.selected_codec}")
    write_selected_payload(sel_rand, out / "assets" / f"random.{sel_rand.selected_codec}")

    poster = out / "assets" / "poster.jpg"
    # Minimal JPEG-ish payload (bytes only; not a real decode target).
    poster.write_bytes(b"\xff\xd8\xff\xe0" + b"\x00" * 40_000 + b"\xff\xd9")
    poster_digest, poster_size = sha256_file(poster)
    assets.append(
        DeliveryAsset(
            asset_id="poster",
            role="poster",
            path=str(poster.relative_to(out)),
            digest=poster_digest,
            bytes=poster_size,
            compression="jpeg",
        )
    )

    shell_bytes = 0
    mobile_bytes = 0
    try:
        rack = build_procedural_rack_glb(
            out / "assets" / "rack_source.glb",
            allow_python_fallback=not args.skip_blender,
        )
        lod_report = generate_lods(
            rack,
            [
                LodBudget(name="shell_L0", max_triangles=200, screen_space_error_px=1.0),
                LodBudget(name="mobile_L2", max_triangles=60, screen_space_error_px=4.0),
                LodBudget(name="detail_L1", max_triangles=120, screen_space_error_px=2.0),
            ],
            out / "assets" / "lods",
            min_silhouette_iou=0.5,
            allow_python_fallback=True,
        )
        atomic_write_json(out / "lod_report.json", lod_report.to_dict())
        print(f"    blender_used={lod_report.blender_used}")
        print(f"    source_triangles={lod_report.source_triangles}")
        for note in lod_report.notes:
            print(f"    note: {note}")
        for level in lod_report.levels:
            print(
                f"    LOD {level.name}: tris={level.triangles} bytes={level.bytes} "
                f"iou={level.silhouette_iou:.4f} within_budget={level.within_budget} "
                f"identity={level.identity_pass}"
            )
            sel = measure_and_select_compression(
                Path(level.path), asset_id=level.name, include_position_quantize=True
            )
            selections.append(sel)
            selected_path = write_selected_payload(
                sel, out / "assets" / f"{level.name}.{sel.selected_codec}"
            )
            role = "shell"
            if "mobile" in level.name:
                role = "mobile"
            elif "detail" in level.name:
                role = "detail"
            assets.append(
                DeliveryAsset(
                    asset_id=level.name,
                    role=role,
                    path=str(selected_path.relative_to(out)),
                    digest=sel.selected.digest or ("0" * 64),
                    bytes=sel.selected.bytes,
                    compression=sel.selected_codec,
                    lod=level.name,
                    decode_ms=sel.selected.decode_ms,
                    main_thread_ms=sel.selected.main_thread_ms,
                    visual_difference=sel.selected.visual_difference,
                    chapter="CAPACITY" if role == "detail" else None,
                )
            )
            if role == "shell":
                shell_bytes = sel.selected.bytes
            if role == "mobile":
                mobile_bytes = sel.selected.bytes
    except Exception as error:  # noqa: BLE001
        print(f"    BLOCKED mesh/LOD pipeline: {error}")
        fake_shell = out / "assets" / "shell_placeholder.bin"
        fake_shell.write_bytes(b"SHELL" + b"\x00" * (2 * 1024 * 1024))
        sel = measure_and_select_compression(
            fake_shell, asset_id="shell_placeholder", include_position_quantize=False
        )
        selections.append(sel)
        selected_path = write_selected_payload(sel, out / "assets" / f"shell.{sel.selected_codec}")
        assets.append(
            DeliveryAsset(
                asset_id="shell_placeholder",
                role="shell",
                path=str(selected_path.relative_to(out)),
                digest=sel.selected.digest or ("0" * 64),
                bytes=sel.selected.bytes,
                compression=sel.selected_codec,
                decode_ms=sel.selected.decode_ms,
                main_thread_ms=sel.selected.main_thread_ms,
                visual_difference=sel.selected.visual_difference,
            )
        )
        shell_bytes = sel.selected.bytes

    _print_compression_table(selections)

    # 7. Delivery manifest + budgets
    plan = build_streaming_plan(
        graph,
        asset_ids_by_role={
            "poster": ["poster"],
            "shell": [a.asset_id for a in assets if a.role == "shell"],
            "detail": [a.asset_id for a in assets if a.role == "detail"],
            "mobile": [a.asset_id for a in assets if a.role == "mobile"],
            "network": ["junction", "second_corridor"],
            "terminal": ["terminal"],
        },
    )
    atomic_write_json(out / "streaming_plan.json", plan.to_dict())
    # Intentionally include a realistic initial JS measurement; may violate or not.
    initial_js = 180 * 1024
    # If shell is huge, violations will surface honestly.
    manifest = build_delivery_manifest(
        manifest_id="flagship-delivery",
        source_scene="flagship-datacentre",
        assets=assets,
        compression_selections=selections,
        streaming_plan=plan,
        budgets=FROZEN_BUDGETS,
        initial_js_compressed_bytes=initial_js,
    )
    write_record(out / "delivery_manifest.json", manifest)
    print("[7] DeliveryManifest")
    print(f"    digest: {manifest.digest}")
    print(f"    total_asset_bytes: {manifest.total_bytes()}")
    print(f"    shell_bytes_selected: {shell_bytes}")
    print(f"    mobile_bytes_selected: {mobile_bytes}")
    print(f"    budgets: {json.dumps(manifest.budgets)}")
    if manifest.budget_violations:
        print(f"    budget_violations ({len(manifest.budget_violations)}):")
        for item in manifest.budget_violations:
            print(f"      - {item}")
        print("    (violations reported; not fatal per contract)")
    else:
        print("    budget_violations: []")

    # Sample camera state for evidence
    state = replay_camera_state(graph, 0.5)
    print("\n[sample] camera state at scroll=0.5")
    print(f"    {json.dumps(state.to_dict(), sort_keys=True)}")

    print(f"\nexit_code={exit_code}")
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())
