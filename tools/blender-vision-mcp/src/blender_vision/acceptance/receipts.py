from __future__ import annotations

import hashlib
import json
import platform
import uuid
from fnmatch import fnmatchcase
from pathlib import Path
from typing import Any

from blender_vision.acceptance.lifecycle import audit_scene_lifecycle
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.backends.generative3d import GenerativeProposalStore
from blender_vision.cameras.decisions import CameraDecisionStore
from blender_vision.cameras.landmarks import CameraLandmarkStore
from blender_vision.comparison.store import ComparisonStore
from blender_vision.core.config import discover_blender
from blender_vision.core.models import FidelityLevel, RegistrationClass
from blender_vision.core.util import (
    atomic_write_json,
    atomic_write_text,
    canonical_json,
    runtime_revision,
    sha256_file,
    utc_now,
)
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.adoption import LegacyReferenceAdoptionStore
from blender_vision.evidence.conflicts import EvidenceConflictStore
from blender_vision.evidence.derivations import ReferenceDerivationStore
from blender_vision.evidence.discovery import SearchProviderStore
from blender_vision.evidence.duplicates import EvidenceDuplicateStore
from blender_vision.evidence.masks import ReferenceMaskStore
from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.evidence.targets import TargetResolver
from blender_vision.intelligence.active_learning import audit_active_learning
from blender_vision.projects.store import ProjectStore
from blender_vision.security.paths import safe_mode
from blender_vision.vision.store import ARTIFACT_FIELDS
from blender_vision.visual_geometry.audit import ManufacturedFormAuditor
from blender_vision.visual_geometry.baseline import VisualBaselineStore
from blender_vision.visual_geometry.bindings import SemanticBindingStore
from blender_vision.visual_geometry.packets import (
    ComponentTaskPacketStore,
    VisualFrequencyScoreStore,
)
from blender_vision.visual_geometry.store import VisualGeometryStore


def _scoped_audited_dimensions(
    inventory: dict[str, Any],
    measurements: list[dict[str, Any]],
    project_metadata: dict[str, Any],
) -> tuple[list[float] | None, dict[str, Any]]:
    """Resolve a measured body scope without conflating it with scene appendages."""
    scopes = {item["value"].get("scope") for item in measurements}
    scopes.discard(None)
    scope = next(iter(scopes)) if len(scopes) == 1 else None
    pattern_sets = {
        tuple(item["value"].get("object_patterns", []))
        for item in measurements
        if item["value"].get("object_patterns")
    }
    source = "measurement"
    patterns = list(next(iter(pattern_sets))) if len(pattern_sets) == 1 else []
    configured = project_metadata.get("metadata", {}).get("body_envelope", {})
    if not patterns and scope and configured.get("scope") == scope:
        patterns = list(configured.get("object_patterns", []))
        source = "project_metadata"
    if not patterns:
        dimensions = inventory.get("canonical_bounds_mm", {}).get("dimensions")
        return dimensions, {
            "kind": "whole_scene",
            "scope": None,
            "object_patterns": [],
            "object_count": len(inventory.get("objects", [])),
            "source": "scene_inventory",
        }

    objects = [
        item
        for item in inventory.get("objects", [])
        if item.get("type") == "MESH"
        and not item.get("hidden_render", False)
        and isinstance(item.get("world_bounds"), dict)
        and any(fnmatchcase(str(item.get("name", "")), pattern) for pattern in patterns)
    ]
    if not objects:
        return None, {
            "kind": "object_pattern_scope",
            "scope": scope,
            "object_patterns": patterns,
            "object_count": 0,
            "source": source,
            "error": "scope patterns matched no renderable mesh objects",
        }
    scale = float(inventory.get("canonical_transform", {}).get("scale_to_millimetres", 1000.0))
    minimum = [
        min(float(item["world_bounds"]["minimum"][axis]) for item in objects) * scale
        for axis in range(3)
    ]
    maximum = [
        max(float(item["world_bounds"]["maximum"][axis]) for item in objects) * scale
        for axis in range(3)
    ]
    return [maximum[axis] - minimum[axis] for axis in range(3)], {
        "kind": "object_pattern_scope",
        "scope": scope,
        "object_patterns": patterns,
        "object_count": len(objects),
        "objects": sorted(str(item["name"]) for item in objects),
        "source": source,
    }


def _camera_document_approved(camera: dict[str, Any]) -> bool:
    stored = camera.get("solution", {})
    if not isinstance(stored, dict):
        return False
    approval = stored.get("approval", {})
    return bool(
        camera.get("approved")
        and stored.get("approved")
        and isinstance(approval, dict)
        and approval.get("state") == "approved"
        and approval.get("reviewer")
        and approval.get("reason")
        and camera.get("decision_receipt_valid") is True
    )


def _active_camera_set(
    cameras: list[dict[str, Any]],
    preferred_solution_ids_by_reference: dict[str, set[str]] | None = None,
) -> dict[str, Any]:
    """Select validated per-reference cameras, falling back to the newest proposal."""
    active_by_reference: dict[str, tuple[dict[str, Any], dict[str, Any]]] = {}
    validated_by_reference: dict[str, tuple[dict[str, Any], dict[str, Any]]] = {}
    preferred = preferred_solution_ids_by_reference or {}
    for document in cameras:
        stored = document.get("solution", {})
        solutions = stored.get("cameras", []) if isinstance(stored, dict) else stored
        if not isinstance(solutions, list):
            continue
        for solution in solutions:
            if not isinstance(solution, dict) or not solution.get("reference_id"):
                continue
            reference_id = str(solution["reference_id"])
            active_by_reference[reference_id] = (document, solution)
            if str(document.get("id", "")) in preferred.get(reference_id, set()):
                validated_by_reference[reference_id] = (document, solution)
    active_by_reference.update(validated_by_reference)

    covered_reference_ids = set(active_by_reference)
    approved_reference_ids = {
        reference_id
        for reference_id, (document, _solution) in active_by_reference.items()
        if _camera_document_approved(document)
    }
    active_solution_ids = sorted(
        {
            str(document["id"])
            for document, _solution in active_by_reference.values()
            if document.get("id")
        }
    )
    return {
        "registration_classes": sorted(
            {
                str(solution["registration_class"])
                for _document, solution in active_by_reference.values()
                if solution.get("registration_class")
            }
        ),
        "approved": bool(active_by_reference) and approved_reference_ids == covered_reference_ids,
        "covered_reference_ids": sorted(covered_reference_ids),
        "approved_reference_ids": sorted(approved_reference_ids),
        "active_solution_ids": active_solution_ids,
        "solution_ids_by_reference": {
            reference_id: str(document["id"])
            for reference_id, (document, _solution) in sorted(active_by_reference.items())
            if document.get("id")
        },
    }


def _compact_historical_scene_inventories(records: dict[str, Any]) -> None:
    """Keep the acceptance scene complete without repeating every old object list."""
    scenes = records.get("scenes", [])
    if not isinstance(scenes, list):
        return
    authoritative_id = next(
        (scene["id"] for scene in scenes if scene.get("is_authoritative")), None
    )
    for scene in scenes:
        if scene["id"] == authoritative_id:
            continue
        inventory = scene.get("inventory")
        if not isinstance(inventory, dict):
            continue
        findings = inventory.get("audit_findings", [])
        objects = inventory.get("objects", [])
        scene["inventory"] = {
            "record_kind": "historical_inventory_summary",
            "full_object_inventory_in_receipt": False,
            "sha256": hashlib.sha256(canonical_json(inventory)).hexdigest(),
            "object_count": len(objects) if isinstance(objects, list) else 0,
            "renderable_mesh_count": sum(
                isinstance(item, dict)
                and item.get("type") == "MESH"
                and not item.get("hidden_render", False)
                for item in objects
            )
            if isinstance(objects, list)
            else 0,
            "canonical_transform": inventory.get("canonical_transform"),
            "canonical_bounds_mm": inventory.get("canonical_bounds_mm"),
            "audit_findings": findings if isinstance(findings, list) else [],
        }


def _component_fit_decision_valid(project: ProjectStore, fit: dict[str, Any]) -> bool:
    if fit["status"] == "proposed":
        return fit.get("decision_digest") is None
    digest = fit.get("decision_digest")
    if not isinstance(digest, str):
        return False
    try:
        path = ArtifactStore(project).path_for(digest)
        if not path.is_file() or sha256_file(path)[0] != digest:
            return False
        decision = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError, json.JSONDecodeError):
        return False
    return (
        decision.get("receipt_type") == "component_fit_review"
        and decision.get("fit_id") == fit["id"]
        and decision.get("proposal_digest") == fit["record_digest"]
        and decision.get("component_id") == fit["component_id"]
        and decision.get("candidate_parameters") == fit["result"].get("candidate_parameters")
        and decision.get("decision") == fit["status"]
        and decision.get("reviewer") == fit.get("reviewer")
        and decision.get("reason") == fit.get("reason")
        and decision.get("component_revision_after") == fit.get("applied_revision")
    )


def _optimization_decision_valid(project: ProjectStore, run: dict[str, Any]) -> bool:
    if run["status"] == "proposed":
        return run.get("decision_digest") is None
    digest = run.get("decision_digest")
    if not isinstance(digest, str):
        return False
    try:
        path = ArtifactStore(project).path_for(digest)
        if not path.is_file() or sha256_file(path)[0] != digest:
            return False
        decision = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError, json.JSONDecodeError):
        return False
    review = run["result"].get("review", {})
    return (
        decision.get("receipt_type") == "optimization_review"
        and decision.get("run_id") == run["id"]
        and decision.get("proposal_digest") == run["record_digest"]
        and decision.get("component_id") == run["component_id"]
        and decision.get("best_parameters") == run["result"].get("best_parameters")
        and decision.get("best_total_loss") == run["result"].get("best_total_loss")
        and decision.get("baseline_total_loss") == run["result"].get("baseline_total_loss")
        and decision.get("decision") == run["status"]
        and decision.get("reviewer") == review.get("reviewer")
        and decision.get("reason") == review.get("reason")
        and decision.get("component_revision_after") == review.get("applied_revision")
    )


def _multiview_search_receipt_valid(
    project: ProjectStore,
    run: dict[str, Any],
    candidates: list[dict[str, Any]],
) -> bool:
    digest = run.get("receipt_digest")
    if run["status"] != "COMPLETE":
        return digest is None
    if not isinstance(digest, str):
        return False
    try:
        path = ArtifactStore(project).path_for(digest)
        if not path.is_file() or sha256_file(path)[0] != digest:
            return False
        receipt = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError, json.JSONDecodeError):
        return False
    with project.connection() as connection:
        optimization = connection.execute(
            "SELECT record_digest,evaluations_json,result_json FROM optimization_runs WHERE id=?",
            (run.get("optimization_run_id"),),
        ).fetchone()
        baseline = connection.execute(
            "SELECT artifact_digest FROM scene_assets WHERE id=?",
            (run["baseline_scene_id"],),
        ).fetchone()
        lineage_valid = True
        disposition_valid = optimization is not None
        selected_candidate_id = None
        if optimization is not None:
            evaluations = json.loads(optimization["evaluations_json"])
            result = json.loads(optimization["result_json"])
            selected_candidate_id = next(
                (
                    item.get("diagnostics", {}).get("candidate_id")
                    for item in evaluations
                    if item["index"] == result["best_candidate_index"]
                ),
                None,
            )
        expected_dispositions = []
        planned_references = set(run["locality_plan"].get("reference_ids", []))
        for candidate in candidates:
            if candidate["status"] != "EVALUATED":
                expected_dispositions.append(
                    {
                        "candidate_id": candidate["id"],
                        "scene_id": candidate.get("scene_id"),
                        "disposition": "evaluation_failed",
                        "transition_receipt_digest": None,
                    }
                )
                continue
            render_run = connection.execute(
                "SELECT scene_id,camera_solution_id,outputs_json FROM render_runs WHERE id=?",
                (candidate.get("render_run_id"),),
            ).fetchone()
            comparison_ids = candidate.get("comparison_ids", [])
            if render_run is None or len(comparison_ids) < 2:
                lineage_valid = False
                break
            placeholders = ",".join("?" for _ in comparison_ids)
            comparisons = connection.execute(
                "SELECT id,reference_id,render_digest FROM comparisons WHERE id IN ("
                + placeholders
                + ")",
                comparison_ids,
            ).fetchall()
            outputs = json.loads(render_run["outputs_json"])
            output_digests = {
                output.get("artifact_digest") for output in outputs if isinstance(output, dict)
            }
            lineage_valid = (
                render_run["scene_id"] == candidate.get("scene_id")
                and render_run["camera_solution_id"] == run["camera_solution_id"]
                and len(comparisons) == len(comparison_ids)
                and {item["reference_id"] for item in comparisons} == planned_references
                and all(item["render_digest"] in output_digests for item in comparisons)
            )
            if not lineage_valid:
                break
            if candidate["id"] == selected_candidate_id:
                expected_dispositions.append(
                    {
                        "candidate_id": candidate["id"],
                        "scene_id": candidate["scene_id"],
                        "disposition": "selected_for_transactional_review",
                        "transition_receipt_digest": None,
                    }
                )
            else:
                transition = connection.execute(
                    "SELECT receipt_digest FROM scene_transitions WHERE scene_id=? "
                    "AND to_state='REJECTED' "
                    "AND reviewer='VisionMCP multiview search policy' "
                    "ORDER BY created_at DESC,id DESC LIMIT 1",
                    (candidate["scene_id"],),
                ).fetchone()
                scene = connection.execute(
                    "SELECT state FROM scene_assets WHERE id=?", (candidate["scene_id"],)
                ).fetchone()
                if transition is None or scene is None or scene["state"] != "REJECTED":
                    disposition_valid = False
                    break
                expected_dispositions.append(
                    {
                        "candidate_id": candidate["id"],
                        "scene_id": candidate["scene_id"],
                        "disposition": "rejected_nonselected",
                        "transition_receipt_digest": transition["receipt_digest"],
                    }
                )
    configuration = run["configuration"]
    return (
        receipt.get("schema_version") == 1
        and receipt.get("receipt_type") == "multiview_parameter_search"
        and receipt.get("id") == run["id"]
        and receipt.get("component_id") == run["component_id"]
        and receipt.get("component_snapshot_sha256")
        == configuration.get("component_snapshot_sha256")
        and receipt.get("camera_solution_id") == run["camera_solution_id"]
        and receipt.get("camera_snapshot_sha256") == configuration.get("camera_snapshot_sha256")
        and receipt.get("baseline_scene_id") == run["baseline_scene_id"]
        and receipt.get("baseline_scene_digest") == configuration.get("baseline_scene_digest")
        and baseline is not None
        and receipt.get("baseline_scene_digest") == baseline["artifact_digest"]
        and receipt.get("locality_plan_digest") == configuration.get("locality_plan_digest")
        and receipt.get("candidate_count") == len(candidates)
        and receipt.get("evaluated_candidate_count")
        == sum(item["status"] == "EVALUATED" for item in candidates)
        and receipt.get("candidates") == candidates
        and receipt.get("scene_dispositions") == expected_dispositions
        and receipt.get("optimization_run_id") == run.get("optimization_run_id")
        and optimization is not None
        and receipt.get("optimization_proposal_digest") == optimization["record_digest"]
        and receipt.get("acceptance_performed") is False
        and configuration.get("acceptance_performed") is False
        and lineage_valid
        and disposition_valid
    )


