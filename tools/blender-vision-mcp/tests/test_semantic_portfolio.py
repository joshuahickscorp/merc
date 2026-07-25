from __future__ import annotations

from pathlib import Path

from PIL import Image

from blender_vision.cameras.solver import CameraSolver
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.evidence.targets import TargetResolver
from blender_vision.geometry.portfolio import ReconstructionPortfolioStore
from blender_vision.geometry.portfolio_executor import PortfolioExecutor
from blender_vision.geometry.scenes import SceneStore
from blender_vision.geometry.semantic_graph import ROVER_COMPONENTS, SemanticTwinGraph
from blender_vision.intelligence.packs import CategoryPackRegistry
from blender_vision.orchestration.locality import LocalityPlanner
from blender_vision.projects.store import ProjectStore
from blender_vision.vision.store import GeometryEvidenceStore


def test_vehicle_pack_bootstraps_semantic_twin_graph(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Semantic vehicle")
    target = TargetResolver(project).resolve(
        {"manufacturer": "Porsche", "model": "911 GT3 RS", "model_year": 2024}
    )
    selected = CategoryPackRegistry().select(target["target"])
    assert selected["id"] == "vehicles"
    assert "wheel_axis_constraints" in selected["priors"]
    assert "diffuser_channel" in selected["constructs"]

    graph = SemanticTwinGraph(project).bootstrap(target_id=target["id"])
    types = {item["type"] for item in graph["nodes"]}
    assert {"digital_twin_root", "body_shell", "wheel", "underbody"} <= types
    assert graph["operations_require_semantic_ids"] is True

    scene_path = tmp_path / "vehicle.blend"
    scene_path.write_bytes(b"fixture")
    scene = SceneStore(project).import_blend(scene_path)
    wheel = next(item for item in graph["nodes"] if item["type"] == "wheel")
    bound = SemanticTwinGraph(project).bind(
        wheel["id"],
        scene_id=scene["id"],
        object_names=["Wheel_FL", "Wheel_FR"],
        reference_ids=[],
        confidence=0.75,
    )
    assert bound["geometry"]["object_names"] == ["Wheel_FL", "Wheel_FR"]
    assert bound["acceptance_state"] == "pending"
    assert project.status()["counts"]["semantic_nodes"] == len(graph["nodes"])


def test_perseverance_selects_space_rover_pack() -> None:
    selected = CategoryPackRegistry().select(
        {
            "manufacturer": "NASA/JPL-Caltech",
            "model": "Mars 2020 Perseverance Rover",
            "generation": "Mars 2020 flight rover",
        }
    )
    assert selected["id"] == "space_rovers"
    assert {"belly_pan", "rocker", "bogie", "remote_sensing_mast"} <= set(
        selected["ontology"]
    )
    assert "rocker_bogie_kinematics" in selected["priors"]


def test_rover_target_extends_vehicle_graph_idempotently(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Semantic rover")
    target = TargetResolver(project).resolve(
        {"manufacturer": "NASA", "model": "Mars 2020 Perseverance Rover", "model_year": 2020}
    )

    graph = SemanticTwinGraph(project).bootstrap(category="vehicles", target_id=target["id"])
    types = {item["type"] for item in graph["nodes"]}
    assert set(ROVER_COMPONENTS) <= types

    extended = SemanticTwinGraph(project).ensure_component_nodes(
        graph["root_id"], ["instrument_mast", "mobility_controller"]
    )
    repeated = SemanticTwinGraph(project).ensure_component_nodes(
        graph["root_id"], ["instrument_mast", "mobility_controller"]
    )

    assert len(extended["created_node_ids"]) == 1
    assert repeated["created_node_ids"] == []


def test_semantic_exports_are_scoped_to_the_requested_target_root(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Multiple semantic targets")
    first = TargetResolver(project).resolve(
        {"manufacturer": "Porsche", "model": "911 GT3 RS"}
    )
    first_graph = SemanticTwinGraph(project).bootstrap(
        category="vehicles", target_id=first["id"]
    )
    second = TargetResolver(project).resolve(
        {"manufacturer": "NVIDIA", "model": "GeForce RTX 5090 Founders Edition"}
    )
    second_graph = SemanticTwinGraph(project).bootstrap(
        category="computer_hardware", target_id=second["id"]
    )

    assert {item["type"] for item in first_graph["nodes"]} >= {"wheel", "body_shell"}
    assert "fan" not in {item["type"] for item in first_graph["nodes"]}
    assert {item["type"] for item in second_graph["nodes"]} >= {"fan", "heat_sink"}
    assert "wheel" not in {item["type"] for item in second_graph["nodes"]}
    assert all(
        item["id"].startswith(second_graph["root_id"])
        for item in second_graph["nodes"]
    )


def test_portfolio_keeps_generative_lanes_as_hypotheses(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Portfolio")
    store = ReconstructionPortfolioStore(project)
    portfolio = store.generate(category="vehicles", resource_profile="beast")
    assert len(portfolio["candidates"]) == 8
    generative = next(
        item for item in portfolio["candidates"] if item["lane"] == "generative_image_to_3d"
    )
    semantic = next(
        item for item in portfolio["candidates"] if item["lane"] == "hybrid_semantic_reconstruction"
    )
    assert generative["acceptance_eligible"] is False
    assert generative["evidence_authority"] == "hypothesis"

    store.record_result(
        generative["id"], metrics={"silhouette": 0.98, "camera": 0.5, "coverage": 0.7}
    )
    store.record_result(
        semantic["id"],
        metrics={
            "silhouette": 0.95,
            "camera": 0.9,
            "coverage": 0.9,
            "semantic_editability": 1.0,
        },
    )
    ranked = store.rank(portfolio["id"])
    assert ranked["selected_editable_candidate_id"] == semantic["id"]
    fusion = store.fusion_plan(portfolio["id"])
    assert fusion["editable_target_candidate_id"] == semantic["id"]
    assert fusion["generated_inputs_labeled_as_inference"] is True


def test_locality_plan_limits_views_passes_and_metrics_to_semantic_binding(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Local semantic validation")
    target = TargetResolver(project).resolve(
        {"manufacturer": "Fixture", "model": "Semantic device", "model_year": 2026}
    )
    graph = SemanticTwinGraph(project).bootstrap(
        category="computer_hardware", target_id=target["id"]
    )
    ingestor = ReferenceIngestor(project)
    references = []
    for label in ("front", "rear"):
        source = tmp_path / f"{label}.png"
        Image.new("RGB", (160, 120), "gray").save(source)
        references.append(
            ingestor.import_file(
                source, rights_state="SYNTHETIC_OWNED", viewpoint_label=label
            )
        )
    camera = CameraSolver(project).solve("turntable_fallback")
    scene_path = tmp_path / "semantic.blend"
    scene_path.write_bytes(b"fixture")
    scene = SceneStore(project).import_blend(scene_path)
    node = next(item for item in graph["nodes"] if item["type"] != "digital_twin_root")
    SemanticTwinGraph(project).bind(
        node["id"],
        scene_id=scene["id"],
        object_names=["Bound_Component"],
        reference_ids=[references[0]["id"]],
        confidence=0.8,
    )

    plan = LocalityPlanner(project).plan(
        [node["id"]], change_kind="topology", camera_solution_id=camera["id"]
    )

    assert plan["reference_ids"] == [references[0]["id"]]
    assert plan["object_names"] == ["Bound_Component"]
    assert set(plan["requested_passes"]) == {
        "silhouette",
        "depth",
        "normal",
        "object_id",
        "feature_id",
    }
    assert "topology" in plan["requested_metrics"]
    assert plan["full_project_recompute"] is False
    assert (project.root / plan["path"]).is_file()


def test_initial_portfolio_executor_evaluates_local_lanes_and_records_missing_workers(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(
        tmp_path / "project",
        "Executable portfolio",
        metadata={"private_starting_model": False},
    )
    target = TargetResolver(project).resolve(
        {"manufacturer": "Fixture", "model": "Portfolio object"}
    )
    graph = SemanticTwinGraph(project).bootstrap(
        category="general_product", target_id=target["id"]
    )
    source = tmp_path / "front.png"
    Image.new("RGB", (160, 120), "gray").save(source)
    reference = ReferenceIngestor(project).import_file(
        source, rights_state="SYNTHETIC_OWNED", viewpoint_label="front"
    )
    diagnostic_source = tmp_path / "diagnostic.png"
    Image.new("RGB", (160, 120), "black").save(diagnostic_source)
    ReferenceIngestor(project).import_file(
        diagnostic_source,
        rights_state="SYNTHETIC_OWNED",
        viewpoint_label="rejected",
        evidence_role="diagnostic_rejected_frame",
        acceptance_eligible=False,
    )
    CameraSolver(project).solve("turntable_fallback")
    scene_path = tmp_path / "seed.blend"
    scene_path.write_bytes(b"editable-seed")
    scene = SceneStore(project).import_blend(scene_path)
    node = next(item for item in graph["nodes"] if item["type"] != "digital_twin_root")
    SemanticTwinGraph(project).bind(
        node["id"],
        scene_id=scene["id"],
        object_names=["Seed_Body"],
        reference_ids=[reference["id"]],
        confidence=0.4,
    )
    portfolio = ReconstructionPortfolioStore(project).generate(category="general_product")
    parametric = next(
        item for item in portfolio["candidates"] if item["lane"] == "parametric_category_model"
    )
    ReconstructionPortfolioStore(project).record_result(
        parametric["id"],
        metrics={"semantic_editability": 1.0, "coverage": 0.1, "silhouette": 0.0},
        artifacts=[scene["artifact"]["digest"]],
        scene_id=scene["id"],
    )

    result = PortfolioExecutor(project).execute_initial(portfolio["id"])
    by_lane = {
        item["lane"]: item
        for item in ReconstructionPortfolioStore(project).list_candidates(portfolio["id"])
    }

    assert by_lane["visual_hull"]["status"] == "EVIDENCE_READY"
    assert "cannot run" in by_lane["visual_hull"]["progress_reason"]
    assert by_lane["hybrid_semantic_reconstruction"]["status"] == "EVALUATED"
    assert by_lane["existing_model_repair"]["status"] == "NOT_APPLICABLE"
    assert by_lane["classical_photogrammetry"]["status"] == "BLOCKED_BACKEND"
    assert by_lane["generative_image_to_3d"]["status"] == "BLOCKED_BACKEND"
    visual_run = GeometryEvidenceStore(project).get(by_lane["visual_hull"]["geometry_run_id"])
    assert len(visual_run["evidence"]["mask_artifacts"]) == 1
    assert result["acceptance_performed"] is False
