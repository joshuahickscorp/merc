from __future__ import annotations

import sys
import zipfile
from pathlib import Path

REQUIRED = {
    "blender_vision/blender/standalone_worker.py",
    "blender_vision/benchmarks/data/mac_studio.json",
    "blender_vision/benchmarks/data/calibration/benchmark.json",
    "blender_vision/benchmarks/data/calibration/create_scene.py",
    "blender_vision/review/static/index.html",
    "blender_vision/review/static/app.js",
    "blender_vision/schemas/receipt.schema.json",
    "blender_vision/schemas/camera-refinement.schema.json",
    "blender_vision/schemas/material-profile.schema.json",
    "blender_vision/schemas/measurement-grid.schema.json",
    "blender_vision/schemas/reference-mask.schema.json",
    "blender_vision/MODEL_LICENSES.json",
    "blender_vision/SECURITY.md",
    "blender_vision/docs/SECURITY_REVIEW.md",
    "blender_vision/docs/RELEASE.md",
    "blender_vision/docs/V1_COMPLIANCE.md",
    "blender_vision/vision/vggt.py",
    "blender_vision/cameras/refinement.py",
    "blender_vision/scheduling/worker.py",
}


def main() -> None:
    directory = Path(sys.argv[1] if len(sys.argv) > 1 else "dist")
    wheels = sorted(directory.glob("*.whl"), key=lambda path: path.stat().st_mtime)
    if not wheels:
        raise SystemExit(f"no wheel found in {directory}")
    with zipfile.ZipFile(wheels[-1]) as archive:
        names = set(archive.namelist())
    missing = sorted(REQUIRED - names)
    forbidden = sorted(
        name
        for name in names
        if name.endswith((".blend", ".sqlite", ".db", ".pt", ".pth", ".ckpt", ".safetensors"))
    )
    if missing or forbidden:
        raise SystemExit(f"wheel verification failed: missing={missing}, forbidden={forbidden}")
    print(f"verified {wheels[-1].name}: {len(names)} files")


if __name__ == "__main__":
    main()