def _evidence_pursuit_receipt_valid(project: ProjectStore, run: dict[str, Any]) -> bool:
    digest = run.get("report_digest")
    if run["status"] == "RUNNING":
        return digest is None
    if not isinstance(digest, str):
        return False
    try:
        path = ArtifactStore(project).path_for(digest)
        if not path.is_file() or sha256_file(path)[0] != digest:
            return False
        report = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError, json.JSONDecodeError):
        return False
    capture_ids = run["capture_request_ids"]
    with project.connection() as connection:
        captures = []
        if capture_ids:
            placeholders = ",".join("?" for _ in capture_ids)
            rows = connection.execute(
                "SELECT id,request_json FROM capture_requests WHERE id IN (" + placeholders + ")",
                capture_ids,
            ).fetchall()
            by_id = {row["id"]: json.loads(row["request_json"]) for row in rows}
            captures = [by_id.get(item) for item in capture_ids]
        discovery = None
        if run.get("discovery_run_id"):
            discovery = connection.execute(
                "SELECT artifact_digest,results_json FROM search_discovery_runs WHERE id=?",
                (run["discovery_run_id"],),
            ).fetchone()
    discovery_results = json.loads(discovery["results_json"]) if discovery else None
    registered_source_ids = (
        discovery_results.get("registered_source_ids", []) if discovery_results else []
    )
    status_consistent = (
        run["status"] == "COVERAGE_COMPLETE"
        and not run["focus_terms"]
        and not capture_ids
        or run["status"] == "SOURCES_DISCOVERED"
        and bool(registered_source_ids)
        and not capture_ids
        or run["status"] == "EVIDENCE_CEILING"
        and bool(run["focus_terms"])
        and len(capture_ids) == len(run["focus_terms"])
    )
    try:
        target_valid = TargetResolver(project).authority_status(run["target_id"])["valid"]
        provider_valid = (
            SearchProviderStore(project).authority_status(run["provider_id"])["valid"]
            if run.get("provider_id")
            else True
        )
        discovery_valid = (
            SearchProviderStore(project).discovery_status(run["discovery_run_id"])["valid"]
            if run.get("discovery_run_id")
            else discovery is None
        )
    except (KeyError, TypeError, ValueError):
        target_valid = provider_valid = discovery_valid = False
    return bool(
        report.get("schema_version") == 1
        and report.get("receipt_type") == "missing_evidence_pursuit"
        and report.get("id") == run["id"]
        and report.get("cache_key") == run["cache_key"]
        and report.get("target_id") == run["target_id"]
        and report.get("provider_id") == run.get("provider_id")
        and report.get("status") == run["status"]
        and report.get("focus_terms") == run["focus_terms"]
        and report.get("coverage_before", {}).get("stable_summary") == run["coverage"]
        and report.get("discovery_run_id") == run.get("discovery_run_id")
        and report.get("discovery_receipt_digest")
        == (discovery["artifact_digest"] if discovery else None)
        and report.get("registered_source_ids") == registered_source_ids
        and None not in captures
        and report.get("capture_requests") == captures
        and report.get("policy", {}).get("source_rights_auto_approved") is False
        and report.get("policy", {}).get("source_download_performed") is False
        and report.get("policy", {}).get("acceptance_performed") is False
        and status_consistent
        and target_valid
        and provider_valid
        and discovery_valid
    )


