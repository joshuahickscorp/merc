from __future__ import annotations

import json
import uuid
from pathlib import Path

from PIL import Image, ImageDraw

from blender_vision.acceptance.receipts import evaluate_acceptance
from blender_vision.blender.passes import (
    GOVERNED_RENDER_PASSES,
    INDUSTRIAL_SURFACE_RENDER_PASSES,
    MAXIMAL_VISUAL_RENDER_PASSES,
)
from blender_vision.cameras.solver import CameraSolver
from blender_vision.core.util import utc_now
from blender_vision.evidence.masks import ReferenceMaskStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore
from blender_vision.visual_geometry.audit import ManufacturedFormAuditor
from blender_vision.visual_geometry.baseline import VisualBaselineStore
from blender_vision.visual_geometry.bindings import SemanticBindingStore
from blender_vision.visual_geometry.diagnosis import (
    DEFECT_CLASSES,
    VisualDefectDiagnosisStore,
)
from blender_vision.visual_geometry.packets import (
    ComponentTaskPacketStore,
    VisualFrequencyScoreStore,
)
from blender_vision.visual_geometry.store import VisualGeometryStore


def _camera(reference_id: str) -> dict:
    return {
        "reference_id": reference_id,
        "model": "PINHOLE",
        "width": 160,
        "height": 120,
        "intrinsics": {"fx": 130.0, "fy": 130.0, "cx": 80.0, "cy": 60.0},
        "world_from_camera": [
            [1.0, 0.0, 0.0, 0.0],
            [0.0, 1.0, 0.0, 500.0],
            [0.0, 0.0, 1.0, 50.0],
            [0.0, 0.0, 0.0, 1.0],
        ],
        "confidence": 0.9,
        "registration_class": "approximate_visual_registration",
        "evidence_class": "SINGLE_VIEW_OBSERVED",
    }


def test_maximal_passes_extend_without_rewriting_frozen_governed_minimum() -> None:
    assert len(GOVERNED_RENDER_PASSES) == 25
    assert {
        "normal_discontinuity",
        "highlight_flow",
    } == INDUSTRIAL_SURFACE_RENDER_PASSES
    assert MAXIMAL_VISUAL_RENDER_PASSES == (
        GOVERNED_RENDER_PASSES | INDUSTRIAL_SURFACE_RENDER_PASSES
    )


