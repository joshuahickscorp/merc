from __future__ import annotations

import hashlib
import json
import tempfile
from pathlib import Path
from typing import Any

from PIL import Image, ImageChops, ImageStat

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import (
    atomic_write_text,
    canonical_json,
    sha256_file,
    utc_now,
)
from blender_vision.perception.query import ObservationQueryService
from blender_vision.projects.store import ProjectStore

_STYLE_KEYS = (
    "display",
    "position",
    "zIndex",
    "opacity",
    "transform",
    "color",
    "backgroundColor",
    "fontFamily",
    "fontSize",
    "fontWeight",
    "lineHeight",
    "borderRadius",
)


class FrontendComparisonService:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.query = ObservationQueryService(project)

    def compare(
        self,
        target_capture_id: str,
        candidate_capture_id: str,
        *,
        selectors: list[str] | None = None,
        thresholds: dict[str, float] | None = None,
    ) -> dict[str, Any]:
        target = self.query.graph(target_capture_id, "LayoutGraph")
        candidate = self.query.graph(candidate_capture_id, "LayoutGraph")
        target_nodes = {
            node["selector"]: node
            for node in target.get("nodes", [])
            if node.get("selector")
        }
        candidate_nodes = {
            node["selector"]: node
            for node in candidate.get("nodes", [])
            if node.get("selector")
        }
        scope = sorted(set(selectors or target_nodes.keys() | candidate_nodes.keys()))
        residuals = [
            self._node_residual(selector, target_nodes.get(selector), candidate_nodes.get(selector))
            for selector in scope
        ]
        geometry = self._mean(
            item["geometry"]["normalized_error"] for item in residuals
        )
        style = self._mean(item["style"]["mismatch_ratio"] for item in residuals)
        semantic = self._mean(item["semantic"]["mismatch_ratio"] for item in residuals)
        missing = self._mean(float(item["status"] != "MATCHED") for item in residuals)
        screenshot = self._screenshot_residual(target_capture_id, candidate_capture_id)
        score = (
            geometry * 0.40
            + style * 0.25
            + semantic * 0.15
            + missing * 0.15
            + (screenshot["normalized_rms"] if screenshot else 0.0) * 0.05
        )
        configured = {
            "maximum_score": 0.02,
            "maximum_geometry_error": 0.02,
            "maximum_style_mismatch": 0.0,
            "maximum_semantic_mismatch": 0.0,
            "maximum_missing_ratio": 0.0,
            **(thresholds or {}),
        }
        gates = {
            "score": score <= configured["maximum_score"],
            "geometry": geometry <= configured["maximum_geometry_error"],
            "style": style <= configured["maximum_style_mismatch"],
            "semantic": semantic <= configured["maximum_semantic_mismatch"],
            "missing": missing <= configured["maximum_missing_ratio"],
        }
        identity = {
            "target_capture_id": target_capture_id,
            "candidate_capture_id": candidate_capture_id,
            "scope": scope,
            "thresholds": configured,
        }
        comparison_id = hashlib.sha256(canonical_json(identity)).hexdigest()
        report = {
            "schema": "vision.frontend-comparison/v1",
            "id": comparison_id,
            "authority": "DERIVED",
            "target_capture_id": target_capture_id,
            "candidate_capture_id": candidate_capture_id,
            "scope": {"selectors": scope, "local": selectors is not None},
            "residuals": residuals,
            "metrics": {
                "geometry_error": geometry,
                "style_mismatch": style,
                "semantic_mismatch": semantic,
                "missing_ratio": missing,
                "screenshot": screenshot,
                "weighted_score": score,
            },
            "thresholds": configured,
            "gates": gates,
            "status": "PASS" if all(gates.values()) else "FAIL",
            "citations": [target["citation"], candidate["citation"]],
            "created_at": utc_now(),
        }
        record = self._ingest_json(report, "frontend-comparison")
        scope_json = canonical_json({"selectors": scope, "thresholds": configured}).decode()
        with self.project.connection() as connection:
            connection.execute(
                "INSERT OR REPLACE INTO perception_comparisons("
                "id,target_capture_id,candidate_capture_id,scope_json,status,score,"
                "report_digest,created_at) VALUES(?,?,?,?,?,?,?,?)",
                (
                    comparison_id,
                    target_capture_id,
                    candidate_capture_id,
                    scope_json,
                    report["status"],
                    score,
                    record["digest"],
                    report["created_at"],
                ),
            )
        return {**report, "report_digest": record["digest"]}

    @staticmethod
    def _node_residual(
        selector: str,
        target: dict[str, Any] | None,
        candidate: dict[str, Any] | None,
    ) -> dict[str, Any]:
        if target is None or candidate is None:
            return {
                "selector": selector,
                "status": "MISSING_TARGET" if target is None else "MISSING_CANDIDATE",
                "geometry": {"absolute": {}, "normalized_error": 1.0},
                "style": {"mismatches": [], "mismatch_ratio": 1.0},
                "semantic": {"mismatches": [], "mismatch_ratio": 1.0},
                "source_binding": {
                    "target": target.get("sourceBinding") if target else None,
                    "candidate": candidate.get("sourceBinding") if candidate else None,
                },
            }
        target_bounds = target.get("bounds") or {}
        candidate_bounds = candidate.get("bounds") or {}
        absolute = {
            key: abs(float(target_bounds.get(key, 0)) - float(candidate_bounds.get(key, 0)))
            for key in ("x", "y", "width", "height")
        }
        denominators = {
            "x": max(1.0, abs(float(target_bounds.get("x", 0))), 100.0),
            "y": max(1.0, abs(float(target_bounds.get("y", 0))), 100.0),
            "width": max(1.0, abs(float(target_bounds.get("width", 0)))),
            "height": max(1.0, abs(float(target_bounds.get("height", 0)))),
        }
        normalized = sum(
            min(1.0, absolute[key] / denominators[key]) for key in absolute
        ) / len(absolute)
        target_styles = target.get("styles", {})
        candidate_styles = candidate.get("styles", {})
        style_mismatches = [
            {
                "property": key,
                "target": target_styles.get(key),
                "candidate": candidate_styles.get(key),
            }
            for key in _STYLE_KEYS
            if target_styles.get(key) != candidate_styles.get(key)
        ]
        comparable_style_keys = {
            key
            for key in _STYLE_KEYS
            if key in target_styles or key in candidate_styles
        }
        semantic_mismatches = [
            {"property": key, "target": target.get(key), "candidate": candidate.get(key)}
            for key in ("role", "text", "accessibleName", "interactive")
            if target.get(key) != candidate.get(key)
        ]
        return {
            "selector": selector,
            "status": "MATCHED",
            "geometry": {"absolute": absolute, "normalized_error": normalized},
            "style": {
                "mismatches": style_mismatches,
                "mismatch_ratio": len(style_mismatches)
                / max(1, len(comparable_style_keys)),
            },
            "semantic": {
                "mismatches": semantic_mismatches,
                "mismatch_ratio": len(semantic_mismatches) / 4,
            },
            "source_binding": {
                "target": target.get("sourceBinding"),
                "candidate": candidate.get("sourceBinding"),
            },
        }

    def _screenshot_residual(
        self, target_capture_id: str, candidate_capture_id: str
    ) -> dict[str, Any] | None:
        target = self._artifact_for_role(target_capture_id, "screenshot.viewport")
        candidate = self._artifact_for_role(candidate_capture_id, "screenshot.viewport")
        if not target or not candidate:
            return None
        with Image.open(target) as left_image, Image.open(candidate) as right_image:
            left = left_image.convert("RGB")
            right = right_image.convert("RGB").resize(left.size)
            difference = ImageChops.difference(left, right)
            statistics = ImageStat.Stat(difference)
            rms = sum(statistics.rms) / len(statistics.rms)
        return {
            "normalized_rms": rms / 255.0,
            "target_digest": target.name,
            "candidate_digest": candidate.name,
        }

    def _artifact_for_role(self, capture_id: str, role: str) -> Path | None:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT artifact_digest FROM observation_capture_artifacts "
                "WHERE capture_id=? AND role=?",
                (capture_id, role),
            ).fetchone()
        return self.artifacts.path_for(row["artifact_digest"]) if row else None

    @staticmethod
    def _mean(values: Any) -> float:
        collected = list(values)
        return sum(collected) / len(collected) if collected else 0.0

    def _ingest_json(self, value: dict[str, Any], prefix: str) -> dict[str, Any]:
        staging = self.project.root / "observations" / ".staging"
        staging.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(
            prefix=f"{prefix}-", suffix=".json", dir=staging
        ) as file:
            file.write(canonical_json(value))
            file.flush()
            return self.artifacts.ingest_file(
                Path(file.name), media_type="application/json"
            ).to_dict()