def _records(project: ProjectStore) -> dict[str, Any]:
    with project.connection() as connection:
        references = [
            dict(row)
            for row in connection.execute(
                "SELECT id, artifact_digest, original_name, media_type, rights_state, "
                "viewpoint_label, evidence_role, acceptance_eligible "
                "FROM reference_items "
                "ORDER BY created_at"
            )
        ]
        reference_masks = [
            dict(row)
            for row in connection.execute("SELECT * FROM reference_masks ORDER BY created_at,id")
        ]
        reference_mask_proposal_rows = [
            dict(row)
            for row in connection.execute(
                "SELECT * FROM reference_mask_proposals ORDER BY created_at,id"
            )
        ]
        scenes = [
            dict(row)
            for row in connection.execute(
                "SELECT id,artifact_digest,original_name,inventory_json,state,is_authoritative "
                "FROM scene_assets "
                "ORDER BY created_at"
            )
        ]
        render_runs = [
            {
                **dict(row),
                "configuration": json.loads(row["config_json"]),
                "outputs": json.loads(row["outputs_json"]),
            }
            for row in connection.execute("SELECT * FROM render_runs ORDER BY created_at")
        ]
        visual_geometry_rigs = [
            {
                **dict(row),
                "config": json.loads(row["config_json"]),
            }
            for row in connection.execute(
                "SELECT * FROM visual_geometry_rigs ORDER BY created_at,id"
            )
        ]
        visual_geometry_scorecards = [
            {
                **dict(row),
                "scorecard": json.loads(row["scorecard_json"]),
            }
            for row in connection.execute(
                "SELECT * FROM visual_geometry_scorecards ORDER BY created_at,id"
            )
        ]
        manufactured_form_audits = [
            {
                **dict(row),
                "report": json.loads(row["report_json"]),
            }
            for row in connection.execute(
                "SELECT * FROM manufactured_form_audits ORDER BY created_at,id"
            )
        ]
        visual_baseline_freezes = [
            {
                **dict(row),
                "snapshot": json.loads(row["snapshot_json"]),
            }
            for row in connection.execute(
                "SELECT * FROM visual_baseline_freezes ORDER BY created_at,id"
            )
        ]
        semantic_geometry_bindings = [
            {
                **dict(row),
                "record": json.loads(row["record_json"]),
            }
            for row in connection.execute(
                "SELECT * FROM semantic_geometry_bindings ORDER BY scene_id,object_name,id"
            )
        ]
        visual_component_packets = [
            {
                **dict(row),
                "packet": json.loads(row["packet_json"]),
            }
            for row in connection.execute(
                "SELECT * FROM visual_component_packets ORDER BY created_at,id"
            )
        ]
        visual_frequency_scorecards = [
            {
                **dict(row),
                "scorecard": json.loads(row["scorecard_json"]),
            }
            for row in connection.execute(
                "SELECT * FROM visual_frequency_scorecards ORDER BY created_at,id"
            )
        ]
        visual_geometry_store = VisualGeometryStore(project)
        for rig in visual_geometry_rigs:
            rig["receipt_valid"] = visual_geometry_store.verify_rig(rig["id"])["valid"]
        for scorecard in visual_geometry_scorecards:
            verification = visual_geometry_store.verify_scorecard(scorecard["id"], replay=True)
            scorecard["receipt_valid"] = verification["receipt_valid"]
            scorecard["replay_valid"] = verification["replay_valid"]
            scorecard["valid"] = verification["valid"]
        manufactured_auditor = ManufacturedFormAuditor(project)
        for audit in manufactured_form_audits:
            verification = manufactured_auditor.verify(audit["id"])
            audit["receipt_valid"] = verification["receipt_valid"]
            audit["replay_valid"] = verification["replay_valid"]
            audit["valid"] = verification["valid"]
        baseline_store = VisualBaselineStore(project)
        for baseline in visual_baseline_freezes:
            baseline["valid"] = baseline_store.verify(baseline["id"])["valid"]
        binding_store = SemanticBindingStore(project)
        for binding in semantic_geometry_bindings:
            binding["valid"] = binding_store.verify(binding["id"])["valid"]
        packet_store = ComponentTaskPacketStore(project)
        for packet in visual_component_packets:
            packet["valid"] = packet_store.verify(packet["id"])["valid"]
        frequency_store = VisualFrequencyScoreStore(project)
        for scorecard in visual_frequency_scorecards:
            scorecard["valid"] = frequency_store.verify(scorecard["id"])["valid"]
        authoritative_scene_row = next(
            (scene for scene in scenes if scene.get("is_authoritative")), None
        )
        semantic_binding_coverage = (
            binding_store.coverage(authoritative_scene_row["id"])
            if authoritative_scene_row is not None
            else None
        )
        assembly_graph_audit = (
            binding_store.assembly_audit(authoritative_scene_row["id"])
            if authoritative_scene_row is not None
            else None
        )
        exports = [
            {
                **dict(row),
                "configuration": json.loads(row["config_json"]),
                "worker": json.loads(row["worker_json"]),
            }
            for row in connection.execute("SELECT * FROM exports ORDER BY created_at")
        ]
        calibration_runs = [
            {
                **dict(row),
                "gates": json.loads(row["gates_json"]),
            }
            for row in connection.execute("SELECT * FROM calibration_runs ORDER BY created_at")
        ]
        job_provenance = [
            {
                "job_id": row["job_id"],
                "operation": row["operation"],
                "status": row["status"],
                "input_hashes": json.loads(row["input_hashes_json"]),
                "backend": json.loads(row["backend_json"]),
                "execution": json.loads(row["execution_json"]),
                "output_hashes": json.loads(row["output_hashes_json"]),
                "metrics": json.loads(row["metrics_json"]),
                "logs": json.loads(row["logs_json"]),
                "failure_class": row["failure_class"],
                "updated_at": row["updated_at"],
            }
            for row in connection.execute(
                "SELECT p.*,j.operation,j.status FROM job_provenance p "
                "JOIN jobs j ON j.id=p.job_id ORDER BY j.created_at"
            )
        ]
        cameras = [
            {
                **dict(row),
                "solution": json.loads(row["solution_json"]),
                "diagnostics": json.loads(row["diagnostics_json"]),
            }
            for row in connection.execute("SELECT * FROM camera_solutions ORDER BY created_at,id")
        ]
        camera_landmark_rows = [
            row[0]
            for row in connection.execute(
                "SELECT id FROM camera_landmark_proposals ORDER BY created_at,id"
            )
        ]
        reference_adoption_rows = [
            row[0]
            for row in connection.execute(
                "SELECT id FROM reference_adoption_proposals ORDER BY created_at,id"
            )
        ]
        camera_refinements = [
            {
                **dict(row),
                "configuration": json.loads(row["config_json"]),
            }
            for row in connection.execute(
                "SELECT * FROM camera_refinement_runs ORDER BY created_at,id"
            )
        ]
        comparisons = [
            {**dict(row), "metrics": json.loads(row["metrics_json"])}
            for row in connection.execute("SELECT * FROM comparisons ORDER BY created_at")
        ]
        coverage_row = connection.execute(
            "SELECT report_json, digest FROM coverage_reports ORDER BY created_at DESC LIMIT 1"
        ).fetchone()
        measurements = [
            {
                **dict(row),
                "value": json.loads(row["value_json"]),
                "uncertainty": json.loads(row["uncertainty_json"]),
            }
            for row in connection.execute("SELECT * FROM measurements ORDER BY created_at")
        ]
        measurement_grids = [
            json.loads(row["record_json"])
            for row in connection.execute(
                "SELECT record_json FROM measurement_grids ORDER BY created_at"
            )
        ]
        capture_requests = [
            json.loads(row["request_json"])
            for row in connection.execute(
                "SELECT request_json FROM capture_requests ORDER BY created_at"
            )
        ]
        tier_reviews = [
            json.loads(row["decision_json"])
            for row in connection.execute(
                "SELECT decision_json FROM tier_reviews ORDER BY created_at"
            )
        ]
        features = [
            json.loads(row["record_json"])
            for row in connection.execute("SELECT record_json FROM features ORDER BY created_at")
        ]
        components = [
            {**json.loads(row["record_json"]), "revision": row["revision"]}
            for row in connection.execute(
                "SELECT record_json,revision FROM components ORDER BY created_at"
            )
        ]
        repairs = [
            {
                **dict(row),
                "config": json.loads(row["config_json"]),
                "evidence": json.loads(row["evidence_json"]),
                "expected": json.loads(row["expected_json"]),
                "result": json.loads(row["result_json"]) if row["result_json"] else None,
            }
            for row in connection.execute("SELECT * FROM repair_proposals ORDER BY created_at")
        ]
        geometry_runs = [
            {
                **dict(row),
                "configuration": json.loads(row["config_json"]),
                "evidence": json.loads(row["evidence_json"]),
                "commercial_eligible": bool(row["commercial_eligible"]),
            }
            for row in connection.execute("SELECT * FROM geometry_runs ORDER BY created_at")
        ]
        geometry_consensus = [
            {
                **dict(row),
                "run_ids": json.loads(row["run_ids_json"]),
                "report": json.loads(row["report_json"]),
            }
            for row in connection.execute("SELECT * FROM geometry_consensus ORDER BY created_at")
        ]
        camera_consensus = [
            {
                **dict(row),
                "solution_ids": json.loads(row["solution_ids_json"]),
                "report": json.loads(row["report_json"]),
            }
            for row in connection.execute("SELECT * FROM camera_consensus ORDER BY created_at")
        ]
        component_fits = [
            {
                **dict(row),
                "inputs": json.loads(row["input_json"]),
                "result": json.loads(row["result_json"]),
            }
            for row in connection.execute("SELECT * FROM component_fits ORDER BY created_at")
        ]
        for fit in component_fits:
            fit["decision_receipt_valid"] = _component_fit_decision_valid(project, fit)
        datasets = [
            {**dict(row), "manifest": json.loads(row["manifest_json"])}
            for row in connection.execute("SELECT * FROM datasets ORDER BY created_at")
        ]
        training_runs = [
            {
                **dict(row),
                "configuration": json.loads(row["config_json"]),
                "result": json.loads(row["result_json"]) if row["result_json"] else None,
            }
            for row in connection.execute("SELECT * FROM training_runs ORDER BY created_at")
        ]
        model_evaluations = [
            {**dict(row), "metrics": json.loads(row["metrics_json"])}
            for row in connection.execute("SELECT * FROM model_evaluations ORDER BY created_at")
        ]
        visual_oracles = [
            {
                **dict(row),
                "configuration": json.loads(row["config_json"]),
                "license": json.loads(row["license_json"]),
                "commercial_eligible": bool(row["commercial_eligible"]),
            }
            for row in connection.execute("SELECT * FROM visual_oracles ORDER BY created_at")
        ]
        optimization_runs = [
            {
                **dict(row),
                "configuration": json.loads(row["config_json"]),
                "evaluations": json.loads(row["evaluations_json"]),
                "result": json.loads(row["result_json"]),
            }
            for row in connection.execute("SELECT * FROM optimization_runs ORDER BY created_at")
        ]
        for run in optimization_runs:
            run["decision_receipt_valid"] = _optimization_decision_valid(project, run)
        multiview_search_candidates = [
            {
                **dict(row),
                "parameters": json.loads(row["parameters_json"]),
                "comparison_ids": json.loads(row["comparison_ids_json"]),
                "errors": json.loads(row["error_json"]) if row["error_json"] else [],
            }
            for row in connection.execute(
                "SELECT * FROM multiview_search_candidates ORDER BY search_id,candidate_index,id"
            )
        ]
        candidates_by_search: dict[str, list[dict[str, Any]]] = {}
        for candidate in multiview_search_candidates:
            candidate.pop("parameters_json", None)
            candidate.pop("comparison_ids_json", None)
            candidate.pop("error_json", None)
            candidates_by_search.setdefault(candidate["search_id"], []).append(candidate)
        multiview_search_runs = [
            {
                **dict(row),
                "semantic_ids": json.loads(row["semantic_ids_json"]),
                "locality_plan": json.loads(row["locality_plan_json"]),
                "configuration": json.loads(row["config_json"]),
            }
            for row in connection.execute(
                "SELECT * FROM multiview_search_runs ORDER BY created_at,id"
            )
        ]
        for run in multiview_search_runs:
            run.pop("semantic_ids_json", None)
            run.pop("locality_plan_json", None)
            run.pop("config_json", None)
            run["candidates"] = candidates_by_search.get(run["id"], [])
            run["receipt_valid"] = _multiview_search_receipt_valid(project, run, run["candidates"])
        model_approvals = [
            {**dict(row), "license": json.loads(row["license_json"])}
            for row in connection.execute("SELECT * FROM model_approvals ORDER BY created_at")
        ]
        model_installations = [
            dict(row)
            for row in connection.execute("SELECT * FROM model_installations ORDER BY installed_at")
        ]
        material_profiles = [
            json.loads(row["record_json"])
            for row in connection.execute(
                "SELECT record_json FROM material_profiles ORDER BY created_at"
            )
        ]
        target_resolutions = [
            {
                **dict(row),
                "target": json.loads(row["target_json"]),
                "alternatives": json.loads(row["alternatives_json"]),
                "ambiguity": json.loads(row["ambiguity_json"]),
            }
            for row in connection.execute("SELECT * FROM target_resolutions ORDER BY rowid")
        ]
        target_authority = TargetResolver(project)
        for resolution in target_resolutions:
            resolution["authority"] = target_authority.authority_status(resolution["id"])
        evidence_sources = [
            {
                **dict(row),
                "source": json.loads(row["source_json"]),
                "rights": json.loads(row["rights_json"]),
            }
            for row in connection.execute(
                "SELECT s.*,r.rights_json,r.reviewed_by,r.reviewed_at "
                "FROM evidence_sources s JOIN rights_ledger r ON r.source_id=s.id "
                "ORDER BY s.created_at,s.id"
            )
        ]
        source_authority = EvidenceAcquisitionStore(project)
        for source in evidence_sources:
            source["authority"] = source_authority.authority_status(source["id"])
        search_providers = [
            {
                **dict(row),
                "configuration": json.loads(row["config_json"]),
            }
            for row in connection.execute("SELECT * FROM search_providers ORDER BY created_at,id")
        ]
        provider_authority = SearchProviderStore(project)
        for provider in search_providers:
            provider["authority"] = provider_authority.authority_status(provider["id"])
        search_discovery_runs = [
            {
                **dict(row),
                "plan": json.loads(row["plan_json"]),
                "results": json.loads(row["results_json"]),
            }
            for row in connection.execute(
                "SELECT * FROM search_discovery_runs ORDER BY created_at,id"
            )
        ]
        for run in search_discovery_runs:
            run["receipt_valid"] = provider_authority.discovery_status(run["id"])["valid"]
        evidence_pursuit_runs = [
            {
                **dict(row),
                "focus_terms": json.loads(row["focus_terms_json"]),
                "coverage": json.loads(row["coverage_json"]),
                "capture_request_ids": json.loads(row["capture_request_ids_json"]),
            }
            for row in connection.execute(
                "SELECT * FROM evidence_pursuit_runs ORDER BY created_at,id"
            )
        ]
        for run in evidence_pursuit_runs:
            run.pop("focus_terms_json", None)
            run.pop("coverage_json", None)
            run.pop("capture_request_ids_json", None)
            run["receipt_valid"] = _evidence_pursuit_receipt_valid(project, run)
        evidence_conflict_runs = [
            {
                **dict(row),
                "report": json.loads(row["report_json"]),
            }
            for row in connection.execute(
                "SELECT * FROM evidence_conflict_runs ORDER BY created_at,id"
            )
        ]
        evidence_conflict_reviews = [
            {
                **dict(row),
                "configuration_model": json.loads(row["configuration_model_json"]),
            }
            for row in connection.execute(
                "SELECT * FROM evidence_conflict_reviews ORDER BY created_at,id"
            )
        ]
        evidence_duplicate_runs = [
            {
                **dict(row),
                "report": json.loads(row["report_json"]),
            }
            for row in connection.execute(
                "SELECT * FROM evidence_duplicate_runs ORDER BY created_at,id"
            )
        ]
        scene_transitions = [
            dict(row)
            for row in connection.execute("SELECT * FROM scene_transitions ORDER BY created_at,id")
        ]
        candidate_evaluations = [
            {
                **dict(row),
                "gates": json.loads(row["gates_json"]),
                "metrics": json.loads(row["metrics_json"]),
                "regressions": json.loads(row["regressions_json"]),
            }
            for row in connection.execute(
                "SELECT * FROM candidate_evaluations ORDER BY created_at,id"
            )
        ]
        video_analysis_runs = [
            {**dict(row), "report": json.loads(row["report_json"])}
            for row in connection.execute(
                "SELECT * FROM video_analysis_runs ORDER BY created_at,id"
            )
        ]
        feature_tracks = [
            {**dict(row), "observations": json.loads(row["observations_json"])}
            for row in connection.execute("SELECT * FROM feature_tracks ORDER BY created_at,id")
        ]
        semantic_nodes = [
            json.loads(row["record_json"])
            for row in connection.execute(
                "SELECT record_json FROM semantic_nodes ORDER BY created_at,id"
            )
        ]
        semantic_edges = [
            json.loads(row["record_json"])
            for row in connection.execute(
                "SELECT record_json FROM semantic_edges ORDER BY created_at,id"
            )
        ]
        reconstruction_portfolios = [
            {**dict(row), "configuration": json.loads(row["configuration_json"])}
            for row in connection.execute(
                "SELECT * FROM reconstruction_portfolios ORDER BY created_at,id"
            )
        ]
        reconstruction_candidates = [
            json.loads(row["record_json"])
            for row in connection.execute(
                "SELECT record_json FROM reconstruction_candidates ORDER BY created_at,id"
            )
        ]
        campaigns = [
            {
                **dict(row),
                "configuration": json.loads(row["config_json"]),
                "budget": json.loads(row["budget_json"]),
                "result": json.loads(row["result_json"]) if row["result_json"] else None,
            }
            for row in connection.execute("SELECT * FROM campaigns ORDER BY created_at,id")
        ]
        agent_proposals = [
            json.loads(row["record_json"])
            for row in connection.execute(
                "SELECT record_json FROM agent_proposals ORDER BY created_at,id"
            )
        ]
        context_packets = [
            json.loads(row["packet_json"])
            for row in connection.execute(
                "SELECT packet_json FROM context_packets ORDER BY created_at,id"
            )
        ]
        synthetic_views = [
            json.loads(row["record_json"])
            for row in connection.execute(
                "SELECT record_json FROM synthetic_views ORDER BY created_at,id"
            )
        ]
        surface_coverage_cells = [
            json.loads(row["record_json"])
            for row in connection.execute(
                "SELECT record_json FROM surface_coverage_cells ORDER BY region,id"
            )
        ]
        warm_services = [
            json.loads(row["record_json"])
            for row in connection.execute("SELECT record_json FROM warm_services ORDER BY name")
        ]
        beast_benchmark_runs = [
            json.loads(row["report_json"])
            for row in connection.execute(
                "SELECT report_json FROM beast_benchmark_runs ORDER BY created_at,id"
            )
        ]
    camera_landmark_proposals = []
    camera_decision_store = CameraDecisionStore(project)
    for camera in cameras:
        decision_verification = camera_decision_store.verify_record(camera)
        camera["decision_receipt_valid"] = decision_verification["valid"]
        camera["decision_receipt"] = decision_verification["decision"]
    landmark_store = CameraLandmarkStore(project)
    for proposal_id in camera_landmark_rows:
        try:
            proposal = landmark_store.get(proposal_id, verify=True)
            proposal["receipt_valid"] = True
            proposal["verification_error"] = None
        except (KeyError, OSError, ValueError) as error:
            proposal = landmark_store.get(proposal_id, verify=False)
            proposal["receipt_valid"] = False
            proposal["verification_error"] = str(error)
        camera_landmark_proposals.append(proposal)
    reference_adoption_proposals = []
    adoption_store = LegacyReferenceAdoptionStore(project)
    for proposal_id in reference_adoption_rows:
        try:
            proposal = adoption_store.get(proposal_id, verify=True)
            proposal["receipt_valid"] = True
            proposal["verification_error"] = None
        except (AttributeError, KeyError, OSError, TypeError, ValueError) as error:
            proposal = adoption_store.get(proposal_id, verify=False)
            proposal["receipt_valid"] = False
            proposal["verification_error"] = str(error)
        reference_adoption_proposals.append(proposal)
    active_learning_audit = audit_active_learning(project)
    active_learning_cycles = active_learning_audit["cycles"]
    generative_audit = GenerativeProposalStore(project).audit()
    generative_requests = generative_audit["requests"]
    generative_results = generative_audit["results"]
    reference_derivation_audit = ReferenceDerivationStore(project).audit()
    verified_measurements = []
    measurement_store = MeasurementStore(project)
    for measurement in measurements:
        try:
            verified = measurement_store.get(measurement["id"], verify=True)
            verified["provenance_valid"] = bool(verified.get("provenance_digest"))
            verified["provenance_error"] = None
        except (KeyError, OSError, ValueError) as error:
            verified = measurement_store.get(measurement["id"], verify=False)
            verified["provenance_valid"] = False
            verified["provenance_error"] = str(error)
        verified_measurements.append(verified)
    measurements = verified_measurements
    for scene in scenes:
        scene["inventory"] = (
            json.loads(scene.pop("inventory_json")) if scene["inventory_json"] else None
        )
    for mask in reference_masks:
        mask["visible_components"] = json.loads(mask.pop("visible_components_json"))
        mask["excluded_components"] = json.loads(mask.pop("excluded_components_json"))
        mask["roi"] = json.loads(mask.pop("roi_json"))
        mask["decision_valid"] = ReferenceMaskStore(project).verify_approved_mask(mask)
    mask_store = ReferenceMaskStore(project)
    reference_mask_proposals = []
    for proposal in reference_mask_proposal_rows:
        proposal["proposal"] = json.loads(proposal.pop("record_json"))
        proposal["proposal_valid"] = mask_store.verify_proposal(proposal["id"])["valid"]
        proposal["decision_valid"] = (
            None
            if proposal["status"] == "PROPOSED"
            else mask_store.verify_decision(proposal["id"])["valid"]
        )
        reference_mask_proposals.append(proposal)
    render_scene_ids_by_digest: dict[str, set[str]] = {}
    render_camera_solution_ids_by_digest: dict[str, set[str]] = {}
    for run in render_runs:
        for output in run["outputs"]:
            digests = {output.get("artifact_digest")}
            pass_digests = output.get("pass_artifact_digests", {})
            if isinstance(pass_digests, dict):
                digests.update(pass_digests.values())
            for digest in digests:
                if digest:
                    render_scene_ids_by_digest.setdefault(str(digest), set()).add(
                        str(run["scene_id"])
                    )
                    render_camera_solution_ids_by_digest.setdefault(str(digest), set()).add(
                        str(run["camera_solution_id"])
                    )
    for comparison in comparisons:
        comparison["render_scene_ids"] = sorted(
            render_scene_ids_by_digest.get(str(comparison.get("render_digest")), set())
        )
        comparison["render_camera_solution_ids"] = sorted(
            render_camera_solution_ids_by_digest.get(str(comparison.get("render_digest")), set())
        )
    comparison_store = ComparisonStore(project)
    comparison_verifications = {
        comparison["id"]: comparison_store.verify_record(comparison, replay=True)
        for comparison in comparisons
    }
    for comparison in comparisons:
        verification = comparison_verifications[comparison["id"]]
        supersession = comparison_store.verify_supersession(
            comparison,
            source_verification=verification,
            replacement_verification=comparison_verifications.get(
                comparison.get("superseded_by_id")
            ),
        )
        comparison["receipt_valid"] = verification["receipt_valid"]
        comparison["replay_valid"] = verification["replay_valid"]
        comparison["comparison_valid"] = verification["valid"]
        comparison["superseded"] = supersession["superseded"]
        comparison["supersession_valid"] = supersession["valid"]
    for run in render_runs:
        run.pop("config_json", None)
        run.pop("outputs_json", None)
    for item in exports:
        item.pop("config_json", None)
        item.pop("worker_json", None)
    for run in calibration_runs:
        run.pop("gates_json", None)
    for camera in cameras:
        camera.pop("solution_json", None)
        camera.pop("diagnostics_json", None)
    for refinement in camera_refinements:
        refinement.pop("config_json", None)
    for comparison in comparisons:
        comparison.pop("metrics_json", None)
    for measurement in measurements:
        measurement.pop("value_json", None)
        measurement.pop("uncertainty_json", None)
        measurement["coordinate_frame"] = measurement["value"].get(
            "coordinate_frame", "canonical_mm"
        )
        measurement["certainty"] = measurement["value"].get(
            "certainty", measurement["value"].get("qualifier", "estimated")
        )
        measurement["reference_ids"] = measurement["value"].get("reference_ids", [])
        measurement["record_sha256"] = hashlib.sha256(
            canonical_json(
                {
                    "id": measurement["id"],
                    "type": measurement["type"],
                    "value": measurement["value"],
                    "evidence_class": measurement["evidence_class"],
                    "uncertainty": measurement["uncertainty"],
                }
            )
        ).hexdigest()
    for repair in repairs:
        repair.pop("config_json", None)
        repair.pop("evidence_json", None)
        repair.pop("expected_json", None)
        repair.pop("result_json", None)
    for run in geometry_runs:
        run.pop("config_json", None)
        run.pop("evidence_json", None)
        run["license"] = run["evidence"].pop("license", {})
    for consensus in geometry_consensus:
        consensus.pop("run_ids_json", None)
        consensus.pop("report_json", None)
    for consensus in camera_consensus:
        consensus.pop("solution_ids_json", None)
        consensus.pop("report_json", None)
    for fit in component_fits:
        fit.pop("input_json", None)
        fit.pop("result_json", None)
    for dataset in datasets:
        dataset.pop("manifest_json", None)
    for run in training_runs:
        run.pop("config_json", None)
        run.pop("result_json", None)
    for evaluation in model_evaluations:
        evaluation.pop("metrics_json", None)
    for oracle in visual_oracles:
        oracle.pop("config_json", None)
        oracle.pop("license_json", None)
    for run in optimization_runs:
        run.pop("config_json", None)
        run.pop("evaluations_json", None)
        run.pop("result_json", None)
    for approval in model_approvals:
        approval.pop("license_json", None)
    for target_resolution in target_resolutions:
        target_resolution.pop("target_json", None)
        target_resolution.pop("alternatives_json", None)
        target_resolution.pop("ambiguity_json", None)
    for source in evidence_sources:
        source.pop("source_json", None)
        source.pop("rights_json", None)
    for evaluation in candidate_evaluations:
        evaluation.pop("gates_json", None)
        evaluation.pop("metrics_json", None)
        evaluation.pop("regressions_json", None)
    for run in video_analysis_runs:
        run.pop("report_json", None)
    for track in feature_tracks:
        track.pop("observations_json", None)
    for portfolio in reconstruction_portfolios:
        portfolio.pop("configuration_json", None)
    for campaign in campaigns:
        campaign.pop("config_json", None)
        campaign.pop("budget_json", None)
        campaign.pop("result_json", None)
    coverage = (
        {"artifact_digest": coverage_row["digest"], **json.loads(coverage_row["report_json"])}
        if coverage_row
        else None
    )
    return {
        "references": references,
        "reference_masks": reference_masks,
        "reference_mask_proposals": reference_mask_proposals,
        "scenes": scenes,
        "render_runs": render_runs,
        "visual_geometry_rigs": visual_geometry_rigs,
        "visual_geometry_scorecards": visual_geometry_scorecards,
        "manufactured_form_audits": manufactured_form_audits,
        "visual_baseline_freezes": visual_baseline_freezes,
        "semantic_geometry_bindings": semantic_geometry_bindings,
        "semantic_binding_coverage": semantic_binding_coverage,
        "assembly_graph_audit": assembly_graph_audit,
        "visual_component_packets": visual_component_packets,
        "visual_frequency_scorecards": visual_frequency_scorecards,
        "exports": exports,
        "calibration_runs": calibration_runs,
        "job_provenance": job_provenance,
        "camera_solutions": cameras,
        "camera_landmark_proposals": camera_landmark_proposals,
        "reference_adoption_proposals": reference_adoption_proposals,
        "reference_derivation_audit": reference_derivation_audit,
        "camera_refinement_runs": camera_refinements,
        "comparisons": comparisons,
        "coverage": coverage,
        "measurements": measurements,
        "measurement_grids": measurement_grids,
        "capture_requests": capture_requests,
        "tier_reviews": tier_reviews,
        "features": features,
        "components": components,
        "repairs": repairs,
        "geometry_runs": geometry_runs,
        "geometry_consensus": geometry_consensus,
        "camera_consensus": camera_consensus,
        "component_fits": component_fits,
        "datasets": datasets,
        "training_runs": training_runs,
        "model_evaluations": model_evaluations,
        "visual_oracles": visual_oracles,
        "optimization_runs": optimization_runs,
        "multiview_search_runs": multiview_search_runs,
        "multiview_search_candidates": multiview_search_candidates,
        "model_approvals": model_approvals,
        "model_installations": model_installations,
        "material_profiles": material_profiles,
        "target_resolutions": target_resolutions,
        "evidence_sources": evidence_sources,
        "search_providers": search_providers,
        "search_discovery_runs": search_discovery_runs,
        "evidence_pursuit_runs": evidence_pursuit_runs,
        "evidence_conflict_runs": evidence_conflict_runs,
        "evidence_conflict_reviews": evidence_conflict_reviews,
        "evidence_conflict_audit": (
            EvidenceConflictStore(project).audit(target_resolutions[-1]["id"], record=False)
            if target_resolutions and target_resolutions[-1].get("authority", {}).get("valid")
            else {
                "finding_count": 0,
                "unresolved_blocking_count": 0,
                "unresolved_warning_count": 0,
                "canonical_merge_permitted": True,
                "source_eligibility": {},
            }
        ),
        "evidence_duplicate_runs": evidence_duplicate_runs,
        "evidence_duplicate_audit": (
            EvidenceDuplicateStore(project).audit(target_resolutions[-1]["id"], record=False)
            if target_resolutions and target_resolutions[-1].get("authority", {}).get("valid")
            else {
                "source_count": 0,
                "unique_media_count": 0,
                "duplicate_group_count": 0,
                "duplicate_groups": [],
                "source_eligibility": {},
            }
        ),
        "scene_transitions": scene_transitions,
        "scene_lifecycle_audit": audit_scene_lifecycle(project),
        "candidate_evaluations": candidate_evaluations,
        "video_analysis_runs": video_analysis_runs,
        "feature_tracks": feature_tracks,
        "semantic_nodes": semantic_nodes,
        "semantic_edges": semantic_edges,
        "reconstruction_portfolios": reconstruction_portfolios,
        "reconstruction_candidates": reconstruction_candidates,
        "generative_requests": generative_requests,
        "generative_results": generative_results,
        "generative_audit": {
            "invalid_request_ids": generative_audit["invalid_request_ids"],
            "invalid_result_ids": generative_audit["invalid_result_ids"],
        },
        "campaigns": campaigns,
        "agent_proposals": agent_proposals,
        "context_packets": context_packets,
        "synthetic_views": synthetic_views,
        "active_learning_cycles": active_learning_cycles,
        "active_learning_events": active_learning_audit["events"],
        "active_model_revisions": active_learning_audit["active_model_revisions"],
        "active_learning_audit": {
            "invalid_cycle_ids": active_learning_audit["invalid_cycle_ids"],
            "invalid_model_revision_ids": active_learning_audit["invalid_model_revision_ids"],
        },
        "surface_coverage_cells": surface_coverage_cells,
        "warm_services": warm_services,
        "beast_benchmark_runs": beast_benchmark_runs,
    }


