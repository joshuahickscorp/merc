from __future__ import annotations

import json
import uuid
from typing import Any

from blender_vision.core.util import utc_now
from blender_vision.orchestration.resources import profile
from blender_vision.projects.store import ProjectStore

STATES = (
    "OBSERVE",
    "DIAGNOSE",
    "PROPOSE",
    "ESTIMATE_BENEFIT",
    "EXECUTE",
    "RENDER",
    "MEASURE",
    "ACCEPT_OR_ROLLBACK",
    "CONTINUE",
)

REQUIRED_PAYLOAD = {
    "OBSERVE": {"coverage", "current_metrics"},
    "DIAGNOSE": {"diagnosis", "supporting_evidence"},
    "PROPOSE": {"affected_components", "proposed_operation", "rollback_checkpoint"},
    "ESTIMATE_BENEFIT": {"expected_metric_changes", "risk", "estimated_cost"},
    "EXECUTE": {"execution_record"},
    "RENDER": {"render_run_id"},
    "MEASURE": {"metrics"},
    "ACCEPT_OR_ROLLBACK": {"accepted", "reason"},
    "CONTINUE": set(),
}

DIAGNOSES = {
    "camera mismatch",
    "scale mismatch",
    "silhouette mismatch",
    "missing component",
    "misplaced component",
    "incorrect depth",
    "incorrect curvature",
    "incorrect repeated pattern",
    "material mismatch",
    "lighting contamination",
    "reference conflict",
    "insufficient evidence",
}

AGENT_ROLES = {
    "Evidence Researcher",
    "Variant Resolver",
    "Capture Planner",
    "Camera Analyst",
    "Geometry Analyst",
    "Component Modeler",
    "Material Analyst",
    "Optimization Planner",
    "Adversarial Reviewer",
    "Acceptance Auditor",
}

ALLOWED_PROPOSAL_PREFIXES = (
    "component.",
    "repair.",
    "portfolio.",
    "optimization.",
    "blender.generate_",
    "evidence.",
    "camera.",
)


