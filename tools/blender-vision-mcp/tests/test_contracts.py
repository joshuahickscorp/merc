import ast
import json
from pathlib import Path

import pytest
from jsonschema import Draft202012Validator

from blender_vision.benchmarks.mac_studio import import_feature_candidates
from blender_vision.constraints.models import Constraint, ConstraintType
from blender_vision.core.errors import ProjectError
from blender_vision.core.models import EvidenceClass
from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.features.ontology import FeatureType, TechnicalFeature
from blender_vision.features.store import FeatureStore
from blender_vision.optimization.losses import LossTerms, LossWeights, weighted_loss
from blender_vision.parametric.components import ComponentSpec, ComponentType
from blender_vision.parametric.fitting import ComponentFitter
from blender_vision.parametric.store import ComponentStore
from blender_vision.projects.store import ProjectStore
from blender_vision.repairs.store import RepairStore


def test_every_public_json_schema_is_valid_draft_2020_12() -> None:
    repository = Path(__file__).resolve().parents[1]
    schemas = sorted((repository / "schemas").glob("*.schema.json"))
    assert schemas
    for path in schemas:
        Draft202012Validator.check_schema(json.loads(path.read_text(encoding="utf-8")))


def test_blender_worker_exposes_the_complete_safe_operation_vocabulary() -> None:
    repository = Path(__file__).resolve().parents[1]
    source = (repository / "blender_worker" / "entry.py").read_text(encoding="utf-8")
    module = ast.parse(source)
    assignment = next(
        node
        for node in module.body
        if isinstance(node, ast.Assign)
        and any(
            isinstance(target, ast.Name) and target.id == "ALLOWED_OPERATIONS"
            for target in node.targets
        )
    )
    worker_operations = ast.literal_eval(assignment.value)
    schema = json.loads(
        (repository / "schemas" / "job-manifest.schema.json").read_text(encoding="utf-8")
    )
    schema_operations = set(schema["properties"]["operation"]["enum"])
    required = {
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
        "prepare_asset",
        "generate_synthetic_dataset",
        "generate_asset_preparation_benchmark",
        "save_checkpoint",
    }
    assert required <= worker_operations
    assert worker_operations == schema_operations


def test_glb_workers_export_only_renderable_scene_objects() -> None:
    repository = Path(__file__).resolve().parents[1]
    worker_paths = (
        repository / "blender_worker" / "entry.py",
        repository / "src" / "blender_vision" / "blender" / "worker_entry.py",
    )
    for path in worker_paths:
        module = ast.parse(path.read_text(encoding="utf-8"))
        function = next(
            node
            for node in module.body
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
            and node.name == "export_glb"
        )
        export_call = next(
            node
            for node in ast.walk(function)
            if isinstance(node, ast.Call)
            and isinstance(node.func, ast.Attribute)
            and node.func.attr == "gltf"
        )
        keywords = {keyword.arg: keyword.value for keyword in export_call.keywords}

        assert ast.literal_eval(keywords["use_renderable"]) is True


def test_render_workers_disable_random_output_dither() -> None:
    repository = Path(__file__).resolve().parents[1]
    worker_paths = (
        repository / "blender_worker" / "entry.py",
        repository / "src" / "blender_vision" / "blender" / "worker_entry.py",
    )
    for path in worker_paths:
        module = ast.parse(path.read_text(encoding="utf-8"))
        function = next(
            node
            for node in module.body
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
            and node.name == "render_passes"
        )
        assignments = [
            node
            for node in ast.walk(function)
            if isinstance(node, ast.Assign)
            and any(
                isinstance(target, ast.Attribute) and target.attr == "dither_intensity"
                for target in node.targets
            )
        ]

        assert assignments
        assert any(ast.literal_eval(node.value) == 0.0 for node in assignments)


def test_feature_and_component_contracts_preserve_evidence() -> None:
    feature = TechnicalFeature(
        id="rear-usbc-1",
        type=FeatureType.USB_C,
        parent_component="rear-panel",
        coordinate_frame="canonical",
        observations=[{"reference_id": "rear"}],
        reference_ids=["rear"],
        confidence=0.8,
        evidence_class=EvidenceClass.SINGLE_VIEW_OBSERVED,
    )
    constraint = Constraint(
        id="port-offset",
        type=ConstraintType.FIXED_OFFSET,
        subjects=[feature.id, "rear-panel"],
        evidence_bindings=[feature.id],
    )
    component = ComponentSpec(
        id="rear-panel",
        type=ComponentType.PANEL,
        parameters={"width_mm": 190.0},
        constraints=[constraint],
        evidence_bindings=[feature.id],
    )
    assert feature.to_dict()["evidence_class"] == "SINGLE_VIEW_OBSERVED"
    assert component.to_dict()["constraints"][0]["type"] == "fixed_offset"


def test_multi_objective_loss_uses_all_terms() -> None:
    terms = LossTerms(silhouette=2.0, feature=3.0, measurement=5.0)
    weights = LossWeights(silhouette=2.0, feature=1.0, measurement=0.5)
    assert weighted_loss(terms, weights) == 9.5


