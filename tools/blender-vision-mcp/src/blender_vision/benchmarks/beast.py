from __future__ import annotations

import json
import math
import uuid
from pathlib import Path
from typing import Any

from blender_vision.acceptance.lifecycle import audit_scene_lifecycle
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.benchmarks.reviews import BenchmarkReviewStore
from blender_vision.blender.passes import GOVERNED_RENDER_PASSES
from blender_vision.cameras.decisions import CameraDecisionStore
from blender_vision.cameras.state import validate_complete_camera_state
from blender_vision.comparison.store import ComparisonStore
from blender_vision.core.models import RegistrationClass
from blender_vision.core.util import atomic_write_json, sha256_file, utc_now
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.conflicts import EvidenceConflictStore
from blender_vision.evidence.duplicates import EvidenceDuplicateStore
from blender_vision.evidence.targets import TargetResolver
from blender_vision.projects.store import ProjectStore

STAGE_NAMES = {
    1: "Mac Studio repair",
    2: "DGX repair",
    3: "RTX 5090 reconstruction",
    4: "autonomous external object",
}
MANDATORY_RENDER_PASSES = GOVERNED_RENDER_PASSES


class BeastBenchmarkAuditor:
    """Evidence-derived stage gates; it records failures instead of accepting declarations."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def audit(self, stage: int) -> dict[str, Any]:
        if stage not in STAGE_NAMES:
            raise ValueError("Beast benchmark stage must be 1 through 4")
        facts = self._facts()
        checks = self._checks(stage, facts)
        run_id = str(uuid.uuid4())
        now = utc_now()
        report = {
            "schema_version": 1,
            "id": run_id,
            "stage": stage,
            "name": STAGE_NAMES[stage],
            "status": "PASSED" if all(check["passed"] for check in checks) else "INCOMPLETE",
            "checks": checks,
            "facts": facts,
            "created_at": now,
        }
        relative = Path("receipts") / f"beast-stage-{stage}-{run_id}.json"
        atomic_write_json(self.project.root / relative, report)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.beast-benchmark-stage+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO beast_benchmark_runs"
                "(id,stage,status,report_json,artifact_digest,created_at) VALUES(?,?,?,?,?,?)",
                (run_id, stage, report["status"], json.dumps(report), artifact.digest, now),
            )
        return {**report, "artifact": artifact.to_dict(), "path": str(relative)}

    def _facts(self) -> dict[str, Any]:
        with self.project.connection() as connection:
            artifact_rows = {
                row["digest"]: dict(row)
                for row in connection.execute(
                    "SELECT digest,size,relative_path FROM artifacts"
                )
            }
            scenes = [
                dict(row)
                for row in connection.execute(
                    "SELECT id,state,is_authoritative,artifact_digest,relative_path "
                    "FROM scene_assets"
                )
            ]
            repairs = [
                {
                    **dict(row),
                    "result": json.loads(row["result_json"]) if row["result_json"] else {},
                }
                for row in connection.execute(
                    "SELECT id,kind,status,result_json FROM repair_proposals"
                )
            ]
            evaluations = [
                dict(row)
                for row in connection.execute(
                    "SELECT id,scene_id,status,gates_json,regressions_json,receipt_digest "
                    "FROM candidate_evaluations"
                )
            ]
            camera_rows = [
                dict(row)
                for row in connection.execute(
                    "SELECT id,solution_json,approved,decision_id,decision_digest "
                    "FROM camera_solutions"
                )
            ]
            render_rows = [
                dict(row)
                for row in connection.execute(
                    "SELECT id,scene_id,camera_solution_id,outputs_json FROM render_runs"
                )
            ]
            comparison_rows = [
                dict(row)
                for row in connection.execute(
                    "SELECT id,reference_id,render_digest,residual_digest,metrics_json,"
                    "receipt_digest,superseded_by_id,supersession_digest,created_at "
                    "FROM comparisons"
                )
            ]
            image_reference_rows = connection.execute(
                "SELECT id FROM reference_items WHERE media_type LIKE 'image/%' "
                "AND acceptance_eligible=1"
            ).fetchall()
            export_rows = [
                dict(row)
                for row in connection.execute(
                    "SELECT scene_id,artifact_digest,format,relative_path FROM exports"
                )
            ]
            counts = {
                table: connection.execute(f"SELECT COUNT(*) FROM {table}").fetchone()[0]
                for table in (
                    "camera_solutions",
                    "render_runs",
                    "exports",
                    "target_resolutions",
                    "evidence_sources",
                    "video_analysis_runs",
                    "semantic_nodes",
                    "reconstruction_portfolios",
                    "reference_masks",
                    "measurements",
                    "campaigns",
                )
            }
            target_rows = connection.execute(
                "SELECT id,status,created_at FROM target_resolutions "
                "ORDER BY rowid DESC"
            ).fetchall()
            evidence_rows = connection.execute(
                "SELECT s.id,s.target_id,s.status,s.reference_id,s.source_json,r.rights_json,"
                "r.reviewed_by,r.reviewed_at,ri.artifact_digest AS reference_digest "
                "FROM evidence_sources s "
                "LEFT JOIN rights_ledger r ON r.source_id=s.id "
                "LEFT JOIN reference_items ri ON ri.id=s.reference_id"
            ).fetchall()
            video_rows = connection.execute(
                "SELECT source_reference_id,report_digest FROM video_analysis_runs"
            ).fetchall()
            mask_rows = connection.execute(
                "SELECT artifact_digest,source_artifact_digest,reviewer,approval_state "
                "FROM reference_masks"
            ).fetchall()
            measurement_rows = connection.execute(
                "SELECT type,value_json,evidence_class FROM measurements"
            ).fetchall()
            portfolio_rows = connection.execute(
                "SELECT id FROM reconstruction_portfolios"
            ).fetchall()
            campaign_rows = connection.execute(
                "SELECT config_json FROM campaigns"
            ).fetchall()
            semantic_pending = connection.execute(
                "SELECT record_json FROM semantic_nodes"
            ).fetchall()

        root = self.project.root.resolve()

        def verified_artifact(digest: Any, relative_path: Any = None) -> bool:
            if not isinstance(digest, str) or digest not in artifact_rows:
                return False
            row = artifact_rows[digest]
            paths = [row["relative_path"]]
            if relative_path is not None:
                paths.append(relative_path)
            for raw in paths:
                try:
                    path = (root / str(raw)).resolve()
                    path.relative_to(root)
                except (OSError, ValueError):
                    return False
                if not path.is_file():
                    return False
                actual_digest, actual_size = sha256_file(path)
                if actual_digest != digest:
                    return False
                if raw == row["relative_path"] and actual_size != int(row["size"]):
                    return False
            return True

        def immutable_camera_valid(camera: dict[str, Any]) -> bool:
            try:
                validate_complete_camera_state(camera)
            except (KeyError, TypeError, ValueError):
                return False
            return True

        eligible_reference_ids = {str(row["id"]) for row in image_reference_rows}
        valid_camera_ids: set[str] = set()
        valid_metric_camera_ids: set[str] = set()
        decision_store = CameraDecisionStore(self.project)
        for row in camera_rows:
            document = json.loads(row["solution_json"])
            cameras = document.get("cameras", []) if isinstance(document, dict) else []
            approval = document.get("approval", {}) if isinstance(document, dict) else {}
            covered_reference_ids = {
                str(camera.get("reference_id")) for camera in cameras
            }
            if (
                bool(row["approved"])
                and bool(cameras)
                and bool(eligible_reference_ids)
                and covered_reference_ids == eligible_reference_ids
                and len(cameras) == len(eligible_reference_ids)
                and all(immutable_camera_valid(camera) for camera in cameras)
                and str(approval.get("reviewer", "")).strip()
                and str(approval.get("reason", "")).strip()
                and decision_store.verify_record(
                    {**row, "solution": document}
                )["valid"]
            ):
                valid_camera_ids.add(row["id"])
                if all(
                    camera.get("registration_class") == RegistrationClass.METRIC.value
                    for camera in cameras
                ):
                    valid_metric_camera_ids.add(row["id"])

        valid_scene_ids = {
            item["id"]
            for item in scenes
            if verified_artifact(item["artifact_digest"], item["relative_path"])
        }
        authoritative = next(
            (
                item
                for item in scenes
                if item["is_authoritative"] and item["id"] in valid_scene_ids
            ),
            None,
        )

        lifecycle_audit = audit_scene_lifecycle(self.project)
        verified_passed_evaluation_ids = set(
            lifecycle_audit["verified_passed_evaluation_ids"]
        )
        target_authorities = {
            row["id"]: TargetResolver(self.project).authority_status(row["id"])
            for row in target_rows
        }
        latest_resolved_target_id = next(
            (
                row["id"]
                for row in target_rows
                if row["status"] == "RESOLVED"
                and target_authorities[row["id"]]["valid"]
            ),
            None,
        )
        conflict_audit = (
            EvidenceConflictStore(self.project).audit(
                latest_resolved_target_id, record=False
            )
            if latest_resolved_target_id
            else {
                "unresolved_blocking_count": 0,
                "source_eligibility": {},
            }
        )
        duplicate_audit = (
            EvidenceDuplicateStore(self.project).audit(
                latest_resolved_target_id, record=False
            )
            if latest_resolved_target_id
            else {"source_eligibility": {}}
        )
        acquired_governed_sources: list[dict[str, Any]] = []
        acquired_source_rights: list[dict[str, Any]] = []
        source_authority = EvidenceAcquisitionStore(self.project)
        for row in evidence_rows:
            if latest_resolved_target_id and row["target_id"] != latest_resolved_target_id:
                continue
            source = json.loads(row["source_json"])
            rights = json.loads(row["rights_json"]) if row["rights_json"] else {}
            reference_digest = row["reference_digest"]
            acquired = (
                row["status"] == "ACQUIRED"
                and bool(row["reference_id"])
                and verified_artifact(reference_digest)
                and source.get("content_hash") == reference_digest
            )
            authority = source_authority.authority_status(row["id"])
            governed = authority["valid"]
            if acquired:
                acquired_source_rights.append(rights)
            conflict_eligible = conflict_audit["source_eligibility"].get(row["id"], {}).get(
                "acceptance_eligible", True
            )
            duplicate_eligible = duplicate_audit["source_eligibility"].get(
                row["id"], {}
            ).get("independent_evidence_eligible", True)
            if acquired and governed and conflict_eligible and duplicate_eligible:
                acquired_governed_sources.append(dict(row))

        valid_video_count = sum(
            bool(row["source_reference_id"])
            and verified_artifact(row["report_digest"])
            for row in video_rows
        )
        valid_mask_count = sum(
            row["approval_state"] == "approved"
            and str(row["reviewer"]).strip()
            and verified_artifact(row["artifact_digest"])
            and verified_artifact(row["source_artifact_digest"])
            for row in mask_rows
        )
        authoritative_dimension_axes = {
            json.loads(row["value_json"]).get("axis")
            for row in measurement_rows
            if row["type"] == "known_overall_dimension"
            and row["evidence_class"] in {"MEASURED", "MANUFACTURER_SPEC"}
        } - {None}
        semantic_records = [json.loads(row["record_json"]) for row in semantic_pending]
        if latest_resolved_target_id:
            root_ids = {
                item["id"]
                for item in semantic_records
                if item.get("type") == "digital_twin_root"
                and item.get("parameters", {}).get("canonical_target_id")
                == latest_resolved_target_id
            }
            selected_semantic_ids = set(root_ids)
            changed = True
            while changed:
                before = len(selected_semantic_ids)
                selected_semantic_ids.update(
                    item["id"]
                    for item in semantic_records
                    if item.get("parent_id") in selected_semantic_ids
                )
                changed = len(selected_semantic_ids) != before
            semantic_records = [
                item for item in semantic_records if item["id"] in selected_semantic_ids
            ]
        portfolio_ids = {row["id"] for row in portfolio_rows}
        target_portfolio_ids = {
            configuration.get("portfolio_id")
            for row in campaign_rows
            for configuration in [json.loads(row["config_json"])]
            if configuration.get("target_id") == latest_resolved_target_id
        } - {None}
        valid_export_formats = {
            str(row["format"]).lower()
            for row in export_rows
            if authoritative
            and row["scene_id"] == authoritative["id"]
            and row["scene_id"] in valid_scene_ids
            and verified_artifact(row["artifact_digest"], row["relative_path"])
        }
        valid_render_primary_digests: dict[str, set[str]] = {}
        metric_render_run_ids: set[str] = set()
        for row in render_rows:
            if (
                not authoritative
                or row["scene_id"] != authoritative["id"]
                or row["scene_id"] not in valid_scene_ids
                or row["camera_solution_id"] not in valid_camera_ids
            ):
                continue
            try:
                outputs = json.loads(row["outputs_json"])
            except (json.JSONDecodeError, TypeError):
                continue
            if not isinstance(outputs, list) or not outputs:
                continue
            if {str(output.get("reference_id")) for output in outputs} != eligible_reference_ids:
                continue
            primary_digests: set[str] = set()
            valid = True
            for output in outputs:
                primary_digest = output.get("artifact_digest")
                pass_digests = output.get("pass_artifact_digests", {})
                if (
                    not isinstance(pass_digests, dict)
                    or set(pass_digests) < MANDATORY_RENDER_PASSES
                    or not verified_artifact(primary_digest, output.get("relative_path"))
                    or not all(
                        verified_artifact(pass_digests.get(name))
                        for name in MANDATORY_RENDER_PASSES
                    )
                ):
                    valid = False
                    break
                primary_digests.add(str(primary_digest))
            if valid:
                valid_render_primary_digests[str(row["id"])] = primary_digests
                if row["camera_solution_id"] in valid_metric_camera_ids:
                    metric_render_run_ids.add(str(row["id"]))

        compared_references_by_run = {
            run_id: set() for run_id in valid_render_primary_digests
        }
        verified_comparison_ids: list[str] = []
        invalid_comparison_ids: list[str] = []
        superseded_comparison_ids: list[str] = []
        comparison_store = ComparisonStore(self.project)
        comparison_verifications = {
            str(row["id"]): comparison_store.verify_record(row, replay=True)
            for row in comparison_rows
        }
        for row in comparison_rows:
            verification = comparison_verifications[str(row["id"])]
            supersession = comparison_store.verify_supersession(
                row,
                source_verification=verification,
                replacement_verification=comparison_verifications.get(
                    str(row.get("superseded_by_id"))
                ),
            )
            if supersession["valid"]:
                superseded_comparison_ids.append(str(row["id"]))
                continue
            if not verification["valid"]:
                invalid_comparison_ids.append(str(row["id"]))
                continue
            try:
                silhouette_iou = float(json.loads(row["metrics_json"])["silhouette_iou"])
            except (KeyError, TypeError, ValueError, json.JSONDecodeError):
                invalid_comparison_ids.append(str(row["id"]))
                continue
            if not math.isfinite(silhouette_iou) or not 0.0 <= silhouette_iou <= 1.0:
                invalid_comparison_ids.append(str(row["id"]))
                continue
            verified_comparison_ids.append(str(row["id"]))
            if str(row["reference_id"]) not in eligible_reference_ids:
                continue
            for run_id, primary_digests in valid_render_primary_digests.items():
                if str(row["render_digest"]) in primary_digests:
                    compared_references_by_run[run_id].add(str(row["reference_id"]))

        comparison_suite_run_ids = sorted(
            run_id
            for run_id, reference_ids in compared_references_by_run.items()
            if bool(eligible_reference_ids) and reference_ids == eligible_reference_ids
        )
        metric_comparison_suite_run_ids = sorted(
            set(comparison_suite_run_ids) & metric_render_run_ids
        )

        project_metadata = self.project.project().get("metadata", {})
        foam_lod_review = BenchmarkReviewStore(self.project).dgx_foam_lod_status()

        return {
            "project_metadata": project_metadata,
            "scenes": scenes,
            "authoritative_scene": authoritative,
            "scene_lifecycle_audit": lifecycle_audit,
            "repair_statuses": [
                {"id": item["id"], "kind": item["kind"], "status": item["status"]}
                for item in repairs
            ],
            "accepted_repair_count": sum(item["status"] == "accepted" for item in repairs),
            "passed_authoritative_transaction": bool(
                authoritative
                and any(
                    item["scene_id"] == authoritative["id"]
                    and item["id"] in verified_passed_evaluation_ids
                    for item in evaluations
                )
            ),
            "rejected_scene_count": sum(
                item["state"] == "REJECTED" and item["id"] in valid_scene_ids
                for item in scenes
            ),
            "counts": counts,
            "export_formats": sorted(valid_export_formats),
            "fixed_camera_solution_count": len(valid_camera_ids),
            "fixed_metric_camera_solution_count": len(valid_metric_camera_ids),
            "mandatory_render_suite_count": len(valid_render_primary_digests),
            "metric_mandatory_render_suite_count": len(metric_render_run_ids),
            "comparison_suite_count": len(comparison_suite_run_ids),
            "metric_comparison_suite_count": len(metric_comparison_suite_run_ids),
            "verified_render_run_ids": sorted(valid_render_primary_digests),
            "verified_comparison_ids": sorted(verified_comparison_ids),
            "comparison_suite_run_ids": comparison_suite_run_ids,
            "metric_comparison_suite_run_ids": metric_comparison_suite_run_ids,
            "invalid_comparison_ids": sorted(invalid_comparison_ids),
            "superseded_comparison_ids": sorted(superseded_comparison_ids),
            "foam_lod_approval_valid": foam_lod_review["valid"],
            "foam_lod_review": foam_lod_review,
            "resolved_target_count": sum(
                row["status"] == "RESOLVED" and target_authorities[row["id"]]["valid"]
                for row in target_rows
            ),
            "target_authority": target_authorities,
            "acquired_governed_source_count": len(acquired_governed_sources),
            "unresolved_blocking_evidence_conflict_count": conflict_audit[
                "unresolved_blocking_count"
            ],
            "duplicate_evidence_group_count": duplicate_audit.get(
                "duplicate_group_count", 0
            ),
            "valid_video_analysis_count": valid_video_count,
            "valid_reference_mask_count": valid_mask_count,
            "valid_portfolio_count": len(portfolio_ids & target_portfolio_ids),
            "authoritative_dimension_axes": sorted(authoritative_dimension_axes),
            "all_sources_redistributable": bool(acquired_source_rights)
            and all(item.get("redistribution") is True for item in acquired_source_rights),
            "semantic_node_count": len(semantic_records),
            "semantic_pending_count": sum(
                item.get("type") != "digital_twin_root"
                and item.get("acceptance_state") not in {"accepted", "not_applicable"}
                for item in semantic_records
            ),
        }

    @staticmethod
    def _checks(stage: int, facts: dict[str, Any]) -> list[dict[str, Any]]:
        authoritative = facts["authoritative_scene"] or {}
        counts = facts["counts"]
        camera_gate = (
            (
                "approved metric camera solution recorded",
                facts["fixed_metric_camera_solution_count"] > 0,
            )
            if stage in {3, 4}
            else (
                "fixed camera solution recorded",
                facts["fixed_camera_solution_count"] > 0,
            )
        )
        render_suite_count = (
            facts["metric_mandatory_render_suite_count"]
            if stage in {3, 4}
            else facts["mandatory_render_suite_count"]
        )
        comparison_suite_count = (
            facts["metric_comparison_suite_count"]
            if stage in {3, 4}
            else facts["comparison_suite_count"]
        )
        common = [
            (
                "authoritative candidate promoted with verified lifecycle receipts",
                authoritative.get("state") == "PROMOTED"
                and facts["scene_lifecycle_audit"][
                    "authoritative_promotion_chain_valid"
                ]
                and not facts["scene_lifecycle_audit"][
                    "unreceipted_superseded_scene_ids"
                ],
            ),
            (
                "all-gate candidate transaction passed",
                facts["passed_authoritative_transaction"]
                and comparison_suite_count > 0
                and not facts["invalid_comparison_ids"],
            ),
            camera_gate,
            ("mandatory render suite recorded", render_suite_count > 0),
            (
                "blend and GLB delivery recorded",
                {"blend", "glb"} <= set(facts["export_formats"]),
            ),
        ]
        if stage == 1:
            specific = [
                ("Mac Studio repair accepted", facts["accepted_repair_count"] > 0),
                (
                    "rear grille repair exists",
                    any("mac_studio" in item["kind"] for item in facts["repair_statuses"]),
                ),
            ]
        elif stage == 2:
            specific = [
                ("DGX v17 regression remains rejected", facts["rejected_scene_count"] > 0),
                (
                    "foam LOD strategy is explicitly approved",
                    facts["foam_lod_approval_valid"],
                ),
            ]
        elif stage == 3:
            specific = [
                ("target variant resolved", facts["resolved_target_count"] > 0),
                (
                    "autonomous evidence acquisition recorded",
                    facts["acquired_governed_source_count"] > 0,
                ),
                ("video intelligence recorded", facts["valid_video_analysis_count"] > 0),
                (
                    "teardown ROI governance recorded",
                    facts["valid_reference_mask_count"] > 0,
                ),
                ("candidate portfolio recorded", facts["valid_portfolio_count"] > 0),
                (
                    "semantic model complete",
                    facts["semantic_node_count"] > 1
                    and facts["semantic_pending_count"] == 0,
                ),
            ]
        else:
            specific = [
                (
                    "benchmark declared external with no private starting model",
                    facts["project_metadata"].get("benchmark_origin") == "external_public"
                    and facts["project_metadata"].get("private_starting_model") is False,
                ),
                ("all acquired sources are redistributable", facts["all_sources_redistributable"]),
                (
                    "official dimensions recorded",
                    facts["authoritative_dimension_axes"] == ["x", "y", "z"],
                ),
                ("owned or reusable video analyzed", facts["valid_video_analysis_count"] > 0),
                ("autonomous campaign recorded", counts["campaigns"] > 0),
            ]
        return [
            {"name": name, "passed": bool(passed)} for name, passed in [*common, *specific]
        ]
