from __future__ import annotations

import hashlib
import json
import math
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.cameras.state import validate_complete_camera_state
from blender_vision.core.errors import ProjectError
from blender_vision.core.util import atomic_write_json, canonical_json, utc_now
from blender_vision.geometry.scenes import SceneStore
from blender_vision.optimization.engine import OptimizationEngine
from blender_vision.orchestration.locality import LocalityPlanner
from blender_vision.parametric.store import ComponentStore
from blender_vision.projects.store import ProjectStore
from blender_vision.workflows.service import ReconstructionService


class MultiviewSearchStore:
    """Execute bounded semantic parameter variants as isolated fixed-camera candidates."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.components = ComponentStore(project)

    def start(
        self,
        component_id: str,
        *,
        semantic_ids: list[str],
        camera_solution_id: str,
        parameter_bounds: dict[str, list[float]],
        baseline_scene_id: str | None = None,
        maximum_dimension: int = 512,
        maximum_candidates: int = 17,
        maximum_attempts: int = 2,
    ) -> dict[str, Any]:
        if not semantic_ids or len(set(semantic_ids)) != len(semantic_ids):
            raise ValueError("multiview search requires unique semantic ids")
        if not 64 <= maximum_dimension <= 2048:
            raise ValueError("multiview search render dimension must be between 64 and 2048")
        if not 2 <= maximum_candidates <= 32:
            raise ValueError("multiview search candidate budget must be between 2 and 32")
        if not 1 <= maximum_attempts <= 3:
            raise ValueError("multiview search attempts must be between 1 and 3")
        component = self.components.get(component_id)
        bounds = self._validate_bounds(component, parameter_bounds)
        baseline_scene = SceneStore(self.project).get(baseline_scene_id)
        if not bool(baseline_scene["is_authoritative"]):
            raise ValueError("multiview search baseline must be the authoritative scene")
        camera = self._approved_camera(camera_solution_id)
        semantic_snapshots = self._semantic_snapshots(semantic_ids)
        component_snapshot = hashlib.sha256(canonical_json(component)).hexdigest()
        key_record = {
            "component_id": component_id,
            "component_snapshot_sha256": component_snapshot,
            "semantic_snapshots": semantic_snapshots,
            "camera_solution_id": camera_solution_id,
            "camera_snapshot_sha256": camera["snapshot_sha256"],
            "baseline_scene_id": baseline_scene["id"],
            "baseline_scene_digest": baseline_scene["artifact_digest"],
            "parameter_bounds": bounds,
            "maximum_dimension": maximum_dimension,
            "maximum_candidates": maximum_candidates,
            "maximum_attempts": maximum_attempts,
        }
        cache_key = hashlib.sha256(canonical_json(key_record)).hexdigest()
        with self.project.connection() as connection:
            existing = connection.execute(
                "SELECT id FROM multiview_search_runs WHERE cache_key=?", (cache_key,)
            ).fetchone()
        if existing:
            return self.get(existing["id"])
        plan = LocalityPlanner(self.project).plan(
            semantic_ids,
            change_kind="geometry",
            camera_solution_id=camera_solution_id,
        )
        if component_id not in set(plan["component_ids"]):
            raise ValueError("search component is not explicitly bound to the semantic target")
        if len(plan["reference_ids"]) < 2:
            raise ValueError("multiview search requires at least two affected camera views")
        candidates = self._candidate_parameters(component, bounds, maximum_candidates)
        if len(candidates) < 2:
            raise ValueError("multiview search bounds do not produce a non-baseline candidate")
        search_id = str(uuid.uuid4())
        now = utc_now()
        configuration = {
            **key_record,
            "component_record": component,
            "locality_plan_digest": plan["artifact"]["digest"],
            "candidate_generation": "baseline_plus_bounded_coordinate_extremes",
            "acceptance_performed": False,
        }
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            connection.execute(
                "INSERT INTO multiview_search_runs(id,cache_key,component_id,"
                "camera_solution_id,baseline_scene_id,status,semantic_ids_json,"
                "locality_plan_json,config_json,optimization_run_id,receipt_digest,"
                "created_at,updated_at) VALUES(?,?,?,?,?,'PLANNED',?,?,?,NULL,NULL,?,?)",
                (
                    search_id,
                    cache_key,
                    component_id,
                    camera_solution_id,
                    baseline_scene["id"],
                    json.dumps(sorted(semantic_ids)),
                    json.dumps(plan),
                    json.dumps(configuration),
                    now,
                    now,
                ),
            )
            for index, parameters in enumerate(candidates):
                connection.execute(
                    "INSERT INTO multiview_search_candidates(id,search_id,candidate_index,"
                    "status,parameters_json,comparison_ids_json,attempt_count,created_at,"
                    "updated_at) VALUES(?,?,?,'PLANNED',?,'[]',0,?,?)",
                    (
                        str(uuid.uuid4()),
                        search_id,
                        index,
                        json.dumps(parameters),
                        now,
                        now,
                    ),
                )
        return self.get(search_id)

    def execute(self, search_id: str) -> dict[str, Any]:
        search = self.get(search_id)
        if search["status"] == "COMPLETE":
            return search
        if search["status"] == "STALE":
            raise ProjectError("multiview search is stale; start a new snapshot-bound search")
        current = self.components.get(search["component_id"])
        if hashlib.sha256(canonical_json(current)).hexdigest() != search["configuration"][
            "component_snapshot_sha256"
        ]:
            self._set_run_status(search_id, "STALE")
            raise ProjectError("component changed after multiview search planning")
        self._set_run_status(search_id, "RUNNING")
        self._recover_interrupted(search_id)
        self._reject_failed_attempt_scenes(search_id)
        maximum_attempts = int(search["configuration"]["maximum_attempts"])
        for candidate in self.get(search_id)["candidates"]:
            if candidate["status"] == "EVALUATED":
                continue
            if candidate["attempt_count"] >= maximum_attempts:
                continue
            claimed = self._claim_candidate(candidate["id"])
            if not claimed:
                continue
            try:
                result = self._evaluate_candidate(
                    self.get(search_id), self._candidate(candidate["id"])
                )
            except Exception as error:
                self._fail_candidate(candidate["id"], error)
                self._reject_failed_attempt_scenes(search_id)
            else:
                self._complete_candidate(candidate["id"], result)
        search = self.get(search_id)
        if any(
            item["status"] != "EVALUATED"
            and item["attempt_count"] < maximum_attempts
            for item in search["candidates"]
        ):
            self._set_run_status(search_id, "RUNNING")
            return self.get(search_id)
        evaluated = [item for item in search["candidates"] if item["status"] == "EVALUATED"]
        baseline = next(
            (item for item in evaluated if item["candidate_index"] == 0), None
        )
        if baseline is None or len(evaluated) < 2:
            exhausted = all(
                item["status"] == "EVALUATED" or item["attempt_count"] >= maximum_attempts
                for item in search["candidates"]
            )
            self._set_run_status(search_id, "FAILED" if exhausted else "RUNNING")
            return self.get(search_id)
        optimization_run_id = search.get("optimization_run_id")
        if optimization_run_id:
            optimization = OptimizationEngine(self.project).get(optimization_run_id)
        else:
            candidates = [self._optimization_candidate(search, item) for item in evaluated]
            optimization = OptimizationEngine(self.project).propose_multiview(
                search["component_id"],
                semantic_ids=search["semantic_ids"],
                camera_solution_id=search["camera_solution_id"],
                candidates=candidates,
                method="bounded_candidate_search",
                hierarchy_stage="component_geometry",
            )
            with self.project.connection() as connection:
                connection.execute(
                    "UPDATE multiview_search_runs SET optimization_run_id=?,updated_at=? "
                    "WHERE id=? AND optimization_run_id IS NULL",
                    (optimization["id"], utc_now(), search_id),
                )
            optimization = OptimizationEngine(self.project).get(optimization["id"])
        return self._finalize(search_id, optimization)

    def get(self, search_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM multiview_search_runs WHERE id=?", (search_id,)
            ).fetchone()
            candidate_rows = connection.execute(
                "SELECT * FROM multiview_search_candidates WHERE search_id=? "
                "ORDER BY candidate_index,id",
                (search_id,),
            ).fetchall()
        if row is None:
            raise KeyError(f"unknown multiview search: {search_id}")
        value = dict(row)
        value["semantic_ids"] = json.loads(value.pop("semantic_ids_json"))
        value["locality_plan"] = json.loads(value.pop("locality_plan_json"))
        value["configuration"] = json.loads(value.pop("config_json"))
        value["candidates"] = [self._decode_candidate(item) for item in candidate_rows]
        return value

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            ids = [
                row[0]
                for row in connection.execute(
                    "SELECT id FROM multiview_search_runs ORDER BY created_at,id"
                )
            ]
        return [self.get(item) for item in ids]

    def _evaluate_candidate(
        self, search: dict[str, Any], candidate: dict[str, Any]
    ) -> dict[str, Any]:
        component = dict(search["configuration"]["component_record"])
        component["parameters"] = {
            **component["parameters"],
            **candidate["parameters"],
        }
        token = f"multiview-{search['id']}-{candidate['candidate_index']}"
        service = ReconstructionService(self.project)
        generated = service.generate_component_variant(
            component,
            scene_id=search["baseline_scene_id"],
            job_id=token,
        )
        self._record_candidate_scene(candidate["id"], generated["generated_scene"]["id"])
        plan = search["locality_plan"]
        rendered = service.render_views(
            scene_id=generated["generated_scene"]["id"],
            solution_id=search["camera_solution_id"],
            job_id=f"{token}-render",
            maximum_dimension=int(search["configuration"]["maximum_dimension"]),
            reference_ids=plan["reference_ids"],
            requested_passes=plan["requested_passes"],
            regions_by_reference=plan["regions_by_reference"],
        )
        comparisons = service.compare_views(rendered["renders"])["comparisons"]
        if {item["reference_id"] for item in comparisons} != set(plan["reference_ids"]):
            raise RuntimeError("candidate comparison coverage differs from its locality plan")
        return {
            "scene_id": generated["generated_scene"]["id"],
            "render_run_id": rendered["id"],
            "comparison_ids": sorted(item["id"] for item in comparisons),
        }

    def _optimization_candidate(
        self, search: dict[str, Any], candidate: dict[str, Any]
    ) -> dict[str, Any]:
        comparisons = OptimizationEngine(self.project)._validated_comparisons(
            candidate["comparison_ids"]
        )
        silhouette = 1.0 - sum(
            float(item["metrics"]["silhouette_iou"]) for item in comparisons
        ) / len(comparisons)
        baseline_parameters = search["candidates"][0]["parameters"]
        complexity = sum(
            abs(float(value) - float(baseline_parameters[name]))
            / max(abs(float(baseline_parameters[name])), 1.0)
            for name, value in candidate["parameters"].items()
        ) / max(1, len(candidate["parameters"]))
        return {
            "parameters": candidate["parameters"],
            "terms": {
                "silhouette": round(silhouette, 8),
                "complexity": round(complexity, 8),
            },
            "baseline": candidate["candidate_index"] == 0,
            "diagnostics": {
                "comparison_ids": candidate["comparison_ids"],
                "search_id": search["id"],
                "candidate_id": candidate["id"],
                "scene_id": candidate["scene_id"],
                "render_run_id": candidate["render_run_id"],
            },
        }

    def _finalize(self, search_id: str, optimization: dict[str, Any]) -> dict[str, Any]:
        search = self.get(search_id)
        if search.get("receipt_digest"):
            self._set_run_status(search_id, "COMPLETE")
            return self.get(search_id)
        created_at = utc_now()
        scene_dispositions = self._scene_dispositions(search, optimization)
        report = {
            "schema_version": 1,
            "receipt_type": "multiview_parameter_search",
            "id": search_id,
            "component_id": search["component_id"],
            "component_snapshot_sha256": search["configuration"][
                "component_snapshot_sha256"
            ],
            "camera_solution_id": search["camera_solution_id"],
            "camera_snapshot_sha256": search["configuration"]["camera_snapshot_sha256"],
            "baseline_scene_id": search["baseline_scene_id"],
            "baseline_scene_digest": search["configuration"]["baseline_scene_digest"],
            "locality_plan_digest": search["configuration"]["locality_plan_digest"],
            "candidate_count": len(search["candidates"]),
            "evaluated_candidate_count": sum(
                item["status"] == "EVALUATED" for item in search["candidates"]
            ),
            "candidates": search["candidates"],
            "scene_dispositions": scene_dispositions,
            "optimization_run_id": optimization["id"],
            "optimization_proposal_digest": optimization["artifact_digest"],
            "acceptance_performed": False,
            "created_at": created_at,
        }
        relative = Path("receipts") / f"multiview-search-{search_id}.json"
        atomic_write_json(self.project.root / relative, report)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.multiview-search+json",
        )
        with self.project.connection() as connection:
            updated = connection.execute(
                "UPDATE multiview_search_runs SET status='COMPLETE',receipt_digest=?,"
                "updated_at=? WHERE id=? AND optimization_run_id=? "
                "AND receipt_digest IS NULL",
                (artifact.digest, created_at, search_id, optimization["id"]),
            )
            if updated.rowcount != 1:
                current = connection.execute(
                    "SELECT receipt_digest FROM multiview_search_runs WHERE id=?", (search_id,)
                ).fetchone()
                if current is None or current["receipt_digest"] is None:
                    raise RuntimeError("multiview search finalization raced with another writer")
        return {**self.get(search_id), "receipt": report, "path": str(relative)}

    def _scene_dispositions(
        self, search: dict[str, Any], optimization: dict[str, Any]
    ) -> list[dict[str, Any]]:
        best_index = optimization["result"]["best_candidate_index"]
        best_evaluation = next(
            item for item in optimization["evaluations"] if item["index"] == best_index
        )
        selected_id = best_evaluation["diagnostics"].get("candidate_id")
        if not selected_id:
            raise RuntimeError("multiview optimization does not identify its selected candidate")
        scenes = SceneStore(self.project)
        dispositions = []
        for candidate in search["candidates"]:
            if candidate["status"] != "EVALUATED":
                dispositions.append(
                    {
                        "candidate_id": candidate["id"],
                        "scene_id": candidate.get("scene_id"),
                        "disposition": "evaluation_failed",
                        "transition_receipt_digest": None,
                    }
                )
                continue
            if candidate["id"] == selected_id:
                dispositions.append(
                    {
                        "candidate_id": candidate["id"],
                        "scene_id": candidate["scene_id"],
                        "disposition": "selected_for_transactional_review",
                        "transition_receipt_digest": None,
                    }
                )
                continue
            scene = scenes.get(candidate["scene_id"])
            if scene["state"] == "CANDIDATE":
                transition = scenes.transition(
                    scene["id"],
                    "REJECTED",
                    reviewer="VisionMCP multiview search policy",
                    reason=(
                        "Bounded fixed-camera search selected a lower-loss candidate; "
                        "this isolated variant was not selected for review."
                    ),
                )
                transition_digest = transition["receipt"]["digest"]
            elif scene["state"] == "REJECTED":
                with self.project.connection() as connection:
                    row = connection.execute(
                        "SELECT receipt_digest FROM scene_transitions WHERE scene_id=? "
                        "AND to_state='REJECTED' ORDER BY created_at DESC,id DESC LIMIT 1",
                        (scene["id"],),
                    ).fetchone()
                if row is None:
                    raise RuntimeError("rejected search scene has no transition receipt")
                transition_digest = row["receipt_digest"]
            else:
                raise RuntimeError(
                    "nonselected multiview search scene has an invalid lifecycle state"
                )
            dispositions.append(
                {
                    "candidate_id": candidate["id"],
                    "scene_id": candidate["scene_id"],
                    "disposition": "rejected_nonselected",
                    "transition_receipt_digest": transition_digest,
                }
            )
        return dispositions

    def _approved_camera(self, solution_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT approved,solution_json FROM camera_solutions WHERE id=?", (solution_id,)
            ).fetchone()
        if row is None or not bool(row["approved"]):
            raise ValueError("multiview search requires an approved camera solution")
        document = json.loads(row["solution_json"])
        for camera in document.get("cameras", []):
            validate_complete_camera_state(camera)
        if not document.get("cameras"):
            raise ValueError("approved camera solution contains no cameras")
        return {
            "document": document,
            "snapshot_sha256": hashlib.sha256(canonical_json(document)).hexdigest(),
        }

    def _semantic_snapshots(self, semantic_ids: list[str]) -> dict[str, str]:
        placeholders = ",".join("?" for _ in semantic_ids)
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT id,record_json FROM semantic_nodes WHERE id IN (" + placeholders + ")",
                semantic_ids,
            ).fetchall()
        if len(rows) != len(semantic_ids):
            raise KeyError("multiview search references unknown semantic nodes")
        return {
            row["id"]: hashlib.sha256(canonical_json(json.loads(row["record_json"]))).hexdigest()
            for row in rows
        }

    @staticmethod
    def _validate_bounds(
        component: dict[str, Any], parameter_bounds: dict[str, list[float]]
    ) -> dict[str, list[float]]:
        if not parameter_bounds or len(parameter_bounds) > 8:
            raise ValueError("multiview search requires bounds for one to eight parameters")
        normalized: dict[str, list[float]] = {}
        for name, raw in sorted(parameter_bounds.items()):
            current = component["parameters"].get(name)
            if not isinstance(current, (int, float)) or not math.isfinite(float(current)):
                raise ValueError(f"search parameter is not a finite scalar: {name}")
            if (
                not isinstance(raw, list)
                or len(raw) != 2
                or any(not isinstance(item, (int, float)) for item in raw)
            ):
                raise ValueError(f"parameter bounds must be [minimum, maximum]: {name}")
            lower, upper = map(float, raw)
            if not math.isfinite(lower) or not math.isfinite(upper) or lower > upper:
                raise ValueError(f"parameter bounds are invalid: {name}")
            if not lower <= float(current) <= upper:
                raise ValueError(f"parameter bounds exclude the authoritative baseline: {name}")
            normalized[name] = [lower, upper]
        return normalized

    @staticmethod
    def _candidate_parameters(
        component: dict[str, Any], bounds: dict[str, list[float]], maximum: int
    ) -> list[dict[str, float]]:
        baseline = {name: float(component["parameters"][name]) for name in bounds}
        values = [baseline]
        for name, (lower, upper) in bounds.items():
            for candidate_value in (lower, upper):
                candidate = dict(baseline)
                candidate[name] = candidate_value
                if candidate not in values:
                    values.append(candidate)
                if len(values) >= maximum:
                    return values
        return values

    def _recover_interrupted(self, search_id: str) -> None:
        now = utc_now()
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT id,error_json,scene_id FROM multiview_search_candidates "
                "WHERE search_id=? AND status='EVALUATING'",
                (search_id,),
            ).fetchall()
            for row in rows:
                history = json.loads(row["error_json"]) if row["error_json"] else []
                history.append(
                    {
                        "error": "InterruptedError: prior worker did not finish",
                        "at": now,
                        "scene_id": row["scene_id"],
                    }
                )
                connection.execute(
                    "UPDATE multiview_search_candidates SET status='FAILED',error_json=?,"
                    "updated_at=? WHERE id=?",
                    (json.dumps(history), now, row["id"]),
                )

    def _reject_failed_attempt_scenes(self, search_id: str) -> None:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT id,scene_id FROM multiview_search_candidates "
                "WHERE search_id=? AND status='FAILED' AND scene_id IS NOT NULL",
                (search_id,),
            ).fetchall()
        scenes = SceneStore(self.project)
        for row in rows:
            scene = scenes.get(row["scene_id"])
            if scene["state"] == "CANDIDATE":
                scenes.transition(
                    scene["id"],
                    "REJECTED",
                    reviewer="VisionMCP multiview retry policy",
                    reason=(
                        "This isolated candidate attempt failed before complete multiview "
                        "evaluation and cannot remain eligible."
                    ),
                )
            elif scene["state"] != "REJECTED":
                raise RuntimeError("failed multiview attempt has an invalid lifecycle state")
            with self.project.connection() as connection:
                connection.execute(
                    "UPDATE multiview_search_candidates SET scene_id=NULL,render_run_id=NULL,"
                    "comparison_ids_json='[]',updated_at=? "
                    "WHERE id=? AND status='FAILED' AND scene_id=?",
                    (utc_now(), row["id"], row["scene_id"]),
                )

    def _record_candidate_scene(self, candidate_id: str, scene_id: str) -> None:
        with self.project.connection() as connection:
            updated = connection.execute(
                "UPDATE multiview_search_candidates SET scene_id=?,updated_at=? "
                "WHERE id=? AND status='EVALUATING'",
                (scene_id, utc_now(), candidate_id),
            )
        if updated.rowcount != 1:
            raise RuntimeError("multiview candidate scene registration raced with another worker")

    def _claim_candidate(self, candidate_id: str) -> bool:
        with self.project.connection() as connection:
            updated = connection.execute(
                "UPDATE multiview_search_candidates SET status='EVALUATING',"
                "attempt_count=attempt_count+1,updated_at=? "
                "WHERE id=? AND status IN ('PLANNED','FAILED')",
                (utc_now(), candidate_id),
            )
        return updated.rowcount == 1

    def _complete_candidate(self, candidate_id: str, result: dict[str, Any]) -> None:
        with self.project.connection() as connection:
            updated = connection.execute(
                "UPDATE multiview_search_candidates SET status='EVALUATED',scene_id=?,"
                "render_run_id=?,comparison_ids_json=?,updated_at=? "
                "WHERE id=? AND status='EVALUATING'",
                (
                    result["scene_id"],
                    result["render_run_id"],
                    json.dumps(result["comparison_ids"]),
                    utc_now(),
                    candidate_id,
                ),
            )
            if updated.rowcount != 1:
                raise RuntimeError("multiview candidate completion raced with another writer")

    def _fail_candidate(self, candidate_id: str, error: Exception) -> None:
        now = utc_now()
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT error_json,scene_id FROM multiview_search_candidates WHERE id=?",
                (candidate_id,),
            ).fetchone()
            history = json.loads(row["error_json"]) if row and row["error_json"] else []
            history.append(
                {
                    "error": f"{type(error).__name__}: {error}",
                    "at": now,
                    "scene_id": row["scene_id"] if row else None,
                }
            )
            connection.execute(
                "UPDATE multiview_search_candidates SET status='FAILED',error_json=?,"
                "updated_at=? WHERE id=? AND status='EVALUATING'",
                (json.dumps(history), now, candidate_id),
            )

    def _set_run_status(self, search_id: str, status: str) -> None:
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE multiview_search_runs SET status=?,updated_at=? WHERE id=?",
                (status, utc_now(), search_id),
            )

    def _candidate(self, candidate_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM multiview_search_candidates WHERE id=?", (candidate_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown multiview search candidate: {candidate_id}")
        return self._decode_candidate(row)

    @staticmethod
    def _decode_candidate(row: Any) -> dict[str, Any]:
        value = dict(row)
        value["parameters"] = json.loads(value.pop("parameters_json"))
        value["comparison_ids"] = json.loads(value.pop("comparison_ids_json"))
        error = value.pop("error_json")
        value["errors"] = json.loads(error) if error else []
        return value