def test_evidence_and_component_stores_are_revisioned(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Stores")
    measurement = MeasurementStore(project).add(
        "known_overall_dimension",
        {"axis": "x", "millimetres": 197.0},
        qualifier="exact",
        evidence_class=EvidenceClass.MANUFACTURER_SPEC,
    )
    feature = TechnicalFeature(
        id="rear-port",
        type=FeatureType.USB_C,
        parent_component="rear-panel",
        coordinate_frame="canonical",
        observations=[],
        reference_ids=[],
        confidence=0.9,
        evidence_class=EvidenceClass.MULTI_VIEW_OBSERVED,
    )
    FeatureStore(project).add(
        feature.type.value,
        parent_component=feature.parent_component or "unassigned",
        dimensions=feature.dimensions,
        coordinate_frame=feature.coordinate_frame,
        observations=feature.observations,
        reference_ids=feature.reference_ids,
        confidence=feature.confidence,
        uncertainty=feature.uncertainty,
        evidence_class=feature.evidence_class,
        model_revision="test",
    )
    component = ComponentStore(project).create(
        ComponentSpec(
            id="rear-panel",
            type=ComponentType.PANEL,
            parameters={"width_mm": 190.0},
            evidence_bindings=[measurement["id"], feature.id],
        )
    )
    revised = ComponentStore(project).update_parameters(component["id"], {"width_mm": 197.0})
    assert revised["revision"] == 2
    assert project.status()["counts"]["components"] == 1


def test_mac_benchmark_candidates_are_scene_bound_and_unapproved(tmp_path: Path) -> None:
    package_root = Path(__file__).resolve().parents[1]
    manifest = __import__("json").loads(
        (package_root / "benchmarks/mac_studio/benchmark.json").read_text(encoding="utf-8")
    )
    names = [
        name for candidate in manifest["feature_candidates"] for name in candidate["scene_objects"]
    ]
    inventory = {
        "canonical_transform": {"scale_to_millimetres": 1000.0},
        "objects": [
            {
                "name": name,
                "location": [0.001, 0.002, 0.003],
                "dimensions": [0.004, 0.005, 0.006],
            }
            for name in names
        ],
    }
    project = ProjectStore.create(tmp_path / "mac", "Mac")
    result = import_feature_candidates(
        project,
        manifest,
        inventory,
        scene_id="scene-1",
        scene_artifact_digest="a" * 64,
    )
    assert result["feature_count"] == 16
    assert result["missing_scene_objects"] == []
    assert all(feature["human_approval"] is False for feature in result["features"])
    assert all(
        feature["observations"][0]["dimensions_mm"] == [4.0, 5.0, 6.0]
        for feature in result["features"]
    )
    grille = next(feature for feature in result["features"] if feature["type"] == "grille")
    assert grille["hero_surface"] is True
    assert grille["evidence_class"] == "INFERRED_LOW_CONFIDENCE"


def test_feature_review_requires_named_evidence_bound_decision(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Feature Review")
    store = FeatureStore(project)
    feature = store.add(
        FeatureType.USB_C,
        parent_component="rear-panel",
        observations=[{"kind": "scene_object", "object_name": "rear-usbc"}],
        confidence=0.8,
        evidence_class=EvidenceClass.INFERRED_HIGH_CONFIDENCE,
        model_revision="scene-sha",
    )
    reviewed = store.review(
        feature["id"], approved=True, reviewer="A. Reviewer", reason="Compared in two views"
    )
    assert reviewed["human_approval"] is True
    assert reviewed["approval"]["state"] == "approved"
    assert reviewed["approval"]["reviewer"] == "A. Reviewer"


def test_repair_store_requires_named_approval_before_application(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Repair governance")
    store = RepairStore(project)
    proposal = store.propose(
        "test_repair",
        {"parameter_mm": 1.0},
        evidence_bindings=[{"kind": "measurement", "id": "measurement-1"}],
        expected_improvement={"residual": {"before": 1.0, "expected": 0.5}},
    )
    assert proposal["status"] == "proposed"
    with pytest.raises(ProjectError, match="not approved"):
        store.mark_applied(proposal["id"], {"acceptance": {"accepted": False}})
    approved = store.approve(proposal["id"], "Test Reviewer")
    assert approved["status"] == "approved"
    assert approved["approved_by"] == "Test Reviewer"
    applied = store.mark_applied(
        proposal["id"], {"acceptance": {"accepted": False, "reason": "test evidence only"}}
    )
    assert applied["status"] == "applied"
    assert applied["result"]["acceptance"]["accepted"] is False
    with pytest.raises(ValueError, match="receipt"):
        store.review_applied(
            proposal["id"],
            accepted=True,
            reviewer="Test Reviewer",
            reason="Looks correct",
        )
    rejected = store.review_applied(
        proposal["id"],
        accepted=False,
        reviewer="Test Reviewer",
        reason="Reference residual remains too large",
    )
    assert rejected["status"] == "rejected"
    assert rejected["result"]["acceptance"]["reviewer"] == "Test Reviewer"


def test_component_fitting_requires_named_revision_checked_review(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Component fitting")
    measurement = MeasurementStore(project).add(
        "line",
        {"role": "panel_width", "millimetres": 197.0},
        evidence_class=EvidenceClass.MEASURED,
        certainty="bounded",
        uncertainty={"millimetres": 0.5},
    )
    ComponentStore(project).create(
        ComponentSpec(
            id="panel",
            type=ComponentType.PANEL,
            parameters={"width_mm": 190.0, "height_mm": 95.0},
            evidence_bindings=[measurement["id"]],
        )
    )
    fitter = ComponentFitter(project)
    proposed = fitter.propose("panel", {"width_mm": [measurement["id"]]})
    assert proposed["status"] == "proposed"
    assert proposed["result"]["candidate_parameters"] == {"width_mm": 197.0}
    assert ComponentStore(project).get("panel")["parameters"]["width_mm"] == 190.0
    accepted = fitter.review(
        proposed["id"],
        accepted=True,
        reviewer="Test Reviewer",
        reason="Authoritative dimension checked against source evidence",
    )
    assert accepted["status"] == "accepted"
    assert accepted["reviewer"] == "Test Reviewer"
    assert accepted["applied_revision"] == 2
    assert ComponentStore(project).get("panel")["parameters"]["width_mm"] == 197.0
    assert project.status()["counts"]["component_fits"] == 1
