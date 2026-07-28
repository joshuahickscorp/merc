from __future__ import annotations

import argparse
import ast
import json
import sys
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]

REQUIRED_PATHS = {
    "README.md",
    "LICENSE",
    "SECURITY.md",
    "MODEL_LICENSES.json",
    "docker/vision.Dockerfile",
    "docker/review.Dockerfile",
    "docker/compose.yaml",
    "blender_worker/entry.py",
    "docs/RELEASE.md",
    "docs/SECURITY_REVIEW.md",
    "docs/V1_COMPLIANCE.md",
    "schemas/backend.schema.json",
    "schemas/camera-refinement.schema.json",
    "schemas/blender-job.schema.json",
    "schemas/component.schema.json",
    "schemas/feature.schema.json",
    "schemas/geometry-evidence.schema.json",
    "schemas/job-manifest.schema.json",
    "schemas/material-profile.schema.json",
    "schemas/measurement-grid.schema.json",
    "schemas/measurement.schema.json",
    "schemas/model-approval.schema.json",
    "schemas/project.schema.json",
    "schemas/receipt.schema.json",
    "schemas/reference-mask.schema.json",
    "schemas/repair-proposal.schema.json",
    "schemas/worker-capability.schema.json",
    "schemas/worker-lease.schema.json",
    "src/blender_vision/acceptance/receipts.py",
    "src/blender_vision/artifacts/transfer.py",
    "src/blender_vision/cameras/solver.py",
    "src/blender_vision/cameras/refinement.py",
    "src/blender_vision/comparison/metrics.py",
    "src/blender_vision/datasets/store.py",
    "src/blender_vision/evidence/masks.py",
    "src/blender_vision/evidence/measurements.py",
    "src/blender_vision/features/detector.py",
    "src/blender_vision/materials/store.py",
    "src/blender_vision/mcp/server.py",
    "src/blender_vision/models/store.py",
    "src/blender_vision/optimization/engine.py",
    "src/blender_vision/parametric/components.py",
    "src/blender_vision/review/server.py",
    "src/blender_vision/scheduling/distributed.py",
    "src/blender_vision/scheduling/worker.py",
    "src/blender_vision/vision/pipeline.py",
    "src/blender_vision/vision/vggt.py",
    "src/blender_vision/visual/oracle.py",
}

REQUIRED_WORKER_OPERATIONS = {
    "inspect_scene",
    "validate_scene",
    "import_asset",
    "create_component",
    "update_component",
    "apply_constraints",
    "create_camera",
    "apply_camera_solution",
    "render_passes",
    "export_glb",
    "export_blend",
    "generate_lod",
    "save_checkpoint",
    "repair_mac_studio_grille",
    "generate_components",
    "generate_synthetic_dataset",
    "generate_calibration_benchmark",
}

FORBIDDEN_SUFFIXES = {".blend", ".db", ".sqlite", ".pt", ".pth", ".ckpt", ".safetensors"}


def assigned_string_set(path: Path, name: str) -> set[str]:
    tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
    for node in tree.body:
        if isinstance(node, ast.Assign) and any(
            isinstance(target, ast.Name) and target.id == name for target in node.targets
        ):
            value = ast.literal_eval(node.value)
            if isinstance(value, set) and all(isinstance(item, str) for item in value):
                return value
    raise ValueError(f"{path} does not assign a literal string set named {name}")


def verify_tree() -> dict[str, object]:
    missing = sorted(path for path in REQUIRED_PATHS if not (ROOT / path).is_file())
    worker_operations = assigned_string_set(ROOT / "blender_worker/entry.py", "ALLOWED_OPERATIONS")
    missing_operations = sorted(REQUIRED_WORKER_OPERATIONS - worker_operations)
    registry = json.loads((ROOT / "MODEL_LICENSES.json").read_text(encoding="utf-8"))
    policy = registry.get("policy", {})
    policy_failures = []
    if policy.get("silent_downloads") is not False:
        policy_failures.append("model registry must disable silent downloads")
    if policy.get("commercial_release_requires_verified_license") is not True:
        policy_failures.append("model registry must require commercial license review")
    forbidden = sorted(
        str(path.relative_to(ROOT))
        for root in (ROOT / "src", ROOT / "benchmarks", ROOT / "examples")
        if root.exists()
        for path in root.rglob("*")
        if path.is_file() and path.suffix.lower() in FORBIDDEN_SUFFIXES
    )
    valid = not missing and not missing_operations and not policy_failures and not forbidden
    return {
        "valid": valid,
        "missing_paths": missing,
        "missing_worker_operations": missing_operations,
        "model_policy_failures": policy_failures,
        "forbidden_distributable_files": forbidden,
        "worker_operation_count": len(worker_operations),
        "model_registry_count": len(registry.get("models", [])),
    }


def verify_wheel(path: Path) -> dict[str, object]:
    with zipfile.ZipFile(path) as archive:
        names = set(archive.namelist())
    required = {
        "blender_vision/blender/standalone_worker.py",
        "blender_vision/MODEL_LICENSES.json",
        "blender_vision/docs/V1_COMPLIANCE.md",
        "blender_vision/schemas/reference-mask.schema.json",
        "blender_vision/vision/vggt.py",
    }
    missing = sorted(required - names)
    forbidden = sorted(name for name in names if Path(name).suffix.lower() in FORBIDDEN_SUFFIXES)
    return {
        "valid": not missing and not forbidden,
        "path": str(path.resolve()),
        "member_count": len(names),
        "missing": missing,
        "forbidden": forbidden,
    }


def verify_project(path: Path) -> dict[str, object]:
    sys.path.insert(0, str(ROOT / "src"))
    from blender_vision.acceptance.receipts import verify_receipt
    from blender_vision.artifacts.store import ArtifactStore
    from blender_vision.projects.store import ProjectStore

    project = ProjectStore.open(path)
    with project.connection() as connection:
        integrity = connection.execute("PRAGMA integrity_check").fetchone()[0]
        receipt = connection.execute(
            "SELECT digest FROM receipts ORDER BY created_at DESC LIMIT 1"
        ).fetchone()
    receipt_verification = (
        verify_receipt(ArtifactStore(project).path_for(receipt["digest"]), project=project)
        if receipt
        else None
    )
    return {
        "valid": integrity == "ok"
        and (receipt_verification is None or receipt_verification["valid"]),
        "sqlite_integrity": integrity,
        "status": project.status(),
        "latest_receipt": receipt_verification,
    }


def main() -> None:
    parser = argparse.ArgumentParser(description="Verify Maximal V1 release surfaces")
    parser.add_argument("--wheel", type=Path)
    parser.add_argument("--project", type=Path)
    args = parser.parse_args()
    report: dict[str, object] = {"tree": verify_tree()}
    if args.wheel:
        report["wheel"] = verify_wheel(args.wheel)
    if args.project:
        report["project"] = verify_project(args.project)
    report["valid"] = all(
        section.get("valid") is True
        for section in report.values()
        if isinstance(section, dict)
    )
    print(json.dumps(report, indent=2, sort_keys=True))
    if not report["valid"]:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
