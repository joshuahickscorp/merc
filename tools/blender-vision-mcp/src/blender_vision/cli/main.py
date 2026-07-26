from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path
from typing import Any

from blender_vision.acceptance.receipts import verify_receipt
from blender_vision.backends.registry import BackendRegistry
from blender_vision.core.config import default_projects_root, doctor_report
from blender_vision.core.errors import BlenderVisionError
from blender_vision.core.models import EvidenceClass, FidelityLevel
from blender_vision.core.util import sha256_file
from blender_vision.projects.store import PROJECT_DIRECTORIES, ProjectStore, slugify
from blender_vision.scheduling.coordinator import Coordinator


def _json(value: Any) -> None:
    print(json.dumps(value, indent=2, sort_keys=True, ensure_ascii=False))


def _project(path: str) -> ProjectStore:
    return ProjectStore.open(Path(path))


def _json_argument(value: str) -> Any:
    candidate = Path(value).expanduser()
    try:
        is_file = candidate.is_file()
    except OSError:
        # Inline JSON can be longer than the platform filename limit.  Treat
        # path-probing failures as evidence that the argument is data, then
        # let json.loads report any actual syntax error.
        is_file = False
    text = candidate.read_text(encoding="utf-8") if is_file else value
    return json.loads(text)


def _run(
    project: ProjectStore, operation: str, config: dict[str, Any], asynchronous: bool
) -> dict[str, Any]:
    coordinator = Coordinator(project)
    if asynchronous:
        return {"job_id": coordinator.enqueue(operation, config), "status": "queued"}
    return coordinator.run(operation, config)


