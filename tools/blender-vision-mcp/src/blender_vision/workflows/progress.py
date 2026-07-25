from __future__ import annotations

import hashlib
import json
from typing import Any

from blender_vision.acceptance.receipts import evaluate_acceptance
from blender_vision.core.models import FidelityLevel, RegistrationClass
from blender_vision.core.util import canonical_json, utc_now
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.conflicts import EvidenceConflictStore
from blender_vision.evidence.coverage_atlas import SurfaceCoverageAtlas
from blender_vision.evidence.duplicates import EvidenceDuplicateStore
from blender_vision.evidence.targets import TargetResolver
from blender_vision.geometry.portfolio import ReconstructionPortfolioStore
from blender_vision.orchestration.campaigns import CampaignStore
from blender_vision.projects.store import ProjectStore
from blender_vision.workflows.executor import AutonomousWorkflowExecutor


class WorkflowProgressReporter:
    """Recompute compact host-visible progress without granting or mutating authority."""

    def __init__(self, project: ProjectStore):
        self.project = project

    def report(self, campaign_id: str | None = None) -> dict[str, Any]:
        campaign = self._campaign(campaign_id)
        target_id = (campaign or {}).get("configuration", {}).get("target_id")
        with self.project.connection() as connection:
            if target_id:
                target_row = connection.execute(
                    "SELECT * FROM target_resolutions WHERE id=?", (target_id,)
                ).fetchone()
            else:
                target_row = connection.execute(
                    "SELECT * FROM target_resolutions ORDER BY rowid DESC LIMIT 1"
                ).fetchone()
            if target_row:
                target_id = target_row["id"]
            source_rows = (
                connection.execute(
                    "SELECT * FROM evidence_sources WHERE target_id=? ORDER BY created_at,id",
                    (target_id,),
                ).fetchall()
                if target_id
                else []
            )
            reference_rows = connection.execute(
                "SELECT id,media_type,evidence_role,acceptance_eligible FROM reference_items"
            ).fetchall()
            measurement_rows = connection.execute(
                "SELECT id,type,value_json,evidence_class,uncertainty_json FROM measurements "
                "ORDER BY created_at,id"
            ).fetchall()
            video_rows = connection.execute(
                "SELECT id,report_json FROM video_analysis_runs ORDER BY created_at,id"
            ).fetchall()
            pursuit_row = connection.execute(
                "SELECT * FROM evidence_pursuit_runs ORDER BY created_at DESC,id DESC LIMIT 1"
            ).fetchone()
            capture_rows = connection.execute(
                "SELECT id,request_json,status FROM capture_requests ORDER BY created_at,id"
            ).fetchall()
            counts = {
                table: connection.execute(f"SELECT COUNT(*) FROM {table}").fetchone()[0]
                for table in (
                    "camera_solutions",
                    "scene_assets",
                    "render_runs",
                    "comparisons",
                    "candidate_evaluations",
                    "semantic_nodes",
                    "exports",
                    "receipts",
                )
            }
            role_rows = (
                connection.execute(
                    "SELECT id,role,objective,status,priority,estimated_cost,confidence "
                    "FROM role_tasks WHERE campaign_id=? "
                    "AND status IN ('ASSIGNED','WAITING_INPUT') "
                    "ORDER BY priority DESC,created_at,id",
                    (campaign["id"],),
                ).fetchall()
                if campaign
                else []
            )
        target = self._target(target_row)
        if target:
            target["authority"] = TargetResolver(self.project).authority_status(target["id"])
        authoritative_target_id = (
            target_id
            if target and target.get("authority", {}).get("valid")
            else None
        )
        acceptance = evaluate_acceptance(self.project)
        source_summary = self._sources(authoritative_target_id, source_rows)
        measurements = self._measurements(measurement_rows)
        videos = self._videos(video_rows, reference_rows)
        atlas = SurfaceCoverageAtlas(self.project).analyze(
            authoritative_target_id or "__invalid_canonical_target__"
        )
        portfolio = self._portfolio(campaign)
        pursuit = self._pursuit(pursuit_row, capture_rows)
        support = self._support_envelope(acceptance, atlas, capture_rows)
        verified_runtime = AutonomousWorkflowExecutor(self.project)._facts()
        review_tasks = [dict(row) for row in role_rows]
        stages = self._stages(
            target=target,
            sources=source_summary,
            measurements=measurements,
            videos=videos,
            atlas=atlas,
            portfolio=portfolio,
            pursuit=pursuit,
            acceptance=acceptance,
            counts=counts,
            verified_runtime=verified_runtime,
        )
        next_action = self._next_action(stages, pursuit, campaign, review_tasks)
        report = {
            "schema_version": 1,
            "report_type": "workflow_progress",
            "created_at": utc_now(),
            "project": {
                "path": str(self.project.root),
                "id": self.project.project()["id"],
                "name": self.project.project()["name"],
            },
            "campaign": self._campaign_summary(campaign),
            "target": target,
            "requested_tier": acceptance["target_fidelity"],
            "project_status": acceptance["project_status"],
            "accepted_tier": acceptance["accepted_fidelity"],
            "evidence": source_summary,
            "measurements": measurements,
            "video": videos,
            "coverage": {
                "surface_cell_count": atlas["cell_count"],
                "resolved_surface_count": atlas["observed_cell_count"],
                "surface_coverage_fraction": atlas["coverage_fraction"],
                "surface_coverage_percent": round(atlas["coverage_fraction"] * 100.0, 2),
                "unresolved_regions": atlas["unresolved_regions"],
            },
            "portfolio": portfolio,
            "missing_evidence": pursuit,
            "support_envelope": support,
            "stages": stages,
            "review_tasks": review_tasks,
            "next_action": next_action,
            "acceptance": {
                "accepted": acceptance["accepted"],
                "blocker_count": len(acceptance["blockers"]),
                "blockers": acceptance["blockers"],
            },
            "human_summary": self._human_summary(
                target,
                source_summary,
                measurements,
                videos,
                atlas,
                portfolio,
                acceptance,
                next_action,
            ),
            "claim_boundary": (
                "Evidence-supported regions are not accepted fidelity scopes. Only verified "
                "transactional acceptance and the final machine receipt may assign a fidelity tier."
            ),
        }
        return {
            **report,
            "report_sha256": hashlib.sha256(canonical_json(report)).hexdigest(),
        }

    def _campaign(self, campaign_id: str | None) -> dict[str, Any] | None:
        store = CampaignStore(self.project)
        if campaign_id:
            return store.get(campaign_id)
        campaigns = store.list()
        return campaigns[-1] if campaigns else None

    @staticmethod
    def _target(row: Any) -> dict[str, Any] | None:
        if row is None:
            return None
        return {
            "id": row["id"],
            "status": row["status"],
            "canonical": json.loads(row["target_json"]),
            "alternatives": json.loads(row["alternatives_json"]),
            "ambiguity": json.loads(row["ambiguity_json"]),
        }

    def _sources(self, target_id: str | None, rows: list[Any]) -> dict[str, Any]:
        if not target_id:
            return {
                "candidate_source_count": 0,
                "acquired_source_count": 0,
                "eligible_acquired_source_count": 0,
                "filtered_out_source_count": 0,
                "rights_review_complete_count": 0,
                "internal_use_permitted_count": 0,
                "unresolved_conflict_count": 0,
                "duplicate_group_count": 0,
            }
        conflicts = EvidenceConflictStore(self.project).audit(target_id, record=False)
        duplicates = EvidenceDuplicateStore(self.project).audit(target_id, record=False)
        authority_store = EvidenceAcquisitionStore(self.project)
        authority = {row["id"]: authority_store.authority_status(row["id"]) for row in rows}
        acquired = [
            row
            for row in rows
            if row["status"] == "ACQUIRED"
            and authority[row["id"]]["acquisition_valid"]
        ]
        eligible = [
            row
            for row in acquired
            if conflicts["source_eligibility"].get(row["id"], {}).get(
                "coverage_eligible", False
            )
            and duplicates["source_eligibility"].get(row["id"], {}).get(
                "coverage_eligible", False
            )
        ]
        rights_rows = self._rights_rows(target_id)
        reviewed = sum(
            bool(
                row["reviewed_by"]
                and row["reviewed_at"]
                and authority[row["source_id"]]["governance_valid"]
            )
            for row in rights_rows
        )
        permitted = sum(
            bool(
                json.loads(row["rights_json"]).get("internal_use")
                and authority[row["source_id"]]["governance_valid"]
            )
            for row in rights_rows
        )
        return {
            "candidate_source_count": len(rows),
            "acquired_source_count": len(acquired),
            "eligible_acquired_source_count": len(eligible),
            "filtered_out_source_count": len(rows) - len(eligible),
            "rights_review_complete_count": reviewed,
            "internal_use_permitted_count": permitted,
            "unresolved_conflict_count": conflicts["unresolved_blocking_count"],
            "duplicate_group_count": duplicates["duplicate_group_count"],
        }

    def _rights_rows(self, target_id: str) -> list[Any]:
        with self.project.connection() as connection:
            return connection.execute(
                "SELECT r.source_id,r.rights_json,r.reviewed_by,r.reviewed_at "
                "FROM rights_ledger r JOIN evidence_sources s "
                "ON s.id=r.source_id WHERE s.target_id=?",
                (target_id,),
            ).fetchall()

    @staticmethod
    def _measurements(rows: list[Any]) -> dict[str, Any]:
        authoritative = []
        for row in rows:
            value = json.loads(row["value_json"])
            if (
                row["type"] == "known_overall_dimension"
                and row["evidence_class"] in {"MEASURED", "MANUFACTURER_SPEC"}
            ):
                authoritative.append(
                    {
                        "id": row["id"],
                        "axis": value.get("axis"),
                        "millimetres": value.get("millimetres"),
                        "evidence_class": row["evidence_class"],
                        "uncertainty": json.loads(row["uncertainty_json"]),
                    }
                )
        return {
            "record_count": len(rows),
            "authoritative_overall_dimension_count": len(authoritative),
            "authoritative_axes": sorted(
                {item["axis"] for item in authoritative if item["axis"]}
            ),
            "authoritative_overall_dimensions": authoritative,
        }

    @staticmethod
    def _videos(rows: list[Any], references: list[Any]) -> dict[str, Any]:
        reports = [json.loads(row["report_json"]) for row in rows]
        return {
            "source_video_count": sum(
                row["media_type"].startswith("video/") for row in references
            ),
            "analysis_run_count": len(reports),
            "extracted_frame_count": sum(int(item.get("frame_count", 0)) for item in reports),
            "selected_keyframe_count": sum(
                len(item.get("acceptance_reference_ids", [])) for item in reports
            ),
            "shot_count": sum(len(item.get("shots", [])) for item in reports),
        }

    def _portfolio(self, campaign: dict[str, Any] | None) -> dict[str, Any]:
        portfolio_id = (campaign or {}).get("configuration", {}).get("portfolio_id")
        if not portfolio_id:
            with self.project.connection() as connection:
                row = connection.execute(
                    "SELECT id FROM reconstruction_portfolios "
                    "ORDER BY created_at DESC,id DESC LIMIT 1"
                ).fetchone()
            portfolio_id = row["id"] if row else None
        if not portfolio_id:
            return {
                "id": None,
                "hypothesis_count": 0,
                "active_hypothesis_count": 0,
                "evaluated_hypothesis_count": 0,
                "status_counts": {},
                "current_best": None,
            }
        store = ReconstructionPortfolioStore(self.project)
        candidates = store.list_candidates(portfolio_id)
        ranking = store.rank(portfolio_id)
        status_counts = {
            status: sum(item["status"] == status for item in candidates)
            for status in sorted({item["status"] for item in candidates})
        }
        best = next(
            (item for item in ranking["ranked"] if item["status"] == "EVALUATED"),
            None,
        )
        return {
            "id": portfolio_id,
            "hypothesis_count": len(candidates),
            "active_hypothesis_count": sum(
                item["status"] in {"PROPOSED", "EVIDENCE_READY", "EVALUATED"}
                for item in candidates
            ),
            "evaluated_hypothesis_count": status_counts.get("EVALUATED", 0),
            "status_counts": status_counts,
            "current_best": (
                {
                    "id": best["id"],
                    "lane": best["lane"],
                    "portfolio_score": best["portfolio_score"],
                    "metrics": best["metrics"],
                    "editable": best["editable"],
                    "acceptance_eligible": best["acceptance_eligible"],
                }
                if best
                else None
            ),
        }

    @staticmethod
    def _pursuit(row: Any, capture_rows: list[Any]) -> dict[str, Any]:
        if row is None:
            return {
                "status": None,
                "focus_terms": [],
                "capture_requests": [],
            }
        capture_ids = set(json.loads(row["capture_request_ids_json"]))
        requests = []
        for item in capture_rows:
            if item["id"] not in capture_ids:
                continue
            request = json.loads(item["request_json"])
            request.update({"id": item["id"], "status": item["status"]})
            requests.append(request)
        return {
            "id": row["id"],
            "status": row["status"],
            "focus_terms": json.loads(row["focus_terms_json"]),
            "capture_requests": requests,
            "receipt_digest": row["report_digest"],
        }

    @staticmethod
    def _support_envelope(
        acceptance: dict[str, Any], atlas: dict[str, Any], capture_rows: list[Any]
    ) -> dict[str, Any]:
        captures = {
            item.get("region"): item
            for item in (json.loads(row["request_json"]) for row in capture_rows)
            if item.get("status") == "requested"
        }
        regions = []
        for cell in atlas["cells"]:
            unresolved = (
                cell["observation_count"] == 0
                or cell["occlusion_fraction"] > 0.5
                or (cell["best_resolution_pixels"] or 0) < 512
            )
            capture = captures.get(cell["region"])
            regions.append(
                {
                    "region": cell["region"],
                    "support_status": "NOT_SUPPORTED" if unresolved else "EVIDENCE_SUPPORTED",
                    "evidence_class": cell["evidence_class"],
                    "observation_count": cell["observation_count"],
                    "uncertainty": cell["uncertainty"],
                    "capture_request_id": capture.get("id") if capture else None,
                    "capture_request_type": capture.get("request_type") if capture else None,
                }
            )
        authority = acceptance["metrics"].get("evidence_authority", {})
        return {
            "overall_acceptance": acceptance["project_status"],
            "accepted_fidelity": acceptance["accepted_fidelity"],
            "accepted_scopes": ["overall"] if acceptance["accepted"] else [],
            "evidence_supported_regions": [
                item["region"]
                for item in regions
                if item["support_status"] == "EVIDENCE_SUPPORTED"
            ],
            "unsupported_regions": [
                item["region"] for item in regions if item["support_status"] == "NOT_SUPPORTED"
            ],
            "regions": regions,
            "authority_counts": authority.get("counts", {}),
            "synthetic_hypothesis_count": authority.get("synthetic_hypothesis_count", 0),
        }

    @staticmethod
    def _stages(
        *,
        target: dict[str, Any] | None,
        sources: dict[str, Any],
        measurements: dict[str, Any],
        videos: dict[str, Any],
        atlas: dict[str, Any],
        portfolio: dict[str, Any],
        pursuit: dict[str, Any],
        acceptance: dict[str, Any],
        counts: dict[str, int],
        verified_runtime: dict[str, Any],
    ) -> list[dict[str, Any]]:
        l3_plus = acceptance["target_fidelity"] in {
            FidelityLevel.L3.value,
            FidelityLevel.L4.value,
            FidelityLevel.L5.value,
        }
        camera = acceptance["metrics"].get("camera", {})
        camera_complete = bool(
            camera.get("approved")
            and (
                not l3_plus
                or set(camera.get("registration_classes", []))
                == {RegistrationClass.METRIC.value}
            )
        )
        lifecycle = acceptance["metrics"].get("scene_lifecycle", {})
        transactions = acceptance["metrics"].get("candidate_transactions", {})
        exports = acceptance["metrics"].get("exports", {})

        def stage(name: str, status: str, detail: str) -> dict[str, str]:
            return {"stage": name, "status": status, "detail": detail}

        return [
            stage(
                "target_resolution",
                "COMPLETE"
                if target
                and target.get("authority", {}).get("valid")
                and target["status"] == "RESOLVED"
                else "BLOCKED",
                (
                    target["status"]
                    if target and target.get("authority", {}).get("valid")
                    else "invalid canonical target authority"
                    if target
                    else "no canonical target"
                ),
            ),
            stage(
                "evidence_acquisition",
                "COMPLETE" if sources["eligible_acquired_source_count"] else "PENDING",
                f"{sources['eligible_acquired_source_count']} eligible acquired sources",
            ),
            stage(
                "rights_and_provenance",
                "COMPLETE"
                if sources["candidate_source_count"]
                and sources["rights_review_complete_count"] == sources["candidate_source_count"]
                else "PENDING",
                (
                    f"{sources['rights_review_complete_count']}/"
                    f"{sources['candidate_source_count']} reviewed"
                ),
            ),
            stage(
                "video_intelligence",
                "NOT_APPLICABLE"
                if not videos["source_video_count"]
                else "COMPLETE"
                if videos["analysis_run_count"]
                else "PENDING",
                f"{videos['selected_keyframe_count']} selected keyframes",
            ),
            stage(
                "coverage",
                "COMPLETE"
                if atlas["cell_count"] and not atlas["unresolved_regions"]
                else "BLOCKED",
                f"{round(atlas['coverage_fraction'] * 100.0, 2)}% canonical surface coverage",
            ),
            stage(
                "authoritative_dimensions",
                "COMPLETE"
                if not l3_plus or measurements["authoritative_axes"] == ["x", "y", "z"]
                else "BLOCKED",
                f"axes: {', '.join(measurements['authoritative_axes']) or 'none'}",
            ),
            stage(
                "camera_ensemble",
                "COMPLETE" if camera_complete else "PENDING",
                (
                    f"{counts['camera_solutions']} stored solutions; "
                    f"approved={camera.get('approved', False)}"
                ),
            ),
            stage(
                "candidate_portfolio",
                "COMPLETE" if portfolio["evaluated_hypothesis_count"] else "PENDING",
                (
                    f"{portfolio['evaluated_hypothesis_count']}/"
                    f"{portfolio['hypothesis_count']} evaluated"
                ),
            ),
            stage(
                "semantic_model",
                "COMPLETE"
                if counts["semantic_nodes"]
                and not acceptance["metrics"].get("semantic_twin", {}).get("pending_node_ids")
                else "PENDING",
                f"{counts['semantic_nodes']} semantic nodes",
            ),
            stage(
                "governed_render_and_compare",
                (
                    "COMPLETE"
                    if verified_runtime["mandatory_render_suite_complete"]
                    and verified_runtime["comparison_coverage_complete"]
                    else "PENDING"
                ),
                (
                    f"{verified_runtime['render_run_count']} verified render suites, "
                    f"{verified_runtime['comparison_count']} verified comparisons"
                ),
            ),
            stage(
                "candidate_transaction",
                "COMPLETE"
                if (transactions.get("authoritative") or {}).get("status") == "PASSED"
                else "PENDING",
                f"{counts['candidate_evaluations']} evaluations",
            ),
            stage(
                "missing_evidence",
                "BLOCKED"
                if pursuit.get("status") == "EVIDENCE_CEILING"
                else "PENDING"
                if pursuit.get("status") in {"RUNNING", "SOURCES_DISCOVERED"}
                else "COMPLETE"
                if not atlas["unresolved_regions"]
                else "PENDING",
                pursuit.get("status") or "no pursuit recorded",
            ),
            stage(
                "safe_promotion",
                "COMPLETE"
                if lifecycle.get("authoritative_promotion_chain_valid")
                else "PENDING",
                f"authoritative state: {lifecycle.get('authoritative_state') or 'none'}",
            ),
            stage(
                "delivery",
                "COMPLETE" if acceptance["accepted"] else "PENDING",
                (
                    f"exports: {', '.join(exports.get('formats', [])) or 'none'}; "
                    f"receipts: {counts['receipts']}"
                ),
            ),
        ]

    @staticmethod
    def _next_action(
        stages: list[dict[str, Any]],
        pursuit: dict[str, Any],
        campaign: dict[str, Any] | None,
        review_tasks: list[dict[str, Any]],
    ) -> dict[str, Any]:
        if pursuit.get("status") == "EVIDENCE_CEILING":
            return {
                "action": "provide_external_evidence",
                "reason": "public evidence pursuit reached a verified evidence ceiling",
                "capture_requests": pursuit["capture_requests"],
            }
        if campaign and campaign["status"] == "PAUSED":
            pause = next(
                (
                    event["payload"].get("reason")
                    for event in reversed(campaign["events"])
                    if event["payload"].get("event") == "paused"
                ),
                "campaign requires review",
            )
            if review_tasks:
                return {
                    "action": "complete_named_review",
                    "reason": pause,
                    "review_task_ids": [task["id"] for task in review_tasks],
                    "roles": sorted({task["role"] for task in review_tasks}),
                }
            return {"action": "resolve_campaign_pause", "reason": pause}
        pending = next(
            (item for item in stages if item["status"] in {"BLOCKED", "PENDING"}), None
        )
        return (
            {
                "action": f"advance_{pending['stage']}",
                "reason": pending["detail"],
            }
            if pending
            else {"action": "none", "reason": "all stages complete"}
        )

    @staticmethod
    def _campaign_summary(campaign: dict[str, Any] | None) -> dict[str, Any] | None:
        if not campaign:
            return None
        latest_progress = next(
            (
                event["payload"]
                for event in reversed(campaign["events"])
                if event["payload"].get("event") == "progress"
            ),
            None,
        )
        return {
            "id": campaign["id"],
            "kind": campaign["kind"],
            "status": campaign["status"],
            "controller_state": campaign["controller_state"],
            "iteration": campaign["iteration"],
            "latest_progress": latest_progress,
            "stop_reason": (campaign.get("result") or {}).get("stop_reason"),
        }

    @staticmethod
    def _human_summary(
        target: dict[str, Any] | None,
        sources: dict[str, Any],
        measurements: dict[str, Any],
        videos: dict[str, Any],
        atlas: dict[str, Any],
        portfolio: dict[str, Any],
        acceptance: dict[str, Any],
        next_action: dict[str, Any],
    ) -> list[str]:
        canonical = (target or {}).get("canonical", {})
        name = " ".join(
            str(canonical.get(key, "")).strip() for key in ("manufacturer", "model")
        ).strip()
        return [
            f"Target: {name or 'unresolved'} ({(target or {}).get('status', 'missing')}).",
            (
                f"Evidence: {sources['candidate_source_count']} candidates, "
                f"{sources['eligible_acquired_source_count']} eligible acquired after filtering."
            ),
            (
                f"Dimensions: {measurements['authoritative_overall_dimension_count']} "
                "authoritative overall records."
            ),
            (
                f"Video: {videos['extracted_frame_count']} frames, "
                f"{videos['selected_keyframe_count']} selected keyframes."
            ),
            f"Surface coverage: {round(atlas['coverage_fraction'] * 100.0, 2)}%.",
            (
                f"Portfolio: {portfolio['active_hypothesis_count']} active hypotheses; "
                f"{portfolio['evaluated_hypothesis_count']} evaluated."
            ),
            (
                f"Acceptance: {acceptance['project_status']}; accepted tier "
                f"{acceptance['accepted_fidelity'] or 'none'}."
            ),
            f"Next: {next_action['action']} — {next_action['reason']}.",
        ]