def test_procedural_calibration_does_not_inherit_product_visual_geometry_gates(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(
        tmp_path / "calibration-policy",
        "Procedural calibration policy fixture",
        metadata={"benchmark": "synthetic_calibration_v1"},
    )

    acceptance = evaluate_acceptance(project)

    assert acceptance["metrics"]["visual_geometry"]["policy_required"] is False
    product_visual_blockers = {
        "L3+ requires an immutable visual baseline freeze covering the authoritative scene",
        "L3+ requires a receipt-valid fixed rig with approved cameras",
        "L3+ visual-geometry scorecards do not cover every acceptance reference",
        "L3+ requires a replayable manufactured-form audit",
        "L3+ visual acceptance is blocked by unbound visible geometry",
        "L3+ requires a component-weighted primary/secondary/tertiary scorecard",
    }
    assert product_visual_blockers.isdisjoint(acceptance["blockers"])


def _clean_inventory() -> dict:
    return {
        "canonical_transform": {"scale_to_millimetres": 1000.0},
        "canonical_bounds_mm": {"dimensions": [150.0, 150.0, 50.0]},
        "objects": [
            {
                "name": "enclosure",
                "type": "MESH",
                "hidden_render": False,
                "component_id": "enclosure",
                "modifiers": [{"name": "edge-treatment", "type": "BEVEL"}],
                "world_bounds": {
                    "minimum": [-0.075, -0.075, -0.025],
                    "maximum": [0.075, 0.075, 0.025],
                    "dimensions": [0.15, 0.15, 0.05],
                },
                "mesh": {
                    "vertices": 8,
                    "edges": 12,
                    "polygons": 6,
                    "loose_vertices": 0,
                    "degenerate_polygons": 0,
                    "duplicate_vertex_positions": 0,
                    "duplicate_faces": 0,
                    "audit_sampling": {"exact": True},
                    "normal_diagnostics": {
                        "zero_length_polygon_normals": 0,
                        "mirrored_transform": False,
                    },
                    "topology": {"non_manifold_edges": 0},
                },
            }
        ],
        "component_correspondence": {
            "bound": [{"object": "enclosure", "component_id": "enclosure"}],
            "unbound_mesh_objects": [],
            "bound_fraction": 1.0,
        },
        "audit_findings": [],
    }


def _fixture(tmp_path: Path, *, approve_camera: bool) -> dict:
    project = ProjectStore.create(
        tmp_path / "project",
        "Visual geometry fixture",
        metadata={"benchmark": "test_device"},
    )
    reference_path = tmp_path / "reference.png"
    reference_image = Image.new("RGB", (160, 120), (24, 28, 34))
    ImageDraw.Draw(reference_image).rounded_rectangle(
        (25, 20, 135, 103), radius=14, fill=(190, 192, 195)
    )
    reference_image.save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path,
        rights_state="SYNTHETIC_OWNED",
        viewpoint_label="front",
    )
    mask_store = ReferenceMaskStore(project)
    proposal = mask_store.propose_automatic(reference["id"])
    camera = CameraSolver(project).import_manual([_camera(reference["id"])])
    if approve_camera:
        CameraSolver(project).approve(
            camera["id"],
            reviewer="Camera reviewer",
            reason="Exact synthetic fixture camera was reviewed",
        )
    scene_path = tmp_path / "fixture.blend"
    scene_path.write_bytes(b"synthetic fixture")
    scene = SceneStore(project).import_blend(scene_path)
    SceneStore(project).set_inventory(scene["id"], _clean_inventory())
    rig = VisualGeometryStore(project).create_rig(
        scene_id=scene["id"],
        camera_solution_id=camera["id"],
        maximum_dimension=160,
    )
    pass_digests = {
        name: (
            proposal["mask_artifact_digest"]
            if name in {"silhouette", "object_id"}
            else reference["artifact"]["digest"]
        )
        for name in MAXIMAL_VISUAL_RENDER_PASSES
    }
    render_run_id = str(uuid.uuid4())
    with project.connection() as connection:
        connection.execute(
            "INSERT INTO render_runs"
            "(id,scene_id,camera_solution_id,config_json,outputs_json,created_at) "
            "VALUES(?,?,?,?,?,?)",
            (
                render_run_id,
                scene["id"],
                camera["id"],
                json.dumps(
                    {
                        "maximum_dimension": 160,
                        "passes": sorted(MAXIMAL_VISUAL_RENDER_PASSES),
                    }
                ),
                json.dumps(
                    [
                        {
                            "reference_id": reference["id"],
                            "artifact_digest": reference["artifact"]["digest"],
                            "pass_artifact_digests": pass_digests,
                            "width": 160,
                            "height": 120,
                            "object_ids": {
                                "enclosure": {
                                    "rgb": [255, 255, 255],
                                    "component_id": "enclosure",
                                    "component_type": "Body",
                                }
                            },
                            "component_ids": {"enclosure": [128, 64, 32]},
                        }
                    ]
                ),
                utc_now(),
            ),
        )
    return {
        "project": project,
        "reference": reference,
        "proposal": proposal,
        "camera": camera,
        "scene": scene,
        "rig": rig,
        "render_run_id": render_run_id,
    }


def test_fixed_rig_preserves_camera_authority_and_detects_tampering(tmp_path: Path) -> None:
    fixture = _fixture(tmp_path, approve_camera=False)
    store = VisualGeometryStore(fixture["project"])
    assert fixture["rig"]["state"] == "DIAGNOSTIC_PROPOSAL"
    assert store.verify_rig(fixture["rig"]["id"])["valid"] is True

    with fixture["project"].connection() as connection:
        row = connection.execute(
            "SELECT config_json FROM visual_geometry_rigs WHERE id=?",
            (fixture["rig"]["id"],),
        ).fetchone()
        config = json.loads(row["config_json"])
        config["maximum_dimension"] = 2048
        connection.execute(
            "UPDATE visual_geometry_rigs SET config_json=? WHERE id=?",
            (json.dumps(config), fixture["rig"]["id"]),
        )
    assert store.verify_rig(fixture["rig"]["id"])["valid"] is False