def _acceptance(
    records: dict[str, Any], target: str, project_metadata: dict[str, Any]
) -> dict[str, Any]:
    blockers: list[str] = []
    metrics: dict[str, Any] = {}
    all_comparison_history = records["comparisons"]
    invalid_comparison_ids = sorted(
        comparison["id"]
        for comparison in all_comparison_history
        if not comparison.get("comparison_valid") and not comparison.get("supersession_valid")
    )
    comparison_history = [
        comparison
        for comparison in all_comparison_history
        if comparison.get("comparison_valid") and not comparison.get("supersession_valid")
    ]
    if invalid_comparison_ids:
        blockers.append("one or more residual comparisons lack a valid replayable receipt")
    scenes = records["scenes"]
    authoritative_scene = next((scene for scene in scenes if scene.get("is_authoritative")), None)
    authoritative_scene_id = authoritative_scene["id"] if authoritative_scene else None
    eligible_comparison_history = [
        comparison
        for comparison in comparison_history
        if authoritative_scene_id is None
        or authoritative_scene_id in comparison.get("render_scene_ids", [])
    ]
    latest_by_reference: dict[str, dict[str, Any]] = {}
    for comparison in eligible_comparison_history:
        reference_id = comparison["reference_id"]
        previous = latest_by_reference.get(reference_id)
        ordering = (comparison.get("created_at", ""), comparison.get("id", ""))
        previous_ordering = (
            (previous.get("created_at", ""), previous.get("id", "")) if previous else None
        )
        if previous_ordering is None or ordering > previous_ordering:
            latest_by_reference[reference_id] = comparison
    comparisons = sorted(
        latest_by_reference.values(), key=lambda item: (item["reference_id"], item["id"])
    )
    cameras = records["camera_solutions"]
    references = records["references"]
    reference_masks = records["reference_masks"]
    reference_mask_proposals = records["reference_mask_proposals"]
    renderable_references = [
        reference
        for reference in references
        if str(reference.get("media_type", "")).startswith("image/")
        and bool(reference.get("acceptance_eligible", True))
    ]
    measurements = records["measurements"]
    features = [
        feature
        for feature in records["features"]
        if feature.get("lifecycle_state", "active") == "active"
    ]
    repairs = records["repairs"]
    geometry_runs = records["geometry_runs"]
    camera_consensus = records["camera_consensus"]
    camera_landmark_proposals = records["camera_landmark_proposals"]
    component_fits = records["component_fits"]
    datasets = records["datasets"]
    training_runs = records["training_runs"]
    model_evaluations = records["model_evaluations"]
    visual_oracles = records["visual_oracles"]
    optimization_runs = records["optimization_runs"]
    multiview_search_runs = records["multiview_search_runs"]
    model_approvals = records["model_approvals"]
    model_installations = records["model_installations"]
    material_profiles = records["material_profiles"]
    exports = records["exports"]
    calibration_runs = records["calibration_runs"]
    target_resolutions = records["target_resolutions"]
    evidence_sources = records["evidence_sources"]
    reference_adoption_proposals = records["reference_adoption_proposals"]
    candidate_evaluations = records["candidate_evaluations"]
    semantic_nodes = records["semantic_nodes"]
    generative_results = records["generative_results"]
    generative_audit = records["generative_audit"]
    campaigns = records["campaigns"]
    synthetic_views = records["synthetic_views"]
    active_learning_cycles = records["active_learning_cycles"]
    active_learning_audit = records["active_learning_audit"]
    surface_coverage_cells = records["surface_coverage_cells"]
    l3_plus = target in {FidelityLevel.L3.value, FidelityLevel.L4.value, FidelityLevel.L5.value}
    benchmark_name = project_metadata.get("metadata", {}).get("benchmark")
    visual_geometry_required = bool(
        l3_plus
        and (
            benchmark_name not in {None, "", "calibration"}
            or project_metadata.get("metadata", {}).get(
                "visual_geometry_acceptance_required", False
            )
        )
    )
    visual_geometry_rigs = records["visual_geometry_rigs"]
    visual_geometry_scorecards = records["visual_geometry_scorecards"]
    manufactured_form_audits = records["manufactured_form_audits"]
    visual_baseline_freezes = records["visual_baseline_freezes"]
    semantic_geometry_bindings = records["semantic_geometry_bindings"]
    visual_component_packets = records["visual_component_packets"]
    visual_frequency_scorecards = records["visual_frequency_scorecards"]
    invalid_rig_ids = sorted(
        item["id"] for item in visual_geometry_rigs if not item.get("receipt_valid")
    )
    invalid_scorecard_ids = sorted(
        item["id"] for item in visual_geometry_scorecards if not item.get("valid")
    )
    invalid_manufactured_audit_ids = sorted(
        item["id"] for item in manufactured_form_audits if not item.get("valid")
    )
    authoritative_visual_rigs = [
        item
        for item in visual_geometry_rigs
        if item.get("receipt_valid")
        and item.get("state") == "AUTHORITATIVE"
        and (authoritative_scene_id is None or item.get("scene_id") == authoritative_scene_id)
    ]
    authoritative_rig_ids = {item["id"] for item in authoritative_visual_rigs}
    eligible_visual_scorecards = [
        item
        for item in visual_geometry_scorecards
        if item.get("valid")
        and item.get("rig_id") in authoritative_rig_ids
        and item.get("scorecard", {}).get("inputs", {}).get("mask_authority") == "REVIEWED_MASK"
        and (authoritative_scene_id is None or item.get("scene_id") == authoritative_scene_id)
    ]
    latest_visual_by_reference: dict[str, dict[str, Any]] = {}
    for item in eligible_visual_scorecards:
        reference_id = str(item["reference_id"])
        previous = latest_visual_by_reference.get(reference_id)
        if previous is None or (item.get("created_at", ""), item["id"]) > (
            previous.get("created_at", ""),
            previous["id"],
        ):
            latest_visual_by_reference[reference_id] = item
    scene_manufactured_audits = [
        item
        for item in manufactured_form_audits
        if item.get("valid")
        and (authoritative_scene_id is None or item.get("scene_id") == authoritative_scene_id)
    ]
    latest_manufactured_audit = (
        max(scene_manufactured_audits, key=lambda item: (item["created_at"], item["id"]))
        if scene_manufactured_audits
        else None
    )
    valid_baselines = [item for item in visual_baseline_freezes if item.get("valid")]
    authoritative_baselines = [
        item
        for item in valid_baselines
        if authoritative_scene_id in item.get("snapshot", {}).get("scene_ids", [])
    ]
    invalid_baseline_ids = sorted(
        item["id"] for item in visual_baseline_freezes if not item.get("valid")
    )
    scene_bindings = [
        item
        for item in semantic_geometry_bindings
        if authoritative_scene_id is None or item.get("scene_id") == authoritative_scene_id
    ]
    invalid_binding_ids = sorted(item["id"] for item in scene_bindings if not item.get("valid"))
    semantic_binding_coverage = records["semantic_binding_coverage"]
    assembly_graph_audit = records["assembly_graph_audit"]
    invalid_component_packet_ids = sorted(
        item["id"] for item in visual_component_packets if not item.get("valid")
    )
    invalid_frequency_scorecard_ids = sorted(
        item["id"] for item in visual_frequency_scorecards if not item.get("valid")
    )
    eligible_frequency_scorecards = [
        item
        for item in visual_frequency_scorecards
        if item.get("valid")
        and item.get("scene_id") == authoritative_scene_id
        and item.get("rig_id") in authoritative_rig_ids
    ]
    latest_frequency_scorecard = (
        max(
            eligible_frequency_scorecards,
            key=lambda item: (item["created_at"], item["id"]),
        )
        if eligible_frequency_scorecards
        else None
    )
    metrics["visual_geometry"] = {
        "policy_required": visual_geometry_required,
        "benchmark": benchmark_name,
        "rig_count": len(visual_geometry_rigs),
        "authoritative_rig_ids": sorted(authoritative_rig_ids),
        "invalid_rig_ids": invalid_rig_ids,
        "scorecard_count": len(visual_geometry_scorecards),
        "active_authoritative_scorecard_ids": sorted(
            item["id"] for item in latest_visual_by_reference.values()
        ),
        "active_status_by_reference": {
            reference_id: item["status"]
            for reference_id, item in sorted(latest_visual_by_reference.items())
        },
        "diagnostic_scorecard_ids": sorted(
            item["id"]
            for item in visual_geometry_scorecards
            if item.get("scorecard", {}).get("inputs", {}).get("mask_authority") != "REVIEWED_MASK"
            or item.get("rig_id") not in authoritative_rig_ids
        ),
        "invalid_scorecard_ids": invalid_scorecard_ids,
        "latest_manufactured_form_audit_id": (
            latest_manufactured_audit["id"] if latest_manufactured_audit else None
        ),
        "latest_manufactured_form_status": (
            latest_manufactured_audit["status"] if latest_manufactured_audit else None
        ),
        "invalid_manufactured_form_audit_ids": invalid_manufactured_audit_ids,
        "baseline_freeze_ids": sorted(item["id"] for item in authoritative_baselines),
        "invalid_baseline_freeze_ids": invalid_baseline_ids,
        "semantic_binding_coverage": semantic_binding_coverage,
        "invalid_semantic_binding_ids": invalid_binding_ids,
        "assembly_graph_audit": assembly_graph_audit,
        "component_packet_count": len(visual_component_packets),
        "invalid_component_packet_ids": invalid_component_packet_ids,
        "latest_visual_frequency_scorecard_id": (
            latest_frequency_scorecard["id"] if latest_frequency_scorecard else None
        ),
        "latest_visual_frequency_status": (
            latest_frequency_scorecard["status"] if latest_frequency_scorecard else None
        ),
        "invalid_visual_frequency_scorecard_ids": invalid_frequency_scorecard_ids,
    }
    if invalid_rig_ids:
        blockers.append("visual-geometry fixed-rig ledger contains invalid receipts")
    if invalid_scorecard_ids:
        blockers.append(
            "visual-geometry scorecard ledger contains invalid or unreplayable receipts"
        )
    if invalid_manufactured_audit_ids:
        blockers.append("manufactured-form audit ledger contains invalid receipts")
    if invalid_baseline_ids:
        blockers.append("visual baseline freeze ledger contains invalid receipts")
    if invalid_binding_ids:
        blockers.append("semantic geometry binding ledger contains invalid receipts")
    if invalid_component_packet_ids:
        blockers.append("visual component packet ledger contains invalid receipts")
    if invalid_frequency_scorecard_ids:
        blockers.append("visual-frequency scorecard ledger contains invalid receipts")
    if visual_geometry_required:
        if not authoritative_baselines:
            blockers.append(
                "L3+ requires an immutable visual baseline freeze covering the authoritative scene"
            )
        if not authoritative_visual_rigs:
            blockers.append("L3+ requires a receipt-valid fixed rig with approved cameras")
        visual_reference_ids = set(latest_visual_by_reference)
        required_visual_reference_ids = {
            str(item["id"])
            for item in records["references"]
            if str(item.get("media_type", "")).startswith("image/")
            and bool(item.get("acceptance_eligible", True))
        }
        if visual_reference_ids != required_visual_reference_ids:
            blockers.append(
                "L3+ visual-geometry scorecards do not cover every acceptance reference"
            )
        if any(item["status"] != "PASS" for item in latest_visual_by_reference.values()):
            blockers.append("L3+ visual-geometry scorecard has unresolved geometry evidence")
        if latest_manufactured_audit is None:
            blockers.append("L3+ requires a replayable manufactured-form audit")
        elif latest_manufactured_audit["status"] != "PASS":
            blockers.append(
                "L3+ manufactured-form audit has failures or unresolved visual warnings"
            )
        if (
            semantic_binding_coverage is None
            or not semantic_binding_coverage["all_visible_resolved"]
        ):
            blockers.append("L3+ visual acceptance is blocked by unbound visible geometry")
        elif not semantic_binding_coverage["all_visible_accepted"]:
            blockers.append(
                "L3+ requires an ACCEPTED_BOUND semantic binding for every visible mesh"
            )
        if assembly_graph_audit and assembly_graph_audit["missing_part_of_objects"]:
            blockers.append("L3+ assembly graph is missing PART_OF relationships")
        if latest_frequency_scorecard is None:
            blockers.append(
                "L3+ requires a component-weighted primary/secondary/tertiary scorecard"
            )
        elif latest_frequency_scorecard["status"] != "PASS":
            blockers.append(
                "L3+ visual-frequency gates contain failed or unscored important components"
            )
    latest_target = target_resolutions[-1] if target_resolutions else None
    metrics["target_resolution"] = latest_target
    metrics["digital_twin_classification"] = (
        latest_target.get("target", {}).get("output_classification")
        if latest_target
        else "REFERENCE RECONSTRUCTION"
    )
    if latest_target and not latest_target.get("authority", {}).get("valid"):
        blockers.append("canonical target resolution receipt is missing or invalid")
    elif latest_target and latest_target["status"] != "RESOLVED":
        blockers.append("canonical target variant has unresolved material ambiguity")
    current_evidence_sources = [
        source
        for source in evidence_sources
        if latest_target and source.get("target_id") == latest_target["id"]
    ]
    accepted_governance_reviews = {"approved", "not_applicable", "user_owned"}
    incomplete_source_rights = [
        source["id"]
        for source in current_evidence_sources
        if not source.get("rights", {}).get("internal_use")
        or not source.get("rights", {}).get("status")
        or not source.get("reviewed_by")
        or not source.get("reviewed_at")
        or source.get("source", {}).get("access_policy", {}).get("robots_respected") is not True
        or source.get("source", {}).get("access_policy", {}).get("authentication_boundary")
        not in {"none", "user_authorized"}
        or source.get("source", {}).get("access_policy", {}).get("source_terms_review")
        not in accepted_governance_reviews
        or source.get("source", {}).get("access_policy", {}).get("privacy_review")
        not in accepted_governance_reviews
        or not source.get("authority", {}).get("governance_valid")
        or (
            source.get("status") == "ACQUIRED"
            and not source.get("authority", {}).get("acquisition_valid")
        )
    ]
    metrics["source_governance"] = {
        "source_count": len(current_evidence_sources),
        "rights_complete": not incomplete_source_rights,
        "redistributable_source_count": sum(
            bool(source.get("rights", {}).get("redistribution"))
            for source in current_evidence_sources
        ),
        "incomplete_source_ids": incomplete_source_rights,
        "authority": {
            source["id"]: source.get("authority", {}) for source in current_evidence_sources
        },
    }
    adoption_statuses = ("PROPOSED", "ADOPTED", "EXCLUDED")
    invalid_adoption_receipts = [
        item["id"] for item in reference_adoption_proposals if not item.get("receipt_valid")
    ]
    governed_reference_ids = {
        source["reference_id"]
        for source in current_evidence_sources
        if source.get("status") == "ACQUIRED"
        and source.get("reference_id")
        and source.get("authority", {}).get("acquisition_valid")
    }
    derivation_audit = records["reference_derivation_audit"]
    governed_reference_ids.update(derivation_audit["valid_governed_reference_ids"])
    orphan_reference_ids = sorted(
        reference["id"]
        for reference in renderable_references
        if reference["id"] not in governed_reference_ids
    )
    metrics["legacy_reference_adoption"] = {
        "proposal_count": len(reference_adoption_proposals),
        "status_counts": {
            status: sum(item["status"] == status for item in reference_adoption_proposals)
            for status in adoption_statuses
        },
        "invalid_receipt_ids": invalid_adoption_receipts,
        "orphan_renderable_reference_ids": orphan_reference_ids,
        "orphan_renderable_reference_count": len(orphan_reference_ids),
    }
    metrics["reference_derivations"] = derivation_audit
    if derivation_audit["missing_receipt_reference_ids"] or derivation_audit["invalid_derivations"]:
        blockers.append("derived acceptance references lack valid governance lineage receipts")
    if invalid_adoption_receipts:
        blockers.append("one or more legacy reference adoption receipts are invalid")
    if (
        metrics["digital_twin_classification"] == "AUTONOMOUS EVIDENCE-BASED RECONSTRUCTION"
        and orphan_reference_ids
    ):
        blockers.append(
            "autonomous reconstruction has legacy references outside the governed source ledger"
        )
    metrics["source_discovery"] = {
        "provider_count": sum(
            item.get("authority", {}).get("valid", False) for item in records["search_providers"]
        ),
        "invalid_provider_ids": [
            item["id"]
            for item in records["search_providers"]
            if not item.get("authority", {}).get("valid")
        ],
        "run_count": len(records["search_discovery_runs"]),
        "completed_run_count": sum(
            item["status"] == "COMPLETED" and item.get("receipt_valid")
            for item in records["search_discovery_runs"]
        ),
        "registered_source_count": sum(
            len(item["results"].get("registered_source_ids", []))
            for item in records["search_discovery_runs"]
            if item.get("receipt_valid")
        ),
        "invalid_run_ids": [
            item["id"] for item in records["search_discovery_runs"] if not item.get("receipt_valid")
        ],
    }
    if metrics["source_discovery"]["invalid_provider_ids"]:
        blockers.append("one or more search providers lack valid named-review authority")
    if metrics["source_discovery"]["invalid_run_ids"]:
        blockers.append("one or more search discovery receipts are invalid")
    pursuit_runs = records["evidence_pursuit_runs"]
    invalid_pursuit_receipts = [
        item["id"]
        for item in pursuit_runs
        if item["status"] != "RUNNING" and not item["receipt_valid"]
    ]
    running_pursuits = [item["id"] for item in pursuit_runs if item["status"] == "RUNNING"]
    latest_pursuit = pursuit_runs[-1] if pursuit_runs else None
    metrics["missing_evidence_pursuit"] = {
        "run_count": len(pursuit_runs),
        "status_counts": {
            status: sum(item["status"] == status for item in pursuit_runs)
            for status in (
                "RUNNING",
                "COVERAGE_COMPLETE",
                "SOURCES_DISCOVERED",
                "EVIDENCE_CEILING",
            )
        },
        "latest_status": latest_pursuit["status"] if latest_pursuit else None,
        "latest_focus_terms": latest_pursuit["focus_terms"] if latest_pursuit else [],
        "invalid_receipt_ids": invalid_pursuit_receipts,
        "running_ids": running_pursuits,
    }
    if invalid_pursuit_receipts:
        blockers.append("one or more missing-evidence pursuit receipts are invalid")
    if running_pursuits:
        blockers.append("missing-evidence pursuit is still running")
    conflict_audit = records["evidence_conflict_audit"]
    metrics["evidence_conflicts"] = {
        **conflict_audit,
        "run_count": len(records["evidence_conflict_runs"]),
        "review_count": len(records["evidence_conflict_reviews"]),
    }
    if conflict_audit["unresolved_blocking_count"]:
        blockers.append("target-incompatible evidence has unresolved blocking conflicts")
    metrics["evidence_duplicates"] = {
        **records["evidence_duplicate_audit"],
        "run_count": len(records["evidence_duplicate_runs"]),
    }
    if incomplete_source_rights:
        blockers.append(
            "evidence source rights/access governance is incomplete or forbids internal use"
        )
    lifecycle_audit = records["scene_lifecycle_audit"]
    metrics["scene_lifecycle"] = {
        "authoritative_scene_id": authoritative_scene_id,
        "authoritative_state": authoritative_scene.get("state") if authoritative_scene else None,
        "transition_count": len(records["scene_transitions"]),
        **lifecycle_audit,
    }
    if l3_plus and authoritative_scene and authoritative_scene.get("state") != "PROMOTED":
        blockers.append(
            "L3+ authoritative scene must complete ACCEPTED and PROMOTED lifecycle gates"
        )
    if (
        l3_plus
        and authoritative_scene
        and not lifecycle_audit["authoritative_promotion_chain_valid"]
    ):
        blockers.append(
            "L3+ authoritative scene lacks a verified, receipt-complete promotion chain"
        )
    if lifecycle_audit["invalid_transition_ids"]:
        blockers.append("one or more scene lifecycle transition receipts are invalid")
    if lifecycle_audit["invalid_evaluation_ids"]:
        blockers.append("one or more candidate evaluation receipts are invalid")
    if lifecycle_audit["unreceipted_superseded_scene_ids"]:
        blockers.append("one or more superseded scenes lack a verified transition receipt")
    authoritative_evaluations = [
        item for item in candidate_evaluations if item["scene_id"] == authoritative_scene_id
    ]
    metrics["candidate_transactions"] = {
        "count": len(candidate_evaluations),
        "authoritative": authoritative_evaluations[-1] if authoritative_evaluations else None,
    }
    if (
        l3_plus
        and authoritative_scene
        and not any(item["status"] == "PASSED" for item in authoritative_evaluations)
    ):
        blockers.append("L3+ authoritative scene lacks a passed all-gate candidate transaction")
    pending_semantic_nodes = [
        item["id"]
        for item in semantic_nodes
        if item.get("type") != "digital_twin_root"
        and (
            not item.get("geometry")
            or item.get("acceptance_state") not in {"accepted", "not_applicable"}
        )
    ]
    metrics["semantic_twin"] = {
        "node_count": len(semantic_nodes),
        "pending_node_ids": pending_semantic_nodes,
    }
    if l3_plus and semantic_nodes and pending_semantic_nodes:
        blockers.append("semantic twin graph contains unbound or unaccepted required components")
    metrics["generative_hypotheses"] = {
        "count": len(generative_results),
        "acceptance_eligible_count": sum(
            bool(item.get("acceptance_eligible")) for item in generative_results
        ),
        "invalid_request_ids": generative_audit["invalid_request_ids"],
        "invalid_result_ids": generative_audit["invalid_result_ids"],
    }
    if any(item.get("acceptance_eligible") for item in generative_results):
        blockers.append("a generative hypothesis was incorrectly marked acceptance eligible")
    if generative_audit["invalid_request_ids"]:
        blockers.append("one or more generative request receipts are invalid")
    if generative_audit["invalid_result_ids"]:
        blockers.append("one or more generative result receipts are invalid")
    metrics["synthetic_views"] = {
        "count": len(synthetic_views),
        "coherent_count": sum(bool(item.get("coherent")) for item in synthetic_views),
        "acceptance_eligible_count": sum(
            bool(item.get("acceptance_eligible")) for item in synthetic_views
        ),
    }
    if any(item.get("acceptance_eligible") for item in synthetic_views):
        blockers.append("a synthetic hypothetical view was incorrectly marked acceptance eligible")
    nonterminal_campaigns = [
        item["id"] for item in campaigns if item["status"] in {"RUNNING", "PAUSED"}
    ]
    unsuccessful_campaigns = [
        item["id"]
        for item in campaigns
        if item["status"] == "STOPPED"
        and (item.get("result") or {}).get("stop_reason") != "all requested gates pass"
    ]
    metrics["campaigns"] = {
        "count": len(campaigns),
        "nonterminal_ids": nonterminal_campaigns,
        "unsuccessful_terminal_ids": unsuccessful_campaigns,
    }
    if campaigns and (nonterminal_campaigns or unsuccessful_campaigns):
        blockers.append("autonomous campaign has not terminated with all requested gates passing")
    evidence_records = (
        [("measurement", item["id"], item.get("evidence_class")) for item in measurements]
        + [("feature", item.get("id", "unknown"), item.get("evidence_class")) for item in features]
        + [
            (
                "material",
                item.get("id", "unknown"),
                item.get("evidence", {}).get("evidence_class"),
            )
            for item in material_profiles
        ]
    )
    authority_buckets = {
        "measured": {"MEASURED", "MANUFACTURER_SPEC"},
        "observed": {
            "MULTI_VIEW_OBSERVED",
            "TEARDOWN_OBSERVED",
            "SINGLE_VIEW_OBSERVED",
        },
        "inferred": {"INFERRED_HIGH_CONFIDENCE", "INFERRED_LOW_CONFIDENCE"},
        "unseen": {"OCCLUDED", "UNSEEN"},
    }
    classified = {
        bucket: [
            {"record_type": record_type, "id": record_id, "evidence_class": evidence_class}
            for record_type, record_id, evidence_class in evidence_records
            if evidence_class in classes
        ]
        for bucket, classes in authority_buckets.items()
    }
    metrics["evidence_authority"] = {
        **classified,
        "counts": {bucket: len(items) for bucket, items in classified.items()},
        "synthetic_hypothesis_count": len(synthetic_views) + len(generative_results),
        "unclassified_count": sum(
            evidence_class not in set().union(*authority_buckets.values())
            for _record_type, _record_id, evidence_class in evidence_records
        ),
    }
    metrics["active_learning"] = {
        "cycle_count": len(active_learning_cycles),
        "status_counts": {
            status: sum(item.get("status") == status for item in active_learning_cycles)
            for status in sorted({str(item.get("status")) for item in active_learning_cycles})
        },
        "invalid_cycle_ids": active_learning_audit["invalid_cycle_ids"],
        "invalid_model_revision_ids": active_learning_audit["invalid_model_revision_ids"],
        "active_model_count": sum(
            item["status"] == "ACTIVE" for item in records["active_model_revisions"]
        ),
    }
    if active_learning_audit["invalid_cycle_ids"]:
        blockers.append("one or more active-learning cycle receipts are invalid")
    if active_learning_audit["invalid_model_revision_ids"]:
        blockers.append("one or more active model activation receipts are invalid")
    unresolved_surface_cells = [
        item["id"]
        for item in surface_coverage_cells
        if item.get("observation_count", 0) == 0
        or item.get("occlusion_fraction", 1.0) > 0.5
        or (item.get("best_resolution_pixels") or 0) < 512
    ]
    metrics["surface_coverage_atlas"] = {
        "cell_count": len(surface_coverage_cells),
        "resolved_count": len(surface_coverage_cells) - len(unresolved_surface_cells),
        "unresolved_cell_ids": unresolved_surface_cells,
    }
    if l3_plus and surface_coverage_cells and unresolved_surface_cells:
        blockers.append("L3+ canonical surface atlas contains unresolved required regions")
    if not references:
        blockers.append("no reference evidence")
    elif not renderable_references:
        blockers.append("no renderable image reference evidence")
    compared_references = {comparison["reference_id"] for comparison in comparisons}
    renderable_reference_ids = {reference["id"] for reference in renderable_references}
    if compared_references != renderable_reference_ids:
        blockers.append("not every reference has a rendered comparison")
    partial_crop_comparisons = [
        item for item in comparisons if item["metrics"].get("reference_partial_object_crop") is True
    ]
    full_object_comparisons = [item for item in comparisons if item not in partial_crop_comparisons]
    if any(item["metrics"].get("silhouette_iou", 0.0) < 0.95 for item in full_object_comparisons):
        blockers.append("silhouette IoU is below the 0.95 L3 threshold")
    if l3_plus and partial_crop_comparisons:
        blockers.append(
            "partial-object crop comparisons are diagnostic and cannot establish "
            "full-object silhouette acceptance"
        )
    valid_mask_proposal_statuses = {"PROPOSED", "APPROVED", "REJECTED"}
    invalid_reference_mask_proposal_ids = sorted(
        item["id"]
        for item in reference_mask_proposals
        if not item.get("proposal_valid")
        or item.get("status") not in valid_mask_proposal_statuses
        or (
            item.get("status") == "PROPOSED"
            and (
                item.get("decision_digest") is not None or item.get("approved_mask_id") is not None
            )
        )
        or (
            item.get("status") in {"APPROVED", "REJECTED"}
            and item.get("decision_valid") is not True
        )
    )
    metrics["reference_mask_proposals"] = {
        "count": len(reference_mask_proposals),
        "pending_count": sum(item.get("status") == "PROPOSED" for item in reference_mask_proposals),
        "approved_count": sum(
            item.get("status") == "APPROVED" for item in reference_mask_proposals
        ),
        "rejected_count": sum(
            item.get("status") == "REJECTED" for item in reference_mask_proposals
        ),
        "invalid_proposal_ids": invalid_reference_mask_proposal_ids,
    }
    if invalid_reference_mask_proposal_ids:
        blockers.append("reference-mask proposal ledger contains invalid receipts")
    approved_masks = {
        item["id"]: item
        for item in reference_masks
        if item.get("approval_state") == "approved"
        and item.get("confidence") == "high"
        and item.get("intended_use") == "silhouette_evaluation"
        and item.get("decision_valid") is True
    }

    def authoritative_segmentation(comparison: dict[str, Any]) -> bool:
        comparison_metrics = comparison["metrics"]
        method = comparison_metrics.get("reference_segmentation")
        if (
            comparison_metrics.get("reference_segmentation_confidence") == "high"
            and method == "embedded_alpha"
        ):
            return True
        binding = comparison_metrics.get("reference_mask", {})
        stored = approved_masks.get(binding.get("id"))
        return bool(
            comparison_metrics.get("reference_segmentation_confidence") == "high"
            and method in {"reviewed_manual_mask", "human_reviewed_machine_proposal"}
            and stored
            and stored["reference_id"] == comparison["reference_id"]
            and stored["artifact_digest"] == binding.get("artifact_digest")
            and stored["source_artifact_digest"] == binding.get("source_artifact_digest")
            and stored["method"] == binding.get("method")
            and stored["method"] in {"reviewed_manual_mask", "human_reviewed_machine_proposal"}
            and stored["reviewer"]
            and stored["reason"]
        )

    if l3_plus and any(not authoritative_segmentation(item) for item in comparisons):
        blockers.append("silhouette comparison requires high-confidence reference masks")
    metrics["comparison_selection"] = {
        "policy": "latest_per_reference_for_authoritative_scene",
        "authoritative_scene_id": authoritative_scene_id,
        "historical_count": len(all_comparison_history),
        "valid_historical_count": len(comparison_history),
        "invalid_comparison_ids": invalid_comparison_ids,
        "validly_superseded_comparison_ids": sorted(
            item["id"] for item in all_comparison_history if item.get("supersession_valid")
        ),
        "eligible_count": len(eligible_comparison_history),
        "active_count": len(comparisons),
        "active_comparison_ids": [item["id"] for item in comparisons],
        "full_object_comparison_ids": [item["id"] for item in full_object_comparisons],
        "diagnostic_partial_crop_comparison_ids": [item["id"] for item in partial_crop_comparisons],
        "active_render_scene_ids": sorted(
            {
                scene_id
                for item in comparisons
                for scene_id in item.get("render_scene_ids", [])
                if authoritative_scene_id is None or scene_id == authoritative_scene_id
            }
        ),
        "unbound_comparison_ids": sorted(
            item["id"] for item in comparison_history if not item.get("render_scene_ids")
        ),
        "superseded_comparison_ids": sorted(
            {item["id"] for item in comparison_history} - {item["id"] for item in comparisons}
        ),
    }
    preferred_camera_solution_ids_by_reference: dict[str, set[str]] = {}
    for comparison in comparisons:
        preferred_camera_solution_ids_by_reference.setdefault(
            str(comparison["reference_id"]), set()
        ).update(str(value) for value in comparison.get("render_camera_solution_ids", []))
    active_cameras = _active_camera_set(
        cameras,
        preferred_camera_solution_ids_by_reference,
    )
    landmark_by_review_id = {
        item["review"]["id"]: item
        for item in camera_landmark_proposals
        if item.get("review") and item["review"].get("id")
    }
    invalid_pnp_solution_ids = []
    pnp_review_ids = []
    for document in cameras:
        if (
            document["id"] not in active_cameras["active_solution_ids"]
            or document.get("backend") != "opencv_pnp_landmarks"
        ):
            continue
        solution = document.get("solution", {})
        camera_records = solution.get("cameras", [])
        review_ids = {
            camera.get("diagnostics", {}).get("landmark_review_id") for camera in camera_records
        }
        review_digests = {
            camera.get("diagnostics", {}).get("landmark_review_digest") for camera in camera_records
        }
        review_ids.discard(None)
        review_digests.discard(None)
        valid = len(review_ids) == 1 and len(review_digests) == 1
        proposal = landmark_by_review_id.get(next(iter(review_ids), ""))
        if valid and proposal:
            valid = bool(
                proposal.get("receipt_valid")
                and proposal.get("status") == "READY_FOR_PNP"
                and proposal.get("review_digest") == next(iter(review_digests))
            )
        else:
            valid = False
        if not valid:
            invalid_pnp_solution_ids.append(document["id"])
        else:
            pnp_review_ids.extend(review_ids)
    camera_classes = set(active_cameras["registration_classes"])
    camera_reference_ids = set(active_cameras["covered_reference_ids"])
    approved_camera_reference_ids = set(active_cameras["approved_reference_ids"])
    camera_approved = bool(active_cameras["approved"])
    metrics["camera"] = {
        **active_cameras,
        "selection_policy": "active_authoritative_comparison_then_latest_per_reference",
        "approved": camera_approved,
        "consensus_count": len(camera_consensus),
        "latest_consensus": camera_consensus[-1]["report"] if camera_consensus else None,
        "pnp_landmark_review_ids": sorted(set(pnp_review_ids)),
        "invalid_pnp_landmark_solution_ids": sorted(invalid_pnp_solution_ids),
        "invalid_decision_solution_ids": sorted(
            document["id"]
            for document in cameras
            if document.get("solution", {}).get("approval", {}).get("state")
            in {"approved", "rejected"}
            and not document.get("decision_receipt_valid")
        ),
    }
    export_formats = sorted({item["format"] for item in exports})
    metrics["exports"] = {
        "count": len(exports),
        "formats": export_formats,
        "artifact_digests": [item["artifact_digest"] for item in exports],
        "latest": exports[-1] if exports else None,
        "authoritative_blend_digest": (
            authoritative_scene["artifact_digest"] if authoritative_scene else None
        ),
    }
    if l3_plus and not {"blend", "glb"}.issubset(export_formats):
        blockers.append("L3+ requires registered editable BLEND and delivery GLB exports")
    if l3_plus and (not camera_classes or camera_classes != {RegistrationClass.METRIC.value}):
        blockers.append("L3+ requires metric camera solutions")
    elif l3_plus and not camera_approved:
        blockers.append("L3+ metric camera solutions require explicit human approval")
    if l3_plus and invalid_pnp_solution_ids:
        blockers.append("L3+ PnP cameras require valid immutable landmark-review receipts")
    if metrics["camera"]["invalid_decision_solution_ids"]:
        blockers.append("one or more camera review decisions lack a valid immutable receipt")
    if l3_plus and camera_reference_ids != renderable_reference_ids:
        blockers.append("L3+ camera set does not cover every reference")
    if l3_plus and approved_camera_reference_ids != renderable_reference_ids:
        blockers.append("L3+ approved camera set does not cover every reference")
    if any(reference.get("rights_state") in {"UNKNOWN", None} for reference in references):
        blockers.append("reference rights/provenance review is incomplete")
    if authoritative_scene is None or authoritative_scene.get("inventory") is None:
        blockers.append("authoritative Blender scene audit is missing")
    elif any(
        finding.get("severity") == "error"
        for finding in authoritative_scene["inventory"].get("audit_findings", [])
    ):
        blockers.append("authoritative Blender scene audit has errors")
    if l3_plus:
        known_dimensions = [
            measurement
            for measurement in measurements
            if measurement["type"] == "known_overall_dimension"
            and measurement["evidence_class"] in {"MEASURED", "MANUFACTURER_SPEC"}
        ]
        if len({item["value"].get("axis") for item in known_dimensions}) < 3:
            blockers.append("L3+ requires authoritative overall dimensions on x, y, and z")
        invalid_measurement_provenance = [
            measurement["id"]
            for measurement in known_dimensions
            if measurement["evidence_class"] == "MANUFACTURER_SPEC"
            and not measurement.get("provenance_valid")
        ]
        metrics["measurement_provenance"] = {
            "manufacturer_measurement_ids": [
                measurement["id"]
                for measurement in known_dimensions
                if measurement["evidence_class"] == "MANUFACTURER_SPEC"
            ],
            "invalid_measurement_ids": invalid_measurement_provenance,
        }
        if invalid_measurement_provenance:
            blockers.append(
                "L3+ manufacturer measurements require valid source provenance receipts"
            )
        dimension_residuals: dict[str, Any] = {}
        inventory = authoritative_scene.get("inventory") if authoritative_scene else None
        audited_dimensions, dimension_scope = _scoped_audited_dimensions(
            inventory or {}, known_dimensions, project_metadata
        )
        metrics["dimension_scope"] = dimension_scope
        if audited_dimensions and len(audited_dimensions) == 3:
            axis_index = {"x": 0, "y": 1, "z": 2}
            for measurement in known_dimensions:
                axis = measurement["value"].get("axis")
                if axis not in axis_index:
                    continue
                expected = float(measurement["value"]["millimetres"])
                actual = float(audited_dimensions[axis_index[axis]])
                tolerance = measurement["uncertainty"].get("millimetres")
                tolerance = float(tolerance) if isinstance(tolerance, (int, float)) else 0.0
                dimension_residuals[axis] = {
                    "audited_scene_mm": actual,
                    "audited_dimension_mm": actual,
                    "authoritative_mm": expected,
                    "delta_mm": actual - expected,
                    "tolerance_mm": tolerance,
                    "within_tolerance": abs(actual - expected) <= tolerance,
                }
            if len(dimension_residuals) == 3 and not all(
                item["within_tolerance"] for item in dimension_residuals.values()
            ):
                blockers.append("L3+ audited scene envelope exceeds authoritative tolerance")
        metrics["dimension_residuals"] = dimension_residuals
        active_features = [
            feature
            for feature in features
            if feature.get("approval", {}).get("state") != "rejected"
        ]
        rejected_features = [
            feature
            for feature in features
            if feature.get("approval", {}).get("state") == "rejected"
        ]
        if not active_features:
            blockers.append("L3+ technical feature graph is empty")
        else:
            required_fields = {
                "type",
                "parent_component",
                "dimensions",
                "coordinate_frame",
                "observations",
                "reference_ids",
                "confidence",
                "uncertainty",
                "evidence_class",
                "model_revision",
                "human_approval",
            }
            incomplete = [
                feature.get("id", "unknown")
                for feature in active_features
                if not required_fields.issubset(feature)
                or not feature.get("parent_component")
                or not feature.get("model_revision")
                or not (feature.get("observations") or feature.get("reference_ids"))
            ]
            if incomplete:
                blockers.append("L3+ feature graph contains incomplete evidence records")
            if any(feature.get("human_approval") is not True for feature in active_features):
                blockers.append("L3+ feature graph contains unapproved features")
            elif any(
                feature.get("approval", {}).get("state") != "approved"
                or not feature.get("approval", {}).get("reviewer")
                or not feature.get("approval", {}).get("reason")
                for feature in active_features
            ):
                blockers.append("L3+ feature approvals lack named review provenance")
            unsupported_classes = {
                "INFERRED_LOW_CONFIDENCE",
                "OCCLUDED",
                "UNSEEN",
            }
            if any(
                feature.get("evidence_class") in unsupported_classes for feature in active_features
            ):
                blockers.append("L3+ feature graph contains low-authority or unseen features")
            required_groups = set(
                project_metadata.get("metadata", {}).get("required_feature_groups", [])
            )
            covered_groups = {
                feature.get("coverage_group")
                for feature in active_features
                if feature.get("coverage_group")
            }
            missing_groups = sorted(required_groups - covered_groups)
            if missing_groups:
                blockers.append(
                    "L3+ feature coverage groups are missing: " + ", ".join(missing_groups)
                )
            metrics["feature_graph"] = {
                "feature_count": len(active_features),
                "rejected_feature_count": len(rejected_features),
                "required_coverage_groups": sorted(required_groups),
                "covered_groups": sorted(covered_groups),
                "missing_groups": missing_groups,
                "approved_count": sum(
                    feature.get("human_approval") is True for feature in active_features
                ),
            }
        unaccepted_applied_repairs = [
            repair["id"]
            for repair in repairs
            if repair["status"] == "applied"
            and not repair.get("result", {}).get("acceptance", {}).get("accepted", False)
        ]
        if unaccepted_applied_repairs:
            blockers.append("L3+ applied repairs still require acceptance evidence")
        latest_scene_id = authoritative_scene_id
        rejected_latest_repairs = [
            repair["id"]
            for repair in repairs
            if repair["status"] == "rejected"
            and repair.get("result", {}).get("generated_scene", {}).get("id") == latest_scene_id
        ]
        if rejected_latest_repairs:
            blockers.append("L3+ authoritative scene contains a rejected repair checkpoint")
        malformed_acceptances = [
            repair["id"]
            for repair in repairs
            if repair["status"] == "accepted"
            and (
                not repair.get("result", {}).get("acceptance", {}).get("accepted")
                or not repair.get("result", {}).get("acceptance", {}).get("reviewer")
                or not repair.get("result", {}).get("acceptance", {}).get("reason")
                or not repair.get("result", {}).get("acceptance", {}).get("receipt_id")
                or not repair.get("result", {})
                .get("acceptance", {})
                .get("evidence", {})
                .get("receipt_verification", {})
                .get("valid")
            )
        ]
        if malformed_acceptances:
            blockers.append("L3+ accepted repairs lack verified named review provenance")
        metrics["repairs"] = {
            "proposal_count": len(repairs),
            "status_counts": {
                status: sum(repair["status"] == status for repair in repairs)
                for status in ("proposed", "approved", "applied", "accepted", "rejected")
            },
            "unaccepted_applied_ids": unaccepted_applied_repairs,
            "rejected_latest_ids": rejected_latest_repairs,
            "malformed_accepted_ids": malformed_acceptances,
        }
        metrics["geometry_evidence"] = {
            "run_count": len(geometry_runs),
            "backends": sorted({run["backend"] for run in geometry_runs}),
            "commercial_eligible_runs": sum(
                bool(run["commercial_eligible"]) for run in geometry_runs
            ),
            "depth_artifact_count": sum(
                len(run["evidence"].get("depth_artifacts", [])) for run in geometry_runs
            ),
            "point_artifact_count": sum(
                len(run["evidence"].get("point_artifacts", [])) for run in geometry_runs
            ),
            "occupancy_artifact_count": sum(
                len(run["evidence"].get("occupancy_artifacts", [])) for run in geometry_runs
            ),
            "visual_hull_artifact_count": sum(
                len(run["evidence"].get("visual_hull_artifacts", [])) for run in geometry_runs
            ),
        }
        metrics["component_fitting"] = {
            "proposal_count": len(component_fits),
            "status_counts": {
                status: sum(fit["status"] == status for fit in component_fits)
                for status in ("proposed", "accepted", "rejected")
            },
            "accepted_with_named_review": sum(
                fit["status"] == "accepted"
                and bool(fit.get("reviewer"))
                and bool(fit.get("reason"))
                and fit.get("applied_revision") is not None
                for fit in component_fits
            ),
            "accepted_with_valid_decision_receipt": sum(
                fit["status"] == "accepted" and fit["decision_receipt_valid"]
                for fit in component_fits
            ),
        }
        invalid_fit_decisions = [
            fit["id"]
            for fit in component_fits
            if fit["status"] in {"accepted", "rejected"} and not fit["decision_receipt_valid"]
        ]
        if invalid_fit_decisions:
            blockers.append("L3+ component fit decisions lack valid immutable receipts")
        metrics["component_fitting"]["invalid_decision_receipt_ids"] = invalid_fit_decisions
        low_authority_accepted_fits = [
            fit["id"]
            for fit in component_fits
            if fit["status"] == "accepted"
            and not fit["result"].get("release_eligible_evidence", False)
        ]
        if low_authority_accepted_fits:
            blockers.append("L3+ accepted component fits contain low-authority evidence")
        metrics["component_fitting"]["low_authority_accepted_ids"] = low_authority_accepted_fits
        metrics["intelligence"] = {
            "dataset_count": len(datasets),
            "generated_dataset_count": sum(item["status"] == "generated" for item in datasets),
            "training_run_count": len(training_runs),
            "completed_training_count": sum(
                item["status"] == "completed" for item in training_runs
            ),
            "model_evaluation_count": len(model_evaluations),
            "visual_oracle_count": len(visual_oracles),
            "commercial_visual_oracle_count": sum(
                item["commercial_eligible"] for item in visual_oracles
            ),
            "optimization_run_count": len(optimization_runs),
            "accepted_optimization_count": sum(
                item["status"] == "accepted" for item in optimization_runs
            ),
            "accepted_optimization_with_valid_receipt_count": sum(
                item["status"] == "accepted" and item["decision_receipt_valid"]
                for item in optimization_runs
            ),
            "model_approval_count": len(model_approvals),
            "installed_model_count": len(model_installations),
        }
        invalid_optimization_decisions = [
            item["id"]
            for item in optimization_runs
            if item["status"] in {"accepted", "rejected"} and not item["decision_receipt_valid"]
        ]
        metrics["intelligence"]["invalid_optimization_decision_ids"] = (
            invalid_optimization_decisions
        )
        if invalid_optimization_decisions:
            blockers.append("L3+ optimization decisions lack valid immutable receipts")
        invalid_multiview_search_receipts = [
            item["id"]
            for item in multiview_search_runs
            if item["status"] == "COMPLETE" and not item["receipt_valid"]
        ]
        searches_claiming_acceptance = [
            item["id"]
            for item in multiview_search_runs
            if item["configuration"].get("acceptance_performed") is not False
        ]
        metrics["multiview_parameter_search"] = {
            "run_count": len(multiview_search_runs),
            "status_counts": {
                status: sum(item["status"] == status for item in multiview_search_runs)
                for status in ("PLANNED", "RUNNING", "COMPLETE", "FAILED", "STALE")
            },
            "completed_with_valid_receipt_count": sum(
                item["status"] == "COMPLETE" and item["receipt_valid"]
                for item in multiview_search_runs
            ),
            "invalid_completed_receipt_ids": invalid_multiview_search_receipts,
            "acceptance_boundary_violation_ids": searches_claiming_acceptance,
        }
        if invalid_multiview_search_receipts:
            blockers.append("L3+ multiview searches lack valid immutable receipts")
        if searches_claiming_acceptance:
            blockers.append("L3+ multiview search crossed the named-review acceptance boundary")
        required_material_regions = set(
            project_metadata.get("metadata", {}).get("required_material_regions", [])
        )
        approved_materials = [
            profile for profile in material_profiles if profile["status"] == "approved"
        ]
        approved_regions = {profile["region_id"] for profile in approved_materials}
        missing_material_regions = sorted(required_material_regions - approved_regions)
        pending_materials = [
            profile["id"] for profile in material_profiles if profile["status"] == "proposed"
        ]
        low_authority_materials = [
            profile["id"]
            for profile in approved_materials
            if profile["confidence"] < 0.8
            or profile["evidence"]["evidence_class"]
            in {"INFERRED_LOW_CONFIDENCE", "OCCLUDED", "UNSEEN"}
        ]
        malformed_material_reviews = [
            profile["id"]
            for profile in approved_materials
            if not profile.get("approval", {}).get("reviewer")
            or not profile.get("approval", {}).get("reason")
            or profile.get("authority", {}).get("may_establish_geometry") is not False
        ]
        if required_material_regions and missing_material_regions:
            blockers.append(
                "L3+ required material regions are missing approved profiles: "
                + ", ".join(missing_material_regions)
            )
        if pending_materials:
            blockers.append("L3+ material appearance profiles still require named review")
        if low_authority_materials:
            blockers.append("L3+ approved material profiles contain low-authority evidence")
        if malformed_material_reviews:
            blockers.append("L3+ material approvals lack valid named review provenance")
        metrics["appearance"] = {
            "geometry_separate_from_rgb": True,
            "profile_count": len(material_profiles),
            "approved_count": len(approved_materials),
            "required_regions": sorted(required_material_regions),
            "approved_regions": sorted(approved_regions),
            "missing_regions": missing_material_regions,
            "pending_profile_ids": pending_materials,
            "low_authority_profile_ids": low_authority_materials,
            "color_calibrated_count": sum(
                profile.get("color_calibration", {}).get("state") not in {None, "unreported"}
                for profile in approved_materials
            ),
            "lighting_estimated_count": sum(
                profile.get("lighting_estimate", {}).get("state") not in {None, "unreported"}
                for profile in approved_materials
            ),
        }
        calibration_required = bool(
            project_metadata.get("metadata", {}).get("required_calibration_gates", False)
        )
        latest_calibration = calibration_runs[-1] if calibration_runs else None
        metrics["calibration"] = {
            "required": calibration_required,
            "run_count": len(calibration_runs),
            "latest": latest_calibration,
        }
        if calibration_required:
            required_gate_names = {
                "known_dimensions",
                "camera_recovery",
                "scale_recovery",
                "repeatability",
                "export_consistency",
            }
            if latest_calibration is None:
                blockers.append("required calibration gate report is missing")
            else:
                report = latest_calibration["gates"]
                gates = report.get("gates", {})
                missing = sorted(required_gate_names - set(gates))
                failed = sorted(
                    name for name in required_gate_names if not gates.get(name, {}).get("passed")
                )
                if missing:
                    blockers.append("required calibration gates are missing: " + ", ".join(missing))
                if failed:
                    blockers.append("required calibration gates failed: " + ", ".join(failed))
                if not report.get("passed"):
                    blockers.append("calibration gate report is not accepted")
    accepted = not blockers
    evidence_status = (
        "ACCEPTED"
        if accepted
        else "PARTIAL_ACCEPTANCE"
        if authoritative_scene is not None and bool(references)
        else "NOT_ACCEPTED"
    )
    return {
        "accepted": accepted,
        "project_status": evidence_status,
        "target_fidelity": target,
        "accepted_fidelity": target if accepted else None,
        "blockers": blockers,
        "metrics": metrics,
        "thresholds": {
            "silhouette_iou": 0.95,
            "camera_registration": RegistrationClass.METRIC.value,
            "material_confidence": 0.8,
        },
    }