def _verify_project(project: ProjectStore) -> dict[str, Any]:
    missing = [
        directory for directory in PROJECT_DIRECTORIES if not (project.root / directory).is_dir()
    ]
    corrupt: list[dict[str, str]] = []
    missing_artifacts: list[str] = []
    with project.connection() as connection:
        rows = connection.execute("SELECT digest, relative_path FROM artifacts").fetchall()
        integrity = connection.execute("PRAGMA integrity_check").fetchone()[0]
    for row in rows:
        path = project.root / row["relative_path"]
        if not path.is_file():
            missing_artifacts.append(row["digest"])
            continue
        actual, _ = sha256_file(path)
        if actual != row["digest"]:
            corrupt.append(
                {"expected": row["digest"], "actual": actual, "path": row["relative_path"]}
            )
    return {
        "valid": not missing and not missing_artifacts and not corrupt and integrity == "ok",
        "sqlite_integrity": integrity,
        "missing_directories": missing,
        "missing_artifacts": missing_artifacts,
        "corrupt_artifacts": corrupt,
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="bvmcp", description="Blender Vision MCP coordinator")
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("doctor", help="discover Blender, vision, and media capabilities")
    capabilities = sub.add_parser("capabilities", help="show registered model backends")
    capabilities.set_defaults(action="capabilities")

    capability = sub.add_parser(
        "capability", help="evaluate the evidence-bound 0–110 capability authority"
    )
    capability_sub = capability.add_subparsers(dest="capability_command", required=True)
    capability_list = capability_sub.add_parser("list")
    capability_list.add_argument("--domain", choices=("app", "3d", "system"))
    capability_show = capability_sub.add_parser("show")
    capability_show.add_argument("facet")
    capability_evaluate = capability_sub.add_parser("evaluate")
    capability_evaluate.add_argument("selector", help="facet ID, app, 3d, system, or all")
    capability_evaluate.add_argument(
        "--evidence", action="append", default=[], help="capability evidence JSON"
    )
    capability_report = capability_sub.add_parser("report")
    capability_report.add_argument(
        "--evidence", action="append", default=[], help="capability evidence JSON"
    )
    capability_report.add_argument(
        "--evidence-dir", help="directory containing capability evidence JSON files"
    )
    capability_report.add_argument(
        "--output",
        default="artifacts/100-plus/capability-report.json",
        help="receipt-bound report destination",
    )
    capability_verify = capability_sub.add_parser("verify-report")
    capability_verify.add_argument(
        "path",
        nargs="?",
        default="artifacts/100-plus/capability-report.json",
    )

    application = sub.add_parser("app", help="compile governed application reference packets")
    application_sub = application.add_subparsers(dest="application_command", required=True)
    application_check = application_sub.add_parser("check")
    application_check.add_argument("packet")
    application_compile = application_sub.add_parser("compile")
    application_compile.add_argument("packet")
    application_compile.add_argument("--workspace", required=True)
    application_compile.add_argument("--candidate-id", required=True)
    application_compile.add_argument(
        "--mode",
        choices=("draft", "promotion_candidate"),
        default="draft",
    )
    application_verify = application_sub.add_parser("verify")
    application_verify.add_argument("candidate")
    application_benchmark = application_sub.add_parser("benchmark")
    application_benchmark.add_argument("--manifest")
    application_benchmark.add_argument("--output", required=True)
    application_benchmark.add_argument(
        "--case",
        action="append",
        default=[],
        help="run only this fixed case; repeat for multiple cases",
    )

    browser = sub.add_parser(
        "browser", help="run governed cross-browser and environment-state verification"
    )
    browser_sub = browser.add_subparsers(dest="browser_command", required=True)
    browser_benchmark = browser_sub.add_parser("benchmark")
    browser_benchmark.add_argument("--manifest")
    browser_benchmark.add_argument("--output", required=True)

    glb = sub.add_parser("glb", help="validate GLB 2.0 structure without external fetches")
    glb_sub = glb.add_subparsers(dest="glb_command", required=True)
    glb_validate = glb_sub.add_parser("validate")
    glb_validate.add_argument("path")
    glb_validate.add_argument("--required-node", action="append", default=[])
    glb_validate.add_argument("--required-mesh", action="append", default=[])

    model = sub.add_parser("model", help="govern manually acquired model checkpoints")
    model_sub = model.add_subparsers(dest="model_command", required=True)
    model_approve = model_sub.add_parser("approve-source")
    model_approve.add_argument("name")
    model_approve.add_argument("source_url")
    model_approve.add_argument("expected_sha256")
    model_approve.add_argument("--project", required=True)
    model_approve.add_argument("--license-record", required=True, help="JSON object or file")
    model_approve.add_argument("--approved-by", required=True)
    model_approve.add_argument("--reason", required=True)
    model_import = model_sub.add_parser("import-checkpoint")
    model_import.add_argument("approval_id")
    model_import.add_argument("checkpoint")
    model_import.add_argument("--project", required=True)
    model_import.add_argument("--revision", required=True)
    model_list = model_sub.add_parser("list")
    model_list.add_argument("--project", required=True)

    serve = sub.add_parser("serve", help="start the MCP server")
    serve.add_argument("--transport", choices=("stdio", "sse", "streamable-http"), default="stdio")
    serve.add_argument("--projects-root", default=str(default_projects_root()))

    daemon = sub.add_parser("daemon", help="run a local SQLite job coordinator")
    daemon.add_argument("--project", required=True)
    daemon.add_argument("--once", action="store_true")
    daemon.add_argument("--poll-seconds", type=float, default=0.25)

    project = sub.add_parser("project", help="project lifecycle")
    project_sub = project.add_subparsers(dest="project_command", required=True)
    create = project_sub.add_parser("create")
    create.add_argument("name")
    create.add_argument("--root")
    create.add_argument(
        "--target-fidelity", choices=[item.value for item in FidelityLevel], default="L3"
    )
    create.add_argument("--scene")
    status = project_sub.add_parser("status")
    status.add_argument("--project", required=True)
    verify = project_sub.add_parser("verify")
    verify.add_argument("--project", required=True)
    project_audit = project_sub.add_parser("audit")
    project_audit.add_argument("--project", required=True)
    project_audit.add_argument("--async", dest="asynchronous", action="store_true")

    reference = sub.add_parser("reference", help="reference evidence")
    reference_sub = reference.add_subparsers(dest="reference_command", required=True)
    reference_import = reference_sub.add_parser("import")
    reference_import.add_argument("sources", nargs="+")
    reference_import.add_argument("--project", required=True)
    reference_import.add_argument("--rights-state", default="UNKNOWN")
    reference_import.add_argument("--viewpoint-label")
    reference_import.add_argument("--async", dest="asynchronous", action="store_true")
    reference_list = reference_sub.add_parser("list")
    reference_list.add_argument("--project", required=True)
    reference_mask = reference_sub.add_parser("import-mask")
    reference_mask.add_argument("source")
    reference_mask.add_argument("--reference-id", required=True)
    reference_mask.add_argument("--project", required=True)
    reference_mask.add_argument("--reviewer", required=True)
    reference_mask.add_argument("--reason", required=True)
    reference_mask_list = reference_sub.add_parser("list-masks")
    reference_mask_list.add_argument("--project", required=True)
    reference_mask_list.add_argument("--reference-id")
    reference_pdf = reference_sub.add_parser("extract-pdf")
    reference_pdf.add_argument("source")
    reference_pdf.add_argument("--project", required=True)
    reference_pdf.add_argument("--rights-state", required=True)
    reference_pdf.add_argument("--maximum-pages", type=int, default=200)
    reference_pdf.add_argument("--resolution-dpi", type=int, default=200)

    measurement = sub.add_parser("measurement", help="metric evidence")
    measurement_sub = measurement.add_subparsers(dest="measurement_command", required=True)
    measurement_add = measurement_sub.add_parser("add")
    measurement_add.add_argument("measurement_type")
    measurement_add.add_argument("--project", required=True)
    measurement_add.add_argument("--value", required=True, help="JSON measurement value")
    measurement_add.add_argument(
        "--evidence-class", choices=[item.value for item in EvidenceClass], required=True
    )
    measurement_add.add_argument(
        "--certainty", choices=("exact", "bounded", "estimated", "derived"), default="estimated"
    )
    measurement_add.add_argument("--uncertainty", default="{}", help="JSON uncertainty record")
    measurement_add.add_argument("--reference-id", action="append", default=[])
    measurement_list = measurement_sub.add_parser("list")
    measurement_list.add_argument("--project", required=True)
    measurement_correct = measurement_sub.add_parser("correct")
    measurement_correct.add_argument("measurement_id")
    measurement_correct.add_argument("--project", required=True)
    measurement_correct.add_argument("--value", required=True, help="replacement JSON value")
    measurement_correct.add_argument("--uncertainty", required=True, help="replacement JSON")
    measurement_correct.add_argument("--corrected-by", required=True)
    measurement_correct.add_argument("--reason", required=True)
    measurement_provenance = measurement_sub.add_parser("bind-source-provenance")
    measurement_provenance.add_argument("measurement_id")
    measurement_provenance.add_argument("--project", required=True)
    measurement_provenance.add_argument("--source-id", required=True)
    measurement_provenance.add_argument("--claim-locator", required=True)
    measurement_grid = measurement_sub.add_parser("grid-create")
    measurement_grid.add_argument("reference_id")
    measurement_grid.add_argument("--project", required=True)
    measurement_grid.add_argument("--definition", required=True, help="JSON object or file")
    measurement_grid.add_argument("--created-by", required=True)
    measurement_grid.add_argument("--uncertainty", default="{}", help="JSON object or file")
    measurement_grid.add_argument("--scale-measurement-id")
    measurement_grid_list = measurement_sub.add_parser("grid-list")
    measurement_grid_list.add_argument("--project", required=True)

    feature = sub.add_parser("feature", help="technical feature evidence and review")
    feature_sub = feature.add_subparsers(dest="feature_command", required=True)
    feature_add = feature_sub.add_parser("add")
    feature_add.add_argument("feature_type")
    feature_add.add_argument("--project", required=True)
    feature_add.add_argument("--parent-component", required=True)
    feature_add.add_argument("--observations", default="[]", help="JSON observation list")
    feature_add.add_argument("--reference-id", action="append", default=[])
    feature_add.add_argument("--dimensions", default="{}", help="JSON dimensions")
    feature_add.add_argument("--confidence", type=float, required=True)
    feature_add.add_argument(
        "--evidence-class", choices=[item.value for item in EvidenceClass], required=True
    )
    feature_add.add_argument("--coordinate-frame", default="canonical_mm")
    feature_add.add_argument("--uncertainty", default="{}")
    feature_add.add_argument("--model-revision", required=True)
    feature_add.add_argument("--coverage-group")
    feature_add.add_argument("--hero-surface", action="store_true")
    feature_list = feature_sub.add_parser("list")
    feature_list.add_argument("--project", required=True)
    feature_review = feature_sub.add_parser("review")
    feature_review.add_argument("feature_id")
    feature_review.add_argument("--project", required=True)
    decision = feature_review.add_mutually_exclusive_group(required=True)
    decision.add_argument("--approve", action="store_true")
    decision.add_argument("--reject", action="store_true")
    feature_review.add_argument("--reviewer", required=True)
    feature_review.add_argument("--reason", required=True)
    feature_link = feature_sub.add_parser("link")
    feature_link.add_argument("feature_id")
    feature_link.add_argument("--project", required=True)
    feature_link.add_argument("--reference-id", required=True)
    feature_link.add_argument("--observation", required=True, help="JSON object or file")
    feature_link.add_argument("--linked-by", required=True)
    feature_link.add_argument("--reason", required=True)

    material = sub.add_parser("material", help="appearance evidence and named review")
    material_sub = material.add_subparsers(dest="material_command", required=True)
    material_create = material_sub.add_parser("create")
    material_create.add_argument("region_id")
    material_create.add_argument("--project", required=True)
    material_create.add_argument("--properties", required=True, help="JSON object or file")
    material_create.add_argument(
        "--evidence-class", choices=[item.value for item in EvidenceClass], required=True
    )
    material_create.add_argument("--confidence", type=float, required=True)
    material_create.add_argument("--reference-id", action="append", default=[])
    material_create.add_argument("--artifact-digest", action="append", default=[])
    material_create.add_argument("--component-id")
    material_create.add_argument("--material-slot")
    material_create.add_argument("--uncertainty", default="{}")
    material_create.add_argument("--color-calibration", default="{}")
    material_create.add_argument("--lighting-estimate", default="{}")
    material_create.add_argument("--reflective-region-mask", action="append", default=[])
    material_create.add_argument("--multi-light-reference-id", action="append", default=[])
    material_create.add_argument("--supersedes-id")
    material_create.add_argument("--notes")
    material_review = material_sub.add_parser("review")
    material_review.add_argument("profile_id")
    material_review.add_argument("--project", required=True)
    material_decision = material_review.add_mutually_exclusive_group(required=True)
    material_decision.add_argument("--approve", dest="approved", action="store_true")
    material_decision.add_argument("--reject", dest="approved", action="store_false")
    material_review.add_argument("--reviewer", required=True)
    material_review.add_argument("--reason", required=True)
    material_list = material_sub.add_parser("list")
    material_list.add_argument("--project", required=True)

    component = sub.add_parser("component", help="typed parametric components and fitting")
    component_sub = component.add_subparsers(dest="component_command", required=True)
    component_create = component_sub.add_parser("create")
    component_create.add_argument("component_id")
    component_create.add_argument("component_type")
    component_create.add_argument("--project", required=True)
    component_create.add_argument("--parameters", required=True, help="JSON object")
    component_create.add_argument("--evidence-binding", action="append", default=[])
    component_create.add_argument("--material-slot", action="append", default=[])
    component_create.add_argument("--lod-rules", default="{}")
    component_update = component_sub.add_parser("update")
    component_update.add_argument("component_id")
    component_update.add_argument("--project", required=True)
    component_update.add_argument("--parameters", required=True, help="JSON object")
    component_fit = component_sub.add_parser("fit")
    component_fit.add_argument("component_id")
    component_fit.add_argument("--project", required=True)
    component_fit.add_argument("--bindings", required=True, help="JSON parameter-to-ID mapping")
    component_fit.add_argument("--huber-delta", type=float, default=1.5)
    component_fit.add_argument("--async", dest="asynchronous", action="store_true")
    component_review = component_sub.add_parser("review-fit")
    component_review.add_argument("fit_id")
    component_review.add_argument("--project", required=True)
    component_decision = component_review.add_mutually_exclusive_group(required=True)
    component_decision.add_argument("--accept", dest="accepted", action="store_true")
    component_decision.add_argument("--reject", dest="accepted", action="store_false")
    component_review.add_argument("--reviewer", required=True)
    component_review.add_argument("--reason", required=True)
    component_generate = component_sub.add_parser("generate")
    component_generate.add_argument("component_ids", nargs="+")
    component_generate.add_argument("--project", required=True)
    component_generate.add_argument("--scene-id")
    component_generate.add_argument("--async", dest="asynchronous", action="store_true")
    component_list = component_sub.add_parser("list")
    component_list.add_argument("--project", required=True)

    blender = sub.add_parser("blender", help="headless Blender operations")
    blender_sub = blender.add_subparsers(dest="blender_command", required=True)
    blender_import = blender_sub.add_parser("import")
    blender_import.add_argument("source")
    blender_import.add_argument("--project", required=True)
    inspect = blender_sub.add_parser("inspect")
    inspect.add_argument("--project", required=True)
    inspect.add_argument("--scene")
    inspect.add_argument("--async", dest="asynchronous", action="store_true")
    render = blender_sub.add_parser("render")
    render.add_argument("--project", required=True)
    render.add_argument("--scene-id")
    render.add_argument("--solution-id")
    render.add_argument("--maximum-dimension", type=int, default=1024)
    render.add_argument("--async", dest="asynchronous", action="store_true")
    export = blender_sub.add_parser("export")
    export.add_argument("--project", required=True)
    export.add_argument("--output-name", default="model.glb")
    export.add_argument("--async", dest="asynchronous", action="store_true")
    lod = blender_sub.add_parser("generate-lod")
    lod.add_argument("--project", required=True)
    lod.add_argument("--ratio", type=float, default=0.5)
    lod.add_argument("--object", action="append", default=[])
    lod.add_argument("--async", dest="asynchronous", action="store_true")
    prepare_asset = blender_sub.add_parser("prepare-asset")
    prepare_asset.add_argument("--project", required=True)
    prepare_asset.add_argument("--scene-id")
    prepare_asset.add_argument(
        "--targets",
        required=True,
        help="JSON target list or path to a JSON document",
    )
    prepare_asset.add_argument("--async", dest="asynchronous", action="store_true")

    vision = sub.add_parser("vision", help="vision backends")
    vision_sub = vision.add_subparsers(dest="vision_command", required=True)
    solve = vision_sub.add_parser("solve-cameras")
    solve.add_argument("--project", required=True)
    solve.add_argument("--backend", default="heuristic-pinhole")
    solve.add_argument("--async", dest="asynchronous", action="store_true")
    geometry_run = vision_sub.add_parser("run")
    geometry_run.add_argument("--project", required=True)
    geometry_run.add_argument("--backend", default="auto")
    geometry_run.add_argument("--configuration", default="{}", help="JSON object")
    geometry_run.add_argument("--async", dest="asynchronous", action="store_true")
    geometry_import = vision_sub.add_parser("import-evidence")
    geometry_import.add_argument("document", help="JSON geometry evidence document")
    geometry_import.add_argument("--project", required=True)
    geometry_compare = vision_sub.add_parser("compare-backends")
    geometry_compare.add_argument("--project", required=True)
    geometry_compare.add_argument("--run-id", action="append", default=[])
    geometry_compare.add_argument("--async", dest="asynchronous", action="store_true")
    camera_compare = vision_sub.add_parser("compare-cameras")
    camera_compare.add_argument("--project", required=True)
    camera_compare.add_argument("--solution-id", action="append", default=[])
    camera_compare.add_argument("--async", dest="asynchronous", action="store_true")
    geometry_list = vision_sub.add_parser("list-evidence")
    geometry_list.add_argument("--project", required=True)
    camera_import = vision_sub.add_parser("import-cameras")
    camera_import.add_argument("document", help="JSON file containing cameras and diagnostics")
    camera_import.add_argument("--project", required=True)
    camera_import.add_argument("--evidence-binding-id", action="append", default=[])
    camera_review = vision_sub.add_parser("review-cameras")
    camera_review.add_argument("solution_id")
    camera_review.add_argument("--project", required=True)
    camera_review.add_argument("--reviewer", required=True)
    camera_review.add_argument("--reason", required=True)
    camera_refine = vision_sub.add_parser("refine-camera")
    camera_refine.add_argument("--project", required=True)
    camera_refine.add_argument("--source-solution-id")
    camera_refine.add_argument("--reference-id")
    camera_refine.add_argument("--scene-id")
    camera_refine.add_argument("--maximum-dimension", type=int, default=256)
    camera_refine.add_argument("--stages", type=int, choices=(1, 2, 3, 4), default=3)
    camera_refine.add_argument("--evidence-binding-id", action="append", default=[])
    camera_refine.add_argument("--async", dest="asynchronous", action="store_true")
    camera_board = vision_sub.add_parser("solve-calibration-board")
    camera_board.add_argument("--project", required=True)
    camera_board.add_argument("--columns", type=int, required=True)
    camera_board.add_argument("--rows", type=int, required=True)
    camera_board.add_argument("--square-size-measurement-id", required=True)
    camera_vanishing = vision_sub.add_parser("solve-vanishing-points")
    camera_vanishing.add_argument("--project", required=True)
    camera_vanishing.add_argument("--grid-id", action="append", default=[])

    dataset = sub.add_parser("dataset", help="synthetic data and feature-model training")
    dataset_sub = dataset.add_subparsers(dest="dataset_command", required=True)
    dataset_plan = dataset_sub.add_parser("plan-synthetic")
    dataset_plan.add_argument("name")
    dataset_plan.add_argument("--project", required=True)
    dataset_plan.add_argument("--sample-count", type=int, required=True)
    dataset_plan.add_argument("--seed", type=int, default=0)
    dataset_plan.add_argument("--scene-id")
    dataset_plan.add_argument("--component-id", action="append", default=[])
    dataset_plan.add_argument("--domain-randomization", default="{}")
    dataset_generate = dataset_sub.add_parser("generate")
    dataset_generate.add_argument("dataset_id")
    dataset_generate.add_argument("--project", required=True)
    dataset_generate.add_argument("--async", dest="asynchronous", action="store_true")
    dataset_list = dataset_sub.add_parser("list")
    dataset_list.add_argument("--project", required=True)

    validate = sub.add_parser("validate", help="reference/render validation")
    validate_sub = validate.add_subparsers(dest="validate_command", required=True)
    compare = validate_sub.add_parser("compare")
    compare.add_argument("--project", required=True)
    compare.add_argument("--scene-id")
    compare.add_argument("--solution-id")
    compare.add_argument("--maximum-dimension", type=int, default=1024)
    compare.add_argument("--async", dest="asynchronous", action="store_true")
    coverage = validate_sub.add_parser("coverage")
    coverage.add_argument("--project", required=True)

    receipt = sub.add_parser("receipt", help="acceptance receipts")
    receipt_sub = receipt.add_subparsers(dest="receipt_command", required=True)
    receipt_export = receipt_sub.add_parser("export")
    receipt_export.add_argument("--project", required=True)
    receipt_verify = receipt_sub.add_parser("verify")
    receipt_verify.add_argument("path")
    receipt_verify.add_argument("--project")

    workflow = sub.add_parser("workflow", help="high-level reconstruction workflows")
    workflow_sub = workflow.add_subparsers(dest="workflow_command", required=True)
    audit = workflow_sub.add_parser("audit-reference-fidelity")
    audit.add_argument("--project", required=True)
    audit.add_argument("--scene")
    audit.add_argument("--reference", action="append", default=[])
    audit.add_argument("--rights-state", default="UNKNOWN")
    audit.add_argument("--viewpoint-label")
    audit.add_argument("--backend", default="heuristic-pinhole")
    audit.add_argument("--maximum-dimension", type=int, default=1024)
    audit.add_argument("--async", dest="asynchronous", action="store_true")

    benchmark = sub.add_parser("benchmark", help="bootstrap governed benchmark projects")
    benchmark_sub = benchmark.add_subparsers(dest="benchmark_command", required=True)
    mac_studio = benchmark_sub.add_parser("bootstrap-mac-studio")
    mac_studio.add_argument("--project", required=True)
    mac_studio.add_argument("--repository-root", default=str(Path.cwd()))
    mac_studio.add_argument("--include-marketing-reference", action="store_true")
    calibration = benchmark_sub.add_parser("bootstrap-calibration")
    calibration.add_argument("--project", required=True)
    calibration.add_argument("--reviewer", required=True)
    calibration.add_argument("--review-reason", required=True)
    asset_preparation = benchmark_sub.add_parser("bootstrap-asset-preparation")
    asset_preparation.add_argument("--output", required=True)
    asset_preparation.add_argument("--manifest")
    appearance = benchmark_sub.add_parser("bootstrap-appearance")
    appearance.add_argument("--output", required=True)
    appearance.add_argument("--manifest")
    performance = benchmark_sub.add_parser("bootstrap-performance")
    performance.add_argument("--output", required=True)
    performance.add_argument("--manifest")
    distributed_runtime = benchmark_sub.add_parser(
        "bootstrap-distributed-runtime"
    )
    distributed_runtime.add_argument("--output", required=True)
    distributed_runtime.add_argument("--manifest")
    adversarial = benchmark_sub.add_parser("bootstrap-adversarial")
    adversarial.add_argument("--output", required=True)
    adversarial.add_argument("--manifest")
    for command_name in ("bootstrap-dgx-spark", "bootstrap-rtx-5090-fe"):
        device = benchmark_sub.add_parser(command_name)
        device.add_argument("--project", required=True)
        device.add_argument("--scene", required=True)
        device.add_argument("--repository-root", default=str(Path.cwd()))
        device.add_argument("--reference-root")
        device.add_argument("--source-revision", required=True)
        device.add_argument("--source-artifact", action="append", default=[])
    rtx_revision = benchmark_sub.add_parser("revise-rtx-5090-fe")
    rtx_revision.add_argument("--project", required=True)
    rtx_revision.add_argument("--scene-id")
    rtx_revision.add_argument("--source-revision", required=True)
    rtx_revision.add_argument("--async", dest="asynchronous", action="store_true")
    rtx_visual = benchmark_sub.add_parser("refine-rtx-5090-fe-visual")
    rtx_visual.add_argument("--project", required=True)
    rtx_visual.add_argument("--scene-id")
    rtx_visual.add_argument("--source-revision", required=True)
    rtx_visual.add_argument("--async", dest="asynchronous", action="store_true")
    rtx_frame = benchmark_sub.add_parser("refine-rtx-5090-fe-front-frame")
    rtx_frame.add_argument("--project", required=True)
    rtx_frame.add_argument("--scene-id")
    rtx_frame.add_argument("--source-revision", required=True)
    rtx_frame.add_argument("--async", dest="asynchronous", action="store_true")
    dgx_visual = benchmark_sub.add_parser("refine-dgx-spark-visual")
    dgx_visual.add_argument("--project", required=True)
    dgx_visual.add_argument("--scene-id")
    dgx_visual.add_argument("--source-revision", required=True)
    dgx_visual.add_argument("--async", dest="asynchronous", action="store_true")
    dgx_base_foot = benchmark_sub.add_parser("refine-dgx-spark-base-foot")
    dgx_base_foot.add_argument("--project", required=True)
    dgx_base_foot.add_argument("--scene-id")
    dgx_base_foot.add_argument("--source-revision", required=True)
    dgx_base_foot.add_argument("--async", dest="asynchronous", action="store_true")

    repair = sub.add_parser("repair", help="evidence-bound repair proposals and checkpoints")
    repair_sub = repair.add_subparsers(dest="repair_command", required=True)
    repair_propose = repair_sub.add_parser("propose-mac-studio-grille")
    repair_propose.add_argument("--project", required=True)
    repair_approve = repair_sub.add_parser("approve")
    repair_approve.add_argument("proposal_id")
    repair_approve.add_argument("--project", required=True)
    repair_approve.add_argument("--approved-by", required=True)
    repair_apply = repair_sub.add_parser("apply")
    repair_apply.add_argument("proposal_id")
    repair_apply.add_argument("--project", required=True)
    repair_apply.add_argument("--scene-id")
    repair_apply.add_argument("--async", dest="asynchronous", action="store_true")
    repair_review = repair_sub.add_parser("review")
    repair_review.add_argument("proposal_id")
    repair_review.add_argument("--project", required=True)
    repair_review.add_argument("--reviewer", required=True)
    repair_review.add_argument("--reason", required=True)
    repair_review.add_argument("--receipt-id")
    repair_review_decision = repair_review.add_mutually_exclusive_group(required=True)
    repair_review_decision.add_argument("--accept", dest="accepted", action="store_true")
    repair_review_decision.add_argument("--reject", dest="accepted", action="store_false")
    repair_list = repair_sub.add_parser("list")
    repair_list.add_argument("--project", required=True)

    review = sub.add_parser("review", help="loopback-only browser review application")
    review_sub = review.add_subparsers(dest="review_command", required=True)
    review_serve = review_sub.add_parser("serve")
    review_serve.add_argument("--project", required=True)
    review_serve.add_argument("--host", default="127.0.0.1")
    review_serve.add_argument("--port", type=int, default=8787)
    review_serve.add_argument("--open", dest="open_browser", action="store_true")
    review_snapshot = review_sub.add_parser("snapshot")
    review_snapshot.add_argument("--project", required=True)

    worker = sub.add_parser("worker", help="distributed worker enrollment and lease recovery")
    worker_sub = worker.add_subparsers(dest="worker_command", required=True)
    worker_register = worker_sub.add_parser("register")
    worker_register.add_argument("name")
    worker_register.add_argument("--project", required=True)
    worker_register.add_argument(
        "--worker-class",
        choices=("blender", "vision", "optimization", "training", "render", "review"),
        required=True,
    )
    worker_register.add_argument(
        "--capabilities",
        required=True,
        help="JSON object or path to a JSON capability document",
    )
    worker_list = worker_sub.add_parser("list")
    worker_list.add_argument("--project", required=True)
    worker_reap = worker_sub.add_parser("reap-expired")
    worker_reap.add_argument("--project", required=True)
    worker_requirements = worker_sub.add_parser("requirements")
    worker_requirements.add_argument("job_id")
    worker_requirements.add_argument("--project", required=True)
    worker_run = worker_sub.add_parser("run", help="claim and execute allowlisted leased jobs")
    worker_run.add_argument("--project", required=True)
    worker_run.add_argument("--worker-id", required=True)
    worker_run.add_argument(
        "--worker-token",
        default=os.environ.get("BVMCP_WORKER_TOKEN"),
        help="worker secret; prefer the BVMCP_WORKER_TOKEN environment variable",
    )
    worker_run.add_argument("--lease-seconds", type=int, default=120)
    worker_run.add_argument("--poll-seconds", type=float, default=1.0)
    worker_run.add_argument("--once", action="store_true")

    job = sub.add_parser("job", help="job state and cancellation")
    job_sub = job.add_subparsers(dest="job_command", required=True)
    job_status = job_sub.add_parser("status")
    job_status.add_argument("job_id")
    job_status.add_argument("--project", required=True)
    job_cancel = job_sub.add_parser("cancel")
    job_cancel.add_argument("job_id")
    job_cancel.add_argument("--project", required=True)
    job_list = job_sub.add_parser("list")
    job_list.add_argument("--project", required=True)
    job_list.add_argument("--limit", type=int, default=100)

    status_alias = sub.add_parser("status", help="project status")
    status_alias.add_argument("--project", required=True)
    jobs_alias = sub.add_parser("jobs", help="recent jobs")
    jobs_alias.add_argument("--project", required=True)
    jobs_alias.add_argument("--limit", type=int, default=100)
    cache = sub.add_parser("cache", help="cache inspection")
    cache_sub = cache.add_subparsers(dest="cache_command", required=True)
    cache_inspect = cache_sub.add_parser("inspect")
    cache_inspect.add_argument("--project", required=True)
    cache_inspect.add_argument("--limit", type=int, default=100)
    return parser