class FrontendRepairService:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.comparison = FrontendComparisonService(project)

    def create_portfolio(
        self,
        target_capture_id: str,
        candidates: list[dict[str, Any]],
        *,
        locality_selectors: list[str],
        thresholds: dict[str, float] | None = None,
    ) -> dict[str, Any]:
        if not candidates or not locality_selectors:
            raise ValueError("portfolio requires candidates and a non-empty locality")
        configured = thresholds or {}
        identity = {
            "target_capture_id": target_capture_id,
            "candidates": candidates,
            "locality_selectors": sorted(set(locality_selectors)),
            "thresholds": configured,
        }
        portfolio_id = hashlib.sha256(canonical_json(identity)).hexdigest()
        now = utc_now()
        with self.project.connection() as connection:
            connection.execute(
                "INSERT OR IGNORE INTO frontend_candidate_portfolios("
                "id,target_capture_id,locality_json,thresholds_json,status,"
                "selected_candidate_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)",
                (
                    portfolio_id,
                    target_capture_id,
                    canonical_json(sorted(set(locality_selectors))).decode(),
                    canonical_json(configured).decode(),
                    "EVALUATING",
                    None,
                    now,
                    now,
                ),
            )
        evaluated = []
        for order, candidate in enumerate(candidates):
            capture_id = str(candidate["capture_id"])
            candidate_id = hashlib.sha256(
                canonical_json(
                    {
                        "portfolio_id": portfolio_id,
                        "capture_id": capture_id,
                        "parameters": candidate.get("parameters", {}),
                    }
                )
            ).hexdigest()
            comparison = self.comparison.compare(
                target_capture_id,
                capture_id,
                selectors=locality_selectors,
                thresholds=configured,
            )
            evaluated.append(
                {
                    "id": candidate_id,
                    "capture_id": capture_id,
                    "parameters": candidate.get("parameters", {}),
                    "comparison": comparison,
                    "score": comparison["metrics"]["weighted_score"],
                    "order": order,
                }
            )
        evaluated.sort(key=lambda item: (item["score"], item["order"], item["id"]))
        selected = evaluated[0]
        with self.project.connection() as connection:
            for rank, item in enumerate(evaluated, start=1):
                status = "SELECTED_LOCAL" if item["id"] == selected["id"] else "REJECTED_LOCAL"
                connection.execute(
                    "INSERT OR REPLACE INTO frontend_candidates("
                    "id,portfolio_id,capture_id,parameters_json,comparison_id,score,"
                    "rank,status,created_at) VALUES(?,?,?,?,?,?,?,?,?)",
                    (
                        item["id"],
                        portfolio_id,
                        item["capture_id"],
                        canonical_json(item["parameters"]).decode(),
                        item["comparison"]["id"],
                        item["score"],
                        rank,
                        status,
                        now,
                    ),
                )
            connection.execute(
                "UPDATE frontend_candidate_portfolios SET status='LOCAL_SELECTED',"
                "selected_candidate_id=?,updated_at=? WHERE id=?",
                (selected["id"], utc_now(), portfolio_id),
            )
        return {
            "id": portfolio_id,
            "target_capture_id": target_capture_id,
            "locality_selectors": locality_selectors,
            "status": "LOCAL_SELECTED",
            "selected_candidate_id": selected["id"],
            "candidates": evaluated,
        }

    def run_global_gate(self, portfolio_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            portfolio = connection.execute(
                "SELECT * FROM frontend_candidate_portfolios WHERE id=?",
                (portfolio_id,),
            ).fetchone()
            if portfolio is None:
                raise KeyError(f"unknown frontend portfolio: {portfolio_id}")
            candidate = connection.execute(
                "SELECT * FROM frontend_candidates WHERE id=?",
                (portfolio["selected_candidate_id"],),
            ).fetchone()
        if candidate is None:
            raise RuntimeError("portfolio has no selected local candidate")
        comparison = self.comparison.compare(
            portfolio["target_capture_id"],
            candidate["capture_id"],
            selectors=None,
            thresholds=json.loads(portfolio["thresholds_json"]),
        )
        status = "PASS" if comparison["status"] == "PASS" else "FAIL"
        gate_id = hashlib.sha256(
            canonical_json(
                {
                    "portfolio_id": portfolio_id,
                    "candidate_id": candidate["id"],
                    "comparison_id": comparison["id"],
                }
            )
        ).hexdigest()
        report = {
            "schema": "vision.frontend-global-gate/v1",
            "id": gate_id,
            "portfolio_id": portfolio_id,
            "candidate_id": candidate["id"],
            "comparison": comparison,
            "status": status,
            "atomic_decision": "ACCEPT_CANDIDATE" if status == "PASS" else "REJECT_CANDIDATE",
            "created_at": utc_now(),
        }
        record = self.comparison._ingest_json(report, "frontend-global-gate")
        with self.project.connection() as connection:
            connection.execute(
                "INSERT OR REPLACE INTO frontend_global_gate_runs("
                "id,portfolio_id,candidate_id,comparison_id,status,report_digest,"
                "created_at) VALUES(?,?,?,?,?,?,?)",
                (
                    gate_id,
                    portfolio_id,
                    candidate["id"],
                    comparison["id"],
                    status,
                    record["digest"],
                    report["created_at"],
                ),
            )
            connection.execute(
                "UPDATE frontend_candidates SET status=? WHERE id=?",
                (
                    "GLOBAL_PASSED" if status == "PASS" else "REJECTED_GLOBAL",
                    candidate["id"],
                ),
            )
            connection.execute(
                "UPDATE frontend_candidate_portfolios SET status=?,updated_at=? WHERE id=?",
                (
                    "GLOBAL_PASSED" if status == "PASS" else "GLOBAL_REJECTED",
                    utc_now(),
                    portfolio_id,
                ),
            )
        return {**report, "report_digest": record["digest"]}

    def propose_css_patch(
        self,
        target_capture_id: str,
        candidate_capture_id: str,
        *,
        target_file: str,
        selectors: list[str],
    ) -> dict[str, Any]:
        file_path = self._confined_target(target_file)
        if not file_path.is_file():
            raise FileNotFoundError(file_path)
        base_digest, _ = sha256_file(file_path)
        comparison = self.comparison.compare(
            target_capture_id,
            candidate_capture_id,
            selectors=selectors,
        )
        declarations: dict[str, dict[str, str]] = {}
        for residual in comparison["residuals"]:
            if residual["status"] != "MATCHED":
                continue
            rules: dict[str, str] = {}
            for key, css_name in (
                ("width", "width"),
                ("height", "height"),
                ("x", "left"),
                ("y", "top"),
            ):
                value = residual["geometry"]["absolute"].get(key, 0)
                if value:
                    target_node = self.comparison.query.query(
                        target_capture_id,
                        {"selector": residual["selector"]},
                    )["matches"][0]
                    rules[css_name] = f"{target_node['bounds'][key]}px"
            for mismatch in residual["style"]["mismatches"]:
                if mismatch["property"] in {
                    "color",
                    "backgroundColor",
                    "fontSize",
                    "fontWeight",
                    "lineHeight",
                    "borderRadius",
                    "opacity",
                    "transform",
                }:
                    css_name = self._css_name(mismatch["property"])
                    if mismatch["target"] is not None:
                        rules[css_name] = str(mismatch["target"])
            if rules:
                declarations[residual["selector"]] = rules
        if not declarations:
            raise ValueError("comparison produced no bounded CSS repair")
        marker = hashlib.sha256(
            canonical_json(
                {
                    "target_capture_id": target_capture_id,
                    "candidate_capture_id": candidate_capture_id,
                    "declarations": declarations,
                }
            )
        ).hexdigest()[:16]
        override = [f"\n/* VisionMCP governed repair {marker} */"]
        for selector, rules in sorted(declarations.items()):
            override.append(f"{selector} {{")
            override.extend(f"  {name}: {value};" for name, value in sorted(rules.items()))
            override.append("}")
        original = file_path.read_text(encoding="utf-8")
        result = f"{original.rstrip()}\n" + "\n".join(override) + "\n"
        result_record = self._ingest_text(result, "frontend-repair-result", "text/css")
        patch = {
            "schema": "vision.frontend-css-patch/v1",
            "target_file": str(file_path.relative_to(self.project.root)),
            "base_digest": base_digest,
            "result_digest": result_record["digest"],
            "selectors": selectors,
            "declarations": declarations,
            "comparison_id": comparison["id"],
            "authority": "PROPOSED",
        }
        patch_record = self.comparison._ingest_json(patch, "frontend-css-patch")
        proposal_id = hashlib.sha256(canonical_json(patch)).hexdigest()
        now = utc_now()
        with self.project.connection() as connection:
            connection.execute(
                "INSERT OR REPLACE INTO frontend_patch_proposals("
                "id,target_capture_id,candidate_capture_id,target_file,base_digest,"
                "result_digest,patch_digest,status,reviewer,reason,decision_digest,"
                "applied_digest,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    proposal_id,
                    target_capture_id,
                    candidate_capture_id,
                    str(file_path.relative_to(self.project.root)),
                    base_digest,
                    result_record["digest"],
                    patch_record["digest"],
                    "PROPOSED",
                    None,
                    None,
                    None,
                    None,
                    now,
                    now,
                ),
            )
        return {
            "id": proposal_id,
            "status": "PROPOSED",
            "patch": patch,
            "patch_digest": patch_record["digest"],
        }

    def review_patch(
        self,
        proposal_id: str,
        *,
        accepted: bool,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        if not reviewer.strip() or not reason.strip():
            raise ValueError("patch review requires a named reviewer and reason")
        proposal = self._proposal(proposal_id)
        if proposal["status"] != "PROPOSED":
            raise ValueError(f"patch proposal is not reviewable: {proposal['status']}")
        decision = {
            "schema": "vision.frontend-patch-decision/v1",
            "proposal_id": proposal_id,
            "accepted": accepted,
            "reviewer": reviewer.strip(),
            "reason": reason.strip(),
            "patch_digest": proposal["patch_digest"],
            "created_at": utc_now(),
        }
        record = self.comparison._ingest_json(decision, "frontend-patch-decision")
        status = "APPROVED" if accepted else "REJECTED"
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE frontend_patch_proposals SET status=?,reviewer=?,reason=?,"
                "decision_digest=?,updated_at=? WHERE id=?",
                (
                    status,
                    reviewer.strip(),
                    reason.strip(),
                    record["digest"],
                    decision["created_at"],
                    proposal_id,
                ),
            )
        return {**decision, "status": status, "decision_digest": record["digest"]}

    def apply_patch(self, proposal_id: str) -> dict[str, Any]:
        proposal = self._proposal(proposal_id)
        if proposal["status"] != "APPROVED" or not proposal["decision_digest"]:
            raise PermissionError("frontend patch requires an approved named decision")
        target = self._confined_target(proposal["target_file"])
        current_digest, _ = sha256_file(target)
        if current_digest != proposal["base_digest"]:
            raise RuntimeError("frontend patch base file changed after proposal")
        result_path = self.artifacts.path_for(proposal["result_digest"])
        result = result_path.read_text(encoding="utf-8")
        backup = self.artifacts.ingest_file(target, media_type="text/css")
        atomic_write_text(target, result)
        applied_digest, _ = sha256_file(target)
        if applied_digest != proposal["result_digest"]:
            raise RuntimeError("applied frontend patch digest mismatch")
        receipt = {
            "schema": "vision.frontend-patch-applied/v1",
            "proposal_id": proposal_id,
            "target_file": proposal["target_file"],
            "base_digest": proposal["base_digest"],
            "backup_digest": backup.digest,
            "applied_digest": applied_digest,
            "decision_digest": proposal["decision_digest"],
            "created_at": utc_now(),
        }
        record = self.comparison._ingest_json(receipt, "frontend-patch-applied")
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE frontend_patch_proposals SET status='APPLIED',applied_digest=?,"
                "updated_at=? WHERE id=?",
                (record["digest"], receipt["created_at"], proposal_id),
            )
        return {**receipt, "receipt_digest": record["digest"], "status": "APPLIED"}

    def _proposal(self, proposal_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM frontend_patch_proposals WHERE id=?",
                (proposal_id,),
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown frontend patch proposal: {proposal_id}")
        return dict(row)

    def _confined_target(self, target_file: str) -> Path:
        path = (self.project.root / target_file).resolve()
        try:
            path.relative_to(self.project.root)
        except ValueError as error:
            raise PermissionError("frontend patch target escapes the project") from error
        return path

    def _ingest_text(self, value: str, prefix: str, media_type: str) -> dict[str, Any]:
        staging = self.project.root / "observations" / ".staging"
        staging.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(
            prefix=f"{prefix}-", suffix=".txt", dir=staging, mode="w+", encoding="utf-8"
        ) as file:
            file.write(value)
            file.flush()
            return self.artifacts.ingest_file(
                Path(file.name), media_type=media_type
            ).to_dict()

    @staticmethod
    def _css_name(value: str) -> str:
        output = []
        for character in value:
            if character.isupper():
                output.extend(("-", character.lower()))
            else:
                output.append(character)
        return "".join(output)
