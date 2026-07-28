from __future__ import annotations

from pathlib import Path
from typing import Any

from blender_vision.acceptance.receipts import ReceiptBuilder
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.blender.gateway import BlenderGateway
from blender_vision.cameras.solver import CameraSolver, camera_specs_for_scene
from blender_vision.comparison.coverage import coverage_report
from blender_vision.comparison.images import compare_project_renders
from blender_vision.core.models import FidelityLevel
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.jobs import JobContext


def audit_reference_fidelity(
    project: ProjectStore,
    *,
    scene: Path,
    references: list[Path] | None = None,
    camera_backend: str = "auto",
    rights_state: str = "UNKNOWN",
    target_fidelity: FidelityLevel | None = None,
    context: JobContext | None = None,
) -> dict[str, Any]:
    def progress(stage: str, **payload: Any) -> None:
        if context:
            context.progress(stage, **payload)

    gateway = BlenderGateway(project)
    progress("import_scene")
    if scene.expanduser().resolve().is_relative_to(project.root):
        project_scene = scene.expanduser().resolve()
        scene_artifact = ArtifactStore(project).ingest_file(
            project_scene, media_type="application/x-blender"
        )
        imported_scene = {
            "artifact": scene_artifact.to_dict(),
            "scene": str(project_scene.relative_to(project.root)),
        }
    else:
        imported_scene = gateway.import_scene(scene)
        project_scene = project.root / imported_scene["scene"]
    if references:
        ingestor = ReferenceIngestor(project)
        progress("import_references", count=len(references))
        for reference in references:
            ingestor.import_file(reference, rights_state=rights_state)
    progress("inspect_scene")
    inventory = gateway.inspect(project_scene)
    progress("solve_cameras", backend=camera_backend)
    cameras = CameraSolver(project).solve(camera_backend)
    progress("render_views", count=len(cameras["cameras"]))
    render_result = gateway.run(
        "render_passes",
        scene=project_scene,
        parameters={
            "output_directory": str(project.root / "renders" / cameras["id"]),
            "cameras": camera_specs_for_scene(cameras, inventory),
        },
        timeout_seconds=900,
    )
    progress("compare", count=len(render_result["renders"]))
    comparison = compare_project_renders(project, render_result["renders"])
    coverage = coverage_report(project, cameras)
    progress("receipt")
    receipt = ReceiptBuilder(project).build(
        scene_inventory=inventory,
        camera_solution=cameras,
        comparison=comparison,
        coverage=coverage,
        requested_fidelity=target_fidelity,
    )
    return {
        "project": str(project.root),
        "scene": imported_scene,
        "inventory": inventory,
        "camera_solution": cameras,
        "renders": render_result,
        "comparison": comparison,
        "coverage": coverage,
        "receipt": receipt,
    }