def _human_receipt(payload: dict[str, Any], payload_sha256: str) -> str:
    acceptance = payload["acceptance"]
    evidence = payload["evidence"]
    project = payload["project"]
    status = "ACCEPTED" if acceptance["accepted"] else "NOT ACCEPTED"
    blockers = acceptance["blockers"] or ["None"]
    lines = [
        f"# Acceptance receipt — {project['name']}",
        "",
        f"**Status:** {status}",
        "",
        f"- Receipt ID: `{payload['receipt_id']}`",
        f"- Created: `{payload['created_at']}`",
        f"- Target fidelity: `{acceptance['target_fidelity']}`",
        f"- Accepted fidelity: `{acceptance['accepted_fidelity'] or 'none'}`",
        f"- Payload SHA-256: `{payload_sha256}`",
        f"- Source revision: `{payload['source_code_revision']}`",
        f"- Blender: `{payload['runtime'].get('blender_version') or 'not discovered'}`",
        f"- Platform: `{payload['runtime']['platform']}`",
        f"- Safe mode: `{str(payload['runtime']['safe_mode']).lower()}`",
        "",
        "## Acceptance blockers",
        "",
        *[f"- {item}" for item in blockers],
        "",
        "## Evidence inventory",
        "",
        "| Record class | Count |",
        "| --- | ---: |",
    ]
    for key in (
        "references",
        "reference_masks",
        "reference_mask_proposals",
        "target_resolutions",
        "evidence_sources",
        "reference_adoption_proposals",
        "search_providers",
        "search_discovery_runs",
        "evidence_pursuit_runs",
        "evidence_conflict_runs",
        "evidence_conflict_reviews",
        "evidence_duplicate_runs",
        "measurements",
        "measurement_grids",
        "capture_requests",
        "tier_reviews",
        "camera_solutions",
        "camera_landmark_proposals",
        "camera_refinement_runs",
        "features",
        "components",
        "scenes",
        "scene_transitions",
        "candidate_evaluations",
        "render_runs",
        "comparisons",
        "exports",
        "calibration_runs",
        "material_profiles",
        "video_analysis_runs",
        "feature_tracks",
        "semantic_nodes",
        "semantic_edges",
        "reconstruction_portfolios",
        "reconstruction_candidates",
        "generative_requests",
        "generative_results",
        "campaigns",
        "agent_proposals",
        "context_packets",
        "synthetic_views",
        "active_learning_cycles",
        "active_learning_events",
        "active_model_revisions",
        "surface_coverage_cells",
        "warm_services",
        "beast_benchmark_runs",
        "multiview_search_runs",
        "multiview_search_candidates",
        "model_installations",
        "job_provenance",
    ):
        value = evidence.get(key)
        count = len(value) if isinstance(value, list) else int(value is not None)
        lines.append(f"| {key.replace('_', ' ')} | {count} |")
    lines.extend(["", "## Authoritative asset hashes", ""])
    for scene in evidence.get("scenes", []):
        lines.append(f"- Blender scene `{scene['artifact_digest']}`")
    for item in evidence.get("exports", []):
        lines.append(f"- {item['format'].upper()} export `{item['artifact_digest']}`")
    if not evidence.get("scenes") and not evidence.get("exports"):
        lines.append("- None")
    lines.extend(
        [
            "",
            "This rendering summarizes the canonical machine-readable JSON receipt. ",
            "Verify the JSON payload hash before relying on this summary.",
            "",
        ]
    )
    return "\n".join(lines)


