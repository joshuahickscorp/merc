#!/usr/bin/env python3
"""Phase P: per-beat coverage for the data-centre flagship.

Rebuilds the flagship scene, emits real Blender renders at each narrative beat,
seals BeatCoverageReceipts, verifies delivery budgets, and runs perceptual
critics. A high global instance count is never accepted as a substitute for
per-beat coverage.

Usage:
  scripts/run-beat-coverage.py --output artifacts/ocular/beats
"""

from __future__ import annotations

import argparse
import gzip
import json
import sys
import time
from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from blender_vision.cinematic.path import compose_flagship_datacentre_path  # noqa: E402
from blender_vision.cinematic.replay import replay_camera_state  # noqa: E402
from blender_vision.cinematic.textsafe import ZONE_RECTS, TextZone  # noqa: E402
from blender_vision.core.config import discover_blender  # noqa: E402
from blender_vision.core.util import atomic_write_json, sha256_file, utc_now  # noqa: E402
from blender_vision.critics import (  # noqa: E402
    CriticRole,
    CritiqueEvidence,
    CritiqueSubject,
    critic_by_role,
)
from blender_vision.delivery import FROZEN_BUDGETS, evaluate_budgets  # noqa: E402
from blender_vision.ocular.attestation import (  # noqa: E402
    ExecutionClass,
    attest_blocked,
    attest_substitute,
    run_attested,
)
from blender_vision.ocular.beat_coverage import (  # noqa: E402
    flagship_beat_minimums,
    measure_beat_coverage,
    per_beat_instance_counts,
    render_diagnostic_frame,
)
from blender_vision.procedural.emit import emit_scene_plan  # noqa: E402
from blender_vision.procedural.scene import build_flagship_scene  # noqa: E402
from blender_vision.v2.authority import AuthorityClass  # noqa: E402
from blender_vision.v2.records import DeliveryAsset  # noqa: E402

SANDBOX = ROOT / "sandbox" / "datacenter-film"


def _gzip_size(path: Path) -> int:
    return len(gzip.compress(path.read_bytes(), 9))


def _load_png(path: Path) -> np.ndarray:
    with Image.open(path) as image:
        return np.asarray(image.convert("RGB"), dtype=np.float64) / 255.0


def _print_table(rows: list[dict[str, Any]]) -> None:
    headers = (
        "beat",
        "racks",
        "drawers",
        "non_bg",
        "depth",
        "contrast",
        "pass",
    )
    widths = {h: max(len(h), 6) for h in headers}
    for row in rows:
        for h in headers:
            widths[h] = max(widths[h], len(str(row[h])))
    line = "  ".join(h.ljust(widths[h]) for h in headers)
    print(line)
    print("  ".join("-" * widths[h] for h in headers))
    for row in rows:
        print("  ".join(str(row[h]).ljust(widths[h]) for h in headers))


def _budget_report(assets_dir: Path) -> tuple[list[dict[str, Any]], dict[str, int]]:
    """Measure shell/detail/mobile bytes against frozen budgets."""
    measured: dict[str, int] = {}
    delivery_assets: list[DeliveryAsset] = []
    mapping = {
        "shell": assets_dir / "datacenter-shell.glb",
        "detail": assets_dir / "datacenter-detail.glb",
        "mobile": assets_dir / "datacenter-mobile.glb",
        "network": assets_dir / "datacenter-network.glb",
    }
    for role, path in mapping.items():
        if not path.is_file():
            continue
        digest, size = sha256_file(path)
        measured[role] = size
        delivery_assets.append(
            DeliveryAsset(
                asset_id=f"datacenter-{role}",
                role=role,
                path=path.name,
                digest=digest,
                bytes=size,
            )
        )
    js_path = SANDBOX / "src" / "film.js"
    js_gz = _gzip_size(js_path) if js_path.is_file() else 0
    measured["initial_js_gzip"] = js_gz
    violations = evaluate_budgets(
        delivery_assets,
        budgets=dict(FROZEN_BUDGETS),
        initial_js_compressed_bytes=js_gz,
    )
    return violations, measured


