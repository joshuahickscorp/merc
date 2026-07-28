from __future__ import annotations

import json
import math
import uuid
from typing import Any

from blender_vision.core.util import utc_now
from blender_vision.intelligence.packs import CategoryPackRegistry
from blender_vision.orchestration.resources import discover_resources
from blender_vision.projects.store import ProjectStore

LANES = {
    "existing_model_repair": {"editable": True, "generative": False},
    "parametric_category_model": {"editable": True, "generative": False},
    "classical_photogrammetry": {"editable": False, "generative": False},
    "learned_multiview_geometry": {"editable": False, "generative": False},
    "generative_image_to_3d": {"editable": False, "generative": True},
    "visual_hull": {"editable": False, "generative": False},
    "gaussian_visual_oracle": {"editable": False, "generative": True},
    "hybrid_semantic_reconstruction": {"editable": True, "generative": False},
}


class ReconstructionPortfolioStore:
    def __init__(self, project: ProjectStore):
        self.project = project

    def generate(
        self,
        *,
        category: str = "general_product",
        lanes: list[str] | None = None,
        resource_profile: str = "standard",
        backend_configuration: dict[str, dict[str, Any]] | None = None,
    ) -> dict[str, Any]:
        CategoryPackRegistry().get(category)
        if resource_profile == "auto":
            resource_profile = discover_resources()["selected_profile"]
        if resource_profile not in {"compact", "standard", "beast", "distributed_beast"}:
            raise ValueError("unknown resource profile")
        selected = lanes or list(LANES)
        unknown = sorted(set(selected) - set(LANES))
        if unknown:
            raise ValueError(f"unknown reconstruction lanes: {', '.join(unknown)}")
        backend_configuration = dict(backend_configuration or {})
        unknown_backend_lanes = sorted(set(backend_configuration) - set(selected))
        if unknown_backend_lanes:
            raise ValueError(
                "backend configuration references unselected lanes: "
                + ", ".join(unknown_backend_lanes)
            )
        if any(not isinstance(value, dict) for value in backend_configuration.values()):
            raise ValueError("portfolio backend configurations must be objects")
        portfolio_id = str(uuid.uuid4())
        now = utc_now()
        configuration = {
            "resource_profile": resource_profile,
            "workflow": "cheap_initial_lanes_then_high_resolution_fitting",
            "backends": backend_configuration,
        }
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO reconstruction_portfolios(id,category,configuration_json,status,"
                "created_at,updated_at) VALUES(?,?,?,?,?,?)",
                (portfolio_id, category, json.dumps(configuration), "ACTIVE", now, now),
            )
        candidates = []
        for lane in selected:
            candidate_id = str(uuid.uuid4())
            record = {
                "id": candidate_id,
                "portfolio_id": portfolio_id,
                "lane": lane,
                "status": "PROPOSED",
                **LANES[lane],
                "metrics": {},
                "artifacts": [],
                "scene_id": None,
                "geometry_run_id": None,
                "evidence_authority": "hypothesis" if LANES[lane]["generative"] else "pending",
                "acceptance_eligible": not LANES[lane]["generative"],
                "created_at": now,
                "updated_at": now,
            }
            with self.project.connection() as connection:
                connection.execute(
                    "INSERT INTO reconstruction_candidates(id,portfolio_id,lane,status,"
                    "record_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?)",
                    (candidate_id, portfolio_id, lane, "PROPOSED", json.dumps(record), now, now),
                )
            candidates.append(record)
        return {
            "id": portfolio_id,
            "category": category,
            "configuration": configuration,
            "status": "ACTIVE",
            "candidates": candidates,
        }

    def record_result(
        self,
        candidate_id: str,
        *,
        metrics: dict[str, float],
        artifacts: list[str] | None = None,
        scene_id: str | None = None,
        geometry_run_id: str | None = None,
        evidence_authority: str | None = None,
        acceptance_eligible: bool | None = None,
    ) -> dict[str, Any]:
        candidate = self.get_candidate(candidate_id)
        if not metrics or any(
            not isinstance(value, (int, float)) or not math.isfinite(float(value))
            for value in metrics.values()
        ):
            raise ValueError("portfolio metrics must be finite numeric values")
        with self.project.connection() as connection:
            known_artifacts = {
                row[0] for row in connection.execute("SELECT digest FROM artifacts")
            }
            known_scene = (
                connection.execute("SELECT id FROM scene_assets WHERE id=?", (scene_id,)).fetchone()
                if scene_id
                else None
            )
            known_geometry_run = (
                connection.execute(
                    "SELECT id FROM geometry_runs WHERE id=?", (geometry_run_id,)
                ).fetchone()
                if geometry_run_id
                else None
            )
        if not set(artifacts or []).issubset(known_artifacts):
            raise ValueError("portfolio result references unregistered artifacts")
        if scene_id and known_scene is None:
            raise ValueError("portfolio result references an unknown scene")
        if geometry_run_id and known_geometry_run is None:
            raise ValueError("portfolio result references an unknown geometry run")
        candidate.update(
            {
                "status": "EVALUATED",
                "metrics": metrics,
                "artifacts": sorted(set(artifacts or [])),
                "scene_id": scene_id,
                "geometry_run_id": geometry_run_id,
                "updated_at": utc_now(),
            }
        )
        if evidence_authority is not None:
            if not evidence_authority.strip():
                raise ValueError("portfolio evidence authority cannot be blank")
            candidate["evidence_authority"] = evidence_authority.strip()
        if acceptance_eligible is not None:
            candidate["acceptance_eligible"] = bool(acceptance_eligible)
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE reconstruction_candidates SET status='EVALUATED',record_json=?,"
                "updated_at=? WHERE id=?",
                (json.dumps(candidate), candidate["updated_at"], candidate_id),
            )
        return candidate

    def record_unavailable(
        self,
        candidate_id: str,
        *,
        status: str,
        reason: str,
        requirements: list[str] | None = None,
    ) -> dict[str, Any]:
        if status not in {"BLOCKED_BACKEND", "NOT_APPLICABLE"}:
            raise ValueError("portfolio unavailable status is invalid")
        if not reason.strip():
            raise ValueError("portfolio unavailable result requires a reason")
        candidate = self.get_candidate(candidate_id)
        candidate.update(
            {
                "status": status,
                "unavailable": {
                    "reason": reason.strip(),
                    "requirements": sorted(set(requirements or [])),
                },
                "updated_at": utc_now(),
            }
        )
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE reconstruction_candidates SET status=?,record_json=?,updated_at=? "
                "WHERE id=?",
                (status, json.dumps(candidate), candidate["updated_at"], candidate_id),
            )
        return candidate

    def record_progress(
        self,
        candidate_id: str,
        *,
        status: str,
        reason: str,
        metrics: dict[str, float],
        artifacts: list[str] | None = None,
        geometry_run_id: str | None = None,
    ) -> dict[str, Any]:
        if status != "EVIDENCE_READY":
            raise ValueError("portfolio progress status is invalid")
        if not reason.strip():
            raise ValueError("portfolio progress requires a reason")
        candidate = self.get_candidate(candidate_id)
        with self.project.connection() as connection:
            known_artifacts = {
                row[0] for row in connection.execute("SELECT digest FROM artifacts")
            }
        if not set(artifacts or []).issubset(known_artifacts):
            raise ValueError("portfolio progress references unregistered artifacts")
        candidate.update(
            {
                "status": status,
                "metrics": metrics,
                "artifacts": sorted(set(artifacts or [])),
                "geometry_run_id": geometry_run_id,
                "progress_reason": reason.strip(),
                "updated_at": utc_now(),
            }
        )
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE reconstruction_candidates SET status=?,record_json=?,updated_at=? "
                "WHERE id=?",
                (status, json.dumps(candidate), candidate["updated_at"], candidate_id),
            )
        return candidate

    def rank(self, portfolio_id: str) -> dict[str, Any]:
        candidates = self.list_candidates(portfolio_id)
        for candidate in candidates:
            metrics = candidate.get("metrics", {})
            candidate["portfolio_score"] = round(
                float(metrics.get("silhouette", 0.0)) * 0.4
                + float(metrics.get("camera", 0.0)) * 0.2
                + float(metrics.get("coverage", 0.0)) * 0.2
                + float(metrics.get("semantic_editability", int(candidate["editable"]))) * 0.2,
                8,
            )
        ranked = sorted(
            candidates,
            key=lambda item: (item["status"] == "EVALUATED", item["portfolio_score"]),
            reverse=True,
        )
        evaluated = [item for item in ranked if item["status"] == "EVALUATED"]
        pareto_ids = [
            candidate["id"]
            for candidate in evaluated
            if not any(
                other["id"] != candidate["id"]
                and self._dominates(other.get("metrics", {}), candidate.get("metrics", {}))
                for other in evaluated
            )
        ]
        return {
            "portfolio_id": portfolio_id,
            "ranked": ranked,
            "selected_editable_candidate_id": next(
                (
                    item["id"]
                    for item in ranked
                    if item["editable"] and item["status"] == "EVALUATED"
                ),
                None,
            ),
            "pareto_candidate_ids": pareto_ids,
            "policy": (
                "generative candidates are proposals and never independently acceptance eligible"
            ),
        }

    def fusion_plan(self, portfolio_id: str) -> dict[str, Any]:
        ranked = self.rank(portfolio_id)["ranked"]
        evaluated = [item for item in ranked if item["status"] == "EVALUATED"]
        return {
            "portfolio_id": portfolio_id,
            "camera_source_candidate_id": self._best(evaluated, "camera"),
            "coarse_topology_candidate_id": self._best(evaluated, "silhouette"),
            "appearance_candidate_id": self._best(evaluated, "appearance"),
            "editable_target_candidate_id": next(
                (item["id"] for item in evaluated if item["editable"]), None
            ),
            "generated_inputs_labeled_as_inference": all(
                item.get("acceptance_eligible") is False
                and item.get("evidence_authority")
                in {"hypothesis", "synthetic_hypothesis", "appearance_oracle_only"}
                for item in evaluated
                if item["generative"]
            ),
        }

    def get_candidate(self, candidate_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT record_json FROM reconstruction_candidates WHERE id=?", (candidate_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown reconstruction candidate: {candidate_id}")
        return json.loads(row["record_json"])

    def list_candidates(self, portfolio_id: str) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT record_json FROM reconstruction_candidates WHERE portfolio_id=? "
                "ORDER BY created_at,id",
                (portfolio_id,),
            ).fetchall()
        return [json.loads(row["record_json"]) for row in rows]

    @staticmethod
    def _best(candidates: list[dict[str, Any]], metric: str) -> str | None:
        return (
            max(candidates, key=lambda item: float(item.get("metrics", {}).get(metric, 0.0)))["id"]
            if candidates
            else None
        )

    @staticmethod
    def _dominates(first: dict[str, Any], second: dict[str, Any]) -> bool:
        keys = set(first) & set(second)
        if not keys:
            return False
        return all(float(first[key]) >= float(second[key]) for key in keys) and any(
            float(first[key]) > float(second[key]) for key in keys
        )
