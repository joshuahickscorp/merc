from __future__ import annotations

import hashlib
import json
import math
import uuid
from dataclasses import asdict
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.cameras.state import validate_complete_camera_state
from blender_vision.core.errors import ProjectError
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.optimization.losses import LossTerms, LossWeights, weighted_loss
from blender_vision.orchestration.locality import LocalityPlanner
from blender_vision.parametric.store import ComponentStore
from blender_vision.projects.store import ProjectStore

METHODS = {
    "analytical": {"closed_form", "least_squares", "robust_scalar_fit"},
    "black_box": {
        "bounded_candidate_search",
        "cma_es",
        "bayesian_optimization",
        "nelder_mead",
        "coordinate_search",
        "population_search",
    },
    "differentiable": {"gradient_descent", "adam", "lbfgs"},
    "discrete": {"enumeration", "beam_search", "branch_and_bound"},
    "learned_proposal": {"model_proposal", "few_shot_adapter"},
}

HIERARCHY = (
    "overall_scale",
    "major_silhouette",
    "component_placement",
    "component_geometry",
    "panel_and_seam_details",
    "materials",
    "microdetail",
)


class OptimizationEngine:
    """Persist multi-objective hypotheses; mutation always requires named review."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.components = ComponentStore(project)

    def propose(
        self,
        component_id: str,
        *,
        tier: str,
        method: str,
        candidates: list[dict[str, Any]],
        weights: dict[str, float] | None = None,
        configuration: dict[str, Any] | None = None,
        evidence_binding_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        if tier not in METHODS:
            raise ValueError("unknown optimization tier")
        if method not in METHODS[tier]:
            raise ValueError(f"method {method} is not valid for {tier} optimization")
        component = self.components.get(component_id)
        if not candidates:
            raise ValueError("optimization requires at least one evaluated candidate")
        bindings = evidence_binding_ids or []
        self._validate_bindings(bindings)
        config = configuration or {}
        stage = config.get("hierarchy_stage")
        if stage is not None and stage not in HIERARCHY:
            raise ValueError("unknown hierarchical fitting stage")
        if tier == "differentiable":
            required = {"renderer", "gradient_trace_digest", "camera_solution_id"}
            if not required.issubset(config):
                raise ValueError("differentiable optimization requires renderer and gradient trace")
            self._validate_bindings([str(config["gradient_trace_digest"])], allow_artifacts=True)
        selected_weights = LossWeights(**(weights or {}))
        evaluations = [
            self._candidate(component, candidate, selected_weights, index)
            for index, candidate in enumerate(candidates)
        ]
        best = min(evaluations, key=lambda item: (item["total_loss"], item["index"]))
        baseline = next((item for item in evaluations if item["baseline"]), None)
        result = {
            "best_candidate_index": best["index"],
            "best_parameters": best["parameters"],
            "best_total_loss": best["total_loss"],
            "baseline_total_loss": baseline["total_loss"] if baseline else None,
            "expected_improvement": (
                baseline["total_loss"] - best["total_loss"] if baseline else None
            ),
            "authority": (
                "inferred proposal only"
                if tier == "learned_proposal"
                else "evaluated proposal awaiting named review"
            ),
            "component_revision": component["revision"],
            "component_snapshot_sha256": hashlib.sha256(
                canonical_json(component)
            ).hexdigest(),
            "review": {"state": "pending", "reviewer": None, "reason": None},
        }
        run_id = str(uuid.uuid4())
        created_at = utc_now()
        record = {
            "schema_version": 1,
            "id": run_id,
            "component_id": component_id,
            "tier": tier,
            "method": method,
            "status": "proposed",
            "configuration": {
                **config,
                "weights": asdict(selected_weights),
                "evidence_binding_ids": bindings,
            },
            "evaluations": evaluations,
            "result": result,
            "created_at": created_at,
        }
        relative = Path("geometry") / "optimization" / f"optimization-{run_id}.json"
        atomic_write_json(self.project.root / relative, record)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.optimization-run+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO optimization_runs("
                "id,component_id,tier,method,status,config_json,evaluations_json,result_json,"
                "record_digest,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
                (
                    run_id,
                    component_id,
                    tier,
                    method,
                    "proposed",
                    json.dumps(record["configuration"]),
                    json.dumps(evaluations),
                    json.dumps(result),
                    artifact.digest,
                    created_at,
                ),
            )
        return {**record, "artifact": artifact.to_dict(), "path": str(relative)}

    def propose_multiview(
        self,
        component_id: str,
        *,
        semantic_ids: list[str],
        camera_solution_id: str,
        candidates: list[dict[str, Any]],
        method: str = "bounded_candidate_search",
        weights: dict[str, float] | None = None,
        hierarchy_stage: str = "component_geometry",
    ) -> dict[str, Any]:
        """Bind candidate silhouette losses to fixed-camera, component-local comparisons."""
        if len(semantic_ids) == 0 or len(set(semantic_ids)) != len(semantic_ids):
            raise ValueError("multiview fitting requires unique semantic component ids")
        with self.project.connection() as connection:
            camera_row = connection.execute(
                "SELECT approved,solution_json FROM camera_solutions WHERE id=?",
                (camera_solution_id,),
            ).fetchone()
        if camera_row is None or not bool(camera_row["approved"]):
            raise ValueError("multiview fitting requires an approved immutable camera solution")
        camera_document = json.loads(camera_row["solution_json"])
        if not camera_document.get("cameras"):
            raise ValueError("multiview fitting camera solution contains no cameras")
        for camera in camera_document["cameras"]:
            validate_complete_camera_state(camera)
        plan = LocalityPlanner(self.project).plan(
            semantic_ids,
            change_kind="geometry",
            camera_solution_id=camera_solution_id,
        )
        if len(plan["reference_ids"]) < 2:
            raise ValueError("multiview fitting requires at least two component-relevant views")
        if component_id not in set(plan["component_ids"]):
            raise ValueError(
                "multiview component must be explicitly bound to the selected semantic nodes"
            )
        planned_references = set(plan["reference_ids"])
        validated_candidates: list[dict[str, Any]] = []
        all_comparison_ids: set[str] = set()
        for candidate in candidates:
            diagnostics = dict(candidate.get("diagnostics") or {})
            comparison_ids = diagnostics.get("comparison_ids")
            if (
                not isinstance(comparison_ids, list)
                or len(set(comparison_ids)) != len(comparison_ids)
                or len(comparison_ids) < 2
            ):
                raise ValueError(
                    "each multiview candidate requires at least two unique comparison ids"
                )
            comparisons = self._validated_comparisons(comparison_ids)
            comparison_references = {item["reference_id"] for item in comparisons}
            if comparison_references != planned_references:
                raise ValueError(
                    "multiview candidate comparisons must cover every locality-plan reference"
                )
            metrics = [item["metrics"] for item in comparisons]
            if any("silhouette_iou" not in item for item in metrics):
                raise ValueError("multiview comparisons require silhouette IoU metrics")
            silhouette_loss = 1.0 - sum(
                float(item["silhouette_iou"]) for item in metrics
            ) / len(metrics)
            supplied_terms = candidate.get("terms") or {}
            supplied_silhouette = supplied_terms.get("silhouette")
            if not isinstance(supplied_silhouette, (int, float)) or not math.isclose(
                float(supplied_silhouette), silhouette_loss, rel_tol=0.0, abs_tol=1e-6
            ):
                raise ValueError(
                    "candidate silhouette loss does not match its stored multiview comparisons"
                )
            validated_candidates.append(
                {
                    **candidate,
                    "diagnostics": {
                        **diagnostics,
                        "comparison_ids": sorted(comparison_ids),
                        "reference_ids": sorted(comparison_references),
                        "locality_plan_digest": plan["artifact"]["digest"],
                        "recomputed_silhouette_loss": round(silhouette_loss, 8),
                    },
                }
            )
            all_comparison_ids.update(comparison_ids)
        proposal = self.propose(
            component_id,
            tier="black_box",
            method=method,
            candidates=validated_candidates,
            weights=weights,
            configuration={
                "hierarchy_stage": hierarchy_stage,
                "image_residual_fit": True,
                "semantic_ids": sorted(semantic_ids),
                "camera_solution_id": camera_solution_id,
                "reference_ids": plan["reference_ids"],
                "locality_plan_digest": plan["artifact"]["digest"],
            },
            evidence_binding_ids=sorted(all_comparison_ids),
        )
        return {**proposal, "locality_plan": plan}

    def review(self, run_id: str, *, accepted: bool, reviewer: str, reason: str) -> dict[str, Any]:
        if not reviewer.strip() or not reason.strip():
            raise ValueError("optimization review requires a named reviewer and reason")
        run = self.get(run_id)
        if run["status"] != "proposed":
            raise ProjectError(f"optimization run is not awaiting review: {run_id}")
        if not self._proposal_artifact_valid(run):
            raise ProjectError(
                "optimization proposal artifact is missing, corrupt, or inconsistent"
            )
        component = self.components.get(run["component_id"])
        current_snapshot = hashlib.sha256(canonical_json(component)).hexdigest()
        expected_snapshot = run["result"].get("component_snapshot_sha256")
        if accepted and (
            component["revision"] != run["result"]["component_revision"]
            or not expected_snapshot
            or current_snapshot != expected_snapshot
        ):
            raise ProjectError("component changed after optimization proposal")
        revision = int(component["revision"]) + 1 if accepted else None
        now = utc_now()
        result = dict(run["result"])
        result["review"] = {
            "state": "accepted" if accepted else "rejected",
            "reviewer": reviewer.strip(),
            "reason": reason.strip(),
            "reviewed_at": now,
            "applied_revision": revision,
        }
        status = "accepted" if accepted else "rejected"
        decision = {
            "schema_version": 1,
            "receipt_type": "optimization_review",
            "run_id": run_id,
            "proposal_digest": run["artifact_digest"],
            "component_id": run["component_id"],
            "component_snapshot_sha256": current_snapshot,
            "component_revision_before": component["revision"],
            "component_revision_after": revision,
            "best_parameters": run["result"]["best_parameters"],
            "best_total_loss": run["result"]["best_total_loss"],
            "baseline_total_loss": run["result"]["baseline_total_loss"],
            "decision": status,
            "reviewer": reviewer.strip(),
            "reason": reason.strip(),
            "created_at": now,
        }
        relative = Path("receipts") / f"optimization-review-{run_id}-{status}.json"
        atomic_write_json(self.project.root / relative, decision)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.optimization-review+json",
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            persisted_run = connection.execute(
                "SELECT status,record_digest FROM optimization_runs WHERE id=?", (run_id,)
            ).fetchone()
            if (
                persisted_run is None
                or persisted_run["status"] != "proposed"
                or persisted_run["record_digest"] != run["artifact_digest"]
            ):
                raise ProjectError(f"optimization review raced with another decision: {run_id}")
            component_row = connection.execute(
                "SELECT record_json,revision,created_at,updated_at FROM components WHERE id=?",
                (run["component_id"],),
            ).fetchone()
            if component_row is None:
                raise FileNotFoundError(f"unknown component: {run['component_id']}")
            component_record = json.loads(component_row["record_json"])
            persisted_component = {
                **component_record,
                "revision": component_row["revision"],
                "created_at": component_row["created_at"],
                "updated_at": component_row["updated_at"],
            }
            persisted_snapshot = hashlib.sha256(
                canonical_json(persisted_component)
            ).hexdigest()
            if persisted_snapshot != current_snapshot:
                raise ProjectError("component changed while optimization review was recorded")
            if accepted:
                if (
                    component_row["revision"] != run["result"]["component_revision"]
                    or persisted_snapshot != expected_snapshot
                ):
                    raise ProjectError("component changed after optimization proposal")
                component_record["parameters"].update(run["result"]["best_parameters"])
                updated = connection.execute(
                    "UPDATE components SET record_json=?,revision=?,updated_at=? "
                    "WHERE id=? AND revision=?",
                    (
                        json.dumps(component_record),
                        revision,
                        now,
                        run["component_id"],
                        component_row["revision"],
                    ),
                )
                if updated.rowcount != 1:
                    raise ProjectError("optimization update raced with another component revision")
            updated = connection.execute(
                "UPDATE optimization_runs SET status=?,result_json=?,decision_digest=? "
                "WHERE id=? AND status='proposed' AND decision_digest IS NULL",
                (status, json.dumps(result), artifact.digest, run_id),
            )
            if updated.rowcount != 1:
                raise ProjectError(f"optimization review raced with another decision: {run_id}")
        return {
            **self.get(run_id),
            "decision_receipt": decision,
            "decision_artifact": artifact.to_dict(),
            "decision_path": str(relative),
        }

    def get(self, run_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM optimization_runs WHERE id=?", (run_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown optimization run: {run_id}")
        value = dict(row)
        value["configuration"] = json.loads(value.pop("config_json"))
        value["evaluations"] = json.loads(value.pop("evaluations_json"))
        value["result"] = json.loads(value.pop("result_json"))
        value["artifact_digest"] = value.pop("record_digest")
        return value

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            ids = [
                row[0]
                for row in connection.execute(
                    "SELECT id FROM optimization_runs ORDER BY created_at"
                )
            ]
        return [self.get(run_id) for run_id in ids]

    def _proposal_artifact_valid(self, run: dict[str, Any]) -> bool:
        try:
            path = self.artifacts.path_for(run["artifact_digest"])
            if not path.is_file() or sha256_file(path)[0] != run["artifact_digest"]:
                return False
            proposal = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError, json.JSONDecodeError):
            return False
        return (
            proposal.get("id") == run["id"]
            and proposal.get("status") == "proposed"
            and proposal.get("configuration") == run["configuration"]
            and proposal.get("evaluations") == run["evaluations"]
            and proposal.get("result") == run["result"]
        )

    @staticmethod
    def _candidate(
        component: dict[str, Any],
        candidate: dict[str, Any],
        weights: LossWeights,
        index: int,
    ) -> dict[str, Any]:
        parameters = candidate.get("parameters")
        terms_value = candidate.get("terms")
        if not isinstance(parameters, dict) or not isinstance(terms_value, dict):
            raise ValueError("optimization candidates require parameters and loss terms")
        if not set(parameters).issubset(component["parameters"]):
            raise ValueError("optimization candidate contains unknown component parameters")
        if any(
            not isinstance(value, (int, float)) or not math.isfinite(float(value))
            for value in parameters.values()
        ):
            raise ValueError("optimization parameters must be finite numeric scalars")
        if not set(terms_value).issubset(LossTerms.__dataclass_fields__):
            raise ValueError("optimization candidate contains unknown loss terms")
        if any(
            not isinstance(value, (int, float))
            or not math.isfinite(float(value))
            or float(value) < 0
            for value in terms_value.values()
        ):
            raise ValueError("optimization loss terms must be finite and non-negative")
        terms = LossTerms(**{key: float(value) for key, value in terms_value.items()})
        return {
            "index": index,
            "parameters": {key: float(value) for key, value in parameters.items()},
            "terms": asdict(terms),
            "total_loss": weighted_loss(terms, weights),
            "baseline": bool(candidate.get("baseline", False)),
            "diagnostics": candidate.get("diagnostics", {}),
        }

    def _validate_bindings(self, bindings: list[str], *, allow_artifacts: bool = False) -> None:
        with self.project.connection() as connection:
            known = {
                row[0]
                for table in ("measurements", "features", "comparisons", "geometry_runs")
                for row in connection.execute(f"SELECT id FROM {table}").fetchall()
            }
            if allow_artifacts:
                known.update(row[0] for row in connection.execute("SELECT digest FROM artifacts"))
        if not set(bindings).issubset(known):
            raise ValueError("optimization references unknown evidence bindings")

    def _validated_comparisons(self, comparison_ids: list[str]) -> list[dict[str, Any]]:
        placeholders = ",".join("?" for _ in comparison_ids)
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM comparisons WHERE id IN (" + placeholders + ")",
                comparison_ids,
            ).fetchall()
        if len(rows) != len(comparison_ids):
            raise ValueError("multiview fitting references unknown comparisons")
        comparisons = []
        for row in rows:
            item = {**dict(row), "metrics": json.loads(row["metrics_json"])}
            for field in ("render_digest", "residual_digest"):
                digest = item.get(field)
                if not isinstance(digest, str):
                    raise ValueError("multiview comparison lacks governed render or residual")
                path = self.artifacts.path_for(digest)
                if not path.is_file() or sha256_file(path)[0] != digest:
                    raise ValueError("multiview comparison artifact is missing or corrupt")
            comparisons.append(item)
        return comparisons