def dispatch(args: argparse.Namespace) -> Any:
    if args.command == "doctor":
        return doctor_report()
    if args.command == "capabilities":
        return {"backends": BackendRegistry().as_dict()}
    if args.command == "capability":
        from blender_vision.scoring import CapabilityAuthority

        authority = CapabilityAuthority()
        if args.capability_command == "list":
            return {
                "catalog_sha256": authority.catalog.catalog_sha256,
                "registry_sha256": authority.catalog.registry_sha256,
                "facets": [
                    facet.model_dump(mode="json") for facet in authority.catalog.list(args.domain)
                ],
            }
        if args.capability_command == "show":
            return authority.catalog.get(args.facet).model_dump(mode="json")
        if args.capability_command == "verify-report":
            return authority.verify_report(Path(args.path))

        evidence_paths = [Path(path) for path in args.evidence]
        if args.capability_command == "report":
            if args.evidence_dir:
                evidence_paths.extend(sorted(Path(args.evidence_dir).glob("*.json")))
            return authority.report(
                evidence_paths,
                output_path=Path(args.output),
            ).model_dump(mode="json")

        evidence_by_facet = {}
        for path in evidence_paths:
            evidence = authority.load_evidence(path)
            if evidence.facet_id in evidence_by_facet:
                raise ValueError(f"duplicate evidence for facet {evidence.facet_id}")
            evidence_by_facet[evidence.facet_id] = evidence
        return {
            "selector": args.selector,
            "catalog_sha256": authority.catalog.catalog_sha256,
            "registry_sha256": authority.catalog.registry_sha256,
            "evaluations": [
                evaluation.model_dump(mode="json")
                for evaluation in authority.evaluate_selector(
                    args.selector,
                    evidence_by_facet,
                )
            ],
        }
    if args.command == "app":
        from blender_vision.app_build import (
            ApplicationBenchmarkRunner,
            BoundedApplicationCompiler,
            ReferenceCompletenessAnalyzer,
            ReferencePacketLoader,
        )

        if args.application_command == "benchmark":
            return (
                ApplicationBenchmarkRunner(Path(args.manifest) if args.manifest else None)
                .run(
                    Path(args.output),
                    case_ids=set(args.case) if args.case else None,
                )
                .model_dump(mode="json")
            )
        if args.application_command == "verify":
            candidate = Path(args.candidate).expanduser().resolve()
            return (
                BoundedApplicationCompiler(candidate.parent)
                .verify_candidate(candidate)
                .model_dump(mode="json")
            )
        loaded = ReferencePacketLoader().load(Path(args.packet))
        packet = loaded.packet
        if args.application_command == "check":
            return (
                ReferenceCompletenessAnalyzer()
                .analyze(
                    packet,
                    verified_source_ids=loaded.verified_source_ids,
                )
                .model_dump(mode="json")
            )
        return (
            BoundedApplicationCompiler(Path(args.workspace))
            .compile(
                packet,
                candidate_id=args.candidate_id,
                mode=args.mode,
                verified_source_ids=loaded.verified_source_ids,
            )
            .model_dump(mode="json")
        )
    if args.command == "browser":
        from blender_vision.perception.browser_benchmark import BrowserBenchmarkRunner

        return (
            BrowserBenchmarkRunner(Path(args.manifest) if args.manifest else None)
            .run(Path(args.output))
            .model_dump(mode="json")
        )
    if args.command == "glb":
        from blender_vision.geometry import GlbValidator

        return GlbValidator().validate(
            Path(args.path),
            required_node_names=args.required_node,
            required_mesh_names=args.required_mesh,
        ).to_dict()
    if args.command == "model":
        from blender_vision.models.store import ModelStore

        store = ModelStore(_project(args.project))
        if args.model_command == "approve-source":
            license_record = _json_argument(args.license_record)
            if not isinstance(license_record, dict):
                raise ValueError("model license record must be a JSON object")
            return store.approve_source(
                args.name,
                args.source_url,
                args.expected_sha256,
                license_record=license_record,
                approved_by=args.approved_by,
                reason=args.reason,
            )
        if args.model_command == "import-checkpoint":
            return store.import_checkpoint(
                args.approval_id, Path(args.checkpoint), revision=args.revision
            )
        return store.list()
    if args.command == "serve":
        from blender_vision.mcp.server import run_server

        run_server(Path(args.projects_root), transport=args.transport)
        return None
    if args.command == "daemon":
        Coordinator(_project(args.project)).daemon(poll_seconds=args.poll_seconds, once=args.once)
        return {"status": "stopped"}
    if args.command == "project":
        if args.project_command == "create":
            root = Path(args.root) if args.root else default_projects_root() / slugify(args.name)
            project = ProjectStore.create(
                root, args.name, target_fidelity=FidelityLevel(args.target_fidelity)
            )
            result: dict[str, Any] = {"root": str(project.root), "status": project.status()}
            if args.scene:
                result["scene_import"] = Coordinator(project).run(
                    "scene.import", {"source": args.scene}
                )
            return result
        project = _project(args.project)
        if args.project_command == "status":
            return project.status()
        if args.project_command == "audit":
            return _run(project, "project.audit", {}, args.asynchronous)
        return _verify_project(project)
    if args.command == "reference":
        project = _project(args.project)
        if args.reference_command in {"import-mask", "list-masks"}:
            from blender_vision.evidence.masks import ReferenceMaskStore

            masks = ReferenceMaskStore(project)
            if args.reference_command == "list-masks":
                return {"reference_masks": masks.list(args.reference_id)}
            return masks.import_reviewed(
                args.reference_id,
                Path(args.source),
                reviewer=args.reviewer,
                reason=args.reason,
            )
        if args.reference_command == "list":
            from blender_vision.evidence.references import ReferenceIngestor

            return {"references": ReferenceIngestor(project).list()}
        if args.reference_command == "extract-pdf":
            from blender_vision.evidence.references import ReferenceIngestor

            return ReferenceIngestor(project).import_pdf_pages(
                Path(args.source),
                rights_state=args.rights_state,
                maximum_pages=args.maximum_pages,
                resolution_dpi=args.resolution_dpi,
            )
        results = [
            _run(
                project,
                "reference.import",
                {
                    "source": source,
                    "rights_state": args.rights_state,
                    "viewpoint_label": args.viewpoint_label,
                },
                args.asynchronous,
            )
            for source in args.sources
        ]
        return {"jobs": results}
    if args.command == "measurement":
        from blender_vision.evidence.measurements import MeasurementGridStore, MeasurementStore

        project = _project(args.project)
        store = MeasurementStore(project)
        if args.measurement_command == "list":
            return {"measurements": store.list()}
        if args.measurement_command == "correct":
            return store.correct(
                args.measurement_id,
                _json_argument(args.value),
                uncertainty=_json_argument(args.uncertainty),
                corrected_by=args.corrected_by,
                reason=args.reason,
            )
        if args.measurement_command == "bind-source-provenance":
            return store.bind_source_provenance(
                args.measurement_id,
                source_id=args.source_id,
                claim_locator=args.claim_locator,
            )
        if args.measurement_command == "grid-create":
            return MeasurementGridStore(project).create(
                args.reference_id,
                _json_argument(args.definition),
                created_by=args.created_by,
                uncertainty=_json_argument(args.uncertainty),
                scale_measurement_id=args.scale_measurement_id,
            )
        if args.measurement_command == "grid-list":
            return {"measurement_grids": MeasurementGridStore(project).list()}
        return store.add(
            args.measurement_type,
            json.loads(args.value),
            evidence_class=EvidenceClass(args.evidence_class),
            uncertainty=json.loads(args.uncertainty),
            certainty=args.certainty,
            reference_ids=args.reference_id,
        )
    if args.command == "feature":
        from blender_vision.features.store import FeatureStore

        store = FeatureStore(_project(args.project))
        if args.feature_command == "list":
            return {"features": store.list()}
        if args.feature_command == "review":
            return store.review(
                args.feature_id,
                approved=args.approve,
                reviewer=args.reviewer,
                reason=args.reason,
            )
        if args.feature_command == "link":
            return store.link_observation(
                args.feature_id,
                args.reference_id,
                _json_argument(args.observation),
                linked_by=args.linked_by,
                reason=args.reason,
            )
        return store.add(
            args.feature_type,
            parent_component=args.parent_component,
            dimensions=json.loads(args.dimensions),
            coordinate_frame=args.coordinate_frame,
            observations=json.loads(args.observations),
            reference_ids=args.reference_id,
            confidence=args.confidence,
            uncertainty=json.loads(args.uncertainty),
            evidence_class=EvidenceClass(args.evidence_class),
            model_revision=args.model_revision,
            coverage_group=args.coverage_group,
            hero_surface=args.hero_surface,
        )
    if args.command == "material":
        from blender_vision.materials.store import MaterialStore

        store = MaterialStore(_project(args.project))
        if args.material_command == "list":
            return {"material_profiles": store.list()}
        if args.material_command == "review":
            return store.review(
                args.profile_id,
                approved=args.approved,
                reviewer=args.reviewer,
                reason=args.reason,
            )
        return store.create(
            args.region_id,
            _json_argument(args.properties),
            evidence_class=EvidenceClass(args.evidence_class),
            confidence=args.confidence,
            reference_ids=args.reference_id,
            artifact_digests=args.artifact_digest,
            component_id=args.component_id,
            material_slot=args.material_slot,
            uncertainty=_json_argument(args.uncertainty),
            color_calibration=_json_argument(args.color_calibration),
            lighting_estimate=_json_argument(args.lighting_estimate),
            reflective_region_masks=args.reflective_region_mask,
            multi_light_reference_ids=args.multi_light_reference_id,
            supersedes_id=args.supersedes_id,
            notes=args.notes,
        )
    if args.command == "blender":
        project = _project(args.project)
        if args.blender_command == "import":
            return _run(project, "scene.import", {"source": args.source}, False)
        if args.blender_command == "inspect":
            if args.scene:
                imported = _run(project, "scene.import", {"source": args.scene}, False)
                if imported["status"] != "succeeded":
                    return imported
            return _run(project, "blender.inspect", {}, args.asynchronous)
        if args.blender_command == "export":
            return _run(
                project,
                "blender.export",
                {"output_name": args.output_name},
                args.asynchronous,
            )
        if args.blender_command == "generate-lod":
            return _run(
                project,
                "blender.generate_lod",
                {"ratio": args.ratio, "objects": args.object},
                args.asynchronous,
            )
        if args.blender_command == "prepare-asset":
            targets = _json_argument(args.targets)
            if not isinstance(targets, list):
                raise ValueError("--targets must resolve to a JSON list")
            return _run(
                project,
                "blender.prepare_asset",
                {"scene_id": args.scene_id, "targets": targets},
                args.asynchronous,
            )
        return _run(
            project,
            "blender.render",
            {
                "scene_id": args.scene_id,
                "solution_id": args.solution_id,
                "maximum_dimension": args.maximum_dimension,
            },
            args.asynchronous,
        )
    if args.command == "component":
        from blender_vision.parametric.components import ComponentSpec, ComponentType
        from blender_vision.parametric.fitting import ComponentFitter
        from blender_vision.parametric.store import ComponentStore

        project = _project(args.project)
        store = ComponentStore(project)
        if args.component_command == "create":
            return store.create(
                ComponentSpec(
                    id=args.component_id,
                    type=ComponentType(args.component_type),
                    parameters=json.loads(args.parameters),
                    evidence_bindings=args.evidence_binding,
                    material_slots=args.material_slot,
                    lod_rules=json.loads(args.lod_rules),
                )
            )
        if args.component_command == "update":
            return store.update_parameters(args.component_id, json.loads(args.parameters))
        if args.component_command == "fit":
            return _run(
                project,
                "component.fit",
                {
                    "component_id": args.component_id,
                    "parameter_bindings": json.loads(args.bindings),
                    "huber_delta": args.huber_delta,
                },
                args.asynchronous,
            )
        if args.component_command == "review-fit":
            return _run(
                project,
                "component.review_fit",
                {
                    "fit_id": args.fit_id,
                    "accepted": args.accepted,
                    "reviewer": args.reviewer,
                    "reason": args.reason,
                },
                False,
            )
        if args.component_command == "generate":
            return _run(
                project,
                "component.generate",
                {"component_ids": args.component_ids, "scene_id": args.scene_id},
                args.asynchronous,
            )
        return {"components": store.list(), "fits": ComponentFitter(project).list()}
    if args.command == "vision":
        from blender_vision.cameras.solver import CameraSolver
        from blender_vision.vision.pipeline import GeometryPipeline
        from blender_vision.vision.store import GeometryEvidenceStore

        if args.vision_command == "import-cameras":
            document = json.loads(Path(args.document).read_text(encoding="utf-8"))
            return CameraSolver(_project(args.project)).import_manual(
                document["cameras"],
                diagnostics=document.get("diagnostics"),
                evidence_binding_ids=args.evidence_binding_id,
            )
        if args.vision_command == "review-cameras":
            return CameraSolver(_project(args.project)).approve(
                args.solution_id,
                reviewer=args.reviewer,
                reason=args.reason,
            )
        if args.vision_command == "refine-camera":
            return _run(
                _project(args.project),
                "vision.refine_camera",
                {
                    "source_solution_id": args.source_solution_id,
                    "reference_id": args.reference_id,
                    "scene_id": args.scene_id,
                    "maximum_dimension": args.maximum_dimension,
                    "stages": args.stages,
                    "evidence_binding_ids": args.evidence_binding_id,
                },
                args.asynchronous,
            )
        if args.vision_command == "solve-calibration-board":
            return CameraSolver(_project(args.project)).solve_calibration_board(
                columns=args.columns,
                rows=args.rows,
                square_size_measurement_id=args.square_size_measurement_id,
            )
        if args.vision_command == "solve-vanishing-points":
            return CameraSolver(_project(args.project)).solve_vanishing_points(args.grid_id or None)
        if args.vision_command == "run":
            return _run(
                _project(args.project),
                "vision.run",
                {
                    "backend": args.backend,
                    "configuration": json.loads(args.configuration),
                },
                args.asynchronous,
            )
        if args.vision_command == "import-evidence":
            document = json.loads(Path(args.document).read_text(encoding="utf-8"))
            return GeometryPipeline(_project(args.project)).import_external(
                backend=document["backend"],
                backend_version=document["backend_version"],
                evidence=document["evidence"],
                evidence_class=document["evidence_class"],
                license_record=document["license"],
                configuration=document.get("configuration"),
            )
        if args.vision_command == "compare-backends":
            return _run(
                _project(args.project),
                "vision.compare_backends",
                {"run_ids": args.run_id or None},
                args.asynchronous,
            )
        if args.vision_command == "compare-cameras":
            return _run(
                _project(args.project),
                "vision.compare_camera_solutions",
                {"solution_ids": args.solution_id or None},
                args.asynchronous,
            )
        if args.vision_command == "list-evidence":
            store = GeometryEvidenceStore(_project(args.project))
            return {"runs": store.list(), "latest_consensus": store.latest_consensus()}
        return _run(
            _project(args.project),
            "vision.solve_cameras",
            {"backend": args.backend},
            args.asynchronous,
        )
    if args.command == "dataset":
        from blender_vision.datasets.store import DatasetStore, TrainingStore

        project = _project(args.project)
        if args.dataset_command == "plan-synthetic":
            return DatasetStore(project).plan_synthetic(
                args.name,
                sample_count=args.sample_count,
                seed=args.seed,
                scene_id=args.scene_id,
                component_ids=args.component_id,
                domain_randomization=json.loads(args.domain_randomization),
            )
        if args.dataset_command == "generate":
            return _run(
                project,
                "dataset.generate",
                {"dataset_id": args.dataset_id},
                args.asynchronous,
            )
        return {
            "datasets": DatasetStore(project).list(),
            "training_runs": TrainingStore(project).list(),
        }
    if args.command == "validate":
        operation = (
            "validation.compare" if args.validate_command == "compare" else "validation.coverage"
        )
        config = {
            "scene_id": getattr(args, "scene_id", None),
            "solution_id": getattr(args, "solution_id", None),
            "maximum_dimension": getattr(args, "maximum_dimension", 1024),
        }
        return _run(_project(args.project), operation, config, getattr(args, "asynchronous", False))
    if args.command == "receipt":
        project = _project(args.project) if args.project else None
        if args.receipt_command == "verify":
            return verify_receipt(Path(args.path), project=project)
        return _run(project, "receipt.export", {}, False)
    if args.command == "workflow":
        references = [
            {
                "source": source,
                "rights_state": args.rights_state,
                "viewpoint_label": args.viewpoint_label,
            }
            for source in args.reference
        ]
        return _run(
            _project(args.project),
            "workflow.audit_reference_fidelity",
            {
                "scene": args.scene,
                "references": references,
                "backend": args.backend,
                "maximum_dimension": args.maximum_dimension,
            },
            args.asynchronous,
        )
    if args.command == "benchmark":
        if args.benchmark_command == "bootstrap-adversarial":
            from blender_vision.benchmarks.adversarial import (
                AdversarialBenchmarkRunner,
            )

            return (
                AdversarialBenchmarkRunner(
                    Path(args.manifest) if args.manifest else None
                )
                .run(Path(args.output))
                .model_dump(mode="json")
            )
        if args.benchmark_command == "bootstrap-distributed-runtime":
            from blender_vision.benchmarks.distributed_runtime import (
                DistributedRuntimeBenchmarkRunner,
            )

            return (
                DistributedRuntimeBenchmarkRunner(
                    Path(args.manifest) if args.manifest else None
                )
                .run(Path(args.output))
                .model_dump(mode="json")
            )
        if args.benchmark_command == "bootstrap-performance":
            from blender_vision.benchmarks.performance import (
                PerformanceBenchmarkRunner,
            )

            return (
                PerformanceBenchmarkRunner(
                    Path(args.manifest) if args.manifest else None
                )
                .run(Path(args.output))
                .model_dump(mode="json")
            )
        if args.benchmark_command == "bootstrap-appearance":
            from blender_vision.benchmarks.appearance import (
                AppearanceBenchmarkRunner,
            )

            return (
                AppearanceBenchmarkRunner(
                    Path(args.manifest) if args.manifest else None
                )
                .run(Path(args.output))
                .model_dump(mode="json")
            )
        if args.benchmark_command == "bootstrap-asset-preparation":
            from blender_vision.benchmarks.asset_preparation import (
                AssetPreparationBenchmarkRunner,
            )

            return (
                AssetPreparationBenchmarkRunner(
                    Path(args.manifest) if args.manifest else None
                )
                .run(Path(args.output))
                .model_dump(mode="json")
            )
        if args.benchmark_command == "bootstrap-calibration":
            from blender_vision.benchmarks.calibration import bootstrap_calibration

            return bootstrap_calibration(
                Path(args.project),
                reviewer=args.reviewer,
                review_reason=args.review_reason,
            )
        if args.benchmark_command in {
            "bootstrap-dgx-spark",
            "bootstrap-rtx-5090-fe",
        }:
            from blender_vision.benchmarks.devices import bootstrap_device_benchmark

            benchmark = (
                "dgx_spark" if args.benchmark_command == "bootstrap-dgx-spark" else "rtx_5090_fe"
            )
            return bootstrap_device_benchmark(
                Path(args.project),
                Path(args.repository_root),
                benchmark=benchmark,
                scene_path=Path(args.scene),
                source_revision=args.source_revision,
                reference_root=Path(args.reference_root) if args.reference_root else None,
                source_artifacts=[Path(path) for path in args.source_artifact],
            )
        if args.benchmark_command == "revise-rtx-5090-fe":
            return _run(
                _project(args.project),
                "benchmark.revise_rtx_5090_fe_candidate",
                {
                    "scene_id": args.scene_id,
                    "source_revision": args.source_revision,
                },
                args.asynchronous,
            )
        if args.benchmark_command == "refine-rtx-5090-fe-visual":
            return _run(
                _project(args.project),
                "benchmark.refine_rtx_5090_fe_visual_candidate",
                {
                    "scene_id": args.scene_id,
                    "source_revision": args.source_revision,
                },
                args.asynchronous,
            )
        if args.benchmark_command == "refine-rtx-5090-fe-front-frame":
            return _run(
                _project(args.project),
                "benchmark.refine_rtx_5090_fe_front_frame_candidate",
                {
                    "scene_id": args.scene_id,
                    "source_revision": args.source_revision,
                },
                args.asynchronous,
            )
        if args.benchmark_command == "refine-dgx-spark-visual":
            return _run(
                _project(args.project),
                "benchmark.refine_dgx_spark_visual_candidate",
                {
                    "scene_id": args.scene_id,
                    "source_revision": args.source_revision,
                },
                args.asynchronous,
            )
        if args.benchmark_command == "refine-dgx-spark-base-foot":
            return _run(
                _project(args.project),
                "benchmark.refine_dgx_spark_base_foot_candidate",
                {
                    "scene_id": args.scene_id,
                    "source_revision": args.source_revision,
                },
                args.asynchronous,
            )
        from blender_vision.benchmarks.mac_studio import bootstrap_mac_studio

        return bootstrap_mac_studio(
            Path(args.project),
            Path(args.repository_root),
            include_marketing_reference=args.include_marketing_reference,
        )
    if args.command == "repair":
        from blender_vision.repairs.store import RepairStore

        project = _project(args.project)
        if args.repair_command == "propose-mac-studio-grille":
            return _run(project, "repair.propose_mac_studio_grille", {}, False)
        if args.repair_command == "approve":
            return _run(
                project,
                "repair.approve",
                {"proposal_id": args.proposal_id, "approved_by": args.approved_by},
                False,
            )
        if args.repair_command == "apply":
            return _run(
                project,
                "repair.apply",
                {"proposal_id": args.proposal_id, "scene_id": args.scene_id},
                args.asynchronous,
            )
        if args.repair_command == "review":
            return _run(
                project,
                "repair.review",
                {
                    "proposal_id": args.proposal_id,
                    "accepted": args.accepted,
                    "reviewer": args.reviewer,
                    "reason": args.reason,
                    "receipt_id": args.receipt_id,
                },
                False,
            )
        return {"repairs": RepairStore(project).list()}
    if args.command == "review":
        from blender_vision.review import ReviewService, serve_review

        project = _project(args.project)
        if args.review_command == "snapshot":
            return ReviewService(project).snapshot()
        serve_review(
            project,
            host=args.host,
            port=args.port,
            open_browser=args.open_browser,
        )
        return None
    if args.command == "worker":
        from blender_vision.scheduling.distributed import DistributedScheduler

        scheduler = DistributedScheduler(_project(args.project))
        if args.worker_command == "register":
            capabilities = _json_argument(args.capabilities)
            if not isinstance(capabilities, dict):
                raise ValueError("worker capabilities must be a JSON object")
            return scheduler.register(args.name, args.worker_class, capabilities)
        if args.worker_command == "reap-expired":
            return scheduler.reap_expired()
        if args.worker_command == "requirements":
            return scheduler.requirements(args.job_id)
        if args.worker_command == "run":
            from blender_vision.scheduling.worker import WorkerRuntime

            if not args.worker_token:
                raise ValueError(
                    "worker token is required via BVMCP_WORKER_TOKEN or --worker-token"
                )
            return WorkerRuntime(
                scheduler.project,
                args.worker_id,
                args.worker_token,
                lease_seconds=args.lease_seconds,
            ).run(once=args.once, poll_seconds=args.poll_seconds)
        return scheduler.snapshot()
    if args.command == "job":
        project = _project(args.project)
        if args.job_command == "status":
            return project.job(args.job_id)
        if args.job_command == "cancel":
            project.request_cancel(args.job_id)
            return project.job(args.job_id)
        return {"jobs": project.list_jobs(limit=args.limit)}
    if args.command == "status":
        return _project(args.project).status()
    if args.command == "jobs":
        return {"jobs": _project(args.project).list_jobs(limit=args.limit)}
    if args.command == "cache":
        return {"entries": _project(args.project).cache_entries(limit=args.limit)}
    raise AssertionError(args.command)


def main(argv: list[str] | None = None) -> None:
    try:
        result = dispatch(build_parser().parse_args(argv))
        if result is not None:
            _json(result)
    except (BlenderVisionError, FileNotFoundError, ValueError, KeyError, RuntimeError) as error:
        _json({"ok": False, "error": {"type": type(error).__name__, "message": str(error)}})
        raise SystemExit(2) from error


if __name__ == "__main__":
    main(sys.argv[1:])