def export_receipt(project: ProjectStore) -> dict[str, Any]:
    project_metadata = project.project()
    records = _records(project)
    blender = discover_blender()
    acceptance = _acceptance(records, project_metadata["target_fidelity"], project_metadata)
    # Historical inventories remain bound by their digest and scene artifact,
    # while the only inventory used for acceptance stays complete in the receipt.
    _compact_historical_scene_inventories(records)
    accepted_fidelity = acceptance["accepted_fidelity"] if acceptance["accepted"] else None
    if project_metadata.get("accepted_fidelity") != accepted_fidelity:
        project_metadata["accepted_fidelity"] = accepted_fidelity
        project_metadata["updated_at"] = utc_now()
        atomic_write_json(project.project_file, project_metadata)
    payload = {
        "schema_version": 1,
        "receipt_id": str(uuid.uuid4()),
        "created_at": utc_now(),
        "project": project_metadata,
        "source_code_revision": runtime_revision(),
        "runtime": {
            "python": platform.python_version(),
            "platform": platform.platform(),
            "blender_path": blender.path,
            "blender_version": blender.version,
            "safe_mode": safe_mode(),
        },
        "evidence": records,
        "acceptance": acceptance,
    }
    envelope = {
        "payload": payload,
        "payload_sha256": hashlib.sha256(canonical_json(payload)).hexdigest(),
    }
    relative = Path("receipts") / f"{payload['receipt_id']}.json"
    receipt_path = project.root / relative
    atomic_write_json(receipt_path, envelope)
    artifact = ArtifactStore(project).ingest_file(
        receipt_path, media_type="application/vnd.bvmcp.receipt+json"
    )
    human_relative = Path("receipts") / f"{payload['receipt_id']}.md"
    human_path = project.root / human_relative
    atomic_write_text(human_path, _human_receipt(payload, envelope["payload_sha256"]))
    human_artifact = ArtifactStore(project).ingest_file(human_path, media_type="text/markdown")
    with project.connection() as connection:
        connection.execute(
            "INSERT INTO receipts(id,digest,fidelity_level,accepted,created_at) VALUES(?,?,?,?,?)",
            (
                payload["receipt_id"],
                artifact.digest,
                project_metadata["target_fidelity"],
                int(payload["acceptance"]["accepted"]),
                payload["created_at"],
            ),
        )
    return {
        "id": payload["receipt_id"],
        "path": str(relative),
        "human_path": str(human_relative),
        "artifact": artifact.to_dict(),
        "human_artifact": human_artifact.to_dict(),
        "payload_sha256": envelope["payload_sha256"],
        "acceptance": payload["acceptance"],
    }


