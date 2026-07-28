from __future__ import annotations

import hashlib
import json
import math
import uuid
from collections import Counter
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, canonical_json, utc_now
from blender_vision.perception.query import ObservationQueryService
from blender_vision.projects.store import ProjectStore

SPECIALISTS = (
    "Pixel Analyst",
    "Layout Geometer",
    "Typography Analyst",
    "Motion Analyst",
    "Interaction Analyst",
    "Accessibility Analyst",
    "Design-System Analyst",
    "Code-Binding Analyst",
    "Camera Analyst",
    "Geometry Analyst",
    "Material and Lighting Analyst",
    "Graphics Runtime Analyst",
    "Performance Analyst",
    "Source/Rights Analyst",
    "Adversarial Reviewer",
    "Acceptance Auditor",
)

_SPECIALIST_GRAPHS = {
    "Pixel Analyst": {
        "LayoutGraph",
        "ImageGraph",
        "DesktopExperienceGraph",
        "VideoNarrativeGraph",
        "GraphicsFrameGraph",
    },
    "Layout Geometer": {"LayoutGraph", "ResponsiveGraph", "DesktopExperienceGraph"},
    "Typography Analyst": {"LayoutGraph", "ImageGraph", "DesignSystemGraph"},
    "Motion Analyst": {"MotionGraph", "VideoNarrativeGraph", "GraphicsFrameGraph"},
    "Interaction Analyst": {"InteractionGraph", "StateGraph"},
    "Accessibility Analyst": {
        "LayoutGraph",
        "InteractionGraph",
        "DesktopExperienceGraph",
    },
    "Design-System Analyst": {"DesignSystemGraph", "LayoutGraph", "CodeGraph"},
    "Code-Binding Analyst": {"CodeGraph", "LayoutGraph", "GraphicsFrameGraph"},
    "Camera Analyst": {"GraphicsFrameGraph", "VideoNarrativeGraph"},
    "Geometry Analyst": {"GraphicsFrameGraph", "VideoNarrativeGraph", "ImageGraph"},
    "Material and Lighting Analyst": {
        "GraphicsFrameGraph",
        "DesignSystemGraph",
        "ImageGraph",
    },
    "Graphics Runtime Analyst": {"GraphicsFrameGraph"},
    "Performance Analyst": {"LayoutGraph", "MotionGraph", "GraphicsFrameGraph"},
    "Source/Rights Analyst": set(),
    "Adversarial Reviewer": set(),
    "Acceptance Auditor": set(),
}
_COMPUTE_COST = {
    specialist: round(0.5 + index * 0.05, 2)
    for index, specialist in enumerate(SPECIALISTS)
}
_ALWAYS = {"Source/Rights Analyst", "Adversarial Reviewer", "Acceptance Auditor"}