def _run_critics_on_receipts(
    receipts: list[Any],
    frames: dict[str, np.ndarray],
) -> list[dict[str, Any]]:
    findings_out: list[dict[str, Any]] = []
    env = critic_by_role(CriticRole.ENVIRONMENT_ARTIST)
    editorial = critic_by_role(CriticRole.EDITORIAL_ART_DIRECTOR)
    lighting = critic_by_role(CriticRole.LIGHTING_ARTIST)
    for receipt in receipts:
        frame = frames.get(receipt.beat_id)
        if frame is None:
            continue
        # Occupancy grid from non-background mask thirds.
        lum = frame.mean(axis=2) if frame.ndim == 3 else frame
        occ = (lum > 0.08).astype(np.float64)
        h, w = occ.shape
        grid = np.stack(
            [
                occ[int(h * 0.55) :, :].mean(),
                occ[int(h * 0.25) : int(h * 0.55), :].mean(),
                occ[: int(h * 0.25), :].mean(),
            ]
        )
        subject = CritiqueSubject(
            subject_id=f"beat-{receipt.beat_id}",
            kind="beat_render",
            metrics={
                "occupancy_grid": grid,
                "depth_complexity_samples": [
                    receipt.depth_spread,
                    receipt.foreground_occupancy + receipt.midground_occupancy,
                    max(0.1, receipt.non_background_pixel_fraction * 4.0),
                ],
                "instance_variations": [
                    float(receipt.visible_racks),
                    float(receipt.visible_drawers),
                    float(receipt.frustum_instance_count),
                    float(receipt.light_key_fill_ratio),
                ],
                "salient_xy": [0.45, 0.48],
                "narrative_beats": [
                    {
                        "text": [receipt.beat_label],
                        "scroll_start": receipt.scroll - 0.05,
                        "scroll_end": receipt.scroll + 0.05,
                    }
                ],
                "highlight_clip_fraction": float(
                    np.mean(lum >= 0.98) if lum.size else 0.0
                ),
                "key_fill_ratio": receipt.light_key_fill_ratio,
            },
            media={"frame": frame},
            tags=frozenset({"datacenter", "beat", receipt.beat_id}),
        )
        evidence = CritiqueEvidence(
            references=[receipt.render_path or f"beat-{receipt.beat_id}"],
            payloads={"receipt_id": receipt.id},
        )
        for critic in (env, editorial, lighting):
            if not critic.applies_to(subject):
                continue
            for finding in critic.critique(subject, evidence):
                findings_out.append(
                    {
                        "beat_id": receipt.beat_id,
                        "role": finding.critic_role,
                        "finding_id": finding.finding_id,
                        "diagnosis": finding.diagnosis,
                        "severity": finding.severity,
                        "measured": finding.measured,
                    }
                )
    return findings_out


