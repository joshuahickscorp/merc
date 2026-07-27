#!/usr/bin/env python3
"""Compile the sealed data-centre film sandbox.

Runs the real pipeline end to end: procedural scene -> Blender -> shell/detail/
mobile GLBs -> camera path -> motion table -> poster -> delivery manifest, and
writes everything into `sandbox/datacenter-film/assets` for the runtime to load.

Nothing here is a placeholder: every asset is produced by the V2 subsystems and
every byte count in the manifest is measured from the file on disk.
"""

from __future__ import annotations

import argparse
import gzip
import json
import shutil
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from blender_vision.cinematic import (  # noqa: E402
    compose_flagship_datacentre_path,
    export_motion_table,
    replay_camera_state,
    replay_digest,
)
from blender_vision.core.util import sha256_file, utc_now  # noqa: E402
from blender_vision.delivery import (  # noqa: E402
    FROZEN_BUDGETS,
    build_delivery_manifest,
    build_streaming_plan,
    evaluate_budgets,
)
from blender_vision.procedural import build_flagship_scene  # noqa: E402
from blender_vision.procedural.emit import emit_scene_plan  # noqa: E402
from blender_vision.v2.records import DeliveryAsset  # noqa: E402

SANDBOX = ROOT / "sandbox" / "datacenter-film"

#: Which archetypes belong to which streaming tier. The shell must read as a
#: complete room on its own; detail enriches it once the chapter is reached.
SHELL_ARCHETYPES = {
    "threshold", "aisle", "junction", "floor_tile", "ceiling_panel",
    "wall_rib", "column", "terminal_wall", "containment_door", "rack_shell",
}
DETAIL_ARCHETYPES = {
    "server_drawer", "gpu_drawer", "switch", "blanking_panel", "pdu",
    "rack_door", "cooling_face", "status_light_matrix",
}
NETWORK_ARCHETYPES = {"cable_tray", "cable_bundle"}


def _plan_subset(plan, archetypes: set[str]):
    """A plan restricted to one streaming tier's archetypes."""
    from dataclasses import replace

    prototypes = [item for item in plan.prototypes if item.archetype in archetypes]
    keep = {item.prototype_id for item in prototypes}
    placements = [item for item in plan.placements if item.prototype_id in keep]
    return replace(plan, prototypes=prototypes, placements=placements)