class CampaignStore:
    def __init__(self, project: ProjectStore):
        self.project = project

    def start(
        self,
        kind: str,
        *,
        configuration: dict[str, Any],
        resource_profile: str = "auto",
        budget: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        campaign_id = str(uuid.uuid4())
        now = utc_now()
        config = {**configuration, "resources": profile(resource_profile)}
        governed_budget = {
            "maximum_iterations": 50,
            "minimum_expected_improvement": 0.001,
            "maximum_compute_cost": None,
            **(budget or {}),
        }
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO campaigns(id,kind,status,controller_state,iteration,config_json,"
                "budget_json,result_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
                (
                    campaign_id,
                    kind,
                    "RUNNING",
                    "OBSERVE",
                    0,
                    json.dumps(config),
                    json.dumps(governed_budget),
                    None,
                    now,
                    now,
                ),
            )
        self._event(campaign_id, "OBSERVE", {"event": "campaign_started"})
        return self.get(campaign_id)

    def advance(self, campaign_id: str, payload: dict[str, Any]) -> dict[str, Any]:
        campaign = self.get(campaign_id)
        if campaign["status"] != "RUNNING":
            raise ValueError("only a running campaign can advance")
        state = campaign["controller_state"]
        missing = sorted(REQUIRED_PAYLOAD[state] - set(payload))
        if missing:
            raise ValueError(f"{state} payload missing fields: {', '.join(missing)}")
        if state == "DIAGNOSE" and payload["diagnosis"] not in DIAGNOSES:
            raise ValueError("unsupported diagnosis class")
        if state == "PROPOSE":
            operation = str(payload["proposed_operation"])
            if not operation.startswith(ALLOWED_PROPOSAL_PREFIXES):
                raise ValueError("proposal operation is outside the governed allowlist")
            role = str(payload.get("agent_role", "Optimization Planner"))
            if role not in AGENT_ROLES:
                raise ValueError("proposal agent role is unsupported")
            payload = {**payload, "agent_role": role}
            self._record_proposal(campaign, payload)
        if state == "EXECUTE":
            with self.project.connection() as connection:
                proposal = connection.execute(
                    "SELECT id FROM agent_proposals WHERE campaign_id=? AND iteration=?",
                    (campaign_id, campaign["iteration"]),
                ).fetchone()
            if proposal is None:
                raise ValueError("execution requires a recorded proposal and rollback checkpoint")
        next_state = STATES[(STATES.index(state) + 1) % len(STATES)]
        iteration = campaign["iteration"]
        result = campaign.get("result") or {}
        if state == "ACCEPT_OR_ROLLBACK":
            result["last_decision"] = payload
            result["rollback_required"] = not bool(payload["accepted"])
        if state == "CONTINUE":
            iteration += 1
            stop_reason = self._stop_reason(campaign, payload, iteration)
            if stop_reason:
                return self.stop(campaign_id, reason=stop_reason, result=result)
        now = utc_now()
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE campaigns SET controller_state=?,iteration=?,result_json=?,updated_at=? "
                "WHERE id=?",
                (next_state, iteration, json.dumps(result), now, campaign_id),
            )
        self._event(campaign_id, state, payload)
        return self.get(campaign_id)

    def pause(self, campaign_id: str, *, reason: str) -> dict[str, Any]:
        return self._status(campaign_id, "PAUSED", reason)

    def resume(self, campaign_id: str) -> dict[str, Any]:
        campaign = self.get(campaign_id)
        if campaign["status"] != "PAUSED":
            raise ValueError("only a paused campaign can resume")
        return self._status(campaign_id, "RUNNING", "campaign resumed")

    def stop(
        self, campaign_id: str, *, reason: str, result: dict[str, Any] | None = None
    ) -> dict[str, Any]:
        campaign = self.get(campaign_id)
        final = {**(campaign.get("result") or {}), **(result or {}), "stop_reason": reason}
        now = utc_now()
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE campaigns SET status='STOPPED',result_json=?,updated_at=? WHERE id=?",
                (json.dumps(final), now, campaign_id),
            )
        self._event(
            campaign_id, campaign["controller_state"], {"event": "stopped", "reason": reason}
        )
        return self.get(campaign_id)

    def progress(
        self, campaign_id: str, *, message: str, details: dict[str, Any] | None = None
    ) -> dict[str, Any]:
        campaign = self.get(campaign_id)
        if not message.strip():
            raise ValueError("campaign progress requires a message")
        self._event(
            campaign_id,
            campaign["controller_state"],
            {"event": "progress", "message": message.strip(), "details": details or {}},
        )
        return self.get(campaign_id)

    def get(self, campaign_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM campaigns WHERE id=?", (campaign_id,)
            ).fetchone()
            events = connection.execute(
                "SELECT * FROM campaign_events WHERE campaign_id=? ORDER BY sequence",
                (campaign_id,),
            ).fetchall()
        if row is None:
            raise KeyError(f"unknown campaign: {campaign_id}")
        value = dict(row)
        value["configuration"] = json.loads(value.pop("config_json"))
        value["budget"] = json.loads(value.pop("budget_json"))
        value["result"] = json.loads(value.pop("result_json")) if value["result_json"] else None
        value["events"] = [
            {**dict(item), "payload": json.loads(item["payload_json"])} for item in events
        ]
        for item in value["events"]:
            item.pop("payload_json")
        return value

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute("SELECT id FROM campaigns ORDER BY created_at,id").fetchall()
        return [self.get(row["id"]) for row in rows]

    def _record_proposal(self, campaign: dict[str, Any], payload: dict[str, Any]) -> None:
        proposal_id = str(uuid.uuid4())
        now = utc_now()
        diagnosis = next(
            (
                event["payload"].get("diagnosis")
                for event in reversed(campaign["events"])
                if event["controller_state"] == "DIAGNOSE"
            ),
            "unrecorded",
        )
        record = {
            "id": proposal_id,
            "campaign_id": campaign["id"],
            "iteration": campaign["iteration"],
            "diagnosis": diagnosis,
            **payload,
            "status": "PROPOSED",
            "created_at": now,
        }
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO agent_proposals(id,campaign_id,iteration,diagnosis,record_json,"
                "status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)",
                (
                    proposal_id,
                    campaign["id"],
                    campaign["iteration"],
                    diagnosis,
                    json.dumps(record),
                    "PROPOSED",
                    now,
                    now,
                ),
            )

    def _stop_reason(
        self, campaign: dict[str, Any], payload: dict[str, Any], iteration: int
    ) -> str | None:
        if payload.get("all_requested_gates_pass"):
            return "all requested gates pass"
        if payload.get("evidence_ceiling_reached"):
            return "evidence ceiling reached"
        if payload.get("compute_budget_reached"):
            return "compute budget reached"
        if payload.get("requires_external_evidence"):
            return "remaining uncertainty requires external evidence"
        if iteration >= int(campaign["budget"]["maximum_iterations"]):
            return "maximum iteration budget reached"
        improvement = payload.get("expected_improvement")
        if improvement is not None and float(improvement) < float(
            campaign["budget"]["minimum_expected_improvement"]
        ):
            return "expected improvement is below threshold"
        return None

    def _status(self, campaign_id: str, status: str, reason: str) -> dict[str, Any]:
        campaign = self.get(campaign_id)
        if campaign["status"] == "STOPPED":
            raise ValueError("stopped campaigns cannot change status")
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE campaigns SET status=?,updated_at=? WHERE id=?",
                (status, utc_now(), campaign_id),
            )
        self._event(
            campaign_id, campaign["controller_state"], {"event": status.lower(), "reason": reason}
        )
        return self.get(campaign_id)

    def _event(self, campaign_id: str, state: str, payload: dict[str, Any]) -> None:
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO campaign_events(campaign_id,controller_state,payload_json,created_at) "
                "VALUES(?,?,?,?)",
                (campaign_id, state, json.dumps(payload), utc_now()),
            )