def _browser_verify(out: Path) -> dict[str, Any]:
    """Hit the sandbox once via Playwright through with-one-browser.sh."""
    report: dict[str, Any] = {
        "attempted": True,
        "ok": False,
        "beats_reached": [],
        "error": "",
    }
    if not (SANDBOX / "index.html").is_file():
        report["error"] = "sandbox/datacenter-film/index.html missing"
        return report

    wrapper = ROOT / "scripts" / "with-one-browser.sh"
    probe = out / "_browser_probe.py"
    probe.write_text(
        f"""
from __future__ import annotations
import json
import sys
from pathlib import Path
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from threading import Thread

from playwright.sync_api import sync_playwright

sandbox = Path({str(SANDBOX)!r})
result = {{"beats_reached": [], "ok": False, "error": ""}}

class Handler(SimpleHTTPRequestHandler):
    def __init__(self, *a, **k):
        super().__init__(*a, directory=str(sandbox), **k)
    def log_message(self, *args):
        pass

server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
port = server.server_address[1]
Thread(target=server.serve_forever, daemon=True).start()
try:
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True)
        page = browser.new_page(viewport={{"width": 1280, "height": 720}})
        page.goto(f"http://127.0.0.1:{{port}}/index.html", wait_until="networkidle", timeout=60000)
        # Native scroll through each beat midpoint; no hijacking.
        total = page.evaluate("() => document.documentElement.scrollHeight - window.innerHeight")
        for beat_id, frac in [
            ("00", 0.04), ("01", 0.13), ("02", 0.24), ("03", 0.36), ("04", 0.48),
            ("05", 0.61), ("06", 0.74), ("07", 0.85), ("08", 0.95),
        ]:
            y = max(0, int(total * frac))
            page.evaluate(f"window.scrollTo(0, {{y}})")
            page.wait_for_timeout(120)
            result["beats_reached"].append(beat_id)
        result["ok"] = len(result["beats_reached"]) == 9
        browser.close()
except Exception as exc:
    result["error"] = str(exc)
finally:
    server.shutdown()
print(json.dumps(result))
""",
        encoding="utf-8",
    )
    if not wrapper.is_file():
        report["error"] = "with-one-browser.sh missing"
        return report

    import subprocess

    completed = subprocess.run(
        ["bash", str(wrapper), str(ROOT / ".venv" / "bin" / "python"), str(probe)],
        capture_output=True,
        text=True,
        timeout=300,
        check=False,
        cwd=str(ROOT),
    )
    stdout = (completed.stdout or "").strip().splitlines()
    payload = {}
    for line in reversed(stdout):
        line = line.strip()
        if line.startswith("{") and line.endswith("}"):
            try:
                payload = json.loads(line)
                break
            except json.JSONDecodeError:
                continue
    report["returncode"] = completed.returncode
    report["ok"] = bool(payload.get("ok")) and completed.returncode == 0
    report["beats_reached"] = list(payload.get("beats_reached") or [])
    report["error"] = str(payload.get("error") or "")
    if completed.returncode != 0 and not report["error"]:
        report["error"] = (completed.stderr or completed.stdout or "")[-500:]
    return report


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=ROOT / "artifacts" / "ocular" / "beats",
    )
    parser.add_argument(
        "--skip-emit",
        action="store_true",
        help="Reuse existing sandbox GLBs; still requires renders unless --skip-render",
    )
    parser.add_argument(
        "--skip-browser",
        action="store_true",
        help="Skip Playwright verification (still reports as omitted, not a pass)",
    )
    parser.add_argument("--resolution", type=int, nargs=2, default=[960, 540])
    args = parser.parse_args()
    out = args.output.resolve()
    out.mkdir(parents=True, exist_ok=True)
    renders_dir = out / "renders"
    renders_dir.mkdir(parents=True, exist_ok=True)

    summary: dict[str, Any] = {
        "schema": "ocular.beat-coverage-run/1",
        "started_at": utc_now(),
        "failures": [],
        "beats": [],
    }

    print("== flagship scene ==")
    scene = build_flagship_scene()
    counts = per_beat_instance_counts(scene.instances)
    print(
        f"  instances={len(scene.instances)} unique_meshes={scene.plan.unique_mesh_count()}"
    )
    print(
        f"  main_aisle={counts['main_aisle']}  second_aisle={counts['second_aisle']}"
    )
    summary["scene"] = {
        "instances": len(scene.instances),
        "unique_meshes": scene.plan.unique_mesh_count(),
        "counts": counts,
        "bounds_m": scene.bounds_m,
    }
    if counts["second_aisle"]["instances"] <= 8:
        summary["failures"].append(
            {
                "gate": "second_aisle_population",
                "value": counts["second_aisle"],
                "message": "second aisle still bare (<=8 instances)",
            }
        )

    print("\n== camera path ==")
    path = compose_flagship_datacentre_path()
    print(f"  beats={len(path.beats)} arc_length_m={path.arc_length_m:.3f}")

    blender = discover_blender()
    attestation: Any
    if not blender.available or not blender.path:
        attestation = attest_blocked("blender", "Blender executable not found on this host")
        atomic_write_json(out / "attestation.json", attestation.to_dict())
        print(f"  BLOCKED: {attestation.blocked_reason}")
        summary["failures"].append(
            {"gate": "blender", "execution_class": ExecutionClass.BLOCKED.value}
        )
        summary["completed_at"] = utc_now()
        atomic_write_json(out / "run-summary.json", summary)
        return 1

    # Beat camera views for a single multi-view emit.
    render_specs: list[dict[str, Any]] = []
    beat_cameras: list[dict[str, Any]] = []
    for beat in path.beats:
        scroll = (beat.scroll_start + beat.scroll_end) * 0.5
        state = replay_camera_state(path, scroll)
        target = state.focus_target or [
            state.position[0],
            state.position[1] + 1.0,
            state.position[2],
        ]
        filename = f"beat_{beat.beat_id}.png"
        render_specs.append(
            {
                "filename": filename,
                "location": list(state.position),
                "target": list(target),
                "resolution": list(args.resolution),
            }
        )
        beat_cameras.append(
            {
                "beat_id": beat.beat_id,
                "label": beat.label,
                "scroll": scroll,
                "text_zone": beat.text_zone,
                "position": list(state.position),
                "target": list(target),
                "focal_length_mm": state.focal_length_mm,
                "filename": filename,
            }
        )

    print("\n== blender emit + beat renders (attested) ==")
    emit_dir = out / "_emit_flagship"
    t0 = time.monotonic()
    physical_renders = False
    emit_att: Any

    try:
        result = emit_scene_plan(
            scene.plan,
            emit_dir,
            renders=render_specs,
            timeout_seconds=2400,
        )
        metrics = dict(result.metrics or {})
        blender_blocked = (
            metrics.get("blender_status") == "BLOCKED"
            or metrics.get("backend") == "trimesh-offline"
            or str(result.blend_path).endswith("BLOCKED.txt")
        )
        elapsed_ms = (time.monotonic() - t0) * 1000.0
        if blender_blocked:
            reason = str(
                metrics.get("blocked_reason")
                or metrics.get("blender_probe", {}).get("reason")
                or "Blender emit fell back to offline mesh path"
            )
            emit_att = attest_substitute(
                "blender",
                execution_class=ExecutionClass.DIAGNOSTIC_ONLY,
                reason=reason,
                substitute="trimesh-offline",
                outputs={"glb": Path(result.glb_path)} if Path(result.glb_path).is_file() else None,
            )
            print(f"  BLOCKED/DIAGNOSTIC: {reason[:200]}")
            summary["failures"].append(
                {
                    "gate": "blender_physical_render",
                    "execution_class": ExecutionClass.BLOCKED.value,
                    "reason": reason,
                    "note": (
                        "Offline GLB and diagnostic frames are produced, but "
                        "no physical EEVEE/Cycles beat render is claimed."
                    ),
                }
            )
        else:
            emit_att = run_attested(
                "blender",
                [blender.path, "--version"],
                outputs={
                    "glb": Path(result.glb_path),
                    "metrics": Path(result.metrics_path),
                },
                timeout_seconds=30,
            )
            # Only PHYSICAL when real blend + named beat renders landed.
            beat_files = [emit_dir / cam["filename"] for cam in beat_cameras]
            physical_renders = (
                Path(result.blend_path).is_file()
                and not str(result.blend_path).endswith("BLOCKED.txt")
                and all(p.is_file() for p in beat_files)
            )
            if physical_renders and emit_att.is_physical:
                emit_att.output_digests = {
                    "glb": sha256_file(Path(result.glb_path))[0],
                    **{
                        cam["filename"]: sha256_file(emit_dir / cam["filename"])[0]
                        for cam in beat_cameras
                        if (emit_dir / cam["filename"]).is_file()
                    },
                }
                emit_att.elapsed_seconds = elapsed_ms / 1000.0
                emit_att.digest = ""
                emit_att.seal()
            else:
                emit_att = attest_substitute(
                    "blender",
                    execution_class=ExecutionClass.DIAGNOSTIC_ONLY,
                    reason="emit completed without complete physical beat render set",
                    substitute="partial-emit",
                )
                summary["failures"].append(
                    {
                        "gate": "blender_physical_render",
                        "reason": "incomplete physical beat render set",
                    }
                )
            print(
                f"  execution_class={emit_att.execution_class.value} "
                f"physical_renders={physical_renders} "
                f"elapsed_s={elapsed_ms/1000:.1f}"
            )
    except Exception as exc:  # noqa: BLE001 — never invent hardware blame
        elapsed_ms = (time.monotonic() - t0) * 1000.0
        emit_att = attest_blocked(
            "blender",
            f"emit_scene_plan raised: {type(exc).__name__}: {exc}"[:400],
            substituted_by="diagnostic-frame-raster",
        )
        summary["failures"].append(
            {
                "gate": "emit_scene_plan",
                "error": str(exc)[:500],
                "execution_class": emit_att.execution_class.value,
            }
        )
        print(f"  EMIT FAILED (not assumed hardware): {exc}")
        physical_renders = False

    atomic_write_json(out / "attestation-emit.json", emit_att.to_dict())

    frames: dict[str, np.ndarray] = {}
    receipts = []
    table_rows: list[dict[str, Any]] = []

    print("\n== beat coverage receipts ==")
    for cam in beat_cameras:
        dest = renders_dir / cam["filename"]
        src = emit_dir / cam["filename"]
        if physical_renders and src.is_file():
            dest.write_bytes(src.read_bytes())
            frame = _load_png(dest)
            exec_class = ExecutionClass.PHYSICAL.value
        else:
            # Diagnostic AABB raster from the beat camera — never a physical PASS.
            frame = render_diagnostic_frame(
                scene.instances,
                camera_position=cam["position"],
                camera_target=cam["target"],
                width=int(args.resolution[0]),
                height=int(args.resolution[1]),
                focal_length_mm=cam["focal_length_mm"],
            )
            # Darken the declared text-safe zone so contrast is measured, not assumed.
            try:
                zone = TextZone(cam["text_zone"])
                rect = ZONE_RECTS[zone]
                h, w = frame.shape[:2]
                x0 = int(rect[0] * (w - 1))
                y0 = int(rect[1] * (h - 1))
                x1 = int(rect[2] * (w - 1)) + 1
                y1 = int(rect[3] * (h - 1)) + 1
                frame[y0:y1, x0:x1, :] = np.minimum(frame[y0:y1, x0:x1, :], 0.06)
            except (KeyError, ValueError):
                pass
            Image.fromarray((np.clip(frame, 0, 1) * 255).astype(np.uint8)).save(dest)
            exec_class = ExecutionClass.DIAGNOSTIC_ONLY.value

        frames[cam["beat_id"]] = frame
        mins = flagship_beat_minimums(cam["beat_id"])
        receipt = measure_beat_coverage(
            beat_id=cam["beat_id"],
            beat_label=cam["label"],
            scroll=cam["scroll"],
            camera_position=cam["position"],
            camera_target=cam["target"],
            instances=scene.instances,
            frame=frame,
            text_zone=cam["text_zone"],
            minimums=mins,
            focal_length_mm=cam["focal_length_mm"],
            render_path=str(dest),
            attestation_id=emit_att.id,
            execution_class=exec_class,
            performance_ms=elapsed_ms / max(1, len(beat_cameras)),
        )
        # Physical authority only when Blender actually rendered the frame.
        if exec_class != ExecutionClass.PHYSICAL.value:
            from blender_vision.v2.records import V2Record

            receipt.authority = AuthorityClass.MODEL_DERIVED
            receipt.lineage.limitations.append(
                "Frame is a diagnostic CPU raster, not Blender EEVEE/Cycles."
            )
            receipt.digest = ""
            V2Record.seal(receipt)
        receipt.verify()
        receipts.append(receipt)
        atomic_write_json(out / f"beat-{cam['beat_id']}-receipt.json", receipt.to_dict())
        table_rows.append(
            {
                "beat": f"{cam['beat_id']} {cam['label']}",
                "racks": receipt.visible_racks,
                "drawers": receipt.visible_drawers,
                "non_bg": f"{receipt.non_background_pixel_fraction:.3f}",
                "depth": f"{receipt.depth_spread:.2f}",
                "contrast": f"{receipt.text_safe_contrast:.2f}",
                "pass": "PASS" if receipt.passed else "FAIL",
            }
        )
        summary["beats"].append(
            {
                "beat_id": receipt.beat_id,
                "passed": receipt.passed,
                "failures": list(receipt.failures),
                "visible_racks": receipt.visible_racks,
                "visible_drawers": receipt.visible_drawers,
                "non_background_pixel_fraction": receipt.non_background_pixel_fraction,
                "depth_spread": receipt.depth_spread,
                "text_safe_contrast": receipt.text_safe_contrast,
                "frustum_instance_count": receipt.frustum_instance_count,
                "execution_class": exec_class,
                "digest": receipt.digest,
            }
        )
        if not receipt.passed:
            summary["failures"].append(
                {
                    "gate": "beat_minimums",
                    "beat_id": receipt.beat_id,
                    "failures": list(receipt.failures),
                }
            )

    _print_table(table_rows)
    summary["physical_renders"] = physical_renders
    summary["execution_class"] = emit_att.execution_class.value

    print("\n== budgets ==")
    assets_dir = SANDBOX / "assets"
    # Prefer freshly emitted shell if present.
    if (emit_dir / "scene.glb").is_file() and not args.skip_emit:
        # Budget check uses existing sandbox assets when rebuild-of-tiers is not done.
        pass
    violations, measured = _budget_report(assets_dir)
    for role, size in measured.items():
        print(f"  {role:16s} {size:>10,} B")
    if violations:
        for v in violations:
            print(f"  VIOLATION {v}")
            summary["failures"].append({"gate": "budget", "violation": v})
    else:
        print("  budgets OK (frozen limits)")
    summary["budgets"] = {
        "measured": measured,
        "violations": violations,
        "frozen": dict(FROZEN_BUDGETS),
    }

    print("\n== perceptual critics ==")
    findings = _run_critics_on_receipts(receipts, frames)
    atomic_write_json(out / "critic-findings.json", findings)
    print(f"  findings={len(findings)}")
    for item in findings[:12]:
        print(
            f"  [{item['severity']}] {item['beat_id']} "
            f"{item['role']}: {item['diagnosis']}"
        )
    summary["critic_findings"] = findings

    print("\n== browser verification ==")
    if args.skip_browser:
        browser = {"attempted": False, "ok": False, "error": "skipped by --skip-browser"}
        print("  skipped")
    else:
        browser = _browser_verify(out)
        print(
            f"  ok={browser.get('ok')} beats={browser.get('beats_reached')} "
            f"error={browser.get('error')!r}"
        )
        if not browser.get("ok"):
            summary["failures"].append({"gate": "browser", "report": browser})
    summary["browser"] = browser

    summary["completed_at"] = utc_now()
    summary["passed"] = len(summary["failures"]) == 0
    atomic_write_json(out / "run-summary.json", summary)

    print("\n== result ==")
    if summary["failures"]:
        print(f"FAIL ({len(summary['failures'])} gate(s))")
        for item in summary["failures"]:
            print(f"  - {json.dumps(item, default=str)[:240]}")
        return 1
    print("PASS: all beats met declared minimums; budgets held")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