def test_proposed_mask_scorecard_is_replayable_but_diagnostic_only(tmp_path: Path) -> None:
    fixture = _fixture(tmp_path, approve_camera=True)
    store = VisualGeometryStore(fixture["project"])
    scorecard = store.evaluate(
        rig_id=fixture["rig"]["id"],
        reference_id=fixture["reference"]["id"],
        render_run_id=fixture["render_run_id"],
        mask_proposal_id=fixture["proposal"]["id"],
    )

    assert fixture["rig"]["state"] == "AUTHORITATIVE"
    assert scorecard["status"] == "DIAGNOSTIC_ONLY"
    assert scorecard["projection"]["metrics"]["silhouette_iou"] == 1.0
    assert scorecard["projection"]["metrics"]["boundary_rmse_px"] == 0.0
    assert scorecard["pass_coverage"]["complete"] is True
    assert scorecard["local_geometry"]["status"] == "NOT_EVALUATED"
    assert store.verify_scorecard(scorecard["id"], replay=True)["valid"] is True

    acceptance = evaluate_acceptance(fixture["project"])
    visual = acceptance["metrics"]["visual_geometry"]
    assert visual["policy_required"] is True
    assert scorecard["id"] in visual["diagnostic_scorecard_ids"]
    assert (
        "L3+ visual-geometry scorecards do not cover every acceptance reference"
        in acceptance["blockers"]
    )


def test_reviewed_mask_scorecard_cannot_hide_missing_local_geometry(tmp_path: Path) -> None:
    fixture = _fixture(tmp_path, approve_camera=True)
    reviewed = ReferenceMaskStore(fixture["project"]).review_proposal(
        fixture["proposal"]["id"],
        accepted=True,
        reviewer="Mask reviewer",
        reason="Synthetic object boundary was reviewed pixel by pixel",
    )
    scorecard = VisualGeometryStore(fixture["project"]).evaluate(
        rig_id=fixture["rig"]["id"],
        reference_id=fixture["reference"]["id"],
        render_run_id=fixture["render_run_id"],
        mask_id=reviewed["approved_mask_id"],
    )

    assert scorecard["status"] == "BLOCKED"
    assert scorecard["projection"]["gates"] == {
        "silhouette_iou": True,
        "silhouette_dice": True,
        "foreground_precision": True,
        "foreground_recall": True,
        "boundary_rmse": True,
    }
    assert {item["cause"] for item in scorecard["cause_attribution"]} == {"REFERENCE"}


def test_residual_diagnosis_binds_required_repair_context_and_replays(
    tmp_path: Path,
) -> None:
    fixture = _fixture(tmp_path, approve_camera=False)
    scorecard = VisualGeometryStore(fixture["project"]).evaluate(
        rig_id=fixture["rig"]["id"],
        reference_id=fixture["reference"]["id"],
        render_run_id=fixture["render_run_id"],
        mask_proposal_id=fixture["proposal"]["id"],
    )
    store = VisualDefectDiagnosisStore(fixture["project"])
    report = store.create(
        scorecard_id=scorecard["id"],
        rollback_scene_id=fixture["scene"]["id"],
    )

    assert set(report["supported_defect_classes"]) == DEFECT_CLASSES
    assert report["status"] == "DIAGNOSED"
    assert report["diagnoses"]
    assert {item["defect_class"] for item in report["diagnoses"]} == {
        "EVIDENCE_MISSING"
    }
    for diagnosis in report["diagnoses"]:
        assert diagnosis["semantic_component"]
        assert diagnosis["views"] == [
            {
                "reference_id": fixture["reference"]["id"],
                "camera_solution_id": fixture["camera"]["id"],
            }
        ]
        assert diagnosis["image_regions_xyxy"] == [[0, 0, 160, 120]]
        assert isinstance(diagnosis["candidate_parameters"], dict)
        assert 0.0 <= diagnosis["confidence"] <= 1.0
        assert diagnosis["expected_visual_impact"]
        assert diagnosis["expected_gate_impact"]
        assert diagnosis["rollback_checkpoint"]["scene_id"] == fixture["scene"]["id"]
        assert diagnosis["authority"] == (
            "DIAGNOSTIC_HYPOTHESIS_NO_REPAIR_OR_ACCEPTANCE_AUTHORITY"
        )
    assert store.verify(report["id"])["valid"] is True

    with fixture["project"].connection() as connection:
        row = connection.execute(
            "SELECT report_json FROM visual_defect_diagnoses WHERE id=?",
            (report["id"],),
        ).fetchone()
        tampered = json.loads(row["report_json"])
        tampered["diagnoses"][0]["confidence"] = 0.01
        connection.execute(
            "UPDATE visual_defect_diagnoses SET report_json=? WHERE id=?",
            (json.dumps(tampered), report["id"]),
        )
    assert store.verify(report["id"])["valid"] is False


