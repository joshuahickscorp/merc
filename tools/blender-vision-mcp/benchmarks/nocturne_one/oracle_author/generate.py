from __future__ import annotations

import argparse
import hashlib
import json
import mimetypes
import platform
import secrets
import shutil
import subprocess
import time
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from PIL import Image, ImageDraw, ImageFont

from blender_vision.benchmarks.nocturne import (
    NocturnePacketAuthority,
    load_nocturne_contract,
)
from blender_vision.core.config import discover_blender, discover_executable
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Generate the sealed NOCTURNE/ONE oracle")
    parser.add_argument("--output", required=True)
    parser.add_argument("--contract")
    return parser


def _media_type(path: Path) -> str:
    if path.suffix == ".json":
        return "application/json"
    if path.suffix == ".png":
        return "image/png"
    if path.suffix == ".mp4":
        return "video/mp4"
    return mimetypes.guess_type(path.name)[0] or "application/octet-stream"


def _video(ffmpeg: str, frames: Path, output: Path, duration: int) -> None:
    result = subprocess.run(
        [
            ffmpeg,
            "-hide_banner",
            "-loglevel",
            "error",
            "-y",
            "-framerate",
            "3",
            "-i",
            str(frames / "%04d.png"),
            "-t",
            str(duration),
            "-r",
            "24",
            "-c:v",
            "libx264",
            "-crf",
            "20",
            "-pix_fmt",
            "yuv420p",
            "-movflags",
            "+faststart",
            str(output),
        ],
        capture_output=True,
        text=True,
        timeout=300,
        check=False,
    )
    if result.returncode:
        raise RuntimeError(f"ffmpeg failed for {output.name}: {result.stderr}")