def evaluate_acceptance(project: ProjectStore) -> dict[str, Any]:
    """Recompute current acceptance without creating or registering a receipt."""
    metadata = project.project()
    return _acceptance(_records(project), metadata["target_fidelity"], metadata)


def verify_receipt(path: Path, *, project: ProjectStore | None = None) -> dict[str, Any]:
    path = path.expanduser().resolve()
    envelope = json.loads(path.read_text(encoding="utf-8"))
    actual_payload = hashlib.sha256(canonical_json(envelope["payload"])).hexdigest()
    payload_valid = actual_payload == envelope.get("payload_sha256")
    artifact_valid: bool | None = None
    referenced_artifacts_valid: bool | None = None
    missing_or_corrupt_artifacts: list[str] = []
    if project:
        artifact_digest, _ = sha256_file(path)
        with project.connection() as connection:
            artifact_valid = (
                connection.execute(
                    "SELECT 1 FROM receipts WHERE digest=?", (artifact_digest,)
                ).fetchone()
                is not None
            )
            artifact_rows = {
                row["digest"]: row["relative_path"]
                for row in connection.execute("SELECT digest,relative_path FROM artifacts")
            }
        referenced = _receipt_artifact_digests(envelope.get("payload", {}))
        for digest in referenced:
            relative = artifact_rows.get(digest)
            if relative is None:
                missing_or_corrupt_artifacts.append(digest)
                continue
            artifact_path = project.root / relative
            if not artifact_path.is_file() or sha256_file(artifact_path)[0] != digest:
                missing_or_corrupt_artifacts.append(digest)
        referenced_artifacts_valid = not missing_or_corrupt_artifacts
    return {
        "valid": (
            payload_valid
            and artifact_valid is not False
            and referenced_artifacts_valid is not False
        ),
        "payload_valid": payload_valid,
        "registered_artifact": artifact_valid,
        "referenced_artifacts_valid": referenced_artifacts_valid,
        "missing_or_corrupt_artifacts": missing_or_corrupt_artifacts,
        "receipt_id": envelope.get("payload", {}).get("receipt_id"),
        "payload_sha256": actual_payload,
    }


