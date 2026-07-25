from __future__ import annotations

import hashlib
import json
import math
import statistics
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.errors import ProjectError
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.parametric.store import ComponentStore
from blender_vision.projects.store import ProjectStore

EVIDENCE_WEIGHTS = {
    "MEASURED": 1.0,
    "MANUFACTURER_SPEC": 1.0,
    "MULTI_VIEW_OBSERVED": 0.8,
    "TEARDOWN_OBSERVED": 0.8,
    "SINGLE_VIEW_OBSERVED": 0.5,
    "INFERRED_HIGH_CONFIDENCE": 0.35,
    "INFERRED_LOW_CONFIDENCE": 0.1,
    "OCCLUDED": 0.02,
    "UNSEEN": 0.0,
}


class ComponentFitter:
    """Robustly fit scalar component parameters to stored measurement evidence."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.components = ComponentStore(project)

    def propose(
        self,
        component_id: str,
        parameter_bindings: dict[str, list[str]],
        *,
        huber_delta: float = 1.5,
    ) -> dict[str, Any]:
        if not math.isfinite(huber_delta) or huber_delta <= 0:
            raise ValueError("huber_delta must be finite and positive")
        component = self.components.get(component_id)
        if not parameter_bindings:
            raise ValueError("component fit requires parameter-to-measurement bindings")
        with self.project.connection() as connection:
            rows = connection.execute("SELECT * FROM measurements").fetchall()
        measurements = {
            row["id"]: {
                **dict(row),
                "value": json.loads(row["value_json"]),
                "uncertainty": json.loads(row["uncertainty_json"]),
            }
            for row in rows
        }
        unknown = sorted(
            measurement_id
            for ids in parameter_bindings.values()
            for measurement_id in ids
            if measurement_id not in measurements
        )
        if unknown:
            raise ValueError("component fit references unknown measurements: " + ", ".join(unknown))
        candidates: dict[str, float] = {}
        parameter_reports: dict[str, Any] = {}
        release_eligible = True
        for parameter, measurement_ids in parameter_bindings.items():
            if parameter not in component["parameters"]:
                raise ValueError(f"component has no parameter named {parameter}")
            if not isinstance(component["parameters"][parameter], (int, float)):
                raise ValueError(f"component parameter is not scalar numeric: {parameter}")
            if not measurement_ids:
                raise ValueError(f"component fit parameter has no measurements: {parameter}")
            observations = [self._observation(measurements[item]) for item in measurement_ids]
            fitted = self._robust_mean(observations, huber_delta)
            candidates[parameter] = fitted
            residuals = [fitted - item["value"] for item in observations]
            rmse = math.sqrt(sum(value * value for value in residuals) / len(residuals))
            release_eligible = release_eligible and all(
                item["evidence_weight"] >= EVIDENCE_WEIGHTS["MULTI_VIEW_OBSERVED"]
                for item in observations
            )
            parameter_reports[parameter] = {
                "before": float(component["parameters"][parameter]),
                "candidate": fitted,
                "delta": fitted - float(component["parameters"][parameter]),
                "rmse": rmse,
                "observations": observations,
            }
        constraint_checks = self._validate_constraints(component, candidates)
        constraints_pass = all(item["passed"] for item in constraint_checks)
        fit_id = str(uuid.uuid4())
        created_at = utc_now()
        inputs = {
            "component_id": component_id,
            "component_revision": component["revision"],
            "component_snapshot_sha256": hashlib.sha256(
                canonical_json(component)
            ).hexdigest(),
            "parameter_bindings": parameter_bindings,
            "huber_delta": huber_delta,
        }
        result = {
            "candidate_parameters": candidates,
            "parameter_reports": parameter_reports,
            "constraint_checks": constraint_checks,
            "constraints_pass": constraints_pass,
            "release_eligible_evidence": release_eligible,
            "requires_named_review": True,
        }
        record = {
            "schema_version": 1,
            "id": fit_id,
            "status": "proposed",
            "inputs": inputs,
            "result": result,
            "created_at": created_at,
        }
        relative = Path("geometry") / "fits" / f"component-fit-{fit_id}.json"
        atomic_write_json(self.project.root / relative, record)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.component-fit+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO component_fits("
                "id,component_id,status,input_json,result_json,record_digest,created_at,"
                "updated_at) "
                "VALUES(?,?,?,?,?,?,?,?)",
                (
                    fit_id,
                    component_id,
                    "proposed",
                    json.dumps(inputs),
                    json.dumps(result),
                    artifact.digest,
                    created_at,
                    created_at,
                ),
            )
        return {**record, "path": str(relative), "artifact": artifact.to_dict()}

    def review(self, fit_id: str, *, accepted: bool, reviewer: str, reason: str) -> dict[str, Any]:
        if not reviewer.strip() or not reason.strip():
            raise ValueError("component fit review requires a named reviewer and reason")
        fit = self.get(fit_id)
        if fit["status"] != "proposed":
            raise ProjectError(f"component fit is not awaiting review: {fit_id}")
        if not self._proposal_artifact_valid(fit):
            raise ProjectError(
                "component fit proposal artifact is missing, corrupt, or inconsistent"
            )
        if accepted and not fit["result"]["constraints_pass"]:
            raise ProjectError("component fit cannot be accepted while constraints fail")
        now = utc_now()
        status = "accepted" if accepted else "rejected"
        current = self.components.get(fit["component_id"])
        expected_snapshot = fit["inputs"].get("component_snapshot_sha256")
        current_snapshot = hashlib.sha256(canonical_json(current)).hexdigest()
        if accepted and (
            current["revision"] != fit["inputs"]["component_revision"]
            or not expected_snapshot
            or current_snapshot != expected_snapshot
        ):
            raise ProjectError("component changed after fit; generate a new fit proposal")
        revision = int(current["revision"]) + 1 if accepted else None
        decision = {
            "schema_version": 1,
            "receipt_type": "component_fit_review",
            "fit_id": fit_id,
            "proposal_digest": fit["artifact_digest"],
            "component_id": fit["component_id"],
            "component_snapshot_sha256": current_snapshot,
            "component_revision_before": current["revision"],
            "component_revision_after": revision,
            "candidate_parameters": fit["result"]["candidate_parameters"],
            "decision": status,
            "reviewer": reviewer.strip(),
            "reason": reason.strip(),
            "created_at": now,
        }
        relative = Path("receipts") / f"component-fit-review-{fit_id}-{status}.json"
        atomic_write_json(self.project.root / relative, decision)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.component-fit-review+json",
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            persisted_fit = connection.execute(
                "SELECT status,record_digest FROM component_fits WHERE id=?", (fit_id,)
            ).fetchone()
            if (
                persisted_fit is None
                or persisted_fit["status"] != "proposed"
                or persisted_fit["record_digest"] != fit["artifact_digest"]
            ):
                raise ProjectError(f"component fit review raced with another decision: {fit_id}")
            component_row = connection.execute(
                "SELECT record_json,revision,created_at,updated_at FROM components WHERE id=?",
                (fit["component_id"],),
            ).fetchone()
            if component_row is None:
                raise FileNotFoundError(f"unknown component: {fit['component_id']}")
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
                raise ProjectError("component changed while the fit decision was being recorded")
            if accepted:
                if (
                    component_row["revision"] != fit["inputs"]["component_revision"]
                    or persisted_snapshot != expected_snapshot
                ):
                    raise ProjectError("component changed after fit; generate a new fit proposal")
                component_record["parameters"].update(
                    fit["result"]["candidate_parameters"]
                )
                updated = connection.execute(
                    "UPDATE components SET record_json=?,revision=?,updated_at=? "
                    "WHERE id=? AND revision=?",
                    (
                        json.dumps(component_record),
                        revision,
                        now,
                        fit["component_id"],
                        component_row["revision"],
                    ),
                )
                if updated.rowcount != 1:
                    raise ProjectError("component fit parameter update raced with another revision")
            updated = connection.execute(
                "UPDATE component_fits SET status=?,reviewer=?,reason=?,reviewed_at=?,"
                "applied_revision=?,decision_digest=?,updated_at=? "
                "WHERE id=? AND status='proposed' AND decision_digest IS NULL",
                (
                    status,
                    reviewer.strip(),
                    reason.strip(),
                    now,
                    revision,
                    artifact.digest,
                    now,
                    fit_id,
                ),
            )
            if updated.rowcount != 1:
                raise ProjectError(f"component fit review raced with another decision: {fit_id}")
        return {
            **self.get(fit_id),
            "decision_receipt": decision,
            "decision_artifact": artifact.to_dict(),
            "decision_path": str(relative),
        }

    def get(self, fit_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM component_fits WHERE id=?", (fit_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown component fit: {fit_id}")
        value = dict(row)
        value["inputs"] = json.loads(value.pop("input_json"))
        value["result"] = json.loads(value.pop("result_json"))
        value["artifact_digest"] = value.pop("record_digest")
        return value

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            ids = [
                row[0]
                for row in connection.execute("SELECT id FROM component_fits ORDER BY created_at")
            ]
        return [self.get(fit_id) for fit_id in ids]

    def _proposal_artifact_valid(self, fit: dict[str, Any]) -> bool:
        try:
            path = self.artifacts.path_for(fit["artifact_digest"])
            if not path.is_file() or sha256_file(path)[0] != fit["artifact_digest"]:
                return False
            proposal = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError, json.JSONDecodeError):
            return False
        return (
            proposal.get("id") == fit["id"]
            and proposal.get("status") == "proposed"
            and proposal.get("inputs") == fit["inputs"]
            and proposal.get("result") == fit["result"]
        )

    @staticmethod
    def _observation(measurement: dict[str, Any]) -> dict[str, Any]:
        value = measurement["value"].get("millimetres", measurement["value"].get("value"))
        if not isinstance(value, (int, float)) or not math.isfinite(float(value)):
            raise ValueError(f"measurement is not scalar numeric: {measurement['id']}")
        uncertainty = measurement["uncertainty"].get("millimetres", 1.0)
        sigma = float(uncertainty) if isinstance(uncertainty, (int, float)) else 1.0
        sigma = max(abs(sigma), 1e-6)
        authority = EVIDENCE_WEIGHTS.get(measurement["evidence_class"], 0.0)
        if authority <= 0:
            raise ValueError(f"measurement has no fitting authority: {measurement['id']}")
        return {
            "measurement_id": measurement["id"],
            "value": float(value),
            "sigma": sigma,
            "evidence_class": measurement["evidence_class"],
            "evidence_weight": authority,
            "base_weight": authority / (sigma * sigma),
        }

    @staticmethod
    def _robust_mean(observations: list[dict[str, Any]], huber_delta: float) -> float:
        weights = [item["base_weight"] for item in observations]
        value = sum(
            item["value"] * weight for item, weight in zip(observations, weights, strict=True)
        ) / sum(weights)
        scale = statistics.median(item["sigma"] for item in observations)
        for _iteration in range(8):
            robust = []
            for item in observations:
                normalized = abs(item["value"] - value) / max(scale, item["sigma"], 1e-6)
                factor = 1.0 if normalized <= huber_delta else huber_delta / normalized
                robust.append(item["base_weight"] * factor)
            value = sum(
                item["value"] * weight for item, weight in zip(observations, robust, strict=True)
            ) / sum(robust)
        return value

    @staticmethod
    def _validate_constraints(
        component: dict[str, Any], candidates: dict[str, float]
    ) -> list[dict[str, Any]]:
        parameters = {**component["parameters"], **candidates}
        checks = []
        for constraint in component.get("constraints", []):
            values = constraint.get("parameters", {})
            parameter = values.get("parameter")
            expected = values.get("value", values.get("millimetres"))
            tolerance = float(values.get("tolerance", values.get("tolerance_mm", 0.0)))
            if constraint.get("type") in {"known_dimension", "fixed_offset"} and (
                parameter in parameters and isinstance(expected, (int, float))
            ):
                residual = float(parameters[parameter]) - float(expected)
                checks.append(
                    {
                        "constraint_id": constraint["id"],
                        "type": constraint["type"],
                        "residual": residual,
                        "tolerance": tolerance,
                        "passed": abs(residual) <= tolerance,
                    }
                )
            else:
                checks.append(
                    {
                        "constraint_id": constraint["id"],
                        "type": constraint["type"],
                        "passed": True,
                        "note": "structural constraint retained for Blender validation hook",
                    }
                )
        return checks
