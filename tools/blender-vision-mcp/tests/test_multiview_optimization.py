from __future__ import annotations

import json
from pathlib import Path

import pytest
from PIL import Image, ImageDraw

from blender_vision.cameras.solver import CameraSolver
from blender_vision.comparison.images import compare_project_renders
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.evidence.targets import TargetResolver
from blender_vision.geometry.scenes import SceneStore
from blender_vision.geometry.semantic_graph import SemanticTwinGraph
from blender_vision.optimization.engine import OptimizationEngine
from blender_vision.parametric.components import ComponentSpec, ComponentType
from blender_vision.parametric.store import ComponentStore
from blender_vision.projects.store import ProjectStore
from blender_vision.workflows.executor import AutonomousWorkflowExecutor


def _fixture(tmp_path: Path) -> tuple[ProjectStore, str, str, list[str], list[str], float]:
    project = ProjectStore.create(tmp_path / "project", "Multiview optimization")
    target = TargetResolver(project).resolve({"manufacturer": "Acme", "model": "Widget"})
    references = []
    rendered = []
    for index, label in enumerate(("front", "rear")):
        image = Image.new("RGBA", (96, 72), (0, 0, 0, 0))
        ImageDraw.Draw(image).rounded_rectangle(
            (18 + index * 3, 14, 76 + index * 3, 60),
            radius=8,
            fill=(180, 180, 180, 255),
        )
        source_path = tmp_path / f"{label}.png"
        image.save(source_path)
        references.append(
            ReferenceIngestor(project).import_file(
                source_path, rights_state="SYNTHETIC_OWNED", viewpoint_label=label
            )
        )
        render_path = project.root / "renders" / f"candidate-{label}.png"
        render_path.parent.mkdir(parents=True, exist_ok=True)
        image.save(render_path)
        rendered.append(
            {
                "reference_id": references[-1]["id"],
                "path": str(render_path.relative_to(project.root)),
            }
        )
    camera = CameraSolver(project).solve("turntable_fallback")
    approved = CameraSolver(project).approve(
        camera["id"],
        reviewer="Camera reviewer",
        reason="Synthetic fixed-camera multiview fitting fixture.",
    )
    scene_path = tmp_path / "candidate.blend"
    scene_path.write_bytes(b"editable-candidate")
    scene = SceneStore(project).import_blend(scene_path)
    ComponentStore(project).create(
        ComponentSpec(
            id="panel",
            type=ComponentType.PANEL,
            parameters={"width_mm": 10.0},
        )
    )
    graph = SemanticTwinGraph(project).bootstrap(
        category="general_product", target_id=target["id"]
    )
    semantic_id = next(
        item["id"] for item in graph["nodes"] if item["type"] != "digital_twin_root"
    )
    SemanticTwinGraph(project).bind(
        semantic_id,
        scene_id=scene["id"],
        object_names=["Panel_Object"],
        component_ids=["panel"],
        reference_ids=[item["id"] for item in references],
        confidence=0.8,
    )
    comparisons = compare_project_renders(project, rendered)
    comparison_ids = [item["id"] for item in comparisons["comparisons"]]
    silhouette_loss = 1.0 - comparisons["summary"]["mean_silhouette_iou"]
    return (
        project,
        semantic_id,
        approved["id"],
        [item["id"] for item in references],
        comparison_ids,
        silhouette_loss,
    )


def test_multiview_proposal_binds_locality_camera_and_recomputed_residuals(
    tmp_path: Path,
) -> None:
    project, semantic_id, camera_id, reference_ids, comparison_ids, loss = _fixture(tmp_path)
    candidates = [
        {
            "parameters": {"width_mm": 10.0},
            "terms": {"silhouette": loss, "complexity": 0.1},
            "baseline": True,
            "diagnostics": {"comparison_ids": comparison_ids},
        },
        {
            "parameters": {"width_mm": 11.0},
            "terms": {"silhouette": loss, "complexity": 0.0},
            "diagnostics": {"comparison_ids": comparison_ids},
        },
    ]

    proposal = OptimizationEngine(project).propose_multiview(
        "panel",
        semantic_ids=[semantic_id],
        camera_solution_id=camera_id,
        candidates=candidates,
    )

    assert proposal["status"] == "proposed"
    assert proposal["configuration"]["image_residual_fit"] is True
    assert proposal["configuration"]["camera_solution_id"] == camera_id
    assert proposal["configuration"]["reference_ids"] == sorted(reference_ids)
    assert proposal["configuration"]["evidence_binding_ids"] == sorted(comparison_ids)
    assert proposal["locality_plan"]["component_ids"] == ["panel"]
    assert proposal["locality_plan"]["full_project_recompute"] is False
    for candidate in proposal["evaluations"]:
        diagnostics = candidate["diagnostics"]
        assert diagnostics["comparison_ids"] == sorted(comparison_ids)
        assert diagnostics["reference_ids"] == sorted(reference_ids)
        assert diagnostics["locality_plan_digest"] == proposal["locality_plan"]["artifact"][
            "digest"
        ]
    assert AutonomousWorkflowExecutor(project)._multiview_fit_targets() == [
        {
            "semantic_id": semantic_id,
            "component_ids": ["panel"],
            "reference_ids": sorted(reference_ids),
        }
    ]


def test_multiview_proposal_rejects_forged_loss_or_unbound_component(tmp_path: Path) -> None:
    project, semantic_id, camera_id, _reference_ids, comparison_ids, loss = _fixture(tmp_path)
    forged = [
        {
            "parameters": {"width_mm": 11.0},
            "terms": {"silhouette": loss + 0.1},
            "diagnostics": {"comparison_ids": comparison_ids},
        }
    ]
    with pytest.raises(ValueError, match="does not match"):
        OptimizationEngine(project).propose_multiview(
            "panel",
            semantic_ids=[semantic_id],
            camera_solution_id=camera_id,
            candidates=forged,
        )

    node = SemanticTwinGraph(project).get(semantic_id)
    node["geometry"]["component_ids"] = []
    with project.connection() as connection:
        connection.execute(
            "UPDATE semantic_nodes SET record_json=? WHERE id=?",
            (json.dumps(node), semantic_id),
        )
    valid = [
        {
            "parameters": {"width_mm": 11.0},
            "terms": {"silhouette": loss},
            "diagnostics": {"comparison_ids": comparison_ids},
        }
    ]
    with pytest.raises(ValueError, match="explicitly bound"):
        OptimizationEngine(project).propose_multiview(
            "panel",
            semantic_ids=[semantic_id],
            camera_solution_id=camera_id,
            candidates=valid,
        )