def _receipt_artifact_digests(value: Any) -> set[str]:
    """Collect only fields whose schema semantics explicitly identify stored artifacts."""
    single_keys = {
        "artifact_digest",
        "record_digest",
        "decision_digest",
        "proposal_digest",
        "receipt_digest",
        "report_digest",
        "predictions_digest",
        "checkpoint_digest",
        "residual_digest",
        "render_digest",
        "best_render_digest",
        "scene_artifact_digest",
        "source_artifact_digest",
        "baseline_scene_digest",
        "locality_plan_digest",
        "optimization_proposal_digest",
        "transition_receipt_digest",
        "discovery_receipt_digest",
        "media_hash",
        "content_hash",
    }
    plural_keys = {"artifact_digests", "pass_artifact_digests", *ARTIFACT_FIELDS}
    digests: set[str] = set()

    def valid(item: Any) -> bool:
        return (
            isinstance(item, str)
            and len(item) == 64
            and all(character in "0123456789abcdef" for character in item)
        )

    def visit(item: Any) -> None:
        if isinstance(item, dict):
            if {"digest", "path", "media_type"}.issubset(item) and valid(item["digest"]):
                digests.add(item["digest"])
            for key, nested in item.items():
                if key in single_keys and valid(nested):
                    digests.add(nested)
                elif key in plural_keys:
                    if isinstance(nested, dict):
                        digests.update(value for value in nested.values() if valid(value))
                    elif isinstance(nested, list):
                        digests.update(value for value in nested if valid(value))
                visit(nested)
        elif isinstance(item, list):
            for nested in item:
                visit(nested)

    visit(value)
    return digests


class ReceiptBuilder:
    """Compatibility facade for workflows that have already persisted their evidence."""

    def __init__(self, project: ProjectStore):
        self.project = project

    def build(self, **_evidence: Any) -> dict[str, Any]:
        result = export_receipt(self.project)
        envelope = json.loads((self.project.root / result["path"]).read_text(encoding="utf-8"))
        return {**result, "receipt": envelope["payload"]}

    @staticmethod
    def verify(path: Path) -> dict[str, Any]:
        return verify_receipt(path)