def test_manufactured_form_audit_replays_and_flags_objective_failures(
    tmp_path: Path,
) -> None:
    fixture = _fixture(tmp_path, approve_camera=False)
    auditor = ManufacturedFormAuditor(fixture["project"])
    clean = auditor.audit(fixture["scene"]["id"])
    assert clean["status"] == "PASS"
    assert clean["summary"]["hard_failure_count"] == 0
    assert auditor.verify(clean["id"])["valid"] is True

    malformed = _clean_inventory()
    mesh = malformed["objects"][0]["mesh"]
    mesh["degenerate_polygons"] = 2
    mesh["duplicate_faces"] = 1
    mesh["normal_diagnostics"]["zero_length_polygon_normals"] = 1
    mesh["topology"]["non_manifold_edges"] = 4
    SceneStore(fixture["project"]).set_inventory(fixture["scene"]["id"], malformed)
    failed = auditor.audit(fixture["scene"]["id"])

    assert failed["status"] == "FAIL"
    assert failed["summary"]["hard_failure_count"] == 4
    assert {item["code"] for item in failed["hard_failures"]} == {
        "DEGENERATE_POLYGONS",
        "DUPLICATE_FACES",
        "NON_MANIFOLD_GEOMETRY",
        "ZERO_LENGTH_NORMALS",
    }
    assert auditor.verify(failed["id"])["valid"] is True
    assert auditor.verify(clean["id"])["valid"] is False


def test_visual_baseline_freeze_captures_and_replays_all_comparison_state(
    tmp_path: Path,
) -> None:
    fixture = _fixture(tmp_path, approve_camera=False)
    store = VisualBaselineStore(fixture["project"])
    baseline = store.freeze(
        label="pre-convergence fixture",
        scene_ids=[fixture["scene"]["id"]],
    )

    assert baseline["state"] == "CURRENT_DIAGNOSTIC_BASELINE"
    assert baseline["snapshot"]["scene_ids"] == [fixture["scene"]["id"]]
    assert baseline["snapshot"]["tables"]["visual_geometry_rigs"]
    assert baseline["snapshot"]["tables"]["render_runs"]
    assert store.verify(baseline["id"])["valid"] is True
    assert (
        store.freeze(
            label="same bytes are idempotent",
            scene_ids=[fixture["scene"]["id"]],
        )["id"]
        == baseline["id"]
    )

    changed = _clean_inventory()
    changed["canonical_bounds_mm"]["dimensions"][0] = 151.0
    SceneStore(fixture["project"]).set_inventory(fixture["scene"]["id"], changed)
    verification = store.verify(baseline["id"])
    assert verification["valid"] is False
    assert verification["captured_rows_valid"] is False