class PerceptionWorkspace:
    """Persistent, evidence-bound specialist findings and deterministic routing."""

    router_version = "deterministic-v1"

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.query = ObservationQueryService(project)

    def run(
        self,
        capture_ids: list[str],
        *,
        compute_budget: float = 8.0,
    ) -> dict[str, Any]:
        captures = sorted(set(capture_ids))
        if not captures:
            raise ValueError("perception workspace requires at least one capture")
        if not math.isfinite(compute_budget) or compute_budget < 1.0:
            raise ValueError("compute budget must be finite and at least one")
        graph_records = self._graphs(captures)
        capture_rows = self._capture_rows(captures)
        identity = {
            "capture_ids": captures,
            "graph_digests": sorted(
                record["citation"]["artifact_digest"] for record in graph_records
            ),
            "router_version": self.router_version,
            "compute_budget": compute_budget,
        }
        workspace_id = hashlib.sha256(canonical_json(identity)).hexdigest()
        existing = self.get(workspace_id, required=False)
        if existing is not None:
            existing["reused"] = True
            return existing
        graph_types = {record["graph_type"] for record in graph_records}
        route = self._route(graph_types, compute_budget)
        now = utc_now()
        task_records = []
        findings = []
        for specialist in route["selected_specialists"]:
            task_id = hashlib.sha256(
                canonical_json({"workspace_id": workspace_id, "specialist": specialist})
            ).hexdigest()
            finding = self._finding(
                workspace_id,
                task_id,
                specialist,
                graph_records,
                capture_rows,
            )
            artifact = self._artifact(
                f"finding-{finding['id']}.json",
                finding,
                "application/vnd.visionmcp.perception-finding+json",
            )
            finding["artifact_digest"] = artifact.digest
            findings.append(finding)
            task_records.append(
                {
                    "id": task_id,
                    "workspace_id": workspace_id,
                    "specialist": specialist,
                    "status": "COMPLETED",
                    "compute_units": _COMPUTE_COST[specialist],
                    "request": {
                        "capture_ids": captures,
                        "graph_types": sorted(graph_types),
                    },
                    "created_at": now,
                    "updated_at": now,
                }
            )
        contradictions = self._contradictions(
            workspace_id, graph_records, findings
        )
        for contradiction in contradictions:
            artifact = self._artifact(
                f"contradiction-{contradiction['id']}.json",
                contradiction,
                "application/vnd.visionmcp.perception-contradiction+json",
            )
            contradiction["artifact_digest"] = artifact.digest
        next_actions = sorted(
            (
                {
                    "specialist": item["specialist"],
                    "action": item["proposed_next_action"],
                    "predicted_information_gain": item[
                        "predicted_information_gain"
                    ],
                    "missing_observations": item["missing_observations"],
                }
                for item in findings
                if item["missing_observations"]
            ),
            key=lambda item: (
                -item["predicted_information_gain"],
                item["specialist"],
            ),
        )
        report = {
            "schema": "vision.perception-workspace/v1",
            "id": workspace_id,
            "capture_ids": captures,
            "status": "COMPLETE",
            "router": route,
            "tasks": task_records,
            "findings": findings,
            "contradictions": contradictions,
            "compute": {
                "budget_units": compute_budget,
                "used_units": round(
                    sum(task["compute_units"] for task in task_records), 3
                ),
                "task_count": len(task_records),
            },
            "next_highest_information_action": next_actions[0] if next_actions else None,
            "created_at": now,
            "updated_at": now,
        }
        report_artifact = self._artifact(
            f"workspace-{workspace_id}.json",
            report,
            "application/vnd.visionmcp.perception-workspace+json",
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            connection.execute(
                "INSERT INTO perception_workspace_runs("
                "id,capture_ids_json,status,router_json,summary_digest,created_at,updated_at"
                ") VALUES(?,?,?,?,?,?,?)",
                (
                    workspace_id,
                    json.dumps(captures),
                    "COMPLETE",
                    json.dumps(route),
                    report_artifact.digest,
                    now,
                    now,
                ),
            )
            for task in task_records:
                connection.execute(
                    "INSERT INTO perception_specialist_tasks("
                    "id,workspace_id,specialist,status,compute_units,request_json,"
                    "created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)",
                    (
                        task["id"],
                        workspace_id,
                        task["specialist"],
                        task["status"],
                        task["compute_units"],
                        json.dumps(task["request"]),
                        now,
                        now,
                    ),
                )
            for finding in findings:
                connection.execute(
                    "INSERT INTO perception_findings("
                    "id,workspace_id,task_id,specialist,kind,authority,confidence,"
                    "evidence_json,finding_json,artifact_digest,created_at"
                    ") VALUES(?,?,?,?,?,?,?,?,?,?,?)",
                    (
                        finding["id"],
                        workspace_id,
                        finding["task_id"],
                        finding["specialist"],
                        finding["kind"],
                        finding["authority"],
                        finding["confidence"],
                        json.dumps(finding["evidence_references"]),
                        json.dumps(finding),
                        finding["artifact_digest"],
                        now,
                    ),
                )
            for contradiction in contradictions:
                connection.execute(
                    "INSERT INTO perception_contradictions("
                    "id,workspace_id,kind,status,record_json,artifact_digest,created_at"
                    ") VALUES(?,?,?,?,?,?,?)",
                    (
                        contradiction["id"],
                        workspace_id,
                        contradiction["kind"],
                        contradiction["status"],
                        json.dumps(contradiction),
                        contradiction["artifact_digest"],
                        now,
                    ),
                )
            example_id = str(uuid.uuid4())
            connection.execute(
                "INSERT INTO perception_router_examples("
                "id,workspace_id,features_json,selected_specialists_json,outcome_json,"
                "created_at) VALUES(?,?,?,?,?,?)",
                (
                    example_id,
                    workspace_id,
                    json.dumps({"graph_types": sorted(graph_types)}),
                    json.dumps(route["selected_specialists"]),
                    json.dumps(
                        {
                            "finding_count": len(findings),
                            "contradiction_count": len(contradictions),
                            "compute_units": report["compute"]["used_units"],
                        }
                    ),
                    now,
                ),
            )
        report["artifact_digest"] = report_artifact.digest
        report["reused"] = False
        return report

    def benchmark_router(
        self,
        cases: list[dict[str, Any]],
        *,
        maximum_specialists: int = 4,
    ) -> dict[str, Any]:
        if not cases:
            raise ValueError("router benchmark requires fixed cases")
        maximum_specialists = max(1, min(maximum_specialists, len(SPECIALISTS)))
        normalized = []
        for index, case in enumerate(cases):
            required = sorted(set(case.get("required_specialists", [])))
            if not required or not set(required).issubset(SPECIALISTS):
                raise ValueError("benchmark cases require known specialist labels")
            normalized.append(
                {
                    "id": str(case.get("id", index)),
                    "graph_types": sorted(set(case.get("graph_types", []))),
                    "required_specialists": required,
                }
            )
        dataset_digest = hashlib.sha256(canonical_json(normalized)).hexdigest()
        with self.project.connection() as connection:
            existing = connection.execute(
                "SELECT report_json FROM perception_router_benchmarks "
                "WHERE dataset_digest=? ORDER BY created_at DESC LIMIT 1",
                (dataset_digest,),
            ).fetchone()
        if existing:
            return json.loads(existing["report_json"])
        frequencies = self._learned_frequencies()
        global_required = Counter(
            specialist
            for case in normalized
            for specialist in case["required_specialists"]
        )
        best_single = (
            sorted(global_required, key=lambda item: (-global_required[item], item))[0]
        )
        strategies = {
            "deterministic": [
                self._route(
                    set(case["graph_types"]),
                    sum(sorted(_COMPUTE_COST.values())[:maximum_specialists]),
                    maximum_specialists=maximum_specialists,
                )["selected_specialists"]
                for case in normalized
            ],
            "learned_candidate": [
                self._learned_route(
                    set(case["graph_types"]), frequencies, maximum_specialists
                )
                for case in normalized
            ],
            "best_single": [[best_single] for _case in normalized],
            "uniform_ensemble": [
                list(SPECIALISTS[:maximum_specialists]) for _case in normalized
            ],
        }
        scores = {
            name: self._score_routes(normalized, routes)
            for name, routes in strategies.items()
        }
        learned = scores["learned_candidate"]["mean_recall"]
        accepted = (
            learned > scores["best_single"]["mean_recall"]
            and learned > scores["uniform_ensemble"]["mean_recall"]
            and learned > scores["deterministic"]["mean_recall"]
            and scores["learned_candidate"]["regression_case_count"] == 0
        )
        status = "ACTIVATED" if accepted else "REFUTED"
        active = "learned-v1" if accepted else self.router_version
        now = utc_now()
        report = {
            "schema": "vision.router-benchmark/v1",
            "dataset_digest": dataset_digest,
            "case_count": len(normalized),
            "matched_compute": {
                "maximum_specialists_per_case": maximum_specialists,
                "scores_computed_by_service": True,
                "caller_supplied_scores_trusted": False,
            },
            "strategies": strategies,
            "scores": scores,
            "status": status,
            "active_router": active,
            "reason": (
                "learned candidate strictly improved all baselines without a case regression"
                if accepted
                else "learned candidate did not strictly beat deterministic, best-single, "
                "and uniform baselines; deterministic routing remains active"
            ),
            "created_at": now,
        }
        artifact = self._artifact(
            f"router-benchmark-{dataset_digest}.json",
            report,
            "application/vnd.visionmcp.router-benchmark+json",
        )
        benchmark_id = str(uuid.uuid4())
        report["id"] = benchmark_id
        report["report_digest"] = artifact.digest
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO perception_router_benchmarks("
                "id,dataset_digest,status,active_router,report_json,report_digest,created_at"
                ") VALUES(?,?,?,?,?,?,?)",
                (
                    benchmark_id,
                    dataset_digest,
                    status,
                    active,
                    json.dumps(report),
                    artifact.digest,
                    now,
                ),
            )
        return report

    def get(
        self, workspace_id: str, *, required: bool = True
    ) -> dict[str, Any] | None:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT summary_digest FROM perception_workspace_runs WHERE id=?",
                (workspace_id,),
            ).fetchone()
        if row is None:
            if required:
                raise KeyError(f"unknown perception workspace: {workspace_id}")
            return None
        value = json.loads(
            self.artifacts.path_for(row["summary_digest"]).read_text(encoding="utf-8")
        )
        value["artifact_digest"] = row["summary_digest"]
        return value

    def progress(self) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT id FROM perception_workspace_runs ORDER BY created_at DESC,id LIMIT 1"
            ).fetchone()
            benchmark = connection.execute(
                "SELECT report_json FROM perception_router_benchmarks "
                "ORDER BY created_at DESC,id LIMIT 1"
            ).fetchone()
        return {
            "latest_workspace": self.get(row["id"]) if row else None,
            "latest_router_benchmark": (
                json.loads(benchmark["report_json"]) if benchmark else None
            ),
        }

    def _graphs(self, capture_ids: list[str]) -> list[dict[str, Any]]:
        records = []
        for capture_id in capture_ids:
            graph_types = self.query.graph_types(capture_id)
            if not graph_types:
                raise ValueError(f"capture has no perceptual graphs: {capture_id}")
            for graph_type in graph_types:
                graph = self.query.graph(capture_id, graph_type)
                graph["_workspace_capture_id"] = capture_id
                records.append(graph)
        return records

    def _capture_rows(self, capture_ids: list[str]) -> list[dict[str, Any]]:
        placeholders = ",".join("?" for _ in capture_ids)
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT id,adapter,rights_decision,authority,manifest_digest,status "
                f"FROM observation_captures WHERE id IN ({placeholders}) ORDER BY id",
                capture_ids,
            ).fetchall()
        if len(rows) != len(capture_ids):
            raise KeyError("one or more workspace capture ids are unknown")
        return [dict(row) for row in rows]

    def _route(
        self,
        graph_types: set[str],
        compute_budget: float,
        *,
        maximum_specialists: int | None = None,
    ) -> dict[str, Any]:
        candidates = []
        for specialist in SPECIALISTS:
            relevant = _SPECIALIST_GRAPHS[specialist] & graph_types
            if specialist not in _ALWAYS and not relevant:
                continue
            information_gain = (
                len(relevant) + (0.35 if specialist in _ALWAYS else 0.0)
            ) / _COMPUTE_COST[specialist]
            candidates.append((information_gain, specialist, sorted(relevant)))
        candidates.sort(key=lambda item: (-item[0], item[1]))
        selected = []
        used = 0.0
        for _gain, specialist, _relevant in candidates:
            if maximum_specialists is not None and len(selected) >= maximum_specialists:
                break
            cost = _COMPUTE_COST[specialist]
            if used + cost > compute_budget and selected:
                continue
            selected.append(specialist)
            used += cost
        return {
            "name": self.router_version,
            "selected_specialists": selected,
            "candidate_scores": [
                {
                    "specialist": specialist,
                    "predicted_information_gain": round(gain, 6),
                    "relevant_graph_types": relevant,
                    "compute_units": _COMPUTE_COST[specialist],
                }
                for gain, specialist, relevant in candidates
            ],
            "compute_budget": compute_budget,
            "predicted_compute_units": round(used, 3),
        }

    def _finding(
        self,
        workspace_id: str,
        task_id: str,
        specialist: str,
        graphs: list[dict[str, Any]],
        capture_rows: list[dict[str, Any]],
    ) -> dict[str, Any]:
        relevant = [
            graph
            for graph in graphs
            if graph["graph_type"] in _SPECIALIST_GRAPHS[specialist]
        ]
        if specialist in _ALWAYS:
            relevant = graphs
        evidence = []
        for graph in relevant:
            evidence.append(graph["citation"])
            for node in graph.get("nodes", [])[:3]:
                evidence.extend(node.get("evidence_references", []))
        evidence = _unique_dicts(evidence)
        missing = sorted(
            _SPECIALIST_GRAPHS[specialist]
            - {graph["graph_type"] for graph in relevant}
        )
        metrics = self._specialist_metrics(specialist, relevant, capture_rows)
        derived = sum(
            node.get("authority") in {"DERIVED", "INFERRED", "HYPOTHESIS"}
            for graph in relevant
            for node in graph.get("nodes", [])
        )
        total = sum(len(graph.get("nodes", [])) for graph in relevant)
        confidence = round(1.0 - min(0.45, derived / max(1, total) * 0.35), 4)
        authority = (
            "OBSERVED"
            if relevant
            and all(graph.get("authority") == "OBSERVED" for graph in relevant)
            else "MIXED"
        )
        next_action = (
            f"observe missing {missing[0]} evidence"
            if missing
            else "query the highest-impact cited node before proposing a change"
        )
        finding_id = hashlib.sha256(
            canonical_json(
                {
                    "workspace_id": workspace_id,
                    "specialist": specialist,
                    "metrics": metrics,
                    "evidence": evidence,
                }
            )
        ).hexdigest()
        return {
            "schema": "vision.perception-finding/v1",
            "id": finding_id,
            "workspace_id": workspace_id,
            "task_id": task_id,
            "specialist": specialist,
            "kind": "EVIDENCE_ANALYSIS",
            "findings": metrics,
            "evidence_references": evidence,
            "confidence": confidence,
            "authority": authority,
            "contradictions": [],
            "missing_observations": missing,
            "proposed_next_action": next_action,
            "predicted_information_gain": round(
                len(missing) / (1.0 + _COMPUTE_COST[specialist]), 6
            ),
            "compute_units": _COMPUTE_COST[specialist],
            "created_at": utc_now(),
        }

    @staticmethod
    def _specialist_metrics(
        specialist: str,
        graphs: list[dict[str, Any]],
        captures: list[dict[str, Any]],
    ) -> dict[str, Any]:
        nodes = [node for graph in graphs for node in graph.get("nodes", [])]
        edges = [edge for graph in graphs for edge in graph.get("edges", [])]
        domain_counts = Counter(node.get("domain_type", "unknown") for node in nodes)
        base: dict[str, Any] = {
            "graph_types": sorted({graph["graph_type"] for graph in graphs}),
            "node_count": len(nodes),
            "edge_count": len(edges),
            "domain_counts": dict(sorted(domain_counts.items())),
        }
        if specialist == "Source/Rights Analyst":
            base["rights_decisions"] = sorted(
                {row["rights_decision"] for row in captures}
            )
            base["capture_authorities"] = sorted(
                {row["authority"] for row in captures}
            )
        elif specialist == "Adversarial Reviewer":
            base["uncertain_node_count"] = sum(
                bool(node.get("uncertainty")) for node in nodes
            )
            base["hypothesis_node_count"] = sum(
                node.get("authority") == "HYPOTHESIS" for node in nodes
            )
        elif specialist == "Acceptance Auditor":
            base["complete_capture_count"] = sum(
                row["status"] == "COMPLETE" for row in captures
            )
            base["manifest_bound_count"] = sum(
                bool(row["manifest_digest"]) for row in captures
            )
        elif specialist == "Motion Analyst":
            base["motion_sample_count"] = sum(
                len(graph.get("camera_motion", []))
                + len(graph.get("timelines", []))
                for graph in graphs
            )
        elif specialist == "Code-Binding Analyst":
            base["runtime_binding_count"] = sum(
                len(graph.get("runtime_bindings", [])) for graph in graphs
            )
        elif specialist == "Design-System Analyst":
            base["token_count"] = domain_counts.get("DesignToken", 0)
            base["component_count"] = domain_counts.get("Component", 0)
        elif specialist == "Accessibility Analyst":
            base["accessibility_node_count"] = domain_counts.get(
                "AccessibilityNode", 0
            ) + sum(bool(node.get("role")) for node in nodes)
        elif specialist in {"Camera Analyst", "Geometry Analyst"}:
            base["depth_unavailable_count"] = sum(
                graph.get("depth", {}).get("status") == "UNAVAILABLE"
                for graph in graphs
            )
        return base

    def _contradictions(
        self,
        workspace_id: str,
        graphs: list[dict[str, Any]],
        findings: list[dict[str, Any]],
    ) -> list[dict[str, Any]]:
        del findings
        records = []
        for graph in graphs:
            if graph["graph_type"] == "DesktopExperienceGraph":
                ax_ids = {
                    node["id"]
                    for node in graph.get("nodes", [])
                    if node.get("domain_type") == "AccessibilityNode"
                }
                bound = {
                    edge["target"]
                    for edge in graph.get("edges", [])
                    if edge.get("type") == "CORRESPONDS_TO"
                }
                if ax_ids - bound:
                    records.append(
                        self._contradiction(
                            workspace_id,
                            "UNBOUND_ACCESSIBILITY_SEMANTICS",
                            "OPEN",
                            "Accessibility nodes lack synchronized visual-region correspondences.",
                            graph["citation"],
                            sorted(ax_ids - bound),
                            "capture clearer pixels or reviewed accessibility bounds",
                        )
                    )
            if graph["graph_type"] == "CodeGraph":
                hypotheses = [
                    item
                    for item in graph.get("runtime_bindings", [])
                    if item["authority"] == "HYPOTHESIS"
                ]
                if hypotheses:
                    records.append(
                        self._contradiction(
                            workspace_id,
                            "UNRESOLVED_RUNTIME_BINDING",
                            "OPEN",
                            "A claimed runtime binding has no matching observed source node.",
                            graph["citation"],
                            [item["id"] for item in hypotheses],
                            "capture source-map or framework instrumentation evidence",
                        )
                    )
        with self.project.connection() as connection:
            drift = connection.execute(
                "SELECT id,report_digest FROM design_drift_runs "
                "WHERE status='DRIFT_DETECTED' ORDER BY created_at"
            ).fetchall()
            regressions = connection.execute(
                "SELECT id,report_digest FROM frontend_global_gate_runs "
                "WHERE status!='PASS' ORDER BY created_at"
            ).fetchall()
        for row in drift:
            records.append(
                self._contradiction(
                    workspace_id,
                    "DESIGN_IMPLEMENTATION_DRIFT",
                    "OPEN",
                    "Observed design and implementation tokens or variants disagree.",
                    {"artifact_digest": row["report_digest"], "run_id": row["id"]},
                    [],
                    "inspect the design drift report before editing",
                )
            )
        for row in regressions:
            records.append(
                self._contradiction(
                    workspace_id,
                    "LOCAL_GAIN_GLOBAL_REGRESSION",
                    "REJECTED",
                    "A local visual candidate failed the mandatory global gate.",
                    {"artifact_digest": row["report_digest"], "run_id": row["id"]},
                    [],
                    "retain the failed candidate and generate a confined alternative",
                )
            )
        return records

    @staticmethod
    def _contradiction(
        workspace_id: str,
        kind: str,
        status: str,
        claim: str,
        evidence: dict[str, Any],
        affected_nodes: list[str],
        next_action: str,
    ) -> dict[str, Any]:
        identity = {
            "workspace_id": workspace_id,
            "kind": kind,
            "evidence": evidence,
            "affected_nodes": affected_nodes,
        }
        return {
            "schema": "vision.perception-contradiction/v1",
            "id": hashlib.sha256(canonical_json(identity)).hexdigest(),
            "workspace_id": workspace_id,
            "kind": kind,
            "status": status,
            "claim": claim,
            "evidence_references": [evidence],
            "affected_nodes": affected_nodes,
            "authority": "DERIVED",
            "confidence": 1.0,
            "next_action": next_action,
            "created_at": utc_now(),
        }

    def _learned_frequencies(self) -> dict[str, Counter[str]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT features_json,selected_specialists_json "
                "FROM perception_router_examples ORDER BY created_at,id"
            ).fetchall()
        result: dict[str, Counter[str]] = {}
        for row in rows:
            features = json.loads(row["features_json"])
            for graph_type in features["graph_types"]:
                result.setdefault(graph_type, Counter()).update(
                    json.loads(row["selected_specialists_json"])
                )
        return result

    @staticmethod
    def _learned_route(
        graph_types: set[str],
        frequencies: dict[str, Counter[str]],
        limit: int,
    ) -> list[str]:
        scores: Counter[str] = Counter()
        for graph_type in graph_types:
            scores.update(frequencies.get(graph_type, {}))
        if not scores:
            return list(SPECIALISTS[:limit])
        return sorted(scores, key=lambda item: (-scores[item], item))[:limit]

    @staticmethod
    def _score_routes(
        cases: list[dict[str, Any]], routes: list[list[str]]
    ) -> dict[str, Any]:
        recalls = []
        regressions = 0
        case_scores = []
        for case, route in zip(cases, routes, strict=True):
            required = set(case["required_specialists"])
            selected = set(route)
            recall = len(required & selected) / len(required)
            recalls.append(recall)
            if recall < 1.0:
                regressions += 1
            case_scores.append(
                {
                    "case_id": case["id"],
                    "recall": recall,
                    "selected_count": len(route),
                }
            )
        return {
            "mean_recall": sum(recalls) / len(recalls),
            "regression_case_count": regressions,
            "case_scores": case_scores,
        }

    def _artifact(self, name: str, payload: dict[str, Any], media_type: str):
        relative = Path("observations") / "workspace" / name
        atomic_write_json(self.project.root / relative, payload)
        return self.artifacts.ingest_file(
            self.project.root / relative, media_type=media_type
        )


def _unique_dicts(values: list[dict[str, Any]]) -> list[dict[str, Any]]:
    result = []
    seen = set()
    for value in values:
        key = canonical_json(value)
        if key in seen:
            continue
        seen.add(key)
        result.append(value)
    return result