def _mood_board(references: Path, output: Path) -> None:
    labels = ("hero", "front", "rear", "top")
    canvas = Image.new("RGB", (1024, 1024), "#070910")
    draw = ImageDraw.Draw(canvas)
    font = ImageFont.load_default(size=24)
    for index, label in enumerate(labels):
        image = Image.open(references / f"{label}.png").convert("RGBA")
        background = Image.new("RGBA", image.size, "#0A0D16")
        background.alpha_composite(image)
        background = background.convert("RGB").resize((480, 480))
        x = 24 + (index % 2) * 496
        y = 24 + (index // 2) * 496
        canvas.paste(background, (x, y))
        draw.text(
            (x + 18, y + 18),
            f"NOCTURNE/ONE — {label.upper()}",
            fill="#F4F2EC",
            font=font,
        )
    canvas.save(output, format="PNG", optimize=True)


def _write_public_documents(
    *,
    packet: Path,
    contract: Any,
    spec: dict[str, Any],
    oracle_manifest: dict[str, Any],
) -> None:
    atomic_write_json(
        packet / "dimension-sheet.json",
        {
            "schema_version": "1",
            "authority": "TEXTUAL_PARAMETRIC_GROUND_TRUTH",
            "coordinate_system": spec["coordinate_system"],
            "overall_dimensions_mm": spec["overall_dimensions_mm"],
            "base": spec["base"],
            "outer_shell": spec["outer_shell"],
            "glass_core": spec["glass_core"],
            "eclipse_disk": spec["eclipse_disk"],
            "acoustic_membrane": spec["acoustic_membrane"],
            "thermal_grille": spec["thermal_grille"],
            "rotary_control": spec["rotary_control"],
            "braided_cable": spec["braided_cable"],
            "public_cameras": oracle_manifest["public_cameras"],
            "hidden_camera_count": contract.hidden_holdout_count,
            "hidden_cameras_disclosed": False,
        },
    )
    atomic_write_json(
        packet / "part-material-specification.json",
        {
            "schema_version": "1",
            "authority": "OWNED_PRODUCT_SPECIFICATION",
            "required_parts": contract.required_parts,
            "internal_assembly": spec["internal_assembly"],
            "material_variants": spec["material_variants"],
            "material_classes": {
                "base": "black_anodized_aluminum",
                "outer_shell": "black_anodized_aluminum",
                "glass_core": "frosted_translucent_glass",
                "eclipse_disk": "warm_emissive_ceramic",
                "acoustic_membrane": "graphite_tensioned_textile",
                "thermal_grille": "black_anodized_aluminum",
                "rotary_control": "machined_aluminum",
                "braided_cable": "braided_graphite",
            },
            "required_separate_editable_materials": [
                "frosted_translucent_glass",
                "warm_emissive_ceramic",
            ],
        },
    )
    atomic_write_json(
        packet / "interaction-storyboard.json",
        {
            "schema_version": "1",
            "scenes": [
                {
                    "id": "hero-arrival",
                    "route": "/",
                    "input": "initial-load",
                    "result": "poster appears before 3D; product resolves from eclipse glow",
                },
                {
                    "id": "hero-orbit",
                    "route": "/",
                    "input": "pointer-drag or touch-drag",
                    "result": "camera orbits without changing configuration state",
                },
                {
                    "id": "scroll-choreography",
                    "route": "/",
                    "input": "scroll",
                    "result": "camera progresses through shell, core, and control chapters",
                },
                {
                    "id": "semantic-explode",
                    "route": "/technology",
                    "input": "select named part",
                    "result": "exploded assembly and accessible text identify the same part",
                },
                {
                    "id": "configure",
                    "route": "/configurator",
                    "input": "variant, light, orientation, accessory controls",
                    "result": "saved application state and 3D state remain identical",
                },
                {
                    "id": "reserve",
                    "route": "/reserve",
                    "input": "validated idempotent form submission",
                    "result": "API/database reservation then receipt status",
                },
            ],
            "reduced_motion": "remove scroll-bound camera animation and use explicit steps",
            "no_webgl": "retain poster, narrative, configuration, reservation, and receipt",
        },
    )
    atomic_write_json(
        packet / "product-application-brief.json",
        {
            "schema_version": "1",
            "product": spec["product"],
            "positioning": "poster-first premium product story with inspectable evidence",
            "routes": contract.application_routes,
            "states": contract.application_states,
            "required_journeys": contract.required_journeys,
            "runtime_probe_contract": contract.runtime_probe_contract,
            "implementation": {
                "language": "TypeScript",
                "source_assets": "self-hosted synthetic owned only",
                "semantic_html": True,
                "keyboard_accessibility": True,
                "reduced_motion": True,
                "responsive_breakpoints": ["mobile", "tablet", "desktop"],
                "no_webgl_fallback": True,
                "editable_blend": True,
                "validated_glb": True,
                "database_migrations": True,
                "deterministic_seed_data": True,
                "fresh_clone_reproduction": True,
            },
            "visual_direction": {
                "palette": ["#070910", "#111318", "#FF6B3D", "#F4F2EC", "#8CA9C7"],
                "type": "editorial grotesk with compact technical annotation",
                "composition": "asymmetric negative space, eclipse glow, restrained glass",
                "motion": "slow spatial reveal; never required for comprehension",
            },
        },
    )
    atomic_write_json(
        packet / "api-contract.json",
        {
            "openapi": "3.1.0",
            "info": {"title": "NOCTURNE/ONE local reservation API", "version": "1.0.0"},
            "paths": {
                "/api/configurations": {
                    "post": {
                        "operationId": "saveConfiguration",
                        "authorization": "authenticated",
                        "idempotencyHeader": "Idempotency-Key",
                        "responses": ["201", "200", "400", "401", "503"],
                    }
                },
                "/api/reservations": {
                    "post": {
                        "operationId": "createReservation",
                        "authorization": "permission",
                        "requiredPermission": "reservation:create",
                        "idempotencyHeader": "Idempotency-Key",
                        "responses": ["201", "200", "400", "401", "403", "409", "503"],
                    }
                },
                "/api/reservations/{id}": {
                    "get": {
                        "operationId": "getReservationStatus",
                        "authorization": "authenticated",
                        "responses": ["200", "401", "404"],
                    }
                },
            },
        },
    )
    atomic_write_json(
        packet / "data-schema.json",
        {
            "schema_version": "1",
            "database": "sqlite",
            "entities": {
                "configuration": {
                    "fields": {
                        "id": "text-primary-key",
                        "variant": "obsidian|lunar|ember",
                        "light_intensity": "integer-0-100",
                        "orientation": "integer--45-45",
                        "accessory": "none|braided-cable",
                        "created_at": "iso8601",
                    }
                },
                "reservation": {
                    "fields": {
                        "id": "text-primary-key",
                        "configuration_id": "configuration-foreign-key",
                        "email": "validated-email",
                        "status": "confirmed|pending|cancelled",
                        "idempotency_key": "unique-text",
                        "created_at": "iso8601",
                    }
                },
            },
            "migrations": ["001_initial_up", "001_initial_down"],
            "seed": {"configuration_variant": "obsidian"},
        },
    )
    atomic_write_json(
        packet / "authorization-policy.json",
        {
            "schema_version": "1",
            "provider": "local_test_header",
            "actor_header": "X-NOCTURNE-ACTOR",
            "permission_header": "X-NOCTURNE-PERMISSIONS",
            "rules": [
                "anonymous actors may read public product assets",
                "configuration persistence requires an authenticated actor",
                "reservation creation requires reservation:create",
                "reservation status is scoped to the creating actor",
                "no cross-actor reservation lookup",
            ],
        },
    )
    atomic_write_json(packet / "performance-budget.json", contract.performance_budget)
    atomic_write_json(
        packet / "accessibility-requirements.json",
        {
            **contract.accessibility_gates,
            "routes": contract.application_routes,
            "requirements": [
                "one level-one heading per route",
                "landmarks and skip link",
                "visible focus",
                "form labels and error associations",
                "live reservation status",
                "textual equivalent for every selectable 3D part",
                "touch targets at least 44 CSS pixels",
                "color is not the sole state indicator",
            ],
        },
    )


def _packet_manifest(
    packet: Path,
    *,
    benchmark_id: str,
    oracle_seed: int,
    contract_sha256: str,
    governed_spec_sha256: str,
) -> dict[str, Any]:
    artifacts = []
    for path in sorted(candidate for candidate in packet.rglob("*") if candidate.is_file()):
        if path.name == "packet.manifest.json":
            continue
        digest, size = sha256_file(path)
        artifacts.append(
            {
                "path": path.relative_to(packet).as_posix(),
                "sha256": digest,
                "size": size,
                "media_type": _media_type(path),
            }
        )
    return {
        "schema_version": "1",
        "benchmark_id": benchmark_id,
        "authority": "GOVERNED_BUILDER_INPUT",
        "oracle_seed": oracle_seed,
        "contract_sha256": contract_sha256,
        "governed_spec_sha256": governed_spec_sha256,
        "generated_at": datetime.now(UTC).isoformat(),
        "artifacts": artifacts,
        "excluded_authority": [
            "oracle .blend",
            "oracle generation source",
            "hidden holdout renders",
            "oracle mesh statistics",
            "oracle material node values",
            "oracle website source",
            "hidden mobile interaction trace",
        ],
        "rights_state": "SYNTHETIC_OWNED",
    }


def main() -> None:
    args = _parser().parse_args()
    contract, contract_path = load_nocturne_contract(
        Path(args.contract) if args.contract else None
    )
    benchmark_root = contract_path.parent
    spec_path = benchmark_root / "governed_spec.json"
    scene_script = benchmark_root / "oracle_author" / "scene.py"
    output = Path(args.output).expanduser().resolve()
    if output.exists() and any(output.iterdir()):
        raise ValueError(f"oracle output must be new or empty: {output}")
    output.mkdir(parents=True, exist_ok=True)
    blender = discover_blender()
    ffmpeg = discover_executable("ffmpeg", ["-version"])
    if not blender.available or not blender.path:
        raise RuntimeError("installed Blender is required")
    if not ffmpeg.available or not ffmpeg.path:
        raise RuntimeError("installed ffmpeg is required")
    started_at = datetime.now(UTC).isoformat()
    started = time.monotonic()
    command = [
        blender.path,
        "--background",
        "--factory-startup",
        "--disable-autoexec",
        "--python-exit-code",
        "1",
        "--python",
        str(scene_script),
        "--",
        str(spec_path),
        str(output),
    ]
    result = subprocess.run(
        command,
        capture_output=True,
        text=True,
        timeout=1800,
        check=False,
    )
    (output / "oracle.blender.stdout.log").write_text(result.stdout, encoding="utf-8")
    (output / "oracle.blender.stderr.log").write_text(result.stderr, encoding="utf-8")
    if result.returncode:
        raise RuntimeError(f"oracle Blender generation failed: {result.stderr[-4000:]}")
    packet = output / "input-packet"
    sealed = output / "sealed-evaluator"
    motion = packet / "motion"
    motion.mkdir()
    _video(ffmpeg.path, output / "turntable-frames", motion / "turntable-12s.mp4", 12)
    _video(ffmpeg.path, output / "exploded-frames", motion / "exploded-8s.mp4", 8)
    _mood_board(packet / "references", packet / "visual-mood-board.png")
    spec = json.loads(spec_path.read_text(encoding="utf-8"))
    oracle_manifest = json.loads(
        (sealed / "oracle.manifest.json").read_text(encoding="utf-8")
    )
    _write_public_documents(
        packet=packet,
        contract=contract,
        spec=spec,
        oracle_manifest=oracle_manifest,
    )
    canary = f"NOCTURNE-ORACLE-{secrets.token_hex(32)}"
    (sealed / "ORACLE_CANARY.txt").write_text(canary, encoding="utf-8")
    mobile = sealed / "mobile"
    mobile.mkdir()
    atomic_write_json(
        mobile / "hidden-interaction-trace.json",
        {
            "schema_version": "1",
            "viewport": [390, 844],
            "device_scale_factor": 3,
            "pointer": "touch",
            "steps": [
                {"action": "goto", "path": "/"},
                {"action": "wait_for", "state": "poster_fallback"},
                {"action": "tap", "target": "enter-3d"},
                {"action": "swipe", "target": "product-canvas", "delta": [96, -18]},
                {"action": "goto", "path": "/configurator"},
                {"action": "select", "target": "variant", "value": "ember"},
                {"action": "set", "target": "light", "value": 72},
                {"action": "goto", "path": "/reserve"},
                {"action": "submit", "expect": "successful_reservation"},
                {"action": "goto", "path": "/receipt"},
            ],
        },
    )
    packet_manifest = _packet_manifest(
        packet,
        benchmark_id=contract.benchmark_id,
        oracle_seed=contract.oracle_seed,
        contract_sha256=sha256_file(contract_path)[0],
        governed_spec_sha256=sha256_file(spec_path)[0],
    )
    atomic_write_json(packet / "packet.manifest.json", packet_manifest)
    packet_verification = NocturnePacketAuthority(contract_path).verify(packet)
    sealed_files = {
        path.relative_to(sealed).as_posix(): sha256_file(path)[0]
        for path in sorted(candidate for candidate in sealed.rglob("*") if candidate.is_file())
    }
    receipt = {
        "schema_version": "1",
        "benchmark_id": contract.benchmark_id,
        "authority": "SEALED_ORACLE_AUTHOR",
        "oracle_seed": contract.oracle_seed,
        "contract_sha256": sha256_file(contract_path)[0],
        "governed_spec_sha256": sha256_file(spec_path)[0],
        "oracle_scene_source_sha256": sha256_file(scene_script)[0],
        "oracle_blend_sha256": sha256_file(
            sealed / "nocturne-one-oracle.blend"
        )[0],
        "oracle_manifest_sha256": sha256_file(sealed / "oracle.manifest.json")[0],
        "oracle_canary_sha256": hashlib.sha256(canary.encode("utf-8")).hexdigest(),
        "packet_verification": packet_verification,
        "sealed_output_digests": sealed_files,
        "public_reference_count": len(contract.public_view_labels),
        "hidden_holdout_count": len(oracle_manifest["hidden_cameras"]),
        "hidden_mobile_trace_count": 1,
        "builder_access_to_sealed_outputs": False,
        "started_at": started_at,
        "completed_at": datetime.now(UTC).isoformat(),
        "elapsed_seconds": time.monotonic() - started,
        "process": {
            "command": command,
            "exit_code": result.returncode,
            "blender_version": blender.version,
            "blender_executable_sha256": sha256_file(Path(blender.path))[0],
            "ffmpeg_version": ffmpeg.version,
        },
        "host": {
            "platform": platform.platform(),
        },
        "claim_boundary": [
            "The public packet contains synthetic owned renders and textual authority only.",
            "Hidden cameras, holdouts, mesh statistics, node values, trace, and BLEND stay "
            "under the sealed evaluator root.",
            "The builder sandbox must independently prove denial of this root and the "
            "oracle-author source root.",
        ],
    }
    receipt["receipt_payload_sha256"] = hashlib.sha256(canonical_json(receipt)).hexdigest()
    atomic_write_json(output / "oracle-author.receipt.json", receipt)
    shutil.rmtree(output / "turntable-frames")
    shutil.rmtree(output / "exploded-frames")


if __name__ == "__main__":
    main()