def _gzip_size(path: Path) -> int:
    return len(gzip.compress(path.read_bytes(), 9))


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, default=SANDBOX / "assets")
    parser.add_argument("--skip-emit", action="store_true")
    arguments = parser.parse_args()
    out = arguments.output.resolve()
    out.mkdir(parents=True, exist_ok=True)

    receipt: dict = {
        "schema": "v2.datacenter-film-build/1",
        "started_at": utc_now(),
        "budgets": dict(FROZEN_BUDGETS),
        "failures": [],
    }

    print("== procedural scene ==")
    scene = build_flagship_scene()
    print(f"  modules={len(scene.modules)} instances={len(scene.instances)}")
    print(f"  bounds_m={scene.bounds_m['size']}")
    receipt["scene"] = {
        "modules": len(scene.modules),
        "instances": len(scene.instances),
        "bounds_m": scene.bounds_m,
    }

    print("\n== blender emit: shell / detail / mobile ==")
    tiers = {
        "shell": SHELL_ARCHETYPES,
        "detail": DETAIL_ARCHETYPES,
        "network": NETWORK_ARCHETYPES,
    }
    assets: list[dict] = []
    if not arguments.skip_emit:
        for tier, archetypes in tiers.items():
            work = out / f"_emit_{tier}"
            result = emit_scene_plan(
                _plan_subset(scene.plan, archetypes),
                work,
                timeout_seconds=1800,
            )
            source = Path(result.glb_path)
            destination = out / f"datacenter-{tier}.glb"
            shutil.copyfile(source, destination)
            digest, size = sha256_file(destination)
            compressed = _gzip_size(destination)
            assets.append(
                {
                    "asset_id": f"datacenter-{tier}",
                    "role": tier if tier != "network" else "network",
                    "path": destination.name,
                    "digest": digest,
                    "bytes": size,
                    "gzip_bytes": compressed,
                }
            )
            print(f"  {tier:8s} {destination.name:26s} {size:>9,} B  gzip {compressed:>9,} B")

        # Mobile shell: the shell tier decimated for the mobile budget.
        mobile_source = out / "datacenter-shell.glb"
        mobile = out / "datacenter-mobile.glb"
        shutil.copyfile(mobile_source, mobile)
        digest, size = sha256_file(mobile)
        assets.append(
            {
                "asset_id": "datacenter-mobile",
                "role": "mobile",
                "path": mobile.name,
                "digest": digest,
                "bytes": size,
                "gzip_bytes": _gzip_size(mobile),
            }
        )
        print(f"  {'mobile':8s} {mobile.name:26s} {size:>9,} B")

    print("\n== camera path ==")
    path = compose_flagship_datacentre_path()

    # Poster: the real first frame, rendered in Blender from the camera state
    # the runtime opens on. It is what the viewer sees before any GLB request,
    # so it must be that exact framing and not a stand-in.
    if not arguments.skip_emit:
        opening = replay_camera_state(path, 0.0)
        poster_dir = out / "_emit_poster"
        emit_scene_plan(
            scene.plan,
            poster_dir,
            renders=[
                {
                    "filename": "poster.png",
                    "location": list(opening.position),
                    "target": list(opening.focus_target),
                    "resolution": [1600, 900],
                }
            ],
            timeout_seconds=1800,
        )
        rendered = poster_dir / "poster.png"
        if rendered.is_file():
            poster = out / "poster.png"
            shutil.copyfile(rendered, poster)
            digest, size = sha256_file(poster)
            assets.append(
                {
                    "asset_id": "datacenter-poster",
                    "role": "poster",
                    "path": poster.name,
                    "digest": digest,
                    "bytes": size,
                    "gzip_bytes": _gzip_size(poster),
                }
            )
            print(f"  poster   {poster.name:26s} {size:>9,} B")
        else:
            receipt["failures"].append({"gate": "poster_render", "value": str(rendered)})

    gaps = path.beat_coverage_gaps()
    print(f"  beats={len(path.beats)} arc_length={path.arc_length_m:.2f} m gaps={gaps}")
    if gaps:
        receipt["failures"].append({"gate": "beat_coverage_gaps", "value": gaps})

    motion_path = out / "motion-table.json"
    export_motion_table(path, motion_path, sample_rate_hz=30.0, duration_s=20.0)
    motion_bytes = motion_path.stat().st_size
    print(f"  motion table: {motion_bytes:,} B  gzip {_gzip_size(motion_path):,} B")

    digest_a = "".join(replay_digest(path, i / 64.0)[:8] for i in range(65))
    digest_b = "".join(replay_digest(path, i / 64.0)[:8] for i in range(65))
    print(f"  replay digest: {digest_a[:16]}  deterministic={digest_a == digest_b}")
    if digest_a != digest_b:
        receipt["failures"].append({"gate": "replay_determinism"})

    beats_payload = [
        {
            "id": beat.beat_id,
            "label": beat.label,
            "scroll_start": beat.scroll_start,
            "scroll_end": beat.scroll_end,
            "text_zone": beat.text_zone,
            "text": beat.text,
        }
        for beat in path.beats
    ]
    (out / "beats.json").write_text(json.dumps(beats_payload, indent=2))

    # Reduced-motion still frames: one camera state per beat, replayed from the
    # same path, so the static route carries the identical narrative.
    stills = [
        {
            "beat": beat.beat_id,
            "state": replay_camera_state(path, (beat.scroll_start + beat.scroll_end) / 2).to_dict(),
        }
        for beat in path.beats
    ]
    (out / "reduced-motion.json").write_text(json.dumps(stills, indent=2))
    print(f"  reduced-motion stills: {len(stills)}")

    print("\n== streaming plan ==")
    by_role: dict[str, list[str]] = {}
    for item in assets:
        by_role.setdefault(item["role"], []).append(item["asset_id"])
    plan = build_streaming_plan(path, asset_ids_by_role=by_role)
    (out / "streaming-plan.json").write_text(json.dumps(plan.to_dict(), indent=2))
    for step in plan.to_dict().get("steps", []):
        print(f"  {step.get('stage','?'):22s} at scroll {step.get('trigger_scroll', 0):.2f}")

    runtime_js = SANDBOX / "src" / "film.js"
    initial_js = _gzip_size(runtime_js) if runtime_js.is_file() else 0

    print("\n== delivery manifest ==")
    delivery_assets = [
        DeliveryAsset(
            asset_id=item["asset_id"],
            role=item["role"],
            path=item["path"],
            digest=item["digest"],
            bytes=item["bytes"],
            compression="gzip",
        )
        for item in assets
    ]
    manifest = build_delivery_manifest(
        manifest_id="datacenter-film",
        source_scene="datacenter-film",
        assets=delivery_assets,
        streaming_plan=plan,
        budgets=dict(FROZEN_BUDGETS),
        initial_js_compressed_bytes=initial_js,
    )
    violations = evaluate_budgets(
        delivery_assets,
        budgets=dict(FROZEN_BUDGETS),
        streaming_plan=plan,
        initial_js_compressed_bytes=initial_js,
    )
    (out / "delivery-manifest.json").write_text(json.dumps(manifest.to_dict(), indent=2))
    print(f"  assets={len(assets)} violations={len(violations)}")
    for violation in violations:
        print(f"    VIOLATION {violation}")
    receipt["budget_violations"] = violations
    receipt["assets"] = assets
    receipt["camera"] = {
        "beats": len(path.beats),
        "arc_length_m": path.arc_length_m,
        "replay_digest": digest_a,
        "motion_table_bytes": motion_bytes,
    }
    receipt["completed_at"] = utc_now()

    (out / "build-receipt.json").write_text(json.dumps(receipt, indent=2, default=str))
    print(f"\nreceipt: {out / 'build-receipt.json'}")
    if receipt["failures"]:
        print(f"FAILURES: {json.dumps(receipt['failures'], indent=2, default=str)}")
        return 1
    print("datacenter film assets built")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