def test_visible_geometry_binding_requires_separate_review_and_acceptance(
    tmp_path: Path,
) -> None:
    fixture = _fixture(tmp_path, approve_camera=False)
    store = SemanticBindingStore(fixture["project"])
    proposal = store.propose_scene(fixture["scene"]["id"])

    assert proposal["coverage"]["visible_mesh_count"] == 1
    assert proposal["coverage"]["all_visible_resolved"] is True
    assert proposal["coverage"]["all_visible_reviewed"] is False
    binding = proposal["bindings"][0]
    assert binding["state"] == "PROVISIONALLY_BOUND"
    assert store.verify(binding["id"])["valid"] is True
    original_proposal_digest = binding["proposal_digest"]
    binding = store.repropose(
        binding["id"],
        reason="Synthetic classifier revision exercise",
        classification={
            "semantic_component": "enclosure_component",
            "parent_assembly": "enclosure_assembly",
            "confidence": 0.95,
        },
    )
    assert binding["record"]["proposal_revision"] == 2
    assert binding["record"]["previous_proposal_digest"] == original_proposal_digest
    assert store.verify(binding["id"])["valid"] is True
    assert store.assembly_audit(fixture["scene"]["id"])["missing_part_of_objects"] == []

    reviewed = store.review(
        binding["id"],
        state="REVIEWED_BOUND",
        reviewer="Semantic reviewer",
        reason="Synthetic enclosure name, bounds, and hierarchy were inspected",
    )
    assert reviewed["state"] == "REVIEWED_BOUND"
    assert store.coverage(fixture["scene"]["id"])["all_visible_reviewed"] is True
    assert store.coverage(fixture["scene"]["id"])["all_visible_accepted"] is False

    accepted = store.review(
        binding["id"],
        state="ACCEPTED_BOUND",
        reviewer="Acceptance reviewer",
        reason="The reviewed synthetic binding is accepted for this fixture",
    )
    assert accepted["state"] == "ACCEPTED_BOUND"
    assert store.verify(binding["id"])["valid"] is True
    assert store.coverage(fixture["scene"]["id"])["all_visible_accepted"] is True


def test_component_packet_uses_native_crop_and_frequency_scores_cannot_hide_gaps(
    tmp_path: Path,
) -> None:
    fixture = _fixture(tmp_path, approve_camera=False)
    binding_store = SemanticBindingStore(fixture["project"])
    proposed = binding_store.propose_scene(
        fixture["scene"]["id"],
        classifications={
            "enclosure": {
                "reference_regions": [
                    {
                        "reference_id": fixture["reference"]["id"],
                        "mask_artifact_digest": fixture["proposal"]["mask_artifact_digest"],
                        "landmarks": [],
                    }
                ]
            }
        },
    )
    binding_id = proposed["bindings"][0]["id"]
    binding_store.review(
        binding_id,
        state="REVIEWED_BOUND",
        reviewer="Component reviewer",
        reason="Synthetic component region and object identity were reviewed",
    )
    binding_store.review(
        binding_id,
        state="ACCEPTED_BOUND",
        reviewer="Acceptance reviewer",
        reason="Synthetic component mask and semantic binding are accepted",
    )

    packet_store = ComponentTaskPacketStore(fixture["project"])
    created = packet_store.create(
        binding_id=binding_id,
        rig_id=fixture["rig"]["id"],
        render_run_id=fixture["render_run_id"],
    )
    packet = created["packets"][0]
    assert packet["status"] == "COMPONENT_EVALUATED"
    assert packet["acceptance_status"] == "PASS"
    assert packet["metrics"]["projection"]["metrics"]["silhouette_iou"] == 1.0
    assert packet["artifacts"]["reference_native"]["status"] == "AVAILABLE"
    assert packet["artifacts"]["candidate_passes"]["zebra"]["status"] == "AVAILABLE"
    assert packet_store.verify(packet["id"])["valid"] is True

    frequency_store = VisualFrequencyScoreStore(fixture["project"])
    scorecard = frequency_store.create(
        scene_id=fixture["scene"]["id"],
        rig_id=fixture["rig"]["id"],
        packet_ids=[packet["id"]],
    )
    assert scorecard["scores"]["semantic_component_weighted_mean"] == 1.0
    assert scorecard["visual_frequencies"]["PRIMARY_FORM"]["status"] == "PASS"
    assert scorecard["visual_frequencies"]["SECONDARY_FORM"]["status"] == "BLOCKED"
    assert scorecard["visual_frequencies"]["TERTIARY_FORM"]["status"] == "BLOCKED"
    assert scorecard["status"] == "BLOCKED"
    assert frequency_store.verify(scorecard["id"])["valid"] is True
