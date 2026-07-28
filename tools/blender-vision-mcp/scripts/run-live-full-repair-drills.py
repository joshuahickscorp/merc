from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import subprocess
import time
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Callable


IGNORED_PARTS = {"node_modules", "dist", "data", "playwright-report", "test-results"}
BLENDER = Path("/Applications/Blender.app/Contents/MacOS/Blender")
GENERATED_3D = (
    Path("3d/nocturne-one.blend"),
    Path("3d/nocturne-one.blend1"),
    Path("public/assets/nocturne-one-hero.glb"),
    Path("public/assets/nocturne-one-low.glb"),
    Path("public/assets/nocturne-one-poster.webp"),
)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def canonical_sha(value: object) -> str:
    return hashlib.sha256(
        json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()


def tree_manifest(root: Path) -> dict[str, str]:
    return {
        path.relative_to(root).as_posix(): sha256_file(path)
        for path in sorted(root.rglob("*"))
        if path.is_file()
        and not path.is_symlink()
        and not any(part in IGNORED_PARTS for part in path.relative_to(root).parts)
    }


def copy_source(source: Path, target: Path) -> None:
    shutil.copytree(
        source,
        target,
        ignore=shutil.ignore_patterns(*IGNORED_PARTS),
    )


def replace_once(path: Path, before: str, after: str) -> None:
    value = path.read_text(encoding="utf-8")
    if value.count(before) != 1:
        raise RuntimeError(
            f"expected one mutation target in {path}, observed {value.count(before)}"
        )
    path.write_text(value.replace(before, after, 1), encoding="utf-8")


def append_text(path: Path, value: str) -> None:
    with path.open("a", encoding="utf-8") as stream:
        stream.write(value)


def restore_paths(source: Path, candidate: Path, paths: tuple[Path, ...]) -> None:
    for relative in paths:
        destination = candidate / relative
        baseline = source / relative
        if baseline.exists():
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(baseline, destination)
        elif destination.exists():
            destination.unlink()


def run_command(
    *,
    identifier: str,
    command: list[str],
    cwd: Path,
    output: Path,
    env: dict[str, str] | None = None,
    timeout: int = 300,
) -> dict[str, object]:
    started_at = datetime.now(UTC).isoformat()
    started = time.monotonic()
    completed = subprocess.run(
        command,
        cwd=cwd,
        env={**os.environ, **(env or {})},
        capture_output=True,
        text=True,
        check=False,
        timeout=timeout,
    )
    elapsed = time.monotonic() - started
    stdout = output / f"{identifier}.stdout.log"
    stderr = output / f"{identifier}.stderr.log"
    stdout.write_text(completed.stdout, encoding="utf-8")
    stderr.write_text(completed.stderr, encoding="utf-8")
    return {
        "id": identifier,
        "command": command,
        "cwd": str(cwd),
        "started_at": started_at,
        "ended_at": datetime.now(UTC).isoformat(),
        "elapsed_seconds": elapsed,
        "exit_status": completed.returncode,
        "stdout": {
            "path": str(stdout),
            "sha256": sha256_file(stdout),
            "bytes": stdout.stat().st_size,
        },
        "stderr": {
            "path": str(stderr),
            "sha256": sha256_file(stderr),
            "bytes": stderr.stat().st_size,
        },
    }


def blender_build(candidate: Path, output: Path, identifier: str) -> dict[str, object]:
    return run_command(
        identifier=identifier,
        command=[
            str(BLENDER),
            "--background",
            "--factory-startup",
            "--disable-autoexec",
            "--python-exit-code",
            "1",
            "--python",
            "3d/build_candidate.py",
        ],
        cwd=candidate,
        output=output,
        env={"NOCTURNE_SKIP_POSTER": "1"},
        timeout=300,
    )


def write_blender_check(path: Path, kind: str) -> None:
    if kind == "material":
        body = """
import bpy
obj = bpy.data.objects.get("glass_core")
observed = obj.data.materials[0].name if obj and obj.data.materials else None
if observed != "MAT_FROSTED_TRANSLUCENT_GLASS":
    raise SystemExit(f"glass_core material mismatch: {observed}")
print("glass_core material class passed")
"""
    elif kind == "camera":
        body = """
import bpy
obj = bpy.data.objects.get("CAM_HERO")
observed = tuple(round(float(value), 6) for value in obj.location) if obj else None
if observed != (470.0, -650.0, 390.0):
    raise SystemExit(f"fixed camera mismatch: {observed}")
print("fixed camera passed")
"""
    elif kind == "animation":
        body = """
import bpy
required = {
    "outer_shell", "glass_core", "eclipse_disk", "acoustic_membrane",
    "internal_frame", "logic_board", "left_driver", "right_driver",
}
bad = {}
for name in required:
    obj = bpy.data.objects.get(name)
    action = obj.animation_data.action if obj and obj.animation_data else None
    frames = sorted({
        round(float(point.co.x), 6)
        for curve in action.fcurves
        for point in curve.keyframe_points
    }) if action else []
    if frames != [1.0, 120.0]:
        bad[name] = frames
if bpy.context.scene.frame_end != 120 or bad:
    raise SystemExit(f"animation timing mismatch: frame_end={bpy.context.scene.frame_end} bad={bad}")
print("frame 1/120 animation timing passed")
"""
    else:
        raise ValueError(kind)
    path.write_text(body.lstrip(), encoding="utf-8")


def blender_check(
    candidate: Path, output: Path, identifier: str, checker: Path
) -> dict[str, object]:
    return run_command(
        identifier=identifier,
        command=[
            str(BLENDER),
            "--background",
            "--factory-startup",
            "--disable-autoexec",
            str(candidate / "3d/nocturne-one.blend"),
            "--python-exit-code",
            "1",
            "--python",
            str(checker),
        ],
        cwd=candidate,
        output=output,
        timeout=180,
    )


@dataclass(frozen=True)
class Drill:
    identifier: str
    facet: str
    mutated_paths: tuple[Path, ...]
    inject: Callable[[Path], None]
    prepare_fault: Callable[[Path, Path], list[dict[str, object]]]
    local_gate: Callable[[Path, Path, str], dict[str, object]]


def npm_gate(script: str) -> Callable[[Path, Path, str], dict[str, object]]:
    def invoke(candidate: Path, output: Path, identifier: str) -> dict[str, object]:
        return run_command(
            identifier=identifier,
            command=["npm", "run", script],
            cwd=candidate,
            output=output,
            timeout=300,
        )

    return invoke


def browser_gate(
    candidate: Path, output: Path, identifier: str
) -> dict[str, object]:
    build = run_command(
        identifier=f"{identifier}-build",
        command=["npm", "run", "build"],
        cwd=candidate,
        output=output,
        timeout=180,
    )
    browser = (
        run_command(
            identifier=f"{identifier}-browser",
            command=["npm", "run", "test:browser"],
            cwd=candidate,
            output=output,
            timeout=300,
        )
        if build["exit_status"] == 0
        else None
    )
    return {
        "id": identifier,
        "command": [["npm", "run", "build"], ["npm", "run", "test:browser"]],
        "cwd": str(candidate),
        "elapsed_seconds": float(build["elapsed_seconds"])
        + (float(browser["elapsed_seconds"]) if browser else 0.0),
        "exit_status": (
            int(build["exit_status"])
            if build["exit_status"] != 0
            else int(browser["exit_status"])
        ),
        "steps": [build, *([browser] if browser else [])],
    }


def no_prepare(candidate: Path, output: Path) -> list[dict[str, object]]:
    return []


def rebuild_prepare(candidate: Path, output: Path) -> list[dict[str, object]]:
    return [blender_build(candidate, output, "fault-blender-build")]


def checker_gate(kind: str) -> Callable[[Path, Path, str], dict[str, object]]:
    def invoke(candidate: Path, output: Path, identifier: str) -> dict[str, object]:
        checker = output / f"{kind}-contract-check.py"
        if not checker.exists():
            write_blender_check(checker, kind)
        return blender_check(candidate, output, identifier, checker)

    return invoke


def inject_geometry(candidate: Path) -> None:
    replace_once(
        candidate / "3d/build_candidate.py",
        '"base", (0.0, 0.0, 17.0), (320.0, 180.0, 34.0)',
        '"base", (0.0, 0.0, 17.0), (350.0, 180.0, 34.0)',
    )


def inject_material(candidate: Path) -> None:
    replace_once(
        candidate / "3d/build_candidate.py",
        '(61.0, 48.0, 126.0),\n        mats["glass"],',
        '(61.0, 48.0, 126.0),\n        mats["black"],',
    )


def inject_camera(candidate: Path) -> None:
    replace_once(
        candidate / "3d/build_candidate.py",
        "camera.location = (470.0, -650.0, 390.0)",
        "camera.location = (570.0, -650.0, 390.0)",
    )


def inject_mobile(candidate: Path) -> None:
    append_text(
        candidate / "src/client/styles.css",
        "\n/* injected repair drill fault */\n.site-header { min-width: 900px !important; }\n",
    )


def inject_animation(candidate: Path) -> None:
    replace_once(
        candidate / "3d/build_candidate.py",
        'obj.keyframe_insert(data_path="location", frame=120, group="Exploded assembly")',
        'obj.keyframe_insert(data_path="location", frame=110, group="Exploded assembly")',
    )


def inject_reduced_motion(candidate: Path) -> None:
    replace_once(
        candidate / "src/client/main.ts",
        "return !reducedMotion;",
        "return true;",
    )


def inject_glb(candidate: Path) -> None:
    with (candidate / "public/assets/nocturne-one-hero.glb").open("ab") as stream:
        stream.write(b"VISIONMCP_OVERSIZED_GLB_FAULT" * 200_000)


def inject_frame_time(candidate: Path) -> None:
    replace_once(
        candidate / "src/client/renderer.ts",
        "const started = performance.now();\n            renderer.render(world, camera);",
        (
            "const started = performance.now();\n"
            "            const blockedUntil = started + 40;\n"
            "            while (performance.now() < blockedUntil) { /* injected GPU stall */ }\n"
            "            renderer.render(world, camera);"
        ),
    )


def inject_api_idempotency(candidate: Path) -> None:
    replace_once(
        candidate / "src/server/app.ts",
        "return res.status(200).json(reservationResponse(db, existing));",
        "return res.status(201).json(reservationResponse(db, existing));",
    )


def inject_migration(candidate: Path) -> None:
    replace_once(
        candidate / "migrations/001_initial_up.sql",
        "CREATE TABLE IF NOT EXISTS reservations (",
        "CREATE TABL IF NOT EXISTS reservations (",
    )


def inject_accessibility(candidate: Path) -> None:
    replace_once(
        candidate / "src/client/main.ts",
        '        alt="NOCTURNE/ONE asymmetric black arch surrounding a frosted acoustic core and warm eclipse light"\n',
        "",
    )


def inject_unrelated_route(candidate: Path) -> None:
    replace_once(
        candidate / "src/client/main.ts",
        "<h1>The eclipse,<br />made inspectable.</h1>",
        "<h2>The eclipse,<br />made inspectable.</h2>",
    )


def migration_gate(
    candidate: Path, output: Path, identifier: str
) -> dict[str, object]:
    database = output / f"{identifier}.sqlite3"
    return run_command(
        identifier=identifier,
        command=["npm", "run", "db:migrate"],
        cwd=candidate,
        output=output,
        env={"DATABASE_PATH": str(database)},
        timeout=120,
    )


DRILLS = (
    Drill(
        "01-geometry-dimension",
        "geometry dimension",
        (Path("3d/build_candidate.py"), *GENERATED_3D),
        inject_geometry,
        rebuild_prepare,
        npm_gate("test:3d"),
    ),
    Drill(
        "02-incorrect-material-class",
        "incorrect material class",
        (Path("3d/build_candidate.py"), *GENERATED_3D),
        inject_material,
        rebuild_prepare,
        checker_gate("material"),
    ),
    Drill(
        "03-fixed-camera-mismatch",
        "fixed camera mismatch",
        (Path("3d/build_candidate.py"), *GENERATED_3D),
        inject_camera,
        rebuild_prepare,
        checker_gate("camera"),
    ),
    Drill(
        "04-broken-mobile-composition",
        "broken mobile composition",
        (Path("src/client/styles.css"),),
        inject_mobile,
        no_prepare,
        browser_gate,
    ),
    Drill(
        "05-animation-timing-drift",
        "animation timing drift",
        (Path("3d/build_candidate.py"), *GENERATED_3D),
        inject_animation,
        rebuild_prepare,
        checker_gate("animation"),
    ),
    Drill(
        "06-reduced-motion-regression",
        "reduced-motion regression",
        (Path("src/client/main.ts"),),
        inject_reduced_motion,
        no_prepare,
        browser_gate,
    ),
    Drill(
        "07-oversized-glb",
        "oversized GLB",
        (Path("public/assets/nocturne-one-hero.glb"),),
        inject_glb,
        no_prepare,
        npm_gate("test:3d"),
    ),
    Drill(
        "08-shader-frame-time-regression",
        "shader/frame-time regression",
        (Path("src/client/renderer.ts"),),
        inject_frame_time,
        no_prepare,
        browser_gate,
    ),
    Drill(
        "09-api-idempotency",
        "API idempotency",
        (Path("src/server/app.ts"),),
        inject_api_idempotency,
        no_prepare,
        npm_gate("test:api"),
    ),
    Drill(
        "10-database-migration",
        "database migration",
        (Path("migrations/001_initial_up.sql"),),
        inject_migration,
        no_prepare,
        migration_gate,
    ),
    Drill(
        "11-accessibility",
        "accessibility",
        (Path("src/client/main.ts"),),
        inject_accessibility,
        no_prepare,
        browser_gate,
    ),
    Drill(
        "12-unrelated-route-regression",
        "unrelated-route regression",
        (Path("src/client/main.ts"),),
        inject_unrelated_route,
        no_prepare,
        browser_gate,
    ),
)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument(
        "--only",
        action="append",
        default=[],
        help="Run only the named drill ID; may be repeated.",
    )
    args = parser.parse_args()
    source = args.source.expanduser().resolve()
    output = args.output.expanduser().resolve()
    if output.exists():
        raise SystemExit("repair output must not already exist")
    output.mkdir(parents=True)
    baseline_manifest = tree_manifest(source)
    baseline_sha = canonical_sha(baseline_manifest)
    results: list[dict[str, object]] = []
    started_at = datetime.now(UTC).isoformat()

    selected = tuple(
        drill for drill in DRILLS if not args.only or drill.identifier in set(args.only)
    )
    if args.only and len(selected) != len(set(args.only)):
        unknown = sorted(set(args.only) - {drill.identifier for drill in selected})
        raise SystemExit(f"unknown repair drill IDs: {unknown}")

    for drill in selected:
        drill_root = output / drill.identifier
        candidate = drill_root / "candidate"
        logs = drill_root / "logs"
        failed_candidate = drill_root / "failed-candidate"
        logs.mkdir(parents=True)
        copy_source(source, candidate)
        commands: list[dict[str, object]] = []
        install = run_command(
            identifier="fresh-npm-ci",
            command=["npm", "ci"],
            cwd=candidate,
            output=logs,
            timeout=180,
        )
        commands.append(install)
        if install["exit_status"] != 0:
            raise RuntimeError(f"{drill.identifier}: npm ci failed")

        before_fault = tree_manifest(candidate)
        drill.inject(candidate)
        commands.extend(drill.prepare_fault(candidate, logs))
        fault_manifest = tree_manifest(candidate)
        changed_by_fault = sorted(
            set(before_fault) | set(fault_manifest),
            key=str,
        )
        changed_by_fault = [
            path
            for path in changed_by_fault
            if before_fault.get(path) != fault_manifest.get(path)
        ]
        copy_source(candidate, failed_candidate)
        detection = drill.local_gate(candidate, logs, "fault-local-gate")
        commands.append(detection)
        detected = detection["exit_status"] != 0

        restore_paths(source, candidate, drill.mutated_paths)
        repaired_manifest = tree_manifest(candidate)
        restored = repaired_manifest == baseline_manifest
        local_pass = drill.local_gate(candidate, logs, "repair-local-gate")
        commands.append(local_pass)
        global_pass = run_command(
            identifier="repair-global-npm-verify",
            command=["npm", "run", "verify"],
            cwd=candidate,
            output=logs,
            timeout=420,
        )
        commands.append(global_pass)
        final_manifest = tree_manifest(candidate)
        original_restored = final_manifest == baseline_manifest
        unrelated_passed = global_pass["exit_status"] == 0
        status = (
            "PASS"
            if detected
            and restored
            and local_pass["exit_status"] == 0
            and unrelated_passed
            and original_restored
            else "FAIL"
        )
        receipt = {
            "schema_version": "visionmcp.full_runtime_repair_drill.v1",
            "id": drill.identifier,
            "facet": drill.facet,
            "classification": "FULL_RUNTIME",
            "status": status,
            "isolated_candidate": str(candidate),
            "failed_candidate": str(failed_candidate),
            "injected_fault_count": 1,
            "changed_paths": changed_by_fault,
            "detection": {
                "demonstrated": detected,
                "exit_status": detection["exit_status"],
            },
            "bounded_repair": {
                "restored_paths": [path.as_posix() for path in drill.mutated_paths],
                "local_gate_passed": local_pass["exit_status"] == 0,
                "global_gate_passed": unrelated_passed,
            },
            "baseline_manifest_sha256": baseline_sha,
            "pre_fault_manifest_sha256": canonical_sha(before_fault),
            "fault_manifest_sha256": canonical_sha(fault_manifest),
            "post_repair_manifest_sha256": canonical_sha(final_manifest),
            "original_state_restored": original_restored,
            "unrelated_routes_and_features_passed": unrelated_passed,
            "commands": commands,
        }
        receipt["receipt_sha256"] = canonical_sha(receipt)
        receipt_path = drill_root / "repair.receipt.json"
        receipt_path.write_text(
            json.dumps(receipt, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        results.append(receipt)
        print(
            json.dumps(
                {
                    "id": drill.identifier,
                    "status": status,
                    "detected": detected,
                    "restored": original_restored,
                    "global_passed": unrelated_passed,
                },
                sort_keys=True,
            ),
            flush=True,
        )

    summary = {
        "schema_version": "visionmcp.full_runtime_repair_matrix.v1",
        "started_at": started_at,
        "completed_at": datetime.now(UTC).isoformat(),
        "source": str(source),
        "baseline_manifest_sha256": baseline_sha,
        "drill_count": len(results),
        "full_runtime_count": sum(
            result["classification"] == "FULL_RUNTIME" for result in results
        ),
        "replay_count": 0,
        "blocked_count": 0,
        "passed_count": sum(result["status"] == "PASS" for result in results),
        "status": "PASS" if all(result["status"] == "PASS" for result in results) else "FAIL",
        "drills": [
            {
                "id": result["id"],
                "facet": result["facet"],
                "classification": result["classification"],
                "status": result["status"],
                "receipt_sha256": result["receipt_sha256"],
            }
            for result in results
        ],
    }
    summary["receipt_sha256"] = canonical_sha(summary)
    (output / "repair-receipt.json").write_text(
        json.dumps(summary, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    print(json.dumps(summary, indent=2, sort_keys=True))
    if summary["status"] != "PASS":
        raise SystemExit(1)


if __name__ == "__main__":
    main()
